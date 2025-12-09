package cloudrun

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestNew_LocalFallback(t *testing.T) {
	ctx := context.Background()

	// Ensure K_SERVICE is not set
	oldVal := os.Getenv("K_SERVICE")
	_ = os.Unsetenv("K_SERVICE") //nolint:errcheck // Test setup
	defer func() {
		if oldVal != "" {
			_ = os.Setenv("K_SERVICE", oldVal) //nolint:errcheck,usetesting // Test cleanup
		}
	}()

	p, err := New[string, string](ctx, "test-cache")
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer func() {
		if err := p.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

	// Verify it's using local files by checking the Location format
	loc := p.Location("test-key")
	if !strings.Contains(loc, "/") && !strings.Contains(loc, "\\") {
		t.Errorf("expected file path in location, got: %s", loc)
	}
}

func TestNew_CloudRunWithoutDatastore(t *testing.T) {
	ctx := context.Background()

	// Set K_SERVICE to simulate Cloud Run
	oldVal := os.Getenv("K_SERVICE")
	_ = os.Setenv("K_SERVICE", "test-service") //nolint:errcheck,usetesting // Test setup
	defer func() {
		if oldVal != "" {
			_ = os.Setenv("K_SERVICE", oldVal) //nolint:errcheck,usetesting // Test cleanup
		} else {
			_ = os.Unsetenv("K_SERVICE") //nolint:errcheck // Test setup
		}
	}()

	// This should try Datastore, fail (no credentials), then fall back to localfs
	p, err := New[string, string](ctx, "test-cache")
	if err != nil {
		t.Fatalf("New() should fall back to localfs even when datastore fails: %v", err)
	}
	defer func() {
		if err := p.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

	// Verify it fell back to local files
	loc := p.Location("test-key")
	if !strings.Contains(loc, "/") && !strings.Contains(loc, "\\") {
		t.Errorf("expected file path in location after fallback, got: %s", loc)
	}
}

func TestNew_BasicOperations(t *testing.T) {
	ctx := context.Background()

	p, err := New[string, int](ctx, "test-ops")
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer func() {
		if err := p.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

	// Test basic store/load cycle
	key := "answer"
	value := 42

	err = p.Set(ctx, key, value, time.Time{})
	if err != nil {
		t.Fatalf("Set() failed: %v", err)
	}

	got, _, found, err := p.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get() failed: %v", err)
	}
	if !found {
		t.Fatal("Get() should find stored value")
	}
	if got != value {
		t.Errorf("Get() = %v, want %v", got, value)
	}
}

func TestNew_DetectsCloudRun(t *testing.T) {
	ctx := context.Background()

	// Unset K_SERVICE to test non-Cloud Run path
	oldVal := os.Getenv("K_SERVICE")
	_ = os.Unsetenv("K_SERVICE") //nolint:errcheck // Test setup
	defer func() {
		if oldVal != "" {
			_ = os.Setenv("K_SERVICE", oldVal) //nolint:errcheck,usetesting // Test cleanup
		}
	}()

	p, err := New[string, string](ctx, "test-detection")
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer func() {
		if err := p.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

	// Should use local files (check location format)
	loc := p.Location("testkey")
	if !strings.Contains(loc, "/") && !strings.Contains(loc, "\\") {
		t.Errorf("expected file path location, got: %s", loc)
	}
}

func TestNew_InvalidCacheID(t *testing.T) {
	ctx := context.Background()

	// Test with invalid cache ID (contains path traversal)
	_, err := New[string, string](ctx, "../invalid")
	if err == nil {
		t.Error("New() should fail with invalid cacheID")
	}

	// Test with empty cache ID
	_, err = New[string, string](ctx, "")
	if err == nil {
		t.Error("New() should fail with empty cacheID")
	}
}

func TestNew_MultipleTypes(t *testing.T) {
	ctx := context.Background()

	// Test with different key/value types
	tests := []struct {
		name string
		fn   func() error
	}{
		{
			name: "string-int",
			fn: func() error {
				p, err := New[string, int](ctx, "test-string-int")
				if err != nil {
					return err
				}
				defer func() {
					if err := p.Close(); err != nil {
						t.Logf("Close error: %v", err)
					}
				}()
				return p.Set(ctx, "key", 42, time.Time{})
			},
		},
		{
			name: "int-string",
			fn: func() error {
				p, err := New[int, string](ctx, "test-int-string")
				if err != nil {
					return err
				}
				defer func() {
					if err := p.Close(); err != nil {
						t.Logf("Close error: %v", err)
					}
				}()
				return p.Set(ctx, 123, "value", time.Time{})
			},
		},
		{
			name: "string-struct",
			fn: func() error {
				type TestStruct struct {
					Name string
					Age  int
				}
				p, err := New[string, TestStruct](ctx, "test-string-struct")
				if err != nil {
					return err
				}
				defer func() {
					if err := p.Close(); err != nil {
						t.Logf("Close error: %v", err)
					}
				}()
				return p.Set(ctx, "user", TestStruct{Name: "Alice", Age: 30}, time.Time{})
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.fn(); err != nil {
				t.Errorf("test %s failed: %v", tt.name, err)
			}
		})
	}
}

func TestNew_CloudRunFallbackWithDelete(t *testing.T) {
	ctx := context.Background()

	// Set K_SERVICE to simulate Cloud Run
	oldVal := os.Getenv("K_SERVICE")
	_ = os.Setenv("K_SERVICE", "test-service-delete") //nolint:errcheck,usetesting // Test setup
	defer func() {
		if oldVal != "" {
			_ = os.Setenv("K_SERVICE", oldVal) //nolint:errcheck,usetesting // Test cleanup
		} else {
			_ = os.Unsetenv("K_SERVICE") //nolint:errcheck // Test setup
		}
	}()

	p, err := New[string, int](ctx, "test-delete")
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer func() {
		if err := p.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

	// Store and delete
	if err := p.Set(ctx, "key1", 100, time.Time{}); err != nil {
		t.Fatalf("Set() failed: %v", err)
	}

	if err := p.Delete(ctx, "key1"); err != nil {
		t.Fatalf("Delete() failed: %v", err)
	}

	// Verify deleted
	_, _, found, err := p.Get(ctx, "key1")
	if err != nil {
		t.Fatalf("Get() failed: %v", err)
	}
	if found {
		t.Error("key should be deleted")
	}
}

func TestNew_ValidateKey(t *testing.T) {
	ctx := context.Background()

	p, err := New[string, int](ctx, "test-validate")
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer func() {
		if err := p.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

	// Test valid key
	if err := p.ValidateKey("valid-key"); err != nil {
		t.Errorf("ValidateKey() should accept valid key: %v", err)
	}

	// Test invalid character
	if err := p.ValidateKey("key/with/slash"); err == nil {
		t.Error("ValidateKey() should reject key with slash")
	}

	// Test very long key (> 127 chars for localfs)
	longKey := string(make([]byte, 200))
	for i := range longKey {
		longKey = longKey[:i] + "a" + longKey[i+1:]
	}
	if err := p.ValidateKey(longKey); err == nil {
		t.Error("ValidateKey() should reject very long key")
	}
}

func TestNew_Cleanup(t *testing.T) {
	ctx := context.Background()

	p, err := New[string, int](ctx, "test-cleanup")
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer func() {
		if err := p.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

	// Cleanup should work without errors
	count, err := p.Cleanup(ctx, 1*time.Hour)
	if err != nil {
		t.Errorf("Cleanup() failed: %v", err)
	}
	t.Logf("Cleanup() removed %d entries", count)
}
