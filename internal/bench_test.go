package internal

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	servertiming "github.com/mitchellh/go-server-timing"
	"github.com/wille/haprovider/internal/cache"
	"github.com/wille/haprovider/internal/core"
	"github.com/wille/haprovider/internal/rpctest"
)

// benchLogs builds a large eth_getLogs result (n entries) for the mock upstream
// to return, so the benchmark exercises the large-payload forwarding path.
func benchLogs(n int) []map[string]string {
	logs := make([]map[string]string, n)
	for i := range logs {
		logs[i] = map[string]string{
			"address":          "0x1111111111111111111111111111111111111111",
			"data":             "0x" + strings.Repeat("ab", 64),
			"blockNumber":      "0x10f2c",
			"transactionHash":  "0x" + strings.Repeat("cd", 32),
			"transactionIndex": "0x1",
			"blockHash":        "0x" + strings.Repeat("ef", 32),
			"logIndex":         "0x2",
		}
	}
	return logs
}

func benchEndpoint(b *testing.B, methods map[string]any) *core.Endpoint {
	srv := rpctest.NewServer(methods, nil)
	b.Cleanup(srv.Close)

	ep := &core.Endpoint{Name: "test", ChainID: "1", Kind: core.KindEth}
	p := &core.Provider{Name: "p", Http: srv.URL, Endpoint: ep}
	ep.Providers = []*core.Provider{p}
	p.MarkHealthy(0)
	return ep
}

// BenchmarkIncomingHttpRpcHandler measures the full per-request cost through the
// HTTP handler against a mock upstream: read body → decode request → select
// provider → upstream round-trip → decode + forward response. Holistic; note it
// includes a loopback HTTP call to the mock, which dominates and adds variance,
// so read it alongside the pure-codec benchmarks in internal/rpc.
func BenchmarkIncomingHttpRpcHandler(b *testing.B) {
	cases := []struct {
		name    string
		methods map[string]any
		body    string
	}{
		{
			name:    "small",
			methods: map[string]any{"eth_blockNumber": "0x10"},
			body:    `{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`,
		},
		{
			name:    "large",
			methods: map[string]any{"eth_getLogs": benchLogs(200)},
			body:    `{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{}]}`,
		},
	}

	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			ep := benchEndpoint(b, c.methods)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				r := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(c.body))
				r.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				IncomingHttpRpcHandler(context.Background(), ep, w, r, &servertiming.Header{})
			}
		})
	}
}

// BenchmarkIncomingHttpRpcHandler_CacheHit measures serving a cacheable request
// entirely from the warm cache, i.e. the path that skips the upstream round-trip.
// Compare against the "small" case above to see the cache win.
func BenchmarkIncomingHttpRpcHandler_CacheHit(b *testing.B) {
	srv := rpctest.NewServer(map[string]any{"eth_getBlockByHash": map[string]string{"hash": "0xabc"}}, nil)
	b.Cleanup(srv.Close)

	ep := &core.Endpoint{Name: "test", ChainID: "1", Kind: core.KindEth}
	if err := ep.InitCache(cache.NewMemory(0, 0)); err != nil {
		b.Fatal(err)
	}
	p := &core.Provider{Name: "p", Http: srv.URL, Endpoint: ep}
	ep.Providers = []*core.Provider{p}
	p.MarkHealthy(0)

	body := `{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByHash","params":["0xh",false]}`

	// Warm the cache so every measured iteration is a hit.
	warm := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(body))
	warm.Header.Set("Content-Type", "application/json")
	IncomingHttpRpcHandler(context.Background(), ep, httptest.NewRecorder(), warm, &servertiming.Header{})

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		IncomingHttpRpcHandler(context.Background(), ep, w, r, &servertiming.Header{})
	}
}
