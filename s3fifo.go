package sfcache

import (
	"fmt"
	"math/bits"
	"runtime"
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

const maxShards = 2048

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
		capacity = 16384
	}

	// More shards reduces lock contention but each shard needs enough
	// entries for S3-FIFO to work effectively.
	// For small caches, use single shard to match reference implementation behavior.
	var nshards int
	if capacity < 65536 {
		// Use single shard for caches < 64K to match reference s3-fifo
		nshards = 1
	} else {
		minShardSize := 256
		if capacity < 262144 {
			minShardSize = 2048 // Conservative for medium caches (< 256K)
		}
		nshards = min(runtime.GOMAXPROCS(0)*8, capacity/minShardSize, maxShards)
		if nshards < 1 {
			nshards = 1
		}
	}
	// Round to power of 2 for fast modulo.
	//nolint:gosec // G115: nshards bounded by [1, maxShards]
	nshards = 1 << (bits.Len(uint(nshards)) - 1)

	shardCap := (capacity + nshards - 1) / nshards // ceiling division

	c := &s3fifo[K, V]{
		shards:    make([]*shard[K, V], nshards),
		numShards: nshards,
		//nolint:gosec // G115: nshards bounded by [1, maxShards]
		shardMask: uint64(nshards - 1),
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

	// S3-FIFO paper recommends small queue at 10% of total capacity.
	// Ghost queue at 100% matches reference implementation for better hit rate.
	var smallRatio, ghostRatio float64
	smallRatio = 0.10
	ghostRatio = 1.0

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

	for i := range nshards {
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
		ghostActive: newBloomFilter(ghostCap, 0.00001),
		ghostAging:  newBloomFilter(ghostCap, 0.00001),
		hasher:      hasher,
	}
	return s
}

func (s *shard[K, V]) newEntry() *entry[K, V] {
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
	if c.keyIsString {
		s := c.shards[wyhashString(*(*string)(unsafe.Pointer(&key)))&c.shardMask]
		s.mu.RLock()
		ent, ok := s.entries[key]
		if !ok {
			s.mu.RUnlock()
			var zero V
			return zero, false
		}
		val := ent.value
		expiry := ent.expiryNano
		s.mu.RUnlock()

		if expiry != 0 && time.Now().UnixNano() > expiry {
			var zero V
			return zero, false
		}

		if f := ent.freq.Load(); f < 3 {
			ent.freq.Store(f + 1)
		}
		return val, true
	}
	if c.keyIsInt {
		//nolint:gosec // G115: intentional wrap for fast modulo
		s := c.shards[uint64(*(*int)(unsafe.Pointer(&key)))&c.shardMask]
		s.mu.RLock()
		ent, ok := s.entries[key]
		if !ok {
			s.mu.RUnlock()
			var zero V
			return zero, false
		}
		val := ent.value
		expiry := ent.expiryNano
		s.mu.RUnlock()

		if expiry != 0 && time.Now().UnixNano() > expiry {
			var zero V
			return zero, false
		}

		if f := ent.freq.Load(); f < 3 {
			ent.freq.Store(f + 1)
		}
		return val, true
	}
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
	if c.keyIsString {
		c.shards[wyhashString(*(*string)(unsafe.Pointer(&key)))&c.shardMask].set(key, value, expiryNano)
		return
	}
	if c.keyIsInt {
		//nolint:gosec // G115: intentional wrap for fast modulo
		c.shards[uint64(*(*int)(unsafe.Pointer(&key)))&c.shardMask].set(key, value, expiryNano)
		return
	}
	c.shard(key).set(key, value, expiryNano)
}

func (s *shard[K, V]) set(key K, value V, expiryNano int64) {
	s.mu.Lock()

	// Fast path: update existing entry
	if ent, ok := s.entries[key]; ok {
		ent.value = value
		ent.expiryNano = expiryNano
		// Increment frequency on update (like reference s3-fifo)
		if f := ent.freq.Load(); f < 3 {
			ent.freq.Store(f + 1)
		}
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
	ent := s.newEntry()
	ent.key = key
	ent.value = value
	ent.expiryNano = expiryNano
	ent.inSmall = !inGhost

	// Evict when at capacity (no overflow buffer)
	for s.small.len+s.main.len >= s.capacity {
		// Use > instead of >= to match reference implementation
		if s.small.len > s.capacity/10 {
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
// Items accessed more than once (freq > 1) are promoted to Main,
// items with freq <= 1 are evicted to ghost queue.
func (s *shard[K, V]) evictFromSmall() {
	mainCap := (s.capacity * 9) / 10 // 90% for main queue

	for s.small.len > 0 {
		ent := s.small.front()
		s.small.remove(ent)

		// Check if accessed more than once (freq > 1 to promote)
		if ent.freq.Load() <= 1 {
			// Not accessed enough - evict and track in ghost
			delete(s.entries, ent.key)

			// Add to ghost queue using two rotating Bloom filters
			h := s.hasher(ent.key)
			if !s.ghostActive.Contains(h) {
				s.ghostActive.Add(h)
			}
			// Rotate filters when active is full (provides approximate FIFO)
			if s.ghostActive.entries >= s.ghostCap {
				s.ghostAging.Reset()
				s.ghostActive, s.ghostAging = s.ghostAging, s.ghostActive
			}

			s.putEntry(ent)
			return
		}

		// Accessed more than once - promote to Main queue
		// Reset frequency: entry must prove itself in Main
		ent.freq.Store(0)
		ent.inSmall = false
		s.main.pushBack(ent)

		// Cascade eviction if main queue exceeds capacity
		if s.main.len > mainCap {
			s.evictFromMain()
		}
	}
}

// evictFromMain evicts an entry from the main queue.
// Per S3-FIFO paper: evicted items from Main are NOT added to ghost queue.
func (s *shard[K, V]) evictFromMain() {
	for s.main.len > 0 {
		ent := s.main.front()
		s.main.remove(ent)

		// Check if accessed since last eviction attempt
		f := ent.freq.Load()
		if f == 0 {
			// Not accessed - evict (no ghost tracking per S3-FIFO)
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
