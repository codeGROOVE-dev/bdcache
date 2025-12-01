package bdcache

import (
	"context"
	"time"
)

// PersistenceLayer defines the interface for cache persistence backends.
type PersistenceLayer[K comparable, V any] interface {
	// ValidateKey checks if a key is valid for this persistence layer.
	// Returns an error if the key violates constraints.
	ValidateKey(key K) error

	// Load retrieves a value from persistent storage.
	// Returns the value, expiry time, whether it was found, and any error.
	Load(ctx context.Context, key K) (V, time.Time, bool, error)

	// Store saves a value to persistent storage with an expiry time.
	Store(ctx context.Context, key K, value V, expiry time.Time) error

	// Delete removes a value from persistent storage.
	Delete(ctx context.Context, key K) error

	// LoadRecent returns channels for streaming the most recently updated entries from persistent storage.
	// Used for warming up the cache on startup. Returns up to 'limit' most recently updated entries.
	// If limit is 0, returns all entries.
	// The entry channel should be closed when all entries have been sent.
	// If an error occurs, send it on the error channel.
	LoadRecent(ctx context.Context, limit int) (<-chan Entry[K, V], <-chan error)

	// Cleanup removes expired entries from persistent storage.
	// maxAge specifies how old entries must be before deletion.
	// Returns the number of entries deleted and any error.
	Cleanup(ctx context.Context, maxAge time.Duration) (int, error)

	// Location returns the storage location/identifier for a given key.
	// For file-based persistence, this returns the file path.
	// For database persistence, this returns the database key/ID.
	// Useful for testing and debugging to verify where items are stored.
	Location(key K) string

	// Flush removes all entries from persistent storage.
	// Returns the number of entries removed and any error.
	Flush(ctx context.Context) (int, error)

	// Len returns the number of entries in persistent storage.
	Len(ctx context.Context) (int, error)

	// Close releases any resources held by the persistence layer.
	Close() error
}

// Entry represents a cache entry with its metadata.
type Entry[K comparable, V any] struct {
	Key       K
	Value     V
	Expiry    time.Time
	UpdatedAt time.Time
}
