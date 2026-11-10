package ws

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/wille/haprovider/internal/core"
)

// fastHealthChecks makes connections check their provider every few
// milliseconds and leave it after grace, for the duration of the test. The
// test gets its own connection tracker, and its connections are closed and
// waited for before the settings are restored, so no connection handler is
// still reading them.
func fastHealthChecks(t *testing.T, grace time.Duration) {
	interval, period := ProviderHealthCheckInterval, UnhealthyProviderGracePeriod
	ProviderHealthCheckInterval, UnhealthyProviderGracePeriod = 10*time.Millisecond, grace
	t.Cleanup(func() {
		ProviderHealthCheckInterval, UnhealthyProviderGracePeriod = interval, period
	})

	tr := useTracker(t)
	t.Cleanup(func() {
		tr.closeAll()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tr.wait(ctx); err != nil {
			t.Errorf("connections still open: %v", err)
		}
	})
}

func TestLeaveUnhealthyProvider(t *testing.T) {
	fastHealthChecks(t, 20*time.Millisecond)

	newEndpoint := func() (*core.Endpoint, *core.Provider, *core.Provider) {
		a := &core.Provider{Name: "a", Ws: "ws://a"}
		b := &core.Provider{Name: "b", Ws: "ws://b"}
		return wsEndpoint(t, a, b), a, b
	}

	ep, a, _ := newEndpoint()
	assert.False(t, leaveUnhealthyProvider(ep, a), "a healthy provider is kept")

	a.MarkUnhealthy(errors.New("node is behind"))
	assert.False(t, leaveUnhealthyProvider(ep, a), "not during the grace period")

	time.Sleep(30 * time.Millisecond)
	assert.True(t, leaveUnhealthyProvider(ep, a), "unhealthy past the grace period, and b is healthy")

	ep, a, b := newEndpoint()
	a.MarkUnhealthy(errors.New("node is behind"))
	b.MarkUnhealthy(errors.New("node is behind"))
	time.Sleep(30 * time.Millisecond)
	assert.False(t, leaveUnhealthyProvider(ep, a), "no healthy provider to move to")

	ep, a, b = newEndpoint()
	b.Ws = ""
	a.MarkUnhealthy(errors.New("node is behind"))
	time.Sleep(30 * time.Millisecond)
	assert.False(t, leaveUnhealthyProvider(ep, a), "b has no WebSocket URL to move to")
}

// TestIncomingWebsocketHandler_ProviderUnhealthy: a connection whose provider
// has been taken out of rotation, and stays out, is closed with
// CloseServiceRestart so the client reconnects to a healthy provider.
func TestIncomingWebsocketHandler_ProviderUnhealthy(t *testing.T) {
	fastHealthChecks(t, 50*time.Millisecond)

	a := wsProvider(t, "a", healthyWSMethods(), http.StatusSwitchingProtocols)
	b := wsProvider(t, "b", healthyWSMethods(), http.StatusSwitchingProtocols)
	ep := wsEndpoint(t, a, b)

	c, resp := dial(t, proxyURL(t, ep))
	assert.Equal(t, "a", resp.Header.Get("X-Provider"))

	a.MarkUnhealthy(errors.New("node is stalled"))

	_, err := readUntilClose(c)
	var ce *websocket.CloseError
	if assert.ErrorAs(t, err, &ce) {
		assert.Equal(t, websocket.CloseServiceRestart, ce.Code)
		assert.Contains(t, ce.Text, "provider unhealthy")
	}

	// Reconnecting lands on the healthy provider.
	_, resp = dial(t, proxyURL(t, ep))
	assert.Equal(t, "b", resp.Header.Get("X-Provider"))
}

// TestIncomingWebsocketHandler_ProviderUnhealthyGrace: a provider that's only
// briefly out of rotation doesn't cost its clients their connection.
func TestIncomingWebsocketHandler_ProviderUnhealthyGrace(t *testing.T) {
	fastHealthChecks(t, time.Hour)

	a := wsProvider(t, "a", healthyWSMethods(), http.StatusSwitchingProtocols)
	b := wsProvider(t, "b", healthyWSMethods(), http.StatusSwitchingProtocols)
	ep := wsEndpoint(t, a, b)

	c, _ := dial(t, proxyURL(t, ep))
	a.MarkUnhealthy(errors.New("timeout"))
	time.Sleep(100 * time.Millisecond) // many health checks

	sendRPC(t, c, "1", "eth_blockNumber")
	assert.Equal(t, "0x10", resultString(t, readRPC(t, c)), "the connection is still open")
}

// TestIncomingWebsocketHandler_ProviderUnhealthyNoAlternative: with no healthy
// provider to move to, the connection is kept rather than cut.
func TestIncomingWebsocketHandler_ProviderUnhealthyNoAlternative(t *testing.T) {
	fastHealthChecks(t, 20*time.Millisecond)

	a := wsProvider(t, "a", healthyWSMethods(), http.StatusSwitchingProtocols)
	ep := wsEndpoint(t, a)

	c, _ := dial(t, proxyURL(t, ep))
	a.MarkUnhealthy(errors.New("node is behind"))
	time.Sleep(100 * time.Millisecond) // past the grace period, many health checks

	sendRPC(t, c, "1", "eth_blockNumber")
	assert.Equal(t, "0x10", resultString(t, readRPC(t, c)), "the connection is still open")
}
