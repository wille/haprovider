package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/lmittmann/tint"
	servertiming "github.com/mitchellh/go-server-timing"
	"github.com/wille/haprovider/internal"
	"github.com/wille/haprovider/internal/healthcheck"
	"github.com/wille/haprovider/internal/httpx"
	"github.com/wille/haprovider/internal/metrics"
	"github.com/wille/haprovider/internal/ws"
	"golang.org/x/term"
)

// Incoming requests
func requestHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Create a new timing handler
	timing := servertiming.FromContext(ctx)

	id := r.PathValue("id")

	endpoint := config.Endpoints[id]

	w.Header().Set("Server", "haprovider/"+httpx.Version)

	if endpoint == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	// A trailing path (e.g. /tron/wallet/getnowblock) is a native HTTP API
	// request that we forward verbatim to the provider. Only chains that expose
	// a path-based HTTP API support this.
	if path := r.PathValue("path"); path != "" {
		if endpoint.Kind != "tron" {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		internal.IncomingHTTPAPIHandler(ctx, endpoint, path, w, r, timing)
		return
	}

	if r.Header.Get("Upgrade") == "websocket" {
		hasWebsocketProvider := false
		for _, provider := range endpoint.Providers {
			if provider.Ws != "" {
				hasWebsocketProvider = true
				break
			}
		}

		if !hasWebsocketProvider {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		ws.IncomingWebsocketHandler(ctx, endpoint, w, r, timing)
		return
	}

	internal.IncomingHttpRpcHandler(ctx, endpoint, w, r, timing)
}

var config *internal.Config

const (
	DefaultConfigFile          = "config.yml"
	DefaultPort                = "127.0.0.1:8080"
	DefaultLogLevel            = "info"
	DefaultLogJSON             = false
	DefaultHealthcheckInterval = 30 * time.Second
	// Empty by default; metrics disabled unless explicitly set
	DefaultMetricsPort = ""
)

func main() {
	var configFile = flag.String("config", DefaultConfigFile, "config file (raw config YAML can be provided via $HA_CONFIG)")
	var port = flag.String("port", DefaultPort, "http service port ($HA_PORT)")
	var metricsPort = flag.String("metrics-port", DefaultMetricsPort, "metrics port ($HA_METRICS_PORT). Leave empty to disable.")
	var logLevel = flag.String("log-level", DefaultLogLevel, "logging level (debug, info, warn, error) ($HA_LOG_LEVEL)")
	var logJSON = flag.Bool("log-json", DefaultLogJSON, "enable JSON logging ($HA_LOG_JSON)")
	var healthcheckInterval = flag.String("healthcheck-interval", DefaultHealthcheckInterval.String(), "healthcheck interval duration (e.g. 10s, 1m) ($HA_HEALTHCHECK_INTERVAL)")

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

	if *metricsPort == "" {
		env := os.Getenv("HA_METRICS_PORT")
		if env != "" {
			*metricsPort = env
		} else if config.MetricsPort != "" {
			*metricsPort = config.MetricsPort
		}
	}

	if *healthcheckInterval == DefaultHealthcheckInterval.String() {
		env := os.Getenv("HA_HEALTHCHECK_INTERVAL")
		if env != "" {
			*healthcheckInterval = env
		} else if config.HealthcheckInterval != "" {
			*healthcheckInterval = config.HealthcheckInterval
		}
	}

	parsedHealthcheckInterval, err := time.ParseDuration(*healthcheckInterval)
	if err != nil {
		log.Fatalf("invalid healthcheck interval %q: %v", *healthcheckInterval, err)
	}
	if parsedHealthcheckInterval <= 0 {
		log.Fatalf("healthcheck interval must be > 0, got %q", *healthcheckInterval)
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
		// Human-friendly colored console output. Colors are disabled automatically
		// when stdout isn't a terminal (e.g. piped to a file or log collector).
		slog.SetDefault(slog.New(tint.NewHandler(os.Stdout, &tint.Options{
			Level:      level,
			TimeFormat: time.TimeOnly,
			NoColor:    !term.IsTerminal(int(os.Stdout.Fd())),
		})))
	}

	slog.Info("starting haprovider", "version", httpx.Version, "healthcheck_interval", parsedHealthcheckInterval.String())

	var wg sync.WaitGroup

	total := 0
	online := 0

	for _, endpoint := range config.Endpoints {
		for _, provider := range endpoint.Providers {
			provider.Logger().Info("connecting to", "http", provider.Http)

			wg.Add(1)
			go func() {
				total++

				start := time.Now()
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				err := healthcheck.Run(ctx, provider)
				cancel()
				took := time.Since(start)

				if err == nil {
					online++
					provider.MarkHealthy(took)
				} else {
					provider.MarkUnhealthy(err)
					provider.Logger().Error("provider failed", "error", err)
				}

				wg.Done()
			}()
		}
	}

	wg.Wait()

	for _, endpoint := range config.Endpoints {
		for _, provider := range endpoint.Providers {
			go func() {
				time.Sleep(parsedHealthcheckInterval)
				healthcheck.Worker(provider, parsedHealthcheckInterval)
			}()
		}
	}

	// Main application server
	mainMux := http.NewServeMux()
	timingMiddleware := servertiming.Middleware(http.HandlerFunc(requestHandler), nil)
	mainMux.Handle("/{id}", timingMiddleware)
	mainMux.Handle("/{id}/{path...}", timingMiddleware)

	mainServer := &http.Server{
		Addr:    *port,
		Handler: mainMux,
		// ReadHeaderTimeout defeats Slowloris (slow-header) attacks. ReadTimeout
		// and IdleTimeout bound slow bodies and idle keep-alives. WriteTimeout is
		// intentionally left unset: this server also serves WebSocket upgrades,
		// and a write deadline would abort long-lived streams. After gorilla
		// hijacks the connection the http.Server deadlines no longer apply, and
		// the WS pump manages its own write/pong deadlines.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Metrics server (optional)
	var metricsServer *http.Server

	if *metricsPort != "" {
		metricsMux := http.NewServeMux()
		metricsMux.Handle("/metrics", metrics.MetricsHandler())
		metricsServer = &http.Server{
			Addr:              *metricsPort,
			Handler:           metricsMux,
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       30 * time.Second,
			WriteTimeout:      30 * time.Second,
			IdleTimeout:       120 * time.Second,
		}
	}

	// Channel to catch server errors and exit fatally
	errCh := make(chan error, 2)

	// Start metrics server (if enabled)
	if metricsServer != nil {
		go func() {
			slog.Info("starting metrics server", "version", httpx.Version, "addr", metricsServer.Addr)
			if err := metricsServer.ListenAndServe(); err != http.ErrServerClosed {
				errCh <- err
			}
		}()
	}

	go func() {
		slog.Info(fmt.Sprintf("started. %d/%d providers connected", online, total), "version", httpx.Version, "addr", *port)
		if err := mainServer.ListenAndServe(); err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	// Handle graceful shutdown
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt, syscall.SIGTERM)

	// Wait for either interrupt signal or server error
	hadServerError := false
	select {
	case <-interrupt:
		slog.Info("shutting down...")
	case err := <-errCh:
		slog.Error("startup failure", "error", err)
		// Proceed to shutdown and then exit non-zero
		hadServerError = true
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Shutdown servers
	if metricsServer != nil {
		_ = metricsServer.Shutdown(ctx)
	}
	_ = mainServer.Shutdown(ctx)

	// If we exited due to a server error, terminate with non-zero status
	if hadServerError {
		os.Exit(1)
	}
}
