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

	"github.com/gorilla/websocket"
)

type ProviderConnection struct {
	Conn      *websocket.Conn
	Endpoint  *Endpoint
	Provider  *Provider
	listeners map[string]chan *RPCResponse
}

func (r *ProviderConnection) Write(req *RPCRequest) error {
	b := SerializeRPCRequest(req)

	err := r.Conn.WriteMessage(websocket.TextMessage, b)
	return err
}

func (r *ProviderConnection) AwaitReply(requestID string, req *RPCRequest) (*RPCResponse, error) {
	req.ID = requestID
	ch := make(chan *RPCResponse, 1)
	r.listeners[requestID] = ch
	defer delete(r.listeners, requestID)
	defer close(ch)

	timeout, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := r.Write(req); err != nil {
		return nil, err
	}

	select {
	case reply := <-ch:
		return reply, nil
	case <-timeout.Done():
		return nil, timeout.Err()
	}
}

func (r *ProviderConnection) healthcheck() error {
	// Skip chainID validation if not set in config
	if r.Provider.ChainID != 0 {
		res, err := r.AwaitReply("ha_chainId", NewRPCRequest("eth_chainId", nil))
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

type Proxy struct {
	ID string

	ClientConn *websocket.Conn
	ClientChan chan *RPCResponse

	Provider     *ProviderConnection
	ProviderChan chan *RPCRequest

	Listeners map[string]chan *RPCResponse

	ErrorChan chan error
	closed    bool
}

// Close gracefully closes the proxy connection
func (p *Proxy) Close() {
	if p.closed {
		return
	}
	p.closed = true

	log.Printf("Closing proxy %s\n", p.ID)

	if p.Provider != nil {
		p.ClientConn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		p.ClientConn.Close()
	}

	if p.Provider != nil {
		p.Provider.Conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		p.Provider.Conn.Close()
	}

	close(p.ClientChan)
	close(p.ProviderChan)
	close(p.ErrorChan)
}

func ProxyWebSocket(provider *Provider, proxy *Proxy) (*ProviderConnection, error) {
	for _, endpoint := range provider.GetActiveEndpoints() {
		if endpoint.Ws == "" {
			continue
		}

		conn, err := DialProvider2(provider, endpoint, proxy)
		if err != nil {
			endpoint.SetStatus(false, err)
			continue
		}

		endpoint.SetStatus(true, nil)

		return conn, nil
	}

	return nil, ErrNoProvidersAvailable
}

func DialProvider2(provider *Provider, endpoint *Endpoint, proxy *Proxy) (*ProviderConnection, error) {
	u, _ := url.Parse(endpoint.Ws)

	headers := http.Header{}
	headers.Set("User-Agent", "haprovider/"+Version)

	ws, resp, err := websocket.DefaultDialer.Dial(u.String(), headers)
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

	conn := &ProviderConnection{
		Conn:      ws,
		Endpoint:  endpoint,
		Provider:  provider,
		listeners: make(map[string]chan *RPCResponse),
	}

	go conn.OpenProviderConnection(proxy)

	clientVersion, err := conn.AwaitReply("ha_clientVersion", NewRPCRequest("web3_clientVersion", nil))
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

func (conn *ProviderConnection) OpenProviderConnection(proxy *Proxy) {
	ws := conn.Conn

	// interrupt := make(chan os.Signal, 1)
	// signal.Notify(interrupt, os.Interrupt)

	errorChan := make(chan error, 1)

	go func() {
		for {
			select {
			case err := <-errorChan:
				// Provider websocket connection died.
				// TODO Close client connection
				log.Println("provider err", err)

				_ = ws.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
				proxy.Close()

				return
			case rpcResponse := <-proxy.ProviderChan:
				err := conn.Write(rpcResponse)
				if err != nil {
					errorChan <- fmt.Errorf("write: %s", err)
				}
			}

		}
	}()

	for {
		mt, message, err := ws.ReadMessage()
		if err != nil {
			errorChan <- fmt.Errorf("read: %s", err)
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
			errorChan <- fmt.Errorf("decode: %s, msg: %s", err, message)
			return
		}

		if rpcCall.Error != nil {
			errorChan <- fmt.Errorf("RPC error %s", rpcCall.Error)
			return
		}

		reqID, _ := GetClientID(rpcCall.ID)

		// Intercept
		if ch := conn.listeners[reqID]; ch != nil {
			ch <- &rpcCall
			continue
		}

		// TODO log subscription messages

		proxy.ClientChan <- &rpcCall
	}
}
