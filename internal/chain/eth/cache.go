package eth

import (
	"bytes"
	"encoding/json"
	"strings"
)

// cacheable is the allowlist of EVM JSON-RPC methods whose responses may be
// cached: read-only calls whose result is immutable for given params (subject to
// the caller's block-tag guard and a TTL). Shared by the Ethereum and Tron
// chains (Tron speaks the EVM JSON-RPC surface).
var cacheable = map[string]struct{}{
	"eth_chainId":                            {},
	"net_version":                            {},
	"web3_clientVersion":                     {},
	"eth_getBlockByHash":                     {},
	"eth_getBlockByNumber":                   {},
	"eth_getBlockReceipts":                   {},
	"eth_getTransactionByHash":               {},
	"eth_getTransactionReceipt":              {},
	"eth_getTransactionByBlockHashAndIndex":  {},
	"eth_getUncleByBlockHashAndIndex":        {},
	"eth_getBlockTransactionCountByHash":     {},
}

// mutableBlockTags are EVM block references whose target changes over time; a
// response for a request referencing one must not be cached.
var mutableBlockTags = []string{"latest", "pending", "safe", "finalized"}

// CacheableRequest reports whether an EVM JSON-RPC request (method + params) may be
// cached: the method must be on the allowlist and the params must not reference
// a mutable block tag (latest/pending/safe/finalized). The tag check is
// conservative: a false positive only costs a missed cache, never a stale answer.
func CacheableRequest(method string, params json.RawMessage) bool {
	if _, ok := cacheable[method]; !ok {
		return false
	}
	s := string(params)
	for _, tag := range mutableBlockTags {
		if strings.Contains(s, `"`+tag+`"`) {
			return false
		}
	}
	return true
}

// CacheableResponse reports whether a specific successful result may be stored.
// Never caches a null/empty result (a missing block/tx or absent receipt). For
// eth_getTransactionByHash it additionally requires the transaction to be
// confirmed (non-null blockNumber): a pending transaction's response is not yet
// immutable and would otherwise be cached with a null blockNumber.
func CacheableResponse(method string, result json.RawMessage) bool {
	if isNullResult(result) {
		return false
	}
	if method == "eth_getTransactionByHash" {
		return hasNonNullField(result, "blockNumber")
	}
	return true
}

func isNullResult(result json.RawMessage) bool {
	trimmed := bytes.TrimSpace(result)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}

// hasNonNullField reports whether the JSON object result has field set to a
// non-null value.
func hasNonNullField(result json.RawMessage, field string) bool {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(result, &obj); err != nil {
		return false
	}
	v, ok := obj[field]
	return ok && !bytes.Equal(bytes.TrimSpace(v), []byte("null"))
}
