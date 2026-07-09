package tron

import "testing"

// Tron delegates to the shared EVM policy.
func TestCacheable(t *testing.T) {
	c := &Chain{}
	for method, want := range map[string]bool{
		"eth_getBlockByHash":       true,
		"eth_getTransactionByHash": true,
		"eth_blockNumber":          false,
		"eth_sendRawTransaction":   false,
	} {
		if got := c.Cacheable(method, nil); got != want {
			t.Errorf("Cacheable(%q) = %v, want %v", method, got, want)
		}
	}
}
