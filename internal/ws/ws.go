package ws

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	servertiming "github.com/mitchellh/go-server-timing"
	"github.com/wille/haprovider/internal/cache"
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

var ErrNoProvidersAvailable = errors.New("no providers available")

type WebSocketProxy struct {
	// The request context
	ctx    context.Context
	cancel context.CancelCauseFunc

	log *slog.Logger

	endpoint *core.Endpoint
	provider *core.Provider

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

	// chain is the endpoint's chain implementation, used for cacheability checks.
	chain chain.Chain

	// pendingCache maps the JSON-RPC id of a forwarded cacheable request to the
	// cache key/method to store its response under when it arrives. Written by
	// pumpClient and read/cleared by pumpProvider, so guarded by cacheMu.
	cacheMu      sync.Mutex
	pendingCache map[string]pendingCacheEntry
}

type pendingCacheEntry struct {
	key    string
	method string
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
		"duration", time.Since(p.opened).String(),
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

func (proxy *WebSocketProxy) pumpProvider(providerClient *Client) {
	for {
		select {
		case <-providerClient.ctx.Done():
			return
		case req := <-proxy.Requests:
			metrics.RecordRequest(proxy.endpoint.Name, proxy.provider.Name, "ws", req.Method, 0)
			proxy.writeRequest(providerClient, req)

			// Flush any remaining requests
			for i := 0; i < len(proxy.Requests); i++ {
				req := <-proxy.Requests
				metrics.RecordRequest(proxy.endpoint.Name, proxy.provider.Name, "ws", req.Method, 0)
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

			if rpcResponse.IsError() {
				metrics.RecordFailedRequest(proxy.endpoint.Name, proxy.provider.Name, "ws", "")

				errorCode, errorMessage := rpcResponse.GetError()

				err := chain.New(proxy.endpoint.Kind).HandleError(errorCode, errorMessage.Error())

				if err != nil {
					proxy.provider.MarkUnhealthy(err)
					proxy.Responses <- rpcResponse
					proxy.Close(err)
					proxy.ClientConn.Close(websocket.CloseServiceRestart, err)
					proxy.ProviderConn.Close(websocket.CloseTryAgainLater, nil)
					return
				} else {
					proxy.log.Debug("error response", "error_code", errorCode, "error_message", errorMessage, "raw_error", rpcResponse.Error)
				}
			}

			proxy.maybeCacheResponse(rpcResponse)
			proxy.Responses <- rpcResponse
		}
	}
}

// maybeCacheResponse stores a successful response for a previously-forwarded
// cacheable request (tracked by id in pendingCache), if the chain deems the
// result cacheable.
func (proxy *WebSocketProxy) maybeCacheResponse(res *rpc.Response) {
	if !proxy.endpoint.CacheEnabled() || res.IsError() {
		return
	}

	id := res.GetID()
	proxy.cacheMu.Lock()
	entry, ok := proxy.pendingCache[id]
	if ok {
		delete(proxy.pendingCache, id)
	}
	proxy.cacheMu.Unlock()

	if !ok {
		return
	}
	if proxy.chain.CacheableResult(entry.method, res.Result) {
		_ = proxy.endpoint.Cache().Set(proxy.ctx, entry.key, res.Result, proxy.endpoint.GetCacheTTL())
	}
}

// serveFromCache serves req from cache if possible. It returns true when a cache
// hit was queued to the client (the request must not be forwarded). On a
// cacheable miss it records the pending id→key mapping so the eventual response
// is stored, and returns false so the caller forwards the request.
func (proxy *WebSocketProxy) serveFromCache(req *rpc.Request) bool {
	if !proxy.endpoint.CacheEnabled() || !proxy.chain.Cacheable(req.Method, req.Params) {
		return false
	}

	key := cache.Key(req.Method, req.Params)
	if val, ok, _ := proxy.endpoint.Cache().Get(proxy.ctx, key); ok {
		metrics.RecordCacheHit(proxy.endpoint.Name, req.Method)
		// Write straight to the client rather than via proxy.Responses: this runs
		// on the pumpClient goroutine, which is also the sole consumer of
		// proxy.Responses, so sending there could deadlock on a full buffer.
		proxy.writeResponse(&rpc.Response{Version: "2.0", ID: req.ID, Result: val})
		return true
	}

	metrics.RecordCacheMiss(proxy.endpoint.Name, req.Method)
	proxy.cacheMu.Lock()
	proxy.pendingCache[req.ID.String()] = pendingCacheEntry{key: key, method: req.Method}
	proxy.cacheMu.Unlock()
	return false
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

			requestLog := proxy.log

			if req.IsBatch {
				batchId := rpc.BatchIDCounter.Add(1)
				requestLog = requestLog.With("batch_id", batchId, "batch_size", len(req.Requests))
			}

			// Break up any batched requests into one request per message
			for i, r := range req.Requests {
				// Serve from cache when possible; otherwise forward upstream.
				if proxy.serveFromCache(r) {
					requestLog.With("rpc_id", r.GetID(), "method", r.Method).Debug("cache hit")
					continue
				}

				proxy.Requests <- r

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

			// Flush any remaining responses
			for i := 0; i < len(proxy.Responses); i++ {
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

func IncomingWebsocketHandler(ctx context.Context, endpoint *core.Endpoint, w http.ResponseWriter, r *http.Request, timing *servertiming.Header) {
	start := time.Now()

	if endpoint.AddXForwardedHeaders {
		ctx = httpx.WithForwardedFor(ctx, r)
	}

	ctx, cancel := context.WithCancelCause(ctx)

	proxy := &WebSocketProxy{
		log:          endpoint.Logger().With("ip", r.RemoteAddr, "transport", "ws"),
		ctx:          ctx,
		cancel:       cancel,
		endpoint:     endpoint,
		Responses:    make(chan *rpc.Response, 32),
		Requests:     make(chan *rpc.Request, 32),
		chain:        chain.New(endpoint.Kind),
		pendingCache: make(map[string]pendingCacheEntry),
	}

	// Dial any provider before upgrading the websocket
	provider, err := proxy.DialAnyProvider(endpoint, timing)

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

	proxy.log.Info("ws open", "client_version", provider.ClientVersion(), "request_time", time.Since(start).String())

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt, syscall.SIGTERM)

	t := time.NewTicker(PrimaryProviderCheckInterval)

	for {
		select {
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

		// Gracefully close the connection both ways
		case <-interrupt:
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
