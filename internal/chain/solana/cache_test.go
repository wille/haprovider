package solana

import (
	"encoding/json"
	"testing"
)

func TestCacheableRequest(t *testing.T) {
	c := &Chain{}
	for method, want := range map[string]bool{
		"getBlock":        true,
		"getTransaction":  true,
		"getGenesisHash":  true,
		"getBalance":      false,
		"sendTransaction": false,
		"getBlockHeight":  false,
	} {
		if got := c.CacheableRequest(method, nil); got != want {
			t.Errorf("CacheableRequest(%q) = %v, want %v", method, got, want)
		}
	}
}

func TestCacheableResponse(t *testing.T) {
	c := &Chain{}
	if c.CacheableResponse("getBlock", json.RawMessage(`null`)) {
		t.Error("null result must not be cacheable")
	}
	if !c.CacheableResponse("getBlock", json.RawMessage(`{"blockhash":"x"}`)) {
		t.Error("non-null result should be cacheable")
	}
}
