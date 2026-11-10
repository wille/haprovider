package healthcheck

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
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

	r, reporting := p.Height()
	assert.True(t, reporting)
	assert.Equal(t, uint64(256), r.Height)
}

// TestRun_InflatedHeightDoesNotTakeOver: a provider reporting a far too high
// block height must not push the honest providers out of rotation, which would
// leave it serving all traffic. With a majority to compare against, it is
// itself taken out instead.
func TestRun_InflatedHeightDoesNotTakeOver(t *testing.T) {
	liarMethods := healthyEthMethods()
	liarMethods["eth_blockNumber"] = "0xffffffffffff"
	liar := rpctest.NewServer(liarMethods, nil)
	h1 := rpctest.NewServer(healthyEthMethods(), nil)
	h2 := rpctest.NewServer(healthyEthMethods(), nil)
	t.Cleanup(liar.Close)
	t.Cleanup(h1.Close)
	t.Cleanup(h2.Close)

	ep := &core.Endpoint{Name: "test", Kind: core.KindEth, ChainID: "1"}
	honest1 := &core.Provider{Name: "honest1", Http: h1.URL, Endpoint: ep}
	honest2 := &core.Provider{Name: "honest2", Http: h2.URL, Endpoint: ep}
	l := &core.Provider{Name: "liar", Http: liar.URL, Endpoint: ep}
	ep.Providers = []*core.Provider{honest1, honest2, l}

	// The liar's first report has nothing later to be compared with yet.
	assert.NoError(t, Run(context.Background(), honest1))
	assert.NoError(t, Run(context.Background(), honest2))
	assert.NoError(t, Run(context.Background(), l))

	assert.NoError(t, Run(context.Background(), honest1), "the liar doesn't make honest1 look behind")
	assert.NoError(t, Run(context.Background(), honest2), "the liar doesn't make honest2 look behind")

	err := Run(context.Background(), l)
	if assert.Error(t, err) {
		assert.Contains(t, err.Error(), "node is ahead")
	}
	assert.NoError(t, Run(context.Background(), honest1), "the liar still doesn't count against the others")
}

// TestRun_FailedCheckStopsReporting: once a provider fails before its height is
// checked (wrong chain, syncing, unreachable), its last height is no longer
// used to judge the others.
func TestRun_FailedCheckStopsReporting(t *testing.T) {
	p := ethProvider(t, healthyEthMethods())
	assert.NoError(t, Run(context.Background(), p))
	_, reporting := p.Height()
	assert.True(t, reporting)

	p.Endpoint.ChainID = "5" // the node reports chain 1
	assert.Error(t, Run(context.Background(), p))
	_, reporting = p.Height()
	assert.False(t, reporting, "a provider on the wrong chain stops reporting")

	syncing := healthyEthMethods()
	syncing["eth_syncing"] = map[string]string{"currentBlock": "0x100", "highestBlock": "0x200"}
	s := ethProvider(t, syncing)
	assert.Error(t, Run(context.Background(), s))
	_, reporting = s.Height()
	assert.False(t, reporting, "a syncing provider doesn't report")
}

// t0 is an arbitrary start time for checkHeight tests, which drive the clock
// themselves.
var t0 = time.Unix(1_700_000_000, 0)

// heightEndpoint returns an eth endpoint with n providers and no upstreams, for
// driving checkHeight with recorded reports.
func heightEndpoint(n int) (*core.Endpoint, []*core.Provider) {
	ep := &core.Endpoint{Name: "test", Kind: core.KindEth}
	for i := range n {
		ep.Providers = append(ep.Providers, &core.Provider{Name: fmt.Sprintf("p%d", i), Endpoint: ep})
	}
	return ep, ep.Providers
}

func tolerance(ep *core.Endpoint, n int) { ep.BlockLagTolerance = &n }

func TestCheckHeight_Behind(t *testing.T) {
	ep, ps := heightEndpoint(3)
	tolerance(ep, 10)
	ps[1].RecordHeight(1000, t0)
	ps[2].RecordHeight(1000, t0)

	err := checkHeight(ps[0], 256, t0.Add(time.Second))
	if assert.Error(t, err) {
		assert.Contains(t, err.Error(), "node is behind")
	}
	assert.NoError(t, checkHeight(ps[0], 990, t0.Add(2*time.Second)), "within the tolerance")
}

// TestCheckHeight_LowHeights: with the median below the tolerance (e.g. a
// fresh devnet), the lag check must not wrap around and fail the provider.
func TestCheckHeight_LowHeights(t *testing.T) {
	_, ps := heightEndpoint(3)
	ps[1].RecordHeight(30, t0)
	ps[2].RecordHeight(30, t0)

	assert.NoError(t, checkHeight(ps[0], 20, t0.Add(time.Second)))
}

// TestCheckHeight_OneHighPeerDoesNotEvict: one provider reporting a far too high
// height doesn't move the median, so it can't make the others look behind.
func TestCheckHeight_OneHighPeerDoesNotEvict(t *testing.T) {
	_, ps := heightEndpoint(3)
	ps[1].RecordHeight(256, t0)
	ps[2].RecordHeight(1<<48, t0)

	assert.NoError(t, checkHeight(ps[0], 250, t0.Add(time.Second)))
}

// TestCheckHeight_TwoProviders: with two providers there is no majority, so
// neither is judged behind or ahead by the other's height.
func TestCheckHeight_TwoProviders(t *testing.T) {
	_, ps := heightEndpoint(2)
	ps[0].RecordHeight(256, t0)
	ps[1].RecordHeight(1<<48, t0.Add(time.Second))

	assert.NoError(t, checkHeight(ps[0], 256, t0.Add(2*time.Second)), "not behind the high one")
	assert.NoError(t, checkHeight(ps[1], 1<<48, t0.Add(3*time.Second)), "not ahead of the low one")
}

// TestCheckHeight_NotReportingPeersIgnored: a provider that stopped reporting
// doesn't count towards the median.
func TestCheckHeight_NotReportingPeersIgnored(t *testing.T) {
	_, ps := heightEndpoint(3)
	ps[1].RecordHeight(1000, t0)
	ps[2].RecordHeight(1000, t0)
	ps[2].StopReporting()

	assert.NoError(t, checkHeight(ps[0], 256, t0.Add(time.Second)))
}

func TestCheckHeight_Ahead(t *testing.T) {
	_, ps := heightEndpoint(3)
	assert.NoError(t, checkHeight(ps[0], 1<<48, t0), "nothing to compare with yet")
	ps[1].RecordHeight(256, t0.Add(time.Second))
	ps[2].RecordHeight(256, t0.Add(time.Second))

	err := checkHeight(ps[0], 256, t0.Add(2*time.Second))
	if assert.Error(t, err) {
		assert.Contains(t, err.Error(), "node is ahead")
	}
	assert.NoError(t, checkHeight(ps[0], 257, t0.Add(3*time.Second)), "back with the others")
}

// TestCheckHeight_AheadOnlyAgainstLaterReports: on a fast chain a fresh report
// is well above the others' older ones. That alone must not count as ahead:
// the previous report is only compared with reports made after it.
func TestCheckHeight_AheadOnlyAgainstLaterReports(t *testing.T) {
	_, ps := heightEndpoint(3)
	ps[1].RecordHeight(1000, t0)
	ps[2].RecordHeight(1000, t0)
	assert.NoError(t, checkHeight(ps[0], 1120, t0.Add(30*time.Second)))
	assert.NoError(t, checkHeight(ps[0], 1240, t0.Add(60*time.Second)), "the others haven't reported since")

	ps[1].RecordHeight(1200, t0.Add(70*time.Second))
	ps[2].RecordHeight(1200, t0.Add(70*time.Second))
	assert.NoError(t, checkHeight(ps[0], 1360, t0.Add(90*time.Second)), "1240 is within the tolerance of 1200")
}

// TestCheckHeight_AheadFloor: with a tolerance of 0 (the Bitcoin default), a
// node a block ahead of the others is just first to see it, not lying.
func TestCheckHeight_AheadFloor(t *testing.T) {
	for _, c := range []struct {
		prev uint64
		fail bool
	}{
		{prev: 101, fail: false},
		{prev: 103, fail: false},
		{prev: 104, fail: true},
	} {
		ep, ps := heightEndpoint(3)
		ep.Kind = core.KindBTC
		ps[0].RecordHeight(c.prev, t0)
		ps[1].RecordHeight(100, t0.Add(time.Second))
		ps[2].RecordHeight(100, t0.Add(time.Second))

		err := checkHeight(ps[0], 100, t0.Add(2*time.Second))
		assert.Equal(t, c.fail, err != nil, "prev=%d: %v", c.prev, err)
	}
}

func TestCheckHeight_Stalled(t *testing.T) {
	_, ps := heightEndpoint(2)
	ps[1].RecordHeight(300, t0)
	assert.NoError(t, checkHeight(ps[0], 256, t0))
	assert.NoError(t, checkHeight(ps[0], 256, t0.Add(time.Minute)), "not stalled for long enough")

	err := checkHeight(ps[0], 256, t0.Add(3*time.Minute))
	if assert.Error(t, err) {
		assert.Contains(t, err.Error(), "node is stalled")
	}
}

// TestCheckHeight_StalledChain: when no provider is further along (the chain
// has halted, or makes blocks only on demand), nobody is stalled.
func TestCheckHeight_StalledChain(t *testing.T) {
	_, ps := heightEndpoint(2)
	ps[1].RecordHeight(256, t0)
	assert.NoError(t, checkHeight(ps[0], 256, t0))
	assert.NoError(t, checkHeight(ps[0], 256, t0.Add(time.Hour)))
}

// TestCheckHeight_AdvancingNotStalled: a provider that keeps advancing is never
// stalled, however high another provider claims to be.
func TestCheckHeight_AdvancingNotStalled(t *testing.T) {
	_, ps := heightEndpoint(2)
	ps[1].RecordHeight(1<<48, t0)
	for i := range 10 {
		assert.NoError(t, checkHeight(ps[0], 256+uint64(i), t0.Add(time.Duration(i)*time.Minute)))
	}
}

func TestCheckHeight_StallDisabled(t *testing.T) {
	ep, ps := heightEndpoint(2)
	off := time.Duration(0)
	ep.StallTimeout = &off
	ps[1].RecordHeight(300, t0)
	assert.NoError(t, checkHeight(ps[0], 256, t0))
	assert.NoError(t, checkHeight(ps[0], 256, t0.Add(time.Hour)))
}

// TestCheckHeight_StallSurvivesDropout: a stalled provider that drops out and
// comes back at the same height is still stalled; dropping out doesn't restart
// the clock.
func TestCheckHeight_StallSurvivesDropout(t *testing.T) {
	_, ps := heightEndpoint(2)
	ps[1].RecordHeight(300, t0)
	assert.NoError(t, checkHeight(ps[0], 256, t0))
	ps[0].StopReporting()

	err := checkHeight(ps[0], 256, t0.Add(3*time.Minute))
	if assert.Error(t, err) {
		assert.Contains(t, err.Error(), "node is stalled")
	}
}

func TestMedians(t *testing.T) {
	lower, upper := medians([]uint64{3, 1, 2})
	assert.Equal(t, uint64(2), lower)
	assert.Equal(t, uint64(2), upper)

	lower, upper = medians([]uint64{4, 1, 3, 2})
	assert.Equal(t, uint64(2), lower)
	assert.Equal(t, uint64(3), upper)
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

// TestRun_Redirect: a provider that redirects fails its healthcheck, so it
// stays out of rotation until it stops redirecting.
func TestRun_Redirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://example.com/rpc", http.StatusMovedPermanently)
	}))
	t.Cleanup(srv.Close)

	ep := &core.Endpoint{Name: "test", Kind: core.KindEth}
	p := &core.Provider{Name: "p", Http: srv.URL, Endpoint: ep}
	ep.Providers = []*core.Provider{p}

	err := Run(context.Background(), p)
	if assert.Error(t, err) {
		assert.Contains(t, err.Error(), "redirect")
	}
}
