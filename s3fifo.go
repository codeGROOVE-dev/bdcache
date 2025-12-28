package multicache

import (
	"fmt"
	"math/bits"
	"runtime"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/puzpuzpuz/xsync/v4"
)

// wyhash constants.
const (
	wyp0 = 0xa0761d6478bd642f
	wyp1 = 0xe7037ed1a0b428db
)

// wyhashString hashes a string using wyhash.
// ~2.6x faster than maphash.String.
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

// maxFreq caps the frequency counter. Paper uses 3; we use 7 for +0.9% meta, +0.8% zipf.
const maxFreq = 7

// s3fifo implements the S3-FIFO cache eviction algorithm.
// See "FIFO queues are all you need for cache eviction" (SOSP'23).
//
// Each shard maintains three queues:
//   - Small (~10%): new entries
//   - Main (~90%): promoted entries
//   - Ghost: recently evicted keys (bloom filter, no values)
//
// New keys go to Small; keys in Ghost go directly to Main.
// Eviction from Small promotes warm entries (freq>0) to Main.
// Eviction from Main gives warm entries a second chance.

type s3fifo[K comparable, V any] struct {
	shards       []*shard[K, V]
	numShards    int
	shardMask    uint64 // numShards-1 for fast modulo (power-of-2 only)
	keyIsInt     bool
	keyIsInt64   bool
	keyIsString  bool
	totalEntries atomic.Int64
	capacity     int
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

func (r *ghostFreqRing) lookup(h uint64) (uint32, bool) {
	for i := range r.hashes {
		if r.hashes[i] == h {
			return r.freqs[i], true
		}
	}
	return 0, false
}

// shard is one partition of the cache. Each has its own lock and queues.
//
// Uses xsync.RBMutex (reader-biased, BRAVO algorithm) for write operations and
// xsync.Map (CLHT-based) for lock-free reads.
// Benchmarked: +191% string-get, +158% getorset, +412% int-get throughput.
// See experiment_results.md Phase 23 for details.
//
//nolint:govet // fieldalignment: padding prevents false sharing
type shard[K comparable, V any] struct {
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

	// Death row: small buffer of recently evicted items for instant resurrection.
	// Improves: +0.04% meta/tencentPhoto, +0.03% wikipedia, +8% set throughput.
	// See experiment_results.md Phase 19, Exp A for details.
	deathRow    [8]*entry[K, V] // ring buffer of pending evictions
	deathRowPos int             // next slot to use

	capacity       int
	smallThresh    int // adaptive small queue threshold
	warmupComplete bool
	parent         *s3fifo[K, V]
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

	// Sharding reduces lock contention at high thread counts.
	// Formula: max(GOMAXPROCS*16, size/256) balances shard count vs S3-FIFO queue size.
	n := min(max(runtime.GOMAXPROCS(0)*16, size/256), max(1, size/1024), maxShards)
	//nolint:gosec // G115: n bounded by [1, maxShards]
	n = 1 << (bits.Len(uint(n)) - 1) // round to power of 2
	scap := (size + n - 1) / n       // per-shard capacity

	c := &s3fifo[K, V]{
		shards:    make([]*shard[K, V], n),
		numShards: n,
		//nolint:gosec // G115: n bounded by [1, maxShards]
		shardMask: uint64(n - 1),
		capacity:  size,
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

	var hasher func(K) uint64
	switch {
	case c.keyIsInt:
		hasher = func(k K) uint64 {
			return hashInt64(int64(*(*int)(unsafe.Pointer(&k))))
		}
	case c.keyIsInt64:
		hasher = func(k K) uint64 {
			return hashInt64(*(*int64)(unsafe.Pointer(&k)))
		}
	case c.keyIsString:
		hasher = func(k K) uint64 {
			return wyhashString(*(*string)(unsafe.Pointer(&k)))
		}
	default:
		hasher = func(k K) uint64 {
			switch v := any(k).(type) {
			case uint:
				//nolint:gosec // G115: intentional bit reinterpretation for hashing
				return hashInt64(int64(v))
			case uint64:
				//nolint:gosec // G115: intentional bit reinterpretation for hashing
				return hashInt64(int64(v))
			case string:
				return wyhashString(v)
			case fmt.Stringer:
				return wyhashString(v.String())
			default:
				return wyhashString(fmt.Sprintf("%v", k))
			}
		}
	}

	for i := range n {
		c.shards[i] = &shard[K, V]{
			mu:          xsync.NewRBMutex(),
			entries:     xsync.NewMap[K, *entry[K, V]](xsync.WithPresize(scap)),
			capacity:    scap,
			smallThresh: scap * 247 / 1000, // 24.7% tuned via sweep
			ghostCap:    scap,
			ghostActive: newBloomFilter(scap, 0.00001),
			ghostAging:  newBloomFilter(scap, 0.00001),
			hasher:      hasher,
			parent:      c,
		}
	}

	return c
}

// shardIdx returns the shard index for a hash value.
func (c *s3fifo[K, V]) shardIdx(h uint64) int {
	//nolint:gosec // G115: result bounded by numShards
	return int(h & c.shardMask)
}

// shard returns the shard for key.
func (c *s3fifo[K, V]) shard(key K) *shard[K, V] {
	if c.keyIsInt {
		//nolint:gosec // G115: intentional wrap for fast modulo
		return c.shards[c.shardIdx(uint64(*(*int)(unsafe.Pointer(&key))))]
	}
	if c.keyIsInt64 {
		//nolint:gosec // G115: intentional wrap for fast modulo
		return c.shards[c.shardIdx(uint64(*(*int64)(unsafe.Pointer(&key))))]
	}
	if c.keyIsString {
		return c.shards[c.shardIdx(wyhashString(*(*string)(unsafe.Pointer(&key))))]
	}
	switch k := any(key).(type) {
	case uint:
		return c.shards[c.shardIdx(uint64(k))]
	case uint64:
		return c.shards[c.shardIdx(k)]
	case string:
		return c.shards[c.shardIdx(wyhashString(k))]
	case fmt.Stringer:
		return c.shards[c.shardIdx(wyhashString(k.String()))]
	default:
		return c.shards[c.shardIdx(wyhashString(fmt.Sprintf("%v", key)))]
	}
}

// get retrieves a value, incrementing its frequency on hit.
func (c *s3fifo[K, V]) get(key K) (V, bool) {
	// Fast paths for common key types avoid interface overhead.
	if c.keyIsString {
		s := c.shards[c.shardIdx(wyhashString(*(*string)(unsafe.Pointer(&key))))]
		ent, ok := s.entries.Load(key)
		if !ok {
			var zero V
			return zero, false
		}
		if ent.onDeathRow {
			return s.resurrectFromDeathRow(key)
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
	if c.keyIsInt {
		//nolint:gosec // G115: intentional wrap for fast modulo
		s := c.shards[c.shardIdx(uint64(*(*int)(unsafe.Pointer(&key))))]
		ent, ok := s.entries.Load(key)
		if !ok {
			var zero V
			return zero, false
		}
		if ent.onDeathRow {
			return s.resurrectFromDeathRow(key)
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
	return c.shard(key).get(key)
}

func (s *shard[K, V]) get(key K) (V, bool) {
	ent, ok := s.entries.Load(key)
	if !ok {
		var zero V
		return zero, false
	}
	if ent.onDeathRow {
		return s.resurrectFromDeathRow(key)
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
func (s *shard[K, V]) resurrectFromDeathRow(key K) (V, bool) {
	s.mu.Lock()
	ent, ok := s.entries.Load(key)
	if !ok || !ent.onDeathRow {
		s.mu.Unlock()
		var zero V
		return zero, ok
	}

	// Remove from death row.
	for i := range s.deathRow {
		if s.deathRow[i] == ent {
			s.deathRow[i] = nil
			break
		}
	}

	// Resurrect to main queue with boosted frequency.
	ent.onDeathRow = false
	ent.inSmall = false
	ent.freq.Store(3)
	ent.peakFreq.Store(3)
	s.main.pushBack(ent)
	s.parent.totalEntries.Add(1)

	val := ent.value
	s.mu.Unlock()
	return val, true
}

// set adds or updates a value. expiryNano of 0 means no expiry.
func (c *s3fifo[K, V]) set(key K, value V, expiryNano int64) {
	if c.keyIsString {
		h := wyhashString(*(*string)(unsafe.Pointer(&key)))
		c.shards[c.shardIdx(h)].setWithHash(key, value, expiryNano, h)
		return
	}
	if c.keyIsInt {
		//nolint:gosec // G115: intentional wrap for fast modulo
		c.shards[c.shardIdx(uint64(*(*int)(unsafe.Pointer(&key))))].setWithHash(key, value, expiryNano, 0)
		return
	}
	c.shard(key).setWithHash(key, value, expiryNano, 0)
}

// setWithHash adds or updates a value. hash=0 means compute when needed.
func (s *shard[K, V]) setWithHash(key K, value V, expiryNano int64, hash uint64) {
	s.mu.Lock()

	// Update existing entry if present.
	if ent, exists := s.entries.Load(key); exists {
		ent.value = value
		ent.expiryNano = expiryNano
		if ent.freq.Load() < maxFreq {
			if newFreq := ent.freq.Add(1); newFreq > ent.peakFreq.Load() {
				ent.peakFreq.Store(newFreq)
			}
		}
		s.mu.Unlock()
		return
	}

	// Create new entry.
	ent := &entry[K, V]{key: key, value: value, expiryNano: expiryNano}

	// Cache hash for fast eviction (avoids re-hashing string keys).
	h := hash
	if h == 0 {
		h = s.hasher(key)
	}
	ent.hash = h

	full := s.parent.totalEntries.Load() >= int64(s.parent.capacity)

	// During warmup, skip eviction logic.
	if !s.warmupComplete && !full {
		ent.inSmall = true
		s.small.pushBack(ent)
		s.entries.Store(key, ent)
		s.parent.totalEntries.Add(1)
		s.mu.Unlock()
		return
	}
	s.warmupComplete = true

	// Only check ghost when full (saves bloom lookups during fill).
	if full {
		inGhost := s.ghostActive.Contains(h) || s.ghostAging.Contains(h)
		ent.inSmall = !inGhost

		// Restore frequency from ghost for returning keys.
		if !ent.inSmall {
			if peak, ok := s.ghostFreqRng.lookup(h); ok {
				ent.freq.Store(peak)
				ent.peakFreq.Store(peak)
			}
		}

		if s.main.len > 0 && s.small.len <= s.smallThresh {
			s.evictFromMain()
		} else if s.small.len > 0 {
			s.evictFromSmall()
		}
	} else {
		ent.inSmall = true
	}

	if ent.inSmall {
		s.small.pushBack(ent)
	} else {
		s.main.pushBack(ent)
	}

	s.entries.Store(key, ent)
	s.parent.totalEntries.Add(1)
	s.mu.Unlock()
}

func (c *s3fifo[K, V]) del(key K) {
	c.shard(key).delete(key)
}

func (s *shard[K, V]) delete(key K) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ent, ok := s.entries.Load(key)
	if !ok {
		return
	}

	if ent.inSmall {
		s.small.remove(ent)
	} else {
		s.main.remove(ent)
	}

	s.entries.Delete(key)
	s.parent.totalEntries.Add(-1)
}

// addToGhost records an evicted key for future admission decisions.
// Uses cached hash from entry to avoid re-hashing.
func (s *shard[K, V]) addToGhost(h uint64, peakFreq uint32) {
	if !s.ghostActive.Contains(h) {
		s.ghostActive.Add(h)
		if peakFreq >= 2 {
			s.ghostFreqRng.add(h, peakFreq)
		}
	}
	if s.ghostActive.entries >= s.ghostCap {
		s.ghostAging.Reset()
		s.ghostActive, s.ghostAging = s.ghostAging, s.ghostActive
	}
}

// evictFromSmall evicts cold entries (freq<2) or promotes warm ones to main.
func (s *shard[K, V]) evictFromSmall() {
	mcap := (s.capacity * 9) / 10

	for s.small.len > 0 {
		e := s.small.head
		f := e.freq.Load()

		if f < 2 {
			s.small.remove(e)
			s.sendToDeathRow(e)
			return
		}

		// Promote to main.
		s.small.remove(e)
		e.freq.Store(0)
		e.inSmall = false
		s.main.pushBack(e)

		if s.main.len > mcap {
			s.evictFromMain()
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
func (s *shard[K, V]) evictFromMain() {
	for s.main.len > 0 {
		e := s.main.head
		f := e.freq.Load()

		if f == 0 {
			s.main.remove(e)
			// Demote once-hot items to small queue for another chance.
			if e.peakFreq.Load() >= 4 {
				e.freq.Store(1)
				e.inSmall = true
				s.small.pushBack(e)
				return
			}
			s.sendToDeathRow(e)
			return
		}

		// Second chance.
		s.main.remove(e)
		e.freq.Store(f - 1)
		s.main.pushBack(e)
	}
}

// sendToDeathRow puts an entry on death row for potential resurrection.
// If death row is full, the oldest pending entry is truly evicted.
func (s *shard[K, V]) sendToDeathRow(e *entry[K, V]) {
	// If death row slot is occupied, truly evict that entry first.
	if old := s.deathRow[s.deathRowPos]; old != nil {
		s.entries.Delete(old.key)
		s.addToGhost(old.hash, old.peakFreq.Load())
		old.onDeathRow = false
	}

	e.onDeathRow = true
	s.deathRow[s.deathRowPos] = e
	s.deathRowPos = (s.deathRowPos + 1) % len(s.deathRow)
	s.parent.totalEntries.Add(-1)
}

func (c *s3fifo[K, V]) len() int {
	total := 0
	for _, s := range c.shards {
		total += s.entries.Size()
	}
	return total
}

func (c *s3fifo[K, V]) flush() int {
	total := 0
	for _, s := range c.shards {
		total += s.flush()
	}
	c.totalEntries.Store(0)
	return total
}

func (s *shard[K, V]) flush() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	n := s.entries.Size()
	s.entries.Clear()
	s.small.head, s.small.tail, s.small.len = nil, nil, 0
	s.main.head, s.main.tail, s.main.len = nil, nil, 0
	s.ghostActive.Reset()
	s.ghostAging.Reset()
	s.ghostFreqRng = ghostFreqRing{}
	for i := range s.deathRow {
		s.deathRow[i] = nil
	}
	s.deathRowPos = 0
	return n
}

// getEntry returns an entry for testing purposes (not for production use).
func (s *shard[K, V]) getEntry(key K) (*entry[K, V], bool) {
	return s.entries.Load(key)
}
