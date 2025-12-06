// Package main benchmarks otter cache memory usage.
package main

import (
	"flag"
	"fmt"
	"runtime"
	"runtime/debug"
	"strconv"
	"time"

	"github.com/maypok86/otter/v2"
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

	cache := otter.Must(&otter.Options[string, []byte]{MaximumSize: *capacity})

	// Run 3 passes to ensure admission policies accept the items
	for range 3 {
		for i := range *capacity {
			key := "key-" + strconv.Itoa(i)
			val := make([]byte, *valSize)
			cache.Set(key, val)
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

	// Count items manually
	count := 0
	for i := range *capacity {
		if _, ok := cache.GetIfPresent("key-" + strconv.Itoa(i)); ok {
			count++
		}
	}

	fmt.Printf(`{"name":"otter", "items":%d, "bytes":%d}`, count, mem.Alloc)
}
