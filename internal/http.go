package internal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"time"

	servertiming "github.com/mitchellh/go-server-timing"
	"github.com/wille/haprovider/internal/chain"
	"github.com/wille/haprovider/internal/core"
	"github.com/wille/haprovider/internal/httpx"
	"github.com/wille/haprovider/internal/metrics"
	"github.com/wille/haprovider/internal/rpc"
)

// coalesceIDCounter assigns a unique id to each coalesced upstream call so its
// "request" log line can be correlated to the group it served.
var coalesceIDCounter atomic.Uint64

func IncomingHttpRpcHandler(ctx context.Context, endpoint *core.Endpoint, w http.ResponseWriter, r *http.Request, timing *servertiming.Header) {
	defer metrics.TrackInflight(endpoint.Name, "http")()

	log := endpoint.Logger().With("ip", httpx.ClientIP(r), "transport", "http")

	if !strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		log.Error("http: close connection: invalid content type")
		writeRPCError(w, log, http.StatusBadRequest, rpc.CodeInvalidRequest, "invalid content type")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, rpc.MaxRequestBodySize)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			log.Error("http: request body too large", "limit", rpc.MaxRequestBodySize)
			writeRPCError(w, log, http.StatusRequestEntityTooLarge, rpc.CodeInvalidRequest, "request too large")
			return
		}

		log.Error("http: error reading body", "error", err)
		writeRPCError(w, log, http.StatusInternalServerError, rpc.CodeInternalError, "error reading body")
		return
	}

	req, err := rpc.DecodeBatchRequest(body)
	if errors.Is(err, rpc.ErrEmptyBatch) {
		log.Warn("http: empty batch request")
		writeRPCError(w, log, http.StatusBadRequest, rpc.CodeInvalidRequest, "empty batch")
		return
	}
	if err != nil {
		log.Error("http: bad request", "error", err, "body", rpc.FormatRawBody(string(body)))
		writeRPCError(w, log, http.StatusBadRequest, rpc.CodeParseError, "bad request")
		return
	}

	if len(req.Requests) > rpc.MaxBatchSize {
		log.Error("batch too large", "batch_size", len(req.Requests), "max", rpc.MaxBatchSize)
		writeRPCError(w, log, http.StatusRequestEntityTooLarge, rpc.CodeInvalidRequest, "batch too large")
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
	if rejected := chain.RejectDisallowed(c, req); rejected != nil {
		log.Warn("method not allowed", "methods", requestMethods(req))
		writeMethodNotAllowed(w, req, rejected, log)
		return
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
	logRequest(log, req, result, 0)
	writeBatchResponse(w, endpoint, result.provider, result.res, log)
}

// forwardResult is the outcome of a successful failover: the upstream response,
// the provider that served it, and how long it took. It is shared verbatim
// between coalesced callers, so callers must not mutate its response before
// writing. coalesceID is set only for a coalesced call, and is logged only by
// callers that actually shared it.
type forwardResult struct {
	res        *rpc.BatchResponse
	provider   *core.Provider
	dur        time.Duration
	coalesceID uint64
}

// errNoProvidersAvailable is returned by forwardWithFailover when every provider
// is offline or has failed the request.
var errNoProvidersAvailable = errors.New("no providers available")

// errRequestTooLarge is returned when every provider rejected the request as too
// large (HTTP 413). The request, not the provider, is the problem, so it maps to
// a 413 for the client and never marks a provider unhealthy.
var errRequestTooLarge = errors.New("request too large")

// noProviderForMethodError is returned by forwardWithFailover when every provider
// has reported it does not support one or more of the requested methods, so the
// request cannot be served. The client receives a JSON-RPC "method not found"
// error naming the offending method(s).
type noProviderForMethodError struct {
	methods []string
}

func (e *noProviderForMethodError) Error() string {
	return "no provider available for method " + strings.Join(e.methods, ", ")
}

// supportsAllMethods reports whether provider currently supports every method in
// req (none is on its unsupported list).
func supportsAllMethods(provider *core.Provider, req *rpc.BatchRequest) bool {
	for _, sub := range req.Requests {
		if !provider.SupportsMethod(sub.Method) {
			return false
		}
	}
	return true
}

// methodsWithoutProvider returns the requested methods that no provider supports,
// regardless of provider health. An empty result means the methods are supported
// somewhere and any failure was an availability problem, not a capability one.
func methodsWithoutProvider(endpoint *core.Endpoint, req *rpc.BatchRequest) []string {
	var missing []string
	seen := make(map[string]bool)
	for _, sub := range req.Requests {
		if seen[sub.Method] {
			continue
		}
		seen[sub.Method] = true

		supported := false
		for _, provider := range endpoint.Providers {
			if provider.SupportsMethod(sub.Method) {
				supported = true
				break
			}
		}
		if !supported {
			missing = append(missing, sub.Method)
		}
	}
	return missing
}

// forwardWithFailover runs the provider failover loop for req and returns the
// first successful response and the provider that produced it. It records
// per-request metrics and marks providers unhealthy on failure. It returns
// ctx.Err() if the context is cancelled mid-flight, or errNoProvidersAvailable
// when no provider could serve the request.
func forwardWithFailover(ctx context.Context, endpoint *core.Endpoint, req *rpc.BatchRequest, timing *servertiming.Header, log *slog.Logger) (*forwardResult, error) {
	// Set when at least one provider skipped or rejected a requested method as
	// unsupported, so exhaustion can report "no provider for method X" instead of
	// the generic "no providers available".
	methodUnsupported := false

	// Set when a provider rejected the request as too large (HTTP 413), so
	// exhaustion returns a 413 rather than "no providers available".
	requestTooLarge := false

	// The most recent response from a healthy provider that couldn't serve this
	// request (e.g. Solana slot/block not available here). If no provider can
	// serve it, we return this upstream error to the client rather than a 503.
	var lastTryNext *forwardResult

NextProvider:
	for _, provider := range endpoint.Providers {
		if !provider.IsOnline() {
			continue
		}

		// Skip a provider we already know doesn't support a requested method;
		// another provider may. If none do, we return a noProviderForMethodError.
		if !supportsAllMethods(provider, req) {
			methodUnsupported = true
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
			// The provider reported (even via a non-2xx status) that it doesn't
			// support this method: record it and fail over WITHOUT marking the
			// provider unhealthy. Only attributable for a single request; a batch
			// transport error falls through to the generic failure below.
			var mns *rpc.MethodNotSupportedError
			if errors.As(err, &mns) && len(req.Requests) == 1 {
				provider.MarkMethodUnsupported(req.Requests[0].Method)
				methodUnsupported = true
				metrics.RecordFailedRequest(endpoint.Name, provider.Name, "http", req.Requests[0].Method)
				continue NextProvider
			}

			// The provider rejected the request as too large (HTTP 413). That's a
			// property of the request, not the provider's health; another provider
			// may have a larger limit, so fail over WITHOUT marking it unhealthy. If
			// none accept it, the loop returns errRequestTooLarge.
			var se *httpx.StatusError
			if errors.As(err, &se) && se.Code == http.StatusRequestEntityTooLarge {
				log.Warn("provider rejected request as too large", "provider", provider.Name)
				requestTooLarge = true
				metrics.RecordFailedRequest(endpoint.Name, provider.Name, "http", "")
				continue NextProvider
			}

			// In a high traffic environment, the provider might be taken offline by another concurrent request
			if provider.IsOnline() {
				provider.MarkUnhealthy(err)
			}

			// Do not pass the failing method here as the whole request failed
			metrics.RecordFailedRequest(endpoint.Name, provider.Name, "http", "")

			continue NextProvider
		}

		// The per-request "request" debug log is emitted by the caller (via
		// logRequest) once the coalesce outcome is known, not here.
		reqLog := log.With("provider", provider.Name, "request_time", time.Since(start).Round(time.Millisecond).String())

		// Validate the response shape before anything indexes into it: a provider
		// that returns a different number of responses than we sent requests is
		// broken (and would otherwise index out of range below).
		if len(res.Responses) != len(req.Requests) {
			reqLog.Error("response size mismatch", "request_size", len(req.Requests), "response_size", len(res.Responses))
			provider.MarkUnhealthy(fmt.Errorf("response size mismatch: sent %d requests, got %d responses", len(req.Requests), len(res.Responses)))
			metrics.RecordFailedRequest(endpoint.Name, provider.Name, "http", "")
			continue NextProvider
		}
		// Answer in the shape the client asked for, even if the provider wrapped a
		// single response in an array.
		res.IsBatch = req.IsBatch

		methods := responseMethods(req, res)

		// Record one metric per (sub-)request, labeled by method, like the WS path.
		elapsed := time.Since(start).Seconds()
		for i, sub := range res.Responses {
			method := methods[i]
			if sub.IsError() {
				metrics.RecordFailedRequest(endpoint.Name, provider.Name, "http", method)
			} else {
				// A provider answered it, so it's a real method: label it by name.
				metrics.AdmitMethod(method)
				metrics.RecordRequest(endpoint.Name, provider.Name, "http", method, elapsed)
			}
		}

		// An in-band "method not supported" error means skip this provider for that
		// method and fail over, without marking it unhealthy. Scan every sub-request
		// first so all unsupported methods in a batch are recorded before moving on.
		methodNotSupported := false
		for i, sub := range res.Responses {
			if !sub.IsError() {
				continue
			}

			errorCode, errorMessage := sub.GetError()
			reqLog.Debug("error response", "error_code", errorCode, "error_message", errorMessage)

			if herr := chain.New(endpoint.Kind).HandleError(errorCode, errorMessage.Error()); herr != nil {
				// The chain reports the method isn't supported (the standard -32601,
				// or a provider variant like QuickNode's -32604/-32004): record it and
				// skip-not-lose to another provider.
				var mns *rpc.MethodNotSupportedError
				if errors.As(herr, &mns) {
					provider.MarkMethodUnsupported(methods[i])
					methodUnsupported = true
					methodNotSupported = true
					continue
				}

				// The provider is healthy but can't serve this specific request;
				// try another provider without marking it unhealthy or caching, and
				// remember the response in case no provider can serve it.
				var tryNext *rpc.ErrorTryNextProvider
				if errors.As(herr, &tryNext) {
					reqLog.Debug("provider cannot serve request, trying next", "error_code", errorCode, "method", methods[i])
					lastTryNext = &forwardResult{res: res, provider: provider, dur: time.Since(start)}
					continue NextProvider
				}

				provider.MarkUnhealthy(herr)
				continue NextProvider
			}
		}
		if methodNotSupported {
			continue NextProvider
		}

		return &forwardResult{res: res, provider: provider, dur: time.Since(start)}, nil
	}

	if methodUnsupported {
		if missing := methodsWithoutProvider(endpoint, req); len(missing) > 0 {
			return nil, &noProviderForMethodError{methods: missing}
		}
	}

	if requestTooLarge {
		return nil, errRequestTooLarge
	}

	// No provider could serve the request, but at least one is healthy and
	// returned a real error (e.g. the block is unavailable everywhere): return
	// that upstream error to the client rather than a generic 503.
	if lastTryNext != nil {
		return lastTryNext, nil
	}

	return nil, errNoProvidersAvailable
}

// responseMethods returns, for each response in res, the method of the request
// it answers. JSON-RPC lets a batch response arrive in any order, so responses
// are matched to requests by id; a response whose id is missing, unknown or
// shared by several requests falls back to its position. The caller has already
// checked that res and req have the same length.
func responseMethods(req *rpc.BatchRequest, res *rpc.BatchResponse) []string {
	methods := make([]string, len(res.Responses))
	if !req.IsBatch {
		methods[0] = req.Requests[0].Method
		return methods
	}

	byID := make(map[string]string, len(req.Requests))
	dup := make(map[string]bool)
	for _, sub := range req.Requests {
		id := sub.GetID()
		if _, seen := byID[id]; seen {
			dup[id] = true
		}
		byID[id] = sub.Method
	}

	for i, sub := range res.Responses {
		id := sub.GetID()
		if m, ok := byID[id]; ok && id != "" && !dup[id] {
			methods[i] = m
		} else {
			methods[i] = req.Requests[i].Method
		}
	}
	return methods
}

// logRequest emits the per-request debug log for a completed forward. A non-zero
// coalesceID means the request was served by a shared (coalesced) upstream call
// and the id identifies that group.
func logRequest(log *slog.Logger, req *rpc.BatchRequest, result *forwardResult, coalesceID uint64) {
	l := log.With("provider", result.provider.Name, "request_time", result.dur.Round(time.Millisecond).String())
	if coalesceID != 0 {
		l = l.With("coalesced", coalesceID)
	}
	if req.IsBatch {
		l.Debug("batch request", "size", len(req.Requests))
	} else {
		l.Debug("request", "method", req.Requests[0].Method)
	}
}

// coalesceRequest routes a single coalesceable request through the endpoint's
// singleflight group so identical concurrent requests share one upstream call,
// then writes the shared response to this client with its own JSON-RPC id.
func coalesceRequest(ctx context.Context, endpoint *core.Endpoint, req *rpc.BatchRequest, timing *servertiming.Header, log *slog.Logger, w http.ResponseWriter) {
	// id is excluded from the key: it varies per client. Params is raw JSON, so
	// only byte-identical params collapse (a safe, if occasionally conservative, key).
	key := req.Requests[0].Method + "\x00" + string(req.Requests[0].Params)

	ch := endpoint.Coalescer.DoChan(key, func() (_ any, err error) {
		// singleflight re-raises a panic from a DoChan callback on a fresh
		// goroutine, which nothing can recover and which kills the process. Turn
		// it into an error so a bug here fails these requests, not the proxy.
		defer func() {
			if p := recover(); p != nil {
				log.Error("panic in coalesced request", "panic", p, "stack", string(debug.Stack()))
				err = fmt.Errorf("panic in coalesced request: %v", p)
			}
		}()

		// Detached context: one client disconnecting must not cancel the shared
		// upstream call the other coalesced callers are waiting on. It stays bounded
		// by the per-provider timeout inside httpx, and preserves ctx values (XFF).
		result, err := forwardWithFailover(context.WithoutCancel(ctx), endpoint, req, timing, log)
		if err != nil {
			return nil, err
		}
		// Assign a coalesce id up front; it is only logged by callers that turn out
		// to actually share this call (sf.Shared).
		result.coalesceID = coalesceIDCounter.Add(1)
		return result, nil
	})

	select {
	case <-ctx.Done():
		// This client left; the shared work continues for the others.
		return
	case sf := <-ch:
		if sf.Err != nil {
			writeForwardError(w, sf.Err, log)
			return
		}

		shared := sf.Val.(*forwardResult)

		// The coalesce id is logged only when this request was genuinely shared
		// with other callers.
		var coalesceID uint64
		if sf.Shared {
			coalesceID = shared.coalesceID
			metrics.RecordCoalescedRequest(endpoint.Name, req.Requests[0].Method)
		}
		logRequest(log, req, shared, coalesceID)
		// Value-copy the shared response and stamp this client's id onto the copy;
		// concurrent callers must never mutate the shared *rpc.Response.
		resp := *shared.res.Responses[0]
		resp.ID = req.Requests[0].ID
		writeBatchResponse(w, endpoint, shared.provider, rpc.NewBatchResponse(&resp), log)
	}
}

// writeRPCError writes a JSON-RPC error response (id null) with the given HTTP
// status, so a client of an RPC endpoint always gets a structured error rather
// than a plain-text body.
func writeRPCError(w http.ResponseWriter, log *slog.Logger, status, code int, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	resp := &rpc.Response{Version: "2.0", ID: nil, Error: rpc.NewError(code, message)}
	if err := rpc.WriteResponse(w, resp); err != nil {
		log.Error("error writing body", "error", err)
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
		writeRPCError(w, log, http.StatusServiceUnavailable, rpc.CodeNoProvidersAvailable, "no providers available")
		return
	}

	// Every provider rejected the request as too large: the request is the
	// problem, not the providers, so return a 413 (and none were marked unhealthy).
	if errors.Is(err, errRequestTooLarge) {
		log.Warn("request too large for all providers")
		writeRPCError(w, log, http.StatusRequestEntityTooLarge, rpc.CodeInvalidRequest, "request too large")
		return
	}

	// No provider supports the requested method: 501 with a JSON-RPC "method not
	// found" error naming it. The method is unavailable, not transiently down, so
	// this is distinct from the 503 above.
	var nom *noProviderForMethodError
	if errors.As(err, &nom) {
		log.Error("no provider available for method", "methods", nom.methods)
		writeRPCError(w, log, http.StatusNotImplemented, rpc.CodeMethodNotFound, err.Error())
		return
	}

	log.Error("request failed", "error", err)
	writeRPCError(w, log, http.StatusInternalServerError, rpc.CodeInternalError, "request failed")
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
		writeRPCError(w, log, http.StatusInternalServerError, rpc.CodeInternalError, "error serializing response")
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	if _, err := w.Write(out); err != nil {
		log.Error("error writing body", "error", err)
	}
}

// requestMethods lists the methods in req, for logging.
func requestMethods(req *rpc.BatchRequest) []string {
	methods := make([]string, len(req.Requests))
	for i, sub := range req.Requests {
		methods[i] = sub.Method
	}
	return methods
}

// writeMethodNotAllowed writes the rejection responses built by
// chain.RejectDisallowed, without contacting any provider.
func writeMethodNotAllowed(w http.ResponseWriter, req *rpc.BatchRequest, responses []*rpc.Response, log *slog.Logger) {
	out, err := rpc.SerializeBatchResponse(&rpc.BatchResponse{Responses: responses, IsBatch: req.IsBatch})
	if err != nil {
		log.Error("error serializing response", "error", err)
		writeRPCError(w, log, http.StatusInternalServerError, rpc.CodeInternalError, "error serializing response")
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

	log := endpoint.Logger().With("ip", httpx.ClientIP(r), "transport", "http", "path", path)

	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !allowedHTTPAPIPath(path) {
		log.Warn("http api path not allowed")
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	// The path from the router is already unescaped: escape it again so a
	// decoded "?" or "#" can't inject a query or fragment into the upstream URL.
	upstreamPath := escapePath(path)

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

	// The most recent 5xx from a provider, returned to the client if no provider
	// can serve the request, so it sees the real upstream error rather than a
	// generic 503.
	var lastServerError *httpx.Response
	var lastServerErrorProvider *core.Provider

	for _, provider := range endpoint.Providers {
		if !provider.IsOnline() {
			continue
		}

		t := strings.TrimSuffix(provider.Http, "/jsonrpc")

		target := t + "/" + upstreamPath
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

		// A 5xx means the provider is broken, not that the request is bad: mark it
		// unhealthy and fail over, keeping the response in case none can serve it.
		if res.StatusCode >= 500 {
			if provider.IsOnline() {
				provider.MarkUnhealthy(&httpx.StatusError{Code: res.StatusCode})
			}
			metrics.RecordFailedRequest(endpoint.Name, provider.Name, "http", path)
			log.Error("provider returned server error", "provider", provider.Name, "status", res.StatusCode)
			lastServerError, lastServerErrorProvider = res, provider
			continue
		}

		// A 2xx means the provider recognized the path: label it by name. Unknown
		// paths (404s) stay "other", so clients can't mint arbitrary labels.
		if res.StatusCode >= 200 && res.StatusCode < 300 {
			metrics.AdmitMethod(path)
		}
		metrics.RecordRequest(endpoint.Name, provider.Name, "http", path, time.Since(start).Seconds())

		writeHTTPAPIResponse(w, endpoint, provider, res, log)
		return
	}

	if lastServerError != nil {
		writeHTTPAPIResponse(w, endpoint, lastServerErrorProvider, lastServerError, log)
		return
	}

	log.Error("no providers available")
	http.Error(w, "no providers available", http.StatusServiceUnavailable)
}

// writeHTTPAPIResponse writes an upstream HTTP API response to the client
// verbatim, adding X-Provider unless the endpoint is public.
func writeHTTPAPIResponse(w http.ResponseWriter, endpoint *core.Endpoint, provider *core.Provider, res *httpx.Response, log *slog.Logger) {
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
}

// httpAPIPrefixes are the TRON full-node HTTP API path prefixes we forward:
// the full-node, solidity and PBFT wallet APIs. Anything else (node admin or
// monitoring endpoints, arbitrary paths) is not proxied.
var httpAPIPrefixes = []string{"wallet/", "walletsolidity/", "walletpbft/"}

// allowedHTTPAPIPath reports whether path (unescaped, without a leading slash)
// is a TRON HTTP API path we forward. Dot and empty segments are rejected so a
// path like "wallet/..%2F..%2Fadmin" can't climb out of the allowed prefix once
// the upstream resolves it.
func allowedHTTPAPIPath(path string) bool {
	for _, s := range strings.Split(path, "/") {
		if s == "" || s == "." || s == ".." {
			return false
		}
	}
	for _, prefix := range httpAPIPrefixes {
		if strings.HasPrefix(path, prefix) && len(path) > len(prefix) {
			return true
		}
	}
	return false
}

// escapePath escapes each segment of an unescaped slash-separated path, so it
// can be appended to a URL without any character being read as a query,
// fragment or other delimiter.
func escapePath(path string) string {
	segments := strings.Split(path, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	return strings.Join(segments, "/")
}
