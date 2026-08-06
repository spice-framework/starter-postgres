module github.com/spice-framework/starter-postgres

go 1.26.0

toolchain go1.26.5

require (
	github.com/jackc/pgx/v5 v5.10.0
	github.com/spice-framework/spice v0.0.0-20260805222830-a2ecd56df246
)

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/spice-framework/development v0.0.0-20260806121906-963bb6676069 // indirect
	github.com/spice-framework/toolchain v0.0.0-20260806054457-a83d9b58034c // indirect
	golang.org/x/mod v0.38.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/text v0.40.0 // indirect
)

tool (
	github.com/spice-framework/development/cmd/spice-dev
	github.com/spice-framework/toolchain/cmd/spice-library-release-verify
)
