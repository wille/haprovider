package internal

import (
	"fmt"
	"log"
	"net/url"
	"os"
	"slices"
	"strings"

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

	// RealIPHeader names the request header carrying the real client IP when
	// haprovider runs behind a trusted proxy/load balancer (e.g. "Cf-Connecting-Ip"
	// or "X-Forwarded-For"). Empty uses the connection's remote address.
	RealIPHeader string `yaml:"real_ip_header,omitempty"`

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

		for i, provider := range endpoint.Providers {
			provider.Endpoint = endpoint

			// Identifies the provider in load errors. Name may still be empty here
			// (it falls back to the URL host below), so use the list index instead.
			id := fmt.Sprintf("%s/%s", endpoint.Name, provider.Name)
			if provider.Name == "" {
				id = fmt.Sprintf("%s/providers[%d]", endpoint.Name, i)
			}

			var host string
			if provider.Http == "" {
				panic(fmt.Errorf("provider %s has no HTTP endpoint", id))
			} else {
				provider.Http = expandURL(id, "http", provider.Http)
				u, err := url.Parse(provider.Http)

				if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
					log.Fatalf("Invalid HTTP URL for provider %s: %s", id, redact.URL(provider.Http))
				}
				host = u.Host
			}

			if provider.Ws != "" {
				provider.Ws = expandURL(id, "ws", provider.Ws)
				u, err := url.Parse(provider.Ws)

				if err != nil || (u.Scheme != "ws" && u.Scheme != "wss") {
					log.Fatalf("Invalid Websocket URL for provider %s: %s", id, redact.URL(provider.Ws))
				}
			}

			// Fall back to the URL host when no name is configured, so redacted
			// logs (which no longer show the URL) still identify the provider. The
			// host carries no API key: keys live in the path, query or userinfo.
			if provider.Name == "" {
				provider.Name = host
			}

			if provider.Headers != nil {
				for k, v := range provider.Headers {
					provider.Headers[k] = os.ExpandEnv(v)
				}
			}
		}

		// Validate after the provider URLs are expanded, so chain checks on the
		// URL (e.g. tron's /jsonrpc suffix) see the real value.
		if err := c.ValidateConfig(endpoint); err != nil {
			panic(err)
		}
	}

	return config
}

// expandURL expands ${VAR} (and $VAR) references in a provider URL. Unlike the
// keys and headers, a reference to an unset or empty variable is an error: it
// would otherwise yield a URL with its API key silently missing. The error
// names the provider and the variable, never the URL or any expanded value.
func expandURL(id, field, raw string) string {
	var missing []string
	expanded := os.Expand(raw, func(name string) string {
		v := os.Getenv(name)
		if v == "" && !slices.Contains(missing, name) {
			missing = append(missing, name)
		}
		return v
	})

	if len(missing) > 0 {
		panic(fmt.Errorf("provider %s: %s URL references unset or empty environment variable(s): %s", id, field, strings.Join(missing, ", ")))
	}

	return expanded
}
