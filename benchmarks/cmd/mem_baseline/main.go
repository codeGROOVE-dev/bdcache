// Package main benchmarks baseline map memory usage.
package main

import (
	"flag"
	"fmt"
	"runtime"
	"runtime/debug"
	"strconv"
	"time"
)

var keepAlive any //nolint:unused // prevents compiler from optimizing away allocations in benchmarks

func main() {
	_ = flag.Int("iter", 100000, "unused in this mode unless target is 0")
	capacity := flag.Int("cap", 25000, "capacity")
	valSize := flag.Int("valSize", 1024, "value size")
	target := flag.Int("target", 0, "if > 0, just fill map with this many items and exit")
	flag.Parse()

	//nolint:revive // explicit GC required for accurate memory benchmarking
	runtime.GC()
	debug.FreeOSMemory()

	// Use target as capacity if specified (fair comparison for partial fills)
	mapCap := *capacity
	if *target > 0 {
		mapCap = *target
	}
	m := make(map[string][]byte, mapCap)

	if *target > 0 {
		// Just fill with N unique items
		for i := range *target {
			key := "key-" + strconv.Itoa(i)
			val := make([]byte, *valSize)
			m[key] = val
		}
	} else {
		// Fallback: fill up to capacity
		for i := range *capacity {
			key := "key-" + strconv.Itoa(i)
			val := make([]byte, *valSize)
			m[key] = val
		}
	}

	keepAlive = m

	//nolint:revive // explicit GC required for accurate memory benchmarking
	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	//nolint:revive // explicit GC required for accurate memory benchmarking
	runtime.GC()
	debug.FreeOSMemory()

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	fmt.Printf(`{"name":"baseline", "items":%d, "bytes":%d}`, len(m), mem.Alloc)
}
