package rpc

import (
	"encoding/json"
	"fmt"
)

// MaxBatchSize is the maximum number of requests allowed in a single batch.
const MaxBatchSize = 100

// MaxRequestBodySize is the maximum size in bytes of an incoming client request
// (HTTP body or WebSocket message). 5 MB matches go-ethereum's HTTP RPC limit.
// This bounds client input only; upstream provider responses are not limited.
const MaxRequestBodySize = 5 * 1024 * 1024

type BatchResponse struct {
	Responses []*Response
	IsBatch   bool
}

var _ json.Unmarshaler = &BatchResponse{}
var _ json.Marshaler = &BatchResponse{}

func NewBatchResponse(res ...*Response) *BatchResponse {
	return &BatchResponse{
		Responses: res,
		IsBatch:   len(res) > 1,
	}
}

// UnmarshalJSON for a batch request supports decoding a single request as well
func (r *BatchResponse) UnmarshalJSON(b []byte) error {
	switch b[0] {
	case '[':
		var res []*Response
		err := json.Unmarshal(b, &res)
		if err != nil {
			return err
		}
		r.Responses = res
		r.IsBatch = true
		return nil
	case '{':
		var res Response
		err := json.Unmarshal(b, &res)
		if err != nil {
			return err
		}
		r.Responses = []*Response{&res}
		r.IsBatch = false
		return nil
	}

	return fmt.Errorf("invalid request: %s", FormatRawBody(string(b)))
}

// MarshalJSON will encode a single (non batched) request to a single object or multiple requests into an array
func (r BatchResponse) MarshalJSON() ([]byte, error) {
	if len(r.Responses) == 0 {
		return nil, fmt.Errorf("empty batch response")
	}

	// If the batch is just one single request then unbatch it
	if !r.IsBatch {
		return json.Marshal(r.Responses[0])
	}

	return json.Marshal(r.Responses)
}

func (r *BatchResponse) GetResponseByID(id string) *Response {
	for _, r := range r.Responses {
		if r.GetID() == id {
			return r
		}
	}
	return nil
}

type BatchRequest struct {
	Requests []*Request
	IsBatch  bool
}

var _ json.Unmarshaler = &BatchRequest{}
var _ json.Marshaler = &BatchRequest{}

func NewBatchRequest(req ...*Request) *BatchRequest {
	return &BatchRequest{
		Requests: req,
		IsBatch:  len(req) > 1,
	}
}

// UnmarshalJSON for a batch request supports decoding a single request as well
func (r *BatchRequest) UnmarshalJSON(b []byte) error {
	switch b[0] {
	case '[':
		var req []*Request
		err := json.Unmarshal(b, &req)
		if err != nil {
			return err
		}
		r.Requests = req
		r.IsBatch = true
		return nil
	case '{':
		var req Request
		err := json.Unmarshal(b, &req)
		if err != nil {
			return err
		}
		r.Requests = []*Request{&req}
		r.IsBatch = false
		return nil
	}

	return fmt.Errorf("invalid request: %s", FormatRawBody(string(b)))
}

// MarshalJSON will encode a single (non batched) request to a single object or multiple requests into an array
func (r BatchRequest) MarshalJSON() ([]byte, error) {
	if len(r.Requests) == 0 {
		return nil, fmt.Errorf("empty batch request")
	}

	// If the batch is just one single request then unbatch it
	if !r.IsBatch {
		return json.Marshal(r.Requests[0])
	}

	return json.Marshal(r.Requests)
}

func DecodeBatchRequest(b []byte) (*BatchRequest, error) {
	var batch BatchRequest
	err := json.Unmarshal(b, &batch)
	if err != nil {
		return nil, err
	}
	return &batch, nil
}

func DecodeBatchResponse(b []byte) (*BatchResponse, error) {
	var batch BatchResponse
	err := json.Unmarshal(b, &batch)
	if err != nil {
		return nil, err
	}
	return &batch, nil
}

func SerializeBatchRequest(req *BatchRequest) ([]byte, error) {
	return json.Marshal(req)
}

func SerializeBatchResponse(res *BatchResponse) ([]byte, error) {
	return json.Marshal(res)
}
