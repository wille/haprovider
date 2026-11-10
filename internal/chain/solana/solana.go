package solana

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/wille/haprovider/internal/core"
	"github.com/wille/haprovider/internal/httpx"
	"github.com/wille/haprovider/internal/rpc"
	"github.com/wille/haprovider/internal/verify"
)

var genesisHash = map[string]string{
	"5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d": "mainnet",
	"4uhcVJyU9pJkvQyS88uRDiswHXSCkY3zQawwpjk2NsNY": "testnet",
	"EtWTRABZaYq6iMfeYKouRu166VU2xqa1wcaWoxPkrZBG": "devnet",
}

type Chain struct{}

// ValidateConfig accepts a known network name, or the genesis hash of a
// network without one (e.g. a local test validator).
func (c *Chain) ValidateConfig(e *core.Endpoint) error {
	if e.ChainID == "" || isGenesisHash(e.ChainID) || slices.Contains(slices.Collect(maps.Values(genesisHash)), e.ChainID) {
		return nil
	}
	return fmt.Errorf("invalid chainId: %s (want mainnet, devnet, testnet, or a genesis hash)", e.ChainID)
}

// base58Alphabet is the alphabet Solana encodes hashes and keys in.
const base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// isGenesisHash reports whether s looks like a base58-encoded 32-byte hash,
// which takes 32 to 44 characters.
func isGenesisHash(s string) bool {
	if len(s) < 32 || len(s) > 44 {
		return false
	}
	for _, r := range s {
		if !strings.ContainsRune(base58Alphabet, r) {
			return false
		}
	}
	return true
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

// verifiers are the Solana methods cross-checked against a second provider
// when an endpoint has verify enabled: the reads a payment processor credits
// deposits from. SOL and token account balances, and the chain height
// confirmations are counted from.
var verifiers = map[string]verify.Func{
	"getBlockHeight":         verify.Lowest(parseUint),
	"getSlot":                verify.Lowest(parseUint),
	"getBalance":             verify.Equal(contextValue),
	"getAccountInfo":         verify.Equal(contextValue),
	"getMultipleAccounts":    verify.Equal(contextValue),
	"getTokenAccountBalance": verify.Equal(tokenAmount),
}

// heights are the cross-checked methods that return a height, which are
// cross-checked at any commitment: the lower one is returned, so they never
// disagree.
var heights = map[string]bool{
	"getBlockHeight": true,
	"getSlot":        true,
}

// Verifier returns how to cross-check the result of a Solana call, or nil if
// it isn't cross-checked. Account reads at processed commitment are not: each
// node's view of blocks that haven't been voted on yet is its own (and may be
// on a fork that gets dropped), so honest providers rarely agree on them.
func (c *Chain) Verifier(method string, params json.RawMessage) verify.Func {
	v := verifiers[method]
	if v == nil {
		return nil
	}
	if !heights[method] && commitment(params) == "processed" {
		return nil
	}
	return v
}

// commitment returns the commitment level a call's config object asks for,
// or "" if none (the node's default).
func commitment(params json.RawMessage) string {
	var args []json.RawMessage
	if err := json.Unmarshal(params, &args); err != nil {
		return ""
	}
	for _, arg := range args {
		var config struct {
			Commitment string `json:"commitment"`
		}
		if json.Unmarshal(arg, &config) == nil && config.Commitment != "" {
			return config.Commitment
		}
	}
	return ""
}

// parseUint reads a JSON number such as a block height or slot.
func parseUint(raw json.RawMessage) (uint64, error) {
	var n uint64
	err := json.Unmarshal(raw, &n)
	return n, err
}

// contextValue reads the value of an RpcResponse ({"context":{...},"value":...}).
// The context is left out: its slot almost always differs between providers,
// even when the value is the same.
func contextValue(raw json.RawMessage) (any, error) {
	var res map[string]json.RawMessage
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}
	value, ok := res["value"]
	if !ok {
		return nil, fmt.Errorf("no value in result")
	}
	return verify.JSON(value)
}

// tokenAmount reads the raw amount of a getTokenAccountBalance result. Only
// the integer amount is compared: uiAmount is a float whose formatting may
// differ between node implementations.
func tokenAmount(raw json.RawMessage) (any, error) {
	var res struct {
		Value *struct {
			Amount *string `json:"amount"`
		} `json:"value"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}
	if res.Value == nil || res.Value.Amount == nil {
		return nil, fmt.Errorf("no value.amount in result")
	}
	return *res.Value.Amount, nil
}

// AllowMethod allows all methods; Solana endpoints are not filtered.
func (c *Chain) AllowMethod(method string) bool {
	return true
}

func (c *Chain) HandleError(code int, message string) error {
	switch code {
	// Standard JSON-RPC "Method not found": the node doesn't implement/enable it.
	case -32601:
		return &rpc.MethodNotSupportedError{Code: code, Message: message}
	// Node is unhealthy / behind by slots.
	case -32005:
		return &rpc.ErrorRateLimited{Message: message}
	// Standard JSON-RPC internal error.
	case -32603:
		return fmt.Errorf("internal error: %s", message)
	// The node doesn't have the requested data (pruned / skipped / behind), but an
	// archival or more in-sync provider may. Try the next provider without marking
	// this one unhealthy or caching anything; if none can serve it, the error is
	// returned to the client. Codes from Agave rpc-client-api custom_error.rs:
	//   -32001 block cleaned up (pruned)
	//   -32004 block not available for slot
	//   -32007 slot skipped or missing (ledger jump to recent snapshot)
	//   -32008 no snapshot
	//   -32009 slot skipped or missing in long-term storage
	//   -32011 transaction history not available from this node
	//   -32014 block status not yet available
	//   -32016 minimum context slot not reached (node behind)
	case -32001, -32004, -32007, -32008, -32009, -32011, -32014, -32016:
		return &rpc.ErrorTryNextProvider{Message: message}
	}

	// Everything else is a client- or transaction-level error returned to the
	// client unchanged: parse / invalid-request / invalid-params (-32700 / -32600 /
	// -32602), tx preflight / signature / precompile / version failures (-32002 /
	// -32003 / -32006 / -32015), secondary-index exclusion (-32010), signature
	// length mismatch (-32013), and account-scan errors (-32012, e.g. an oversized
	// getProgramAccounts scan — a client condition, so we must not mark the healthy
	// provider unhealthy over it).
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
//
// A network we don't know by name is identified by the hash itself, so a node
// of an unknown network never passes for one we do (or for an endpoint without
// a chainId).
func getChainId(res *rpc.Response) (string, error) {
	hash, err := rpc.DecodeResult[string](res)
	if err != nil {
		return "", fmt.Errorf("getGenesisHash failed: %w", err)
	}
	if name, ok := genesisHash[*hash]; ok {
		return name, nil
	}
	if !isGenesisHash(*hash) {
		return "", fmt.Errorf("invalid genesis hash: %q", *hash)
	}
	return *hash, nil
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
