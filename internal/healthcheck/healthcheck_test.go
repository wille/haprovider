package healthcheck

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/wille/haprovider/internal/core"
	"github.com/wille/haprovider/internal/rpctest"
)

// ethProvider builds an eth provider backed by a mock upstream returning methods.
func ethProvider(t *testing.T, methods map[string]any) *core.Provider {
	srv := rpctest.NewServer(methods, nil)
	t.Cleanup(srv.Close)

	ep := &core.Endpoint{Name: "test", Kind: core.KindEth}
	p := &core.Provider{Name: "p", Http: srv.URL, Endpoint: ep}
	ep.Providers = []*core.Provider{p}
	return p
}

func healthyEthMethods() map[string]any {
	return map[string]any{
		"web3_clientVersion": "geth/test",
		"eth_chainId":        "0x1",
		"eth_blockNumber":    "0x100", // 256
		"eth_syncing":        false,
	}
}

func TestRun_Healthy(t *testing.T) {
	p := ethProvider(t, healthyEthMethods())

	assert.NoError(t, Run(context.Background(), p))
	assert.Equal(t, "geth/test", p.ClientVersion())
	assert.Equal(t, uint64(256), p.HighestBlock())
}

func TestRun_BlockLag(t *testing.T) {
	p := ethProvider(t, healthyEthMethods()) // reports block 256
	tol := 10
	p.Endpoint.BlockLagTolerance = &tol
	p.Endpoint.ObserveBlock(1000) // another provider has seen the network far ahead

	err := Run(context.Background(), p)
	if assert.Error(t, err) {
		assert.Contains(t, err.Error(), "node is behind")
	}
}

// TestRun_BlockLagAcrossProviders: a node that has stalled is caught by
// comparing it with the other providers of its endpoint. Comparing a provider
// only with its own history, as before, never flagged it: its height was
// always its own highest.
func TestRun_BlockLagAcrossProviders(t *testing.T) {
	ahead := rpctest.NewServer(map[string]any{
		"web3_clientVersion": "geth/test",
		"eth_chainId":        "0x1",
		"eth_blockNumber":    "0x1000", // 4096
		"eth_syncing":        false,
	}, nil)
	stalled := rpctest.NewServer(healthyEthMethods(), nil) // stuck at 256
	t.Cleanup(ahead.Close)
	t.Cleanup(stalled.Close)

	ep := &core.Endpoint{Name: "test", Kind: core.KindEth}
	a := &core.Provider{Name: "ahead", Http: ahead.URL, Endpoint: ep}
	s := &core.Provider{Name: "stalled", Http: stalled.URL, Endpoint: ep}
	ep.Providers = []*core.Provider{a, s}

	assert.NoError(t, Run(context.Background(), a))
	assert.Equal(t, uint64(4096), ep.HighestBlock())

	err := Run(context.Background(), s)
	if assert.Error(t, err) {
		assert.Contains(t, err.Error(), "node is behind")
	}
	assert.Equal(t, uint64(4096), ep.HighestBlock(), "a lagging provider never lowers the tip")
}

// TestRun_BlockLagWithinTolerance: a provider trailing the tip by no more than
// the tolerance is healthy.
func TestRun_BlockLagWithinTolerance(t *testing.T) {
	p := ethProvider(t, healthyEthMethods()) // reports block 256
	tol := 10
	p.Endpoint.BlockLagTolerance = &tol
	p.Endpoint.ObserveBlock(266)

	assert.NoError(t, Run(context.Background(), p))
}

// TestRun_BlockLagLowHeights: with the tip below the tolerance (e.g. a fresh
// devnet), the lag check must not wrap around and fail every provider.
func TestRun_BlockLagLowHeights(t *testing.T) {
	methods := healthyEthMethods()
	methods["eth_blockNumber"] = "0x14" // 20
	p := ethProvider(t, methods)
	// Both below the default eth tolerance of 50. (The old check subtracted the
	// tolerance from the provider's own highest block and wrapped around.)
	p.Endpoint.ObserveBlock(30)
	p.SetHighestBlock(30)

	assert.NoError(t, Run(context.Background(), p))
}

// TestRun_FailingProviderDoesNotRaiseTip: a provider that fails its
// healthcheck for another reason (here, a chain ID mismatch) doesn't move the
// endpoint's tip, so it can't push healthy providers out of rotation.
func TestRun_FailingProviderDoesNotRaiseTip(t *testing.T) {
	methods := healthyEthMethods()
	methods["eth_blockNumber"] = "0xffffff"
	p := ethProvider(t, methods)
	p.Endpoint.ChainID = "5" // the node reports chain 1

	assert.Error(t, Run(context.Background(), p))
	assert.Equal(t, uint64(0), p.Endpoint.HighestBlock())
}

func TestRun_HighestBlockTracking(t *testing.T) {
	p := ethProvider(t, healthyEthMethods()) // reports block 256
	p.SetHighestBlock(100)                   // previously-seen high is lower

	assert.NoError(t, Run(context.Background(), p))
	assert.Equal(t, uint64(256), p.HighestBlock(), "highest block should advance to the latest seen")
}

func TestRun_ChainIDDefaultsToNode(t *testing.T) {
	p := ethProvider(t, healthyEthMethods()) // reports chainId 0x1
	assert.Equal(t, "", p.Endpoint.ChainID, "chainId starts unconfigured")

	assert.NoError(t, Run(context.Background(), p))
	assert.Equal(t, "1", p.Endpoint.ChainID, "chainId defaults to the first node's chainId")
}

func TestRun_ChainIDMismatch(t *testing.T) {
	// Two providers on the same endpoint reporting different chainIds. The first
	// check defaults the endpoint chainId; the second must fail on mismatch.
	ep := &core.Endpoint{Name: "test", Kind: core.KindEth}

	srv1 := rpctest.NewServer(healthyEthMethods(), nil) // chainId 0x1
	t.Cleanup(srv1.Close)
	p1 := &core.Provider{Name: "p1", Http: srv1.URL, Endpoint: ep}

	other := healthyEthMethods()
	other["eth_chainId"] = "0x2"
	srv2 := rpctest.NewServer(other, nil)
	t.Cleanup(srv2.Close)
	p2 := &core.Provider{Name: "p2", Http: srv2.URL, Endpoint: ep}

	ep.Providers = []*core.Provider{p1, p2}

	assert.NoError(t, Run(context.Background(), p1))
	assert.Equal(t, "1", ep.ChainID)

	err := Run(context.Background(), p2)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "chainId mismatch")
	assert.Equal(t, "1", ep.ChainID, "the configured chainId is not overwritten on mismatch")
}

// TestRun_ChainIDConcurrent exercises two providers on the same endpoint whose
// healthchecks race to set/validate the shared endpoint chainId. It must be
// data-race clean (run with -race) and one of the two must fail on mismatch;
// both defaulting to conflicting chainIds is the bug SetChainID prevents.
func TestRun_ChainIDConcurrent(t *testing.T) {
	ep := &core.Endpoint{Name: "test", Kind: core.KindEth}

	srv1 := rpctest.NewServer(healthyEthMethods(), nil) // chainId 0x1
	t.Cleanup(srv1.Close)
	p1 := &core.Provider{Name: "p1", Http: srv1.URL, Endpoint: ep}

	other := healthyEthMethods()
	other["eth_chainId"] = "0x2"
	srv2 := rpctest.NewServer(other, nil)
	t.Cleanup(srv2.Close)
	p2 := &core.Provider{Name: "p2", Http: srv2.URL, Endpoint: ep}

	ep.Providers = []*core.Provider{p1, p2}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); errs[0] = Run(context.Background(), p1) }()
	go func() { defer wg.Done(); errs[1] = Run(context.Background(), p2) }()
	wg.Wait()

	// Exactly one provider wins the chainId; the other must be rejected.
	failed := 0
	for _, err := range errs {
		if err != nil {
			assert.Contains(t, err.Error(), "chainId mismatch")
			failed++
		}
	}
	assert.Equal(t, 1, failed, "exactly one provider must be rejected on chainId mismatch")
	assert.Contains(t, []string{"1", "2"}, ep.ChainID)
}

func TestRun_ConfiguredChainIDMatches(t *testing.T) {
	p := ethProvider(t, healthyEthMethods()) // reports chainId 0x1 (normalized to "1")
	p.Endpoint.ChainID = "1"                 // already configured to match

	assert.NoError(t, Run(context.Background(), p))
}

func TestRun_UnknownKind(t *testing.T) {
	ep := &core.Endpoint{Name: "test", Kind: core.Kind("doge")}
	p := &core.Provider{Name: "p", Endpoint: ep}
	ep.Providers = []*core.Provider{p}

	err := Run(context.Background(), p)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unknown endpoint kind")
}

func TestNextAttemptDelay(t *testing.T) {
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{0, 10 * time.Second},
		{4, 10 * time.Second},
		{5, 30 * time.Second},
		{9, 30 * time.Second},
		{10, time.Minute},
		{100, time.Minute},
	}
	for _, c := range cases {
		assert.Equalf(t, c.want, nextAttemptDelay(c.attempt), "attempt=%d", c.attempt)
	}
}

func TestRunOnce_Success(t *testing.T) {
	p := ethProvider(t, healthyEthMethods())
	interval := 30 * time.Second

	next := runOnce(p, interval)
	assert.Equal(t, interval, next, "a passing check waits the normal interval")
	assert.True(t, p.IsOnline())
}

func TestRunOnce_Failure(t *testing.T) {
	// Upstream returns an RPC error so the healthcheck fails deterministically.
	methods := healthyEthMethods()
	methods["web3_clientVersion"] = rpctest.RPCError{Code: -32000, Message: "boom"}
	p := ethProvider(t, methods)
	p.MarkHealthy(0) // start online so the failure is a real online->offline transition

	next := runOnce(p, 30*time.Second)
	assert.False(t, p.IsOnline(), "a failing check marks the provider offline")
	assert.Equal(t, 10*time.Second, next, "first failure backs off to nextAttemptDelay(0)")
}
