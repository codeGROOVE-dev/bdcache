package sfcache

import (
	"fmt"
	"math/bits"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

// wyhash constants for fast string hashing.
const (
	wyp0 = 0xa0761d6478bd642f
	wyp1 = 0xe7037ed1a0b428db
)

// wyhashString is a fast hash function for strings.
// Adapted from wyhash (https://github.com/wangyi-fudan/wyhash).
// About 2.6x faster than maphash.String with acceptable distribution.
func wyhashString(s string) uint64 {
	n := len(s)
	if n == 0 {
		return 0
	}

	p := unsafe.Pointer(unsafe.StringData(s))
	var a, b uint64

	if n <= 8 {
		if n >= 4 {
			a = uint64(*(*uint32)(p))
			b = uint64(*(*uint32)(unsafe.Add(p, n-4)))
		} else {
			a = uint64(*(*byte)(p))<<16 | uint64(*(*byte)(unsafe.Add(p, n>>1)))<<8 | uint64(*(*byte)(unsafe.Add(p, n-1)))
			b = 0
		}
	} else {
		a = *(*uint64)(p)
		b = *(*uint64)(unsafe.Add(p, n-8))
	}

	// wymix
	hi, lo := bits.Mul64(a^wyp0, b^uint64(n)^wyp1)
	return hi ^ lo
}

const (
	maxShards          = 2048
	minEntriesPerShard = 256 // Minimum entries per shard for S3-FIFO algorithm to work well
)

// s3fifo implements the S3-FIFO eviction algorithm from SOSP'23 paper
// "FIFO queues are all you need for cache eviction"
//
// This implementation uses dynamic sharding for improved concurrent performance.
// The number of shards is determined by capacity to ensure each shard has enough
// entries for the S3-FIFO algorithm to work effectively.
// Each shard is an independent S3-FIFO instance with its own queues and lock.
//
// Algorithm per shard:
// - Small queue (S): 10-20% of shard capacity, for new entries
// - Main queue (M): 80-90% of shard capacity, for promoted entries
// - Ghost queue (G): Tracks evicted keys (no data)
//
// On cache miss:
//   - If entry not in ghost → insert into Small
//   - If entry in ghost → insert into Main (was accessed before)
//
// On eviction from Small:
//   - If freq == 0 → evict and add to ghost
//   - If freq > 0 → promote to Main and reset freq to 0
//
// On eviction from Main:
//   - If freq == 0 → evict (don't add to ghost, already there)
//   - If freq > 0 → reinsert to back of Main and decrement freq (lazy promotion)
type s3fifo[K comparable, V any] struct {
	shards      []*shard[K, V]
	numShards   int
	shardMask   uint64 // For fast modulo via bitwise AND
	keyIsInt    bool   // Fast path flag for int keys
	keyIsInt64  bool   // Fast path flag for int64 keys
	keyIsString bool   // Fast path flag for string keys
}

// shard is an independent S3-FIFO cache partition.
// Uses RWMutex for read-heavy workloads; sharding reduces contention across goroutines.
// The entries map provides O(1) lookup while intrusive lists maintain queue order.
//
//nolint:govet // fieldalignment: padding is intentional to prevent false sharing
type shard[K comparable, V any] struct {
	mu      sync.RWMutex       // RWMutex is faster for read-heavy workloads with sharding
	_       [40]byte           // Padding to cache line boundary
	entries map[K]*entry[K, V] // Direct map access (protected by mu)
	small   entryList[K, V]    // Intrusive list for small queue
	main    entryList[K, V]    // Intrusive list for main queue

	// Two-stage Bloom filter ghost: tracks recently evicted keys with low memory overhead.
	// Two filters rotate to provide approximate FIFO.
	// When ghostActive fills up, ghostAging is cleared and they swap roles.
	ghostActive *bloomFilter
	ghostAging  *bloomFilter
	ghostCap    int
	hasher      func(K) uint64

	capacity int
	smallCap int

	// Free list for reducing allocations
	freeEntries *entry[K, V]
}

// entryList is an intrusive doubly-linked list for cache entries.
// Zero value is a valid empty list.
type entryList[K comparable, V any] struct {
	head *entry[K, V]
	tail *entry[K, V]
	len  int
}

func (l *entryList[K, V]) pushBack(e *entry[K, V]) {
	e.prev = l.tail
	e.next = nil
	if l.tail != nil {
		l.tail.next = e
	} else {
		l.head = e
	}
	l.tail = e
	l.len++
}

func (l *entryList[K, V]) remove(e *entry[K, V]) {
	if e.prev != nil {
		e.prev.next = e.next
	} else {
		l.head = e.next
	}
	if e.next != nil {
		e.next.prev = e.prev
	} else {
		l.tail = e.prev
	}
	e.prev = nil
	e.next = nil
	l.len--
}

func (l *entryList[K, V]) front() *entry[K, V] {
	return l.head
}

func (l *entryList[K, V]) init() {
	l.head = nil
	l.tail = nil
	l.len = 0
}

// timeToNano converts a time.Time to Unix nanoseconds, returning 0 for zero time.
func timeToNano(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

// entry represents a cached value with metadata.
type entry[K comparable, V any] struct {
	key        K
	value      V
	prev       *entry[K, V] // Intrusive list pointers
	next       *entry[K, V]
	expiryNano int64        // Unix nanoseconds; 0 means no expiry
	freq       atomic.Int32 // Frequency counter for improved S3-FIFO/LFU
	inSmall    bool         // True if in Small queue, false if in Main
}

// newS3FIFO creates a new sharded S3-FIFO cache with the given total capacity.
func newS3FIFO[K comparable, V any](cfg *config) *s3fifo[K, V] {
	capacity := cfg.size
	if capacity <= 0 {
		capacity = 16384 // 2^14, divides evenly by 16 shards
	}

	// Calculate number of shards: ensure each shard has at least minEntriesPerShard
	// Round down to nearest power of 2 for fast modulo via bitwise AND
	numShards := capacity / minEntriesPerShard
	if numShards < 1 {
		numShards = 1
	}
	if numShards > maxShards {
		numShards = maxShards
	}
	// Round down to power of 2
	//nolint:gosec // G115: numShards is bounded by [1, maxShards], conversion is safe
	numShards = 1 << (bits.Len(uint(numShards)) - 1)

	// Use ceiling division to ensure total capacity >= requested
	shardCap := (capacity + numShards - 1) / numShards

	c := &s3fifo[K, V]{
		shards:    make([]*shard[K, V], numShards),
		numShards: numShards,
		//nolint:gosec // G115: numShards is bounded by [1, maxShards], conversion is safe
		shardMask: uint64(numShards - 1),
	}

	// Detect key type at construction time to enable fast-path hash functions.
	// This avoids the type switch overhead on every Get/Set call.
	var zeroKey K
	switch any(zeroKey).(type) {
	case int:
		c.keyIsInt = true
	case int64:
		c.keyIsInt64 = true
	case string:
		c.keyIsString = true
	}

	// Auto-tune ratios based on capacity.
	// Small caches with Zipf workloads need tight ghost tracking to avoid false promotions.
	// Large caches benefit from more ghost history for better admission decisions.
	//
	// Zipf benchmark results:
	//   500 items (0.1%):  ghostRatio=0.2 -> 48.12% (wins)
	//   5000 items (1%):   ghostRatio=0.2 -> 64.50% (wins)
	//   50000 items (10%): testing larger small queue for better ghost learning
	var smallRatio, ghostRatio float64
	if capacity <= 10000 {
		smallRatio = 0.01 // 1% small queue for Zipf-skewed workloads
		ghostRatio = 0.2  // 20% per bloom = ~40% total ghost tracking
	} else {
		smallRatio = 0.10 // 10% for large caches - standard S3-FIFO ratio
		ghostRatio = 0.2  // 20% per bloom = ~40% total ghost tracking
	}

	// Prepare hasher for Bloom filter
	var hasher func(K) uint64
	switch {
	case c.keyIsInt:
		hasher = func(key K) uint64 {
			return hashInt64(int64(*(*int)(unsafe.Pointer(&key))))
		}
	case c.keyIsInt64:
		hasher = func(key K) uint64 {
			return hashInt64(*(*int64)(unsafe.Pointer(&key)))
		}
	case c.keyIsString:
		hasher = func(key K) uint64 {
			return wyhashString(*(*string)(unsafe.Pointer(&key)))
		}
	default:
		hasher = func(key K) uint64 {
			switch k := any(key).(type) {
			case uint:
				//nolint:gosec // G115: intentional bit reinterpretation for hashing
				return hashInt64(int64(k))
			case uint64:
				//nolint:gosec // G115: intentional bit reinterpretation for hashing
				return hashInt64(int64(k))
			case string:
				return wyhashString(k)
			case fmt.Stringer:
				return wyhashString(k.String())
			default:
				return wyhashString(fmt.Sprintf("%v", k))
			}
		}
	}

	for i := range numShards {
		c.shards[i] = newShard[K, V](shardCap, smallRatio, ghostRatio, hasher)
	}

	return c
}

// newShard creates a new S3-FIFO shard with the given capacity.
func newShard[K comparable, V any](capacity int, smallRatio, ghostRatio float64, hasher func(K) uint64) *shard[K, V] {
	// Small queue: recommended 10%
	smallCap := int(float64(capacity) * smallRatio)
	if smallCap < 1 {
		smallCap = 1
	}

	// Ghost capacity: controls rotation frequency
	ghostCap := int(float64(capacity) * ghostRatio)
	if ghostCap < 1 {
		ghostCap = 1
	}

	s := &shard[K, V]{
		capacity:    capacity,
		smallCap:    smallCap,
		ghostCap:    ghostCap,
		entries:     make(map[K]*entry[K, V], capacity),
		ghostActive: newBloomFilter(ghostCap, 0.0001),
		ghostAging:  newBloomFilter(ghostCap, 0.0001),
		hasher:      hasher,
	}
	return s
}

func (s *shard[K, V]) getEntry() *entry[K, V] {
	if s.freeEntries != nil {
		e := s.freeEntries
		s.freeEntries = e.next
		e.next = nil
		e.prev = nil
		return e
	}
	return &entry[K, V]{}
}

func (s *shard[K, V]) putEntry(e *entry[K, V]) {
	var zeroK K
	var zeroV V
	e.key = zeroK
	e.value = zeroV
	e.expiryNano = 0
	e.freq.Store(0)
	e.inSmall = false
	e.prev = nil

	e.next = s.freeEntries
	s.freeEntries = e
}

// shard returns the shard for a given key using type-optimized hashing.
// Uses bitwise AND with shardMask for fast modulo (numShards must be power of 2).
// Fast paths for int, int64, and string keys avoid the type switch overhead entirely.
//
//go:nosplit
func (c *s3fifo[K, V]) shard(key K) *shard[K, V] {
	// Fast path for int keys (most common case in benchmarks).
	// The keyIsInt flag is set once at construction, so this branch is predictable.
	if c.keyIsInt {
		//nolint:gosec // G115: intentional wrap for fast modulo
		return c.shards[uint64(*(*int)(unsafe.Pointer(&key)))&c.shardMask]
	}
	if c.keyIsInt64 {
		//nolint:gosec // G115: intentional wrap for fast modulo
		return c.shards[uint64(*(*int64)(unsafe.Pointer(&key)))&c.shardMask]
	}
	if c.keyIsString {
		return c.shards[wyhashString(*(*string)(unsafe.Pointer(&key)))&c.shardMask]
	}
	return c.shards[c.shardIndexSlow(key)]
}

// shardIndexSlow computes the shard index using a type switch.
// This is the fallback for key types other than int/int64/string.
func (c *s3fifo[K, V]) shardIndexSlow(key K) uint64 {
	switch k := any(key).(type) {
	case uint:
		return uint64(k) & c.shardMask
	case uint64:
		return k & c.shardMask
	case string:
		return wyhashString(k) & c.shardMask
	case fmt.Stringer:
		return wyhashString(k.String()) & c.shardMask
	default:
		// Fallback: convert to string representation and hash.
		// This is not fast, but is reliable for any comparable type.
		// Avoid using structs as keys if performance matters.
		return wyhashString(fmt.Sprintf("%v", key)) & c.shardMask
	}
}

// get retrieves a value from the cache.
// On hit, increments frequency counter (used during eviction).
func (c *s3fifo[K, V]) get(key K) (V, bool) {
	return c.shard(key).get(key)
}

func (s *shard[K, V]) get(key K) (V, bool) {
	s.mu.RLock()
	ent, ok := s.entries[key]
	if !ok {
		s.mu.RUnlock()
		var zero V
		return zero, false
	}

	// Read values while holding lock to avoid race with concurrent set()
	val := ent.value
	expiry := ent.expiryNano
	s.mu.RUnlock()

	// Check expiration (lazy - actual cleanup happens in background)
	if expiry != 0 && time.Now().UnixNano() > expiry {
		var zero V
		return zero, false
	}

	// S3-FIFO: Mark as accessed for lazy promotion.
	// Fast path: check if already at max freq
	if f := ent.freq.Load(); f < 3 {
		ent.freq.Store(f + 1)
	}

	return val, true
}

// set adds or updates a value in the cache.
// expiryNano is Unix nanoseconds; 0 means no expiry.
func (c *s3fifo[K, V]) set(key K, value V, expiryNano int64) {
	c.shard(key).set(key, value, expiryNano)
}

func (s *shard[K, V]) set(key K, value V, expiryNano int64) {
	s.mu.Lock()

	// Fast path: update existing entry
	if ent, ok := s.entries[key]; ok {
		ent.value = value
		ent.expiryNano = expiryNano
		s.mu.Unlock()
		return
	}

	// Slow path: insert new key (already holding lock)

	// Check if key is in ghost (Bloom filter)
	h := s.hasher(key)
	inGhost := s.ghostActive.Contains(h)
	if !inGhost {
		inGhost = s.ghostAging.Contains(h)
	}

	// Create new entry
	ent := s.getEntry()
	ent.key = key
	ent.value = value
	ent.expiryNano = expiryNano
	ent.inSmall = !inGhost

	// Evict when at capacity (no overflow buffer)
	for s.small.len+s.main.len >= s.capacity {
		if s.small.len >= s.smallCap {
			s.evictFromSmall()
		} else {
			s.evictFromMain()
		}
	}

	// Add to appropriate queue
	if ent.inSmall {
		s.small.pushBack(ent)
	} else {
		s.main.pushBack(ent)
	}

	// In-place map insertion
	s.entries[key] = ent
	s.mu.Unlock()
}

// del removes a value from the cache.
func (c *s3fifo[K, V]) del(key K) {
	c.shard(key).delete(key)
}

func (s *shard[K, V]) delete(key K) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ent, ok := s.entries[key]
	if !ok {
		return
	}

	if ent.inSmall {
		s.small.remove(ent)
	} else {
		s.main.remove(ent)
	}

	delete(s.entries, key)
	s.putEntry(ent)
}

// evictFromSmall evicts an entry from the small queue.
func (s *shard[K, V]) evictFromSmall() {
	for s.small.len > 0 {
		ent := s.small.front()
		s.small.remove(ent)

		// Check if accessed since last eviction attempt
		if ent.freq.Load() == 0 {
			// Not accessed - evict and track in ghost
			delete(s.entries, ent.key)
			s.addToGhost(ent.key)
			s.putEntry(ent)
			return
		}

		// Accessed - promote to Main queue
		// Reset frequency per S3-FIFO paper: entry must prove itself in Main
		ent.freq.Store(0)
		ent.inSmall = false
		s.main.pushBack(ent)
	}
}

// evictFromMain evicts an entry from the main queue.
func (s *shard[K, V]) evictFromMain() {
	for s.main.len > 0 {
		ent := s.main.front()
		s.main.remove(ent)

		// Check if accessed since last eviction attempt
		f := ent.freq.Load()
		if f == 0 {
			// Not accessed - evict
			delete(s.entries, ent.key)
			s.putEntry(ent)
			return
		}

		// Accessed - give second chance (FIFO-Reinsertion)
		// Decrement frequency
		ent.freq.Store(f - 1)
		s.main.pushBack(ent)
	}
}

// addToGhost adds a key to the ghost queue using two rotating Bloom filters.
func (s *shard[K, V]) addToGhost(key K) {
	h := s.hasher(key)
	if !s.ghostActive.Contains(h) {
		s.ghostActive.Add(h)
	}

	// Rotate filters when active is full (provides approximate FIFO)
	if s.ghostActive.entries >= s.ghostCap {
		// Reset aging filter and swap - aging becomes new active
		s.ghostAging.Reset()
		s.ghostActive, s.ghostAging = s.ghostAging, s.ghostActive
	}
}

// len returns the total number of entries across all shards.
func (c *s3fifo[K, V]) len() int {
	total := 0
	for i := range c.shards {
		s := c.shards[i]
		s.mu.Lock()
		total += len(s.entries)
		s.mu.Unlock()
	}
	return total
}

// flush removes all entries from all shards.
func (c *s3fifo[K, V]) flush() int {
	total := 0
	for i := range c.shards {
		total += c.shards[i].flush()
	}
	return total
}

func (s *shard[K, V]) flush() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	n := len(s.entries)
	s.entries = make(map[K]*entry[K, V], s.capacity)
	s.small.init()
	s.main.init()
	s.ghostActive.Reset()
	s.ghostAging.Reset()
	return n
}
