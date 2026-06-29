package eth

import (
	"context"
	"testing"

	"github.com/wille/haprovider/internal/core"
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
