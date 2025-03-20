package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	servertiming "github.com/mitchellh/go-server-timing"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{} // use default options

const (
	// Time allowed to write a message to the peer.
	writeWait = 10 * time.Second

	// Time allowed to read the next pong message from the peer.
	pongWait = 60 * time.Second

	// Send pings to peer with this period. Must be less than pongWait.
	pingPeriod = (pongWait * 9) / 10
)

type ProviderWSConnection struct {
	ctx context.Context

	// Conn is the underlying websocket connection to the provider
	*websocket.Conn

	// Endpoint is the endpoint we're connected to
	*Endpoint

	// Provider is the provider we're connected to
	*Provider

	// listeners is a map of request IDs to channels for the responses
	listeners map[string]chan *RPCResponse
}

func (r *ProviderWSConnection) Write(req *RPCRequest) error {
	b := SerializeRPCRequest(req)

	// TODO adjustable timeout
	r.Conn.SetWriteDeadline(time.Now().Add(writeWait))

	err := r.Conn.WriteMessage(websocket.TextMessage, b)
	return err
}

func (r *ProviderWSConnection) AwaitReply(ctx context.Context, requestID string, req *RPCRequest) (*RPCResponse, error) {
	req.ID = requestID

	ch := make(chan *RPCResponse, 1)
	defer close(ch)

	r.listeners[requestID] = ch
	defer delete(r.listeners, requestID)

	if err := r.Write(req); err != nil {
		return nil, err
	}

	select {
	case reply := <-ch:
		return reply, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (r *ProviderWSConnection) healthcheck() error {
	// Skip chainID validation if not set in config
	if r.Provider.ChainID != 0 {
		res, err := r.AwaitReply(r.ctx, "ha_chainId", NewRPCRequest("eth_chainId", nil))
		if err != nil {
			return err
		}

		i, err := HexToInt(res.Result.(string))
		if err != nil {
			return fmt.Errorf("invalid chainId received: %s (%s)", res.Result, err)
		}

		if int(i) != r.Provider.ChainID {
			return fmt.Errorf("chainId mismatch: received=%d, expected=%d", i, r.Provider.ChainID)
		}
	}

	return nil
}

type WebSocketProxy struct {
	ctx    context.Context
	cancel context.CancelFunc

	provider *Provider
	endpoint *Endpoint

	ID string

	ClientConn *websocket.Conn
	Responses  chan *RPCResponse

	ProviderConn *ProviderWSConnection
	Requests     chan *RPCRequest

	closed bool
}

// Close gracefully closes the proxy connection
func (p *WebSocketProxy) Close(reason error) {
	if p.closed {
		return
	}
	p.closed = true

	log.Printf("Connection %s: %s\n", p.ID, reason)

	if p.ProviderConn != nil {
		// p.ClientConn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		p.ClientConn.Close()
	}

	if p.ProviderConn != nil {
		// p.Provider.Conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		p.ProviderConn.Conn.Close()
	}

	p.provider.openConnections--
	p.cancel()

	close(p.Responses)
	close(p.Requests)
}

func DialWebSocketProvider(ctx context.Context, provider *Provider, endpoint *Endpoint, proxy *WebSocketProxy) (*ProviderWSConnection, error) {
	u, _ := url.Parse(endpoint.Ws)

	headers := http.Header{}
	headers.Set("User-Agent", "haprovider/"+Version)

	// TODO adjustable timeout
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	ws, resp, err := websocket.DefaultDialer.DialContext(ctx, u.String(), headers)
	if err != nil {
		return nil, err
	}

	switch resp.StatusCode {
	case http.StatusSwitchingProtocols:
		break
	case http.StatusTooManyRequests: // Some providers might send a 429 on the websocket connection attempt
		ws.Close()
		return nil, endpoint.HandleTooManyRequests(resp)
	default:
		ws.Close()
		return nil, fmt.Errorf("unexpected status code %d", resp.StatusCode)
	}

	conn := &ProviderWSConnection{
		ctx:       ctx,
		Conn:      ws,
		Endpoint:  endpoint,
		Provider:  provider,
		listeners: make(map[string]chan *RPCResponse),
	}

	go conn.OpenProviderConnection(proxy)

	clientVersion, err := conn.AwaitReply(ctx, "ha_clientVersion", NewRPCRequest("web3_clientVersion", nil))
	if err != nil {
		ws.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		ws.Close()
		return nil, fmt.Errorf("web3_clientVersion error: %s", err)
	}
	endpoint.clientVersion = clientVersion.Result.(string)

	if err = conn.healthcheck(); err != nil {
		ws.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		ws.Close()
		return nil, fmt.Errorf("healthcheck failed: %s", err)
	}

	log.Printf("websocket %s -> %s [%s]\n", proxy.ClientConn.NetConn().RemoteAddr(), u.String(), endpoint.clientVersion)

	return conn, nil
}

func (conn *ProviderWSConnection) OpenProviderConnection(proxy *WebSocketProxy) {
	ws := conn.Conn

	// interrupt := make(chan os.Signal, 1)
	// signal.Notify(interrupt, os.Interrupt)

	ticker := time.NewTicker(pingPeriod)
	defer ticker.Stop()

	// From client to provider
	go func() {
		for {
			select {
			case <-proxy.ctx.Done():
				return
			case req := <-proxy.Requests:
				err := conn.Write(req)
				if err != nil {
					proxy.Close(fmt.Errorf("write to provider: %s", err))
					return
				}
			case <-ticker.C:
				ws.SetWriteDeadline(time.Now().Add(writeWait))
				if err := ws.WriteMessage(websocket.PingMessage, nil); err != nil {
					proxy.Close(fmt.Errorf("write to provider: %s", err))
					return
				}
			}

		}
	}()

	ws.SetReadDeadline(time.Now().Add(pongWait))
	ws.SetPongHandler(func(string) error {
		ws.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	// From provider to client
	for {
		// TODO adjustable timeout
		mt, message, err := ws.ReadMessage()
		if err != nil {
			proxy.Close(fmt.Errorf("read from provider: %s", err))
			break
		}

		// TODO message validation
		if mt != websocket.TextMessage {
			log.Printf("Ignoring unknown message type %d\n", mt)
			continue
		}

		if strings.Contains(string(message), "error") {
			log.Println("error:", string(message))
		}

		var rpcCall RPCResponse
		err = json.Unmarshal(message, &rpcCall)
		if err != nil {
			proxy.provider.failedRequestCount++
			proxy.Close(fmt.Errorf("decode: %s, msg: %s", err, message))
			return
		}

		if rpcCall.Error != nil {
			// TODO don't fail on RPC error unless fatal  like a rate limit
			proxy.Close(fmt.Errorf("RPC error %s", rpcCall.Error))
			proxy.provider.failedRequestCount++
			return
		}

		reqID, _ := GetClientID(rpcCall.ID)

		// Intercept
		if ch := conn.listeners[reqID]; ch != nil {
			ch <- &rpcCall
			continue
		}

		// TODO log subscription messages
		proxy.provider.requestCount++

		proxy.Responses <- &rpcCall
	}
}

func NewWebSocketProxy(ctx context.Context, provider *Provider, clientConn *websocket.Conn) (*WebSocketProxy, error) {
	ctx, cancel := context.WithCancel(ctx)

	proxy := &WebSocketProxy{
		ctx:        ctx,
		cancel:     cancel,
		provider:   provider,
		ID:         clientConn.RemoteAddr().String(),
		ClientConn: clientConn,
		Responses:  make(chan *RPCResponse, 32),
		Requests:   make(chan *RPCRequest, 32),
	}

	for _, endpoint := range provider.GetActiveEndpoints() {
		if endpoint.Ws == "" {
			continue
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		conn, err := DialWebSocketProvider(ctx, provider, endpoint, proxy)
		if err != nil {
			endpoint.SetStatus(false, err)
			continue
		}

		provider.openConnections++
		proxy.endpoint = endpoint
		proxy.ProviderConn = conn

		endpoint.SetStatus(true, nil)

		return proxy, nil
	}

	return nil, ErrNoProvidersAvailable
}

func IncomingWebsocketHandler(ctx context.Context, provider *Provider, w http.ResponseWriter, r *http.Request, timing *servertiming.Header) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Println("Error upgrading connection", err)
		return
	}

	proxy, err := NewWebSocketProxy(ctx, provider, ws)

	if err != nil {
		http.Error(w, "failed to connect: "+err.Error(), http.StatusBadGateway)
		return
	}

	ticker := time.NewTicker(pingPeriod)
	defer ticker.Stop()

	// From provider to client
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case rpcResponse := <-proxy.Responses:
				ss := SerializeRPCResponse(rpcResponse)

				proxy.ClientConn.SetWriteDeadline(time.Now().Add(writeWait))
				err := proxy.ClientConn.WriteMessage(websocket.TextMessage, ss)
				if err != nil {
					proxy.Close(fmt.Errorf("write to client: %s", err))
					return
				}
			case <-ticker.C:
				proxy.ClientConn.SetWriteDeadline(time.Now().Add(writeWait))
				if err := proxy.ClientConn.WriteMessage(websocket.PingMessage, nil); err != nil {
					proxy.Close(fmt.Errorf("write to client: %s", err))
					return
				}
			}
		}
	}()

	proxy.ClientConn.SetReadDeadline(time.Now().Add(pongWait))
	proxy.ClientConn.SetPongHandler(func(string) error {
		proxy.ClientConn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	// From client to provider
	for {
		mt, message, err := proxy.ClientConn.ReadMessage()
		if err != nil {
			proxy.Close(fmt.Errorf("read from client: %s", err))
			break
		}

		if mt != websocket.TextMessage {
			log.Printf("Ignoring unknown message type %d\n", mt)
			continue
		}

		var req RPCRequest
		err = json.Unmarshal(message, &req)
		if err != nil {
			proxy.Close(fmt.Errorf("failed to decode request: %s, msg: %s", err, FormatRawBody(string(message))))
			continue
		}

		log.Printf("%s %s [%f] %s\n", proxy.ClientConn.RemoteAddr().String(), proxy.ProviderConn.Endpoint.GetName(), req.ID, req.Method)

		proxy.Requests <- &req
	}
}
