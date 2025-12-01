// Package localfs provides local filesystem persistence for bdcache.
package localfs

import (
	"bufio"
	"context"
	"encoding/gob"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/codeGROOVE-dev/bdcache"
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

// persister implements PersistenceLayer using local files with gob encoding.
//
//nolint:govet // fieldalignment - current layout groups related fields logically (mutex with map it protects)
type persister[K comparable, V any] struct {
	subdirsMu   sync.RWMutex
	Dir         string          // Exported for testing - directory path
	subdirsMade map[string]bool // Cache of created subdirectories
}

// New creates a new file-based persistence layer.
// The cacheID is used as a subdirectory name under the OS cache directory.
// If dir is provided (non-empty), it's used as the base directory instead of OS cache dir.
// This is useful for testing with temporary directories.
func New[K comparable, V any](cacheID string, dir string) (bdcache.PersistenceLayer[K, V], error) {
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
	if err := os.Remove(testFile); err != nil {
		slog.Debug("failed to remove test file", "file", testFile, "error", err)
	}

	slog.Debug("initialized local filesystem persistence", "dir", fullDir)

	return &persister[K, V]{
		Dir:         fullDir,
		subdirsMade: make(map[string]bool),
	}, nil
}

// ValidateKey checks if a key is valid for file persistence.
// Keys must be alphanumeric, dash, underscore, period, or colon, and max 127 characters.
func (*persister[K, V]) ValidateKey(key K) error {
	keyStr := fmt.Sprintf("%v", key)
	if len(keyStr) > maxKeyLength {
		return fmt.Errorf("key too long: %d bytes (max %d)", len(keyStr), maxKeyLength)
	}

	// Allow alphanumeric, dash, underscore, period, colon
	for _, ch := range keyStr {
		if (ch < 'a' || ch > 'z') && (ch < 'A' || ch > 'Z') &&
			(ch < '0' || ch > '9') && ch != '-' && ch != '_' && ch != '.' && ch != ':' {
			return fmt.Errorf("invalid character %q in key (only alphanumeric, dash, underscore, period, colon allowed)", ch)
		}
	}

	return nil
}

// keyToFilename converts a cache key to a filename with squid-style directory layout.
// Uses first 2 characters of key as subdirectory (e.g., "ab/abcd123.gob").
func (*persister[K, V]) keyToFilename(key K) string {
	keyStr := fmt.Sprintf("%v", key)

	// Squid-style: use first 2 chars as subdirectory
	if len(keyStr) >= 2 {
		subdir := keyStr[:2]
		return filepath.Join(subdir, keyStr+".gob")
	}

	// For single-char keys, use the char itself as subdirectory
	return filepath.Join(keyStr, keyStr+".gob")
}

// Location returns the full file path where a key is stored.
// Implements the PersistenceLayer interface Location() method.
func (p *persister[K, V]) Location(key K) string {
	return filepath.Join(p.Dir, p.keyToFilename(key))
}

// Load retrieves a value from a file.
//
//nolint:revive // function-result-limit - required by PersistenceLayer interface
func (p *persister[K, V]) Load(ctx context.Context, key K) (value V, expiry time.Time, found bool, err error) {
	var zero V
	filename := filepath.Join(p.Dir, p.keyToFilename(key))

	file, err := os.Open(filename)
	if err != nil {
		if os.IsNotExist(err) {
			return zero, time.Time{}, false, nil
		}
		return zero, time.Time{}, false, fmt.Errorf("open file: %w", err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			slog.Debug("failed to close file", "file", filename, "error", err)
		}
	}()

	// Get reader from pool and reset it for this file
	reader, ok := readerPool.Get().(*bufio.Reader)
	if !ok {
		reader = bufio.NewReaderSize(file, 4096)
	}
	reader.Reset(file)
	defer readerPool.Put(reader)

	var entry bdcache.Entry[K, V]
	dec := gob.NewDecoder(reader)
	if err := dec.Decode(&entry); err != nil {
		// File corrupted, remove it
		if err := os.Remove(filename); err != nil && !os.IsNotExist(err) {
			slog.Debug("failed to remove corrupted file", "file", filename, "error", err)
		}
		return zero, time.Time{}, false, nil
	}

	// Check expiration
	if !entry.Expiry.IsZero() && time.Now().After(entry.Expiry) {
		if err := os.Remove(filename); err != nil && !os.IsNotExist(err) {
			slog.Debug("failed to remove expired file", "file", filename, "error", err)
		}
		return zero, time.Time{}, false, nil
	}

	return entry.Value, entry.Expiry, true, nil
}

// Store saves a value to a file.
func (p *persister[K, V]) Store(ctx context.Context, key K, value V, expiry time.Time) error {
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

	entry := bdcache.Entry[K, V]{
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
		if err := os.Remove(tempFile); err != nil && !os.IsNotExist(err) {
			slog.Debug("failed to remove temp file after encode error", "file", tempFile, "error", err)
		}
		return fmt.Errorf("encode entry: %w", encErr)
	}

	if closeErr != nil {
		if err := os.Remove(tempFile); err != nil && !os.IsNotExist(err) {
			slog.Debug("failed to remove temp file after close error", "file", tempFile, "error", err)
		}
		return fmt.Errorf("close temp file: %w", closeErr)
	}

	// Atomic rename
	if err := os.Rename(tempFile, filename); err != nil {
		if err := os.Remove(tempFile); err != nil && !os.IsNotExist(err) {
			slog.Debug("failed to remove temp file after rename error", "file", tempFile, "error", err)
		}
		return fmt.Errorf("rename file: %w", err)
	}

	return nil
}

// Delete removes a file.
func (p *persister[K, V]) Delete(ctx context.Context, key K) error {
	filename := filepath.Join(p.Dir, p.keyToFilename(key))
	err := os.Remove(filename)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove file: %w", err)
	}
	return nil
}

// LoadRecent streams entries from files, returning up to 'limit' most recently updated entries.
//
//nolint:gocritic // unnamedResult - channel returns are self-documenting
func (p *persister[K, V]) LoadRecent(ctx context.Context, limit int) (<-chan bdcache.Entry[K, V], <-chan error) {
	entryCh := make(chan bdcache.Entry[K, V], 100)
	errCh := make(chan error, 1)

	go func() {
		defer close(entryCh)
		defer close(errCh)

		now := time.Now()
		expired := 0

		// Load all entries first to sort by UpdatedAt
		var entries []bdcache.Entry[K, V]

		// Walk the directory tree to support squid-style subdirectories
		err := filepath.Walk(p.Dir, func(path string, info os.FileInfo, err error) error {
			// Check context cancellation
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}

			if err != nil {
				slog.Warn("error walking cache dir", "path", path, "error", err)
				return nil // Continue walking
			}

			if info.IsDir() || filepath.Ext(info.Name()) != ".gob" {
				return nil
			}

			file, err := os.Open(path)
			if err != nil {
				slog.Warn("failed to open cache file", "file", path, "error", err)
				return nil
			}

			// Get reader from pool and reset it for this file
			reader, ok := readerPool.Get().(*bufio.Reader)
			if !ok {
				reader = bufio.NewReaderSize(file, 4096)
			}
			reader.Reset(file)

			var e bdcache.Entry[K, V]
			dec := gob.NewDecoder(reader)
			if err := dec.Decode(&e); err != nil {
				slog.Warn("failed to decode cache file", "file", path, "error", err)
				readerPool.Put(reader)
				if err := file.Close(); err != nil {
					slog.Debug("failed to close file after decode error", "file", path, "error", err)
				}
				if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
					slog.Debug("failed to remove corrupted file", "file", path, "error", err)
				}
				return nil
			}
			readerPool.Put(reader)
			if err := file.Close(); err != nil {
				slog.Debug("failed to close file", "file", path, "error", err)
			}

			// Skip expired entries and clean up
			if !e.Expiry.IsZero() && now.After(e.Expiry) {
				if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
					slog.Debug("failed to remove expired file", "file", path, "error", err)
				}
				expired++
				return nil
			}

			entries = append(entries, e)
			return nil
		})
		if err != nil {
			errCh <- fmt.Errorf("walk dir: %w", err)
			return
		}

		// Sort by UpdatedAt descending (most recent first)
		sort.Slice(entries, func(i, j int) bool {
			return entries[i].UpdatedAt.After(entries[j].UpdatedAt)
		})

		// Send only up to limit entries
		loaded := 0
		for _, e := range entries {
			if limit > 0 && loaded >= limit {
				break
			}
			entryCh <- e
			loaded++
		}

		slog.Info("loaded cache entries from disk", "loaded", loaded, "expired", expired, "total", len(entries))
	}()

	return entryCh, errCh
}

// Cleanup removes expired entries from file storage.
// Walks through all cache files and deletes those with expired timestamps.
func (p *persister[K, V]) Cleanup(ctx context.Context, maxAge time.Duration) (int, error) {
	cutoff := time.Now().Add(-maxAge)
	deleted := 0

	// Walk directory tree to handle squid-style subdirectories
	err := filepath.Walk(p.Dir, func(path string, info os.FileInfo, err error) error {
		// Check context cancellation
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if err != nil {
			slog.Debug("error walking cache dir during cleanup", "path", path, "error", err)
			return nil // Continue walking
		}

		// Skip directories and non-gob files
		if info.IsDir() || filepath.Ext(info.Name()) != ".gob" {
			return nil
		}

		// Read and check expiry
		file, err := os.Open(path)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			slog.Debug("failed to open file for cleanup", "file", path, "error", err)
			return nil
		}

		// Get reader from pool
		reader, ok := readerPool.Get().(*bufio.Reader)
		if !ok {
			reader = bufio.NewReaderSize(file, 4096)
		}
		reader.Reset(file)

		var entry bdcache.Entry[K, V]
		decoder := gob.NewDecoder(reader)
		err = decoder.Decode(&entry)

		readerPool.Put(reader)

		if closeErr := file.Close(); closeErr != nil {
			slog.Debug("failed to close file during cleanup", "file", path, "error", closeErr)
		}

		if err != nil {
			slog.Debug("failed to decode file for cleanup", "file", path, "error", err)
			return nil
		}

		// Delete if expired
		if !entry.Expiry.IsZero() && entry.Expiry.Before(cutoff) {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				slog.Debug("failed to remove expired file", "file", path, "error", err)
				return nil
			}
			deleted++
		}

		return nil
	})
	if err != nil {
		return deleted, fmt.Errorf("walk directory: %w", err)
	}

	if deleted > 0 {
		slog.Info("cleaned up expired file entries", "count", deleted, "dir", p.Dir)
	}
	return deleted, nil
}

// Flush removes all entries from the file-based cache.
// Returns the number of entries removed and any error.
func (p *persister[K, V]) Flush(ctx context.Context) (int, error) {
	n := 0
	err := filepath.Walk(p.Dir, func(path string, fi os.FileInfo, err error) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if err != nil {
			slog.Debug("error walking cache dir during flush", "path", path, "error", err)
			return nil
		}
		if fi.IsDir() || filepath.Ext(fi.Name()) != ".gob" {
			return nil
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			slog.Debug("failed to remove file during flush", "file", path, "error", err)
			return nil
		}
		n++
		return nil
	})
	if err != nil {
		return n, fmt.Errorf("walk directory: %w", err)
	}

	p.subdirsMu.Lock()
	p.subdirsMade = make(map[string]bool)
	p.subdirsMu.Unlock()

	if n > 0 {
		slog.Info("flushed file cache", "count", n, "dir", p.Dir)
	}
	return n, nil
}

// Len returns the number of entries in the file-based cache.
func (p *persister[K, V]) Len(ctx context.Context) (int, error) {
	n := 0
	err := filepath.Walk(p.Dir, func(path string, fi os.FileInfo, err error) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if err != nil {
			slog.Debug("error walking cache dir during len", "path", path, "error", err)
			return nil
		}
		if fi.IsDir() || filepath.Ext(fi.Name()) != ".gob" {
			return nil
		}
		n++
		return nil
	})
	if err != nil {
		return n, fmt.Errorf("walk directory: %w", err)
	}
	return n, nil
}

// Close cleans up resources.
func (*persister[K, V]) Close() error {
	// No resources to clean up for file-based persistence
	return nil
}
