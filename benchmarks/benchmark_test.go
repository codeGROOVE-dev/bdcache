//nolint:errcheck,thelper // benchmark code - errors not critical for performance measurement
package benchmarks

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codeGROOVE-dev/sfcache"
	"github.com/codeGROOVE-dev/sfcache/benchmarks/pkg/workload"
	"github.com/coocood/freecache"
	"github.com/dgraph-io/ristretto"
	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/maypok86/otter/v2"
	"github.com/vmihailenco/go-tinylfu"
)

// =============================================================================
// Full Benchmark Suite
// =============================================================================

func TestMemoryOverhead(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping memory benchmark in short mode")
	}
	runMemoryBenchmark(t)
}

// TestBenchmarkSuite runs the 5 key benchmarks for tracking sfcache performance.
// Run with: go test -run=TestBenchmarkSuite -v
func TestBenchmarkSuite(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping benchmark suite in short mode")
	}

	fmt.Println()
	fmt.Println("sfcache benchmark bake-off")
	fmt.Println()

	// 1. Single-threaded latency
	printTestHeader("TestLatency", "Single-Threaded Latency")
	runPerformanceBenchmark()

	// 2. Single-threaded throughput (Zipf)
	printTestHeader("TestZipfThroughput1", "Zipf Throughput (1 thread)")
	runZipfThroughputBenchmark(1)

	// 3. Multi-threaded throughput (Zipf)
	printTestHeader("TestZipfThroughput16", "Zipf Throughput (16 threads)")
	runZipfThroughputBenchmark(16)

	// 4. Real-world hit rate from Meta KVCache production trace
	printTestHeader("TestMetaTrace", "Meta Trace Hit Rate (10M ops)")
	runMetaTraceHitRate()

	// 5. Synthetic hit rate with Zipf distribution
	printTestHeader("TestHitRate", "Zipf Hit Rate")
	runHitRateBenchmark()

	// 6. Memory Overhead
	printTestHeader("TestMemoryOverhead", "Memory Usage (10k items, 1KB values)")
	runMemoryBenchmark(t)
}

func printTestHeader(testName, description string) {
	fmt.Printf(">>> %s: %s (go test -run=%s -v)\n", testName, description, testName)
}

// =============================================================================
// Exported Benchmarks (for go test -bench=.)
// =============================================================================

// Single-threaded benchmarks
func BenchmarkSFCacheGet(b *testing.B)   { benchSFCacheGet(b) }
func BenchmarkSFCacheSet(b *testing.B)   { benchSFCacheSet(b) }
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
// Formatting Helpers
// =============================================================================

func formatPercent(pct float64) string {
	absPct := pct
	if absPct < 0 {
		absPct = -absPct
	}
	if absPct < 0.1 {
		return fmt.Sprintf("%.3f%%", pct)
	}
	if absPct < 1 {
		return fmt.Sprintf("%.2f%%", pct)
	}
	if absPct < 10 {
		return fmt.Sprintf("%.1f%%", pct)
	}
	return fmt.Sprintf("%.0f%%", pct)
}

func formatCacheName(name string) string {
	return fmt.Sprintf("%-13s", name)
}

// =============================================================================
// Hit Rate Implementation
// =============================================================================

const (
	hitRateKeySpace = 1000000
	hitRateWorkload = 1000000
	hitRateAlpha    = 0.99
)

type hitRateResult struct {
	name  string
	rates []float64
}

func runHitRateBenchmark() {
	fmt.Println()
	fmt.Println("### Hit Rate (Zipf alpha=0.99, 1M ops, 1M keyspace)")
	fmt.Println()
	fmt.Println("| Cache         | Size=1% | Size=2.5% | Size=5% |")
	fmt.Println("|---------------|---------|-----------|---------|")

	keys := workload.GenerateZipfInt(hitRateWorkload, hitRateKeySpace, hitRateAlpha, 42)
	cacheSizes := []int{10000, 25000, 50000}

	caches := []struct {
		name string
		fn   func([]int, int) float64
	}{
		{"sfcache", hitRateSFCache},
		{"otter", hitRateOtter},
		{"ristretto", hitRateRistretto},
		{"tinylfu", hitRateTinyLFU},
		{"freecache", hitRateFreecache},
		{"lru", hitRateLRU},
	}

	results := make([]hitRateResult, len(caches))
	for i, c := range caches {
		rates := make([]float64, len(cacheSizes))
		for j, size := range cacheSizes {
			rates[j] = c.fn(keys, size)
		}
		results[i] = hitRateResult{name: c.name, rates: rates}

		fmt.Printf("| %s |  %5.2f%% |    %5.2f%% |  %5.2f%% |\n",
			formatCacheName(c.name), rates[0], rates[1], rates[2])
	}

	fmt.Println()
	printHitRateSummary(results)
}

func printHitRateSummary(results []hitRateResult) {
	type avgResult struct {
		name string
		avg  float64
	}
	avgs := make([]avgResult, len(results))
	for i, r := range results {
		sum := 0.0
		for _, rate := range r.rates {
			sum += rate
		}
		avgs[i] = avgResult{name: r.name, avg: sum / float64(len(r.rates))}
	}

	for i := range len(avgs) - 1 {
		for j := i + 1; j < len(avgs); j++ {
			if avgs[j].avg > avgs[i].avg {
				avgs[i], avgs[j] = avgs[j], avgs[i]
			}
		}
	}

	sfcacheIdx := -1
	for i, r := range avgs {
		if r.name == "sfcache" {
			sfcacheIdx = i
			break
		}
	}
	if sfcacheIdx < 0 {
		return
	}

	if sfcacheIdx == 0 {
		pct := (avgs[0].avg - avgs[1].avg) / avgs[1].avg * 100
		fmt.Printf("- 🔥 Hit rate: %s better than next best (%s)\n\n", formatPercent(pct), avgs[1].name)
	} else {
		pct := (avgs[0].avg - avgs[sfcacheIdx].avg) / avgs[sfcacheIdx].avg * 100
		fmt.Printf("- 💧 Hit rate: %s worse than best (%s)\n\n", formatPercent(pct), avgs[0].name)
	}
}

func hitRateSFCache(keys []int, cacheSize int) float64 {
	cache := sfcache.New[int, int](sfcache.Size(cacheSize))
	var hits int
	for _, key := range keys {
		if _, found := cache.Get(key); found {
			hits++
		} else {
			cache.Set(key, key)
		}
	}
	return float64(hits) / float64(len(keys)) * 100
}

func hitRateOtter(keys []int, cacheSize int) float64 {
	cache := otter.Must(&otter.Options[int, int]{MaximumSize: cacheSize})
	var hits int
	for _, key := range keys {
		if _, found := cache.GetIfPresent(key); found {
			hits++
		} else {
			cache.Set(key, key)
		}
	}
	return float64(hits) / float64(len(keys)) * 100
}

func hitRateRistretto(keys []int, cacheSize int) float64 {
	cache, _ := ristretto.NewCache(&ristretto.Config{
		NumCounters: int64(cacheSize * 10),
		MaxCost:     int64(cacheSize),
		BufferItems: 64,
	})
	defer cache.Close()
	var hits int
	for _, key := range keys {
		if _, found := cache.Get(key); found {
			hits++
		} else {
			cache.Set(key, key, 1)
			cache.Wait()
		}
	}
	return float64(hits) / float64(len(keys)) * 100
}

func hitRateLRU(keys []int, cacheSize int) float64 {
	cache, _ := lru.New[int, int](cacheSize)
	var hits int
	for _, key := range keys {
		if _, found := cache.Get(key); found {
			hits++
		} else {
			cache.Add(key, key)
		}
	}
	return float64(hits) / float64(len(keys)) * 100
}

func hitRateTinyLFU(keys []int, cacheSize int) float64 {
	cache := tinylfu.New(cacheSize, cacheSize*10)
	// Pre-compute keys to avoid strconv overhead affecting hit rate measurement
	precomputedKeys := make([]string, hitRateKeySpace)
	for i := range hitRateKeySpace {
		precomputedKeys[i] = strconv.Itoa(i)
	}
	var hits int
	for _, key := range keys {
		k := precomputedKeys[key]
		if _, found := cache.Get(k); found {
			hits++
		} else {
			cache.Set(&tinylfu.Item{Key: k, Value: key})
		}
	}
	return float64(hits) / float64(len(keys)) * 100
}

func hitRateFreecache(keys []int, cacheSize int) float64 {
	cacheBytes := cacheSize * 24
	if cacheBytes < 512*1024 {
		cacheBytes = 512 * 1024
	}
	cache := freecache.NewCache(cacheBytes)
	// Pre-compute keys and values to avoid conversion overhead affecting hit rate measurement
	precomputedKeys := make([][]byte, hitRateKeySpace)
	vals := make([][]byte, hitRateKeySpace)
	for i := range hitRateKeySpace {
		precomputedKeys[i] = []byte(strconv.Itoa(i))
		vals[i] = make([]byte, 8)
		binary.LittleEndian.PutUint64(vals[i], uint64(i))
	}
	var hits int
	for _, key := range keys {
		if _, err := cache.Get(precomputedKeys[key]); err == nil {
			hits++
		} else {
			cache.Set(precomputedKeys[key], vals[key], 0)
		}
	}
	return float64(hits) / float64(len(keys)) * 100
}

// =============================================================================
// Latency Implementation
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
		measurePerf("sfcache", benchSFCacheGet, benchSFCacheSet),
		measurePerf("otter", benchOtterGet, benchOtterSet),
		measurePerf("ristretto", benchRistrettoGet, benchRistrettoSet),
		measurePerf("tinylfu", benchTinyLFUGet, benchTinyLFUSet),
		measurePerf("freecache", benchFreecacheGet, benchFreecacheSet),
		measurePerf("lru", benchLRUGet, benchLRUSet),
	}

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
	fmt.Println("| Cache         | Get ns/op | Get B/op | Get allocs | Set ns/op | Set B/op | Set allocs |")
	fmt.Println("|---------------|-----------|----------|------------|-----------|----------|------------|")

	for _, r := range results {
		fmt.Printf("| %s | %9.1f | %8d | %10d | %9.1f | %8d | %10d |\n",
			formatCacheName(r.name),
			r.getNs, r.getB, r.getAlloc,
			r.setNs, r.setB, r.setAlloc)
	}

	fmt.Println()
	printLatencySummary(results, "Get", func(r perfResult) float64 { return r.getNs })
	printLatencySummary(results, "Set", func(r perfResult) float64 { return r.setNs })
	fmt.Println()
}

func printLatencySummary(results []perfResult, metric string, extract func(perfResult) float64) {
	sorted := make([]perfResult, len(results))
	copy(sorted, results)
	for i := range len(sorted) - 1 {
		for j := i + 1; j < len(sorted); j++ {
			if extract(sorted[j]) < extract(sorted[i]) {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}

	sfcacheIdx := -1
	for i, r := range sorted {
		if r.name == "sfcache" {
			sfcacheIdx = i
			break
		}
	}
	if sfcacheIdx < 0 {
		return
	}

	if sfcacheIdx == 0 {
		pct := (extract(sorted[1]) - extract(sorted[0])) / extract(sorted[0]) * 100
		fmt.Printf("- 🔥 %s: %s better than next best (%s)\n", metric, formatPercent(pct), sorted[1].name)
	} else {
		pct := (extract(sorted[sfcacheIdx]) - extract(sorted[0])) / extract(sorted[0]) * 100
		fmt.Printf("- 💧 %s: %s worse than best (%s)\n", metric, formatPercent(pct), sorted[0].name)
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

func benchSFCacheGet(b *testing.B) {
	cache := sfcache.New[int, int](sfcache.Size(perfCacheSize))
	for i := range perfCacheSize {
		cache.Set(i, i)
	}
	b.ResetTimer()
	for i := range b.N {
		cache.Get(i % perfCacheSize)
	}
}

func benchSFCacheSet(b *testing.B) {
	cache := sfcache.New[int, int](sfcache.Size(perfCacheSize))
	b.ResetTimer()
	for i := range b.N {
		cache.Set(i%perfCacheSize, i)
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
	defer cache.Close()
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
	defer cache.Close()
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
	// Pre-compute keys to avoid strconv overhead in hot path
	keys := make([]string, perfCacheSize)
	for i := range perfCacheSize {
		keys[i] = strconv.Itoa(i)
		cache.Set(&tinylfu.Item{Key: keys[i], Value: i})
	}
	b.ResetTimer()
	for i := range b.N {
		cache.Get(keys[i%perfCacheSize])
	}
}

func benchTinyLFUSet(b *testing.B) {
	cache := tinylfu.NewSync(perfCacheSize, perfCacheSize*10)
	// Pre-compute keys to avoid strconv overhead in hot path
	keys := make([]string, perfCacheSize)
	for i := range perfCacheSize {
		keys[i] = strconv.Itoa(i)
	}
	b.ResetTimer()
	for i := range b.N {
		cache.Set(&tinylfu.Item{Key: keys[i%perfCacheSize], Value: i})
	}
}

func benchFreecacheGet(b *testing.B) {
	cache := freecache.NewCache(perfCacheSize * 256)
	// Pre-compute keys to avoid strconv/[]byte overhead in hot path
	keys := make([][]byte, perfCacheSize)
	var buf [8]byte
	for i := range perfCacheSize {
		keys[i] = []byte(strconv.Itoa(i))
		binary.LittleEndian.PutUint64(buf[:], uint64(i))
		cache.Set(keys[i], buf[:], 0)
	}
	b.ResetTimer()
	for i := range b.N {
		cache.Get(keys[i%perfCacheSize])
	}
}

func benchFreecacheSet(b *testing.B) {
	cache := freecache.NewCache(perfCacheSize * 256)
	// Pre-compute keys and values to avoid conversion overhead in hot path
	keys := make([][]byte, perfCacheSize)
	vals := make([][]byte, perfCacheSize)
	for i := range perfCacheSize {
		keys[i] = []byte(strconv.Itoa(i))
		vals[i] = make([]byte, 8)
		binary.LittleEndian.PutUint64(vals[i], uint64(i))
	}
	b.ResetTimer()
	for i := range b.N {
		cache.Set(keys[i%perfCacheSize], vals[i%perfCacheSize], 0)
	}
}

// =============================================================================
// Throughput Implementation
// =============================================================================

const concurrentDuration = 4 * time.Second

type concurrentResult struct {
	name string
	qps  float64 // total QPS (75% reads + 25% writes)
}

func printThroughputSummary(results []concurrentResult) {
	// Results are already sorted by qps descending
	sfcacheIdx := -1
	for i, r := range results {
		if r.name == "sfcache" {
			sfcacheIdx = i
			break
		}
	}
	if sfcacheIdx < 0 {
		return
	}

	if sfcacheIdx == 0 {
		pct := (results[0].qps - results[1].qps) / results[1].qps * 100
		fmt.Printf("- 🔥 Throughput: %s faster than next best (%s)\n\n", formatPercent(pct), results[1].name)
	} else {
		pct := (results[0].qps - results[sfcacheIdx].qps) / results[sfcacheIdx].qps * 100
		fmt.Printf("- 💧 Throughput: %s slower than best (%s)\n\n", formatPercent(pct), results[0].name)
	}
}

// Batch size for counter updates - reduces atomic contention overhead.
// Also controls how often we check the stop flag (every opsBatchSize ops).
const opsBatchSize = 1000

// =============================================================================
// Zipf Throughput Implementation (realistic access patterns)
// =============================================================================

const (
	zipfWorkloadSize = 1000000 // Pre-generated workload size
	zipfAlpha        = 0.99    // Zipf skew parameter
)

func runZipfThroughputBenchmark(threads int) {
	// Generate Zipf workload once for all caches
	keys := workload.GenerateZipfInt(zipfWorkloadSize, perfCacheSize, zipfAlpha, 42)

	caches := []string{"sfcache", "otter", "ristretto", "tinylfu", "freecache", "lru"}

	results := make([]concurrentResult, len(caches))
	for i, name := range caches {
		results[i] = concurrentResult{
			name: name,
			qps:  measureZipfQPS(name, threads, keys),
		}
	}

	// Sort by QPS descending
	for i := range len(results) - 1 {
		for j := i + 1; j < len(results); j++ {
			if results[j].qps > results[i].qps {
				results[i], results[j] = results[j], results[i]
			}
		}
	}

	fmt.Println()
	fmt.Printf("### Zipf Throughput (alpha=%.2f, 75%% read / 25%% write): %d threads\n", zipfAlpha, threads)
	fmt.Println()
	fmt.Println("| Cache         | QPS        |")
	fmt.Println("|---------------|------------|")

	for _, r := range results {
		fmt.Printf("| %s | %7.2fM   |\n", formatCacheName(r.name), r.qps/1e6)
	}

	fmt.Println()
	printThroughputSummary(results)
}

//nolint:gocognit,maintidx // benchmark code with repetitive cache setup
func measureZipfQPS(cacheName string, threads int, keys []int) float64 {
	var ops atomic.Int64
	var stop atomic.Bool
	var wg sync.WaitGroup
	workloadLen := len(keys)
	var ristrettoCache *ristretto.Cache // Track for cleanup

	switch cacheName {
	case "sfcache":
		cache := sfcache.New[int, int](sfcache.Size(perfCacheSize))
		for i := range perfCacheSize {
			cache.Set(i, i)
		}
		for range threads {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; ; {
					for range opsBatchSize {
						key := keys[i%workloadLen]
						if i%4 == 0 { // 25% writes
							cache.Set(key, i)
						} else { // 75% reads
							cache.Get(key)
						}
						i++
					}
					ops.Add(opsBatchSize)
					if stop.Load() {
						return
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
				for i := 0; ; {
					for range opsBatchSize {
						key := keys[i%workloadLen]
						if i%4 == 0 { // 25% writes
							cache.Set(key, i)
						} else { // 75% reads
							cache.GetIfPresent(key)
						}
						i++
					}
					ops.Add(opsBatchSize)
					if stop.Load() {
						return
					}
				}
			}()
		}

	case "ristretto":
		ristrettoCache, _ = ristretto.NewCache(&ristretto.Config{
			NumCounters: int64(perfCacheSize * 10),
			MaxCost:     int64(perfCacheSize),
			BufferItems: 64,
		})
		for i := range perfCacheSize {
			ristrettoCache.Set(i, i, 1)
		}
		ristrettoCache.Wait()
		for range threads {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; ; {
					for range opsBatchSize {
						key := keys[i%workloadLen]
						if i%4 == 0 { // 25% writes
							ristrettoCache.Set(key, i, 1)
						} else { // 75% reads
							ristrettoCache.Get(key)
						}
						i++
					}
					ops.Add(opsBatchSize)
					if stop.Load() {
						return
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
				for i := 0; ; {
					for range opsBatchSize {
						key := keys[i%workloadLen]
						if i%4 == 0 { // 25% writes
							cache.Add(key, i)
						} else { // 75% reads
							cache.Get(key)
						}
						i++
					}
					ops.Add(opsBatchSize)
					if stop.Load() {
						return
					}
				}
			}()
		}

	case "tinylfu":
		cache := tinylfu.NewSync(perfCacheSize, perfCacheSize*10)
		keysAsStrings := make([]string, perfCacheSize)
		for i := range perfCacheSize {
			keysAsStrings[i] = strconv.Itoa(i)
			cache.Set(&tinylfu.Item{Key: keysAsStrings[i], Value: i})
		}
		for range threads {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; ; {
					for range opsBatchSize {
						key := keys[i%workloadLen]
						if i%4 == 0 { // 25% writes
							cache.Set(&tinylfu.Item{Key: keysAsStrings[key], Value: i})
						} else { // 75% reads
							cache.Get(keysAsStrings[key])
						}
						i++
					}
					ops.Add(opsBatchSize)
					if stop.Load() {
						return
					}
				}
			}()
		}

	case "freecache":
		cache := freecache.NewCache(perfCacheSize * 256)
		keysAsBytes := make([][]byte, perfCacheSize)
		vals := make([][]byte, perfCacheSize)
		for i := range perfCacheSize {
			keysAsBytes[i] = []byte(strconv.Itoa(i))
			vals[i] = make([]byte, 8)
			binary.LittleEndian.PutUint64(vals[i], uint64(i))
			cache.Set(keysAsBytes[i], vals[i], 0)
		}
		for range threads {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; ; {
					for range opsBatchSize {
						key := keys[i%workloadLen]
						if i%4 == 0 { // 25% writes
							cache.Set(keysAsBytes[key], vals[key], 0)
						} else { // 75% reads
							cache.Get(keysAsBytes[key])
						}
						i++
					}
					ops.Add(opsBatchSize)
					if stop.Load() {
						return
					}
				}
			}()
		}
	}

	time.Sleep(concurrentDuration)
	stop.Store(true)
	wg.Wait()

	// Clean up ristretto to prevent goroutine leaks
	if ristrettoCache != nil {
		ristrettoCache.Close()
	}

	return float64(ops.Load()) / concurrentDuration.Seconds()
}

// =============================================================================

// Memory Overhead Implementation

// =============================================================================

// =============================================================================

// Memory Overhead Implementation (External Process)

// =============================================================================

type runnerOutput struct {
	Name string `json:"name"`

	Items int `json:"items"`

	Bytes uint64 `json:"bytes"`
}

func runMemoryBenchmark(t *testing.T) {
	fmt.Println()

	fmt.Println("### Memory Usage (32k cap, 32k unique items, 1KB values) - Isolated Processes")

	fmt.Println("    Workload: Repeated Access to force admission and fill capacity")

	fmt.Println()

	fmt.Println("| Cache         | Items Stored | Memory (MB) | Overhead vs Map (bytes/item) |")

	fmt.Println("|---------------|--------------|-------------|------------------------------|")

	caches := []string{"mem_sfcache", "mem_otter", "mem_ristretto", "mem_tinylfu", "mem_freecache", "mem_lru"}

	results := make([]runnerOutput, len(caches))

	for i, name := range caches {
		results[i] = buildAndRun(t, name)
	}

	// Sort by memory usage ascending

	for i := range len(results) - 1 {
		for j := i + 1; j < len(results); j++ {
			if results[j].Bytes < results[i].Bytes {
				results[i], results[j] = results[j], results[i]
			}
		}
	}

	for _, r := range results {
		// Run baseline with exact same number of items for fair comparison

		baseline := buildAndRun(t, "mem_baseline", "-target", strconv.Itoa(r.Items))

		// Calculate overhead relative to baseline

		diff := int64(r.Bytes) - int64(baseline.Bytes)

		var overheadPerItem int64

		if r.Items > 0 {
			overheadPerItem = diff / int64(r.Items)
		}

		mb := float64(r.Bytes) / 1024 / 1024

		fmt.Printf("| %s | %12d | %8.2f MB | %28d |\n",
			formatCacheName(r.Name), r.Items, mb, overheadPerItem)
	}
	fmt.Println()
}

func buildAndRun(t *testing.T, cmdDir string, args ...string) runnerOutput {
	// Binary name = cmdDir (e.g., mem_sfcache)

	binName := "./" + cmdDir + ".bin"

	srcDir := "./cmd/" + cmdDir

	// Build

	buildCmd := exec.Command("go", "build", "-o", binName, srcDir)

	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to build %s: %v\n%s", srcDir, err, out)
	}

	defer os.Remove(binName)

	// Run

	runArgs := append([]string{"-iter", "250000", "-cap", "32768"}, args...)

	runCmd := exec.Command(binName, runArgs...)

	out, err := runCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("failed to run %s: %v\nOutput: %s", binName, err, out)
	}

	var res runnerOutput

	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("failed to parse output for %s: %v\nOutput: %s", binName, err, out)
	}

	return res
}
