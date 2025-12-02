# bdcache - Big Dumb Cache

<img src="media/logo-small.png" alt="bdcache logo" width="256">

[![Go Reference](https://pkg.go.dev/badge/github.com/codeGROOVE-dev/bdcache.svg)](https://pkg.go.dev/github.com/codeGROOVE-dev/bdcache)
[![Go Report Card](https://goreportcard.com/badge/github.com/codeGROOVE-dev/bdcache)](https://goreportcard.com/report/github.com/codeGROOVE-dev/bdcache)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](https://opensource.org/licenses/Apache-2.0)

<br clear="right">

Blazing fast in-memory Go cache with optional L2 persistence layer.

## Install

```bash
go get github.com/codeGROOVE-dev/bdcache
```

## Use

```go
import (
    "github.com/codeGROOVE-dev/bdcache"
    "github.com/codeGROOVE-dev/bdcache/persist/localfs"
)

// Memory only
cache, _ := bdcache.New[string, int](ctx)
cache.Set(ctx, "answer", 42, 0)           // Synchronous: returns after persistence completes
cache.SetAsync(ctx, "answer", 42, 0)      // Async: returns immediately, persists in background
val, found, _ := cache.Get(ctx, "answer")

// With local file persistence
p, _ := localfs.New[string, User]("myapp", "")
cache, _ := bdcache.New[string, User](ctx,
    bdcache.WithPersistence(p))

// With Valkey/Redis persistence
p, _ := valkey.New[string, User](ctx, "myapp", "localhost:6379")
cache, _ := bdcache.New[string, User](ctx,
    bdcache.WithPersistence(p))

// Cloud Run auto-detection (datastore in Cloud Run, localfs elsewhere)
p, _ := cloudrun.New[string, User](ctx, "myapp")
cache, _ := bdcache.New[string, User](ctx,
    bdcache.WithPersistence(p))
```

## Features

- **Faster than a bat out of hell** - Low latency, high throughput
- **S3-FIFO eviction** - Better hit-rates than LRU ([learn more](https://s3fifo.com/))
- **Pluggable persistence** - Bring your own database or use built-in backends:
  - [`persist/localfs`](persist/localfs) - Local files (gob encoding, zero dependencies)
  - [`persist/datastore`](persist/datastore) - Google Cloud Datastore
  - [`persist/valkey`](persist/valkey) - Valkey/Redis
  - [`persist/cloudrun`](persist/cloudrun) - Auto-detect Cloud Run
- **Per-item TTL** - Optional expiration
- **Graceful degradation** - Cache works even if persistence fails
- **Zero allocation reads** - minimal GC thrashing
- **Type safe** - Go generics

## Performance against the Competition

bdcache prioritizes high hit-rates and low read latency, but it performs quite well all around.

Here's the results from an M4 MacBook Pro - run `make bench` to see the results for yourself:
```
════════════════════════════════════════════════════════════════════════════╝

### Hit Rate (Zipf α=0.99, 1M ops, 1M keyspace)

| Cache         | Size=1% | Size=2.5% | Size=5% |
|---------------|---------|-----------|---------|
| bdcache 🟡    |  94.46% |    94.89% |  95.09% |
| otter 🦦      |  94.28% |    94.69% |  95.09% |
| ristretto ☕  |  91.62% |    92.45% |  93.03% |
| tinylfu 🔬    |  94.31% |    94.87% |  95.09% |
| freecache 🆓  |  94.03% |    94.15% |  94.75% |
| lru 📚        |  94.10% |    94.84% |  95.09% |

🏆 Hit rate: +0.1% better than 2nd best (tinylfu)

### Single-Threaded Latency (sorted by Get)

| Cache         | Get ns/op | Get B/op | Get allocs | Set ns/op | Set B/op | Set allocs |
|---------------|-----------|----------|------------|-----------|----------|------------|
| bdcache 🟡    |       9.0 |        0 |          0 |      21.0 |        0 |          0 |
| lru 📚        |      24.0 |        0 |          0 |      23.0 |        0 |          0 |
| ristretto ☕  |      32.0 |       14 |          0 |      67.0 |      119 |          3 |
| otter 🦦      |      35.0 |        0 |          0 |     140.0 |       51 |          1 |
| freecache 🆓  |      73.0 |       15 |          1 |      58.0 |        4 |          0 |
| tinylfu 🔬    |      88.0 |        3 |          0 |     107.0 |      175 |          3 |

🏆 Get latency: +167% faster than 2nd best (lru)
🏆 Set latency: +9.5% faster than 2nd best (lru)

### Single-Threaded Throughput (mixed read/write)

| Cache         | Get QPS    | Set QPS    |
|---------------|------------|------------|
| bdcache 🟡    |   75.49M   |   41.56M   |
| lru 📚        |   34.86M   |   35.33M   |
| ristretto ☕  |   28.38M   |   13.59M   |
| otter 🦦      |   25.59M   |    7.17M   |
| freecache 🆓  |   12.79M   |   15.80M   |
| tinylfu 🔬    |   10.77M   |    8.94M   |

🏆 Get throughput: +117% faster than 2nd best (lru)
🏆 Set throughput: +18% faster than 2nd best (lru)

### Concurrent Throughput (mixed read/write): 4 threads

| Cache         | Get QPS    | Set QPS    |
|---------------|------------|------------|
| bdcache 🟡    |   29.51M   |   31.43M   |
| otter 🦦      |   28.96M   |    4.17M   |
| ristretto ☕  |   27.16M   |   13.23M   |
| freecache 🆓  |   25.06M   |   21.94M   |
| lru 📚        |    9.43M   |    9.59M   |
| tinylfu 🔬    |    5.51M   |    4.85M   |

🏆 Get throughput: +1.9% faster than 2nd best (otter)
🏆 Set throughput: +43% faster than 2nd best (freecache)

### Concurrent Throughput (mixed read/write): 8 threads

| Cache         | Get QPS    | Set QPS    |
|---------------|------------|------------|
| bdcache 🟡    |   22.16M   |   18.82M   |
| otter 🦦      |   19.51M   |    3.14M   |
| ristretto ☕  |   18.62M   |   11.60M   |
| freecache 🆓  |   16.60M   |   15.92M   |
| lru 📚        |    7.62M   |    7.75M   |
| tinylfu 🔬    |    4.95M   |    4.26M   |

🏆 Get throughput: +14% faster than 2nd best (otter)
🏆 Set throughput: +18% faster than 2nd best (freecache)

### Concurrent Throughput (mixed read/write): 12 threads

| Cache         | Get QPS    | Set QPS    |
|---------------|------------|------------|
| bdcache 🟡    |   24.29M   |   24.21M   |
| ristretto ☕  |   22.76M   |   11.54M   |
| otter 🦦      |   21.65M   |    2.79M   |
| freecache 🆓  |   17.25M   |   16.53M   |
| lru 📚        |    7.58M   |    7.62M   |
| tinylfu 🔬    |    4.51M   |    3.87M   |

🏆 Get throughput: +6.7% faster than 2nd best (ristretto)
🏆 Set throughput: +47% faster than 2nd best (freecache)

### Concurrent Throughput (mixed read/write): 16 threads

| Cache         | Get QPS    | Set QPS    |
|---------------|------------|------------|
| bdcache 🟡    |   16.24M   |   15.77M   |
| otter 🦦      |   16.02M   |    2.76M   |
| ristretto ☕  |   15.41M   |   12.50M   |
| freecache 🆓  |   15.05M   |   14.61M   |
| lru 📚        |    7.45M   |    7.47M   |
| tinylfu 🔬    |    4.71M   |    3.61M   |

🏆 Get throughput: +1.4% faster than 2nd best (otter)
🏆 Set throughput: +8.0% faster than 2nd best (freecache)

### Concurrent Throughput (mixed read/write): 24 threads

| Cache         | Get QPS    | Set QPS    |
|---------------|------------|------------|
| bdcache 🟡    |   16.16M   |   15.47M   |
| otter 🦦      |   15.80M   |    2.87M   |
| ristretto ☕  |   15.48M   |   13.28M   |
| freecache 🆓  |   14.92M   |   14.36M   |
| lru 📚        |    7.69M   |    7.59M   |
| tinylfu 🔬    |    5.03M   |    3.84M   |

🏆 Get throughput: +2.3% faster than 2nd best (otter)
🏆 Set throughput: +7.7% faster than 2nd best (freecache)

### Concurrent Throughput (mixed read/write): 32 threads

| Cache         | Get QPS    | Set QPS    |
|---------------|------------|------------|
| bdcache 🟡    |   15.85M   |   15.41M   |
| otter 🦦      |   15.71M   |    2.85M   |
| ristretto ☕  |   15.60M   |   13.16M   |
| freecache 🆓  |   14.33M   |   14.13M   |
| lru 📚        |    7.70M   |    8.07M   |
| tinylfu 🔬    |    5.32M   |    2.99M   |

🏆 Get throughput: +0.9% faster than 2nd best (otter)
🏆 Set throughput: +9.1% faster than 2nd best (freecache)
```

There will certainly be benchmarks where other caches perform faster, but no solution blends speed and persistence the way that bdcache does.

## License

Apache 2.0
