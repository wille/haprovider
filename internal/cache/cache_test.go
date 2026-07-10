package cache

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestMemory_GetSet(t *testing.T) {
	m := NewMemory(0, 0)
	ctx := context.Background()

	if _, ok, _ := m.Get(ctx, "k"); ok {
		t.Fatal("expected miss on empty cache")
	}

	_ = m.Set(ctx, "k", []byte("v"), time.Minute)
	v, ok, _ := m.Get(ctx, "k")
	if !ok || string(v) != "v" {
		t.Fatalf("expected hit with v, got ok=%v v=%q", ok, v)
	}
}

func TestMemory_Expiry(t *testing.T) {
	m := NewMemory(0, 0)
	ctx := context.Background()

	_ = m.Set(ctx, "k", []byte("v"), 20*time.Millisecond)
	if _, ok, _ := m.Get(ctx, "k"); !ok {
		t.Fatal("expected hit before expiry")
	}
	time.Sleep(40 * time.Millisecond)
	if _, ok, _ := m.Get(ctx, "k"); ok {
		t.Fatal("expected miss after expiry")
	}
}

func TestMemory_NonPositiveTTLNoOp(t *testing.T) {
	m := NewMemory(0, 0)
	ctx := context.Background()
	_ = m.Set(ctx, "k", []byte("v"), 0)
	if _, ok, _ := m.Get(ctx, "k"); ok {
		t.Fatal("ttl<=0 must not store")
	}
}

func TestMemory_OversizedNotCached(t *testing.T) {
	m := NewMemory(0, 0)
	ctx := context.Background()
	_ = m.Set(ctx, "k", make([]byte, MaxValueBytes+1), time.Minute)
	if _, ok, _ := m.Get(ctx, "k"); ok {
		t.Fatal("values larger than MaxValueBytes must not be cached")
	}
	// A value at the limit is cached.
	_ = m.Set(ctx, "ok", make([]byte, MaxValueBytes), time.Minute)
	if _, ok, _ := m.Get(ctx, "ok"); !ok {
		t.Fatal("value at MaxValueBytes should be cached")
	}
}

func TestMemory_EvictByEntryCount(t *testing.T) {
	m := NewMemory(0, 3) // at most 3 entries
	ctx := context.Background()

	for i := range 5 {
		_ = m.Set(ctx, strconv.Itoa(i), []byte("v"), time.Minute)
	}
	if got := m.Len(); got != 3 {
		t.Fatalf("expected 3 entries, got %d", got)
	}
	// The two oldest (0,1) should be evicted; 2,3,4 remain.
	for _, k := range []string{"0", "1"} {
		if _, ok, _ := m.Get(ctx, k); ok {
			t.Errorf("key %s should have been evicted", k)
		}
	}
	for _, k := range []string{"2", "3", "4"} {
		if _, ok, _ := m.Get(ctx, k); !ok {
			t.Errorf("key %s should still be present", k)
		}
	}
}

func TestMemory_EvictByBytes(t *testing.T) {
	ctx := context.Background()
	// Budget fits ~3 of these ~1000-byte values.
	m := NewMemory(3300, 0)
	val := make([]byte, 1000)

	for i := range 5 {
		_ = m.Set(ctx, fmt.Sprintf("key-%d", i), val, time.Minute)
	}
	if m.Bytes() > 3300 {
		t.Fatalf("cache exceeded byte budget: %d > 3300", m.Bytes())
	}
	if m.Len() > 3 {
		t.Fatalf("expected at most 3 entries within budget, got %d", m.Len())
	}
}

func TestMemory_LRUOrdering(t *testing.T) {
	m := NewMemory(0, 2)
	ctx := context.Background()

	_ = m.Set(ctx, "a", []byte("1"), time.Minute)
	_ = m.Set(ctx, "b", []byte("1"), time.Minute)
	// Touch "a" so it becomes most-recently-used; adding "c" should evict "b".
	if _, ok, _ := m.Get(ctx, "a"); !ok {
		t.Fatal("a should be present")
	}
	_ = m.Set(ctx, "c", []byte("1"), time.Minute)

	if _, ok, _ := m.Get(ctx, "b"); ok {
		t.Error("b should have been evicted as least-recently-used")
	}
	if _, ok, _ := m.Get(ctx, "a"); !ok {
		t.Error("a was recently used and should survive")
	}
	if _, ok, _ := m.Get(ctx, "c"); !ok {
		t.Error("c was just added and should be present")
	}
}

func TestMemory_UpdateAdjustsBytes(t *testing.T) {
	m := NewMemory(0, 0)
	ctx := context.Background()

	_ = m.Set(ctx, "k", make([]byte, 1000), time.Minute)
	before := m.Bytes()
	_ = m.Set(ctx, "k", make([]byte, 10), time.Minute) // shrink same key
	after := m.Bytes()

	if m.Len() != 1 {
		t.Fatalf("updating an existing key must not add an entry, len=%d", m.Len())
	}
	if after >= before {
		t.Fatalf("byte accounting not adjusted on update: before=%d after=%d", before, after)
	}
}

func TestMemory_Concurrent(t *testing.T) {
	m := NewMemory(1024*1024, 0)
	ctx := context.Background()

	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := range 1000 {
				k := fmt.Sprintf("g%d-%d", g, i%50)
				_ = m.Set(ctx, k, []byte("value"), time.Minute)
				_, _, _ = m.Get(ctx, k)
			}
		}(g)
	}
	wg.Wait()

	if m.Bytes() > 1024*1024 {
		t.Fatalf("byte budget exceeded under concurrency: %d", m.Bytes())
	}
}
