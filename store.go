package sfcache

import (
	"context"
	"time"
)

// Store defines the interface for cache persistence backends.
type Store[K comparable, V any] interface {
	// ValidateKey checks if a key is valid for this persistence store.
	ValidateKey(key K) error

	// Get retrieves a value from persistent storage.
	Get(ctx context.Context, key K) (V, time.Time, bool, error)

	// Set saves a value to persistent storage with an expiry time.
	Set(ctx context.Context, key K, value V, expiry time.Time) error

	// Delete removes a value from persistent storage.
	Delete(ctx context.Context, key K) error

	// LoadRecent streams up to limit most recently updated entries.
	// If limit is 0, returns all entries.
	LoadRecent(ctx context.Context, limit int) (<-chan Entry[K, V], <-chan error)

	// Cleanup removes expired entries older than maxAge.
	Cleanup(ctx context.Context, maxAge time.Duration) (int, error)

	// Location returns the storage location for a given key.
	Location(key K) string

	// Flush removes all entries from persistent storage.
	Flush(ctx context.Context) (int, error)

	// Len returns the number of entries in persistent storage.
	Len(ctx context.Context) (int, error)

	// Close releases any resources held by the persistence store.
	Close() error
}

// Entry represents a cache entry with its metadata.
type Entry[K comparable, V any] struct {
	Key       K
	Value     V
	Expiry    time.Time
	UpdatedAt time.Time
}
