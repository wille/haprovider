package ws

import (
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/wille/haprovider/internal/core"
)

// streamingProvider is a WebSocket upstream that, once it receives any message,
// streams subscription notifications as fast as it can until the connection
// breaks, like a busy eth_subscribe("logs") feed.
func streamingProvider(t *testing.T) *core.Provider {
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		// Keep reading so gorilla answers the proxy's pings.
		go func() {
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()

		note := []byte(`{"jsonrpc":"2.0","method":"eth_subscription","params":{"subscription":"0x1","result":{}}}`)
		for {
			if err := conn.WriteMessage(websocket.TextMessage, note); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return &core.Provider{Name: "stream", Ws: wsScheme(srv.URL)}
}

// TestIncomingWebsocketHandler_NoLeakOnClientDisconnect: when a client drops
// while its provider is still streaming, every goroutine of the proxied
// connection exits. Previously the provider pump blocked forever handing
// notifications to a client pump that had already gone, pinning it and the
// provider read pump (and their buffers) for the life of the process.
func TestIncomingWebsocketHandler_NoLeakOnClientDisconnect(t *testing.T) {
	ep := wsEndpoint(t, streamingProvider(t))
	url := proxyURL(t, ep)

	baseline := runtime.NumGoroutine()

	for i := 0; i < 5; i++ {
		c, _, err := websocket.DefaultDialer.Dial(url, nil)
		require.NoError(t, err)
		require.NoError(t, c.WriteMessage(websocket.TextMessage, []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["logs"]}`)))
		_, _, err = c.ReadMessage()
		require.NoError(t, err)

		// Drop the connection without a close handshake, mid-stream.
		_ = c.NetConn().Close()
	}

	// Poll on this goroutine: assert.Eventually runs its condition in a
	// goroutine of its own, which would itself be counted.
	deadline := time.Now().Add(5 * time.Second)
	n := runtime.NumGoroutine()
	for n > baseline && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		n = runtime.NumGoroutine()
	}
	if n > baseline {
		buf := make([]byte, 1<<20)
		t.Fatalf("goroutines leaked: baseline %d, now %d\n%s", baseline, n, buf[:runtime.Stack(buf, true)])
	}
}
