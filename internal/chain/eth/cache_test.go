package eth

import (
	"encoding/json"
	"testing"
)

func TestCacheableRequest(t *testing.T) {
	cases := []struct {
		method string
		params string
		want   bool
	}{
		{"eth_chainId", `[]`, true},
		{"eth_getBlockByHash", `["0xhash",false]`, true},
		{"eth_getBlockByNumber", `["0x1234",false]`, true},
		{"eth_getTransactionByHash", `["0xtx"]`, true},
		{"eth_getTransactionReceipt", `["0xtx"]`, true},
		// not cacheable: mutable / state-dependent methods
		{"eth_blockNumber", `[]`, false},
		{"eth_getBalance", `["0xaddr","0x1"]`, false},
		{"eth_getTransactionCount", `["0xaddr","0x1"]`, false},
		{"eth_call", `[{}]`, false},
		{"eth_sendRawTransaction", `["0xsigned"]`, false},
		// allowlisted method but a mutable block tag in params: not cacheable
		{"eth_getBlockByNumber", `["latest",false]`, false},
		{"eth_getBlockByNumber", `["pending",false]`, false},
	}
	c := &Ethereum{}
	for _, tc := range cases {
		if got := c.CacheableRequest(tc.method, json.RawMessage(tc.params)); got != tc.want {
			t.Errorf("CacheableRequest(%q, %s) = %v, want %v", tc.method, tc.params, got, tc.want)
		}
	}
}

func TestCacheableResponse(t *testing.T) {
	c := &Ethereum{}

	// null / empty results are never cached.
	if c.CacheableResponse("eth_getTransactionReceipt", json.RawMessage(`null`)) {
		t.Error("null result must not be cacheable")
	}
	if c.CacheableResponse("eth_getBlockByHash", json.RawMessage(``)) {
		t.Error("empty result must not be cacheable")
	}

	// A non-null receipt (implies mined) is cacheable.
	if !c.CacheableResponse("eth_getTransactionReceipt", json.RawMessage(`{"status":"0x1"}`)) {
		t.Error("non-null receipt should be cacheable")
	}

	// eth_getTransactionByHash: pending (null blockNumber) not cacheable; confirmed is.
	pending := json.RawMessage(`{"hash":"0xabc","blockNumber":null}`)
	if c.CacheableResponse("eth_getTransactionByHash", pending) {
		t.Error("pending transaction (null blockNumber) must not be cacheable")
	}
	confirmed := json.RawMessage(`{"hash":"0xabc","blockNumber":"0x10"}`)
	if !c.CacheableResponse("eth_getTransactionByHash", confirmed) {
		t.Error("confirmed transaction should be cacheable")
	}
}
