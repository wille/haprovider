package internal

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	servertiming "github.com/mitchellh/go-server-timing"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{} // use default options

const (
	// Time allowed to write a message to the peer.
	writeWait = 10 * time.Second

	// Time allowed to read the next pong message from the peer.
	pongWait = 10 * time.Second

	// Send pings to peer with this period. Must be less than pongWait.
	pingPeriod = 5 * time.Second
)

type WebSocketProxy struct {
	// The request context
	ctx    context.Context
	cancel context.CancelCauseFunc

	log *slog.Logger

	provider *Provider
	endpoint *Endpoint

	// The ID of the proxy
	ID string

	// ClientConn is the incoming client connection
	ClientConn *Client

	// ProviderConn is the upstream provider connection
	ProviderConn *Client

	// Client requests to be sent to the provider
	Requests chan *RPCRequest

	// Provider responses to be sent to the client
	Responses chan *RPCResponse

	subscriptions map[string]chan *RPCResponse

	badRequests int
}

func (r *WebSocketProxy) AwaitReply(ctx context.Context, req *RPCRequest) (*RPCResponse, error) {
	ch := make(chan *RPCResponse, 1)
	defer close(ch)

	id := GetRequestIDString(req.ID)

	r.subscriptions[id] = ch
	defer delete(r.subscriptions, id)

	r.Requests <- req

	select {
	case reply := <-ch:
		return reply, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (r *WebSocketProxy) Healthcheck() error {
	ctx, cancel := context.WithTimeout(r.ctx, time.Second)
	defer cancel()

	// Skip chainID validation if not set in config
	if r.provider.ChainID != 0 {
		res, err := r.AwaitReply(ctx, NewRPCRequest("ha_chainId", "eth_chainId", nil))
		if err != nil {
			return err
		}

		if res.Result.(string) != fmt.Sprintf("0x%x", r.provider.ChainID) {
			err = fmt.Errorf("chainId mismatch: received=%s, expected=%d", res.Result, r.provider.ChainID)
			return err
		}
	}

	_, err := r.AwaitReply(ctx, NewRPCRequest("ha_height", "eth_blockNumber", nil))
	if err != nil {
		return err
	}

	return nil
}

// Close destroys the proxy connection.
// It's up to the caller to close the client and provider connections.
func (p *WebSocketProxy) Close(reason error) {
	p.cancel(reason)
}

func (proxy *WebSocketProxy) DialProvider(provider *Provider, endpoint *Endpoint) error {
	u, _ := url.Parse(endpoint.Ws)

	headers := http.Header{}
	headers.Set("User-Agent", "haprovider/"+Version)

	ctx, cancel := context.WithTimeout(proxy.ctx, endpoint.GetTimeout())
	defer cancel()

	var dialer = &websocket.Dialer{
		Proxy:             http.ProxyFromEnvironment,
		HandshakeTimeout:  endpoint.GetTimeout(),
		EnableCompression: true,
	}
	ws, resp, err := dialer.DialContext(ctx, u.String(), headers)
	if err != nil {
		return err
	}

	switch resp.StatusCode {
	case http.StatusSwitchingProtocols:
		break
	case http.StatusTooManyRequests: // Some providers might send a 429 on the websocket connection attempt
		ws.Close()
		return endpoint.HandleTooManyRequests(resp)
	default:
		ws.Close()
		return fmt.Errorf("status code %d", resp.StatusCode)
	}

	providerClient := NewClient(ws)

	proxy.ProviderConn = providerClient
	proxy.endpoint = endpoint

	go proxy.pumpProvider(proxy.ProviderConn)

	clientVersion, err := proxy.AwaitReply(ctx, NewRPCRequest("ha_clientVersion", "web3_clientVersion", nil))
	if err != nil {
		providerClient.Close(websocket.CloseNormalClosure, nil)
		return fmt.Errorf("web3_clientVersion error: %s", err)
	}
	endpoint.clientVersion = clientVersion.Result.(string)

	if err = proxy.Healthcheck(); err != nil {
		providerClient.Close(websocket.CloseNormalClosure, nil)
		return fmt.Errorf("healthcheck failed: %s", err)
	}

	return nil
}

func (proxy *WebSocketProxy) pumpProvider(providerClient *Client) {
	for {
		select {
		case <-providerClient.ctx.Done():
			return
		case req := <-proxy.Requests:
			providerClient.Write(SerializeRPCRequest(req))
		case message := <-providerClient.Read():
			if strings.Contains(string(message), "error") {
				log.Println("error:", string(message))
			}

			rpcResponse, err := DecodeRPCResponse(message)
			if err != nil {
				err := fmt.Errorf("received bad data from provider: %s, msg: %s", err, FormatRawBody(string(message)))
				proxy.Close(err)
				proxy.ProviderConn.Close(websocket.CloseUnsupportedData, nil)
				proxy.ClientConn.Close(websocket.CloseUnsupportedData, err)
				proxy.provider.failedRequestCount++
				return
			}

			proxy.provider.requestCount++

			id := GetRequestIDString(rpcResponse.ID)
			// Intercept
			if ch := proxy.subscriptions[id]; ch != nil {
				ch <- rpcResponse
				continue
			}

			if rpcResponse.IsError() {
				errorCode, _ := rpcResponse.GetError()

				if errorCode == RateLimited {
					// Set the provider as offline
					err = proxy.endpoint.HandleTooManyRequests(nil)
					proxy.endpoint.SetStatus(false, err)

					// Forward the error to the client
					proxy.Responses <- rpcResponse

					// Close the connection
					proxy.Close(err)
					proxy.ProviderConn.Close(websocket.CloseGoingAway, nil)
					proxy.ClientConn.Close(websocket.CloseTryAgainLater, err)
					proxy.provider.failedRequestCount++
					return
				}
			}

			proxy.Responses <- rpcResponse
		}
	}
}

func (proxy *WebSocketProxy) pumpClient(client *Client) {
	for {
		select {
		case <-client.ctx.Done():
			return
		case message, ok := <-proxy.ClientConn.Read():
			if !ok {
				proxy.log.Debug("ClientConn read closed")
				continue
			}

			req, err := DecodeRPCRequest(message)
			if err != nil {
				proxy.log.Debug("bad client request", "error", err, "msg", FormatRawBody(string(message)))

				proxy.badRequests++

				// Drop clients who sends too many bad requests
				if proxy.badRequests > 10 {
					proxy.Close(fmt.Errorf("closing client: too many bad requests"))
					proxy.ClientConn.Close(websocket.CloseUnsupportedData, nil)
					proxy.ProviderConn.Close(websocket.CloseGoingAway, nil)
					return
				}

				continue
			}

			// Reset the bad request counter
			proxy.badRequests = 0

			proxy.Requests <- req

			id := GetRequestIDString(req.ID)
			proxy.log.Debug("request", "rpc_id", id, "method", req.Method)
		case rpcResponse, ok := <-proxy.Responses:
			if !ok {
				proxy.log.Debug("proxy.Responses closed")
				continue
			}
			ss := SerializeRPCResponse(rpcResponse)
			proxy.ClientConn.Write(ss)

		}
	}
}

// DialAnyProvider dials any provider and returns a WebSocketProxy
func (proxy *WebSocketProxy) DialAnyProvider(provider *Provider, timing *servertiming.Header) (*Endpoint, error) {
	for _, endpoint := range provider.GetActiveEndpoints() {
		if endpoint.Ws == "" {
			continue
		}

		m := timing.NewMetric(endpoint.GetName()).Start()
		err := proxy.DialProvider(provider, endpoint)
		m.Stop()

		// In case the request was closed before the provider connection was established
		// return to not treat the error as a provider error
		select {
		case <-proxy.ctx.Done():
			if err != nil {
				proxy.ProviderConn.Close(websocket.CloseGoingAway, nil)
			}
			return nil, proxy.ctx.Err()
		default:
		}

		if err != nil {
			endpoint.SetStatus(false, err)
			continue
		}

		provider.openConnections++

		endpoint.SetStatus(true, nil)

		return endpoint, nil
	}

	return nil, ErrNoProvidersAvailable
}

func IncomingWebsocketHandler(ctx context.Context, provider *Provider, w http.ResponseWriter, r *http.Request, timing *servertiming.Header) {
	start := time.Now()

	ctx, cancel := context.WithCancelCause(ctx)

	proxy := &WebSocketProxy{
		ID:            GetRequestID(r),
		log:           slog.With("ip", r.RemoteAddr, "provider", provider.Name),
		ctx:           ctx,
		cancel:        cancel,
		provider:      provider,
		Responses:     make(chan *RPCResponse, 32),
		Requests:      make(chan *RPCRequest, 32),
		subscriptions: make(map[string]chan *RPCResponse),
	}

	// provider.ActiveMu.Lock()
	// provider.Active[r.RemoteAddr] = proxy
	// provider.ActiveMu.Unlock()
	// defer func() {
	// 	provider.ActiveMu.Lock()
	// 	delete(provider.Active, r.RemoteAddr)
	// 	provider.ActiveMu.Unlock()
	// }()

	fmt.Println("dialing provider")

	// Dial any provider before upgrading the websocket
	endpoint, err := proxy.DialAnyProvider(provider, timing)

	if err == ErrNoProvidersAvailable {
		proxy.log.Error("no providers available")
		http.Error(w, "no providers available", http.StatusServiceUnavailable)
		return
	}

	if err != nil {
		select {
		case <-proxy.ctx.Done():
			proxy.log.Error("client closed the connection", "error", err)
		default:
			proxy.log.Error("error dialing provider", "error", err)
			http.Error(w, "error dialing provider: "+err.Error(), http.StatusInternalServerError)
		}

		return
	}

	if !provider.Public {
		w.Header().Set("X-Provider", endpoint.GetName())
	}

	if provider.Xfwd {
		AddXfwdHeaders(r, w)
	}

	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		proxy.log.Error("error upgrading connection", "error", err)
		proxy.ProviderConn.Close(websocket.CloseNormalClosure, nil)
		return
	}

	proxy.ClientConn = NewClient(ws)
	go proxy.pumpClient(proxy.ClientConn)

	proxy.log = proxy.log.With("endpoint", endpoint.GetName())
	proxy.log.Info("ws open", "client_version", endpoint.clientVersion, "request_time", time.Since(start))

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt, syscall.SIGTERM)

	for {
		select {
		case <-interrupt:
			// Gracefully close the connection both ways
			proxy.ClientConn.Close(websocket.CloseGoingAway, fmt.Errorf("haprovider shutting down"))
			proxy.ProviderConn.Close(websocket.CloseGoingAway, nil)
			return
		case <-proxy.ctx.Done():
			// We closed the connection
			proxy.log.Error("ws closed", "error", context.Cause(proxy.ctx))
			return
		case <-proxy.ClientConn.ctx.Done():
			proxy.log.Info("client connection closed", "error", context.Cause(proxy.ClientConn.ctx))
			proxy.ProviderConn.Close(websocket.CloseGoingAway, fmt.Errorf("client connection closed: %w", context.Cause(proxy.ClientConn.ctx)))
			return
		case <-proxy.ProviderConn.ctx.Done():
			proxy.log.Error("provider connection closed", "error", context.Cause(proxy.ProviderConn.ctx))
			proxy.ClientConn.Close(websocket.CloseTryAgainLater, fmt.Errorf("provider connection closed: %w", context.Cause(proxy.ProviderConn.ctx)))
			return
		}
	}
}
