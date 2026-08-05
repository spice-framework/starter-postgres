# Support and compatibility

| Contract | Current development support |
|---|---|
| Go | Exactly 1.26.5 for development and release verification |
| Spice | `v0.0.0-20260805175412-383c17744300` |
| PostgreSQL | 18.4 real-system acceptance; server versions supported by pgx v5.10.0 remain integration targets, not yet release claims |
| Operating systems | Windows, Linux, and macOS; Linux container acceptance |
| Architectures | amd64 and arm64 compilation through the public core API |
| Transport security | `verify-full` by default; explicit secure modes accepted; insecure mode requires opt-in |
| Real-system artifact | `postgres:18.4-alpine3.24` index digest `sha256:9a8afca54e7861fd90fab5fdf4c42477a6b1cb7d293595148e674e0a3181de15` |

The first preview tag will define the minimum supported Spice version. Until
then, development commits intentionally declare one exact compatible Spice
commit and fail closed outside that tested combination. Future releases will
test both the published minimum and current supported Spice lines before
raising that floor.
