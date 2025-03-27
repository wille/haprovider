package internal

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"time"

	"github.com/wille/haprovider/internal/rpc"
)

const (
	DefaultHealthcheckInterval = 10 * time.Second

	RateLimited = -32005
	// -32603	Internal JSON-RPC error	This error is typically due to a bad or invalid payload
)

var ErrorCodeText = map[int]string{
	RateLimited: "rate limited",
}

type Healthcheck func(ctx context.Context, p *Endpoint, e *Provider, read rpc.ReaderFunc) error

var (
	_ Healthcheck = EthereumHealthCheck
	_ Healthcheck = SolanaHealthcheck
)

func EthereumHealthCheck(ctx context.Context, p *Endpoint, e *Provider, read rpc.ReaderFunc) error {
	clientVersion, err := read(ctx, rpc.NewRequest("ha_clientVersion", "web3_clientVersion", []string{}), false)
	if err != nil {
		return err
	}

	// If the 'web3' api is not enabled on the node, the clientVersion will be an empty string
	if version, ok := clientVersion.Result.(string); ok {
		if e.clientVersion != "" && e.clientVersion != version {
			slog.Info("clientVersion changed", "old", e.clientVersion, "new", version)
		}

		e.clientVersion = version
	}

	res, err := read(ctx, rpc.NewRequest("ha_chainId", "eth_chainId", nil), true)
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

	res, err = read(ctx, rpc.NewRequest("ha_height", "eth_blockNumber", nil), true)
	if err != nil {
		return err
	}

	if _, ok := res.Result.(string); !ok {
		return fmt.Errorf("block number is not a string")
	}

	return nil
}

func SolanaHealthcheck(ctx context.Context, provider *Endpoint, endpoint *Provider, read rpc.ReaderFunc) error {
	res, err := read(ctx, rpc.NewRequest("ha_version", "getVersion", nil), true)
	if err != nil {
		return err
	}

	if r, ok := res.Result.(map[string]any); ok {
		if version, ok := r["solana-core"].(string); ok {
			if endpoint.clientVersion != "" && endpoint.clientVersion != version {
				slog.Info("clientVersion changed", "old", endpoint.clientVersion, "new", version)
			}
			endpoint.clientVersion = version
		}
	}

	res, err = read(ctx, rpc.NewRequest("ha_health", "getHealth", nil), true)
	if err != nil {
		return err
	}

	res, err = read(ctx, rpc.NewRequest("ha_height", "getBlockHeight", nil), true)
	if err != nil {
		return err
	}

	if _, ok := res.Result.(float64); !ok {
		return fmt.Errorf("block height is not a number")
	}

	return nil
}
