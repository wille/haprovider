package core_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	servertiming "github.com/mitchellh/go-server-timing"
	"github.com/stretchr/testify/assert"
	"github.com/wille/haprovider/internal"
	"github.com/wille/haprovider/internal/core"
	"github.com/wille/haprovider/internal/healthcheck"
	"github.com/wille/haprovider/internal/rpctest"
)

// ethBlockReq is a minimal single eth_blockNumber JSON-RPC request body.
const ethBlockReq = `{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`

// proxyHTTP drives the HTTP RPC handler against an endpoint and returns the
// recorded response (the handler replaced the old ProxyHTTP helper).
func proxyHTTP(endpoint *core.Endpoint, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	internal.IncomingHttpRpcHandler(context.Background(), endpoint, w, r, &servertiming.Header{})
	return w
}

// TestInvalidChainID tests connecting to a provider with invalid chainID
func TestInvalidChainID(t *testing.T) {
	// Setup a test server that returns a different chainID (0x2 -> 2) than expected
	server := rpctest.NewServer(map[string]any{
		"web3_clientVersion": "geth/test",
		"eth_chainId":        "0x2",
		"eth_blockNumber":    "0x10",
		"eth_syncing":        false,
	}, nil)
	defer server.Close()

	// Create a provider expecting chainID 1
	provider := &core.Endpoint{
		ChainID: "1",
		Kind:    "eth",
		Providers: []*core.Provider{
			{
				Name: "test-invalid-chain",
				Http: server.URL,
			},
		},
	}

	// Initialize the endpoint status
	provider.Providers[0].Endpoint = provider
	err := healthcheck.Run(context.Background(), provider.Providers[0])

	// Verify the endpoint was marked as offline due to chainID mismatch
	assert.Error(t, err)
	assert.False(t, provider.Providers[0].IsOnline())
	assert.Contains(t, err.Error(), "chainId mismatch")

	// Verify no active endpoints are returned
	for _, p := range provider.Providers {
		assert.False(t, p.IsOnline())
	}
}

// TestRateLimitWithRetryAfter tests connecting to a provider that returns a 429 rate limit with retry-after header
func TestRateLimitWithRetryAfter(t *testing.T) {
	// Setup a test server that returns a 429 with a Retry-After header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "5") // Retry after 5 seconds
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32005,"message":"rate limit exceeded"}}`))
	}))
	defer server.Close()

	// Create a provider to test GetActiveEndpoints
	provider := &core.Endpoint{
		ChainID: "1",
		Providers: []*core.Provider{{
			Name: "test-rate-limit",
			Http: server.URL,
		}},
	}
	provider.Kind = core.KindEth
	provider.Providers[0].Endpoint = provider
	provider.Providers[0].MarkHealthy(0) // Start with the endpoint online

	// The only provider is rate-limited, so the request fails over to nothing.
	w := proxyHTTP(provider, ethBlockReq)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)

	// The Retry-After: 5 header must be honored (~now+5s), not the 30s default.
	rl := provider.Providers[0].RateLimitedUntil
	assert.False(t, rl.IsZero())
	assert.True(t, rl.After(time.Now().Add(3*time.Second)), "should honor Retry-After ~5s")
	assert.True(t, rl.Before(time.Now().Add(8*time.Second)), "should not fall back to the 30s default")

	// Verify endpoint is not returned as active during rate limiting
	for _, p := range provider.Providers {
		assert.False(t, p.IsOnline())
	}
}

// TestSlowProvider tests connecting to a provider that is slow to respond
func TestSlowProvider(t *testing.T) {
	// Setup a test server that is slow to respond
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Sleep to simulate a slow response
		time.Sleep(time.Second)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x1"}`))
	}))
	defer server.Close()

	// Setup a fast server as an alternative
	fastServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x1"}`))
	}))
	defer fastServer.Close()

	// Create a provider with both slow and fast endpoints
	provider := &core.Endpoint{
		ChainID: "1",
		Kind:    "eth",
		Providers: []*core.Provider{
			{
				Name:    "slow-provider",
				Http:    server.URL,
				Timeout: time.Second / 2,
			},
			{
				Name: "fast-provider",
				Http: fastServer.URL,
			},
		},
	}
	for _, p := range provider.Providers {
		p.Endpoint = provider
		p.MarkHealthy(0)
	}

	// Attempt to proxy a request - it should use the fast provider because the first one is too slow
	w := proxyHTTP(provider, ethBlockReq)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "fast-provider", w.Header().Get("X-Provider"))
	assert.False(t, provider.Providers[0].IsOnline())
}

// TestNonRespondingProvider tests connecting to a provider that is not responding
func TestNonRespondingProvider(t *testing.T) {
	// Setup a URL that doesn't exist/respond
	nonRespondingURL := "http://non-existing-provider:9999"

	// Create a provider with a non-responding endpoint and a working endpoint
	workingServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x1"}`))
	}))
	defer workingServer.Close()

	provider := &core.Endpoint{
		ChainID: "1",
		Kind:    "eth",
		Providers: []*core.Provider{
			{
				Name: "non-responding-provider",
				Http: nonRespondingURL,
			},
			{
				Name: "working-provider",
				Http: workingServer.URL,
			},
		},
	}
	for _, p := range provider.Providers {
		p.Endpoint = provider
		p.MarkHealthy(0)
	}

	// Attempt to use the non-responding provider first
	w := proxyHTTP(provider, ethBlockReq)

	// Verify we got a response from the working provider
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "working-provider", w.Header().Get("X-Provider"))

	// Verify the non-responding provider was marked as offline
	assert.False(t, provider.Providers[0].IsOnline())
}

// Helper function to run all tests as a group
func TestProviders(t *testing.T) {
	t.Run("InvalidChainID", TestInvalidChainID)
	t.Run("RateLimitWithRetryAfter", TestRateLimitWithRetryAfter)
	t.Run("SlowProvider", TestSlowProvider)
	t.Run("NonRespondingProvider", TestNonRespondingProvider)
}

// rlProvider returns a standalone provider wired to an endpoint, for unit tests
// that don't need a server.
func rlProvider() *core.Provider {
	p := &core.Provider{Name: "p"}
	p.Endpoint = &core.Endpoint{Name: "test", Providers: []*core.Provider{p}}
	return p
}

// TestHandleTooManyRequests_RetryAfter verifies the Retry-After header is honored.
func TestHandleTooManyRequests_RetryAfter(t *testing.T) {
	p := rlProvider()

	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("Retry-After", "5")
	_ = p.HandleTooManyRequests(resp)

	got := time.Until(p.RateLimitedUntil).Seconds()
	assert.InDelta(t, (5 * time.Second).Seconds(), got, 1.0, "Retry-After: 5 should rate-limit for ~5s")
}

// TestHandleTooManyRequests_NoRetryAfter falls back to the default backoff.
func TestHandleTooManyRequests_NoRetryAfter(t *testing.T) {
	p := rlProvider()
	_ = p.HandleTooManyRequests(&http.Response{Header: http.Header{}})

	got := time.Until(p.RateLimitedUntil).Seconds()
	assert.InDelta(t, core.DefaultRateLimitBackoff.Seconds(), got, 1.0)
}

// TestHandleTooManyRequests_InvalidRetryAfter falls back to the default backoff.
func TestHandleTooManyRequests_InvalidRetryAfter(t *testing.T) {
	p := rlProvider()

	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("Retry-After", "not-a-number")
	_ = p.HandleTooManyRequests(resp)

	got := time.Until(p.RateLimitedUntil).Seconds()
	assert.InDelta(t, core.DefaultRateLimitBackoff.Seconds(), got, 1.0)
}

// TestHandleTooManyRequests_NilResponse covers the in-band/websocket case where
// there's no HTTP response to read a Retry-After from.
func TestHandleTooManyRequests_NilResponse(t *testing.T) {
	p := rlProvider()
	_ = p.HandleTooManyRequests(nil)

	got := time.Until(p.RateLimitedUntil).Seconds()
	assert.InDelta(t, core.DefaultRateLimitBackoff.Seconds(), got, 1.0)
}

// TestProviderLifecycle covers the online/offline transitions and attempt counter.
func TestProviderLifecycle(t *testing.T) {
	p := rlProvider()

	assert.False(t, p.IsOnline())

	p.MarkHealthy(0)
	assert.True(t, p.IsOnline())
	assert.Equal(t, 0, p.GetCurrentConnectionAttempts())
	assert.True(t, p.RateLimitedUntil.IsZero())

	// online -> offline: the transition itself does not bump the attempt counter.
	p.MarkUnhealthy(assert.AnError)
	assert.False(t, p.IsOnline())
	assert.Equal(t, 0, p.GetCurrentConnectionAttempts())

	// further failures while already offline increment attempts.
	p.MarkUnhealthy(assert.AnError)
	p.MarkUnhealthy(assert.AnError)
	assert.Equal(t, 2, p.GetCurrentConnectionAttempts())

	// recovery clears the attempt counter and any rate-limit window.
	_ = p.HandleTooManyRequests(nil)
	p.MarkHealthy(0)
	assert.True(t, p.IsOnline())
	assert.Equal(t, 0, p.GetCurrentConnectionAttempts())
	assert.True(t, p.RateLimitedUntil.IsZero())
}

// TestRateLimit429Failover verifies a 429'd provider is skipped and the request
// fails over to a healthy provider, with the rate-limit window recorded.
func TestRateLimit429Failover(t *testing.T) {
	rateLimited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer rateLimited.Close()

	healthy := rpctest.NewServer(map[string]any{"eth_blockNumber": "0x1"}, nil)
	defer healthy.Close()

	endpoint := &core.Endpoint{
		ChainID: "1",
		Kind:    core.KindEth,
		Providers: []*core.Provider{
			{Name: "rate-limited", Http: rateLimited.URL},
			{Name: "healthy", Http: healthy.URL},
		},
	}
	for _, p := range endpoint.Providers {
		p.Endpoint = endpoint
		p.MarkHealthy(0)
	}

	w := proxyHTTP(endpoint, ethBlockReq)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "healthy", w.Header().Get("X-Provider"))

	assert.False(t, endpoint.Providers[0].IsOnline(), "rate-limited provider should be offline")
	assert.False(t, endpoint.Providers[0].RateLimitedUntil.IsZero(), "rate-limit window should be set")
	assert.True(t, endpoint.Providers[1].IsOnline())
}

// TestProviderConcurrentAccess hammers the provider state mutators and readers
// from many goroutines so `go test -race` proves the locking is sound. It mirrors
// production: the healthcheck Worker flips state while request goroutines read it.
func TestProviderConcurrentAccess(t *testing.T) {
	p := rlProvider()

	const goroutines = 16
	const iterations = 200

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				switch (g + i) % 5 {
				case 0:
					p.MarkHealthy(time.Millisecond)
				case 1:
					p.MarkUnhealthy(assert.AnError)
				case 2:
					_ = p.HandleTooManyRequests(nil)
				case 3:
					_ = p.IsOnline()
					_ = p.GetLastStateChange()
				case 4:
					_ = p.GetNextCheckTime()
					_ = p.GetCurrentConnectionAttempts()
				}
			}
		}(g)
	}

	wg.Wait()
}
