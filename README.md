# bdcache - Big Dumb Cache

<img src="media/logo-small.png" alt="bdcache logo" width="256">

[![Go Reference](https://pkg.go.dev/badge/github.com/codeGROOVE-dev/bdcache.svg)](https://pkg.go.dev/github.com/codeGROOVE-dev/bdcache)
[![Go Report Card](https://goreportcard.com/badge/github.com/codeGROOVE-dev/bdcache)](https://goreportcard.com/report/github.com/codeGROOVE-dev/bdcache)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](https://opensource.org/licenses/Apache-2.0)

<br clear="right">

Fast, persistent Go cache with S3-FIFO eviction - better hit rates than LRU, survives restarts with pluggable persistence backends, zero allocations.

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

- **S3-FIFO eviction** - Better than LRU ([learn more](https://s3fifo.com/))
- **Type safe** - Go generics
- **Pluggable persistence** - Bring your own database or use built-in backends:
  - [`persist/localfs`](persist/localfs) - Local files (gob encoding, zero dependencies)
  - [`persist/datastore`](persist/datastore) - Google Cloud Datastore
  - [`persist/valkey`](persist/valkey) - Valkey/Redis
  - [`persist/cloudrun`](persist/cloudrun) - Auto-detect Cloud Run
- **Graceful degradation** - Cache works even if persistence fails
- **Per-item TTL** - Optional expiration

## Performance against the Competition

bdcache biases toward being the highest hit-rate for real-world workloads with the lowest read latency. We're not bad either!

* #1 in hit-rate for real-world workloads (zipf)
* #1 in single-threaded read latency (9 ns/op) - half the competition
* #1 for read/write throughput - 10X faster writes than otter!

Here's the results from an M4 MacBook Pro - run `make bench` to see the results for yourself:

```
### Hit Rate (Zipf α=0.99, 1M ops, 1M keyspace)

| Cache      | Size=2.5% | Size=5% | Size=10% |
|------------|-----------|---------|----------|
| bdcache    |   94.89% | 95.09% |  95.09% |
| otter      |   94.69% | 95.09% |  95.09% |
| ristretto  |   92.45% | 93.02% |  93.55% |
| tinylfu    |   94.87% | 95.09% |  95.09% |
| freecache  |   94.15% | 94.75% |  95.09% |
| lru        |   94.84% | 95.09% |  95.09% |

### Single-Threaded Latency (sorted by Get)

| Cache      | Get ns/op | Get B/op | Get allocs | Set ns/op | Set B/op | Set allocs |
|------------|-----------|----------|------------|-----------|----------|------------|
| bdcache    |       9.0 |        0 |          0 |      21.0 |        1 |          0 |
| lru        |      23.0 |        0 |          0 |      22.0 |        0 |          0 |
| ristretto  |      32.0 |       14 |          0 |      67.0 |      119 |          3 |
| otter      |      35.0 |        0 |          0 |     139.0 |       51 |          1 |
| freecache  |      71.0 |       15 |          1 |      56.0 |        4 |          0 |
| tinylfu    |      83.0 |        3 |          0 |     106.0 |      175 |          3 |

### Single-Threaded Throughput (mixed read/write)

| Cache      | Get QPS    | Set QPS    |
|------------|------------|------------|
| bdcache    |   75.69M   |   43.02M   |
| lru        |   36.51M   |   36.91M   |
| ristretto  |   27.79M   |   13.96M   |
| otter      |   25.36M   |    7.43M   |
| freecache  |   13.12M   |   16.20M   |
| tinylfu    |   11.27M   |    9.07M   |

### Concurrent Throughput (mixed read/write): 4 threads

| Cache      | Get QPS    | Set QPS    |
|------------|------------|------------|
| otter      |   29.15M   |    4.33M   |
| bdcache    |   28.75M   |   30.41M   |
| ristretto  |   26.98M   |   13.33M   |
| freecache  |   25.14M   |   21.76M   |
| lru        |    9.22M   |    9.32M   |
| tinylfu    |    5.42M   |    4.93M   |

### Concurrent Throughput (mixed read/write): 8 threads

| Cache      | Get QPS    | Set QPS    |
|------------|------------|------------|
| bdcache    |   21.98M   |   18.58M   |
| otter      |   19.51M   |    2.98M   |
| ristretto  |   18.43M   |   11.15M   |
| freecache  |   16.79M   |   15.99M   |
| lru        |    7.72M   |    7.71M   |
| tinylfu    |    4.88M   |    4.18M   |

### Concurrent Throughput (mixed read/write): 12 threads

| Cache      | Get QPS    | Set QPS    |
|------------|------------|------------|
| bdcache    |   23.59M   |   23.84M   |
| ristretto  |   22.35M   |   11.22M   |
| otter      |   21.69M   |    2.83M   |
| freecache  |   16.95M   |   16.43M   |
| lru        |    7.43M   |    7.44M   |
| tinylfu    |    4.50M   |    4.07M   |

...

### Concurrent Throughput (mixed read/write): 32 threads

| Cache      | Get QPS    | Set QPS    |
|------------|------------|------------|
| bdcache    |   16.45M   |   15.37M   |
| otter      |   15.62M   |    2.84M   |
| ristretto  |   15.47M   |   13.35M   |
| freecache  |   14.58M   |   14.29M   |
| lru        |    7.77M   |    7.92M   |
| tinylfu    |    5.23M   |    3.50M   |
```

There will certainly be scenarios where other caches perform faster, but no one blends speed and persistence the way that bdcache does.

## License

Apache 2.0
