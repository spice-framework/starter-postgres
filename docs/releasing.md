# Releasing starter-postgres

This library owns its release construction. It does not use GoReleaser or a
second dependency/build graph: `cmd/starter-postgres-release` reads the
Git-tracked source tree and the committed `go.mod`, `go.sum`, and
`vendor/modules.txt` accepted by repository verification.

## Artifact contract

For `v0.1.0`, a production build creates exactly:

- `starter-postgres_0.1.0_source.tar.gz`, containing every file in the exact
  committed `HEAD` tree under the single `starter-postgres-0.1.0/` prefix;
- `starter-postgres_0.1.0_sbom.spdx.json`, an SPDX 2.3 document for the root
  module and every exact module in the committed vendor graph;
- `checksums.txt`, with SHA-256 hashes for the source archive and SBOM;
- `checksums.txt.sig`, a raw Ed25519 signature over the exact checksum file;
- `checksums.txt.pem`, the matching public key.

Archive ordering, paths, executable modes, safe relative symlinks, tar/PAX
headers, gzip headers, and SPDX creation time are derived only from sorted
`HEAD` tree objects and the source commit epoch. Gitlinks, unsafe paths, and
symlinks that escape the archive root fail closed.
Generated metadata contains no current time or absolute workspace path. The
builder performs no dependency resolution or network access. It refuses stale
vendor metadata, unsafe tracked paths, an existing output directory, or partial
output: artifacts are staged and renamed only after the complete build succeeds.

Production mode fails closed unless the checkout is completely clean, `HEAD`
has the exact requested canonical `vX.Y.Z` tag, the supplied source epoch equals
the tagged commit epoch, and an Ed25519 private key is supplied. Even untracked
files make a production checkout dirty.

`-rehearsal` is an explicit local exception. A rehearsal is always unsigned,
may be untagged or dirty, and rejects `-signing-key` rather than producing an
artifact that could be confused with a production release:

```text
go run ./cmd/starter-postgres-release -rehearsal -version v0.1.0-rc.1 -output dist-rehearsal
```

Even in a dirty rehearsal, the source archive contains committed `HEAD` bytes,
not uncommitted worktree or index content.

## Unsigned dual-builder rehearsal

The library module authorizes an exact central renderer through its
`go.mod` tool directive. `make release-parity` runs that fully qualified tool
and the retained repository builder twice each with `GOWORK=off`,
`GOPROXY=off`, `GOTOOLCHAIN=local`, and `GOFLAGS=-mod=vendor`. It first asks the
central tool for a read-only plan and then renders the plan without resolving
an ambient workspace or downloading a module.

The central signer is the protected production path. The retained repository
builder remains only the parity oracle:

```text
make release-parity
```

Both rehearsals are unsigned, deterministic across two independent outputs,
and archive the exact committed `HEAD` tree. The older retained builder and the
central renderer intentionally spell the single archive root differently:
`starter-postgres-VERSION/` and `starter-postgres_VERSION/`, respectively.
Parity therefore decodes both PAX archives, normalizes only those exact
prefixes, and requires identical entry order, paths, modes, types, links,
sizes, timestamps, extended records, gzip metadata, and content hashes. It
does not claim the compressed archives are byte-identical.

The SPDX documents must contain the same package facts and dependency
relationships after semantic ordering. These R1 differences are intentional
and validated explicitly:

- document name (`Spice PostgreSQL Starter VERSION` retained and
  `starter-postgres VERSION` centrally);
- namespace identity (the central namespace includes `spdx/v1/`);
- tool creator identifying the actual builder;
- package and relationship ordering; and
- the central document's one `DESCRIBES` relationship, which the retained R1
  builder predates and omits.

Both builders use `Organization: Spice Framework`; changing that value is not
an allowed provenance difference. Every other decoded SPDX field must match.
Each checksum file must canonically verify its own archive and SBOM. Because
both payloads have documented differences, checksum files are not expected to
be byte-identical. Extra artifacts, signatures, malformed checksums, archive
entry drift, or undocumented SBOM drift fail closed.

`make verify-release` runs this dual-builder proof. The retained command stays
in the repository for that proof but is not called by the production workflow.

## Signing and verification

Generate an offline Ed25519 PKCS#8 key and keep it outside the repository:

```text
openssl genpkey -algorithm ED25519 -out starter-postgres-release-key.pem
```

This repository owns a distinct public trust anchor at
`security/release/ed25519-public.pem`. Its SHA-256 fingerprint is
`a84f6ffe579d5779274cebebb7bc3bdb9b4718cabb8a864ab477875875c16e17`.
Review that fingerprint independently before configuring the matching private
key as `SPICE_LIBRARY_RELEASE_SIGNING_KEY` in the protected `release-signing`
environment. The private key is never copied into source, SBOM, logs, or
release output, and the committed public anchor does not by itself mean a
signed release exists.

The repository must also have a protected `release-publish` environment. Do
not create or push any release tag until both environments, required reviewers,
the secret, and the reviewed public anchor are configured. The caller does not
forward repository secrets; the central signing job can read only the secret
attached to its named environment.

Verify downloaded assets before use:

```text
openssl pkeyutl -verify -pubin -inkey checksums.txt.pem -rawin -in checksums.txt -sigfile checksums.txt.sig
sha256sum -c checksums.txt
```

Consumers must authenticate `checksums.txt.sig` against the reviewed committed
`security/release/ed25519-public.pem`, not an untrusted key downloaded beside
the release. The central workflow refuses a signing key that does not match
that trust anchor, and an independent tool verifies the complete artifact set
before the protected publish job receives it.

PowerShell users can compare the first checksum column with
`Get-FileHash -Algorithm SHA256` for each named artifact.

## Release ceremony

1. Confirm the committed public-key fingerprint above and both protected environments are active.
2. Run `make verify` once on the final clean commit, then `make verify-release`.
3. Create and push an annotated canonical `vX.Y.Z` tag.
4. The pinned central workflow validates the exact tag, signs with the protected
   key, independently authenticates the result, and publishes only the verified
   artifact set from `release-publish`.
5. Download the published assets and independently verify the signature,
   checksums, source prefix, and SPDX document.

GitHub is the distribution mirror; the same repository command constructs
identical artifacts offline on Windows, Linux, and macOS.
