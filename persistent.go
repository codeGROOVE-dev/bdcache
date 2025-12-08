package sfcache

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// TieredCache is a cache with an in-memory layer backed by persistent storage.
// The memory layer provides fast access, while the store provides durability.
// Core operations require context for I/O, while memory operations like Len() do not.
type TieredCache[K comparable, V any] struct {
	// Store provides direct access to the persistence layer.
	// Use this for persistence-specific operations:
	//   cache.Store.Len(ctx)
	//   cache.Store.Flush(ctx)
	//   cache.Store.Cleanup(ctx, maxAge)
	Store Store[K, V]

	flights    [numFlightShards]flightGroup[K, V]
	memory     *s3fifo[K, V]
	defaultTTL time.Duration
}

// NewTiered creates a cache with an in-memory layer backed by persistent storage.
//
// Example:
//
//	store, _ := localfs.New[string, User]("myapp", "")
//	cache, err := sfcache.NewTiered[string, User](store,
//	    sfcache.Size(10000),
//	    sfcache.TTL(time.Hour),
//	)
//	if err != nil {
//	    return err
//	}
//	defer cache.Close()
//
//	cache.Set(ctx, "user:123", user)              // uses default TTL
//	cache.Set(ctx, "user:123", user, time.Hour)   // explicit TTL
//	user, ok, err := cache.Get(ctx, "user:123")
//	storeCount, _ := cache.Store.Len(ctx)
func NewTiered[K comparable, V any](store Store[K, V], opts ...Option) (*TieredCache[K, V], error) {
	cfg := defaultConfig()
	for _, opt := range opts {
		opt(cfg)
	}

	if store == nil {
		return nil, errors.New("store cannot be nil")
	}

	cache := &TieredCache[K, V]{
		Store:      store,
		memory:     newS3FIFO[K, V](cfg),
		defaultTTL: cfg.defaultTTL,
	}

	return cache, nil
}

// Get retrieves a value from the cache.
// It first checks the memory cache, then falls back to persistence.
//
//nolint:gocritic // unnamedResult - public API signature is intentionally clear without named returns
func (c *TieredCache[K, V]) Get(ctx context.Context, key K) (V, bool, error) {
	// Check memory first
	if val, ok := c.memory.get(key); ok {
		return val, true, nil
	}

	var zero V

	// Validate key before accessing persistence (security: prevent path traversal)
	if err := c.Store.ValidateKey(key); err != nil {
		return zero, false, fmt.Errorf("invalid key: %w", err)
	}

	// Check persistence
	val, expiry, found, err := c.Store.Get(ctx, key)
	if err != nil {
		return zero, false, fmt.Errorf("persistence load: %w", err)
	}

	if !found {
		return zero, false, nil
	}

	// Add to memory cache for future hits
	c.memory.set(key, val, timeToNano(expiry))

	return val, true, nil
}

// expiry returns the expiry time based on TTL and default TTL.
func (c *TieredCache[K, V]) expiry(ttl time.Duration) time.Time {
	if ttl <= 0 {
		ttl = c.defaultTTL
	}
	if ttl <= 0 {
		return time.Time{}
	}
	return time.Now().Add(ttl)
}

// Set stores a value in the cache.
// If no TTL is provided, the default TTL is used.
// The value is ALWAYS stored in memory, even if persistence fails.
// Returns an error if the key violates persistence constraints or if persistence fails.
func (c *TieredCache[K, V]) Set(ctx context.Context, key K, value V, ttl ...time.Duration) error {
	var t time.Duration
	if len(ttl) > 0 {
		t = ttl[0]
	}
	expiry := c.expiry(t)

	// Validate key early
	if err := c.Store.ValidateKey(key); err != nil {
		return err
	}

	// ALWAYS update memory first - reliability guarantee
	c.memory.set(key, value, timeToNano(expiry))

	// Update persistence
	if err := c.Store.Set(ctx, key, value, expiry); err != nil {
		return fmt.Errorf("persistence store failed: %w", err)
	}

	return nil
}

// SetAsync stores a value in the cache, handling persistence asynchronously.
// If no TTL is provided, the default TTL is used.
// Key validation and in-memory caching happen synchronously.
// Persistence errors are logged but not returned (fire-and-forget).
// Returns an error only for validation failures (e.g., invalid key format).
func (c *TieredCache[K, V]) SetAsync(ctx context.Context, key K, value V, ttl ...time.Duration) error {
	var t time.Duration
	if len(ttl) > 0 {
		t = ttl[0]
	}
	expiry := c.expiry(t)

	// Validate key early (synchronous)
	if err := c.Store.ValidateKey(key); err != nil {
		return err
	}

	// ALWAYS update memory first - reliability guarantee (synchronous)
	c.memory.set(key, value, timeToNano(expiry))

	// Update persistence asynchronously (fire-and-forget)
	//nolint:contextcheck // Intentionally detached - persistence should complete even if caller cancels
	go func() {
		storeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := c.Store.Set(storeCtx, key, value, expiry); err != nil {
			slog.Error("async persistence failed", "key", key, "error", err)
		}
	}()

	return nil
}

// GetSet retrieves a value from the cache (memory or persistence), or calls loader to compute and store it.
// This prevents thundering herd: if multiple goroutines request the same missing key
// concurrently, only one loader runs while others wait for its result.
//
// Example:
//
//	user, err := cache.GetSet(ctx, "user:123", func(ctx context.Context) (User, error) {
//	    return fetchUserFromAPI(ctx, "123")
//	})
//
//	// Or with explicit TTL:
//	user, err := cache.GetSet(ctx, "user:123", func(ctx context.Context) (User, error) {
//	    return fetchUserFromAPI(ctx, "123")
//	}, time.Hour)
func (c *TieredCache[K, V]) GetSet(ctx context.Context, key K, loader func(context.Context) (V, error), ttl ...time.Duration) (V, error) {
	var zero V

	// Fast path: check memory cache first
	if val, ok := c.memory.get(key); ok {
		return val, nil
	}

	// Validate key before any persistence operations
	if err := c.Store.ValidateKey(key); err != nil {
		return zero, fmt.Errorf("invalid key: %w", err)
	}

	// Check persistence
	val, expiry, found, err := c.Store.Get(ctx, key)
	if err != nil {
		return zero, fmt.Errorf("persistence load: %w", err)
	}
	if found {
		// Add to memory cache for future hits
		c.memory.set(key, val, timeToNano(expiry))
		return val, nil
	}

	// Slow path: use flight group to ensure only one loader runs per key
	idx := flightShard(key)
	return c.flights[idx].do(key, func() (V, error) {
		// Double-check memory (another goroutine may have populated it)
		if val, ok := c.memory.get(key); ok {
			return val, nil
		}

		// Double-check persistence
		val, expiry, found, err := c.Store.Get(ctx, key)
		if err != nil {
			var zero V
			return zero, fmt.Errorf("persistence load: %w", err)
		}
		if found {
			c.memory.set(key, val, timeToNano(expiry))
			return val, nil
		}

		// Call loader
		val, err = loader(ctx)
		if err != nil {
			var zero V
			return zero, err
		}

		// Store in both memory and persistence
		var t time.Duration
		if len(ttl) > 0 {
			t = ttl[0]
		}
		exp := c.expiry(t)
		c.memory.set(key, val, timeToNano(exp))

		if err := c.Store.Set(ctx, key, val, exp); err != nil {
			slog.Warn("GetSet persistence failed", "key", key, "error", err)
			// Return the value anyway - memory cache has it
		}

		return val, nil
	})
}

// Delete removes a value from the cache.
// The value is always removed from memory. Returns an error if persistence deletion fails.
func (c *TieredCache[K, V]) Delete(ctx context.Context, key K) error {
	// Remove from memory first (always succeeds)
	c.memory.del(key)

	// Validate key before accessing persistence (security: prevent path traversal)
	if err := c.Store.ValidateKey(key); err != nil {
		return fmt.Errorf("invalid key: %w", err)
	}

	if err := c.Store.Delete(ctx, key); err != nil {
		return fmt.Errorf("persistence delete: %w", err)
	}

	return nil
}

// Flush removes all entries from the cache, including persistent storage.
// Returns the total number of entries removed from memory and persistence.
func (c *TieredCache[K, V]) Flush(ctx context.Context) (int, error) {
	memoryRemoved := c.memory.flush()

	persistRemoved, err := c.Store.Flush(ctx)
	if err != nil {
		return memoryRemoved, fmt.Errorf("persistence flush: %w", err)
	}

	return memoryRemoved + persistRemoved, nil
}

// Len returns the number of entries in the memory cache.
// For persistence entry count, use cache.Store.Len(ctx).
func (c *TieredCache[K, V]) Len() int {
	return c.memory.len()
}

// Close releases resources held by the cache.
func (c *TieredCache[K, V]) Close() error {
	if err := c.Store.Close(); err != nil {
		return fmt.Errorf("close persistence: %w", err)
	}
	return nil
}
