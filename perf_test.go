//go:build !race

package fido

import (
	"testing"
	"time"
)

func TestCache_ReadPerformance(t *testing.T) {
	cache := New[int, int]()

	// Populate cache
	for i := range 10000 {
		cache.Set(i, i)
	}

	// Warm up
	for i := range 1000 {
		cache.Get(i % 10000)
	}

	// Measure read performance
	const iterations = 100000
	start := time.Now()
	for i := range iterations {
		cache.Get(i % 10000)
	}
	elapsed := time.Since(start)
	nsPerOp := float64(elapsed.Nanoseconds()) / float64(iterations)

	const maxNsPerOp = 80.0
	if nsPerOp > maxNsPerOp {
		t.Errorf("single-threaded read performance: %.2f ns/op exceeds %.0f ns/op threshold", nsPerOp, maxNsPerOp)
	}
	t.Logf("single-threaded read performance: %.2f ns/op", nsPerOp)
}
