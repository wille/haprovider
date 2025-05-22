package rpc

import (
	"encoding/json"
	"testing"
)

// Incoming single request should be serialized as a single object
func TestNoBatchRequest(t *testing.T) {
	req := BatchRequest{}
	err := json.Unmarshal([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x1", false]}`), &req)
	if err != nil {
		t.Fatalf("error unmarshalling batch request: %v", err)
	}

	if req.IsBatch {
		t.Fatalf("single request IsBatch=true")
	}

	b := SerializeBatchRequest(&req)
	if b[0] != '{' {
		t.Fatalf("expected single request to be serialized as a single object")
	}
}

func TestBatchRequest(t *testing.T) {
	req := BatchRequest{}
	err := json.Unmarshal([]byte(`[{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x1", false]},{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x1", false]}]`), &req)
	if err != nil {
		t.Fatalf("error unmarshalling batch request: %v", err)
	}

	if !req.IsBatch {
		t.Fatalf("batch request IsBatch=false")
	}

	if len(req.Requests) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(req.Requests))
	}

	b := SerializeBatchRequest(&req)
	if b[0] != '[' {
		t.Fatalf("expected batch request to be serialized as an array")
	}
}

func TestNoBatchResponse(t *testing.T) {
	res := BatchResponse{}
	err := json.Unmarshal([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x1"}`), &res)
	if err != nil {
		t.Fatalf("error unmarshalling batch response: %v", err)
	}

	if res.IsBatch {
		t.Fatalf("single response IsBatch=true")
	}

	b := SerializeBatchResponse(&res)
	if b[0] != '{' {
		t.Fatalf("expected single response to be serialized as a single object")
	}
}

func TestBatchResponse(t *testing.T) {
	res := BatchResponse{}
	err := json.Unmarshal([]byte(`[{"jsonrpc":"2.0","id":1,"result":"0x1"}]`), &res)
	if err != nil {
		t.Fatalf("error unmarshalling batch response: %v", err)
	}

	if !res.IsBatch {
		t.Fatalf("batch response IsBatch=false")
	}

	b := SerializeBatchResponse(&res)
	if b[0] != '[' {
		t.Fatalf("expected batch response to be serialized as an array")
	}
}
