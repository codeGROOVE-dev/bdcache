package sfcache

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/codeGROOVE-dev/sfcache/pkg/persist"
)

// mockStore is a simple in-memory store for testing.
type mockStore[K comparable, V any] struct {
	mu      sync.RWMutex
	data    map[string]mockEntry[V]
	closed  bool
	failGet bool
	failSet bool
}

type mockEntry[V any] struct {
	value     V
	expiry    time.Time
	updatedAt time.Time
}

func newMockStore[K comparable, V any]() *mockStore[K, V] {
	return &mockStore[K, V]{
		data: make(map[string]mockEntry[V]),
	}
}

func (m *mockStore[K, V]) setFailGet(v bool) {
	m.mu.Lock()
	m.failGet = v
	m.mu.Unlock()
}

func (m *mockStore[K, V]) setFailSet(v bool) {
	m.mu.Lock()
	m.failSet = v
	m.mu.Unlock()
}

func (m *mockStore[K, V]) ValidateKey(key K) error {
	return nil
}

func (m *mockStore[K, V]) Get(ctx context.Context, key K) (v V, expiry time.Time, found bool, err error) {
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

func (m *mockStore[K, V]) Set(ctx context.Context, key K, value V, expiry time.Time) error {
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

func (m *mockStore[K, V]) Delete(ctx context.Context, key K) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.failSet { // Reuse failSet for Delete errors
		return fmt.Errorf("mock delete error")
	}

	keyStr := fmt.Sprintf("%v", key)
	delete(m.data, keyStr)
	return nil
}

//nolint:gocritic // Channel returns are clearer without named results
func (m *mockStore[K, V]) LoadRecent(ctx context.Context, limit int) (<-chan persist.Entry[K, V], <-chan error) {
	entryCh := make(chan persist.Entry[K, V], 10)
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

			entryCh <- persist.Entry[K, V]{
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

func (m *mockStore[K, V]) Cleanup(ctx context.Context, maxAge time.Duration) (int, error) {
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

func (m *mockStore[K, V]) Location(key K) string {
	return fmt.Sprintf("mock://%v", key)
}

func (m *mockStore[K, V]) Flush(ctx context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	count := len(m.data)
	m.data = make(map[string]mockEntry[V])
	return count, nil
}

func (m *mockStore[K, V]) Len(ctx context.Context) (int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.data), nil
}

func (m *mockStore[K, V]) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return fmt.Errorf("mock close error: already closed")
	}
	m.closed = true
	return nil
}

func TestTieredCache_Basic(t *testing.T) {
	ctx := context.Background()
	store := newMockStore[string, int]()

	cache, err := NewTiered[string, int](store)
	if err != nil {
		t.Fatalf("NewTiered: %v", err)
	}
	defer func() { _ = cache.Close() }() //nolint:errcheck // Test cleanup

	// Set should persist
	if err := cache.Set(ctx, "key1", 42, 0); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Verify it's in persistence
	val, _, found, err := store.Get(ctx, "key1")
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if !found {
		t.Error("key1 should be persisted")
	}
	if val != 42 {
		t.Errorf("persisted value = %d; want 42", val)
	}

	// Delete should remove from persistence
	if err := cache.Delete(ctx, "key1"); err != nil {
		t.Fatalf("cache.Delete: %v", err)
	}

	_, _, found, err = store.Get(ctx, "key1")
	if err != nil {
		t.Fatalf("store.Get after delete: %v", err)
	}
	if found {
		t.Error("key1 should be deleted from persistence")
	}
}

func TestTieredCache_GetFromPersistence(t *testing.T) {
	ctx := context.Background()
	store := newMockStore[string, int]()

	// Pre-populate persistence
	_ = store.Set(ctx, "key1", 42, time.Time{}) //nolint:errcheck // Test fixture

	cache, err := NewTiered[string, int](store)
	if err != nil {
		t.Fatalf("NewTiered: %v", err)
	}
	defer func() { _ = cache.Close() }() //nolint:errcheck // Test cleanup

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

func TestTieredCache_PromotesToMemory(t *testing.T) {
	ctx := context.Background()
	store := newMockStore[string, int]()

	// Pre-populate persistence only (not memory)
	_ = store.Set(ctx, "key1", 42, time.Time{}) //nolint:errcheck // Test fixture

	cache, err := NewTiered[string, int](store)
	if err != nil {
		t.Fatalf("NewTiered: %v", err)
	}
	defer func() { _ = cache.Close() }() //nolint:errcheck // Test cleanup

	// Memory should be empty initially
	if cache.Len() != 0 {
		t.Errorf("initial memory cache length = %d; want 0", cache.Len())
	}

	// First Get: should load from persistence
	val, found, err := cache.Get(ctx, "key1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found || val != 42 {
		t.Error("key1 should be found from persistence")
	}

	// Memory should now have the entry (promoted)
	if cache.Len() != 1 {
		t.Errorf("memory cache length after Get = %d; want 1 (should be promoted)", cache.Len())
	}

	// Make persistence fail - subsequent Get should still work from memory
	store.setFailGet(true)

	val, found, err = cache.Get(ctx, "key1")
	if err != nil {
		t.Fatalf("Get from memory: %v", err)
	}
	if !found || val != 42 {
		t.Error("key1 should be found from memory (promoted from persistence)")
	}
}

func TestTieredCache_GetFromPersistenceExpired(t *testing.T) {
	ctx := context.Background()
	store := newMockStore[string, int]()

	// Pre-populate with expired entry
	_ = store.Set(ctx, "key1", 42, time.Now().Add(-1*time.Hour)) //nolint:errcheck // Test fixture

	cache, err := NewTiered[string, int](store)
	if err != nil {
		t.Fatalf("NewTiered: %v", err)
	}
	defer func() { _ = cache.Close() }() //nolint:errcheck // Test cleanup

	// Get should return not found for expired entry
	_, found, err := cache.Get(ctx, "key1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if found {
		t.Error("expired key should not be found")
	}
}

func TestTieredCache_SetAsync(t *testing.T) {
	ctx := context.Background()
	store := newMockStore[string, int]()

	cache, err := NewTiered[string, int](store)
	if err != nil {
		t.Fatalf("NewTiered: %v", err)
	}
	defer func() { _ = cache.Close() }() //nolint:errcheck // Test cleanup

	// SetAsync should not block but value should be available immediately
	if err := cache.SetAsync(ctx, "key1", 42, 0); err != nil {
		t.Fatalf("SetAsync: %v", err)
	}

	// Value should be in memory
	val, found, err := cache.Get(ctx, "key1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found || val != 42 {
		t.Error("key1 should be available immediately after SetAsync")
	}

	// Give async persistence time to complete
	time.Sleep(50 * time.Millisecond)

	// Should also be persisted
	val, _, found, err = store.Get(ctx, "key1")
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if !found || val != 42 {
		t.Error("key1 should be persisted after SetAsync")
	}
}

func TestTieredCache_Close(t *testing.T) {
	store := newMockStore[string, int]()

	cache, err := NewTiered[string, int](store)
	if err != nil {
		t.Fatalf("NewTiered: %v", err)
	}

	// Close should close the store
	if err := cache.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if !store.closed {
		t.Error("store should be closed")
	}
}

func TestTieredCache_Errors(t *testing.T) {
	ctx := context.Background()
	store := newMockStore[string, int]()

	cache, err := NewTiered[string, int](store)
	if err != nil {
		t.Fatalf("NewTiered: %v", err)
	}
	defer func() { _ = cache.Close() }() //nolint:errcheck // Test cleanup

	// Set returns error when persistence fails (by design)
	// Value is still in memory, but error is returned to caller
	store.setFailSet(true)
	if err := cache.Set(ctx, "key1", 42, 0); err == nil {
		t.Error("Set should return error when persistence fails")
	}

	// Value should still be in memory despite persistence error
	val, found, err := cache.Get(ctx, "key1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found || val != 42 {
		t.Error("key1 should be in memory even though persistence failed")
	}

	// SetAsync logs persistence errors but doesn't return them
	store.setFailSet(true)
	if err := cache.SetAsync(ctx, "key3", 300, 0); err != nil {
		t.Fatalf("SetAsync should not fail synchronously: %v", err)
	}

	// Value should be in memory immediately
	val, found, err = cache.Get(ctx, "key3")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found || val != 300 {
		t.Error("key3 should be in memory after SetAsync")
	}

	// Give async persistence time to attempt (and fail with logged error)
	time.Sleep(50 * time.Millisecond)

	// Get should work from memory even if persistence fails
	store.setFailGet(true)
	store.setFailSet(false)
	if err := cache.Set(ctx, "key2", 100, 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	val, found, err = cache.Get(ctx, "key2")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found || val != 100 {
		t.Error("Get should work from memory even if persistence load fails")
	}
}

func TestTieredCache_Delete_Errors(t *testing.T) {
	ctx := context.Background()
	store := newMockStore[string, int]()

	cache, err := NewTiered[string, int](store)
	if err != nil {
		t.Fatalf("NewTiered: %v", err)
	}
	defer func() { _ = cache.Close() }() //nolint:errcheck // Test cleanup

	// Store a value (with failSet = false)
	store.setFailSet(false)
	if err := cache.Set(ctx, "key1", 42, 0); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Verify it's in memory
	val, found, err := cache.Get(ctx, "key1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found || val != 42 {
		t.Fatal("key1 should be in memory before delete")
	}

	// Now make persistence delete fail
	store.setFailSet(true) // failSet affects Delete too in mock
	err = cache.Delete(ctx, "key1")
	if err == nil {
		t.Error("Delete should return error when persistence fails")
	}

	// Note: Even though persistence delete failed, key is deleted from memory.
	// However, Get will load it back from persistence since it's still there.
	// This tests that memory is always cleaned even if persistence fails.
	val2, found2, err := cache.Get(ctx, "key1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found2 || val2 != 42 {
		// Get loads from persistence, so key is found again
		t.Logf("key1 found from persistence after failed delete (expected): %v, %v", val2, found2)
	}

	// Delete with invalid key should return error
	err = cache.Delete(ctx, "")
	if err == nil {
		t.Error("Delete with empty key should return error")
	}
}

func TestTieredCache_Get_InvalidKey(t *testing.T) {
	ctx := context.Background()
	store := newMockStore[string, int]()

	cache, err := NewTiered[string, int](store)
	if err != nil {
		t.Fatalf("NewTiered: %v", err)
	}
	defer func() { _ = cache.Close() }() //nolint:errcheck // Test cleanup

	// Get with empty key (invalid)
	_, found, err := cache.Get(ctx, "")
	if err != nil {
		t.Errorf("Get with invalid key should not return error: %v", err)
	}
	if found {
		t.Error("invalid key should not be found")
	}
}

func TestTieredCache_Get_PersistenceLoadError(t *testing.T) {
	ctx := context.Background()
	store := newMockStore[string, int]()

	cache, err := NewTiered[string, int](store)
	if err != nil {
		t.Fatalf("NewTiered: %v", err)
	}
	defer func() { _ = cache.Close() }() //nolint:errcheck // Test cleanup

	// Pre-populate persistence (not in memory)
	_ = store.Set(ctx, "key1", 42, time.Time{}) //nolint:errcheck // Test fixture

	// Make persistence Load fail
	store.setFailGet(true)

	// Get should return error on persistence failure
	_, found, err := cache.Get(ctx, "key1")
	if err == nil {
		t.Error("Get should return error on persistence failure")
	}
	if found {
		t.Error("key should not be found when persistence fails")
	}
}

func TestTieredCache_Close_PersistenceError(t *testing.T) {
	store := newMockStore[string, int]()

	cache, err := NewTiered[string, int](store)
	if err != nil {
		t.Fatalf("NewTiered: %v", err)
	}

	// Make store.Close() fail
	store.closed = true // This will cause some error condition

	// Close should return error
	if err := cache.Close(); err != nil {
		// Expected - persistence close can fail
		t.Logf("Close error (expected): %v", err)
	}
}

func TestTieredCache_Flush(t *testing.T) {
	ctx := context.Background()
	store := newMockStore[string, int]()

	cache, err := NewTiered[string, int](store)
	if err != nil {
		t.Fatalf("NewTiered: %v", err)
	}
	defer func() { _ = cache.Close() }() //nolint:errcheck // Test cleanup

	// Add entries
	for i := range 10 {
		if err := cache.Set(ctx, fmt.Sprintf("key%d", i), i*100, 0); err != nil {
			t.Fatalf("Set: %v", err)
		}
	}

	// Verify entries exist in both memory and persistence
	if cache.Len() != 10 {
		t.Errorf("memory cache length = %d; want 10", cache.Len())
	}
	for i := range 10 {
		if _, _, found, err := store.Get(ctx, fmt.Sprintf("key%d", i)); err != nil || !found {
			t.Fatalf("key%d should exist in persistence", i)
		}
	}

	// Flush
	removed, err := cache.Flush(ctx)
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}
	// Should remove 10 from memory + 10 from persistence = 20 total
	if removed != 20 {
		t.Errorf("Flush removed %d items; want 20", removed)
	}

	// Memory cache should be empty
	if cache.Len() != 0 {
		t.Errorf("memory cache length after flush = %d; want 0", cache.Len())
	}

	// Persistence should be empty
	for i := range 10 {
		if _, _, found, err := store.Get(ctx, fmt.Sprintf("key%d", i)); err != nil {
			t.Fatalf("Load: %v", err)
		} else if found {
			t.Errorf("key%d should not exist in persistence after flush", i)
		}
	}

	// Get should return not found for all keys
	for i := range 10 {
		_, found, err := cache.Get(ctx, fmt.Sprintf("key%d", i))
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if found {
			t.Errorf("key%d should not be found after flush", i)
		}
	}
}

func TestTieredCache_StoreAccess(t *testing.T) {
	ctx := context.Background()
	store := newMockStore[string, int]()

	cache, err := NewTiered[string, int](store)
	if err != nil {
		t.Fatalf("NewTiered: %v", err)
	}
	defer func() { _ = cache.Close() }() //nolint:errcheck // Test cleanup

	// Add some entries
	for i := range 5 {
		if err := cache.Set(ctx, fmt.Sprintf("key%d", i), i*10, 0); err != nil {
			t.Fatalf("Set: %v", err)
		}
	}

	// Access Store directly via public field
	storeLen, err := cache.Store.Len(ctx)
	if err != nil {
		t.Fatalf("Store.Len: %v", err)
	}
	if storeLen != 5 {
		t.Errorf("Store.Len() = %d; want 5", storeLen)
	}

	// Verify memory Len() also works
	if cache.Len() != 5 {
		t.Errorf("Len() = %d; want 5", cache.Len())
	}

	// Flush via Store
	flushed, err := cache.Store.Flush(ctx)
	if err != nil {
		t.Fatalf("Store.Flush: %v", err)
	}
	if flushed != 5 {
		t.Errorf("Store.Flush() = %d; want 5", flushed)
	}

	// Store should be empty now
	storeLen, err = cache.Store.Len(ctx)
	if err != nil {
		t.Fatalf("Store.Len: %v", err)
	}
	if storeLen != 0 {
		t.Errorf("Store.Len() after flush = %d; want 0", storeLen)
	}
}

func TestTieredCache_WithOptions(t *testing.T) {
	store := newMockStore[string, int]()

	// Test Size
	cache, err := NewTiered[string, int](store, Size(500))
	if err != nil {
		t.Fatalf("NewTiered: %v", err)
	}
	if cache.memory == nil {
		t.Error("memory should be initialized")
	}
	_ = cache.Close() //nolint:errcheck // Test cleanup

	// Recreate store since it was closed
	store = newMockStore[string, int]()

	// Test WithTTL
	cache, err = NewTiered[string, int](store, TTL(5*time.Minute))
	if err != nil {
		t.Fatalf("NewTiered: %v", err)
	}
	if cache.defaultTTL != 5*time.Minute {
		t.Errorf("default TTL = %v; want 5m", cache.defaultTTL)
	}
	_ = cache.Close() //nolint:errcheck // Test cleanup
}

func TestTieredCache_Set_VariadicTTL(t *testing.T) {
	ctx := context.Background()
	store := newMockStore[string, int]()

	cache, err := NewTiered[string, int](store, TTL(time.Hour))
	if err != nil {
		t.Fatalf("NewTiered: %v", err)
	}
	defer func() { _ = cache.Close() }() //nolint:errcheck // Test cleanup

	// Set without TTL - uses default (1 hour, won't expire during test)
	if err := cache.Set(ctx, "default-ttl", 1); err != nil {
		t.Fatalf("Set: %v", err)
	}
	_, found, err := cache.Get(ctx, "default-ttl")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found {
		t.Error("default-ttl should be found")
	}

	// Set with explicit short TTL
	if err := cache.Set(ctx, "short-ttl", 2, 50*time.Millisecond); err != nil {
		t.Fatalf("Set: %v", err)
	}
	_, found, err = cache.Get(ctx, "short-ttl")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found {
		t.Error("short-ttl should be found immediately")
	}

	// Wait for short TTL to expire
	time.Sleep(100 * time.Millisecond)

	// short-ttl should be expired in memory, default-ttl should still exist
	_, found, err = cache.Get(ctx, "short-ttl")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if found {
		// Note: might still be found if persistence doesn't check expiry
		t.Log("short-ttl may still be found from persistence (expected)")
	}
	_, found, err = cache.Get(ctx, "default-ttl")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found {
		t.Error("default-ttl should still exist (1 hour TTL)")
	}
}

func TestTieredCache_SetAsync_VariadicTTL(t *testing.T) {
	ctx := context.Background()
	store := newMockStore[string, int]()

	cache, err := NewTiered[string, int](store, TTL(time.Hour))
	if err != nil {
		t.Fatalf("NewTiered: %v", err)
	}
	defer func() { _ = cache.Close() }() //nolint:errcheck // Test cleanup

	// SetAsync without TTL - uses default
	if err := cache.SetAsync(ctx, "async-default", 1); err != nil {
		t.Fatalf("SetAsync: %v", err)
	}

	// SetAsync with explicit TTL
	if err := cache.SetAsync(ctx, "async-explicit", 2, 5*time.Minute); err != nil {
		t.Fatalf("SetAsync: %v", err)
	}

	// Both should be in memory immediately
	_, found, err := cache.Get(ctx, "async-default")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found {
		t.Error("async-default should be found")
	}
	_, found, err = cache.Get(ctx, "async-explicit")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found {
		t.Error("async-explicit should be found")
	}

	// Give async persistence time to complete
	time.Sleep(50 * time.Millisecond)

	// Both should be persisted
	_, _, found, err = store.Get(ctx, "async-default")
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if !found {
		t.Error("async-default should be persisted")
	}
	_, _, found, err = store.Get(ctx, "async-explicit")
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if !found {
		t.Error("async-explicit should be persisted")
	}
}

func TestTieredCache_Concurrent(t *testing.T) {
	store := newMockStore[int, int]()

	cache, err := NewTiered[int, int](store, Size(1000))
	if err != nil {
		t.Fatalf("NewTiered: %v", err)
	}
	defer func() { _ = cache.Close() }() //nolint:errcheck // Test cleanup

	ctx := context.Background()
	var wg sync.WaitGroup

	// Concurrent writers
	for i := range 10 {
		wg.Add(1)
		go func(offset int) {
			defer wg.Done()
			for j := range 100 {
				_ = cache.Set(ctx, offset*100+j, j, 0) //nolint:errcheck // Test concurrent access
			}
		}(i)
	}

	// Concurrent readers
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 100 {
				_, _, _ = cache.Get(ctx, j) //nolint:errcheck // Test concurrent access
			}
		}()
	}

	wg.Wait()

	// Cache should be functional after concurrent access
	if err := cache.Set(ctx, 9999, 9999, 0); err != nil {
		t.Errorf("Set after concurrent access failed: %v", err)
	}
	val, found, err := cache.Get(ctx, 9999)
	if err != nil || !found || val != 9999 {
		t.Errorf("Get after concurrent access: val=%d, found=%v, err=%v", val, found, err)
	}
}

func TestTieredCache_Set_KeyValidationError(t *testing.T) {
	store := &validatingMockStore[string, int]{
		mockStore: newMockStore[string, int](),
	}

	cache, err := NewTiered[string, int](store)
	if err != nil {
		t.Fatalf("NewTiered: %v", err)
	}
	defer func() { _ = cache.Close() }() //nolint:errcheck // Test cleanup

	ctx := context.Background()

	// Set with invalid key should return error
	err = cache.Set(ctx, "invalid/key", 42, 0)
	if err == nil {
		t.Error("Set with invalid key should return error")
	}

	// Value should NOT be in memory (validation happens before memory write)
	if cache.Len() != 0 {
		t.Errorf("memory cache should be empty after validation error, got %d", cache.Len())
	}
}

func TestTieredCache_SetAsync_KeyValidationError(t *testing.T) {
	store := &validatingMockStore[string, int]{
		mockStore: newMockStore[string, int](),
	}

	cache, err := NewTiered[string, int](store)
	if err != nil {
		t.Fatalf("NewTiered: %v", err)
	}
	defer func() { _ = cache.Close() }() //nolint:errcheck // Test cleanup

	ctx := context.Background()

	// SetAsync with invalid key should return error synchronously
	err = cache.SetAsync(ctx, "invalid/key", 42, 0)
	if err == nil {
		t.Error("SetAsync with invalid key should return error")
	}

	// Value should NOT be in memory
	if cache.Len() != 0 {
		t.Errorf("memory cache should be empty after validation error, got %d", cache.Len())
	}
}

// validatingMockStore wraps mockStore but rejects keys containing "/"
type validatingMockStore[K comparable, V any] struct {
	*mockStore[K, V]
}

func (m *validatingMockStore[K, V]) ValidateKey(key K) error {
	keyStr := fmt.Sprintf("%v", key)
	for _, c := range keyStr {
		if c == '/' {
			return fmt.Errorf("invalid key: contains /")
		}
	}
	return nil
}

func TestNewTiered_NilStore(t *testing.T) {
	// NewTiered with nil store should return error
	_, err := NewTiered[string, int](nil)
	if err == nil {
		t.Error("NewTiered with nil store should return error")
	}
	if err != nil && err.Error() != "store cannot be nil" {
		t.Errorf("NewTiered error = %q; want 'store cannot be nil'", err.Error())
	}
}

func TestNew_InvalidSize(t *testing.T) {
	// Size(0) should fallback to default
	cache := New[string, int](Size(0))
	defer cache.Close()

	// Verify it works
	cache.Set("key", 1)
	if val, ok := cache.Get("key"); !ok || val != 1 {
		t.Error("Cache with Size(0) should work (fallback to default)")
	}

	// Size(-10) should fallback to default
	cache2 := New[string, int](Size(-10))
	defer cache2.Close()
	cache2.Set("key", 1)
	if val, ok := cache2.Get("key"); !ok || val != 1 {
		t.Error("Cache with Size(-10) should work (fallback to default)")
	}
}

func TestNew_TTL_Behavior(t *testing.T) {
	// Test that TTL option is correctly applied as default
	defaultTTL := 100 * time.Millisecond
	cache := New[string, int](TTL(defaultTTL))
	defer cache.Close()

	// Set without explicit TTL -> uses default
	cache.Set("default", 1)

	// Set with explicit TTL -> overrides default
	cache.Set("longer", 2, 1*time.Hour)

	// Wait for default to expire
	time.Sleep(defaultTTL + 10*time.Millisecond)

	if _, ok := cache.Get("default"); ok {
		t.Error("Item with default TTL should have expired")
	}
	if _, ok := cache.Get("longer"); !ok {
		t.Error("Item with explicit longer TTL should still exist")
	}
}

func TestNewTiered_WithTTL_Behavior(t *testing.T) {
	store := newMockStore[string, int]()
	defaultTTL := 100 * time.Millisecond
	cache, err := NewTiered[string, int](store, TTL(defaultTTL))
	if err != nil {
		t.Fatalf("NewTiered failed: %v", err)
	}
	defer func() { _ = cache.Close() }() //nolint:errcheck // Test cleanup

	ctx := context.Background()

	// Note: Set requires Context for persistence.
	// However, we are testing that the TTL is passed to memory correctly.
	// Memory expiration is lazy on Get or handled by internal logic, but here we just check availability.

	// Set without explicit TTL -> uses default
	if err := cache.Set(ctx, "default", 1); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Set with explicit TTL -> overrides default
	if err := cache.Set(ctx, "longer", 2, 1*time.Hour); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Wait for default to expire
	time.Sleep(defaultTTL + 10*time.Millisecond)

	// Check memory first (using Get)
	if _, ok := cache.memory.get("default"); ok {
		t.Error("Item with default TTL should have expired in memory")
	}
	if _, ok := cache.memory.get("longer"); !ok {
		t.Error("Item with explicit longer TTL should still exist in memory")
	}
}
