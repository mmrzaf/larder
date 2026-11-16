# Kind interface

A kind turns a request path into a decision. Everything else (storage, fetching, serving, verification, eviction) is the engine's job. This page defines the contract; exact Go signatures may be adapted as long as the behaviour is kept.

```go
package kinds

type Class int

const (
    Immutable   Class = iota + 1 // stored forever once verified
    Mutable                      // revalidated, generations kept
    Passthrough                  // streamed, never stored
    Deny                         // 404
    Local                        // answered without upstream or storage
)

// Request is the normalized view of an incoming request.
type Request struct {
    Method     string
    Path       string      // path after the mount, begins with "/", percent-encoding preserved
    RawQuery   string
    Header     http.Header
    PublicBase string      // public URL of this repo, no trailing slash
}

// Plan tells the engine what to do.
type Plan struct {
    Class        Class
    Key          string        // normalized key without variant
    Variant      string        // "", "abbrev", "full", "html", "json"
    UpstreamPath string        // appended to each upstream base; may carry a query
    UpstreamHdr  http.Header   // extra request headers (for example Accept)
    FreshTTL     time.Duration // 0 uses the repo default
    Binary       bool          // reject text/html bodies
    Redirect     string        // non-empty: answer 301 to this path
    AllowPost    bool          // permit POST (npm audit endpoints)
    Timeout      time.Duration // per-request passthrough timeout
    FailureStatus int          // passthrough failure status; 0 means 502
    Local        *LocalResponse
    Hooks        Hooks
}

// FetchFunc retrieves one document through the repository's upstreams.
type FetchFunc func(ctx context.Context, path string) ([]byte, error)

// ErrVerificationUnavailable means verification must be retried later.
var ErrVerificationUnavailable = errors.New("verification unavailable")

type Hooks struct {
    // Resolve overrides how upstream URLs are built (crates download templates,
    // pypi _files hosts). Return the full URLs to try, in order.
    Resolve func(ups []Upstream) ([]URLCandidate, error)

    // Validate inspects the finished temporary file before commit (zip opens,
    // JSON decodes). Returning an error discards the candidate.
    Validate func(ctx context.Context, f *os.File, size int64) error

    // Verify performs protocol-level verification that needs an upstream
    // document (Go sumdb), returning the digest to record or
    // ErrVerificationUnavailable to defer it. Returning another error discards
    // the candidate.
    Verify func(ctx context.Context, raw io.ReaderAt, size int64, fetch FetchFunc) (string, error)

    // Expect returns digests the stored bytes must match, beyond those in the
    // expectations table (by-hash paths, generic config entries).
    Expect func(key string) []Digest

    // Companions lists digest files to fetch next to an immutable artifact,
    // in the order they are tried.
    Companions func(key string) []Companion

    // ChecksumFor maps a checksum file to the artifact key and algorithm it
    // describes, so a conflicting checksum can be reported.
    ChecksumFor func(key string) (artifactKey, algo string, ok bool)

    // RefreshOnMismatch returns metadata keys to revalidate before one retry
    // when a digest expectation disagrees with a candidate.
    RefreshOnMismatch func(key string) []string

    // Ingest runs after commit with the raw bytes and may record expectations
    // and invalidate related objects through the Sink.
    Ingest func(ctx context.Context, obj ObjectRef, raw io.Reader, sink Sink) error

    // Rewrite transforms a stored mutable document when it is served.
    // It must be deterministic for (bytes, PublicBase).
    Rewrite func(raw []byte, publicBase string) ([]byte, error)
}

type Kind interface {
    Name() string
    Classify(req Request) (Plan, error) // returns an HTTP-status-carrying error for bad input
}

// Sink is what Ingest may do.
type Sink interface {
    Expect(key, algo, hexDigest string)         // buffered; flushed in one transaction
    MarkStale(prefixGlob string)               // reset freshness of mutable objects
    Logger() *slog.Logger
}

// A Sink may also implement ReplaceExpectation(key, algo, hexDigest string).
// The engine treats the digest as authoritative and replaces older rows for
// the same repo and key, which apt uses when a Release file changes.
```

## Contract

1. `Classify` is a pure function of its input and the repo's configuration. No I/O.
2. `Key` is stable: the same logical object always yields the same key, whatever spelling the client used.
3. `Rewrite` output depends only on `(raw, publicBase)`.
4. `Ingest` is streaming where documents can be large, bounded by `max_parse_bytes`, and must not panic on malformed input; malformed documents are logged and ignored (the object is still stored).
5. `Validate` and `Ingest` never perform network requests. `Verify` uses the engine-provided `FetchFunc`; companion fetching is the engine's job, driven by `Companions`.
6. Errors from `Classify` map to client statuses through `kinds.StatusError{Code, Msg}`.

## Registry

`kinds.Register(name, constructor)` in each kind package's `init`-free registration function called from `cmd/larder` (explicit wiring, no `init()` side effects). A constructor receives the typed repo configuration and returns a `Kind` or a validation error.

## Test helper

`kindtest.Table(t, kind, []Case{{Path, Header, Want Plan}})` runs classification tables; every kind has one with all rows from its ecosystem page.
