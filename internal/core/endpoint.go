package core

import (
	"fmt"
	"log/slog"
	"sync"

	"golang.org/x/sync/singleflight"
)

// DefaultMaxResponseSize bounds how large an upstream provider response may be
// (HTTP body or WebSocket message) before we reject it, to protect the proxy
// from memory exhaustion (including decompression bombs over WebSocket).
// Generous by default because legitimate responses (eth_getLogs, traces,
// getProgramAccounts) can be large. Override per endpoint with max_response_size;
// set it to 0 for unlimited.
const DefaultMaxResponseSize int64 = 100 * 1024 * 1024

type Kind string

const (
	KindEth    Kind = "eth"
	KindSolana Kind = "solana"
	KindTron   Kind = "tron"
	KindBTC    Kind = "btc"
)

type Endpoint struct {
	// Name is inferred from the key in the config if unset in the object
	Name string `yaml:"name"`

	// ChainID is the Ethereum/L2 chain ID we're expecting.
	// If it's not matching we will refuse using the provider.
	// Guarded by mu; use SetChainID to mutate it concurrently.
	ChainID string `yaml:"chainId"`

	// Kind is the type of
	Kind Kind `yaml:"kind"`

	Providers []*Provider `yaml:"providers"`

	// Public indicates that the endpoint is public and that we
	// will skip sending debug headers and detailed error messages to the client.
	Public bool `yaml:"public,omitempty"`

	// AddXForwardedHeaders adds X-Forwarded-For header to requests sent to upstream providers
	AddXForwardedHeaders bool `yaml:"add_xfwd_headers,omitempty"`

	BlockLagTolerance *int `yaml:"block_lag_tolerance,omitempty"`

	// MaxResponseSize caps the size in bytes of an upstream provider response.
	// Unset uses DefaultMaxResponseSize; 0 means unlimited.
	MaxResponseSize *int64 `yaml:"max_response_size,omitempty"`

	// mu guards ChainID, which is set/validated concurrently by per-provider
	// healthcheck goroutines sharing this endpoint.
	mu sync.Mutex

	// Coalescer deduplicates identical concurrent upstream requests on the HTTP
	// path. The zero value is ready to use; keys are scoped to this endpoint.
	Coalescer singleflight.Group
}

// SetChainID records the chain ID reported by a provider. The first provider to
// report sets the endpoint's chain ID; any later provider reporting a different
// chain ID is rejected with an error.
func (e *Endpoint) SetChainID(chainID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.ChainID == "" {
		e.ChainID = chainID
		e.Logger().Info("chainId not configured, now using the first seen chainId", "chainId", chainID)
		return nil
	}

	if e.ChainID != chainID {
		return fmt.Errorf("chainId mismatch: received=%s, expected=%s", chainID, e.ChainID)
	}

	return nil
}

// GetChainID returns the endpoint's chain ID, synchronized against concurrent
// SetChainID calls from per-provider healthcheck goroutines.
func (e *Endpoint) GetChainID() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.ChainID
}

// Logger returns a structured logger tagged with the endpoint name.
func (e *Endpoint) Logger() *slog.Logger {
	return slog.With("endpoint", e.Name)
}

// GetMaxResponseSize returns the upstream response size cap in bytes, or
// DefaultMaxResponseSize when unconfigured. A return of 0 means unlimited.
func (e *Endpoint) GetMaxResponseSize() int64 {
	if e.MaxResponseSize != nil {
		return *e.MaxResponseSize
	}
	return DefaultMaxResponseSize
}
