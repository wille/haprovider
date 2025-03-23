package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
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

var (
	ActiveConnections   = make(map[string]*WebSocketProxy)
	activeConnectionsMu = sync.RWMutex{}
)

type WebSocketProxy struct {
	ctx    context.Context
	cancel context.CancelCauseFunc

	provider *Provider
	endpoint *Endpoint

	ID string

	ClientConn *Client

	ProviderConn *Client

	// Client requests to be sent to the provider
	Requests chan *RPCRequest

	// Provider responses to be sent to the client
	Responses chan *RPCResponse

	listeners map[string]chan *RPCResponse

	closed bool
}

func (r *WebSocketProxy) SendRPCRequest(req *RPCRequest) error {
	b := SerializeRPCRequest(req)

	select {
	case r.ProviderConn.send <- b:
	case <-r.ProviderConn.ctx.Done():
		return fmt.Errorf("provider connection closed")
	}

	return nil
}

func (r *WebSocketProxy) AwaitReply(ctx context.Context, requestID string, req *RPCRequest) (*RPCResponse, error) {
	req.ID = requestID

	ch := make(chan *RPCResponse, 1)
	defer close(ch)

	r.listeners[requestID] = ch
	defer delete(r.listeners, requestID)

	if err := r.SendRPCRequest(req); err != nil {
		return nil, err
	}

	select {
	case reply := <-ch:
		return reply, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (r *WebSocketProxy) healthcheck() error {
	// Skip chainID validation if not set in config
	if r.provider.ChainID != 0 {
		res, err := r.AwaitReply(r.ctx, "ha_chainId", NewRPCRequest("eth_chainId", nil))
		if err != nil {
			return err
		}

		i, err := HexToInt(res.Result.(string))
		if err != nil {
			return fmt.Errorf("invalid chainId received: %s (%s)", res.Result, err)
		}

		if int(i) != r.provider.ChainID {
			return fmt.Errorf("chainId mismatch: received=%d, expected=%d", i, r.provider.ChainID)
		}
	}

	return nil
}

var closeCodeText = map[int]string{
	websocket.CloseNormalClosure:      "normal close",
	websocket.CloseGoingAway:          "going away",
	websocket.CloseProtocolError:      "protocol error",
	websocket.CloseUnsupportedData:    "unsupported data",
	websocket.CloseNoStatusReceived:   "no status received",
	websocket.CloseAbnormalClosure:    "abnormal closure",
	websocket.CloseServiceRestart:     "service restart",
	websocket.CloseTryAgainLater:      "try again later",
	websocket.CloseTLSHandshake:       "TLS handshake",
	websocket.CloseMessageTooBig:      "message too big",
	websocket.CloseMandatoryExtension: "mandatory extension",
	websocket.CloseInternalServerErr:  "internal server error",
}

// Close destroys the proxy connection.
// It's up to the caller to close the client and provider connections.
func (p *WebSocketProxy) Close(reason error) {
	p.cancel(reason)
}

func DialWebSocketProvider(provider *Provider, endpoint *Endpoint, proxy *WebSocketProxy) error {
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
		return fmt.Errorf("unexpected status code %d", resp.StatusCode)
	}

	providerClient := NewClient(context.TODO(), nil, ws)

	proxy.ProviderConn = providerClient
	proxy.endpoint = endpoint

	go proxy.pumpProvider(proxy.ProviderConn)

	clientVersion, err := proxy.AwaitReply(proxy.ctx, "ha_clientVersion", NewRPCRequest("web3_clientVersion", nil))
	if err != nil {
		providerClient.Close(websocket.CloseNormalClosure, nil)
		return fmt.Errorf("web3_clientVersion error: %s", err)
	}
	endpoint.clientVersion = clientVersion.Result.(string)

	if err = proxy.healthcheck(); err != nil {
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
			proxy.SendRPCRequest(req)
		case message := <-providerClient.Read():
			if strings.Contains(string(message), "error") {
				log.Println("error:", string(message))
			}

			var rpcCall RPCResponse
			err := json.Unmarshal(message, &rpcCall)
			if err != nil {
				proxy.Close(fmt.Errorf("received bad data: %s, msg: %s", err, FormatRawBody(string(message))))
				proxy.ProviderConn.Close(websocket.CloseUnsupportedData, nil)
				proxy.ClientConn.Close(websocket.CloseUnsupportedData, fmt.Errorf("received bad data: %s, msg: %s", err, FormatRawBody(string(message))))
				proxy.provider.failedRequestCount++
				return
			}

			reqID, _ := GetClientID(rpcCall.ID)

			// Intercept
			if ch := proxy.listeners[reqID]; ch != nil {
				ch <- &rpcCall
				continue
			}

			// TODO log subscription messages
			proxy.provider.requestCount++

			proxy.Responses <- &rpcCall

			// -32005
			if rpcCall.Error != nil {
				fmt.Println(rpcCall.Error.(map[string]any)["code"])
				if rpcCall.Error.(map[string]any)["code"].(float64) == -32005 {
					// TODO rate limited!
					r := fmt.Errorf("rate limited by provider: %s", rpcCall.Error.(map[string]any)["message"])
					// proxy.cancel(fmt.Errorf("rate limit and die"))
					proxy.Close(r)
					proxy.ProviderConn.Close(websocket.CloseGoingAway, fmt.Errorf("disconnecting because we got rate limited"))
					proxy.ClientConn.Close(websocket.CloseServiceRestart, r)

					proxy.provider.failedRequestCount++

					// log.Printf("rate limited! disconnecting from %s: %s", proxy.endpoint.GetName(), rpcCall.Error.(map[string]any)["message"])

					return
				}
			}
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
				log.Printf("ClientConn read closed")
				continue
			}

			var req RPCRequest
			err := json.Unmarshal(message, &req)
			if err != nil {
				log.Printf("bad client request. ignoring: %s, msg: %s", err, FormatRawBody(string(message)))
				continue
			}

			log.Printf("%s %s [%f] %s\n", proxy.ClientConn.Conn.RemoteAddr().String(), proxy.endpoint.GetName(), req.ID, req.Method)

			proxy.Requests <- &req
		case rpcResponse, ok := <-proxy.Responses:
			if !ok {
				log.Printf("proxy.Responses closed")
				continue
			}
			ss := SerializeRPCResponse(rpcResponse)
			proxy.ClientConn.Write(ss)

		}
	}
}

// DialAnyProvider dials any provider and returns a WebSocketProxy
func DialAnyProvider(provider *Provider, proxy *WebSocketProxy, timing *servertiming.Header) (*Endpoint, error) {
	for _, endpoint := range provider.GetActiveEndpoints() {
		if endpoint.Ws == "" {
			continue
		}

		m := timing.NewMetric(endpoint.GetName()).Start()
		err := DialWebSocketProvider(provider, endpoint, proxy)
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
	ctx, cancel := context.WithCancelCause(ctx)

	proxy := &WebSocketProxy{
		ID:        r.RemoteAddr,
		ctx:       ctx,
		cancel:    cancel,
		provider:  provider,
		Responses: make(chan *RPCResponse, 32),
		Requests:  make(chan *RPCRequest, 32),
		listeners: make(map[string]chan *RPCResponse),
	}

	activeConnectionsMu.Lock()
	ActiveConnections[proxy.ID] = proxy
	activeConnectionsMu.Unlock()
	defer func() {
		activeConnectionsMu.Lock()
		delete(ActiveConnections, proxy.ID)
		activeConnectionsMu.Unlock()
	}()

	// Dial any provider before upgrading the websocket
	endpoint, err := DialAnyProvider(provider, proxy, timing)

	if err == ErrNoProvidersAvailable {
		log.Printf("%s [%s]: no providers available", proxy.ID, provider.Name)
		http.Error(w, "no providers available", http.StatusServiceUnavailable)
		return
	}

	if err != nil {
		log.Printf("error dialing provider: %s", err)
		http.Error(w, "error dialing provider: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("X-Provider", endpoint.GetName())

	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Println("Error upgrading connection", err)
		proxy.ProviderConn.Close(websocket.CloseNormalClosure, fmt.Errorf(""))
		return
	}

	proxy.ClientConn = NewClient(context.TODO(), nil, ws)

	log.Printf("ws open %s -> %s [%s]\n", proxy.ID, endpoint.GetName(), endpoint.clientVersion)
	logClose := func(reason error) {
		log.Printf("ws close %s -> %s: %s", proxy.ID, endpoint.GetName(), reason)
	}

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt, syscall.SIGTERM)

	go proxy.pumpClient(proxy.ClientConn)

	for {
		select {
		case <-interrupt:
			// Gracefully close the connection both ways
			proxy.ClientConn.Close(websocket.CloseGoingAway, fmt.Errorf("haprovider shutting down"))
			proxy.ProviderConn.Close(websocket.CloseGoingAway, nil)
			return
		case <-proxy.ctx.Done():
			// We closed the connection
			logClose(context.Cause(proxy.ctx))
			return
		case <-proxy.ClientConn.ctx.Done():
			logClose(fmt.Errorf("client connection closed: %s", context.Cause(proxy.ClientConn.ctx)))
			proxy.ProviderConn.Close(websocket.CloseGoingAway, fmt.Errorf("client connection closed: %s", context.Cause(proxy.ClientConn.ctx)))
			return
		case <-proxy.ProviderConn.ctx.Done():
			logClose(fmt.Errorf("provider connection closed: %s", context.Cause(proxy.ProviderConn.ctx)))
			proxy.ClientConn.Close(websocket.CloseTryAgainLater, fmt.Errorf("provider connection closed: %s", context.Cause(proxy.ProviderConn.ctx)))
			return
		}
	}
}
