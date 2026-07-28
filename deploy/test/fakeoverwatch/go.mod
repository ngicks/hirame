// A separate module, not a package of apps/search-api: this is test-harness
// scaffolding for one environment and must not become a dependency of the
// shipped binaries.
module github.com/ngicks/hirame/deploy/test/fakeoverwatch

go 1.26.5

require (
	github.com/ngicks/go-overwatch/overwatch v0.0.0-20260727200048-eeb6a0d98a39
	google.golang.org/grpc v1.82.1
	google.golang.org/protobuf v1.36.11
)

require (
	golang.org/x/net v0.53.0 // indirect
	golang.org/x/sys v0.46.0 // indirect
	golang.org/x/text v0.36.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260414002931-afd174a4e478 // indirect
)

// Same replace apps/search-api carries, and for a stronger reason here: the
// module proxy cannot serve this module at all (its zip contains
// e2e/vm-truenas/con.py, and "con" is a reserved path component on Windows, so
// sum.golang.org answers 404). The submodule checkout is the only source.
replace github.com/ngicks/go-overwatch/overwatch => ../../../go-overwatch/overwatch
