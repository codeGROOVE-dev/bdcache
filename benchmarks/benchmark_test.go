//nolint:errcheck,thelper // benchmark code - errors not critical for performance measurement
package benchmarks

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"math/rand/v2"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codeGROOVE-dev/bdcache"
	"github.com/coocood/freecache"
	"github.com/dgraph-io/ristretto"
	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/maypok86/otter/v2"
	"github.com/vmihailenco/go-tinylfu"
)

// TestBenchmarkSuite runs the full benchmark comparison and outputs formatted tables.
// Run with: go test -run=TestBenchmarkSuite -v
func TestBenchmarkSuite(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping benchmark suite in short mode")
	}

	fmt.Println()
	fmt.Println("╔══════════════════════════════════════════════════════════════════════════════╗")
	fmt.Println("║                        BDCACHE BENCHMARK COMPARISON                          ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════════════════════╝")

	runHitRateBenchmark()
	runPerformanceBenchmark()
	runConcurrentBenchmark()
}

// =============================================================================
// Hit Rate Comparison
// =============================================================================

const (
	hitRateKeySpace = 500000
	hitRateWorkload = 750000
	hitRateAlpha    = 0.99
)

func runHitRateBenchmark() {
	fmt.Println()
	fmt.Println("### Hit Rate (Zipf α=0.99, 750K ops)")
	fmt.Println()
	fmt.Println("| Cache      | Size=0.1% | Size=1% | Size=10% |")
	fmt.Println("|------------|-----------|---------|----------|")

	workload := generateWorkload(hitRateWorkload, hitRateKeySpace, hitRateAlpha, 42)
	cacheSizes := []int{500, 5000, 50000}

	caches := []struct {
		name string
		fn   func([]int, int) float64
	}{
		{"bdcache", hitRateBdcache},
		{"otter", hitRateOtter},
		{"ristretto", hitRateRistretto},
		{"tinylfu", hitRateTinyLFU},
		{"freecache", hitRateFreecache},
		{"lru", hitRateLRU},
	}

	for _, c := range caches {
		rates := make([]float64, len(cacheSizes))
		for i, size := range cacheSizes {
			rates[i] = c.fn(workload, size)
		}
		fmt.Printf("| %-10s |   %5.2f%% | %5.2f%% |  %5.2f%% |\n",
			c.name, rates[0], rates[1], rates[2])
	}
}

func generateWorkload(n, keySpace int, alpha float64, seed uint64) []int {
	rng := rand.New(rand.NewPCG(seed, seed+1))
	keys := make([]int, n)
	for i := range n {
		u := rng.Float64()
		keys[i] = int(math.Floor(float64(keySpace) * math.Pow(u, 1.0/(1.0-alpha))))
	}
	return keys
}

func hitRateBdcache(workload []int, cacheSize int) float64 {
	ctx := context.Background()
	cache, _ := bdcache.New[int, int](ctx, bdcache.WithMemorySize(cacheSize))
	var hits int
	for _, key := range workload {
		if _, found, _ := cache.Get(ctx, key); found {
			hits++
		} else {
			_ = cache.Set(ctx, key, key, 0)
		}
	}
	return float64(hits) / float64(len(workload)) * 100
}

func hitRateOtter(workload []int, cacheSize int) float64 {
	cache := otter.Must(&otter.Options[int, int]{MaximumSize: cacheSize})
	var hits int
	for _, key := range workload {
		if _, found := cache.GetIfPresent(key); found {
			hits++
		} else {
			cache.Set(key, key)
		}
	}
	return float64(hits) / float64(len(workload)) * 100
}

func hitRateRistretto(workload []int, cacheSize int) float64 {
	cache, _ := ristretto.NewCache(&ristretto.Config{
		NumCounters: int64(cacheSize * 10),
		MaxCost:     int64(cacheSize),
		BufferItems: 64,
	})
	defer cache.Close()
	var hits int
	for _, key := range workload {
		if _, found := cache.Get(key); found {
			hits++
		} else {
			cache.Set(key, key, 1)
			cache.Wait()
		}
	}
	return float64(hits) / float64(len(workload)) * 100
}

func hitRateLRU(workload []int, cacheSize int) float64 {
	cache, _ := lru.New[int, int](cacheSize)
	var hits int
	for _, key := range workload {
		if _, found := cache.Get(key); found {
			hits++
		} else {
			cache.Add(key, key)
		}
	}
	return float64(hits) / float64(len(workload)) * 100
}

func hitRateTinyLFU(workload []int, cacheSize int) float64 {
	cache := tinylfu.New(cacheSize, cacheSize*10)
	var hits int
	for _, key := range workload {
		k := strconv.Itoa(key)
		if _, found := cache.Get(k); found {
			hits++
		} else {
			cache.Set(&tinylfu.Item{Key: k, Value: key})
		}
	}
	return float64(hits) / float64(len(workload)) * 100
}

func hitRateFreecache(workload []int, cacheSize int) float64 {
	// freecache uses bytes, estimate ~64 bytes per entry
	cacheBytes := cacheSize * 64
	if cacheBytes < 512*1024 {
		cacheBytes = 512 * 1024 // minimum 512KB
	}
	cache := freecache.NewCache(cacheBytes)
	var hits int
	var buf [8]byte
	for _, key := range workload {
		k := strconv.Itoa(key)
		if _, err := cache.Get([]byte(k)); err == nil {
			hits++
		} else {
			binary.LittleEndian.PutUint64(buf[:], uint64(key))
			cache.Set([]byte(k), buf[:], 0)
		}
	}
	return float64(hits) / float64(len(workload)) * 100
}

// =============================================================================
// Performance Comparison (using Go benchmark infrastructure)
// =============================================================================

const perfCacheSize = 10000

type perfResult struct {
	name     string
	getNs    float64
	setNs    float64
	getB     int64
	setB     int64
	getAlloc int64
	setAlloc int64
}

func runPerformanceBenchmark() {
	results := []perfResult{
		measurePerf("bdcache", benchBdcacheGet, benchBdcacheSet),
		measurePerf("otter", benchOtterGet, benchOtterSet),
		measurePerf("ristretto", benchRistrettoGet, benchRistrettoSet),
		measurePerf("tinylfu", benchTinyLFUGet, benchTinyLFUSet),
		measurePerf("freecache", benchFreecacheGet, benchFreecacheSet),
		measurePerf("lru", benchLRUGet, benchLRUSet),
	}

	// Sort by get ns/op (lowest first)
	for i := range len(results) - 1 {
		for j := i + 1; j < len(results); j++ {
			if results[j].getNs < results[i].getNs {
				results[i], results[j] = results[j], results[i]
			}
		}
	}

	fmt.Println()
	fmt.Println("### Single-Threaded Latency (sorted by Get)")
	fmt.Println()
	fmt.Println("| Cache      | Get ns/op | Get B/op | Get allocs | Set ns/op | Set B/op | Set allocs |")
	fmt.Println("|------------|-----------|----------|------------|-----------|----------|------------|")

	for _, r := range results {
		fmt.Printf("| %-10s | %9.1f | %8d | %10d | %9.1f | %8d | %10d |\n",
			r.name,
			r.getNs, r.getB, r.getAlloc,
			r.setNs, r.setB, r.setAlloc)
	}
}

func measurePerf(name string, getFn, setFn func(b *testing.B)) perfResult {
	getResult := testing.Benchmark(getFn)
	setResult := testing.Benchmark(setFn)

	return perfResult{
		name:     name,
		getNs:    float64(getResult.NsPerOp()),
		setNs:    float64(setResult.NsPerOp()),
		getB:     getResult.AllocedBytesPerOp(),
		setB:     setResult.AllocedBytesPerOp(),
		getAlloc: getResult.AllocsPerOp(),
		setAlloc: setResult.AllocsPerOp(),
	}
}

func benchBdcacheGet(b *testing.B) {
	ctx := context.Background()
	cache, _ := bdcache.New[int, int](ctx, bdcache.WithMemorySize(perfCacheSize))
	for i := range perfCacheSize {
		_ = cache.Set(ctx, i, i, 0)
	}
	b.ResetTimer()
	for i := range b.N {
		_, _, _ = cache.Get(ctx, i%perfCacheSize)
	}
}

func benchBdcacheSet(b *testing.B) {
	ctx := context.Background()
	cache, _ := bdcache.New[int, int](ctx, bdcache.WithMemorySize(perfCacheSize))
	b.ResetTimer()
	for i := range b.N {
		_ = cache.Set(ctx, i%perfCacheSize, i, 0)
	}
}

func benchOtterGet(b *testing.B) {
	cache := otter.Must(&otter.Options[int, int]{MaximumSize: perfCacheSize})
	for i := range perfCacheSize {
		cache.Set(i, i)
	}
	b.ResetTimer()
	for i := range b.N {
		cache.GetIfPresent(i % perfCacheSize)
	}
}

func benchOtterSet(b *testing.B) {
	cache := otter.Must(&otter.Options[int, int]{MaximumSize: perfCacheSize})
	b.ResetTimer()
	for i := range b.N {
		cache.Set(i%perfCacheSize, i)
	}
}

func benchRistrettoGet(b *testing.B) {
	cache, _ := ristretto.NewCache(&ristretto.Config{
		NumCounters: int64(perfCacheSize * 10),
		MaxCost:     int64(perfCacheSize),
		BufferItems: 64,
	})
	for i := range perfCacheSize {
		cache.Set(i, i, 1)
	}
	cache.Wait()
	b.ResetTimer()
	for i := range b.N {
		cache.Get(i % perfCacheSize)
	}
}

func benchRistrettoSet(b *testing.B) {
	cache, _ := ristretto.NewCache(&ristretto.Config{
		NumCounters: int64(perfCacheSize * 10),
		MaxCost:     int64(perfCacheSize),
		BufferItems: 64,
	})
	b.ResetTimer()
	for i := range b.N {
		cache.Set(i%perfCacheSize, i, 1)
	}
}

func benchLRUGet(b *testing.B) {
	cache, _ := lru.New[int, int](perfCacheSize)
	for i := range perfCacheSize {
		cache.Add(i, i)
	}
	b.ResetTimer()
	for i := range b.N {
		cache.Get(i % perfCacheSize)
	}
}

func benchLRUSet(b *testing.B) {
	cache, _ := lru.New[int, int](perfCacheSize)
	b.ResetTimer()
	for i := range b.N {
		cache.Add(i%perfCacheSize, i)
	}
}

func benchTinyLFUGet(b *testing.B) {
	cache := tinylfu.NewSync(perfCacheSize, perfCacheSize*10)
	for i := range perfCacheSize {
		cache.Set(&tinylfu.Item{Key: strconv.Itoa(i), Value: i})
	}
	b.ResetTimer()
	for i := range b.N {
		cache.Get(strconv.Itoa(i % perfCacheSize))
	}
}

func benchTinyLFUSet(b *testing.B) {
	cache := tinylfu.NewSync(perfCacheSize, perfCacheSize*10)
	b.ResetTimer()
	for i := range b.N {
		cache.Set(&tinylfu.Item{Key: strconv.Itoa(i % perfCacheSize), Value: i})
	}
}

func benchFreecacheGet(b *testing.B) {
	cache := freecache.NewCache(perfCacheSize * 256)
	var buf [8]byte
	for i := range perfCacheSize {
		binary.LittleEndian.PutUint64(buf[:], uint64(i))
		cache.Set([]byte(strconv.Itoa(i)), buf[:], 0)
	}
	b.ResetTimer()
	for i := range b.N {
		cache.Get([]byte(strconv.Itoa(i % perfCacheSize)))
	}
}

func benchFreecacheSet(b *testing.B) {
	cache := freecache.NewCache(perfCacheSize * 256)
	var buf [8]byte
	b.ResetTimer()
	for i := range b.N {
		binary.LittleEndian.PutUint64(buf[:], uint64(i))
		cache.Set([]byte(strconv.Itoa(i%perfCacheSize)), buf[:], 0)
	}
}

// =============================================================================
// Exported Benchmarks (for go test -bench=.)
// =============================================================================

func BenchmarkBdcacheGet(b *testing.B)   { benchBdcacheGet(b) }
func BenchmarkBdcacheSet(b *testing.B)   { benchBdcacheSet(b) }
func BenchmarkOtterGet(b *testing.B)     { benchOtterGet(b) }
func BenchmarkOtterSet(b *testing.B)     { benchOtterSet(b) }
func BenchmarkRistrettoGet(b *testing.B) { benchRistrettoGet(b) }
func BenchmarkRistrettoSet(b *testing.B) { benchRistrettoSet(b) }
func BenchmarkTinyLFUGet(b *testing.B)   { benchTinyLFUGet(b) }
func BenchmarkTinyLFUSet(b *testing.B)   { benchTinyLFUSet(b) }
func BenchmarkFreecacheGet(b *testing.B) { benchFreecacheGet(b) }
func BenchmarkFreecacheSet(b *testing.B) { benchFreecacheSet(b) }
func BenchmarkLRUGet(b *testing.B)       { benchLRUGet(b) }
func BenchmarkLRUSet(b *testing.B)       { benchLRUSet(b) }

// =============================================================================
// Concurrent Throughput Benchmarks
// =============================================================================

const concurrentDuration = 1 * time.Second

type concurrentResult struct {
	name   string
	getQPS float64
	setQPS float64
}

func runConcurrentBenchmark() {
	threadCounts := []int{1, 4, 8, runtime.NumCPU()}
	caches := []string{"bdcache", "otter", "ristretto", "tinylfu", "freecache", "lru"}

	for _, threads := range threadCounts {
		results := make([]concurrentResult, len(caches))
		for i, name := range caches {
			results[i] = concurrentResult{
				name:   name,
				getQPS: measureConcurrentQPS(name, threads, false),
				setQPS: measureConcurrentQPS(name, threads, true),
			}
		}

		// Sort by get QPS (highest first)
		for i := range len(results) - 1 {
			for j := i + 1; j < len(results); j++ {
				if results[j].getQPS > results[i].getQPS {
					results[i], results[j] = results[j], results[i]
				}
			}
		}

		fmt.Println()
		if threads == 1 {
			fmt.Println("### Single-Threaded Throughput (mixed read/write)")
		} else {
			fmt.Printf("### Concurrent Throughput (mixed read/write): %d threads\n", threads)
		}
		fmt.Println()
		fmt.Println("| Cache      | Get QPS    | Set QPS    |")
		fmt.Println("|------------|------------|------------|")

		for _, r := range results {
			fmt.Printf("| %-10s | %7.2fM   | %7.2fM   |\n",
				r.name, r.getQPS/1e6, r.setQPS/1e6)
		}
	}
}

//nolint:gocognit // benchmark code with repetitive cache setup
func measureConcurrentQPS(cacheName string, threads int, write bool) float64 {
	ctx := context.Background()
	var ops atomic.Int64
	var wg sync.WaitGroup
	done := make(chan struct{})

	switch cacheName {
	case "bdcache":
		cache, _ := bdcache.New[int, int](ctx, bdcache.WithMemorySize(perfCacheSize))
		for i := range perfCacheSize {
			_ = cache.Set(ctx, i, i, 0)
		}
		for range threads {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; ; i++ {
					select {
					case <-done:
						return
					default:
						if write {
							_ = cache.Set(ctx, i%perfCacheSize, i, 0)
						} else {
							_, _, _ = cache.Get(ctx, i%perfCacheSize)
						}
						ops.Add(1)
					}
				}
			}()
		}

	case "otter":
		cache := otter.Must(&otter.Options[int, int]{MaximumSize: perfCacheSize})
		for i := range perfCacheSize {
			cache.Set(i, i)
		}
		for range threads {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; ; i++ {
					select {
					case <-done:
						return
					default:
						if write {
							cache.Set(i%perfCacheSize, i)
						} else {
							cache.GetIfPresent(i % perfCacheSize)
						}
						ops.Add(1)
					}
				}
			}()
		}

	case "ristretto":
		cache, _ := ristretto.NewCache(&ristretto.Config{
			NumCounters: int64(perfCacheSize * 10),
			MaxCost:     int64(perfCacheSize),
			BufferItems: 64,
		})
		for i := range perfCacheSize {
			cache.Set(i, i, 1)
		}
		cache.Wait()
		for range threads {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; ; i++ {
					select {
					case <-done:
						return
					default:
						if write {
							cache.Set(i%perfCacheSize, i, 1)
						} else {
							cache.Get(i % perfCacheSize)
						}
						ops.Add(1)
					}
				}
			}()
		}

	case "lru":
		cache, _ := lru.New[int, int](perfCacheSize)
		for i := range perfCacheSize {
			cache.Add(i, i)
		}
		for range threads {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; ; i++ {
					select {
					case <-done:
						return
					default:
						if write {
							cache.Add(i%perfCacheSize, i)
						} else {
							cache.Get(i % perfCacheSize)
						}
						ops.Add(1)
					}
				}
			}()
		}

	case "tinylfu":
		cache := tinylfu.NewSync(perfCacheSize, perfCacheSize*10)
		for i := range perfCacheSize {
			cache.Set(&tinylfu.Item{Key: strconv.Itoa(i), Value: i})
		}
		for range threads {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; ; i++ {
					select {
					case <-done:
						return
					default:
						if write {
							cache.Set(&tinylfu.Item{Key: strconv.Itoa(i % perfCacheSize), Value: i})
						} else {
							cache.Get(strconv.Itoa(i % perfCacheSize))
						}
						ops.Add(1)
					}
				}
			}()
		}

	case "freecache":
		cache := freecache.NewCache(perfCacheSize * 256)
		var buf [8]byte
		for i := range perfCacheSize {
			binary.LittleEndian.PutUint64(buf[:], uint64(i))
			cache.Set([]byte(strconv.Itoa(i)), buf[:], 0)
		}
		for range threads {
			wg.Add(1)
			go func() {
				defer wg.Done()
				var buf [8]byte
				for i := 0; ; i++ {
					select {
					case <-done:
						return
					default:
						if write {
							binary.LittleEndian.PutUint64(buf[:], uint64(i))
							cache.Set([]byte(strconv.Itoa(i%perfCacheSize)), buf[:], 0)
						} else {
							cache.Get([]byte(strconv.Itoa(i % perfCacheSize)))
						}
						ops.Add(1)
					}
				}
			}()
		}
	}

	time.Sleep(concurrentDuration)
	close(done)
	wg.Wait()

	return float64(ops.Load()) / concurrentDuration.Seconds()
}
