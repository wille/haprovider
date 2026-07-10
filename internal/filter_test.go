package internal

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/wille/haprovider/internal/core"
	"github.com/wille/haprovider/internal/rpc"
)

// btcEndpoint builds a Bitcoin endpoint backed by a mock upstream that counts how
// many requests actually reach it and echoes each request id with a fixed result.
func btcEndpoint(t *testing.T) (*core.Endpoint, *atomic.Int64) {
	var hits atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		b, _ := io.ReadAll(r.Body)
		batch, err := rpc.DecodeBatchRequest(b)
		if err != nil || len(batch.Requests) == 0 {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		resps := make([]*rpc.Response, 0, len(batch.Requests))
		for _, req := range batch.Requests {
			resps = append(resps, &rpc.Response{Version: "2.0", ID: req.ID, Result: []byte(`"0xok"`)})
		}
		out, _ := rpc.SerializeBatchResponse(&rpc.BatchResponse{Responses: resps, IsBatch: batch.IsBatch})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
	}))
	t.Cleanup(srv.Close)

	ep := &core.Endpoint{Name: "btc", Kind: core.KindBTC}
	p := &core.Provider{Name: "p", Http: srv.URL, Endpoint: ep}
	ep.Providers = []*core.Provider{p}
	p.MarkHealthy(0)
	return ep, &hits
}

// TestBTCMethodFilter_Rejected: a non-blockchain Bitcoin RPC is rejected with a
// JSON-RPC error and never reaches the upstream.
func TestBTCMethodFilter_Rejected(t *testing.T) {
	ep, hits := btcEndpoint(t)

	w := doRPC(ep, `{"jsonrpc":"2.0","id":1,"method":"getwalletinfo","params":[]}`)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "method not allowed")
	assert.Equal(t, int64(0), hits.Load(), "disallowed method must not reach the upstream")
}

// TestBTCMethodFilter_Allowed: an allowlisted blockchain RPC is forwarded.
func TestBTCMethodFilter_Allowed(t *testing.T) {
	ep, hits := btcEndpoint(t)

	w := doRPC(ep, `{"jsonrpc":"2.0","id":1,"method":"getblock","params":["0xhash"]}`)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "0xok")
	assert.Equal(t, int64(1), hits.Load(), "allowed method should be forwarded")
}

// TestBTCMethodFilter_BatchRejected: a batch containing any disallowed method is
// rejected in full without reaching the upstream.
func TestBTCMethodFilter_BatchRejected(t *testing.T) {
	ep, hits := btcEndpoint(t)

	body := `[{"jsonrpc":"2.0","id":1,"method":"getblock","params":["0xh"]},` +
		`{"jsonrpc":"2.0","id":2,"method":"stop","params":[]}]`
	w := doRPC(ep, body)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "method not allowed")
	assert.Equal(t, int64(0), hits.Load(), "a batch with a disallowed method must not reach the upstream")
}
