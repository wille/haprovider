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

func (e *Endpoint) GetName() string {
	return fmt.Sprintf("%s/%s", e.ProviderName, e.Name)
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

	log := slog.With("endpoint", e.GetName())

	if e.online == online {
		if !online {
			e.attempt++

			// Simple backoff
			e.retryAt = time.Now().Add(time.Duration(e.attempt) * time.Second)

			diff := time.Until(e.retryAt)
			log.Info("endpoint still offline", "error", err, "retry_in", diff)
		}
		return
	}

	e.online = online
	if !online {
		e.retryAt = time.Now().Add(30 * time.Second)
		diff := time.Until(e.retryAt)
		log.Info("endpoint offline", "error", err, "retry_in", diff)
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

func (e *Endpoint) RateLimit() {

}

func (p *Provider) HTTPHealthcheck(e *Endpoint) error {
	if !e.retryAt.IsZero() && time.Since(e.retryAt) < 0 {
		return fmt.Errorf("retrying in %s", time.Until(e.retryAt))
	}

	clientVersion, err := SendHTTPRPCRequest(context.Background(), e, NewRPCRequest("ha_clientVersion", "web3_clientVersion", []string{}))
	if err != nil {
		e.SetStatus(false, err)
		return err
	}

	if _, ok := clientVersion.Result.(string); ok {
		e.clientVersion = clientVersion.Result.(string)
	} else {
		e.clientVersion = ""
	}

	chainId, err := SendHTTPRPCRequest(context.Background(), e, NewRPCRequest("ha_chainId", "eth_chainId", []string{}))
	if err != nil {
		e.SetStatus(false, err)
		return err
	}

	if chainId.Result.(string) != fmt.Sprintf("0x%x", p.ChainID) {
		err = fmt.Errorf("chainId mismatch: received=%s, expected=%d", chainId.Result, p.ChainID)
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

	ActiveMu sync.RWMutex
	Active   map[string]*WebSocketProxy
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
