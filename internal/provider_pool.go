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
	ErrNoProvidersAvailable    = errors.New("no providers available")
	ErrNoProvidersForWebsocket = errors.New("no providers available for websocket connections")
	ErrNoProvidersForHTTP      = errors.New("no providers available for http connections")
)

type Endpoint struct {
	ProviderName string
	Name         string `yaml:"name"`
	Http         string `yaml:"http"`
	Ws           string `yaml:"ws"`

	// The timeout for the provider. Use Endpoint.GetTimeout() to get the actual timeout
	Timeout time.Duration `yaml:"timeout,omitempty"`

	retryAt       time.Time
	attempt       int
	online        bool
	clientVersion string

	m sync.Mutex
}

func (e *Endpoint) GetTimeout() time.Duration {
	if e.Timeout > 0 {
		return e.Timeout
	}
	return 10 * time.Second
}

func (e *Endpoint) SetStatus(online bool, err error) {
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

			backoff := e.attempt
			if backoff > 12 {
				backoff = 12
			}

			e.retryAt = e.retryAt.Add(time.Duration(backoff) * 10 * time.Second)

			diff := time.Until(e.retryAt)
			log.Info("endpoint still offline", "error", err, "attempt", e.attempt, "retry_in", diff.String())
		}
		return
	}

	e.online = online
	if !online {
		e.retryAt = time.Now().Add(30 * time.Second)
		diff := time.Until(e.retryAt)
		log.Info("endpoint offline", "error", err, "retry_in", diff.String())
	} else {
		log.Info("endpoint online", "client_version", e.clientVersion)
		e.retryAt = time.Time{}
		e.attempt = 0
	}
}

// HandleTooManyRequests handles a 429 response and stops using the provider until it's ready again.
func (e *Endpoint) HandleTooManyRequests(req *http.Response) error {
	var retryAfter time.Duration

	if req != nil {
		header := req.Header.Get("Retry-After")

		if sec, err := strconv.Atoi(header); err != nil && sec > 0 {
			retryAfter = time.Duration(sec) * time.Second
		} else {
			retryAfter = DefaultRateLimitBackoff
		}
	} else {
		// We have no request, we probably received a rate limit message over the websocket
		// which contains no details on how long we're actually rate limited for.
		retryAfter = DefaultRateLimitBackoff
	}

	e.retryAt = time.Now().Add(retryAfter)

	return fmt.Errorf("rate limited for %f", retryAfter.Seconds())
}

func (e *Endpoint) IsOnline() bool {
	return e.online
}

func (e *Endpoint) IsRateLimited() bool {
	return !e.retryAt.IsZero() && time.Since(e.retryAt) < 0
}

// func (p *Provider) DelayedHealthcheck(p *Provider, delay time.Duration) error {
// 	t := time.NewTicker(delay)
// 	defer t.Stop()

// 	<-t.C

// 	return e.Healthcheck(p)
// }

func (p *Provider) HTTPHealthcheck(e *Endpoint) error {
	// TODO move to healthcheck loop
	if e.IsRateLimited() {
		return fmt.Errorf("retrying in %s", time.Until(e.retryAt))
	}

	err := e.Healthcheck(p)

	if err != nil {
		e.SetStatus(false, err)
		return err
	}

	e.SetStatus(true, nil)

	return nil
}

type Provider struct {
	Name     string
	ChainID  int         `yaml:"chainId"`
	Kind     string      `yaml:"kind"`
	Endpoint []*Endpoint `yaml:"endpoints"`

	// Public indicates that the endpoint is public and that we
	// will skip debug headers and detailed error messages to the client.
	// TODO
	Public bool `yaml:"public,omitempty"`

	Xfwd bool `yaml:"xfwd,omitempty"`

	requestCount       int
	failedRequestCount int
	openConnections    int
}

// GetActiveEndpoints returns a list of endpoints that are currently considered online
func (p *Provider) GetActiveEndpoints() []*Endpoint {
	var active []*Endpoint

	for _, e := range p.Endpoint {

		if !e.online {
			continue
		}

		active = append(active, e)
	}

	return active
}

func (e *Endpoint) Healthcheck(p *Provider) error {
	ctx := context.TODO()

	fn := func(ctx context.Context, req *rpc.Request, errRpcError bool) (*rpc.Response, error) {
		// TODO errRpcError
		return SendHTTPRPCRequest(ctx, e, req)
	}

	switch p.Kind {
	case "eth":
		return EthereumHealthCheck(ctx, p, e, fn)
	case "solana":
		return SolanaHealthcheck(ctx, p, e, fn)
	}

	return nil
}
