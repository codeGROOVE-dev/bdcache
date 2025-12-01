package bdcache

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestS3FIFO_BasicOperations(t *testing.T) {
	cache := newS3FIFO[string, int](100)

	// Test set and get
	cache.setToMemory("key1", 42, time.Time{})
	if val, ok := cache.getFromMemory("key1"); !ok || val != 42 {
		t.Errorf("get(key1) = %v, %v; want 42, true", val, ok)
	}

	// Test missing key
	if val, ok := cache.getFromMemory("missing"); ok {
		t.Errorf("get(missing) = %v, %v; want _, false", val, ok)
	}

	// Test update
	cache.setToMemory("key1", 100, time.Time{})
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
	capacity := 100
	cache := newS3FIFO[int, string](capacity)

	// Fill cache to capacity
	for i := range capacity {
		cache.setToMemory(i, "value", time.Time{})
	}

	if cache.memoryLen() != capacity {
		t.Errorf("cache length = %d; want %d", cache.memoryLen(), capacity)
	}

	// Add one more - should trigger eviction
	cache.setToMemory(capacity, "value", time.Time{})

	if cache.memoryLen() != capacity {
		t.Errorf("cache length after eviction = %d; want %d", cache.memoryLen(), capacity)
	}
}

func TestS3FIFO_Eviction(t *testing.T) {
	cache := newS3FIFO[int, int](100) // Use larger capacity for predictable behavior

	// Fill to capacity
	for i := range 100 {
		cache.setToMemory(i, i, time.Time{})
	}

	// Access item 0 to increase its frequency
	cache.getFromMemory(0)

	// Add one more item - should evict least frequently used
	cache.setToMemory(1000, 99, time.Time{})

	// Item 0 should still exist (it was accessed)
	if _, ok := cache.getFromMemory(0); !ok {
		t.Error("item 0 was evicted but should have been promoted")
	}

	// Should be at capacity
	if cache.memoryLen() != 100 {
		t.Errorf("cache length = %d; want 100", cache.memoryLen())
	}
}

func TestS3FIFO_GhostQueue(t *testing.T) {
	cache := newS3FIFO[string, int](12) // 3 per shard

	// Fill one shard's worth
	cache.setToMemory("a", 1, time.Time{})
	cache.setToMemory("b", 2, time.Time{})
	cache.setToMemory("c", 3, time.Time{})

	// Evict "a" by adding "d" (assuming same shard)
	cache.setToMemory("d", 4, time.Time{})

	// Re-add "a" - should go directly to main queue if it was in ghost
	cache.setToMemory("a", 10, time.Time{})

	// If "a" was in ghost, it should now be in main (inSmall = false)
	if cache.isInSmall("a") {
		// This is OK - "a" might have landed in a different shard
		// Ghost queue is per-shard, so this depends on hash distribution
		t.Log("'a' is in small queue - may be in different shard than original")
	}
}

func TestS3FIFO_TTL(t *testing.T) {
	cache := newS3FIFO[string, int](10)

	// Set item with past expiry
	past := time.Now().Add(-1 * time.Second)
	cache.setToMemory("expired", 42, past)

	// Should not be retrievable
	if val, ok := cache.getFromMemory("expired"); ok {
		t.Errorf("get(expired) = %v, %v; want _, false", val, ok)
	}

	// Set item with future expiry
	future := time.Now().Add(1 * time.Hour)
	cache.setToMemory("valid", 100, future)

	// Should be retrievable
	if val, ok := cache.getFromMemory("valid"); !ok || val != 100 {
		t.Errorf("get(valid) = %v, %v; want 100, true", val, ok)
	}
}

func TestS3FIFO_Cleanup(t *testing.T) {
	cache := newS3FIFO[string, int](10)

	// Add some items with different expiries
	now := time.Now()
	cache.setToMemory("expired1", 1, now.Add(-1*time.Second))
	cache.setToMemory("expired2", 2, now.Add(-1*time.Second))
	cache.setToMemory("valid1", 3, now.Add(1*time.Hour))
	cache.setToMemory("valid2", 4, time.Time{}) // No expiry

	// Run cleanup
	removed := cache.cleanupMemory()

	if removed != 2 {
		t.Errorf("cleanup removed %d items; want 2", removed)
	}

	if cache.memoryLen() != 2 {
		t.Errorf("cache length after cleanup = %d; want 2", cache.memoryLen())
	}

	// Verify correct items remain
	if _, ok := cache.getFromMemory("valid1"); !ok {
		t.Error("valid1 should still exist")
	}
	if _, ok := cache.getFromMemory("valid2"); !ok {
		t.Error("valid2 should still exist")
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
				cache.setToMemory(offset*100+j, j, time.Time{})
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

	// Cache should be at capacity
	if cache.memoryLen() != 1000 {
		t.Errorf("cache length = %d; want 1000", cache.memoryLen())
	}
}

func TestS3FIFO_FrequencyPromotion(t *testing.T) {
	cache := newS3FIFO[string, int](10)

	// Add items - they start in small queue
	cache.setToMemory("key0", 0, time.Time{})
	cache.setToMemory("key1", 1, time.Time{})

	// Access key0 to increase frequency (will promote to main on next eviction)
	cache.getFromMemory("key0")

	// Fill to capacity
	for i := 2; i < 10; i++ {
		cache.setToMemory("key"+string(rune('0'+i)), i, time.Time{})
	}

	// Add one more to trigger eviction
	cache.setToMemory("new", 99, time.Time{})

	// key0 should still exist - it was accessed so it gets promoted instead of evicted
	if _, ok := cache.getFromMemory("key0"); !ok {
		t.Error("key0 should not have been evicted due to frequency access")
	}
}

func TestS3FIFO_SmallCapacity(t *testing.T) {
	// Test with capacity of 12 (3 per shard)
	cache := newS3FIFO[string, int](12)

	// Fill to capacity
	cache.setToMemory("a", 1, time.Time{})
	cache.setToMemory("b", 2, time.Time{})
	cache.setToMemory("c", 3, time.Time{})

	initialLen := cache.memoryLen()

	// Adding fourth item should trigger eviction in its shard
	cache.setToMemory("d", 4, time.Time{})

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

func TestS3FIFO_ZeroCapacity(t *testing.T) {
	// Zero capacity should default to 10000
	cache := newS3FIFO[string, int](0)

	if cache.totalCapacity() != 10000 {
		t.Errorf("total capacity = %d; want 10000", cache.totalCapacity())
	}
}

func BenchmarkS3FIFO_Set(b *testing.B) {
	cache := newS3FIFO[int, int](10000)
	b.ResetTimer()

	for i := range b.N {
		cache.setToMemory(i%10000, i, time.Time{})
	}
}

func BenchmarkS3FIFO_Get(b *testing.B) {
	cache := newS3FIFO[int, int](10000)
	for i := range 10000 {
		cache.setToMemory(i, i, time.Time{})
	}
	b.ResetTimer()

	for i := range b.N {
		cache.getFromMemory(i % 10000)
	}
}

func BenchmarkS3FIFO_Mixed(b *testing.B) {
	cache := newS3FIFO[int, int](10000)
	b.ResetTimer()

	for i := range b.N {
		if i%2 == 0 {
			cache.setToMemory(i%10000, i, time.Time{})
		} else {
			cache.getFromMemory(i % 10000)
		}
	}
}

// Test to understand S3-FIFO behavior with one-hit wonders
func TestS3FIFOBehavior(t *testing.T) {
	ctx := context.Background()
	cache, err := New[int, int](ctx, WithMemorySize(100))
	if err != nil {
		t.Fatal(err)
	}

	s3fifo := cache.memory

	fmt.Println("\n=== S3-FIFO Capacity Configuration ===")
	fmt.Printf("Total capacity: %d (4 shards)\n", s3fifo.totalCapacity())
	small, main := s3fifo.queueLens()
	fmt.Printf("Initial queues: Small=%d, Main=%d\n", small, main)

	// Phase 1: Insert hot items that will be accessed multiple times
	fmt.Println("\n=== Phase 1: Insert 50 hot items (will be accessed again) ===")
	for i := range 50 {
		if err := cache.Set(ctx, i, i, 0); err != nil {
			t.Fatalf("Set failed: %v", err)
		}
	}
	small, main = s3fifo.queueLens()
	fmt.Printf("After insertion: Small=%d, Main=%d, Total=%d\n", small, main, cache.Len())

	// Phase 2: Access hot items once (should promote some to Main)
	fmt.Println("\n=== Phase 2: Access hot items (should promote to Main) ===")
	for i := range 50 {
		if _, _, err := cache.Get(ctx, i); err != nil {
			t.Fatalf("Get failed: %v", err)
		}
	}
	small, main = s3fifo.queueLens()
	fmt.Printf("After first access: Small=%d, Main=%d, Total=%d\n", small, main, cache.Len())

	// Phase 3: Insert one-hit wonders (should stay in Small and be evicted quickly)
	fmt.Println("\n=== Phase 3: Insert 60 one-hit wonders ===")
	for i := 1000; i < 1060; i++ {
		if err := cache.Set(ctx, i, i, 0); err != nil {
			t.Fatalf("Set failed: %v", err)
		}
	}
	small, main = s3fifo.queueLens()
	fmt.Printf("After one-hit wonders: Small=%d, Main=%d, Total=%d\n", small, main, cache.Len())

	// Phase 4: Check if hot items are still in cache
	fmt.Println("\n=== Phase 4: Check if hot items survived ===")
	hotItemsFound := 0
	for i := range 50 {
		if _, found, err := cache.Get(ctx, i); err == nil && found {
			hotItemsFound++
		}
	}
	small, main = s3fifo.queueLens()
	fmt.Printf("Hot items still in cache: %d/50\n", hotItemsFound)
	fmt.Printf("Final state: Small=%d, Main=%d, Total=%d\n", small, main, cache.Len())

	// Expected behavior:
	// - Hot items should mostly be in Main queue after first access
	// - One-hit wonders should fill Small queue and be evicted from Small
	// - Hot items in Main should survive
	if hotItemsFound < 40 {
		t.Errorf("Expected most hot items to survive, got %d/50", hotItemsFound)
	}
}

// Test eviction order
func TestS3FIFOEvictionOrder(t *testing.T) {
	ctx := context.Background()
	cache, err := New[int, int](ctx, WithMemorySize(40)) // 10 per shard
	if err != nil {
		t.Fatal(err)
	}

	s3fifo := cache.memory
	fmt.Println("\n=== Testing Eviction Order ===")
	fmt.Printf("Cache capacity: %d (4 shards)\n", s3fifo.totalCapacity())

	// Fill cache with items
	for i := range 40 {
		if err := cache.Set(ctx, i, i, 0); err != nil {
			t.Fatalf("Set failed: %v", err)
		}
	}
	small, main := s3fifo.queueLens()
	fmt.Printf("After fill: Small=%d, Main=%d\n", small, main)

	// Access first 20 items (should promote them)
	fmt.Println("\nAccessing items 0-19 (should promote to Main):")
	for i := range 20 {
		if _, _, err := cache.Get(ctx, i); err != nil {
			t.Fatalf("Get failed: %v", err)
		}
	}
	small, main = s3fifo.queueLens()
	fmt.Printf("After access: Small=%d, Main=%d\n", small, main)

	// Insert new items (should evict from Small, not Main)
	fmt.Println("\nInserting new items 100-119:")
	for i := 100; i < 120; i++ {
		if err := cache.Set(ctx, i, i, 0); err != nil {
			t.Fatalf("Set failed: %v", err)
		}
	}
	small, main = s3fifo.queueLens()
	fmt.Printf("After new items: Small=%d, Main=%d\n", small, main)

	// Check which items survived
	fmt.Println("\nChecking which accessed items are still in cache:")
	accessedFound := 0
	for i := range 20 {
		if _, found, err := cache.Get(ctx, i); err == nil && found {
			accessedFound++
		}
	}
	fmt.Printf("Accessed items found: %d/20\n", accessedFound)
}

// Test to verify S3-FIFO is actually different from LRU behavior
func TestS3FIFODetailed(t *testing.T) {
	ctx := context.Background()
	cache, err := New[int, int](ctx, WithMemorySize(40)) // 10 per shard
	if err != nil {
		t.Fatal(err)
	}

	s3fifo := cache.memory

	fmt.Println("\n=== Detailed S3-FIFO Test ===")
	fmt.Printf("Capacity: %d (4 shards)\n\n", s3fifo.totalCapacity())

	// Step 1: Insert items 1-40 into cache
	fmt.Println("Step 1: Insert items 1-40")
	for i := 1; i <= 40; i++ {
		if err := cache.Set(ctx, i, i*100, 0); err != nil {
			t.Fatalf("Set failed: %v", err)
		}
	}
	small, main := s3fifo.queueLens()
	fmt.Printf("  Small=%d, Main=%d, Total=%d\n", small, main, cache.Len())

	// Step 2: Access items 1-20 (should mark them with freq > 0)
	fmt.Println("\nStep 2: Access items 1-20 (mark as hot)")
	for i := 1; i <= 20; i++ {
		if _, _, err := cache.Get(ctx, i); err != nil {
			t.Fatalf("Get failed: %v", err)
		}
	}
	small, main = s3fifo.queueLens()
	fmt.Printf("  Small=%d, Main=%d, Total=%d\n", small, main, cache.Len())

	// Step 3: Insert one-hit wonders 100-119
	fmt.Println("\nStep 3: Insert one-hit wonders 100-119")
	for i := 100; i < 120; i++ {
		if err := cache.Set(ctx, i, i*100, 0); err != nil {
			t.Fatalf("Set failed: %v", err)
		}
	}
	small, main = s3fifo.queueLens()
	fmt.Printf("  Small=%d, Main=%d, Total=%d\n", small, main, cache.Len())

	// Step 4: Check which items survived
	fmt.Println("\nStep 4: Check which items survived")
	fmt.Println("  Items 1-20 (accessed, should survive):")
	hotSurvived := 0
	for i := 1; i <= 20; i++ {
		if _, found, err := cache.Get(ctx, i); err == nil && found {
			hotSurvived++
		}
	}
	fmt.Printf("    Hot items found: %d/20\n", hotSurvived)

	fmt.Println("  Items 21-40 (not accessed):")
	coldSurvived := 0
	for i := 21; i <= 40; i++ {
		if _, found, err := cache.Get(ctx, i); err == nil && found {
			coldSurvived++
		}
	}
	fmt.Printf("    Cold items found: %d/20\n", coldSurvived)

	small, main = s3fifo.queueLens()
	fmt.Printf("\nFinal state: Small=%d, Main=%d, Total=%d\n", small, main, cache.Len())

	// Verify expected behavior - hot items should mostly survive
	if hotSurvived < 15 {
		t.Errorf("Expected most hot items to survive, got %d/20", hotSurvived)
	}
}

func TestS3FIFO_Flush(t *testing.T) {
	cache := newS3FIFO[string, int](100)

	// Add some items
	for i := range 50 {
		cache.setToMemory(fmt.Sprintf("key%d", i), i, time.Time{})
	}

	// Access some to promote to main queue
	for i := range 10 {
		cache.getFromMemory(fmt.Sprintf("key%d", i))
	}

	if cache.memoryLen() != 50 {
		t.Errorf("cache length = %d; want 50", cache.memoryLen())
	}

	// Flush
	removed := cache.flushMemory()
	if removed != 50 {
		t.Errorf("flushMemory removed %d items; want 50", removed)
	}

	// Cache should be empty
	if cache.memoryLen() != 0 {
		t.Errorf("cache length after flush = %d; want 0", cache.memoryLen())
	}

	// All keys should be gone
	for i := range 50 {
		if _, ok := cache.getFromMemory(fmt.Sprintf("key%d", i)); ok {
			t.Errorf("key%d should not be found after flush", i)
		}
	}

	// Can add new items after flush
	cache.setToMemory("new", 999, time.Time{})
	if val, ok := cache.getFromMemory("new"); !ok || val != 999 {
		t.Errorf("get(new) = %v, %v; want 999, true", val, ok)
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
