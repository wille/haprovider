package internal

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/wille/haprovider/internal/cache"
	"github.com/wille/haprovider/internal/core"
	"github.com/wille/haprovider/internal/rpc"
)

// cacheEndpoint builds an eth endpoint with caching enabled, backed by a mock
// upstream that counts requests and answers each with respond(req).
func cacheEndpoint(t *testing.T, respond func(*rpc.Request) *rpc.Response) (*core.Endpoint, *atomic.Int64) {
	var hits atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		batch, err := rpc.DecodeBatchRequest(b)
		if err != nil || len(batch.Requests) == 0 {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		resps := make([]*rpc.Response, 0, len(batch.Requests))
		for _, req := range batch.Requests {
			hits.Add(1) // count sub-requests that actually reach the upstream
			resps = append(resps, respond(req))
		}
		out, _ := rpc.SerializeBatchResponse(&rpc.BatchResponse{Responses: resps, IsBatch: batch.IsBatch})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
	}))
	t.Cleanup(srv.Close)

	ep := &core.Endpoint{Name: "test", ChainID: "1", Kind: core.KindEth}
	if err := ep.InitCache(cache.NewMemory(0, 0)); err != nil {
		t.Fatal(err)
	}
	p := &core.Provider{Name: "p", Http: srv.URL, Endpoint: ep}
	ep.Providers = []*core.Provider{p}
	p.MarkHealthy(0)
	return ep, &hits
}

func result(req *rpc.Request, raw string) *rpc.Response {
	return &rpc.Response{Version: "2.0", ID: req.ID, Result: json.RawMessage(raw)}
}

// TestCache_HitAfterMiss: a cacheable read misses (one upstream hit) then is
// served from cache on repeat, with the second caller's own id echoed.
func TestCache_HitAfterMiss(t *testing.T) {
	ep, hits := cacheEndpoint(t, func(req *rpc.Request) *rpc.Response {
		return result(req, `"0xblockdata"`)
	})

	body := `{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByHash","params":["0xhash",false]}`
	w1 := doRPC(ep, body)
	assert.Equal(t, http.StatusOK, w1.Code)
	assert.Contains(t, w1.Body.String(), "0xblockdata")
	assert.Equal(t, int64(1), hits.Load())
	assert.Empty(t, w1.Header().Get("X-Cache"))

	// Identical request but different id: served from cache, no new upstream hit.
	w2 := doRPC(ep, `{"jsonrpc":"2.0","id":99,"method":"eth_getBlockByHash","params":["0xhash",false]}`)
	assert.Equal(t, http.StatusOK, w2.Code)
	assert.Contains(t, w2.Body.String(), "0xblockdata")
	assert.Contains(t, w2.Body.String(), `"id":99`)
	assert.Equal(t, "HIT", w2.Header().Get("X-Cache"))
	assert.Equal(t, int64(1), hits.Load(), "second identical read must be served from cache")
}

// TestCache_LatestNotCached: a mutable block tag is never cached.
func TestCache_LatestNotCached(t *testing.T) {
	ep, hits := cacheEndpoint(t, func(req *rpc.Request) *rpc.Response {
		return result(req, `"0xblock"`)
	})
	body := `{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["latest",false]}`
	doRPC(ep, body)
	doRPC(ep, body)
	assert.Equal(t, int64(2), hits.Load(), `"latest" params must not be cached`)
}

// TestCache_NullResultNotCached: a null result (e.g. missing receipt) is not cached.
func TestCache_NullResultNotCached(t *testing.T) {
	ep, hits := cacheEndpoint(t, func(req *rpc.Request) *rpc.Response {
		return result(req, `null`)
	})
	body := `{"jsonrpc":"2.0","id":1,"method":"eth_getTransactionReceipt","params":["0xtx"]}`
	doRPC(ep, body)
	doRPC(ep, body)
	assert.Equal(t, int64(2), hits.Load(), "null result must not be cached")
}

// TestCache_ErrorNotCached: an RPC error response is not cached.
func TestCache_ErrorNotCached(t *testing.T) {
	ep, hits := cacheEndpoint(t, func(req *rpc.Request) *rpc.Response {
		// -32601 is not classified as unhealthy, so it is passed through.
		return &rpc.Response{Version: "2.0", ID: req.ID, Error: rpc.NewError(-32601, "nope")}
	})
	body := `{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByHash","params":["0xhash",false]}`
	doRPC(ep, body)
	doRPC(ep, body)
	assert.Equal(t, int64(2), hits.Load(), "error responses must not be cached")
}

// TestCache_UnconfirmedTxNotCached: eth_getTransactionByHash with a null
// blockNumber (pending) is not cached; once confirmed it is.
func TestCache_UnconfirmedTxNotCached(t *testing.T) {
	var confirmed atomic.Bool
	ep, hits := cacheEndpoint(t, func(req *rpc.Request) *rpc.Response {
		if confirmed.Load() {
			return result(req, `{"hash":"0xtx","blockNumber":"0x10"}`)
		}
		return result(req, `{"hash":"0xtx","blockNumber":null}`)
	})
	body := `{"jsonrpc":"2.0","id":1,"method":"eth_getTransactionByHash","params":["0xtx"]}`

	doRPC(ep, body) // pending -> not cached
	doRPC(ep, body) // still pending -> upstream again
	assert.Equal(t, int64(2), hits.Load(), "pending tx must not be cached")

	confirmed.Store(true)
	doRPC(ep, body) // confirmed -> cached now
	assert.Equal(t, int64(3), hits.Load())
	doRPC(ep, body) // served from cache
	assert.Equal(t, int64(3), hits.Load(), "confirmed tx should be cached")
}

// methodResult answers each sub-request with a JSON string of its own method, so
// merged batch responses can be checked element by element.
func methodResult(req *rpc.Request) *rpc.Response {
	return result(req, `"`+req.Method+`"`)
}

// decodeBatch decodes a recorder's body as a batch response.
func decodeBatch(t *testing.T, w *httptest.ResponseRecorder) *rpc.BatchResponse {
	res, err := rpc.DecodeBatchResponse(w.Body.Bytes())
	if err != nil {
		t.Fatalf("decode batch response: %v (body=%s)", err, w.Body.String())
	}
	return res
}

// TestCache_BatchFullyCached: every element of a batch is cached on the first
// call, so an identical repeat is served entirely from cache.
func TestCache_BatchFullyCached(t *testing.T) {
	ep, hits := cacheEndpoint(t, methodResult)

	batch := `[{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByHash","params":["0xh",false]},` +
		`{"jsonrpc":"2.0","id":2,"method":"eth_getTransactionReceipt","params":["0xt"]}]`

	w1 := doRPC(ep, batch)
	assert.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, int64(2), hits.Load(), "first batch forwards both sub-requests")

	w2 := doRPC(ep, batch)
	assert.Equal(t, int64(2), hits.Load(), "identical batch must be fully served from cache")
	assert.Equal(t, "HIT", w2.Header().Get("X-Cache"))

	// Responses come back in order with their own ids and results.
	res := decodeBatch(t, w2)
	if assert.Len(t, res.Responses, 2) {
		assert.Equal(t, "1", res.Responses[0].GetID())
		assert.Contains(t, string(res.Responses[0].Result), "eth_getBlockByHash")
		assert.Equal(t, "2", res.Responses[1].GetID())
		assert.Contains(t, string(res.Responses[1].Result), "eth_getTransactionReceipt")
	}
}

// TestCache_BatchPartial: only the uncached / non-cacheable elements of a batch
// are forwarded; cached elements are merged back in their original positions.
func TestCache_BatchPartial(t *testing.T) {
	ep, hits := cacheEndpoint(t, methodResult)

	// Warm the cache for one read.
	doRPC(ep, `{"jsonrpc":"2.0","id":9,"method":"eth_getBlockByHash","params":["0xh",false]}`)
	assert.Equal(t, int64(1), hits.Load())

	// Batch: [cached getBlockByHash, non-cacheable eth_blockNumber].
	batch := `[{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByHash","params":["0xh",false]},` +
		`{"jsonrpc":"2.0","id":2,"method":"eth_blockNumber","params":[]}]`
	w := doRPC(ep, batch)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, int64(2), hits.Load(), "only the non-cacheable element should reach upstream")

	res := decodeBatch(t, w)
	if assert.Len(t, res.Responses, 2) {
		assert.Equal(t, "1", res.Responses[0].GetID(), "order preserved: cached element first")
		assert.Contains(t, string(res.Responses[0].Result), "eth_getBlockByHash")
		assert.Equal(t, "2", res.Responses[1].GetID())
		assert.Contains(t, string(res.Responses[1].Result), "eth_blockNumber")
	}
}

// TestCache_IncludeTransactionsInKey: eth_getBlockByNumber's second param
// (includeTransactions) is part of the cache key, so true and false responses
// are cached separately and never conflated.
func TestCache_IncludeTransactionsInKey(t *testing.T) {
	ep, hits := cacheEndpoint(t, func(req *rpc.Request) *rpc.Response {
		return result(req, `"0xblock"`)
	})
	full := `{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x1234",true]}`
	light := `{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x1234",false]}`

	doRPC(ep, full)
	doRPC(ep, full)
	assert.Equal(t, int64(1), hits.Load(), "repeat of includeTransactions=true should hit cache")

	doRPC(ep, light) // different includeTransactions -> distinct key -> upstream
	assert.Equal(t, int64(2), hits.Load(), "includeTransactions=false must not reuse the true entry")
	doRPC(ep, light)
	assert.Equal(t, int64(2), hits.Load(), "repeat of includeTransactions=false should hit cache")
}

// TestCache_NonCacheableMethod: a method not on the allowlist is never cached.
func TestCache_NonCacheableMethod(t *testing.T) {
	ep, hits := cacheEndpoint(t, func(req *rpc.Request) *rpc.Response {
		return result(req, `"0x10"`)
	})
	body := `{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`
	doRPC(ep, body)
	doRPC(ep, body)
	assert.Equal(t, int64(2), hits.Load(), "non-allowlisted method must not be cached")
}
