package internal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/wille/haprovider/internal/rpc"
)

var (
	ErrNoProvidersAvailable = errors.New("no providers available")
)

type Provider struct {
	EndpointName string
	Name         string `yaml:"name"`
	Http         string `yaml:"http"`
	Ws           string `yaml:"ws"`

	Headers map[string]string `yaml:"headers,omitempty"`

	// The timeout for the provider. Use Endpoint.GetTimeout() to get the actual timeout
	Timeout time.Duration `yaml:"timeout,omitempty"`

	retryAt time.Time
	attempt int

	online   bool
	onlineAt time.Time

	highestBlock uint64

	clientVersion string

	m sync.Mutex
}

func (e *Provider) GetTimeout() time.Duration {
	if e.Timeout > 0 {
		return e.Timeout
	}
	return 10 * time.Second
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

func (p *Provider) SetStatus(online bool, err error) {
	p.m.Lock()
	defer p.m.Unlock()

	log := slog.With("endpoint", p.EndpointName, "provider", p.Name)

	if p.online == online {
		if !online {
			p.attempt++

			backoff := nextAttemptDelay(p.attempt)
			p.retryAt = time.Now().Add(backoff)

			log.Info("provider still offline", "error", err, "attempt", p.attempt, "retry_in", backoff.String())
		}
		return
	}

	p.online = online
	if !online {
		backoff := nextAttemptDelay(1)
		p.retryAt = time.Now().Add(backoff)
		p.onlineAt = time.Time{}
		log.Info("provider offline", "error", err, "attempt", p.attempt, "retry_in", backoff.String())
	} else {
		log.Info("provider online", "client_version", p.clientVersion)
		p.onlineAt = time.Now()
		p.retryAt = time.Time{}
		p.attempt = 0
	}
}

// HandleTooManyRequests handles a 429 response and stops using the provider until it's ready again.
func (e *Provider) HandleTooManyRequests(req *http.Response) error {
	var retryAfter time.Duration
	receivedDuration := false

	if req != nil {
		header := req.Header.Get("Retry-After")

		if sec, err := strconv.Atoi(header); err != nil && sec > 0 {
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

	e.retryAt = time.Now().Add(retryAfter)

	if receivedDuration {
		return fmt.Errorf("rate limited for %s", retryAfter)
	}

	return fmt.Errorf("rate limited")
}

func (e *Provider) IsOnline() bool {
	return e.online
}

func (e *Provider) IsRateLimited() bool {
	return !e.retryAt.IsZero() && time.Since(e.retryAt) < 0
}

func (e *Provider) Healthcheck(p *Endpoint) error {
	ctx := context.Background()

	fn := func(ctx context.Context, req *rpc.Request, errRpcError bool) (*rpc.Response, error) {
		res, err := SendHTTPRPCRequest(ctx, e, rpc.NewBatchRequest(req))
		if err != nil {
			return nil, err
		}

		if errRpcError && res.Responses[0].IsError() {
			_, err := res.Responses[0].GetError()
			return nil, err
		}

		return res.Responses[0], nil
	}

	switch p.Kind {
	case "eth":
		return EthereumHealthCheck(ctx, p, e, fn)
	case "solana":
		return SolanaHealthcheck(ctx, p, e, fn)
	}

	return nil
}
