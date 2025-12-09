// Package localfs provides local filesystem persistence for sfcache.
package localfs

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Entry represents a cache entry with its metadata for serialization.
type Entry[K comparable, V any] struct {
	Key       K
	Value     V
	Expiry    time.Time
	UpdatedAt time.Time
}

const maxKeyLength = 127 // Maximum key length to avoid filesystem constraints

var (
	// Pool for bufio.Writer to reduce allocations.
	writerPool = sync.Pool{
		New: func() any {
			return bufio.NewWriterSize(nil, 4096)
		},
	}
	// Pool for bufio.Reader to reduce allocations.
	readerPool = sync.Pool{
		New: func() any {
			return bufio.NewReaderSize(nil, 4096)
		},
	}
)

// Store implements file-based persistence using local files with gob encoding.
//
//nolint:govet // fieldalignment - current layout groups related fields logically (mutex with map it protects)
type Store[K comparable, V any] struct {
	subdirsMu   sync.RWMutex
	Dir         string          // Exported for testing - directory path
	subdirsMade map[string]bool // Cache of created subdirectories
}

// New creates a new file-based persistence layer.
// The cacheID is used as a subdirectory name under the OS cache directory.
// If dir is provided (non-empty), it's used as the base directory instead of OS cache dir.
// This is useful for testing with temporary directories.
func New[K comparable, V any](cacheID string, dir string) (*Store[K, V], error) {
	// Validate cacheID to prevent path traversal attacks
	if cacheID == "" {
		return nil, errors.New("cacheID cannot be empty")
	}
	// Check for path traversal attempts
	if strings.Contains(cacheID, "..") || strings.Contains(cacheID, "/") || strings.Contains(cacheID, "\\") {
		return nil, errors.New("invalid cacheID: contains path separators or traversal sequences")
	}
	// Check for null bytes (security)
	if strings.Contains(cacheID, "\x00") {
		return nil, errors.New("invalid cacheID: contains null byte")
	}

	// Use provided dir or get OS-appropriate cache directory
	var fullDir string
	if dir != "" {
		// Use provided directory (typically for testing)
		fullDir = filepath.Join(dir, cacheID)
	} else {
		// Get OS cache directory
		baseDir, err := os.UserCacheDir()
		if err != nil {
			return nil, fmt.Errorf("get user cache dir: %w", err)
		}
		fullDir = filepath.Join(baseDir, cacheID)
	}

	// Create directory and verify accessibility (assert readiness)
	if err := os.MkdirAll(fullDir, 0o750); err != nil {
		return nil, fmt.Errorf("create cache dir: %w", err)
	}

	// Verify directory is writable by creating a test file
	testFile := filepath.Join(fullDir, ".write_test")
	if err := os.WriteFile(testFile, []byte("test"), 0o600); err != nil {
		return nil, fmt.Errorf("cache dir not writable: %w", err)
	}
	if err := os.Remove(testFile); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("remove test file: %w", err)
	}

	return &Store[K, V]{
		Dir:         fullDir,
		subdirsMade: make(map[string]bool),
	}, nil
}

// ValidateKey checks if a key is valid for file persistence.
// Keys must be alphanumeric, dash, underscore, period, or colon, and max 127 characters.
func (*Store[K, V]) ValidateKey(key K) error {
	s := fmt.Sprintf("%v", key)
	if len(s) > maxKeyLength {
		return fmt.Errorf("key too long: %d bytes (max %d)", len(s), maxKeyLength)
	}

	// Allow alphanumeric, dash, underscore, period, colon
	for _, ch := range s {
		if (ch < 'a' || ch > 'z') && (ch < 'A' || ch > 'Z') &&
			(ch < '0' || ch > '9') && ch != '-' && ch != '_' && ch != '.' && ch != ':' {
			return fmt.Errorf("invalid character %q in key (only alphanumeric, dash, underscore, period, colon allowed)", ch)
		}
	}

	return nil
}

// keyToFilename converts a cache key to a filename with squid-style directory layout.
// Hashes the key and uses first 2 characters of hex hash as subdirectory for even distribution
// (e.g., key "http://example.com" -> "a3/a3f2...gob").
func (*Store[K, V]) keyToFilename(key K) string {
	s := fmt.Sprintf("%v", key)
	sum := sha256.Sum256([]byte(s))
	h := hex.EncodeToString(sum[:])

	// Squid-style: use first 2 chars of hash as subdirectory
	return filepath.Join(h[:2], h+".gob")
}

// Location returns the full file path where a key is stored.
// Implements the Store interface Location() method.
func (s *Store[K, V]) Location(key K) string {
	return filepath.Join(s.Dir, s.keyToFilename(key))
}

// Get retrieves a value from a file.
//
//nolint:revive // function-result-limit - required by persist.Store interface
func (s *Store[K, V]) Get(ctx context.Context, key K) (value V, expiry time.Time, found bool, err error) {
	var zero V
	fn := filepath.Join(s.Dir, s.keyToFilename(key))

	f, err := os.Open(fn)
	if err != nil {
		if os.IsNotExist(err) {
			return zero, time.Time{}, false, nil
		}
		return zero, time.Time{}, false, fmt.Errorf("open file: %w", err)
	}

	r, ok := readerPool.Get().(*bufio.Reader)
	if !ok {
		r = bufio.NewReaderSize(f, 4096)
	}
	r.Reset(f)

	var e Entry[K, V]
	decErr := gob.NewDecoder(r).Decode(&e)

	readerPool.Put(r)
	closeErr := f.Close()

	if decErr != nil {
		rmErr := os.Remove(fn)
		return zero, time.Time{}, false, errors.Join(
			fmt.Errorf("decode file: %w", decErr),
			closeErr,
			rmErr,
		)
	}

	if closeErr != nil {
		return zero, time.Time{}, false, fmt.Errorf("close file: %w", closeErr)
	}

	if !e.Expiry.IsZero() && time.Now().After(e.Expiry) {
		if err := os.Remove(fn); err != nil && !os.IsNotExist(err) {
			return zero, time.Time{}, false, fmt.Errorf("remove expired file: %w", err)
		}
		return zero, time.Time{}, false, nil
	}

	return e.Value, e.Expiry, true, nil
}

// Set saves a value to a file.
func (s *Store[K, V]) Set(ctx context.Context, key K, value V, expiry time.Time) error {
	fn := filepath.Join(s.Dir, s.keyToFilename(key))
	dir := filepath.Dir(fn)

	// Check if subdirectory already created (cache to avoid syscalls)
	s.subdirsMu.RLock()
	exists := s.subdirsMade[dir]
	s.subdirsMu.RUnlock()

	if !exists {
		// Hold write lock during check-and-create to avoid race
		s.subdirsMu.Lock()
		// Double-check after acquiring write lock
		if !s.subdirsMade[dir] {
			// Create subdirectory if needed (MkdirAll is idempotent)
			if err := os.MkdirAll(dir, 0o750); err != nil {
				s.subdirsMu.Unlock()
				return fmt.Errorf("create subdirectory: %w", err)
			}
			// Cache that we created it
			s.subdirsMade[dir] = true
		}
		s.subdirsMu.Unlock()
	}

	e := Entry[K, V]{
		Key:       key,
		Value:     value,
		Expiry:    expiry,
		UpdatedAt: time.Now(),
	}

	// Write to temp file first, then rename for atomicity
	tmp := fn + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}

	// Get writer from pool and reset it for this file
	w, ok := writerPool.Get().(*bufio.Writer)
	if !ok {
		w = bufio.NewWriterSize(f, 4096)
	}
	w.Reset(f)

	encErr := gob.NewEncoder(w).Encode(e)
	if encErr == nil {
		encErr = w.Flush() // Ensure buffered data is written
	}

	// Return writer to pool
	writerPool.Put(w)

	closeErr := f.Close()

	if encErr != nil {
		rmErr := os.Remove(tmp)
		return errors.Join(fmt.Errorf("encode entry: %w", encErr), rmErr)
	}

	if closeErr != nil {
		rmErr := os.Remove(tmp)
		return errors.Join(fmt.Errorf("close temp file: %w", closeErr), rmErr)
	}

	// Atomic rename
	if err := os.Rename(tmp, fn); err != nil {
		rmErr := os.Remove(tmp)
		return errors.Join(fmt.Errorf("rename file: %w", err), rmErr)
	}

	return nil
}

// Delete removes a file.
func (s *Store[K, V]) Delete(ctx context.Context, key K) error {
	fn := filepath.Join(s.Dir, s.keyToFilename(key))
	if err := os.Remove(fn); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove file: %w", err)
	}
	return nil
}

// Cleanup removes expired entries from file storage.
// Walks through all cache files and deletes those with expired timestamps.
// Returns the count of deleted entries and any errors encountered.
func (s *Store[K, V]) Cleanup(ctx context.Context, maxAge time.Duration) (int, error) {
	cutoff := time.Now().Add(-maxAge)
	n := 0
	var errs []error

	// Walk directory tree to handle squid-style subdirectories
	walkErr := filepath.Walk(s.Dir, func(path string, fi os.FileInfo, err error) error {
		// Check context cancellation
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if err != nil {
			errs = append(errs, fmt.Errorf("walk %s: %w", path, err))
			return nil
		}

		// Skip directories and non-gob files
		if fi.IsDir() || filepath.Ext(fi.Name()) != ".gob" {
			return nil
		}

		// Read and check expiry
		f, err := os.Open(path)
		if err != nil {
			errs = append(errs, fmt.Errorf("open %s: %w", path, err))
			return nil
		}

		// Get reader from pool
		r, ok := readerPool.Get().(*bufio.Reader)
		if !ok {
			r = bufio.NewReaderSize(f, 4096)
		}
		r.Reset(f)

		var e Entry[K, V]
		decErr := gob.NewDecoder(r).Decode(&e)

		readerPool.Put(r)
		closeErr := f.Close()

		if decErr != nil {
			errs = append(errs, errors.Join(
				fmt.Errorf("decode %s: %w", path, decErr),
				closeErr,
			))
			return nil
		}

		if closeErr != nil {
			errs = append(errs, fmt.Errorf("close %s: %w", path, closeErr))
			return nil
		}

		// Delete if expired
		if !e.Expiry.IsZero() && e.Expiry.Before(cutoff) {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				errs = append(errs, fmt.Errorf("remove %s: %w", path, err))
			} else {
				n++
			}
		}

		return nil
	})

	if walkErr != nil {
		errs = append(errs, fmt.Errorf("walk directory: %w", walkErr))
	}

	return n, errors.Join(errs...)
}

// Flush removes all entries from the file-based cache.
// Returns the number of entries removed and any errors encountered.
func (s *Store[K, V]) Flush(ctx context.Context) (int, error) {
	n := 0
	var errs []error

	walkErr := filepath.Walk(s.Dir, func(path string, fi os.FileInfo, err error) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("walk %s: %w", path, err))
			return nil
		}
		if fi.IsDir() || filepath.Ext(fi.Name()) != ".gob" {
			return nil
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("remove %s: %w", path, err))
		} else {
			n++
		}
		return nil
	})

	if walkErr != nil {
		errs = append(errs, fmt.Errorf("walk directory: %w", walkErr))
	}

	s.subdirsMu.Lock()
	s.subdirsMade = make(map[string]bool)
	s.subdirsMu.Unlock()

	return n, errors.Join(errs...)
}

// Len returns the number of entries in the file-based cache.
func (s *Store[K, V]) Len(ctx context.Context) (int, error) {
	n := 0
	var errs []error

	walkErr := filepath.Walk(s.Dir, func(_ string, fi os.FileInfo, err error) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if err != nil {
			errs = append(errs, err)
			return nil
		}
		if fi.IsDir() || filepath.Ext(fi.Name()) != ".gob" {
			return nil
		}
		n++
		return nil
	})

	if walkErr != nil {
		errs = append(errs, fmt.Errorf("walk directory: %w", walkErr))
	}

	return n, errors.Join(errs...)
}

// Close cleans up resources.
func (*Store[K, V]) Close() error {
	// No resources to clean up for file-based persistence
	return nil
}
