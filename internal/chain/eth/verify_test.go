package eth

import (
	"encoding/json"
	"testing"

	"github.com/wille/haprovider/internal/verify"
)

func TestVerifier(t *testing.T) {
	cases := []struct {
		method string
		a, b   string
		want   verify.Outcome
		result string // expected result, when it isn't a
	}{
		{"eth_getBalance", `"0xa"`, `"0x0a"`, verify.Agree, ""},
		{"eth_getBalance", `"0xde0b6b3a7640000"`, `"0xDE0B6B3A7640000"`, verify.Agree, ""},
		{"eth_getBalance", `"0x1"`, `"0x2"`, verify.Disagree, ""},
		{"eth_getBalance", `"0x1"`, `"0xzz"`, verify.Inconclusive, ""},
		{"eth_getBalance", `"0x1"`, `1`, verify.Inconclusive, ""},
		{"eth_getBalance", `"0x1"`, `"0x"`, verify.Inconclusive, ""},
		{"eth_getBalance", `"1"`, `"0x1"`, verify.Disagree, ""},

		{"eth_call", `"0xAbC0"`, `"0xabc0"`, verify.Agree, ""},
		{"eth_call", `"0x"`, `"0x"`, verify.Agree, ""},
		{"eth_call", `"0x01"`, `"0x02"`, verify.Disagree, ""},
		{"eth_call", `"0x01"`, `"0x0"`, verify.Inconclusive, ""},

		{"eth_blockNumber", `"0x105"`, `"0x100"`, verify.Agree, `"0x100"`},
		{"eth_blockNumber", `"0x100"`, `"0x105"`, verify.Agree, `"0x100"`},
		{"eth_blockNumber", `"0x100"`, `null`, verify.Inconclusive, ""},
	}

	for _, c := range cases {
		v := Verifier(c.method, nil)
		if v == nil {
			t.Fatalf("%s: no verifier", c.method)
		}

		got, outcome := v(json.RawMessage(c.a), json.RawMessage(c.b))
		if outcome != c.want {
			t.Errorf("%s(%s, %s) = %s, want %s", c.method, c.a, c.b, outcome, c.want)
		}

		want := c.result
		if want == "" {
			want = c.a
		}
		if string(got) != want {
			t.Errorf("%s(%s, %s) returned %s, want %s", c.method, c.a, c.b, got, want)
		}
	}
}

func TestVerifier_OnlyListedMethods(t *testing.T) {
	for _, method := range []string{"eth_sendRawTransaction", "eth_getLogs", "eth_chainId", "eth_estimateGas"} {
		if Verifier(method, nil) != nil {
			t.Errorf("%s should not be cross-checked", method)
		}
	}
}

// TestVerifier_Pending: reads at the pending block aren't cross-checked, since
// each node builds its pending block from its own mempool.
func TestVerifier_Pending(t *testing.T) {
	cases := []struct {
		method, params string
		checked        bool
	}{
		{"eth_call", `[{"to":"0x1"},"pending"]`, false},
		{"eth_getBalance", `["0x1","pending"]`, false},
		{"eth_call", `[{"to":"0x1"},"latest"]`, true},
		{"eth_call", `[{"to":"0x1"}]`, true}, // omitted: latest
		{"eth_call", `[{"to":"0x1"},"finalized"]`, true},
		{"eth_call", `[{"to":"0x1"},"0x10"]`, true},
		{"eth_call", `[{"to":"0x1"},{"blockHash":"0xabc"}]`, true},
		{"eth_getBalance", `["0x1","latest"]`, true},
		{"eth_blockNumber", `[]`, true},
	}
	for _, c := range cases {
		if got := Verifier(c.method, json.RawMessage(c.params)) != nil; got != c.checked {
			t.Errorf("%s %s: checked = %v, want %v", c.method, c.params, got, c.checked)
		}
	}
}
