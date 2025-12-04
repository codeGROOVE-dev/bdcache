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
	mu        sync.RWMutex       // RWMutex is faster for read-heavy workloads with sharding
	_         [40]byte           // Padding to cache line boundary
	entries   map[K]*entry[K, V] // Direct map access (protected by mu)
	small     entryList[K, V]    // Intrusive list for small queue
	main      entryList[K, V]    // Intrusive list for main queue
	ghost     ghostList[K]       // Intrusive list for ghost queue
	ghostKeys map[K]*ghostEntry[K]
	capacity  int
	smallCap  int
	ghostCap  int

	// Free lists for reducing allocations
	freeEntries      *entry[K, V]
	freeGhostEntries *ghostEntry[K]
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

// ghostEntry is a node in the ghost queue (tracks evicted keys only).
type ghostEntry[K comparable] struct {
	key  K
	prev *ghostEntry[K]
	next *ghostEntry[K]
}

// ghostList is an intrusive doubly-linked list for ghost entries.
type ghostList[K comparable] struct {
	head *ghostEntry[K]
	tail *ghostEntry[K]
	len  int
}

func (l *ghostList[K]) pushBack(e *ghostEntry[K]) {
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

func (l *ghostList[K]) remove(e *ghostEntry[K]) {
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

func (l *ghostList[K]) front() *ghostEntry[K] {
	return l.head
}

func (l *ghostList[K]) init() {
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

	// Auto-tune configuration if not explicitly set
	smallRatio := cfg.smallRatio
	if smallRatio <= 0 {
		if capacity <= 16384 {
			smallRatio = 0.01 // 1% for small caches (Zipf-friendly)
		} else {
			smallRatio = 0.05 // 5% for large caches (Meta trace optimal)
		}
	}

	ghostRatio := cfg.ghostRatio
	if ghostRatio <= 0 {
		if capacity <= 16384 {
			ghostRatio = 0.5 // 50% for small caches
		} else {
			ghostRatio = 1.0 // 100% for large caches
		}
	}

	for i := range numShards {
		c.shards[i] = newShard[K, V](shardCap, smallRatio, ghostRatio)
	}

	return c
}

// newShard creates a new S3-FIFO shard with the given capacity.
func newShard[K comparable, V any](capacity int, smallRatio, ghostRatio float64) *shard[K, V] {
	// Small queue: recommended 10%
	smallCap := int(float64(capacity) * smallRatio)
	if smallCap < 1 {
		smallCap = 1
	}

	// Ghost queue: recommended 100%
	ghostCap := int(float64(capacity) * ghostRatio)
	if ghostCap < 1 {
		ghostCap = 1
	}

	s := &shard[K, V]{
		capacity:  capacity,
		smallCap:  smallCap,
		ghostCap:  ghostCap,
		entries:   make(map[K]*entry[K, V], capacity),
		ghostKeys: make(map[K]*ghostEntry[K], ghostCap),
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

func (s *shard[K, V]) getGhostEntry() *ghostEntry[K] {
	if s.freeGhostEntries != nil {
		e := s.freeGhostEntries
		s.freeGhostEntries = e.next
		e.next = nil
		e.prev = nil
		return e
	}
	return &ghostEntry[K]{}
}

func (s *shard[K, V]) putGhostEntry(e *ghostEntry[K]) {
	var zeroK K
	e.key = zeroK
	e.prev = nil

	e.next = s.freeGhostEntries
	s.freeGhostEntries = e
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
	s.mu.RUnlock()

	if !ok {
		var zero V
		return zero, false
	}

	// Check expiration (lazy - actual cleanup happens in background)
	if ent.expiryNano != 0 && time.Now().UnixNano() > ent.expiryNano {
		var zero V
		return zero, false
	}

	// S3-FIFO: Mark as accessed for lazy promotion.
	// Fast path: check if already at max freq
	if f := ent.freq.Load(); f < 3 {
		ent.freq.Store(f + 1)
	}

	return ent.value, true
}

// getOrSet retrieves a value or sets it if not found, in a single operation.
// Returns the value and true if found, or sets the value and returns false.
func (c *s3fifo[K, V]) getOrSet(key K, value V, expiryNano int64) (V, bool) {
	return c.shard(key).getOrSet(key, value, expiryNano)
}

func (s *shard[K, V]) getOrSet(key K, value V, expiryNano int64) (V, bool) {
	// Fast path: try read lock first (optimistic for cache hits)
	s.mu.RLock()
	if ent, ok := s.entries[key]; ok {
		// Check if NOT expired - return early with cache hit
		if ent.expiryNano == 0 || time.Now().UnixNano() <= ent.expiryNano {
			if f := ent.freq.Load(); f < 3 {
				ent.freq.Store(f + 1)
			}
			v := ent.value
			s.mu.RUnlock()
			return v, true
		}
		// Expired - need write lock to update, fall through to slow path
		s.mu.RUnlock()
	} else {
		s.mu.RUnlock()
	}

	// Slow path: take write lock for insert/update
	s.mu.Lock()

	// Double-check after acquiring write lock
	if ent, ok := s.entries[key]; ok {
		// Check expiration
		if ent.expiryNano != 0 && time.Now().UnixNano() > ent.expiryNano {
			// Expired - update in place
			ent.value = value
			ent.expiryNano = expiryNano
			s.mu.Unlock()
			return value, false
		}
		// Another goroutine inserted it - return existing value
		if f := ent.freq.Load(); f < 3 {
			ent.freq.Store(f + 1)
		}
		v := ent.value
		s.mu.Unlock()
		return v, true
	}

	// Not found - insert new entry
	inGhost := false
	if ghostEnt, ok := s.ghostKeys[key]; ok {
		inGhost = true
		s.ghost.remove(ghostEnt)
		delete(s.ghostKeys, key)
		s.putGhostEntry(ghostEnt)
	}

	ent := s.getEntry()
	ent.key = key
	ent.value = value
	ent.expiryNano = expiryNano
	ent.inSmall = !inGhost

	for s.small.len+s.main.len >= s.capacity {
		s.evict()
	}

	if ent.inSmall {
		s.small.pushBack(ent)
	} else {
		s.main.pushBack(ent)
	}

	s.entries[key] = ent
	s.mu.Unlock()
	return value, false
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

	// Check if key is in ghost queue
	inGhost := false
	if ghostEnt, ok := s.ghostKeys[key]; ok {
		inGhost = true
		s.ghost.remove(ghostEnt)
		delete(s.ghostKeys, key)
		s.putGhostEntry(ghostEnt)
	}

	// Create new entry
	ent := s.getEntry()
	ent.key = key
	ent.value = value
	ent.expiryNano = expiryNano
	ent.inSmall = !inGhost

	// Make room if at capacity
	for s.small.len+s.main.len >= s.capacity {
		s.evict()
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

// evict removes one entry according to S3-FIFO algorithm.
func (s *shard[K, V]) evict() {
	if s.small.len >= s.smallCap {
		s.evictFromSmall()
		return
	}
	s.evictFromMain()
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

// addToGhost adds a key to the ghost queue.
func (s *shard[K, V]) addToGhost(key K) {
	if s.ghost.len >= s.ghostCap {
		oldest := s.ghost.front()
		delete(s.ghostKeys, oldest.key)
		s.ghost.remove(oldest)
		s.putGhostEntry(oldest)
	}

	ent := s.getGhostEntry()
	ent.key = key
	s.ghost.pushBack(ent)
	s.ghostKeys[key] = ent
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
	s.ghost.init()
	s.ghostKeys = make(map[K]*ghostEntry[K], s.ghostCap)
	return n
}
