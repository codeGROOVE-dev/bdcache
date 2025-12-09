module github.com/codeGROOVE-dev/sfcache/pkg/store/cloudrun

go 1.25.4

require (
	github.com/codeGROOVE-dev/sfcache/pkg/store/datastore v1.3.0
	github.com/codeGROOVE-dev/sfcache/pkg/store/localfs v1.4.0
)

require github.com/codeGROOVE-dev/ds9 v0.8.0 // indirect

replace github.com/codeGROOVE-dev/sfcache/pkg/store/datastore => ../datastore

replace github.com/codeGROOVE-dev/sfcache/pkg/store/localfs => ../localfs
