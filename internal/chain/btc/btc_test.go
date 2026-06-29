package btc

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
		Endpoint: &core.Endpoint{Name: "test", Kind: core.KindBTC},
	}
}

func TestHealthcheckHealthy(t *testing.T) {
	srv := rpctest.NewServer(map[string]any{
		"getnetworkinfo": map[string]any{"subversion": "/Satoshi:27.0.0/"},
		"getblockhash":   "000000000019d6689c085ae165831e934ff763ae46a2a6c172b3f1b60a8ce26f",
		"getblockchaininfo": map[string]any{
			"blocks":               800000,
			"headers":              800000,
			"verificationprogress": 0.9999,
			"initialblockdownload": false,
		},
		"getmempoolinfo": map[string]any{"loaded": true},
	}, nil)
	defer srv.Close()

	info, err := (&Chain{}).Healthcheck(context.Background(), newProvider(srv.URL))
	if err != nil {
		t.Fatalf("Healthcheck: %v", err)
	}
	if info.ClientVersion != "/Satoshi:27.0.0/" {
		t.Errorf("ClientVersion = %q, want /Satoshi:27.0.0/", info.ClientVersion)
	}
	if info.ChainID != "mainnet" {
		t.Errorf("ChainID = %q, want mainnet", info.ChainID)
	}
	if info.BlockHeight != 800000 {
		t.Errorf("BlockHeight = %d, want 800000", info.BlockHeight)
	}
}

func TestHealthcheckBehind(t *testing.T) {
	srv := rpctest.NewServer(map[string]any{
		"getnetworkinfo": map[string]any{"subversion": "/Satoshi:27.0.0/"},
		"getblockhash":   "000000000019d6689c085ae165831e934ff763ae46a2a6c172b3f1b60a8ce26f",
		"getblockchaininfo": map[string]any{
			"blocks":               799900,
			"headers":              800000,
			"verificationprogress": 0.98,
			"initialblockdownload": false,
		},
	}, nil)
	defer srv.Close()

	info, err := (&Chain{}).Healthcheck(context.Background(), newProvider(srv.URL))
	if err == nil {
		t.Fatal("Healthcheck = nil; want error when blocks trail headers")
	}
	// NodeInfo is populated even though the healthcheck failed.
	if info == nil || info.BlockHeight != 799900 {
		t.Errorf("NodeInfo not populated on error: %+v", info)
	}
}
