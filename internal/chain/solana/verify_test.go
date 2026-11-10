package solana

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
		// The context slot differs between providers; only the value counts.
		{"getBalance", `{"context":{"slot":1},"value":5}`, `{"context":{"slot":2},"value":5}`, verify.Agree, ""},
		{"getBalance", `{"context":{"slot":1},"value":5}`, `{"context":{"slot":1},"value":6}`, verify.Disagree, ""},
		{"getBalance", `{"context":{"slot":1},"value":5}`, `{"context":{"slot":1}}`, verify.Inconclusive, ""},
		{"getBalance", `{"context":{"slot":1}}`, `{"context":{"slot":1},"value":5}`, verify.Disagree, ""},

		{"getAccountInfo",
			`{"context":{"slot":1},"value":{"data":["AQ==","base64"],"lamports":10,"owner":"x","rentEpoch":18446744073709551615}}`,
			`{"context":{"slot":9},"value":{"owner":"x","rentEpoch":18446744073709551615,"lamports":10,"data":["AQ==","base64"]}}`,
			verify.Agree, ""},
		{"getAccountInfo",
			`{"context":{"slot":1},"value":{"data":["AQ==","base64"],"lamports":10}}`,
			`{"context":{"slot":1},"value":{"data":["Ag==","base64"],"lamports":10}}`,
			verify.Disagree, ""},
		{"getAccountInfo", `{"context":{"slot":1},"value":null}`, `{"context":{"slot":2},"value":null}`, verify.Agree, ""},

		{"getMultipleAccounts", `{"context":{"slot":1},"value":[null,{"lamports":1}]}`, `{"context":{"slot":2},"value":[null,{"lamports":1}]}`, verify.Agree, ""},

		// uiAmount formatting may differ; the integer amount must not.
		{"getTokenAccountBalance",
			`{"context":{"slot":1},"value":{"amount":"100000","decimals":6,"uiAmount":0.1,"uiAmountString":"0.1"}}`,
			`{"context":{"slot":2},"value":{"amount":"100000","decimals":6,"uiAmount":0.10000000000000001,"uiAmountString":"0.1"}}`,
			verify.Agree, ""},
		{"getTokenAccountBalance",
			`{"context":{"slot":1},"value":{"amount":"100000"}}`,
			`{"context":{"slot":1},"value":{"amount":"200000"}}`,
			verify.Disagree, ""},

		{"getBlockHeight", `105`, `100`, verify.Agree, `100`},
		{"getSlot", `100`, `105`, verify.Agree, `100`},
		{"getSlot", `100`, `"x"`, verify.Inconclusive, ""},
	}

	c := &Chain{}
	for _, tc := range cases {
		v := c.Verifier(tc.method, nil)
		if v == nil {
			t.Fatalf("%s: no verifier", tc.method)
		}

		got, outcome := v(json.RawMessage(tc.a), json.RawMessage(tc.b))
		if outcome != tc.want {
			t.Errorf("%s(%s, %s) = %s, want %s", tc.method, tc.a, tc.b, outcome, tc.want)
		}

		want := tc.result
		if want == "" {
			want = tc.a
		}
		if string(got) != want {
			t.Errorf("%s(%s, %s) returned %s, want %s", tc.method, tc.a, tc.b, got, want)
		}
	}
}

func TestVerifier_OnlyListedMethods(t *testing.T) {
	c := &Chain{}
	for _, method := range []string{"sendTransaction", "getSignaturesForAddress", "getProgramAccounts", "getLatestBlockhash"} {
		if c.Verifier(method, nil) != nil {
			t.Errorf("%s should not be cross-checked", method)
		}
	}
}

// TestVerifier_Processed: account reads at processed commitment aren't
// cross-checked, since each node's unvoted view is its own; heights still are.
func TestVerifier_Processed(t *testing.T) {
	cases := []struct {
		method, params string
		checked        bool
	}{
		{"getBalance", `["addr",{"commitment":"processed"}]`, false},
		{"getAccountInfo", `["addr",{"encoding":"base64","commitment":"processed"}]`, false},
		{"getMultipleAccounts", `[["a","b"],{"commitment":"processed"}]`, false},
		{"getBalance", `["addr",{"commitment":"confirmed"}]`, true},
		{"getBalance", `["addr"]`, true},
		{"getBlockHeight", `[{"commitment":"processed"}]`, true},
		{"getSlot", `[{"commitment":"processed"}]`, true},
	}
	c := &Chain{}
	for _, tc := range cases {
		if got := c.Verifier(tc.method, json.RawMessage(tc.params)) != nil; got != tc.checked {
			t.Errorf("%s %s: checked = %v, want %v", tc.method, tc.params, got, tc.checked)
		}
	}
}
