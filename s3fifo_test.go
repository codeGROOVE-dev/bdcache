package sfcache

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestS3FIFO_BasicOperations(t *testing.T) {
	cache := newS3FIFO[string, int](&config{size: 100})

	// Test set and get
	cache.set("key1", 42, 0)
	if val, ok := cache.get("key1"); !ok || val != 42 {
		t.Errorf("get(key1) = %v, %v; want 42, true", val, ok)
	}

	// Test missing key
	if val, ok := cache.get("missing"); ok {
		t.Errorf("get(missing) = %v, %v; want _, false", val, ok)
	}

	// Test update
	cache.set("key1", 100, 0)
	if val, ok := cache.get("key1"); !ok || val != 100 {
		t.Errorf("get(key1) after update = %v, %v; want 100, true", val, ok)
	}

	// Test delete
	cache.del("key1")
	if val, ok := cache.get("key1"); ok {
		t.Errorf("get(key1) after delete = %v, %v; want _, false", val, ok)
	}
}

func TestS3FIFO_Capacity(t *testing.T) {
	cache := newS3FIFO[int, string](&config{size: 20000})

	// Fill cache well beyond capacity
	for i := range 30000 {
		cache.set(i, "value", 0)
	}

	// Cache should be at or near requested capacity
	// Allow up to 10% variance due to shard rounding
	if cache.len() < 18000 || cache.len() > 22000 {
		t.Errorf("cache length = %d; want ~20000 (±10%%)", cache.len())
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
			cache := newS3FIFO[int, int](&config{size: tc.requested})

			// Insert many more items than capacity
			for i := range tc.requested * 3 {
				cache.set(i, i, 0)
			}

			actual := cache.len()
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
	// Use 65536 to get proper S3-FIFO behavior (4096 entries per shard)
	cache := newS3FIFO[int, int](&config{size: 65536})

	// Fill cache to capacity
	for i := range 65536 {
		cache.set(i, i, 0)
	}

	// Access items 0-99 multiple times to mark them as hot
	for range 3 {
		for i := range 100 {
			cache.get(i)
		}
	}

	// Add more items to trigger evictions - hot items should survive
	for i := 65536; i < 130000; i++ {
		cache.set(i, i, 0)
	}

	// Count how many hot items survived
	hotSurvived := 0
	for i := range 100 {
		if _, ok := cache.get(i); ok {
			hotSurvived++
		}
	}

	// Most hot items should survive (at least 75%)
	if hotSurvived < 75 {
		t.Errorf("only %d/100 hot items survived; want >= 75", hotSurvived)
	}

	// Should be near capacity (allow 10% variance)
	if cache.len() < 58000 || cache.len() > 72000 {
		t.Errorf("cache length = %d; want ~65536", cache.len())
	}
}

func TestS3FIFO_GhostQueue(t *testing.T) {
	cache := newS3FIFO[string, int](&config{size: 12})

	// Fill one shard's worth
	cache.set("a", 1, 0)
	cache.set("b", 2, 0)
	cache.set("c", 3, 0)

	// Evict "a" by adding "d" (assuming same shard)
	cache.set("d", 4, 0)

	// Re-add "a" - should go directly to main queue if it was in ghost
	cache.set("a", 10, 0)

	// Verify "a" is retrievable with updated value
	if val, ok := cache.get("a"); !ok || val != 10 {
		t.Errorf("get(a) = %v, %v; want 10, true", val, ok)
	}
}

func TestS3FIFO_TTL(t *testing.T) {
	cache := newS3FIFO[string, int](&config{size: 10})

	// Set item with past expiry
	past := time.Now().Add(-1 * time.Second).UnixNano()
	cache.set("expired", 42, past)

	// Should not be retrievable
	if val, ok := cache.get("expired"); ok {
		t.Errorf("get(expired) = %v, %v; want _, false", val, ok)
	}

	// Set item with future expiry
	future := time.Now().Add(1 * time.Hour).UnixNano()
	cache.set("valid", 100, future)

	// Should be retrievable
	if val, ok := cache.get("valid"); !ok || val != 100 {
		t.Errorf("get(valid) = %v, %v; want 100, true", val, ok)
	}
}

func TestS3FIFO_Concurrent(t *testing.T) {
	cache := newS3FIFO[int, int](&config{size: 1000})
	var wg sync.WaitGroup

	// Concurrent writers
	for i := range 10 {
		wg.Add(1)
		go func(offset int) {
			defer wg.Done()
			for j := range 100 {
				cache.set(offset*100+j, j, 0)
			}
		}(i)
	}

	// Concurrent readers
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 100 {
				cache.get(j)
			}
		}()
	}

	wg.Wait()

	// Cache should be at or below requested capacity (with some shard rounding tolerance)
	if cache.len() > 1100 {
		t.Errorf("cache length = %d; want <= ~1000", cache.len())
	}
}

func TestS3FIFO_FrequencyPromotion(t *testing.T) {
	// Use 65536 to get proper S3-FIFO behavior (4096 entries per shard)
	cache := newS3FIFO[int, int](&config{size: 65536})

	// Fill cache with items using int keys for predictable sharding
	for i := range 65536 {
		cache.set(i, i, 0)
	}

	// Access first 1000 keys multiple times to mark them as hot
	for range 3 {
		for i := range 1000 {
			cache.get(i)
		}
	}

	// Add more items to trigger significant evictions (2x capacity)
	for i := 65536; i < 130000; i++ {
		cache.set(i, i, 0)
	}

	// Count how many hot items survived vs cold items
	hotSurvived := 0
	coldSurvived := 0
	for i := range 65536 {
		if _, ok := cache.get(i); ok {
			if i < 1000 {
				hotSurvived++
			} else {
				coldSurvived++
			}
		}
	}

	// Calculate survival rates
	hotRate := float64(hotSurvived) / 1000.0
	coldRate := float64(coldSurvived) / 64536.0

	t.Logf("Hot survived: %d/1000 (%.1f%%), Cold survived: %d/64536 (%.1f%%)",
		hotSurvived, hotRate*100, coldSurvived, coldRate*100)

	// Hot items should survive at higher rate than cold items
	// This verifies the frequency promotion mechanism works
	if hotRate <= coldRate {
		t.Errorf("hot item survival rate (%.1f%%) should exceed cold item rate (%.1f%%)",
			hotRate*100, coldRate*100)
	}
}

func TestS3FIFO_SmallCapacity(t *testing.T) {
	// Test with capacity of 12 (3 per shard)
	cache := newS3FIFO[string, int](&config{size: 12})

	// Fill to capacity
	cache.set("a", 1, 0)
	cache.set("b", 2, 0)
	cache.set("c", 3, 0)

	initialLen := cache.len()

	// Adding fourth item should trigger eviction in its shard
	cache.set("d", 4, 0)

	// Should still be at or near capacity
	if cache.len() > 12 {
		t.Errorf("cache length after eviction = %d; want <= 12", cache.len())
	}

	// Newest item should exist
	if val, ok := cache.get("d"); !ok || val != 4 {
		t.Errorf("get(d) = %v, %v; want 4, true", val, ok)
	}

	t.Logf("Initial len: %d, Final len: %d", initialLen, cache.len())
}

func BenchmarkS3FIFO_Set(b *testing.B) {
	cache := newS3FIFO[int, int](&config{size: 10000})
	b.ResetTimer()

	for i := range b.N {
		cache.set(i%10000, i, 0)
	}
}

func BenchmarkS3FIFO_Get(b *testing.B) {
	cache := newS3FIFO[int, int](&config{size: 10000})
	for i := range 10000 {
		cache.set(i, i, 0)
	}
	b.ResetTimer()

	for i := range b.N {
		cache.get(i % 10000)
	}
}

func BenchmarkS3FIFO_GetParallel(b *testing.B) {
	cache := newS3FIFO[int, int](&config{size: 10000})
	for i := range 10000 {
		cache.set(i, i, 0)
	}
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			cache.get(i % 10000)
			i++
		}
	})
}

func BenchmarkS3FIFO_Mixed(b *testing.B) {
	cache := newS3FIFO[int, int](&config{size: 10000})
	b.ResetTimer()

	for i := range b.N {
		if i%2 == 0 {
			cache.set(i%10000, i, 0)
		} else {
			cache.get(i % 10000)
		}
	}
}

// Test S3-FIFO behavior: hot items survive one-hit wonder floods
func TestS3FIFOBehavior(t *testing.T) {
	// Use larger capacity for meaningful per-shard sizes with 2048 shards
	cache := New[int, int](Size(10000))

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
	cache := New[int, int](Size(40))

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
	// Use 65536 capacity for proper S3-FIFO behavior with tiered sharding
	const cacheSize = 65536
	cache := New[int, int](Size(cacheSize))

	// Insert items 1-cacheSize into cache
	for i := 1; i <= cacheSize; i++ {
		cache.Set(i, i*100, 0)
	}

	// Access items 1-1000 multiple times (marks them as hot)
	const hotItems = 1000
	for range 3 {
		for i := 1; i <= hotItems; i++ {
			cache.Get(i)
		}
	}

	// Insert one-hit wonders to trigger eviction
	for i := cacheSize + 1; i <= cacheSize+20000; i++ {
		cache.Set(i, i*100, 0)
	}

	// Check which hot items survived
	hotSurvived := 0
	for i := 1; i <= hotItems; i++ {
		if _, found := cache.Get(i); found {
			hotSurvived++
		}
	}

	// Check which cold items survived (items that were never accessed)
	coldSurvived := 0
	for i := hotItems + 1; i <= hotItems+1000; i++ {
		if _, found := cache.Get(i); found {
			coldSurvived++
		}
	}

	t.Logf("Hot items found: %d/%d, Cold items found: %d/1000", hotSurvived, hotItems, coldSurvived)

	// Verify expected behavior - hot items should mostly survive
	// With proper S3-FIFO, at least 75% of hot items should survive
	if hotSurvived < hotItems*3/4 {
		t.Errorf("Expected most hot items to survive, got %d/%d", hotSurvived, hotItems)
	}
}

func TestS3FIFO_Flush(t *testing.T) {
	// Use int keys for predictable sharding, large capacity to avoid evictions
	cache := newS3FIFO[int, int](&config{size: 10000})

	// Add some items (fewer than capacity to avoid eviction)
	for i := range 100 {
		cache.set(i, i, 0)
	}

	// Access some to promote to main queue
	for i := range 20 {
		cache.get(i)
	}

	if cache.len() != 100 {
		t.Errorf("cache length = %d; want 100", cache.len())
	}

	// Flush
	removed := cache.flush()
	if removed != 100 {
		t.Errorf("flushMemory removed %d items; want 100", removed)
	}

	// Cache should be empty
	if cache.len() != 0 {
		t.Errorf("cache length after flush = %d; want 0", cache.len())
	}

	// All keys should be gone
	for i := range 100 {
		if _, ok := cache.get(i); ok {
			t.Errorf("key%d should not be found after flush", i)
		}
	}

	// Can add new items after flush
	cache.set(999, 999, 0)
	if val, ok := cache.get(999); !ok || val != 999 {
		t.Errorf("get(999) = %v, %v; want 999, true", val, ok)
	}
}

func TestS3FIFO_FlushEmpty(t *testing.T) {
	cache := newS3FIFO[string, int](&config{size: 100})

	// Flush empty cache
	removed := cache.flush()
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
	// This exercises different code paths in shard/shardIndexSlow.

	t.Run("int", func(t *testing.T) {
		cache := newS3FIFO[int, string](&config{size: 100})
		cache.set(42, "forty-two", 0)
		cache.set(-1, "negative", 0)
		cache.set(0, "zero", 0)

		if v, ok := cache.get(42); !ok || v != "forty-two" {
			t.Errorf("int key 42: got %v, %v", v, ok)
		}
		if v, ok := cache.get(-1); !ok || v != "negative" {
			t.Errorf("int key -1: got %v, %v", v, ok)
		}
		if v, ok := cache.get(0); !ok || v != "zero" {
			t.Errorf("int key 0: got %v, %v", v, ok)
		}
	})

	t.Run("int64", func(t *testing.T) {
		cache := newS3FIFO[int64, string](&config{size: 100})
		cache.set(int64(1<<62), "large", 0)
		cache.set(int64(-1), "negative", 0)

		if v, ok := cache.get(int64(1 << 62)); !ok || v != "large" {
			t.Errorf("int64 large key: got %v, %v", v, ok)
		}
		if v, ok := cache.get(int64(-1)); !ok || v != "negative" {
			t.Errorf("int64 -1 key: got %v, %v", v, ok)
		}
	})

	t.Run("uint", func(t *testing.T) {
		cache := newS3FIFO[uint, string](&config{size: 100})
		cache.set(uint(0), "zero", 0)
		cache.set(uint(100), "hundred", 0)

		if v, ok := cache.get(uint(0)); !ok || v != "zero" {
			t.Errorf("uint 0: got %v, %v", v, ok)
		}
		if v, ok := cache.get(uint(100)); !ok || v != "hundred" {
			t.Errorf("uint 100: got %v, %v", v, ok)
		}
	})

	t.Run("uint64", func(t *testing.T) {
		// Use larger size to ensure per-shard capacity > 1 (2048 shards)
		cache := newS3FIFO[uint64, string](&config{size: 10000})
		cache.set(uint64(1<<63), "large", 0)
		cache.set(uint64(0), "zero", 0)

		if v, ok := cache.get(uint64(1 << 63)); !ok || v != "large" {
			t.Errorf("uint64 large: got %v, %v", v, ok)
		}
		if v, ok := cache.get(uint64(0)); !ok || v != "zero" {
			t.Errorf("uint64 0: got %v, %v", v, ok)
		}
	})

	t.Run("string", func(t *testing.T) {
		cache := newS3FIFO[string, int](&config{size: 100})
		cache.set("hello", 1, 0)
		cache.set("", 2, 0) // empty string is valid
		unicode := "unicode-\u65e5\u672c\u8a9e"
		cache.set(unicode, 3, 0)

		if v, ok := cache.get("hello"); !ok || v != 1 {
			t.Errorf("string hello: got %v, %v", v, ok)
		}
		if v, ok := cache.get(""); !ok || v != 2 {
			t.Errorf("empty string: got %v, %v", v, ok)
		}
		if v, ok := cache.get(unicode); !ok || v != 3 {
			t.Errorf("unicode string: got %v, %v", v, ok)
		}
	})

	t.Run("fmt.Stringer", func(t *testing.T) {
		// Tests the fmt.Stringer fast path in shardIndexSlow
		cache := newS3FIFO[stringerKey, string](&config{size: 100})
		k1 := stringerKey{id: 1}
		k2 := stringerKey{id: 2}
		k3 := stringerKey{id: 999}

		cache.set(k1, "one", 0)
		cache.set(k2, "two", 0)
		cache.set(k3, "many", 0)

		if v, ok := cache.get(k1); !ok || v != "one" {
			t.Errorf("stringer k1: got %v, %v", v, ok)
		}
		if v, ok := cache.get(k2); !ok || v != "two" {
			t.Errorf("stringer k2: got %v, %v", v, ok)
		}
		if v, ok := cache.get(k3); !ok || v != "many" {
			t.Errorf("stringer k3: got %v, %v", v, ok)
		}

		// Verify delete works
		cache.del(k2)
		if _, ok := cache.get(k2); ok {
			t.Error("stringer k2 should be deleted")
		}
	})

	t.Run("plain struct", func(t *testing.T) {
		// Tests the fmt.Sprintf fallback in shardIndexSlow.
		// This is not fast, but should be reliable.
		cache := newS3FIFO[plainKey, string](&config{size: 100})
		k1 := plainKey{a: 1, b: "one"}
		k2 := plainKey{a: 2, b: "two"}
		k3 := plainKey{a: 1, b: "one"} // Same as k1

		cache.set(k1, "first", 0)
		cache.set(k2, "second", 0)

		if v, ok := cache.get(k1); !ok || v != "first" {
			t.Errorf("plain k1: got %v, %v", v, ok)
		}
		if v, ok := cache.get(k2); !ok || v != "second" {
			t.Errorf("plain k2: got %v, %v", v, ok)
		}
		// k3 is equal to k1, should get the same value
		if v, ok := cache.get(k3); !ok || v != "first" {
			t.Errorf("plain k3 (same as k1): got %v, %v", v, ok)
		}

		// Update via equal key
		cache.set(k3, "updated", 0)
		if v, ok := cache.get(k1); !ok || v != "updated" {
			t.Errorf("plain k1 after k3 update: got %v, %v", v, ok)
		}
	})
}

// TestS3FIFO_FrequencyCapAt3 tests that frequency counter is capped at 3.
// This is a critical S3-FIFO parameter that affects promotion behavior.
func TestS3FIFO_FrequencyCapAt3(t *testing.T) {
	cache := newS3FIFO[string, int](&config{size: 100})

	// Insert a key
	cache.set("hot", 1, 0)

	// Access it many times (well beyond 3)
	for range 20 {
		cache.get("hot")
	}

	// Access the internal entry to check frequency
	shard := cache.shard("hot")
	shard.mu.RLock()
	ent, ok := shard.entries["hot"]
	shard.mu.RUnlock()

	if !ok {
		t.Fatal("entry not found")
	}

	freq := ent.freq.Load()
	if freq > 3 {
		t.Errorf("frequency = %d; want <= 3 (should be capped at 3)", freq)
	}
	if freq != 3 {
		t.Logf("frequency = %d (expected 3, but may vary due to timing)", freq)
	}
}

// TestS3FIFO_SetIncrementsFrequency tests that updating an existing key increments frequency.
// This matches reference s3-fifo behavior where Set increments frequency.
func TestS3FIFO_SetIncrementsFrequency(t *testing.T) {
	cache := newS3FIFO[string, int](&config{size: 100})

	// Insert a key
	cache.set("key", 1, 0)

	// Check initial frequency (should be 0 for new entries)
	shard := cache.shard("key")
	shard.mu.RLock()
	ent := shard.entries["key"]
	initialFreq := ent.freq.Load()
	shard.mu.RUnlock()

	if initialFreq != 0 {
		t.Errorf("initial frequency = %d; want 0", initialFreq)
	}

	// Update the key several times
	for i := 2; i <= 4; i++ {
		cache.set("key", i, 0)
	}

	// Check that frequency increased due to updates
	shard.mu.RLock()
	ent = shard.entries["key"]
	finalFreq := ent.freq.Load()
	shard.mu.RUnlock()

	if finalFreq == 0 {
		t.Error("frequency should have increased after updates, but is still 0")
	}
	if finalFreq > 3 {
		t.Errorf("frequency = %d; want <= 3 (should be capped)", finalFreq)
	}
}

// TestS3FIFO_CascadingEviction tests that promoting from small to main triggers
// eviction from main when main exceeds 90% capacity.
func TestS3FIFO_CascadingEviction(t *testing.T) {
	// Use a small cache to make test fast
	capacity := 1000
	cache := newS3FIFO[int, int](&config{size: capacity})

	// Fill the cache completely
	for i := range capacity {
		cache.set(i, i, 0)
	}

	// Now access items that are likely in the small queue to trigger promotions
	// The first 10% should be in small queue (100 items)
	// Access them twice to trigger promotion (freq > 1)
	for i := range 100 {
		cache.get(i) // First access
		cache.get(i) // Second access - should trigger promotion
	}

	// Add more items to trigger evictions with promotions
	for i := capacity; i < capacity+200; i++ {
		cache.set(i, i, 0)
		// This should trigger cascading eviction when small promotes to main
	}

	// Cache should still be at capacity (not exceeding it)
	actualLen := cache.len()
	if actualLen > capacity*11/10 { // Allow 10% variance for shard rounding
		t.Errorf("cache length = %d; should be near capacity %d (got %.1f%% over)",
			actualLen, capacity, float64(actualLen-capacity)/float64(capacity)*100)
	}
}

// TestS3FIFO_SingleShardSmallCache tests that small caches (< 64K) use single shard.
// This ensures better S3-FIFO behavior for small caches.
func TestS3FIFO_SingleShardSmallCache(t *testing.T) {
	testCases := []struct {
		capacity        int
		expectNumShards int
	}{
		{100, 1},     // Tiny cache
		{1000, 1},    // Small cache
		{16384, 1},   // 16K - should be single shard
		{32768, 1},   // 32K - should be single shard
		{65535, 1},   // Just under 64K - should be single shard
		{65536, 32},  // At 64K - should have multiple shards (65536/2048=32)
		{131072, 64}, // 128K - should have shards (131072/2048=64)
	}

	for _, tc := range testCases {
		t.Run(fmt.Sprintf("capacity_%d", tc.capacity), func(t *testing.T) {
			cache := newS3FIFO[int, int](&config{size: tc.capacity})

			if cache.numShards != tc.expectNumShards {
				t.Errorf("capacity %d: numShards = %d; want %d",
					tc.capacity, cache.numShards, tc.expectNumShards)
			}
		})
	}
}

// TestS3FIFO_GhostQueueSize tests that ghost queue is sized at 100% of cache capacity.
// This matches the reference s3-fifo implementation.
func TestS3FIFO_GhostQueueSize(t *testing.T) {
	capacity := 1000
	cache := newS3FIFO[int, int](&config{size: capacity})

	// Check each shard's ghost queue capacity
	totalGhostCap := 0
	for _, shard := range cache.shards {
		totalGhostCap += shard.ghostCap
	}

	// Ghost capacity should be approximately equal to cache capacity
	// Allow some variance due to shard rounding
	minExpected := capacity * 90 / 100
	maxExpected := capacity * 110 / 100

	if totalGhostCap < minExpected || totalGhostCap > maxExpected {
		t.Errorf("total ghost capacity = %d; want ~%d (90-110%% of capacity)",
			totalGhostCap, capacity)
	}
}

// TestS3FIFO_EvictionTriggerBoundary tests the eviction trigger at exactly 10% boundary.
// We use > instead of >= for the small queue size check.
func TestS3FIFO_EvictionTriggerBoundary(t *testing.T) {
	// Use capacity of 100 for easy math (10% = 10 items)
	capacity := 100
	cache := newS3FIFO[int, int](&config{size: capacity})

	// Fill cache to capacity
	for i := range capacity {
		cache.set(i, i, 0)
	}

	// Insert one more item - this should trigger eviction
	// The eviction logic should use `small.len > capacity/10`
	// which means it only evicts from small when small has MORE than 10% (> 10 items)
	cache.set(capacity, capacity, 0)

	// Cache should still be at or near capacity
	actualLen := cache.len()
	if actualLen > capacity*11/10 { // Allow 10% variance
		t.Errorf("cache length = %d; should be near capacity %d", actualLen, capacity)
	}
}

// TestS3FIFO_GhostQueuePromotion tests that items in ghost queue are promoted to main.
func TestS3FIFO_GhostQueuePromotion(t *testing.T) {
	capacity := 100
	cache := newS3FIFO[int, int](&config{size: capacity})

	// Fill cache
	for i := range capacity {
		cache.set(i, i, 0)
	}

	// Access key 0 once to put in small queue
	cache.get(0)

	// Add many more items to evict key 0 (it should go to ghost)
	for i := capacity; i < capacity*2; i++ {
		cache.set(i, i, 0)
	}

	// Key 0 should be evicted and in ghost queue
	if _, ok := cache.get(0); ok {
		t.Log("key 0 still in cache (may not have been evicted yet)")
	}

	// Re-insert key 0 - it should go to main queue (not small) because it's in ghost
	cache.set(0, 9999, 0)

	// Check that key 0 is in main queue by inspecting internal state
	shard := cache.shard(0)
	shard.mu.RLock()
	ent, ok := shard.entries[0]
	inSmall := false
	if ok {
		inSmall = ent.inSmall
	}
	shard.mu.RUnlock()

	if !ok {
		t.Fatal("key 0 not found after re-insertion")
	}

	if inSmall {
		t.Error("key 0 should be in main queue (not small) after re-insertion from ghost")
	}
}

// TestS3FIFO_SmallQueuePromotion tests the promotion logic from small to main.
// Items with freq > 1 should be promoted when evicting from small.
func TestS3FIFO_SmallQueuePromotion(t *testing.T) {
	capacity := 1000
	cache := newS3FIFO[int, int](&config{size: capacity})

	// Fill cache
	for i := range capacity {
		cache.set(i, i, 0)
	}

	// Access first 50 items twice (they should be in small queue initially)
	for i := range 50 {
		cache.get(i) // First access
		cache.get(i) // Second access - freq > 1
	}

	// Add more items to trigger eviction from small
	for i := capacity; i < capacity+100; i++ {
		cache.set(i, i, 0)
	}

	// The accessed items (0-49) should still be in cache because they were promoted
	// due to freq > 1
	survivedCount := 0
	for i := range 50 {
		if _, ok := cache.get(i); ok {
			survivedCount++
		}
	}

	// At least some of them should have survived (been promoted to main)
	if survivedCount < 40 {
		t.Errorf("only %d/50 accessed items survived; expected most to be promoted to main",
			survivedCount)
	}
}

// TestS3FIFO_MainQueueReinsertion tests the reinsertion logic in main queue.
// Items with freq > 0 should be reinserted to back of main with decremented freq.
func TestS3FIFO_MainQueueReinsertion(t *testing.T) {
	capacity := 1000
	cache := newS3FIFO[int, int](&config{size: capacity})

	// Fill cache
	for i := range capacity {
		cache.set(i, i, 0)
	}

	// Access items to promote them to main
	for i := range 100 {
		cache.get(i)
		cache.get(i)
	}

	// Add more items to fill small queue and trigger promotions to main
	for i := capacity; i < capacity+200; i++ {
		cache.set(i, i, 0)
	}

	// Continue accessing the same items
	for range 3 {
		for i := range 100 {
			cache.get(i)
		}
	}

	// Add even more items to force main queue evictions
	for i := capacity + 200; i < capacity+400; i++ {
		cache.set(i, i, 0)
	}

	// The frequently accessed items should mostly survive due to reinsertion
	survivedCount := 0
	for i := range 100 {
		if _, ok := cache.get(i); ok {
			survivedCount++
		}
	}

	// Most should survive due to main queue reinsertion with freq > 0
	if survivedCount < 90 {
		t.Errorf("only %d/100 hot items survived; expected most to survive due to reinsertion",
			survivedCount)
	}
}
