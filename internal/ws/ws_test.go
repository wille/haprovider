package ws

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	servertiming "github.com/mitchellh/go-server-timing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wille/haprovider/internal/core"
	"github.com/wille/haprovider/internal/rpc"
	"github.com/wille/haprovider/internal/rpctest"
)

// wsScheme rewrites an http(s):// test server URL to ws(s)://.
func wsScheme(httpURL string) string {
	return "ws" + strings.TrimPrefix(httpURL, "http")
}

func healthyWSMethods() map[string]any {
	return map[string]any{
		"web3_clientVersion": "geth/test",
		"eth_chainId":        "0x1",
		"eth_blockNumber":    "0x10",
	}
}

// wsProvider creates a provider backed by a mock WebSocket upstream returning
// methods. statusOnUpgrade controls the upstream handshake status (use
// http.StatusSwitchingProtocols for a normal upgrade, http.StatusTooManyRequests
// to simulate a 429 on the handshake).
func wsProvider(t *testing.T, name string, methods map[string]any, statusOnUpgrade int) *core.Provider {
	up := rpctest.NewWSServer(methods, statusOnUpgrade)
	t.Cleanup(up.Close)
	return &core.Provider{Name: name, Ws: wsScheme(up.URL)}
}

// wsEndpoint wires the given providers into an eth endpoint and marks them online.
func wsEndpoint(t *testing.T, providers ...*core.Provider) *core.Endpoint {
	ep := &core.Endpoint{Name: "test", ChainID: "1", Kind: core.KindEth, Providers: providers}
	for _, p := range providers {
		p.Endpoint = ep
		p.MarkHealthy(0)
	}
	return ep
}

// proxyURL fronts IncomingWebsocketHandler with an httptest server and returns
// its ws:// URL.
func proxyURL(t *testing.T, ep *core.Endpoint) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		IncomingWebsocketHandler(r.Context(), ep, w, r, &servertiming.Header{})
	}))
	t.Cleanup(srv.Close)
	return wsScheme(srv.URL)
}

// dial connects to the proxy as a websocket client with a read deadline so a
// missing message surfaces as a failure rather than a hang.
func dial(t *testing.T, url string) (*websocket.Conn, *http.Response) {
	c, resp, err := websocket.DefaultDialer.Dial(url, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	return c, resp
}

func sendRPC(t *testing.T, c *websocket.Conn, id, method string) {
	data, err := rpc.SerializeRequest(rpc.NewRequest(id, method, nil))
	require.NoError(t, err)
	require.NoError(t, c.WriteMessage(websocket.TextMessage, data))
}

func readRPC(t *testing.T, c *websocket.Conn) *rpc.Response {
	_, msg, err := c.ReadMessage()
	require.NoError(t, err)
	res, err := rpc.DecodeResponse(msg)
	require.NoError(t, err)
	return res
}

// resultString decodes a response's string result (Result is raw JSON).
func resultString(t *testing.T, res *rpc.Response) string {
	v, err := rpc.DecodeResult[string](res)
	require.NoError(t, err)
	return *v
}

// readUntilClose drains data frames until the peer closes, returning the close
// error and any data messages seen first.
func readUntilClose(c *websocket.Conn) ([][]byte, error) {
	var msgs [][]byte
	for {
		_, msg, err := c.ReadMessage()
		if err != nil {
			return msgs, err
		}
		msgs = append(msgs, msg)
	}
}

func offline(t *testing.T, p *core.Provider) {
	assert.Eventually(t, func() bool { return !p.IsOnline() }, time.Second, 10*time.Millisecond,
		"provider %s should be marked offline", p.Name)
}

// TestIncomingWebsocketHandler_Success: a client request is proxied to the
// upstream and the response (with the echoed id) comes back; the provider stays
// online and X-Provider is set on the handshake response.
func TestIncomingWebsocketHandler_Success(t *testing.T) {
	p := wsProvider(t, "p", healthyWSMethods(), http.StatusSwitchingProtocols)
	ep := wsEndpoint(t, p)

	c, resp := dial(t, proxyURL(t, ep))
	assert.Equal(t, "p", resp.Header.Get("X-Provider"), "X-Provider should be set on the handshake response")

	sendRPC(t, c, "1", "eth_blockNumber")
	res := readRPC(t, c)

	assert.Equal(t, "1", res.GetID())
	assert.Equal(t, "0x10", resultString(t, res))
	assert.False(t, res.IsError())
	assert.True(t, p.IsOnline())
}

// TestIncomingWebsocketHandler_RPCRateLimit: an in-band -32005 from the upstream
// marks the provider offline + rate-limited and closes the connection with
// CloseServiceRestart.
func TestIncomingWebsocketHandler_RPCRateLimit(t *testing.T) {
	p := wsProvider(t, "p", map[string]any{
		"eth_blockNumber": rpctest.RPCError{Code: -32005, Message: "rate limit exceeded"},
	}, http.StatusSwitchingProtocols)
	ep := wsEndpoint(t, p)

	c, _ := dial(t, proxyURL(t, ep))
	sendRPC(t, c, "1", "eth_blockNumber")

	_, err := readUntilClose(c)
	assert.True(t, websocket.IsCloseError(err, websocket.CloseServiceRestart), "got: %v", err)

	offline(t, p)
	assert.Eventually(t, func() bool { return !p.GetNextCheckTime().IsZero() }, time.Second, 10*time.Millisecond,
		"a rate-limit window should be set")
}

// TestIncomingWebsocketHandler_InternalError: an in-band -32603 marks the
// provider offline (no rate-limit window) and closes the client connection.
func TestIncomingWebsocketHandler_InternalError(t *testing.T) {
	p := wsProvider(t, "p", map[string]any{
		"eth_blockNumber": rpctest.RPCError{Code: -32603, Message: "boom"},
	}, http.StatusSwitchingProtocols)
	ep := wsEndpoint(t, p)

	c, _ := dial(t, proxyURL(t, ep))
	sendRPC(t, c, "1", "eth_blockNumber")

	_, err := readUntilClose(c)
	assert.True(t, websocket.IsCloseError(err, websocket.CloseServiceRestart), "got: %v", err)

	offline(t, p)
	assert.True(t, p.GetNextCheckTime().IsZero(), "internal error must not set a rate-limit window")
}

// TestIncomingWebsocketHandler_DialFailover: when the first provider rejects the
// handshake, the proxy fails over to the next healthy provider and serves the
// client through it.
func TestIncomingWebsocketHandler_DialFailover(t *testing.T) {
	// The first provider rejects the handshake with a 429, so the proxy should
	// fail over to the second (healthy) provider and rate-limit the first.
	bad := wsProvider(t, "bad", healthyWSMethods(), http.StatusTooManyRequests)
	good := wsProvider(t, "good", healthyWSMethods(), http.StatusSwitchingProtocols)
	ep := wsEndpoint(t, bad, good)

	c, resp := dial(t, proxyURL(t, ep))
	assert.Equal(t, "good", resp.Header.Get("X-Provider"), "should fail over to the healthy provider")

	sendRPC(t, c, "1", "eth_blockNumber")
	res := readRPC(t, c)
	assert.Equal(t, "0x10", resultString(t, res))

	offline(t, bad)
	assert.False(t, bad.GetNextCheckTime().IsZero(), "a 429 handshake should set a rate-limit window")
	assert.True(t, good.IsOnline())
}

// TestIncomingWebsocketHandler_NoProviders: with no usable providers the
// handshake is rejected with 503 before the upgrade.
func TestIncomingWebsocketHandler_NoProviders(t *testing.T) {
	p := wsProvider(t, "p", healthyWSMethods(), http.StatusSwitchingProtocols)
	ep := wsEndpoint(t, p)
	p.MarkUnhealthy(assert.AnError) // take the only provider offline

	_, resp, err := websocket.DefaultDialer.Dial(proxyURL(t, ep), nil)
	assert.ErrorIs(t, err, websocket.ErrBadHandshake)
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

// TestIncomingWebsocketHandler_DialRateLimitWindow: a 429 on the WebSocket dial
// handshake records a rate-limit window honoring the upstream's Retry-After.
func TestIncomingWebsocketHandler_DialRateLimitWindow(t *testing.T) {
	// The mock sends Retry-After: 5 alongside the 429 handshake.
	p := wsProvider(t, "p", healthyWSMethods(), http.StatusTooManyRequests)
	ep := wsEndpoint(t, p)

	// The only provider is rate-limited, so the dial is rejected with 503.
	_, resp, err := websocket.DefaultDialer.Dial(proxyURL(t, ep), nil)
	assert.ErrorIs(t, err, websocket.ErrBadHandshake)
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)

	offline(t, p)

	// The Retry-After: 5 header must be honored (~now+5s), not the 30s default.
	rl := p.GetNextCheckTime()
	assert.False(t, rl.IsZero(), "a rate-limit window should be set")
	assert.True(t, rl.After(time.Now().Add(3*time.Second)), "should honor Retry-After ~5s")
	assert.True(t, rl.Before(time.Now().Add(8*time.Second)), "should not fall back to the 30s default")
}

// TestIncomingWebsocketHandler_BatchSplit: a batch request is split into
// individual upstream requests; the client receives one response per item.
func TestIncomingWebsocketHandler_BatchSplit(t *testing.T) {
	p := wsProvider(t, "p", healthyWSMethods(), http.StatusSwitchingProtocols)
	ep := wsEndpoint(t, p)

	c, _ := dial(t, proxyURL(t, ep))

	batch := `[{"jsonrpc":"2.0","id":"1","method":"eth_blockNumber","params":[]},` +
		`{"jsonrpc":"2.0","id":"2","method":"eth_chainId","params":[]}]`
	require.NoError(t, c.WriteMessage(websocket.TextMessage, []byte(batch)))

	got := map[string]string{}
	for i := 0; i < 2; i++ {
		res := readRPC(t, c)
		got[res.GetID()] = resultString(t, res)
	}
	assert.Equal(t, map[string]string{"1": "0x10", "2": "0x1"}, got)
}

// TestIncomingWebsocketHandler_BadRequestsDropped: a client that floods invalid
// requests is dropped with CloseUnsupportedData.
func TestIncomingWebsocketHandler_BadRequestsDropped(t *testing.T) {
	p := wsProvider(t, "p", healthyWSMethods(), http.StatusSwitchingProtocols)
	ep := wsEndpoint(t, p)

	c, _ := dial(t, proxyURL(t, ep))

	// >10 consecutive bad requests trips the drop guard (ws.go badRequests > 10).
	for i := 0; i < 15; i++ {
		if err := c.WriteMessage(websocket.TextMessage, []byte("not json")); err != nil {
			break
		}
	}

	// The client must be dropped. When we get a clean close frame it should carry
	// CloseUnsupportedData; under load the frame can race the connection teardown,
	// so a non-close read error still confirms the drop.
	_, err := readUntilClose(c)
	require.Error(t, err)
	if ce, ok := err.(*websocket.CloseError); ok {
		assert.Equal(t, websocket.CloseUnsupportedData, ce.Code, "unexpected close code")
	}
}

// TestIncomingWebsocketHandler_ForwardedForUpstream: with AddXForwardedHeaders
// the client's forwarded chain is sent upstream on the dial handshake.
func TestIncomingWebsocketHandler_ForwardedForUpstream(t *testing.T) {
	gotXFF := make(chan string, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case gotXFF <- r.Header.Get("X-Forwarded-For"):
		default:
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(up.Close)

	ep := &core.Endpoint{Name: "test", ChainID: "1", Kind: core.KindEth, AddXForwardedHeaders: true}
	p := &core.Provider{Name: "p", Ws: wsScheme(up.URL), Endpoint: ep}
	ep.Providers = []*core.Provider{p}
	p.MarkHealthy(0)

	c, _, err := websocket.DefaultDialer.Dial(proxyURL(t, ep), http.Header{"X-Forwarded-For": {"10.1.2.3"}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	select {
	case xff := <-gotXFF:
		assert.True(t, strings.HasPrefix(xff, "10.1.2.3,"),
			"upstream handshake should carry the forwarded chain, got %q", xff)
	case <-time.After(2 * time.Second):
		t.Fatal("upstream never received the handshake")
	}
}

// TestIncomingWebsocketHandler_UpstreamResponseTooLarge: an upstream message
// larger than the endpoint's max_response_size tears down the connection rather
// than buffering unbounded data.
func TestIncomingWebsocketHandler_UpstreamResponseTooLarge(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
			// Reply with a frame far larger than the endpoint's cap.
			_ = conn.WriteMessage(websocket.TextMessage, []byte(strings.Repeat("a", 50_000)))
		}
	}))
	t.Cleanup(up.Close)

	limit := int64(1000)
	ep := &core.Endpoint{Name: "test", ChainID: "1", Kind: core.KindEth, MaxResponseSize: &limit}
	p := &core.Provider{Name: "p", Ws: wsScheme(up.URL), Endpoint: ep}
	ep.Providers = []*core.Provider{p}
	p.MarkHealthy(0)

	c, _ := dial(t, proxyURL(t, ep))
	sendRPC(t, c, "1", "eth_blockNumber")

	_, err := readUntilClose(c)
	assert.True(t, websocket.IsCloseError(err, websocket.CloseTryAgainLater), "got: %v", err)
}

// TestClient_CloseConcurrent verifies Close is idempotent and never blocks when
// called repeatedly and concurrently from multiple goroutines, even though the
// writePump consumes only a single close frame. Run under -race, it also covers
// the concurrent writes to the closed flag. Before the fix, the second send on
// the unbuffered close channel would block forever and leak the goroutine.
func TestClient_CloseConcurrent(t *testing.T) {
	// A real connected websocket pair so NewClient's read/write pumps run.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)

	conn, _, err := websocket.DefaultDialer.Dial(wsScheme(srv.URL), nil)
	require.NoError(t, err)
	client := NewClient(conn)

	const n = 20
	done := make(chan struct{}, n)
	for i := 0; i < n; i++ {
		go func() {
			client.Close(websocket.CloseNormalClosure, nil)
			done <- struct{}{}
		}()
	}
	for i := 0; i < n; i++ {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("Close blocked: concurrent/repeated Close is not safe")
		}
	}
}
