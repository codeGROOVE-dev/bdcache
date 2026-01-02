package fido

import (
	"context"
	"time"
)

// Store is the persistence backend interface.
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
