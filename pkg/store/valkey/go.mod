module github.com/codeGROOVE-dev/fido/pkg/store/valkey

go 1.25.4

require (
	github.com/codeGROOVE-dev/fido/pkg/store/compress v1.10.0
	github.com/valkey-io/valkey-go v1.0.70
)

require (
	github.com/klauspost/compress v1.18.3 // indirect
	golang.org/x/sys v0.40.0 // indirect
)

replace github.com/codeGROOVE-dev/fido/pkg/store/compress => ../compress
