package internal

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	servertiming "github.com/mitchellh/go-server-timing"
	"github.com/stretchr/testify/assert"
	"github.com/wille/haprovider/internal/core"
	"github.com/wille/haprovider/internal/rpc"
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

// assertRPCError decodes w's body as a JSON-RPC response and asserts it carries
// an error with the given code — i.e. the proxy returned a structured JSON-RPC
// error, not a plain-text body.
func assertRPCError(t *testing.T, w *httptest.ResponseRecorder, code int) {
	t.Helper()
	res, err := rpc.DecodeResponse(w.Body.Bytes())
	assert.NoError(t, err, "body should be a JSON-RPC response, got %q", w.Body.String())
	if assert.True(t, res.IsError(), "response should be an error") {
		got, _ := res.GetError()
		assert.Equal(t, code, got)
	}
}

// TestRPCErrorEnvelopes: request-level failures the proxy detects itself are
// returned as JSON-RPC error envelopes, not plain text.
func TestRPCErrorEnvelopes(t *testing.T) {
	endpoint, _ := handlerEndpoint(t, map[string]any{"eth_blockNumber": "0x10"})

	post := func(contentType, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(body))
		r.Header.Set("Content-Type", contentType)
		w := httptest.NewRecorder()
		IncomingHttpRpcHandler(context.Background(), endpoint, w, r, &servertiming.Header{})
		return w
	}

	// Invalid content type -> Invalid Request.
	w := post("text/plain", blockNumberReq)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assertRPCError(t, w, rpc.CodeInvalidRequest)

	// Unparseable body -> Parse error.
	w = post("application/json", "{not json")
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assertRPCError(t, w, rpc.CodeParseError)
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

// TestMethodNotSupported_ChainSpecificCode: a provider-specific not-supported
// code mapped by the chain's HandleError (QuickNode's -32004 on EVM) is treated
// like the shared codes — skip-not-lose to another provider, no unhealthy.
func TestMethodNotSupported_ChainSpecificCode(t *testing.T) {
	endpoint, a, _ := twoProviderEndpoint(t,
		map[string]any{"eth_getLogs": rpctest.RPCError{Code: -32004, Message: "the method does not exist/is not supported"}},
		map[string]any{"eth_getLogs": []any{"0xlog"}},
	)

	w := doRPC(endpoint, getLogsReq)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "0xlog")
	assert.Equal(t, "b", w.Header().Get("X-Provider"))
	assert.True(t, a.IsOnline(), "provider A must not be lost over an unsupported method")
	assert.True(t, a.RateLimitedUntil.IsZero())
	assert.False(t, a.SupportsMethod("eth_getLogs"), "A should be flagged as not supporting the method")
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

// solanaTwoProviders builds a Solana endpoint with two online providers backed by
// the given method maps.
func solanaTwoProviders(t *testing.T, methodsA, methodsB map[string]any) (*core.Endpoint, *core.Provider, *core.Provider) {
	srvA := rpctest.NewServer(methodsA, nil)
	srvB := rpctest.NewServer(methodsB, nil)
	t.Cleanup(srvA.Close)
	t.Cleanup(srvB.Close)

	ep := &core.Endpoint{Name: "test", Kind: core.KindSolana}
	a := &core.Provider{Name: "a", Http: srvA.URL, Endpoint: ep}
	b := &core.Provider{Name: "b", Http: srvB.URL, Endpoint: ep}
	ep.Providers = []*core.Provider{a, b}
	a.MarkHealthy(0)
	b.MarkHealthy(0)
	return ep, a, b
}

const getBlockReq = `{"jsonrpc":"2.0","id":1,"method":"getBlock","params":[123]}`

// TestTryNextProvider_FailsOver: a Solana -32009 (slot missing in long-term
// storage) from provider A fails over to a provider that has the block, without
// marking A unhealthy or caching the method as unsupported.
func TestTryNextProvider_FailsOver(t *testing.T) {
	ep, a, _ := solanaTwoProviders(t,
		map[string]any{"getBlock": rpctest.RPCError{Code: -32009, Message: "Slot 123 was skipped"}},
		map[string]any{"getBlock": map[string]any{"blockhash": "abc"}},
	)

	w := doRPC(ep, getBlockReq)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "blockhash")
	assert.Equal(t, "b", w.Header().Get("X-Provider"))
	assert.True(t, a.IsOnline(), "a data-availability miss must not mark the provider unhealthy")
	assert.True(t, a.RateLimitedUntil.IsZero())
	assert.True(t, a.SupportsMethod("getBlock"), "must not be cached as unsupported")
}

// TestTryNextProvider_AllUnavailable: when no provider has the block, the upstream
// error is returned to the client (not a 503), and providers stay online.
func TestTryNextProvider_AllUnavailable(t *testing.T) {
	unavailable := map[string]any{"getBlock": rpctest.RPCError{Code: -32009, Message: "Slot 123 was skipped"}}
	ep, a, b := solanaTwoProviders(t, unavailable, unavailable)

	w := doRPC(ep, getBlockReq)

	assert.Equal(t, http.StatusOK, w.Code, "the upstream error is returned as a JSON-RPC error")
	assert.Contains(t, w.Body.String(), "-32009")
	assert.Contains(t, w.Body.String(), "was skipped")
	assert.True(t, a.IsOnline(), "data unavailability must not mark providers unhealthy")
	assert.True(t, b.IsOnline())
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

// TestEmptyBatch_NotUnhealthy: an empty batch is rejected as an Invalid Request
// without contacting any provider. It used to fail to serialize upstream, which
// the failover loop blamed on each provider in turn, so one "[]" request took
// every provider offline.
func TestEmptyBatch_NotUnhealthy(t *testing.T) {
	endpoint, a, b := twoProviderEndpoint(t, map[string]any{"eth_blockNumber": "0x10"}, map[string]any{"eth_blockNumber": "0x10"})

	w := doRPC(endpoint, `[]`)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assertRPCError(t, w, rpc.CodeInvalidRequest)
	assert.True(t, a.IsOnline(), "provider a must stay online")
	assert.True(t, b.IsOnline(), "provider b must stay online")
}

// rawThenMockEndpoint builds an eth endpoint whose first provider "a" always
// answers with the given raw body and whose second provider "b" is a normal mock.
func rawThenMockEndpoint(t *testing.T, rawBody string, methodsB map[string]any) (*core.Endpoint, *core.Provider, *core.Provider) {
	srvA := rawStatusProvider(t, http.StatusOK, rawBody)
	srvB := rpctest.NewServer(methodsB, nil)
	t.Cleanup(srvB.Close)

	ep := &core.Endpoint{Name: "test", ChainID: "1", Kind: core.KindEth}
	a := &core.Provider{Name: "a", Http: srvA.URL, Endpoint: ep}
	b := &core.Provider{Name: "b", Http: srvB.URL, Endpoint: ep}
	ep.Providers = []*core.Provider{a, b}
	a.MarkHealthy(0)
	b.MarkHealthy(0)
	return ep, a, b
}

// TestResponseSizeMismatch_FailsOver: a provider answering a single request with
// more responses than requested is treated as broken and failed over, rather
// than indexing out of range. Covers both the plain path and the coalesced path,
// where the panic used to escape singleflight.DoChan and kill the process.
func TestResponseSizeMismatch_FailsOver(t *testing.T) {
	const twoResponses = `[{"jsonrpc":"2.0","id":1,"result":"0x1"},{"jsonrpc":"2.0","id":2,"result":"0x2"}]`

	for name, body := range map[string]string{
		"plain":     `{"jsonrpc":"2.0","id":1,"method":"eth_getProof","params":["0x0",[],"latest"]}`,
		"coalesced": `{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x0","latest"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			endpoint, a, b := rawThenMockEndpoint(t, twoResponses, map[string]any{
				"eth_getProof":   "0xproof",
				"eth_getBalance": "0x10",
			})

			w := doRPC(endpoint, body)

			assert.Equal(t, http.StatusOK, w.Code)
			assert.Equal(t, "b", w.Header().Get("X-Provider"))
			assert.False(t, a.IsOnline(), "a returned a malformed response and should be marked unhealthy")
			assert.True(t, b.IsOnline())
		})
	}
}

// TestSingleResponseInArray_Unwrapped: a provider that wraps the answer to a
// single request in a one-element array is fine, but the client gets back the
// single object it asked for.
func TestSingleResponseInArray_Unwrapped(t *testing.T) {
	endpoint, _, _ := rawThenMockEndpoint(t, `[{"jsonrpc":"2.0","id":1,"result":"0xproof"}]`, nil)

	// A non-coalesceable method: the coalesced path rebuilds its response anyway.
	w := doRPC(endpoint, `{"jsonrpc":"2.0","id":1,"method":"eth_getProof","params":["0x0",[],"latest"]}`)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.JSONEq(t, `{"jsonrpc":"2.0","id":1,"result":"0xproof"}`, w.Body.String())
}

// TestBatchOutOfOrder_AttributedByID: batch responses may come back in any
// order, so a "method not supported" error is attributed to the request with
// the matching id, not the request at the same position.
func TestBatchOutOfOrder_AttributedByID(t *testing.T) {
	endpoint, a, _ := rawThenMockEndpoint(t,
		`[{"jsonrpc":"2.0","id":2,"error":{"code":-32601,"message":"the method eth_getLogs does not exist"}},{"jsonrpc":"2.0","id":1,"result":"0x10"}]`,
		map[string]any{"eth_blockNumber": "0x10", "eth_getLogs": []any{}},
	)

	w := doRPC(endpoint, `[{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]},{"jsonrpc":"2.0","id":2,"method":"eth_getLogs","params":[{}]}]`)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "b", w.Header().Get("X-Provider"))
	assert.False(t, a.SupportsMethod("eth_getLogs"), "the error answered id 2, eth_getLogs")
	assert.True(t, a.SupportsMethod("eth_blockNumber"), "eth_blockNumber must not be blamed for eth_getLogs' error")
}

// tronAPIEndpoint builds a tron endpoint whose providers are the given handlers,
// in order, all online.
func tronAPIEndpoint(t *testing.T, handlers ...http.HandlerFunc) (*core.Endpoint, []*core.Provider) {
	ep := &core.Endpoint{Name: "tron", ChainID: "mainnet", Kind: core.KindTron}
	var providers []*core.Provider
	for i, h := range handlers {
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		p := &core.Provider{Name: string(rune('a' + i)), Http: srv.URL + "/jsonrpc", Endpoint: ep}
		p.MarkHealthy(0)
		providers = append(providers, p)
	}
	ep.Providers = providers
	return ep, providers
}

// doAPI sends a request to the TRON HTTP API passthrough. target is the request
// URI as a client would send it (escaped), e.g. "/tron/wallet/getnowblock".
// It goes through a real ServeMux so the {path...} wildcard is decoded exactly
// as in production.
func doAPI(endpoint *core.Endpoint, method, target string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	mux.HandleFunc("/{id}/{path...}", func(w http.ResponseWriter, r *http.Request) {
		IncomingHTTPAPIHandler(r.Context(), endpoint, r.PathValue("path"), w, r, &servertiming.Header{})
	})
	r := httptest.NewRequest(method, target, strings.NewReader(`{}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

// TestHTTPAPI_EscapesPath: a decoded "?" in the path stays part of the path
// upstream instead of injecting a query string.
func TestHTTPAPI_EscapesPath(t *testing.T) {
	got := make(chan *http.Request, 1)
	endpoint, _ := tronAPIEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		got <- r
		_, _ = w.Write([]byte(`{}`))
	})

	w := doAPI(endpoint, http.MethodPost, "/tron/wallet/getnowblock%3Fvisible=true")

	assert.Equal(t, http.StatusOK, w.Code)
	r := <-got
	assert.Equal(t, "/wallet/getnowblock?visible=true", r.URL.Path, "the ? must stay in the path")
	assert.Empty(t, r.URL.RawQuery, "no query may be injected")
}

// TestHTTPAPI_PreservesQuery: a real query string is still forwarded.
func TestHTTPAPI_PreservesQuery(t *testing.T) {
	got := make(chan *http.Request, 1)
	endpoint, _ := tronAPIEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		got <- r
		_, _ = w.Write([]byte(`{}`))
	})

	w := doAPI(endpoint, http.MethodGet, "/tron/wallet/getnowblock?visible=true")

	assert.Equal(t, http.StatusOK, w.Code)
	r := <-got
	assert.Equal(t, "/wallet/getnowblock", r.URL.Path)
	assert.Equal(t, "visible=true", r.URL.RawQuery)
}

// TestHTTPAPI_Restricted: only the wallet API prefixes and GET/POST are
// forwarded; nothing else reaches a provider.
func TestHTTPAPI_Restricted(t *testing.T) {
	endpoint, _ := tronAPIEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("request must not reach the provider: %s %s", r.Method, r.URL.Path)
	})

	for _, tc := range []struct {
		method, target string
		status         int
	}{
		{http.MethodPost, "/tron/monitor/getstatsinfo", http.StatusNotFound},
		{http.MethodPost, "/tron/wallet", http.StatusNotFound},
		{http.MethodPost, "/tron/wallet/", http.StatusNotFound},
		{http.MethodPost, "/tron/wallet/x%2F..%2F..%2Fmonitor%2Fgetstatsinfo", http.StatusNotFound},
		{http.MethodDelete, "/tron/wallet/getnowblock", http.StatusMethodNotAllowed},
	} {
		w := doAPI(endpoint, tc.method, tc.target)
		assert.Equal(t, tc.status, w.Code, "%s %s", tc.method, tc.target)
	}
}

// TestHTTPAPI_ServerErrorFailsOver: a 5xx marks the provider unhealthy and
// fails over; when every provider fails, the client gets the last upstream
// error rather than a generic 503.
func TestHTTPAPI_ServerErrorFailsOver(t *testing.T) {
	broken := func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"Error":"boom"}`, http.StatusInternalServerError)
	}

	endpoint, ps := tronAPIEndpoint(t, broken, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"blockID":"x"}`))
	})
	w := doAPI(endpoint, http.MethodPost, "/tron/wallet/getnowblock")
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "b", w.Header().Get("X-Provider"))
	assert.False(t, ps[0].IsOnline(), "a returned 500 and should be marked unhealthy")

	endpoint, _ = tronAPIEndpoint(t, broken, broken)
	w = doAPI(endpoint, http.MethodPost, "/tron/wallet/getnowblock")
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "boom")
}

// TestHTTPAPI_PaymentRequiredBacksOff: a 402 (quota exhausted) is a rate limit,
// as on the JSON-RPC path: the provider backs off and the request fails over.
func TestHTTPAPI_PaymentRequiredBacksOff(t *testing.T) {
	endpoint, ps := tronAPIEndpoint(t,
		func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusPaymentRequired) },
		func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{}`)) },
	)

	w := doAPI(endpoint, http.MethodPost, "/tron/wallet/getnowblock")

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "b", w.Header().Get("X-Provider"))
	assert.False(t, ps[0].IsOnline())
	assert.False(t, ps[0].GetNextCheckTime().IsZero(), "a should be backed off until its rate limit passes")
}

// TestRedirect_FailsOver: a provider that redirects is a failing provider: the
// request fails over to the next one, the redirecting provider is marked
// unhealthy, and the host it redirects to is never contacted.
func TestRedirect_FailsOver(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0xdeadbeef"}`))
	}))
	t.Cleanup(target.Close)
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirecting.Close)
	good := rpctest.NewServer(map[string]any{"eth_blockNumber": "0x10"}, nil)
	t.Cleanup(good.Close)

	ep := &core.Endpoint{Name: "test", ChainID: "1", Kind: core.KindEth}
	a := &core.Provider{Name: "a", Http: redirecting.URL, Endpoint: ep}
	b := &core.Provider{Name: "b", Http: good.URL, Endpoint: ep}
	ep.Providers = []*core.Provider{a, b}
	a.MarkHealthy(0)
	b.MarkHealthy(0)

	w := doRPC(ep, blockNumberReq)

	res, err := rpc.DecodeResponse(w.Body.Bytes())
	if assert.NoError(t, err) {
		assert.JSONEq(t, `"0x10"`, string(res.Result))
	}
	assert.Equal(t, "b", w.Header().Get("X-Provider"))
	assert.False(t, a.IsOnline(), "the redirecting provider is marked unhealthy")
	assert.Zero(t, hits.Load(), "the redirect target is never contacted")
}

// TestHTTPAPI_RedirectFailsOver: on the TRON HTTP API passthrough a redirect
// is a provider failure too, rather than a response passed to the client.
func TestHTTPAPI_RedirectFailsOver(t *testing.T) {
	endpoint, providers := tronAPIEndpoint(t,
		func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://example.com/elsewhere", http.StatusFound)
		},
		func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"ok":true}`))
		},
	)

	w := doAPI(endpoint, http.MethodPost, "/tron/wallet/getnowblock")

	assert.Equal(t, http.StatusOK, w.Code)
	assert.JSONEq(t, `{"ok":true}`, w.Body.String())
	assert.Equal(t, "b", w.Header().Get("X-Provider"))
	assert.False(t, providers[0].IsOnline())
}

// TestResponseIDMismatch_FailsOver: an answer with an id the client didn't
// send is someone else's answer. The request fails over and the provider that
// crossed wires is marked unhealthy.
func TestResponseIDMismatch_FailsOver(t *testing.T) {
	endpoint, a, b := rawThenMockEndpoint(t,
		`{"jsonrpc":"2.0","id":999,"result":"0xdead"}`,
		map[string]any{"eth_protocolVersion": "0x41"},
	)

	w := doRPC(endpoint, `{"jsonrpc":"2.0","id":1,"method":"eth_protocolVersion","params":[]}`)

	res, err := rpc.DecodeResponse(w.Body.Bytes())
	if assert.NoError(t, err) {
		assert.JSONEq(t, `"0x41"`, string(res.Result))
	}
	assert.Equal(t, "b", w.Header().Get("X-Provider"))
	assert.False(t, a.IsOnline(), "the provider that crossed wires is marked unhealthy")
	assert.True(t, b.IsOnline())
}

// TestResponseIDMismatch_Coalesced: a coalesced response gets the client's own
// id written over the provider's, so the provider's id is checked before that
// can hide a crossed wire.
func TestResponseIDMismatch_Coalesced(t *testing.T) {
	endpoint, a, _ := rawThenMockEndpoint(t,
		`{"jsonrpc":"2.0","id":999,"result":"0xde0b6b3a7640000"}`,
		map[string]any{"eth_getBalance": "0x0"},
	)

	w := doRPC(endpoint, `{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xabc","latest"]}`)

	res, err := rpc.DecodeResponse(w.Body.Bytes())
	if assert.NoError(t, err) {
		assert.JSONEq(t, `"0x0"`, string(res.Result), "the balance comes from the provider that answered this request")
		assert.Equal(t, "1", res.GetID())
	}
	assert.False(t, a.IsOnline())
}

// TestResponseIDMismatch_Batch: a batch answer that answers one request twice
// (and so another not at all) fails over.
func TestResponseIDMismatch_Batch(t *testing.T) {
	endpoint, a, _ := rawThenMockEndpoint(t,
		`[{"jsonrpc":"2.0","id":1,"result":"0x1"},{"jsonrpc":"2.0","id":1,"result":"0x1"}]`,
		map[string]any{"eth_protocolVersion": "0x41", "net_version": "1"},
	)

	w := doRPC(endpoint, `[
		{"jsonrpc":"2.0","id":1,"method":"eth_protocolVersion","params":[]},
		{"jsonrpc":"2.0","id":2,"method":"net_version","params":[]}
	]`)

	res, err := rpc.DecodeBatchResponse(w.Body.Bytes())
	if assert.NoError(t, err) && assert.Len(t, res.Responses, 2) {
		assert.JSONEq(t, `"0x41"`, string(res.GetResponseByID("1").Result))
		assert.JSONEq(t, `"1"`, string(res.GetResponseByID("2").Result))
	}
	assert.False(t, a.IsOnline())
}

// TestResponseIDNullError_Accepted: JSON-RPC lets an error answer have a null
// id when the server couldn't read the request's. That's still the provider's
// answer to this request; it isn't treated as a crossed wire, so a client
// can't take providers out of rotation with a request they can't parse.
func TestResponseIDNullError_Accepted(t *testing.T) {
	endpoint, a, b := rawThenMockEndpoint(t,
		`{"jsonrpc":"2.0","id":null,"error":{"code":-32602,"message":"invalid params"}}`,
		map[string]any{"eth_protocolVersion": "0x41"},
	)

	w := doRPC(endpoint, `{"jsonrpc":"2.0","id":1,"method":"eth_protocolVersion","params":[]}`)

	assertRPCError(t, w, -32602)
	assert.Equal(t, "a", w.Header().Get("X-Provider"))
	assert.True(t, a.IsOnline())
	assert.True(t, b.IsOnline())
}
