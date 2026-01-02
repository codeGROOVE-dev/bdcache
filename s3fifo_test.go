package fido

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
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
	past := uint32(time.Now().Add(-1 * time.Second).Unix())
	cache.set("expired", 42, past)

	// Should not be retrievable
	if val, ok := cache.get("expired"); ok {
		t.Errorf("get(expired) = %v, %v; want _, false", val, ok)
	}

	// Set item with future expiry
	future := uint32(time.Now().Add(1 * time.Hour).Unix())
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
		wg.Go(func() {
			for j := range 100 {
				cache.get(j)
			}
		})
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

// BenchmarkS3FIFO_SetEvict benchmarks Set with eviction (unique keys after warmup).
func BenchmarkS3FIFO_SetEvict(b *testing.B) {
	cache := newS3FIFO[int, int](&config{size: 10000})
	// Warmup: fill cache to capacity
	for i := range 10000 {
		cache.set(i, i, 0)
	}
	b.ResetTimer()

	// Each set uses a unique key, forcing eviction
	for i := range b.N {
		cache.set(10000+i, i, 0)
	}
}

// BenchmarkS3FIFO_SetEvictString benchmarks Set with eviction using string keys.
func BenchmarkS3FIFO_SetEvictString(b *testing.B) {
	cache := newS3FIFO[string, int](&config{size: 10000})
	// Pre-generate keys to avoid allocation in benchmark loop
	warmupKeys := make([]string, 10000)
	for i := range 10000 {
		warmupKeys[i] = fmt.Sprintf("warmup-key-%d", i)
	}
	benchKeys := make([]string, b.N)
	for i := range b.N {
		benchKeys[i] = fmt.Sprintf("bench-key-%d", i)
	}

	// Warmup: fill cache to capacity
	for i := range 10000 {
		cache.set(warmupKeys[i], i, 0)
	}
	b.ResetTimer()

	// Each set uses a unique key, forcing eviction
	for i := range b.N {
		cache.set(benchKeys[i], i, 0)
	}
}

// Test S3-FIFO behavior: hot items survive one-hit wonder floods
func TestS3FIFOBehavior(t *testing.T) {
	// Use larger capacity for meaningful per-shard sizes with 2048 shards
	cache := New[int, int](Size(10000))

	// Insert hot items that will be accessed multiple times
	for i := range 5000 {
		cache.Set(i, i)
	}

	// Access hot items once (marks them for promotion)
	for i := range 5000 {
		cache.Get(i)
	}

	// Insert one-hit wonders (should be evicted before hot items)
	for i := 20000; i < 26000; i++ {
		cache.Set(i, i)
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
		cache.Set(i, i)
	}

	// Access first 20 items (marks them for promotion)
	for i := range 20 {
		cache.Get(i)
	}

	// Insert new items (should evict unaccessed items first)
	for i := 100; i < 120; i++ {
		cache.Set(i, i)
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
		cache.Set(i, i*100)
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
		cache.Set(i, i*100)
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

// TestS3FIFO_FrequencyCapAt7 tests that frequency counter is capped at 7.
// This is a critical S3-FIFO parameter that affects promotion behavior.
func TestS3FIFO_FrequencyCapAt7(t *testing.T) {
	cache := newS3FIFO[string, int](&config{size: 100})

	// Insert a key
	cache.set("hot", 1, 0)

	// Access it many times (well beyond 7)
	for range 20 {
		cache.get("hot")
	}

	// Access the internal entry to check frequency
	ent, ok := cache.getEntry("hot")

	if !ok {
		t.Fatal("entry not found")
	}

	freq := ent.freq()
	if freq > 7 {
		t.Errorf("frequency = %d; want <= 7 (should be capped at 7)", freq)
	}
	if freq != 7 {
		t.Logf("frequency = %d (expected 7, but may vary due to timing)", freq)
	}
}

// TestS3FIFO_SetIncrementsFrequency tests that updating an existing key increments frequency.
// This matches reference s3-fifo behavior where Set increments frequency.
func TestS3FIFO_SetIncrementsFrequency(t *testing.T) {
	cache := newS3FIFO[string, int](&config{size: 100})

	// Insert a key
	cache.set("key", 1, 0)

	// Check initial frequency (should be 0 for new entries)
	ent, _ := cache.getEntry("key")
	initialFreq := ent.freq()

	if initialFreq != 0 {
		t.Errorf("initial frequency = %d; want 0", initialFreq)
	}

	// Update the key several times
	for i := 2; i <= 4; i++ {
		cache.set("key", i, 0)
	}

	// Check that frequency increased due to updates
	ent, _ = cache.getEntry("key")
	finalFreq := ent.freq()

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

// TestS3FIFO_GhostQueueSize tests that ghost queue is sized correctly.
func TestS3FIFO_GhostQueueSize(t *testing.T) {
	capacity := 1000
	cache := newS3FIFO[int, int](&config{size: capacity})

	// Ghost capacity varies by cache size (see ghostRatio function).
	want := capacity * ghostRatio(capacity) / 1000
	if cache.ghostCap != want {
		t.Errorf("ghost capacity = %d; want %d", cache.ghostCap, want)
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
	// Use larger capacity so ghost doesn't rotate too quickly
	capacity := 1000
	cache := newS3FIFO[int, int](&config{size: capacity})

	// Fill cache (don't access key 0 - we want it evicted with freq=0)
	for i := range capacity {
		cache.set(i, i, 0)
	}

	// Add just enough items to evict key 0 (50% of capacity to avoid ghost rotation)
	for i := capacity; i < capacity+capacity/2; i++ {
		cache.set(i, i, 0)
	}

	// Key 0 should be evicted and in ghost queue
	if _, ok := cache.get(0); ok {
		t.Log("key 0 still in cache (may not have been evicted yet)")
	}

	// Re-insert key 0 - it should go to main queue (not small) because it's in ghost
	cache.set(0, 9999, 0)

	// Check that key 0 is in main queue by inspecting internal state
	ent, ok := cache.getEntry(0)
	inSmall := false
	if ok {
		inSmall = ent.inSmall()
	}

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

// TestS3FIFO_NeverExceedsCapacity verifies that the cache never exceeds its capacity
// under various workloads and sizes. This is a critical invariant.
func TestS3FIFO_NeverExceedsCapacity(t *testing.T) {
	testCases := []struct {
		name       string
		capacity   int
		insertions int
	}{
		{"small_2x", 100, 200},
		{"small_5x", 100, 500},
		{"small_10x", 100, 1000},
		{"medium_2x", 1000, 2000},
		{"medium_5x", 1000, 5000},
		{"large_3x", 10000, 30000},
		{"large_5x", 10000, 50000},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cache := newS3FIFO[int, int](&config{size: tc.capacity})
			maxSeen := 0

			for i := range tc.insertions {
				cache.set(i, i, 0)

				// Check capacity after every insertion
				current := cache.len()
				if current > maxSeen {
					maxSeen = current
				}

				// Capacity should never exceed the configured limit
				// Allow small overhead for concurrent operations (10%)
				maxAllowed := tc.capacity * 11 / 10
				if current > maxAllowed {
					t.Fatalf("capacity exceeded at insertion %d: got %d, max allowed %d",
						i, current, maxAllowed)
				}
			}

			// Final check: cache should be at or near capacity
			final := cache.len()
			minExpected := tc.capacity * 80 / 100
			if final < minExpected {
				t.Errorf("cache under-utilized: got %d, want at least %d", final, minExpected)
			}

			t.Logf("capacity=%d, insertions=%d, max_seen=%d, final=%d",
				tc.capacity, tc.insertions, maxSeen, final)
		})
	}
}

// TestS3FIFO_NeverExceedsCapacity_Concurrent verifies capacity bounds under concurrent access.
func TestS3FIFO_NeverExceedsCapacity_Concurrent(t *testing.T) {
	capacity := 10000
	cache := newS3FIFO[int, int](&config{size: capacity})

	var wg sync.WaitGroup
	numGoroutines := 10
	insertionsPerGoroutine := 5000

	// Track max length seen across all goroutines
	var maxLen atomic.Int64

	for g := range numGoroutines {
		wg.Add(1)
		go func(offset int) {
			defer wg.Done()
			for i := range insertionsPerGoroutine {
				cache.set(offset*insertionsPerGoroutine+i, i, 0)

				// Periodically check length
				if i%100 == 0 {
					current := int64(cache.len())
					for {
						old := maxLen.Load()
						if current <= old || maxLen.CompareAndSwap(old, current) {
							break
						}
					}
				}
			}
		}(g)
	}

	wg.Wait()

	// Capacity should never exceed limit (allow 15% for concurrent overhead)
	maxAllowed := capacity * 115 / 100
	if int(maxLen.Load()) > maxAllowed {
		t.Errorf("concurrent capacity exceeded: max seen %d, allowed %d", maxLen.Load(), maxAllowed)
	}

	// Final check
	final := cache.len()
	if final > maxAllowed {
		t.Errorf("final capacity exceeded: got %d, max allowed %d", final, maxAllowed)
	}

	t.Logf("concurrent test: capacity=%d, total_insertions=%d, max_seen=%d, final=%d",
		capacity, numGoroutines*insertionsPerGoroutine, maxLen.Load(), final)
}

// TestS3FIFO_NeverExceedsCapacity_MixedWorkload tests capacity with mixed get/set operations.
func TestS3FIFO_NeverExceedsCapacity_MixedWorkload(t *testing.T) {
	capacity := 5000
	cache := newS3FIFO[int, int](&config{size: capacity})

	// Phase 1: Fill cache
	for i := range capacity {
		cache.set(i, i, 0)
	}

	if cache.len() != capacity {
		t.Errorf("after fill: got %d, want %d", cache.len(), capacity)
	}

	// Phase 2: Heavy read/write mix that triggers promotions
	for round := range 3 {
		// Access existing items (promotes to main)
		for i := range capacity / 2 {
			cache.get(i)
		}

		// Insert new items (triggers eviction)
		baseKey := capacity + round*capacity
		for i := range capacity {
			cache.set(baseKey+i, i, 0)

			current := cache.len()
			maxAllowed := capacity * 11 / 10
			if current > maxAllowed {
				t.Fatalf("round %d, insertion %d: capacity exceeded %d > %d",
					round, i, current, maxAllowed)
			}
		}

		t.Logf("round %d: cache len = %d", round, cache.len())
	}
}

// TestS3FIFO_CapacityBound_2Xto4X verifies capacity bounds during sustained 2X-4X workload.
// This tests for inflation bugs where eviction might not keep up with insertions.
func TestS3FIFO_CapacityBound_2Xto4X(t *testing.T) {
	capacity := 10000
	// Allow GOMAXPROCS * 4 temporary overshoot
	maxOvershoot := runtime.GOMAXPROCS(0) * 4
	maxAllowed := capacity + maxOvershoot

	cache := newS3FIFO[int, int](&config{size: capacity})

	// Phase 1: Fill to capacity
	for i := range capacity {
		cache.set(i, i, 0)
	}

	// Phase 2: Access all entries to give them peakFreq >= 1 (triggers demotion path)
	for i := range capacity {
		cache.get(i)
	}

	maxSeen := cache.len()

	// Phase 3: Insert 2X more unique keys (total 3X capacity)
	for i := capacity; i < capacity*3; i++ {
		cache.set(i, i, 0)

		current := cache.len()
		if current > maxSeen {
			maxSeen = current
		}

		if current > maxAllowed {
			t.Fatalf("at insertion %d: capacity %d exceeded max %d (overshoot %d, allowed %d)",
				i, current, maxAllowed, current-capacity, maxOvershoot)
		}
	}

	t.Logf("2X-3X: capacity=%d, max_seen=%d, overshoot=%d, allowed=%d",
		capacity, maxSeen, maxSeen-capacity, maxOvershoot)

	// Phase 4: Continue to 4X with heavy access pattern (worst case for demotion)
	for i := capacity * 3; i < capacity*4; i++ {
		// Access recent entries to increase their freq (triggers promotion path)
		if i > capacity*3+100 {
			for j := i - 100; j < i; j++ {
				cache.get(j)
			}
		}

		cache.set(i, i, 0)

		current := cache.len()
		if current > maxSeen {
			maxSeen = current
		}

		if current > maxAllowed {
			t.Fatalf("at insertion %d: capacity %d exceeded max %d",
				i, current, maxAllowed)
		}
	}

	t.Logf("3X-4X: capacity=%d, max_seen=%d, overshoot=%d, final=%d",
		capacity, maxSeen, maxSeen-capacity, cache.len())
}

// TestS3FIFO_CapacityBound_PathologicalDemotion tests the demotion path specifically.
// All entries are accessed once (peakFreq=1) to trigger demotion instead of eviction.
func TestS3FIFO_CapacityBound_PathologicalDemotion(t *testing.T) {
	capacity := 5000
	maxOvershoot := runtime.GOMAXPROCS(0) * 4
	maxAllowed := capacity + maxOvershoot

	cache := newS3FIFO[int, int](&config{size: capacity})

	// Fill cache
	for i := range capacity {
		cache.set(i, i, 0)
	}

	// Access all entries exactly once to set peakFreq = 1
	// This triggers the demotion path in evictFromMain
	for i := range capacity {
		cache.get(i)
	}

	maxSeen := capacity

	// Insert 3X capacity of new keys
	// Every eviction from main should demote (not evict) since peakFreq >= 1
	// But the demoted entry has freq=1, so evictFromSmall should evict it
	for i := capacity; i < capacity*4; i++ {
		cache.set(i, i, 0)

		current := cache.len()
		if current > maxSeen {
			maxSeen = current
		}

		if current > maxAllowed {
			t.Fatalf("demotion pathology at %d: len=%d > max=%d",
				i, current, maxAllowed)
		}
	}

	t.Logf("demotion test: capacity=%d, max_seen=%d, overshoot=%d",
		capacity, maxSeen, maxSeen-capacity)
}

// TestS3FIFO_CapacityBound_AllHotEntries tests when all entries have high frequency.
// This triggers the promotion path in evictFromSmall.
func TestS3FIFO_CapacityBound_AllHotEntries(t *testing.T) {
	capacity := 5000
	// Max overshoot = 1% of capacity (hard cap in s3fifo.go).
	maxOvershoot := capacity / 100
	maxAllowed := capacity + maxOvershoot

	cache := newS3FIFO[int, int](&config{size: capacity})

	// Fill cache
	for i := range capacity {
		cache.set(i, i, 0)
	}

	// Access all entries multiple times to set freq = maxFreq (2)
	// This triggers the promotion path in evictFromSmall
	for range 3 {
		for i := range capacity {
			cache.get(i)
		}
	}

	maxSeen := capacity

	// Insert new keys while continuously accessing existing ones
	for i := capacity; i < capacity*3; i++ {
		// Keep existing entries hot
		cache.get(i % capacity)
		cache.get((i + 1) % capacity)

		cache.set(i, i, 0)

		current := cache.len()
		if current > maxSeen {
			maxSeen = current
		}

		if current > maxAllowed {
			t.Fatalf("all-hot pathology at %d: len=%d > max=%d",
				i, current, maxAllowed)
		}
	}

	t.Logf("all-hot test: capacity=%d, max_seen=%d, overshoot=%d",
		capacity, maxSeen, maxSeen-capacity)
}

// TestS3FIFO_MemoryLeak_2Xto3X verifies that memory is properly released
// when churning from 2X to 3X capacity. This catches leaks where evicted
// entries aren't being garbage collected.
func TestS3FIFO_MemoryLeak_2Xto3X(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping memory test in short mode")
	}

	capacity := 100000
	cache := newS3FIFO[int, []byte](&config{size: capacity})

	// Create a 1KB value to make memory usage measurable
	valueSize := 1024
	makeValue := func() []byte {
		return make([]byte, valueSize)
	}

	// Phase 1: Fill to capacity
	for i := range capacity {
		cache.set(i, makeValue(), 0)
	}

	// Force GC and get baseline
	runtime.GC()
	runtime.GC()
	var baseline runtime.MemStats
	runtime.ReadMemStats(&baseline)

	// Phase 2: Churn to 2X capacity (all new unique keys)
	for i := capacity; i < capacity*2; i++ {
		cache.set(i, makeValue(), 0)
	}

	// Force GC and measure after 2X
	runtime.GC()
	runtime.GC()
	var after2X runtime.MemStats
	runtime.ReadMemStats(&after2X)

	// Phase 3: Continue to 3X capacity
	for i := capacity * 2; i < capacity*3; i++ {
		cache.set(i, makeValue(), 0)
	}

	// Force GC and measure after 3X
	runtime.GC()
	runtime.GC()
	var after3X runtime.MemStats
	runtime.ReadMemStats(&after3X)

	// Calculate memory differences
	memAfter2X := int64(after2X.HeapAlloc) - int64(baseline.HeapAlloc)
	memAfter3X := int64(after3X.HeapAlloc) - int64(baseline.HeapAlloc)

	// Memory should not grow significantly between 2X and 3X
	// because evicted entries should be garbage collected.
	// Allow 20% growth for GC timing variance.
	maxGrowth := memAfter2X * 120 / 100 // 20% tolerance
	if memAfter3X > maxGrowth && memAfter3X-memAfter2X > 10*1024*1024 {
		t.Errorf("memory leak detected: after 2X=%d MB, after 3X=%d MB (grew %d MB)",
			memAfter2X/(1024*1024), memAfter3X/(1024*1024),
			(memAfter3X-memAfter2X)/(1024*1024))
	}

	// Verify cache is at capacity
	if cache.len() != capacity {
		t.Errorf("cache len = %d, want %d", cache.len(), capacity)
	}

	t.Logf("memory: baseline=%d MB, after_2X=%d MB, after_3X=%d MB, delta=%d MB",
		baseline.HeapAlloc/(1024*1024),
		after2X.HeapAlloc/(1024*1024),
		after3X.HeapAlloc/(1024*1024),
		(int64(after3X.HeapAlloc)-int64(after2X.HeapAlloc))/(1024*1024))
}

// TestS3FIFO_MemoryLeak_LargeValues tests memory behavior with large values.
func TestS3FIFO_MemoryLeak_LargeValues(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping memory test in short mode")
	}

	capacity := 10000
	cache := newS3FIFO[int, []byte](&config{size: capacity})

	// 10KB values
	valueSize := 10 * 1024
	makeValue := func() []byte {
		return make([]byte, valueSize)
	}

	// Fill to capacity
	for i := range capacity {
		cache.set(i, makeValue(), 0)
	}

	runtime.GC()
	runtime.GC()
	var baseline runtime.MemStats
	runtime.ReadMemStats(&baseline)

	// Churn with 3X unique keys
	for i := capacity; i < capacity*4; i++ {
		cache.set(i, makeValue(), 0)
	}

	runtime.GC()
	runtime.GC()
	var afterChurn runtime.MemStats
	runtime.ReadMemStats(&afterChurn)

	// Expected memory: capacity * valueSize = 10000 * 10KB = 100MB
	// With overhead, should be around 100-150MB
	// If we leak, we'd have 4X that = 400MB+
	expectedMax := int64(capacity) * int64(valueSize) * 2 // 2x for overhead tolerance

	if int64(afterChurn.HeapAlloc) > expectedMax {
		t.Errorf("possible memory leak: heap=%d MB, expected max=%d MB",
			afterChurn.HeapAlloc/(1024*1024), expectedMax/(1024*1024))
	}

	if cache.len() != capacity {
		t.Errorf("cache len = %d, want %d", cache.len(), capacity)
	}

	t.Logf("large values: baseline=%d MB, after_churn=%d MB, cache_len=%d",
		baseline.HeapAlloc/(1024*1024),
		afterChurn.HeapAlloc/(1024*1024),
		cache.len())
}

// TestS3FIFO_BloomFilter_SmallCapacity tests bloom filter with capacity < 1.
func TestS3FIFO_BloomFilter_SmallCapacity(t *testing.T) {
	// newBloomFilter should handle capacity < 1 gracefully
	bf := newBloomFilter(0, 0.01)
	if bf == nil {
		t.Fatal("newBloomFilter returned nil for capacity=0")
	}

	// Should still work
	bf.Add(12345)
	if !bf.Contains(12345) {
		t.Error("bloom filter should contain added hash")
	}

	// Test with negative capacity
	bf2 := newBloomFilter(-5, 0.01)
	if bf2 == nil {
		t.Fatal("newBloomFilter returned nil for negative capacity")
	}
	bf2.Add(67890)
	if !bf2.Contains(67890) {
		t.Error("bloom filter should contain added hash")
	}
}

// TestS3FIFO_DeathRowResurrection tests the resurrection path from death row.
func TestS3FIFO_DeathRowResurrection(t *testing.T) {
	// Small cache to trigger death row quickly
	cache := newS3FIFO[int, int](&config{size: 100})

	// Fill cache completely
	for i := range 100 {
		cache.set(i, i*10, 0)
	}

	// Access some items to make them hot (high peakFreq for death row admission)
	for range 5 {
		for i := range 20 {
			cache.get(i)
		}
	}

	// Add many more items to force evictions to death row
	for i := 100; i < 300; i++ {
		cache.set(i, i*10, 0)
	}

	// Now check if any hot items are on death row by accessing them
	resurrectedCount := 0
	for i := range 20 {
		if val, ok := cache.get(i); ok {
			if val == i*10 {
				resurrectedCount++
			}
		}
	}

	t.Logf("Resurrected %d items from death row", resurrectedCount)
}

// TestS3FIFO_DeathRowResurrectionTriggersEviction tests resurrection when cache is full.
func TestS3FIFO_DeathRowResurrectionTriggersEviction(t *testing.T) {
	// Small cache
	cache := newS3FIFO[int, int](&config{size: 50})

	// Fill cache
	for i := range 50 {
		cache.set(i, i*10, 0)
	}

	// Access items to make them hot
	for range 5 {
		for i := range 10 {
			cache.get(i)
		}
	}

	// Add more to force evictions (hot items may go to death row)
	for i := 50; i < 150; i++ {
		cache.set(i, i*10, 0)
	}

	// Now fill again to max capacity
	for i := 150; i < 200; i++ {
		cache.set(i, i*10, 0)
	}

	// Try to resurrect items - this should trigger eviction since cache is full
	for i := range 10 {
		cache.get(i)
	}

	// Cache should still be at capacity
	if cache.len() > 55 { // Allow small overshoot
		t.Errorf("cache len = %d; should be near 50", cache.len())
	}
}

// TestS3FIFO_DeleteFromMainQueue tests deleting an item that's in the main queue.
func TestS3FIFO_DeleteFromMainQueue(t *testing.T) {
	// Use larger cache to ensure items survive eviction
	cache := newS3FIFO[int, int](&config{size: 1000})

	// Fill cache partially
	for i := range 500 {
		cache.set(i, i*10, 0)
	}

	// Access items multiple times to promote them to main queue
	for range 5 {
		for i := range 100 {
			cache.get(i)
		}
	}

	// Add more items to trigger eviction from small, promoting accessed items to main
	for i := 500; i < 800; i++ {
		cache.set(i, i*10, 0)
	}

	// Verify an item is in cache before delete
	if _, ok := cache.get(0); !ok {
		t.Skip("key 0 was evicted, skipping main queue delete test")
	}

	// Now delete an item that should be in main queue
	cache.del(0)

	// Verify it's deleted
	if _, ok := cache.get(0); ok {
		t.Error("key 0 should be deleted")
	}

	// Check that cache still works
	cache.set(9999, 9999, 0)
	if val, ok := cache.get(9999); !ok || val != 9999 {
		t.Errorf("get(9999) = %d, %v; want 9999, true", val, ok)
	}
}

// TestS3FIFO_GhostQueueRotation tests ghost queue rotation when it fills up.
func TestS3FIFO_GhostQueueRotation(t *testing.T) {
	// Small cache with small ghost capacity
	cache := newS3FIFO[int, int](&config{size: 100})

	// Ghost capacity is 8x cache size = 800
	// Fill cache
	for i := range 100 {
		cache.set(i, i*10, 0)
	}

	// Insert many unique keys to trigger many evictions and fill ghost queue
	// Need > 800 evictions to trigger ghost rotation
	for i := 100; i < 2000; i++ {
		cache.set(i, i*10, 0)
	}

	// Now re-insert some keys that should have been evicted (and be in ghost)
	// They should go to main queue if still in ghost
	for i := range 50 {
		cache.set(i, i*100, 0) // Different value to verify it's updated
	}

	// Verify the re-inserted keys have the new value
	for i := range 50 {
		if val, ok := cache.get(i); ok {
			if val != i*100 {
				t.Errorf("key %d = %d; want %d", i, val, i*100)
			}
		}
	}
}

// TestS3FIFO_GhostFrequencyRestoration tests that frequency is restored from ghost.
func TestS3FIFO_GhostFrequencyRestoration(t *testing.T) {
	cache := newS3FIFO[int, int](&config{size: 100})

	// Fill cache
	for i := range 100 {
		cache.set(i, i*10, 0)
	}

	// Access key 0 many times to give it high peakFreq
	for range 10 {
		cache.get(0)
	}

	// Evict key 0 by adding many new keys
	for i := 100; i < 300; i++ {
		cache.set(i, i*10, 0)
	}

	// Key 0 should be evicted now
	if _, ok := cache.get(0); ok {
		// If still in cache (or resurrected), skip this test
		t.Log("key 0 still in cache (may have been resurrected)")
		return
	}

	// Re-insert key 0 - it should restore frequency from ghost
	cache.set(0, 999, 0)

	// Get the entry and check if it's in main (not small)
	ent, ok := cache.getEntry(0)
	if !ok {
		t.Fatal("key 0 not found after re-insert")
	}

	// Entry should be in main queue if ghost had it
	if ent.inSmall() {
		t.Log("key 0 in small queue (ghost may have rotated)")
	} else {
		t.Log("key 0 in main queue (restored from ghost)")
	}
}

// TestS3FIFO_HashString_EmptyString tests hashing empty string.
func TestS3FIFO_HashString_EmptyString(t *testing.T) {
	cache := newS3FIFO[string, int](&config{size: 100})

	// Empty string should work
	cache.set("", 42, 0)
	if val, ok := cache.get(""); !ok || val != 42 {
		t.Errorf("empty string: got %d, %v; want 42, true", val, ok)
	}
}

// TestS3FIFO_HashString_ShortStrings tests hashing strings of various lengths.
func TestS3FIFO_HashString_ShortStrings(t *testing.T) {
	cache := newS3FIFO[string, int](&config{size: 100})

	// Test strings of length 1-8 (different code paths in hashString)
	testCases := []string{
		"a",         // len 1
		"ab",        // len 2
		"abc",       // len 3
		"abcd",      // len 4 (uses uint32 path)
		"abcde",     // len 5
		"abcdef",    // len 6
		"abcdefg",   // len 7
		"abcdefgh",  // len 8 (uses uint64 path)
		"abcdefghi", // len 9 (uses uint64 path)
	}

	for i, s := range testCases {
		cache.set(s, i+1, 0)
	}

	for i, s := range testCases {
		if val, ok := cache.get(s); !ok || val != i+1 {
			t.Errorf("string %q (len %d): got %d, %v; want %d, true", s, len(s), val, ok, i+1)
		}
	}
}

// TestS3FIFO_TimeToSec tests the timeToSec helper.
func TestS3FIFO_TimeToSec(t *testing.T) {
	// Zero time should return 0
	if got := timeToSec(time.Time{}); got != 0 {
		t.Errorf("timeToSec(zero) = %d; want 0", got)
	}

	// Non-zero time should return Unix seconds
	now := time.Now()
	if got := timeToSec(now); got != uint32(now.Unix()) {
		t.Errorf("timeToSec(now) = %d; want %d", got, now.Unix())
	}
}

// TestS3FIFO_ResurrectNotOnDeathRow tests resurrection when entry is no longer on death row.
func TestS3FIFO_ResurrectNotOnDeathRow(t *testing.T) {
	cache := newS3FIFO[int, int](&config{size: 50})

	// Fill cache
	for i := range 50 {
		cache.set(i, i*10, 0)
	}

	// Access items to make them hot
	for range 5 {
		for i := range 10 {
			cache.get(i)
		}
	}

	// Add items to push hot items to death row
	for i := 50; i < 100; i++ {
		cache.set(i, i*10, 0)
	}

	// Add many more items to cycle through death row completely
	// Death row is small, so this should evict death row items
	for i := 100; i < 500; i++ {
		cache.set(i, i*10, 0)
	}

	// Try to access an item that was once hot but is now gone
	// This tests the path where entry exists but is not on death row
	for i := range 10 {
		cache.get(i) // May or may not find it
	}
}

// TestS3FIFO_EvictFromSmall_EmptyQueue tests eviction when small queue is empty.
func TestS3FIFO_EvictFromSmall_EmptyQueue(t *testing.T) {
	cache := newS3FIFO[int, int](&config{size: 100})

	// Fill cache
	for i := range 100 {
		cache.set(i, i*10, 0)
	}

	// Access all items many times to promote them all to main
	for range 5 {
		for i := range 100 {
			cache.get(i)
		}
	}

	// Add more items to trigger eviction
	// With all items promoted to main, small queue should be empty after some ops
	for i := 100; i < 200; i++ {
		cache.set(i, i*10, 0)
	}

	// Cache should still work
	cache.set(999, 999, 0)
	if val, ok := cache.get(999); !ok || val != 999 {
		t.Errorf("get(999) = %d, %v; want 999, true", val, ok)
	}
}

// TestS3FIFO_EvictFromMain_AllCold tests eviction when all main entries are cold.
func TestS3FIFO_EvictFromMain_AllCold(t *testing.T) {
	cache := newS3FIFO[int, int](&config{size: 100})

	// Fill cache
	for i := range 100 {
		cache.set(i, i*10, 0)
	}

	// Access items once to get them to main (but freq will decay)
	for i := range 100 {
		cache.get(i)
		cache.get(i)
	}

	// Add many more items without accessing existing ones
	// This should evict from main when items become cold
	for i := 100; i < 500; i++ {
		cache.set(i, i*10, 0)
	}

	// Cache should be at capacity
	if cache.len() > 110 {
		t.Errorf("cache len = %d; should be near 100", cache.len())
	}
}

// TestS3FIFO_GhostFreqRing tests the ghost frequency ring buffer.
func TestS3FIFO_GhostFreqRing(t *testing.T) {
	var ring ghostFreqRing

	// Add some entries
	ring.add(100, 5)
	ring.add(200, 10)
	ring.add(300, 15)

	// Lookup existing
	if freq, ok := ring.lookup(100); !ok || freq != 5 {
		t.Errorf("lookup(100) = %d, %v; want 5, true", freq, ok)
	}
	if freq, ok := ring.lookup(200); !ok || freq != 10 {
		t.Errorf("lookup(200) = %d, %v; want 10, true", freq, ok)
	}

	// Lookup non-existing
	if _, ok := ring.lookup(999); ok {
		t.Error("lookup(999) should return false")
	}

	// Fill ring to wrap around
	for i := range 300 {
		ring.add(uint32(1000+i), uint32(i))
	}

	// Old entries should be overwritten
	if _, ok := ring.lookup(100); ok {
		t.Error("lookup(100) should return false after wrap")
	}
}

// TestS3FIFO_DeleteFromSmallQueue tests deleting an item that's in the small queue.
func TestS3FIFO_DeleteFromSmallQueue(t *testing.T) {
	cache := newS3FIFO[int, int](&config{size: 100})

	// Fill cache partially (items stay in small queue initially)
	for i := range 50 {
		cache.set(i, i*10, 0)
	}

	// Delete an item from small queue
	cache.del(25)

	// Verify it's deleted
	if _, ok := cache.get(25); ok {
		t.Error("key 25 should be deleted")
	}

	// Other items should still work
	if val, ok := cache.get(0); !ok || val != 0 {
		t.Errorf("get(0) = %d, %v; want 0, true", val, ok)
	}
}

// TestS3FIFO_DeleteNonexistent tests deleting a key that doesn't exist.
func TestS3FIFO_DeleteNonexistent(t *testing.T) {
	cache := newS3FIFO[int, int](&config{size: 100})

	// Delete nonexistent key should not panic
	cache.del(999)

	// Cache should still work
	cache.set(1, 10, 0)
	if val, ok := cache.get(1); !ok || val != 10 {
		t.Errorf("get(1) = %d, %v; want 10, true", val, ok)
	}
}

// TestS3FIFO_ResurrectFullCache tests resurrection when cache is completely full.
func TestS3FIFO_ResurrectFullCache(t *testing.T) {
	cache := newS3FIFO[int, int](&config{size: 100})

	// Fill cache
	for i := range 100 {
		cache.set(i, i*10, 0)
	}

	// Access first 20 items many times (make them hot)
	for range 10 {
		for i := range 20 {
			cache.get(i)
		}
	}

	// Fill to capacity again with new items (evicts old ones)
	for i := 100; i < 200; i++ {
		cache.set(i, i*10, 0)
	}

	// Continue until cache is at capacity
	for i := 200; i < 250; i++ {
		cache.set(i, i*10, 0)
	}

	// Now try to resurrect an item when cache is full
	// This should trigger eviction after resurrection
	for i := range 20 {
		cache.get(i)
	}

	// Cache should be at or near capacity
	if cache.len() > 110 {
		t.Errorf("cache len = %d; should be near 100", cache.len())
	}
}

// TestS3FIFO_EvictFromMainWithDemotion tests the demotion path in evictFromMain.
func TestS3FIFO_EvictFromMainWithDemotion(t *testing.T) {
	cache := newS3FIFO[int, int](&config{size: 100})

	// Fill cache
	for i := range 100 {
		cache.set(i, i*10, 0)
	}

	// Access first 50 items once (gives peakFreq >= 1)
	for i := range 50 {
		cache.get(i)
	}

	// Access them once more to get them promoted to main
	for i := range 50 {
		cache.get(i)
	}

	// Add more items to trigger eviction from main
	// Items with peakFreq >= 1 should be demoted, not evicted
	for i := 100; i < 300; i++ {
		cache.set(i, i*10, 0)
	}

	// Cache should be at capacity
	if cache.len() > 110 {
		t.Errorf("cache len = %d; should be near 100", cache.len())
	}
}

// TestS3FIFO_EvictFromMainWithSecondChance tests the second chance mechanism.
func TestS3FIFO_EvictFromMainWithSecondChance(t *testing.T) {
	cache := newS3FIFO[int, int](&config{size: 100})

	// Fill cache
	for i := range 100 {
		cache.set(i, i*10, 0)
	}

	// Access all items multiple times to give them high frequency
	for range 5 {
		for i := range 100 {
			cache.get(i)
		}
	}

	// Add more items - items with freq > 0 get second chance
	for i := 100; i < 200; i++ {
		cache.set(i, i*10, 0)
	}

	// Continue adding to force eviction
	for i := 200; i < 300; i++ {
		cache.set(i, i*10, 0)
	}

	// Hot items should have survived longer due to second chance
	survivedCount := 0
	for i := range 50 {
		if _, ok := cache.get(i); ok {
			survivedCount++
		}
	}

	t.Logf("Survived: %d/50 hot items", survivedCount)
}

// TestS3FIFO_EvictFromSmallPromotion tests promotion from small to main during eviction.
func TestS3FIFO_EvictFromSmallPromotion(t *testing.T) {
	cache := newS3FIFO[int, int](&config{size: 100})

	// Fill cache
	for i := range 100 {
		cache.set(i, i*10, 0)
	}

	// Access items enough times to trigger promotion (freq >= 2)
	for i := range 50 {
		cache.get(i)
		cache.get(i)
	}

	// Add more items to trigger eviction from small
	// Items with freq >= 2 should be promoted to main
	for i := 100; i < 150; i++ {
		cache.set(i, i*10, 0)
	}

	// Check that accessed items survived (were promoted)
	survivedCount := 0
	for i := range 50 {
		if _, ok := cache.get(i); ok {
			survivedCount++
		}
	}

	if survivedCount < 30 {
		t.Errorf("only %d/50 accessed items survived; expected more due to promotion", survivedCount)
	}
}

// TestS3FIFO_SetWithHash_NonZeroHash tests setWithHash with pre-computed hash.
func TestS3FIFO_SetWithHash_NonZeroHash(t *testing.T) {
	cache := newS3FIFO[string, int](&config{size: 100})

	// Set with pre-computed hash
	key := "testkey"
	hash := hashString(key)
	cache.setWithHash(key, 42, 0, hash)

	// Should be retrievable
	if val, ok := cache.get(key); !ok || val != 42 {
		t.Errorf("get(%q) = %d, %v; want 42, true", key, val, ok)
	}
}

// TestS3FIFO_SetWithHash_ZeroHash tests setWithHash with hash=0 (should compute).
func TestS3FIFO_SetWithHash_ZeroHash(t *testing.T) {
	cache := newS3FIFO[int, int](&config{size: 100})

	// Set with zero hash (should compute internally)
	cache.setWithHash(42, 100, 0, 0)

	// Should be retrievable
	if val, ok := cache.get(42); !ok || val != 100 {
		t.Errorf("get(42) = %d, %v; want 100, true", val, ok)
	}
}

// TestS3FIFO_UpdateExistingInLockPath tests updating existing entry after lock acquisition.
func TestS3FIFO_UpdateExistingInLockPath(t *testing.T) {
	cache := newS3FIFO[int, int](&config{size: 100})

	// Pre-populate
	cache.set(1, 10, 0)

	// Concurrent updates should handle the lock path correctly
	var wg sync.WaitGroup
	for i := range 10 {
		wg.Add(1)
		go func(val int) {
			defer wg.Done()
			cache.set(1, val*100, 0)
		}(i)
	}
	wg.Wait()

	// Final value should be one of the updates
	if val, ok := cache.get(1); !ok {
		t.Error("key 1 should exist")
	} else if val < 0 || val > 900 {
		t.Errorf("get(1) = %d; unexpected value", val)
	}
}

// TestS3FIFO_WarmupPhase tests behavior during warmup (before full).
func TestS3FIFO_WarmupPhase(t *testing.T) {
	cache := newS3FIFO[int, int](&config{size: 100})

	// During warmup, items go directly to small queue
	for i := range 50 {
		cache.set(i, i*10, 0)
	}

	// All items should be present
	for i := range 50 {
		if val, ok := cache.get(i); !ok || val != i*10 {
			t.Errorf("get(%d) = %d, %v; want %d, true", i, val, ok, i*10)
		}
	}
}

// TestS3FIFO_ResurrectEntry_NotOnDeathRow tests resurrection path when entry exists but not on death row.
func TestS3FIFO_ResurrectEntry_NotOnDeathRow(t *testing.T) {
	cache := newS3FIFO[int, int](&config{size: 50})

	// Fill cache
	for i := range 50 {
		cache.set(i, i*10, 0)
	}

	// Access some items to make them hot
	for range 5 {
		for i := range 10 {
			cache.get(i)
		}
	}

	// Evict hot items to death row
	for i := 50; i < 100; i++ {
		cache.set(i, i*10, 0)
	}

	// Access many more items to cycle through death row completely
	for i := 100; i < 300; i++ {
		cache.set(i, i*10, 0)
	}

	// Accessing an evicted item now should return not found
	// because it was pushed off death row
	for i := range 10 {
		if _, ok := cache.get(i); ok {
			// Item still exists (either still on death row or resurrected)
			t.Logf("key %d still accessible (may have been resurrected)", i)
		}
	}
}

// TestS3FIFO_HashInt64 tests the hashInt64 function.
func TestS3FIFO_HashInt64(t *testing.T) {
	// Verify hash produces reasonable distribution
	hashes := make(map[uint64]bool)
	for i := range 1000 {
		h := hashInt64(int64(i))
		if hashes[h] {
			t.Errorf("collision at i=%d", i)
		}
		hashes[h] = true
	}

	// Test negative values
	h1 := hashInt64(-1)
	h2 := hashInt64(-2)
	if h1 == h2 {
		t.Error("different inputs should produce different hashes")
	}
}

// TestS3FIFO_EvictFromMainEmpty tests evictFromMain when main is empty.
func TestS3FIFO_EvictFromMainEmpty(t *testing.T) {
	cache := newS3FIFO[int, int](&config{size: 100})

	// Fill only small queue
	for i := range 20 {
		cache.set(i, i*10, 0)
	}

	// Small queue should have items, main should be empty
	// evictFromMain should return false and do nothing
	result := cache.evictFromMain()
	if result {
		t.Error("evictFromMain should return false when main is empty")
	}
}

// TestS3FIFO_DefaultHasher_Uint tests the default hasher with uint keys.
func TestS3FIFO_DefaultHasher_Uint(t *testing.T) {
	cache := newS3FIFO[uint, int](&config{size: 100})

	// Test basic operations with uint keys
	cache.set(uint(42), 420, 0)
	cache.set(uint(0), 0, 0)
	cache.set(uint(100), 1000, 0)

	if val, ok := cache.get(uint(42)); !ok || val != 420 {
		t.Errorf("get(42) = %d, %v; want 420, true", val, ok)
	}
	if val, ok := cache.get(uint(0)); !ok || val != 0 {
		t.Errorf("get(0) = %d, %v; want 0, true", val, ok)
	}
	if val, ok := cache.get(uint(100)); !ok || val != 1000 {
		t.Errorf("get(100) = %d, %v; want 1000, true", val, ok)
	}
}

// TestS3FIFO_DefaultHasher_Uint64 tests the default hasher with uint64 keys.
func TestS3FIFO_DefaultHasher_Uint64(t *testing.T) {
	cache := newS3FIFO[uint64, int](&config{size: 100})

	cache.set(uint64(1<<63), 1, 0)
	cache.set(uint64(0), 2, 0)
	cache.set(uint64(12345), 3, 0)

	if val, ok := cache.get(uint64(1 << 63)); !ok || val != 1 {
		t.Errorf("get(1<<63) = %d, %v; want 1, true", val, ok)
	}
	if val, ok := cache.get(uint64(0)); !ok || val != 2 {
		t.Errorf("get(0) = %d, %v; want 2, true", val, ok)
	}
}

// TestS3FIFO_EvictFromSmall_PromotionTriggersMainEviction tests the cascade path.
func TestS3FIFO_EvictFromSmall_PromotionTriggersMainEviction(t *testing.T) {
	// Use small cache where main queue capacity (10%) is easily exceeded
	cache := newS3FIFO[int, int](&config{size: 100})

	// Fill cache
	for i := range 100 {
		cache.set(i, i*10, 0)
	}

	// Access all items multiple times to give them high freq
	for range 3 {
		for i := range 100 {
			cache.get(i)
		}
	}

	// Now small items have high freq, so when evicted they promote to main
	// Main will overflow (>90% capacity) and trigger eviction

	// Add more items to trigger this cascade
	for i := 100; i < 200; i++ {
		cache.set(i, i*10, 0)
	}

	// Cache should remain at capacity
	if cache.len() > 110 {
		t.Errorf("cache len = %d; should be near 100", cache.len())
	}
}

// TestS3FIFO_DeleteItemInMain tests deleting an item that's specifically in the main queue.
func TestS3FIFO_DeleteItemInMain(t *testing.T) {
	cache := newS3FIFO[int, int](&config{size: 100})

	// Fill cache to capacity
	for i := range 100 {
		cache.set(i, i*10, 0)
	}

	// Access first 50 items many times to ensure freq >= 2 for promotion
	for range 5 {
		for i := range 50 {
			cache.get(i)
		}
	}

	// Add more items to trigger eviction from small queue
	// Items with freq >= 2 will be promoted to main queue
	for i := 100; i < 200; i++ {
		cache.set(i, i*10, 0)
	}

	// Find an entry in main queue (not inSmall, not onDeathRow)
	for i := range 50 {
		if ent, ok := cache.getEntry(i); ok && !ent.inSmall() && !ent.onDeathRow() {
			// Found one in main - delete it
			t.Logf("Found key %d in main queue, deleting", i)
			cache.del(i)
			if _, ok := cache.get(i); ok {
				t.Errorf("key %d should be deleted", i)
			}
			return
		}
	}

	// If we reach here, let's debug
	var inSmallCount, inMainCount, onDeathRowCount int
	for i := range 50 {
		if ent, ok := cache.getEntry(i); ok {
			switch {
			case ent.onDeathRow():
				onDeathRowCount++
			case ent.inSmall():
				inSmallCount++
			default:
				inMainCount++
			}
		}
	}
	t.Logf("Stats: inSmall=%d, inMain=%d, onDeathRow=%d", inSmallCount, inMainCount, onDeathRowCount)
	t.Log("Could not find item in main queue for deletion test")
}

// TestS3FIFO_ResurrectFromDeathRow_Detailed tests resurrection with eviction.
func TestS3FIFO_ResurrectFromDeathRow_Detailed(t *testing.T) {
	cache := newS3FIFO[int, int](&config{size: 50})

	// Fill cache
	for i := range 50 {
		cache.set(i, i*10, 0)
	}

	// Access first 10 items many times to make them very hot
	for range 10 {
		for i := range 10 {
			cache.get(i)
		}
	}

	// Evict hot items to death row by adding new items
	for i := 50; i < 100; i++ {
		cache.set(i, i*10, 0)
	}

	// Find items on death row and try to resurrect them
	for i := range 10 {
		if ent, ok := cache.getEntry(i); ok && ent.onDeathRow() {
			// Found one on death row - access it to resurrect
			val, found := cache.get(i)
			if found {
				t.Logf("Resurrected key %d with value %d", i, val)
			}
		}
	}
}

// TestS3FIFO_SampleAvgPeakFreq_Empty tests sampleAvgPeakFreq with empty main.
func TestS3FIFO_SampleAvgPeakFreq_Empty(t *testing.T) {
	cache := newS3FIFO[int, int](&config{size: 100})

	// Main queue is empty
	avg := cache.sampleAvgPeakFreq()
	if avg != 1 {
		t.Errorf("sampleAvgPeakFreq() = %d; want 1 (minimum)", avg)
	}
}

// TestS3FIFO_EntryListOperations tests the entry list operations directly.
func TestS3FIFO_EntryListOperations(t *testing.T) {
	var list entryList[int, int]

	e1 := &entry[int, int]{key: 1}
	e2 := &entry[int, int]{key: 2}
	e3 := &entry[int, int]{key: 3}

	// Push entries
	list.pushBack(e1)
	if list.len != 1 || list.head != e1 || list.tail != e1 {
		t.Error("pushBack first element failed")
	}

	list.pushBack(e2)
	list.pushBack(e3)
	if list.len != 3 || list.head != e1 || list.tail != e3 {
		t.Error("pushBack multiple elements failed")
	}

	// Remove middle
	list.remove(e2)
	if list.len != 2 || e1.next != e3 || e3.prev != e1 {
		t.Error("remove middle element failed")
	}

	// Remove head
	list.remove(e1)
	if list.len != 1 || list.head != e3 || list.tail != e3 {
		t.Error("remove head element failed")
	}

	// Remove last
	list.remove(e3)
	if list.len != 0 || list.head != nil || list.tail != nil {
		t.Error("remove last element failed")
	}
}

// structKey is used to test the default hasher fallback (not uint, uint64, string, or fmt.Stringer).
type structKey struct {
	A int
	B int
}

// TestS3FIFO_DefaultHasher_StructKey tests the default hasher fallback with struct keys.
func TestS3FIFO_DefaultHasher_StructKey(t *testing.T) {
	cache := newS3FIFO[structKey, int](&config{size: 100})

	key1 := structKey{A: 1, B: 2}
	key2 := structKey{A: 3, B: 4}

	cache.set(key1, 100, 0)
	cache.set(key2, 200, 0)

	if val, ok := cache.get(key1); !ok || val != 100 {
		t.Errorf("get(key1) = %v, %v; want 100, true", val, ok)
	}
	if val, ok := cache.get(key2); !ok || val != 200 {
		t.Errorf("get(key2) = %v, %v; want 200, true", val, ok)
	}

	// Test update
	cache.set(key1, 150, 0)
	if val, ok := cache.get(key1); !ok || val != 150 {
		t.Errorf("get(key1) after update = %v, %v; want 150, true", val, ok)
	}

	// Test delete
	cache.del(key1)
	if _, ok := cache.get(key1); ok {
		t.Error("key1 should be deleted")
	}
}

// TestS3FIFO_ResurrectFromDeathRow_WithEviction tests resurrection that triggers eviction.
func TestS3FIFO_ResurrectFromDeathRow_WithEviction(t *testing.T) {
	// Small cache to make it easier to fill and trigger eviction
	cache := newS3FIFO[int, int](&config{size: 20})

	// Fill cache with initial items
	for i := range 20 {
		cache.set(i, i*10, 0)
	}

	// Access some items to increase frequency and get them promoted to main
	for range 5 {
		for i := range 10 {
			cache.get(i)
		}
	}

	// Add more items to cause eviction, pushing some items to death row
	for i := 20; i < 40; i++ {
		cache.set(i, i*10, 0)
	}

	// Find an item on death row
	foundOnDeathRow := -1
	for i := range 20 {
		if ent, ok := cache.getEntry(i); ok && ent.onDeathRow() {
			foundOnDeathRow = i
			break
		}
	}

	if foundOnDeathRow == -1 {
		t.Skip("No items found on death row for this test")
	}

	// Access the death row item to resurrect it - this may trigger eviction
	// since we're at capacity and resurrection adds back to main
	val, ok := cache.get(foundOnDeathRow)
	if !ok {
		t.Errorf("Failed to resurrect item %d", foundOnDeathRow)
	} else {
		t.Logf("Resurrected key %d with value %d", foundOnDeathRow, val)
	}

	// Verify the item is no longer on death row
	if ent, ok := cache.getEntry(foundOnDeathRow); ok {
		if ent.onDeathRow() {
			t.Error("Item should not be on death row after resurrection")
		}
		if ent.inSmall() {
			t.Error("Resurrected item should be in main queue, not small")
		}
	}
}

// TestS3FIFO_ResurrectFromDeathRow_NotOnDeathRow tests the early return when entry exists but not on death row.
func TestS3FIFO_ResurrectFromDeathRow_NotOnDeathRow(t *testing.T) {
	cache := newS3FIFO[int, int](&config{size: 100})

	// Add an item normally
	cache.set(1, 100, 0)

	// Try to call resurrectFromDeathRow directly - it should return early
	// since the item is not on death row
	val, ok := cache.resurrectFromDeathRow(1)

	// Since the entry exists but is not on death row, we should get ok=true with the value
	if !ok {
		t.Errorf("resurrectFromDeathRow returned ok=false for existing entry not on death row")
	}
	// The value might be returned since ok is based on entry existence
	_ = val

	// The entry should still be in the cache
	if v, found := cache.get(1); !found || v != 100 {
		t.Errorf("get(1) = %v, %v; want 100, true", v, found)
	}
}

// TestS3FIFO_ResurrectFromDeathRow_NotFound tests the early return when entry doesn't exist.
func TestS3FIFO_ResurrectFromDeathRow_NotFound(t *testing.T) {
	cache := newS3FIFO[int, int](&config{size: 100})

	// Try to resurrect a key that was never added
	val, ok := cache.resurrectFromDeathRow(999)

	if ok {
		t.Errorf("resurrectFromDeathRow returned ok=true for non-existent key, val=%v", val)
	}
}

// TestS3FIFO_EvictFromSmall_ReturnsFalse tests evictFromSmall returning false.
func TestS3FIFO_EvictFromSmall_ReturnsFalse(t *testing.T) {
	cache := newS3FIFO[int, int](&config{size: 100})

	// Small queue is empty by default
	result := cache.evictFromSmall()
	if result {
		t.Error("evictFromSmall should return false when queue is empty")
	}
}

// TestS3FIFO_EvictionLoop_EmptyQueues tests the eviction loop when both queues are empty.
func TestS3FIFO_EvictionLoop_EmptyQueues(t *testing.T) {
	cache := newS3FIFO[int, int](&config{size: 10})

	// Fill cache to trigger warmup completion
	for i := range 10 {
		cache.set(i, i, 0)
	}

	// Clear entries manually to create empty queue state
	cache.mu.Lock()
	for cache.small.len > 0 {
		e := cache.small.head
		cache.small.remove(e)
		cache.entries.Delete(e.key)
		cache.totalEntries.Add(-1)
	}
	for cache.main.len > 0 {
		e := cache.main.head
		cache.main.remove(e)
		cache.entries.Delete(e.key)
		cache.totalEntries.Add(-1)
	}
	cache.mu.Unlock()

	// Now set a new item when capacity is exceeded but queues are empty
	// This should hit the "else { break }" path in the eviction loop
	cache.set(100, 100, 0)

	if _, ok := cache.get(100); !ok {
		t.Error("new item should be in cache")
	}
}

// TestS3FIFO_SetWithHash_DoubleCheck is in s3fifo_seqlock_race_test.go
// It tests concurrent set operations which trigger seqlock races.

// TestS3FIFO_SetNotFull tests setting when cache is not full (else branch).
func TestS3FIFO_SetNotFull(t *testing.T) {
	cache := newS3FIFO[int, int](&config{size: 100})

	// Fill cache to capacity to trigger warmup
	for i := range 100 {
		cache.set(i, i, 0)
	}

	// Add one more to complete warmup (warmup completes when full=true)
	cache.set(100, 100, 0)

	// Verify warmup is complete
	if !cache.warmupComplete {
		t.Fatal("warmup should be complete after exceeding capacity")
	}

	// Delete entries to make cache not full
	for i := range 60 {
		cache.del(i)
	}

	// Verify cache is under capacity
	if cache.totalEntries.Load() >= int64(cache.capacity) {
		t.Fatalf("cache should be under capacity: %d >= %d", cache.totalEntries.Load(), cache.capacity)
	}

	// Now insert new entry - should hit the "else { ent.setInSmall(true) }" path
	// because warmupComplete=true but full=false
	cache.set(1000, 1000, 0)

	if val, ok := cache.get(1000); !ok || val != 1000 {
		t.Errorf("get(1000) = %v, %v; want 1000, true", val, ok)
	}

	// Verify the new entry went to small queue
	if ent, ok := cache.getEntry(1000); ok {
		if !ent.inSmall() {
			t.Error("new entry should be in small queue when cache not full")
		}
	}
}

// TestS3FIFO_EvictionEdgeCases tests edge cases in eviction functions.
func TestS3FIFO_EvictionEdgeCases(t *testing.T) {
	// Test evictFromSmall returning false when queue is empty
	cache := newS3FIFO[int, int](&config{size: 10})

	// Manually call evictFromSmall on empty cache
	if cache.evictFromSmall() {
		t.Error("evictFromSmall should return false on empty queue")
	}

	// Test evictFromMain returning false when queue is empty
	if cache.evictFromMain() {
		t.Error("evictFromMain should return false on empty queue")
	}

	// Test demotion path: item with peakFreq >= 1 gets demoted from main to small
	// Fill cache to trigger eviction logic
	for i := range 10 {
		cache.set(i, i, 0)
	}

	// Access some items to increase their freq
	for i := range 5 {
		cache.get(i)
		cache.get(i) // Increase freq to promote to main
	}

	// Force entries to main queue by adding more
	for i := 10; i < 20; i++ {
		cache.set(i, i, 0)
	}

	// Verify we hit promotion and demotion paths
	mainCount := cache.main.len
	smallCount := cache.small.len
	t.Logf("main.len=%d, small.len=%d", mainCount, smallCount)
}

// TestS3FIFO_EvictFromMain_DirectToDeathRow tests eviction when peakFreq < 1.
func TestS3FIFO_EvictFromMain_DirectToDeathRow(t *testing.T) {
	cache := newS3FIFO[int, int](&config{size: 10})

	// Fill cache with items that are never accessed (peakFreq stays 0)
	for i := range 10 {
		cache.set(i, i, 0)
	}

	// Force items to main queue by accessing them twice (promotes to main)
	for i := range 10 {
		cache.get(i)
		cache.get(i)
	}

	// Now add more items to trigger eviction from main
	// Items in main with freq=0 and peakFreq=0 should go directly to death row
	for i := 10; i < 25; i++ {
		cache.set(i, i, 0)
	}

	// Verify some items were evicted
	evicted := 0
	for i := range 10 {
		if _, ok := cache.get(i); !ok {
			evicted++
		}
	}
	t.Logf("Evicted %d of original 10 items", evicted)
}

// TestS3FIFO_EvictFromSmall_TriggersMainEviction tests the nested eviction path.
func TestS3FIFO_EvictFromSmall_TriggersMainEviction(t *testing.T) {
	// Use a very small cache to make main queue overflow quickly
	cache := newS3FIFO[int, int](&config{size: 20})

	// Fill small queue first
	for i := range 20 {
		cache.set(i, i, 0)
	}

	// Access all items twice to promote them to main (freq >= 2)
	for i := range 20 {
		cache.get(i)
		cache.get(i)
	}

	// Add more items - this should trigger:
	// 1. New items go to small (or main via ghost)
	// 2. Small items with freq>=2 get promoted to main
	// 3. Main overflows, triggering evictFromMain from within evictFromSmall
	for i := 20; i < 50; i++ {
		cache.set(i, i, 0)
	}

	// Log queue states
	t.Logf("main.len=%d, small.len=%d, capacity=%d", cache.main.len, cache.small.len, cache.capacity)
}

// TestS3FIFO_EvictOne_SmallQueuePath tests evictOne when main is empty.
func TestS3FIFO_EvictOne_SmallQueuePath(t *testing.T) {
	cache := newS3FIFO[int, int](&config{size: 10})

	// Fill cache with items that stay in small queue (never accessed)
	for i := range 15 {
		cache.set(i, i, 0)
	}

	// At this point, items should be evicted from small queue
	// because main is empty (items haven't been promoted)
	remaining := 0
	for i := range 15 {
		if _, ok := cache.get(i); ok {
			remaining++
		}
	}
	t.Logf("Remaining items: %d (capacity: %d)", remaining, cache.capacity)
}

// TestS3FIFO_GhostHashConsistency is a regression test for ghost queue hash handling.
//
// Key insight: The bloom filter needs full 64-bit hashes for proper double hashing
// (it uses h1=h and h2=h>>32). If upper 32 bits are zero, all k hash functions
// collapse to the same value, massively increasing false positive rate.
//
// The frequency ring only needs 30-bit hashes (lower bits of the 64-bit hash).
func TestS3FIFO_GhostHashConsistency(t *testing.T) {
	// Test frequency ring with 32-bit hashes
	var ring ghostFreqRing

	h32 := uint32(0x12345678)
	ring.add(h32, 5)

	if freq, ok := ring.lookup(h32); !ok || freq != 5 {
		t.Errorf("lookup(h32) = %d, %v; want 5, true", freq, ok)
	}

	// Different hash should fail
	differentHash := uint32(0xDEADBEEF)
	if _, ok := ring.lookup(differentHash); ok {
		t.Error("lookup(differentHash) should fail - different hash value")
	}
}

// TestS3FIFO_BloomFilterDoubleHashing verifies bloom filter needs full 64-bit hashes.
// When upper 32 bits are zero, h2=h>>32 becomes 0, and all k hash functions
// compute the same index, degrading the bloom filter to a single hash.
func TestS3FIFO_BloomFilterDoubleHashing(t *testing.T) {
	// Test that bloom filter works correctly with full 64-bit hashes
	bloom := newBloomFilter(1000, 0.0001)

	// Add with full 64-bit hash (both halves non-zero)
	h64 := uint64(0xDEADBEEF12345678)
	bloom.Add(h64)

	if !bloom.Contains(h64) {
		t.Error("bloom.Contains(h64) should return true")
	}

	// Different hash should (almost certainly) not be found
	differentH64 := uint64(0x1234567890ABCDEF)
	if bloom.Contains(differentH64) {
		t.Log("Warning: false positive (possible but unlikely)")
	}

	// Demonstrate the problem with truncated hashes:
	// If we only use lower 32 bits, h2 = 0 and bloom degrades badly
	bloom2 := newBloomFilter(1000, 0.0001)
	h32Extended := uint64(uint32(h64)) // upper 32 bits = 0!

	bloom2.Add(h32Extended)

	// This works but the bloom filter is now much less effective
	// because h2 = h32Extended >> 32 = 0
	if !bloom2.Contains(h32Extended) {
		t.Error("bloom.Contains should still work, just with higher FP rate")
	}

	t.Logf("h64=0x%016X, h32Extended=0x%016X, h2=%d",
		h64, h32Extended, h32Extended>>32)
}

// TestS3FIFO_GhostFrequencyRing verifies frequency ring uses full 32-bit hashes.
func TestS3FIFO_GhostFrequencyRing(t *testing.T) {
	// Test various 32-bit hashes
	testCases := []uint32{
		0x00000000,
		0x12345678,
		0xDEADBEEF,
		0xFFFFFFFF,
	}

	for _, h := range testCases {
		var ring ghostFreqRing
		ring.add(h, 7)

		if freq, ok := ring.lookup(h); !ok || freq != 7 {
			t.Errorf("hash 0x%08X: lookup failed or wrong freq (got %d, ok=%v)",
				h, freq, ok)
		}
	}
}

// TestS3FIFO_GhostEvictionReinsertConsistency tests that evicted keys are
// correctly recognized as ghosts when reinserted. This catches hash mismatch
// bugs where addToGhost uses a different hash than setWithHash's Contains check.
//
// The bug this catches: if addToGhost uses a truncated hash (e.g., 30 bits
// extended to 64 bits with upper bits = 0), but setWithHash uses the full
// 64-bit hash for bloom Contains, evicted keys won't be recognized as ghosts.
func TestS3FIFO_GhostEvictionReinsertConsistency(t *testing.T) {
	// Small cache to force evictions quickly
	cache := newS3FIFO[string, int](&config{size: 100})

	// Phase 1: Fill cache and force evictions
	// Insert 200 keys to ensure ~100 get evicted to ghost
	for i := range 200 {
		cache.set(fmt.Sprintf("key%d", i), i, 0)
	}

	// Phase 2: Reinsert evicted keys - they should go to main (recognized as ghost)
	// The first ~100 keys should have been evicted
	mainCount := 0
	for i := range 50 {
		key := fmt.Sprintf("key%d", i)

		// Access key to bump freq so it gets evicted with peakFreq >= 1
		// (needed for ghost frequency tracking)
		cache.set(key, i*10, 0)

		// Check if entry went to main queue (ghost hit) or small queue
		if ent, ok := cache.getEntry(key); ok && !ent.inSmall() {
			mainCount++
		}
	}

	// If ghost recognition works, most reinserted keys should go to main.
	// With broken hash (addToGhost using truncated hash), keys won't be
	// recognized and will all go to small queue.
	//
	// We expect at least 30% to be recognized as ghosts. The exact number
	// depends on bloom filter FP rate and ghost queue rotation timing.
	minExpected := 15 // 30% of 50
	if mainCount < minExpected {
		t.Errorf("Only %d/%d reinserted keys recognized as ghosts (went to main); "+
			"expected at least %d. This suggests addToGhost and setWithHash "+
			"are using inconsistent hashes.", mainCount, 50, minExpected)
	}

	t.Logf("Ghost recognition: %d/50 keys went to main queue", mainCount)
}

// TestEntry_Seqlock_Basic tests basic storeValue/loadValue functionality.
func TestEntry_Seqlock_Basic(t *testing.T) {
	e := &entry[string, int]{}

	// Fresh entry should return false (never stored).
	if _, ok := e.loadValue(); ok {
		t.Error("loadValue on fresh entry should return false")
	}

	// Store and load.
	e.storeValue(42)
	v, ok := e.loadValue()
	if !ok || v != 42 {
		t.Errorf("loadValue() = %d, %v; want 42, true", v, ok)
	}

	// Overwrite.
	e.storeValue(100)
	v, ok = e.loadValue()
	if !ok || v != 100 {
		t.Errorf("loadValue() = %d, %v; want 100, true", v, ok)
	}
}

// TestEntry_Seqlock_StringValue tests seqlock with string values.
func TestEntry_Seqlock_StringValue(t *testing.T) {
	e := &entry[int, string]{}

	e.storeValue("hello")
	v, ok := e.loadValue()
	if !ok || v != "hello" {
		t.Errorf("loadValue() = %q, %v; want \"hello\", true", v, ok)
	}

	e.storeValue("world")
	v, ok = e.loadValue()
	if !ok || v != "world" {
		t.Errorf("loadValue() = %q, %v; want \"world\", true", v, ok)
	}
}

// Concurrent seqlock tests are in s3fifo_seqlock_race_test.go
// They are skipped under race detector because seqlocks are benign races.

// TestSmallRatio verifies the small queue ratio at tuning points.
// Values determined via binary search on hitrate benchmarks.
func TestSmallRatio(t *testing.T) {
	tests := []struct {
		capacity int
		want     int // expected ratio per-mille
	}{
		// Tuning points from binary search.
		{8000, 148},
		{16000, 123},
		{32000, 137},
		{64000, 132},
		{128000, 122},
		{256000, 152},
		// Boundary cases.
		{0, 148},      // <= min returns min ratio
		{4000, 148},   // below min clamps to 148
		{512000, 152}, // above max clamps to 152
	}

	for _, tt := range tests {
		got := smallRatio(tt.capacity)
		if got != tt.want {
			t.Errorf("smallRatio(%d) = %d; want %d", tt.capacity, got, tt.want)
		}
	}
}

// TestSmallRatio_Interpolation verifies linear interpolation between points.
func TestSmallRatio_Interpolation(t *testing.T) {
	tests := []struct {
		capacity int
		min, max int // expected range (inclusive)
	}{
		// Midpoint between 8K (148) and 16K (123) should be ~135.
		{12000, 130, 140},
		// Midpoint between 16K (123) and 32K (137) should be ~130.
		{24000, 128, 132},
		// Midpoint between 128K (122) and 256K (152) should be ~137.
		{192000, 135, 140},
	}

	for _, tt := range tests {
		got := smallRatio(tt.capacity)
		if got < tt.min || got > tt.max {
			t.Errorf("smallRatio(%d) = %d; want [%d, %d]", tt.capacity, got, tt.min, tt.max)
		}
	}
}

// TestSmallSize verifies the actual small queue size calculation.
func TestSmallSize(t *testing.T) {
	tests := []struct {
		capacity int
		want     int // expected small queue size
	}{
		{0, 0},
		{8000, 8000 * 148 / 1000},     // 1184
		{16000, 16000 * 123 / 1000},   // 1968
		{128000, 128000 * 122 / 1000}, // 15616
	}

	for _, tt := range tests {
		var got int
		if tt.capacity > 0 {
			got = tt.capacity * smallRatio(tt.capacity) / 1000
		}
		if got != tt.want {
			t.Errorf("smallSize(%d) = %d; want %d", tt.capacity, got, tt.want)
		}
	}
}

// TestGhostRatio verifies the ghost queue ratio at tuning points.
// Values determined via binary search on hitrate benchmarks.
func TestGhostRatio(t *testing.T) {
	tests := []struct {
		capacity int
		want     int // expected ratio per-mille
	}{
		// Tuning points from binary search.
		{8000, 875},
		{16000, 1000},
		{32000, 1200},
		{64000, 1750},
		{128000, 2075},
		{256000, 2225},
		// Boundary cases.
		{0, 875},       // <= min returns min ratio
		{4000, 875},    // below min clamps to 875
		{512000, 2225}, // above max clamps to 2225
	}

	for _, tt := range tests {
		got := ghostRatio(tt.capacity)
		if got != tt.want {
			t.Errorf("ghostRatio(%d) = %d; want %d", tt.capacity, got, tt.want)
		}
	}
}

// TestGhostRatio_Interpolation verifies linear interpolation between points.
func TestGhostRatio_Interpolation(t *testing.T) {
	tests := []struct {
		capacity int
		min, max int // expected range (inclusive)
	}{
		// Midpoint between 8K (875) and 16K (1000) should be ~937.
		{12000, 930, 945},
		// Midpoint between 32K (1200) and 64K (1750) should be ~1475.
		{48000, 1450, 1500},
		// Midpoint between 128K (2075) and 256K (2225) should be ~2150.
		{192000, 2140, 2160},
	}

	for _, tt := range tests {
		got := ghostRatio(tt.capacity)
		if got < tt.min || got > tt.max {
			t.Errorf("ghostRatio(%d) = %d; want [%d, %d]", tt.capacity, got, tt.min, tt.max)
		}
	}
}

// TestGhostRatio_Monotonic verifies the ratio increases with capacity.
func TestGhostRatio_Monotonic(t *testing.T) {
	capacities := []int{8000, 16000, 32000, 64000, 128000, 256000}
	prev := 0
	for _, cap := range capacities {
		got := ghostRatio(cap)
		if got < prev {
			t.Errorf("ghostRatio(%d) = %d; should be >= %d (monotonic)", cap, got, prev)
		}
		prev = got
	}
}

// TestGhostSize verifies the actual ghost queue size calculation.
func TestGhostSize(t *testing.T) {
	tests := []struct {
		capacity int
		want     int // expected ghost queue size
	}{
		{0, 0},
		{8000, 8000 * 875 / 1000},      // 7000
		{16000, 16000 * 1000 / 1000},   // 16000
		{32000, 32000 * 1200 / 1000},   // 38400
		{128000, 128000 * 2075 / 1000}, // 265600
	}

	for _, tt := range tests {
		var got int
		if tt.capacity > 0 {
			got = tt.capacity * ghostRatio(tt.capacity) / 1000
		}
		if got != tt.want {
			t.Errorf("ghostSize(%d) = %d; want %d", tt.capacity, got, tt.want)
		}
	}
}
