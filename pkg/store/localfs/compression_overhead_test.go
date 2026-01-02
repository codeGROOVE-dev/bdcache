package localfs

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/codeGROOVE-dev/fido/pkg/store/compress"
)

type benchValue struct {
	Name    string
	Count   int
	Tags    []string
	Created time.Time
}

// BenchmarkCompressionOverhead measures overhead of compression abstraction.
// The None compressor should have near-zero overhead vs no-compression path.
func BenchmarkCompressionOverhead(b *testing.B) {
	ctx := context.Background()
	value := benchValue{
		Name:    "benchmark",
		Count:   42,
		Tags:    []string{"test", "benchmark", "compression"},
		Created: time.Now(),
	}

	b.Run("None", func(b *testing.B) {
		dir := b.TempDir()
		store, err := New[string, benchValue]("bench-none", dir)
		if err != nil {
			b.Fatal(err)
		}
		defer func() { _ = store.Close() }() //nolint:errcheck // cleanup

		b.ResetTimer()
		for range b.N {
			key := fmt.Sprintf("key-%d", b.N)
			if err := store.Set(ctx, key, value, time.Time{}); err != nil {
				b.Fatal(err)
			}
			if _, _, _, err := store.Get(ctx, key); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("NoneExplicit", func(b *testing.B) {
		dir := b.TempDir()
		store, err := New[string, benchValue]("bench-none-explicit", dir, compress.None())
		if err != nil {
			b.Fatal(err)
		}
		defer func() { _ = store.Close() }() //nolint:errcheck // cleanup

		b.ResetTimer()
		for range b.N {
			key := fmt.Sprintf("key-%d", b.N)
			if err := store.Set(ctx, key, value, time.Time{}); err != nil {
				b.Fatal(err)
			}
			if _, _, _, err := store.Get(ctx, key); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("S2", func(b *testing.B) {
		dir := b.TempDir()
		store, err := New[string, benchValue]("bench-s2", dir, compress.S2())
		if err != nil {
			b.Fatal(err)
		}
		defer func() { _ = store.Close() }() //nolint:errcheck // cleanup

		b.ResetTimer()
		for range b.N {
			key := fmt.Sprintf("key-%d", b.N)
			if err := store.Set(ctx, key, value, time.Time{}); err != nil {
				b.Fatal(err)
			}
			if _, _, _, err := store.Get(ctx, key); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("Zstd", func(b *testing.B) {
		dir := b.TempDir()
		store, err := New[string, benchValue]("bench-zstd", dir, compress.Zstd(1))
		if err != nil {
			b.Fatal(err)
		}
		defer func() { _ = store.Close() }() //nolint:errcheck // cleanup

		b.ResetTimer()
		for range b.N {
			key := fmt.Sprintf("key-%d", b.N)
			if err := store.Set(ctx, key, value, time.Time{}); err != nil {
				b.Fatal(err)
			}
			if _, _, _, err := store.Get(ctx, key); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// TestCompressionOverheadReport generates a human-readable report.
func TestCompressionOverheadReport(t *testing.T) {
	if os.Getenv("LOCALFS_BENCH") == "" {
		t.Skip("Set LOCALFS_BENCH=1 to run overhead report")
	}

	ctx := context.Background()
	value := benchValue{
		Name:    "benchmark",
		Count:   42,
		Tags:    []string{"test", "benchmark", "compression"},
		Created: time.Now(),
	}

	type result struct {
		name    string
		setNs   int64
		getNs   int64
		setSize int64
	}

	var results []result
	iterations := 1000

	compressors := []struct {
		name string
		c    compress.Compressor
	}{
		{"None (default)", nil},
		{"None (explicit)", compress.None()},
		{"S2", compress.S2()},
		{"Zstd-1", compress.Zstd(1)},
		{"Zstd-4", compress.Zstd(4)},
	}

	for _, tc := range compressors {
		dir := t.TempDir()
		var store *Store[string, benchValue]
		var err error
		if tc.c == nil {
			store, err = New[string, benchValue]("bench-"+tc.name, dir)
		} else {
			store, err = New[string, benchValue]("bench-"+tc.name, dir, tc.c)
		}
		if err != nil {
			t.Fatal(err)
		}

		// Warm up (best-effort, errors ignored for benchmark warmup)
		for i := range 10 {
			key := fmt.Sprintf("warmup-%d", i)
			_ = store.Set(ctx, key, value, time.Time{}) //nolint:errcheck // warmup
			_, _, _, _ = store.Get(ctx, key)            //nolint:errcheck // warmup
		}

		// Measure Set
		setStart := time.Now()
		for i := range iterations {
			key := fmt.Sprintf("key-%d", i)
			if err := store.Set(ctx, key, value, time.Time{}); err != nil {
				t.Fatal(err)
			}
		}
		setDur := time.Since(setStart)

		// Check file size (sample one file)
		loc := store.Location("key-0")
		fi, err := os.Stat(loc)
		var size int64
		if err == nil && fi != nil {
			size = fi.Size()
		}

		// Measure Get
		getStart := time.Now()
		for i := range iterations {
			key := fmt.Sprintf("key-%d", i)
			if _, _, _, err := store.Get(ctx, key); err != nil {
				t.Fatal(err)
			}
		}
		getDur := time.Since(getStart)

		results = append(results, result{
			name:    tc.name,
			setNs:   setDur.Nanoseconds() / int64(iterations),
			getNs:   getDur.Nanoseconds() / int64(iterations),
			setSize: size,
		})

		_ = store.Close() //nolint:errcheck // cleanup
	}

	// Print report
	fmt.Println("\nCompression Overhead Report")
	fmt.Println("===========================")
	fmt.Printf("\n| %-16s | %10s | %10s | %10s |\n", "Compressor", "Set ns/op", "Get ns/op", "File Size")
	fmt.Println("|------------------|------------|------------|------------|")
	for _, r := range results {
		fmt.Printf("| %-16s | %10d | %10d | %10d |\n", r.name, r.setNs, r.getNs, r.setSize)
	}

	// Verify None has minimal overhead
	noneDefault := results[0]
	noneExplicit := results[1]

	// None (default) and None (explicit) should be within 20% of each other
	setDiff := float64(noneExplicit.setNs-noneDefault.setNs) / float64(noneDefault.setNs) * 100
	getDiff := float64(noneExplicit.getNs-noneDefault.getNs) / float64(noneDefault.getNs) * 100

	fmt.Printf("\nOverhead Analysis:\n")
	fmt.Printf("  None (explicit) vs None (default): Set %.1f%%, Get %.1f%%\n", setDiff, getDiff)

	if setDiff > 20 {
		t.Errorf("None (explicit) Set overhead too high: %.1f%%", setDiff)
	}
	if getDiff > 20 {
		t.Errorf("None (explicit) Get overhead too high: %.1f%%", getDiff)
	}
}
