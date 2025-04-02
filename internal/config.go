package internal

import (
	"os"
	"time"

	yaml "gopkg.in/yaml.v2"
)

const DefaultRateLimitBackoff = time.Duration(30) * time.Second

type Config struct {
	Endpoints   map[string]*Endpoint `yaml:"endpoints"`
	LogLevel    string               `yaml:"log_level,omitempty"`
	LogJSON     bool                 `yaml:"log_json,omitempty"`
	Port        string               `yaml:"port,omitempty"`
	MetricsPort string               `yaml:"metrics_port,omitempty"`
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
	return config
}
