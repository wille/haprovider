package ws

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	servertiming "github.com/mitchellh/go-server-timing"
	"github.com/wille/haprovider/internal/chain"
	"github.com/wille/haprovider/internal/core"
	"github.com/wille/haprovider/internal/httpx"
	"github.com/wille/haprovider/internal/metrics"
	"github.com/wille/haprovider/internal/rpc"

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

	// PrimaryProviderCheckInterval is the interval at which we check if the primary provider is back online.
	PrimaryProviderCheckInterval = time.Minute

	// PrimaryProviderAliveThreshold is the threshold for which we consider the primary provider alive
	// and close connections to secondary providers so they can reconnect to the primary provider.
	PrimaryProviderAliveThreshold = 5 * time.Minute
)

// Vars rather than consts so tests can shorten them.
var (
	// ProviderHealthCheckInterval is how often a connection checks whether its
	// provider has been taken out of rotation.
	ProviderHealthCheckInterval = 5 * time.Second

	// UnhealthyProviderGracePeriod is how long a connection's provider must
	// have been out of rotation before the connection is closed, so a provider
	// marked unhealthy by one failed request (which the healthcheck usually
	// clears within seconds) doesn't disconnect all its clients.
	UnhealthyProviderGracePeriod = 30 * time.Second
)

// leaveUnhealthyProvider reports whether a connection to provider should be
// closed so its client reconnects to another: provider has been out of
// rotation (behind, stalled, on the wrong chain, rate limited, ...) for longer
// than UnhealthyProviderGracePeriod, and another provider with a WebSocket URL
// is healthy. With nowhere better to go, closing would only cut the stream.
func leaveUnhealthyProvider(endpoint *core.Endpoint, provider *core.Provider) bool {
	if provider.IsOnline() || time.Since(provider.GetLastStateChange()) < UnhealthyProviderGracePeriod {
		return false
	}
	for _, p := range endpoint.Providers {
		if p != provider && p.Ws != "" && p.IsOnline() {
			return true
		}
	}
	return false
}

var ErrNoProvidersAvailable = errors.New("no providers available")

// errProxyClosed is the cancel cause once the connection handler has returned.
var errProxyClosed = errors.New("proxy connection closed")

type WebSocketProxy struct {
	// The request context
	ctx    context.Context
	cancel context.CancelCauseFunc

	log *slog.Logger

	endpoint *core.Endpoint
	provider *core.Provider

	// chain is the endpoint's chain implementation, used to enforce its method
	// allowlist on client requests.
	chain chain.Chain

	// opened is the timestamp the client websocket connection was established.
	opened time.Time

	// ClientConn is the incoming client connection
	ClientConn *Client

	// ProviderConn is the upstream provider connection
	ProviderConn *Client

	// Client requests to be sent to the provider
	Requests chan *rpc.Request

	// Provider responses to be sent to the client
	Responses chan *rpc.Response

	badRequests int
}

// Close destroys the proxy connection.
// It's up to the caller to close the client and provider connections.
func (p *WebSocketProxy) Close(reason error) {
	p.cancel(reason)
}

// logClose logs a connection-close message with the fields common to every
// close path: how long the connection was open and how many data messages the
// client exchanged with us. err is omitted when nil.
//
// The counts are logged from the client's perspective: "sent" is what the
// client sent us (requests, i.e. frames we read off the client socket) and
// "received" is what the client received from us (responses we wrote to it).
// This is why the getters look inverted relative to the labels.
func (p *WebSocketProxy) logClose(level slog.Level, msg string, err error) {
	attrs := []any{
		"opened", p.opened,
		"duration", time.Since(p.opened).Round(time.Millisecond).String(),
		"sent", p.ClientConn.MessagesReceived(),
		"received", p.ClientConn.MessagesSent(),
	}
	if err != nil {
		attrs = append(attrs, "error", err)
	}
	p.log.Log(context.Background(), level, msg, attrs...)
}

func (proxy *WebSocketProxy) DialProvider(endpoint *core.Endpoint, provider *core.Provider) error {
	u, _ := url.Parse(provider.Ws)

	headers := http.Header{}

	if provider.Headers != nil {
		for k, v := range provider.Headers {
			headers.Set(k, v)
		}
	}

	headers.Set("User-Agent", httpx.UserAgent)
	httpx.SetForwardedFor(proxy.ctx, headers)

	ctx, cancel := context.WithTimeout(proxy.ctx, provider.GetTimeout())
	defer cancel()

	var dialer = &websocket.Dialer{
		Proxy:             http.ProxyFromEnvironment,
		HandshakeTimeout:  provider.GetTimeout(),
		EnableCompression: true,
	}
	ws, resp, err := dialer.DialContext(ctx, u.String(), headers)
	if err != nil {
		// A failed handshake returns websocket.ErrBadHandshake with a non-nil
		// resp (and a nil conn). Some providers send a 429 on the connection
		// attempt; honor it like an HTTP 429 instead of a generic error.
		if resp != nil && resp.StatusCode == http.StatusTooManyRequests {
			return provider.HandleTooManyRequests(resp)
		}
		return err
	}

	// A nil error from DialContext means the handshake succeeded (101).
	// Bound upstream message size before the read pump starts. gorilla enforces
	// this against the decompressed size, so it also caps decompression bombs.
	ws.SetReadLimit(endpoint.GetMaxResponseSize())

	providerClient := NewClient(ws)

	proxy.ProviderConn = providerClient
	proxy.provider = provider

	// Tag the logger with the chosen provider before starting the pump goroutine
	// that reads proxy.log; mutating it afterwards would race with the pump.
	proxy.log = proxy.log.With("provider", provider.Name)

	go proxy.pumpProvider(proxy.ProviderConn)

	return nil
}

// writeRequest serializes a client request and forwards it to the provider. A
// serialization failure (should be impossible for decoded JSON-RPC) is logged
// and the message dropped rather than panicking the pump goroutine.
func (proxy *WebSocketProxy) writeRequest(providerClient *Client, req *rpc.Request) {
	data, err := rpc.SerializeRequest(req)
	if err != nil {
		proxy.log.Error("dropping request: failed to serialize", "error", err, "method", req.Method)
		return
	}
	providerClient.Write(data)
}

// writeResponse serializes a provider response and forwards it to the client,
// logging and dropping on a serialization failure instead of panicking.
func (proxy *WebSocketProxy) writeResponse(res *rpc.Response) {
	data, err := rpc.SerializeResponse(res)
	if err != nil {
		proxy.log.Error("dropping response: failed to serialize", "error", err)
		return
	}
	proxy.ClientConn.Write(data)
}

// sendResponse hands a provider response to the client pump. It gives up, and
// returns false, once the proxy or the provider connection is closing, so the
// provider pump can never block forever on a client pump that has gone.
func (proxy *WebSocketProxy) sendResponse(providerClient *Client, res *rpc.Response) bool {
	select {
	case proxy.Responses <- res:
		return true
	case <-proxy.ctx.Done():
		return false
	case <-providerClient.ctx.Done():
		return false
	}
}

// sendRequest hands a client request to the provider pump. It gives up, and
// returns false, once the proxy or either connection is closing, so the client
// pump can never block forever on a provider pump that has gone.
func (proxy *WebSocketProxy) sendRequest(client *Client, req *rpc.Request) bool {
	select {
	case proxy.Requests <- req:
		return true
	case <-proxy.ctx.Done():
		return false
	case <-client.ctx.Done():
		return false
	case <-proxy.ProviderConn.ctx.Done():
		return false
	}
}

// trackPending records an in-flight request's method keyed by its id so a later
// error response (which carries no method) can be attributed to it. Requests
// without an id are JSON-RPC notifications that get no response, so they are not
// tracked — storing pending[""] would only produce a stale, misattributed entry.
func trackPending(pending map[string]string, req *rpc.Request) {
	if id := req.GetID(); id != "" {
		pending[id] = req.Method
	}
}

func (proxy *WebSocketProxy) pumpProvider(providerClient *Client) {
	// pending maps an in-flight request id to its method, so a "method not
	// supported" error response (which carries no method) can be attributed to the
	// method that caused it. Owned solely by this goroutine — both request-send and
	// response-receive happen here — so it needs no lock. It's connection-scoped:
	// entries for responses that never arrive are freed when the goroutine exits.
	pending := make(map[string]string)

	for {
		select {
		case <-providerClient.ctx.Done():
			return
		case req := <-proxy.Requests:
			metrics.RecordRequest(proxy.endpoint.Name, proxy.provider.Name, "ws", req.Method, 0)
			trackPending(pending, req)
			proxy.writeRequest(providerClient, req)

			// Flush any remaining requests. Count first: len shrinks as we drain.
			for n := len(proxy.Requests); n > 0; n-- {
				req := <-proxy.Requests
				metrics.RecordRequest(proxy.endpoint.Name, proxy.provider.Name, "ws", req.Method, 0)
				trackPending(pending, req)
				proxy.writeRequest(providerClient, req)
			}

		case message := <-providerClient.Read():
			rpcResponse, err := rpc.DecodeResponse(message)
			if err != nil {
				err := fmt.Errorf("received bad data from provider: %s, msg: %s", err, rpc.FormatRawBody(string(message)))
				proxy.Close(err)
				proxy.ProviderConn.Close(websocket.CloseGoingAway, nil)
				proxy.ClientConn.Close(websocket.CloseUnsupportedData, err)
				return
			}

			// Attribute the response to its request's method. Only match on a
			// non-empty id: notifications and id:null error responses have no id, and
			// looking up pending[""] would attach a stale method and evict unrelated
			// bookkeeping.
			var method string
			if id := rpcResponse.GetID(); id != "" {
				method = pending[id]
				delete(pending, id)
			}

			// A provider answered it, so it's a real method: label it by name. The
			// request itself was counted when sent, so a method's very first call is
			// recorded as "other".
			if !rpcResponse.IsError() {
				metrics.AdmitMethod(method)
			}

			if rpcResponse.IsError() {
				metrics.RecordFailedRequest(proxy.endpoint.Name, proxy.provider.Name, "ws", method)

				errorCode, errorMessage := rpcResponse.GetError()

				if err := chain.New(proxy.endpoint.Kind).HandleError(errorCode, errorMessage.Error()); err != nil {
					var mns *rpc.MethodNotSupportedError
					var tryNext *rpc.ErrorTryNextProvider
					if errors.As(err, &mns) {
						// The chain reports the method isn't supported: record it (shared
						// with the HTTP path and future dials) and forward the error to the
						// client. Do NOT mark the provider unhealthy — it works, it just
						// lacks this method.
						proxy.provider.MarkMethodUnsupported(method)
						proxy.log.Warn("method not supported by provider", "method", method, "error_code", errorCode)
					} else if errors.As(err, &tryNext) {
						// The provider can't serve this specific request. A pinned WS
						// connection has no per-request failover, so forward the error to
						// the client and keep the connection — don't mark it unhealthy.
						proxy.log.Warn("provider cannot serve request", "method", method, "error_code", errorCode)
					} else {
						proxy.provider.MarkUnhealthy(err)
						proxy.sendResponse(providerClient, rpcResponse)
						proxy.Close(err)
						proxy.ClientConn.Close(websocket.CloseServiceRestart, err)
						proxy.ProviderConn.Close(websocket.CloseTryAgainLater, nil)
						return
					}
				} else {
					proxy.log.Debug("error response", "error_code", errorCode, "error_message", errorMessage, "raw_error", rpcResponse.Error)
				}
			}

			if !proxy.sendResponse(providerClient, rpcResponse) {
				return
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
				proxy.log.Debug("ClientConn read closed")
				continue
			}

			req, err := rpc.DecodeBatchRequest(message)
			if err == nil && len(req.Requests) > rpc.MaxBatchSize {
				err = fmt.Errorf("batch too large: %d > %d", len(req.Requests), rpc.MaxBatchSize)
			}
			if err != nil {
				proxy.log.Debug("bad client request", "error", err, "msg", rpc.FormatRawBody(string(message)))

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

			// Enforce the chain's method allowlist (e.g. Bitcoin wallet/control
			// commands), exactly as the HTTP path does. A batch containing any
			// disallowed method is rejected in full, without reaching the provider.
			if rejected := chain.RejectDisallowed(proxy.chain, req); rejected != nil {
				for i, res := range rejected {
					proxy.log.Warn("method not allowed", "rpc_id", req.Requests[i].GetID(), "method", req.Requests[i].Method)
					proxy.writeResponse(res)
				}
				continue
			}

			requestLog := proxy.log

			if req.IsBatch {
				batchId := rpc.BatchIDCounter.Add(1)
				requestLog = requestLog.With("batch_id", batchId, "batch_size", len(req.Requests))
			}

			// Break up any batched requests into one request per message
			for i, r := range req.Requests {
				if !proxy.sendRequest(client, r) {
					return
				}

				if req.IsBatch {
					requestLog.With("batch_index", i, "rpc_id", r.GetID(), "method", r.Method).Debug("request")
				} else {
					requestLog.With("rpc_id", r.GetID(), "method", r.Method).Debug("request")
				}
			}

		case rpcResponse, ok := <-proxy.Responses:
			if !ok {
				proxy.log.Debug("proxy.Responses closed")
				continue
			}

			proxy.writeResponse(rpcResponse)

			// Flush any remaining responses. Count first: len shrinks as we drain.
			for n := len(proxy.Responses); n > 0; n-- {
				proxy.writeResponse(<-proxy.Responses)
			}
		}
	}
}

// DialAnyProvider dials any provider and returns a WebSocketProxy
func (proxy *WebSocketProxy) DialAnyProvider(e *core.Endpoint, timing *servertiming.Header) (*core.Provider, error) {
	for _, p := range e.Providers {
		if p.Ws == "" {
			continue
		}

		if !p.IsOnline() {
			continue
		}

		m := timing.NewMetric(p.Name).Start()
		err := proxy.DialProvider(e, p)
		m.Stop()

		// The client went away while we were dialing: give up without treating
		// it as a provider error. If the dial did succeed, nobody will use that
		// connection, so close it rather than leak it. (On a failed dial there's
		// no connection, and ProviderConn is nil.)
		select {
		case <-proxy.ctx.Done():
			if err == nil {
				proxy.ProviderConn.Close(websocket.CloseGoingAway, nil)
			}
			return nil, proxy.ctx.Err()
		default:
		}

		if err != nil {
			// In a high traffic environment, the provider might be taken offline by another concurrent request
			if p.IsOnline() {
				p.MarkUnhealthy(err)
			}
			continue
		}

		return p, nil
	}

	return nil, ErrNoProvidersAvailable
}

// writeHandshakeError writes a JSON-RPC error response for a failure that occurs
// before the WebSocket is upgraded (e.g. no provider to dial). The client's
// upgrade fails with this HTTP status, and a client that inspects the body gets a
// structured JSON-RPC error rather than plain text.
func writeHandshakeError(w http.ResponseWriter, status, code int, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	resp := &rpc.Response{Version: "2.0", ID: nil, Error: rpc.NewError(code, message)}
	_ = rpc.WriteResponse(w, resp)
}

func IncomingWebsocketHandler(ctx context.Context, endpoint *core.Endpoint, w http.ResponseWriter, r *http.Request, timing *servertiming.Header) {
	start := time.Now()

	if endpoint.AddXForwardedHeaders {
		ctx = httpx.WithForwardedFor(ctx, r)
	}

	ctx, cancel := context.WithCancelCause(ctx)
	// However the handler exits, cancel the proxy context so both pumps, which
	// select on it, stop.
	defer cancel(errProxyClosed)

	proxy := &WebSocketProxy{
		log:       endpoint.Logger().With("ip", httpx.ClientIP(r), "transport", "ws"),
		ctx:       ctx,
		cancel:    cancel,
		endpoint:  endpoint,
		chain:     chain.New(endpoint.Kind),
		Responses: make(chan *rpc.Response, 32),
		Requests:  make(chan *rpc.Request, 32),
	}

	// Dial any provider before upgrading the websocket
	provider, err := proxy.DialAnyProvider(endpoint, timing)

	if err == ErrNoProvidersAvailable {
		proxy.log.Error("no providers available")
		writeHandshakeError(w, http.StatusServiceUnavailable, rpc.CodeNoProvidersAvailable, "no providers available")
		return
	}

	if err != nil {
		select {
		case <-proxy.ctx.Done():
			proxy.log.Error("client closed the connection", "error", err)
		default:
			proxy.log.Error("error dialing provider", "error", err)
			// redact.String strips any provider URL (with its API key) from the
			// dial error before it goes back to the external client.
			writeHandshakeError(w, http.StatusInternalServerError, rpc.CodeInternalError, "error dialing provider")
		}

		return
	}

	// Register the connection for graceful shutdown. Once shutdown has begun,
	// refuse it rather than open a connection that is closed straight away.
	tracker := connections
	if !tracker.add() {
		proxy.log.Info("refusing connection, shutting down")
		proxy.ProviderConn.Close(websocket.CloseGoingAway, nil)
		writeHandshakeError(w, http.StatusServiceUnavailable, rpc.CodeNoProvidersAvailable, "shutting down")
		return
	}
	defer tracker.done()

	// gorilla writes its own 101 handshake response and ignores w.Header(), so
	// any headers we want the client to see must be passed to Upgrade.
	respHeader := http.Header{}

	if !endpoint.Public {
		respHeader.Set("X-Provider", provider.Name)
	}

	ws, err := upgrader.Upgrade(w, r, respHeader)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		proxy.log.Error("error upgrading connection", "error", err)
		proxy.ProviderConn.Close(websocket.CloseNormalClosure, nil)
		return
	}

	proxy.opened = time.Now()

	defer func() {
		metrics.RecordCloseConnection(endpoint.Name, provider.Name)
	}()
	metrics.RecordOpenConnection(endpoint.Name, provider.Name)

	// Bound the size of incoming client messages. Set before NewClient starts
	// the read pump. Upstream provider responses are intentionally not limited.
	ws.SetReadLimit(rpc.MaxRequestBodySize)

	proxy.ClientConn = NewClient(ws)
	go proxy.pumpClient(proxy.ClientConn)

	proxy.log.Info("ws open", "client_version", provider.ClientVersion(), "request_time", time.Since(start).Round(time.Millisecond).String())

	t := time.NewTicker(PrimaryProviderCheckInterval)
	defer t.Stop()

	health := time.NewTicker(ProviderHealthCheckInterval)
	defer health.Stop()

	for {
		select {
		// The provider was taken out of rotation and stayed out: without this the
		// client would keep receiving whatever it sends (stale heads, another
		// chain's data) for as long as the connection lasts. Close it so the
		// client reconnects to a healthy provider.
		case <-health.C:
			if leaveUnhealthyProvider(endpoint, provider) {
				proxy.logClose(slog.LevelWarn, "provider unhealthy, closing connection", nil)
				proxy.ClientConn.Close(websocket.CloseServiceRestart, fmt.Errorf("provider unhealthy"))
				proxy.ProviderConn.Close(websocket.CloseGoingAway, nil)
				return
			}

		// Check if the primary provider is back online and close the connection so the client can reconnect
		case <-t.C:
			primaryProvider := endpoint.Providers[0]
			// The primary provider has been online for more than 5 minutes
			if provider != primaryProvider && primaryProvider.IsOnline() && time.Since(primaryProvider.GetLastStateChange()) > PrimaryProviderAliveThreshold {
				proxy.logClose(slog.LevelInfo, "primary provider is back online, closing connection", nil)
				proxy.ClientConn.Close(websocket.CloseServiceRestart, fmt.Errorf("primary provider is back online"))
				proxy.ProviderConn.Close(websocket.CloseGoingAway, nil)
				return
			}

		// Shutting down (see CloseAll): gracefully close the connection both ways
		case <-tracker.shutdown:
			proxy.logClose(slog.LevelInfo, "ws closed, shutting down", nil)
			proxy.ClientConn.Close(websocket.CloseGoingAway, fmt.Errorf("haprovider shutting down"))
			proxy.ProviderConn.Close(websocket.CloseGoingAway, nil)
			return

		// We closed the connection
		case <-proxy.ctx.Done():
			proxy.logClose(slog.LevelError, "ws closed", context.Cause(proxy.ctx))
			return

		case <-proxy.ClientConn.ctx.Done():
			proxy.logClose(slog.LevelInfo, "client connection closed", context.Cause(proxy.ClientConn.ctx))
			proxy.ProviderConn.Close(websocket.CloseGoingAway, fmt.Errorf("client connection closed: %w", context.Cause(proxy.ClientConn.ctx)))
			return

		case <-proxy.ProviderConn.ctx.Done():
			proxy.logClose(slog.LevelError, "provider connection closed", context.Cause(proxy.ProviderConn.ctx))
			proxy.ClientConn.Close(websocket.CloseTryAgainLater, fmt.Errorf("provider connection closed: %w", context.Cause(proxy.ProviderConn.ctx)))
			return
		}
	}
}
