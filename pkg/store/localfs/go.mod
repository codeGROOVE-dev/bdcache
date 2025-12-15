module github.com/codeGROOVE-dev/sfcache/pkg/store/localfs

go 1.25.4

require (
	github.com/codeGROOVE-dev/sfcache/pkg/store/compress v1.5.0
	github.com/klauspost/compress v1.18.2
	github.com/pierrec/lz4/v4 v4.1.22
)

replace github.com/codeGROOVE-dev/sfcache/pkg/store/compress => ../compress
