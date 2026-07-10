package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	servertiming "github.com/mitchellh/go-server-timing"
	"github.com/stretchr/testify/assert"
	"github.com/wille/haprovider/internal/core"
	"github.com/wille/haprovider/internal/rpc"
)

// countingUpstream returns an eth endpoint backed by a mock upstream that counts
// how many HTTP requests it receives, sleeps for delay (to force concurrent
// requests to overlap), and echoes each JSON-RPC request's id with the mapped
// result.
func countingUpstream(t *testing.T, delay time.Duration, methods map[string]any) (*core.Endpoint, *atomic.Int64) {
	var hits atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if delay > 0 {
			time.Sleep(delay)
		}

		b, _ := io.ReadAll(r.Body)
		batch, err := rpc.DecodeBatchRequest(b)
		if err != nil || len(batch.Requests) == 0 {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		responses := make([]*rpc.Response, 0, len(batch.Requests))
		for _, req := range batch.Requests {
			res := &rpc.Response{Version: "2.0", ID: req.ID}
			if result, ok := methods[req.Method]; ok {
				res.Result, _ = json.Marshal(result)
			} else {
				res.Error = rpc.NewError(-32601, "method not found: "+req.Method)
			}
			responses = append(responses, res)
		}

		out, _ := rpc.SerializeBatchResponse(&rpc.BatchResponse{Responses: responses, IsBatch: batch.IsBatch})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
	}))
	t.Cleanup(srv.Close)

	ep := &core.Endpoint{Name: "test", ChainID: "1", Kind: core.KindEth}
	p := &core.Provider{Name: "p", Http: srv.URL, Endpoint: ep}
	ep.Providers = []*core.Provider{p}
	p.MarkHealthy(0)
	return ep, &hits
}

// fireConcurrent sends n requests (built by body(i)) at the endpoint concurrently,
// releasing them all at once, and returns the recorders in order.
func fireConcurrent(ep *core.Endpoint, n int, body func(i int) string) []*httptest.ResponseRecorder {
	recorders := make([]*httptest.ResponseRecorder, n)
	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			r := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(body(i)))
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			<-release
			IncomingHttpRpcHandler(context.Background(), ep, w, r, &servertiming.Header{})
			recorders[i] = w
		}(i)
	}
	close(release)
	wg.Wait()
	return recorders
}

// TestCoalesce_ConcurrentIdenticalReads: N identical concurrent read requests
// collapse into a single upstream call, and every client gets a correct response
// with its own JSON-RPC id echoed back.
func TestCoalesce_ConcurrentIdenticalReads(t *testing.T) {
	ep, hits := countingUpstream(t, 150*time.Millisecond, map[string]any{"eth_call": "0xdeadbeef"})

	const n = 20
	recorders := fireConcurrent(ep, n, func(i int) string {
		return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_call","params":["x"]}`, i)
	})

	assert.Equal(t, int64(1), hits.Load(), "identical concurrent reads should collapse to one upstream call")

	for i, w := range recorders {
		assert.Equal(t, http.StatusOK, w.Code)
		assert.Contains(t, w.Body.String(), "0xdeadbeef")
		assert.Contains(t, w.Body.String(), fmt.Sprintf(`"id":%d`, i), "each client must get its own id echoed")
	}
}

// TestCoalesce_UnsafeMethodNotCoalesced: identical concurrent writes are NOT
// coalesced — each must reach the upstream independently.
func TestCoalesce_UnsafeMethodNotCoalesced(t *testing.T) {
	ep, hits := countingUpstream(t, 50*time.Millisecond, map[string]any{"eth_sendRawTransaction": "0xhash"})

	const n = 10
	recorders := fireConcurrent(ep, n, func(i int) string {
		return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_sendRawTransaction","params":["0xsigned"]}`, i)
	})

	assert.Equal(t, int64(n), hits.Load(), "sendRawTransaction must never be coalesced")
	for _, w := range recorders {
		assert.Equal(t, http.StatusOK, w.Code)
		assert.Contains(t, w.Body.String(), "0xhash")
	}
}

// TestCoalesce_DifferentParamsNotCoalesced: same method, different params must
// not share an upstream call.
func TestCoalesce_DifferentParamsNotCoalesced(t *testing.T) {
	ep, hits := countingUpstream(t, 100*time.Millisecond, map[string]any{"eth_getBalance": "0x1"})

	const n = 5
	recorders := fireConcurrent(ep, n, func(i int) string {
		return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_getBalance","params":["0xaddr%d"]}`, i, i)
	})

	assert.Equal(t, int64(n), hits.Load(), "distinct params must not coalesce")
	for _, w := range recorders {
		assert.Equal(t, http.StatusOK, w.Code)
	}
}

// TestCoalesce_BatchNotCoalesced: batch requests bypass coalescing and behave as
// before (each reaches the upstream, both sub-results returned).
func TestCoalesce_BatchNotCoalesced(t *testing.T) {
	ep, hits := countingUpstream(t, 100*time.Millisecond, map[string]any{
		"eth_call":       "0xaa",
		"eth_getBalance": "0xbb",
	})

	const n = 4
	recorders := fireConcurrent(ep, n, func(i int) string {
		return `[{"jsonrpc":"2.0","id":1,"method":"eth_call","params":["x"]},` +
			`{"jsonrpc":"2.0","id":2,"method":"eth_getBalance","params":["y"]}]`
	})

	assert.Equal(t, int64(n), hits.Load(), "batches must not be coalesced")
	for _, w := range recorders {
		assert.Equal(t, http.StatusOK, w.Code)
		assert.Contains(t, w.Body.String(), "0xaa")
		assert.Contains(t, w.Body.String(), "0xbb")
	}
}
