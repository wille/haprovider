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
	"github.com/wille/haprovider/internal/metrics"
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

func ProxyHTTP(ctx context.Context, endpoint *Endpoint, req *rpc.BatchRequest, timing *servertiming.Header) (*rpc.BatchResponse, *Provider, error) {
	for _, provider := range endpoint.Providers {
		if !provider.online {
			continue
		}

		m := timing.NewMetric(provider.Name).Start()
		res, err := SendHTTPRPCRequest(ctx, provider, req)
		m.Stop()

		// In case the request was closed before the provider connection was established
		// return to not treat the error as a provider error
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		default:
		}

		if err != nil {
			// In a high traffic environment, the provider might be taken offline by another concurrent request
			if provider.online {
				provider.SetStatus(false, err)
			}
			continue
		}

		provider.SetStatus(true, nil)

		return res, provider, nil
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

	if provider.Headers != nil {
		for k, v := range provider.Headers {
			req.Header.Set(k, v)
		}
	}

	req.Header.Set("User-Agent", UserAgent)
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

func SendHTTPRPCRequest(ctx context.Context, p *Provider, req *rpc.BatchRequest) (*rpc.BatchResponse, error) {
	body := rpc.SerializeBatchRequest(req)

	b, err := SendHTTPRequest(ctx, p, body)
	if err != nil {
		return nil, err
	}

	response, err := rpc.DecodeBatchResponse(b)
	if err != nil {
		return nil, fmt.Errorf("bad response: %w, raw: %s", err, string(b))
	}

	return response, nil
}

func IncomingHttpHandler(ctx context.Context, endpoint *Endpoint, w http.ResponseWriter, r *http.Request, timing *servertiming.Header) {
	start := time.Now()

	log := slog.With("ip", r.RemoteAddr, "transport", "http", "endpoint", endpoint.Name)

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

	req, err := rpc.DecodeBatchRequest(body)
	if err != nil {
		log.Error("http: bad request", "error", err, "body", rpc.FormatRawBody(string(body)))
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	if req.IsBatch {
		batchId := rpc.BatchIDCounter.Add(1)
		log = log.With("batch_id", batchId, "batch_size", len(req.Requests))
	} else {
		log = log.With("rpc_id", req.Requests[0].GetID(), "method", req.Requests[0].Method)
	}

	res, provider, err := ProxyHTTP(ctx, endpoint, req, timing)

	if err != nil {
		for _, req := range req.Requests {
			metrics.RecordFailedRequest(endpoint.Name, provider.Name, "http", req.Method)
		}

		if err == ErrNoProvidersAvailable {
			log.Error("no providers available")
			http.Error(w, "no providers available", http.StatusServiceUnavailable)
			return
		}

		log.Error("error proxying request", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	log = log.With("provider", provider.Name, "request_time", time.Since(start))

	for i, res := range req.Requests {
		if req.IsBatch {
			log.Debug("request", "batch_index", i, "rpc_id", res.GetID(), "method", res.Method)
		} else {
			// id and method is already set for this single request
			log.Debug("request")
		}
	}

	didGoOffline := false

	for i, res := range res.Responses {
		method := req.Requests[i].Method

		if res.IsError() {
			log.Error("error", "error", res.Error)
			metrics.RecordFailedRequest(endpoint.Name, provider.Name, "http", method)
		} else {
			metrics.RecordRequest(endpoint.Name, provider.Name, "http", method, time.Since(start).Seconds())
		}

		if !didGoOffline && res.IsError() {
			errorCode, errorMessage := res.GetError()

			// TODO: These errors are Ethereum specific. We should handle them in a more generic way.
			switch errorCode {
			case EthErrorRateLimited:
				provider.HandleTooManyRequests(nil)
				provider.SetStatus(false, errorMessage)

			case EthErrorInternalError:
				provider.SetStatus(false, errorMessage)

			default:
				log.Warn("error response", "error_code", errorCode, "error_message", errorMessage, "raw_error", res.Error)
				continue
			}

			// Only go offline once. We still want to return error responses to the client.
			didGoOffline = true
		}
	}

	if req.IsBatch {
		if len(res.Responses) != len(req.Requests) {
			log.Error("batch response size mismatch", "request_size", len(req.Requests), "response_size", len(res.Responses))
			http.Error(w, "batch response size mismatch", http.StatusInternalServerError)
			return
		}
	}

	if !endpoint.Public {
		w.Header().Set("X-Provider", provider.Name)
	}

	if endpoint.AddXForwardedHeaders {
		addXfwdHeaders(r, w)
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	_, err = w.Write(rpc.SerializeBatchResponse(res))
	if err != nil {
		log.Error("error writing body", "error", err)
		return
	}
}
