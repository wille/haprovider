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
	"github.com/wille/haprovider/internal/chain"
	"github.com/wille/haprovider/internal/core"
	"github.com/wille/haprovider/internal/httpx"
	"github.com/wille/haprovider/internal/metrics"
	"github.com/wille/haprovider/internal/rpc"
)

// errCodeResourceUnavailable is the JSON-RPC error code ("Resource Unavailable")
// returned when a chain does not allow a client to call a method.
const errCodeResourceUnavailable = -32002

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

	// Coalesce a single (non-batch) read-only call: identical concurrent requests
	// share one upstream call. Batches and stateful/non-idempotent methods run
	// their own request (see chain.Coalesceable).
	c := chain.New(endpoint.Kind)

	// Reject requests to methods the chain doesn't allow clients to call (e.g.
	// Bitcoin wallet/mining/control commands). A request containing any disallowed
	// method is rejected in full.
	if c != nil {
		for _, sub := range req.Requests {
			if !c.AllowMethod(sub.Method) {
				log.Warn("method not allowed", "method", sub.Method)
				writeMethodNotAllowed(w, c, req, log)
				return
			}
		}
	}

	if !req.IsBatch && len(req.Requests) == 1 && c != nil && c.Coalesceable(req.Requests[0].Method) {
		coalesceRequest(ctx, endpoint, req, timing, log, w)
		return
	}

	result, err := forwardWithFailover(ctx, endpoint, req, timing, log)
	if err != nil {
		writeForwardError(w, err, log)
		return
	}
	writeBatchResponse(w, endpoint, result.provider, result.res, log)
}

// forwardResult is the outcome of a successful failover: the upstream response
// and the provider that served it. It is shared verbatim between coalesced
// callers, so callers must not mutate its response before writing.
type forwardResult struct {
	res      *rpc.BatchResponse
	provider *core.Provider
}

// errNoProvidersAvailable is returned by forwardWithFailover when every provider
// is offline or has failed the request.
var errNoProvidersAvailable = errors.New("no providers available")

// forwardWithFailover runs the provider failover loop for req and returns the
// first successful response and the provider that produced it. It records
// per-request metrics and marks providers unhealthy on failure. It returns
// ctx.Err() if the context is cancelled mid-flight, or errNoProvidersAvailable
// when no provider could serve the request.
func forwardWithFailover(ctx context.Context, endpoint *core.Endpoint, req *rpc.BatchRequest, timing *servertiming.Header, log *slog.Logger) (*forwardResult, error) {
NextProvider:
	for _, provider := range endpoint.Providers {
		if !provider.IsOnline() {
			continue
		}

		start := time.Now()

		m := timing.NewMetric(provider.Name).Start()
		res, err := httpx.SendRPCBatchRequest(ctx, provider, req)
		m.Stop()

		// Client connection closed (or, for a coalesced call, the detached
		// context — which never cancels).
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
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

		reqLog := log.With("provider", provider.Name, "request_time", time.Since(start).String())
		if req.IsBatch {
			reqLog.Debug("batch request", "size", len(req.Requests))
		} else {
			reqLog.Debug("request", "method", req.Requests[0].Method)
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
				reqLog.Debug("error response", "error_code", errorCode, "error_message", errorMessage)

				if herr := chain.New(endpoint.Kind).HandleError(errorCode, errorMessage.Error()); herr != nil {
					provider.MarkUnhealthy(herr)
					continue NextProvider
				}
			}
		}

		if req.IsBatch && len(res.Responses) != len(req.Requests) {
			reqLog.Error("batch response size mismatch", "request_size", len(req.Requests), "response_size", len(res.Responses))
			provider.MarkUnhealthy(fmt.Errorf("batch response size mismatch"))
			continue NextProvider
		}

		return &forwardResult{res: res, provider: provider}, nil
	}

	return nil, errNoProvidersAvailable
}

// coalesceRequest routes a single coalesceable request through the endpoint's
// singleflight group so identical concurrent requests share one upstream call,
// then writes the shared response to this client with its own JSON-RPC id.
func coalesceRequest(ctx context.Context, endpoint *core.Endpoint, req *rpc.BatchRequest, timing *servertiming.Header, log *slog.Logger, w http.ResponseWriter) {
	// id is excluded from the key: it varies per client. Params is raw JSON, so
	// only byte-identical params collapse (a safe, if occasionally conservative, key).
	key := req.Requests[0].Method + "\x00" + string(req.Requests[0].Params)

	ch := endpoint.Coalescer.DoChan(key, func() (any, error) {
		// Detached context: one client disconnecting must not cancel the shared
		// upstream call the other coalesced callers are waiting on. It stays bounded
		// by the per-provider timeout inside httpx, and preserves ctx values (XFF).
		return forwardWithFailover(context.WithoutCancel(ctx), endpoint, req, timing, log)
	})

	select {
	case <-ctx.Done():
		// This client left; the shared work continues for the others.
		return
	case sf := <-ch:
		if sf.Shared {
			metrics.RecordCoalescedRequest(endpoint.Name, req.Requests[0].Method)
			log.Debug("request coalesced", "method", req.Requests[0].Method, "params", rpc.FormatRawBody(string(req.Requests[0].Params)))
		}
		if sf.Err != nil {
			writeForwardError(w, sf.Err, log)
			return
		}

		shared := sf.Val.(*forwardResult)
		// Value-copy the shared response and stamp this client's id onto the copy;
		// concurrent callers must never mutate the shared *rpc.Response.
		resp := *shared.res.Responses[0]
		resp.ID = req.Requests[0].ID
		writeBatchResponse(w, endpoint, shared.provider, rpc.NewBatchResponse(&resp), log)
	}
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

// writeBatchResponse serializes res and writes it to the client, setting the
// X-Provider header (unless the endpoint is public) and content type.
func writeBatchResponse(w http.ResponseWriter, endpoint *core.Endpoint, provider *core.Provider, res *rpc.BatchResponse, log *slog.Logger) {
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
}

// writeMethodNotAllowed rejects a request that contains a method the chain does
// not allow, without contacting any provider. Each sub-request gets a JSON-RPC
// error: the offending method(s) report "method not allowed", and any other
// members of the same batch report that the batch was rejected.
func writeMethodNotAllowed(w http.ResponseWriter, c chain.Chain, req *rpc.BatchRequest, log *slog.Logger) {
	responses := make([]*rpc.Response, len(req.Requests))
	for i, sub := range req.Requests {
		msg := "method not allowed: " + sub.Method
		if c.AllowMethod(sub.Method) {
			msg = "request rejected: batch contains a method that is not allowed"
		}
		responses[i] = &rpc.Response{Version: "2.0", ID: sub.ID, Error: rpc.NewError(errCodeResourceUnavailable, msg)}
	}

	out, err := rpc.SerializeBatchResponse(&rpc.BatchResponse{Responses: responses, IsBatch: req.IsBatch})
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
