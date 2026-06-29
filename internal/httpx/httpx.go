// Package httpx holds the shared HTTP transport primitives used to talk to
// upstream providers: the tuned HTTP client, User-Agent, X-Forwarded-For
// handling and the request/forward helpers. It depends only on core and rpc so
// that chain packages can reuse it without importing the internal package.
package httpx

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/wille/haprovider/internal/core"
	"github.com/wille/haprovider/internal/rpc"
)

// Version is the haprovider build version, set via -ldflags at build time.
var Version = "dev"

// UserAgent is sent on all upstream requests.
var UserAgent = fmt.Sprintf("haprovider/%s (+https://github.com/wille/haprovider)", Version)

var client = &http.Client{
	Transport: &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     5 * time.Minute,
	},
}

// Response is a raw upstream HTTP response, used when forwarding non-JSON-RPC
// requests (e.g. a chain's native HTTP API) verbatim to the client.
type Response struct {
	StatusCode  int
	ContentType string
	Body        []byte
}

type ctxKey int

const forwardedForKey ctxKey = iota

// ForwardedFor computes the X-Forwarded-For value to send upstream for a client
// request: the client's host, appended to any X-Forwarded-For chain the client
// already sent.
func ForwardedFor(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}

	if prior := r.Header.Get("X-Forwarded-For"); prior != "" {
		return prior + "," + host
	}
	return host
}

// WithForwardedFor stores the X-Forwarded-For value for r on the context so the
// upstream request builders (SendHTTPRequest/ForwardHTTPRequest) and the
// WebSocket dialer add it to requests sent to providers.
func WithForwardedFor(ctx context.Context, r *http.Request) context.Context {
	return context.WithValue(ctx, forwardedForKey, ForwardedFor(r))
}

// SetForwardedFor adds the X-Forwarded-For header carried on ctx (if any) to an
// outgoing upstream request's headers.
func SetForwardedFor(ctx context.Context, h http.Header) {
	if v, _ := ctx.Value(forwardedForKey).(string); v != "" {
		h.Set("X-Forwarded-For", v)
	}
}

// readUpstreamBody reads an upstream response body bounded by the endpoint's
// max response size, protecting the proxy from memory exhaustion on huge
// responses. A limit of 0 (or a nil endpoint) means unlimited.
func readUpstreamBody(provider *core.Provider, body io.Reader) ([]byte, error) {
	limit := core.DefaultMaxResponseSize
	if provider.Endpoint != nil {
		limit = provider.Endpoint.GetMaxResponseSize()
	}

	if limit <= 0 {
		return io.ReadAll(body)
	}

	b, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("upstream response exceeded max_response_size (%d bytes)", limit)
	}
	return b, nil
}

// SendHTTPRequest POSTs body to an explicit URL using the provider's headers,
// timeout and rate-limit handling, returning the response body. A non-200,
// non-429 status is returned as an error; a 429 is handled as a rate limit.
func SendHTTPRequest(ctx context.Context, provider *core.Provider, rawURL string, body []byte) ([]byte, error) {
	u, _ := url.Parse(rawURL)

	ctx, cancel := context.WithTimeout(ctx, provider.GetTimeout())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	if provider.Auth.User != "" && provider.Auth.Password != "" {
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(provider.Auth.User+":"+provider.Auth.Password)))
	}

	if provider.Headers != nil {
		for k, v := range provider.Headers {
			req.Header.Set(k, v)
		}
	}

	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	SetForwardedFor(ctx, req.Header)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	b, err := readUpstreamBody(provider, resp.Body)
	if err != nil {
		return nil, err
	}

	switch resp.StatusCode {
	case http.StatusOK:
		break
	case http.StatusTooManyRequests:
		// Handle 429 on the request level. Some providers will 429, others will return 200 but a rate limit error in the response.
		return b, provider.HandleTooManyRequests(resp)
	case http.StatusRequestEntityTooLarge:
		fallthrough
	default:
		return b, fmt.Errorf("http %d", resp.StatusCode)
	}

	return b, nil
}

// SendRPCBatchRequest sends a JSON-RPC batch request to the provider's configured
// HTTP endpoint and decodes the batch response.
func SendRPCBatchRequest(ctx context.Context, p *core.Provider, req *rpc.BatchRequest) (*rpc.BatchResponse, error) {
	body, err := rpc.SerializeBatchRequest(req)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize request: %w", err)
	}

	targetUrl := p.Http
	if p.Endpoint.Kind == "tron" && !strings.HasSuffix(targetUrl, "/jsonrpc") {
		targetUrl += "/jsonrpc"
	}

	b, err := SendHTTPRequest(ctx, p, targetUrl, body)
	if err != nil {
		// Try to parse a RPC error response
		response, _ := rpc.DecodeBatchResponse(b)
		if response != nil && len(response.Responses) > 0 {
			_, message := response.Responses[0].GetError()
			if message != nil {
				return nil, fmt.Errorf("%s: %s", err, message)
			}
		}
		return nil, err
	}

	response, err := rpc.DecodeBatchResponse(b)
	if err != nil {
		return nil, fmt.Errorf("bad response: %w, raw: %s", err, string(b))
	}

	return response, nil
}

// ForwardHTTPRequest forwards a request to an explicit upstream URL, preserving the method
// and body and reusing the provider's headers, timeout, User-Agent and
// rate-limit handling. A 429 is returned as an error (so the caller can mark the
// provider offline); all other upstream responses are returned verbatim.
func ForwardHTTPRequest(ctx context.Context, provider *core.Provider, method, rawURL string, body []byte, contentType string) (*Response, error) {
	ctx, cancel := context.WithTimeout(ctx, provider.GetTimeout())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, method, rawURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	if provider.Auth.User != "" && provider.Auth.Password != "" {
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(provider.Auth.User+":"+provider.Auth.Password)))
	}

	if provider.Headers != nil {
		for k, v := range provider.Headers {
			req.Header.Set(k, v)
		}
	}

	req.Header.Set("User-Agent", UserAgent)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	} else {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	SetForwardedFor(ctx, req.Header)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	b, err := readUpstreamBody(provider, resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, provider.HandleTooManyRequests(resp)
	}

	return &Response{
		StatusCode:  resp.StatusCode,
		ContentType: resp.Header.Get("Content-Type"),
		Body:        b,
	}, nil
}
