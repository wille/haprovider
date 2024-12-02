package internal

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"log"
)

var client = &http.Client{}

func ProxyHTTP(provider *Provider, body []byte) ([]byte, error) {
	endpoints := provider.GetActiveEndpoints()

	for _, endpoint := range endpoints {
		pbody, err := SendHTTPRequest(endpoint, body)
		if err != nil {
			endpoint.SetStatus(false, err)
			continue
		}

		return pbody, nil
	}

	return nil, ErrNoProvidersAvailable
}

func SendHTTPRequest(endpoint *Endpoint, body []byte) ([]byte, error) {
	url, _ := url.Parse(endpoint.Http)

	req := &http.Request{
		Method: "POST",
		URL:    url,
	}
	req.Header = make(http.Header)
	req.Header.Set("User-Agent", "haprovider/"+Version)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	req.ContentLength = int64(len(body))
	req.Body = io.NopCloser(bytes.NewReader(body))

	resp, err := client.Do(req)

	if err != nil {
		return nil, err
	}

	b, err := io.ReadAll(resp.Body)

	switch resp.StatusCode {
	case http.StatusOK:
		break
	case http.StatusTooManyRequests:
		return nil, endpoint.HandleTooManyRequests(resp)
	default:
		return nil, fmt.Errorf("unexpected status code %d, %s", resp.StatusCode, FormatRawBody(string(b)))
	}

	if err != nil {
		return nil, err
	}

	return b, nil
}

func SendHTTPRPCRequest(endpoint *Endpoint, requestID string, rpcreq *RPCRequest) (*RPCResponse, error) {
	req := SerializeRPCRequest(rpcreq)

	b, err := SendHTTPRequest(endpoint, req)
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

func IncomingHttpHandler(provider *Provider, w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Println("Error reading body", err)
		http.Error(w, "Error reading body", http.StatusInternalServerError)
		return
	}

	// TODO validate JSON body

	pbody, err := ProxyHTTP(provider, body)
	if err != nil {
		log.Println("Could not forward request to any provider", err)
		http.Error(w, fmt.Sprintf("Could not forward request to any provider %s", err), http.StatusServiceUnavailable)
		return
	}

	_, err = w.Write(pbody)
	if err != nil {
		log.Println("Error writing body", err)
		return
	}

}
