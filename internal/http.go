package internal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"log"

	servertiming "github.com/mitchellh/go-server-timing"
)

var client = &http.Client{}

func ProxyHTTP(ctx context.Context, provider *Provider, body []byte, timing *servertiming.Header) ([]byte, *Endpoint, error) {
	endpoints := provider.GetActiveEndpoints()

	for _, endpoint := range endpoints {
		m := timing.NewMetric(endpoint.GetName()).Start()
		pbody, err := SendHTTPRequest(ctx, endpoint, body)
		m.Stop()

		// In case the request was closed before the provider connection was established
		// return to not treat the error as a provider error
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		default:
		}

		if err != nil {
			endpoint.SetStatus(false, err)
			provider.failedRequestCount++
			continue
		}

		endpoint.SetStatus(true, nil)
		provider.requestCount++
		return pbody, endpoint, nil
	}

	return nil, nil, ErrNoProvidersAvailable
}

func SendHTTPRequest(ctx context.Context, endpoint *Endpoint, body []byte) ([]byte, error) {
	url, _ := url.Parse(endpoint.Http)

	ctx, cancel := context.WithTimeout(ctx, endpoint.GetTimeout())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", url.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	req.Header = make(http.Header)
	req.Header.Set("User-Agent", "haprovider/"+Version)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	// req.ContentLength = int64(len(body))
	// req.Body = io.NopCloser(bytes.NewReader(body))

	resp, err := client.Do(req)

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
		return nil, endpoint.HandleTooManyRequests(resp)
	default:
		return nil, fmt.Errorf("unexpected status code %d, %s", resp.StatusCode, FormatRawBody(string(b)))
	}

	return b, nil
}

func SendHTTPRPCRequest(ctx context.Context, endpoint *Endpoint, requestID string, rpcreq *RPCRequest) (*RPCResponse, error) {
	req := SerializeRPCRequest(rpcreq)

	b, err := SendHTTPRequest(ctx, endpoint, req)
	if err != nil {
		return nil, err
	}

	var response = &RPCResponse{}
	err = json.Unmarshal(b, response)
	if err != nil {
		return nil, fmt.Errorf("bad response: %w, raw: %s", err, string(b))
	}

	return response, nil
}

func IncomingHttpHandler(ctx context.Context, provider *Provider, w http.ResponseWriter, r *http.Request, timing *servertiming.Header) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Println("Error reading body", err)
		http.Error(w, "Error reading body", http.StatusInternalServerError)
		return
	}

	// TODO validate JSON body

	pbody, endpoint, err := ProxyHTTP(ctx, provider, body, timing)

	if err == ErrNoProvidersAvailable {
		log.Printf("no providers available")
		http.Error(w, "no providers available", http.StatusServiceUnavailable)
		return
	}

	if err != nil {
		log.Printf("error dialing provider: %s", err)
		http.Error(w, "error dialing provider: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("X-Provider", endpoint.GetName())
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	_, err = w.Write(pbody)
	if err != nil {
		log.Println("Error writing body", err)
		return
	}

}
