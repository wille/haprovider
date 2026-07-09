// Package cache provides a pluggable response-cache storage adapter for
// haprovider, with an in-memory default implementation. The Storage interface is
// the seam for alternative backends (e.g. Redis) later.
package cache

import (
	"context"
	"encoding/json"
	"time"
)

// Storage is a byte-value cache with per-entry TTL. Implementations must be safe
// for concurrent use.
type Storage interface {
	// Get returns the value for key and whether it was present (and unexpired).
	Get(ctx context.Context, key string) (value []byte, ok bool, err error)

	// Set stores value under key for the given ttl. A ttl <= 0 is a no-op.
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
}

// Key builds the cache key for a JSON-RPC request. The request id is excluded
// (it varies per client); only method and raw params identify the response.
// Caches are per-endpoint, so the endpoint/chain is not part of the key.
func Key(method string, params json.RawMessage) string {
	return method + "\x00" + string(params)
}
