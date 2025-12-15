module github.com/codeGROOVE-dev/sfcache/pkg/store/valkey

go 1.25.4

require (
	github.com/codeGROOVE-dev/sfcache/pkg/store/compress v1.5.0
	github.com/valkey-io/valkey-go v1.0.69
)

require (
	github.com/klauspost/compress v1.18.2 // indirect
	golang.org/x/sys v0.39.0 // indirect
)

replace github.com/codeGROOVE-dev/sfcache/pkg/store/compress => ../compress
