package healthcheck

import (
	"context"
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
	p.SetHighestBlock(1000) // the network is far ahead of this node

	err := Run(context.Background(), p)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "node is behind")
}

func TestRun_HighestBlockTracking(t *testing.T) {
	p := ethProvider(t, healthyEthMethods()) // reports block 256
	p.SetHighestBlock(100)                   // previously-seen high is lower

	assert.NoError(t, Run(context.Background(), p))
	assert.Equal(t, uint64(256), p.HighestBlock(), "highest block should advance to the latest seen")
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
