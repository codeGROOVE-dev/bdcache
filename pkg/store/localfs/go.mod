module github.com/codeGROOVE-dev/fido/pkg/store/localfs

go 1.25.4

require (
	github.com/codeGROOVE-dev/fido/pkg/store/compress v1.10.0
	github.com/klauspost/compress v1.18.3
	github.com/pierrec/lz4/v4 v4.1.22
)

replace github.com/codeGROOVE-dev/fido/pkg/store/compress => ../compress
