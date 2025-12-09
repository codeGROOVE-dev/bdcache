# persist/datastore

Google Cloud Datastore persistence with native TTL support.

## Features

- Scales automatically, native TTL support
- JSON encoding with base64 for binary safety
- Streaming loads for warmup
- Works across Cloud Run instances

## Usage

```go
import (
    "github.com/codeGROOVE-dev/sfcache"
    "github.com/codeGROOVE-dev/sfcache/persist/datastore"
)

// cacheID becomes the Datastore database name
p, _ := datastore.New[string, User](ctx, "myapp")

// Optional: clean up stale entries on startup
p.Cleanup(ctx, 24*time.Hour)

cache, _ := sfcache.New[string, User](ctx,
    sfcache.WithPersistence(p))
```

## TTL Setup (Recommended)

```bash
gcloud firestore fields ttls update expiry \
  --collection-group=CacheEntry \
  --enable-ttl \
  --database=myapp
```

One-time setup per database. Datastore deletes expired entries within 24 hours.

## Fallback Pattern

```go
p, err := datastore.New[string, User](ctx, "myapp")
if err != nil {
    p, _ = localfs.New[string, User]("myapp", "")
}
```

## Key Constraints

- Maximum key length: 1500 characters (Datastore limit)
