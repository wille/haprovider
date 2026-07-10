package solana

import (
	"context"
	"fmt"
	"maps"

	"github.com/wille/haprovider/internal/core"
	"github.com/wille/haprovider/internal/httpx"
	"github.com/wille/haprovider/internal/rpc"
)

var genesisHash = map[string]string{
	"5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d": "mainnet",
	"4uhcVJyU9pJkvQyS88uRDiswHXSCkY3zQawwpjk2NsNY": "testnet",
	"EtWTRABZaYq6iMfeYKouRu166VU2xqa1wcaWoxPkrZBG": "devnet",
}

type Chain struct{}

func (c *Chain) ValidateConfig(e *core.Endpoint) error {
	if e.ChainID != "" {
		for v := range maps.Values(genesisHash) {
			if v == e.ChainID {
				return nil
			}
		}
		return fmt.Errorf("invalid chainId: %s", e.ChainID)
	}

	return nil
}

// coalesceable is the allowlist of Solana JSON-RPC methods that are safe to
// collapse into a single upstream call. Anything not listed is not coalesced.
var coalesceable = map[string]struct{}{
	"getAccountInfo":         {},
	"getMultipleAccounts":    {},
	"getBalance":             {},
	"getBlock":               {},
	"getBlockHeight":         {},
	"getSlot":                {},
	"getTransaction":         {},
	"getSignatureStatuses":   {},
	"getLatestBlockhash":     {},
	"getEpochInfo":           {},
	"getVersion":             {},
	"getGenesisHash":         {},
	"getHealth":              {},
	"getTokenAccountBalance": {},
}

// Coalesceable reports whether identical concurrent Solana calls may be deduplicated.
func (c *Chain) Coalesceable(method string) bool {
	_, ok := coalesceable[method]
	return ok
}

// AllowMethod allows all methods; Solana endpoints are not filtered.
func (c *Chain) AllowMethod(method string) bool {
	return true
}

func (c *Chain) HandleError(code int, message string) error {
	switch code {
	// Node is unhealthy / behind by slots
	case -32005:
		return &rpc.ErrorRateLimited{Message: message}
	// Standard JSON-RPC internal error
	case -32603:
		return fmt.Errorf("internal error: %s", message)
	}

	// -32009 Slot was skipped or missing in long-term storage - check next provider
	// -32014 Block status not yet available - check next provider

	return nil
}

// Healthcheck batches the node-info RPCs (version, genesis hash, block height)
// together with the chain-specific getHealth check. The returned NodeInfo is
// populated with everything parsed so far, even when an error is returned.
func (c *Chain) Healthcheck(ctx context.Context, p *core.Provider) (*core.NodeInfo, error) {
	res, err := httpx.SendRPCBatchRequest(ctx, p, rpc.NewBatchRequest(
		rpc.NewRequest("ha_version", "getVersion", nil),
		rpc.NewRequest("ha_chainId", "getGenesisHash", nil),
		rpc.NewRequest("ha_height", "getBlockHeight", nil),
		rpc.NewRequest("ha_health", "getHealth", nil),
	))
	if err != nil {
		return nil, err
	}

	info := &core.NodeInfo{}

	clientVersion, err := getClientVersion(res.GetResponseByID("ha_version"))
	if err != nil {
		return info, err
	}
	info.ClientVersion = clientVersion

	chainID, err := getChainId(res.GetResponseByID("ha_chainId"))
	if err != nil {
		return info, err
	}
	info.ChainID = chainID

	blockHeight, err := getBlockHeight(res.GetResponseByID("ha_height"))
	if err != nil {
		return info, err
	}
	info.BlockHeight = blockHeight

	// Chain-specific healthcheck once the node info is populated.
	if err := checkHealth(res.GetResponseByID("ha_health")); err != nil {
		return info, err
	}

	return info, nil
}

// solanaVersion is the subset of the getVersion result we read.
type solanaVersion struct {
	// SolanaCore is the node software version, e.g. "1.18.15".
	SolanaCore string `json:"solana-core"`
}

// getVersion returns an object; we only need the solana-core field.
func getClientVersion(res *rpc.Response) (string, error) {
	version, err := rpc.DecodeResult[solanaVersion](res)
	if err != nil {
		return "", fmt.Errorf("getVersion failed: %w", err)
	}
	if version.SolanaCore == "" {
		return "", fmt.Errorf("no client version")
	}
	return version.SolanaCore, nil
}

// getGenesisHash returns the genesis hash string, which we map to a network name.
func getChainId(res *rpc.Response) (string, error) {
	hash, err := rpc.DecodeResult[string](res)
	if err != nil {
		return "", fmt.Errorf("getGenesisHash failed: %w", err)
	}
	return genesisHash[*hash], nil
}

// getBlockHeight returns a JSON number; DecodeResult[uint64] decodes it directly
// so large heights don't lose precision through a float64.
func getBlockHeight(res *rpc.Response) (uint64, error) {
	height, err := rpc.DecodeResult[uint64](res)
	if err != nil {
		return 0, fmt.Errorf("getBlockHeight failed: %w", err)
	}
	return *height, nil
}

// getHealth returns the string "ok" on a healthy node.
func checkHealth(res *rpc.Response) error {
	status, err := rpc.DecodeResult[string](res)
	if err != nil {
		return fmt.Errorf("getHealth failed: %w", err)
	}
	if *status != "ok" {
		return fmt.Errorf("node is not healthy: %s", *status)
	}
	return nil
}
