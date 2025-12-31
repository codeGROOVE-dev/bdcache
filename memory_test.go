package multicache

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCache_Basic(t *testing.T) {
	cache := New[string, int]()
	defer cache.Close()

	// Test Set and Get
	cache.Set("key1", 42, 0)

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
	defer cache.Close()

	// Set with short TTL
	cache.Set("temp", "value", 50*time.Millisecond)

	// Should be available immediately
	val, found := cache.Get("temp")
	if !found || val != "value" {
		t.Error("temp should be found immediately")
	}

	// Wait for expiration
	time.Sleep(100 * time.Millisecond)

	// Should be expired
	_, found = cache.Get("temp")
	if found {
		t.Error("temp should be expired")
	}
}

func TestCache_DefaultTTL(t *testing.T) {
	cache := New[string, int](TTL(50 * time.Millisecond))
	defer cache.Close()

	// Set without explicit TTL (ttl=0 uses default)
	cache.Set("key", 100, 0)

	// Should be available immediately
	_, found := cache.Get("key")
	if !found {
		t.Error("key should be found immediately")
	}

	// Wait for default TTL expiration
	time.Sleep(100 * time.Millisecond)

	// Should be expired
	_, found = cache.Get("key")
	if found {
		t.Error("key should be expired after default TTL")
	}
}

func TestCache_Concurrent(t *testing.T) {
	cache := New[int, int](Size(1000))
	defer cache.Close()

	var wg sync.WaitGroup

	// Concurrent writers
	for i := range 10 {
		wg.Add(1)
		go func(offset int) {
			defer wg.Done()
			for j := range 100 {
				cache.Set(offset*100+j, j, 0)
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
	defer cache.Close()

	if cache.Len() != 0 {
		t.Errorf("initial length = %d; want 0", cache.Len())
	}

	cache.Set("a", 1, 0)
	cache.Set("b", 2, 0)
	cache.Set("c", 3, 0)

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
	defer cache.Close()

	b.ResetTimer()
	for i := range b.N {
		cache.Set(i%10000, i, 0)
	}
}

func BenchmarkCache_Get_Hit(b *testing.B) {
	cache := New[int, int]()
	defer cache.Close()

	// Populate cache
	for i := range 10000 {
		cache.Set(i, i, 0)
	}

	b.ResetTimer()
	for i := range b.N {
		cache.Get(i % 10000)
	}
}

func BenchmarkCache_Get_Miss(b *testing.B) {
	cache := New[int, int]()
	defer cache.Close()

	b.ResetTimer()
	for i := range b.N {
		cache.Get(i)
	}
}

func BenchmarkCache_Mixed(b *testing.B) {
	cache := New[int, int]()
	defer cache.Close()

	b.ResetTimer()
	for i := range b.N {
		if i%3 == 0 {
			cache.Set(i%10000, i, 0)
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
	cache.Close()

	// Test TTL
	cache = New[string, int](TTL(5 * time.Minute))
	if cache.defaultTTL != 5*time.Minute {
		t.Errorf("default TTL = %v; want 5m", cache.defaultTTL)
	}
	cache.Close()
}

func TestCache_DeleteNonExistent(t *testing.T) {
	cache := New[string, int]()
	defer cache.Close()

	// Delete non-existent key should not panic
	cache.Delete("does-not-exist")

	// Cache should still work
	cache.Set("key1", 42, 0)
	val, found := cache.Get("key1")
	if !found || val != 42 {
		t.Error("cache should still work after deleting non-existent key")
	}
}

func TestCache_EvictFromMain(t *testing.T) {
	// Cache with 20000 capacity (approx 10 per shard with 2048 shards)
	cache := New[int, int](Size(20000))
	defer cache.Close()

	// Fill cache and promote items to main by accessing them
	for i := range 25000 {
		cache.Set(i, i, 0)
		// Access immediately to promote to main
		cache.Get(i)
	}

	// Add more items to force eviction from main queue
	for i := range 10000 {
		cache.Set(i+30000, i+30000, 0)
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
	defer cache.Close()

	// Set with very short TTL
	cache.Set("key1", 42, 1*time.Millisecond)

	// Wait for expiration
	time.Sleep(10 * time.Millisecond)

	// Get should return not found
	_, found := cache.Get("key1")
	if found {
		t.Error("expired key should not be found")
	}
}

func TestCache_SetUpdateExisting(t *testing.T) {
	cache := New[string, int]()
	defer cache.Close()

	// Set initial value
	cache.Set("key1", 42, 0)

	// Update value
	cache.Set("key1", 100, 0)

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
	defer cache.Close()

	// Add entries
	for i := range 10 {
		cache.Set(fmt.Sprintf("key%d", i), i, 0)
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
	defer cache.Close()

	// Flush empty cache
	removed := cache.Flush()
	if removed != 0 {
		t.Errorf("Flush removed %d items; want 0", removed)
	}
}

func TestCache_GhostQueue(t *testing.T) {
	// Small capacity to force ghost queue usage
	cache := New[string, int](Size(10))
	defer cache.Close()

	// Fill small queue (10% of 10 = 1)
	// Insert items to trigger ghost queue
	for i := range 20 {
		cache.Set(fmt.Sprintf("key%d", i), i, 0)
	}

	// Access some items to create hot items
	for i := range 5 {
		cache.Get(fmt.Sprintf("key%d", i))
	}

	// Insert more to trigger evictions from small queue to ghost queue
	for i := range 15 {
		cache.Set(fmt.Sprintf("key%d", i+20), i+20, 0)
	}

	// Test should complete without panic
	t.Logf("Cache length: %d", cache.Len())
}

func TestCache_MainQueueEviction(t *testing.T) {
	// Create cache with 20000 capacity (approx 10 per shard with 2048 shards)
	cache := New[string, int](Size(20000))
	defer cache.Close()

	// Insert and access items to get them into Main queue
	for i := range 25000 {
		key := fmt.Sprintf("key%d", i)
		cache.Set(key, i, 0)
		// Access to promote to Main
		cache.Get(key)
	}

	// Insert more items to trigger eviction from Main queue
	for i := range 10000 {
		key := fmt.Sprintf("key%d", i+30000)
		cache.Set(key, i+30000, 0)
		cache.Get(key)
	}

	// Verify cache is at capacity (with 2048 shards, effective capacity is ~20480)
	if cache.Len() > 20480 {
		t.Errorf("Cache length %d exceeds capacity 20480", cache.Len())
	}
}

func TestCache_Set_VariadicTTL(t *testing.T) {
	cache := New[string, int](TTL(time.Hour))
	defer cache.Close()

	// Set without TTL - uses default (1 hour, won't expire during test)
	cache.Set("default-ttl", 1)
	if _, found := cache.Get("default-ttl"); !found {
		t.Error("default-ttl should be found")
	}

	// Set with explicit short TTL
	cache.Set("short-ttl", 2, 50*time.Millisecond)
	if _, found := cache.Get("short-ttl"); !found {
		t.Error("short-ttl should be found immediately")
	}

	// Wait for short TTL to expire
	time.Sleep(100 * time.Millisecond)

	// short-ttl should be expired, default-ttl should still exist
	if _, found := cache.Get("short-ttl"); found {
		t.Error("short-ttl should be expired")
	}
	if _, found := cache.Get("default-ttl"); !found {
		t.Error("default-ttl should still exist (1 hour TTL)")
	}
}

func TestCache_Set_NoDefaultTTL(t *testing.T) {
	cache := New[string, int]() // No default TTL
	defer cache.Close()

	// Set without TTL - should never expire
	cache.Set("no-expiry", 42)

	// Wait a bit
	time.Sleep(50 * time.Millisecond)

	// Should still exist
	val, found := cache.Get("no-expiry")
	if !found || val != 42 {
		t.Error("no-expiry should still exist (no TTL means no expiry)")
	}
}

func TestCache_GetSet_Basic(t *testing.T) {
	cache := New[string, int]()
	defer cache.Close()

	loaderCalls := 0
	loader := func() (int, error) {
		loaderCalls++
		return 42, nil
	}

	// First call - should call loader
	val, err := cache.GetSet("key1", loader)
	if err != nil {
		t.Fatalf("GetSet error: %v", err)
	}
	if val != 42 {
		t.Errorf("GetSet value = %d; want 42", val)
	}
	if loaderCalls != 1 {
		t.Errorf("loader calls = %d; want 1", loaderCalls)
	}

	// Second call - should use cached value, not call loader
	val, err = cache.GetSet("key1", loader)
	if err != nil {
		t.Fatalf("GetSet error: %v", err)
	}
	if val != 42 {
		t.Errorf("GetSet value = %d; want 42", val)
	}
	if loaderCalls != 1 {
		t.Errorf("loader calls = %d; want 1 (should use cache)", loaderCalls)
	}
}

func TestCache_GetSet_LoaderError(t *testing.T) {
	cache := New[string, int]()
	defer cache.Close()

	loader := func() (int, error) {
		return 0, fmt.Errorf("loader error")
	}

	_, err := cache.GetSet("key1", loader)
	if err == nil {
		t.Fatal("GetSet should return error from loader")
	}

	// Value should not be cached
	_, found := cache.Get("key1")
	if found {
		t.Error("failed loader should not cache a value")
	}
}

func TestCache_GetSet_ThunderingHerd(t *testing.T) {
	cache := New[string, int]()
	defer cache.Close()

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

	// Launch many concurrent GetSet calls for the same key
	var wg sync.WaitGroup
	results := make([]int, 100)
	errors := make([]error, 100)

	for i := range 100 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx], errors[idx] = cache.GetSet("key1", loader)
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

func TestCache_GetSet_WithTTL(t *testing.T) {
	cache := New[string, int]()
	defer cache.Close()

	loaderCalls := 0
	loader := func() (int, error) {
		loaderCalls++
		return loaderCalls * 10, nil
	}

	// First call with short TTL
	val, err := cache.GetSet("key1", loader, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("GetSet error: %v", err)
	}
	if val != 10 {
		t.Errorf("first GetSet value = %d; want 10", val)
	}

	// Wait for TTL to expire
	time.Sleep(100 * time.Millisecond)

	// Second call - should call loader again (cache expired)
	val, err = cache.GetSet("key1", loader, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("GetSet error: %v", err)
	}
	if val != 20 {
		t.Errorf("second GetSet value = %d; want 20", val)
	}
	if loaderCalls != 2 {
		t.Errorf("loader calls = %d; want 2", loaderCalls)
	}
}

func TestCache_GetSet_IntKeys(t *testing.T) {
	cache := New[int, int](Size(1000))
	defer cache.Close()

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
			val, err := cache.GetSet(123, loader)
			if err != nil {
				t.Errorf("GetSet error: %v", err)
			}
			if val != 42 {
				t.Errorf("GetSet value = %d; want 42", val)
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
		_, err := cache.GetSet(i, func() (int, error) {
			atomic.AddInt32(&loaderCalls, 1)
			return i * 10, nil
		})
		if err != nil {
			t.Fatalf("GetSet key %d error: %v", i, err)
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
			defer cache.Close()

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
			defer cache.Close()

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

func TestCache_SetIfAbsent_Basic(t *testing.T) {
	cache := New[string, int]()
	defer cache.Close()

	// SetIfAbsent on missing key should insert
	val, existed := cache.SetIfAbsent("key1", 42)
	if existed {
		t.Error("key1 should not have existed")
	}
	if val != 42 {
		t.Errorf("SetIfAbsent returned %d; want 42", val)
	}

	// Verify it was stored
	got, found := cache.Get("key1")
	if !found || got != 42 {
		t.Errorf("Get after SetIfAbsent: got %d, found %v; want 42, true", got, found)
	}

	// SetIfAbsent on existing key should return existing value
	val, existed = cache.SetIfAbsent("key1", 100)
	if !existed {
		t.Error("key1 should have existed")
	}
	if val != 42 {
		t.Errorf("SetIfAbsent returned %d; want 42 (original value)", val)
	}

	// Verify value unchanged
	got, found = cache.Get("key1")
	if !found || got != 42 {
		t.Errorf("Get after second SetIfAbsent: got %d, found %v; want 42, true", got, found)
	}
}

func TestCache_SetIfAbsent_WithTTL(t *testing.T) {
	cache := New[string, int](TTL(time.Hour))
	defer cache.Close()

	// SetIfAbsent with explicit TTL
	val, existed := cache.SetIfAbsent("key1", 42, 50*time.Millisecond)
	if existed {
		t.Error("key1 should not have existed")
	}
	if val != 42 {
		t.Errorf("SetIfAbsent returned %d; want 42", val)
	}

	// Value should be available immediately
	if _, found := cache.Get("key1"); !found {
		t.Error("key1 should be found immediately after SetIfAbsent")
	}

	// Wait for TTL to expire
	time.Sleep(100 * time.Millisecond)

	// Value should be expired
	if _, found := cache.Get("key1"); found {
		t.Error("key1 should be expired")
	}
}

func TestCache_SetIfAbsent_NoTTL(t *testing.T) {
	cache := New[string, int]() // No default TTL
	defer cache.Close()

	// SetIfAbsent without TTL on cache with no default
	val, existed := cache.SetIfAbsent("key1", 42)
	if existed {
		t.Error("key1 should not have existed")
	}
	if val != 42 {
		t.Errorf("SetIfAbsent returned %d; want 42", val)
	}

	// Value should persist indefinitely (no expiry)
	time.Sleep(50 * time.Millisecond)
	if _, found := cache.Get("key1"); !found {
		t.Error("key1 should still exist (no TTL)")
	}
}

func TestCache_SetIfAbsent_Concurrent(t *testing.T) {
	cache := New[int, int](Size(1000))
	defer cache.Close()

	var wg sync.WaitGroup
	existedCount := atomic.Int32{}
	notExistedCount := atomic.Int32{}

	// Many goroutines try to SetIfAbsent the same key
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, existed := cache.SetIfAbsent(42, 100)
			if existed {
				existedCount.Add(1)
			} else {
				notExistedCount.Add(1)
			}
		}()
	}

	wg.Wait()

	// At least some should have seen it as existing (not all created it)
	// SetIfAbsent is not fully atomic (get then set), so multiple may "win"
	if existedCount.Load() < 50 {
		t.Errorf("existedCount = %d; want >= 50 (most should see existing)", existedCount.Load())
	}

	// Final value should be 100
	if val, found := cache.Get(42); !found || val != 100 {
		t.Errorf("Get(42) = %d, %v; want 100, true", val, found)
	}

	t.Logf("existedCount = %d, notExistedCount = %d", existedCount.Load(), notExistedCount.Load())
}

func TestCache_Close_NoOp(t *testing.T) {
	cache := New[string, int]()

	// Close should be a no-op for in-memory cache
	cache.Close()

	// Multiple closes should be safe
	cache.Close()
	cache.Close()
}

func TestCache_GetSet_CacheHitDuringSingleflight(t *testing.T) {
	cache := New[string, int](Size(1000))
	defer cache.Close()

	var wg sync.WaitGroup
	loaderCalls := atomic.Int32{}

	// Start first loader that's slow
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = cache.GetSet("key1", func() (int, error) {
			loaderCalls.Add(1)
			// While loader is running, another goroutine populates cache
			time.Sleep(100 * time.Millisecond)
			return 42, nil
		})
	}()

	// Let first goroutine start and enter singleflight
	time.Sleep(10 * time.Millisecond)

	// While first is waiting, directly set the value in cache
	cache.Set("key1", 99)

	// Start second loader that should wait for first
	wg.Add(1)
	go func() {
		defer wg.Done()
		val, _ := cache.GetSet("key1", func() (int, error) {
			loaderCalls.Add(1)
			return 77, nil
		})
		// Second should get either 99 (from cache) or 42 (from first loader)
		if val != 99 && val != 42 {
			t.Errorf("unexpected value: %d", val)
		}
	}()

	wg.Wait()

	t.Logf("loader calls: %d", loaderCalls.Load())
}

func TestCache_GetSet_RaceCondition(t *testing.T) {
	// Test the path where cache is populated between first check and singleflight
	cache := New[string, int](Size(1000))
	defer cache.Close()

	var wg sync.WaitGroup

	// Run many concurrent GetSets with a mix of slow and fast loaders
	for i := range 20 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			key := fmt.Sprintf("key%d", idx%5) // Only 5 unique keys

			val, err := cache.GetSet(key, func() (int, error) {
				if idx%3 == 0 {
					time.Sleep(10 * time.Millisecond)
				}
				return idx * 10, nil
			})
			if err != nil {
				t.Errorf("GetSet error: %v", err)
			}
			if val < 0 {
				t.Errorf("unexpected value: %d", val)
			}
		}(i)
	}

	wg.Wait()
}

// TestCache_GetSet_MemoryHitAfterSingleflightAcquire tests the path where
// the cache is populated between winning singleflight and checking cache again.
func TestCache_GetSet_MemoryHitAfterSingleflightAcquire(t *testing.T) {
	// This is tricky to test because the window is very small.
	// We use a contrived scenario with concurrent access.
	cache := New[string, int](Size(100))
	defer cache.Close()

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

		_, _ = cache.GetSet(key, func() (int, error) {
			// Wait long enough for the second Set to happen
			time.Sleep(50 * time.Millisecond)
			return 1, nil
		})
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

// TestCache_GetSet_WithDefaultTTL tests GetSet using the default TTL.
func TestCache_GetSet_WithDefaultTTL(t *testing.T) {
	cache := New[string, int](TTL(time.Hour))
	defer cache.Close()

	val, err := cache.GetSet("key1", func() (int, error) {
		return 42, nil
	})
	if err != nil {
		t.Fatalf("GetSet error: %v", err)
	}
	if val != 42 {
		t.Errorf("GetSet value = %d; want 42", val)
	}
}

// TestCache_GetSet_DoubleCheckPath attempts to hit the double-check cache hit path.
// This path is triggered when:
// 1. First check misses (no cache hit)
// 2. We win the singleflight (not loaded)
// 3. Another call populated the cache before our double-check
// 4. Double-check finds the value
func TestCache_GetSet_DoubleCheckPath(t *testing.T) {
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
			_, _ = cache.GetSet(key, func() (int, error) {
				loaderCalled.Store(true)
				return 1, nil
			})
		}()

		// Goroutine 2: Directly sets value, racing with goroutine 1
		go func() {
			defer wg.Done()
			cache.Set(key, 99)
		}()

		wg.Wait()
		cache.Close()

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
