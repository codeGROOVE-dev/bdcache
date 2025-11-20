package bdcache

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
)

const maxKeyLength = 127 // Maximum key length to avoid filesystem constraints

var (
	// Pool for bufio.Writer to reduce allocations
	writerPool = sync.Pool{
		New: func() any {
			return bufio.NewWriterSize(nil, 4096)
		},
	}
	// Pool for bufio.Reader to reduce allocations
	readerPool = sync.Pool{
		New: func() any {
			return bufio.NewReaderSize(nil, 4096)
		},
	}
)

// filePersist implements PersistenceLayer using local files with gob encoding.
type filePersist[K comparable, V any] struct {
	dir          string
	subdirsMu    sync.RWMutex
	subdirsMade  map[string]bool // Cache of created subdirectories
}

// ValidateKey checks if a key is valid for file persistence.
// Keys must be alphanumeric, dash, underscore, period, or colon, and max 127 characters.
func (*filePersist[K, V]) ValidateKey(key K) error {
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

// newFilePersist creates a new file-based persistence layer.
func newFilePersist[K comparable, V any](cacheID string) (*filePersist[K, V], error) {
	// Validate cacheID to prevent path traversal attacks
	if cacheID == "" {
		return nil, errors.New("cacheID cannot be empty")
	}
	// Check for path traversal attempts
	if strings.Contains(cacheID, "..") || strings.Contains(cacheID, "/") || strings.Contains(cacheID, "\\") {
		return nil, fmt.Errorf("invalid cacheID: contains path separators or traversal sequences")
	}
	// Check for null bytes (security)
	if strings.Contains(cacheID, "\x00") {
		return nil, errors.New("invalid cacheID: contains null byte")
	}

	// Get OS-appropriate cache directory
	baseDir, err := os.UserCacheDir()
	if err != nil {
		return nil, fmt.Errorf("get user cache dir: %w", err)
	}

	dir := filepath.Join(baseDir, cacheID)

	// Create directory if it doesn't exist
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create cache dir: %w", err)
	}

	slog.Debug("initialized file persistence", "dir", dir)

	return &filePersist[K, V]{
		dir:         dir,
		subdirsMade: make(map[string]bool),
	}, nil
}

// keyToFilename converts a cache key to a filename with squid-style directory layout.
// Uses first 2 characters of key as subdirectory (e.g., "ab/abcd123.gob").
func (*filePersist[K, V]) keyToFilename(key K) string {
	keyStr := fmt.Sprintf("%v", key)

	// Squid-style: use first 2 chars as subdirectory
	if len(keyStr) >= 2 {
		subdir := keyStr[:2]
		return filepath.Join(subdir, keyStr+".gob")
	}

	// For single-char keys, use the char itself as subdirectory
	return filepath.Join(keyStr, keyStr+".gob")
}

// Load retrieves a value from a file.
func (f *filePersist[K, V]) Load(ctx context.Context, key K) (V, time.Time, bool, error) {
	var zero V
	filename := filepath.Join(f.dir, f.keyToFilename(key))

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
	reader := readerPool.Get().(*bufio.Reader)
	reader.Reset(file)
	defer readerPool.Put(reader)

	var entry Entry[K, V]
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
func (f *filePersist[K, V]) Store(ctx context.Context, key K, value V, expiry time.Time) error {
	filename := filepath.Join(f.dir, f.keyToFilename(key))
	subdir := filepath.Dir(filename)

	// Check if subdirectory already created (cache to avoid syscalls)
	f.subdirsMu.RLock()
	exists := f.subdirsMade[subdir]
	f.subdirsMu.RUnlock()

	if !exists {
		// Create subdirectory if needed
		if err := os.MkdirAll(subdir, 0o750); err != nil {
			return fmt.Errorf("create subdirectory: %w", err)
		}
		// Cache that we created it
		f.subdirsMu.Lock()
		f.subdirsMade[subdir] = true
		f.subdirsMu.Unlock()
	}

	entry := Entry[K, V]{
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
	writer := writerPool.Get().(*bufio.Writer)
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
func (f *filePersist[K, V]) Delete(ctx context.Context, key K) error {
	filename := filepath.Join(f.dir, f.keyToFilename(key))
	err := os.Remove(filename)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove file: %w", err)
	}
	return nil
}

// LoadRecent streams entries from files, returning up to 'limit' most recently updated entries.
func (f *filePersist[K, V]) LoadRecent(ctx context.Context, limit int) (<-chan Entry[K, V], <-chan error) {
	entryCh := make(chan Entry[K, V], 100)
	errCh := make(chan error, 1)

	go func() {
		defer close(entryCh)
		defer close(errCh)

		now := time.Now()
		expired := 0

		// Load all entries first to sort by UpdatedAt
		var entries []Entry[K, V]

		// Walk the directory tree to support squid-style subdirectories
		err := filepath.Walk(f.dir, func(path string, info os.FileInfo, err error) error {
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
			reader := readerPool.Get().(*bufio.Reader)
			reader.Reset(file)

			var e Entry[K, V]
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

// LoadAll streams all entries from files (no limit).
func (f *filePersist[K, V]) LoadAll(ctx context.Context) (<-chan Entry[K, V], <-chan error) {
	return f.LoadRecent(ctx, 0)
}

// Close cleans up resources.
func (*filePersist[K, V]) Close() error {
	// No resources to clean up for file-based persistence
	return nil
}
