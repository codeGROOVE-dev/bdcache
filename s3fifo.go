package multicache

import (
	"fmt"
	"math/bits"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
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
	shardMask    uint64 // numShards-1 for fast modulo
	keyIsInt     bool
	keyIsInt64   bool
	keyIsString  bool
	totalEntries atomic.Int64
	capacity     int
}

// shard is one partition of the cache. Each has its own lock and queues.
//
//nolint:govet // fieldalignment: padding prevents false sharing
type shard[K comparable, V any] struct {
	mu      sync.RWMutex
	_       [40]byte // pad to cache line
	entries map[K]*entry[K, V]
	small   entryList[K, V]
	main    entryList[K, V]

	// Ghost uses two rotating bloom filters for approximate FIFO eviction tracking.
	ghostActive     *bloomFilter
	ghostAging      *bloomFilter
	ghostFreqActive map[uint64]uint32 // freq at eviction, for restoring returning keys
	ghostFreqAging  map[uint64]uint32
	ghostCap        int
	hasher          func(K) uint64

	capacity       int
	smallThresh    int          // adaptive small queue threshold
	freeEntries    *entry[K, V] // reuse pool
	warmupComplete bool         // skip eviction until first fill
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
	expiryNano int64         // 0 means no expiry
	freq       atomic.Uint32 // access count, capped at maxFreq
	peakFreq   atomic.Uint32 // max freq seen, for ghost restore
	inSmall    bool
}

func newS3FIFO[K comparable, V any](cfg *config) *s3fifo[K, V] {
	capacity := cfg.size
	if capacity <= 0 {
		capacity = 16384
	}

	// Sharding reduces RWMutex contention at high thread counts.
	// At 32 threads: 21x throughput improvement for Get, 6x for Set vs single shard.
	// Formula: max(GOMAXPROCS*16, capacity/256) balances shard count vs S3-FIFO queue size.
	targetShards := max(runtime.GOMAXPROCS(0)*16, capacity/256)
	const minEntriesPerShard = 1024 // fewer entries per shard hurts S3-FIFO eviction accuracy
	maxByCapacity := max(1, capacity/minEntriesPerShard)
	nshards := min(targetShards, maxByCapacity, maxShards)
	//nolint:gosec // G115: nshards bounded by [1, maxShards]
	nshards = 1 << (bits.Len(uint(nshards)) - 1) // round to power of 2

	shardCap := (capacity + nshards - 1) / nshards

	cache := &s3fifo[K, V]{
		shards:    make([]*shard[K, V], nshards),
		numShards: nshards,
		//nolint:gosec // G115: nshards bounded by [1, maxShards]
		shardMask: uint64(nshards - 1),
		capacity:  capacity,
	}

	// Detect key type once to avoid type switch on every operation.
	var zk K
	switch any(zk).(type) {
	case int:
		cache.keyIsInt = true
	case int64:
		cache.keyIsInt64 = true
	case string:
		cache.keyIsString = true
	}

	const ghostRatio = 1.0 // ghost size as fraction of shard capacity

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
		ghostCap := max(int(float64(shardCap)*ghostRatio), 1)
		cache.shards[i] = &shard[K, V]{
			capacity:        shardCap,
			smallThresh:     shardCap * 247 / 1000, // 24.7% - tuned via sweep from 10-35% to maximize wins across production traces
			ghostCap:        ghostCap,
			entries:         make(map[K]*entry[K, V], shardCap),
			ghostActive:     newBloomFilter(ghostCap, 0.00001),
			ghostAging:      newBloomFilter(ghostCap, 0.00001),
			ghostFreqActive: make(map[uint64]uint32, ghostCap),
			ghostFreqAging:  make(map[uint64]uint32, ghostCap),
			hasher:          hasher,
			parent:          cache,
		}
	}

	return cache
}

func (s *shard[K, V]) allocEntry() *entry[K, V] {
	if s.freeEntries != nil {
		e := s.freeEntries
		s.freeEntries = e.next
		e.next, e.prev = nil, nil
		return e
	}
	return &entry[K, V]{}
}

func (s *shard[K, V]) freeEntry(e *entry[K, V]) {
	var zk K
	var zv V
	e.key = zk
	e.value = zv
	e.expiryNano = 0
	e.freq.Store(0)
	e.peakFreq.Store(0)
	e.inSmall = false
	e.prev = nil
	e.next = s.freeEntries
	s.freeEntries = e
}

// shard returns the shard for key.
func (c *s3fifo[K, V]) shard(key K) *shard[K, V] {
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

// get retrieves a value, incrementing its frequency on hit.
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
			newFreq := ent.freq.Add(1)
			if newFreq > ent.peakFreq.Load() {
				ent.peakFreq.Store(newFreq)
			}
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
			newFreq := ent.freq.Add(1)
			if newFreq > ent.peakFreq.Load() {
				ent.peakFreq.Store(newFreq)
			}
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
	val := ent.value
	expiry := ent.expiryNano
	s.mu.RUnlock()

	if expiry != 0 && time.Now().UnixNano() > expiry {
		var zero V
		return zero, false
	}

	if ent.freq.Load() < maxFreq {
		newFreq := ent.freq.Add(1)
		if newFreq > ent.peakFreq.Load() {
			ent.peakFreq.Store(newFreq)
		}
	}
	return val, true
}

// set adds or updates a value. expiryNano of 0 means no expiry.
func (c *s3fifo[K, V]) set(key K, value V, expiryNano int64) {
	if c.keyIsString {
		h := wyhashString(*(*string)(unsafe.Pointer(&key)))
		c.shards[h&c.shardMask].setWithHash(key, value, expiryNano, h)
		return
	}
	if c.keyIsInt {
		//nolint:gosec // G115: intentional wrap for fast modulo
		c.shards[uint64(*(*int)(unsafe.Pointer(&key)))&c.shardMask].setWithHash(key, value, expiryNano, 0)
		return
	}
	c.shard(key).setWithHash(key, value, expiryNano, 0)
}

// setWithHash adds or updates a value. hash=0 means compute when needed.
func (s *shard[K, V]) setWithHash(key K, value V, expiryNano int64, hash uint64) {
	s.mu.Lock()

	if ent, ok := s.entries[key]; ok {
		ent.value = value
		ent.expiryNano = expiryNano
		if ent.freq.Load() < maxFreq {
			newFreq := ent.freq.Add(1)
			if newFreq > ent.peakFreq.Load() {
				ent.peakFreq.Store(newFreq)
			}
		}
		s.mu.Unlock()
		return
	}

	ent := s.allocEntry()
	ent.key = key
	ent.value = value
	ent.expiryNano = expiryNano

	full := s.parent.totalEntries.Load() >= int64(s.parent.capacity)

	// During warmup, skip eviction logic.
	if !s.warmupComplete && !full {
		ent.inSmall = true
		s.small.pushBack(ent)
		s.entries[key] = ent
		s.parent.totalEntries.Add(1)
		s.mu.Unlock()
		return
	}
	s.warmupComplete = true

	// Only check ghost when full (saves bloom lookups during fill).
	if full {
		h := hash
		if h == 0 {
			h = s.hasher(key)
		}
		inGhost := s.ghostActive.Contains(h) || s.ghostAging.Contains(h)
		ent.inSmall = !inGhost

		// Restore 50% of peak frequency for items returning from ghost.
		// Helps zipf +0.26%, meta +0.10% by recognizing previously-popular keys.
		if !ent.inSmall {
			if peak, ok := s.ghostFreqActive[h]; ok {
				restored := peak / 2
				ent.freq.Store(restored)
				ent.peakFreq.Store(restored)
			} else if peak, ok := s.ghostFreqAging[h]; ok {
				restored := peak / 2
				ent.freq.Store(restored)
				ent.peakFreq.Store(restored)
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

	s.entries[key] = ent
	s.parent.totalEntries.Add(1)
	s.mu.Unlock()
}

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
	s.freeEntry(ent)
	s.parent.totalEntries.Add(-1)
}

// addToGhost records an evicted key for future admission decisions.
func (s *shard[K, V]) addToGhost(key K, peakFreq uint32) {
	h := s.hasher(key)
	if !s.ghostActive.Contains(h) {
		s.ghostActive.Add(h)
		if peakFreq >= 2 {
			s.ghostFreqActive[h] = peakFreq
		}
	}
	if s.ghostActive.entries >= s.ghostCap {
		s.ghostAging.Reset()
		s.ghostFreqAging = make(map[uint64]uint32, s.ghostCap)
		s.ghostActive, s.ghostAging = s.ghostAging, s.ghostActive
		s.ghostFreqActive, s.ghostFreqAging = s.ghostFreqAging, s.ghostFreqActive
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
			delete(s.entries, e.key)
			s.addToGhost(e.key, e.peakFreq.Load())
			s.freeEntry(e)
			s.parent.totalEntries.Add(-1)
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
			delete(s.entries, e.key)
			s.addToGhost(e.key, e.peakFreq.Load())
			s.freeEntry(e)
			s.parent.totalEntries.Add(-1)
			return
		}

		// Second chance.
		s.main.remove(e)
		e.freq.Store(f - 1)
		s.main.pushBack(e)
	}
}

func (c *s3fifo[K, V]) len() int {
	total := 0
	for i := range c.shards {
		s := c.shards[i]
		s.mu.RLock()
		total += len(s.entries)
		s.mu.RUnlock()
	}
	return total
}

func (c *s3fifo[K, V]) flush() int {
	total := 0
	for i := range c.shards {
		total += c.shards[i].flush()
	}
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
	s.ghostFreqActive = make(map[uint64]uint32, s.ghostCap)
	s.ghostFreqAging = make(map[uint64]uint32, s.ghostCap)
	return n
}
