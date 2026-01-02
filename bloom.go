package fido

import (
	"math"
	"math/bits"
)

// bloomFilter is a probabilistic set membership test.
type bloomFilter struct {
	data    []uint64
	mask    uint64
	k       int // hash count
	entries int
}

// newBloomFilter creates a filter sized for capacity at fpRate false positive rate.
func newBloomFilter(capacity int, fpRate float64) *bloomFilter {
	if capacity < 1 {
		capacity = 1
	}

	ln2 := math.Log(2)
	m := float64(capacity) * -math.Log(fpRate) / (ln2 * ln2)

	mInt := uint64(1) << bits.Len64(uint64(m)-1) // round up to power of 2
	mInt = max(mInt, 64)

	k := min(max(int(float64(mInt)/float64(capacity)*ln2), 1), 16)

	return &bloomFilter{
		data: make([]uint64, mInt/64),
		mask: mInt - 1,
		k:    k,
	}
}

func (b *bloomFilter) Add(h uint64) {
	h1 := h
	h2 := h >> 32
	for i := range b.k {
		//nolint:gosec // G115: i is bounded by b.k which is capped at 16
		idx := (h1 + uint64(i)*h2) & b.mask
		b.data[idx/64] |= 1 << (idx % 64)
	}
	b.entries++
}

func (b *bloomFilter) Contains(h uint64) bool {
	h1 := h
	h2 := h >> 32

	for i := range b.k {
		//nolint:gosec // G115: i is bounded by b.k which is capped at 16
		idx := (h1 + uint64(i)*h2) & b.mask
		if b.data[idx/64]&(1<<(idx%64)) == 0 {
			return false
		}
	}
	return true
}

func (b *bloomFilter) Reset() {
	clear(b.data)
	b.entries = 0
}

// hashInt64 mixes an int64 into a well-distributed uint64 (SplitMix64).
func hashInt64(x int64) uint64 {
	//nolint:gosec // G115: intentional bit reinterpretation for hashing
	z := uint64(x)
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	z ^= z >> 31
	return z
}
