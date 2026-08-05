# PostgreSQL runtime dependency review

## Decision

The starter uses `github.com/jackc/pgx/v5` v5.10.0 through its standard
`database/sql` adapter. pgx is isolated in this opt-in module and never enters
the Spice core dependency graph.

## Maintenance and license

pgx v5 is actively maintained, uses the MIT license, and supports PostgreSQL
versions from the last five years. The module license and transitive licenses
are retained by vendoring. The official PostgreSQL container is a hosted test
dependency only and is pinned by immutable multi-platform index digest.

## Security and configuration

- Connection identity must be a complete PostgreSQL URL; environment fallback
  and file-backed service configuration are rejected.
- TLS hostname verification is inserted by default. Insecure mode is an
  explicit opt-in intended for isolated local/container tests.
- Validation errors never include the URL or password.
- Pool bounds and application-name metadata are validated before construction.
- gosec, govulncheck, Dependabot, and the repository vendor graph cover the
  selected runtime and tool dependencies.

## Cancellation and ownership

Construction performs no network I/O. Each pool is instance-owned and must be
closed by its application. Connection establishment, readiness, transactions,
queries, migration locks, batch operations, outbox operations, and SQL test
slices use caller-owned contexts. Real-system tests prove canceled advisory
lock waits and bounded cleanup.

## Observability and durability

The bounded application name is safe server metadata. The starter uses Spice's
driver-neutral transaction, migration, batch, and outbox contracts, so callers
can apply module-aware observation without global driver hooks. Deterministic
DDL is application-owned. Migration registry updates share the migration
transaction; batch and outbox ownership use atomic PostgreSQL statements and
reject stale leases or receipts.

Primary references:

- <https://pkg.go.dev/github.com/jackc/pgx/v5>
- <https://pkg.go.dev/github.com/jackc/pgx/v5/stdlib>
- <https://hub.docker.com/_/postgres>
