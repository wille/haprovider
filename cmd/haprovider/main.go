package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	. "github.com/wille/haprovider/internal"
)

var addr = flag.String("addr", "localhost:8080", "http service address")

var upgrader = websocket.Upgrader{} // use default options

func NewProxy(context context.Context, provider *Provider, clientConn *websocket.Conn) *Proxy {
	proxy := &Proxy{
		ID:           clientConn.RemoteAddr().String(),
		ClientConn:   clientConn,
		ClientChan:   make(chan *RPCResponse, 32),
		ProviderChan: make(chan *RPCRequest, 32),
		Listeners:    make(map[string]chan *RPCResponse),
		ErrorChan:    make(chan error, 1),
	}

	return proxy
}

// Incoming requests
func server(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	provider := config.Providers[id]

	r.Header.Set("Server", "haprovider")

	if provider == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	if r.Header.Get("Upgrade") != "websocket" {
		IncomingHttpHandler(provider, w, r)
		return
	}

	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Println("Error upgrading connection", err)
		return
	}

	proxy := NewProxy(r.Context(), provider, ws)

	proxy.Provider, err = ProxyWebSocket(provider, proxy)

	if err != nil {
		proxy.Close()
		log.Printf("failed %s", err)
		return
	}

	ErrorChan := proxy.ErrorChan

	defer proxy.Close()

	// From upstream to client
	go func() {
		for {
			select {
			case <-r.Context().Done():
				ErrorChan <- fmt.Errorf("client connection closed")
				return
			case rpcResponse := <-proxy.ClientChan:
				ss := SerializeRPCResponse(rpcResponse)

				err := proxy.ClientConn.WriteMessage(websocket.TextMessage, ss)
				if err != nil {
					log.Printf("write err: %s", err)
					return
				}
			}
		}
	}()

	// From client to upstream
	for {
		mt, message, err := proxy.ClientConn.ReadMessage()
		if err != nil {
			fmt.Errorf("read: %s", err)
			break
		}

		if mt != websocket.TextMessage {
			log.Printf("Ignoring unknown message type %d\n", mt)
			continue
		}

		var req RPCRequest
		err = json.Unmarshal(message, &req)
		if err != nil {
			fmt.Errorf("decode: %s, msg: %s", err, message)
			continue
		}

		reqID, _ := GetClientID(req.ID)

		log.Printf("%s %s [%s] %s\n", proxy.ClientConn.RemoteAddr().String(), proxy.Provider.Endpoint.GetName(), reqID, req.Method)

		// Overwrite client request ID with ours
		// if req.ID != nil {
		// 	req.ID = SetClientID(client.ID, req.ID)
		// }

		// > {"jsonrpc":"2.0","id": 1, "method": "eth_subscribe", "params": ["newHeads"]}
		// < {"jsonrpc":"2.0", "id":1, "result":"0x9ce59a13059e417087c02d3236a0b1cc"}

		proxy.ProviderChan <- &req

		if err != nil {
			fmt.Errorf("write: %s", err)
			break
		}
	}

}

var config = LoadConfig()

func main() {
	log.SetFlags(log.Ltime)
	http.HandleFunc("/{id}", server)

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

			log.Printf("Loaded endpoint %s/%s (%s, %s)\n", name, endpoint.Name, endpoint.Http, endpoint.Ws)
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
