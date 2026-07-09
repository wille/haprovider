package tron

import "testing"

// Tron delegates to the shared EVM policy.
func TestCacheableRequest(t *testing.T) {
	c := &Chain{}
	for method, want := range map[string]bool{
		"eth_getBlockByHash":       true,
		"eth_getTransactionByHash": true,
		"eth_blockNumber":          false,
		"eth_sendRawTransaction":   false,
	} {
		if got := c.CacheableRequest(method, nil); got != want {
			t.Errorf("CacheableRequest(%q) = %v, want %v", method, got, want)
		}
	}
}
