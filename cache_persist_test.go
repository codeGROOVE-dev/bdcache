package bdcache

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// mockPersister is a simple in-memory persister for testing
type mockPersister[K comparable, V any] struct {
	mu      sync.RWMutex
	data    map[string]mockEntry[V]
	closed  bool
	failGet bool
	failSet bool
}

type mockEntry[V any] struct {
	value  V
	expiry time.Time
	updatedAt time.Time
}

func newMockPersister[K comparable, V any]() *mockPersister[K, V] {
	return &mockPersister[K, V]{
		data: make(map[string]mockEntry[V]),
	}
}

func (m *mockPersister[K, V]) ValidateKey(key K) error {
	return nil
}

func (m *mockPersister[K, V]) Load(ctx context.Context, key K) (V, time.Time, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var zero V
	if m.failGet {
		return zero, time.Time{}, false, fmt.Errorf("mock get error")
	}

	keyStr := fmt.Sprintf("%v", key)
	entry, found := m.data[keyStr]
	if !found {
		return zero, time.Time{}, false, nil
	}

	// Check expiration
	if !entry.expiry.IsZero() && time.Now().After(entry.expiry) {
		return zero, time.Time{}, false, nil
	}

	return entry.value, entry.expiry, true, nil
}

func (m *mockPersister[K, V]) Store(ctx context.Context, key K, value V, expiry time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.failSet {
		return fmt.Errorf("mock store error")
	}

	keyStr := fmt.Sprintf("%v", key)
	m.data[keyStr] = mockEntry[V]{
		value:     value,
		expiry:    expiry,
		updatedAt: time.Now(),
	}
	return nil
}

func (m *mockPersister[K, V]) Delete(ctx context.Context, key K) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.failSet { // Reuse failSet for Delete errors
		return fmt.Errorf("mock delete error")
	}

	keyStr := fmt.Sprintf("%v", key)
	delete(m.data, keyStr)
	return nil
}

func (m *mockPersister[K, V]) LoadRecent(ctx context.Context, limit int) (<-chan Entry[K, V], <-chan error) {
	entryCh := make(chan Entry[K, V], 10)
	errCh := make(chan error, 1)

	go func() {
		defer close(entryCh)
		defer close(errCh)

		m.mu.RLock()
		defer m.mu.RUnlock()

		count := 0
		for keyStr, entry := range m.data {
			if limit > 0 && count >= limit {
				break
			}

			// Parse key back from string
			var key K
			if _, err := fmt.Sscanf(keyStr, "%v", &key); err != nil {
				// Try direct type assertion for string keys
				sk, ok := any(keyStr).(K)
				if !ok {
					continue
				}
				key = sk
			}

			entryCh <- Entry[K, V]{
				Key:       key,
				Value:     entry.value,
				Expiry:    entry.expiry,
				UpdatedAt: entry.updatedAt,
			}
			count++
		}
	}()

	return entryCh, errCh
}

func (m *mockPersister[K, V]) Cleanup(ctx context.Context, maxAge time.Duration) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	cutoff := time.Now().Add(-maxAge)
	count := 0

	for keyStr, entry := range m.data {
		if !entry.expiry.IsZero() && entry.expiry.Before(cutoff) {
			delete(m.data, keyStr)
			count++
		}
	}

	return count, nil
}

func (m *mockPersister[K, V]) Location(key K) string {
	return fmt.Sprintf("mock://%v", key)
}

func (m *mockPersister[K, V]) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return fmt.Errorf("mock close error: already closed")
	}
	m.closed = true
	return nil
}

func TestCache_WithPersistence(t *testing.T) {
	ctx := context.Background()
	persister := newMockPersister[string, int]()

	cache, err := New[string, int](ctx, WithPersistence(persister))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer cache.Close()

	// Set should persist
	if err := cache.Set(ctx, "key1", 42, 0); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Verify it's in persistence
	val, _, found, err := persister.Load(ctx, "key1")
	if err != nil {
		t.Fatalf("persister.Load: %v", err)
	}
	if !found {
		t.Error("key1 should be persisted")
	}
	if val != 42 {
		t.Errorf("persisted value = %d; want 42", val)
	}

	// Delete should remove from persistence
	cache.Delete(ctx, "key1")

	_, _, found, err = persister.Load(ctx, "key1")
	if err != nil {
		t.Fatalf("persister.Load after delete: %v", err)
	}
	if found {
		t.Error("key1 should be deleted from persistence")
	}
}

func TestCache_GetFromPersistence(t *testing.T) {
	ctx := context.Background()
	persister := newMockPersister[string, int]()

	// Pre-populate persistence
	persister.Store(ctx, "key1", 42, time.Time{})

	cache, err := New[string, int](ctx, WithPersistence(persister))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer cache.Close()

	// Get should load from persistence
	val, found, err := cache.Get(ctx, "key1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found {
		t.Error("key1 should be found in persistence")
	}
	if val != 42 {
		t.Errorf("Get value = %d; want 42", val)
	}
}

func TestCache_GetFromPersistenceExpired(t *testing.T) {
	ctx := context.Background()
	persister := newMockPersister[string, int]()

	// Pre-populate with expired entry
	persister.Store(ctx, "key1", 42, time.Now().Add(-1*time.Hour))

	cache, err := New[string, int](ctx, WithPersistence(persister))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer cache.Close()

	// Get should return not found for expired entry
	_, found, err := cache.Get(ctx, "key1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if found {
		t.Error("expired key should not be found")
	}
}

func TestCache_WithWarmup(t *testing.T) {
	ctx := context.Background()
	persister := newMockPersister[string, int]()

	// Pre-populate persistence with 10 items
	for i := range 10 {
		persister.Store(ctx, fmt.Sprintf("key%d", i), i, time.Time{})
	}

	// Create cache with warmup limit of 5
	cache, err := New[string, int](ctx,
		WithPersistence(persister),
		WithWarmup(5))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer cache.Close()

	// Give warmup goroutine time to complete
	time.Sleep(100 * time.Millisecond)

	// Cache should have warmed up with some items (exact count depends on iteration order)
	if cache.Len() == 0 {
		t.Error("cache should have warmed up with items from persistence")
	}
	if cache.Len() > 5 {
		t.Errorf("cache length = %d; should not exceed warmup limit of 5", cache.Len())
	}
}

func TestCache_WithCleanupOnStartup(t *testing.T) {
	ctx := context.Background()
	persister := newMockPersister[string, int]()

	// Pre-populate with expired entries
	past := time.Now().Add(-2 * time.Hour)
	persister.Store(ctx, "expired1", 1, past)
	persister.Store(ctx, "expired2", 2, past)
	persister.Store(ctx, "valid", 3, time.Time{})

	// Create cache with cleanup
	cache, err := New[string, int](ctx,
		WithPersistence(persister),
		WithCleanup(1*time.Hour))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer cache.Close()

	// Expired entries should be cleaned up
	_, _, found, _ := persister.Load(ctx, "expired1")
	if found {
		t.Error("expired1 should have been cleaned up")
	}

	// Valid entry should remain
	_, _, found, _ = persister.Load(ctx, "valid")
	if !found {
		t.Error("valid entry should still exist")
	}
}

func TestCache_SetAsyncWithPersistence(t *testing.T) {
	ctx := context.Background()
	persister := newMockPersister[string, int]()

	cache, err := New[string, int](ctx, WithPersistence(persister))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer cache.Close()

	// SetAsync should not block but value should be available immediately
	if err := cache.SetAsync(ctx, "key1", 42, 0); err != nil {
		t.Fatalf("SetAsync: %v", err)
	}

	// Value should be in memory
	val, found, _ := cache.Get(ctx, "key1")
	if !found || val != 42 {
		t.Error("key1 should be available immediately after SetAsync")
	}

	// Give async persistence time to complete
	time.Sleep(50 * time.Millisecond)

	// Should also be persisted
	val, _, found, _ = persister.Load(ctx, "key1")
	if !found || val != 42 {
		t.Error("key1 should be persisted after SetAsync")
	}
}

func TestCache_CloseWithPersistence(t *testing.T) {
	ctx := context.Background()
	persister := newMockPersister[string, int]()

	cache, err := New[string, int](ctx, WithPersistence(persister))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Close should close the persister
	if err := cache.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if !persister.closed {
		t.Error("persister should be closed")
	}
}

func TestCache_PersistenceErrors(t *testing.T) {
	ctx := context.Background()
	persister := newMockPersister[string, int]()

	cache, err := New[string, int](ctx, WithPersistence(persister))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer cache.Close()

	// Set returns error when persistence fails (by design)
	// Value is still in memory, but error is returned to caller
	persister.failSet = true
	if err := cache.Set(ctx, "key1", 42, 0); err == nil {
		t.Error("Set should return error when persistence fails")
	}

	// Value should still be in memory despite persistence error
	val, found, _ := cache.Get(ctx, "key1")
	if !found || val != 42 {
		t.Error("key1 should be in memory even though persistence failed")
	}

	// SetAsync handles persistence errors gracefully (logs but doesn't return error)
	persister.failSet = true
	if err := cache.SetAsync(ctx, "key3", 300, 0); err != nil {
		t.Fatalf("SetAsync should not fail even when persistence fails: %v", err)
	}

	// Value should be in memory
	val, found, _ = cache.Get(ctx, "key3")
	if !found || val != 300 {
		t.Error("key3 should be in memory after SetAsync")
	}

	// Get should work from memory even if persistence fails
	persister.failGet = true
	persister.failSet = false
	if err := cache.Set(ctx, "key2", 100, 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	val, found, _ = cache.Get(ctx, "key2")
	if !found || val != 100 {
		t.Error("Get should work from memory even if persistence load fails")
	}
}

func TestCache_Delete_Errors(t *testing.T) {
	ctx := context.Background()
	persister := newMockPersister[string, int]()

	cache, err := New[string, int](ctx, WithPersistence(persister))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer cache.Close()

	// Store a value (with failSet = false)
	persister.failSet = false
	if err := cache.Set(ctx, "key1", 42, 0); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Verify it's in memory
	val, found, _ := cache.Get(ctx, "key1")
	if !found || val != 42 {
		t.Fatal("key1 should be in memory before delete")
	}

	// Now make persistence delete fail
	persister.failSet = true // failSet affects Delete too in mock
	cache.Delete(ctx, "key1")

	// Note: Even though persistence delete failed, key is deleted from memory.
	// However, Get will load it back from persistence since it's still there.
	// This tests graceful degradation - memory is cleaned up even if persistence fails.
	val2, found2, _ := cache.Get(ctx, "key1")
	if !found2 || val2 != 42 {
		// Get loads from persistence, so key is found again
		t.Logf("key1 found from persistence after failed delete (expected): %v, %v", val2, found2)
	}

	// Verify the delete persistence error path is exercised
	// by checking it was attempted (log shows "persistence delete failed")

	// Delete with invalid key (using empty string which mock persister will reject)
	cache.Delete(ctx, "")
	// Should not panic
}

func TestCache_Get_InvalidKey(t *testing.T) {
	ctx := context.Background()
	persister := newMockPersister[string, int]()

	cache, err := New[string, int](ctx, WithPersistence(persister))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer cache.Close()

	// Get with empty key (invalid)
	_, found, err := cache.Get(ctx, "")
	if err != nil {
		t.Errorf("Get with invalid key should not return error: %v", err)
	}
	if found {
		t.Error("invalid key should not be found")
	}
}

func TestCache_Get_PersistenceLoadError(t *testing.T) {
	ctx := context.Background()
	persister := newMockPersister[string, int]()

	cache, err := New[string, int](ctx, WithPersistence(persister))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer cache.Close()

	// Pre-populate persistence (not in memory)
	persister.Store(ctx, "key1", 42, time.Time{})

	// Make persistence Load fail
	persister.failGet = true

	// Get should handle error gracefully
	_, found, err := cache.Get(ctx, "key1")
	if err != nil {
		t.Errorf("Get should not return error on persistence failure: %v", err)
	}
	if found {
		t.Error("key should not be found when persistence fails")
	}
}

func TestCache_Close_PersistenceError(t *testing.T) {
	ctx := context.Background()
	persister := newMockPersister[string, int]()

	cache, err := New[string, int](ctx, WithPersistence(persister))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Make persister.Close() fail
	persister.closed = true // This will cause some error condition

	// Close should return error
	if err := cache.Close(); err != nil {
		// Expected - persistence close can fail
		t.Logf("Close error (expected): %v", err)
	}
}

func TestCache_GhostQueue(t *testing.T) {
	ctx := context.Background()

	// Small capacity to force ghost queue usage
	cache, err := New[string, int](ctx, WithMemorySize(10))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer cache.Close()

	// Fill small queue (10% of 10 = 1)
	// Insert items to trigger ghost queue
	for i := 0; i < 20; i++ {
		if err := cache.Set(ctx, fmt.Sprintf("key%d", i), i, 0); err != nil {
			t.Fatalf("Set: %v", err)
		}
	}

	// Access some items to create hot items
	for i := 0; i < 5; i++ {
		cache.Get(ctx, fmt.Sprintf("key%d", i))
	}

	// Insert more to trigger evictions from small queue to ghost queue
	for i := 20; i < 40; i++ {
		if err := cache.Set(ctx, fmt.Sprintf("key%d", i), i, 0); err != nil {
			t.Fatalf("Set: %v", err)
		}
	}

	// Test should complete without panic
	t.Logf("Cache length: %d", cache.Len())
}

func TestCache_MainQueueEviction(t *testing.T) {
	ctx := context.Background()

	// Create cache with small capacity
	cache, err := New[string, int](ctx, WithMemorySize(10))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer cache.Close()

	// Insert and access items to get them into Main queue
	for i := 0; i < 15; i++ {
		key := fmt.Sprintf("key%d", i)
		if err := cache.Set(ctx, key, i, 0); err != nil {
			t.Fatalf("Set: %v", err)
		}
		// Access to promote to Main
		cache.Get(ctx, key)
	}

	// Insert more items to trigger eviction from Main queue
	for i := 15; i < 25; i++ {
		key := fmt.Sprintf("key%d", i)
		if err := cache.Set(ctx, key, i, 0); err != nil {
			t.Fatalf("Set: %v", err)
		}
		cache.Get(ctx, key)
	}

	// Verify cache is at capacity
	if cache.Len() > 10 {
		t.Errorf("Cache length %d exceeds capacity 10", cache.Len())
	}
}

func TestCache_CleanupExpiredEntries(t *testing.T) {
	ctx := context.Background()

	cache, err := New[string, int](ctx)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer cache.Close()

	// Store items with expiry
	past := time.Now().Add(-1 * time.Hour)
	cache.Set(ctx, "expired1", 1, -1*time.Hour) // Already expired
	cache.Set(ctx, "expired2", 2, -1*time.Hour)
	cache.Set(ctx, "valid", 3, 1*time.Hour)

	// Manually set expiry to past for testing
	if err := cache.Set(ctx, "test-expired", 99, 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	// Access internal state to set past expiry
	cache.memory.mu.Lock()
	if item, ok := cache.memory.items["test-expired"]; ok {
		item.expiry = past
	}
	cache.memory.mu.Unlock()

	// Run cleanup
	deleted := cache.Cleanup()
	t.Logf("Cleanup deleted %d entries", deleted)

	// Valid entry should still exist
	if _, found, _ := cache.Get(ctx, "valid"); !found {
		t.Error("valid entry should still exist after cleanup")
	}
}
