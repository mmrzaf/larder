# Project rules for Larder

Larder is a self-hosted write-through package cache written in Go. The specification is in `docs/` and is the single source of truth. Start at `docs/README.md`.

## Working method

1. Read the documents named in the task before writing code. Read them fully, not by skimming headings.
2. Work in small steps: implement one unit, write its tests, run them, then continue.
3. Do not invent behaviour. When the documents are silent, choose the simplest option that is consistent with them, and list the choice in the final report under "Decisions".
4. When the documents and the code must differ, make the code correct first, then update the documents so they describe the final behaviour as it is.
5. Do not run `git commit`, `git push`, or change branches. The owner commits.
6. Do not delete or weaken tests to make them pass. Fix the code.
7. Do not leave stubs or placeholders in delivered scope. If something in scope cannot be done, stop and report it.
8. Do not add dependencies beyond `docs/07-engineering/dependencies.md`.

## Content rules for every file you write

- Code, comments, documentation, commit-ready text, and test names describe the system as it is.
- Never write planning or progress remarks: no mention of stages, phases, tasks, steps, iterations, reviews, reviewers, change sets, "added", "new", "fixed", "improved", or of any tool or person that produced the code.
- No TODO or FIXME unless it names a concrete external condition, for example `TODO(go1.25): use slices.X`.
- Comments explain why, not what. Doc comments on exported identifiers describe behaviour.

## Engineering rules (details in `docs/07-engineering/coding-standards.md`)

- Go 1.23 or newer, `CGO_ENABLED=0`, standard library first.
- Stream bodies; memory must not grow with object size.
- No global mutable state; pass dependencies in; time through a `Clock` interface.
- Wrap errors with context; never ignore errors from `Close`, `Sync`, `Rename` on written files.
- Every goroutine has an owner and a stop path.
- Structured logging with `log/slog`; never log secrets.
- Paths on disk derive from digests only, never from request data.
- Files mode 0640, directories 0750.

## Tests

- Name scenario tests `TestMatrix_<ID>_<Description>` using IDs from `docs/07-engineering/test-matrix.md`.
- Hermetic: no internet; use `httptest` fake upstreams and the fault injection helpers in `testutil`.
- Use fake clocks, channels, and counters instead of sleeps.
- Add fuzz tests for path normalization and every parser.
- Add a regression test for every bug you fix.

## Quality gates

Run all of these before reporting; every one must pass:

```
gofmt -l .                       # prints nothing
go vet ./...
CGO_ENABLED=0 go build ./...
go test ./...
go test -race ./...              # when a C toolchain is available; otherwise say so in the report
go test -cover ./...             # >= 80% for cache, store, index, storage, upstream and each kind
```

Also run a grep for banned remarks (see "Content rules") over everything you wrote.

## Self-audit before reporting (perfectionism pass)

1. Re-read each document you were told to read. For every requirement sentence, locate the code and the test that covers it. List anything without both and fix it.
2. Check every error path: what happens on failure, timeout, cancellation, partial data.
3. Check resource handling: files closed, goroutines stopped, temp files removed.
4. Check security items in `docs/02-architecture/security.md` that touch your code.
5. Re-run all quality gates after the last change.

## Final report format

Keep it short and factual:

```
Scope: <what was implemented, by package>
Gates: <each command and PASS/FAIL, race detector availability>
Matrix: <IDs covered by tests; IDs not covered with the reason>
Decisions: <choices made where the docs were silent or ambiguous>
Docs updated: <files changed, or none>
Open issues: <anything not done, or none>
```
