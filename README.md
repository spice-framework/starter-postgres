# Spice PostgreSQL starter

`github.com/spice-framework/starter-postgres` is the independently versioned,
opt-in PostgreSQL integration for Spice. It provides a secure pgx-backed
`database/sql` pool plus deterministic adapters for Spice transactions,
migrations, durable outbox delivery, restartable batch execution, and SQL test
slices. Importing core alone never selects pgx.

```go
database, err := postgres.Open(postgres.Options{
    URL:             configuration.DatabaseURL,
    ApplicationName: "orders-service",
})
if err != nil {
    return nil, err
}
```

`Open` validates configuration and creates a caller-owned pool without network
I/O. `Ping` performs the explicit readiness check using the caller's context.
TLS hostname verification is the default; `sslmode=disable` requires the
explicit `AllowInsecure` test-only opt-in.

The same pool can be passed to `data.NewManager`, `NewMigrationBackend`,
`NewBatchStore`, `NewOutboxStore`, and `spicetest.NewSQL`. Schema constructors
return deterministic DDL for application-owned migrations and never mutate a
database implicitly.

## Install

```text
go get github.com/spice-framework/starter-postgres@latest
```

During preview development, applications should pin the exact compatible
commit recorded in [support metadata](docs/support.md).

## Verify

Go 1.26.5 is mandatory:

```text
make check
make release-parity
make verify
make verify-release
```

The normal verifier checks formatting, module/vendor reproducibility, vet,
allowlisted lint and nil safety, gosec, govulncheck, shuffled race tests, at
least 85% repository coverage, and offline vendor builds.

Core compatibility is a separate, network-capable release check. It exercises
the minimum and current exact versions in `spice-compatibility.json`. The
minimum must equal the direct Spice requirement in `go.mod`. Both checks use
temporary alternate module files; they never follow a moving branch during the
build or rewrite `go.mod`, `go.sum`, or `vendor`:

```text
make compatibility
```

Release parity runs the exact `spice-dev` tool authorized by `go.mod` and the
retained repository builder twice each, entirely from `vendor` with network and
workspace resolution disabled. It compares archive entries after normalizing
only the builders' documented root-directory spelling, requires equivalent
SBOM package and dependency facts, verifies both checksum files, and forbids
rehearsal signatures on Windows and Linux.

`make verify` includes both compatibility lines and one execution of the local
quality gate. Hosted CI runs the two compatibility lines independently so they
can complete in parallel.

Real-system acceptance runs all integration-tagged tests against the pinned
PostgreSQL 18.4 image digest. It proves transactions, repositories, advisory
locked migrations, rollback, cancellation, batch leases/restart, outbox
leasing/retry, and SQL test-slice rollback:

```text
docker run --detach --name spice-postgres --publish 55432:5432 \
  --env POSTGRES_PASSWORD=spice-test --env POSTGRES_DB=spice \
  postgres@sha256:9a8afca54e7861fd90fab5fdf4c42477a6b1cb7d293595148e674e0a3181de15
SPICE_POSTGRES_TEST_URL='postgres://postgres:spice-test@127.0.0.1:55432/spice?sslmode=disable' \
  make integration
docker rm --force spice-postgres
```

See [the dependency review](docs/dependency-review.md) and
[support contract](docs/support.md) before production adoption.

## Releases

The repository builds deterministic source-only releases with an SPDX 2.3
SBOM, SHA-256 checksums, and Ed25519 signatures. See the exact artifact and
clean-tag ceremony in [the release guide](docs/releasing.md).
The retained repository builder and signed production workflow remain the
release authority while the centrally rendered unsigned candidate is held to
the dual-builder parity contract.
