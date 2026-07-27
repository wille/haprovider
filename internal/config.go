package internal

import (
	"fmt"
	"log"
	"net/url"
	"os"

	"github.com/wille/haprovider/internal/chain"
	"github.com/wille/haprovider/internal/core"
	"github.com/wille/haprovider/internal/redact"
	yaml "gopkg.in/yaml.v2"
)

type Config struct {
	Endpoints           map[string]*core.Endpoint `yaml:"endpoints"`
	LogLevel            string                    `yaml:"log_level,omitempty"`
	LogJSON             bool                      `yaml:"log_json,omitempty"`
	Port                string                    `yaml:"port,omitempty"`
	MetricsPort         string                    `yaml:"metrics_port,omitempty"`
	HealthcheckInterval string                    `yaml:"healthcheck_interval,omitempty"`

	// Keys are inbound bearer tokens accepted on every endpoint. Combined with
	// each endpoint's own keys. Env references are expanded at load.
	Keys []string `yaml:"keys,omitempty"`
}

func LoadConfig(configFile string) *Config {
	configFromEnv := os.Getenv("HA_CONFIG")
	if configFromEnv == "" {
		z, err := os.ReadFile(configFile)
		if err != nil {
			panic(err)
		}

		configFromEnv = string(z)
	}

	config := &Config{}

	err := yaml.Unmarshal([]byte(configFromEnv), config)
	if err != nil {
		panic(err)
	}

	// Expand env references in the global keys (e.g. "${HA_KEY}").
	for i, k := range config.Keys {
		config.Keys[i] = os.ExpandEnv(k)
	}

	// Add a reference to the endpoint in the provider
	for endpointName, endpoint := range config.Endpoints {
		if endpoint.Name == "" {
			endpoint.Name = endpointName
		}

		// Expand env references in the endpoint keys, then compute the effective
		// accepted-token set (endpoint keys + global keys).
		for i, k := range endpoint.Keys {
			endpoint.Keys[i] = os.ExpandEnv(k)
		}
		endpoint.InitAuth(config.Keys)

		// Always default to eth for backwards compatibility
		if endpoint.Kind == "" {
			endpoint.Kind = core.KindEth
		}

		c := chain.New(endpoint.Kind)
		if c == nil {
			panic(fmt.Errorf("unknown chain kind: %s", endpoint.Kind))
		}

		if err := c.ValidateConfig(endpoint); err != nil {
			panic(err)
		}

		for _, provider := range endpoint.Providers {
			provider.Endpoint = endpoint

			var host string
			if provider.Http == "" {
				panic(fmt.Errorf("provider %s has no HTTP endpoint", provider.Name))
			} else {
				u, err := url.Parse(provider.Http)

				if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
					log.Fatalf("Invalid HTTP URL: %s", redact.URL(provider.Http))
				}
				host = u.Host
			}

			if provider.Ws != "" {
				u, err := url.Parse(provider.Ws)

				if err != nil || (u.Scheme != "ws" && u.Scheme != "wss") {
					log.Fatalf("Invalid Websocket URL: %s", redact.URL(provider.Ws))
				}
			}

			// Fall back to the URL host when no name is configured, so redacted
			// logs (which no longer show the URL) still identify the provider. The
			// host carries no API key.
			if provider.Name == "" {
				provider.Name = host
			}

			if provider.Headers != nil {
				for k, v := range provider.Headers {
					provider.Headers[k] = os.ExpandEnv(v)
				}
			}
		}
	}

	return config
}
