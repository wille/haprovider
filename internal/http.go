package internal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	servertiming "github.com/mitchellh/go-server-timing"
	"github.com/wille/haprovider/internal/cache"
	"github.com/wille/haprovider/internal/chain"
	"github.com/wille/haprovider/internal/core"
	"github.com/wille/haprovider/internal/httpx"
	"github.com/wille/haprovider/internal/metrics"
	"github.com/wille/haprovider/internal/rpc"
)

func IncomingHttpRpcHandler(ctx context.Context, endpoint *core.Endpoint, w http.ResponseWriter, r *http.Request, timing *servertiming.Header) {
	defer metrics.TrackInflight(endpoint.Name, "http")()

	log := endpoint.Logger().With("ip", r.RemoteAddr, "transport", "http")

	if !strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		log.Error("http: close connection: invalid content type")
		http.Error(w, "invalid content type", http.StatusBadRequest)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, rpc.MaxRequestBodySize)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			log.Error("http: request body too large", "limit", rpc.MaxRequestBodySize)
			http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
			return
		}

		log.Error("http: error reading body", "error", err)
		http.Error(w, "error reading body", http.StatusInternalServerError)
		return
	}

	req, err := rpc.DecodeBatchRequest(body)
	if err != nil {
		log.Error("http: bad request", "error", err, "body", rpc.FormatRawBody(string(body)))
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	if len(req.Requests) > rpc.MaxBatchSize {
		log.Error("batch too large", "batch_size", len(req.Requests), "max", rpc.MaxBatchSize)
		http.Error(w, "batch too large", http.StatusRequestEntityTooLarge)
		return
	}

	if req.IsBatch {
		batchId := rpc.BatchIDCounter.Add(1)
		log = log.With("batch_id", batchId, "batch_size", len(req.Requests))
	} else {
		log = log.With("rpc_id", req.Requests[0].GetID(), "method", req.Requests[0].Method)
	}

	if endpoint.AddXForwardedHeaders {
		ctx = httpx.WithForwardedFor(ctx, r)
	}

	c := chain.New(endpoint.Kind)

	// Cache plan: for each sub-request, serve cacheable hits from the cache and
	// collect the rest (misses + non-cacheable) to forward upstream. This works
	// for a single request and for every element of a batch independently.
	cacheKeys := make([]string, len(req.Requests)) // "" = not cacheable, so not stored
	responses := make([]*rpc.Response, len(req.Requests))
	missIdx := make([]int, 0, len(req.Requests))

	for i, sub := range req.Requests {
		if endpoint.CacheEnabled() && c.CacheableRequest(sub.Method, sub.Params) {
			key := cache.Key(sub.Method, sub.Params)
			cacheKeys[i] = key
			if val, ok, _ := endpoint.Cache().Get(ctx, key); ok {
				metrics.RecordCacheHit(endpoint.Name, sub.Method)
				responses[i] = &rpc.Response{Version: "2.0", ID: sub.ID, Result: val}
				continue
			}
			metrics.RecordCacheMiss(endpoint.Name, sub.Method)
		}
		missIdx = append(missIdx, i)
	}

	// Everything was a cache hit: answer without contacting a provider.
	if len(missIdx) == 0 {
		log.Debug("cache hit", "batch", req.IsBatch, "size", len(req.Requests))
		if !endpoint.Public {
			w.Header().Set("X-Cache", "HIT")
		}
		writeBatch(w, &rpc.BatchResponse{Responses: responses, IsBatch: req.IsBatch}, log)
		return
	}

	// Forward only the misses upstream, preserving the original request shape.
	miss := &rpc.BatchRequest{IsBatch: req.IsBatch, Requests: make([]*rpc.Request, len(missIdx))}
	for j, idx := range missIdx {
		miss.Requests[j] = req.Requests[idx]
	}

	res, provider, err := forwardBatch(ctx, endpoint, c, miss, timing, log)
	if err != nil {
		writeForwardError(w, err, log)
		return
	}

	// Store fresh cacheable results and merge the upstream responses back into
	// their original positions.
	for j, idx := range missIdx {
		sub := res.Responses[j]
		responses[idx] = sub
		if cacheKeys[idx] != "" && !sub.IsError() && c.CacheableResponse(req.Requests[idx].Method, sub.Result) {
			_ = endpoint.Cache().Set(ctx, cacheKeys[idx], sub.Result, endpoint.GetCacheTTL())
		}
	}

	if !endpoint.Public {
		w.Header().Set("X-Provider", provider.Name)
	}
	writeBatch(w, &rpc.BatchResponse{Responses: responses, IsBatch: req.IsBatch}, log)
}

// errNoProvidersAvailable is returned by forwardBatch when no online provider
// could serve the request.
var errNoProvidersAvailable = errors.New("no providers available")

// forwardBatch runs the provider failover loop for req and returns the first
// successful response and the provider that produced it. It records per-request
// metrics and marks providers unhealthy on failure. It returns ctx.Err() if the
// context is cancelled mid-flight, or errNoProvidersAvailable when exhausted.
func forwardBatch(ctx context.Context, endpoint *core.Endpoint, c chain.Chain, req *rpc.BatchRequest, timing *servertiming.Header, log *slog.Logger) (*rpc.BatchResponse, *core.Provider, error) {
NextProvider:
	for _, provider := range endpoint.Providers {
		if !provider.IsOnline() {
			continue
		}

		start := time.Now()

		m := timing.NewMetric(provider.Name).Start()
		res, err := httpx.SendRPCBatchRequest(ctx, provider, req)
		m.Stop()

		// Client connection closed
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		default:
		}

		if err != nil {
			// In a high traffic environment, the provider might be taken offline by another concurrent request
			if provider.IsOnline() {
				provider.MarkUnhealthy(err)
			}

			// Do not pass the failing method here as the whole request failed
			metrics.RecordFailedRequest(endpoint.Name, provider.Name, "http", "")

			continue NextProvider
		}

		// A response must map one-to-one to the request; otherwise the merge back
		// to the client is unsafe.
		if len(res.Responses) != len(req.Requests) {
			log.Error("batch response size mismatch", "request_size", len(req.Requests), "response_size", len(res.Responses))
			provider.MarkUnhealthy(fmt.Errorf("batch response size mismatch"))
			continue NextProvider
		}

		// Record one metric per (sub-)request, labeled by method, like the WS path.
		elapsed := time.Since(start).Seconds()
		for i, sub := range res.Responses {
			method := req.Requests[i].Method
			if sub.IsError() {
				metrics.RecordFailedRequest(endpoint.Name, provider.Name, "http", method)
			} else {
				metrics.RecordRequest(endpoint.Name, provider.Name, "http", method, elapsed)
			}
		}

		for _, sub := range res.Responses {
			if sub.IsError() {
				errorCode, errorMessage := sub.GetError()
				log.Debug("error response", "error_code", errorCode, "error_message", errorMessage)

				if herr := c.HandleError(errorCode, errorMessage.Error()); herr != nil {
					provider.MarkUnhealthy(herr)
					continue NextProvider
				}
			}
		}

		log.With("provider", provider.Name, "request_time", time.Since(start).String()).Debug("forwarded")
		return res, provider, nil
	}

	return nil, nil, errNoProvidersAvailable
}

// writeForwardError writes the appropriate client response for a failover error.
func writeForwardError(w http.ResponseWriter, err error, log *slog.Logger) {
	// Client disconnected mid-flight: nothing left to write to.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}

	if errors.Is(err, errNoProvidersAvailable) {
		log.Error("no providers available")
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		if werr := rpc.WriteResponse(w, &rpc.ErrorResponseNoProvidersAvailable); werr != nil {
			log.Error("error writing body", "error", werr)
		}
		return
	}

	log.Error("request failed", "error", err)
	http.Error(w, "request failed", http.StatusInternalServerError)
}

// writeBatch serializes and writes a batch response, setting the JSON content type.
func writeBatch(w http.ResponseWriter, res *rpc.BatchResponse, log *slog.Logger) {
	out, err := rpc.SerializeBatchResponse(res)
	if err != nil {
		log.Error("error serializing response", "error", err)
		http.Error(w, "error serializing response", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	if _, err := w.Write(out); err != nil {
		log.Error("error writing body", "error", err)
	}
}

// IncomingHTTPAPIHandler proxies a native TRON full-node HTTP API request (path,
// method, query and body preserved) to an online provider, with provider
// selection and failover. The path is forwarded under the provider's HTTP API
// base (see HTTPAPIBase). This is TRON-specific: other chains proxy JSON-RPC.
func IncomingHTTPAPIHandler(ctx context.Context, endpoint *core.Endpoint, path string, w http.ResponseWriter, r *http.Request, timing *servertiming.Header) {
	defer metrics.TrackInflight(endpoint.Name, "http")()

	log := endpoint.Logger().With("ip", r.RemoteAddr, "transport", "http", "path", path)

	r.Body = http.MaxBytesReader(w, r.Body, rpc.MaxRequestBodySize)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			log.Error("http: request body too large", "limit", rpc.MaxRequestBodySize)
			http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
			return
		}

		log.Error("http: error reading body", "error", err)
		http.Error(w, "error reading body", http.StatusInternalServerError)
		return
	}

	if endpoint.AddXForwardedHeaders {
		ctx = httpx.WithForwardedFor(ctx, r)
	}

	for _, provider := range endpoint.Providers {
		if !provider.IsOnline() {
			continue
		}

		t := strings.TrimSuffix(provider.Http, "/jsonrpc")

		target := t + "/" + path
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}

		start := time.Now()
		m := timing.NewMetric(provider.Name).Start()
		res, err := httpx.ForwardHTTPRequest(ctx, provider, r.Method, target, body, r.Header.Get("Content-Type"))
		m.Stop()

		log.Debug("forwarded http request", "target", target, "method", r.Method, "body", string(body), "content_type", r.Header.Get("Content-Type"))

		// Client connection closed
		select {
		case <-ctx.Done():
			return
		default:
		}

		if err != nil {
			// In a high traffic environment, the provider might be taken offline by another concurrent request
			if provider.IsOnline() {
				provider.MarkUnhealthy(err)
			}
			metrics.RecordFailedRequest(endpoint.Name, provider.Name, "http", path)
			log.Error("error proxying request", "error", err)
			continue
		}

		metrics.RecordRequest(endpoint.Name, provider.Name, "http", path, time.Since(start).Seconds())

		if !endpoint.Public {
			w.Header().Set("X-Provider", provider.Name)
		}
		if res.ContentType != "" {
			w.Header().Set("Content-Type", res.ContentType)
		}
		w.WriteHeader(res.StatusCode)
		if _, err := w.Write(res.Body); err != nil {
			log.Error("error writing body", "error", err)
		}

		return
	}

	log.Error("no providers available")
	http.Error(w, "no providers available", http.StatusServiceUnavailable)
}
