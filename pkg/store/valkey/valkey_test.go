package valkey

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// skipIfNoValkey skips the test if Valkey is not available.
func skipIfNoValkey(t *testing.T) {
	t.Helper()
	addr := os.Getenv("VALKEY_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Try to create a test connection
	p, err := New[string, int](ctx, "test-skip", addr)
	if err != nil {
		t.Skipf("Skipping valkey tests: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Logf("Close error: %v", err)
	}
}

// Unit tests that don't require Valkey connection

func TestValkey_New_InvalidCacheID(t *testing.T) {
	ctx := context.Background()

	// Empty cacheID should fail
	_, err := New[string, int](ctx, "", "localhost:6379")
	if err == nil {
		t.Error("New() should fail with empty cacheID")
	}
}

func TestValkey_ValidateKey(t *testing.T) {
	skipIfNoValkey(t)

	ctx := context.Background()
	p, err := New[string, int](ctx, "test-validate", "localhost:6379")
	if err != nil {
		t.Skip("Valkey not available")
	}
	defer func() {
		if err := p.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

	tests := []struct {
		name    string
		key     string
		wantErr bool
	}{
		{"empty key", "", true},
		{"valid key", "key123", false},
		{"valid long key", string(make([]byte, 512)), false},
		{"key too long", string(make([]byte, 513)), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := p.ValidateKey(tt.key)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateKey() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestValkey_Location(t *testing.T) {
	skipIfNoValkey(t)

	ctx := context.Background()
	p, err := New[string, int](ctx, "testapp", "localhost:6379")
	if err != nil {
		t.Skip("Valkey not available")
	}
	defer func() {
		if err := p.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

	loc := p.Location("mykey")
	expected := "testapp:mykey"
	if loc != expected {
		t.Errorf("Location() = %q; want %q", loc, expected)
	}
}

func TestValkey_Cleanup(t *testing.T) {
	skipIfNoValkey(t)

	ctx := context.Background()
	p, err := New[string, int](ctx, "test-cleanup", "localhost:6379")
	if err != nil {
		t.Skip("Valkey not available")
	}
	defer func() {
		if err := p.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

	// Cleanup always returns 0 since Valkey handles TTL automatically
	count, err := p.Cleanup(ctx, time.Hour)
	if err != nil {
		t.Errorf("Cleanup() error = %v", err)
	}
	if count != 0 {
		t.Errorf("Cleanup() count = %d; want 0 (Valkey handles TTL automatically)", count)
	}
}

func TestValkeyPersist_StoreLoad(t *testing.T) {
	skipIfNoValkey(t)

	ctx := context.Background()
	addr := os.Getenv("VALKEY_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}

	p, err := New[string, int](ctx, "test-cache-store", addr)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := p.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

	// Set a value
	if err := p.Set(ctx, "key1", 42, time.Time{}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Get the value
	val, expiry, found, err := p.Get(ctx, "key1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found {
		t.Fatal("key1 not found")
	}
	if val != 42 {
		t.Errorf("Get value = %d; want 42", val)
	}
	if !expiry.IsZero() {
		t.Error("expiry should be zero for no TTL")
	}

	// Cleanup
	if err := p.Delete(ctx, "key1"); err != nil {
		t.Logf("Delete error: %v", err)
	}
}

func TestValkeyPersist_LoadMissing(t *testing.T) {
	skipIfNoValkey(t)

	ctx := context.Background()
	addr := os.Getenv("VALKEY_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}

	p, err := New[string, int](ctx, "test-cache-missing", addr)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := p.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

	// Get non-existent key
	_, _, found, err := p.Get(ctx, "missing-key-99999")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if found {
		t.Error("missing key should not be found")
	}
}

func TestValkeyPersist_TTL(t *testing.T) {
	skipIfNoValkey(t)

	ctx := context.Background()
	addr := os.Getenv("VALKEY_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}

	p, err := New[string, string](ctx, "test-cache-ttl", addr)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := p.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

	// Set with short TTL
	expiry := time.Now().Add(1 * time.Second)
	if err := p.Set(ctx, "expires-soon", "value", expiry); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Should be gettable immediately
	val, loadedExpiry, found, err := p.Get(ctx, "expires-soon")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found {
		t.Fatal("key not found")
	}
	if val != "value" {
		t.Errorf("value = %s; want value", val)
	}
	if loadedExpiry.IsZero() {
		t.Error("expiry should not be zero")
	}

	// Wait for expiration
	time.Sleep(2 * time.Second)

	// Should not be gettable after expiration
	_, _, found, err = p.Get(ctx, "expires-soon")
	if err != nil {
		t.Fatalf("Get after expiry: %v", err)
	}
	if found {
		t.Error("expired key should not be found")
	}
}

func TestValkeyPersist_Delete(t *testing.T) {
	skipIfNoValkey(t)

	ctx := context.Background()
	addr := os.Getenv("VALKEY_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}

	p, err := New[string, int](ctx, "test-cache-delete", addr)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := p.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

	// Set and delete
	if err := p.Set(ctx, "key1", 42, time.Time{}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	if err := p.Delete(ctx, "key1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Should not be gettable
	_, _, found, err := p.Get(ctx, "key1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if found {
		t.Error("deleted key should not be found")
	}

	// Deleting non-existent key should not error
	if err := p.Delete(ctx, "missing-key-88888"); err != nil {
		t.Errorf("Delete missing key: %v", err)
	}
}

func TestValkeyPersist_Update(t *testing.T) {
	skipIfNoValkey(t)

	ctx := context.Background()
	addr := os.Getenv("VALKEY_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}

	p, err := New[string, string](ctx, "test-cache-update", addr)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := p.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

	// Set initial value
	if err := p.Set(ctx, "key", "value1", time.Time{}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Update value
	if err := p.Set(ctx, "key", "value2", time.Time{}); err != nil {
		t.Fatalf("Set update: %v", err)
	}

	// Get and verify updated value
	val, _, found, err := p.Get(ctx, "key")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found {
		t.Fatal("key not found")
	}
	if val != "value2" {
		t.Errorf("Get value = %s; want value2", val)
	}

	// Cleanup
	if err := p.Delete(ctx, "key"); err != nil {
		t.Logf("Delete error: %v", err)
	}
}

func TestValkeyPersist_ComplexValue(t *testing.T) {
	skipIfNoValkey(t)

	type User struct {
		Name  string
		Email string
		Age   int
	}

	ctx := context.Background()
	addr := os.Getenv("VALKEY_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}

	p, err := New[string, User](ctx, "test-cache-complex", addr)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := p.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

	user := User{
		Name:  "Alice",
		Email: "alice@example.com",
		Age:   30,
	}

	// Set complex value
	if err := p.Set(ctx, "user1", user, time.Time{}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Get and verify
	loaded, _, found, err := p.Get(ctx, "user1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found {
		t.Fatal("user1 not found")
	}
	if loaded.Name != user.Name || loaded.Email != user.Email || loaded.Age != user.Age {
		t.Errorf("Get value = %+v; want %+v", loaded, user)
	}

	// Cleanup
	if err := p.Delete(ctx, "user1"); err != nil {
		t.Logf("Delete error: %v", err)
	}
}

func TestValkeyPersist_Location(t *testing.T) {
	skipIfNoValkey(t)

	ctx := context.Background()
	addr := os.Getenv("VALKEY_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}

	p, err := New[string, int](ctx, "test-cache-loc", addr)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := p.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

	location := p.Location("mykey")
	expected := "test-cache-loc:mykey"
	if location != expected {
		t.Errorf("Location = %s; want %s", location, expected)
	}
}

func TestValkeyPersist_ConcurrentAccess(t *testing.T) {
	skipIfNoValkey(t)

	ctx := context.Background()
	addr := os.Getenv("VALKEY_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}

	p, err := New[string, int](ctx, "test-cache-concurrent", addr)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := p.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

	// Test concurrent writes
	const numGoroutines = 50
	const numOpsPerGoroutine = 20

	errCh := make(chan error, numGoroutines*numOpsPerGoroutine)

	// Concurrent writes
	for i := range numGoroutines {
		go func(id int) {
			for j := range numOpsPerGoroutine {
				key := fmt.Sprintf("key-%d-%d", id, j)
				value := id*1000 + j
				if err := p.Set(ctx, key, value, time.Time{}); err != nil {
					errCh <- fmt.Errorf("store %s: %w", key, err)
					return
				}
			}
		}(i)
	}

	// Wait a bit for writes to complete
	time.Sleep(2 * time.Second)

	// Check for errors
	close(errCh)
	for err := range errCh {
		t.Errorf("concurrent write error: %v", err)
	}

	// Concurrent reads
	readErrCh := make(chan error, numGoroutines*numOpsPerGoroutine)
	for i := range numGoroutines {
		go func(id int) {
			for j := range numOpsPerGoroutine {
				key := fmt.Sprintf("key-%d-%d", id, j)
				expectedValue := id*1000 + j
				val, _, found, err := p.Get(ctx, key)
				if err != nil {
					readErrCh <- fmt.Errorf("load %s: %w", key, err)
					return
				}
				if !found {
					readErrCh <- fmt.Errorf("key %s not found", key)
					return
				}
				if val != expectedValue {
					readErrCh <- fmt.Errorf("key %s: got %d, want %d", key, val, expectedValue)
					return
				}
			}
		}(i)
	}

	// Wait for reads
	time.Sleep(2 * time.Second)

	// Check for read errors
	close(readErrCh)
	for err := range readErrCh {
		t.Errorf("concurrent read error: %v", err)
	}

	// Cleanup
	for i := range numGoroutines {
		for j := range numOpsPerGoroutine {
			key := fmt.Sprintf("key-%d-%d", i, j)
			if err := p.Delete(ctx, key); err != nil {
				t.Logf("cleanup delete error: %v", err)
			}
		}
	}
}

func TestValkeyPersist_LargeValue(t *testing.T) {
	skipIfNoValkey(t)

	ctx := context.Background()
	addr := os.Getenv("VALKEY_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}

	p, err := New[string, []byte](ctx, "test-cache-large", addr)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := p.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

	// Test with 1MB value
	largeValue := make([]byte, 1024*1024)
	for i := range largeValue {
		largeValue[i] = byte(i % 256)
	}

	if err := p.Set(ctx, "large-key", largeValue, time.Time{}); err != nil {
		t.Fatalf("Set large value: %v", err)
	}

	loaded, _, found, err := p.Get(ctx, "large-key")
	if err != nil {
		t.Fatalf("Get large value: %v", err)
	}
	if !found {
		t.Fatal("large value not found")
	}
	if len(loaded) != len(largeValue) {
		t.Errorf("loaded size = %d; want %d", len(loaded), len(largeValue))
	}

	// Verify content
	for i := range loaded {
		if loaded[i] != largeValue[i] {
			t.Errorf("byte %d: got %d, want %d", i, loaded[i], largeValue[i])
			break
		}
	}

	// Cleanup
	if err := p.Delete(ctx, "large-key"); err != nil {
		t.Logf("Delete error: %v", err)
	}
}

func TestValkeyPersist_SpecialCharacters(t *testing.T) {
	skipIfNoValkey(t)

	ctx := context.Background()
	addr := os.Getenv("VALKEY_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}

	p, err := New[string, string](ctx, "test-cache-special", addr)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := p.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

	// Test keys and values with special characters
	tests := []struct {
		key   string
		value string
	}{
		{"key-with-dashes", "value-with-dashes"},
		{"key_with_underscores", "value_with_underscores"},
		{"key.with.dots", "value.with.dots"},
		{"key:with:colons", "value:with:colons"},
		{"simple", "value with spaces and special chars: !@#$%^&*()"},
		{"unicode-key", "Unicode value: \u4f60\u597d\u4e16\u754c \U0001f680"},
	}

	for _, tt := range tests {
		// Store
		if err := p.Set(ctx, tt.key, tt.value, time.Time{}); err != nil {
			t.Errorf("Set key=%s: %v", tt.key, err)
			continue
		}

		// Get and verify
		val, _, found, err := p.Get(ctx, tt.key)
		if err != nil {
			t.Errorf("Get key=%s: %v", tt.key, err)
			continue
		}
		if !found {
			t.Errorf("key %s not found", tt.key)
			continue
		}
		if val != tt.value {
			t.Errorf("key %s: got %q, want %q", tt.key, val, tt.value)
		}

		// Cleanup
		if err := p.Delete(ctx, tt.key); err != nil {
			t.Logf("Delete error for key=%s: %v", tt.key, err)
		}
	}
}

func TestValkeyPersist_EmptyValues(t *testing.T) {
	skipIfNoValkey(t)

	ctx := context.Background()
	addr := os.Getenv("VALKEY_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}

	p, err := New[string, string](ctx, "test-cache-empty", addr)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := p.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

	// Set empty string
	if err := p.Set(ctx, "empty-key", "", time.Time{}); err != nil {
		t.Fatalf("Set empty value: %v", err)
	}

	val, _, found, err := p.Get(ctx, "empty-key")
	if err != nil {
		t.Fatalf("Get empty value: %v", err)
	}
	if !found {
		t.Fatal("empty value not found")
	}
	if val != "" {
		t.Errorf("got %q, want empty string", val)
	}

	// Cleanup
	if err := p.Delete(ctx, "empty-key"); err != nil {
		t.Logf("Delete error: %v", err)
	}
}

func TestValkeyPersist_InvalidConnection(t *testing.T) {
	ctx := context.Background()

	// Try to connect to invalid address
	_, err := New[string, int](ctx, "test-invalid", "invalid-host:99999")
	if err == nil {
		t.Error("expected error for invalid address, got nil")
	}
}

func TestValkeyPersist_KeyValidation(t *testing.T) {
	skipIfNoValkey(t)

	ctx := context.Background()
	addr := os.Getenv("VALKEY_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}

	p, err := New[string, int](ctx, "test-cache-validation", addr)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := p.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

	// Test very long key
	longKey := string(make([]byte, 1000))
	for i := range []byte(longKey) {
		longKey = longKey[:i] + "a" + longKey[i+1:]
	}

	err = p.ValidateKey(longKey)
	if err == nil {
		t.Error("expected error for key longer than 512 bytes")
	}

	// Test empty key
	err = p.ValidateKey("")
	if err == nil {
		t.Error("expected error for empty key")
	}
}

func TestValkeyPersist_Flush(t *testing.T) {
	skipIfNoValkey(t)

	ctx := context.Background()
	addr := os.Getenv("VALKEY_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}

	p, err := New[string, int](ctx, "test-cache-flush", addr)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := p.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

	// Set 10 entries
	for i := range 10 {
		key := fmt.Sprintf("flush-key-%d", i)
		if err := p.Set(ctx, key, i*100, time.Time{}); err != nil {
			t.Fatalf("Set %s: %v", key, err)
		}
	}

	// Verify entries exist
	for i := range 10 {
		key := fmt.Sprintf("flush-key-%d", i)
		if _, _, found, err := p.Get(ctx, key); err != nil || !found {
			t.Fatalf("%s should exist before flush", key)
		}
	}

	// Flush
	deleted, err := p.Flush(ctx)
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if deleted != 10 {
		t.Errorf("Flush deleted %d entries; want 10", deleted)
	}

	// All entries should be gone
	for i := range 10 {
		key := fmt.Sprintf("flush-key-%d", i)
		if _, _, found, err := p.Get(ctx, key); err != nil {
			t.Fatalf("Get: %v", err)
		} else if found {
			t.Errorf("%s should not exist after flush", key)
		}
	}
}

func TestValkeyPersist_FlushEmpty(t *testing.T) {
	skipIfNoValkey(t)

	ctx := context.Background()
	addr := os.Getenv("VALKEY_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}

	p, err := New[string, int](ctx, "test-cache-flush-empty", addr)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := p.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

	// Flush empty cache
	deleted, err := p.Flush(ctx)
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if deleted != 0 {
		t.Errorf("Flush deleted %d entries from empty cache; want 0", deleted)
	}
}
