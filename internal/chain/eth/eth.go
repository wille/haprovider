package eth

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/wille/haprovider/internal/core"
	"github.com/wille/haprovider/internal/httpx"
	"github.com/wille/haprovider/internal/rpc"
)

const (
	// codeRateLimited is the JSON-RPC error code returned when a provider is rate limiting us.
	codeRateLimited = -32005

	// codeInternalError is the standard JSON-RPC internal error code.
	codeInternalError = -32603

	// This error occurs when there is a problem with the connection between the client and server, such as a timeout or a dropped connection
	codeNetworkError = -32011
)

type Ethereum struct{}

func (c *Ethereum) HandleError(code int, message string) error {
	switch code {
	case codeRateLimited:
		return &rpc.ErrorRateLimited{Message: message}
	case codeInternalError:
		return fmt.Errorf("internal error: %s", message)
	case codeNetworkError:
		return fmt.Errorf("provider network error: %s", message)
	}

	return nil
}

func (c *Ethereum) CacheableRequest(method string, params json.RawMessage) bool {
	return CacheableRequest(method, params)
}

func (c *Ethereum) CacheableResponse(method string, result json.RawMessage) bool {
	return CacheableResponse(method, result)
}

func (c *Ethereum) Coalesceable(method string) bool {
	return Coalesceable(method)
}

// coalesceable is the allowlist of EVM JSON-RPC methods that are safe to collapse
// into a single upstream call (read-only and idempotent). Anything not listed is
// not coalesced. Shared by the Ethereum and Tron chains (Tron speaks EVM JSON-RPC).
var coalesceable = map[string]struct{}{
	"eth_call":                  {},
	"eth_getBalance":            {},
	"eth_getCode":               {},
	"eth_getStorageAt":          {},
	"eth_getTransactionByHash":  {},
	"eth_getTransactionReceipt": {},
	"eth_getBlockByNumber":      {},
	"eth_getBlockByHash":        {},
	"eth_getBlockReceipts":      {},
	"eth_getLogs":               {},
	"eth_getTransactionCount":   {},
	"eth_estimateGas":           {},
	"eth_gasPrice":              {},
	"eth_feeHistory":            {},
	"eth_maxPriorityFeePerGas":  {},
	"eth_blockNumber":           {},
	"eth_chainId":               {},
	"net_version":               {},
	"web3_clientVersion":        {},
}

// Coalesceable reports whether identical concurrent calls to an EVM JSON-RPC
// method may be deduplicated into a single upstream request. Exported so the
// Tron chain (which speaks the EVM JSON-RPC surface) can reuse the same policy.
func Coalesceable(method string) bool {
	_, ok := coalesceable[method]
	return ok
}

func (c Ethereum) ValidateConfig(e *core.Endpoint) error {
	if e.ChainID != "" {
		_, err := strconv.ParseUint(e.ChainID, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid chainId: %s", e.ChainID)
		}
	}
	return nil
}

// Healthcheck batches the node-info RPCs (client version, chain id, block height)
// together with the chain-specific eth_syncing check. The returned NodeInfo is
// populated with everything parsed so far, even when an error is returned.
func (c *Ethereum) Healthcheck(ctx context.Context, p *core.Provider) (*core.NodeInfo, error) {
	res, err := httpx.SendRPCBatchRequest(ctx, p, rpc.NewBatchRequest(
		rpc.NewRequest("ha_clientVersion", "web3_clientVersion", []string{}),
		rpc.NewRequest("ha_chainId", "eth_chainId", nil),
		rpc.NewRequest("ha_height", "eth_blockNumber", nil),
		rpc.NewRequest("ha_syncing", "eth_syncing", nil),
	))
	if err != nil {
		return nil, err
	}

	// Some providers return a single error response for a batch request like when rate limiting
	for _, r := range res.Responses {
		if r.IsError() {
			_, err := r.GetError()
			return nil, err
		}
	}

	info := &core.NodeInfo{}

	clientVersion, err := getClientVersion(res.GetResponseByID("ha_clientVersion"))
	if err != nil {
		return info, err
	}
	info.ClientVersion = clientVersion

	chainID, err := getChainID(res.GetResponseByID("ha_chainId"))
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
	if err := checkSyncing(res.GetResponseByID("ha_syncing")); err != nil {
		return info, err
	}

	return info, nil
}

// ethSyncing is the object form of the eth_syncing result, returned while a node
// is still catching up. A fully-synced node returns the JSON literal `false`
// instead, so this is only decoded after the `false` case is ruled out.
type ethSyncing struct {
	// CurrentBlock is the node's current block, as a hex string (e.g. "0x1a").
	CurrentBlock string `json:"currentBlock"`
	// HighestBlock is the estimated chain tip the node is syncing towards.
	HighestBlock string `json:"highestBlock"`
}

// web3_clientVersion returns the node software version as a plain string.
func getClientVersion(res *rpc.Response) (string, error) {
	version, err := rpc.DecodeResult[string](res)
	if err != nil {
		return "", fmt.Errorf("web3_clientVersion failed: %w", err)
	}
	return *version, nil
}

// eth_chainId returns the chain id as a hex string; we normalize it to decimal.
func getChainID(res *rpc.Response) (string, error) {
	chainID, err := rpc.DecodeResult[string](res)
	if err != nil {
		return "", fmt.Errorf("eth_chainId failed: %w", err)
	}

	n, err := ParseHexNumber(*chainID)
	if err != nil {
		return "", fmt.Errorf("failed to parse chain id: %v (%s)", err, *chainID)
	}
	return strconv.FormatUint(n, 10), nil
}

// eth_blockNumber returns the latest block height as a hex string.
func getBlockHeight(res *rpc.Response) (uint64, error) {
	height, err := rpc.DecodeResult[string](res)
	if err != nil {
		return 0, fmt.Errorf("eth_blockNumber failed: %w", err)
	}

	n, err := ParseHexNumber(*height)
	if err != nil {
		return 0, fmt.Errorf("failed to parse block height: %v (%s)", err, *height)
	}
	return n, nil
}

func checkSyncing(res *rpc.Response) error {
	// A fully-synced node returns the literal `false`.
	if synced, err := rpc.DecodeResult[bool](res); err == nil {
		if !*synced {
			return nil
		}
		return fmt.Errorf("node is syncing")
	}

	// Otherwise it returns a progress object.
	syncing, err := rpc.DecodeResult[ethSyncing](res)
	if err != nil {
		return fmt.Errorf("eth_syncing failed: %w", err)
	}

	current, _ := ParseHexNumber(syncing.CurrentBlock)
	highest, _ := ParseHexNumber(syncing.HighestBlock)
	return fmt.Errorf("node is not synced: currentBlock=%d, highestBlock=%d", current, highest)
}

// ParseHexNumber parses a hex string and returns decimal number
// Note that this function does not handle large Ethereum balances in wei.
func ParseHexNumber(hex string) (uint64, error) {
	if !strings.HasPrefix(hex, "0x") {
		return 0, fmt.Errorf("hex number must start with 0x")
	}

	i, err := strconv.ParseUint(hex[2:], 16, 64)
	if err != nil {
		return 0, err
	}

	if i == math.MaxUint64 {
		return 0, fmt.Errorf("number is too large")
	}

	return i, nil
}
