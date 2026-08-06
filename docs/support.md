# Support and compatibility

| Contract | Current development support |
|---|---|
| Go | Exactly 1.26.5 for development and release verification |
| Spice minimum/current | Exact versions in [`spice-compatibility.json`](../spice-compatibility.json) |
| PostgreSQL | 18.4 real-system acceptance; server versions supported by pgx v5.10.0 remain integration targets, not yet release claims |
| Operating systems | Windows, Linux, and macOS; Linux container acceptance |
| Architectures | amd64 and arm64 compilation through the public core API |
| Transport security | `verify-full` by default; explicit secure modes accepted; insecure mode requires opt-in |
| Real-system artifact | `postgres:18.4-alpine3.24` index digest `sha256:9a8afca54e7861fd90fab5fdf4c42477a6b1cb7d293595148e674e0a3181de15` |
| Release parity tool | `github.com/spice-framework/development/cmd/spice-dev` at `v0.0.0-20260806132124-4c308d1b9fda` |
| Release verifier tool | `github.com/spice-framework/toolchain/cmd/spice-library-release-verify` at `v0.0.0-20260806133530-71211498297c` |
| Release trust anchor | [`security/release/ed25519-public.pem`](../security/release/ed25519-public.pem), SHA-256 fingerprint `a84f6ffe579d5779274cebebb7bc3bdb9b4718cabb8a864ab477875875c16e17` |

The first preview tag will define the first published minimum Spice version.
Until then, `spice-compatibility.json` is the sole compatibility boundary
source. Its minimum must equal the exact direct Spice requirement in `go.mod`;
its current value is a forward-compatibility endpoint, not an unbounded runtime
dependency. The compatibility runner uses an isolated alternate modfile and
does not mutate the committed module or vendor graph.

A release may raise the minimum only through an intentional `go.mod` change,
an updated table above, and green minimum/current compatibility jobs. A moving
branch name is never written to release metadata or used by applications.

The pinned central signer and independent verifier are the protected production
path. Windows and Linux CI still compare the central renderer with the retained
builder under vendor-only offline resolution; the retained command is a parity
oracle only.

The reviewed public release anchor is committed and is safe to distribute. It
does not mean a signed release exists: publication remains blocked until the
matching private key and protected release environments are configured and the
documented release ceremony completes.
