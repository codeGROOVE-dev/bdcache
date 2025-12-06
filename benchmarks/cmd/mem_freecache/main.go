// Package main benchmarks freecache memory usage.
package main

import (
	"flag"
	"fmt"
	"runtime"
	"runtime/debug"
	"strconv"
	"time"

	"github.com/coocood/freecache"
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

	// Freecache size in bytes
	overhead := 256 // per entry overhead estimate
	size := *capacity * (*valSize + overhead)
	cache := freecache.NewCache(size)

	// Run 3 passes to ensure admission policies accept the items
	for range 3 {
		for i := range *capacity {
			key := "key-" + strconv.Itoa(i)
			val := make([]byte, *valSize)
			if err := cache.Set([]byte(key), val, 0); err != nil {
				panic(fmt.Sprintf("freecache.Set failed: %v", err))
			}
		}
	}

	keepAlive = cache

	//nolint:revive // explicit GC required for accurate memory benchmarking
	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	//nolint:revive // explicit GC required for accurate memory benchmarking
	runtime.GC()
	debug.FreeOSMemory()

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	fmt.Printf(`{"name":"freecache", "items":%d, "bytes":%d}`, cache.EntryCount(), mem.Alloc)
}
