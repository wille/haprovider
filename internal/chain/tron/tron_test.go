package tron

import (
	"context"
	"testing"

	"github.com/wille/haprovider/internal/core"
	"github.com/wille/haprovider/internal/rpctest"
)

func newProvider(url string) *core.Provider {
	return &core.Provider{
		Name:     "test",
		Http:     url,
		Endpoint: &core.Endpoint{Name: "test", Kind: core.KindTron},
	}
}

func TestHealthcheck(t *testing.T) {
	srv := rpctest.NewServer(map[string]any{
		"web3_clientVersion": "TRON/v4.7.3",
		"eth_chainId":        "0x2b6653dc",
		"eth_blockNumber":    "0x100",
	}, nil)
	defer srv.Close()

	info, err := (&Chain{}).Healthcheck(context.Background(), newProvider(srv.URL))
	if err != nil {
		t.Fatalf("Healthcheck: %v", err)
	}
	if info.ClientVersion != "TRON/v4.7.3" {
		t.Errorf("ClientVersion = %q, want TRON/v4.7.3", info.ClientVersion)
	}
	if info.ChainID != "mainnet" {
		t.Errorf("ChainID = %q, want mainnet", info.ChainID)
	}
	if info.BlockHeight != 256 {
		t.Errorf("BlockHeight = %d, want 256", info.BlockHeight)
	}
}
