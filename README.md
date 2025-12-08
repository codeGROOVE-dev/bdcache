# sfcache - Stupid Fast Cache

<img src="media/logo-small.png" alt="sfcache logo" width="256">

[![Go Reference](https://pkg.go.dev/badge/github.com/codeGROOVE-dev/sfcache.svg)](https://pkg.go.dev/github.com/codeGROOVE-dev/sfcache)
[![Go Report Card](https://goreportcard.com/badge/github.com/codeGROOVE-dev/sfcache)](https://goreportcard.com/report/github.com/codeGROOVE-dev/sfcache)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](https://opensource.org/licenses/Apache-2.0)

<br clear="right">

Stupid fast in-memory Go cache with optional L2 persistence layer.

Designed for persistently caching API requests in an unreliable environment, this cache has something for everyone.

## Features

- **Faster than a bat out of hell** - Best-in-class latency and throughput
- **S3-FIFO eviction** - Better hit-rates than LRU ([learn more](https://s3fifo.com/))
- **L2 Persistence (optional)** - Bring your own database or use built-in backends:
  - [`pkg/persist/localfs`](pkg/persist/localfs) - Local files (gob encoding, zero dependencies)
  - [`pkg/persist/datastore`](pkg/persist/datastore) - Google Cloud Datastore
  - [`pkg/persist/valkey`](pkg/persist/valkey) - Valkey/Redis
  - [`pkg/persist/cloudrun`](pkg/persist/cloudrun) - Auto-detect Cloud Run
- **Per-item TTL** - Optional expiration
- **Thundering herd prevention** - `GetSet` deduplicates concurrent loads for the same key
- **Graceful degradation** - Cache works even if persistence fails
- **Zero allocation reads** - minimal GC thrashing
- **Type safe** - Go generics

## Usage

As a stupid-fast in-memory cache:

```go
import "github.com/codeGROOVE-dev/sfcache"

// strings as keys, ints as values
cache := sfcache.New[string, int]()
cache.Set("answer", 42)
val, found := cache.Get("answer")
```

or with local file persistence to survive restarts:

```go
import (
  "github.com/codeGROOVE-dev/sfcache"
  "github.com/codeGROOVE-dev/sfcache/pkg/persist/localfs"
)

p, _ := localfs.New[string, User]("myapp", "")
cache, _ := sfcache.NewTiered[string, User](p)

cache.SetAsync(ctx, "user:123", user) // Don't wait for the key to persist
cache.Store.Len(ctx)                  // Access persistence layer directly
```

A persistent cache suitable for Cloud Run or local development; uses Cloud Datastore if available

```go
import "github.com/codeGROOVE-dev/sfcache/pkg/persist/cloudrun"

p, _ := cloudrun.New[string, User](ctx, "myapp")
cache, _ := sfcache.NewTiered[string, User](p)
```

## Performance against the Competition

sfcache prioritizes high hit-rates and low read latency, but it's excellent all around. Run `make bench` to see the results for yourself:

```
>>> TestLatencyNoEviction: Latency - No Evictions (Set cycles within cache size) (go test -run=TestLatencyNoEviction -v)
| Cache         | Get ns/op | Get B/op | Get allocs | Set ns/op | Set B/op | Set allocs |
|---------------|-----------|----------|------------|-----------|----------|------------|
| sfcache       |       7.0 |        0 |          0 |      21.0 |        0 |          0 |
| lru           |      21.0 |        0 |          0 |      21.0 |        0 |          0 |
| ristretto     |      32.0 |       14 |          0 |      76.0 |      121 |          4 |
| otter         |      34.0 |        0 |          0 |     137.0 |       51 |          1 |
| freecache     |      57.0 |        8 |          1 |      48.0 |        0 |          0 |
| tinylfu       |      71.0 |        0 |          0 |     108.0 |      168 |          3 |

- 🔥 Get: 200% better than next best (lru)
- 🔥 Set: 0.000% better than next best (lru)

>>> TestLatencyWithEviction: Latency - With Evictions (Set uses 20x unique keys) (go test -run=TestLatencyWithEviction -v)
| Cache         | Get ns/op | Get B/op | Get allocs | Set ns/op | Set B/op | Set allocs |
|---------------|-----------|----------|------------|-----------|----------|------------|
| sfcache       |       8.0 |        0 |          0 |      79.0 |        0 |          0 |
| lru           |      21.0 |        0 |          0 |      80.0 |       80 |          1 |
| ristretto     |      30.0 |       13 |          0 |      74.0 |      119 |          3 |
| otter         |      34.0 |        0 |          0 |     175.0 |       60 |          1 |
| freecache     |      58.0 |        8 |          1 |      94.0 |        1 |          0 |
| tinylfu       |      73.0 |        0 |          0 |     108.0 |      168 |          3 |

- 🔥 Get: 162% better than next best (lru)
- 💧 Set: 6.8% worse than best (ristretto)

>>> TestZipfThroughput1: Zipf Throughput (1 thread) (go test -run=TestZipfThroughput1 -v)

### Zipf Throughput (alpha=0.99, 75% read / 25% write): 1 threads

| Cache         | QPS        |
|---------------|------------|
| sfcache       |   98.80M   |
| lru           |   47.40M   |
| tinylfu       |   20.10M   |
| freecache     |   15.59M   |
| otter         |   13.37M   |
| ristretto     |   11.41M   |

- 🔥 Throughput: 108% faster than next best (lru)

>>> TestZipfThroughput16: Zipf Throughput (16 threads) (go test -run=TestZipfThroughput16 -v)

### Zipf Throughput (alpha=0.99, 75% read / 25% write): 16 threads

| Cache         | QPS        |
|---------------|------------|
| sfcache       |   42.18M   |
| freecache     |   15.08M   |
| ristretto     |   14.10M   |
| otter         |   10.70M   |
| lru           |    6.03M   |
| tinylfu       |    4.21M   |

- 🔥 Throughput: 180% faster than next best (freecache)

>>> TestMetaTrace: Meta Trace Hit Rate (10M ops) (go test -run=TestMetaTrace -v)

### Meta Trace Hit Rate (10M ops from Meta KVCache)

| Cache         | 50K cache | 100K cache |
|---------------|-----------|------------|
| sfcache       |   68.53%  |   76.34%   |
| otter         |   41.37%  |   56.14%   |
| ristretto     |   40.35%  |   48.95%   |
| tinylfu       |   53.70%  |   54.79%   |
| freecache     |   56.86%  |   65.52%   |
| lru           |   65.21%  |   74.22%   |

- 🔥 Meta trace: 2.9% better than next best (lru)

>>> TestHitRate: Zipf Hit Rate (go test -run=TestHitRate -v)

### Hit Rate (Zipf alpha=0.99, 1M ops, 1M keyspace)

| Cache         | Size=1% | Size=2.5% | Size=5% |
|---------------|---------|-----------|---------|
| sfcache       |  64.41% |    69.24% |  72.57% |
| otter         |  62.28% |    67.81% |  71.42% |
| ristretto     |  34.87% |    41.25% |  46.49% |
| tinylfu       |  63.83% |    68.25% |  71.56% |
| freecache     |  56.65% |    57.75% |  63.39% |
| lru           |  57.33% |    64.55% |  69.92% |

- 🔥 Hit rate: 1.3% better than next best (tinylfu)
```

Cache performance is a game of balancing trade-offs. There will be workloads where other cache implementations are better, but nobody blends speed and persistence like we do.

## Implementation Notes

### Differences from the S3-FIFO paper

sfcache implements the core S3-FIFO algorithm (Small/Main/Ghost queues with frequency-based promotion) with these optimizations:

1. **Dynamic Sharding** - 1-256 independent S3-FIFO shards (vs single-threaded) for concurrent workloads
2. **Bloom Filter Ghosts** - Two rotating Bloom filters track evicted keys (vs storing actual keys), reducing memory 10-100x
3. **Lazy Ghost Checks** - Only check ghosts when evicting, saving 5-9% latency when cache isn't full
4. **Intrusive Lists** - Embed pointers in entries (vs separate nodes) for zero-allocation queue ops
5. **Fast-path Hashing** - Specialized for `int`/`string` keys using wyhash and bit mixing

## License

Apache 2.0
