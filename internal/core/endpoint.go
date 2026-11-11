package core

import (
	"crypto/subtle"
	"fmt"
	"log/slog"
	"sync"
	"time"

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
	// Note: public does NOT bypass authentication; it only controls debug output.
	Public bool `yaml:"public,omitempty"`

	// Keys are the inbound bearer tokens accepted for this endpoint (in addition
	// to any global keys). Env references are expanded at config load.
	Keys []string `yaml:"keys,omitempty"`

	// authKeys is the effective set of accepted tokens (endpoint Keys + global
	// keys), populated by InitAuth at config load.
	authKeys []string

	// AddXForwardedHeaders adds X-Forwarded-For header to requests sent to upstream providers
	AddXForwardedHeaders bool `yaml:"add_xfwd_headers,omitempty"`

	// Verify cross-checks the results of the methods the chain lists (balances,
	// contract reads, chain height) against a second provider on the HTTP path,
	// so a single provider can't get a wrong answer through. Best effort: when
	// no other provider answers, the result is sent unverified.
	Verify bool `yaml:"verify,omitempty"`

	// Broadcast sends transactions (eth_sendRawTransaction, Solana
	// sendTransaction) to every provider at once on the HTTP path, instead of
	// only the first, so one provider dropping a transaction can't hold it up.
	// Opt-in: it would defeat a private or MEV-protected provider.
	Broadcast bool `yaml:"broadcast,omitempty"`

	// VerifyRetryDelay is how long to wait before asking two disagreeing
	// providers once more, when verify is enabled. Honest providers a block
	// apart disagree on a value that changed in that block, and have usually
	// caught up by then. Unset uses DefaultVerifyRetryDelay; 0 disables it.
	VerifyRetryDelay *time.Duration `yaml:"verify_retry_delay,omitempty"`

	// BlockLagTolerance is how many blocks a provider may trail (or lead) the
	// median height of the endpoint's providers. Unset uses a per-chain default.
	BlockLagTolerance *int `yaml:"block_lag_tolerance,omitempty"`

	// StallTimeout is how long a provider's block height may stay unchanged
	// while another provider reports a higher one before it's considered
	// stalled. Unset uses a per-chain default; 0 disables the check.
	StallTimeout *time.Duration `yaml:"stall_timeout,omitempty"`

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

// DefaultVerifyRetryDelay is how long to wait before asking two disagreeing
// providers once more (see Endpoint.VerifyRetryDelay).
const DefaultVerifyRetryDelay = time.Second

// GetVerifyRetryDelay returns how long to wait before asking two disagreeing
// providers once more, or 0 if they aren't asked again.
func (e *Endpoint) GetVerifyRetryDelay() time.Duration {
	if e.VerifyRetryDelay != nil {
		return max(*e.VerifyRetryDelay, 0)
	}
	return DefaultVerifyRetryDelay
}

// GetMaxResponseSize returns the upstream response size cap in bytes, or
// DefaultMaxResponseSize when unconfigured. A return of 0 means unlimited.
func (e *Endpoint) GetMaxResponseSize() int64 {
	if e.MaxResponseSize != nil {
		return *e.MaxResponseSize
	}
	return DefaultMaxResponseSize
}

// InitAuth sets the effective set of accepted inbound tokens for this endpoint:
// its own Keys plus the given global keys. Called once at config load.
func (e *Endpoint) InitAuth(globalKeys []string) {
	e.authKeys = append(append([]string{}, e.Keys...), globalKeys...)
}

// RequiresAuth reports whether this endpoint has any accepted keys and therefore
// requires callers to authenticate.
func (e *Endpoint) RequiresAuth() bool {
	return len(e.authKeys) > 0
}

// Authorized reports whether token matches one of the endpoint's accepted keys.
// The comparison is constant-time to avoid leaking key material via timing.
func (e *Endpoint) Authorized(token string) bool {
	for _, k := range e.authKeys {
		if k != "" && subtle.ConstantTimeCompare([]byte(token), []byte(k)) == 1 {
			return true
		}
	}
	return false
}
