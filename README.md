# multicache

A sharded in-memory cache for Go with optional persistence.

Implements S3-FIFO from ["FIFO queues are all you need for cache eviction"](https://dl.acm.org/doi/10.1145/3600006.3613147) (SOSP'23). S3-FIFO matches or exceeds LRU hit rates with simpler operations and better concurrency.

## Install

```
go get github.com/codeGROOVE-dev/multicache
```

## Use

```go
cache := multicache.New[string, int](multicache.Size(10000))
cache.Set("answer", 42)
val, ok := cache.Get("answer")
```

With persistence:

```go
store, _ := localfs.New[string, User]("myapp", "")
cache, _ := multicache.NewTiered(store)

cache.Set(ctx, "user:123", user)           // sync write
cache.SetAsync(ctx, "user:456", user)      // async write
```

GetSet deduplicates concurrent loads:

```go
user, err := cache.GetSet("user:123", func() (User, error) {
    return db.LoadUser("123")
})
```

## Options

```go
multicache.Size(n)           // max entries (default 16384)
multicache.TTL(time.Hour)    // default expiration
```

## Persistence

Memory cache backed by durable storage. Reads check memory first; writes go to both.

| Backend | Import |
|---------|--------|
| Local filesystem | `pkg/store/localfs` |
| Valkey/Redis | `pkg/store/valkey` |
| Google Cloud Datastore | `pkg/store/datastore` |
| Auto-detect (Cloud Run) | `pkg/store/cloudrun` |

All backends support S2 or Zstd compression via `pkg/store/compress`.

## Performance

[gocachemark](https://github.com/tstromberg/gocachemark) compares 15 cache libraries across hit rate, latency, throughput, and memory. Composite scores (Dec 2025):

```
#1  multicache   132 points
#2  otter         51 points
#3  2q            47 points
```

Where multicache wins:

- **Throughput**: 6x faster than otter at 32 threads (Get/Set)
- **Hit rate**: +4% vs LRU on Meta's KVCache trace
- **Latency**: 7ns Get, zero allocations

Where others win:

- **Memory**: otter and freecache use less memory per entry
- **Some traces**: LRU beats S3-FIFO on purely temporal workloads

Run `make bench` or see gocachemark for full results.

## Algorithm

S3-FIFO uses three queues: small (new entries), main (promoted entries), and ghost (recently evicted keys). New items enter small; items accessed twice move to main. The ghost queue tracks evicted keys in a bloom filter to fast-track their return.

This implementation adds:

- **Dynamic sharding** - scales to 16×GOMAXPROCS shards; at 32 threads: 21x Get throughput, 6x Set throughput vs single shard
- **Tuned small queue** - 24.7% vs paper's 10%, chosen via sweep in 0.1% increments to maximize wins across 9 production traces
- **Ghost frequency restoration** - returning keys restore 50% of their previous access count; +0.26% zipf, +0.1% meta
- **Extended frequency cap** - max freq=7 vs paper's 3; +0.9% meta, +0.8% zipf
- **Hot item demotion** - items that were once hot (freq≥4) get demoted to small queue instead of evicted; +0.24% zipf

The ghost queue bloom filters and frequency maps add ~80 bytes/item overhead compared to CLOCK (119 vs 38 bytes/item)

Details: [s3fifo.com](https://s3fifo.com/)

## License

Apache 2.0
