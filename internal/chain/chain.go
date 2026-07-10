package chain

import (
	"context"

	"github.com/wille/haprovider/internal/chain/btc"
	"github.com/wille/haprovider/internal/chain/eth"
	"github.com/wille/haprovider/internal/chain/solana"
	"github.com/wille/haprovider/internal/chain/tron"
	"github.com/wille/haprovider/internal/core"
)

type ErrorAction int

const (
	// ErrorActionUnhealthy sets the provider as unhealthy
	ErrorActionUnhealthy ErrorAction = iota

	// ErrorActionHealthy we can continue using the provider
	ErrorActionHealthy ErrorAction = iota

	// ErrorActionNextProvider we should try the next provider
	ErrorActionNextProvider ErrorAction = iota

	// ErrorActionRateLimited we are rate limited, we should wait and try again later
	ErrorActionRateLimited ErrorAction = iota
)

// Chain is the interface every supported chain implements.
type Chain interface {
	// Healthcheck implements chain specific healthchecks
	Healthcheck(ctx context.Context, p *core.Provider) (*core.NodeInfo, error)

	// ValidateConfig validates the endpoint configuration for this chain
	ValidateConfig(e *core.Endpoint) error

	// ParseErrorResponse parses the error response from the provider and returns an error if the error should set the provider as unhealthy.
	HandleError(code int, message string) error

	// Coalesceable reports whether identical concurrent calls to this method may
	// be collapsed into a single upstream request (in-flight deduplication).
	// Default-deny: only methods on the chain's read-only allowlist return true.
	Coalesceable(method string) bool
}

var (
	_ Chain = (*eth.Ethereum)(nil)
	_ Chain = (*solana.Chain)(nil)
	_ Chain = (*tron.Chain)(nil)
	_ Chain = (*btc.Chain)(nil)
)

// New returns the Chain implementation for the given kind, or nil if the kind is
// unknown.
func New(kind core.Kind) Chain {
	switch kind {
	case core.KindEth:
		return &eth.Ethereum{}
	case core.KindSolana:
		return &solana.Chain{}
	case core.KindTron:
		return &tron.Chain{}
	case core.KindBTC:
		return &btc.Chain{}
	default:
		return nil
	}
}
