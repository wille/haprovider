package ws

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/wille/haprovider/internal/cache"
	"github.com/wille/haprovider/internal/core"
	"github.com/wille/haprovider/internal/rpc"
)

// TestCache_WS: a cacheable request warms the cache; an identical follow-up on
// the same connection is served from cache without a second upstream message.
func TestCache_WS(t *testing.T) {
	var upstreamMsgs atomic.Int64

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		for {
			_, message, err := conn.ReadMessage()
			if err != nil {
				return
			}
			upstreamMsgs.Add(1)
			batch, err := rpc.DecodeBatchRequest(message)
			if err != nil {
				continue
			}
			resps := make([]*rpc.Response, 0, len(batch.Requests))
			for _, rq := range batch.Requests {
				resps = append(resps, &rpc.Response{Version: "2.0", ID: rq.ID, Result: json.RawMessage(`"0xblock"`)})
			}
			out, _ := rpc.SerializeBatchResponse(&rpc.BatchResponse{Responses: resps, IsBatch: batch.IsBatch})
			_ = conn.WriteMessage(websocket.TextMessage, out)
		}
	}))
	t.Cleanup(up.Close)

	p := &core.Provider{Name: "p", Ws: wsScheme(up.URL)}
	ep := wsEndpoint(t, p)
	if err := ep.InitCache(cache.NewMemory(0, 0)); err != nil {
		t.Fatal(err)
	}

	c, _ := dial(t, proxyURL(t, ep))

	// First request: forwarded upstream, its response cached.
	sendRPC(t, c, "1", "eth_getBlockByHash")
	assert.Equal(t, "0xblock", resultString(t, readRPC(t, c)))

	// Second identical request (different id): served from cache.
	sendRPC(t, c, "2", "eth_getBlockByHash")
	res2 := readRPC(t, c)
	assert.Equal(t, "0xblock", resultString(t, res2))
	assert.Equal(t, "2", res2.GetID(), "cache hit must echo the caller's id")

	assert.Equal(t, int64(1), upstreamMsgs.Load(), "second identical WS request should be served from cache")
}
