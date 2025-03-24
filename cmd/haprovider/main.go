package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	servertiming "github.com/mitchellh/go-server-timing"
	. "github.com/wille/haprovider/internal"
)

// Incoming requests
func requestHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Create a new timing handler
	timing := servertiming.FromContext(ctx)

	id := r.PathValue("id")

	provider := config.Providers[id]

	w.Header().Set("Server", "haprovider/"+Version)

	if provider == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	if r.Header.Get("Upgrade") == "websocket" {
		IncomingWebsocketHandler(ctx, provider, w, r, timing)
		return
	}

	IncomingHttpHandler(ctx, provider, w, r, timing)
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
	var configFile = flag.String("config", "config.yml", "config file (Raw config can be provided via $HA_CONFIG)")
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

	for name, provider := range config.Providers {
		provider.Active = make(map[string]*WebSocketProxy)

		switch provider.Kind {
		case "", "eth":
			provider.Kind = "eth"
		case "solana", "btc":
		default:
			log.Fatalf("Unknown provider kind %s", provider.Kind)
		}

		provider.Name = name

		for i, endpoint := range provider.Endpoint {
			endpoint.ProviderName = name

			if endpoint.Name == "" {
				endpoint.Name = fmt.Sprintf("%s/%d", name, i)
			}
			if endpoint.Ws != "" {
				url, err := url.Parse(endpoint.Ws)

				if err != nil || url.Scheme == "http" || url.Scheme == "https" {
					log.Fatalf("Invalid Websocket URL: %s", endpoint.Ws)
				}
			}
			if endpoint.Http != "" {
				url, err := url.Parse(endpoint.Http)

				if err != nil || url.Scheme == "ws" || url.Scheme == "wss" {
					log.Fatalf("Invalid HTTP URL: %s", endpoint.Http)
				}
			}
		}
	}

	log.Printf("Performing initial healthcheck...")

	var wg sync.WaitGroup

	total := 0
	online := 0

	for name, provider := range config.Providers {
		for _, endpoint := range provider.Endpoint {

			if endpoint.Http != "" {
				log.Printf("Connecting to %s/%s (%s)\n", name, endpoint.Name, endpoint.Http)

				wg.Add(1)
				go func() {
					total++

					err := provider.HTTPHealthcheck(endpoint)

					if err == nil {
						online++
					}

					wg.Done()

					c := time.NewTicker(10 * time.Second)
					for {
						select {
						case <-c.C:
							provider.HTTPHealthcheck(endpoint)
						}
					}
				}()
			} else if endpoint.Ws != "" {
				// No initial healthcheck on websocket providers yet
				// log.Printf("Connecting to %s\n", endpoint.Ws)
				// NewWebsocketConnection(endpoint, endpoint)
				endpoint.SetStatus(true, nil)
			}
		}
	}

	wg.Wait()

	if total == 0 {
		log.Fatalf("All providers offline")
	}

	log.Printf("%d/%d providers available\n", online, total)

	server := http.Server{
		Addr:    *addr,
		Handler: http.DefaultServeMux,
	}

	go server.ListenAndServe()

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt, syscall.SIGTERM)

	<-interrupt

	log.Printf("shutting down...")

	wg = sync.WaitGroup{}
	for _, provider := range config.Providers {
		provider.ActiveMu.RLock()
		for _, conn := range provider.Active {
			wg.Add(1)
			go func() {
				conn.Close(fmt.Errorf("shutting down"))
				conn.ProviderConn.Close(websocket.CloseGoingAway, nil)
				conn.ClientConn.Close(websocket.CloseGoingAway, nil)
				wg.Done()
			}()
		}
		provider.ActiveMu.RUnlock()
	}

	wg.Wait()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	server.Shutdown(ctx)

	log.Printf("shutdown complete")
}
