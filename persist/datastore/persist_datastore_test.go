package datastore

import (
	"context"
	"os"
	"testing"
	"time"
)

// Note: These tests require DATASTORE_EMULATOR_HOST to be set or actual GCP credentials.
// They will be skipped if the environment is not configured.

func skipIfNoDatastore(t *testing.T) {
	t.Helper()
	if os.Getenv("DATASTORE_EMULATOR_HOST") == "" && os.Getenv("GOOGLE_APPLICATION_CREDENTIALS") == "" {
		t.Skip("Skipping datastore tests: no emulator or credentials configured")
	}
}

func TestDatastorePersist_StoreLoad(t *testing.T) {
	skipIfNoDatastore(t)

	ctx := context.Background()
	dp, err := New[string, int](ctx, "test-cache")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := dp.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

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

	// Cleanup
	if err := dp.Delete(ctx, "key1"); err != nil {
		t.Logf("Delete error: %v", err)
	}
}

func TestDatastorePersist_LoadMissing(t *testing.T) {
	skipIfNoDatastore(t)

	ctx := context.Background()
	dp, err := New[string, int](ctx, "test-cache")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := dp.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

	// Load non-existent key
	_, _, found, err := dp.Load(ctx, "missing-key-12345")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if found {
		t.Error("missing key should not be found")
	}
}

func TestDatastorePersist_TTL(t *testing.T) {
	skipIfNoDatastore(t)

	ctx := context.Background()
	dp, err := New[string, string](ctx, "test-cache")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := dp.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

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

func TestDatastorePersist_Delete(t *testing.T) {
	skipIfNoDatastore(t)

	ctx := context.Background()
	dp, err := New[string, int](ctx, "test-cache")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := dp.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

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
	if err := dp.Delete(ctx, "missing-key-99999"); err != nil {
		t.Errorf("Delete missing key: %v", err)
	}
}

func TestDatastorePersist_Update(t *testing.T) {
	skipIfNoDatastore(t)

	ctx := context.Background()
	dp, err := New[string, string](ctx, "test-cache")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := dp.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

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

	// Cleanup
	if err := dp.Delete(ctx, "key"); err != nil {
		t.Logf("Delete error: %v", err)
	}
}

func TestDatastorePersist_ComplexValue(t *testing.T) {
	skipIfNoDatastore(t)

	type User struct {
		Name  string
		Email string
		Age   int
	}

	ctx := context.Background()
	dp, err := New[string, User](ctx, "test-cache")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := dp.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

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

	// Cleanup
	if err := dp.Delete(ctx, "user1"); err != nil {
		t.Logf("Delete error: %v", err)
	}
}

func TestNewDatastorePersist_Integration(t *testing.T) {
	ctx := context.Background()

	// Try to create with invalid project (will fail but tests the path)
	_, err := New[string, int](ctx, "test-invalid-project")
	// Error is expected - we're testing the code path
	if err == nil {
		t.Log("New succeeded unexpectedly - might have credentials")
	}
}

func TestDatastorePersist_ValidateKey(t *testing.T) {
	ctx := context.Background()

	// Create a persister (may fail without credentials, but we can still test ValidateKey on the type)
	dp, err := New[string, int](ctx, "test-cache")
	if err != nil {
		// Can't create client, but we can still test the validation logic
		t.Skip("Skipping: no datastore access")
	}
	defer dp.Close()

	tests := []struct {
		name    string
		key     string
		wantErr bool
	}{
		{"empty key", "", true},
		{"valid short key", "key123", false},
		{"valid long key", string(make([]byte, 1500)), false},
		{"key too long", string(make([]byte, 1501)), true},
		{"valid with special chars", "key:123-test", false},
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

func TestDatastorePersist_Location(t *testing.T) {
	ctx := context.Background()

	dp, err := New[string, int](ctx, "test-cache")
	if err != nil {
		t.Skip("Skipping: no datastore access")
	}
	defer dp.Close()

	loc := dp.Location("mykey")
	if loc == "" {
		t.Error("Location() should return non-empty string")
	}

	// Should contain the kind and key
	if loc != "CacheEntry/mykey" {
		t.Errorf("Location() = %q; want %q", loc, "CacheEntry/mykey")
	}
}

func TestDatastorePersist_LoadRecent(t *testing.T) {
	skipIfNoDatastore(t)

	ctx := context.Background()
	dp, err := New[string, int](ctx, "test-cache")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer dp.Close()

	// Store multiple entries
	for i := range 5 {
		key := "test-" + string(rune('a'+i))
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

	// Cleanup
	for i := range 5 {
		key := "test-" + string(rune('a'+i))
		dp.Delete(ctx, key)
	}
}

func TestDatastorePersist_Cleanup(t *testing.T) {
	skipIfNoDatastore(t)

	ctx := context.Background()
	dp, err := New[string, int](ctx, "test-cache")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer dp.Close()

	// Store entries with different expiry times
	past := time.Now().Add(-2 * time.Hour)
	future := time.Now().Add(2 * time.Hour)

	dp.Store(ctx, "expired-1", 1, past)
	dp.Store(ctx, "expired-2", 2, past)
	dp.Store(ctx, "valid-1", 3, future)
	dp.Store(ctx, "no-expiry", 4, time.Time{})

	// Cleanup entries older than 1 hour
	count, err := dp.Cleanup(ctx, 1*time.Hour)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	// Should have cleaned up 2 expired entries
	if count != 2 {
		t.Errorf("Cleanup count = %d; want 2", count)
	}

	// Cleanup remaining test entries
	dp.Delete(ctx, "valid-1")
	dp.Delete(ctx, "no-expiry")
}

func TestDatastorePersist_CleanupEmpty(t *testing.T) {
	skipIfNoDatastore(t)

	ctx := context.Background()
	dp, err := New[string, int](ctx, "test-cache")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer dp.Close()

	// Cleanup with no expired entries
	count, err := dp.Cleanup(ctx, 1*time.Hour)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	// Should find 0 entries to clean
	if count != 0 {
		t.Logf("Cleanup count = %d (found existing expired entries)", count)
	}
}
