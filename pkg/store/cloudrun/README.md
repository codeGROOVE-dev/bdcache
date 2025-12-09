# persist/cloudrun

Automatic persistence backend selection for Cloud Run environments.

## Overview

This package detects whether your application is running in Google Cloud Run and automatically selects the best persistence backend:

- **In Cloud Run** (when `K_SERVICE` env var is set): Attempts to use Google Cloud Datastore
- **Datastore unavailable**: Gracefully falls back to local file persistence
- **Outside Cloud Run**: Uses local file persistence

## Usage

```go
import (
    "github.com/codeGROOVE-dev/sfcache"
    "github.com/codeGROOVE-dev/sfcache/persist/cloudrun"
)

// Automatic backend selection with fallback
p, _ := cloudrun.New[string, User](ctx, "myapp")
cache, _ := sfcache.New[string, User](ctx,
    sfcache.WithPersistence(p))
```

The function always succeeds by falling back to local files if Datastore is unavailable due to:
- Missing credentials
- Network issues
- Configuration problems
- Running outside Cloud Run

## When to Use

Use this package when:
- Deploying to Cloud Run but want the flexibility to run locally
- You want automatic environment detection without manual configuration
- You prefer graceful degradation over hard failures

For production deployments where you need explicit control, consider using the `datastore` or `localfs` packages directly.
