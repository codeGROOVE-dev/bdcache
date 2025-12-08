module github.com/codeGROOVE-dev/sfcache/pkg/persist/valkey

go 1.25.4

require (
	github.com/codeGROOVE-dev/sfcache v1.2.1
	github.com/valkey-io/valkey-go v1.0.69
)

require golang.org/x/sys v0.39.0 // indirect

replace github.com/codeGROOVE-dev/sfcache => ../../..
