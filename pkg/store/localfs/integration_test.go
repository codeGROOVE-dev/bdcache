package localfs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFilePersist_StoreLoad(t *testing.T) {
	dir := t.TempDir()
	fp, err := New[string, int](filepath.Base(dir), filepath.Dir(dir))
	if err != nil {
		t.Fatalf("newFilePersist: %v", err)
	}
	defer func() {
		if err := fp.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

	// Override directory to use temp dir

	ctx := context.Background()

	// Set a value
	if err := fp.Set(ctx, "key1", 42, time.Time{}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Get the value
	val, expiry, found, err := fp.Get(ctx, "key1")
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
		t.Error("expiry should be zero")
	}
}

func TestFilePersist_LoadMissing(t *testing.T) {
	dir := t.TempDir()
	fp, err := New[string, int](filepath.Base(dir), filepath.Dir(dir))
	if err != nil {
		t.Fatalf("newFilePersist: %v", err)
	}
	defer func() {
		if err := fp.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

	ctx := context.Background()

	// Get non-existent key
	_, _, found, err := fp.Get(ctx, "missing")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if found {
		t.Error("missing key should not be found")
	}
}

func TestFilePersist_TTL(t *testing.T) {
	dir := t.TempDir()
	fp, err := New[string, string](filepath.Base(dir), filepath.Dir(dir))
	if err != nil {
		t.Fatalf("newFilePersist: %v", err)
	}
	defer func() {
		if err := fp.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

	ctx := context.Background()

	// Set with past expiry
	past := time.Now().Add(-1 * time.Second)
	if err := fp.Set(ctx, "expired", "value", past); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Should not be gettable
	_, _, found, err := fp.Get(ctx, "expired")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if found {
		t.Error("expired key should not be found")
	}

	// File should be removed (subdirectory may remain, but should be empty or only have empty subdirs)
	filename := fp.Location("expired")
	if _, err := os.Stat(filename); !os.IsNotExist(err) {
		t.Error("expired file should be removed")
	}
}

func TestFilePersist_Delete(t *testing.T) {
	dir := t.TempDir()
	fp, err := New[string, int](filepath.Base(dir), filepath.Dir(dir))
	if err != nil {
		t.Fatalf("newFilePersist: %v", err)
	}
	defer func() {
		if err := fp.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

	ctx := context.Background()

	// Set and delete
	if err := fp.Set(ctx, "key1", 42, time.Time{}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	if err := fp.Delete(ctx, "key1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Should not be gettable
	_, _, found, err := fp.Get(ctx, "key1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if found {
		t.Error("deleted key should not be found")
	}

	// Deleting non-existent key should not error
	if err := fp.Delete(ctx, "missing"); err != nil {
		t.Errorf("Delete missing key: %v", err)
	}
}

func TestFilePersist_Update(t *testing.T) {
	dir := t.TempDir()
	fp, err := New[string, string](filepath.Base(dir), filepath.Dir(dir))
	if err != nil {
		t.Fatalf("newFilePersist: %v", err)
	}
	defer func() {
		if err := fp.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

	ctx := context.Background()

	// Set initial value
	if err := fp.Set(ctx, "key", "value1", time.Time{}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Update value
	if err := fp.Set(ctx, "key", "value2", time.Time{}); err != nil {
		t.Fatalf("Set update: %v", err)
	}

	// Get and verify updated value
	val, _, found, err := fp.Get(ctx, "key")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found {
		t.Fatal("key not found")
	}
	if val != "value2" {
		t.Errorf("Get value = %s; want value2", val)
	}
}

func TestFilePersist_Store_CompleteFlow(t *testing.T) {
	dir := t.TempDir()
	fp, err := New[string, string](filepath.Base(dir), filepath.Dir(dir))
	if err != nil {
		t.Fatalf("newFilePersist: %v", err)
	}
	defer func() {
		if err := fp.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

	ctx := context.Background()

	// Test complete store flow with expiry
	expiry := time.Now().Add(1 * time.Hour)
	if err := fp.Set(ctx, "key1", "value1", expiry); err != nil {
		t.Fatalf("Set with expiry: %v", err)
	}

	// Load and verify expiry is set
	val, loadedExpiry, found, err := fp.Get(ctx, "key1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found {
		t.Fatal("key1 not found")
	}
	if val != "value1" {
		t.Errorf("value = %s; want value1", val)
	}

	// Verify expiry was stored correctly (within 1 second)
	if loadedExpiry.Sub(expiry).Abs() > time.Second {
		t.Errorf("expiry = %v; want ~%v", loadedExpiry, expiry)
	}
}

func TestFilePersist_New_Errors(t *testing.T) {
	tests := []struct {
		name    string
		cacheID string
		wantErr bool
	}{
		{"empty cacheID", "", true},
		{"path traversal ..", "../foo", true},
		{"path traversal with slash", "foo/bar", true},
		{"path traversal backslash", "foo\\bar", true},
		{"null byte", "foo\x00bar", true},
		{"valid alphanumeric", "myapp123", false},
		{"valid with dash", "my-app", false},
		{"valid with underscore", "my_app", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			_, err := New[string, int](tt.cacheID, dir)
			if (err != nil) != tt.wantErr {
				t.Errorf("New() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestFilePersist_ValidateKey(t *testing.T) {
	dir := t.TempDir()
	fp, err := New[string, int]("test", dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := fp.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

	// Create a valid 127-character key
	validMaxKey := make([]byte, 127)
	for i := range validMaxKey {
		validMaxKey[i] = 'a'
	}

	tests := []struct {
		name    string
		key     string
		wantErr bool
	}{
		{"valid short key", "key123", false},
		{"valid with dash", "key-123", false},
		{"valid with underscore", "key_123", false},
		{"valid with period", "key.123", false},
		{"valid with colon", "key:123", false},
		{"key at max length", string(validMaxKey), false},
		{"key too long", string(make([]byte, 128)), true},
		{"key with space", "my key", true},
		{"key with unicode", "key-日本語", true}, //nolint:gosmopolitan // Testing unicode handling
		{"key with slash", "key/123", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := fp.ValidateKey(tt.key)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateKey() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestFilePersist_Cleanup(t *testing.T) {
	dir := t.TempDir()
	fp, err := New[string, int]("test", dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := fp.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

	ctx := context.Background()

	// Set items with different expiry times
	// Cleanup deletes entries where Expiry < (now - maxAge)
	past := time.Now().Add(-2 * time.Hour)           // Will be cleaned up (< now - 1h)
	recentPast := time.Now().Add(-90 * time.Minute)  // Just outside 1h window, should be cleaned
	recentFuture := time.Now().Add(30 * time.Minute) // Future expiry, should stay
	future := time.Now().Add(2 * time.Hour)          // Far future, should stay

	if err := fp.Set(ctx, "expired-old", 1, past); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := fp.Set(ctx, "expired-recent", 2, recentPast); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := fp.Set(ctx, "valid-soon", 3, recentFuture); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := fp.Set(ctx, "valid-future", 4, future); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := fp.Set(ctx, "no-expiry", 5, time.Time{}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Cleanup items with expiry older than 1 hour
	count, err := fp.Cleanup(ctx, 1*time.Hour)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	// Should have cleaned up 2 entries (expired-old and expired-recent)
	if count != 2 {
		t.Errorf("Cleanup count = %d; want 2", count)
	}

	// Verify expired entries are gone (they're deleted from disk)
	// Note: Load won't find them even without cleanup since they're expired

	// Verify valid items still exist
	_, _, found, err := fp.Get(ctx, "valid-soon")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found {
		t.Error("valid-soon should still exist")
	}

	_, _, found, err = fp.Get(ctx, "valid-future")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found {
		t.Error("valid-future should still exist")
	}

	_, _, found, err = fp.Get(ctx, "no-expiry")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found {
		t.Error("no-expiry should still exist")
	}
}

func TestFilePersist_Location(t *testing.T) {
	dir := t.TempDir()
	fp, err := New[string, int]("test", dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := fp.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

	loc := fp.Location("mykey")
	if loc == "" {
		t.Error("Location() should return non-empty string")
	}

	// Should contain the cache directory
	if !filepath.IsAbs(loc) {
		t.Error("Location() should return absolute path")
	}
}

func TestFilePersist_LoadErrors(t *testing.T) {
	dir := t.TempDir()
	fp, err := New[string, int]("test", dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := fp.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

	ctx := context.Background()

	// Set a value
	if err := fp.Set(ctx, "test", 42, time.Time{}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Corrupt the file by writing invalid data
	loc := fp.Location("test")
	if err := os.WriteFile(loc, []byte("invalid gob data"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Get should handle corrupt file gracefully
	_, _, found, err := fp.Get(ctx, "test")
	if found {
		t.Error("Load should not find corrupted entry")
	}
	// Error is acceptable for corrupted data
	_ = err
}

func TestFilePersist_StoreCreateDir(t *testing.T) {
	dir := t.TempDir()
	subdir := filepath.Join(dir, "subdir")

	// Create store with non-existent subdir
	fp, err := New[string, int]("testcache", subdir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := fp.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

	ctx := context.Background()

	// Set should create directories as needed
	if err := fp.Set(ctx, "key1", 42, time.Time{}); err != nil {
		t.Fatalf("Store should create directories: %v", err)
	}

	// Verify file was created
	val, _, found, err := fp.Get(ctx, "key1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found || val != 42 {
		t.Error("stored value should be retrievable")
	}
}

func TestFilePersist_CleanupEmptyDir(t *testing.T) {
	dir := t.TempDir()
	fp, err := New[string, int]("test", dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := fp.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

	ctx := context.Background()

	// Cleanup on empty directory should work
	count, err := fp.Cleanup(ctx, 1*time.Hour)
	if err != nil {
		t.Fatalf("Cleanup on empty dir: %v", err)
	}
	if count != 0 {
		t.Errorf("Cleanup count = %d; want 0 for empty dir", count)
	}
}

func TestFilePersist_KeyToFilename_Short(t *testing.T) {
	dir := t.TempDir()
	fp, err := New[string, int]("test", dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := fp.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

	// Test with very short key (less than 2 characters for subdirectory)
	ctx := context.Background()
	if err := fp.Set(ctx, "a", 1, time.Time{}); err != nil {
		t.Fatalf("Set short key: %v", err)
	}

	val, _, found, err := fp.Get(ctx, "a")
	if err != nil {
		t.Fatalf("Get short key: %v", err)
	}
	if !found || val != 1 {
		t.Error("short key should be stored and retrieved")
	}
}

func TestFilePersist_Delete_NonExistent(t *testing.T) {
	dir := t.TempDir()
	fp, err := New[string, int]("test", dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := fp.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

	ctx := context.Background()

	// Delete a non-existent key should not error
	if err := fp.Delete(ctx, "does-not-exist"); err != nil {
		t.Errorf("Delete non-existent key should not error: %v", err)
	}
}

func TestFilePersist_New_UseDefaultCacheDir(t *testing.T) {
	// Test creating store without providing dir (uses OS cache dir)
	fp, err := New[string, int]("test-default-dir", "")
	if err != nil {
		t.Fatalf("New with default dir: %v", err)
	}
	defer func() {
		if err := fp.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
		// Clean up the test directory from OS cache dir
		_ = os.RemoveAll(fp.Dir) //nolint:errcheck // Test cleanup
	}()

	ctx := context.Background()

	// Should be able to set and get
	if err := fp.Set(ctx, "key1", 42, time.Time{}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	val, _, found, err := fp.Get(ctx, "key1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found || val != 42 {
		t.Error("should be able to use default cache dir")
	}
}

func TestFilePersist_Store_WithExpiry(t *testing.T) {
	dir := t.TempDir()
	fp, err := New[string, int]("test", dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := fp.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

	ctx := context.Background()

	// Set with future expiry
	expiry := time.Now().Add(1 * time.Hour)
	if err := fp.Set(ctx, "key1", 42, expiry); err != nil {
		t.Fatalf("Set with expiry: %v", err)
	}

	// Get and check expiry is preserved
	val, loadedExpiry, found, err := fp.Get(ctx, "key1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found {
		t.Error("key1 should be found")
	}
	if val != 42 {
		t.Errorf("val = %d; want 42", val)
	}

	// Expiry should be within 1 second of what we set
	if loadedExpiry.Sub(expiry).Abs() > time.Second {
		t.Errorf("expiry = %v; want ~%v", loadedExpiry, expiry)
	}
}

func TestFilePersist_Cleanup_ContextCancellation(t *testing.T) {
	dir := t.TempDir()
	fp, err := New[string, int]("test", dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := fp.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

	// Set many expired entries
	past := time.Now().Add(-2 * time.Hour)
	for i := range 100 {
		if err := fp.Set(context.Background(), fmt.Sprintf("expired-%d", i), i, past); err != nil {
			t.Fatalf("Set: %v", err)
		}
	}

	// Create context that we'll cancel
	ctx, cancel := context.WithCancel(context.Background())

	// Cancel immediately
	cancel()

	// Try to cleanup
	_, err = fp.Cleanup(ctx, 1*time.Hour)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled error, got: %v", err)
	}
}

func TestFilePersist_Flush(t *testing.T) {
	dir := t.TempDir()
	fp, err := New[string, int]("test", dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := fp.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

	ctx := context.Background()

	// Set multiple entries
	for i := range 10 {
		if err := fp.Set(ctx, fmt.Sprintf("key-%d", i), i*100, time.Time{}); err != nil {
			t.Fatalf("Set: %v", err)
		}
	}

	// Verify files exist
	for i := range 10 {
		if _, _, found, err := fp.Get(ctx, fmt.Sprintf("key-%d", i)); err != nil || !found {
			t.Fatalf("key-%d should exist before flush", i)
		}
	}

	// Flush
	deleted, err := fp.Flush(ctx)
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if deleted != 10 {
		t.Errorf("Flush deleted %d entries; want 10", deleted)
	}

	// All entries should be gone
	for i := range 10 {
		if _, _, found, err := fp.Get(ctx, fmt.Sprintf("key-%d", i)); err != nil {
			t.Fatalf("Get: %v", err)
		} else if found {
			t.Errorf("key-%d should not exist after flush", i)
		}
	}
}

func TestFilePersist_Flush_Empty(t *testing.T) {
	dir := t.TempDir()
	fp, err := New[string, int]("test", dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := fp.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

	// Flush empty cache
	deleted, err := fp.Flush(context.Background())
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if deleted != 0 {
		t.Errorf("Flush deleted %d entries; want 0", deleted)
	}
}

func TestFilePersist_Flush_RemovesFiles(t *testing.T) {
	dir := t.TempDir()
	fp, err := New[string, int]("test", dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := fp.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

	ctx := context.Background()
	cacheDir := fp.Dir

	// Set multiple entries
	for i := range 10 {
		if err := fp.Set(ctx, fmt.Sprintf("key-%d", i), i*100, time.Time{}); err != nil {
			t.Fatalf("Set: %v", err)
		}
	}

	// Count .gob files on disk before flush
	countGobFiles := func() int {
		count := 0
		//nolint:errcheck // WalkDir errors are handled by returning nil to continue walking
		_ = filepath.WalkDir(cacheDir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil //nolint:nilerr // Intentionally continue walking on errors
			}
			if !d.IsDir() && filepath.Ext(path) == ".gob" {
				count++
			}
			return nil
		})
		return count
	}

	beforeFlush := countGobFiles()
	if beforeFlush != 10 {
		t.Errorf("expected 10 .gob files before flush, got %d", beforeFlush)
	}

	// Flush
	deleted, err := fp.Flush(ctx)
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if deleted != 10 {
		t.Errorf("Flush deleted %d entries; want 10", deleted)
	}

	// Verify no .gob files remain
	afterFlush := countGobFiles()
	if afterFlush != 0 {
		t.Errorf("expected 0 .gob files after flush, got %d", afterFlush)
	}
}

func TestFilePersist_Flush_ContextCancellation(t *testing.T) {
	dir := t.TempDir()
	fp, err := New[string, int]("test", dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := fp.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

	// Set many entries
	for i := range 100 {
		if err := fp.Set(context.Background(), fmt.Sprintf("key-%d", i), i, time.Time{}); err != nil {
			t.Fatalf("Set: %v", err)
		}
	}

	// Create context that we'll cancel
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Try to flush
	_, err = fp.Flush(ctx)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled error, got: %v", err)
	}
}

func TestFilePersist_Len(t *testing.T) {
	dir := t.TempDir()
	fp, err := New[string, int]("test", dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := fp.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

	ctx := context.Background()

	// Empty cache should have length 0
	n, err := fp.Len(ctx)
	if err != nil {
		t.Fatalf("Len: %v", err)
	}
	if n != 0 {
		t.Errorf("Len() = %d; want 0 for empty cache", n)
	}

	// Set entries
	for i := range 10 {
		if err := fp.Set(ctx, fmt.Sprintf("key-%d", i), i*100, time.Time{}); err != nil {
			t.Fatalf("Set: %v", err)
		}
	}

	// Should have 10 entries
	n, err = fp.Len(ctx)
	if err != nil {
		t.Fatalf("Len: %v", err)
	}
	if n != 10 {
		t.Errorf("Len() = %d; want 10", n)
	}

	// Delete some entries
	for i := range 3 {
		if err := fp.Delete(ctx, fmt.Sprintf("key-%d", i)); err != nil {
			t.Fatalf("Delete: %v", err)
		}
	}

	// Should have 7 entries
	n, err = fp.Len(ctx)
	if err != nil {
		t.Fatalf("Len: %v", err)
	}
	if n != 7 {
		t.Errorf("Len() = %d; want 7", n)
	}

	// Flush and verify 0
	_, err = fp.Flush(ctx)
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}

	n, err = fp.Len(ctx)
	if err != nil {
		t.Fatalf("Len: %v", err)
	}
	if n != 0 {
		t.Errorf("Len() = %d; want 0 after Flush", n)
	}
}

func TestFilePersist_Len_NewStoreSameDir(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	// Create first store and set entries
	fp1, err := New[string, int]("test", dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for i := range 10 {
		if err := fp1.Set(ctx, fmt.Sprintf("key-%d", i), i*100, time.Time{}); err != nil {
			t.Fatalf("Set: %v", err)
		}
	}

	// Close first store
	if err := fp1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Create second store pointing to same directory
	fp2, err := New[string, int]("test", dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := fp2.Close(); err != nil {
			t.Logf("Close error: %v", err)
		}
	}()

	// Len should return 10 from disk (entries from previous session)
	n, err := fp2.Len(ctx)
	if err != nil {
		t.Fatalf("Len: %v", err)
	}
	if n != 10 {
		t.Errorf("Len() = %d; want 10 (entries from previous session)", n)
	}

	// Flush should clear all entries from disk
	deleted, err := fp2.Flush(ctx)
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if deleted != 10 {
		t.Errorf("Flush deleted %d entries; want 10", deleted)
	}

	// Len should now be 0
	n, err = fp2.Len(ctx)
	if err != nil {
		t.Fatalf("Len: %v", err)
	}
	if n != 0 {
		t.Errorf("Len() = %d; want 0 after Flush", n)
	}
}
