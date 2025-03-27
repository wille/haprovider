package main

import (
	"context"
	"flag"
	"log"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	servertiming "github.com/mitchellh/go-server-timing"
	. "github.com/wille/haprovider/internal"
)

// Incoming requests
func requestHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Create a new timing handler
	timing := servertiming.FromContext(ctx)

	id := r.PathValue("id")

	endpoint := config.Endpoints[id]

	w.Header().Set("Server", "haprovider/"+Version)

	if endpoint == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	if r.Header.Get("Upgrade") == "websocket" {
		IncomingWebsocketHandler(ctx, endpoint, w, r, timing)
		return
	}

	IncomingHttpHandler(ctx, endpoint, w, r, timing)
}

var config *Config

func getEnvWithDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func main() {
	var addr = flag.String("addr", getEnvWithDefault("HA_ADDR", "0.0.0.0:8080"), "http service address ($HA_ADDR)")
	var configFile = flag.String("config", getEnvWithDefault("HA_CONFIGFILE", "config.yml"), "config file ($HA_CONFIGFILE) (Raw config can be provided via $HA_CONFIG)")
	var debug = flag.Bool("debug", getEnvWithDefault("HA_DEBUG", "false") == "true", "debug logging ($HA_DEBUG)")
	var json = flag.Bool("json", getEnvWithDefault("HA_JSON", "false") == "true", "json logging ($HA_JSON)")

	flag.Parse()

	config = LoadConfig(*configFile)

	level := slog.LevelInfo

	if *debug {
		level = slog.LevelDebug
	}

	if *json {
		slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
			Level: level,
		})))
	} else {
		slog.SetLogLoggerLevel(level)
	}

	timingMiddleware := servertiming.Middleware(http.HandlerFunc(requestHandler), nil)

	http.Handle("/{id}", timingMiddleware)

	for endpointName, endpoint := range config.Endpoints {
		switch endpoint.Kind {
		case "", "eth":
			endpoint.Kind = "eth"
		case "solana", "btc":
		default:
			log.Fatalf("Unknown endpoint kind %s", endpoint.Kind)
		}

		if endpoint.Name == "" {
			endpoint.Name = endpointName
		}

		for _, provider := range endpoint.Providers {
			provider.ProviderName = endpointName
			if provider.Ws != "" {
				url, err := url.Parse(provider.Ws)

				if err != nil || url.Scheme == "http" || url.Scheme == "https" {
					log.Fatalf("Invalid Websocket URL: %s", provider.Ws)
				}
			}
			if provider.Http != "" {
				url, err := url.Parse(provider.Http)

				if err != nil || url.Scheme == "ws" || url.Scheme == "wss" {
					log.Fatalf("Invalid HTTP URL: %s", provider.Http)
				}
			}

			if provider.Http == "" && provider.Ws == "" {
				log.Fatalf("Provider %s has no HTTP or WS endpoint", endpointName)
			}
		}
	}

	slog.Info("starting haprovider", "version", Version)

	var wg sync.WaitGroup

	total := 0
	online := 0

	for name, endpoint := range config.Endpoints {
		for _, provider := range endpoint.Providers {
			if provider.Http != "" {
				slog.Info("connecting to", "provider", name, "endpoint", provider.Name, "http", provider.Http)

				wg.Add(1)
				go func() {
					total++

					err := endpoint.HTTPHealthcheck(provider)

					if err == nil {
						online++
					}

					wg.Done()

					c := time.NewTicker(DefaultHealthcheckInterval)
					for range c.C {
						endpoint.HTTPHealthcheck(provider)
					}
				}()
			} else if provider.Ws != "" {
				// No initial healthcheck on websocket-only providers yet
				slog.Warn("no http endpoint for provider. skipping healthcheck", "provider", name, "endpoint", provider.Name)
				provider.SetStatus(true, nil)
			}
		}
	}

	wg.Wait()

	server := http.Server{
		Addr: *addr,
	}

	slog.Info("haprovider started", "addr", *addr, "online", online, "total", total)

	go server.ListenAndServe()

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt, syscall.SIGTERM)

	<-interrupt

	slog.Info("shutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	server.Shutdown(ctx)

	slog.Info("shutdown complete")
}
