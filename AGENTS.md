# Starter PostgreSQL implementation contract

This repository owns the independently versioned PostgreSQL integration for
Spice. Work directly on local `main` in bounded commits. Fetch before editing
and immediately before pushing; never overwrite unexpected remote work.

Go 1.26.5 is mandatory. Every product change must preserve secure connection
defaults, caller-owned contexts and pools, transaction semantics, deterministic
migration/outbox/batch behavior, and public core-contract isolation. Add
positive and failure-path tests, update public documentation, run `make verify`
on the exact commit tree, and push only a green commit.

The normal gate is offline after dependencies are cached. Docker-backed
PostgreSQL acceptance is an additional release and hosted integration gate; it
may not replace deterministic unit and failure-path tests. Never commit live
database credentials or weaken TLS defaults to simplify production use.

Release-rehearsal work must preserve the exact `spice-dev` tool version
authorized by the root `go.mod`, invoke its full package path, and render the
same inert plan twice with workspace and network resolution disabled in vendor
mode. The protected central workflow is the sole production builder. An
unsigned rehearsal must never manufacture signatures or key material.
