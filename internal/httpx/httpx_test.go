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
