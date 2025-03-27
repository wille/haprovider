package internal

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	servertiming "github.com/mitchellh/go-server-timing"
	"github.com/wille/haprovider/internal/rpc"
)

var defaultClient = &http.Client{
	Transport: &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     5 * time.Minute,
	},
}

func ProxyHTTP(ctx context.Context, endpoint *Endpoint, body []byte, timing *servertiming.Header) ([]byte, *Provider, error) {
	providers := endpoint.GetActiveProviders()

	for _, provider := range providers {
		m := timing.NewMetric(provider.Name).Start()
		pbody, err := SendHTTPRequest(ctx, provider, body)
		m.Stop()

		// In case the request was closed before the provider connection was established
		// return to not treat the error as a provider error
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		default:
		}

		if err != nil {
			provider.SetStatus(false, err)
			endpoint.failedRequestCount++
			continue
		}

		provider.SetStatus(true, nil)
		endpoint.requestCount++
		endpoint.totalConnections++
		return pbody, provider, nil
	}

	return nil, nil, ErrNoProvidersAvailable
}

func SendHTTPRequest(ctx context.Context, provider *Provider, body []byte) ([]byte, error) {
	url, _ := url.Parse(provider.Http)

	ctx, cancel := context.WithTimeout(ctx, provider.GetTimeout())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", url.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	req.Header = make(http.Header)
	req.Header.Set("User-Agent", "haprovider/"+Version)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := defaultClient.Do(req)

	if err != nil {
		return nil, err
	}

	b, err := io.ReadAll(resp.Body)

	if err != nil {
		return nil, err
	}

	switch resp.StatusCode {
	case http.StatusOK:
		break
	case http.StatusTooManyRequests:
		return nil, provider.HandleTooManyRequests(resp)
	default:
		return nil, fmt.Errorf("status code %d, %s", resp.StatusCode, rpc.FormatRawBody(string(b)))
	}

	return b, nil
}

func SendHTTPRPCRequest(ctx context.Context, p *Provider, rpcreq *rpc.Request) (*rpc.Response, error) {
	req := rpc.SerializeRequest(rpcreq)

	b, err := SendHTTPRequest(ctx, p, req)
	if err != nil {
		return nil, err
	}

	response, err := rpc.DecodeResponse(b)
	if err != nil {
		return nil, fmt.Errorf("bad response: %w, raw: %s", err, string(b))
	}

	return response, nil
}

func IncomingHttpHandler(ctx context.Context, endpoint *Endpoint, w http.ResponseWriter, r *http.Request, timing *servertiming.Header) {
	start := time.Now()

	log := slog.With("ip", r.RemoteAddr, "transport", "http", "provider", endpoint.Name)

	if !strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		log.Error("http: close connection: invalid content type")
		http.Error(w, "invalid content type", http.StatusBadRequest)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Error("http: error reading body", "error", err)
		http.Error(w, "error reading body", http.StatusInternalServerError)
		return
	}

	rpcReq, err := rpc.DecodeRequest(body)
	if err != nil {
		log.Error("http: bad request", "error", err, "msg", rpc.FormatRawBody(string(body)))
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	log = log.With("rpc_id", rpc.GetRequestIDString(rpcReq.ID), "method", rpcReq.Method)

	pbody, provider, err := ProxyHTTP(ctx, endpoint, body, timing)

	if err == ErrNoProvidersAvailable {
		log.Error("no providers available")
		http.Error(w, "no providers available", http.StatusServiceUnavailable)
		return
	}

	if err != nil {
		log.Error("error proxying request", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	log = log.With("provider", provider.Name, "request_time", time.Since(start))

	log.Debug("request")

	if !endpoint.Public {
		w.Header().Set("X-Provider", provider.Name)
	}

	if endpoint.AddXForwardedHeaders {
		addXfwdHeaders(r, w)
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	_, err = w.Write(pbody)
	if err != nil {
		log.Error("error writing body", "error", err)
		return
	}
}
