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

func doRPC(endpoint *core.Endpoint, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	IncomingHttpRpcHandler(context.Background(), endpoint, w, r, &servertiming.Header{})
	return w
}

const blockNumberReq = `{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`

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
