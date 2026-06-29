package solana

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
		Endpoint: &core.Endpoint{Name: "test", Kind: core.KindSolana},
	}
}

func healthyMethods() map[string]any {
	return map[string]any{
		"getVersion":     map[string]any{"solana-core": "1.18.0"},
		"getGenesisHash": "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d",
		"getBlockHeight": 12345,
		"getHealth":      "ok",
	}
}

func TestHealthcheckHealthy(t *testing.T) {
	srv := rpctest.NewServer(healthyMethods(), nil)
	defer srv.Close()

	info, err := (&Chain{}).Healthcheck(context.Background(), newProvider(srv.URL))
	if err != nil {
		t.Fatalf("Healthcheck: %v", err)
	}
	if info.ClientVersion != "1.18.0" {
		t.Errorf("ClientVersion = %q, want 1.18.0", info.ClientVersion)
	}
	if info.ChainID != "mainnet" {
		t.Errorf("ChainID = %q, want mainnet", info.ChainID)
	}
	if info.BlockHeight != 12345 {
		t.Errorf("BlockHeight = %d, want 12345", info.BlockHeight)
	}
}

func TestHealthcheckUnhealthy(t *testing.T) {
	methods := healthyMethods()
	methods["getHealth"] = "behind"
	srv := rpctest.NewServer(methods, nil)
	defer srv.Close()

	info, err := (&Chain{}).Healthcheck(context.Background(), newProvider(srv.URL))
	if err == nil {
		t.Fatal("Healthcheck = nil; want error when getHealth is not ok")
	}
	// NodeInfo is populated even though the healthcheck failed.
	if info == nil || info.ClientVersion != "1.18.0" || info.BlockHeight != 12345 {
		t.Errorf("NodeInfo not populated on error: %+v", info)
	}
}
