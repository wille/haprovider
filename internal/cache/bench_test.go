package cache

import (
	"context"
	"strconv"
	"testing"
	"time"
)

// BenchmarkMemoryGet measures a single-threaded cache hit (the per-request hot
// path: lookup + LRU move-to-front under the mutex).
func BenchmarkMemoryGet(b *testing.B) {
	m := NewMemory(0, 0)
	ctx := context.Background()
	_ = m.Set(ctx, "key", make([]byte, 512), time.Hour)

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_, _, _ = m.Get(ctx, "key")
	}
}

// BenchmarkMemoryGetParallel measures cache-hit throughput under contention: Get
// takes the full mutex (LRU reordering is a write), so this is the ceiling for
// concurrent cache reads on one endpoint.
func BenchmarkMemoryGetParallel(b *testing.B) {
	m := NewMemory(0, 0)
	ctx := context.Background()
	const keys = 1000
	for i := range keys {
		_ = m.Set(ctx, "key"+strconv.Itoa(i), make([]byte, 512), time.Hour)
	}

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			_, _, _ = m.Get(ctx, "key"+strconv.Itoa(i%keys))
			i++
		}
	})
}

// BenchmarkMemorySet measures inserts into a byte-bounded cache, exercising the
// eviction path once the budget is reached.
func BenchmarkMemorySet(b *testing.B) {
	m := NewMemory(16*1024*1024, 0)
	ctx := context.Background()
	val := make([]byte, 512)

	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		_ = m.Set(ctx, "key"+strconv.Itoa(i), val, time.Hour)
	}
}
