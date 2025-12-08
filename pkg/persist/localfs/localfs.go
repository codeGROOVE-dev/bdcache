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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/codeGROOVE-dev/sfcache"
)

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

// store implements Store using local files with gob encoding.
//
//nolint:govet // fieldalignment - current layout groups related fields logically (mutex with map it protects)
type store[K comparable, V any] struct {
	subdirsMu   sync.RWMutex
	Dir         string          // Exported for testing - directory path
	subdirsMade map[string]bool // Cache of created subdirectories
}

// New creates a new file-based persistence layer.
// The cacheID is used as a subdirectory name under the OS cache directory.
// If dir is provided (non-empty), it's used as the base directory instead of OS cache dir.
// This is useful for testing with temporary directories.
func New[K comparable, V any](cacheID string, dir string) (sfcache.Store[K, V], error) {
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

	return &store[K, V]{
		Dir:         fullDir,
		subdirsMade: make(map[string]bool),
	}, nil
}

// ValidateKey checks if a key is valid for file persistence.
// Keys must be alphanumeric, dash, underscore, period, or colon, and max 127 characters.
func (*store[K, V]) ValidateKey(key K) error {
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
func (*store[K, V]) keyToFilename(key K) string {
	s := fmt.Sprintf("%v", key)
	sum := sha256.Sum256([]byte(s))
	h := hex.EncodeToString(sum[:])

	// Squid-style: use first 2 chars of hash as subdirectory
	return filepath.Join(h[:2], h+".gob")
}

// Location returns the full file path where a key is stored.
// Implements the Store interface Location() method.
func (p *store[K, V]) Location(key K) string {
	return filepath.Join(p.Dir, p.keyToFilename(key))
}

// Get retrieves a value from a file.
//
//nolint:revive // function-result-limit - required by persist.Store interface
func (p *store[K, V]) Get(ctx context.Context, key K) (value V, expiry time.Time, found bool, err error) {
	var zero V
	filename := filepath.Join(p.Dir, p.keyToFilename(key))

	file, err := os.Open(filename)
	if err != nil {
		if os.IsNotExist(err) {
			return zero, time.Time{}, false, nil
		}
		return zero, time.Time{}, false, fmt.Errorf("open file: %w", err)
	}

	// Get reader from pool and reset it for this file
	reader, ok := readerPool.Get().(*bufio.Reader)
	if !ok {
		reader = bufio.NewReaderSize(file, 4096)
	}
	reader.Reset(file)

	var entry sfcache.Entry[K, V]
	dec := gob.NewDecoder(reader)
	decErr := dec.Decode(&entry)

	readerPool.Put(reader)
	closeErr := file.Close()

	if decErr != nil {
		// File corrupted, try to remove it
		rmErr := os.Remove(filename)
		return zero, time.Time{}, false, errors.Join(
			fmt.Errorf("decode file: %w", decErr),
			closeErr,
			rmErr,
		)
	}

	if closeErr != nil {
		return zero, time.Time{}, false, fmt.Errorf("close file: %w", closeErr)
	}

	// Check expiration
	if !entry.Expiry.IsZero() && time.Now().After(entry.Expiry) {
		if err := os.Remove(filename); err != nil && !os.IsNotExist(err) {
			return zero, time.Time{}, false, fmt.Errorf("remove expired file: %w", err)
		}
		return zero, time.Time{}, false, nil
	}

	return entry.Value, entry.Expiry, true, nil
}

// Set saves a value to a file.
func (p *store[K, V]) Set(ctx context.Context, key K, value V, expiry time.Time) error {
	filename := filepath.Join(p.Dir, p.keyToFilename(key))
	subdir := filepath.Dir(filename)

	// Check if subdirectory already created (cache to avoid syscalls)
	p.subdirsMu.RLock()
	exists := p.subdirsMade[subdir]
	p.subdirsMu.RUnlock()

	if !exists {
		// Hold write lock during check-and-create to avoid race
		p.subdirsMu.Lock()
		// Double-check after acquiring write lock
		if !p.subdirsMade[subdir] {
			// Create subdirectory if needed (MkdirAll is idempotent)
			if err := os.MkdirAll(subdir, 0o750); err != nil {
				p.subdirsMu.Unlock()
				return fmt.Errorf("create subdirectory: %w", err)
			}
			// Cache that we created it
			p.subdirsMade[subdir] = true
		}
		p.subdirsMu.Unlock()
	}

	entry := sfcache.Entry[K, V]{
		Key:       key,
		Value:     value,
		Expiry:    expiry,
		UpdatedAt: time.Now(),
	}

	// Write to temp file first, then rename for atomicity
	tempFile := filename + ".tmp"
	file, err := os.Create(tempFile)
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}

	// Get writer from pool and reset it for this file
	writer, ok := writerPool.Get().(*bufio.Writer)
	if !ok {
		writer = bufio.NewWriterSize(file, 4096)
	}
	writer.Reset(file)

	enc := gob.NewEncoder(writer)
	encErr := enc.Encode(entry)
	if encErr == nil {
		encErr = writer.Flush() // Ensure buffered data is written
	}

	// Return writer to pool
	writerPool.Put(writer)

	closeErr := file.Close()

	if encErr != nil {
		rmErr := os.Remove(tempFile)
		return errors.Join(fmt.Errorf("encode entry: %w", encErr), rmErr)
	}

	if closeErr != nil {
		rmErr := os.Remove(tempFile)
		return errors.Join(fmt.Errorf("close temp file: %w", closeErr), rmErr)
	}

	// Atomic rename
	if err := os.Rename(tempFile, filename); err != nil {
		rmErr := os.Remove(tempFile)
		return errors.Join(fmt.Errorf("rename file: %w", err), rmErr)
	}

	return nil
}

// Delete removes a file.
func (p *store[K, V]) Delete(ctx context.Context, key K) error {
	filename := filepath.Join(p.Dir, p.keyToFilename(key))
	err := os.Remove(filename)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove file: %w", err)
	}
	return nil
}

// LoadRecent streams entries from files, returning up to 'limit' most recently updated entries.
// Errors encountered while reading individual files are collected and returned via the error channel.
//
//nolint:gocritic // unnamedResult - channel returns are self-documenting
func (p *store[K, V]) LoadRecent(ctx context.Context, limit int) (<-chan sfcache.Entry[K, V], <-chan error) {
	entryCh := make(chan sfcache.Entry[K, V], 100)
	errCh := make(chan error, 1)

	go func() {
		defer close(entryCh)
		defer close(errCh)

		now := time.Now()
		var errs []error

		// Load all entries first to sort by UpdatedAt
		var entries []sfcache.Entry[K, V]

		// Walk the directory tree to support squid-style subdirectories
		walkErr := filepath.Walk(p.Dir, func(path string, info os.FileInfo, err error) error {
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

			if info.IsDir() || filepath.Ext(info.Name()) != ".gob" {
				return nil
			}

			file, err := os.Open(path)
			if err != nil {
				errs = append(errs, fmt.Errorf("open %s: %w", path, err))
				return nil
			}

			// Get reader from pool and reset it for this file
			reader, ok := readerPool.Get().(*bufio.Reader)
			if !ok {
				reader = bufio.NewReaderSize(file, 4096)
			}
			reader.Reset(file)

			var e sfcache.Entry[K, V]
			dec := gob.NewDecoder(reader)
			decErr := dec.Decode(&e)

			readerPool.Put(reader)
			closeErr := file.Close()

			if decErr != nil {
				rmErr := os.Remove(path)
				errs = append(errs, errors.Join(
					fmt.Errorf("decode %s: %w", path, decErr),
					closeErr,
					rmErr,
				))
				return nil
			}

			if closeErr != nil {
				errs = append(errs, fmt.Errorf("close %s: %w", path, closeErr))
				return nil
			}

			// Skip expired entries and clean up
			if !e.Expiry.IsZero() && now.After(e.Expiry) {
				if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
					errs = append(errs, fmt.Errorf("remove expired %s: %w", path, err))
				}
				return nil
			}

			entries = append(entries, e)
			return nil
		})

		if walkErr != nil {
			errs = append(errs, fmt.Errorf("walk dir: %w", walkErr))
		}

		// Sort by UpdatedAt descending (most recent first)
		sort.Slice(entries, func(i, j int) bool {
			return entries[i].UpdatedAt.After(entries[j].UpdatedAt)
		})

		// Send only up to limit entries
		sent := 0
		for _, e := range entries {
			if limit > 0 && sent >= limit {
				break
			}
			entryCh <- e
			sent++
		}

		// Send collected errors
		if len(errs) > 0 {
			errCh <- errors.Join(errs...)
		}
	}()

	return entryCh, errCh
}

// Cleanup removes expired entries from file storage.
// Walks through all cache files and deletes those with expired timestamps.
// Returns the count of deleted entries and any errors encountered.
func (p *store[K, V]) Cleanup(ctx context.Context, maxAge time.Duration) (int, error) {
	cutoff := time.Now().Add(-maxAge)
	deleted := 0
	var errs []error

	// Walk directory tree to handle squid-style subdirectories
	walkErr := filepath.Walk(p.Dir, func(path string, info os.FileInfo, err error) error {
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
		if info.IsDir() || filepath.Ext(info.Name()) != ".gob" {
			return nil
		}

		// Read and check expiry
		file, err := os.Open(path)
		if err != nil {
			errs = append(errs, fmt.Errorf("open %s: %w", path, err))
			return nil
		}

		// Get reader from pool
		reader, ok := readerPool.Get().(*bufio.Reader)
		if !ok {
			reader = bufio.NewReaderSize(file, 4096)
		}
		reader.Reset(file)

		var entry sfcache.Entry[K, V]
		decoder := gob.NewDecoder(reader)
		decErr := decoder.Decode(&entry)

		readerPool.Put(reader)
		closeErr := file.Close()

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
		if !entry.Expiry.IsZero() && entry.Expiry.Before(cutoff) {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				errs = append(errs, fmt.Errorf("remove %s: %w", path, err))
			} else {
				deleted++
			}
		}

		return nil
	})

	if walkErr != nil {
		errs = append(errs, fmt.Errorf("walk directory: %w", walkErr))
	}

	return deleted, errors.Join(errs...)
}

// Flush removes all entries from the file-based cache.
// Returns the number of entries removed and any errors encountered.
func (p *store[K, V]) Flush(ctx context.Context) (int, error) {
	n := 0
	var errs []error

	walkErr := filepath.Walk(p.Dir, func(path string, fi os.FileInfo, err error) error {
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

	p.subdirsMu.Lock()
	p.subdirsMade = make(map[string]bool)
	p.subdirsMu.Unlock()

	return n, errors.Join(errs...)
}

// Len returns the number of entries in the file-based cache.
func (p *store[K, V]) Len(ctx context.Context) (int, error) {
	n := 0
	var errs []error

	walkErr := filepath.Walk(p.Dir, func(_ string, fi os.FileInfo, err error) error {
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
func (*store[K, V]) Close() error {
	// No resources to clean up for file-based persistence
	return nil
}
