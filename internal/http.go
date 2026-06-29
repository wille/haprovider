package internal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	servertiming "github.com/mitchellh/go-server-timing"
	"github.com/wille/haprovider/internal/chain"
	"github.com/wille/haprovider/internal/core"
	"github.com/wille/haprovider/internal/httpx"
	"github.com/wille/haprovider/internal/metrics"
	"github.com/wille/haprovider/internal/rpc"
)

func IncomingHttpRpcHandler(ctx context.Context, endpoint *core.Endpoint, w http.ResponseWriter, r *http.Request, timing *servertiming.Header) {

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
			return
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

		log = log.With("provider", provider.Name, "request_time", time.Since(start))

		if req.IsBatch {
			log.Debug("batch request", "size", len(req.Requests))
		} else {
			log.Debug("request", "method", req.Requests[0].Method)
		}

		for _, res := range res.Responses {
			if res.IsError() {
				errorCode, errorMessage := res.GetError()
				log.Debug("error response", "error_code", errorCode, "error_message", errorMessage)

				err := chain.New(endpoint.Kind).HandleError(errorCode, errorMessage.Error())

				if err != nil {
					provider.MarkUnhealthy(err)
					continue NextProvider
				}
			}
		}

		if req.IsBatch && len(res.Responses) != len(req.Requests) {
			log.Error("batch response size mismatch", "request_size", len(req.Requests), "response_size", len(res.Responses))
			provider.MarkUnhealthy(fmt.Errorf("batch response size mismatch"))
			continue NextProvider
		}

		if !endpoint.Public {
			w.Header().Set("X-Provider", provider.Name)
		}

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

		return
	}

	log.Error("no providers available")

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusServiceUnavailable)
	if err := rpc.WriteResponse(w, &rpc.ErrorResponseNoProvidersAvailable); err != nil {
		log.Error("error writing body", "error", err)
	}
}

// IncomingHTTPAPIHandler proxies a native TRON full-node HTTP API request (path,
// method, query and body preserved) to an online provider, with provider
// selection and failover. The path is forwarded under the provider's HTTP API
// base (see HTTPAPIBase). This is TRON-specific: other chains proxy JSON-RPC.
func IncomingHTTPAPIHandler(ctx context.Context, endpoint *core.Endpoint, path string, w http.ResponseWriter, r *http.Request, timing *servertiming.Header) {
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

		metrics.RecordRequest(endpoint.Name, provider.Name, "http", path, 0)

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
