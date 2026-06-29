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
