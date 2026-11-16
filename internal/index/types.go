package index

import (
	"fmt"
	"time"
)

// Blob is a row in the blobs table.
type Blob struct {
	SHA256     string
	RawSize    int64
	StoredSize int64
	StoredEnc  string
	RefCount   int64
	CreatedAt  time.Time
}

// Object is one generation of a cached key.
type Object struct {
	ID           int64
	Repo         string
	Key          string
	Class        string
	Generation   int64
	Current      bool
	SHA256       string
	ContentType  string
	ETag         string
	LastModified string
	Upstream     string
	FetchedAt    time.Time
	ValidatedAt  time.Time
	LastAccess   time.Time
	Hits         int64
	Verified     bool
}

// PutObjectParams describes an object to store or refresh.
type PutObjectParams struct {
	Repo         string
	Key          string
	Class        string
	SHA256       string
	ContentType  string
	ETag         string
	LastModified string
	Upstream     string
	FetchedAt    time.Time
	ValidatedAt  time.Time
	Verified     bool
}

// Expectation is a digest learned from a metadata document.
type Expectation struct {
	Repo      string
	Key       string
	Algo      string
	Digest    string
	Source    string
	CreatedAt time.Time
}

// PinSet is a named group of pinned keys.
type PinSet struct {
	Name      string
	CreatedAt time.Time
	Note      string
	Items     int64
	Rules     int64
}

// PinItem is an exact pinned key.
type PinItem struct {
	SetName string
	Repo    string
	Key     string
}

// PinRule is a glob pinned key.
type PinRule struct {
	SetName string
	Repo    string
	Pattern string
}

// Candidate is a current object joined with its blob, used by eviction.
type Candidate struct {
	Object
	RawSize    int64
	StoredSize int64
	StoredEnc  string
}

// RepoClassTotal is the distinct blob count and stored bytes for one repo and class.
type RepoClassTotal struct {
	Repo        string `json:"repo"`
	Class       string `json:"class"`
	Blobs       int64  `json:"blobs"`
	StoredBytes int64  `json:"stored_bytes"`
}

func unixTime(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func fromUnix(seconds int64) time.Time {
	if seconds == 0 {
		return time.Time{}
	}
	return time.Unix(seconds, 0).UTC()
}

func validateClass(class string) error {
	switch class {
	case "immutable", "mutable":
		return nil
	default:
		return fmt.Errorf("invalid object class %q", class)
	}
}

func validateAlgo(algo string) error {
	switch algo {
	case "sha256", "sha512", "sha1", "md5":
		return nil
	default:
		return fmt.Errorf("invalid digest algorithm %q", algo)
	}
}
