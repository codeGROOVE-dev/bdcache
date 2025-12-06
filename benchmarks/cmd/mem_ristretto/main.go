// Package main benchmarks ristretto cache memory usage.
package main

import (
	"flag"
	"fmt"
	"runtime"
	"runtime/debug"
	"strconv"
	"time"

	"github.com/dgraph-io/ristretto"
)

var keepAlive any //nolint:unused // prevents compiler from optimizing away allocations in benchmarks

func main() {
	_ = flag.Int("iter", 100000, "unused in this mode")
	capacity := flag.Int("cap", 25000, "capacity")
	valSize := flag.Int("valSize", 1024, "value size")
	flag.Parse()

	//nolint:revive // explicit GC required for accurate memory benchmarking
	runtime.GC()
	debug.FreeOSMemory()

	// Ristretto config: NumCounters should be 10x MaxCost for best performance
	cache, err := ristretto.NewCache(&ristretto.Config{
		NumCounters:        int64(*capacity * 10),
		MaxCost:            int64(*capacity),
		BufferItems:        64 * 1024, // Increase buffer to avoid drops during ingestion
		IgnoreInternalCost: true,
	})
	if err != nil {
		panic(fmt.Sprintf("ristretto.NewCache failed: %v", err))
	}

	// Run 3 passes to ensure admission policies accept the items
	for range 3 {
		for i := range *capacity {
			key := "key-" + strconv.Itoa(i)
			val := make([]byte, *valSize)
			cache.Set(key, val, 1) // Cost 1 per item
		}
	}
	cache.Wait()

	keepAlive = cache

	//nolint:revive // explicit GC required for accurate memory benchmarking
	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	//nolint:revive // explicit GC required for accurate memory benchmarking
	runtime.GC()
	debug.FreeOSMemory()

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	count := 0
	for i := range *capacity {
		if _, ok := cache.Get("key-" + strconv.Itoa(i)); ok {
			count++
		}
	}

	fmt.Printf(`{"name":"ristretto", "items":%d, "bytes":%d}`, count, mem.Alloc)
}
