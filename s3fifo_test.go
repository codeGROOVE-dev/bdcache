package bdcache

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestS3FIFO_BasicOperations(t *testing.T) {
	cache := newS3FIFO[string, int](100)

	// Test set and get
	cache.setToMemory("key1", 42, 0)
	if val, ok := cache.getFromMemory("key1"); !ok || val != 42 {
		t.Errorf("get(key1) = %v, %v; want 42, true", val, ok)
	}

	// Test missing key
	if val, ok := cache.getFromMemory("missing"); ok {
		t.Errorf("get(missing) = %v, %v; want _, false", val, ok)
	}

	// Test update
	cache.setToMemory("key1", 100, 0)
	if val, ok := cache.getFromMemory("key1"); !ok || val != 100 {
		t.Errorf("get(key1) after update = %v, %v; want 100, true", val, ok)
	}

	// Test delete
	cache.deleteFromMemory("key1")
	if val, ok := cache.getFromMemory("key1"); ok {
		t.Errorf("get(key1) after delete = %v, %v; want _, false", val, ok)
	}
}

func TestS3FIFO_Capacity(t *testing.T) {
	cache := newS3FIFO[int, string](20000)

	// Fill cache well beyond capacity
	for i := range 30000 {
		cache.setToMemory(i, "value", 0)
	}

	// Cache should be at or near requested capacity
	// Allow up to 10% variance due to shard rounding
	if cache.memoryLen() < 18000 || cache.memoryLen() > 22000 {
		t.Errorf("cache length = %d; want ~20000 (±10%%)", cache.memoryLen())
	}
}

// TestS3FIFO_CapacityAccuracy verifies that cache capacity is accurate across sizes.
// This is a regression test for the bug where small caches were inflated to numShards.
func TestS3FIFO_CapacityAccuracy(t *testing.T) {
	testCases := []struct {
		requested int
		maxActual int // Allow some overhead for shard rounding
	}{
		{100, 128},       // Small cache
		{500, 512},       // Very small cache (was inflated to 2048 before fix)
		{1000, 1024},     // Medium-small cache
		{10000, 10240},   // Medium cache
		{100000, 102400}, // Large cache
	}

	for _, tc := range testCases {
		t.Run(fmt.Sprintf("capacity_%d", tc.requested), func(t *testing.T) {
			cache := newS3FIFO[int, int](tc.requested)

			// Insert many more items than capacity
			for i := range tc.requested * 3 {
				cache.setToMemory(i, i, 0)
			}

			actual := cache.memoryLen()
			if actual > tc.maxActual {
				t.Errorf("requested %d, got %d items (max expected %d)",
					tc.requested, actual, tc.maxActual)
			}
			// Should be at least 80% of requested to ensure we're not under-sizing
			minExpected := tc.requested * 80 / 100
			if actual < minExpected {
				t.Errorf("requested %d, got only %d items (min expected %d)",
					tc.requested, actual, minExpected)
			}
		})
	}
}

func TestS3FIFO_Eviction(t *testing.T) {
	cache := newS3FIFO[int, int](10000)

	// Fill cache to capacity
	for i := range 10000 {
		cache.setToMemory(i, i, 0)
	}

	// Access item 0 to mark it for promotion
	cache.getFromMemory(0)

	// Add more items to trigger evictions - item 0 should survive
	for i := 10000; i < 15000; i++ {
		cache.setToMemory(i, i, 0)
	}

	// Item 0 should still exist (it was accessed before evictions)
	if _, ok := cache.getFromMemory(0); !ok {
		t.Error("item 0 was evicted but should have been promoted")
	}

	// Should be near capacity (allow 10% variance)
	if cache.memoryLen() < 9000 || cache.memoryLen() > 11000 {
		t.Errorf("cache length = %d; want ~10000", cache.memoryLen())
	}
}

func TestS3FIFO_GhostQueue(t *testing.T) {
	cache := newS3FIFO[string, int](12) // 3 per shard

	// Fill one shard's worth
	cache.setToMemory("a", 1, 0)
	cache.setToMemory("b", 2, 0)
	cache.setToMemory("c", 3, 0)

	// Evict "a" by adding "d" (assuming same shard)
	cache.setToMemory("d", 4, 0)

	// Re-add "a" - should go directly to main queue if it was in ghost
	cache.setToMemory("a", 10, 0)

	// Verify "a" is retrievable with updated value
	if val, ok := cache.getFromMemory("a"); !ok || val != 10 {
		t.Errorf("get(a) = %v, %v; want 10, true", val, ok)
	}
}

func TestS3FIFO_TTL(t *testing.T) {
	cache := newS3FIFO[string, int](10)

	// Set item with past expiry
	past := time.Now().Add(-1 * time.Second).UnixNano()
	cache.setToMemory("expired", 42, past)

	// Should not be retrievable
	if val, ok := cache.getFromMemory("expired"); ok {
		t.Errorf("get(expired) = %v, %v; want _, false", val, ok)
	}

	// Set item with future expiry
	future := time.Now().Add(1 * time.Hour).UnixNano()
	cache.setToMemory("valid", 100, future)

	// Should be retrievable
	if val, ok := cache.getFromMemory("valid"); !ok || val != 100 {
		t.Errorf("get(valid) = %v, %v; want 100, true", val, ok)
	}
}

func TestS3FIFO_Concurrent(t *testing.T) {
	cache := newS3FIFO[int, int](1000)
	var wg sync.WaitGroup

	// Concurrent writers
	for i := range 10 {
		wg.Add(1)
		go func(offset int) {
			defer wg.Done()
			for j := range 100 {
				cache.setToMemory(offset*100+j, j, 0)
			}
		}(i)
	}

	// Concurrent readers
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 100 {
				cache.getFromMemory(j)
			}
		}()
	}

	wg.Wait()

	// Cache should be at or below requested capacity (with some shard rounding tolerance)
	if cache.memoryLen() > 1100 {
		t.Errorf("cache length = %d; want <= ~1000", cache.memoryLen())
	}
}

func TestS3FIFO_FrequencyPromotion(t *testing.T) {
	// Use a larger capacity to ensure meaningful per-shard capacity
	// With 512 shards, 10000 items = ~20 per shard
	cache := newS3FIFO[int, int](10000)

	// Fill cache with items using int keys for predictable sharding
	for i := range 10000 {
		cache.setToMemory(i, i, 0)
	}

	// Access even-numbered keys to increase their frequency
	for i := 0; i < 10000; i += 2 {
		cache.getFromMemory(i)
	}

	// Add more items to trigger evictions
	for i := 10000; i < 15000; i++ {
		cache.setToMemory(i, i, 0)
	}

	// Count how many accessed items survived vs unaccessed
	accessedSurvived := 0
	unaccesedSurvived := 0
	for i := range 10000 {
		if _, ok := cache.getFromMemory(i); ok {
			if i%2 == 0 {
				accessedSurvived++
			} else {
				unaccesedSurvived++
			}
		}
	}

	// Accessed items should survive at higher rate than unaccessed
	// This verifies the frequency promotion mechanism works
	if accessedSurvived <= unaccesedSurvived {
		t.Errorf("accessed items (%d) should survive more than unaccessed (%d)",
			accessedSurvived, unaccesedSurvived)
	}
}

func TestS3FIFO_SmallCapacity(t *testing.T) {
	// Test with capacity of 12 (3 per shard)
	cache := newS3FIFO[string, int](12)

	// Fill to capacity
	cache.setToMemory("a", 1, 0)
	cache.setToMemory("b", 2, 0)
	cache.setToMemory("c", 3, 0)

	initialLen := cache.memoryLen()

	// Adding fourth item should trigger eviction in its shard
	cache.setToMemory("d", 4, 0)

	// Should still be at or near capacity
	if cache.memoryLen() > 12 {
		t.Errorf("cache length after eviction = %d; want <= 12", cache.memoryLen())
	}

	// Newest item should exist
	if val, ok := cache.getFromMemory("d"); !ok || val != 4 {
		t.Errorf("get(d) = %v, %v; want 4, true", val, ok)
	}

	t.Logf("Initial len: %d, Final len: %d", initialLen, cache.memoryLen())
}

func BenchmarkS3FIFO_Set(b *testing.B) {
	cache := newS3FIFO[int, int](10000)
	b.ResetTimer()

	for i := range b.N {
		cache.setToMemory(i%10000, i, 0)
	}
}

func BenchmarkS3FIFO_Get(b *testing.B) {
	cache := newS3FIFO[int, int](10000)
	for i := range 10000 {
		cache.setToMemory(i, i, 0)
	}
	b.ResetTimer()

	for i := range b.N {
		cache.getFromMemory(i % 10000)
	}
}

func BenchmarkS3FIFO_GetParallel(b *testing.B) {
	cache := newS3FIFO[int, int](10000)
	for i := range 10000 {
		cache.setToMemory(i, i, 0)
	}
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			cache.getFromMemory(i % 10000)
			i++
		}
	})
}

func BenchmarkS3FIFO_Mixed(b *testing.B) {
	cache := newS3FIFO[int, int](10000)
	b.ResetTimer()

	for i := range b.N {
		if i%2 == 0 {
			cache.setToMemory(i%10000, i, 0)
		} else {
			cache.getFromMemory(i % 10000)
		}
	}
}

// Test S3-FIFO behavior: hot items survive one-hit wonder floods
func TestS3FIFOBehavior(t *testing.T) {
	// Use larger capacity for meaningful per-shard sizes with 2048 shards
	cache := Memory[int, int](WithSize(10000))

	// Insert hot items that will be accessed multiple times
	for i := range 5000 {
		cache.Set(i, i, 0)
	}

	// Access hot items once (marks them for promotion)
	for i := range 5000 {
		cache.Get(i)
	}

	// Insert one-hit wonders (should be evicted before hot items)
	for i := 20000; i < 26000; i++ {
		cache.Set(i, i, 0)
	}

	// Check if hot items survived
	hotItemsFound := 0
	for i := range 5000 {
		if _, found := cache.Get(i); found {
			hotItemsFound++
		}
	}

	// Hot items should mostly survive - S3-FIFO protects frequently accessed items
	if hotItemsFound < 4000 {
		t.Errorf("Expected most hot items to survive, got %d/5000", hotItemsFound)
	}
}

// Test eviction order: accessed items survive new insertions
func TestS3FIFOEvictionOrder(t *testing.T) {
	cache := Memory[int, int](WithSize(40))

	// Fill cache with items
	for i := range 40 {
		cache.Set(i, i, 0)
	}

	// Access first 20 items (marks them for promotion)
	for i := range 20 {
		cache.Get(i)
	}

	// Insert new items (should evict unaccessed items first)
	for i := 100; i < 120; i++ {
		cache.Set(i, i, 0)
	}

	// Verify accessed items survived
	accessedFound := 0
	for i := range 20 {
		if _, found := cache.Get(i); found {
			accessedFound++
		}
	}
	t.Logf("Accessed items found: %d/20", accessedFound)
}

// Test S3-FIFO vs LRU: hot items survive, cold items evicted
func TestS3FIFODetailed(t *testing.T) {
	cache := Memory[int, int](WithSize(40))

	// Insert items 1-40 into cache
	for i := 1; i <= 40; i++ {
		cache.Set(i, i*100, 0)
	}

	// Access items 1-20 (marks them as hot)
	for i := 1; i <= 20; i++ {
		cache.Get(i)
	}

	// Insert one-hit wonders 100-119
	for i := 100; i < 120; i++ {
		cache.Set(i, i*100, 0)
	}

	// Check which items survived
	hotSurvived := 0
	for i := 1; i <= 20; i++ {
		if _, found := cache.Get(i); found {
			hotSurvived++
		}
	}

	coldSurvived := 0
	for i := 21; i <= 40; i++ {
		if _, found := cache.Get(i); found {
			coldSurvived++
		}
	}

	t.Logf("Hot items found: %d/20, Cold items found: %d/20", hotSurvived, coldSurvived)

	// Verify expected behavior - hot items should mostly survive
	if hotSurvived < 15 {
		t.Errorf("Expected most hot items to survive, got %d/20", hotSurvived)
	}
}

func TestS3FIFO_Flush(t *testing.T) {
	// Use int keys for predictable sharding, large capacity to avoid evictions
	cache := newS3FIFO[int, int](10000)

	// Add some items (fewer than capacity to avoid eviction)
	for i := range 100 {
		cache.setToMemory(i, i, 0)
	}

	// Access some to promote to main queue
	for i := range 20 {
		cache.getFromMemory(i)
	}

	if cache.memoryLen() != 100 {
		t.Errorf("cache length = %d; want 100", cache.memoryLen())
	}

	// Flush
	removed := cache.flushMemory()
	if removed != 100 {
		t.Errorf("flushMemory removed %d items; want 100", removed)
	}

	// Cache should be empty
	if cache.memoryLen() != 0 {
		t.Errorf("cache length after flush = %d; want 0", cache.memoryLen())
	}

	// All keys should be gone
	for i := range 100 {
		if _, ok := cache.getFromMemory(i); ok {
			t.Errorf("key%d should not be found after flush", i)
		}
	}

	// Can add new items after flush
	cache.setToMemory(999, 999, 0)
	if val, ok := cache.getFromMemory(999); !ok || val != 999 {
		t.Errorf("get(999) = %v, %v; want 999, true", val, ok)
	}
}

func TestS3FIFO_FlushEmpty(t *testing.T) {
	cache := newS3FIFO[string, int](100)

	// Flush empty cache
	removed := cache.flushMemory()
	if removed != 0 {
		t.Errorf("flushMemory removed %d items; want 0", removed)
	}
}

// stringerKey implements fmt.Stringer for testing the Stringer fast path.
type stringerKey struct {
	id int
}

func (k stringerKey) String() string {
	return fmt.Sprintf("stringer-%d", k.id)
}

// plainKey is a struct without String() method for testing the fallback path.
type plainKey struct {
	a int
	b string
}

//nolint:gocognit // Test function intentionally exercises many code paths via subtests
func TestS3FIFO_VariousKeyTypes(t *testing.T) {
	// Test that various key types work correctly with the sharding logic.
	// This exercises different code paths in getShard/shardIndexSlow.

	t.Run("int", func(t *testing.T) {
		cache := newS3FIFO[int, string](100)
		cache.setToMemory(42, "forty-two", 0)
		cache.setToMemory(-1, "negative", 0)
		cache.setToMemory(0, "zero", 0)

		if v, ok := cache.getFromMemory(42); !ok || v != "forty-two" {
			t.Errorf("int key 42: got %v, %v", v, ok)
		}
		if v, ok := cache.getFromMemory(-1); !ok || v != "negative" {
			t.Errorf("int key -1: got %v, %v", v, ok)
		}
		if v, ok := cache.getFromMemory(0); !ok || v != "zero" {
			t.Errorf("int key 0: got %v, %v", v, ok)
		}
	})

	t.Run("int64", func(t *testing.T) {
		cache := newS3FIFO[int64, string](100)
		cache.setToMemory(int64(1<<62), "large", 0)
		cache.setToMemory(int64(-1), "negative", 0)

		if v, ok := cache.getFromMemory(int64(1 << 62)); !ok || v != "large" {
			t.Errorf("int64 large key: got %v, %v", v, ok)
		}
		if v, ok := cache.getFromMemory(int64(-1)); !ok || v != "negative" {
			t.Errorf("int64 -1 key: got %v, %v", v, ok)
		}
	})

	t.Run("uint", func(t *testing.T) {
		cache := newS3FIFO[uint, string](100)
		cache.setToMemory(uint(0), "zero", 0)
		cache.setToMemory(uint(100), "hundred", 0)

		if v, ok := cache.getFromMemory(uint(0)); !ok || v != "zero" {
			t.Errorf("uint 0: got %v, %v", v, ok)
		}
		if v, ok := cache.getFromMemory(uint(100)); !ok || v != "hundred" {
			t.Errorf("uint 100: got %v, %v", v, ok)
		}
	})

	t.Run("uint64", func(t *testing.T) {
		// Use larger size to ensure per-shard capacity > 1 (2048 shards)
		cache := newS3FIFO[uint64, string](10000)
		cache.setToMemory(uint64(1<<63), "large", 0)
		cache.setToMemory(uint64(0), "zero", 0)

		if v, ok := cache.getFromMemory(uint64(1 << 63)); !ok || v != "large" {
			t.Errorf("uint64 large: got %v, %v", v, ok)
		}
		if v, ok := cache.getFromMemory(uint64(0)); !ok || v != "zero" {
			t.Errorf("uint64 0: got %v, %v", v, ok)
		}
	})

	t.Run("string", func(t *testing.T) {
		cache := newS3FIFO[string, int](100)
		cache.setToMemory("hello", 1, 0)
		cache.setToMemory("", 2, 0) // empty string is valid
		unicode := "unicode-\u65e5\u672c\u8a9e"
		cache.setToMemory(unicode, 3, 0)

		if v, ok := cache.getFromMemory("hello"); !ok || v != 1 {
			t.Errorf("string hello: got %v, %v", v, ok)
		}
		if v, ok := cache.getFromMemory(""); !ok || v != 2 {
			t.Errorf("empty string: got %v, %v", v, ok)
		}
		if v, ok := cache.getFromMemory(unicode); !ok || v != 3 {
			t.Errorf("unicode string: got %v, %v", v, ok)
		}
	})

	t.Run("fmt.Stringer", func(t *testing.T) {
		// Tests the fmt.Stringer fast path in shardIndexSlow
		cache := newS3FIFO[stringerKey, string](100)
		k1 := stringerKey{id: 1}
		k2 := stringerKey{id: 2}
		k3 := stringerKey{id: 999}

		cache.setToMemory(k1, "one", 0)
		cache.setToMemory(k2, "two", 0)
		cache.setToMemory(k3, "many", 0)

		if v, ok := cache.getFromMemory(k1); !ok || v != "one" {
			t.Errorf("stringer k1: got %v, %v", v, ok)
		}
		if v, ok := cache.getFromMemory(k2); !ok || v != "two" {
			t.Errorf("stringer k2: got %v, %v", v, ok)
		}
		if v, ok := cache.getFromMemory(k3); !ok || v != "many" {
			t.Errorf("stringer k3: got %v, %v", v, ok)
		}

		// Verify delete works
		cache.deleteFromMemory(k2)
		if _, ok := cache.getFromMemory(k2); ok {
			t.Error("stringer k2 should be deleted")
		}
	})

	t.Run("plain struct", func(t *testing.T) {
		// Tests the fmt.Sprintf fallback in shardIndexSlow.
		// This is not fast, but should be reliable.
		cache := newS3FIFO[plainKey, string](100)
		k1 := plainKey{a: 1, b: "one"}
		k2 := plainKey{a: 2, b: "two"}
		k3 := plainKey{a: 1, b: "one"} // Same as k1

		cache.setToMemory(k1, "first", 0)
		cache.setToMemory(k2, "second", 0)

		if v, ok := cache.getFromMemory(k1); !ok || v != "first" {
			t.Errorf("plain k1: got %v, %v", v, ok)
		}
		if v, ok := cache.getFromMemory(k2); !ok || v != "second" {
			t.Errorf("plain k2: got %v, %v", v, ok)
		}
		// k3 is equal to k1, should get the same value
		if v, ok := cache.getFromMemory(k3); !ok || v != "first" {
			t.Errorf("plain k3 (same as k1): got %v, %v", v, ok)
		}

		// Update via equal key
		cache.setToMemory(k3, "updated", 0)
		if v, ok := cache.getFromMemory(k1); !ok || v != "updated" {
			t.Errorf("plain k1 after k3 update: got %v, %v", v, ok)
		}
	})
}
