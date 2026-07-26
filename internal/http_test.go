package internal

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	servertiming "github.com/mitchellh/go-server-timing"
	"github.com/stretchr/testify/assert"
	"github.com/wille/haprovider/internal/core"
	"github.com/wille/haprovider/internal/rpctest"
)

// handlerEndpoint builds an eth endpoint with a single online provider backed by
// a mock upstream returning the given methods.
func handlerEndpoint(t *testing.T, methods map[string]any) (*core.Endpoint, *core.Provider) {
	srv := rpctest.NewServer(methods, nil)
	t.Cleanup(srv.Close)

	ep := &core.Endpoint{Name: "test", ChainID: "1", Kind: core.KindEth}
	p := &core.Provider{Name: "p", Http: srv.URL, Endpoint: ep}
	ep.Providers = []*core.Provider{p}
	p.MarkHealthy(0)
	return ep, p
}

// twoProviderEndpoint builds an eth endpoint with two online providers "a" and
// "b", each backed by a mock upstream returning the given methods.
func twoProviderEndpoint(t *testing.T, methodsA, methodsB map[string]any) (*core.Endpoint, *core.Provider, *core.Provider) {
	srvA := rpctest.NewServer(methodsA, nil)
	srvB := rpctest.NewServer(methodsB, nil)
	t.Cleanup(srvA.Close)
	t.Cleanup(srvB.Close)

	ep := &core.Endpoint{Name: "test", ChainID: "1", Kind: core.KindEth}
	a := &core.Provider{Name: "a", Http: srvA.URL, Endpoint: ep}
	b := &core.Provider{Name: "b", Http: srvB.URL, Endpoint: ep}
	ep.Providers = []*core.Provider{a, b}
	a.MarkHealthy(0)
	b.MarkHealthy(0)
	return ep, a, b
}

// rawStatusProvider returns an httptest server that always answers with status
// and body, plus its cleanup registered on t.
func rawStatusProvider(t *testing.T, status int, body string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// twoRawEndpoint builds an eth endpoint with two online providers, each backed by
// a raw upstream that always returns the given status and body.
func twoRawEndpoint(t *testing.T, status int, body string) (*core.Endpoint, *core.Provider, *core.Provider) {
	srvA := rawStatusProvider(t, status, body)
	srvB := rawStatusProvider(t, status, body)

	ep := &core.Endpoint{Name: "test", ChainID: "1", Kind: core.KindEth}
	a := &core.Provider{Name: "a", Http: srvA.URL, Endpoint: ep}
	b := &core.Provider{Name: "b", Http: srvB.URL, Endpoint: ep}
	ep.Providers = []*core.Provider{a, b}
	a.MarkHealthy(0)
	b.MarkHealthy(0)
	return ep, a, b
}

func doRPC(endpoint *core.Endpoint, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	IncomingHttpRpcHandler(context.Background(), endpoint, w, r, &servertiming.Header{})
	return w
}

const blockNumberReq = `{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`

const getLogsReq = `{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{}]}`

// TestMethodNotSupported_SkipNotLose: an in-band "method not supported" (-32604)
// from provider A makes the request fail over to B without marking A unhealthy;
// A just gets flagged as not supporting that method.
func TestMethodNotSupported_SkipNotLose(t *testing.T) {
	endpoint, a, b := twoProviderEndpoint(t,
		map[string]any{"eth_getLogs": rpctest.RPCError{Code: -32604, Message: "this request method is not supported"}},
		map[string]any{"eth_getLogs": []any{"0xlog"}},
	)

	w := doRPC(endpoint, getLogsReq)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "0xlog")
	assert.Equal(t, "b", w.Header().Get("X-Provider"), "should be served by the provider that supports the method")
	assert.True(t, a.IsOnline(), "provider A must not be lost over an unsupported method")
	assert.True(t, a.RateLimitedUntil.IsZero(), "an unsupported method must not set a rate-limit window")
	assert.False(t, a.SupportsMethod("eth_getLogs"), "A should be flagged as not supporting the method")
	assert.True(t, b.SupportsMethod("eth_getLogs"))
}

// TestMethodNotSupported_NoProviderSupports: when every provider reports the
// method unsupported, the client gets a 501 naming the method and no provider is
// marked unhealthy.
func TestMethodNotSupported_NoProviderSupports(t *testing.T) {
	notSupported := map[string]any{"eth_getLogs": rpctest.RPCError{Code: -32604, Message: "this request method is not supported"}}
	endpoint, a, b := twoProviderEndpoint(t, notSupported, notSupported)

	w := doRPC(endpoint, getLogsReq)

	assert.Equal(t, http.StatusNotImplemented, w.Code)
	assert.Contains(t, w.Body.String(), "no provider available for method eth_getLogs")
	assert.True(t, a.IsOnline(), "providers must stay online when a method is simply unsupported")
	assert.True(t, b.IsOnline())
}

// TestMethodNotSupported_Transport400: a provider that reports the method as an
// HTTP 400 with a -32604 body (QuickNode's shape) is skipped, not lost, and the
// request fails over to a healthy provider.
func TestMethodNotSupported_Transport400(t *testing.T) {
	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32604,"message":"this request method is not supported"}}`))
	}))
	t.Cleanup(srvA.Close)
	srvB := rpctest.NewServer(map[string]any{"eth_getLogs": []any{"0xlog"}}, nil)
	t.Cleanup(srvB.Close)

	ep := &core.Endpoint{Name: "test", ChainID: "1", Kind: core.KindEth}
	a := &core.Provider{Name: "a", Http: srvA.URL, Endpoint: ep}
	b := &core.Provider{Name: "b", Http: srvB.URL, Endpoint: ep}
	ep.Providers = []*core.Provider{a, b}
	a.MarkHealthy(0)
	b.MarkHealthy(0)

	w := doRPC(ep, getLogsReq)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "0xlog")
	assert.Equal(t, "b", w.Header().Get("X-Provider"))
	assert.True(t, a.IsOnline(), "an HTTP 400 method-not-supported must not lose the provider")
	assert.True(t, a.RateLimitedUntil.IsZero())
	assert.False(t, a.SupportsMethod("eth_getLogs"))
}

// TestClientError_ReturnedNotUnhealthy: a request-level error (invalid params)
// returned as HTTP 400 with a JSON-RPC body is handed back to the client, not
// treated as a provider failure. A single bad request must not knock providers
// offline or fail over across all of them.
func TestClientError_ReturnedNotUnhealthy(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"invalid params"}}`
	ep, a, b := twoRawEndpoint(t, http.StatusBadRequest, body)

	w := doRPC(ep, getLogsReq)

	assert.Equal(t, http.StatusOK, w.Code, "a client error is returned as a normal JSON-RPC error")
	assert.Contains(t, w.Body.String(), "invalid params")
	assert.Contains(t, w.Body.String(), "-32602")
	assert.Equal(t, "a", w.Header().Get("X-Provider"), "served by the first provider, no failover")
	assert.True(t, a.IsOnline(), "a client error must not mark the provider unhealthy")
	assert.True(t, b.IsOnline())
	assert.True(t, a.RateLimitedUntil.IsZero())
}

// TestRequestTooLarge_NotUnhealthy: an HTTP 413 from every provider returns a 413
// to the client and leaves all providers online — the request is too big, the
// providers are fine.
func TestRequestTooLarge_NotUnhealthy(t *testing.T) {
	ep, a, b := twoRawEndpoint(t, http.StatusRequestEntityTooLarge, "request entity too large")

	w := doRPC(ep, getLogsReq)

	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
	assert.True(t, a.IsOnline(), "a 413 must not mark the provider unhealthy")
	assert.True(t, b.IsOnline())
	assert.True(t, a.RateLimitedUntil.IsZero())
	assert.True(t, b.RateLimitedUntil.IsZero())
}

// TestRequestTooLarge_FailsOver: a provider with a smaller limit (413) is skipped,
// not lost, and the request succeeds on a provider that accepts it.
func TestRequestTooLarge_FailsOver(t *testing.T) {
	srvA := rawStatusProvider(t, http.StatusRequestEntityTooLarge, "request entity too large")
	srvB := rpctest.NewServer(map[string]any{"eth_getLogs": []any{"0xlog"}}, nil)
	t.Cleanup(srvB.Close)

	ep := &core.Endpoint{Name: "test", ChainID: "1", Kind: core.KindEth}
	a := &core.Provider{Name: "a", Http: srvA.URL, Endpoint: ep}
	b := &core.Provider{Name: "b", Http: srvB.URL, Endpoint: ep}
	ep.Providers = []*core.Provider{a, b}
	a.MarkHealthy(0)
	b.MarkHealthy(0)

	w := doRPC(ep, getLogsReq)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "0xlog")
	assert.Equal(t, "b", w.Header().Get("X-Provider"))
	assert.True(t, a.IsOnline(), "a 413 must not lose the provider")
}

// TestMethodNotSupported_Status501: a provider reporting the method as HTTP 501
// with no usable body is skipped (not lost) and the request fails over.
func TestMethodNotSupported_Status501(t *testing.T) {
	srvA := rawStatusProvider(t, http.StatusNotImplemented, "Not Implemented")
	srvB := rpctest.NewServer(map[string]any{"eth_getLogs": []any{"0xlog"}}, nil)
	t.Cleanup(srvB.Close)

	ep := &core.Endpoint{Name: "test", ChainID: "1", Kind: core.KindEth}
	a := &core.Provider{Name: "a", Http: srvA.URL, Endpoint: ep}
	b := &core.Provider{Name: "b", Http: srvB.URL, Endpoint: ep}
	ep.Providers = []*core.Provider{a, b}
	a.MarkHealthy(0)
	b.MarkHealthy(0)

	w := doRPC(ep, getLogsReq)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "0xlog")
	assert.Equal(t, "b", w.Header().Get("X-Provider"))
	assert.True(t, a.IsOnline(), "a 501 must not lose the provider")
	assert.False(t, a.SupportsMethod("eth_getLogs"))
}

// TestPaymentRequired_Backoff: an HTTP 402 (quota exhausted) backs the provider
// off with a rate-limit window and fails over, rather than failing hard.
func TestPaymentRequired_Backoff(t *testing.T) {
	srvA := rawStatusProvider(t, http.StatusPaymentRequired, "quota exceeded")
	srvB := rpctest.NewServer(map[string]any{"eth_getLogs": []any{"0xlog"}}, nil)
	t.Cleanup(srvB.Close)

	ep := &core.Endpoint{Name: "test", ChainID: "1", Kind: core.KindEth}
	a := &core.Provider{Name: "a", Http: srvA.URL, Endpoint: ep}
	b := &core.Provider{Name: "b", Http: srvB.URL, Endpoint: ep}
	ep.Providers = []*core.Provider{a, b}
	a.MarkHealthy(0)
	b.MarkHealthy(0)

	w := doRPC(ep, getLogsReq)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "b", w.Header().Get("X-Provider"))
	assert.False(t, a.IsOnline(), "402 should take the provider offline")
	assert.False(t, a.RateLimitedUntil.IsZero(), "402 should set a backoff window")
}

// TestIncomingHttpRpcHandler_RPCRateLimit: a 200 response carrying an in-band
// -32005 rate-limit error marks the provider offline + rate-limited and fails
// over. With only one provider, that leaves none available (503).
func TestIncomingHttpRpcHandler_RPCRateLimit(t *testing.T) {
	endpoint, p := handlerEndpoint(t, map[string]any{
		"eth_blockNumber": rpctest.RPCError{Code: -32005, Message: "rate limit exceeded"},
	})

	w := doRPC(endpoint, blockNumberReq)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "no providers available")
	assert.False(t, p.IsOnline(), "provider should be marked offline on a rate-limit error")
	assert.False(t, p.RateLimitedUntil.IsZero(), "a rate-limit window should be set")
}

// TestIncomingHttpRpcHandler_InternalError: an in-band -32603 marks the provider
// offline (no rate-limit window) and fails over → 503 with a single provider.
func TestIncomingHttpRpcHandler_InternalError(t *testing.T) {
	endpoint, p := handlerEndpoint(t, map[string]any{
		"eth_blockNumber": rpctest.RPCError{Code: -32603, Message: "boom"},
	})

	w := doRPC(endpoint, blockNumberReq)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.False(t, p.IsOnline(), "provider should be marked offline on an internal error")
	assert.True(t, p.RateLimitedUntil.IsZero(), "internal error must not set a rate-limit window")
}

// TestIncomingHttpRpcHandler_Success: happy path forwards the result and sets
// the X-Provider header, leaving the provider online.
func TestIncomingHttpRpcHandler_Success(t *testing.T) {
	endpoint, p := handlerEndpoint(t, map[string]any{"eth_blockNumber": "0x10"})

	w := doRPC(endpoint, blockNumberReq)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "0x10")
	assert.Equal(t, "p", w.Header().Get("X-Provider"))
	assert.True(t, p.IsOnline())
}

// xffUpstream returns a raw upstream server that records the X-Forwarded-For
// header it receives, plus an eth endpoint pointing at it.
func xffUpstream(t *testing.T, addXFF bool) (*core.Endpoint, *string) {
	got := new(string)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*got = r.Header.Get("X-Forwarded-For")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x10"}`))
	}))
	t.Cleanup(srv.Close)

	ep := &core.Endpoint{Name: "test", ChainID: "1", Kind: core.KindEth, AddXForwardedHeaders: addXFF}
	p := &core.Provider{Name: "p", Http: srv.URL, Endpoint: ep}
	ep.Providers = []*core.Provider{p}
	p.MarkHealthy(0)
	return ep, got
}

// TestIncomingHttpRpcHandler_ForwardedForUpstream: with AddXForwardedHeaders the
// client IP is forwarded to the upstream provider (not the client response).
func TestIncomingHttpRpcHandler_ForwardedForUpstream(t *testing.T) {
	ep, gotXFF := xffUpstream(t, true)

	r := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(blockNumberReq))
	r.Header.Set("Content-Type", "application/json")
	r.RemoteAddr = "203.0.113.7:1234"
	w := httptest.NewRecorder()
	IncomingHttpRpcHandler(context.Background(), ep, w, r, &servertiming.Header{})

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "203.0.113.7", *gotXFF, "upstream should receive the client IP as X-Forwarded-For")
	assert.Empty(t, w.Header().Get("X-Forwarded-For"), "X-Forwarded-For must not leak into the client response")
}

// TestIncomingHttpRpcHandler_NoForwardedFor: without the flag no X-Forwarded-For
// is sent upstream.
func TestIncomingHttpRpcHandler_NoForwardedFor(t *testing.T) {
	ep, gotXFF := xffUpstream(t, false)

	r := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(blockNumberReq))
	r.Header.Set("Content-Type", "application/json")
	r.RemoteAddr = "203.0.113.7:1234"
	IncomingHttpRpcHandler(context.Background(), ep, httptest.NewRecorder(), r, &servertiming.Header{})

	assert.Empty(t, *gotXFF)
}
