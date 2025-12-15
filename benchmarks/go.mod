module github.com/codeGROOVE-dev/sfcache/benchmarks

go 1.25.4

require (
	github.com/codeGROOVE-dev/sfcache v1.5.0
	github.com/coocood/freecache v1.2.4
	github.com/dgraph-io/ristretto v0.2.0
	github.com/hashicorp/golang-lru/v2 v2.0.7
	github.com/klauspost/compress v1.18.2
	github.com/maypok86/otter/v2 v2.2.1
	github.com/vmihailenco/go-tinylfu v0.2.2
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	golang.org/x/sys v0.39.0 // indirect
)

replace github.com/codeGROOVE-dev/sfcache => ../
