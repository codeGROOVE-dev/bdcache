package bdcache

import (
	"context"
	"fmt"
	"time"
)

// PersistentCache is a cache backed by both memory and persistent storage.
// Core operations require context for I/O, while memory operations like Len() do not.
type PersistentCache[K comparable, V any] struct {
	// Store provides direct access to the persistence layer.
	// Use this for persistence-specific operations:
	//   cache.Store.Len(ctx)
	//   cache.Store.Flush(ctx)
	//   cache.Store.Cleanup(ctx, maxAge)
	Store PersistenceLayer[K, V]

	memory     *s3fifo[K, V]
	defaultTTL time.Duration
	warmup     int
}

// Persistent creates a cache with persistence backing.
//
// Example:
//
//	store, _ := localfs.New[string, User]("myapp", "")
//	cache, err := bdcache.Persistent[string, User](ctx, store,
//	    bdcache.WithSize(10000),
//	    bdcache.WithTTL(time.Hour),
//	    bdcache.WithWarmup(1000),
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
func Persistent[K comparable, V any](ctx context.Context, p PersistenceLayer[K, V], opts ...Option) (*PersistentCache[K, V], error) {
	cfg := defaultConfig()
	for _, opt := range opts {
		opt(cfg)
	}

	cache := &PersistentCache[K, V]{
		Store:      p,
		memory:     newS3FIFO[K, V](cfg.size),
		defaultTTL: cfg.defaultTTL,
		warmup:     cfg.warmup,
	}

	// Warm up cache from persistence if configured
	if cfg.warmup > 0 {
		//nolint:contextcheck // Background warmup uses detached context to complete independently
		go func() {
			// Create detached context with timeout - warmup should complete independently
			bgCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()

			cache.doWarmup(bgCtx)
		}()
	}

	return cache, nil
}

// doWarmup loads entries from persistence into memory cache.
// Errors are silently ignored since warmup is best-effort.
func (c *PersistentCache[K, V]) doWarmup(ctx context.Context) {
	entryCh, errCh := c.Store.LoadRecent(ctx, c.warmup)

	for entry := range entryCh {
		c.memory.setToMemory(entry.Key, entry.Value, timeToNano(entry.Expiry))
	}

	// Drain error channel (errors silently ignored for best-effort warmup)
	<-errCh
}

// Get retrieves a value from the cache.
// It first checks the memory cache, then falls back to persistence.
//
//nolint:gocritic // unnamedResult - public API signature is intentionally clear without named returns
func (c *PersistentCache[K, V]) Get(ctx context.Context, key K) (V, bool, error) {
	// Check memory first
	if val, ok := c.memory.getFromMemory(key); ok {
		return val, true, nil
	}

	var zero V

	// Validate key before accessing persistence (security: prevent path traversal)
	if err := c.Store.ValidateKey(key); err != nil {
		return zero, false, fmt.Errorf("invalid key: %w", err)
	}

	// Check persistence
	val, expiry, found, err := c.Store.Load(ctx, key)
	if err != nil {
		return zero, false, fmt.Errorf("persistence load: %w", err)
	}

	if !found {
		return zero, false, nil
	}

	// Add to memory cache for future hits
	c.memory.setToMemory(key, val, timeToNano(expiry))

	return val, true, nil
}

// GetOrSet retrieves a value from the cache, or computes and stores it if not found.
// The loader function is only called if the key is not in the cache.
// If no TTL is provided, the default TTL is used.
// If the loader returns an error, it is propagated.
func (c *PersistentCache[K, V]) GetOrSet(ctx context.Context, key K, loader func(context.Context) (V, error), ttl ...time.Duration) (V, error) {
	val, ok, err := c.Get(ctx, key)
	if err != nil {
		var zero V
		return zero, err
	}
	if ok {
		return val, nil
	}

	val, err = loader(ctx)
	if err != nil {
		var zero V
		return zero, err
	}

	if err := c.Set(ctx, key, val, ttl...); err != nil {
		return val, err
	}
	return val, nil
}

// expiry returns the expiry time based on TTL and default TTL.
func (c *PersistentCache[K, V]) expiry(ttl time.Duration) time.Time {
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
func (c *PersistentCache[K, V]) Set(ctx context.Context, key K, value V, ttl ...time.Duration) error {
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
	c.memory.setToMemory(key, value, timeToNano(expiry))

	// Update persistence
	if err := c.Store.Store(ctx, key, value, expiry); err != nil {
		return fmt.Errorf("persistence store failed: %w", err)
	}

	return nil
}

// SetAsync stores a value in the cache, handling persistence asynchronously.
// If no TTL is provided, the default TTL is used.
// Key validation and in-memory caching happen synchronously.
// Returns a channel that will receive any persistence error (or be closed on success).
// Returns an error only for validation failures (e.g., invalid key format).
func (c *PersistentCache[K, V]) SetAsync(ctx context.Context, key K, value V, ttl ...time.Duration) (<-chan error, error) {
	var t time.Duration
	if len(ttl) > 0 {
		t = ttl[0]
	}
	expiry := c.expiry(t)

	// Validate key early (synchronous)
	if err := c.Store.ValidateKey(key); err != nil {
		return nil, err
	}

	// ALWAYS update memory first - reliability guarantee (synchronous)
	c.memory.setToMemory(key, value, timeToNano(expiry))

	// Update persistence asynchronously
	errCh := make(chan error, 1)
	go func() {
		defer close(errCh)
		storeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := c.Store.Store(storeCtx, key, value, expiry); err != nil {
			errCh <- fmt.Errorf("async persistence: %w", err)
		}
	}()

	return errCh, nil
}

// Delete removes a value from the cache.
// The value is always removed from memory. Returns an error if persistence deletion fails.
func (c *PersistentCache[K, V]) Delete(ctx context.Context, key K) error {
	// Remove from memory first (always succeeds)
	c.memory.deleteFromMemory(key)

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
func (c *PersistentCache[K, V]) Flush(ctx context.Context) (int, error) {
	memoryRemoved := c.memory.flushMemory()

	persistRemoved, err := c.Store.Flush(ctx)
	if err != nil {
		return memoryRemoved, fmt.Errorf("persistence flush: %w", err)
	}

	return memoryRemoved + persistRemoved, nil
}

// Len returns the number of items in the memory cache.
// For persistence item count, use cache.Store.Len(ctx).
func (c *PersistentCache[K, V]) Len() int {
	return c.memory.memoryLen()
}

// Close releases resources held by the cache.
func (c *PersistentCache[K, V]) Close() error {
	if err := c.Store.Close(); err != nil {
		return fmt.Errorf("close persistence: %w", err)
	}
	return nil
}
