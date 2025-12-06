// Package main benchmarks sfcache memory usage.
package main

import (
	"flag"
	"fmt"
	"runtime"
	"runtime/debug"
	"strconv"
	"time"

	"github.com/codeGROOVE-dev/sfcache"
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

	cache := sfcache.New[string, []byte](sfcache.Size(*capacity))

	// Run 3 passes to ensure admission policies (like TinyLFU/Ristretto) accept the items
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

	fmt.Printf(`{"name":"sfcache", "items":%d, "bytes":%d}`, cache.Len(), mem.Alloc)
}
