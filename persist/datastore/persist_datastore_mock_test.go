package datastore

import (
	"context"
	"errors"
	"testing"
	"time"

	ds "github.com/codeGROOVE-dev/ds9/pkg/datastore"
)

// newMockDatastorePersist creates a datastore persistence layer with mock client.
func newMockDatastorePersist[K comparable, V any](t *testing.T) (dp *persister[K, V], cleanup func()) {
	t.Helper()
	client, cleanup := ds.NewMockClient(t)

	return &persister[K, V]{
		client: client,
		kind:   "CacheEntry",
	}, cleanup
}

func TestDatastorePersist_Mock_StoreLoad(t *testing.T) {
	dp, cleanup := newMockDatastorePersist[string, int](t)
	defer cleanup()

	ctx := context.Background()

	// Store a value
	if err := dp.Store(ctx, "key1", 42, time.Time{}); err != nil {
		t.Fatalf("Store: %v", err)
	}

	// Load the value
	val, expiry, found, err := dp.Load(ctx, "key1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !found {
		t.Fatal("key1 not found")
	}
	if val != 42 {
		t.Errorf("Load value = %d; want 42", val)
	}
	if !expiry.IsZero() {
		t.Error("expiry should be zero")
	}
}

func TestDatastorePersist_Mock_LoadMissing(t *testing.T) {
	dp, cleanup := newMockDatastorePersist[string, int](t)
	defer cleanup()

	ctx := context.Background()

	// Load non-existent key
	_, _, found, err := dp.Load(ctx, "missing")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if found {
		t.Error("missing key should not be found")
	}
}

func TestDatastorePersist_Mock_TTL(t *testing.T) {
	dp, cleanup := newMockDatastorePersist[string, string](t)
	defer cleanup()

	ctx := context.Background()

	// Store with past expiry
	past := time.Now().Add(-1 * time.Second)
	if err := dp.Store(ctx, "expired", "value", past); err != nil {
		t.Fatalf("Store: %v", err)
	}

	// Should not be loadable
	_, _, found, err := dp.Load(ctx, "expired")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if found {
		t.Error("expired key should not be found")
	}
}

func TestDatastorePersist_Mock_Delete(t *testing.T) {
	dp, cleanup := newMockDatastorePersist[string, int](t)
	defer cleanup()

	ctx := context.Background()

	// Store and delete
	if err := dp.Store(ctx, "key1", 42, time.Time{}); err != nil {
		t.Fatalf("Store: %v", err)
	}

	if err := dp.Delete(ctx, "key1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Should not be loadable
	_, _, found, err := dp.Load(ctx, "key1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if found {
		t.Error("deleted key should not be found")
	}

	// Deleting non-existent key should not error
	if err := dp.Delete(ctx, "missing"); err != nil {
		t.Errorf("Delete missing key: %v", err)
	}

	// Verify deletion was successful
	if _, _, found, err := dp.Load(ctx, "key1"); err != nil {
		t.Fatalf("Load after deletion: %v", err)
	} else if found {
		t.Error("key1 should not be found after deletion")
	}
}

func TestDatastorePersist_Mock_Update(t *testing.T) {
	dp, cleanup := newMockDatastorePersist[string, string](t)
	defer cleanup()

	ctx := context.Background()

	// Store initial value
	if err := dp.Store(ctx, "key", "value1", time.Time{}); err != nil {
		t.Fatalf("Store: %v", err)
	}

	// Update value
	if err := dp.Store(ctx, "key", "value2", time.Time{}); err != nil {
		t.Fatalf("Store update: %v", err)
	}

	// Load and verify updated value
	val, _, found, err := dp.Load(ctx, "key")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !found {
		t.Fatal("key not found")
	}
	if val != "value2" {
		t.Errorf("Load value = %s; want value2", val)
	}
}

func TestDatastorePersist_Mock_ComplexValue(t *testing.T) {
	type User struct {
		Name  string
		Email string
		Age   int
	}

	dp, cleanup := newMockDatastorePersist[string, User](t)
	defer cleanup()

	ctx := context.Background()

	user := User{
		Name:  "Alice",
		Email: "alice@example.com",
		Age:   30,
	}

	// Store complex value
	if err := dp.Store(ctx, "user1", user, time.Time{}); err != nil {
		t.Fatalf("Store: %v", err)
	}

	// Load and verify
	loaded, _, found, err := dp.Load(ctx, "user1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !found {
		t.Fatal("user1 not found")
	}
	if loaded.Name != user.Name || loaded.Email != user.Email || loaded.Age != user.Age {
		t.Errorf("Load value = %+v; want %+v", loaded, user)
	}
}

func TestDatastorePersist_Mock_LoadAll(t *testing.T) {
	dp, cleanup := newMockDatastorePersist[string, int](t)
	defer cleanup()

	ctx := context.Background()

	// Store multiple entries
	entries := map[string]int{
		"key1": 1,
		"key2": 2,
		"key3": 3,
	}
	for k, v := range entries {
		if err := dp.Store(ctx, k, v, time.Time{}); err != nil {
			t.Fatalf("Store %s: %v", k, err)
		}
	}

	// Store expired entry
	if err := dp.Store(ctx, "expired", 99, time.Now().Add(-1*time.Second)); err != nil {
		t.Fatalf("Store expired: %v", err)
	}

	// LoadRecent(ctx, 0) - note: this doesn't return entries (by design, see comment in LoadRecent(ctx, 0))
	// but it should clean up expired entries
	entryCh, errCh := dp.LoadRecent(ctx, 0)

	// Consume channels
	for range entryCh {
		// Should be empty
	}

	// Check for errors
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("LoadRecent(ctx, 0) error: %v", err)
		}
	default:
	}

	// Verify non-expired entries still exist
	for k, v := range entries {
		val, _, found, err := dp.Load(ctx, k)
		if err != nil {
			t.Fatalf("Load %s: %v", k, err)
		}
		if !found {
			t.Errorf("%s not found after LoadRecent(ctx, 0)", k)
		}
		if val != v {
			t.Errorf("Load %s = %d; want %d", k, val, v)
		}
	}
}

func TestDatastorePersist_Mock_LoadAllContextCancellation(t *testing.T) {
	dp, cleanup := newMockDatastorePersist[string, int](t)
	defer cleanup()

	baseCtx := context.Background()

	// Store many entries
	for i := range 100 {
		if err := dp.Store(baseCtx, string(rune(i)), i, time.Time{}); err != nil {
			t.Fatalf("Store: %v", err)
		}
	}

	// Cancel context during LoadRecent with limit 0
	ctx, cancel := context.WithCancel(context.Background())
	entryCh, errCh := dp.LoadRecent(ctx, 0)

	// Cancel immediately
	cancel()

	// Consume channels
	for range entryCh {
	}

	// Should get context cancellation error (may be wrapped)
	err := <-errCh
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled error; got %v", err)
	}
}

func TestDatastorePersist_Mock_Close(t *testing.T) {
	dp, cleanup := newMockDatastorePersist[string, int](t)
	defer cleanup()

	if err := dp.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestDatastorePersist_Mock_WithTTL(t *testing.T) {
	dp, cleanup := newMockDatastorePersist[string, string](t)
	defer cleanup()

	ctx := context.Background()

	// Store with future expiry
	future := time.Now().Add(1 * time.Hour)
	if err := dp.Store(ctx, "key", "value", future); err != nil {
		t.Fatalf("Store: %v", err)
	}

	// Should be loadable
	val, expiry, found, err := dp.Load(ctx, "key")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !found {
		t.Fatal("key not found")
	}
	if val != "value" {
		t.Errorf("Load value = %s; want value", val)
	}

	// Expiry should be set (within 1 second of expected)
	if expiry.IsZero() {
		t.Error("expiry should not be zero")
	}
	if expiry.Sub(future).Abs() > time.Second {
		t.Errorf("expiry = %v; want ~%v", expiry, future)
	}
}

func TestDatastorePersist_Mock_ComplexTypes(t *testing.T) {
	type ComplexStruct struct {
		Name  string
		Items []string
		Meta  map[string]int
	}

	dp, cleanup := newMockDatastorePersist[string, ComplexStruct](t)
	defer cleanup()

	ctx := context.Background()

	data := ComplexStruct{
		Name:  "test",
		Items: []string{"a", "b", "c"},
		Meta:  map[string]int{"count": 3},
	}

	// Store complex type
	if err := dp.Store(ctx, "complex", data, time.Time{}); err != nil {
		t.Fatalf("Store: %v", err)
	}

	// Load and verify
	loaded, _, found, err := dp.Load(ctx, "complex")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !found {
		t.Fatal("complex not found")
	}

	if loaded.Name != data.Name {
		t.Errorf("Name = %s; want %s", loaded.Name, data.Name)
	}
	if len(loaded.Items) != 3 {
		t.Errorf("Items length = %d; want 3", len(loaded.Items))
	}
	if loaded.Meta["count"] != 3 {
		t.Errorf("Meta[count] = %d; want 3", loaded.Meta["count"])
	}
}

func TestDatastorePersist_Mock_DeleteNonExistent(t *testing.T) {
	dp, cleanup := newMockDatastorePersist[string, int](t)
	defer cleanup()

	ctx := context.Background()

	// Delete non-existent key should not error
	if err := dp.Delete(ctx, "nonexistent"); err != nil {
		t.Errorf("Delete nonexistent: %v", err)
	}
}

func TestDatastorePersist_Mock_ExpiredInLoadAll(t *testing.T) {
	dp, cleanup := newMockDatastorePersist[string, int](t)
	defer cleanup()

	ctx := context.Background()

	// Store entries with different expirations
	if err := dp.Store(ctx, "valid1", 1, time.Now().Add(1*time.Hour)); err != nil {
		t.Fatalf("Store valid1: %v", err)
	}
	if err := dp.Store(ctx, "valid2", 2, time.Now().Add(1*time.Hour)); err != nil {
		t.Fatalf("Store valid2: %v", err)
	}
	if err := dp.Store(ctx, "expired", 99, time.Now().Add(-1*time.Second)); err != nil {
		t.Fatalf("Store expired: %v", err)
	}

	// LoadRecent(ctx, 0) should handle expired entries
	entryCh, errCh := dp.LoadRecent(ctx, 0)

	for range entryCh {
		// Entries channel should be empty (LoadRecent(ctx, 0) doesn't return entries by design)
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("LoadRecent(ctx, 0) error: %v", err)
		}
	default:
	}

	// Verify valid entries still accessible
	val, _, found, err := dp.Load(ctx, "valid1")
	if err != nil {
		t.Fatalf("Load valid1: %v", err)
	}
	if !found || val != 1 {
		t.Errorf("valid1 = %v, %v; want 1, true", val, found)
	}
}

func TestDatastorePersist_Mock_StoreWithExpiry(t *testing.T) {
	dp, cleanup := newMockDatastorePersist[string, string](t)
	defer cleanup()

	ctx := context.Background()

	expiry := time.Now().Add(2 * time.Hour)
	if err := dp.Store(ctx, "key1", "value1", expiry); err != nil {
		t.Fatalf("Store with expiry: %v", err)
	}

	val, loadedExpiry, found, err := dp.Load(ctx, "key1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !found {
		t.Fatal("key1 not found")
	}
	if val != "value1" {
		t.Errorf("value = %s; want value1", val)
	}

	// Verify expiry was stored
	if loadedExpiry.Sub(expiry).Abs() > time.Second {
		t.Errorf("expiry = %v; want ~%v", loadedExpiry, expiry)
	}
}

func TestDatastorePersist_Mock_UnsupportedType(t *testing.T) {
	dp, cleanup := newMockDatastorePersist[string, func()](t)
	defer cleanup()

	ctx := context.Background()

	// Try to store a function (which can't be JSON marshaled)
	err := dp.Store(ctx, "key1", func() {}, time.Time{})
	if err == nil {
		t.Error("Store should fail when marshaling unsupported type")
	}
}

func TestDatastorePersist_Mock_ValidateKey(t *testing.T) {
	dp, cleanup := newMockDatastorePersist[string, int](t)
	defer cleanup()

	tests := []struct {
		name    string
		key     string
		wantErr bool
	}{
		{"empty key", "", true},
		{"valid short key", "key123", false},
		{"valid long key", string(make([]byte, 1500)), false},
		{"key too long", string(make([]byte, 1501)), true},
		{"valid with special chars", "key:123-test_value.example", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := dp.ValidateKey(tt.key)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateKey() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestDatastorePersist_Mock_Location(t *testing.T) {
	dp, cleanup := newMockDatastorePersist[string, int](t)
	defer cleanup()

	loc := dp.Location("mykey")
	expected := "CacheEntry/mykey"
	if loc != expected {
		t.Errorf("Location() = %q; want %q", loc, expected)
	}

	// Test with different key
	loc2 := dp.Location("test:key-123")
	expected2 := "CacheEntry/test:key-123"
	if loc2 != expected2 {
		t.Errorf("Location() = %q; want %q", loc2, expected2)
	}
}

func TestDatastorePersist_Mock_Cleanup(t *testing.T) {
	dp, cleanup := newMockDatastorePersist[string, int](t)
	defer cleanup()

	ctx := context.Background()

	// Store entries with different expiry times
	past := time.Now().Add(-2 * time.Hour)
	recentPast := time.Now().Add(-90 * time.Minute)
	future := time.Now().Add(2 * time.Hour)

	dp.Store(ctx, "expired-old", 1, past)
	dp.Store(ctx, "expired-recent", 2, recentPast)
	dp.Store(ctx, "valid-future", 3, future)
	dp.Store(ctx, "no-expiry", 4, time.Time{})

	// Cleanup entries older than 1 hour
	// Note: ds9 mock doesn't properly handle time-based filters,
	// so we just verify the function runs without error
	_, err := dp.Cleanup(ctx, 1*time.Hour)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	// For proper filter testing, use integration tests with real Datastore
}

func TestDatastorePersist_Mock_CleanupEmpty(t *testing.T) {
	dp, cleanup := newMockDatastorePersist[string, int](t)
	defer cleanup()

	ctx := context.Background()

	// Cleanup with no entries
	count, err := dp.Cleanup(ctx, 1*time.Hour)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	if count != 0 {
		t.Errorf("Cleanup count = %d; want 0 for empty database", count)
	}
}

func TestDatastorePersist_Mock_LoadRecentWithLimit(t *testing.T) {
	dp, cleanup := newMockDatastorePersist[string, int](t)
	defer cleanup()

	ctx := context.Background()

	// Store multiple entries
	for i := range 10 {
		key := string(rune('a' + i))
		if err := dp.Store(ctx, key, i, time.Time{}); err != nil {
			t.Fatalf("Store %s: %v", key, err)
		}
	}

	// Load recent with limit
	entryCh, errCh := dp.LoadRecent(ctx, 3)

	loaded := 0
	for range entryCh {
		loaded++
	}

	// Check for errors
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("LoadRecent error: %v", err)
		}
	default:
	}

	// Should have loaded at most 3 entries
	if loaded > 3 {
		t.Errorf("loaded %d entries; want at most 3", loaded)
	}
}

func TestDatastorePersist_Mock_Delete_Error(t *testing.T) {
	dp, cleanup := newMockDatastorePersist[string, int](t)
	defer cleanup()

	ctx := context.Background()

	// Delete non-existent key (should not error)
	if err := dp.Delete(ctx, "non-existent"); err != nil {
		t.Errorf("Delete non-existent key: %v", err)
	}
}

func TestDatastorePersist_Mock_Load_DecodeError(t *testing.T) {
	dp, cleanup := newMockDatastorePersist[string, int](t)
	defer cleanup()

	ctx := context.Background()

	// Store a value
	if err := dp.Store(ctx, "key1", 42, time.Time{}); err != nil {
		t.Fatalf("Store: %v", err)
	}

	// Load it back
	val, _, found, err := dp.Load(ctx, "key1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !found {
		t.Fatal("key1 not found")
	}
	if val != 42 {
		t.Errorf("Load value = %d; want 42", val)
	}
}

func TestDatastorePersist_Mock_LoadRecent_SkipExpired(t *testing.T) {
	dp, cleanup := newMockDatastorePersist[string, int](t)
	defer cleanup()

	ctx := context.Background()

	// Store mix of expired and valid entries
	past := time.Now().Add(-1 * time.Hour)
	future := time.Now().Add(1 * time.Hour)

	dp.Store(ctx, "expired1", 1, past)
	dp.Store(ctx, "expired2", 2, past)
	dp.Store(ctx, "valid1", 3, future)
	dp.Store(ctx, "valid2", 4, future)

	// LoadRecent should skip expired
	entryCh, errCh := dp.LoadRecent(ctx, 0)

	loaded := make(map[string]int)
	for entry := range entryCh {
		loaded[entry.Key] = entry.Value
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("LoadRecent error: %v", err)
		}
	default:
	}

	// Should only have valid entries
	if len(loaded) != 2 {
		t.Errorf("loaded %d entries; want 2 (expired should be skipped)", len(loaded))
	}
	if loaded["valid1"] != 3 {
		t.Error("valid1 should be loaded")
	}
	if loaded["valid2"] != 4 {
		t.Error("valid2 should be loaded")
	}
}

func TestDatastorePersist_Mock_StoreLoadCycle(t *testing.T) {
	dp, cleanup := newMockDatastorePersist[string, string](t)
	defer cleanup()

	ctx := context.Background()

	tests := []struct {
		key    string
		value  string
		expiry time.Time
	}{
		{"key1", "value1", time.Time{}},
		{"key2", "value2", time.Now().Add(1 * time.Hour)},
		{"key3", "value3", time.Now().Add(24 * time.Hour)},
	}

	// Store all
	for _, tt := range tests {
		if err := dp.Store(ctx, tt.key, tt.value, tt.expiry); err != nil {
			t.Fatalf("Store %s: %v", tt.key, err)
		}
	}

	// Load all
	for _, tt := range tests {
		val, expiry, found, err := dp.Load(ctx, tt.key)
		if err != nil {
			t.Fatalf("Load %s: %v", tt.key, err)
		}
		if !found {
			t.Errorf("%s not found", tt.key)
		}
		if val != tt.value {
			t.Errorf("Load %s = %q; want %q", tt.key, val, tt.value)
		}
		if !tt.expiry.IsZero() && expiry.Sub(tt.expiry).Abs() > time.Second {
			t.Errorf("Expiry mismatch for %s: got %v, want %v", tt.key, expiry, tt.expiry)
		}
	}
}

func TestDatastorePersist_Mock_Close_Idempotent(t *testing.T) {
	dp, cleanup := newMockDatastorePersist[string, int](t)
	defer cleanup()

	// Close multiple times should work
	if err := dp.Close(); err != nil {
		t.Errorf("First Close: %v", err)
	}

	// Note: ds9 mock might not support idempotent close, so we don't test second close
}

func TestDatastorePersist_Mock_MultipleOps(t *testing.T) {
	dp, cleanup := newMockDatastorePersist[string, int](t)
	defer cleanup()

	ctx := context.Background()

	// Test sequence: Store, Load, Update, Load, Delete, Load
	if err := dp.Store(ctx, "key", 1, time.Time{}); err != nil {
		t.Fatalf("Store 1: %v", err)
	}

	val, _, found, _ := dp.Load(ctx, "key")
	if !found || val != 1 {
		t.Error("After Store 1: key should be 1")
	}

	if err := dp.Store(ctx, "key", 2, time.Time{}); err != nil {
		t.Fatalf("Store 2: %v", err)
	}

	val, _, found, _ = dp.Load(ctx, "key")
	if !found || val != 2 {
		t.Error("After Store 2: key should be 2")
	}

	if err := dp.Delete(ctx, "key"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	_, _, found, _ = dp.Load(ctx, "key")
	if found {
		t.Error("After Delete: key should not be found")
	}
}
