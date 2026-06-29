package rpc

import (
	"strings"
	"testing"
)

// TestError_WellFormed: a spec error decodes to its code/message, and the whole
// error object (including data) is forwarded verbatim.
func TestError_WellFormed(t *testing.T) {
	raw := `{"jsonrpc":"2.0","id":1,"error":{"code":-32005,"message":"rate limited","data":"0xdeadbeef"}}`

	res, err := DecodeResponse([]byte(raw))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !res.IsError() {
		t.Fatal("expected IsError")
	}

	code, e := res.GetError()
	if code != -32005 {
		t.Fatalf("expected code -32005, got %d", code)
	}
	if !strings.Contains(e.Error(), "rate limited") {
		t.Fatalf("expected message in error, got %q", e.Error())
	}

	// The error object — including the data field — is preserved byte-for-byte.
	out, err := SerializeResponse(res)
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}
	if !strings.Contains(string(out), `"data":"0xdeadbeef"`) {
		t.Fatalf("data not preserved: %s", out)
	}
}

// TestError_NonObjectLenient: a non-spec error (a bare string) is NOT rejected —
// it decodes fine, GetError degrades to code 0 + raw text, and it's forwarded
// verbatim so the client still sees what the provider said.
func TestError_NonObjectLenient(t *testing.T) {
	raw := `{"jsonrpc":"2.0","id":1,"error":"server busy"}`

	res, err := DecodeResponse([]byte(raw))
	if err != nil {
		t.Fatalf("a non-object error must not fail decoding: %v", err)
	}
	if !res.IsError() {
		t.Fatal("expected IsError")
	}

	code, e := res.GetError()
	if code != 0 {
		t.Fatalf("expected code 0 for a non-object error, got %d", code)
	}
	if !strings.Contains(e.Error(), "server busy") {
		t.Fatalf("expected raw error text, got %q", e.Error())
	}

	out, err := SerializeResponse(res)
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}
	if !strings.Contains(string(out), `"error":"server busy"`) {
		t.Fatalf("error not forwarded verbatim: %s", out)
	}
}
