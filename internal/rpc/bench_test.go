package rpc

import (
	"fmt"
	"strings"
	"testing"
)

// These benchmarks track the cost of the JSON-RPC codec the proxy runs on every
// request. The headline metric is allocs/op and B/op: the RawMessage fields
// (Result, Params, Error, ID) exist to avoid materializing/​re-encoding payloads,
// and that win scales with payload size — hence the small vs large cases.
//
// Compare across changes with: go test -run=^$ -bench=. -count=10 ./internal/rpc/
// then benchstat.

// logsResult builds a realistic large eth_getLogs response with n entries.
func logsResult(n int) string {
	var sb strings.Builder
	sb.WriteString(`{"jsonrpc":"2.0","id":1,"result":[`)
	for i := 0; i < n; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, `{"address":"0x%040x","topics":["0x%064x","0x%064x"],`+
			`"data":"0x%0128x","blockNumber":"0x%x","transactionHash":"0x%064x",`+
			`"transactionIndex":"0x%x","blockHash":"0x%064x","logIndex":"0x%x","removed":false}`,
			i, i, i, i, i, i, i, i, i)
	}
	sb.WriteString(`]}`)
	return sb.String()
}

// batchRequest builds an n-element batch request.
func batchRequest(n int) string {
	var sb strings.Builder
	sb.WriteByte('[')
	for i := 0; i < n; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, `{"jsonrpc":"2.0","id":%d,"method":"eth_getBalance","params":["0x%040x","latest"]}`, i, i)
	}
	sb.WriteByte(']')
	return sb.String()
}

var benchResponses = []struct {
	name string
	body []byte
}{
	{"small", []byte(`{"jsonrpc":"2.0","id":1,"result":"0x10"}`)},
	{"large", []byte(logsResult(200))},
}

var benchRequests = []struct {
	name string
	body []byte
}{
	{"single", []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0x1111111111111111111111111111111111111111","data":"0xabcdef"},"latest"]}`)},
	{"batch100", []byte(batchRequest(100))},
}

func BenchmarkDecodeBatchResponse(b *testing.B) {
	for _, c := range benchResponses {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(c.body)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := DecodeBatchResponse(c.body); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkSerializeBatchResponse(b *testing.B) {
	for _, c := range benchResponses {
		b.Run(c.name, func(b *testing.B) {
			res, err := DecodeBatchResponse(c.body)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := SerializeBatchResponse(res); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkResponseRoundTrip mirrors what the proxy does to every upstream
// response: decode it, then re-serialize it for the client. Most representative
// of the per-request response cost, with no network noise.
func BenchmarkResponseRoundTrip(b *testing.B) {
	for _, c := range benchResponses {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(c.body)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				res, err := DecodeBatchResponse(c.body)
				if err != nil {
					b.Fatal(err)
				}
				if _, err := SerializeBatchResponse(res); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkDecodeBatchRequest(b *testing.B) {
	for _, c := range benchRequests {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(c.body)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := DecodeBatchRequest(c.body); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkSerializeBatchRequest(b *testing.B) {
	for _, c := range benchRequests {
		b.Run(c.name, func(b *testing.B) {
			req, err := DecodeBatchRequest(c.body)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := SerializeBatchRequest(req); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// benchBlock is a small typed result, like a chain healthcheck parses.
type benchBlock struct {
	Number  string `json:"number"`
	Hash    string `json:"hash"`
	GasUsed string `json:"gasUsed"`
}

// BenchmarkDecodeResult measures DecodeResult[T] (the healthcheck parse path):
// a single Unmarshal of the raw result into a struct.
func BenchmarkDecodeResult(b *testing.B) {
	res, err := DecodeResponse([]byte(`{"jsonrpc":"2.0","id":1,"result":{"number":"0x10","hash":"0xabc123","gasUsed":"0x5208"}}`))
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := DecodeResult[benchBlock](res); err != nil {
			b.Fatal(err)
		}
	}
}
