package solana

import (
	"encoding/json"
	"testing"
)

func TestCacheable(t *testing.T) {
	c := &Chain{}
	for method, want := range map[string]bool{
		"getBlock":        true,
		"getTransaction":  true,
		"getGenesisHash":  true,
		"getBalance":      false,
		"sendTransaction": false,
		"getBlockHeight":  false,
	} {
		if got := c.Cacheable(method, nil); got != want {
			t.Errorf("Cacheable(%q) = %v, want %v", method, got, want)
		}
	}
}

func TestCacheableResult(t *testing.T) {
	c := &Chain{}
	if c.CacheableResult("getBlock", json.RawMessage(`null`)) {
		t.Error("null result must not be cacheable")
	}
	if !c.CacheableResult("getBlock", json.RawMessage(`{"blockhash":"x"}`)) {
		t.Error("non-null result should be cacheable")
	}
}
