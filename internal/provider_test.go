package internal

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	servertiming "github.com/mitchellh/go-server-timing"
	"github.com/stretchr/testify/assert"
	"github.com/wille/haprovider/internal/rpc"
)

// TestInvalidChainID tests connecting to a provider with invalid chainID
func TestInvalidChainID(t *testing.T) {
	// Setup a test server that returns a different chainID than expected
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Return a different chainID (0x2 instead of 0x1)
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x2"}`))
	}))
	defer server.Close()

	// Create a provider expecting chainID 1
	provider := &Endpoint{
		ChainID: 1,
		Kind:    "eth",
		Providers: []*Provider{
			{
				Name: "test-invalid-chain",
				Http: server.URL,
			},
		},
	}

	// Initialize the endpoint status
	err := provider.HTTPHealthcheck(provider.Providers[0])

	// Verify the endpoint was marked as offline due to chainID mismatch
	assert.Error(t, err)
	assert.False(t, provider.Providers[0].online)
	assert.Contains(t, err.Error(), "chainId mismatch")

	// Verify no active endpoints are returned
	for _, p := range provider.Providers {
		assert.False(t, p.online)
	}
}

// TestRateLimitWithRetryAfter tests connecting to a provider that returns a 429 rate limit with retry-after header
func TestRateLimitWithRetryAfter(t *testing.T) {
	// Setup a test server that returns a 429 with a Retry-After header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "5") // Retry after 5 seconds
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32005,"message":"rate limit exceeded"}}`))
	}))
	defer server.Close()

	// Create a provider to test GetActiveEndpoints
	provider := &Endpoint{
		ChainID: 1,
		Providers: []*Provider{{
			Name:         "test-rate-limit",
			EndpointName: "test-provider",
			Http:         server.URL,
			online:       true, // Start with the endpoint online
		}},
	}

	// Send a request that will trigger rate limiting
	req := rpc.NewBatchRequest(
		rpc.NewRequest("1", "eth_blockNumber", []interface{}{}),
	)
	_, _, err := ProxyHTTP(context.Background(), provider, req, &servertiming.Header{})

	// Verify the error and retry time was set correctly
	assert.Error(t, err)
	assert.Contains(t, err.Error(), ErrNoProvidersAvailable.Error())

	// Verify retry time is in the future (between now and now+10s)
	assert.False(t, provider.Providers[0].retryAt.IsZero())
	assert.True(t, provider.Providers[0].retryAt.After(time.Now()))
	assert.True(t, provider.Providers[0].retryAt.Before(time.Now().Add(30*time.Second)))

	// Verify endpoint is not returned as active during rate limiting
	for _, p := range provider.Providers {
		assert.False(t, p.online)
	}
}

// TestSlowProvider tests connecting to a provider that is slow to respond
func TestSlowProvider(t *testing.T) {
	// Setup a test server that is slow to respond
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Sleep to simulate a slow response
		time.Sleep(time.Second)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x1"}`))
	}))
	defer server.Close()

	// Setup a fast server as an alternative
	fastServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x1"}`))
	}))
	defer fastServer.Close()

	// Create a provider with both slow and fast endpoints
	provider := &Endpoint{
		ChainID: 1,
		Kind:    "eth",
		Providers: []*Provider{
			{
				Name:    "slow-provider",
				Http:    server.URL,
				online:  true,
				Timeout: time.Second / 2,
			},
			{
				Name:   "fast-provider",
				Http:   fastServer.URL,
				online: true,
			},
		},
	}

	timing := &servertiming.Header{}

	// Attempt to proxy a request - it should use the fast provider because the first one is too slow
	req := rpc.NewBatchRequest(
		rpc.NewRequest("1", "eth_blockNumber", []interface{}{}),
	)
	resp, endpoint, err := ProxyHTTP(context.Background(), provider, req, timing)

	// Verify we got a response
	assert.NoError(t, err)
	assert.NotNil(t, resp)
	assert.Equal(t, "fast-provider", endpoint.Name)
	assert.False(t, provider.Providers[0].online)
}

// TestNonRespondingProvider tests connecting to a provider that is not responding
func TestNonRespondingProvider(t *testing.T) {
	// Setup a URL that doesn't exist/respond
	nonRespondingURL := "http://non-existing-provider:9999"

	// Create a provider with a non-responding endpoint and a working endpoint
	workingServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x1"}`))
	}))
	defer workingServer.Close()

	provider := &Endpoint{
		ChainID: 1,
		Kind:    "eth",
		Providers: []*Provider{
			{
				Name:   "non-responding-provider",
				Http:   nonRespondingURL,
				online: true, // Start as online
			},
			{
				Name:   "working-provider",
				Http:   workingServer.URL,
				online: true,
			},
		},
	}

	// Attempt to use the non-responding provider first
	timing := &servertiming.Header{}
	req := rpc.NewBatchRequest(
		rpc.NewRequest("1", "eth_blockNumber", []interface{}{}),
	)
	resp, endpoint, err := ProxyHTTP(context.Background(), provider, req, timing)

	// Verify we got a response from the working provider
	assert.NoError(t, err)
	assert.NotNil(t, resp)
	assert.Equal(t, "working-provider", endpoint.Name)

	// Verify the non-responding provider was marked as offline
	assert.False(t, provider.Providers[0].online)
}

// Helper function to run all tests as a group
func TestProviders(t *testing.T) {
	t.Run("InvalidChainID", TestInvalidChainID)
	t.Run("RateLimitWithRetryAfter", TestRateLimitWithRetryAfter)
	t.Run("SlowProvider", TestSlowProvider)
	t.Run("NonRespondingProvider", TestNonRespondingProvider)
}
