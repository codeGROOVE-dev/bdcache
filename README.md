# sfcache - Stupid Fast Cache

<img src="media/logo-small.png" alt="sfcache logo" width="256">

[![Go Reference](https://pkg.go.dev/badge/github.com/codeGROOVE-dev/sfcache.svg)](https://pkg.go.dev/github.com/codeGROOVE-dev/sfcache)
[![Go Report Card](https://goreportcard.com/badge/github.com/codeGROOVE-dev/sfcache)](https://goreportcard.com/report/github.com/codeGROOVE-dev/sfcache)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](https://opensource.org/licenses/Apache-2.0)

<br clear="right">

sfcache is the fastest in-memory cache for Go. Need multi-tier persistence? We have it. Need thundering herd protection? We've got that too.

Designed for persistently caching API requests in an unreliable environment, this cache has an abundance of production-ready features:

## Features

- **Faster than a bat out of hell** - Best-in-class latency and throughput
- **S3-FIFO eviction** - Better hit-rates than LRU ([learn more](https://s3fifo.com/))
- **Multi-tier persistent cache (optional)** - Bring your own database or use built-in backends:
  - [`pkg/store/cloudrun`](pkg/store/cloudrun) - Automatically select Google Cloud Datastore in Cloud Run, localfs elsewhere
  - [`pkg/store/datastore`](pkg/store/datastore) - Google Cloud Datastore
  - [`pkg/store/localfs`](pkg/store/localfs) - Local files (gob encoding, zero dependencies)
  - [`pkg/store/null`](pkg/store/null) - No-op (for testing or TieredCache API compatibility)
  - [`pkg/store/valkey`](pkg/store/valkey) - Valkey/Redis
- **Per-item TTL** - Optional expiration
- **Thundering herd prevention** - `GetSet` deduplicates concurrent loads for the same key
- **Graceful degradation** - Cache works even if persistence fails
- **Zero allocation updates** - minimal GC thrashing

## Usage

As a stupid-fast in-memory cache:

```go
import "github.com/codeGROOVE-dev/sfcache"

// strings as keys, ints as values
cache := sfcache.New[string, int]()
cache.Set("answer", 42)
val, found := cache.Get("answer")
```

Or as a multi-tier cache with local persistence to survive restarts:

```go
import (
  "github.com/codeGROOVE-dev/sfcache"
  "github.com/codeGROOVE-dev/sfcache/pkg/store/localfs"
)

p, _ := localfs.New[string, User]("myapp", "")
cache, _ := sfcache.NewTiered(p)

cache.SetAsync(ctx, "user:123", user) // Don't wait for the key to persist
cache.Store.Len(ctx)                  // Access persistence layer directly
```

How about a persistent cache suitable for Cloud Run or local development? This uses Cloud DataStore if available, local files if not:

```go
import "github.com/codeGROOVE-dev/sfcache/pkg/store/cloudrun"

p, _ := cloudrun.New[string, User](ctx, "myapp")
cache, _ := sfcache.NewTiered(p)
```

## Performance against the Competition

sfcache prioritizes high hit-rates and low read latency. We have our own built in `make bench` that asserts cache dominance:

```
>>> TestLatencyNoEviction: Latency - No Evictions (Set cycles within cache size) (go test -run=TestLatencyNoEviction -v)
| Cache         | Get ns/op | Get B/op | Get allocs | Set ns/op | Set B/op | Set allocs |
|---------------|-----------|----------|------------|-----------|----------|------------|
| sfcache       |       7.0 |        0 |          0 |      23.0 |        0 |          0 |
| lru           |      23.0 |        0 |          0 |      23.0 |        0 |          0 |
| ristretto     |      28.0 |       13 |          0 |      77.0 |      118 |          3 |
| otter         |      34.0 |        0 |          0 |     160.0 |       51 |          1 |
| freecache     |      74.0 |        8 |          1 |      53.0 |        0 |          0 |
| tinylfu       |      80.0 |        0 |          0 |     110.0 |      168 |          3 |

- 🔥 Get: 229% better than next best (lru)
- 🔥 Set: 0.000% better than next best (lru)

>>> TestLatencyWithEviction: Latency - With Evictions (Set uses 20x unique keys) (go test -run=TestLatencyWithEviction -v)
| Cache         | Get ns/op | Get B/op | Get allocs | Set ns/op | Set B/op | Set allocs |
|---------------|-----------|----------|------------|-----------|----------|------------|
| sfcache       |       7.0 |        0 |          0 |      94.0 |        0 |          0 |
| lru           |      24.0 |        0 |          0 |      83.0 |       80 |          1 |
| ristretto     |      31.0 |       14 |          0 |      73.0 |      119 |          3 |
| otter         |      34.0 |        0 |          0 |     176.0 |       61 |          1 |
| freecache     |      69.0 |        8 |          1 |     102.0 |        1 |          0 |
| tinylfu       |      79.0 |        0 |          0 |     115.0 |      168 |          3 |

- 🔥 Get: 243% better than next best (lru)
- 💧 Set: 29% worse than best (ristretto)

>>> TestZipfThroughput1: Zipf Throughput (1 thread) (go test -run=TestZipfThroughput1 -v)

### Zipf Throughput (alpha=0.99, 75% read / 25% write): 1 threads

| Cache         | QPS        |
|---------------|------------|
| sfcache       |  100.26M   |
| lru           |   44.58M   |
| tinylfu       |   18.42M   |
| freecache     |   14.07M   |
| otter         |   13.52M   |
| ristretto     |   11.32M   |

- 🔥 Throughput: 125% faster than next best (lru)

>>> TestZipfThroughput16: Zipf Throughput (16 threads) (go test -run=TestZipfThroughput16 -v)

### Zipf Throughput (alpha=0.99, 75% read / 25% write): 16 threads

| Cache         | QPS        |
|---------------|------------|
| sfcache       |   36.46M   |
| freecache     |   15.00M   |
| ristretto     |   13.47M   |
| otter         |   10.75M   |
| lru           |    5.87M   |
| tinylfu       |    4.19M   |

- 🔥 Throughput: 143% faster than next best (freecache)

>>> TestMetaTrace: Meta Trace Hit Rate (10M ops) (go test -run=TestMetaTrace -v)

### Meta Trace Hit Rate (10M ops from Meta KVCache)

| Cache         | 50K cache | 100K cache |
|---------------|-----------|------------|
| sfcache       |   71.16%  |   78.30%   |
| otter         |   41.12%  |   56.34%   |
| ristretto     |   40.35%  |   48.99%   |
| tinylfu       |   53.70%  |   54.79%   |
| freecache     |   56.86%  |   65.52%   |
| lru           |   65.21%  |   74.22%   |

- 🔥 Meta trace: 5.5% better than next best (lru)

>>> TestHitRate: Zipf Hit Rate (go test -run=TestHitRate -v)

### Hit Rate (Zipf alpha=0.99, 1M ops, 1M keyspace)

| Cache         | Size=1% | Size=2.5% | Size=5% |
|---------------|---------|-----------|---------|
| sfcache       |  63.80% |    68.71% |  71.84% |
| otter         |  61.77% |    67.67% |  71.33% |
| ristretto     |  34.91% |    41.23% |  46.58% |
| tinylfu       |  63.83% |    68.25% |  71.56% |
| freecache     |  56.65% |    57.84% |  63.39% |
| lru           |  57.33% |    64.55% |  69.92% |

- 🔥 Hit rate: 0.34% better than next best (tinylfu)
```

Want even more comprehensive benchmarks? See https://github.com/tstromberg/gocachemark where we win the top score.

## Implementation Notes

### Differences from the S3-FIFO paper

sfcache implements the core S3-FIFO algorithm (Small/Main/Ghost queues with frequency-based promotion) with these optimizations:

1. **Dynamic Sharding** - 1-2048 independent S3-FIFO shards (vs single-threaded) for concurrent workloads
2. **Bloom Filter Ghosts** - Two rotating Bloom filters track evicted keys (vs storing actual keys), reducing memory 10-100x
3. **Lazy Ghost Checks** - Only check ghosts when evicting, saving 5-9% latency when cache isn't full
4. **Intrusive Lists** - Embed pointers in entries (vs separate nodes) for zero-allocation queue ops
5. **Fast-path Hashing** - Specialized for `int`/`string` keys using wyhash and bit mixing

## License

Apache 2.0
