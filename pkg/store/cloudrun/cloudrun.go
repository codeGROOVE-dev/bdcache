// Package cloudrun provides automatic persistence backend selection for Cloud Run.
// Detects Cloud Run via K_SERVICE env var and tries Datastore first,
// falling back to local files if unavailable.
package cloudrun

import (
	"context"
	"os"
	"time"

	"github.com/codeGROOVE-dev/fido/pkg/store/compress"
	"github.com/codeGROOVE-dev/fido/pkg/store/datastore"
	"github.com/codeGROOVE-dev/fido/pkg/store/localfs"
)

// Store is the persistence interface returned by New.
// Matches fido.Store so callers can pass it to fido.NewTiered.
type Store[K comparable, V any] interface {
	ValidateKey(key K) error
	Get(ctx context.Context, key K) (V, time.Time, bool, error)
	Set(ctx context.Context, key K, value V, expiry time.Time) error
	Delete(ctx context.Context, key K) error
	Cleanup(ctx context.Context, maxAge time.Duration) (int, error)
	Flush(ctx context.Context) (int, error)
	Len(ctx context.Context) (int, error)
	Close() error
}

// New creates a persistence layer for Cloud Run environments.
// In Cloud Run: tries Datastore, falls back to local files on error.
// Outside Cloud Run: uses local files directly.
// Optional compressor enables compression (e.g., compress.S2() for Snappy-compatible).
func New[K comparable, V any](ctx context.Context, cacheID string, c ...compress.Compressor) (Store[K, V], error) {
	if os.Getenv("K_SERVICE") != "" {
		if p, err := datastore.New[K, V](ctx, cacheID, c...); err == nil {
			return p, nil
		}
	}
	return localfs.New[K, V](cacheID, "", c...)
}
