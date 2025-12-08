// Package cloudrun provides automatic persistence backend selection for Cloud Run environments.
// It detects Cloud Run via K_SERVICE environment variable and attempts to use Datastore,
// falling back to local files if Datastore is unavailable or if not running in Cloud Run.
package cloudrun

import (
	"context"
	"os"

	"github.com/codeGROOVE-dev/sfcache"
	"github.com/codeGROOVE-dev/sfcache/pkg/persist/datastore"
	"github.com/codeGROOVE-dev/sfcache/pkg/persist/localfs"
)

// New creates a persistence layer optimized for Cloud Run environments.
// It automatically selects the best available backend:
//   - In Cloud Run (K_SERVICE env var set): tries Datastore, falls back to local files on error
//   - Outside Cloud Run: uses local files
//
// The cacheID is used as the database name for Datastore or subdirectory for local files.
// This function always succeeds by falling back to local files if Datastore is unavailable.
func New[K comparable, V any](ctx context.Context, cacheID string) (sfcache.Store[K, V], error) {
	// Try Datastore in Cloud Run environments
	if os.Getenv("K_SERVICE") != "" {
		p, err := datastore.New[K, V](ctx, cacheID)
		if err == nil {
			return p, nil
		}
		// Datastore unavailable, fall through to local files
	}

	// Fall back to local files (either not in Cloud Run, or Datastore failed)
	return localfs.New[K, V](cacheID, "")
}
