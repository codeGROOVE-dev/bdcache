package sfcache

import (
	"fmt"
	"math/bits"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

// debugAdaptive enables adaptive mode debug output when SFCACHE_DEBUG=1
var debugAdaptive = os.Getenv("SFCACHE_DEBUG") == "1"

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

// maxFreq is the maximum frequency counter value (0-7).
// Higher values give better resolution for distinguishing hot items.
const maxFreq = 7

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

	// Global capacity tracking ensures exactly Size() entries can be stored,
	// regardless of hash distribution across shards.
	totalEntries atomic.Int64
	capacity     int
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

	// Warmup: during initial fill, admit everything without eviction checks
	warmupComplete bool

	// Adaptive mode detection based on ghost hit rate:
	// - Mode 0 (scan-heavy, ghost rate < 1%): pure recency, skip ghost tracking
	// - Mode 1 (balanced, ghost rate 1-5%): lenient promotion (freq > 0)
	// - Mode 2 (frequency-heavy, ghost rate > 5%): strict promotion (freq > 1)
	insertions            uint32
	ghostHits             uint32
	adaptiveMode          uint8  // 0=scan/recency, 1=balanced, 2=frequency-heavy
	adaptiveMinInsertions uint32 // min insertions before adaptive kicks in

	// Parent pointer for global capacity tracking
	parent *s3fifo[K, V]
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
	expiryNano int64         // Unix nanoseconds; 0 means no expiry
	freq       atomic.Uint32 // Frequency counter (0-7) for S3-FIFO eviction decisions; atomic for lock-free reads
	inSmall    bool          // True if in Small queue, false if in Main
}

// newS3FIFO creates a new sharded S3-FIFO cache with the given total capacity.
func newS3FIFO[K comparable, V any](cfg *config) *s3fifo[K, V] {
	capacity := cfg.size
	if capacity <= 0 {
		capacity = 16384
	}

	// More shards reduces lock contention, but too many shards causes capacity
	// loss due to hash distribution imbalance. Use 2x GOMAXPROCS as a balance
	// between concurrency and capacity efficiency.
	minShards := runtime.GOMAXPROCS(0) * 2
	// Each shard needs enough entries for good hash distribution (95%+ efficiency).
	// With 256 entries per shard, hash variance is diluted enough for consistent efficiency.
	const minEntriesPerShard = 256
	maxByCapacity := max(1, capacity/minEntriesPerShard)
	nshards := min(minShards, maxByCapacity, maxShards)
	// Round to power of 2 for fast modulo.
	//nolint:gosec // G115: nshards bounded by [1, maxShards]
	nshards = 1 << (bits.Len(uint(nshards)) - 1)

	shardCap := (capacity + nshards - 1) / nshards // ceiling division

	cache := &s3fifo[K, V]{
		shards:    make([]*shard[K, V], nshards),
		numShards: nshards,
		//nolint:gosec // G115: nshards bounded by [1, maxShards]
		shardMask: uint64(nshards - 1),
		capacity:  capacity,
	}

	// Detect key type at construction time to enable fast-path hash functions.
	// This avoids the type switch overhead on every Get/Set call.
	var zeroKey K
	switch any(zeroKey).(type) {
	case int:
		cache.keyIsInt = true
	case int64:
		cache.keyIsInt64 = true
	case string:
		cache.keyIsString = true
	}

	// Adaptive small queue ratio: larger for smaller caches to allow more frequency accumulation.
	// S3-FIFO paper recommends 10%, but smaller caches need more room for frequency estimation.
	var smallRatio float64
	switch {
	case capacity <= 32768: // ≤32K: 20% small queue
		smallRatio = 0.20
	case capacity <= 131072: // ≤128K: 15% small queue
		smallRatio = 0.15
	default: // >128K: 10% small queue (paper recommendation)
		smallRatio = 0.10
	}
	// Ghost queue at 100% matches reference implementation for better hit rate.
	const ghostRatio = 1.0

	// Prepare hasher for Bloom filter
	var hasher func(K) uint64
	switch {
	case cache.keyIsInt:
		hasher = func(key K) uint64 {
			return hashInt64(int64(*(*int)(unsafe.Pointer(&key))))
		}
	case cache.keyIsInt64:
		hasher = func(key K) uint64 {
			return hashInt64(*(*int64)(unsafe.Pointer(&key)))
		}
	case cache.keyIsString:
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
		smallCap := max(int(float64(shardCap)*smallRatio), 1)
		ghostCap := max(int(float64(shardCap)*ghostRatio), 1)
		minIns := uint32(max(shardCap, 256)) //nolint:gosec // G115: shardCap bounded by capacity/nshards
		cache.shards[i] = &shard[K, V]{
			capacity:              shardCap,
			smallCap:              smallCap,
			ghostCap:              ghostCap,
			entries:               make(map[K]*entry[K, V], shardCap),
			ghostActive:           newBloomFilter(ghostCap, 0.00001),
			ghostAging:            newBloomFilter(ghostCap, 0.00001),
			hasher:                hasher,
			adaptiveMinInsertions: minIns,
			adaptiveMode:          1, // Start in balanced mode
			parent:                cache,
		}
	}

	return cache
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
	var (
		zeroK K
		zeroV V
	)
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
func (c *s3fifo[K, V]) shard(key K) *shard[K, V] {
	// Fast paths for common key types avoid type switch overhead.
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
	// Fallback for other key types
	switch k := any(key).(type) {
	case uint:
		return c.shards[uint64(k)&c.shardMask]
	case uint64:
		return c.shards[k&c.shardMask]
	case string:
		return c.shards[wyhashString(k)&c.shardMask]
	case fmt.Stringer:
		return c.shards[wyhashString(k.String())&c.shardMask]
	default:
		return c.shards[wyhashString(fmt.Sprintf("%v", key))&c.shardMask]
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

		if ent.freq.Load() < maxFreq {
			ent.freq.Add(1)
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

		if ent.freq.Load() < maxFreq {
			ent.freq.Add(1)
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

	// S3-FIFO: increment frequency for promotion decisions.
	// Skip if already at max to reduce contention on hot keys.
	if ent.freq.Load() < maxFreq {
		ent.freq.Add(1)
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
		if ent.freq.Load() < maxFreq {
			ent.freq.Add(1)
		}
		s.mu.Unlock()
		return
	}

	// Slow path: insert new key (already holding lock)

	// Create new entry
	ent := s.newEntry()
	ent.key = key
	ent.value = value
	ent.expiryNano = expiryNano

	// Check if cache is at capacity
	atCapacity := s.parent.totalEntries.Load() >= int64(s.parent.capacity)

	// Warmup Bypass: During warmup, admit everything to small queue without eviction
	if !s.warmupComplete && !atCapacity {
		ent.inSmall = true
		s.small.pushBack(ent)
		s.entries[key] = ent
		s.incrementGlobal()
		s.mu.Unlock()
		return
	}
	// Mark warmup complete once we've reached capacity
	s.warmupComplete = true

	// Lazily check ghost only if at capacity (when eviction matters)
	// This saves 2× bloom filter checks + hash computation when cache isn't full
	if atCapacity {
		// Track insertions for adaptive mode detection
		s.insertions++

		// In scan/recency mode (mode=0), skip ghost checks entirely
		if s.adaptiveMode == 0 {
			ent.inSmall = true
		} else {
			// Check if key is in ghost (Bloom filter)
			h := s.hasher(key)
			inGhost := s.ghostActive.Contains(h) || s.ghostAging.Contains(h)
			ent.inSmall = !inGhost

			// Track ghost hits and apply frequency boost
			if inGhost {
				s.ghostHits++
				// Ghost Freq Boost: Items returning from ghost start with freq=1
				// This rewards items that proved popularity, but actual re-accesses build more frequency
				ent.freq.Store(1)
			}
		}

		// Adaptive mode detection: check every 256 insertions after warmup
		// Mode 0: scan-heavy (ghost rate < 1%) - pure recency, skip ghost
		// Mode 1: balanced (ghost rate 1-5% or >10%) - lenient promotion (freq > 0)
		// Mode 2: frequency-heavy (ghost rate 5-10%) - strict promotion (freq > 1)
		//
		// Insight: High ghost rates (>10%) indicate strong recency patterns
		// where items return quickly - these benefit from lenient promotion (mode 1)
		// rather than strict frequency requirements (mode 2).
		if s.insertions >= s.adaptiveMinInsertions && s.insertions&0xFF == 0 {
			ghostRate := s.ghostHits * 100 / s.insertions // percentage
			oldMode := s.adaptiveMode
			switch {
			case ghostRate < 1:
				s.adaptiveMode = 0 // Scan-heavy: use pure recency
			case ghostRate < 5:
				s.adaptiveMode = 1 // Balanced: lenient promotion
			case ghostRate <= 10:
				s.adaptiveMode = 2 // Frequency-heavy: strict promotion
			default:
				s.adaptiveMode = 1 // High recency: back to lenient (items returning fast)
			}
			if debugAdaptive && s.adaptiveMode != oldMode {
				fmt.Printf("[sfcache] mode %d→%d (ghost=%d%%, cap=%d)\n",
					oldMode, s.adaptiveMode, ghostRate, s.capacity)
			}
			// Reset counters for next period
			s.insertions = 0
			s.ghostHits = 0
		}

		// Evict one entry to make room
		if s.main.len > 0 && s.small.len <= s.capacity/10 {
			s.evictFromMain()
		} else if s.small.len > 0 {
			s.evictFromSmall()
		}
	} else {
		// Cache not full, always insert to small queue
		ent.inSmall = true
	}

	// Add to appropriate queue
	if ent.inSmall {
		s.small.pushBack(ent)
	} else {
		s.main.pushBack(ent)
	}

	s.entries[key] = ent
	s.incrementGlobal()
	s.mu.Unlock()
}

// incrementGlobal increments the global entry counter.
func (s *shard[K, V]) incrementGlobal() {
	s.parent.totalEntries.Add(1)
}

// decrementGlobal decrements the global entry counter.
func (s *shard[K, V]) decrementGlobal() {
	s.parent.totalEntries.Add(-1)
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
	s.decrementGlobal()
}

// evictFromSmall evicts an entry from the small queue.
// Promotion threshold adapts based on workload characteristics:
//   - Mode 0 (scan): always promote (pure recency).
//   - Mode 1 (balanced): need freq > 0 (one access).
//   - Mode 2 (frequency): need freq > 1 (two accesses).
func (s *shard[K, V]) evictFromSmall() {
	mainCap := (s.capacity * 9) / 10 // 90% for main queue

	// Adaptive promotion threshold based on detected workload type
	thresh := uint32(s.adaptiveMode) // 0, 1, or 2

	for s.small.len > 0 {
		e := s.small.head
		s.small.remove(e)

		if e.freq.Load() < thresh {
			// Not accessed enough - evict
			evictedKey := e.key
			delete(s.entries, evictedKey)

			// Track in ghost queue only if not in scan/recency mode
			// (scan-heavy workloads don't benefit from ghost tracking)
			if s.adaptiveMode > 0 {
				h := s.hasher(evictedKey)
				if !s.ghostActive.Contains(h) {
					s.ghostActive.Add(h)
				}
				// Rotate filters when active is full (provides approximate FIFO)
				if s.ghostActive.entries >= s.ghostCap {
					s.ghostAging.Reset()
					s.ghostActive, s.ghostAging = s.ghostAging, s.ghostActive
				}
			}

			s.putEntry(e)
			s.decrementGlobal()
			return
		}

		// Accessed enough - promote to Main queue
		e.freq.Store(0) // Reset frequency: entry must prove itself in Main
		e.inSmall = false
		s.main.pushBack(e)

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
		e := s.main.head
		s.main.remove(e)

		// Check if accessed since last eviction attempt
		if e.freq.Load() == 0 {
			// Not accessed - evict (no ghost tracking per S3-FIFO)
			delete(s.entries, e.key)
			s.putEntry(e)
			s.decrementGlobal()
			return
		}

		// Accessed - give second chance (FIFO-Reinsertion)
		// Decrement frequency (under lock, so Store is safe)
		e.freq.Store(e.freq.Load() - 1)
		s.main.pushBack(e)
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
	// Reset global counter
	c.totalEntries.Store(0)
	return total
}

func (s *shard[K, V]) flush() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	n := len(s.entries)
	s.entries = make(map[K]*entry[K, V], s.capacity)
	s.small.head, s.small.tail, s.small.len = nil, nil, 0
	s.main.head, s.main.tail, s.main.len = nil, nil, 0
	s.ghostActive.Reset()
	s.ghostAging.Reset()
	return n
}
