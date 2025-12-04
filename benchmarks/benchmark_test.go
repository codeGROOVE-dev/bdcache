//nolint:errcheck,thelper // benchmark code - errors not critical for performance measurement
package benchmarks

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/rand/v2"
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

// =============================================================================
// Full Benchmark Suite
// =============================================================================

// TestBenchmarkSuite runs the full benchmark comparison and outputs formatted tables.
// Run with: go test -run=TestBenchmarkSuite -v
func TestBenchmarkSuite(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping benchmark suite in short mode")
	}

	fmt.Println()
	fmt.Println("bdcache benchmark bake-off")
	fmt.Println()

	printTestHeader("TestHitRate", "Hit Rate (Zipf)")
	runHitRateBenchmark()

	printTestHeader("TestDNSCache", "DNS Cache Pattern")
	runDNSCacheHitRate()

	printTestHeader("TestProxyCache", "HTTP Proxy Cache Pattern")
	runProxyCacheHitRate()

	printTestHeader("TestLatency", "Single-Threaded Latency")
	runPerformanceBenchmark()

	printTestHeader("TestMixed", "Mixed Get/Set Throughput (1 thread)")
	runConcurrentBenchmarkForThreads(1)

	printTestHeader("TestMixed4Threads", "Mixed Get/Set Throughput (4 threads)")
	runConcurrentBenchmarkForThreads(4)

	printTestHeader("TestMixed8Threads", "Mixed Get/Set Throughput (8 threads)")
	runConcurrentBenchmarkForThreads(8)

	printTestHeader("TestMixed16Threads", "Mixed Get/Set Throughput (16 threads)")
	runConcurrentBenchmarkForThreads(16)
}

func printTestHeader(testName, description string) {
	fmt.Printf(">>> %s: %s (go test -run=%s -v)\n", testName, description, testName)
}

// =============================================================================
// Exported Benchmarks (for go test -bench=.)
// =============================================================================

// Single-threaded benchmarks
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

// Parallel benchmarks - use b.RunParallel which handles goroutine management and counting.
// Run with: go test -bench=BenchmarkParallel -cpu=1,4,8,16

func BenchmarkParallelBdcacheGet(b *testing.B) {
	cache := bdcache.Memory[int, int](bdcache.WithSize(perfCacheSize))
	for i := range perfCacheSize {
		cache.Set(i, i)
	}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			cache.Get(i % perfCacheSize)
			i++
		}
	})
}

func BenchmarkParallelBdcacheSet(b *testing.B) {
	cache := bdcache.Memory[int, int](bdcache.WithSize(perfCacheSize))
	for i := range perfCacheSize {
		cache.Set(i, i)
	}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			cache.Set(i%perfCacheSize, i)
			i++
		}
	})
}

func BenchmarkParallelOtterGet(b *testing.B) {
	cache := otter.Must(&otter.Options[int, int]{MaximumSize: perfCacheSize})
	for i := range perfCacheSize {
		cache.Set(i, i)
	}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			cache.GetIfPresent(i % perfCacheSize)
			i++
		}
	})
}

func BenchmarkParallelOtterSet(b *testing.B) {
	cache := otter.Must(&otter.Options[int, int]{MaximumSize: perfCacheSize})
	for i := range perfCacheSize {
		cache.Set(i, i)
	}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			cache.Set(i%perfCacheSize, i)
			i++
		}
	})
}

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

// TestHitRateSmallCache reproduces the external go-cache-benchmark conditions exactly:
// itemSize=500000, workloads=7500000, cacheSize=0.10%, zipf's alpha=0.99, seed=19931203
// Reference: s3-fifo achieves 47.16% hit rate in go-cache-benchmark
func TestHitRateSmallCache(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	const (
		keySpace      = 500000
		workloads     = 7500000
		cacheSize     = 500 // 0.10% of keySpace
		alpha         = 0.99
		seed          = 19931203 // Same seed as go-cache-benchmark
		s3fifoHitRate = 47.16    // Reference hit rate from go-cache-benchmark
	)

	workload := generateWorkload(workloads, keySpace, alpha, seed)

	t.Logf("Hit Rate Test (Zipf alpha=%.2f, %dM ops, %dK keyspace, cache=%.2f%%, seed=%d)",
		alpha, workloads/1000000, keySpace/1000, float64(cacheSize)/float64(keySpace)*100, seed)

	// Run bdcache and verify it beats s3-fifo reference
	bdcacheRate := hitRateBdcache(workload, cacheSize)
	t.Logf("bdcache hit rate: %.2f%% (s3-fifo reference: %.2f%%)", bdcacheRate, s3fifoHitRate)

	if bdcacheRate < s3fifoHitRate {
		t.Errorf("bdcache hit rate %.2f%% is worse than s3-fifo reference %.2f%%", bdcacheRate, s3fifoHitRate)
	}

	// Also test other caches for comparison
	caches := []struct {
		name string
		fn   func([]int, int) float64
	}{
		{"otter", hitRateOtter},
		{"tinylfu", hitRateTinyLFU},
		{"lru", hitRateLRU},
	}

	for _, c := range caches {
		rate := c.fn(workload, cacheSize)
		t.Logf("%s hit rate: %.2f%%", c.name, rate)
	}
}

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

	workload := generateWorkload(hitRateWorkload, hitRateKeySpace, hitRateAlpha, 42)
	cacheSizes := []int{10000, 25000, 50000}

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

	results := make([]hitRateResult, len(caches))
	for i, c := range caches {
		rates := make([]float64, len(cacheSizes))
		for j, size := range cacheSizes {
			rates[j] = c.fn(workload, size)
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

	bdcacheIdx := -1
	for i, r := range avgs {
		if r.name == "bdcache" {
			bdcacheIdx = i
			break
		}
	}
	if bdcacheIdx < 0 {
		return
	}

	if bdcacheIdx == 0 {
		pct := (avgs[0].avg - avgs[1].avg) / avgs[1].avg * 100
		fmt.Printf("- 🔥 Hit rate: %s better than next best (%s)\n\n", formatPercent(pct), avgs[1].name)
	} else {
		pct := (avgs[0].avg - avgs[bdcacheIdx].avg) / avgs[bdcacheIdx].avg * 100
		fmt.Printf("- 💧 Hit rate: %s worse than best (%s)\n\n", formatPercent(pct), avgs[0].name)
	}
}

// =============================================================================
// Real-World Hit Rate Patterns
// =============================================================================

// runDNSCacheHitRate simulates DNS resolver cache behavior.
// DNS caches see very high locality - a small number of popular domains
// (google.com, facebook.com, etc.) dominate query volume.
// Parameters based on real DNS traffic studies:
// - ~100K unique domains in typical resolver
// - Top 1% of domains = 80% of queries (Zipf alpha ~1.2)
// - Cache sizes: 2500 (small), 5000 (medium), 20000 (large)
func runDNSCacheHitRate() {
	const (
		dnsKeySpace = 100000  // Unique domains
		dnsWorkload = 1000000 // DNS queries
		dnsAlpha    = 1.2     // High skew - popular domains dominate
	)

	fmt.Println()
	fmt.Println("### DNS Cache Pattern (alpha=1.2, 100K domains, 1M queries)")
	fmt.Println()
	fmt.Println("| Cache         | 2.5% (2.5K) | 5% (5K) | 20% (20K) |")
	fmt.Println("|---------------|-------------|---------|-----------|")

	workload := generateWorkload(dnsWorkload, dnsKeySpace, dnsAlpha, 42)
	cacheSizes := []int{2500, 5000, 20000}

	caches := []struct {
		name string
		fn   func([]int, int) float64
	}{
		{"bdcache", hitRateBdcache},
		{"otter", hitRateOtter},
		{"tinylfu", hitRateTinyLFU},
		{"lru", hitRateLRU},
	}

	results := make([]hitRateResult, len(caches))
	for i, c := range caches {
		rates := make([]float64, len(cacheSizes))
		for j, size := range cacheSizes {
			rates[j] = c.fn(workload, size)
		}
		results[i] = hitRateResult{name: c.name, rates: rates}
		fmt.Printf("| %s |    %5.2f%%    | %5.2f%% |  %5.2f%%   |\n",
			formatCacheName(c.name), rates[0], rates[1], rates[2])
	}
	fmt.Println()
	printHitRateSummary(results)
}

// runProxyCacheHitRate simulates HTTP proxy/CDN cache behavior.
// Proxy caches see more diverse traffic with temporal locality and scans.
// Key characteristics:
// - Large working set (100K+ unique URLs)
// - Moderate skew (Zipf alpha ~0.8)
// - Periodic scans (crawlers, batch jobs) that shouldn't evict hot content
func runProxyCacheHitRate() {
	const (
		proxyKeySpace = 100000  // Unique URLs
		proxyWorkload = 1000000 // HTTP requests
		proxyAlpha    = 0.8     // Moderate skew - more diverse than DNS
	)

	fmt.Println()
	fmt.Println("### HTTP Proxy Pattern (alpha=0.8, 100K URLs, 1M requests)")
	fmt.Println()
	fmt.Println("| Cache         | 5% (5K)  | 10% (10K) | 25% (25K) |")
	fmt.Println("|---------------|----------|-----------|-----------|")

	workload := generateWorkload(proxyWorkload, proxyKeySpace, proxyAlpha, 42)
	cacheSizes := []int{5000, 10000, 25000}

	caches := []struct {
		name string
		fn   func([]int, int) float64
	}{
		{"bdcache", hitRateBdcache},
		{"otter", hitRateOtter},
		{"tinylfu", hitRateTinyLFU},
		{"lru", hitRateLRU},
	}

	results := make([]hitRateResult, len(caches))
	for i, c := range caches {
		rates := make([]float64, len(cacheSizes))
		for j, size := range cacheSizes {
			rates[j] = c.fn(workload, size)
		}
		results[i] = hitRateResult{name: c.name, rates: rates}
		fmt.Printf("| %s |  %5.2f%%  |  %5.2f%%  |  %5.2f%%   |\n",
			formatCacheName(c.name), rates[0], rates[1], rates[2])
	}
	fmt.Println()
	printHitRateSummary(results)
}

func generateWorkload(n, keySpace int, theta float64, seed uint64) []int {
	rng := rand.New(rand.NewPCG(seed, seed+1))
	keys := make([]int, n)

	// Use YCSB-style Zipf distribution (matches CockroachDB/go-cache-benchmark exactly)
	// The external benchmark uses iMin=0, iMax=keySpace, so spread = iMax+1-iMin = keySpace+1
	spread := keySpace + 1

	// Precompute zeta values using spread (not keySpace)
	zeta2 := computeZeta(2, theta)
	zetaN := computeZeta(uint64(spread), theta)
	alpha := 1.0 / (1.0 - theta)
	eta := (1 - math.Pow(2.0/float64(spread), 1.0-theta)) / (1.0 - zeta2/zetaN)
	halfPowTheta := 1.0 + math.Pow(0.5, theta)

	for i := range n {
		u := rng.Float64()
		uz := u * zetaN
		var result int
		switch {
		case uz < 1.0:
			result = 0
		case uz < halfPowTheta:
			result = 1
		default:
			result = int(float64(spread) * math.Pow(eta*u-eta+1.0, alpha))
		}
		if result >= keySpace {
			result = keySpace - 1
		}
		keys[i] = result
	}
	return keys
}

// computeZeta calculates zeta(n, theta) = sum(1/i^theta) for i=1 to n
func computeZeta(n uint64, theta float64) float64 {
	sum := 0.0
	for i := uint64(1); i <= n; i++ {
		sum += 1.0 / math.Pow(float64(i), theta)
	}
	return sum
}

func hitRateBdcache(workload []int, cacheSize int) float64 {
	cache := bdcache.Memory[int, int](bdcache.WithSize(cacheSize))
	var hits int
	for _, key := range workload {
		if _, found := cache.Get(key); found {
			hits++
		} else {
			cache.Set(key, key)
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
	// Pre-compute keys to avoid strconv overhead affecting hit rate measurement
	keys := make([]string, hitRateKeySpace)
	for i := range hitRateKeySpace {
		keys[i] = strconv.Itoa(i)
	}
	var hits int
	for _, key := range workload {
		k := keys[key]
		if _, found := cache.Get(k); found {
			hits++
		} else {
			cache.Set(&tinylfu.Item{Key: k, Value: key})
		}
	}
	return float64(hits) / float64(len(workload)) * 100
}

func hitRateFreecache(workload []int, cacheSize int) float64 {
	cacheBytes := cacheSize * 24
	if cacheBytes < 512*1024 {
		cacheBytes = 512 * 1024
	}
	cache := freecache.NewCache(cacheBytes)
	// Pre-compute keys and values to avoid conversion overhead affecting hit rate measurement
	keys := make([][]byte, hitRateKeySpace)
	vals := make([][]byte, hitRateKeySpace)
	for i := range hitRateKeySpace {
		keys[i] = []byte(strconv.Itoa(i))
		vals[i] = make([]byte, 8)
		binary.LittleEndian.PutUint64(vals[i], uint64(i))
	}
	var hits int
	for _, key := range workload {
		if _, err := cache.Get(keys[key]); err == nil {
			hits++
		} else {
			cache.Set(keys[key], vals[key], 0)
		}
	}
	return float64(hits) / float64(len(workload)) * 100
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
		measurePerf("bdcache", benchBdcacheGet, benchBdcacheSet),
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

	bdcacheIdx := -1
	for i, r := range sorted {
		if r.name == "bdcache" {
			bdcacheIdx = i
			break
		}
	}
	if bdcacheIdx < 0 {
		return
	}

	if bdcacheIdx == 0 {
		pct := (extract(sorted[1]) - extract(sorted[0])) / extract(sorted[0]) * 100
		fmt.Printf("- 🔥 %s: %s better than next best (%s)\n", metric, formatPercent(pct), sorted[1].name)
	} else {
		pct := (extract(sorted[bdcacheIdx]) - extract(sorted[0])) / extract(sorted[0]) * 100
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

func benchBdcacheGet(b *testing.B) {
	cache := bdcache.Memory[int, int](bdcache.WithSize(perfCacheSize))
	for i := range perfCacheSize {
		cache.Set(i, i)
	}
	b.ResetTimer()
	for i := range b.N {
		cache.Get(i % perfCacheSize)
	}
}

func benchBdcacheSet(b *testing.B) {
	cache := bdcache.Memory[int, int](bdcache.WithSize(perfCacheSize))
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
// Concurrent Throughput Implementation
// =============================================================================

const concurrentDuration = 4 * time.Second

type concurrentResult struct {
	name string
	qps  float64 // total QPS (75% reads + 25% writes)
}

func runConcurrentBenchmarkForThreads(threads int) {
	caches := []string{"bdcache", "otter", "ristretto", "tinylfu", "freecache", "lru"}

	results := make([]concurrentResult, len(caches))
	for i, name := range caches {
		results[i] = concurrentResult{
			name: name,
			qps:  measureMixedQPS(name, threads),
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
	if threads == 1 {
		fmt.Println("### Single-Threaded Throughput (75% read / 25% write)")
	} else {
		fmt.Printf("### Concurrent Throughput (75%% read / 25%% write): %d threads\n", threads)
	}
	fmt.Println()
	fmt.Println("| Cache         | QPS        |")
	fmt.Println("|---------------|------------|")

	for _, r := range results {
		fmt.Printf("| %s | %7.2fM   |\n", formatCacheName(r.name), r.qps/1e6)
	}

	fmt.Println()
	printThroughputSummary(results)
}

func printThroughputSummary(results []concurrentResult) {
	// Results are already sorted by qps descending
	bdcacheIdx := -1
	for i, r := range results {
		if r.name == "bdcache" {
			bdcacheIdx = i
			break
		}
	}
	if bdcacheIdx < 0 {
		return
	}

	if bdcacheIdx == 0 {
		pct := (results[0].qps - results[1].qps) / results[1].qps * 100
		fmt.Printf("- 🔥 Throughput: %s faster than next best (%s)\n\n", formatPercent(pct), results[1].name)
	} else {
		pct := (results[0].qps - results[bdcacheIdx].qps) / results[bdcacheIdx].qps * 100
		fmt.Printf("- 💧 Throughput: %s slower than best (%s)\n\n", formatPercent(pct), results[0].name)
	}
}

// Batch size for counter updates - reduces atomic contention overhead.
// Also controls how often we check the stop flag (every opsBatchSize ops).
const opsBatchSize = 1000

//nolint:gocognit,maintidx // benchmark code with repetitive cache setup
func measureMixedQPS(cacheName string, threads int) float64 {
	var ops atomic.Int64
	var stop atomic.Bool
	var wg sync.WaitGroup

	switch cacheName {
	case "bdcache":
		cache := bdcache.Memory[int, int](bdcache.WithSize(perfCacheSize))
		for i := range perfCacheSize {
			cache.Set(i, i)
		}
		for range threads {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; ; {
					for range opsBatchSize {
						if i%4 == 0 { // 25% writes
							cache.Set(i%perfCacheSize, i)
						} else { // 75% reads
							cache.Get(i % perfCacheSize)
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
						if i%4 == 0 { // 25% writes
							cache.Set(i%perfCacheSize, i)
						} else { // 75% reads
							cache.GetIfPresent(i % perfCacheSize)
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
				for i := 0; ; {
					for range opsBatchSize {
						if i%4 == 0 { // 25% writes
							cache.Set(i%perfCacheSize, i, 1)
						} else { // 75% reads
							cache.Get(i % perfCacheSize)
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
						if i%4 == 0 { // 25% writes
							cache.Add(i%perfCacheSize, i)
						} else { // 75% reads
							cache.Get(i % perfCacheSize)
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
		// Pre-compute keys to avoid strconv overhead in hot path
		keys := make([]string, perfCacheSize)
		for i := range perfCacheSize {
			keys[i] = strconv.Itoa(i)
			cache.Set(&tinylfu.Item{Key: keys[i], Value: i})
		}
		for range threads {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; ; {
					for range opsBatchSize {
						if i%4 == 0 { // 25% writes
							cache.Set(&tinylfu.Item{Key: keys[i%perfCacheSize], Value: i})
						} else { // 75% reads
							cache.Get(keys[i%perfCacheSize])
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
		// Pre-compute keys and values to avoid conversion overhead in hot path
		keys := make([][]byte, perfCacheSize)
		vals := make([][]byte, perfCacheSize)
		for i := range perfCacheSize {
			keys[i] = []byte(strconv.Itoa(i))
			vals[i] = make([]byte, 8)
			binary.LittleEndian.PutUint64(vals[i], uint64(i))
			cache.Set(keys[i], vals[i], 0)
		}
		for range threads {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; ; {
					for range opsBatchSize {
						if i%4 == 0 { // 25% writes
							cache.Set(keys[i%perfCacheSize], vals[i%perfCacheSize], 0)
						} else { // 75% reads
							cache.Get(keys[i%perfCacheSize])
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

	return float64(ops.Load()) / concurrentDuration.Seconds()
}

// =============================================================================
// Zipf Throughput Implementation (realistic access patterns)
// =============================================================================

const (
	zipfWorkloadSize = 1000000 // Pre-generated workload size
	zipfAlpha        = 0.99    // Zipf skew parameter
)

// TestZipfThroughput runs throughput benchmarks with Zipf access pattern.
func TestZipfThroughput(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	printTestHeader("TestZipfThroughput", "Zipf Throughput (16 threads)")
	runZipfThroughputBenchmark(16)
}

func runZipfThroughputBenchmark(threads int) {
	// Generate Zipf workload once for all caches
	workload := generateWorkload(zipfWorkloadSize, perfCacheSize, zipfAlpha, 42)

	caches := []string{"bdcache", "otter", "ristretto", "tinylfu", "freecache", "lru"}

	results := make([]concurrentResult, len(caches))
	for i, name := range caches {
		results[i] = concurrentResult{
			name: name,
			qps:  measureZipfQPS(name, threads, workload),
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
func measureZipfQPS(cacheName string, threads int, workload []int) float64 {
	var ops atomic.Int64
	var stop atomic.Bool
	var wg sync.WaitGroup
	workloadLen := len(workload)

	switch cacheName {
	case "bdcache":
		cache := bdcache.Memory[int, int](bdcache.WithSize(perfCacheSize))
		for i := range perfCacheSize {
			cache.Set(i, i)
		}
		for range threads {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; ; {
					for range opsBatchSize {
						key := workload[i%workloadLen]
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
						key := workload[i%workloadLen]
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
				for i := 0; ; {
					for range opsBatchSize {
						key := workload[i%workloadLen]
						if i%4 == 0 { // 25% writes
							cache.Set(key, i, 1)
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
						key := workload[i%workloadLen]
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
		keys := make([]string, perfCacheSize)
		for i := range perfCacheSize {
			keys[i] = strconv.Itoa(i)
			cache.Set(&tinylfu.Item{Key: keys[i], Value: i})
		}
		for range threads {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; ; {
					for range opsBatchSize {
						key := workload[i%workloadLen]
						if i%4 == 0 { // 25% writes
							cache.Set(&tinylfu.Item{Key: keys[key], Value: i})
						} else { // 75% reads
							cache.Get(keys[key])
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
		keys := make([][]byte, perfCacheSize)
		vals := make([][]byte, perfCacheSize)
		for i := range perfCacheSize {
			keys[i] = []byte(strconv.Itoa(i))
			vals[i] = make([]byte, 8)
			binary.LittleEndian.PutUint64(vals[i], uint64(i))
			cache.Set(keys[i], vals[i], 0)
		}
		for range threads {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; ; {
					for range opsBatchSize {
						key := workload[i%workloadLen]
						if i%4 == 0 { // 25% writes
							cache.Set(keys[key], vals[key], 0)
						} else { // 75% reads
							cache.Get(keys[key])
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

	return float64(ops.Load()) / concurrentDuration.Seconds()
}

// =============================================================================
// String Key Benchmarks (tests hash function performance)
// =============================================================================

// TestStringKeyLatency runs latency benchmarks with string keys.
func TestStringKeyLatency(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	printTestHeader("TestStringKeyLatency", "String Key Latency")
	runStringKeyLatencyBenchmark()
}

// TestStringKeyThroughput runs throughput benchmarks with string keys.
func TestStringKeyThroughput(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	printTestHeader("TestStringKeyThroughput", "String Key Throughput (16 threads)")
	runStringKeyThroughputBenchmark(16)
}

func runStringKeyLatencyBenchmark() {
	// Pre-compute string keys
	keys := make([]string, perfCacheSize)
	for i := range perfCacheSize {
		keys[i] = fmt.Sprintf("key-%d-with-some-padding", i)
	}

	results := []perfResult{
		measureStringKeyPerf("bdcache", keys),
		measureStringKeyPerf("otter", keys),
		measureStringKeyPerf("tinylfu", keys),
	}

	for i := range len(results) - 1 {
		for j := i + 1; j < len(results); j++ {
			if results[j].getNs < results[i].getNs {
				results[i], results[j] = results[j], results[i]
			}
		}
	}

	fmt.Println()
	fmt.Println("### String Key Latency (sorted by Get)")
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

func measureStringKeyPerf(name string, keys []string) perfResult {
	var getResult, setResult testing.BenchmarkResult

	switch name {
	case "bdcache":
		getResult = testing.Benchmark(func(b *testing.B) {
			cache := bdcache.Memory[string, int](bdcache.WithSize(perfCacheSize))
			for i, k := range keys {
				cache.Set(k, i)
			}
			b.ResetTimer()
			for i := range b.N {
				cache.Get(keys[i%perfCacheSize])
			}
		})
		setResult = testing.Benchmark(func(b *testing.B) {
			cache := bdcache.Memory[string, int](bdcache.WithSize(perfCacheSize))
			b.ResetTimer()
			for i := range b.N {
				cache.Set(keys[i%perfCacheSize], i)
			}
		})

	case "otter":
		getResult = testing.Benchmark(func(b *testing.B) {
			cache := otter.Must(&otter.Options[string, int]{MaximumSize: perfCacheSize})
			for i, k := range keys {
				cache.Set(k, i)
			}
			b.ResetTimer()
			for i := range b.N {
				cache.GetIfPresent(keys[i%perfCacheSize])
			}
		})
		setResult = testing.Benchmark(func(b *testing.B) {
			cache := otter.Must(&otter.Options[string, int]{MaximumSize: perfCacheSize})
			b.ResetTimer()
			for i := range b.N {
				cache.Set(keys[i%perfCacheSize], i)
			}
		})

	case "tinylfu":
		getResult = testing.Benchmark(func(b *testing.B) {
			cache := tinylfu.NewSync(perfCacheSize, perfCacheSize*10)
			for i, k := range keys {
				cache.Set(&tinylfu.Item{Key: k, Value: i})
			}
			b.ResetTimer()
			for i := range b.N {
				cache.Get(keys[i%perfCacheSize])
			}
		})
		setResult = testing.Benchmark(func(b *testing.B) {
			cache := tinylfu.NewSync(perfCacheSize, perfCacheSize*10)
			b.ResetTimer()
			for i := range b.N {
				cache.Set(&tinylfu.Item{Key: keys[i%perfCacheSize], Value: i})
			}
		})
	}

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

func runStringKeyThroughputBenchmark(threads int) {
	// Pre-compute string keys
	keys := make([]string, perfCacheSize)
	for i := range perfCacheSize {
		keys[i] = fmt.Sprintf("key-%d-with-some-padding", i)
	}

	caches := []string{"bdcache", "otter", "tinylfu"}

	results := make([]concurrentResult, len(caches))
	for i, name := range caches {
		results[i] = concurrentResult{
			name: name,
			qps:  measureStringKeyQPS(name, threads, keys),
		}
	}

	for i := range len(results) - 1 {
		for j := i + 1; j < len(results); j++ {
			if results[j].qps > results[i].qps {
				results[i], results[j] = results[j], results[i]
			}
		}
	}

	fmt.Println()
	fmt.Printf("### String Key Throughput: %d threads (75%% read / 25%% write)\n", threads)
	fmt.Println()
	fmt.Println("| Cache         | QPS        |")
	fmt.Println("|---------------|------------|")

	for _, r := range results {
		fmt.Printf("| %s | %7.2fM   |\n", formatCacheName(r.name), r.qps/1e6)
	}

	fmt.Println()
	printThroughputSummary(results)
}

//nolint:gocognit // benchmark code with repetitive cache setup
func measureStringKeyQPS(cacheName string, threads int, keys []string) float64 {
	var ops atomic.Int64
	var stop atomic.Bool
	var wg sync.WaitGroup

	switch cacheName {
	case "bdcache":
		cache := bdcache.Memory[string, int](bdcache.WithSize(perfCacheSize))
		for i, k := range keys {
			cache.Set(k, i)
		}
		for range threads {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; ; {
					for range opsBatchSize {
						if i%4 == 0 { // 25% writes
							cache.Set(keys[i%perfCacheSize], i)
						} else { // 75% reads
							cache.Get(keys[i%perfCacheSize])
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
		cache := otter.Must(&otter.Options[string, int]{MaximumSize: perfCacheSize})
		for i, k := range keys {
			cache.Set(k, i)
		}
		for range threads {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; ; {
					for range opsBatchSize {
						if i%4 == 0 { // 25% writes
							cache.Set(keys[i%perfCacheSize], i)
						} else { // 75% reads
							cache.GetIfPresent(keys[i%perfCacheSize])
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
		for i, k := range keys {
			cache.Set(&tinylfu.Item{Key: k, Value: i})
		}
		for range threads {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; ; {
					for range opsBatchSize {
						if i%4 == 0 { // 25% writes
							cache.Set(&tinylfu.Item{Key: keys[i%perfCacheSize], Value: i})
						} else { // 75% reads
							cache.Get(keys[i%perfCacheSize])
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

	return float64(ops.Load()) / concurrentDuration.Seconds()
}

// =============================================================================
// External Benchmark Compatibility Test
// =============================================================================

// TestExternalBenchmark replicates the exact conditions from go-cache-benchmark:
// - itemSize=100000, workloads=1500000, cacheSize=10%, zipf alpha=0.99
// - Uses string keys (not int), concurrency=1
// - Get-miss-then-Set pattern (not pure Get or Set)
func TestExternalBenchmark(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	printTestHeader("TestExternalBenchmark", "External Benchmark Comparison")
	runExternalBenchmark(1)
	runExternalBenchmark(4)
}

func runExternalBenchmark(concurrency int) {
	const (
		itemSize  = 100000
		workloads = 1500000
		alpha     = 0.99
	)
	cacheSize := int(float64(itemSize) * 0.10) // 10% = 10000

	fmt.Println()
	fmt.Printf("### External Benchmark Conditions (concurrency=%d)\n", concurrency)
	fmt.Printf("itemSize=%d, workloads=%d, cacheSize=%d (10%%), alpha=%.2f\n\n", itemSize, workloads, cacheSize, alpha)

	// Generate workload keys as strings (matching external benchmark)
	workload := generateWorkload(workloads, itemSize, alpha, 19931203)
	stringKeys := make([]string, len(workload))
	for i, k := range workload {
		stringKeys[i] = strconv.Itoa(k)
	}

	type result struct {
		name    string
		qps     float64
		hitRate float64
		hits    int64
		misses  int64
	}

	results := []result{}

	// bdcache
	{
		cache := bdcache.Memory[string, string](bdcache.WithSize(cacheSize))
		var hits, misses int64
		start := time.Now()
		runExtBenchConcurrent(concurrency, stringKeys, func(key string) bool {
			_, ok := cache.Get(key)
			return ok
		}, func(key string) {
			cache.Set(key, key)
		}, &hits, &misses)
		elapsed := time.Since(start)
		cache.Close()
		qps := float64(workloads) / elapsed.Seconds()
		hitRate := float64(hits) / float64(hits+misses) * 100
		results = append(results, result{"bdcache", qps, hitRate, hits, misses})
	}

	// tinylfu
	{
		cache := tinylfu.New(cacheSize, 10*cacheSize)
		var hits, misses int64
		start := time.Now()
		runExtBenchConcurrent(concurrency, stringKeys, func(key string) bool {
			_, ok := cache.Get(key)
			return ok
		}, func(key string) {
			cache.Set(&tinylfu.Item{Key: key, Value: key})
		}, &hits, &misses)
		elapsed := time.Since(start)
		qps := float64(workloads) / elapsed.Seconds()
		hitRate := float64(hits) / float64(hits+misses) * 100
		results = append(results, result{"tinylfu", qps, hitRate, hits, misses})
	}

	// lru
	{
		cache, _ := lru.New[string, string](cacheSize)
		var hits, misses int64
		start := time.Now()
		runExtBenchConcurrent(concurrency, stringKeys, func(key string) bool {
			_, ok := cache.Get(key)
			return ok
		}, func(key string) {
			cache.Add(key, key)
		}, &hits, &misses)
		elapsed := time.Since(start)
		qps := float64(workloads) / elapsed.Seconds()
		hitRate := float64(hits) / float64(hits+misses) * 100
		results = append(results, result{"lru", qps, hitRate, hits, misses})
	}

	// otter
	{
		cache := otter.Must(&otter.Options[string, string]{MaximumSize: cacheSize})
		var hits, misses int64
		start := time.Now()
		runExtBenchConcurrent(concurrency, stringKeys, func(key string) bool {
			_, found := cache.GetIfPresent(key)
			return found
		}, func(key string) {
			cache.Set(key, key)
		}, &hits, &misses)
		elapsed := time.Since(start)
		qps := float64(workloads) / elapsed.Seconds()
		hitRate := float64(hits) / float64(hits+misses) * 100
		results = append(results, result{"otter", qps, hitRate, hits, misses})
	}

	// Sort by hit rate
	for i := range len(results) - 1 {
		for j := i + 1; j < len(results); j++ {
			if results[j].hitRate > results[i].hitRate {
				results[i], results[j] = results[j], results[i]
			}
		}
	}

	fmt.Println("| Cache         | HitRate |    QPS     |  Hits   | Misses  |")
	fmt.Println("|---------------|---------|------------|---------|---------|")
	for _, r := range results {
		fmt.Printf("| %s | %5.2f%% | %10.0f | %7d | %7d |\n",
			formatCacheName(r.name), r.hitRate, r.qps, r.hits, r.misses)
	}
	fmt.Println()
}

func runExtBenchConcurrent(concurrency int, keys []string, get func(string) bool, set func(string), hits, misses *int64) {
	if concurrency == 1 {
		// Single-threaded - no need for atomics or goroutines
		for _, key := range keys {
			if get(key) {
				*hits++
			} else {
				*misses++
				set(key)
			}
		}
		return
	}

	// Multi-threaded
	each := len(keys) / concurrency
	var wg sync.WaitGroup
	hitsArr := make([]int64, concurrency)
	missesArr := make([]int64, concurrency)

	for i := range concurrency {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			start := idx * each
			end := start + each
			if idx == concurrency-1 {
				end = len(keys)
			}
			for j := start; j < end; j++ {
				if get(keys[j]) {
					hitsArr[idx]++
				} else {
					missesArr[idx]++
					set(keys[j])
				}
			}
		}(i)
	}
	wg.Wait()

	for i := range concurrency {
		*hits += hitsArr[i]
		*misses += missesArr[i]
	}
}
