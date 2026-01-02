<p align="center">
  <img src="media/logo-small.png" alt="fido logo" width="200">
</p>

# fido

[![Go Report Card](https://goreportcard.com/badge/github.com/codeGROOVE-dev/fido)](https://goreportcard.com/report/github.com/codeGROOVE-dev/fido)
[![Go Reference](https://pkg.go.dev/badge/github.com/codeGROOVE-dev/fido.svg)](https://pkg.go.dev/github.com/codeGROOVE-dev/fido)
[![Release](https://img.shields.io/github/v/release/codeGROOVE-dev/fido)](https://github.com/codeGROOVE-dev/fido/releases)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](https://opensource.org/licenses/Apache-2.0)

fido is the most well-rounded cache implementation for Go today.

Designed for real-world applications in unstable environments, it has a higher average hit rate, higher throughput, and lower latency for production workloads than any other cache. To deal with process eviction in environments like Kubernetes, Cloud Run, or Borg, it also offers an optional persistence tier.

## Install

```
go get github.com/codeGROOVE-dev/fido
```

## Use

```go
cache := fido.New[string, int](fido.Size(10000))
cache.Set("answer", 42)
val, ok := cache.Get("answer")
```

With persistence:

```go
store, _ := localfs.New[string, User]("myapp", "")
cache, _ := fido.NewTiered(store)

_ = cache.Set(ctx, "user:123", user)       // sync write
_ = cache.SetAsync(ctx, "user:456", user)  // async write
```

GetSet deduplicates concurrent loads to prevent thundering herd situations:

```go
user, err := cache.GetSet("user:123", func() (User, error) {
    return db.LoadUser("123")
})
```

## Options

```go
fido.Size(n)           // max entries (default 16384)
fido.TTL(time.Hour)    // default expiration
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

fido has been exhaustively tested for performance using [gocachemark](https://github.com/tstromberg/gocachemark).

Where fido wins:

- **Throughput**: 551M int gets/sec avg (2.4X faster than otter). 89M string sets/sec avg (27X faster than otter).
- **Hit rate**: Wins 6 of 9 workloads. Highest average across all datasets (+2.7% vs otter, +0.9% vs sieve).
- **Latency**: 8ns int gets, 10ns string gets, zero allocations (3.5X lower latency than otter)

Where others win:

- **Memory**: freelru and otter use less memory per entry (73 bytes/item overhead vs 14 for otter)
- **Specific workloads**: sieve +0.5% on thesios-block, clock +0.1% on ibm-docker, theine +0.6% on zipf

Much of the credit for high throughput goes to [puzpuzpuz/xsync](https://github.com/puzpuzpuz/xsync) and its lock-free data structures.

Run `make benchmark` for full results, or see [benchmarks/gocachemark_results.md](benchmarks/gocachemark_results.md).

## Algorithm

fido uses [S3-FIFO](https://s3fifo.com/), which features three queues: small (new entries), main (promoted entries), and ghost (recently evicted keys). New items enter small; items accessed twice move to main. The ghost queue tracks evicted keys in a bloom filter to fast-track their return.

fido has been hyper-tuned for high performance, and deviates from the original paper in a handful of ways:

- **Tuned small queue** - 13.7% vs paper's 10%, tuned via binary search to maximize average hit rate across 9 production traces
- **Full ghost frequency restoration** - returning keys restore 100% of their previous access count
- **Increased frequency cap** - max freq=5 vs paper's 3, tuned via binary search for best average hit rate
- **Death row** - hot items (high peakFreq) get a second chance before eviction
- **Extended ghost capacity** - 1.22x cache size for ghost tracking, tuned via binary search
- **Ghost frequency ring buffer** - fixed-size 256-entry ring replaces map allocations

## License

Apache 2.0
