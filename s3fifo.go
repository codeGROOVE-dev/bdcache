package fido

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
	// maxFreq caps the frequency counter for eviction. Paper uses 3; 5 tuned via binary search.
	// WARNING: Must be >= 2. Setting to 1 creates infinite loop in eviction (items with
	// freq=1 get promoted instead of evicted, causing evictFromSmall to never return true).
	maxFreq = 5

	// maxPeakFreq caps peakFreq for death row admission decisions.
	maxPeakFreq = 21

	// ghostFPRate is the bloom filter false positive rate for ghost tracking.
	ghostFPRate = 0.00001

	// deathRowThresholdPerMille scales the death row admission threshold.
	// 1000 = average peakFreq. Wide plateau from 10-1500 (all ~61.62%).
	deathRowThresholdPerMille = 1000

	// minDeathRowSize is the minimum death row slots.
	// Death row size scales with capacity to match pre-sharding behavior.
	minDeathRowSize = 8
)

// smallRatio returns the optimal small queue ratio (per-mille) for a capacity.
// Tuned via binary search across cache sizes (hitrate benchmarks):
//
//	  8K: 148 (14.8%) - small caches need aggressive filtering
//	 16K: 123 (12.3%)
//	 32K: 137 (13.7%)
//	 64K: 132 (13.2%)
//	128K: 122 (12.2%) - minimum ratio
//	256K: 152 (15.2%) - large caches need more filtering again
//
// Uses piecewise linear interpolation between measured points.
func smallRatio(capacity int) int {
	// Tuning points from binary search (capacity -> ratio per-mille).
	// Interpolate linearly between points.
	type point struct{ cap, ratio int }
	points := [...]point{
		{8000, 148},
		{16000, 123},
		{32000, 137},
		{64000, 132},
		{128000, 122},
		{256000, 152},
	}

	// Clamp to bounds.
	if capacity <= points[0].cap {
		return points[0].ratio
	}
	if capacity >= points[len(points)-1].cap {
		return points[len(points)-1].ratio
	}

	// Find segment and interpolate.
	for i := 1; i < len(points); i++ {
		if capacity <= points[i].cap {
			p0, p1 := points[i-1], points[i]
			// Linear interpolation: ratio = r0 + (r1-r0) * (cap-c0) / (c1-c0)
			return p0.ratio + (p1.ratio-p0.ratio)*(capacity-p0.cap)/(p1.cap-p0.cap)
		}
	}
	return points[len(points)-1].ratio
}

// ghostRatio returns the optimal ghost queue ratio (per-mille) for a capacity.
// Tuned via binary search across cache sizes (hitrate benchmarks):
//
//	  8K:  875 ( 88%) - smaller caches need less ghost tracking
//	 16K: 1000 (100%)
//	 32K: 1200 (120%)
//	 64K: 1750 (175%)
//	128K: 2075 (208%)
//	256K: 2225 (223%) - larger caches benefit from more ghost tracking
//
// Monotonic increase: larger caches have more unique keys cycling through,
// so they need proportionally larger ghost queues to track evictions.
func ghostRatio(capacity int) int {
	// Tuning points from binary search (capacity -> ratio per-mille).
	// Interpolate linearly between points.
	type point struct{ cap, ratio int }
	points := [...]point{
		{8000, 875},
		{16000, 1000},
		{32000, 1200},
		{64000, 1750},
		{128000, 2075},
		{256000, 2225},
	}

	// Clamp to bounds.
	if capacity <= points[0].cap {
		return points[0].ratio
	}
	if capacity >= points[len(points)-1].cap {
		return points[len(points)-1].ratio
	}

	// Find segment and interpolate.
	for i := 1; i < len(points); i++ {
		if capacity <= points[i].cap {
			p0, p1 := points[i-1], points[i]
			// Linear interpolation: ratio = r0 + (r1-r0) * (cap-c0) / (c1-c0)
			return p0.ratio + (p1.ratio-p0.ratio)*(capacity-p0.cap)/(p1.cap-p0.cap)
		}
	}
	return points[len(points)-1].ratio
}

// s3fifo implements the S3-FIFO cache eviction algorithm.
// See "FIFO queues are all you need for cache eviction" (SOSP'23).
//
// The cache maintains three queues:
//   - Small (~14%): new entries (filter for one-hit wonders)
//   - Main (~86%): promoted entries (protected from scans)
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
	// Items on death row remain in memory, so larger death row effectively
	// increases cache size. Increase sparingly.
	deathRow    []*entry[K, V] // ring buffer of pending evictions
	deathRowPos int            // next slot to use

	// Entry recycling to reduce allocations during eviction.
	freeEntry *entry[K, V]

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
// Uses uint32 hashes (sufficient for ghost queue collision avoidance).
type ghostFreqRing struct {
	hashes [256]uint32
	freqs  [256]uint32
	pos    uint8
}

func (r *ghostFreqRing) add(h uint32, freq uint32) {
	r.hashes[r.pos] = h
	r.freqs[r.pos] = freq
	r.pos++ // uint8 wraps at 256
}

// lookup performs O(256) linear scan to find frequency for hash.
// This is acceptable because: (1) 256 iterations is constant-time,
// (2) only called during eviction (not on every get), (3) cache-friendly
// sequential access, (4) replaces map that caused GC pressure.
func (r *ghostFreqRing) lookup(h uint32) (uint32, bool) {
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

func timeToSec(t time.Time) uint32 {
	if t.IsZero() {
		return 0
	}
	//nolint:gosec // G115: Unix seconds fit in uint32 until year 2106
	return uint32(t.Unix())
}

// entry is a cached key-value pair with eviction metadata.
// Uses seqlock for zero-allocation value storage.
//
//nolint:govet // fieldalignment: generic struct layout varies by type parameters
type entry[K comparable, V any] struct {
	key       K
	value     V             // stored inline, protected by seqlock
	seq       atomic.Uint64 // seqlock: odd = write in progress
	prev      *entry[K, V]
	next      *entry[K, V]
	hash64    uint64        // full 64-bit hash for bloom filter (avoids re-hashing on eviction)
	expirySec atomic.Uint32 // 0 means no expiry; seconds since Unix epoch
	freqFlags atomic.Uint32 // bits 0-3: freq, bits 4-9: peakFreq, bit 30: inSmall, bit 31: onDeathRow
}

// storeValue stores a value using seqlock protocol (zero allocations).
// Uses CAS to ensure only one writer can be active at a time, preventing
// sequence corruption when multiple goroutines update the same entry.
func (e *entry[K, V]) storeValue(v V) {
	for {
		seq := e.seq.Load()
		if seq&1 != 0 {
			// Another writer is in progress, spin
			continue
		}
		if e.seq.CompareAndSwap(seq, seq+1) {
			// Successfully marked as writing (seq is now odd)
			e.value = v
			e.seq.Store(seq + 2) // End write (seq is now even)
			return
		}
	}
}

// loadValue loads a value using seqlock protocol.
func (e *entry[K, V]) loadValue() (V, bool) {
	for range 1000 { // bounded retry
		s1 := e.seq.Load() // acquire semantics
		if s1&1 != 0 {
			continue // write in progress, retry
		}
		v := e.value
		s2 := e.seq.Load() // acquire semantics
		if s2 == s1 {
			return v, s1 > 0 // s1>0 means value was stored at least once
		}
	}
	var zero V
	return zero, false
}

// Bitfield constants for freqFlags.
const (
	freqMask      = 0xF  // bits 0-3 for freq (0-15)
	peakFreqShift = 4    // peakFreq starts at bit 4
	peakFreqMask  = 0x3F // bits 4-9 for peakFreq (0-63), accessed after shift
	inSmallBit    = 1 << 30
	onDeathRowBit = 1 << 31
)

// freq returns the access frequency (0-15).
func (e *entry[K, V]) freq() uint32 { return e.freqFlags.Load() & freqMask }

// peakFreq returns the peak frequency for ghost restoration (0-63).
func (e *entry[K, V]) peakFreq() uint32 {
	return (e.freqFlags.Load() >> peakFreqShift) & peakFreqMask
}

// setFreq sets the access frequency via CAS loop.
func (e *entry[K, V]) setFreq(f uint32) {
	for {
		cur := e.freqFlags.Load()
		updated := (cur &^ freqMask) | (f & freqMask)
		if e.freqFlags.CompareAndSwap(cur, updated) {
			return
		}
	}
}

// incFreq increments freq up to limit via CAS loop.
func (e *entry[K, V]) incFreq(limit uint32) {
	for {
		cur := e.freqFlags.Load()
		f := cur & freqMask
		if f >= limit {
			return
		}
		updated := (cur &^ freqMask) | (f + 1)
		if e.freqFlags.CompareAndSwap(cur, updated) {
			return
		}
	}
}

// incPeakFreq increments peakFreq up to limit via CAS loop.
func (e *entry[K, V]) incPeakFreq(limit uint32) {
	for {
		cur := e.freqFlags.Load()
		p := (cur >> peakFreqShift) & peakFreqMask
		if p >= limit {
			return
		}
		// Clear old peakFreq bits, set new value, preserve freq and flags
		updated := (cur &^ (peakFreqMask << peakFreqShift)) | ((p + 1) << peakFreqShift)
		if e.freqFlags.CompareAndSwap(cur, updated) {
			return
		}
	}
}

// setFreqPeak sets freq and peakFreq, preserving flags. Must be called under mutex.
func (e *entry[K, V]) setFreqPeak(f, p uint32) {
	cur := e.freqFlags.Load()
	flags := cur & (inSmallBit | onDeathRowBit)
	e.freqFlags.Store((f & freqMask) | ((p & peakFreqMask) << peakFreqShift) | flags)
}

// inSmall returns true if entry is in small queue.
func (e *entry[K, V]) inSmall() bool { return e.freqFlags.Load()&inSmallBit != 0 }

// onDeathRow returns true if entry is pending eviction.
func (e *entry[K, V]) onDeathRow() bool { return e.freqFlags.Load()&onDeathRowBit != 0 }

// setInSmall sets the inSmall flag. Must be called under mutex.
func (e *entry[K, V]) setInSmall(v bool) {
	cur := e.freqFlags.Load()
	if v {
		e.freqFlags.Store(cur | inSmallBit)
	} else {
		e.freqFlags.Store(cur &^ inSmallBit)
	}
}

// setOnDeathRow sets the onDeathRow flag. Must be called under mutex.
func (e *entry[K, V]) setOnDeathRow(v bool) {
	cur := e.freqFlags.Load()
	if v {
		e.freqFlags.Store(cur | onDeathRowBit)
	} else {
		e.freqFlags.Store(cur &^ onDeathRowBit)
	}
}

func newS3FIFO[K comparable, V any](cfg *config) *s3fifo[K, V] {
	size := cfg.size
	if size <= 0 {
		size = 16384
	}

	// Scale death row with capacity. Items on death row remain in memory, so larger
	// death row effectively increases cache size. Never use divisor < 768 or death row
	// becomes a second cache that distorts benchmark results.
	deathRowSize := max(minDeathRowSize, size/768)

	c := &s3fifo[K, V]{
		mu:          xsync.NewRBMutex(),
		entries:     xsync.NewMap[K, *entry[K, V]](xsync.WithPresize(size)),
		capacity:    size,
		smallThresh: size * smallRatio(size) / 1000,
		ghostCap:    size * ghostRatio(size) / 1000,
		ghostActive: newBloomFilter(size, ghostFPRate),
		ghostAging:  newBloomFilter(size, ghostFPRate),
		deathRow:    make([]*entry[K, V], deathRowSize),
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
	if ent.onDeathRow() {
		return c.resurrectFromDeathRow(key)
	}
	//nolint:gosec // G115: Unix seconds fit in uint32 until year 2106
	if exp := ent.expirySec.Load(); exp != 0 && uint32(time.Now().Unix()) > exp {
		var zero V
		return zero, false
	}
	// Hot path: single Load to check if both counters need increment.
	// Under Zipf, most accesses hit entries already at max - skip CAS loops.
	flags := ent.freqFlags.Load()
	if flags&freqMask < maxFreq {
		ent.incFreq(maxFreq)
	}
	if (flags>>peakFreqShift)&peakFreqMask < maxPeakFreq {
		ent.incPeakFreq(maxPeakFreq)
	}
	return ent.loadValue()
}

// resurrectFromDeathRow brings an entry back from pending eviction.
// Resurrected items go to main queue with freq=3 to protect them from immediate re-eviction.
//
// NOTE: Uses manual unlock instead of defer for -6% throughput improvement on hot path.
func (c *s3fifo[K, V]) resurrectFromDeathRow(key K) (V, bool) {
	c.mu.Lock()
	ent, ok := c.entries.Load(key)
	if !ok || !ent.onDeathRow() {
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
	ent.setOnDeathRow(false)
	ent.setInSmall(false)
	ent.setFreqPeak(3, 3)
	c.main.pushBack(ent)
	c.totalEntries.Add(1)

	// Evict to maintain capacity after resurrection.
	if c.totalEntries.Load() > int64(c.capacity) {
		c.evictOne()
	}

	val, ok := ent.loadValue()
	c.mu.Unlock()
	return val, ok
}

// set adds or updates a value. expirySec of 0 means no expiry.
func (c *s3fifo[K, V]) set(key K, value V, expirySec uint32) {
	var h uint64
	if c.keyIsString {
		h = hashString(*(*string)(unsafe.Pointer(&key)))
	}
	c.setWithHash(key, value, expirySec, h)
}

// updateEntry updates an existing entry's value and frequency counters.
func (*s3fifo[K, V]) updateEntry(ent *entry[K, V], value V, expirySec uint32) {
	ent.storeValue(value)
	ent.expirySec.Store(expirySec)
	// Hot path: single Load to check if counters need increment.
	flags := ent.freqFlags.Load()
	if flags&freqMask < maxFreq {
		ent.incFreq(maxFreq)
	}
	if (flags>>peakFreqShift)&peakFreqMask < maxPeakFreq {
		ent.incPeakFreq(maxPeakFreq)
	}
}

// setWithHash adds or updates a value. hash=0 means compute when needed.
//
// NOTE: Uses manual unlock instead of defer for -5% throughput improvement on hot path.
func (c *s3fifo[K, V]) setWithHash(key K, value V, expirySec uint32, hash uint64) {
	// Fast path: lock-free update for existing entries.
	if ent, exists := c.entries.Load(key); exists {
		c.updateEntry(ent, value, expirySec)
		return
	}

	// Slow path: need lock for new entry insertion.
	c.mu.Lock()

	// Double-check after acquiring lock.
	if ent, exists := c.entries.Load(key); exists {
		c.updateEntry(ent, value, expirySec)
		c.mu.Unlock()
		return
	}

	// Allocate-first: reuse recycled entry or allocate new one.
	ent := c.freeEntry
	if ent != nil {
		c.freeEntry = nil
		ent.key = key
		ent.freqFlags.Store(0) // clears freq, peakFreq, inSmall, onDeathRow
	} else {
		ent = &entry[K, V]{key: key}
	}
	ent.storeValue(value)
	ent.expirySec.Store(expirySec)

	// Cache full hash for bloom filter (avoids re-hashing on eviction).
	h := hash
	if h == 0 {
		h = c.hasher(key)
	}
	ent.hash64 = h

	full := c.totalEntries.Load() >= int64(c.capacity)

	// During warmup, skip eviction logic.
	if !c.warmupComplete && !full {
		ent.setInSmall(true)
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
		ent.setInSmall(!inGhost)

		// Restore frequency from ghost for returning keys.
		if !ent.inSmall() {
			//nolint:gosec // G115: intentional truncation to 32-bit hash
			if peak, ok := c.ghostFreqRng.lookup(uint32(h)); ok {
				ent.setFreqPeak(peak, peak)
			}
		}

		c.evictOne()
	} else {
		ent.setInSmall(true)
	}

	if ent.inSmall() {
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

	if ent.inSmall() {
		c.small.remove(ent)
	} else {
		c.main.remove(ent)
	}

	c.entries.Delete(key)
	c.totalEntries.Add(-1)
}

// addToGhost records an evicted key's hash for future admission decisions.
// Bloom filter uses full 64-bit hash for proper double hashing (h2 = h >> 32).
// Frequency ring uses lower 32 bits (sufficient for collision avoidance).
func (c *s3fifo[K, V]) addToGhost(h64 uint64, peakFreq uint32) {
	c.ghostActive.Add(h64)
	if peakFreq >= 1 {
		//nolint:gosec // G115: intentional truncation to 32-bit hash
		c.ghostFreqRng.add(uint32(h64), peakFreq)
	}
	if c.ghostActive.entries >= c.ghostCap {
		c.ghostAging.Reset()
		c.ghostActive, c.ghostAging = c.ghostAging, c.ghostActive
	}
}

// evictOne evicts a single entry, preferring main when small is at or below threshold.
// Called after adding an entry when the cache is at capacity.
func (c *s3fifo[K, V]) evictOne() {
	for {
		if c.main.len > 0 && c.small.len <= c.smallThresh {
			if c.evictFromMain() {
				return
			}
		} else if c.small.len > 0 {
			if c.evictFromSmall() {
				return
			}
		}
	}
}

// evictFromSmall evicts cold entries (freq<2) or promotes warm ones to main.
// Returns true if an entry was actually evicted.
func (c *s3fifo[K, V]) evictFromSmall() bool {
	mcap := (c.capacity * 9) / 10

	for c.small.len > 0 {
		e := c.small.head
		f := e.freq()

		if f < 2 {
			c.small.remove(e)
			c.sendToDeathRow(e)
			return true
		}

		// Promote to main.
		c.small.remove(e)
		e.setFreq(0)
		e.setInSmall(false)
		c.main.pushBack(e)

		if c.main.len > mcap {
			if c.evictFromMain() {
				return true
			}
		}
	}
	return false
}

// evictFromMain evicts cold entries (freq==0) or gives warm ones a second chance.
// Returns true if an entry was actually evicted.
//
// Deviation from paper: items that were accessed at least once (peakFreq >= 1)
// get demoted to small queue with freq=1 instead of being evicted. This gives
// them another chance to prove themselves before final eviction.
// Improves meta by +4%, wikipedia by +1%, and most other traces.
func (c *s3fifo[K, V]) evictFromMain() bool {
	for c.main.len > 0 {
		e := c.main.head
		f := e.freq()

		if f == 0 {
			c.main.remove(e)
			// Demote once-hot items to small queue for another chance.
			if e.peakFreq() >= 1 {
				e.setFreq(1)
				e.setInSmall(true)
				c.small.pushBack(e)
				return false // demotion, not eviction
			}
			c.sendToDeathRow(e)
			return true
		}

		// Second chance.
		c.main.remove(e)
		e.setFreq(f - 1)
		c.main.pushBack(e)
	}
	return false
}

// sampleAvgPeakFreq samples up to 5 entries from main and returns the average peakFreq (rounded up).
// Used as adaptive threshold for death row admission.
func (c *s3fifo[K, V]) sampleAvgPeakFreq() uint32 {
	const sampleSize = 5
	var sum, count uint32

	// Sample from main queue (higher frequency entries, more selective threshold).
	for e := c.main.head; e != nil && count < sampleSize; e = e.next {
		sum += e.peakFreq()
		count++
	}

	if count == 0 {
		return 1 // minimum threshold
	}
	// Round up: (sum + count - 1) / count
	return (sum + count - 1) / count
}

// sendToDeathRow puts an entry on death row for potential resurrection.
// If death row is full, the oldest pending entry is truly evicted.
func (c *s3fifo[K, V]) sendToDeathRow(e *entry[K, V]) {
	// Compute adaptive threshold by sampling current entries.
	// Only admit entries with above-threshold frequency to death row.
	threshold := c.sampleAvgPeakFreq() * deathRowThresholdPerMille / 1000
	if threshold == 0 {
		threshold = 1
	}
	if e.peakFreq() < threshold {
		c.entries.Delete(e.key)
		c.addToGhost(e.hash64, e.peakFreq())
		e.prev, e.next = nil, nil
		c.freeEntry = e
		c.totalEntries.Add(-1)
		return
	}

	// If death row slot is occupied, truly evict that entry first.
	if old := c.deathRow[c.deathRowPos]; old != nil {
		c.entries.Delete(old.key)
		c.addToGhost(old.hash64, old.peakFreq())
		old.setOnDeathRow(false)
		// Recycle entry for reuse (reduces allocations).
		old.prev, old.next = nil, nil
		c.freeEntry = old
	}

	e.setOnDeathRow(true)
	c.deathRow[c.deathRowPos] = e
	c.deathRowPos = (c.deathRowPos + 1) % len(c.deathRow)
	c.totalEntries.Add(-1)
}

func (c *s3fifo[K, V]) len() int {
	// Return live entries only (excludes items pending eviction on death row).
	return int(c.totalEntries.Load())
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
	clear(c.deathRow)
	c.deathRowPos = 0
	c.totalEntries.Store(0)
	return n
}
