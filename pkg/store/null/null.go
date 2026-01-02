// Package null provides a no-op store implementation for fido.
// All gets return not found, all sets are discarded.
// Useful for testing or when the TieredCache API is desired without persistence.
package null

import (
	"context"
	"time"

	"github.com/codeGROOVE-dev/fido/pkg/store/compress"
)

// Store implements a no-op persistence store.
// It satisfies the fido.Store interface without storing anything.
type Store[K comparable, V any] struct{}

// New creates a new null store.
// Optional compressor is accepted for API consistency but ignored.
func New[K comparable, V any](_ ...compress.Compressor) *Store[K, V] {
	return &Store[K, V]{}
}

// ValidateKey always returns nil (all keys are valid).
func (*Store[K, V]) ValidateKey(_ K) error {
	return nil
}

// Get always returns not found.
//
//nolint:revive // function-result-limit: required by Store interface
func (*Store[K, V]) Get(_ context.Context, _ K) (value V, expiry time.Time, found bool, err error) {
	var zero V
	return zero, time.Time{}, false, nil
}

// Set discards the value and returns nil.
func (*Store[K, V]) Set(_ context.Context, _ K, _ V, _ time.Time) error {
	return nil
}

// Delete is a no-op and returns nil.
func (*Store[K, V]) Delete(_ context.Context, _ K) error {
	return nil
}

// Cleanup is a no-op and returns 0.
func (*Store[K, V]) Cleanup(_ context.Context, _ time.Duration) (int, error) {
	return 0, nil
}

// Location returns "null".
func (*Store[K, V]) Location(_ K) string {
	return "null"
}

// Flush is a no-op and returns 0.
func (*Store[K, V]) Flush(_ context.Context) (int, error) {
	return 0, nil
}

// Len always returns 0.
func (*Store[K, V]) Len(_ context.Context) (int, error) {
	return 0, nil
}

// Close is a no-op and returns nil.
func (*Store[K, V]) Close() error {
	return nil
}
