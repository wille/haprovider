package internal

import (
	"context"
	"errors"
	"fmt"
	"log"
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

	// Public indicates that the endpoint is public and that we
	// will skip debug headers and detailed error messages to the client.
	// TODO
	Public bool `yaml:"public,omitempty"`

	// Xfwd makes us add X-Forwarded-* headers to the upstream request
	Xfwd bool `yaml:"xfwd,omitempty"`

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

	if e.online == online {
		if !online {
			diff := time.Until(e.retryAt)
			log.Printf("Endpoint %s still offline (%s). Delaying retry for %s\n", e.GetName(), err, diff.String())
			e.attempt++
		}
		return
	}

	e.online = online
	if !online {
		e.retryAt = time.Now().Add(30 * time.Second)
		diff := time.Until(e.retryAt)
		log.Printf("Endpoint %s/%s offline (%s). Delaying retry for %s\n", e.ProviderName, e.Name, err, diff.String())
	} else {
		log.Printf("Endpoint %s/%s online [%s]\n", e.ProviderName, e.Name, e.clientVersion)
		e.retryAt = time.Time{}
		e.attempt = 0
	}
}

// HandleTooManyRequests handles a 429 response and stops using the provider until it's ready again
func (e *Endpoint) HandleTooManyRequests(req *http.Response) error {
	header := req.Header.Get("Retry-After")

	var retryAfter time.Duration
	if sec, err := strconv.Atoi(header); err != nil && sec > 0 {
		retryAfter = time.Duration(sec) * time.Second
	} else {
		retryAfter = DefaultRateLimitBackoff
	}

	e.retryAt = time.Now().Add(retryAfter)

	return fmt.Errorf("rate limited for %f seconds", retryAfter.Seconds())
}

func (e *Endpoint) RateLimit() {

}

func (p *Provider) HTTPHealthcheck(e *Endpoint) error {
	if !e.retryAt.IsZero() && time.Since(e.retryAt) < 0 {
		return fmt.Errorf("rate limited for %f seconds", time.Since(e.retryAt).Seconds())
	}

	clientVersion, err := SendHTTPRPCRequest(context.Background(), e, "ha_clientVersion", NewRPCRequest("web3_clientVersion", []string{}))
	if err != nil {
		e.SetStatus(false, err)
		return err
	}

	if _, ok := clientVersion.Result.(string); ok {
		// err = fmt.Errorf("could not get a proper web3_clientVersion: %v", clientVersion.Error)
		// e.SetStatus(false, err)
		// return err
		e.clientVersion = clientVersion.Result.(string)
	} else {
		e.clientVersion = "unknown"
	}

	chainId, err := SendHTTPRPCRequest(context.Background(), e, "ha_chainId", NewRPCRequest("eth_chainId", []string{}))
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

	requestCount       int
	failedRequestCount int
	openConnections    int
}

// GetActiveEndpoints returns a list of endpoints that are currently considered online
func (p *Provider) GetActiveEndpoints() []*Endpoint {
	var active []*Endpoint

	for _, e := range p.Endpoint {

		// backoff

		if e.online {
			active = append(active, e)
		}
	}

	return active
}
