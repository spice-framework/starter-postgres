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
| Release renderer | `github.com/spice-framework/development/cmd/spice-dev` at `v0.0.0-20260806132124-4c308d1b9fda` |
| Release verifier tool | `github.com/spice-framework/toolchain/cmd/spice-library-release-verify` at `v0.0.0-20260806133530-71211498297c` |
| Release trust anchor | [`security/release/ed25519-public.pem`](../security/release/ed25519-public.pem), DER SHA-256 fingerprint `cc42428a74b539af7f6975d84b63c830267ac227062fc412970fc5ad586b7e65` |

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
path. Windows and Linux CI render the same inert central plan twice under
vendor-only offline resolution and require byte-identical unsigned artifacts.

The reviewed public release anchor is committed and its matching private key is
stored only as the repository Actions secret
`SPICE_LIBRARY_RELEASE_SIGNING_KEY`. The caller forwards that one named secret;
the protected `release-signing` environment contains no secret copy and still
gates the signing job through required review. Only a GitHub Release produced
through the complete protected ceremony is a signed publication claim.
