# bdcache Benchmarks

This directory contains comparison benchmarks against popular Go cache libraries.

## Attribution

The throughput and hit ratio benchmarks are inspired by and adapted from:
- [go-cache-benchmark-plus](https://github.com/Yiling-J/go-cache-benchmark-plus) by Yiling-J (theine-go author)
- Original benchmark framework designed for comparing cache implementations

## ⚠️ Important Disclaimers

### Cherrypicked Benchmarks

These benchmarks are **intentionally cherrypicked** to demonstrate S3-FIFO's strengths:

- **Scan resistance workloads** - Where large scans of cold data shouldn't evict hot working set
- **One-hit wonder scenarios** - Where many items are accessed once and shouldn't pollute the cache
- **Memory-only Get operations** - Pure speed comparisons without I/O

**Different workloads favor different algorithms:**
- **LRU** excels with temporal locality and simple sequential access patterns
- **TinyLFU (Ristretto)** shines with frequency-based workloads and large caches
- **S3-FIFO** handles mixed workloads with both hot items and one-hit wonders

Your mileage **will** vary based on:
- Access patterns (sequential, random, zipfian, etc.)
- Working set size vs cache capacity
- Read/write ratio
- Key/value sizes
- Hardware (CPU, memory speed)

### The Real Differentiator: Persistence

**bdcache's primary advantage isn't raw speed or hit rates** - it's the automatic per-item persistence designed for unreliable cloud environments:

- **Cloud Run** - Instances shut down unpredictably after idle periods
- **Kubernetes** - Pods can be evicted, rescheduled, or killed anytime
- **Container environments** - Restarts lose all in-memory data
- **Crash recovery** - Application failures don't lose cache state

Other libraries require manual save/load of the entire cache, which:
- Doesn't work when shutdowns are unexpected
- Requires coordination and timing logic
- Risks data loss on crashes
- Adds operational complexity

## Running Benchmarks

### Parallel Throughput (go-cache-benchmark-plus style)

```bash
# Run with varying CPU counts
go test -bench=BenchmarkThroughput -benchmem -cpu=1,4,8,16

# Just Get operations
go test -bench=BenchmarkThroughputGetParallel -benchmem -cpu=16

# Just Set operations
go test -bench=BenchmarkThroughputSetParallel -benchmem -cpu=16

# Hot key contention test
go test -bench=BenchmarkThroughputGetSingle -benchmem -cpu=16
```

### Hit Ratio Tests (trace-based patterns)

```bash
# Quick comparison for tuning
go test -run=TestQuickHitRate -v

# Full trace-based hit ratio tests
go test -run=TestTraceHitRate -v

# Individual trace patterns
go test -run=TestTraceHitRateZipf -v      # Zipf distribution
go test -run=TestTraceHitRateDatabase -v  # Database/ERP pattern
go test -run=TestTraceHitRateSearch -v    # Search engine pattern
go test -run=TestTraceHitRateScan -v      # Scan resistance test
go test -run=TestTraceHitRateMixed -v     # Mixed GET/SET workload
```

### Speed Comparison

```bash
go test -bench=BenchmarkSpeed -benchmem
```

Compares raw Get operation performance across:
- bdcache (S3-FIFO)
- golang-lru (LRU)
- otter (S3-FIFO with manual persistence)
- ristretto (TinyLFU)

### Full Benchmark Suite

```bash
go test -run=TestBenchmarkSuite -v
```

Runs the complete benchmark comparison including hit rates, latency, and concurrent throughput across all thread counts (1, 4, 8, 12, 16, 24, 32).

## Benchmark Files

- `benchmark_test.go` - Speed, hit rate, and throughput benchmarks across libraries
- `throughput_test.go` - Parallel throughput benchmarks (go-cache-benchmark-plus style)
- `hitrate_trace_test.go` - Hit ratio benchmarks using synthetic trace patterns

## Interpreting Results

When evaluating caches for your use case:

1. **Profile your actual workload** - Synthetic benchmarks don't capture real-world complexity
2. **Measure what matters** - Hit rate, latency, throughput, memory usage
3. **Consider operational needs** - Persistence, observability, graceful degradation
4. **Test with your data** - Key/value sizes and access patterns vary wildly
5. **Benchmark in production-like environments** - Hardware and load matter

**Don't choose a cache based solely on these benchmarks.** Choose based on your specific requirements, with special attention to operational characteristics like persistence if you're running in unreliable cloud environments.
