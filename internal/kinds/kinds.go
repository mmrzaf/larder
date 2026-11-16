// Package kinds defines how ecosystems classify requests and hook into the cache engine.
package kinds

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/mmrzaf/larder/internal/config"
	"github.com/mmrzaf/larder/internal/integrity"
)

// Class is how the engine treats an object.
type Class int

const (
	// Immutable means bytes never change once stored.
	Immutable Class = iota + 1
	// Mutable means content may change and is revalidated.
	Mutable
	// Passthrough means the response is streamed and never stored.
	Passthrough
	// Deny means the path is not served.
	Deny
	// Local means the answer is synthesized without upstream or storage.
	Local
)

// String returns the configuration name of the class.
func (c Class) String() string {
	switch c {
	case Immutable:
		return "immutable"
	case Mutable:
		return "mutable"
	case Passthrough:
		return "passthrough"
	case Deny:
		return "deny"
	case Local:
		return "local"
	default:
		return "unknown"
	}
}

// Request is the normalized request view passed to a kind.
type Request struct {
	Method     string
	Path       string
	RawQuery   string
	Header     http.Header
	PublicBase string
}

// Upstream is a configured base URL offered to a Resolve hook.
type Upstream struct {
	URL string
}

// URLCandidate is a complete URL a Resolve hook wants the engine to try.
type URLCandidate struct {
	URL           string
	Header        http.Header
	RestrictHosts bool
}

// Companion is a digest file fetched next to an artifact.
type Companion struct {
	Suffix string
	Algo   string
}

// ObjectRef identifies a committed object for an Ingest hook.
type ObjectRef struct {
	Repo        string
	Key         string
	Class       Class
	SHA256      string
	ContentType string
	Size        int64
	Verified    bool
}

// LocalResponse is a synthesized response.
type LocalResponse struct {
	Status      int
	Header      http.Header
	Body        []byte
	ContentType string
}

// FetchFunc retrieves one document through the repository's upstreams.
type FetchFunc func(ctx context.Context, path string) ([]byte, error)

// ErrVerificationUnavailable means verification could not be performed yet.
var ErrVerificationUnavailable = errors.New("verification unavailable")

// Hooks are the optional ecosystem behaviors used by the engine.
type Hooks struct {
	Resolve           func(ups []Upstream) ([]URLCandidate, error)
	Validate          func(ctx context.Context, f *os.File, size int64) error
	Verify            func(ctx context.Context, raw io.ReaderAt, size int64, fetch FetchFunc) (string, error)
	Expect            func(key string) []integrity.Digest
	Companions        func(key string) []Companion
	ChecksumFor       func(key string) (artifactKey, algo string, ok bool)
	RefreshOnMismatch func(key string) []string
	Ingest            func(ctx context.Context, obj ObjectRef, raw io.Reader, sink Sink) error
	Rewrite           func(raw []byte, publicBase string) ([]byte, error)
}

// Plan tells the engine what to do with a request.
type Plan struct {
	Class         Class
	Key           string
	Variant       string
	UpstreamPath  string
	UpstreamHdr   http.Header
	FreshTTL      time.Duration
	Binary        bool
	Redirect      string
	AllowPost     bool
	Timeout       time.Duration
	FailureStatus int
	Local         *LocalResponse
	Hooks         Hooks
}

// Sink is what an Ingest hook may do.
type Sink interface {
	Expect(key, algo, hexDigest string)
	MarkStale(prefixGlob string)
	Logger() *slog.Logger
}

// Kind turns a request path into a plan.
type Kind interface {
	Name() string
	Classify(req Request) (Plan, error)
}

// StatusError is an error that maps to an HTTP status.
type StatusError struct {
	Code int
	Msg  string
}

// Error implements error.
func (e *StatusError) Error() string {
	if e.Msg == "" {
		return http.StatusText(e.Code)
	}
	return e.Msg
}

// Message returns the client-facing message.
func (e *StatusError) Message() string { return e.Error() }

// Status returns a StatusError.
func Status(code int, msg string) error { return &StatusError{Code: code, Msg: msg} }

// Constructor builds a kind from one repository's configuration.
type Constructor func(config.Repo) (Kind, error)

// Registry holds the explicitly registered kinds.
type Registry struct {
	constructors map[string]Constructor
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{constructors: make(map[string]Constructor)}
}

// Register adds a constructor. It returns an error for duplicates or empty names.
func (r *Registry) Register(name string, constructor Constructor) error {
	if name == "" || constructor == nil {
		return Status(http.StatusInternalServerError, "invalid kind registration")
	}
	if _, exists := r.constructors[name]; exists {
		return Status(http.StatusInternalServerError, "kind already registered: "+name)
	}
	r.constructors[name] = constructor
	return nil
}

// New builds the configured kind for a repository.
func (r *Registry) New(repo config.Repo) (Kind, error) {
	constructor, ok := r.constructors[repo.Kind]
	if !ok {
		return nil, Status(http.StatusInternalServerError, "unknown kind: "+repo.Kind)
	}
	return constructor(repo)
}
