package internal

import (
	"os"
	"time"

	yaml "gopkg.in/yaml.v2"
)

const DefaultRateLimitBackoff = time.Duration(5) * time.Second

const (
	KindEth = "eth"
	KindBtc = "btc"
	KindSol = "solana"
)

type Config struct {
	Providers map[string]*Provider `yaml:"providers"`
}

func LoadConfig() *Config {
	z, err := os.ReadFile("config.yml")
	if err != nil {
		panic(err)
	}

	config := &Config{}

	err = yaml.Unmarshal(z, config)
	if err != nil {
		panic(err)
	}
	return config
}
