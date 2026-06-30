package core

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/wille/haprovider/internal/metrics"
	"github.com/wille/haprovider/internal/rpc"
)

// DefaultRateLimitBackoff is used when a provider rate limits us without
// telling us for how long.
const DefaultRateLimitBackoff = 30 * time.Second
const DefaultNodeVersionRetention = time.Hour
const DefaultConnectionTimeout = 10 * time.Second

type Provider struct {

	// Name of the provider
	Name string `yaml:"name"`

	// HTTP endpoint to use for the provider
	Http string `yaml:"http"`

	// Websocket endpoint to use for the provider
	Ws string `yaml:"ws"`

	// Headers to send with the requests
	Headers map[string]string `yaml:"headers,omitempty"`

	// The timeout for the provider. Use Endpoint.GetTimeout() to get the actual timeout
	Timeout time.Duration `yaml:"timeout,omitempty"`

	// Endpoint this provider belongs to
	Endpoint *Endpoint

	RateLimitedUntil time.Time
	attempt          int

	online bool

	// The time the provider last changed state (online or offline)
	lastStateChange time.Time

	// The highest block observed for the provider
	highestBlock uint64

	// The latest client version observed for the provider
	clientVersion string

	m sync.Mutex

	versionsSeen map[string]time.Time
}

// Logger returns a structured logger tagged with the endpoint and provider name.
func (p *Provider) Logger() *slog.Logger {
	return slog.With("provider", fmt.Sprintf("%s/%s", p.Endpoint.Name, p.Name))
}

func (p *Provider) GetTimeout() time.Duration {
	if p.Timeout > 0 {
		return p.Timeout
	}
	return DefaultConnectionTimeout
}

func (p *Provider) GetCurrentConnectionAttempts() int {
	p.m.Lock()
	defer p.m.Unlock()
	return p.attempt
}

func (p *Provider) GetNextCheckTime() time.Time {
	p.m.Lock()
	defer p.m.Unlock()
	return p.RateLimitedUntil
}

func (p *Provider) MarkUnhealthy(err error) {
	p.m.Lock()
	defer p.m.Unlock()

	var rl *rpc.ErrorRateLimited
	if errors.As(err, &rl) {
		retryAfter := rl.RetryAfter
		if retryAfter <= 0 {
			retryAfter = DefaultRateLimitBackoff
		}
		p.RateLimitedUntil = time.Now().Add(retryAfter)
	}

	metrics.RecordProviderHealth(p.Endpoint.Name, p.Name, false)

	if !p.online {
		p.attempt++

		if p.lastStateChange.IsZero() {
			p.lastStateChange = time.Now()
		}

		return
	}

	p.lastStateChange = time.Now()
	p.online = false
	// Read p.attempt directly: we already hold p.m, and GetCurrentConnectionAttempts
	// would try to lock it again (deadlock).
	p.Logger().Info("provider lost", "error", err, "attempt", p.attempt)
}

// MarkHealthy marks the provider online. took is how long the successful
// connection or healthcheck attempt took.
func (p *Provider) MarkHealthy(took time.Duration) {
	p.m.Lock()
	defer p.m.Unlock()

	if p.online {
		return
	}

	p.online = true
	p.lastStateChange = time.Now()
	p.attempt = 0
	p.RateLimitedUntil = time.Time{}

	metrics.RecordProviderHealth(p.Endpoint.Name, p.Name, true)

	p.Logger().Info("provider connected", "client_version", p.clientVersion, "chain_id", p.Endpoint.ChainID, "block_height", p.highestBlock, "took", took.String())

}

// HandleTooManyRequests handles a 429 response and stops using the provider until it's ready again.
func (p *Provider) HandleTooManyRequests(req *http.Response) error {
	var retryAfter time.Duration
	receivedDuration := false

	if req != nil {
		header := req.Header.Get("Retry-After")

		if sec, err := strconv.Atoi(header); err == nil && sec > 0 {
			retryAfter = time.Duration(sec) * time.Second
			receivedDuration = true
		} else {
			retryAfter = DefaultRateLimitBackoff
		}
	} else {
		// We have no request, we probably received a rate limit message over the websocket
		// which contains no details on how long we're actually rate limited for.
		retryAfter = DefaultRateLimitBackoff
	}

	p.m.Lock()
	p.RateLimitedUntil = time.Now().Add(retryAfter)
	p.m.Unlock()

	if receivedDuration {
		return fmt.Errorf("rate limited for %s", retryAfter)
	}

	return fmt.Errorf("rate limited")
}

func (p *Provider) IsOnline() bool {
	p.m.Lock()
	defer p.m.Unlock()
	return p.online
}

// GetLastStateChange returns the time the provider last came online.
func (p *Provider) GetLastStateChange() time.Time {
	p.m.Lock()
	defer p.m.Unlock()
	return p.lastStateChange
}

// HighestBlock returns the highest block observed across healthchecks for the provider.
func (p *Provider) HighestBlock() uint64 {
	p.m.Lock()
	defer p.m.Unlock()
	return p.highestBlock
}

func (p *Provider) SetHighestBlock(b uint64) {
	p.m.Lock()
	defer p.m.Unlock()
	p.highestBlock = b
}

// ClientVersion returns the node software version reported by the provider.
func (p *Provider) ClientVersion() string {
	p.m.Lock()
	defer p.m.Unlock()
	return p.clientVersion
}

func (p *Provider) SetClientVersion(v string) {
	p.m.Lock()
	defer p.m.Unlock()

	if p.versionsSeen == nil {
		p.versionsSeen = make(map[string]time.Time)
	}

	if p.clientVersion != "" && p.versionsSeen[v].IsZero() {
		p.Logger().Info("new version seen", "client_version", v, "total_versions_seen", len(p.versionsSeen))
	}

	for k, t := range p.versionsSeen {
		if time.Since(t) > DefaultNodeVersionRetention {
			p.Logger().Info("version not seen for a while", "client_version", k, "total_versions_seen", len(p.versionsSeen))
			delete(p.versionsSeen, k)
		}
	}

	p.versionsSeen[v] = time.Now()
	p.clientVersion = v

}
