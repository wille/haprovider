package tron

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strings"

	"github.com/wille/haprovider/internal/chain/eth"
	"github.com/wille/haprovider/internal/core"
	"github.com/wille/haprovider/internal/httpx"
	"github.com/wille/haprovider/internal/rpc"
)

var chainIds = map[uint64]string{
	3448148188: "nile",
	728126428:  "mainnet",
	2494104990: "shasta",
}

// -32005 Limit Exceeded
// -32002 Internal Error
type Chain struct{}

func (c *Chain) HandleError(code int, message string) error {
	switch code {
	case -32005:
		return &rpc.ErrorRateLimited{Message: message}
	case -32002, -32603:
		return fmt.Errorf("internal error: %s", message)
	}

	return nil
}

// CacheableRequest and CacheableResponse delegate to the shared EVM policy: Tron speaks
// the EVM JSON-RPC surface.
func (c *Chain) CacheableRequest(method string, params json.RawMessage) bool {
	return eth.CacheableRequest(method, params)
}

func (c *Chain) CacheableResponse(method string, result json.RawMessage) bool {
	return eth.CacheableResponse(method, result)
}

// Healthcheck batches the node-info RPCs (client version, chain id, block
// height). Tron has no custom healthcheck beyond the node info. The returned
// NodeInfo is populated with everything parsed so far, even when an error is
// returned.
func (c *Chain) Healthcheck(ctx context.Context, p *core.Provider) (*core.NodeInfo, error) {
	res, err := httpx.SendRPCBatchRequest(ctx, p, rpc.NewBatchRequest(
		rpc.NewRequest("ha_clientVersion", "web3_clientVersion", []string{}),
		rpc.NewRequest("ha_chainId", "eth_chainId", nil),
		rpc.NewRequest("ha_height", "eth_blockNumber", nil),
	))
	if err != nil {
		return nil, err
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

	return info, nil
}

func (c *Chain) ValidateConfig(e *core.Endpoint) error {
	if e.ChainID != "" {
		for v := range maps.Values(chainIds) {
			if v == e.ChainID {
				return nil
			}
		}
		return fmt.Errorf("invalid chainId: %s", e.ChainID)
	}

	for _, p := range e.Providers {
		if !strings.HasSuffix(p.Http, "/jsonrpc") {
			return fmt.Errorf("provider %s http url must end with /jsonrpc and node must support the JSON-RPC API", p.Name)
		}
	}
	return nil
}

// Tron exposes an Ethereum-compatible JSON-RPC API, so all of the results we
// read are the same scalar shapes as eth: a version string and hex-encoded
// numbers. They're decoded with rpc.DecodeResult[string] rather than dedicated structs.

// web3_clientVersion returns the node software version as a plain string.
func getClientVersion(res *rpc.Response) (string, error) {
	version, err := rpc.DecodeResult[string](res)
	if err != nil {
		return "", fmt.Errorf("web3_clientVersion failed: %w", err)
	}
	return *version, nil
}

// eth_chainId returns a hex string we map to a known Tron network name.
func getChainID(res *rpc.Response) (string, error) {
	chainID, err := rpc.DecodeResult[string](res)
	if err != nil {
		return "", fmt.Errorf("eth_chainId failed: %w", err)
	}

	n, err := eth.ParseHexNumber(*chainID)
	if err != nil {
		return "", fmt.Errorf("failed to parse chain id: %v (%s)", err, *chainID)
	}

	name, ok := chainIds[n]
	if !ok {
		return "", fmt.Errorf("unknown tron chain id: %d", n)
	}
	return name, nil
}

// eth_blockNumber returns the latest block height as a hex string.
func getBlockHeight(res *rpc.Response) (uint64, error) {
	height, err := rpc.DecodeResult[string](res)
	if err != nil {
		return 0, fmt.Errorf("eth_blockNumber failed: %w", err)
	}

	n, err := eth.ParseHexNumber(*height)
	if err != nil {
		return 0, fmt.Errorf("failed to parse block height: %v (%s)", err, *height)
	}
	return n, nil
}
