package internal

import (
	"fmt"
	"time"
)

type Endpoint struct {
	// Name is inferred from the key in the config if unset in the object
	Name string `yaml:"name"`

	// ChainID is the Ethereum/L2 chain ID we're expecting.
	// If it's not matching we will refuse using the provider
	ChainID int `yaml:"chainId"`

	// Kind is the type of
	Kind string `yaml:"kind"`

	Providers []*Provider `yaml:"providers"`

	// Public indicates that the endpoint is public and that we
	// will skip sending debug headers and detailed error messages to the client.
	Public bool `yaml:"public,omitempty"`

	// AddXForwardedHeaders adds X-Forwarded-For header to requests sent to upstream providers
	AddXForwardedHeaders bool `yaml:"add_xfwd_headers,omitempty"`
}

func (e *Endpoint) HTTPHealthcheck(provider *Provider) error {
	// TODO move to healthcheck loop
	if provider.IsRateLimited() {
		return fmt.Errorf("retrying in %s", time.Until(provider.retryAt))
	}

	err := provider.Healthcheck(e)

	if err != nil {
		provider.SetStatus(false, err)
		return err
	}

	provider.SetStatus(true, nil)

	return nil
}
