module github.com/codeGROOVE-dev/bdcache/benchmarks

go 1.25.4

require (
	github.com/bits-and-blooms/bloom/v3 v3.7.1
	github.com/codeGROOVE-dev/bdcache v0.0.0
	github.com/dgraph-io/ristretto v0.2.0
	github.com/dgryski/go-tinylfu v0.1.0
	github.com/hashicorp/golang-lru/v2 v2.0.7
	github.com/maypok86/otter/v2 v2.2.1
)

require (
	github.com/allegro/bigcache/v3 v3.1.0 // indirect
	github.com/bits-and-blooms/bitset v1.24.2 // indirect
	github.com/cespare/xxhash/v2 v2.1.2 // indirect
	github.com/coocood/freecache v1.2.4 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/vmihailenco/go-tinylfu v0.2.2 // indirect
	golang.org/x/sys v0.34.0 // indirect
)

replace github.com/codeGROOVE-dev/bdcache => ../
