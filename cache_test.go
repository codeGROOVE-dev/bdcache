package bdcache

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestCache_MemoryOnly(t *testing.T) {
	ctx := context.Background()
	cache, err := New[string, int](ctx)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := cache.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

	// Test Set and Get
	if err := cache.Set(ctx, "key1", 42, 0); err != nil {
		t.Fatalf("Set: %v", err)
	}

	val, found, err := cache.Get(ctx, "key1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found {
		t.Fatal("key1 not found")
	}
	if val != 42 {
		t.Errorf("Get value = %d; want 42", val)
	}

	// Test miss
	_, found, err = cache.Get(ctx, "missing")
	if err != nil {
		t.Fatalf("Get missing: %v", err)
	}
	if found {
		t.Error("missing key should not be found")
	}

	// Test delete
	cache.Delete(ctx, "key1")

	_, found, err = cache.Get(ctx, "key1")
	if err != nil {
		t.Fatalf("Get after delete: %v", err)
	}
	if found {
		t.Error("deleted key should not be found")
	}
}

func TestCache_WithTTL(t *testing.T) {
	ctx := context.Background()
	cache, err := New[string, string](ctx)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := cache.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

	// Set with short TTL
	if err := cache.Set(ctx, "temp", "value", 50*time.Millisecond); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Should be available immediately
	val, found, err := cache.Get(ctx, "temp")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found || val != "value" {
		t.Error("temp should be found immediately")
	}

	// Wait for expiration
	time.Sleep(100 * time.Millisecond)

	// Should be expired
	_, found, err = cache.Get(ctx, "temp")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if found {
		t.Error("temp should be expired")
	}
}

func TestCache_DefaultTTL(t *testing.T) {
	ctx := context.Background()
	cache, err := New[string, int](ctx, WithDefaultTTL(50*time.Millisecond))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := cache.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

	// Set without explicit TTL (ttl=0 uses default)
	if err := cache.Set(ctx, "key", 100, 0); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Should be available immediately
	_, found, err := cache.Get(ctx, "key")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found {
		t.Error("key should be found immediately")
	}

	// Wait for default TTL expiration
	time.Sleep(100 * time.Millisecond)

	// Should be expired
	_, found, err = cache.Get(ctx, "key")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if found {
		t.Error("key should be expired after default TTL")
	}
}

func TestCache_Cleanup(t *testing.T) {
	ctx := context.Background()
	cache, err := New[string, int](ctx)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := cache.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

	// Add expired and valid entries
	if err := cache.Set(ctx, "expired1", 1, 1*time.Millisecond); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := cache.Set(ctx, "expired2", 2, 1*time.Millisecond); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := cache.Set(ctx, "valid", 3, 1*time.Hour); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Wait for expiration
	time.Sleep(10 * time.Millisecond)

	// Run cleanup
	removed := cache.Cleanup()
	if removed != 2 {
		t.Errorf("Cleanup removed %d items; want 2", removed)
	}

	// Valid entry should still exist
	_, found, err := cache.Get(ctx, "valid")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found {
		t.Error("valid entry should still exist")
	}
}

func TestCache_Concurrent(t *testing.T) {
	ctx := context.Background()
	cache, err := New[int, int](ctx, WithMemorySize(1000))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := cache.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

	var wg sync.WaitGroup

	// Concurrent writers
	for i := range 10 {
		wg.Add(1)
		go func(offset int) {
			defer wg.Done()
			for j := range 100 {
				if err := cache.Set(ctx, offset*100+j, j, 0); err != nil {
					t.Errorf("Set: %v", err)
				}
			}
		}(i)
	}

	// Concurrent readers
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 100 {
				_, _, _ = cache.Get(ctx, j) //nolint:errcheck // Intentionally ignoring errors in concurrent stress test
			}
		}()
	}

	wg.Wait()

	// Cache should be at or near capacity
	if cache.Len() > 1000 {
		t.Errorf("cache length = %d; should not exceed capacity", cache.Len())
	}
}

func TestCache_Len(t *testing.T) {
	ctx := context.Background()
	cache, err := New[string, int](ctx)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := cache.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

	if cache.Len() != 0 {
		t.Errorf("initial length = %d; want 0", cache.Len())
	}

	if err := cache.Set(ctx, "a", 1, 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := cache.Set(ctx, "b", 2, 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := cache.Set(ctx, "c", 3, 0); err != nil {
		t.Fatalf("Set: %v", err)
	}

	if cache.Len() != 3 {
		t.Errorf("length = %d; want 3", cache.Len())
	}

	cache.Delete(ctx, "b")

	if cache.Len() != 2 {
		t.Errorf("length after delete = %d; want 2", cache.Len())
	}
}

func BenchmarkCache_Set(b *testing.B) {
	ctx := context.Background()
	cache, err := New[int, int](ctx)
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	defer func() {
		if err := cache.Close(); err != nil {
			b.Logf("Close error: %v", err)
		}
	}()

	b.ResetTimer()
	for i := range b.N {
		if err := cache.Set(ctx, i%10000, i, 0); err != nil {
			b.Fatalf("Set: %v", err)
		}
	}
}

func BenchmarkCache_Get_Hit(b *testing.B) {
	ctx := context.Background()
	cache, err := New[int, int](ctx)
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	defer func() {
		if err := cache.Close(); err != nil {
			b.Logf("Close error: %v", err)
		}
	}()

	// Populate cache
	for i := range 10000 {
		if err := cache.Set(ctx, i, i, 0); err != nil {
			b.Fatalf("Set: %v", err)
		}
	}

	b.ResetTimer()
	for i := range b.N {
		_, _, _ = cache.Get(ctx, i%10000) //nolint:errcheck // Benchmarking performance, errors not critical
	}
}

func BenchmarkCache_Get_Miss(b *testing.B) {
	ctx := context.Background()
	cache, err := New[int, int](ctx)
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	defer func() {
		if err := cache.Close(); err != nil {
			b.Logf("Close error: %v", err)
		}
	}()

	b.ResetTimer()
	for i := range b.N {
		_, _, _ = cache.Get(ctx, i) //nolint:errcheck // Benchmarking performance, errors not critical
	}
}

func BenchmarkCache_Mixed(b *testing.B) {
	ctx := context.Background()
	cache, err := New[int, int](ctx)
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	defer func() {
		if err := cache.Close(); err != nil {
			b.Logf("Close error: %v", err)
		}
	}()

	b.ResetTimer()
	for i := range b.N {
		if i%3 == 0 {
			if err := cache.Set(ctx, i%10000, i, 0); err != nil {
				b.Fatalf("Set: %v", err)
			}
		} else {
			_, _, _ = cache.Get(ctx, i%10000) //nolint:errcheck // Benchmarking performance, errors not critical
		}
	}
}

func TestCache_New_DefaultOptions(t *testing.T) {
	ctx := context.Background()

	cache, err := New[string, int](ctx)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := cache.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

	if cache.opts.MemorySize != 10000 {
		t.Errorf("default memory size = %d; want 10000", cache.opts.MemorySize)
	}

	if cache.opts.DefaultTTL != 0 {
		t.Errorf("default TTL = %v; want 0", cache.opts.DefaultTTL)
	}

	if cache.persist != nil {
		t.Error("persist should be nil with default options")
	}
}

func TestCache_SetDefaultWithExplicitTTL(t *testing.T) {
	ctx := context.Background()

	cache, err := New[string, int](ctx, WithDefaultTTL(1*time.Hour))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := cache.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

	// Set with ttl=0 should use the default TTL
	if err := cache.Set(ctx, "key1", 42, 0); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Verify it's set
	val, found, err := cache.Get(ctx, "key1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found || val != 42 {
		t.Errorf("Get = %v, %v; want 42, true", val, found)
	}
}

func TestCache_SetExplicitTTLOverridesDefault(t *testing.T) {
	ctx := context.Background()

	cache, err := New[string, int](ctx, WithDefaultTTL(1*time.Hour))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := cache.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

	// Set with explicit short TTL (overrides default)
	if err := cache.Set(ctx, "key1", 42, 50*time.Millisecond); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Should exist immediately
	_, found, err := cache.Get(ctx, "key1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found {
		t.Error("key1 should exist immediately")
	}

	// Wait for explicit TTL to expire (not default)
	time.Sleep(100 * time.Millisecond)

	// Should be expired
	_, found, err = cache.Get(ctx, "key1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if found {
		t.Error("key1 should be expired after explicit TTL")
	}
}

func TestCache_SetAsync(t *testing.T) {
	ctx := context.Background()
	cache, err := New[string, int](ctx)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := cache.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

	// SetAsync should return immediately
	if err := cache.SetAsync(ctx, "key1", 42, 0); err != nil {
		t.Fatalf("SetAsync: %v", err)
	}

	// Value should be immediately available in memory
	val, found, err := cache.Get(ctx, "key1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found {
		t.Error("key1 should be found after SetAsync")
	}
	if val != 42 {
		t.Errorf("Get value = %d; want 42", val)
	}
}

func TestCache_WithOptions(t *testing.T) {
	ctx := context.Background()

	// Test WithMemorySize
	cache, err := New[string, int](ctx, WithMemorySize(500))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if cache.opts.MemorySize != 500 {
		t.Errorf("memory size = %d; want 500", cache.opts.MemorySize)
	}
	cache.Close()

	// Test WithDefaultTTL
	cache, err = New[string, int](ctx, WithDefaultTTL(5*time.Minute))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if cache.opts.DefaultTTL != 5*time.Minute {
		t.Errorf("default TTL = %v; want 5m", cache.opts.DefaultTTL)
	}
	cache.Close()

	// Test WithWarmup
	cache, err = New[string, int](ctx, WithWarmup(100))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if cache.opts.WarmupLimit != 100 {
		t.Errorf("warmup limit = %d; want 100", cache.opts.WarmupLimit)
	}
	cache.Close()

	// Test WithCleanup
	cache, err = New[string, int](ctx, WithCleanup(1*time.Hour))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !cache.opts.CleanupEnabled {
		t.Error("cleanup should be enabled")
	}
	if cache.opts.CleanupMaxAge != 1*time.Hour {
		t.Errorf("cleanup max age = %v; want 1h", cache.opts.CleanupMaxAge)
	}
	cache.Close()
}

func TestCache_DeleteNonExistent(t *testing.T) {
	ctx := context.Background()
	cache, err := New[string, int](ctx)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer cache.Close()

	// Delete non-existent key should not error
	cache.Delete(ctx, "does-not-exist")

	// Cache should still work
	if err := cache.Set(ctx, "key1", 42, 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	val, found, _ := cache.Get(ctx, "key1")
	if !found || val != 42 {
		t.Error("cache should still work after deleting non-existent key")
	}
}

func TestCache_EvictFromMain(t *testing.T) {
	ctx := context.Background()
	// Small cache to force eviction from main queue
	cache, err := New[int, int](ctx, WithMemorySize(10))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer cache.Close()

	// Fill small queue and promote items to main by accessing them twice
	for i := range 15 {
		cache.Set(ctx, i, i, 0)
		// Access immediately to promote to main
		cache.Get(ctx, i)
	}

	// Add more items to force eviction from main queue
	for i := 100; i < 110; i++ {
		cache.Set(ctx, i, i, 0)
		cache.Get(ctx, i)
	}

	// Cache should not exceed capacity
	if cache.Len() > 10 {
		t.Errorf("cache length = %d; should not exceed 10", cache.Len())
	}
}

func TestCache_GetExpired(t *testing.T) {
	ctx := context.Background()
	cache, err := New[string, int](ctx)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer cache.Close()

	// Set with very short TTL
	if err := cache.Set(ctx, "key1", 42, 1*time.Millisecond); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Wait for expiration
	time.Sleep(10 * time.Millisecond)

	// Get should return not found
	_, found, err := cache.Get(ctx, "key1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if found {
		t.Error("expired key should not be found")
	}
}

func TestCache_SetUpdateExisting(t *testing.T) {
	ctx := context.Background()
	cache, err := New[string, int](ctx)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer cache.Close()

	// Set initial value
	if err := cache.Set(ctx, "key1", 42, 0); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Update value
	if err := cache.Set(ctx, "key1", 100, 0); err != nil {
		t.Fatalf("Set update: %v", err)
	}

	// Should have new value
	val, found, err := cache.Get(ctx, "key1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found {
		t.Error("key1 should be found")
	}
	if val != 100 {
		t.Errorf("Get value = %d; want 100", val)
	}
}
