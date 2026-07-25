package internal

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/wille/haprovider/internal/core"
)

// verbatimEndpoint backs an eth endpoint with a mock that returns exactly raw,
// so tests can assert the proxy forwards the upstream body byte-for-byte.
func verbatimEndpoint(t *testing.T, raw string) *core.Endpoint {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(raw))
	}))
	t.Cleanup(srv.Close)

	ep := &core.Endpoint{Name: "test", ChainID: "1", Kind: core.KindEth}
	p := &core.Provider{Name: "p", Http: srv.URL, Endpoint: ep}
	ep.Providers = []*core.Provider{p}
	p.MarkHealthy(0)
	return ep
}

// TestPassThrough_Verbatim: the upstream body is forwarded to the client
// byte-for-byte (no re-encode), on both the direct and coalesce paths. The mock
// returns a deliberately pretty-printed body that our compact re-encode would
// not reproduce, so an exact match proves we passed the raw bytes through.
func TestPassThrough_Verbatim(t *testing.T) {
	raw := "{\n  \"jsonrpc\": \"2.0\",\n  \"id\": 1,\n  \"result\": [\"0xdeadbeef\"]\n}"
	ep := verbatimEndpoint(t, raw)

	// Coalesceable method (eth_getLogs) with id matching the upstream body's id ->
	// coalesce path, non-shared -> verbatim raw passthrough.
	w := doRPC(ep, `{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{}]}`)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, raw, w.Body.String(), "coalesceable request should be forwarded verbatim")

	// Non-coalesceable method (eth_sendRawTransaction) -> direct path -> verbatim.
	w2 := doRPC(ep, `{"jsonrpc":"2.0","id":1,"method":"eth_sendRawTransaction","params":["0x"]}`)
	assert.Equal(t, http.StatusOK, w2.Code)
	assert.Equal(t, raw, w2.Body.String(), "direct request should be forwarded verbatim")
}

// TestPassThrough_EmptyResponseIs500: a provider replying with an empty batch is
// not forwarded; the client gets a 500. Covers both the coalesce path
// (eth_getLogs) and the direct path (eth_sendRawTransaction).
func TestPassThrough_EmptyResponseIs500(t *testing.T) {
	ep := verbatimEndpoint(t, "[]")

	w := doRPC(ep, `{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{}]}`)
	assert.Equal(t, http.StatusInternalServerError, w.Code, "empty response (coalesce path) should 500")

	w2 := doRPC(ep, `{"jsonrpc":"2.0","id":1,"method":"eth_sendRawTransaction","params":["0x"]}`)
	assert.Equal(t, http.StatusInternalServerError, w2.Code, "empty response (direct path) should 500")
}
