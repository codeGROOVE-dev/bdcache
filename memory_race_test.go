//go:build !race

package fido

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Tests with concurrent cache access are skipped under race detector.
// The seqlock value storage uses intentional "benign races" that the
// race detector cannot understand.

func TestCache_Fetch_CacheHitDuringSingleflight(t *testing.T) {
	cache := New[string, int](Size(1000))

	var wg sync.WaitGroup
	loaderCalls := atomic.Int32{}

	// Start first loader that's slow
	wg.Go(func() {
		if _, err := cache.Fetch("key1", func() (int, error) {
			loaderCalls.Add(1)
			// While loader is running, another goroutine populates cache
			time.Sleep(100 * time.Millisecond)
			return 42, nil
		}); err != nil {
			t.Errorf("Fetch error: %v", err)
		}
	})

	// Let first goroutine start and enter singleflight
	time.Sleep(10 * time.Millisecond)

	// While first is waiting, directly set the value in cache
	cache.Set("key1", 99)

	// Start second loader that should wait for first
	wg.Go(func() {
		val, err := cache.Fetch("key1", func() (int, error) {
			loaderCalls.Add(1)
			return 77, nil
		})
		if err != nil {
			t.Errorf("Fetch error: %v", err)
			return
		}
		// Second should get either 99 (from cache) or 42 (from first loader)
		if val != 99 && val != 42 {
			t.Errorf("unexpected value: %d", val)
		}
	})

	wg.Wait()

	t.Logf("loader calls: %d", loaderCalls.Load())
}
