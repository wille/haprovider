package btc

import (
	"encoding/json"
	"testing"
)

func TestCacheable(t *testing.T) {
	c := &Chain{}
	for method, want := range map[string]bool{
		"getblock":           true,
		"getblockheader":     true,
		"getblockhash":       true,
		"getrawtransaction":  false,
		"sendrawtransaction": false,
	} {
		if got := c.Cacheable(method, nil); got != want {
			t.Errorf("Cacheable(%q) = %v, want %v", method, got, want)
		}
	}
}

func TestCacheableResult(t *testing.T) {
	c := &Chain{}
	if c.CacheableResult("getblock", json.RawMessage(`null`)) {
		t.Error("null result must not be cacheable")
	}
	if !c.CacheableResult("getblock", json.RawMessage(`{"hash":"x"}`)) {
		t.Error("non-null result should be cacheable")
	}
}
