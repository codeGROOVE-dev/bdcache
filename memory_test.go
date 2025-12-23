package sfcache

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMemoryCache_Basic(t *testing.T) {
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

func TestMemoryCache_WithTTL(t *testing.T) {
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

func TestMemoryCache_DefaultTTL(t *testing.T) {
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

func TestMemoryCache_Concurrent(t *testing.T) {
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

func TestMemoryCache_Len(t *testing.T) {
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

func BenchmarkMemoryCache_Set(b *testing.B) {
	cache := New[int, int]()
	defer cache.Close()

	b.ResetTimer()
	for i := range b.N {
		cache.Set(i%10000, i, 0)
	}
}

func BenchmarkMemoryCache_Get_Hit(b *testing.B) {
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

func BenchmarkMemoryCache_Get_Miss(b *testing.B) {
	cache := New[int, int]()
	defer cache.Close()

	b.ResetTimer()
	for i := range b.N {
		cache.Get(i)
	}
}

func BenchmarkMemoryCache_Mixed(b *testing.B) {
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

func TestMemoryCache_WithOptions(t *testing.T) {
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

func TestMemoryCache_DeleteNonExistent(t *testing.T) {
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

func TestMemoryCache_EvictFromMain(t *testing.T) {
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

func TestMemoryCache_GetExpired(t *testing.T) {
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

func TestMemoryCache_SetUpdateExisting(t *testing.T) {
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

func TestMemoryCache_Flush(t *testing.T) {
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

func TestMemoryCache_FlushEmpty(t *testing.T) {
	cache := New[string, int]()
	defer cache.Close()

	// Flush empty cache
	removed := cache.Flush()
	if removed != 0 {
		t.Errorf("Flush removed %d items; want 0", removed)
	}
}

func TestMemoryCache_GhostQueue(t *testing.T) {
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

func TestMemoryCache_MainQueueEviction(t *testing.T) {
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

func TestMemoryCache_Set_VariadicTTL(t *testing.T) {
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

func TestMemoryCache_Set_NoDefaultTTL(t *testing.T) {
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

func TestMemoryCache_GetSet_Basic(t *testing.T) {
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

func TestMemoryCache_GetSet_LoaderError(t *testing.T) {
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

func TestMemoryCache_GetSet_ThunderingHerd(t *testing.T) {
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

func TestMemoryCache_GetSet_WithTTL(t *testing.T) {
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

func TestMemoryCache_GetSet_IntKeys(t *testing.T) {
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

func TestMemoryCache_CapacityEfficiency(t *testing.T) {
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

func TestMemoryCache_CapacityEfficiency_StringKeys(t *testing.T) {
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
