// Package valkey provides Valkey/Redis persistence for sfcache.
package valkey

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/valkey-io/valkey-go"
)

const (
	maxKeyLength = 512 // Maximum key length for Valkey
)

// Store implements persistence using Valkey/Redis.
type Store[K comparable, V any] struct {
	client valkey.Client
	prefix string // Key prefix to namespace cache entries
}

// New creates a new Valkey-based persistence layer.
// The cacheID is used as a key prefix to namespace cache entries.
// addr should be in the format "host:port" (e.g., "localhost:6379").
func New[K comparable, V any](ctx context.Context, cacheID, addr string) (*Store[K, V], error) {
	if cacheID == "" {
		return nil, errors.New("cacheID cannot be empty")
	}
	if addr == "" {
		addr = "localhost:6379"
	}

	// Create Valkey client
	client, err := valkey.NewClient(valkey.ClientOption{
		InitAddress: []string{addr},
	})
	if err != nil {
		return nil, fmt.Errorf("create valkey client: %w", err)
	}

	// Verify connectivity with PING
	if err := client.Do(ctx, client.B().Ping().Build()).Error(); err != nil {
		client.Close()
		return nil, fmt.Errorf("valkey ping failed: %w", err)
	}

	return &Store[K, V]{
		client: client,
		prefix: cacheID + ":",
	}, nil
}

// ValidateKey checks if a key is valid for Valkey persistence.
func (*Store[K, V]) ValidateKey(key K) error {
	k := fmt.Sprintf("%v", key)
	if len(k) > maxKeyLength {
		return fmt.Errorf("key too long: %d bytes (max %d)", len(k), maxKeyLength)
	}
	if k == "" {
		return errors.New("key cannot be empty")
	}
	return nil
}

// makeKey creates a Valkey key from a cache key with prefix.
func (s *Store[K, V]) makeKey(key K) string {
	return s.prefix + fmt.Sprintf("%v", key)
}

// Location returns the Valkey key for a given cache key.
func (s *Store[K, V]) Location(key K) string {
	return s.makeKey(key)
}

// Get retrieves a value from Valkey.
//
//nolint:revive,gocritic // function-result-limit, unnamedResult - required by persist.Store interface
func (s *Store[K, V]) Get(ctx context.Context, key K) (V, time.Time, bool, error) {
	var zero V
	k := s.makeKey(key)

	// Get value and TTL in a pipeline for efficiency
	cmds := []valkey.Completed{
		s.client.B().Get().Key(k).Build(),
		s.client.B().Pttl().Key(k).Build(),
	}

	resps := s.client.DoMulti(ctx, cmds...)

	// Check if key exists
	data, err := resps[0].ToString()
	if err != nil {
		if valkey.IsValkeyNil(err) {
			return zero, time.Time{}, false, nil
		}
		return zero, time.Time{}, false, fmt.Errorf("valkey get: %w", err)
	}

	// Parse value
	var v V
	if err := json.Unmarshal([]byte(data), &v); err != nil {
		return zero, time.Time{}, false, fmt.Errorf("unmarshal value: %w", err)
	}

	// Parse TTL
	var exp time.Time
	ms, err := resps[1].AsInt64()
	if err == nil && ms > 0 {
		exp = time.Now().Add(time.Duration(ms) * time.Millisecond)
	}

	return v, exp, true, nil
}

// Set saves a value to Valkey with optional expiry.
func (s *Store[K, V]) Set(ctx context.Context, key K, value V, expiry time.Time) error {
	k := s.makeKey(key)

	// Marshal value to JSON
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal value: %w", err)
	}

	// Calculate TTL
	var ttl time.Duration
	if !expiry.IsZero() {
		ttl = time.Until(expiry)
		if ttl <= 0 {
			// Already expired, don't store
			return nil
		}
	}

	// Store with TTL if specified
	var cmd valkey.Completed
	if ttl > 0 {
		cmd = s.client.B().Set().Key(k).Value(string(data)).Px(ttl).Build()
	} else {
		cmd = s.client.B().Set().Key(k).Value(string(data)).Build()
	}

	if err := s.client.Do(ctx, cmd).Error(); err != nil {
		return fmt.Errorf("valkey set: %w", err)
	}

	return nil
}

// Delete removes a value from Valkey.
func (s *Store[K, V]) Delete(ctx context.Context, key K) error {
	k := s.makeKey(key)
	if err := s.client.Do(ctx, s.client.B().Del().Key(k).Build()).Error(); err != nil {
		return fmt.Errorf("valkey delete: %w", err)
	}
	return nil
}

// Cleanup removes expired entries from Valkey.
// Valkey handles expiration automatically via TTL, so this is a no-op.
func (*Store[K, V]) Cleanup(_ context.Context, _ time.Duration) (int, error) {
	// Valkey automatically handles TTL expiration
	return 0, nil
}

// Flush removes all entries with this cache's prefix from Valkey.
// Returns the number of entries removed and any error.
func (s *Store[K, V]) Flush(ctx context.Context) (int, error) {
	n := 0
	pat := s.prefix + "*"
	var cur uint64

	for {
		select {
		case <-ctx.Done():
			return n, ctx.Err()
		default:
		}

		scan, err := s.client.Do(ctx, s.client.B().Scan().Cursor(cur).Match(pat).Count(100).Build()).AsScanEntry()
		if err != nil {
			return n, fmt.Errorf("scan keys: %w", err)
		}

		if len(scan.Elements) > 0 {
			if c, err := s.client.Do(ctx, s.client.B().Del().Key(scan.Elements...).Build()).AsInt64(); err == nil {
				n += int(c)
			}
		}

		cur = scan.Cursor
		if cur == 0 {
			break
		}
	}

	return n, nil
}

// Len returns the number of entries with this cache's prefix in Valkey.
func (s *Store[K, V]) Len(ctx context.Context) (int, error) {
	n := 0
	pat := s.prefix + "*"
	var cur uint64

	for {
		select {
		case <-ctx.Done():
			return n, ctx.Err()
		default:
		}

		scan, err := s.client.Do(ctx, s.client.B().Scan().Cursor(cur).Match(pat).Count(100).Build()).AsScanEntry()
		if err != nil {
			return n, fmt.Errorf("scan keys: %w", err)
		}

		n += len(scan.Elements)
		cur = scan.Cursor
		if cur == 0 {
			break
		}
	}

	return n, nil
}

// Close releases Valkey client resources.
func (s *Store[K, V]) Close() error {
	s.client.Close()
	return nil
}
