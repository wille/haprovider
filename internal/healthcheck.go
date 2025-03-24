package internal

import (
	"context"
	"fmt"
	"log"
)

const (
	RateLimited = -32005
	// -32603	Internal JSON-RPC error	This error is typically due to a bad or invalid payload
)

var ErrorCodeText = map[int]string{
	RateLimited: "rate limited",
}

type Healthcheck func(ctx context.Context, p *Provider, e *Endpoint, read RPCReaderFunc) error

var (
	_ Healthcheck = EthereumHealthCheck
	_ Healthcheck = SolanaHealthcheck
)

func EthereumHealthCheck(ctx context.Context, p *Provider, e *Endpoint, read RPCReaderFunc) error {
	clientVersion, err := read(ctx, NewRPCRequest("ha_clientVersion", "web3_clientVersion", []string{}), true)
	if err != nil {
		e.SetStatus(false, err)
		return err
	}

	// If the 'web3' api is not enabled on the node, the clientVersion will be an empty string
	if _, ok := clientVersion.Result.(string); ok {
		e.clientVersion = clientVersion.Result.(string)
	}

	res, err := read(ctx, NewRPCRequest("ha_chainId", "eth_chainId", nil), true)
	if err != nil {
		return err
	}

	if p.ChainID != 0 {
		ourChainId := fmt.Sprintf("0x%x", p.ChainID)
		if res.Result.(string) != ourChainId {
			err = fmt.Errorf("chainId mismatch: received=%s, expected=%s", res.Result, ourChainId)
			return err
		}
	} else {
		log.Printf("chainId is not set, received=%s", res.Result)
	}

	res, err = read(ctx, NewRPCRequest("ha_height", "eth_blockNumber", nil), true)
	if err != nil {
		return err
	}

	if _, ok := res.Result.(string); !ok {
		return fmt.Errorf("block number is not a string")
	}

	return nil
}

func SolanaHealthcheck(ctx context.Context, provider *Provider, endpoint *Endpoint, read RPCReaderFunc) error {
	res, err := read(ctx, NewRPCRequest("ha_version", "getVersion", nil), true)
	if err != nil {
		return err
	}

	if r, ok := res.Result.(map[string]any); ok {
		endpoint.clientVersion = r["solana-core"].(string)
	}

	res, err = read(ctx, NewRPCRequest("ha_health", "getHealth", nil), true)
	if err != nil {
		return err
	}

	res, err = read(ctx, NewRPCRequest("ha_height", "getBlockHeight", nil), true)
	if err != nil {
		return err
	}

	if _, ok := res.Result.(float64); !ok {
		return fmt.Errorf("block height is not a number")
	}

	return nil
}
