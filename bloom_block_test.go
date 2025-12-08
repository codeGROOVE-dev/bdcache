package sfcache

import (
	"math/rand"
	"testing"
)

func TestBlockBloomFilter(t *testing.T) {
	capacity := 1000
	fpRate := 0.01
	bf := newBlockBloomFilter(capacity, fpRate)

	// Add items with better hash distribution
	hashes := make([]uint64, capacity)
	for i := range capacity {
		// Use a better mixing function for test hashes
		h := uint64(i)
		h = h*0x9e3779b97f4a7c15 + 0x6a09e667f3bcc908
		hashes[i] = h
		bf.Add(h)
	}

	// All added items should be found
	for i, h := range hashes {
		if !bf.Contains(h) {
			t.Errorf("Hash %d (%d) should be in filter", i, h)
		}
	}

	// Check false positive rate with different hash pattern
	falsePositives := 0
	testSize := 10000
	for i := 0; i < testSize; i++ {
		h := uint64(i+capacity) * 0x9e3779b97f4a7c15 // Different pattern
		if bf.Contains(h) {
			falsePositives++
		}
	}

	actualFPRate := float64(falsePositives) / float64(testSize)
	t.Logf("Block bloom filter: capacity=%d, k=%d, blocks=%d, FP rate=%.4f (target=%.4f)",
		capacity, bf.k, len(bf.blocks), actualFPRate, fpRate)

	// Block bloom filters have slightly higher FP rate due to constrained bits
	// Allow 3-4x tolerance for blocked design
	if actualFPRate > fpRate*4 {
		t.Errorf("False positive rate too high: %.4f > %.4f (4x target)", actualFPRate, fpRate*4)
	}
}

func TestBlockBloomFilterReset(t *testing.T) {
	bf := newBlockBloomFilter(100, 0.01)

	// Add some items
	for i := 0; i < 50; i++ {
		bf.Add(uint64(i))
	}

	// Reset
	bf.Reset()

	// Should not contain items anymore (might have false positives but unlikely)
	found := 0
	for i := 0; i < 50; i++ {
		if bf.Contains(uint64(i)) {
			found++
		}
	}

	// After reset, we expect very few false positives
	if found > 2 {
		t.Errorf("After reset, found %d items (expected ~0-2 false positives)", found)
	}
}

func TestBlockBloomVsStandardBloom(t *testing.T) {
	capacity := 5000
	fpRate := 0.01

	standard := newBloomFilter(capacity, fpRate)
	blocked := newBlockBloomFilter(capacity, fpRate)

	t.Logf("Standard: k=%d, bits=%d, memory=%d bytes",
		standard.k, len(standard.data)*64, len(standard.data)*8)
	t.Logf("Blocked:  k=%d, blocks=%d, memory=%d bytes",
		blocked.k, len(blocked.blocks), len(blocked.blocks)*64)

	// Add same items to both
	hashes := make([]uint64, capacity)
	for i := range capacity {
		hashes[i] = uint64(rand.Int63())
		standard.Add(hashes[i])
		blocked.Add(hashes[i])
	}

	// Both should contain all items
	for i, h := range hashes {
		if !standard.Contains(h) {
			t.Errorf("Standard filter missing hash %d", i)
		}
		if !blocked.Contains(h) {
			t.Errorf("Block filter missing hash %d", i)
		}
	}

	// Compare false positive rates
	testSize := 10000
	standardFP := 0
	blockedFP := 0

	for i := 0; i < testSize; i++ {
		h := uint64(rand.Int63())
		if standard.Contains(h) {
			standardFP++
		}
		if blocked.Contains(h) {
			blockedFP++
		}
	}

	standardRate := float64(standardFP) / float64(testSize)
	blockedRate := float64(blockedFP) / float64(testSize)

	t.Logf("Standard FP rate: %.4f", standardRate)
	t.Logf("Blocked FP rate:  %.4f", blockedRate)

	// Both should be within reasonable bounds
	// Block filters can have higher FP rate due to design constraints
	if standardRate > fpRate*3 {
		t.Errorf("Standard FP rate too high: %.4f", standardRate)
	}
	if blockedRate > fpRate*5 {
		t.Errorf("Blocked FP rate too high: %.4f", blockedRate)
	}
}

// Benchmark comparison
func BenchmarkStandardBloomAdd(b *testing.B) {
	bf := newBloomFilter(10000, 0.01)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bf.Add(uint64(i))
	}
}

func BenchmarkBlockBloomAdd(b *testing.B) {
	bf := newBlockBloomFilter(10000, 0.01)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bf.Add(uint64(i))
	}
}

func BenchmarkStandardBloomContains(b *testing.B) {
	bf := newBloomFilter(10000, 0.01)
	for i := 0; i < 10000; i++ {
		bf.Add(uint64(i))
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bf.Contains(uint64(i % 10000))
	}
}

func BenchmarkBlockBloomContains(b *testing.B) {
	bf := newBlockBloomFilter(10000, 0.01)
	for i := 0; i < 10000; i++ {
		bf.Add(uint64(i))
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bf.Contains(uint64(i % 10000))
	}
}
