// Package sfcache provides a high-performance cache with S3-FIFO eviction and optional persistence.
package sfcache

import (
	"sync"
	"time"
)

const numFlightShards = 256

// MemoryCache is a fast in-memory cache without persistence.
// All operations are context-free and never return errors.
type MemoryCache[K comparable, V any] struct {
	flights    [numFlightShards]flightGroup[K, V]
	memory     *s3fifo[K, V]
	defaultTTL time.Duration
}

// flightGroup prevents thundering herd for a set of keys.
// Unlike singleflight, it uses the key type directly to avoid string conversion.
//
//nolint:govet // fieldalignment: semantic grouping preferred
type flightGroup[K comparable, V any] struct {
	mu    sync.Mutex
	calls map[K]*flightCall[V]
}

//nolint:govet // fieldalignment: semantic grouping preferred
type flightCall[V any] struct {
	wg  sync.WaitGroup
	val V
	err error
}

// do executes fn once per key, with concurrent callers waiting for the result.
func (g *flightGroup[K, V]) do(key K, fn func() (V, error)) (V, error) {
	g.mu.Lock()
	if g.calls == nil {
		g.calls = make(map[K]*flightCall[V])
	}
	if c, ok := g.calls[key]; ok {
		g.mu.Unlock()
		c.wg.Wait()
		return c.val, c.err
	}
	c := &flightCall[V]{}
	c.wg.Add(1)
	g.calls[key] = c
	g.mu.Unlock()

	c.val, c.err = fn()

	g.mu.Lock()
	delete(g.calls, key)
	g.mu.Unlock()
	c.wg.Done()

	return c.val, c.err
}

// New creates a new in-memory cache.
//
// Example:
//
//	cache := sfcache.New[string, User](
//	    sfcache.Size(10000),
//	    sfcache.TTL(time.Hour),
//	)
//	defer cache.Close()
//
//	cache.Set("user:123", user)              // uses default TTL
//	cache.Set("user:123", user, time.Hour)   // explicit TTL
//	user, ok := cache.Get("user:123")
func New[K comparable, V any](opts ...Option) *MemoryCache[K, V] {
	cfg := defaultConfig()
	for _, opt := range opts {
		opt(cfg)
	}

	return &MemoryCache[K, V]{
		memory:     newS3FIFO[K, V](cfg),
		defaultTTL: cfg.defaultTTL,
	}
}

// Get retrieves a value from the cache.
// Returns the value and true if found, or the zero value and false if not found.
func (c *MemoryCache[K, V]) Get(key K) (V, bool) {
	return c.memory.get(key)
}

// Set stores a value in the cache.
// If no TTL is provided, the default TTL is used.
// If no default TTL is configured, the entry never expires.
func (c *MemoryCache[K, V]) Set(key K, value V, ttl ...time.Duration) {
	var t time.Duration
	if len(ttl) > 0 {
		t = ttl[0]
	}
	c.memory.set(key, value, timeToNano(c.expiry(t)))
}

// Delete removes a value from the cache.
func (c *MemoryCache[K, V]) Delete(key K) {
	c.memory.del(key)
}

// GetSet retrieves a value from the cache, or calls loader to compute and store it.
// This prevents thundering herd: if multiple goroutines request the same missing key
// concurrently, only one loader runs while others wait for its result.
//
// Example:
//
//	user, err := cache.GetSet("user:123", func() (User, error) {
//	    return fetchUserFromDB("123")
//	})
//
//	// Or with explicit TTL:
//	user, err := cache.GetSet("user:123", func() (User, error) {
//	    return fetchUserFromDB("123")
//	}, time.Hour)
func (c *MemoryCache[K, V]) GetSet(key K, loader func() (V, error), ttl ...time.Duration) (V, error) {
	// Fast path: check cache first
	if val, ok := c.memory.get(key); ok {
		return val, nil
	}

	// Slow path: use flight group to ensure only one loader runs per key
	idx := flightShard(key)
	return c.flights[idx].do(key, func() (V, error) {
		// Double-check: another goroutine may have populated the cache
		if val, ok := c.memory.get(key); ok {
			return val, nil
		}

		// Call loader
		val, err := loader()
		if err != nil {
			return val, err
		}

		// Store in cache
		var t time.Duration
		if len(ttl) > 0 {
			t = ttl[0]
		}
		c.memory.set(key, val, timeToNano(c.expiry(t)))

		return val, nil
	})
}

// flightShard returns a shard index for a key.
// Uses fast paths for common types.
func flightShard[K comparable](key K) int {
	switch k := any(key).(type) {
	case int:
		return k & (numFlightShards - 1)
	case int64:
		return int(k) & (numFlightShards - 1)
	case uint64:
		return int(k) & (numFlightShards - 1) //nolint:gosec // result is always 0-255
	case string:
		return int(wyhashString(k)) & (numFlightShards - 1) //nolint:gosec // result is always 0-255
	default:
		return 0
	}
}

// Len returns the number of entries in the cache.
func (c *MemoryCache[K, V]) Len() int {
	return c.memory.len()
}

// Flush removes all entries from the cache.
// Returns the number of entries removed.
func (c *MemoryCache[K, V]) Flush() int {
	return c.memory.flush()
}

// Close releases resources held by the cache.
// For MemoryCache this is a no-op, but provided for API consistency.
func (*MemoryCache[K, V]) Close() {
	// No-op for memory-only cache
}

// expiry returns the expiry time based on TTL and default TTL.
func (c *MemoryCache[K, V]) expiry(ttl time.Duration) time.Time {
	if ttl <= 0 {
		ttl = c.defaultTTL
	}
	if ttl <= 0 {
		return time.Time{}
	}
	return time.Now().Add(ttl)
}

// config holds configuration for both MemoryCache and TieredCache.
type config struct {
	size       int
	defaultTTL time.Duration
}

func defaultConfig() *config {
	return &config{
		size: 16384, // 2^14, divides evenly by numShards
	}
}

// Option configures a MemoryCache or TieredCache.
type Option func(*config)

// Size sets the maximum number of entries in the memory cache.
// Default is 16384.
func Size(n int) Option {
	return func(c *config) {
		c.size = n
	}
}

// TTL sets the default TTL for cache entries.
// Entries without an explicit TTL will use this value.
// Default is 0 (no expiration).
func TTL(d time.Duration) Option {
	return func(c *config) {
		c.defaultTTL = d
	}
}
