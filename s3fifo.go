package bdcache

import (
	"container/list"
	"sync"
	"time"
)

// s3fifo implements the S3-FIFO eviction algorithm.
// S3-FIFO uses three queues: Small (10%), Main (90%), and Ghost (for frequency tracking).
// Items start in Small, get promoted to Main if accessed again, and Ghost tracks evicted keys.
type s3fifo[K comparable, V any] struct {
	items     map[K]*entry[K, V]
	small     *list.List
	main      *list.List
	ghost     *list.List
	ghostKeys map[K]*list.Element
	capacity  int
	smallCap  int
	mainCap   int
	ghostCap  int
	mu        sync.RWMutex
}

// entry represents a cached item with metadata.
type entry[K comparable, V any] struct {
	expiry  time.Time
	key     K
	value   V
	element *list.Element
	freq    int
	inSmall bool
}

// newS3FIFO creates a new S3-FIFO cache with the given capacity.
func newS3FIFO[K comparable, V any](capacity int) *s3fifo[K, V] {
	if capacity <= 0 {
		capacity = 10000
	}

	smallCap := capacity / 10
	if smallCap < 1 {
		smallCap = 1
	}
	mainCap := capacity - smallCap

	return &s3fifo[K, V]{
		capacity:  capacity,
		smallCap:  smallCap,
		mainCap:   mainCap,
		ghostCap:  capacity,
		items:     make(map[K]*entry[K, V]),
		small:     list.New(),
		main:      list.New(),
		ghost:     list.New(),
		ghostKeys: make(map[K]*list.Element),
	}
}

// get retrieves a value from the cache.
func (c *s3fifo[K, V]) get(key K) (V, bool) {
	c.mu.Lock()
	ent, ok := c.items[key]
	if !ok {
		c.mu.Unlock()
		var zero V
		return zero, false
	}

	// Check expiration (skip time.Now() if no expiry set)
	if !ent.expiry.IsZero() && time.Now().After(ent.expiry) {
		c.mu.Unlock()
		var zero V
		return zero, false
	}

	// Increment frequency counter (used during eviction)
	ent.freq++
	val := ent.value
	c.mu.Unlock()

	return val, true
}

// set adds or updates a value in the cache.
func (c *s3fifo[K, V]) set(key K, value V, expiry time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Update existing entry
	if ent, ok := c.items[key]; ok {
		ent.value = value
		ent.expiry = expiry
		ent.freq++
		return
	}

	// Check if key is in ghost queue (previously evicted)
	inGhost := false
	if ghostElem, ok := c.ghostKeys[key]; ok {
		inGhost = true
		c.ghost.Remove(ghostElem)
		delete(c.ghostKeys, key)
	}

	// Create new entry
	ent := &entry[K, V]{
		key:     key,
		value:   value,
		expiry:  expiry,
		freq:    0,
		inSmall: !inGhost, // If in ghost, promote directly to main
	}

	// Evict if necessary
	if len(c.items) >= c.capacity {
		c.evict()
	}

	// Add to appropriate queue
	if ent.inSmall {
		ent.element = c.small.PushBack(ent)
	} else {
		ent.element = c.main.PushBack(ent)
	}

	c.items[key] = ent
}

// delete removes a value from the cache.
func (c *s3fifo[K, V]) delete(key K) {
	c.mu.Lock()
	defer c.mu.Unlock()

	ent, ok := c.items[key]
	if !ok {
		return
	}

	if ent.inSmall {
		c.small.Remove(ent.element)
	} else {
		c.main.Remove(ent.element)
	}

	delete(c.items, key)
}

// evict removes items according to S3-FIFO algorithm.
func (c *s3fifo[K, V]) evict() {
	// Try to evict from small queue first
	if c.small.Len() > 0 {
		c.evictFromSmall()
		return
	}

	// Evict from main queue
	c.evictFromMain()
}

// evictFromSmall evicts an item from the small queue.
func (c *s3fifo[K, V]) evictFromSmall() {
	for c.small.Len() > 0 {
		elem := c.small.Front()
		ent, ok := elem.Value.(*entry[K, V])
		if !ok {
			c.small.Remove(elem)
			continue
		}

		c.small.Remove(elem)

		// If accessed (freq > 0), promote to main queue
		if ent.freq == 0 {
			// Evict and add to ghost queue
			delete(c.items, ent.key)
			c.addToGhost(ent.key)
			return
		}
		ent.freq = 0
		ent.inSmall = false
		ent.element = c.main.PushBack(ent)
	}
}

// evictFromMain evicts an item from the main queue.
func (c *s3fifo[K, V]) evictFromMain() {
	for c.main.Len() > 0 {
		elem := c.main.Front()
		ent, ok := elem.Value.(*entry[K, V])
		if !ok {
			c.main.Remove(elem)
			continue
		}

		c.main.Remove(elem)

		// If accessed (freq > 0), move to back of main queue
		if ent.freq == 0 {
			// Evict and add to ghost queue
			delete(c.items, ent.key)
			c.addToGhost(ent.key)
			return
		}
		ent.freq--
		ent.element = c.main.PushBack(ent)
	}
}

// addToGhost adds a key to the ghost queue for frequency tracking.
func (c *s3fifo[K, V]) addToGhost(key K) {
	// Evict from ghost if at capacity
	if c.ghost.Len() >= c.ghostCap {
		elem := c.ghost.Front()
		ghostKey, ok := elem.Value.(K)
		if ok {
			c.ghost.Remove(elem)
			delete(c.ghostKeys, ghostKey)
		} else {
			c.ghost.Remove(elem)
		}
	}

	// Add to ghost queue
	elem := c.ghost.PushBack(key)
	c.ghostKeys[key] = elem
}

// len returns the number of items in the cache.
func (c *s3fifo[K, V]) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}

// cleanup removes expired entries from the cache.
func (c *s3fifo[K, V]) cleanup() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	removed := 0

	// Collect expired keys
	var expired []K
	for key, ent := range c.items {
		if !ent.expiry.IsZero() && now.After(ent.expiry) {
			expired = append(expired, key)
		}
	}

	// Remove expired entries
	for _, key := range expired {
		ent := c.items[key]
		if ent.inSmall {
			c.small.Remove(ent.element)
		} else {
			c.main.Remove(ent.element)
		}
		delete(c.items, key)
		removed++
	}

	return removed
}
