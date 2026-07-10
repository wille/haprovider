package btc

import (
	"encoding/json"
	"testing"
)

func TestCacheableRequest(t *testing.T) {
	c := &Chain{}
	for method, want := range map[string]bool{
		"getblock":           true,
		"getblockheader":     true,
		"getblockhash":       true,
		"getrawtransaction":  false,
		"sendrawtransaction": false,
	} {
		if got := c.CacheableRequest(method, nil); got != want {
			t.Errorf("CacheableRequest(%q) = %v, want %v", method, got, want)
		}
	}
}

func TestCacheableResponse(t *testing.T) {
	c := &Chain{}
	if c.CacheableResponse("getblock", json.RawMessage(`null`)) {
		t.Error("null result must not be cacheable")
	}
	if !c.CacheableResponse("getblock", json.RawMessage(`{"hash":"x"}`)) {
		t.Error("non-null result should be cacheable")
	}
}
