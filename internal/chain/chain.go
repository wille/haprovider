package chain

import (
	"context"
	"encoding/json"

	"github.com/wille/haprovider/internal/chain/btc"
	"github.com/wille/haprovider/internal/chain/eth"
	"github.com/wille/haprovider/internal/chain/solana"
	"github.com/wille/haprovider/internal/chain/tron"
	"github.com/wille/haprovider/internal/core"
	"github.com/wille/haprovider/internal/rpc"
	"github.com/wille/haprovider/internal/verify"
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

	// AllowMethod reports whether a client may call this method on this chain.
	// Most chains allow everything; Bitcoin restricts clients to blockchain-query
	// RPCs and rejects wallet/mining/control/etc. commands.
	AllowMethod(method string) bool

	// Verifier returns how to cross-check a result for a call to method with
	// params against a second provider's when the endpoint has verify enabled,
	// or nil when the call isn't cross-checked. Default-deny: only the reads a
	// payment processor credits deposits from (balances, contract reads, chain
	// height) are listed, and not at a block or commitment level honest
	// providers rarely agree on (e.g. eth's pending block).
	Verifier(method string, params json.RawMessage) verify.Func
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

// RejectDisallowed checks req against c's method allowlist. If every method is
// allowed it returns nil. Otherwise the whole request is rejected and it
// returns one JSON-RPC error response per sub-request, in order: the offending
// method(s) report "method not allowed", and any other members of the same
// batch report that the batch was rejected. Used by both the HTTP and the
// WebSocket path so the allowlist is enforced the same way on each.
func RejectDisallowed(c Chain, req *rpc.BatchRequest) []*rpc.Response {
	if c == nil {
		return nil
	}

	allowed := true
	for _, sub := range req.Requests {
		if !c.AllowMethod(sub.Method) {
			allowed = false
			break
		}
	}
	if allowed {
		return nil
	}

	responses := make([]*rpc.Response, len(req.Requests))
	for i, sub := range req.Requests {
		msg := "method not allowed: " + sub.Method
		if c.AllowMethod(sub.Method) {
			msg = "request rejected: batch contains a method that is not allowed"
		}
		responses[i] = &rpc.Response{Version: "2.0", ID: sub.ID, Error: rpc.NewError(rpc.CodeResourceUnavailable, msg)}
	}
	return responses
}
