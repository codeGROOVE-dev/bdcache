# multicache

multicache is an absurdly fast multi-threaded multi-tiered in-memory cache library for Go -- it offers higher performance than any other option ever created for the language.

It offers optional persistence with compression, and has been specifically optimized for Cloud Compute environments where the process is periodically restarted, such as Kubernetes or Google Cloud Run. 

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

GetSet deduplicates concurrent loads to prevent thundering herd situations:

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

For maximum efficiency, all backends support S2 or Zstd compression via `pkg/store/compress`.

## Performance

multicache has been exhaustively tested for performance using [gocachemark](https://github.com/tstromberg/gocachemark).

Where multicache wins:

- **Throughput**: 954M int gets/sec at 16 threads (2.2X faster than otter). 140M string sets/sec (9X faster than otter).
- **Hit rate**: Wins 7 of 9 workloads. Highest average across all datasets (+2.9% vs otter, +0.9% vs sieve).
- **Latency**: 8ns int gets, 10ns string gets, zero allocations (4X lower latency than otter)

Where others win:

- **Memory**: freelru and otter use less memory per entry (73 bytes/item overhead vs 15 for otter)
- **Specific workloads**: clock +0.07% on ibm-docker, theine +0.34% on zipf

Much of the credit for high throughput goes to [puzpuzpuz/xsync](https://github.com/puzpuzpuz/xsync). While highly sharded maps and flightGroups performed well, you can't beat xsync's lock-free data structures.

Run `make benchmark` for full results, or see [benchmarks/gocachemark_results.md](benchmarks/gocachemark_results.md).

## Algorithm

multicache uses [S3-FIFO](https://s3fifo.com/), which features three queues: small (new entries), main (promoted entries), and ghost (recently evicted keys). New items enter small; items accessed twice move to main. The ghost queue tracks evicted keys in a bloom filter to fast-track their return.

multicache has been hyper-tuned for high performance, and deviates from the original paper in a handful of ways:

- **Dynamic sharding** - scales to 16×GOMAXPROCS shards; at 32 threads: 21x Get throughput, 6x Set throughput vs single shard
- **Tuned small queue** - 90% vs paper's 10%, tuned via binary search to maximize average hit rate across 9 production traces
- **Full ghost frequency restoration** - returning keys restore 100% of their previous access count
- **Reduced frequency cap** - max freq=2 vs paper's 3, tuned via binary search for best average hit rate
- **Hot item demotion** - items accessed at least once (peakFreq≥1) get demoted to small queue instead of evicted
- **Extended ghost capacity** - 8x cache size for ghost tracking, tuned via binary search
- **Death row buffer** - capacity/768 buffer holds recently evicted items for instant resurrection
- **Ghost frequency ring buffer** - fixed-size 256-entry ring replaces map allocations; -5.1% string latency, -44.5% memory

## License

Apache 2.0
