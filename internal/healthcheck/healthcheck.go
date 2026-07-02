package healthcheck

import (
	"context"
	"fmt"
	"time"

	"github.com/wille/haprovider/internal/chain"
	"github.com/wille/haprovider/internal/core"
)

var defaultBlockLagTolerance = map[core.Kind]int{
	core.KindEth:    50,
	core.KindSolana: 50,
	core.KindTron:   10,
	core.KindBTC:    0,
}

// Run the healthcheck for a provider, dispatching on the endpoint Kind.
// the endpoint Kind. The read closure issues RPC requests over HTTP to the provider.
func Run(ctx context.Context, p *core.Provider) error {
	c := chain.New(p.Endpoint.Kind)
	if c == nil {
		return fmt.Errorf("unknown endpoint kind: %s", p.Endpoint.Kind)
	}

	nodeInfo, err := c.Healthcheck(ctx, p)
	if err != nil {
		return err
	}

	p.SetClientVersion(nodeInfo.ClientVersion)

	if err := p.Endpoint.SetChainID(nodeInfo.ChainID); err != nil {
		return err
	}

	blockLagTolerance := defaultBlockLagTolerance[p.Endpoint.Kind]
	if p.Endpoint.BlockLagTolerance != nil {
		blockLagTolerance = *p.Endpoint.BlockLagTolerance
	}

	highestBlock := p.HighestBlock()

	if nodeInfo.BlockHeight > highestBlock {
		p.SetHighestBlock(nodeInfo.BlockHeight)
	}

	if highestBlock > 0 && nodeInfo.BlockHeight < highestBlock-uint64(blockLagTolerance) {
		return fmt.Errorf("node is behind: currentBlock=%d, highestBlock=%d missing=%d", nodeInfo.BlockHeight, highestBlock, highestBlock-nodeInfo.BlockHeight)
	}

	return nil
}

func Worker(p *core.Provider, defaultHealthcheckInterval time.Duration) {
	for {
		if until := p.GetNextCheckTime(); !until.IsZero() && time.Now().Before(until) {
			p.Logger().Debug("rate limited", "until", time.Until(until).String())
			time.Sleep(time.Until(until))
		}

		time.Sleep(runOnce(p, defaultHealthcheckInterval))
	}
}

// runOnce performs a single healthcheck cycle: it runs the healthcheck, updates
// the provider status accordingly, and returns how long to wait before the next
// cycle. It does not sleep, so it can be exercised directly in tests.
func runOnce(p *core.Provider, defaultHealthcheckInterval time.Duration) time.Duration {
	ctx, cancel := context.WithTimeout(context.Background(), p.GetTimeout())
	defer cancel()

	start := time.Now()
	err := Run(ctx, p)
	duration := time.Since(start)

	if err == nil {
		if p.IsOnline() {
			p.Logger().Debug("healthcheck passed", "uptime", time.Since(p.GetLastStateChange()).String(), "next_check_in", defaultHealthcheckInterval.String(), "duration", duration.String())
		}

		p.MarkHealthy(duration)
		return defaultHealthcheckInterval
	}

	nextRun := nextAttemptDelay(p.GetCurrentConnectionAttempts())
	if !p.IsOnline() {
		p.Logger().Debug("provider still failing", "downtime", time.Since(p.GetLastStateChange()).String(), "attempts", p.GetCurrentConnectionAttempts(), "next_check_in", nextRun.String(), "duration", duration.String(), "error", err)
	}

	p.MarkUnhealthy(err)
	return nextRun
}

func nextAttemptDelay(attempt int) time.Duration {
	switch {
	case attempt < 5:
		return 10 * time.Second
	case attempt < 10:
		return 30 * time.Second
	default:
		return 1 * time.Minute
	}
}
