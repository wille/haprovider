package internal

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"time"

	"github.com/wille/haprovider/internal/eth"
	"github.com/wille/haprovider/internal/rpc"
)

const (
	DefaultHealthcheckInterval = 10 * time.Second

	// Rate limited
	EthErrorRateLimited = -32005

	// Internal JSON-RPC error
	EthErrorInternalError = -32603

	BlockLagTolerance = 50
)

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

	if currentBlockHex, ok := res.Result.(string); ok {
		currentBlock, err := eth.ParseHexNumber(currentBlockHex)
		if err != nil {
			return fmt.Errorf("failed to parse block number: %v (%s)", err, currentBlockHex)
		}

		if e.highestBlock != 0 && currentBlock < e.highestBlock-BlockLagTolerance {
			log := slog.With("provider", e.Name, "endpoint", p.Name)
			log.Warn("node is behind", "currentBlock", currentBlock, "highestBlock", e.highestBlock)

			return fmt.Errorf("node is behind: currentBlock=%d, highestBlock=%d", currentBlock, e.highestBlock)
		}

		if currentBlock > e.highestBlock {
			e.highestBlock = currentBlock
		}
	} else {
		return fmt.Errorf("block number is not a string: %v", res.Result)
	}

	res, err = read(ctx, rpc.NewRequest("ha_syncing", "eth_syncing", nil), true)
	if err != nil {
		return err
	}

	if m, ok := res.Result.(map[string]any); ok {
		currentBlockHex := m["currentBlock"].(string)
		highestBlockHex := m["highestBlock"].(string)

		currentBlock, _ := eth.ParseHexNumber(currentBlockHex)
		highestBlock, _ := eth.ParseHexNumber(highestBlockHex)

		return fmt.Errorf("node is not synced: currentBlock=%d, highestBlock=%d", currentBlock, highestBlock)
	}

	return nil
}

func SolanaHealthcheck(ctx context.Context, endpoint *Endpoint, provider *Provider, read rpc.ReaderFunc) error {
	res, err := read(ctx, rpc.NewRequest("ha_version", "getVersion", nil), true)
	if err != nil {
		return err
	}

	if r, ok := res.Result.(map[string]any); ok {
		if version, ok := r["solana-core"].(string); ok {
			provider.clientVersion = version
		}
	}

	_, err = read(ctx, rpc.NewRequest("ha_health", "getHealth", nil), true)
	if err != nil {
		return err
	}

	res, err = read(ctx, rpc.NewRequest("ha_height", "getBlockHeight", nil), true)
	if err != nil {
		return err
	}

	if bf, ok := res.Result.(float64); ok {
		blockHeight := uint64(bf)

		if blockHeight > provider.highestBlock {
			provider.highestBlock = blockHeight
		}

		if blockHeight < provider.highestBlock-BlockLagTolerance {
			return fmt.Errorf("node is behind: currentBlock=%d, highestBlock=%d", blockHeight, provider.highestBlock)
		}
	} else {
		return fmt.Errorf("block height is not a number: %v", res.Result)
	}

	return nil
}
