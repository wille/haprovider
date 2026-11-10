package btc

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/wille/haprovider/internal/core"
	"github.com/wille/haprovider/internal/rpctest"
)

const (
	mainnetGenesis  = "000000000019d6689c085ae165831e934ff763ae46a2a6c172b3f1b60a8ce26f"
	testnet3Genesis = "000000000933ea01ad0ee984209779baaec3ced90fa3f408719526f8d77f4943"
	testnet4Genesis = "00000000da84f2bafbbc53dee25a72ae507ff4914b867c565be350b0da8bf043"
	regtestGenesis  = "0f9188f13cb7b2c71f2a335e3a4fc328bf5beb436012afca590b1a11466e2206"

	// forkBlock840000 stands in for a fork's block at the checkpoint height:
	// anything but Bitcoin's.
	forkBlock840000 = "0000000000000000011c1ec3a8f2ebd8b0af9e1e1a4b7f2ed6f3b0a7c2c8a2f1"
)

func newProvider(url string) *core.Provider {
	return &core.Provider{
		Name:     "test",
		Http:     url,
		Endpoint: &core.Endpoint{Name: "test", Kind: core.KindBTC},
	}
}

// node returns the methods of a synced mock node whose block 0 is genesis and
// whose block 840000 is checkpoint; an empty checkpoint means the node has no
// block at that height.
func node(genesis, checkpoint string) map[string]any {
	return map[string]any{
		"getnetworkinfo": map[string]any{"subversion": "/Satoshi:27.0.0/"},
		"getblockhash": rpctest.Handler(func(params json.RawMessage) any {
			var args []uint64
			_ = json.Unmarshal(params, &args)
			switch {
			case len(args) == 1 && args[0] == 0:
				return genesis
			case len(args) == 1 && args[0] == 840000 && checkpoint != "":
				return checkpoint
			}
			return rpctest.RPCError{Code: -8, Message: "Block height out of range"}
		}),
		"getblockchaininfo": map[string]any{
			"blocks":               900000,
			"headers":              900000,
			"verificationprogress": 0.9999,
			"initialblockdownload": false,
		},
		"getmempoolinfo": map[string]any{"loaded": true},
	}
}

func healthcheck(t *testing.T, methods map[string]any) (*core.NodeInfo, error) {
	t.Helper()
	srv := rpctest.NewServer(methods, nil)
	t.Cleanup(srv.Close)
	return (&Chain{}).Healthcheck(context.Background(), newProvider(srv.URL))
}

func TestHealthcheckHealthy(t *testing.T) {
	info, err := healthcheck(t, node(mainnetGenesis, mainnetCheckpoint.Hash))
	if err != nil {
		t.Fatalf("Healthcheck: %v", err)
	}
	if info.ClientVersion != "/Satoshi:27.0.0/" {
		t.Errorf("ClientVersion = %q, want /Satoshi:27.0.0/", info.ClientVersion)
	}
	if info.ChainID != "mainnet" {
		t.Errorf("ChainID = %q, want mainnet", info.ChainID)
	}
	if info.BlockHeight != 900000 {
		t.Errorf("BlockHeight = %d, want 900000", info.BlockHeight)
	}
}

func TestHealthcheckBehind(t *testing.T) {
	methods := node(mainnetGenesis, mainnetCheckpoint.Hash)
	methods["getblockchaininfo"] = map[string]any{
		"blocks":               899900,
		"headers":              900000,
		"verificationprogress": 0.98,
		"initialblockdownload": false,
	}

	info, err := healthcheck(t, methods)
	if err == nil {
		t.Fatal("Healthcheck = nil; want error when blocks trail headers")
	}
	// NodeInfo is populated even though the healthcheck failed.
	if info == nil || info.BlockHeight != 899900 {
		t.Errorf("NodeInfo not populated on error: %+v", info)
	}
}

// TestHealthcheckFork: a node of a fork that shares Bitcoin's genesis block
// (Bitcoin Cash, SV, Gold) passes the genesis check but not the checkpoint.
func TestHealthcheckFork(t *testing.T) {
	_, err := healthcheck(t, node(mainnetGenesis, forkBlock840000))
	if err == nil || !strings.Contains(err.Error(), "not Bitcoin mainnet") {
		t.Fatalf("Healthcheck = %v; want a 'not Bitcoin mainnet' error", err)
	}
}

// TestHealthcheckNoCheckpoint: a mainnet node that can't show its block at the
// checkpoint height can't prove it isn't a fork.
func TestHealthcheckNoCheckpoint(t *testing.T) {
	_, err := healthcheck(t, node(mainnetGenesis, ""))
	if err == nil || !strings.Contains(err.Error(), "checkpoint") {
		t.Fatalf("Healthcheck = %v; want a checkpoint error", err)
	}
}

// TestHealthcheckNetworks: other networks are named by their genesis block (the
// checkpoint is mainnet's and isn't read), and a network without a name is
// identified by its genesis hash rather than passing for any network.
func TestHealthcheckNetworks(t *testing.T) {
	for genesis, want := range map[string]string{
		testnet3Genesis: "testnet3",
		testnet4Genesis: "testnet4",
		regtestGenesis:  regtestGenesis,
	} {
		info, err := healthcheck(t, node(genesis, ""))
		if err != nil {
			t.Errorf("%s: Healthcheck: %v", want, err)
			continue
		}
		if info.ChainID != want {
			t.Errorf("ChainID = %q, want %q", info.ChainID, want)
		}
	}
}

func TestHealthcheckInvalidGenesis(t *testing.T) {
	if _, err := healthcheck(t, node("not a hash", "")); err == nil {
		t.Fatal("Healthcheck = nil; want an error for an invalid genesis hash")
	}
}

func TestValidateConfig(t *testing.T) {
	for chainID, ok := range map[string]bool{
		"mainnet":                       true,
		"testnet3":                      true,
		"testnet4":                      true,
		"signet":                        true,
		regtestGenesis:                  true,
		strings.ToUpper(regtestGenesis): false,
		"regtest":                       false,
		"bitcoin":                       false,
	} {
		err := (&Chain{}).ValidateConfig(&core.Endpoint{Name: "btc", Kind: core.KindBTC, ChainID: chainID})
		if ok != (err == nil) {
			t.Errorf("ValidateConfig(%q) = %v, want ok=%v", chainID, err, ok)
		}
	}
}
