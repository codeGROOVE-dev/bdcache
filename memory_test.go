package fido

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCache_Basic(t *testing.T) {
	cache := New[string, int]()

	// Test Set and Get
	cache.Set("key1", 42)

	val, found := cache.Get("key1")
	if !found {
		t.Fatal("key1 not found")
	}
	if val != 42 {
		t.Errorf("Get value = %d; want 42", val)
	}

	// Test miss
	_, found = cache.Get("missing")
	if found {
		t.Error("missing key should not be found")
	}

	// Test delete
	cache.Delete("key1")

	_, found = cache.Get("key1")
	if found {
		t.Error("deleted key should not be found")
	}
}

func TestCache_WithTTL(t *testing.T) {
	cache := New[string, string]()

	// Set with short TTL (minimum 1 second granularity)
	cache.SetTTL("temp", "value", 1*time.Second)

	// Should be available immediately
	val, found := cache.Get("temp")
	if !found || val != "value" {
		t.Error("temp should be found immediately")
	}

	// Wait for expiration
	time.Sleep(2 * time.Second)

	// Should be expired
	_, found = cache.Get("temp")
	if found {
		t.Error("temp should be expired")
	}
}

func TestCache_DefaultTTL(t *testing.T) {
	cache := New[string, int](TTL(1 * time.Second))

	// Set without explicit TTL (ttl=0 uses default)
	cache.Set("key", 100)

	// Should be available immediately
	_, found := cache.Get("key")
	if !found {
		t.Error("key should be found immediately")
	}

	// Wait for default TTL expiration
	time.Sleep(2 * time.Second)

	// Should be expired
	_, found = cache.Get("key")
	if found {
		t.Error("key should be expired after default TTL")
	}
}

func TestCache_Concurrent(t *testing.T) {
	cache := New[int, int](Size(1000))

	var wg sync.WaitGroup

	// Concurrent writers
	for i := range 10 {
		wg.Add(1)
		go func(offset int) {
			defer wg.Done()
			for j := range 100 {
				cache.Set(offset*100+j, j)
			}
		}(i)
	}

	// Concurrent readers
	for range 10 {
		wg.Go(func() {
			for j := range 100 {
				cache.Get(j)
			}
		})
	}

	wg.Wait()

	// Cache should be at or near capacity
	if cache.Len() > 1000 {
		t.Errorf("cache length = %d; should not exceed capacity", cache.Len())
	}
}

func TestCache_Len(t *testing.T) {
	cache := New[string, int]()

	if cache.Len() != 0 {
		t.Errorf("initial length = %d; want 0", cache.Len())
	}

	cache.Set("a", 1)
	cache.Set("b", 2)
	cache.Set("c", 3)

	if cache.Len() != 3 {
		t.Errorf("length = %d; want 3", cache.Len())
	}

	cache.Delete("b")

	if cache.Len() != 2 {
		t.Errorf("length after delete = %d; want 2", cache.Len())
	}
}

func BenchmarkCache_Set(b *testing.B) {
	cache := New[int, int]()

	b.ResetTimer()
	for i := range b.N {
		cache.Set(i%10000, i)
	}
}

func BenchmarkCache_Get_Hit(b *testing.B) {
	cache := New[int, int]()

	// Populate cache
	for i := range 10000 {
		cache.Set(i, i)
	}

	b.ResetTimer()
	for i := range b.N {
		cache.Get(i % 10000)
	}
}

func BenchmarkCache_Get_Miss(b *testing.B) {
	cache := New[int, int]()

	b.ResetTimer()
	for i := range b.N {
		cache.Get(i)
	}
}

func BenchmarkCache_Mixed(b *testing.B) {
	cache := New[int, int]()

	b.ResetTimer()
	for i := range b.N {
		if i%3 == 0 {
			cache.Set(i%10000, i)
		} else {
			cache.Get(i % 10000)
		}
	}
}

func TestCache_WithOptions(t *testing.T) {
	// Test Size
	cache := New[string, int](Size(500))
	if cache.memory == nil {
		t.Error("memory should be initialized")
	}

	// Test TTL
	cache = New[string, int](TTL(5 * time.Minute))
	if cache.defaultTTL != 5*time.Minute {
		t.Errorf("default TTL = %v; want 5m", cache.defaultTTL)
	}
}

func TestCache_DeleteNonExistent(t *testing.T) {
	cache := New[string, int]()

	// Delete non-existent key should not panic
	cache.Delete("does-not-exist")

	// Cache should still work
	cache.Set("key1", 42)
	val, found := cache.Get("key1")
	if !found || val != 42 {
		t.Error("cache should still work after deleting non-existent key")
	}
}

func TestCache_EvictFromMain(t *testing.T) {
	// Cache with 20000 capacity (approx 10 per shard with 2048 shards)
	cache := New[int, int](Size(20000))

	// Fill cache and promote items to main by accessing them
	for i := range 25000 {
		cache.Set(i, i)
		// Access immediately to promote to main
		cache.Get(i)
	}

	// Add more items to force eviction from main queue
	for i := range 10000 {
		cache.Set(i+30000, i+30000)
		cache.Get(i + 30000)
	}

	// Cache should not exceed configured capacity by more than shard overhead
	// With 2048 shards and rounding, effective capacity is ~20480
	if cache.Len() > 20480 {
		t.Errorf("cache length = %d; should not exceed 20480", cache.Len())
	}
}

func TestCache_GetExpired(t *testing.T) {
	cache := New[string, int]()

	// Set with short TTL (1 second granularity)
	cache.SetTTL("key1", 42, 1*time.Second)

	// Wait for expiration
	time.Sleep(2 * time.Second)

	// Get should return not found
	_, found := cache.Get("key1")
	if found {
		t.Error("expired key should not be found")
	}
}

func TestCache_SetUpdateExisting(t *testing.T) {
	cache := New[string, int]()

	// Set initial value
	cache.Set("key1", 42)

	// Update value
	cache.Set("key1", 100)

	// Should have new value
	val, found := cache.Get("key1")
	if !found {
		t.Error("key1 should be found")
	}
	if val != 100 {
		t.Errorf("Get value = %d; want 100", val)
	}
}

func TestCache_Flush(t *testing.T) {
	cache := New[string, int]()

	// Add entries
	for i := range 10 {
		cache.Set(fmt.Sprintf("key%d", i), i)
	}

	if cache.Len() != 10 {
		t.Errorf("cache length = %d; want 10", cache.Len())
	}

	// Flush
	removed := cache.Flush()
	if removed != 10 {
		t.Errorf("Flush removed %d items; want 10", removed)
	}

	// Cache should be empty
	if cache.Len() != 0 {
		t.Errorf("cache length after flush = %d; want 0", cache.Len())
	}

	// All keys should be gone
	for i := range 10 {
		_, found := cache.Get(fmt.Sprintf("key%d", i))
		if found {
			t.Errorf("key%d should not be found after flush", i)
		}
	}
}

func TestCache_FlushEmpty(t *testing.T) {
	cache := New[string, int]()

	// Flush empty cache
	removed := cache.Flush()
	if removed != 0 {
		t.Errorf("Flush removed %d items; want 0", removed)
	}
}

func TestCache_GhostQueue(t *testing.T) {
	// Small capacity to force ghost queue usage
	cache := New[string, int](Size(10))

	// Fill small queue (10% of 10 = 1)
	// Insert items to trigger ghost queue
	for i := range 20 {
		cache.Set(fmt.Sprintf("key%d", i), i)
	}

	// Access some items to create hot items
	for i := range 5 {
		cache.Get(fmt.Sprintf("key%d", i))
	}

	// Insert more to trigger evictions from small queue to ghost queue
	for i := range 15 {
		cache.Set(fmt.Sprintf("key%d", i+20), i+20)
	}

	// Test should complete without panic
	t.Logf("Cache length: %d", cache.Len())
}

func TestCache_MainQueueEviction(t *testing.T) {
	// Create cache with 20000 capacity (approx 10 per shard with 2048 shards)
	cache := New[string, int](Size(20000))

	// Insert and access items to get them into Main queue
	for i := range 25000 {
		key := fmt.Sprintf("key%d", i)
		cache.Set(key, i)
		// Access to promote to Main
		cache.Get(key)
	}

	// Insert more items to trigger eviction from Main queue
	for i := range 10000 {
		key := fmt.Sprintf("key%d", i+30000)
		cache.Set(key, i+30000)
		cache.Get(key)
	}

	// Verify cache is at capacity (with 2048 shards, effective capacity is ~20480)
	if cache.Len() > 20480 {
		t.Errorf("Cache length %d exceeds capacity 20480", cache.Len())
	}
}

func TestCache_SetTTL(t *testing.T) {
	cache := New[string, int](TTL(time.Hour))

	// Set uses default TTL (1 hour, won't expire during test)
	cache.Set("default-ttl", 1)
	if _, found := cache.Get("default-ttl"); !found {
		t.Error("default-ttl should be found")
	}

	// SetTTL uses explicit short TTL (1 second granularity)
	cache.SetTTL("short-ttl", 2, 1*time.Second)
	if _, found := cache.Get("short-ttl"); !found {
		t.Error("short-ttl should be found immediately")
	}

	// SetTTL with zero TTL - should never expire
	cache.SetTTL("zero-ttl", 3, 0)
	if _, found := cache.Get("zero-ttl"); !found {
		t.Error("zero-ttl should be found")
	}

	// SetTTL with negative TTL - should never expire
	cache.SetTTL("neg-ttl", 4, -1*time.Second)
	if _, found := cache.Get("neg-ttl"); !found {
		t.Error("neg-ttl should be found")
	}

	// Wait for short TTL to expire
	time.Sleep(2 * time.Second)

	// short-ttl should be expired, others should still exist
	if _, found := cache.Get("short-ttl"); found {
		t.Error("short-ttl should be expired")
	}
	if _, found := cache.Get("default-ttl"); !found {
		t.Error("default-ttl should still exist (1 hour TTL)")
	}
	if _, found := cache.Get("zero-ttl"); !found {
		t.Error("zero-ttl should still exist (no expiry)")
	}
	if _, found := cache.Get("neg-ttl"); !found {
		t.Error("neg-ttl should still exist (no expiry)")
	}
}

func TestCache_Set_NoDefaultTTL(t *testing.T) {
	cache := New[string, int]() // No default TTL

	// Set without TTL - should never expire
	cache.Set("no-expiry", 42)

	// Wait a bit
	time.Sleep(100 * time.Millisecond)

	// Should still exist
	val, found := cache.Get("no-expiry")
	if !found || val != 42 {
		t.Error("no-expiry should still exist (no TTL means no expiry)")
	}
}

func TestCache_Fetch_Basic(t *testing.T) {
	cache := New[string, int]()

	loaderCalls := 0
	loader := func() (int, error) {
		loaderCalls++
		return 42, nil
	}

	// First call - should call loader
	val, err := cache.Fetch("key1", loader)
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if val != 42 {
		t.Errorf("Fetch value = %d; want 42", val)
	}
	if loaderCalls != 1 {
		t.Errorf("loader calls = %d; want 1", loaderCalls)
	}

	// Second call - should use cached value, not call loader
	val, err = cache.Fetch("key1", loader)
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if val != 42 {
		t.Errorf("Fetch value = %d; want 42", val)
	}
	if loaderCalls != 1 {
		t.Errorf("loader calls = %d; want 1 (should use cache)", loaderCalls)
	}
}

func TestCache_Fetch_LoaderError(t *testing.T) {
	cache := New[string, int]()

	loader := func() (int, error) {
		return 0, fmt.Errorf("loader error")
	}

	_, err := cache.Fetch("key1", loader)
	if err == nil {
		t.Fatal("Fetch should return error from loader")
	}

	// Value should not be cached
	_, found := cache.Get("key1")
	if found {
		t.Error("failed loader should not cache a value")
	}
}

func TestCache_Fetch_ThunderingHerd(t *testing.T) {
	cache := New[string, int]()

	var loaderCalls int32
	var mu sync.Mutex
	loader := func() (int, error) {
		mu.Lock()
		loaderCalls++
		count := loaderCalls // Capture for potential error simulation
		mu.Unlock()
		time.Sleep(50 * time.Millisecond) // Simulate slow operation
		// Return error on hypothetical second call (won't happen due to singleflight)
		if count > 1 {
			return 0, fmt.Errorf("unexpected second call")
		}
		return 42, nil
	}

	// Launch many concurrent Fetch calls for the same key
	var wg sync.WaitGroup
	results := make([]int, 100)
	errors := make([]error, 100)

	for i := range 100 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx], errors[idx] = cache.Fetch("key1", loader)
		}(i)
	}

	wg.Wait()

	// All goroutines should get the same result
	for i := range 100 {
		if errors[i] != nil {
			t.Errorf("goroutine %d error: %v", i, errors[i])
		}
		if results[i] != 42 {
			t.Errorf("goroutine %d result = %d; want 42", i, results[i])
		}
	}

	// Loader should only be called once (thundering herd prevented)
	if loaderCalls != 1 {
		t.Errorf("loader calls = %d; want 1 (thundering herd prevention failed)", loaderCalls)
	}
}

func TestCache_Fetch_WithTTL(t *testing.T) {
	cache := New[string, int]()

	loaderCalls := 0
	loader := func() (int, error) {
		loaderCalls++
		return loaderCalls * 10, nil
	}

	// First call with short TTL (1 second granularity)
	val, err := cache.FetchTTL("key1", 1*time.Second, loader)
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if val != 10 {
		t.Errorf("first Fetch value = %d; want 10", val)
	}

	// Wait for TTL to expire
	time.Sleep(2 * time.Second)

	// Second call - should call loader again (cache expired)
	val, err = cache.FetchTTL("key1", 1*time.Second, loader)
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if val != 20 {
		t.Errorf("second Fetch value = %d; want 20", val)
	}
	if loaderCalls != 2 {
		t.Errorf("loader calls = %d; want 2", loaderCalls)
	}
}

func TestCache_Fetch_IntKeys(t *testing.T) {
	cache := New[int, int](Size(1000))

	var loaderCalls int32
	loader := func() (int, error) { //nolint:unparam // error is always nil in test
		atomic.AddInt32(&loaderCalls, 1)
		time.Sleep(10 * time.Millisecond)
		return 42, nil
	}

	// Test thundering herd with int keys (uses different flightShard path)
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Go(func() {
			// All goroutines request the same key
			val, err := cache.Fetch(123, loader)
			if err != nil {
				t.Errorf("Fetch error: %v", err)
			}
			if val != 42 {
				t.Errorf("Fetch value = %d; want 42", val)
			}
		})
		// Stagger slightly to ensure overlap
		if i%10 == 0 {
			time.Sleep(time.Millisecond)
		}
	}
	wg.Wait()

	if loaderCalls != 1 {
		t.Errorf("loader calls = %d; want 1", loaderCalls)
	}

	// Verify different int keys work independently
	loaderCalls = 0
	for i := range 10 {
		_, err := cache.Fetch(i, func() (int, error) {
			atomic.AddInt32(&loaderCalls, 1)
			return i * 10, nil
		})
		if err != nil {
			t.Fatalf("Fetch key %d error: %v", i, err)
		}
	}

	if loaderCalls != 10 {
		t.Errorf("loader calls = %d; want 10 (one per unique key)", loaderCalls)
	}
}

func TestCache_CapacityEfficiency(t *testing.T) {
	// Test that cache stores exactly the requested capacity.
	// Global capacity tracking ensures 100% efficiency regardless of
	// hash distribution across shards.
	sizes := []int{1000, 16384, 65536}

	for _, size := range sizes {
		t.Run(fmt.Sprintf("size_%d", size), func(t *testing.T) {
			cache := New[int, int](Size(size))

			// Fill to capacity
			for i := range size {
				cache.Set(i, i)
			}

			stored := cache.Len()
			if stored != size {
				t.Errorf("stored %d entries; want exactly %d (100%% efficiency)", stored, size)
			}
		})
	}
}

func TestCache_CapacityEfficiency_StringKeys(t *testing.T) {
	// String keys have different hash distribution - verify 100% efficiency.
	sizes := []int{1000, 16384}

	for _, size := range sizes {
		t.Run(fmt.Sprintf("size_%d", size), func(t *testing.T) {
			cache := New[string, int](Size(size))

			// Fill to capacity with string keys
			for i := range size {
				cache.Set(fmt.Sprintf("user:%d:profile", i), i)
			}

			stored := cache.Len()
			if stored != size {
				t.Errorf("stored %d entries; want exactly %d (100%% efficiency)", stored, size)
			}
		})
	}
}

// TestCache_Fetch_CacheHitDuringSingleflight is in memory_race_test.go
// It tests concurrent cache access which triggers seqlock races.

func TestCache_Fetch_RaceCondition(t *testing.T) {
	// Test the path where cache is populated between first check and singleflight
	cache := New[string, int](Size(1000))

	var wg sync.WaitGroup

	// Run many concurrent Fetchs with a mix of slow and fast loaders
	for i := range 20 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			key := fmt.Sprintf("key%d", idx%5) // Only 5 unique keys

			val, err := cache.Fetch(key, func() (int, error) {
				if idx%3 == 0 {
					time.Sleep(10 * time.Millisecond)
				}
				return idx * 10, nil
			})
			if err != nil {
				t.Errorf("Fetch error: %v", err)
			}
			if val < 0 {
				t.Errorf("unexpected value: %d", val)
			}
		}(i)
	}

	wg.Wait()
}

// TestCache_Fetch_MemoryHitAfterSingleflightAcquire tests the path where
// the cache is populated between winning singleflight and checking cache again.
func TestCache_Fetch_MemoryHitAfterSingleflightAcquire(t *testing.T) {
	// This is tricky to test because the window is very small.
	// We use a contrived scenario with concurrent access.
	cache := New[string, int](Size(100))

	// Key that will be set by another goroutine
	const key = "contested"

	var started sync.WaitGroup
	started.Add(1)

	var done sync.WaitGroup
	done.Add(2)

	// First goroutine: slow loader
	go func() {
		defer done.Done()
		started.Done() // Signal that we've started

		if _, err := cache.Fetch(key, func() (int, error) {
			// Wait long enough for the second Set to happen
			time.Sleep(50 * time.Millisecond)
			return 1, nil
		}); err != nil {
			t.Errorf("Fetch error: %v", err)
		}
	}()

	// Wait for first goroutine to start
	started.Wait()
	time.Sleep(5 * time.Millisecond)

	// Second goroutine: direct Set while first is in loader
	go func() {
		defer done.Done()
		cache.Set(key, 99)
	}()

	done.Wait()

	// Value should be either 99 (from Set) or 1 (from loader)
	if val, ok := cache.Get(key); !ok {
		t.Error("key should exist")
	} else if val != 99 && val != 1 {
		t.Errorf("unexpected value: %d", val)
	}
}

// TestCache_Fetch_WithDefaultTTL tests Fetch using the default TTL.
func TestCache_Fetch_WithDefaultTTL(t *testing.T) {
	cache := New[string, int](TTL(time.Hour))

	val, err := cache.Fetch("key1", func() (int, error) {
		return 42, nil
	})
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if val != 42 {
		t.Errorf("Fetch value = %d; want 42", val)
	}
}

// TestCache_Fetch_DoubleCheckPath attempts to hit the double-check cache hit path.
// This path is triggered when:
// 1. First check misses (no cache hit)
// 2. We win the singleflight (not loaded)
// 3. Another call populated the cache before our double-check
// 4. Double-check finds the value
func TestCache_Fetch_DoubleCheckPath(t *testing.T) {
	var hitCount int
	for iteration := range 1000 {
		cache := New[string, int](Size(100))

		key := fmt.Sprintf("key%d", iteration)
		var loaderCalled atomic.Bool

		var wg sync.WaitGroup
		wg.Add(2)

		// Goroutine 1: Will try to win singleflight
		go func() {
			defer wg.Done()
			if _, err := cache.Fetch(key, func() (int, error) {
				loaderCalled.Store(true)
				return 1, nil
			}); err != nil {
				t.Errorf("Fetch error: %v", err)
			}
		}()

		// Goroutine 2: Directly sets value, racing with goroutine 1
		go func() {
			defer wg.Done()
			cache.Set(key, 99)
		}()

		wg.Wait()

		// If loader wasn't called, we hit the double-check path
		if !loaderCalled.Load() {
			hitCount++
		}
	}
	if hitCount > 0 {
		t.Logf("Hit double-check path %d times out of 1000", hitCount)
	} else {
		t.Log("Could not reliably hit double-check path (race dependent)")
	}
}
