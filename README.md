# bdcache - Big Dumb Cache

<img src="media/logo-small.png" alt="bdcache logo" width="256" align="right">

[![Go Reference](https://pkg.go.dev/badge/github.com/codeGROOVE-dev/bdcache.svg)](https://pkg.go.dev/github.com/codeGROOVE-dev/bdcache)
[![Go Report Card](https://goreportcard.com/badge/github.com/codeGROOVE-dev/bdcache)](https://goreportcard.com/report/github.com/codeGROOVE-dev/bdcache)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](https://opensource.org/licenses/Apache-2.0)

Simple, fast, secure Go cache with [S3-FIFO eviction](https://s3fifo.com/) - better hit rates than LRU.

## Why?

- **S3-FIFO Algorithm** - [Superior cache hit rates](https://s3fifo.com/) compared to LRU/LFU
- **Fast** - ~20ns per operation, zero allocations
- **Reliable** - Memory cache always works, even if persistence fails
- **Smart Persistence** - Local files for dev, Cloud Datastore for Cloud Run
- **Minimal Dependencies** - Only one (Cloud Datastore)

## Install

```bash
go get github.com/codeGROOVE-dev/bdcache
```

## Use

```go
// Memory only
cache, err := bdcache.New[string, int](ctx)
if err != nil {
    panic(err)
}
if err := cache.Set(ctx, "answer", 42, 0); err != nil {
    panic(err)
}
val, found, err := cache.Get(ctx, "answer")

// With smart persistence (files for dev, Datastore for Cloud Run)
cache, err := bdcache.New[string, User](ctx, bdcache.WithBestStore("myapp"))
```

## Features

- **S3-FIFO eviction** - Better than LRU ([learn more](https://s3fifo.com/))
- **Type safe** - Go generics
- **Persistence** - Local files (gob) or Cloud Datastore (JSON)
- **Graceful degradation** - Cache works even if persistence fails
- **Per-item TTL** - Optional expiration

## Performance

Benchmarks from MacBook Pro M4 Max:

```
Memory-only operations:
BenchmarkCache_Get_Hit-16      56M ops/sec    17.8 ns/op       0 B/op     0 allocs
BenchmarkCache_Set-16          56M ops/sec    17.8 ns/op       0 B/op     0 allocs

With file persistence enabled:
BenchmarkCache_Get_PersistMemoryHit-16    85M ops/sec    11.8 ns/op       0 B/op     0 allocs
BenchmarkCache_Get_PersistDiskRead-16     73K ops/sec    13.8 µs/op    7921 B/op   178 allocs
BenchmarkCache_Set_WithPersistence-16      9K ops/sec   112.3 µs/op    2383 B/op    36 allocs
```

## License

Apache 2.0
