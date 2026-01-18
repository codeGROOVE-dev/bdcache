module github.com/codeGROOVE-dev/fido/pkg/store/datastore

go 1.25.4

require (
	github.com/codeGROOVE-dev/ds9 v0.8.1
	github.com/codeGROOVE-dev/fido/pkg/store/compress v1.10.0
)

require github.com/klauspost/compress v1.18.3 // indirect

replace github.com/codeGROOVE-dev/fido/pkg/store/compress => ../compress
