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
	"github.com/codeGROOVE-dev/sfcache"
)

const (
	datastoreKind      = "CacheEntry"
	maxDatastoreKeyLen = 1500 // Datastore has stricter key length limits
)

// store implements Store using Google Cloud Datastore.
type store[K comparable, V any] struct {
	client *ds.Client
	kind   string
}

// ValidateKey checks if a key is valid for Datastore persistence.
// Datastore has stricter key length limits than files.
func (*store[K, V]) ValidateKey(key K) error {
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
func (p *store[K, V]) Location(key K) string {
	return fmt.Sprintf("%s/%v", p.kind, key)
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
func New[K comparable, V any](ctx context.Context, cacheID string) (sfcache.Store[K, V], error) {
	// Empty project ID lets ds9 auto-detect
	client, err := ds.NewClientWithDatabase(ctx, "", cacheID)
	if err != nil {
		return nil, fmt.Errorf("create datastore client: %w", err)
	}

	// Verify connectivity (assert readiness)
	// Note: ds9 doesn't expose Ping, but client creation validates connectivity

	return &store[K, V]{
		client: client,
		kind:   datastoreKind,
	}, nil
}

// makeKey creates a Datastore key from a cache key.
// We use the string representation directly as the key name.
func (p *store[K, V]) makeKey(key K) *ds.Key {
	return ds.NameKey(p.kind, fmt.Sprintf("%v", key), nil)
}

// Get retrieves a value from Datastore.
//
//nolint:revive // function-result-limit - required by persist.Store interface
func (p *store[K, V]) Get(ctx context.Context, key K) (value V, expiry time.Time, found bool, err error) {
	var zero V
	dsKey := p.makeKey(key)

	var e entry
	if err := p.client.Get(ctx, dsKey, &e); err != nil {
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
	vb, err := base64.StdEncoding.DecodeString(e.Value)
	if err != nil {
		return zero, time.Time{}, false, fmt.Errorf("decode base64: %w", err)
	}

	// Decode value from JSON
	if err := json.Unmarshal(vb, &value); err != nil {
		return zero, time.Time{}, false, fmt.Errorf("unmarshal value: %w", err)
	}

	return value, e.Expiry, true, nil
}

// Set saves a value to Datastore.
func (p *store[K, V]) Set(ctx context.Context, key K, value V, expiry time.Time) error {
	dsKey := p.makeKey(key)

	// Encode value as JSON then base64
	vb, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal value: %w", err)
	}
	vs := base64.StdEncoding.EncodeToString(vb)

	e := entry{
		Value:     vs,
		Expiry:    expiry,
		UpdatedAt: time.Now(),
	}

	if _, err := p.client.Put(ctx, dsKey, &e); err != nil {
		return fmt.Errorf("datastore put: %w", err)
	}

	return nil
}

// Delete removes a value from Datastore.
func (p *store[K, V]) Delete(ctx context.Context, key K) error {
	dsKey := p.makeKey(key)

	if err := p.client.Delete(ctx, dsKey); err != nil {
		return fmt.Errorf("datastore delete: %w", err)
	}

	return nil
}

// LoadRecent streams entries from Datastore, returning up to 'limit' most recently updated entries.
func (p *store[K, V]) LoadRecent(ctx context.Context, limit int) (entries <-chan sfcache.Entry[K, V], errs <-chan error) {
	entryCh := make(chan sfcache.Entry[K, V], 100)
	errCh := make(chan error, 1)

	go func() {
		defer close(entryCh)
		defer close(errCh)

		// Query ordered by UpdatedAt descending, limited
		query := ds.NewQuery(p.kind).Order("-updated_at")
		if limit > 0 {
			query = query.Limit(limit)
		}

		iter := p.client.Run(ctx, query)
		now := time.Now()

		for {
			var e entry
			dsKey, err := iter.Next(&e)
			if errors.Is(err, ds.Done) {
				break
			}
			if err != nil {
				errCh <- fmt.Errorf("query next: %w", err)
				return
			}

			// Check context cancellation
			select {
			case <-ctx.Done():
				errCh <- ctx.Err()
				return
			default:
			}

			// Skip expired entries - cleanup is handled by native TTL or periodic Cleanup() calls
			if !e.Expiry.IsZero() && now.After(e.Expiry) {
				continue
			}

			// Extract key from Datastore entity key name
			// We need to parse the key string back to type K
			// For now, we'll use fmt.Sscanf for simple types
			var key K
			ks := dsKey.Name
			if _, err := fmt.Sscanf(ks, "%v", &key); err != nil {
				// If Sscanf fails, try direct type assertion for string keys
				sk, ok := any(ks).(K)
				if !ok {
					continue
				}
				key = sk
			}

			// Decode value from base64
			vb, err := base64.StdEncoding.DecodeString(e.Value)
			if err != nil {
				continue
			}

			var value V
			if err := json.Unmarshal(vb, &value); err != nil {
				continue
			}

			entryCh <- sfcache.Entry[K, V]{
				Key:       key,
				Value:     value,
				Expiry:    e.Expiry,
				UpdatedAt: e.UpdatedAt,
			}
		}
	}()

	return entryCh, errCh
}

// Cleanup removes expired entries from Datastore.
// maxAge specifies how old entries must be (based on expiry field) before deletion.
// If native Datastore TTL is properly configured, this will find no entries.
func (p *store[K, V]) Cleanup(ctx context.Context, maxAge time.Duration) (int, error) {
	cutoff := time.Now().Add(-maxAge)

	// Query for entries with expiry before cutoff
	// This finds entries that should have expired based on maxAge
	query := ds.NewQuery(p.kind).
		Filter("expiry >", time.Time{}).
		Filter("expiry <", cutoff).
		KeysOnly()

	keys, err := p.client.AllKeys(ctx, query)
	if err != nil {
		return 0, fmt.Errorf("query expired keys: %w", err)
	}

	if len(keys) == 0 {
		return 0, nil
	}

	// Batch delete expired entries
	if err := p.client.DeleteMulti(ctx, keys); err != nil {
		return 0, fmt.Errorf("delete expired entries: %w", err)
	}

	return len(keys), nil
}

// Flush removes all entries from Datastore.
// Returns the number of entries removed and any error.
func (p *store[K, V]) Flush(ctx context.Context) (int, error) {
	// Query for all keys
	query := ds.NewQuery(p.kind).KeysOnly()

	keys, err := p.client.AllKeys(ctx, query)
	if err != nil {
		return 0, fmt.Errorf("query all keys: %w", err)
	}

	if len(keys) == 0 {
		return 0, nil
	}

	// Batch delete all entries
	if err := p.client.DeleteMulti(ctx, keys); err != nil {
		return 0, fmt.Errorf("delete all entries: %w", err)
	}

	return len(keys), nil
}

// Len returns the number of entries in Datastore.
func (p *store[K, V]) Len(ctx context.Context) (int, error) {
	query := ds.NewQuery(p.kind)
	count, err := p.client.Count(ctx, query)
	if err != nil {
		return 0, fmt.Errorf("count entries: %w", err)
	}
	return count, nil
}

// Close releases Datastore client resources.
func (p *store[K, V]) Close() error {
	return p.client.Close()
}
