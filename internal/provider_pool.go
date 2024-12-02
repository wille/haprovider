package internal

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"
)

var ErrNoProvidersAvailable = errors.New("No providers available")

type Endpoint struct {
	ProviderName string
	Name         string `yaml:"name"`
	Http         string `yaml:"http"`
	Ws           string `yaml:"ws"`

	retryAt       time.Time
	attempt       int
	online        bool
	clientVersion string

	m sync.Mutex
}

func (e *Endpoint) GetName() string {
	return fmt.Sprintf("%s/%s", e.ProviderName, e.Name)

}

func (e *Endpoint) SetStatus(online bool, err error) {
	e.m.Lock()
	defer e.m.Unlock()

	if e.online == online {
		if !online {
			log.Printf("Endpoint %s still offline (%s)\n", e.GetName(), err)
			e.attempt++
		}
		return
	}

	e.online = online
	if !online {
		log.Printf("Endpoint %s/%s offline (%s)\n", e.ProviderName, e.Name, err)
		e.retryAt = time.Now().Add(30 * time.Second)
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

	return fmt.Errorf("Rate limited for %f seconds", retryAfter.Seconds())
}

func (e *Endpoint) RateLimit() {

}

func (p *Provider) HTTPHealthcheck(e *Endpoint) error {
	clientVersion, err := SendHTTPRPCRequest(e, "ha_clientVersion", NewRPCRequest("web3_clientVersion", []string{}))
	if err != nil {
		e.SetStatus(false, err)
		return err
	}

	if _, ok := clientVersion.Result.(string); !ok {
		err = fmt.Errorf("could not get a proper web3_clientVersion")
		e.SetStatus(false, err)
		return err
	}

	e.clientVersion = clientVersion.Result.(string)

	chainId, err := SendHTTPRPCRequest(e, "ha_chainId", NewRPCRequest("eth_chainId", []string{}))
	if err != nil {
		e.SetStatus(false, err)
		return err
	}

	if chainId.Result.(string) != fmt.Sprintf("0x%x", p.ChainID) {
		err = fmt.Errorf("chainId mismatch: received=%s, expected=%d", chainId.Result, p.ChainID)
		e.SetStatus(false, err)
	}

	e.SetStatus(true, nil)

	return nil
}

type Provider struct {
	ChainID  int         `yaml:"chainId"`
	Kind     string      `yaml:"kind"`
	Endpoint []*Endpoint `yaml:"endpoints"`
}

// GetActiveEndpoints returns a list of endpoints that are currently considered online
func (p *Provider) GetActiveEndpoints() []*Endpoint {
	var active []*Endpoint

	for _, e := range p.Endpoint {
		if !e.retryAt.IsZero() && time.Since(e.retryAt) < 0 {
			log.Printf("Endpoint %s rate limited for %f seconds more...", e.Name, time.Since(e.retryAt).Seconds())
			continue
		}

		// backoff

		if e.online {
			active = append(active, e)
		}
	}

	return active
}
