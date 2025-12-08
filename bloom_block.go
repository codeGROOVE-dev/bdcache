package sfcache

import "math"

// bloomBlock is a 512-bit block (8 uint64s = 64 bytes = typical cache line).
// All k hash bits for a single item are stored within one block for cache efficiency.
type bloomBlock [8]uint64

// blockBloomFilter is a cache-friendly blocked bloom filter.
// Instead of spreading k hash bits across random positions (poor cache locality),
// all k bits for a hash are confined to a single 512-bit block (one cache line).
//
// This reduces cache misses from k random accesses to 1 cache line access.
// Trade-off: Slightly different false positive characteristics, but maintains
// the same mathematical false positive rate when properly configured.
type blockBloomFilter struct {
	blocks  []bloomBlock
	mask    uint64 // number of blocks - 1 (for fast modulo)
	k       int    // number of hash functions
	entries int    // number of entries added
}

// newBlockBloomFilter creates a blocked bloom filter optimized for cache locality.
func newBlockBloomFilter(capacity int, fpRate float64) *blockBloomFilter {
	if capacity < 1 {
		capacity = 1
	}

	ln2 := math.Log(2)

	// Calculate target number of bits: m = -n * ln(p) / (ln(2)^2)
	m := float64(capacity) * -math.Log(fpRate) / (ln2 * ln2)

	// Round up to blocks (512 bits per block)
	const bitsPerBlock = 512
	numBlocks := int((m + bitsPerBlock - 1) / bitsPerBlock)

	// Ensure power of 2 for fast modulo
	numBlocks = int(nextPowerOf2(uint64(numBlocks)))
	if numBlocks < 1 {
		numBlocks = 1
	}

	// For blocked bloom filters, we need to account for reduced entropy
	// within each block. The effective bits per item is bitsPerBlock (not total bits)
	// because all k probes for an item are within one block.
	//
	// Use formula: k = ln(1/fpRate) / ln(2)
	// This is derived from FP rate = (1 - e^(-k*n/m))^k
	// For blocked filters where m_effective = 512 bits per block
	k := int(math.Ceil(-math.Log(fpRate) / ln2))
	if k < 1 {
		k = 1
	}
	if k > 16 {
		k = 16
	}

	// Increase block count if k suggests we need more bits
	// We want at least 8-10 bits per item within a block
	minBlocks := int(math.Ceil(float64(capacity*k) / float64(bitsPerBlock)))
	if numBlocks < minBlocks {
		numBlocks = int(nextPowerOf2(uint64(minBlocks)))
	}

	return &blockBloomFilter{
		blocks: make([]bloomBlock, numBlocks),
		mask:   uint64(numBlocks - 1),
		k:      k,
	}
}

// Add adds a hash to the blocked bloom filter.
// All k bit positions are within a single cache line (block).
func (b *blockBloomFilter) Add(h uint64) {
	// Select which block - use upper bits for block selection
	blockIdx := (h >> 32) & b.mask
	block := &b.blocks[blockIdx]

	// Use enhanced double hashing within the block for better distribution
	// Mix the bits better to get independent hash functions
	h1 := h & 0xFFFFFFFF        // Lower 32 bits
	h2 := (h >> 32) | (h << 32) // Upper 32 bits rotated

	// Set k bits within the same block (512 bits = 0-511)
	for i := 0; i < b.k; i++ {
		// Generate bit position with better mixing
		// Multiply by large prime for better distribution
		bitPos := (h1 + uint64(i)*h2 + uint64(i*i)*0x9e3779b1) & 511

		// Set the bit: bitPos / 64 = word index (0-7), bitPos % 64 = bit in word
		wordIdx := bitPos >> 6   // Divide by 64
		bitInWord := bitPos & 63 // Modulo 64
		block[wordIdx] |= 1 << bitInWord
	}

	b.entries++
}

// Contains checks if a hash might be in the filter.
// All k probes hit the same cache line, reducing memory stalls.
func (b *blockBloomFilter) Contains(h uint64) bool {
	// Select which block - must match Add()
	blockIdx := (h >> 32) & b.mask
	block := &b.blocks[blockIdx]

	// Check k bits within the same block using same hash mixing as Add
	h1 := h & 0xFFFFFFFF
	h2 := (h >> 32) | (h << 32)

	for i := 0; i < b.k; i++ {
		bitPos := (h1 + uint64(i)*h2 + uint64(i*i)*0x9e3779b1) & 511

		wordIdx := bitPos >> 6
		bitInWord := bitPos & 63

		if block[wordIdx]&(1<<bitInWord) == 0 {
			return false
		}
	}

	return true
}

// Reset clears the filter.
func (b *blockBloomFilter) Reset() {
	for i := range b.blocks {
		b.blocks[i] = bloomBlock{} // Zero out the block
	}
	b.entries = 0
}
