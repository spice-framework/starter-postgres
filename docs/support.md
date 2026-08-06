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
| Release parity tool | `github.com/spice-framework/development/cmd/spice-dev` at `v0.0.0-20260806034648-1856466df09d` |

The first preview tag will define the first published minimum Spice version.
Until then, `spice-compatibility.json` is the sole compatibility boundary
source. Its minimum must equal the exact direct Spice requirement in `go.mod`;
its current value is a forward-compatibility endpoint, not an unbounded runtime
dependency. The compatibility runner uses an isolated alternate modfile and
does not mutate the committed module or vendor graph.

A release may raise the minimum only through an intentional `go.mod` change,
an updated table above, and green minimum/current compatibility jobs. A moving
branch name is never written to release metadata or used by applications.

The pinned central tool renders unsigned rehearsal candidates only. Windows
and Linux CI compare them with the retained builder under vendor-only offline
resolution; the retained command remains the signed production authority.
