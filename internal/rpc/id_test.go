package rpc

import (
	"strings"
	"testing"
)

// TestID_Validation: a request id must be a string, number, or null; an object,
// array, or bool is rejected at decode time.
func TestID_Validation(t *testing.T) {
	cases := []struct {
		name string
		id   string // the raw "id" value, or "" to omit it
		ok   bool
	}{
		{"number", `1`, true},
		{"string", `"abc"`, true},
		{"null", `null`, true},
		{"object", `{"x":1}`, false},
		{"array", `[1]`, false},
		{"bool", `true`, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := `{"jsonrpc":"2.0","id":` + c.id + `,"method":"m"}`
			_, err := DecodeBatchRequest([]byte(body))
			if c.ok && err != nil {
				t.Fatalf("expected valid id, got error: %v", err)
			}
			if !c.ok && err == nil {
				t.Fatalf("expected %s id to be rejected, got none", c.name)
			}
		})
	}
}

// TestID_EchoFaithful: ids are forwarded verbatim — a number stays a number, a
// string stays a string, and a large integer keeps full precision (the old
// any→float64 path would have mangled it).
func TestID_EchoFaithful(t *testing.T) {
	for _, id := range []string{`1`, `"abc"`, `123456789012345678`} {
		req, err := DecodeBatchRequest([]byte(`{"jsonrpc":"2.0","id":` + id + `,"method":"m"}`))
		if err != nil {
			t.Fatalf("decode %s: %v", id, err)
		}
		b, err := SerializeBatchRequest(req)
		if err != nil {
			t.Fatalf("serialize %s: %v", id, err)
		}
		if !strings.Contains(string(b), `"id":`+id) {
			t.Fatalf("id not echoed faithfully: want %q in %s", id, b)
		}
	}
}

func TestCheckResponseIDs(t *testing.T) {
	cases := []struct {
		name     string
		req, res string
		ok       bool
	}{
		{"same id", `{"jsonrpc":"2.0","id":1,"method":"m"}`, `{"jsonrpc":"2.0","id":1,"result":"0x1"}`, true},
		{"same id, other type", `{"jsonrpc":"2.0","id":1,"method":"m"}`, `{"jsonrpc":"2.0","id":"1","result":"0x1"}`, true},
		{"other id", `{"jsonrpc":"2.0","id":1,"method":"m"}`, `{"jsonrpc":"2.0","id":2,"result":"0x1"}`, false},
		{"result without id", `{"jsonrpc":"2.0","id":1,"method":"m"}`, `{"jsonrpc":"2.0","result":"0x1"}`, false},
		{"result with null id", `{"jsonrpc":"2.0","id":1,"method":"m"}`, `{"jsonrpc":"2.0","id":null,"result":"0x1"}`, false},
		{"error with null id", `{"jsonrpc":"2.0","id":1,"method":"m"}`, `{"jsonrpc":"2.0","id":null,"error":{"code":-32005,"message":"rate limited"}}`, true},
		{"error with other id", `{"jsonrpc":"2.0","id":1,"method":"m"}`, `{"jsonrpc":"2.0","id":2,"error":{"code":-32000,"message":"x"}}`, false},

		{"batch, any order",
			`[{"jsonrpc":"2.0","id":1,"method":"m"},{"jsonrpc":"2.0","id":"a","method":"m"}]`,
			`[{"jsonrpc":"2.0","id":"a","result":"0x2"},{"jsonrpc":"2.0","id":1,"result":"0x1"}]`, true},
		{"batch, two answers to one request",
			`[{"jsonrpc":"2.0","id":1,"method":"m"},{"jsonrpc":"2.0","id":2,"method":"m"}]`,
			`[{"jsonrpc":"2.0","id":1,"result":"0x1"},{"jsonrpc":"2.0","id":1,"result":"0x1"}]`, false},
		{"batch, an answer to another request",
			`[{"jsonrpc":"2.0","id":1,"method":"m"},{"jsonrpc":"2.0","id":2,"method":"m"}]`,
			`[{"jsonrpc":"2.0","id":1,"result":"0x1"},{"jsonrpc":"2.0","id":3,"result":"0x3"}]`, false},
		{"batch, an error with null id",
			`[{"jsonrpc":"2.0","id":1,"method":"m"},{"jsonrpc":"2.0","id":2,"method":"m"}]`,
			`[{"jsonrpc":"2.0","id":1,"result":"0x1"},{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"x"}}]`, true},

		// The client's own ids can't be matched, so nothing is checked.
		{"request without id", `{"jsonrpc":"2.0","method":"m"}`, `{"jsonrpc":"2.0","id":7,"result":"0x1"}`, true},
		{"batch with a repeated request id",
			`[{"jsonrpc":"2.0","id":1,"method":"m"},{"jsonrpc":"2.0","id":1,"method":"m"}]`,
			`[{"jsonrpc":"2.0","id":5,"result":"0x1"},{"jsonrpc":"2.0","id":6,"result":"0x1"}]`, true},
	}

	for _, c := range cases {
		req, err := DecodeBatchRequest([]byte(c.req))
		if err != nil {
			t.Fatalf("%s: decode request: %v", c.name, err)
		}
		res, err := DecodeBatchResponse([]byte(c.res))
		if err != nil {
			t.Fatalf("%s: decode response: %v", c.name, err)
		}

		err = CheckResponseIDs(req, res)
		if c.ok && err != nil {
			t.Errorf("%s: unexpected error: %v", c.name, err)
		}
		if !c.ok && err == nil {
			t.Errorf("%s: expected an error", c.name)
		}
	}
}
