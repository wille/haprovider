package internal

import (
	"context"
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
	"github.com/wille/haprovider/internal/core"
	"github.com/wille/haprovider/internal/rpc"
)

// BenchmarkCoalescing compares a burst of N concurrent identical requests when
// the method is coalesceable (eth_call) versus not (eth_sendRawTransaction).
// Both run the exact same handler; the only difference is whether the requests
// collapse into one upstream call. The upstream is given a small latency so all
// N requests overlap in flight (the window coalescing acts on).
//
// The headline metric is "upstream-calls/op": ~1 when coalesced, ~N when not.
func BenchmarkCoalescing(b *testing.B) {
	const (
		concurrency  = 32
		upstreamWait = 2 * time.Millisecond
	)

	cases := []struct {
		name   string
		method string
		params string
	}{
		{"coalesced", "eth_call", `[{"to":"0x0"},"latest"]`},
		{"uncoalesced", "eth_sendRawTransaction", `["0xsigned"]`},
	}

	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			var upstream atomic.Int64

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstream.Add(1)
				time.Sleep(upstreamWait)
				body, _ := io.ReadAll(r.Body)
				batch, _ := rpc.DecodeBatchRequest(body)
				resps := make([]*rpc.Response, 0, len(batch.Requests))
				for _, req := range batch.Requests {
					resps = append(resps, &rpc.Response{Version: "2.0", ID: req.ID, Result: []byte(`"0xresult"`)})
				}
				out, _ := rpc.SerializeBatchResponse(&rpc.BatchResponse{Responses: resps, IsBatch: batch.IsBatch})
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(out)
			}))
			b.Cleanup(srv.Close)

			ep := &core.Endpoint{Name: "test", ChainID: "1", Kind: core.KindEth}
			p := &core.Provider{Name: "p", Http: srv.URL, Endpoint: ep}
			ep.Providers = []*core.Provider{p}
			p.MarkHealthy(0)

			body := func(i int) string {
				return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q,"params":%s}`, i, tc.method, tc.params)
			}

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				fireBurst(ep, concurrency, body)
			}
			b.StopTimer()

			b.ReportMetric(float64(upstream.Load())/float64(b.N), "upstream-calls/op")
		})
	}
}

// fireBurst sends n identical concurrent requests through the handler, released
// together so they overlap in flight, and waits for all to complete.
func fireBurst(ep *core.Endpoint, n int, body func(i int) string) {
	var wg sync.WaitGroup
	release := make(chan struct{})
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			r := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(body(i)))
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			<-release
			IncomingHttpRpcHandler(context.Background(), ep, w, r, &servertiming.Header{})
		}(i)
	}
	close(release)
	wg.Wait()
}
