module github.com/ngicks/hirame/apps/search-api

go 1.26.5

require (
	connectrpc.com/connect v1.20.0
	github.com/aws/aws-sdk-go-v2 v1.43.0
	github.com/aws/aws-sdk-go-v2/credentials v1.19.30
	github.com/aws/aws-sdk-go-v2/service/s3 v1.106.0
	github.com/caarlos0/env/v11 v11.4.1
	github.com/jackc/pgx/v5 v5.10.0
	github.com/ngicks/go-overwatch/overwatch v0.0.0-20260727200048-eeb6a0d98a39
	github.com/riverqueue/river v0.41.0
	github.com/riverqueue/river/riverdriver/riverpgxv5 v0.41.0
	github.com/riverqueue/river/rivertype v0.41.0
	golang.org/x/sync v0.22.0
	golang.org/x/text v0.40.0
	google.golang.org/grpc v1.82.1
	google.golang.org/protobuf v1.36.11
)

// The overwatch module cannot be fetched from the module proxy at any revision:
// it carries overwatch/e2e/vm-truenas/con.py, and "con" is a reserved Windows
// device name that `create zip` rejects outright. Until that file is renamed
// upstream the only way to consume the module is from the pinned submodule
// checkout, whose revision D-006 already records in this repository's index.
//
// This is deliberately a replace rather than a go.work entry: go.work would
// shadow whatever go.mod resolved and hide that a published revision was never
// in play, while a replace states outright that the submodule is the source.
replace github.com/ngicks/go-overwatch/overwatch => ../../go-overwatch/overwatch

require (
	github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream v1.7.14 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.4.31 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.7.31 // indirect
	github.com/aws/aws-sdk-go-v2/internal/v4a v1.4.32 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.13.13 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/checksum v1.9.24 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.13.31 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/s3shared v1.19.32 // indirect
	github.com/aws/smithy-go v1.27.3
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/ngicks/gahaku v0.0.0-00010101000000-000000000000
	github.com/pmezard/go-difflib v1.0.0 // indirect
	github.com/riverqueue/river/riverdriver v0.41.0 // indirect
	github.com/riverqueue/river/rivershared v0.41.0 // indirect
	github.com/stretchr/testify v1.11.1 // indirect
	github.com/tidwall/gjson v1.19.0 // indirect
	github.com/tidwall/match v1.2.0 // indirect
	github.com/tidwall/pretty v1.2.1 // indirect
	github.com/tidwall/sjson v1.2.5 // indirect
	go.uber.org/goleak v1.3.0 // indirect
	golang.org/x/net v0.57.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260414002931-afd174a4e478 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

// Gahaku commits its generated gRPC bindings
// (api/gen/proto/go/ngicks/gahaku/v1), so the render client consumes them
// directly instead of this repository generating a second copy from
// gahaku.proto. Two copies would be two things to keep in step with the
// submodule revision, and api/proto/ here owns the hirame contract only.
//
// A replace rather than a go.work entry, for the reason go.work spells out: an
// entry would shadow whatever go.mod resolved and hide that no published
// revision was ever in play, while a replace states outright that the pinned
// submodule (D-006) is the source.
replace github.com/ngicks/gahaku => ../../gahaku
