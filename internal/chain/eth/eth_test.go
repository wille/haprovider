package eth

import (
	"context"
	"errors"
	"testing"

	"github.com/wille/haprovider/internal/core"
	"github.com/wille/haprovider/internal/rpc"
	"github.com/wille/haprovider/internal/rpctest"
)

func TestParseHexNumber(t *testing.T) {
	i, _ := ParseHexNumber("0x11")

	if i != 17 {
		t.Errorf("ParseHexNumber(%q) = %v, want %v", "0x11", i, 17)
	}

	_, err := ParseHexNumber("0x10000000000000000000000000000000000000000000000000000000000000000")
	if err == nil {
		t.Errorf("Did not fail on out of range number")
	}
}

func newProvider(url string) *core.Provider {
	return &core.Provider{
		Name:     "test",
		Http:     url,
		Endpoint: &core.Endpoint{Name: "test", Kind: core.KindEth},
	}
}

func TestHealthcheckSynced(t *testing.T) {
	srv := rpctest.NewServer(map[string]any{
		"web3_clientVersion": "Geth/v1.13.0",
		"eth_chainId":        "0x1",
		"eth_blockNumber":    "0x100",
		"eth_syncing":        false,
	}, nil)
	defer srv.Close()

	info, err := (&Ethereum{}).Healthcheck(context.Background(), newProvider(srv.URL))
	if err != nil {
		t.Fatalf("Healthcheck: %v", err)
	}
	if info.ClientVersion != "Geth/v1.13.0" {
		t.Errorf("ClientVersion = %q, want Geth/v1.13.0", info.ClientVersion)
	}
	if info.ChainID != "1" {
		t.Errorf("ChainID = %q, want 1", info.ChainID)
	}
	if info.BlockHeight != 256 {
		t.Errorf("BlockHeight = %d, want 256", info.BlockHeight)
	}
}

func TestHealthcheckSyncing(t *testing.T) {
	srv := rpctest.NewServer(map[string]any{
		"web3_clientVersion": "Geth/v1.13.0",
		"eth_chainId":        "0x1",
		"eth_blockNumber":    "0x100",
		"eth_syncing":        map[string]any{"currentBlock": "0x10", "highestBlock": "0x20"},
	}, nil)
	defer srv.Close()

	info, err := (&Ethereum{}).Healthcheck(context.Background(), newProvider(srv.URL))
	if err == nil {
		t.Fatal("Healthcheck = nil; want error for a syncing node")
	}
	// NodeInfo is populated even though the healthcheck failed.
	if info == nil || info.ClientVersion != "Geth/v1.13.0" || info.BlockHeight != 256 {
		t.Errorf("NodeInfo not populated on error: %+v", info)
	}
}

// TestHandleError verifies the eth error-code dispositions across the EVM
// ecosystem (EIP-1474 / Geth / QuickNode).
func TestHandleError(t *testing.T) {
	c := &Ethereum{}

	// Method not supported: standard -32601, and provider variants -32604/-32004,
	// plus QuickNode's trace-gating -32612/-32613.
	var mns *rpc.MethodNotSupportedError
	for _, code := range []int{-32601, -32604, -32004, -32612, -32613} {
		if err := c.HandleError(code, "not supported"); !errors.As(err, &mns) {
			t.Errorf("code %d: expected *rpc.MethodNotSupportedError, got %v", code, err)
		}
	}

	// Rate limited.
	var rl *rpc.ErrorRateLimited
	if err := c.HandleError(-32005, "slow down"); !errors.As(err, &rl) {
		t.Errorf("code -32005: expected *rpc.ErrorRateLimited, got %v", err)
	}

	// Provider-unhealthy: internal (-32603) and network (-32011) errors.
	for _, code := range []int{-32603, -32011} {
		if err := c.HandleError(code, "boom"); err == nil {
			t.Errorf("code %d: expected a non-nil (unhealthy) error", code)
		}
	}

	// Data-availability: resource-not-found / unavailable, and -32000 with a
	// pruned/archive/not-synced message → try the next provider.
	var tryNext *rpc.ErrorTryNextProvider
	if err := c.HandleError(-32001, "resource not found"); !errors.As(err, &tryNext) {
		t.Errorf("code -32001: expected *rpc.ErrorTryNextProvider, got %v", err)
	}
	if err := c.HandleError(-32002, "resource unavailable"); !errors.As(err, &tryNext) {
		t.Errorf("code -32002: expected *rpc.ErrorTryNextProvider, got %v", err)
	}
	for _, msg := range []string{"missing trie node abc", "header not found", "required historical state unavailable"} {
		if err := c.HandleError(-32000, msg); !errors.As(err, &tryNext) {
			t.Errorf("-32000 %q: expected *rpc.ErrorTryNextProvider, got %v", msg, err)
		}
	}

	// Client-level results are returned to the client (nil): invalid params, and a
	// -32000 that is NOT a data-availability message (nonce, revert, filter).
	for _, tc := range []struct {
		code int
		msg  string
	}{
		{-32602, "invalid params"},
		{-32000, "nonce too low"},
		{-32000, "execution reverted"},
		{-32000, "filter not found"},
		{3, "execution reverted"},
	} {
		if err := c.HandleError(tc.code, tc.msg); err != nil {
			t.Errorf("code %d %q: expected nil (return to client), got %v", tc.code, tc.msg, err)
		}
	}
}
