// Package valkey provides Valkey/Redis persistence for sfcache.
package valkey

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/codeGROOVE-dev/sfcache"
	"github.com/valkey-io/valkey-go"
)

const (
	maxKeyLength = 512 // Maximum key length for Valkey
)

// store implements Store using Valkey/Redis.
type store[K comparable, V any] struct {
	client valkey.Client
	prefix string // Key prefix to namespace cache entries
}

// New creates a new Valkey-based persistence layer.
// The cacheID is used as a key prefix to namespace cache entries.
// addr should be in the format "host:port" (e.g., "localhost:6379").
func New[K comparable, V any](ctx context.Context, cacheID, addr string) (sfcache.Store[K, V], error) {
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

	return &store[K, V]{
		client: client,
		prefix: cacheID + ":",
	}, nil
}

// ValidateKey checks if a key is valid for Valkey persistence.
func (*store[K, V]) ValidateKey(key K) error {
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
func (p *store[K, V]) makeKey(key K) string {
	return p.prefix + fmt.Sprintf("%v", key)
}

// Location returns the Valkey key for a given cache key.
func (p *store[K, V]) Location(key K) string {
	return p.makeKey(key)
}

// Get retrieves a value from Valkey.
//
//nolint:revive,gocritic // function-result-limit, unnamedResult - required by persist.Store interface
func (p *store[K, V]) Get(ctx context.Context, key K) (V, time.Time, bool, error) {
	var zero V
	vk := p.makeKey(key)

	// Get value and TTL in a pipeline for efficiency
	cmds := []valkey.Completed{
		p.client.B().Get().Key(vk).Build(),
		p.client.B().Pttl().Key(vk).Build(),
	}

	resps := p.client.DoMulti(ctx, cmds...)

	// Check if key exists
	data, err := resps[0].ToString()
	if err != nil {
		if valkey.IsValkeyNil(err) {
			return zero, time.Time{}, false, nil
		}
		return zero, time.Time{}, false, fmt.Errorf("valkey get: %w", err)
	}

	// Parse value
	var value V
	if err := json.Unmarshal([]byte(data), &value); err != nil {
		return zero, time.Time{}, false, fmt.Errorf("unmarshal value: %w", err)
	}

	// Parse TTL
	var expiry time.Time
	ttlMs, err := resps[1].AsInt64()
	if err == nil && ttlMs > 0 {
		expiry = time.Now().Add(time.Duration(ttlMs) * time.Millisecond)
	}

	return value, expiry, true, nil
}

// Set saves a value to Valkey with optional expiry.
func (p *store[K, V]) Set(ctx context.Context, key K, value V, expiry time.Time) error {
	vk := p.makeKey(key)

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
		cmd = p.client.B().Set().Key(vk).Value(string(data)).Px(ttl).Build()
	} else {
		cmd = p.client.B().Set().Key(vk).Value(string(data)).Build()
	}

	if err := p.client.Do(ctx, cmd).Error(); err != nil {
		return fmt.Errorf("valkey set: %w", err)
	}

	return nil
}

// Delete removes a value from Valkey.
func (p *store[K, V]) Delete(ctx context.Context, key K) error {
	vk := p.makeKey(key)

	cmd := p.client.B().Del().Key(vk).Build()
	if err := p.client.Do(ctx, cmd).Error(); err != nil {
		return fmt.Errorf("valkey delete: %w", err)
	}

	return nil
}

// LoadRecent returns channels for streaming entries from Valkey.
// Uses SCAN to iterate over all keys with the cache prefix.
// If limit > 0, returns up to limit entries (not guaranteed to be most recent).
//
//nolint:gocritic // unnamedResult - channel returns are self-documenting
func (p *store[K, V]) LoadRecent(ctx context.Context, limit int) (<-chan sfcache.Entry[K, V], <-chan error) {
	entryCh := make(chan sfcache.Entry[K, V], 100)
	errCh := make(chan error, 1)

	go func() {
		defer close(entryCh)
		defer close(errCh)

		sent := 0
		pattern := p.prefix + "*"

		// Use SCAN to iterate over keys
		var cursor uint64
		for {
			// Check context cancellation
			select {
			case <-ctx.Done():
				errCh <- ctx.Err()
				return
			default:
			}

			// SCAN for keys
			cmd := p.client.B().Scan().Cursor(cursor).Match(pattern).Count(100).Build()
			resp := p.client.Do(ctx, cmd)

			scanResp, err := resp.AsScanEntry()
			if err != nil {
				errCh <- fmt.Errorf("scan keys: %w", err)
				return
			}

			// Load each key
			for _, vk := range scanResp.Elements {
				// Check limit
				if limit > 0 && sent >= limit {
					return
				}

				// Get value and TTL
				cmds := []valkey.Completed{
					p.client.B().Get().Key(vk).Build(),
					p.client.B().Pttl().Key(vk).Build(),
				}

				resps := p.client.DoMulti(ctx, cmds...)

				// Parse value
				data, err := resps[0].ToString()
				if err != nil {
					continue // Key was deleted or error
				}

				var value V
				if err := json.Unmarshal([]byte(data), &value); err != nil {
					continue
				}

				// Parse TTL
				var expiry time.Time
				ttlMs, err := resps[1].AsInt64()
				if err == nil && ttlMs > 0 {
					expiry = time.Now().Add(time.Duration(ttlMs) * time.Millisecond)
				}

				// Extract original key (remove prefix)
				ks := vk[len(p.prefix):]
				var key K
				if _, err := fmt.Sscanf(ks, "%v", &key); err != nil {
					// For string keys, try direct conversion
					sk, ok := any(ks).(K)
					if !ok {
						continue
					}
					key = sk
				}

				entryCh <- sfcache.Entry[K, V]{
					Key:       key,
					Value:     value,
					Expiry:    expiry,
					UpdatedAt: time.Now(), // Valkey doesn't track update time
				}
				sent++
			}

			// Check if we're done scanning
			cursor = scanResp.Cursor
			if cursor == 0 {
				break
			}
		}
	}()

	return entryCh, errCh
}

// Cleanup removes expired entries from Valkey.
// Valkey handles expiration automatically via TTL, so this is a no-op.
func (*store[K, V]) Cleanup(_ context.Context, _ time.Duration) (int, error) {
	// Valkey automatically handles TTL expiration
	return 0, nil
}

// Flush removes all entries with this cache's prefix from Valkey.
// Returns the number of entries removed and any error.
func (p *store[K, V]) Flush(ctx context.Context) (int, error) {
	n := 0
	pat := p.prefix + "*"
	var cursor uint64

	for {
		select {
		case <-ctx.Done():
			return n, ctx.Err()
		default:
		}

		cmd := p.client.B().Scan().Cursor(cursor).Match(pat).Count(100).Build()
		scan, err := p.client.Do(ctx, cmd).AsScanEntry()
		if err != nil {
			return n, fmt.Errorf("scan keys: %w", err)
		}

		if len(scan.Elements) > 0 {
			del := p.client.B().Del().Key(scan.Elements...).Build()
			if c, err := p.client.Do(ctx, del).AsInt64(); err == nil {
				n += int(c)
			}
		}

		cursor = scan.Cursor
		if cursor == 0 {
			break
		}
	}

	return n, nil
}

// Len returns the number of entries with this cache's prefix in Valkey.
func (p *store[K, V]) Len(ctx context.Context) (int, error) {
	n := 0
	pat := p.prefix + "*"
	var cursor uint64

	for {
		select {
		case <-ctx.Done():
			return n, ctx.Err()
		default:
		}

		cmd := p.client.B().Scan().Cursor(cursor).Match(pat).Count(100).Build()
		scan, err := p.client.Do(ctx, cmd).AsScanEntry()
		if err != nil {
			return n, fmt.Errorf("scan keys: %w", err)
		}

		n += len(scan.Elements)
		cursor = scan.Cursor
		if cursor == 0 {
			break
		}
	}

	return n, nil
}

// Close releases Valkey client resources.
func (p *store[K, V]) Close() error {
	p.client.Close()
	return nil
}
