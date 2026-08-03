package httpx_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/wille/haprovider/internal/core"
	"github.com/wille/haprovider/internal/httpx"
)

func TestForwardedFor(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.RemoteAddr = "203.0.113.7:55555"
	assert.Equal(t, "203.0.113.7", httpx.ForwardedFor(r), "no prior chain → just the client host")

	r.Header.Set("X-Forwarded-For", "10.0.0.1")
	assert.Equal(t, "10.0.0.1,203.0.113.7", httpx.ForwardedFor(r), "client host appended to the existing chain")
}

func TestClientIP(t *testing.T) {
	// Default: no configured header → remote address, port stripped.
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.RemoteAddr = "203.0.113.7:55555"
	assert.Equal(t, "203.0.113.7", httpx.ClientIP(r))

	// A configured single-value header (e.g. Cloudflare) is used verbatim.
	withRealIPHeader(t, "Cf-Connecting-Ip")
	r.Header.Set("Cf-Connecting-Ip", "198.51.100.9")
	assert.Equal(t, "198.51.100.9", httpx.ClientIP(r))

	// Configured but absent on the request → fall back to the remote address.
	r2 := httptest.NewRequest(http.MethodPost, "/", nil)
	r2.RemoteAddr = "203.0.113.7:55555"
	assert.Equal(t, "203.0.113.7", httpx.ClientIP(r2))

	// A comma-separated header (X-Forwarded-For chain) → the origin client (first).
	withRealIPHeader(t, "X-Forwarded-For")
	r3 := httptest.NewRequest(http.MethodPost, "/", nil)
	r3.RemoteAddr = "10.0.0.1:1234"
	r3.Header.Set("X-Forwarded-For", "198.51.100.9, 70.0.0.1, 10.0.0.1")
	assert.Equal(t, "198.51.100.9", httpx.ClientIP(r3))
}

func TestForwardedFor_RealIPHeader(t *testing.T) {
	// With a Cloudflare-style header, the real client IP is forwarded, appended to
	// any existing chain.
	withRealIPHeader(t, "Cf-Connecting-Ip")
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.RemoteAddr = "10.0.0.1:1234"
	r.Header.Set("Cf-Connecting-Ip", "198.51.100.9")
	assert.Equal(t, "198.51.100.9", httpx.ForwardedFor(r))

	r.Header.Set("X-Forwarded-For", "70.0.0.1")
	assert.Equal(t, "70.0.0.1,198.51.100.9", httpx.ForwardedFor(r))

	// When the real-IP header IS X-Forwarded-For, don't duplicate it — forward the
	// origin client only.
	withRealIPHeader(t, "X-Forwarded-For")
	r2 := httptest.NewRequest(http.MethodPost, "/", nil)
	r2.RemoteAddr = "10.0.0.1:1234"
	r2.Header.Set("X-Forwarded-For", "198.51.100.9, 70.0.0.1")
	assert.Equal(t, "198.51.100.9", httpx.ForwardedFor(r2))
}

// withRealIPHeader sets httpx.RealIPHeader for the duration of a test.
func withRealIPHeader(t *testing.T, header string) {
	prev := httpx.RealIPHeader
	httpx.RealIPHeader = header
	t.Cleanup(func() { httpx.RealIPHeader = prev })
}

func TestSetForwardedFor(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.RemoteAddr = "203.0.113.7:55555"

	ctx := httpx.WithForwardedFor(context.Background(), r)
	h := http.Header{}
	httpx.SetForwardedFor(ctx, h)
	assert.Equal(t, "203.0.113.7", h.Get("X-Forwarded-For"))

	// A context without a stored value must not add the header.
	empty := http.Header{}
	httpx.SetForwardedFor(context.Background(), empty)
	assert.Empty(t, empty.Get("X-Forwarded-For"))
}

func httpProvider(t *testing.T, body string, maxResponseSize int64) *core.Provider {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	ep := &core.Endpoint{Name: "t", MaxResponseSize: &maxResponseSize}
	p := &core.Provider{Name: "p", Http: srv.URL, Endpoint: ep}
	ep.Providers = []*core.Provider{p}
	return p
}

// TestReadUpstreamBody_OverLimit: an upstream response larger than the endpoint
// cap is rejected with an error rather than buffered into memory.
func TestReadUpstreamBody_OverLimit(t *testing.T) {
	p := httpProvider(t, strings.Repeat("a", 2000), 1000)

	_, err := httpx.SendHTTPRequest(context.Background(), p, p.Http, []byte("{}"))
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "max_response_size")
}

// TestReadUpstreamBody_WithinLimit: a response within the cap is returned intact.
func TestReadUpstreamBody_WithinLimit(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":1,"result":"0x1"}`
	p := httpProvider(t, body, 1<<20)

	b, err := httpx.SendHTTPRequest(context.Background(), p, p.Http, []byte("{}"))
	assert.NoError(t, err)
	assert.Equal(t, body, string(b))
}
