package ws

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	servertiming "github.com/mitchellh/go-server-timing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wille/haprovider/internal/core"
	"github.com/wille/haprovider/internal/rpc"
)

// newTestProxy builds a WebSocketProxy for ep the way IncomingWebsocketHandler
// does, with a context the test controls.
func newTestProxy(ep *core.Endpoint) (*WebSocketProxy, context.CancelCauseFunc) {
	ctx, cancel := context.WithCancelCause(context.Background())
	return &WebSocketProxy{
		log:       ep.Logger(),
		ctx:       ctx,
		cancel:    cancel,
		endpoint:  ep,
		Responses: make(chan *rpc.Response, 32),
		Requests:  make(chan *rpc.Request, 32),
	}, cancel
}

// TestDialAnyProvider_ClientGoneDuringFailedDial: when the client goes away
// while a provider dial is failing, DialAnyProvider returns the context error
// without blaming the provider. It used to call Close on the nil ProviderConn
// and panic.
func TestDialAnyProvider_ClientGoneDuringFailedDial(t *testing.T) {
	var cancel context.CancelCauseFunc
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cancel(context.Canceled)                      // the client disconnects mid-dial...
		w.WriteHeader(http.StatusInternalServerError) // ...and the dial fails
	}))
	t.Cleanup(up.Close)

	p := &core.Provider{Name: "p", Ws: wsScheme(up.URL)}
	ep := wsEndpoint(t, p)
	proxy, c := newTestProxy(ep)
	cancel = c

	got, err := proxy.DialAnyProvider(ep, &servertiming.Header{})

	assert.Nil(t, got)
	assert.ErrorIs(t, err, context.Canceled)
	assert.True(t, p.IsOnline(), "the client leaving is not the provider's fault")
}

// TestDialAnyProvider_ClientGoneDuringSuccessfulDial: when the client goes away
// just as a provider dial succeeds, the new provider connection is closed
// instead of leaked. The cancellation races the end of the handshake, so
// repeat it: whichever way each race goes, every upstream connection must end
// up closed.
func TestDialAnyProvider_ClientGoneDuringSuccessfulDial(t *testing.T) {
	for i := 0; i < 50; i++ {
		var cancel context.CancelCauseFunc
		closed := make(chan struct{})
		upgrader := websocket.Upgrader{}
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			cancel(context.Canceled) // the client disconnects as the dial completes
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					close(closed)
					return
				}
			}
		}))

		p := &core.Provider{Name: "p", Ws: wsScheme(up.URL)}
		ep := wsEndpoint(t, p)
		proxy, c := newTestProxy(ep)
		cancel = c

		got, err := proxy.DialAnyProvider(ep, &servertiming.Header{})
		if err == nil {
			// The dial finished before the cancellation was noticed, so the
			// connection is legitimately handed to the caller, which owns it.
			require.NotNil(t, got)
			proxy.ProviderConn.Close(websocket.CloseGoingAway, nil)
		}

		select {
		case <-closed:
		case <-time.After(2 * time.Second):
			t.Fatalf("iteration %d: provider connection leaked after the client left (err=%v)", i, err)
		}
		up.Close()
	}
}
