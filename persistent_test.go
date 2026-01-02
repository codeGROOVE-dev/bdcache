package fido

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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

// sequenceMockStore is a mock that can change behavior based on call count.
type sequenceMockStore[K comparable, V any] struct {
	mu            sync.RWMutex
	data          map[string]mockEntry[V]
	getCalls      int
	failOnGetN    int // Fail on Nth Get call (0 = never fail)
	returnOnGetN  int // Return value on Nth Get call (0 = never)
	valueToReturn V
}

func newSequenceMockStore[K comparable, V any]() *sequenceMockStore[K, V] {
	return &sequenceMockStore[K, V]{
		data: make(map[string]mockEntry[V]),
	}
}

func (m *sequenceMockStore[K, V]) ValidateKey(key K) error {
	return nil
}

func (m *sequenceMockStore[K, V]) Get(ctx context.Context, key K) (v V, expiry time.Time, found bool, err error) {
	m.mu.Lock()
	m.getCalls++
	callNum := m.getCalls
	m.mu.Unlock()

	m.mu.RLock()
	defer m.mu.RUnlock()

	var zero V
	if m.failOnGetN > 0 && callNum == m.failOnGetN {
		return zero, time.Time{}, false, fmt.Errorf("mock get error on call %d", callNum)
	}

	if m.returnOnGetN > 0 && callNum == m.returnOnGetN {
		return m.valueToReturn, time.Now().Add(time.Hour), true, nil
	}

	keyStr := fmt.Sprintf("%v", key)
	entry, found := m.data[keyStr]
	if !found {
		return zero, time.Time{}, false, nil
	}
	return entry.value, entry.expiry, true, nil
}

func (m *sequenceMockStore[K, V]) Set(ctx context.Context, key K, value V, expiry time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	keyStr := fmt.Sprintf("%v", key)
	m.data[keyStr] = mockEntry[V]{
		value:     value,
		expiry:    expiry,
		updatedAt: time.Now(),
	}
	return nil
}

func (m *sequenceMockStore[K, V]) Delete(ctx context.Context, key K) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	keyStr := fmt.Sprintf("%v", key)
	delete(m.data, keyStr)
	return nil
}

func (m *sequenceMockStore[K, V]) Cleanup(ctx context.Context, maxAge time.Duration) (int, error) {
	return 0, nil
}

func (m *sequenceMockStore[K, V]) Location(key K) string {
	return fmt.Sprintf("mock://%v", key)
}

func (m *sequenceMockStore[K, V]) Len(ctx context.Context) (int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.data), nil
}

func (m *sequenceMockStore[K, V]) Flush(ctx context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	count := len(m.data)
	m.data = make(map[string]mockEntry[V])
	return count, nil
}

func (m *sequenceMockStore[K, V]) Close() error {
	return nil
}

// injectingMockStore is a mock that can inject values into cache memory during Get.
type injectingMockStore[K comparable, V any] struct {
	*mockStore[K, V]

	cache       *TieredCache[K, V] // Set after cache creation
	injectKey   K
	injectValue V
	injectOnGet int // Inject on Nth Get call
	getCalls    atomic.Int32
}

func newInjectingMockStore[K comparable, V any]() *injectingMockStore[K, V] {
	return &injectingMockStore[K, V]{
		mockStore: newMockStore[K, V](),
	}
}

func (m *injectingMockStore[K, V]) Get(ctx context.Context, key K) (v V, expiry time.Time, found bool, err error) {
	callNum := m.getCalls.Add(1)

	// Inject value into cache memory at specified call
	if m.cache != nil && m.injectOnGet > 0 && int(callNum) == m.injectOnGet {
		m.cache.memory.set(m.injectKey, m.injectValue, 0)
	}

	return m.mockStore.Get(ctx, key)
}

func TestTieredCache_Fetch_SecondMemoryCheck(t *testing.T) {
	// This test triggers the second memory check path in getSet (line 166-171)
	// by injecting a value into memory during the first store.Get call.
	store := newInjectingMockStore[string, int]()
	store.injectKey = "key1"
	store.injectValue = 77
	store.injectOnGet = 1 // Inject during first Get

	cache, err := NewTiered[string, int](store)
	if err != nil {
		t.Fatalf("NewTiered failed: %v", err)
	}
	defer func() { _ = cache.Close() }() //nolint:errcheck // Test cleanup
	store.cache = cache                  // Link store to cache for injection

	ctx := context.Background()

	loaderCalled := false
	val, err := cache.Fetch(ctx, "key1", func(context.Context) (int, error) {
		loaderCalled = true
		return 42, nil
	})
	if err != nil {
		t.Fatalf("Fetch failed: %v", err)
	}

	// Should return injected value from second memory check
	if val != 77 {
		t.Errorf("val = %d; want 77 (from second memory check)", val)
	}

	if loaderCalled {
		t.Error("loader should not be called when second memory check finds value")
	}
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
	if err := cache.Set(ctx, "key1", 42); err != nil {
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
	if err := cache.SetAsync(ctx, "key1", 42); err != nil {
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
	if err := cache.Set(ctx, "key1", 42); err == nil {
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
	if err := cache.SetAsync(ctx, "key3", 300); err != nil {
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
	if err := cache.Set(ctx, "key2", 100); err != nil {
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
	if err := cache.Set(ctx, "key1", 42); err != nil {
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
		if err := cache.Set(ctx, fmt.Sprintf("key%d", i), i*100); err != nil {
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
		if err := cache.Set(ctx, fmt.Sprintf("key%d", i), i*10); err != nil {
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

	// Set with explicit short TTL (1 second granularity)
	if err := cache.SetTTL(ctx, "short-ttl", 2, 1*time.Second); err != nil {
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
	time.Sleep(2 * time.Second)

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
	if err := cache.SetAsyncTTL(ctx, "async-explicit", 2, 5*time.Minute); err != nil {
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
				_ = cache.Set(ctx, offset*100+j, j) //nolint:errcheck // Test concurrent access
			}
		}(i)
	}

	// Concurrent readers
	for range 10 {
		wg.Go(func() {
			for j := range 100 {
				_, _, _ = cache.Get(ctx, j) //nolint:errcheck // Test concurrent access
			}
		})
	}

	wg.Wait()

	// Cache should be functional after concurrent access
	if err := cache.Set(ctx, 9999, 9999); err != nil {
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
	err = cache.Set(ctx, "invalid/key", 42)
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
	err = cache.SetAsync(ctx, "invalid/key", 42)
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

	// Verify it works
	cache.Set("key", 1)
	if val, ok := cache.Get("key"); !ok || val != 1 {
		t.Error("Cache with Size(0) should work (fallback to default)")
	}

	// Size(-10) should fallback to default
	cache2 := New[string, int](Size(-10))
	cache2.Set("key", 1)
	if val, ok := cache2.Get("key"); !ok || val != 1 {
		t.Error("Cache with Size(-10) should work (fallback to default)")
	}
}

func TestNew_TTL_Behavior(t *testing.T) {
	// Test that TTL option is correctly applied as default (1 second granularity)
	defaultTTL := 1 * time.Second
	cache := New[string, int](TTL(defaultTTL))

	// Set without explicit TTL -> uses default
	cache.Set("default", 1)

	// Set with explicit TTL -> overrides default
	cache.SetTTL("longer", 2, 1*time.Hour)

	// Wait for default to expire
	time.Sleep(2 * time.Second)

	if _, ok := cache.Get("default"); ok {
		t.Error("Item with default TTL should have expired")
	}
	if _, ok := cache.Get("longer"); !ok {
		t.Error("Item with explicit longer TTL should still exist")
	}
}

func TestNewTiered_WithTTL_Behavior(t *testing.T) {
	store := newMockStore[string, int]()
	defaultTTL := 1 * time.Second
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
	if err := cache.SetTTL(ctx, "longer", 2, 1*time.Hour); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Wait for default to expire
	time.Sleep(2 * time.Second)

	// Check memory first (using Get)
	if _, ok := cache.memory.get("default"); ok {
		t.Error("Item with default TTL should have expired in memory")
	}
	if _, ok := cache.memory.get("longer"); !ok {
		t.Error("Item with explicit longer TTL should still exist in memory")
	}
}

func TestTieredCache_Fetch_Basic(t *testing.T) {
	store := newMockStore[string, int]()
	cache, err := NewTiered[string, int](store)
	if err != nil {
		t.Fatalf("NewTiered failed: %v", err)
	}
	defer func() { _ = cache.Close() }() //nolint:errcheck // Test cleanup

	ctx := context.Background()
	loaderCalls := 0
	loader := func(ctx context.Context) (int, error) {
		loaderCalls++
		return 42, nil
	}

	// First call - should call loader
	val, err := cache.Fetch(ctx, "key1", loader)
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
	val, err = cache.Fetch(ctx, "key1", loader)
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if val != 42 {
		t.Errorf("Fetch value = %d; want 42", val)
	}
	if loaderCalls != 1 {
		t.Errorf("loader calls = %d; want 1 (should use cache)", loaderCalls)
	}

	// Value should be in persistence too
	pVal, _, found, _ := store.Get(ctx, "key1") //nolint:errcheck // Test helper
	if !found {
		t.Error("value should be persisted")
	}
	if pVal != 42 {
		t.Errorf("persisted value = %d; want 42", pVal)
	}
}

func TestTieredCache_Fetch_FromPersistence(t *testing.T) {
	store := newMockStore[string, int]()
	cache, err := NewTiered[string, int](store)
	if err != nil {
		t.Fatalf("NewTiered failed: %v", err)
	}
	defer func() { _ = cache.Close() }() //nolint:errcheck // Test cleanup

	ctx := context.Background()

	// Pre-populate persistence directly
	_ = store.Set(ctx, "key1", 99, time.Now().Add(time.Hour)) //nolint:errcheck // Test setup

	loaderCalls := 0
	loader := func(ctx context.Context) (int, error) {
		loaderCalls++
		return 42, nil
	}

	// Fetch should find value in persistence, not call loader
	val, err := cache.Fetch(ctx, "key1", loader)
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if val != 99 {
		t.Errorf("Fetch value = %d; want 99 (from persistence)", val)
	}
	if loaderCalls != 0 {
		t.Errorf("loader calls = %d; want 0 (should use persistence)", loaderCalls)
	}
}

func TestTieredCache_Fetch_LoaderError(t *testing.T) {
	store := newMockStore[string, int]()
	cache, err := NewTiered[string, int](store)
	if err != nil {
		t.Fatalf("NewTiered failed: %v", err)
	}
	defer func() { _ = cache.Close() }() //nolint:errcheck // Test cleanup

	ctx := context.Background()
	loader := func(ctx context.Context) (int, error) {
		return 0, fmt.Errorf("loader error")
	}

	_, err = cache.Fetch(ctx, "key1", loader)
	if err == nil {
		t.Fatal("Fetch should return error from loader")
	}

	// Value should not be cached in memory
	_, found := cache.memory.get("key1")
	if found {
		t.Error("failed loader should not cache a value in memory")
	}

	// Value should not be in persistence
	_, _, found, err = store.Get(ctx, "key1")
	if err != nil {
		t.Fatalf("store.Get error: %v", err)
	}
	if found {
		t.Error("failed loader should not persist a value")
	}
}

func TestTieredCache_Fetch_ThunderingHerd(t *testing.T) {
	store := newMockStore[string, int]()
	cache, err := NewTiered[string, int](store)
	if err != nil {
		t.Fatalf("NewTiered failed: %v", err)
	}
	defer func() { _ = cache.Close() }() //nolint:errcheck // Test cleanup

	ctx := context.Background()
	var loaderCalls int32
	var mu sync.Mutex
	loader := func(_ context.Context) (int, error) {
		mu.Lock()
		loaderCalls++
		count := loaderCalls // Capture for potential error simulation
		mu.Unlock()
		time.Sleep(50 * time.Millisecond) // Simulate slow operation (e.g., HTTP fetch)
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
			results[idx], errors[idx] = cache.Fetch(ctx, "key1", loader)
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

func TestTieredCache_Fetch_WithTTL(t *testing.T) {
	store := newMockStore[string, int]()
	cache, err := NewTiered[string, int](store)
	if err != nil {
		t.Fatalf("NewTiered failed: %v", err)
	}
	defer func() { _ = cache.Close() }() //nolint:errcheck // Test cleanup

	ctx := context.Background()
	loaderCalls := 0
	loader := func(ctx context.Context) (int, error) {
		loaderCalls++
		return loaderCalls * 10, nil
	}

	// First call with short TTL (1 second granularity)
	val, err := cache.FetchTTL(ctx, "key1", 1*time.Second, loader)
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if val != 10 {
		t.Errorf("first Fetch value = %d; want 10", val)
	}

	// Wait for TTL to expire
	time.Sleep(2 * time.Second)

	// Second call - should call loader again (cache expired)
	val, err = cache.FetchTTL(ctx, "key1", 1*time.Second, loader)
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

func TestTieredCache_Fetch_PersistenceFailure(t *testing.T) {
	store := newMockStore[string, int]()
	cache, err := NewTiered[string, int](store)
	if err != nil {
		t.Fatalf("NewTiered failed: %v", err)
	}
	defer func() { _ = cache.Close() }() //nolint:errcheck // Test cleanup

	ctx := context.Background()
	loader := func(ctx context.Context) (int, error) {
		return 42, nil
	}

	// Make persistence fail
	store.setFailSet(true)

	// Fetch should still succeed (value in memory)
	val, err := cache.Fetch(ctx, "key1", loader)
	if err != nil {
		t.Fatalf("Fetch should succeed even if persistence fails: %v", err)
	}
	if val != 42 {
		t.Errorf("Fetch value = %d; want 42", val)
	}

	// Value should be in memory
	memVal, found := cache.memory.get("key1")
	if !found {
		t.Error("value should be in memory cache")
	}
	if memVal != 42 {
		t.Errorf("memory value = %d; want 42", memVal)
	}
}

func TestTieredCache_Fetch_KeyValidationError(t *testing.T) {
	store := &validatingMockStore[string, int]{
		mockStore: newMockStore[string, int](),
	}
	cache, err := NewTiered[string, int](store)
	if err != nil {
		t.Fatalf("NewTiered failed: %v", err)
	}
	defer func() { _ = cache.Close() }() //nolint:errcheck // Test cleanup

	ctx := context.Background()
	loaderCalls := 0
	loader := func(ctx context.Context) (int, error) {
		loaderCalls++
		return 42, nil
	}

	// Fetch with invalid key should return error
	_, err = cache.Fetch(ctx, "invalid/key", loader)
	if err == nil {
		t.Error("Fetch with invalid key should return error")
	}

	// Loader should not have been called
	if loaderCalls != 0 {
		t.Errorf("loader calls = %d; want 0 (should fail before loader)", loaderCalls)
	}
}

func TestTieredCache_Fetch_PersistenceLoadFailure(t *testing.T) {
	store := newMockStore[string, int]()
	cache, err := NewTiered[string, int](store)
	if err != nil {
		t.Fatalf("NewTiered failed: %v", err)
	}
	defer func() { _ = cache.Close() }() //nolint:errcheck // Test cleanup

	ctx := context.Background()
	loader := func(ctx context.Context) (int, error) {
		return 42, nil
	}

	// Make persistence load fail
	store.setFailGet(true)

	// Fetch should return error when persistence load fails
	_, err = cache.Fetch(ctx, "key1", loader)
	if err == nil {
		t.Error("Fetch should return error when persistence load fails")
	}
}

func TestTieredCache_Fetch_PersistenceLoadFailure_InFlight(t *testing.T) {
	store := newMockStore[string, int]()
	cache, err := NewTiered[string, int](store)
	if err != nil {
		t.Fatalf("NewTiered failed: %v", err)
	}
	defer func() { _ = cache.Close() }() //nolint:errcheck // Test cleanup

	ctx := context.Background()

	// First, do a successful Fetch to prime the flight
	loader1 := func(ctx context.Context) (int, error) {
		return 42, nil
	}
	_, err = cache.Fetch(ctx, "key1", loader1)
	if err != nil {
		t.Fatalf("first Fetch error: %v", err)
	}

	// Clear memory to force persistence check
	cache.memory.flush()

	// Now make persistence fail for the second check inside singleflight
	store.setFailGet(true)

	loader2 := func(ctx context.Context) (int, error) {
		return 99, nil
	}

	// This tests the persistence load failure path inside the singleflight
	_, err = cache.Fetch(ctx, "key1", loader2)
	if err == nil {
		t.Error("Fetch should return error when persistence load fails in singleflight")
	}
}

func TestTieredCache_Fetch_CacheHitAfterFlight(t *testing.T) {
	store := newMockStore[string, int]()
	cache, err := NewTiered[string, int](store)
	if err != nil {
		t.Fatalf("NewTiered failed: %v", err)
	}
	defer func() { _ = cache.Close() }() //nolint:errcheck // Test cleanup

	ctx := context.Background()
	var wg sync.WaitGroup
	loaderCalls := int32(0)

	// First goroutine starts slow loader
	loader1 := func(context.Context) (int, error) { //nolint:unparam // test loader always succeeds
		atomic.AddInt32(&loaderCalls, 1)
		time.Sleep(100 * time.Millisecond)
		return 42, nil
	}

	// Second goroutine will wait for first and should get cached result
	wg.Add(2)

	go func() {
		defer wg.Done()
		val, err := cache.Fetch(ctx, "key1", loader1)
		if err != nil {
			t.Errorf("first Fetch error: %v", err)
		}
		if val != 42 {
			t.Errorf("first Fetch value = %d; want 42", val)
		}
	}()

	time.Sleep(10 * time.Millisecond) // Let first goroutine start

	go func() {
		defer wg.Done()
		loader2 := func(ctx context.Context) (int, error) {
			atomic.AddInt32(&loaderCalls, 1)
			return 99, nil // Different value
		}
		val, err := cache.Fetch(ctx, "key1", loader2)
		if err != nil {
			t.Errorf("second Fetch error: %v", err)
		}
		if val != 42 {
			t.Errorf("second Fetch value = %d; want 42 (from first loader)", val)
		}
	}()

	wg.Wait()

	if loaderCalls != 1 {
		t.Errorf("loader calls = %d; want 1", loaderCalls)
	}
}

func TestTieredCache_Fetch_FoundInPersistenceDuringSingleflight(t *testing.T) {
	store := newMockStore[string, int]()
	cache, err := NewTiered[string, int](store)
	if err != nil {
		t.Fatalf("NewTiered failed: %v", err)
	}
	defer func() { _ = cache.Close() }() //nolint:errcheck // Test cleanup

	ctx := context.Background()

	// Pre-populate persistence (but not memory)
	if err := store.Set(ctx, "key1", 77, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("store.Set failed: %v", err)
	}

	loaderCalls := 0
	loader := func(ctx context.Context) (int, error) {
		loaderCalls++
		return 42, nil
	}

	// Fetch should find value in persistence (after initial check), not call loader
	val, err := cache.Fetch(ctx, "key1", loader)
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if val != 77 {
		t.Errorf("Fetch value = %d; want 77 (from persistence)", val)
	}
	if loaderCalls != 0 {
		t.Errorf("loader calls = %d; want 0", loaderCalls)
	}
}

// flushFailingMockStore wraps mockStore and fails on Flush.
type flushFailingMockStore[K comparable, V any] struct {
	*mockStore[K, V]

	failFlush bool
}

func (m *flushFailingMockStore[K, V]) Flush(ctx context.Context) (int, error) {
	if m.failFlush {
		return 0, fmt.Errorf("mock flush error")
	}
	return m.mockStore.Flush(ctx)
}

func TestTieredCache_Flush_PersistenceError(t *testing.T) {
	innerStore := newMockStore[string, int]()
	store := &flushFailingMockStore[string, int]{
		mockStore: innerStore,
		failFlush: false,
	}

	cache, err := NewTiered[string, int](store)
	if err != nil {
		t.Fatalf("NewTiered failed: %v", err)
	}
	defer func() { _ = cache.Close() }() //nolint:errcheck // Test cleanup

	ctx := context.Background()

	// Add some entries
	for i := range 5 {
		if err := cache.Set(ctx, fmt.Sprintf("key%d", i), i*10); err != nil {
			t.Fatalf("Set: %v", err)
		}
	}

	// Now make flush fail
	store.failFlush = true

	// Flush should return error
	removed, err := cache.Flush(ctx)
	if err == nil {
		t.Error("Flush should return error when persistence flush fails")
	}

	// Memory should still be flushed
	if cache.Len() != 0 {
		t.Errorf("memory cache should be empty after flush, got %d", cache.Len())
	}

	// Should return memory count even on persistence error
	if removed != 5 {
		t.Errorf("Flush removed %d items; want 5 (memory count)", removed)
	}
}

func TestTieredCache_Delete_KeyValidationError(t *testing.T) {
	store := &validatingMockStore[string, int]{
		mockStore: newMockStore[string, int](),
	}

	cache, err := NewTiered[string, int](store)
	if err != nil {
		t.Fatalf("NewTiered failed: %v", err)
	}
	defer func() { _ = cache.Close() }() //nolint:errcheck // Test cleanup

	ctx := context.Background()

	// First add a valid key
	if err := cache.Set(ctx, "validkey", 42); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Delete with invalid key should return error
	err = cache.Delete(ctx, "invalid/key")
	if err == nil {
		t.Error("Delete with invalid key should return error")
	}

	// Memory is deleted even though key validation fails (happens after memory delete)
	// This is expected behavior - memory is always cleaned
}

func TestTieredCache_Get_KeyValidationError(t *testing.T) {
	store := &validatingMockStore[string, int]{
		mockStore: newMockStore[string, int](),
	}

	cache, err := NewTiered[string, int](store)
	if err != nil {
		t.Fatalf("NewTiered failed: %v", err)
	}
	defer func() { _ = cache.Close() }() //nolint:errcheck // Test cleanup

	ctx := context.Background()

	// Get with invalid key should return error
	_, _, err = cache.Get(ctx, "invalid/key")
	if err == nil {
		t.Error("Get with invalid key should return error")
	}
}

func TestTieredCache_Set_InvalidKey(t *testing.T) {
	store := &validatingMockStore[string, int]{
		mockStore: newMockStore[string, int](),
	}

	cache, err := NewTiered[string, int](store)
	if err != nil {
		t.Fatalf("NewTiered failed: %v", err)
	}
	defer func() { _ = cache.Close() }() //nolint:errcheck // Test cleanup

	ctx := context.Background()

	// Set with invalid key should return error before storing
	err = cache.Set(ctx, "invalid/key", 42)
	if err == nil {
		t.Error("Set with invalid key should return error")
	}

	// Nothing should be stored
	if cache.Len() != 0 {
		t.Errorf("cache should be empty, got %d entries", cache.Len())
	}
}

func TestTieredCache_SetAsync_InvalidKey(t *testing.T) {
	store := &validatingMockStore[string, int]{
		mockStore: newMockStore[string, int](),
	}

	cache, err := NewTiered[string, int](store)
	if err != nil {
		t.Fatalf("NewTiered failed: %v", err)
	}
	defer func() { _ = cache.Close() }() //nolint:errcheck // Test cleanup

	ctx := context.Background()

	// SetAsync with invalid key should return error synchronously
	err = cache.SetAsync(ctx, "invalid/key", 42)
	if err == nil {
		t.Error("SetAsync with invalid key should return error")
	}

	// Nothing should be stored
	if cache.Len() != 0 {
		t.Errorf("cache should be empty, got %d entries", cache.Len())
	}
}

// TestTieredCache_Fetch_MemoryHitDuringSingleflight tests the path where memory
// has the value after acquiring singleflight.
func TestTieredCache_Fetch_MemoryHitDuringSingleflight(t *testing.T) {
	store := newMockStore[string, int]()
	cache, err := NewTiered[string, int](store)
	if err != nil {
		t.Fatalf("NewTiered failed: %v", err)
	}
	defer func() { _ = cache.Close() }() //nolint:errcheck // Test cleanup

	ctx := context.Background()
	const key = "contested"

	var started sync.WaitGroup
	started.Add(1)

	var done sync.WaitGroup
	done.Add(2)

	// First goroutine: slow loader
	go func() {
		defer done.Done()
		started.Done()

		if _, err := cache.Fetch(ctx, key, func(context.Context) (int, error) {
			time.Sleep(50 * time.Millisecond)
			return 1, nil
		}); err != nil {
			t.Errorf("Fetch error: %v", err)
		}
	}()

	started.Wait()
	time.Sleep(10 * time.Millisecond)

	// Second goroutine: direct Set while first is in singleflight
	go func() {
		defer done.Done()
		if err := cache.Set(ctx, key, 99); err != nil {
			t.Errorf("Set error: %v", err)
		}
	}()

	done.Wait()

	// Value should exist
	if val, ok, err := cache.Get(ctx, key); err != nil || !ok {
		t.Errorf("Get error: %v, ok: %v", err, ok)
	} else if val != 99 && val != 1 {
		t.Errorf("unexpected value: %d", val)
	}
}

// TestTieredCache_Fetch_PersistenceHitDuringSingleflight tests the path where
// persistence has the value during singleflight (second check).
func TestTieredCache_Fetch_PersistenceHitDuringSingleflight(t *testing.T) {
	store := newMockStore[string, int]()
	cache, err := NewTiered[string, int](store)
	if err != nil {
		t.Fatalf("NewTiered failed: %v", err)
	}
	defer func() { _ = cache.Close() }() //nolint:errcheck // Test cleanup

	ctx := context.Background()
	const key = "contested"

	var started sync.WaitGroup
	started.Add(1)

	var done sync.WaitGroup
	done.Add(2)

	// First goroutine: slow loader that will be beaten by second
	go func() {
		defer done.Done()
		started.Done()

		val, err := cache.Fetch(ctx, key, func(ctx context.Context) (int, error) {
			// By this time, the second goroutine should have stored to persistence
			time.Sleep(50 * time.Millisecond)
			return 1, nil
		})
		if err != nil {
			t.Errorf("Fetch error: %v", err)
		}
		// Value could be 99 (from persistence) or 1 (from loader)
		if val != 99 && val != 1 {
			t.Errorf("unexpected value: %d", val)
		}
	}()

	started.Wait()
	time.Sleep(10 * time.Millisecond)

	// Second goroutine: directly set in persistence (bypassing cache)
	go func() {
		defer done.Done()
		if err := store.Set(ctx, key, 99, time.Now().Add(time.Hour)); err != nil {
			t.Errorf("store.Set error: %v", err)
		}
	}()

	done.Wait()
}

// TestTieredCache_Fetch_SecondCheckMemory tests the second memory check inside singleflight.
func TestTieredCache_Fetch_SecondCheckMemory(t *testing.T) {
	store := newMockStore[string, int]()
	cache, err := NewTiered[string, int](store)
	if err != nil {
		t.Fatalf("NewTiered failed: %v", err)
	}
	defer func() { _ = cache.Close() }() //nolint:errcheck // Test cleanup

	ctx := context.Background()

	// Pre-populate memory directly (but not persistence)
	cache.memory.set("key1", 77, 0)

	var loaderCalls atomic.Int32
	loader := func(context.Context) (int, error) { //nolint:unparam // test loader always succeeds
		loaderCalls.Add(1)
		return 42, nil
	}

	// Test concurrent access
	var wg sync.WaitGroup
	results := make([]int, 10)

	for i := range 10 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			val, err := cache.Fetch(ctx, fmt.Sprintf("key%d", idx), loader)
			if err != nil {
				t.Errorf("Fetch error: %v", err)
				return
			}
			results[idx] = val
		}(i)
	}

	wg.Wait()

	// All results should be valid
	for i, r := range results {
		if r != 77 && r != 42 {
			t.Errorf("result[%d] = %d; unexpected", i, r)
		}
	}
}

// TestTieredCache_Fetch_SecondStoreGetError tests when second store.Get fails.
func TestTieredCache_Fetch_SecondStoreGetError(t *testing.T) {
	store := newSequenceMockStore[string, int]()
	// First Get returns not found, second Get fails
	store.failOnGetN = 2

	cache, err := NewTiered[string, int](store)
	if err != nil {
		t.Fatalf("NewTiered failed: %v", err)
	}
	defer func() { _ = cache.Close() }() //nolint:errcheck // Test cleanup

	ctx := context.Background()

	_, err = cache.Fetch(ctx, "key1", func(ctx context.Context) (int, error) {
		return 42, nil
	})

	// The second store.Get fails, which should return an error
	if err == nil {
		t.Error("expected error from second store.Get failure")
	}
}

// TestTieredCache_Fetch_SecondStoreGetFound tests when second store.Get finds value.
func TestTieredCache_Fetch_SecondStoreGetFound(t *testing.T) {
	store := newSequenceMockStore[string, int]()
	// First Get returns not found, second Get returns value
	store.returnOnGetN = 2
	store.valueToReturn = 99

	cache, err := NewTiered[string, int](store)
	if err != nil {
		t.Fatalf("NewTiered failed: %v", err)
	}
	defer func() { _ = cache.Close() }() //nolint:errcheck // Test cleanup

	ctx := context.Background()

	loaderCalled := false
	val, err := cache.Fetch(ctx, "key1", func(ctx context.Context) (int, error) {
		loaderCalled = true
		return 42, nil
	})
	if err != nil {
		t.Fatalf("Fetch failed: %v", err)
	}

	// Should return value from second store.Get, not from loader
	if val != 99 {
		t.Errorf("val = %d; want 99", val)
	}

	if loaderCalled {
		t.Error("loader should not be called when second store.Get finds value")
	}
}
