package ws

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// useTracker swaps in a fresh connection tracker for the test, so shutting it
// down doesn't affect other tests.
func useTracker(t *testing.T) *tracker {
	prev := connections
	connections = newTracker()
	t.Cleanup(func() { connections = prev })
	return connections
}

func TestTracker(t *testing.T) {
	tr := newTracker()

	require.True(t, tr.add())
	require.True(t, tr.add())
	tr.done()

	tr.closeAll()
	tr.closeAll() // idempotent
	assert.False(t, tr.add(), "no new connections once shutdown has begun")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	assert.ErrorIs(t, tr.wait(ctx), context.DeadlineExceeded, "one connection is still open")

	tr.done()
	assert.NoError(t, tr.wait(context.Background()))
}

func TestTracker_CloseAllWhenIdle(t *testing.T) {
	tr := newTracker()
	tr.closeAll()
	assert.NoError(t, tr.wait(context.Background()))
}

// TestIncomingWebsocketHandler_GracefulShutdown: CloseAll sends open clients a
// "going away" close frame, Wait returns once they're closed, and new upgrades
// are refused with a 503 instead of being opened and cut off.
func TestIncomingWebsocketHandler_GracefulShutdown(t *testing.T) {
	useTracker(t)

	p := wsProvider(t, "p", healthyWSMethods(), http.StatusSwitchingProtocols)
	ep := wsEndpoint(t, p)
	url := proxyURL(t, ep)

	c, _ := dial(t, url)
	sendRPC(t, c, "1", "eth_blockNumber")
	assert.Equal(t, "0x10", resultString(t, readRPC(t, c)))

	CloseAll()

	_, err := readUntilClose(c)
	var ce *websocket.CloseError
	if assert.ErrorAs(t, err, &ce) {
		assert.Equal(t, websocket.CloseGoingAway, ce.Code)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	assert.NoError(t, Wait(ctx), "Wait must return once every connection has closed")

	_, resp, err := websocket.DefaultDialer.Dial(url, nil)
	require.Error(t, err)
	if assert.NotNil(t, resp) {
		assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	}
}
