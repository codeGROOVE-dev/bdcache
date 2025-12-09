# persist/valkey

Valkey/Redis persistence for shared cache across instances.

## Features

- Share cache across multiple application instances
- Native TTL support via SETEX/GET
- JSON encoding for values
- Automatic connection pooling
- Compatible with both Valkey and Redis

## Usage

```go
import (
    "github.com/codeGROOVE-dev/sfcache"
    "github.com/codeGROOVE-dev/sfcache/persist/valkey"
)

// Connect to Valkey/Redis server
p, _ := valkey.New[string, User](ctx, "myapp", "localhost:6379")

cache, _ := sfcache.New[string, User](ctx,
    sfcache.WithPersistence(p))
```

## Configuration

The `cacheID` parameter is used as a key prefix to namespace your cache entries.

## When to Use

- Multiple application instances need to share cache state
- You already have Valkey/Redis infrastructure
- You need distributed cache invalidation
- Horizontal scaling requires shared cache

## Key Format

Keys are stored as: `{cacheID}:{key}`

For example, with cacheID "myapp" and key "user:123":
- Redis key: `myapp:user:123`
