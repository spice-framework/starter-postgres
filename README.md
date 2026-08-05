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
make verify
```

The normal verifier checks formatting, module/vendor reproducibility, vet,
allowlisted lint and nil safety, gosec, govulncheck, shuffled race tests, at
least 85% repository coverage, and offline vendor builds.

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
