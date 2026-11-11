package eth

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"

	"github.com/wille/haprovider/internal/broadcast"
	"github.com/wille/haprovider/internal/core"
	"github.com/wille/haprovider/internal/httpx"
	"github.com/wille/haprovider/internal/rpc"
	"github.com/wille/haprovider/internal/verify"
	"golang.org/x/crypto/sha3"
)

const (
	// Method-not-supported codes. -32601 is the JSON-RPC standard "Method not
	// found"; -32604 and -32004 are used by some providers (e.g. QuickNode, the
	// latter on some EVM chains like Arbitrum) for a method the endpoint/plan
	// doesn't support. -32612/-32613 are QuickNode's "custom traces not allowed"
	// (debug/trace methods gated behind the plan).
	codeMethodNotFound        = -32601
	codeMethodNotSupported    = -32604
	codeMethodNotSupportedEVM = -32004
	codeTracesBlocked         = -32612
	codeTracesNotAllowed      = -32613

	// codeServerError (-32000) is Geth's catch-all "server error"; its meaning is
	// in the message, not the code (see HandleError).
	codeServerError = -32000

	// Data-availability codes (EIP-1474): the node doesn't have the requested
	// resource, but an archival or more in-sync provider may.
	codeResourceNotFound    = -32001
	codeResourceUnavailable = -32002

	// codeRateLimited is the JSON-RPC error code returned when a provider is rate limiting us.
	codeRateLimited = -32005

	// codeInternalError is the standard JSON-RPC internal error code.
	codeInternalError = -32603

	// This error occurs when there is a problem with the connection between the client and server, such as a timeout or a dropped connection
	codeNetworkError = -32011
)

// dataUnavailableHints are substrings that appear in a Geth -32000 message when
// the node simply doesn't have the requested data yet: pruned state, a block it
// hasn't synced, an archival miss, or transaction indexing still in progress.
// -32000 is Geth's catch-all code (it also covers nonce/gas errors, reverts,
// timeouts, filter-not-found), so the disposition must come from the message.
var dataUnavailableHints = []string{
	"missing trie node",
	"header not found",
	"block not found",
	"historical state",
	"state not available",
	"state is not available",
	"no state available",
	"not synced",
	"not in sync",
	"indexing",
}

func isDataUnavailable(message string) bool {
	m := strings.ToLower(message)
	for _, hint := range dataUnavailableHints {
		if strings.Contains(m, hint) {
			return true
		}
	}
	return false
}

type Ethereum struct{}

func (c *Ethereum) HandleError(code int, message string) error {
	switch code {
	case codeMethodNotFound, codeMethodNotSupported, codeMethodNotSupportedEVM, codeTracesBlocked, codeTracesNotAllowed:
		return &rpc.MethodNotSupportedError{Code: code, Message: message}
	case codeRateLimited:
		return &rpc.ErrorRateLimited{Message: message}
	case codeInternalError:
		return fmt.Errorf("internal error: %s", message)
	case codeNetworkError:
		return fmt.Errorf("provider network error: %s", message)
	case codeResourceNotFound, codeResourceUnavailable:
		// The node doesn't have the requested resource (or timed out); an archival
		// or more in-sync provider may. Try the next provider without marking this
		// one unhealthy or caching anything.
		return &rpc.ErrorTryNextProvider{Message: message}
	case codeServerError:
		// -32000 is Geth's catch-all: only the pruned/archive/not-synced messages
		// mean another provider may serve it. Everything else (reverts, nonce/gas
		// errors, timeouts, filter-not-found) is a client-level result returned as-is.
		if isDataUnavailable(message) {
			return &rpc.ErrorTryNextProvider{Message: message}
		}
	}

	return nil
}

func (c *Ethereum) Coalesceable(method string) bool {
	return Coalesceable(method)
}

// AllowMethod allows all methods; Ethereum/EVM endpoints are not filtered.
func (c *Ethereum) AllowMethod(method string) bool {
	return true
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

func (c *Ethereum) Verifier(method string, params json.RawMessage) verify.Func {
	return Verifier(method, params)
}

// verifiers are the EVM JSON-RPC methods cross-checked against a second
// provider when an endpoint has verify enabled: the reads a payment processor
// credits deposits from. Native balances, contract reads (ERC-20 balanceOf,
// multicall), and the chain height that confirmations are counted from.
var verifiers = map[string]verify.Func{
	"eth_blockNumber": verify.Lowest(parseQuantity),
	"eth_getBalance":  verify.Equal(quantity),
	"eth_call":        verify.Equal(data),
}

// blockTagParam is the position of the block parameter of the cross-checked
// methods that take one.
var blockTagParam = map[string]int{
	"eth_getBalance": 1,
	"eth_call":       1,
}

// Verifier returns how to cross-check the result of an EVM JSON-RPC call, or
// nil if it isn't cross-checked. Reads at the pending block are not: each node
// builds its pending block from its own mempool, so honest providers rarely
// agree on them. Exported so the Tron chain can reuse it.
func Verifier(method string, params json.RawMessage) verify.Func {
	v := verifiers[method]
	if v == nil {
		return nil
	}
	if pos, ok := blockTagParam[method]; ok && blockTag(params, pos) == "pending" {
		return nil
	}
	return v
}

// blockTag returns the block tag string ("latest", "pending", ...) at
// position pos of params, or "" if there is none there (an omitted block
// defaults to latest; an EIP-1898 block object or a number names a block).
func blockTag(params json.RawMessage, pos int) string {
	var args []json.RawMessage
	if err := json.Unmarshal(params, &args); err != nil || pos >= len(args) {
		return ""
	}
	var tag string
	if err := json.Unmarshal(args[pos], &tag); err != nil {
		return ""
	}
	return tag
}

// parseQuantity reads a JSON hex quantity (e.g. "0x1a") as a uint64.
func parseQuantity(raw json.RawMessage) (uint64, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return 0, err
	}
	return ParseHexNumber(s)
}

// quantity reads a JSON hex quantity of any size (e.g. a balance in wei) as a
// decimal string, so "0x0a" and "0xa" compare equal.
func quantity(raw json.RawMessage) (any, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	digits, ok := strings.CutPrefix(s, "0x")
	if !ok || digits == "" {
		return nil, fmt.Errorf("not a hex quantity: %q", s)
	}
	n, ok := new(big.Int).SetString(digits, 16)
	if !ok {
		return nil, fmt.Errorf("not a hex quantity: %q", s)
	}
	return n.String(), nil
}

// data reads JSON hex data (e.g. an eth_call return value) in lower case, so
// results that differ only in letter case compare equal.
func data(raw json.RawMessage) (any, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	digits, ok := strings.CutPrefix(s, "0x")
	if !ok {
		return nil, fmt.Errorf("not hex data: %q", s)
	}
	if _, err := hex.DecodeString(digits); err != nil {
		return nil, fmt.Errorf("not hex data: %q", s)
	}
	return strings.ToLower(s), nil
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

func (c *Ethereum) Broadcaster(method string) *broadcast.Spec {
	return Broadcaster(method)
}

// sendRawTransaction broadcasts eth_sendRawTransaction calls. "Already known"
// is how geth, reth and the providers built on them say it ("already known"),
// as do Erigon ("ALREADY_EXISTS"), Nethermind ("AlreadyKnown") and Besu
// ("Known transaction").
var sendRawTransaction = &broadcast.Spec{
	Hash:     rawTransactionHash,
	SameHash: strings.EqualFold,
	AlreadyKnown: func(message string) bool {
		return broadcast.ContainsAny(message, "already known", "alreadyknown", "already_exists", "already exists", "known transaction", "already imported")
	},
}

// Broadcaster returns how to broadcast an EVM JSON-RPC method to every
// provider, or nil if it doesn't submit a transaction.
func Broadcaster(method string) *broadcast.Spec {
	if method == "eth_sendRawTransaction" {
		return sendRawTransaction
	}
	return nil
}

// rawTransactionHash returns the hash of the signed transaction an
// eth_sendRawTransaction call submits: the keccak256 of its raw bytes, which
// holds for legacy and typed (EIP-2718) transactions alike.
func rawTransactionHash(params json.RawMessage) (string, error) {
	var args []string
	if err := json.Unmarshal(params, &args); err != nil || len(args) != 1 {
		return "", fmt.Errorf("want one raw transaction")
	}
	digits, ok := strings.CutPrefix(args[0], "0x")
	if !ok {
		return "", fmt.Errorf("raw transaction is not 0x-prefixed hex")
	}
	raw, err := hex.DecodeString(digits)
	if err != nil || len(raw) == 0 {
		return "", fmt.Errorf("raw transaction is not hex")
	}

	h := sha3.NewLegacyKeccak256()
	h.Write(raw)
	return "0x" + hex.EncodeToString(h.Sum(nil)), nil
}
