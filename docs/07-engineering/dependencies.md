# Dependencies

The binary is static (`CGO_ENABLED=0`). The allowed third-party modules:

| Module | Purpose | Notes |
|---|---|---|
| `modernc.org/sqlite` | embedded index database | pure Go, no cgo |
| `gopkg.in/yaml.v3` | configuration and seed files | strict decoding |
| `golang.org/x/net` | `proxy` package for SOCKS5 dialing | also `http/httpproxy` for `env` proxy mode and `html` for PyPI index parsing |
| `golang.org/x/mod` | `module`, `semver`, `sumdb/dirhash` for Go modules | strict Go verification, path validation |
| `golang.org/x/sys` | `unix` for `flock` and `statfs` | Linux |
| `github.com/ulikunitz/xz` | `.xz` decompression for apt `Packages.xz` | pure Go |
| `github.com/pelletier/go-toml/v2` | rustup channel manifests | |

Standard library packages relied on: `net/http`, `net/http/httptest`, `log/slog`, `database/sql`, `compress/gzip`, `compress/bzip2`, `archive/zip`, `archive/tar`, `crypto/sha256`, `crypto/sha512`, `crypto/sha1`, `crypto/md5`, `encoding/json`, `encoding/xml`, `flag`, `regexp`, `context`, `sync`, `os/signal`, `testing`.

Not used: web frameworks, CLI frameworks, logging libraries, the Prometheus client library (the text format is implemented in `internal/metrics`), ORMs, dependency injection frameworks.

## Rules for adding a dependency

1. Standard library cannot reasonably do it.
2. Pure Go, maintained, permissive license (MIT, BSD, Apache-2.0).
3. Small and focused.
4. Added to this table with its purpose in the same change.

## Supply chain

- `go.sum` committed; `go mod verify` in CI.
- `-trimpath`, version stamped with `-ldflags`.
- Final image contains only the binary and CA certificates.
