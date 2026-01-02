// Package fido provides a high-performance cache with optional persistence.
package fido

import (
	"sync"
	"time"

	"github.com/puzpuzpuz/xsync/v4"
)

// calculateExpiry returns the expiry time for a given TTL, falling back to defaultTTL.
// Returns zero Time (no expiry) if both TTL and defaultTTL are zero or negative.
func calculateExpiry(ttl, defaultTTL time.Duration) time.Time {
	if ttl <= 0 {
		ttl = defaultTTL
	}
	if ttl <= 0 {
		return time.Time{}
	}
	return time.Now().Add(ttl)
}

// Cache is an in-memory cache. All operations are synchronous and infallible.
type Cache[K comparable, V any] struct {
	flights    *xsync.Map[K, *flightCall[V]]
	memory     *s3fifo[K, V]
	defaultTTL time.Duration
	noExpiry   bool
}

// flightCall holds an in-flight computation for singleflight deduplication.
//
//nolint:govet // fieldalignment: semantic grouping preferred
type flightCall[V any] struct {
	wg  sync.WaitGroup
	val V
	err error
}

// New creates an in-memory cache.
func New[K comparable, V any](opts ...Option) *Cache[K, V] {
	cfg := defaultConfig()
	for _, opt := range opts {
		opt(cfg)
	}

	return &Cache[K, V]{
		flights:    xsync.NewMap[K, *flightCall[V]](),
		memory:     newS3FIFO[K, V](cfg),
		defaultTTL: cfg.defaultTTL,
		noExpiry:   cfg.defaultTTL == 0,
	}
}

// Get returns the value for key, or zero and false if not found.
func (c *Cache[K, V]) Get(key K) (V, bool) {
	return c.memory.get(key)
}

// Set stores a value using the default TTL specified at cache creation.
// If no default TTL was set, the entry never expires.
func (c *Cache[K, V]) Set(key K, value V) {
	if c.noExpiry {
		c.memory.set(key, value, 0)
		return
	}
	c.memory.set(key, value, timeToSec(c.expiry(0)))
}

// SetTTL stores a value with an explicit TTL.
// A zero or negative TTL means the entry never expires.
func (c *Cache[K, V]) SetTTL(key K, value V, ttl time.Duration) {
	if ttl <= 0 {
		c.memory.set(key, value, 0)
		return
	}
	//nolint:gosec // G115: Unix seconds fit in uint32 until year 2106
	c.memory.set(key, value, uint32(time.Now().Add(ttl).Unix()))
}

// Delete removes a key from the cache.
func (c *Cache[K, V]) Delete(key K) {
	c.memory.del(key)
}

// GetSet returns cached value or calls loader to compute it.
// Concurrent calls for the same key share one loader invocation.
// Computed values are stored with the default TTL.
func (c *Cache[K, V]) GetSet(key K, loader func() (V, error)) (V, error) {
	return c.getSet(key, loader, 0)
}

// GetSetTTL is like GetSet but stores computed values with an explicit TTL.
func (c *Cache[K, V]) GetSetTTL(key K, loader func() (V, error), ttl time.Duration) (V, error) {
	return c.getSet(key, loader, ttl)
}

func (c *Cache[K, V]) getSet(key K, loader func() (V, error), ttl time.Duration) (V, error) {
	if val, ok := c.memory.get(key); ok {
		return val, nil
	}

	call, loaded := c.flights.LoadOrCompute(key, func() (*flightCall[V], bool) {
		fc := &flightCall[V]{}
		fc.wg.Add(1)
		return fc, false
	})

	if loaded {
		call.wg.Wait()
		return call.val, call.err
	}

	if val, ok := c.memory.get(key); ok {
		call.val = val
		c.flights.Delete(key)
		call.wg.Done()
		return val, nil
	}

	val, err := loader()
	if err == nil {
		if ttl <= 0 {
			c.Set(key, val)
		} else {
			c.SetTTL(key, val, ttl)
		}
	}

	call.val, call.err = val, err
	c.flights.Delete(key)
	call.wg.Done()

	return val, err
}

// Len returns the number of entries.
func (c *Cache[K, V]) Len() int {
	return c.memory.len()
}

// Flush removes all entries. Returns count removed.
func (c *Cache[K, V]) Flush() int {
	return c.memory.flush()
}

func (c *Cache[K, V]) expiry(ttl time.Duration) time.Time {
	return calculateExpiry(ttl, c.defaultTTL)
}

type config struct {
	size       int
	defaultTTL time.Duration
}

func defaultConfig() *config {
	return &config{size: 16384}
}

// Option configures a Cache.
type Option func(*config)

// Size sets maximum entries. Default 16384.
func Size(n int) Option {
	return func(c *config) { c.size = n }
}

// TTL sets default expiration. Default 0 (none).
func TTL(d time.Duration) Option {
	return func(c *config) { c.defaultTTL = d }
}
