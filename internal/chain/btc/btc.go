package btc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"

	"github.com/wille/haprovider/internal/core"
	"github.com/wille/haprovider/internal/httpx"
	"github.com/wille/haprovider/internal/rpc"
)

var genesisBlocks = map[string]string{
	"000000000019d6689c085ae165831e934ff763ae46a2a6c172b3f1b60a8ce26f":  "mainnet",
	"000000000933ea01ad0ee984209779baaec3ced07f354f05b0d0c3d9b07117560": "testnet3",
	"00000000da84f2bafbbc53dee25a72ae507ff4914b867c565be350b0da8bf043":  "testnet4",
	"00000008819873e925422c1ff0f99f7cc9bbb232af63a077a480a3633bee1ef6":  "signet",
}

type Chain struct{}

// cacheable is the allowlist of Bitcoin RPC methods whose responses may be
// cached. Restricted to by-hash lookups whose result is immutable; verbose
// getrawtransaction is excluded because its "confirmations" field changes.
var cacheable = map[string]struct{}{
	"getblock":       {},
	"getblockheader": {},
	"getblockhash":   {},
}

func (c *Chain) CacheableRequest(method string, _ json.RawMessage) bool {
	_, ok := cacheable[method]
	return ok
}

// CacheableResponse caches any non-null, non-empty successful result.
func (c *Chain) CacheableResponse(_ string, result json.RawMessage) bool {
	trimmed := bytes.TrimSpace(result)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null"))
}

type networkInfo struct {
	Subversion string `json:"subversion"`
}

// blockchainInfo is the subset of the getblockchaininfo result we care about.
type blockchainInfo struct {
	// Blocks is the number of blocks synced
	Blocks uint64 `json:"blocks"`

	// Headers is the number of blocks known by the node
	Headers uint64 `json:"headers"`

	// VerificationProgress is the progress of the sync (0.0 to 1.0)
	VerificationProgress float64 `json:"verificationprogress"`

	// InitialBlockDownload is true if the node is in initial block download
	InitialBlockDownload bool `json:"initialblockdownload"`
}

type mempoolInfo struct {
	Loaded bool `json:"loaded"`
}

func (c *Chain) Healthcheck(ctx context.Context, p *core.Provider) (*core.NodeInfo, error) {
	res, err := httpx.SendRPCBatchRequest(ctx, p, rpc.NewBatchRequest(
		rpc.NewRequest("getnetworkinfo", "getnetworkinfo", []any{}),
		rpc.NewRequest("getblockhash", "getblockhash", []any{0}),
		rpc.NewRequest("getblockchaininfo", "getblockchaininfo", []any{}),
		rpc.NewRequest("getmempoolinfo", "getmempoolinfo", []any{}),
	))
	if err != nil {
		return nil, err
	}

	info := &core.NodeInfo{}

	blockchainInfo, err := rpc.DecodeResult[blockchainInfo](res.GetResponseByID("getblockchaininfo"))
	if err != nil {
		return info, fmt.Errorf("failed to parse getblockchaininfo: %w", err)
	}
	info.BlockHeight = blockchainInfo.Blocks

	networkInfo, err := rpc.DecodeResult[networkInfo](res.GetResponseByID("getnetworkinfo"))
	if err != nil {
		return info, fmt.Errorf("failed to parse getnetworkinfo: %w", err)
	}
	info.ClientVersion = networkInfo.Subversion

	blockHash, err := rpc.DecodeResult[string](res.GetResponseByID("getblockhash"))
	if err != nil {
		return info, fmt.Errorf("failed to parse getblockhash: %w", err)
	}
	info.ChainID = genesisBlocks[*blockHash]

	// Rest of the healthchecks when node info is populated

	if blockchainInfo.Blocks < blockchainInfo.Headers {
		return info, fmt.Errorf("node is behind the header tip: blocks=%d, headers=%d, progress=%.4f", blockchainInfo.Blocks, blockchainInfo.Headers, blockchainInfo.VerificationProgress)
	}

	mempoolInfo, err := rpc.DecodeResult[mempoolInfo](res.GetResponseByID("getmempoolinfo"))
	if err != nil {
		return info, fmt.Errorf("failed to parse getmempoolinfo: %w", err)
	}
	if !mempoolInfo.Loaded {
		return info, fmt.Errorf("mempool is not loaded")
	}

	return info, nil
}

// https://github.com/bitcoin/bitcoin/blob/master/src/rpc/protocol.h
func (c *Chain) HandleError(code int, message string) error {
	switch code {
	// RPC_INTERNAL_ERROR
	case -32603:
		return fmt.Errorf("internal error: %s", message)
	}
	return nil
}

func (c *Chain) ValidateConfig(e *core.Endpoint) error {
	if e.ChainID != "" {
		for v := range maps.Values(genesisBlocks) {
			if v == e.ChainID {
				return nil
			}
		}
		return fmt.Errorf("invalid chainId: %s", e.ChainID)
	}

	return nil
}
