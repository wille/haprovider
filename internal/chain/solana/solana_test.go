package solana

import (
	"context"
	"errors"
	"testing"

	"github.com/wille/haprovider/internal/core"
	"github.com/wille/haprovider/internal/rpc"
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

// TestHandleError verifies Solana error-code dispositions: node-health, data
// availability (try next provider), and client errors (returned as-is).
func TestHandleError(t *testing.T) {
	c := &Chain{}

	// Method not found -> method not supported (skip-not-lose).
	var mns *rpc.MethodNotSupportedError
	if err := c.HandleError(-32601, "method not found"); !errors.As(err, &mns) {
		t.Errorf("code -32601: expected *rpc.MethodNotSupportedError, got %v", err)
	}

	// Data-availability codes: an archival / more in-sync provider may have it.
	for _, code := range []int{-32001, -32004, -32007, -32008, -32009, -32011, -32014, -32016} {
		err := c.HandleError(code, "unavailable")
		var tryNext *rpc.ErrorTryNextProvider
		if !errors.As(err, &tryNext) {
			t.Errorf("code %d: expected *rpc.ErrorTryNextProvider, got %v", code, err)
		}
	}

	// Node unhealthy / behind -> rate limited.
	var rl *rpc.ErrorRateLimited
	if err := c.HandleError(-32005, "behind"); !errors.As(err, &rl) {
		t.Errorf("code -32005: expected *rpc.ErrorRateLimited, got %v", err)
	}

	// Internal error -> generic error (marks unhealthy).
	if err := c.HandleError(-32603, "boom"); err == nil {
		t.Error("code -32603: expected a non-nil error")
	}

	// Client / transaction errors are returned to the client (nil from HandleError).
	// -32012 is an account-scan error (oversized query) — a client condition, so we
	// must not mark the healthy provider unhealthy over it.
	for _, code := range []int{-32002, -32003, -32006, -32010, -32012, -32013, -32015} {
		if err := c.HandleError(code, "client error"); err != nil {
			t.Errorf("code %d: expected nil (return to client), got %v", code, err)
		}
	}
}
