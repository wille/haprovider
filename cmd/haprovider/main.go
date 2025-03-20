package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"

	servertiming "github.com/mitchellh/go-server-timing"
	. "github.com/wille/haprovider/internal"
)

var addr = flag.String("addr", "0.0.0.0:8080", "http service address")
var configFile = flag.String("config", "config.yml", "config file")
var verbose = flag.Bool("verbose", false, "verbose logging")

// Incoming requests
func requestHandler(w http.ResponseWriter, r *http.Request) {
	// Create a new timing handler
	timing := servertiming.FromContext(r.Context())

	id := r.PathValue("id")

	provider := config.Providers[id]

	w.Header().Set("Server", "haprovider/"+Version)

	if provider == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	if r.Header.Get("Upgrade") == "websocket" {
		IncomingWebsocketHandler(r.Context(), provider, w, r, timing)
		return
	}

	IncomingHttpHandler(r.Context(), provider, w, r, timing)
}

var config = LoadConfig()

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)

	flag.Parse()

	timingMiddleware := servertiming.Middleware(http.HandlerFunc(requestHandler), nil)

	http.Handle("/{id}", timingMiddleware)

	for name, provider := range config.Providers {
		if provider.ChainID == 0 {
			log.Printf("ChainID not set for provider %v, skipping validation\n", provider)
		}

		switch provider.Kind {
		case "", "eth", "solana", "btc":
			if provider.Kind == "" {
				provider.Kind = "eth"
			}
		default:
			log.Fatalf("Unknown provider kind %s", provider.Kind)
		}

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

	log.Fatal(http.ListenAndServe(*addr, nil))
}
