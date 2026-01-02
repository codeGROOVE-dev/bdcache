// Package localfs provides local filesystem persistence for fido.
package localfs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/codeGROOVE-dev/fido/pkg/store/compress"
)

// Entry represents a cache entry with its metadata for serialization.
type Entry[K comparable, V any] struct {
	Key       K
	Value     V
	Expiry    time.Time
	UpdatedAt time.Time
}

const maxKeyLength = 127 // Maximum key length to avoid filesystem constraints

// Store implements file-based persistence using local files with JSON encoding.
//
//nolint:govet // fieldalignment - current layout groups related fields logically (mutex with map it protects)
type Store[K comparable, V any] struct {
	subdirsMu   sync.RWMutex
	Dir         string              // Exported for testing - directory path
	subdirsMade map[string]bool     // Cache of created subdirectories
	compressor  compress.Compressor // Compression algorithm
	ext         string              // File extension based on compressor
}

// New creates a new file-based persistence layer.
// The cacheID is used as a subdirectory name under the OS cache directory.
// If dir is provided (non-empty), it's used as the base directory instead of OS cache dir.
// Optional compressor enables compression (default: no compression, plain JSON with .j extension).
func New[K comparable, V any](cacheID, dir string, c ...compress.Compressor) (*Store[K, V], error) {
	if cacheID == "" {
		return nil, errors.New("cacheID cannot be empty")
	}
	if strings.Contains(cacheID, "..") || strings.Contains(cacheID, "/") || strings.Contains(cacheID, "\\") {
		return nil, errors.New("invalid cacheID: contains path separators or traversal sequences")
	}
	if strings.Contains(cacheID, "\x00") {
		return nil, errors.New("invalid cacheID: contains null byte")
	}

	comp := compress.None()
	if len(c) > 0 && c[0] != nil {
		comp = c[0]
	}

	var fullDir string
	if dir != "" {
		fullDir = filepath.Join(dir, cacheID)
	} else {
		baseDir, err := os.UserCacheDir()
		if err != nil {
			return nil, fmt.Errorf("get user cache dir: %w", err)
		}
		fullDir = filepath.Join(baseDir, cacheID)
	}

	if err := os.MkdirAll(fullDir, 0o750); err != nil {
		return nil, fmt.Errorf("create cache dir: %w", err)
	}

	testFile := filepath.Join(fullDir, ".write_test")
	if err := os.WriteFile(testFile, []byte("test"), 0o600); err != nil {
		return nil, fmt.Errorf("cache dir not writable: %w", err)
	}
	_ = os.Remove(testFile) //nolint:errcheck // best-effort cleanup

	ext := comp.Extension()
	if ext == "" {
		ext = ".j"
	}

	return &Store[K, V]{
		Dir:         fullDir,
		subdirsMade: make(map[string]bool),
		compressor:  comp,
		ext:         ext,
	}, nil
}

// ValidateKey checks if a key is valid for file persistence.
// Since keys are hashed to SHA256, any characters are allowed.
// Only length is validated to prevent memory issues.
func (*Store[K, V]) ValidateKey(key K) error {
	k := fmt.Sprintf("%v", key)
	if k == "" {
		return errors.New("key cannot be empty")
	}
	if len(k) > maxKeyLength {
		return fmt.Errorf("key too long: %d bytes (max %d)", len(k), maxKeyLength)
	}
	return nil
}

// keyToFilename converts a cache key to a filename with squid-style directory layout.
// Hashes the key and uses first 2 characters of hex hash as subdirectory for even distribution
// (e.g., key "mykey" -> "a3/a3f2....j" or "a3/a3f2....s" with S2 compression).
func (s *Store[K, V]) keyToFilename(key K) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%v", key))
	h := hex.EncodeToString(sum[:])
	return filepath.Join(h[:2], h+s.ext)
}

// Location returns the full file path where a key is stored.
func (s *Store[K, V]) Location(key K) string {
	return filepath.Join(s.Dir, s.keyToFilename(key))
}

// Get retrieves a value from a file.
//
//nolint:revive // function-result-limit - required by persist.Store interface
func (s *Store[K, V]) Get(ctx context.Context, key K) (value V, expiry time.Time, found bool, err error) {
	var zero V
	fn := filepath.Join(s.Dir, s.keyToFilename(key))

	data, err := os.ReadFile(fn)
	if err != nil {
		if os.IsNotExist(err) {
			return zero, time.Time{}, false, nil
		}
		return zero, time.Time{}, false, fmt.Errorf("read file: %w", err)
	}

	jsonData, err := s.compressor.Decode(data)
	if err != nil {
		rmErr := os.Remove(fn)
		return zero, time.Time{}, false, errors.Join(fmt.Errorf("decompress: %w", err), rmErr)
	}

	var e Entry[K, V]
	if err := json.Unmarshal(jsonData, &e); err != nil {
		rmErr := os.Remove(fn)
		return zero, time.Time{}, false, errors.Join(
			fmt.Errorf("decode file: %w", err),
			rmErr,
		)
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

	jsonData, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("encode entry: %w", err)
	}

	data, err := s.compressor.Encode(jsonData)
	if err != nil {
		return fmt.Errorf("compress: %w", err)
	}

	// Write to temp file first, then rename for atomicity
	tmp := fn + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write temp file: %w", err)
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

// isCacheFile returns true if the file matches the store's cache file extension.
func (s *Store[K, V]) isCacheFile(name string) bool {
	return filepath.Ext(name) == s.ext
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

		// Skip directories and non-matching files
		if fi.IsDir() || !s.isCacheFile(fi.Name()) {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			errs = append(errs, fmt.Errorf("read %s: %w", path, err))
			return nil
		}

		jsonData, err := s.compressor.Decode(data)
		if err != nil {
			errs = append(errs, fmt.Errorf("decompress %s: %w", path, err))
			return nil
		}

		var e Entry[K, V]
		if err := json.Unmarshal(jsonData, &e); err != nil {
			errs = append(errs, fmt.Errorf("decode %s: %w", path, err))
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
		if fi.IsDir() || !s.isCacheFile(fi.Name()) {
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
		if fi.IsDir() || !s.isCacheFile(fi.Name()) {
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
