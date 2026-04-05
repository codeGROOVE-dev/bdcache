module github.com/codeGROOVE-dev/fido/pkg/store/valkey

go 1.25.4

require (
	github.com/codeGROOVE-dev/fido/pkg/store/compress v1.11.0
	github.com/valkey-io/valkey-go v1.0.73
)

require (
	github.com/klauspost/compress v1.18.5 // indirect
	golang.org/x/sys v0.42.0 // indirect
)

replace github.com/codeGROOVE-dev/fido/pkg/store/compress => ../compress
