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

## Signing and verification

Generate an offline Ed25519 PKCS#8 key and keep it outside the repository:

```text
openssl genpkey -algorithm ED25519 -out starter-postgres-release-key.pem
```

The command also accepts a base64-encoded 32-byte Ed25519 seed or 64-byte
private key. GitHub reads the protected `SPICE_POSTGRES_RELEASE_SIGNING_KEY` secret. The
private key is never copied into source, SBOM, logs, or release output.

Verify downloaded assets before use:

```text
openssl pkeyutl -verify -pubin -inkey checksums.txt.pem -rawin -in checksums.txt -sigfile checksums.txt.sig
sha256sum -c checksums.txt
```

The public key is currently distributed beside the signature. That proves the
checksum file and the published key belong together, but it is not by itself an
independent identity anchor: an attacker able to replace every release asset
could replace both. Until a reviewed starter-postgres public-key fingerprint is
pinned in this repository, authenticity is rooted in the protected GitHub
repository, exact Git tag, release page, and signing-secret controls. Consumers
must obtain assets from that channel and must not treat an untrusted mirror plus
its accompanying key as authenticated. Pinning and publishing the long-lived
key fingerprint is required before claiming key-rooted authenticity.

PowerShell users can compare the first checksum column with
`Get-FileHash -Algorithm SHA256` for each named artifact.

## Release ceremony

1. Run `make verify` once on the final clean commit, then `make verify-release`.
2. Create and push an annotated canonical `vX.Y.Z` tag.
3. The tag workflow repeats both gates, derives the epoch from the tag commit,
   builds and signs the artifacts, verifies signature and hashes, and publishes
   them with `gh release create`.
4. Download the published assets and independently verify the signature,
   checksums, source prefix, and SPDX document.

GitHub is the distribution mirror; the same repository command constructs
identical artifacts offline on Windows, Linux, and macOS.
