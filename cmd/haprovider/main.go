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
	"github.com/wille/haprovider/internal"
	"github.com/wille/haprovider/internal/metrics"
)

// Incoming requests
func requestHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Create a new timing handler
	timing := servertiming.FromContext(ctx)

	id := r.PathValue("id")

	endpoint := config.Endpoints[id]

	w.Header().Set("Server", "haprovider/"+internal.Version)

	if endpoint == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	if r.Header.Get("Upgrade") == "websocket" {
		internal.IncomingWebsocketHandler(ctx, endpoint, w, r, timing)
		return
	}

	internal.IncomingHttpHandler(ctx, endpoint, w, r, timing)
}

var config *internal.Config

const (
	DefaultConfigFile  = "config.yml"
	DefaultPort        = "127.0.0.1:8080"
	DefaultLogLevel    = "info"
	DefaultLogJSON     = false
	DefaultMetricsPort = "127.0.0.1:9090"
)

func main() {
	var configFile = flag.String("config", DefaultConfigFile, "config file ($HA_CONFIGFILE) (Raw config can be provided via $HA_CONFIG)")
	var port = flag.String("port", DefaultPort, "http service port ($HA_PORT)")
	var metricsPort = flag.String("metrics-port", DefaultMetricsPort, "metrics port ($HA_METRICS_PORT)")
	var logLevel = flag.String("log-level", DefaultLogLevel, "logging level (debug, info, warn, error) ($HA_LOG_LEVEL)")
	var logJSON = flag.Bool("log-json", DefaultLogJSON, "enable JSON logging ($HA_LOG_JSON)")

	flag.Parse()

	config = internal.LoadConfig(*configFile)

	if *logLevel == DefaultLogLevel {
		env := os.Getenv("HA_LOG_LEVEL")
		if env != "" {
			*logLevel = env
		} else if config.LogLevel != "" {
			*logLevel = config.LogLevel
		}
	}

	if !*logJSON {
		env := os.Getenv("HA_LOG_JSON")
		if env == "true" {
			*logJSON = true
		} else if config.LogJSON {
			*logJSON = true
		}
	}

	if *port == DefaultPort {
		env := os.Getenv("HA_PORT")
		if env != "" {
			*port = env
		} else if config.Port != "" {
			*port = config.Port
		}
	}

	if *metricsPort == DefaultMetricsPort {
		env := os.Getenv("HA_METRICS_PORT")
		if env != "" {
			*metricsPort = env
		} else if config.MetricsPort != "" {
			*metricsPort = config.MetricsPort
		}
	}

	// Set log level from config if not specified by command line
	level := slog.LevelInfo
	if *logLevel != "info" {
		switch *logLevel {
		case "debug":
			level = slog.LevelDebug
		case "info":
			level = slog.LevelInfo
		case "warn":
			level = slog.LevelWarn
		case "error":
			level = slog.LevelError
		}
	}

	if *logJSON {
		slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
			Level: level,
		})))
	} else {
		slog.SetLogLoggerLevel(level)
	}

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

	slog.Info("starting haprovider", "version", internal.Version)

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

					c := time.NewTicker(internal.DefaultHealthcheckInterval)
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

	// Main application server
	mainMux := http.NewServeMux()
	timingMiddleware := servertiming.Middleware(http.HandlerFunc(requestHandler), nil)
	mainMux.Handle("/{id}", timingMiddleware)

	mainServer := &http.Server{
		Addr:    *port,
		Handler: mainMux,
	}

	// Metrics server
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", metrics.MetricsHandler())

	metricsServer := &http.Server{
		Addr:    *metricsPort,
		Handler: metricsMux,
	}

	// Start both servers
	go func() {
		slog.Info("starting metrics server", "version", internal.Version, "addr", *metricsPort)
		if err := metricsServer.ListenAndServe(); err != http.ErrServerClosed {
			slog.Error("metrics server error", "error", err)
		}
	}()

	go func() {
		slog.Info("starting haprovider", "version", internal.Version, "addr", *port)
		if err := mainServer.ListenAndServe(); err != http.ErrServerClosed {
			slog.Error("main server error", "error", err)
		}
	}()

	// Handle graceful shutdown
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt, syscall.SIGTERM)

	<-interrupt
	slog.Info("shutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Shutdown both servers
	metricsServer.Shutdown(ctx)
	mainServer.Shutdown(ctx)
}
