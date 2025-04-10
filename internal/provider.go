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
	ProviderName string
	Name         string `yaml:"name"`
	Http         string `yaml:"http"`
	Ws           string `yaml:"ws"`

	// The timeout for the provider. Use Endpoint.GetTimeout() to get the actual timeout
	Timeout time.Duration `yaml:"timeout,omitempty"`

	retryAt time.Time
	attempt int

	online   bool
	onlineAt time.Time

	clientVersion string

	m sync.Mutex
}

func (e *Provider) GetTimeout() time.Duration {
	if e.Timeout > 0 {
		return e.Timeout
	}
	return 10 * time.Second
}

func backoff(attempt int) time.Duration {
	backoff := attempt
	if backoff > 12 {
		backoff = 12
	}
	return time.Duration(backoff) * 10 * time.Second
}

func (e *Provider) SetStatus(online bool, err error) {
	e.m.Lock()
	defer e.m.Unlock()

	log := slog.With("provider", e.ProviderName, "endpoint", e.Name)

	if e.online == online {
		if !online {
			e.attempt++

			// Simple backoff
			if e.retryAt.IsZero() {
				e.retryAt = time.Now()
			}

			e.retryAt = e.retryAt.Add(backoff(e.attempt))

			diff := time.Until(e.retryAt)
			log.Info("endpoint still offline", "error", err, "attempt", e.attempt, "retry_in", diff.String())
		}
		return
	}

	e.online = online
	if !online {
		e.retryAt = time.Now().Add(backoff(1))
		e.onlineAt = time.Time{}
		diff := time.Until(e.retryAt)
		log.Info("endpoint offline", "error", err, "retry_in", diff.String())
	} else {
		log.Info("endpoint online", "client_version", e.clientVersion)
		e.onlineAt = time.Now()
		e.retryAt = time.Time{}
		e.attempt = 0
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

// func (p *Provider) DelayedHealthcheck(p *Provider, delay time.Duration) error {
// 	t := time.NewTicker(delay)
// 	defer t.Stop()

// 	<-t.C

// 	return e.Healthcheck(p)
// }

func (e *Provider) Healthcheck(p *Endpoint) error {
	ctx := context.TODO()

	fn := func(ctx context.Context, req *rpc.Request, errRpcError bool) (*rpc.Response, error) {
		res, err := SendHTTPRPCRequest(ctx, e, req)
		if err != nil {
			return nil, err
		}

		if errRpcError && res.IsError() {
			_, err := res.GetError()
			return nil, err
		}

		return res, nil
	}

	switch p.Kind {
	case "eth":
		return EthereumHealthCheck(ctx, p, e, fn)
	case "solana":
		return SolanaHealthcheck(ctx, p, e, fn)
	}

	return nil
}
