# Coding standards

## Language and style

- Go 1.23 or newer. `gofmt` formatting, `go vet` clean.
- Standard library first. Add a dependency only if it appears in [dependencies](dependencies.md).
- Package names short and lower case; no stutter (`store.Store` is acceptable, `store.StoreBlob` is not).
- Exported identifiers have doc comments that describe behaviour. Unexported helpers are commented only when the reason is not obvious.
- No global mutable state. Configuration, clocks, loggers, and clients are passed in.
- No `init()` for registration or side effects.

## Comments and documentation text

- Comments, doc comments, README, and documentation describe the code and the system as they are now.
- They contain no references to how or by whom the code was produced, no planning or progress notes, no references to build order, iterations, or reviews, and no change-log style remarks.
- Open work is tracked outside the source tree. A reminder comment is allowed only in the form `// TODO(go1.25): replace with slices.X`: a concrete external condition in parentheses, then the reason.

## Errors

- Wrap with context: `fmt.Errorf("commit blob %s: %w", sha, err)`. Messages lowercase, no trailing punctuation.
- Sentinel errors for conditions callers branch on; `errors.Is` and `errors.As` for checks.
- Errors that become HTTP statuses use `StatusError`.
- Never ignore errors from `Close` on written files, `Rename`, `Sync`; handle or log them.
- Panics only for programmer errors at startup; handlers recover.

## Concurrency

- Every goroutine has an owner and a way to stop (context or channel) and is joined on shutdown.
- Shared state is protected by a mutex or owned by one goroutine; document which.
- Contexts flow from the request or from the server lifecycle; detached downloads use a server-lifetime context with their own timeouts.
- The race detector must pass: `go test -race ./...` where cgo is available.

## I/O

- Stream bodies; never read a whole artifact into memory. Documents that are parsed or rewritten are bounded by `max_parse_bytes`.
- Always use `io.Copy` style loops with limits (`io.LimitReader`) for untrusted sizes.
- File creation modes 0640 and directories 0750.
- Temp files live in `data_dir/tmp`, never in the system temp directory.

## Logging

- `log/slog`, structured, key names from [observability](../02-architecture/observability.md).
- No secrets, no full upstream header values, no request bodies.
- Levels: debug for per-attempt details, info for requests and lifecycle, warn for recoverable problems, error for data-integrity and operator-attention events.

## Configuration

- Strict YAML decoding (`KnownFields(true)`); defaults applied in one place; validation returns all errors together.
- Every setting is documented in the [reference](../05-configuration/reference.md); a test checks that each key in the config structs appears in the reference.

## Tests

- Table-driven where natural; names say what behaviour is checked.
- Hermetic: no network, no reliance on wall-clock sleeps (use the fake clock and channels).
- Every bug fix adds a regression test.

## Quality gates (all must pass before work is considered complete)

1. `gofmt -l .` prints nothing.
2. `go vet ./...` passes.
3. `go build ./...` passes with `CGO_ENABLED=0`.
4. `go test ./...` passes; `go test -race ./...` passes where available.
5. Test coverage: at least 80% statement coverage for `cache`, `store`, `index`, `storage`, `upstream`, and every kind package; reported with `go test -cover`.
6. No file in the repository contains planning or progress remarks (grep check in `scripts/ci.sh`).
7. Every behaviour in the documents touched by the work has a test in the [test matrix](test-matrix.md).
