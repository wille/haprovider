package internal

import (
	"fmt"
	"log"
	"net/url"
	"os"

	"github.com/wille/haprovider/internal/chain"
	"github.com/wille/haprovider/internal/core"
	yaml "gopkg.in/yaml.v2"
)

type Config struct {
	Endpoints           map[string]*core.Endpoint `yaml:"endpoints"`
	LogLevel            string                    `yaml:"log_level,omitempty"`
	LogJSON             bool                      `yaml:"log_json,omitempty"`
	Port                string                    `yaml:"port,omitempty"`
	MetricsPort         string                    `yaml:"metrics_port,omitempty"`
	HealthcheckInterval string                    `yaml:"healthcheck_interval,omitempty"`
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

	// Add a reference to the endpoint in the provider
	for endpointName, endpoint := range config.Endpoints {
		if endpoint.Name == "" {
			endpoint.Name = endpointName
		}

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

			if provider.Http == "" {
				panic(fmt.Errorf("provider %s has no HTTP endpoint", provider.Name))
			} else {
				url, err := url.Parse(provider.Http)

				if err != nil || (url.Scheme != "http" && url.Scheme != "https") {
					log.Fatalf("Invalid HTTP URL: %s", provider.Http)
				}
			}

			if provider.Ws != "" {
				url, err := url.Parse(provider.Ws)

				if err != nil || (url.Scheme != "ws" && url.Scheme != "wss") {
					log.Fatalf("Invalid Websocket URL: %s", provider.Ws)
				}
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
