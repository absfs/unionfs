module github.com/absfs/unionfs/examples/testing

go 1.23.0

toolchain go1.24.7

replace github.com/absfs/unionfs => ../..

require (
	github.com/absfs/unionfs v0.0.0
	github.com/spf13/afero v1.15.0
)

require golang.org/x/text v0.28.0 // indirect
