// Package datastore provides Google Cloud Datastore persistence for sfcache.
package datastore

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	ds "github.com/codeGROOVE-dev/ds9/pkg/datastore"
)

const (
	datastoreKind      = "CacheEntry"
	maxDatastoreKeyLen = 1500 // Datastore has stricter key length limits
)

// Store implements persistence using Google Cloud Datastore.
type Store[K comparable, V any] struct {
	client *ds.Client
	kind   string
}

// ValidateKey checks if a key is valid for Datastore persistence.
// Datastore has stricter key length limits than files.
func (*Store[K, V]) ValidateKey(key K) error {
	s := fmt.Sprintf("%v", key)
	if len(s) > maxDatastoreKeyLen {
		return fmt.Errorf("key too long: %d bytes (max %d for datastore)", len(s), maxDatastoreKeyLen)
	}
	if s == "" {
		return errors.New("key cannot be empty")
	}
	return nil
}

// Location returns the Datastore key path for a given cache key.
// Implements the Store interface Location() method.
// Format: "kind/key" (e.g., "CacheEntry/mykey").
func (s *Store[K, V]) Location(key K) string {
	return fmt.Sprintf("%s/%v", s.kind, key)
}

// entry represents a cache entry in Datastore.
// We use base64-encoded string for Value to avoid datastore []byte limitations.
// The key is stored in the Datastore entity key itself.
type entry struct {
	Expiry    time.Time `datastore:"expiry,omitempty,noindex"`
	UpdatedAt time.Time `datastore:"updated_at"`
	Value     string    `datastore:"value,noindex"`
}

// New creates a new Datastore-based persistence layer.
// The cacheID is used as the Datastore database name.
// An empty projectID will be auto-detected from the environment.
func New[K comparable, V any](ctx context.Context, cacheID string) (*Store[K, V], error) {
	// Empty project ID lets ds9 auto-detect
	client, err := ds.NewClientWithDatabase(ctx, "", cacheID)
	if err != nil {
		return nil, fmt.Errorf("create datastore client: %w", err)
	}

	// Verify connectivity (assert readiness)
	// Note: ds9 doesn't expose Ping, but client creation validates connectivity

	return &Store[K, V]{
		client: client,
		kind:   datastoreKind,
	}, nil
}

// makeKey creates a Datastore key from a cache key.
// We use the string representation directly as the key name.
func (s *Store[K, V]) makeKey(key K) *ds.Key {
	return ds.NameKey(s.kind, fmt.Sprintf("%v", key), nil)
}

// Get retrieves a value from Datastore.
//
//nolint:revive // function-result-limit - required by persist.Store interface
func (s *Store[K, V]) Get(ctx context.Context, key K) (value V, expiry time.Time, found bool, err error) {
	var zero V
	k := s.makeKey(key)

	var e entry
	if err := s.client.Get(ctx, k, &e); err != nil {
		if errors.Is(err, ds.ErrNoSuchEntity) {
			return zero, time.Time{}, false, nil
		}
		return zero, time.Time{}, false, fmt.Errorf("datastore get: %w", err)
	}

	// Check expiration - return miss but don't delete
	// Cleanup is handled by native Datastore TTL or periodic Cleanup() calls
	if !e.Expiry.IsZero() && time.Now().After(e.Expiry) {
		return zero, time.Time{}, false, nil
	}

	// Decode from base64
	b, err := base64.StdEncoding.DecodeString(e.Value)
	if err != nil {
		return zero, time.Time{}, false, fmt.Errorf("decode base64: %w", err)
	}

	// Decode value from JSON
	if err := json.Unmarshal(b, &value); err != nil {
		return zero, time.Time{}, false, fmt.Errorf("unmarshal value: %w", err)
	}

	return value, e.Expiry, true, nil
}

// Set saves a value to Datastore.
func (s *Store[K, V]) Set(ctx context.Context, key K, value V, expiry time.Time) error {
	k := s.makeKey(key)

	// Encode value as JSON then base64
	b, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal value: %w", err)
	}

	e := entry{
		Value:     base64.StdEncoding.EncodeToString(b),
		Expiry:    expiry,
		UpdatedAt: time.Now(),
	}

	if _, err := s.client.Put(ctx, k, &e); err != nil {
		return fmt.Errorf("datastore put: %w", err)
	}

	return nil
}

// Delete removes a value from Datastore.
func (s *Store[K, V]) Delete(ctx context.Context, key K) error {
	if err := s.client.Delete(ctx, s.makeKey(key)); err != nil {
		return fmt.Errorf("datastore delete: %w", err)
	}
	return nil
}

// Cleanup removes expired entries from Datastore.
// maxAge specifies how old entries must be (based on expiry field) before deletion.
// If native Datastore TTL is properly configured, this will find no entries.
func (s *Store[K, V]) Cleanup(ctx context.Context, maxAge time.Duration) (int, error) {
	cutoff := time.Now().Add(-maxAge)

	// Query for entries with expiry before cutoff
	q := ds.NewQuery(s.kind).
		Filter("expiry >", time.Time{}).
		Filter("expiry <", cutoff).
		KeysOnly()

	keys, err := s.client.AllKeys(ctx, q)
	if err != nil {
		return 0, fmt.Errorf("query expired keys: %w", err)
	}

	if len(keys) == 0 {
		return 0, nil
	}

	if err := s.client.DeleteMulti(ctx, keys); err != nil {
		return 0, fmt.Errorf("delete expired entries: %w", err)
	}

	return len(keys), nil
}

// Flush removes all entries from Datastore.
// Returns the number of entries removed and any error.
func (s *Store[K, V]) Flush(ctx context.Context) (int, error) {
	q := ds.NewQuery(s.kind).KeysOnly()

	keys, err := s.client.AllKeys(ctx, q)
	if err != nil {
		return 0, fmt.Errorf("query all keys: %w", err)
	}

	if len(keys) == 0 {
		return 0, nil
	}

	if err := s.client.DeleteMulti(ctx, keys); err != nil {
		return 0, fmt.Errorf("delete all entries: %w", err)
	}

	return len(keys), nil
}

// Len returns the number of entries in Datastore.
func (s *Store[K, V]) Len(ctx context.Context) (int, error) {
	n, err := s.client.Count(ctx, ds.NewQuery(s.kind))
	if err != nil {
		return 0, fmt.Errorf("count entries: %w", err)
	}
	return n, nil
}

// Close releases Datastore client resources.
func (s *Store[K, V]) Close() error {
	return s.client.Close()
}
