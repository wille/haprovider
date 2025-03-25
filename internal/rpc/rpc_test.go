package rpc

import (
	"testing"
)

func TestRPCResponse_IsError(t *testing.T) {
	raw := `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"Internal error"}}`

	rpcResponse, err := DecodeResponse([]byte(raw))
	if err != nil {
		t.Fatalf("error decoding RPC response: %v", err)
	}

	if !rpcResponse.IsError() {
		t.Fatalf("expected error")
	}

	code, err := rpcResponse.GetError()
	if err.Error() != "-32000 Internal error" {
		t.Fatalf("expected error code -32000, got %d", code)
	}

	if code != -32000 {
		t.Fatalf("expected error code -32000, got %d", code)
	}
}
