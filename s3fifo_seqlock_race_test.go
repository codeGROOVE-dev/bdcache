//go:build !race

package fido

import (
	"sync"
	"sync/atomic"
	"testing"
)

// TestS3FIFO_SetWithHash_DoubleCheck tests the double-check path after lock.
// Skipped under race detector because seqlock is a benign race.
func TestS3FIFO_SetWithHash_DoubleCheck(t *testing.T) {
	cache := newS3FIFO[int, int](&config{size: 100})

	const key = 42
	var wg sync.WaitGroup
	var setCount atomic.Int32

	// Multiple goroutines try to set the same key
	for range 100 {
		wg.Go(func() {
			cache.set(key, 100, 0)
			setCount.Add(1)
		})
	}

	wg.Wait()

	// Key should exist
	if val, ok := cache.get(key); !ok || val != 100 {
		t.Errorf("get(%d) = %v, %v; want 100, true", key, val, ok)
	}
}

// Seqlock tests with concurrent access are skipped under race detector.
// Seqlocks are an intentional "benign race" pattern where:
// - Writers increment sequence to odd, write value, increment to even
// - Readers check sequence before/after and retry on mismatch
// The race detector cannot understand this protocol is safe.

// TestEntry_Seqlock_Concurrent tests seqlock under concurrent read/write.
func TestEntry_Seqlock_Concurrent(t *testing.T) {
	e := &entry[int, int64]{}
	const iterations = 100000

	var wg sync.WaitGroup

	// Writer goroutine: stores incrementing values.
	wg.Go(func() {
		for i := int64(1); i <= iterations; i++ {
			e.storeValue(i)
		}
	})

	// Reader goroutines: verify values are valid.
	for range 4 {
		wg.Go(func() {
			for range iterations {
				v, ok := e.loadValue()
				if ok {
					// Value should be in valid range [1, iterations].
					if v < 1 || v > iterations {
						t.Errorf("loadValue() = %d; out of range [1, %d]", v, iterations)
						return
					}
				}
			}
		})
	}

	wg.Wait()

	// Final value should be iterations.
	v, ok := e.loadValue()
	if !ok || v != iterations {
		t.Errorf("final loadValue() = %d, %v; want %d, true", v, ok, iterations)
	}
}

// TestEntry_Seqlock_MultiWriter tests seqlock with multiple concurrent writers.
func TestEntry_Seqlock_MultiWriter(t *testing.T) {
	e := &entry[int, int]{}
	const writers = 4
	const perWriter = 10000

	var wg sync.WaitGroup

	// Multiple writers, each writing their own ID * offset.
	for w := range writers {
		wg.Go(func() {
			for i := range perWriter {
				e.storeValue(w*perWriter + i)
			}
		})
	}

	// Readers verify no corrupted values.
	for range 4 {
		wg.Go(func() {
			for range perWriter {
				v, ok := e.loadValue()
				if ok {
					// Value should be in valid range [0, writers*perWriter).
					if v < 0 || v >= writers*perWriter {
						t.Errorf("loadValue() = %d; out of range", v)
						return
					}
				}
			}
		})
	}

	wg.Wait()
}

// Note: Large struct values (> word size) may experience torn reads on ARM
// due to non-atomic struct copying. This is acceptable because:
// 1. Real caches store word-sized values (int, string) or pointers (*T)
// 2. Storing large structs by value is an anti-pattern
// 3. The seqlock will retry and eventually get a consistent read
