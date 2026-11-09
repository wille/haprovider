package healthcheck

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/wille/haprovider/internal/chain"
	"github.com/wille/haprovider/internal/core"
	"github.com/wille/haprovider/internal/metrics"
)

var defaultBlockLagTolerance = map[core.Kind]int{
	core.KindEth:    50,
	core.KindSolana: 50,
	core.KindTron:   10,
	core.KindBTC:    0,
}

// defaultStallTimeout is how long a provider's block height may stay unchanged
// while another provider reports a higher one before it's considered stalled.
// Bitcoin blocks can be more than an hour apart, so it gets far longer.
var defaultStallTimeout = map[core.Kind]time.Duration{
	core.KindEth:    2 * time.Minute,
	core.KindSolana: 2 * time.Minute,
	core.KindTron:   2 * time.Minute,
	core.KindBTC:    2 * time.Hour,
}

// minProvidersForMedian is how many providers must report a block height
// before their median is used to judge one of them behind or ahead. With fewer
// there is no majority: of two providers that disagree, nothing tells the one
// lagging behind from the one lying ahead, and judging by the higher one would
// let a single provider push the other out of rotation.
const minProvidersForMedian = 3

// minAheadTolerance is the least a provider may lead the median by before it's
// considered ahead, so a node that is just first to see a new block isn't
// taken out when block_lag_tolerance is 0 (the Bitcoin default).
const minAheadTolerance = 3

// Run the healthcheck for a provider, dispatching on the endpoint Kind.
func Run(ctx context.Context, p *core.Provider) error {
	c := chain.New(p.Endpoint.Kind)
	if c == nil {
		return fmt.Errorf("unknown endpoint kind: %s", p.Endpoint.Kind)
	}

	nodeInfo, err := c.Healthcheck(ctx, p)
	if err != nil {
		// Unreachable, rate limited or syncing: its last height no longer says
		// where the chain is, so don't measure other providers against it.
		p.StopReporting()
		return err
	}

	p.SetClientVersion(nodeInfo.ClientVersion)

	if err := p.Endpoint.SetChainID(nodeInfo.ChainID); err != nil {
		p.StopReporting()
		return err
	}

	return checkHeight(p, nodeInfo.BlockHeight, time.Now())
}

// checkHeight records the block height p reported at now and judges it
// against the endpoint's other providers. p fails if it is:
//
//   - behind: more than the tolerance below the median height;
//   - ahead: its previous report was more than the tolerance above the median
//     of the reports the others have made since;
//   - stalled: its height hasn't moved for the stall timeout while another
//     provider reports a higher one.
//
// The median needs minProvidersForMedian reports, so one provider reporting a
// wrong height can't push the others out of rotation. A report is only ever
// compared with ones made before it (behind) or after it (ahead), so the time
// between healthchecks works in the judged provider's favour: on a fast chain
// an older report is lower than the chain is now.
func checkHeight(p *core.Provider, height uint64, now time.Time) error {
	e := p.Endpoint

	tolerance := uint64(defaultBlockLagTolerance[e.Kind])
	if e.BlockLagTolerance != nil {
		tolerance = uint64(max(*e.BlockLagTolerance, 0))
	}

	prev, lastAdvance := p.RecordHeight(height, now)
	peers := peerHeights(p)

	// Behind: the new report against the others' latest, all made before it.
	heights := []uint64{height}
	for _, r := range peers {
		heights = append(heights, r.Height)
	}
	if len(heights) >= minProvidersForMedian {
		// The lower median: with an even count, the one kinder to p.
		median, _ := medians(heights)
		// Written as height+tolerance < median so it can't wrap around when the
		// median is below the tolerance.
		if height+tolerance < median {
			return fmt.Errorf("node is behind: currentBlock=%d, medianBlock=%d missing=%d", height, median, median-height)
		}
	}

	// Ahead: the previous report against the others' reports made after it.
	if !prev.At.IsZero() {
		heights := []uint64{prev.Height}
		for _, r := range peers {
			if r.At.After(prev.At) {
				heights = append(heights, r.Height)
			}
		}
		if len(heights) >= minProvidersForMedian {
			// The upper median: with an even count, the one kinder to p.
			_, median := medians(heights)
			if prev.Height > median+max(tolerance, minAheadTolerance) {
				return fmt.Errorf("node is ahead of the other providers: reportedBlock=%d, medianBlock=%d", prev.Height, median)
			}
		}
	}

	// Stalled: not moving while another provider is further along. Requiring a
	// higher peer means a chain that has halted takes nobody out, and a
	// provider lying about its height can't make one that is still advancing
	// look stalled.
	stallTimeout := defaultStallTimeout[e.Kind]
	if e.StallTimeout != nil {
		stallTimeout = *e.StallTimeout
	}
	if stalled := now.Sub(lastAdvance); stallTimeout > 0 && stalled > stallTimeout {
		highest := height
		for _, r := range peers {
			highest = max(highest, r.Height)
		}
		if highest > height {
			return fmt.Errorf("node is stalled: currentBlock=%d unchanged for %s, highestBlock=%d", height, stalled.Round(time.Second), highest)
		}
	}

	return nil
}

// peerHeights returns the latest height reports of the other providers of p's
// endpoint that are still reporting.
func peerHeights(p *core.Provider) []core.HeightReport {
	var peers []core.HeightReport
	for _, q := range p.Endpoint.Providers {
		if q == p {
			continue
		}
		if r, ok := q.Height(); ok {
			peers = append(peers, r)
		}
	}
	return peers
}

// medians sorts heights in place and returns its lower and upper median, which
// are the same for an odd count. heights must not be empty.
func medians(heights []uint64) (lower, upper uint64) {
	slices.Sort(heights)
	return heights[(len(heights)-1)/2], heights[len(heights)/2]
}

func Worker(p *core.Provider, defaultHealthcheckInterval time.Duration) {
	for {
		if until := p.GetNextCheckTime(); !until.IsZero() && time.Now().Before(until) {
			p.Logger().Debug("rate limited", "until", time.Until(until).Round(time.Millisecond).String())
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
			uptime := time.Since(p.GetLastStateChange())
			p.Logger().Debug("healthcheck passed", "uptime", uptime.Round(time.Millisecond).String(), "next_check_in", defaultHealthcheckInterval.Round(time.Millisecond).String(), "duration", duration.Round(time.Millisecond).String())
			metrics.SetProviderUptime(p.Endpoint.Name, p.Name, uptime.Seconds())
		}

		p.MarkHealthy(duration)
		return defaultHealthcheckInterval
	}

	nextRun := nextAttemptDelay(p.GetCurrentConnectionAttempts())
	if !p.IsOnline() {
		p.Logger().Debug("provider still failing", "downtime", time.Since(p.GetLastStateChange()).Round(time.Millisecond).String(), "attempts", p.GetCurrentConnectionAttempts(), "next_check_in", nextRun.Round(time.Millisecond).String(), "duration", duration.Round(time.Millisecond).String(), "error", err)
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
