package multicache

import (
	"fmt"
	"math/bits"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/puzpuzpuz/xsync/v4"
)

// wyhash constants for fast string hashing.
// Using wyhash instead of maphash: benchmarked +12% string-get, +16% getOrSet throughput.
// maphash.String with fixed seed was tested and showed -12.1% string-get, -16.3% getOrSet.
const (
	wyp0 = 0xa0761d6478bd642f
	wyp1 = 0xe7037ed1a0b428db
)

// hashString hashes a string using wyhash.
// Uses unsafe.Pointer for direct memory access - benchmarked 2.6x faster than maphash.String.
// Replacing with maphash causes -12% string-get throughput, -16% getOrSet throughput.
func hashString(s string) uint64 {
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
	// maxFreq caps the frequency counter. Paper uses 3; we use 7 for +0.9% meta, +0.8% zipf.
	maxFreq = 7

	// smallQueueRatio is the small queue size as per-mille of shard capacity.
	// 24.7% tuned empirically via parameter sweep.
	smallQueueRatio = 247 // per-mille (divide by 1000)

	// ghostFPRate is the bloom filter false positive rate for ghost tracking.
	ghostFPRate = 0.00001

	// minDeathRowSize is the minimum death row slots.
	// Death row size scales with capacity to match pre-sharding behavior.
	minDeathRowSize = 8
)

// s3fifo implements the S3-FIFO cache eviction algorithm.
// See "FIFO queues are all you need for cache eviction" (SOSP'23).
//
// The cache maintains three queues:
//   - Small (~10%): new entries
//   - Main (~90%): promoted entries
//   - Ghost: recently evicted keys (bloom filter, no values)
//
// New keys go to Small; keys in Ghost go directly to Main.
// Eviction from Small promotes warm entries (freq>0) to Main.
// Eviction from Main gives warm entries a second chance.
//
//nolint:govet // fieldalignment: padding prevents false sharing
type s3fifo[K comparable, V any] struct {
	mu      *xsync.RBMutex              // reader-biased mutex for write operations
	_       [32]byte                    // pad to cache line
	entries *xsync.Map[K, *entry[K, V]] // lock-free concurrent map
	small   entryList[K, V]
	main    entryList[K, V]

	// Ghost uses two rotating bloom filters for approximate FIFO eviction tracking.
	ghostActive  *bloomFilter
	ghostAging   *bloomFilter
	ghostFreqRng ghostFreqRing // ring buffer for ghost frequencies (replaces maps)
	ghostCap     int
	hasher       func(K) uint64

	// Death row: buffer of recently evicted items for instant resurrection.
	// Size scales with capacity (was per-shard × 8, now capacity/128).
	// Removal tested: -7.1% stringSet, -7.0% getOrSet, -3.9% stringGet throughput.
	// See experiment_results.md Phase 19, Exp A for details.
	deathRow    []*entry[K, V] // ring buffer of pending evictions
	deathRowPos int            // next slot to use

	capacity       int
	smallThresh    int // adaptive small queue threshold
	warmupComplete bool
	totalEntries   atomic.Int64

	// Type flags cache key type detection done once at construction.
	// Enables fast paths that avoid interface{} boxing on every get/set.
	// Removing these and using runtime type switches causes -6.4% throughput.
	keyIsInt    bool
	keyIsInt64  bool
	keyIsString bool
}

// ghostFreqRing is a fixed-size ring buffer for ghost frequency tracking.
// Replaces map[uint64]uint32 to eliminate allocation during ghost rotation.
// 256 entries with uint8 wrapping = zero-cost modulo.
// Improves: -5.1% string latency, -44.5% memory (119 → 66 bytes/item).
// See experiment_results.md Phase 20, Exp A for details.
type ghostFreqRing struct {
	hashes [256]uint64
	freqs  [256]uint32
	pos    uint8
}

func (r *ghostFreqRing) add(h uint64, freq uint32) {
	r.hashes[r.pos] = h
	r.freqs[r.pos] = freq
	r.pos++ // uint8 wraps at 256
}

// lookup performs O(256) linear scan to find frequency for hash.
// This is acceptable because: (1) 256 iterations is constant-time,
// (2) only called during eviction (not on every get), (3) cache-friendly
// sequential access, (4) replaces map that caused GC pressure.
func (r *ghostFreqRing) lookup(h uint64) (uint32, bool) {
	for i := range r.hashes {
		if r.hashes[i] == h {
			return r.freqs[i], true
		}
	}
	return 0, false
}

// entryList is an intrusive doubly-linked list. Zero value is valid.
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

func timeToNano(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

// entry is a cached key-value pair with eviction metadata.
type entry[K comparable, V any] struct {
	key        K
	value      V
	prev       *entry[K, V]
	next       *entry[K, V]
	hash       uint64        // cached key hash, avoids re-hashing on eviction (Phase 20, Exp B)
	expiryNano int64         // 0 means no expiry
	freq       atomic.Uint32 // access count, capped at maxFreq
	peakFreq   atomic.Uint32 // max freq seen, for ghost restore
	inSmall    bool
	onDeathRow bool // pending eviction, can be resurrected on access
}

func newS3FIFO[K comparable, V any](cfg *config) *s3fifo[K, V] {
	size := cfg.size
	if size <= 0 {
		size = 16384
	}

	c := &s3fifo[K, V]{
		mu:          xsync.NewRBMutex(),
		entries:     xsync.NewMap[K, *entry[K, V]](xsync.WithPresize(size)),
		capacity:    size,
		smallThresh: size * smallQueueRatio / 1000,
		ghostCap:    size,
		ghostActive: newBloomFilter(size, ghostFPRate),
		ghostAging:  newBloomFilter(size, ghostFPRate),
	}

	// Detect key type once to avoid type switch on every operation.
	var zk K
	switch any(zk).(type) {
	case int:
		c.keyIsInt = true
	case int64:
		c.keyIsInt64 = true
	case string:
		c.keyIsString = true
	}

	switch {
	case c.keyIsInt:
		c.hasher = func(k K) uint64 {
			return hashInt64(int64(*(*int)(unsafe.Pointer(&k))))
		}
	case c.keyIsInt64:
		c.hasher = func(k K) uint64 {
			return hashInt64(*(*int64)(unsafe.Pointer(&k)))
		}
	case c.keyIsString:
		c.hasher = func(k K) uint64 {
			return hashString(*(*string)(unsafe.Pointer(&k)))
		}
	default:
		c.hasher = func(k K) uint64 {
			switch v := any(k).(type) {
			case uint:
				//nolint:gosec // G115: intentional bit reinterpretation for hashing
				return hashInt64(int64(v))
			case uint64:
				//nolint:gosec // G115: intentional bit reinterpretation for hashing
				return hashInt64(int64(v))
			case string:
				return hashString(v)
			case fmt.Stringer:
				return hashString(v.String())
			default:
				return hashString(fmt.Sprintf("%v", k))
			}
		}
	}

	return c
}

// get retrieves a value, incrementing its frequency on hit.
func (c *s3fifo[K, V]) get(key K) (V, bool) {
	ent, ok := c.entries.Load(key)
	if !ok {
		var zero V
		return zero, false
	}
	if ent.onDeathRow {
		return c.resurrectFromDeathRow(key)
	}
	if ent.expiryNano != 0 && time.Now().UnixNano() > ent.expiryNano {
		var zero V
		return zero, false
	}
	if ent.freq.Load() < maxFreq {
		if newFreq := ent.freq.Add(1); newFreq > ent.peakFreq.Load() {
			ent.peakFreq.Store(newFreq)
		}
	}
	return ent.value, true
}

// resurrectFromDeathRow brings an entry back from pending eviction.
// Resurrected items go to main queue with freq=3 to protect them from immediate re-eviction.
//
// NOTE: Uses manual unlock instead of defer for -6% throughput improvement on hot path.
func (c *s3fifo[K, V]) resurrectFromDeathRow(key K) (V, bool) {
	c.mu.Lock()
	ent, ok := c.entries.Load(key)
	if !ok || !ent.onDeathRow {
		c.mu.Unlock()
		var zero V
		return zero, ok
	}

	// Remove from death row.
	for i := range c.deathRow {
		if c.deathRow[i] == ent {
			c.deathRow[i] = nil
			break
		}
	}

	// Resurrect to main queue with boosted frequency.
	ent.onDeathRow = false
	ent.inSmall = false
	ent.freq.Store(3)
	ent.peakFreq.Store(3)
	c.main.pushBack(ent)
	c.totalEntries.Add(1)

	val := ent.value
	c.mu.Unlock()
	return val, true
}

// set adds or updates a value. expiryNano of 0 means no expiry.
func (c *s3fifo[K, V]) set(key K, value V, expiryNano int64) {
	var h uint64
	if c.keyIsString {
		h = hashString(*(*string)(unsafe.Pointer(&key)))
	}
	c.setWithHash(key, value, expiryNano, h)
}

// setWithHash adds or updates a value. hash=0 means compute when needed.
//
// NOTE: Uses manual unlock instead of defer for -5% throughput improvement on hot path.
func (c *s3fifo[K, V]) setWithHash(key K, value V, expiryNano int64, hash uint64) {
	c.mu.Lock()

	// Update existing entry if present.
	if ent, exists := c.entries.Load(key); exists {
		ent.value = value
		ent.expiryNano = expiryNano
		if ent.freq.Load() < maxFreq {
			if newFreq := ent.freq.Add(1); newFreq > ent.peakFreq.Load() {
				ent.peakFreq.Store(newFreq)
			}
		}
		c.mu.Unlock()
		return
	}

	// Create new entry.
	ent := &entry[K, V]{key: key, value: value, expiryNano: expiryNano}

	// Cache hash for fast eviction (avoids re-hashing string keys).
	h := hash
	if h == 0 {
		h = c.hasher(key)
	}
	ent.hash = h

	full := c.totalEntries.Load() >= int64(c.capacity)

	// During warmup, skip eviction logic.
	if !c.warmupComplete && !full {
		ent.inSmall = true
		c.small.pushBack(ent)
		c.entries.Store(key, ent)
		c.totalEntries.Add(1)
		c.mu.Unlock()
		return
	}
	c.warmupComplete = true

	// Only check ghost when full (saves bloom lookups during fill).
	if full {
		inGhost := c.ghostActive.Contains(h) || c.ghostAging.Contains(h)
		ent.inSmall = !inGhost

		// Restore frequency from ghost for returning keys.
		if !ent.inSmall {
			if peak, ok := c.ghostFreqRng.lookup(h); ok {
				ent.freq.Store(peak)
				ent.peakFreq.Store(peak)
			}
		}

		if c.main.len > 0 && c.small.len <= c.smallThresh {
			c.evictFromMain()
		} else if c.small.len > 0 {
			c.evictFromSmall()
		}
	} else {
		ent.inSmall = true
	}

	if ent.inSmall {
		c.small.pushBack(ent)
	} else {
		c.main.pushBack(ent)
	}

	c.entries.Store(key, ent)
	c.totalEntries.Add(1)
	c.mu.Unlock()
}

func (c *s3fifo[K, V]) del(key K) {
	c.mu.Lock()
	defer c.mu.Unlock()

	ent, ok := c.entries.Load(key)
	if !ok {
		return
	}

	if ent.inSmall {
		c.small.remove(ent)
	} else {
		c.main.remove(ent)
	}

	c.entries.Delete(key)
	c.totalEntries.Add(-1)
}

// addToGhost records an evicted key for future admission decisions.
// Uses cached hash from entry to avoid re-hashing.
func (c *s3fifo[K, V]) addToGhost(h uint64, peakFreq uint32) {
	if !c.ghostActive.Contains(h) {
		c.ghostActive.Add(h)
		if peakFreq >= 2 {
			c.ghostFreqRng.add(h, peakFreq)
		}
	}
	if c.ghostActive.entries >= c.ghostCap {
		c.ghostAging.Reset()
		c.ghostActive, c.ghostAging = c.ghostAging, c.ghostActive
	}
}

// evictFromSmall evicts cold entries (freq<2) or promotes warm ones to main.
func (c *s3fifo[K, V]) evictFromSmall() {
	mcap := (c.capacity * 9) / 10

	for c.small.len > 0 {
		e := c.small.head
		f := e.freq.Load()

		if f < 2 {
			c.small.remove(e)
			c.sendToDeathRow(e)
			return
		}

		// Promote to main.
		c.small.remove(e)
		e.freq.Store(0)
		e.inSmall = false
		c.main.pushBack(e)

		if c.main.len > mcap {
			c.evictFromMain()
		}
	}
}

// evictFromMain evicts cold entries (freq==0) or gives warm ones a second chance.
//
// Deviation from paper: items that were once hot (peakFreq >= 4) get demoted to
// small queue with freq=1 instead of being evicted. This gives them another chance
// to prove themselves before final eviction. Improves Zipf workloads by +0.24%
// (concentrated at small cache sizes: +0.72% at 16K) with no regressions on other
// traces. See experiment_results.md Phase 10, Exp C for details.
func (c *s3fifo[K, V]) evictFromMain() {
	for c.main.len > 0 {
		e := c.main.head
		f := e.freq.Load()

		if f == 0 {
			c.main.remove(e)
			// Demote once-hot items to small queue for another chance.
			if e.peakFreq.Load() >= 4 {
				e.freq.Store(1)
				e.inSmall = true
				c.small.pushBack(e)
				return
			}
			c.sendToDeathRow(e)
			return
		}

		// Second chance.
		c.main.remove(e)
		e.freq.Store(f - 1)
		c.main.pushBack(e)
	}
}

// sendToDeathRow puts an entry on death row for potential resurrection.
// If death row is full, the oldest pending entry is truly evicted.
func (c *s3fifo[K, V]) sendToDeathRow(e *entry[K, V]) {
	// If death row slot is occupied, truly evict that entry first.
	if old := c.deathRow[c.deathRowPos]; old != nil {
		c.entries.Delete(old.key)
		c.addToGhost(old.hash, old.peakFreq.Load())
		old.onDeathRow = false
	}

	e.onDeathRow = true
	c.deathRow[c.deathRowPos] = e
	c.deathRowPos = (c.deathRowPos + 1) % len(c.deathRow)
	c.totalEntries.Add(-1)
}

func (c *s3fifo[K, V]) len() int {
	return c.entries.Size()
}

// getEntry returns an entry for testing purposes (not for production use).
func (c *s3fifo[K, V]) getEntry(key K) (*entry[K, V], bool) {
	return c.entries.Load(key)
}

func (c *s3fifo[K, V]) flush() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	n := c.entries.Size()
	c.entries.Clear()
	c.small.head, c.small.tail, c.small.len = nil, nil, 0
	c.main.head, c.main.tail, c.main.len = nil, nil, 0
	c.ghostActive.Reset()
	c.ghostAging.Reset()
	c.ghostFreqRng = ghostFreqRing{}
	for i := range c.deathRow {
		c.deathRow[i] = nil
	}
	c.deathRowPos = 0
	c.totalEntries.Store(0)
	return n
}
