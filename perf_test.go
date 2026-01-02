//go:build !race

package fido

// Performance tests removed - CI environments have variable performance
// characteristics that make hard-coded thresholds unreliable.
// Use `go test -bench=.` for performance benchmarking instead.
