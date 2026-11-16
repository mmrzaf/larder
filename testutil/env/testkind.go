package env

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/mmrzaf/larder/internal/config"
	"github.com/mmrzaf/larder/internal/integrity"
	"github.com/mmrzaf/larder/internal/kinds"
)

// TestKind exercises every kind hook. It is registered as kind "test".
type TestKind struct{}

// NewTestKind builds the test-only kind.
func NewTestKind(config.Repo) (kinds.Kind, error) { return TestKind{}, nil }

// Name returns "test".
func (TestKind) Name() string { return "test" }

// Classify maps test paths to plans.
func (TestKind) Classify(req kinds.Request) (kinds.Plan, error) {
	pathValue := req.Path
	upstreamPath := pathValue
	if req.RawQuery != "" {
		upstreamPath += "?" + req.RawQuery
	}
	plan := kinds.Plan{Key: pathValue, UpstreamPath: upstreamPath, Class: kinds.Mutable}

	switch {
	case pathValue == "/deny":
		plan.Class = kinds.Deny
	case pathValue == "/local":
		plan.Class = kinds.Local
		plan.Local = &kinds.LocalResponse{Status: http.StatusOK, Body: []byte("local")}
	case pathValue == "/redirect":
		plan.Redirect = "/local"
	case pathValue == "/passthrough":
		plan.Class = kinds.Passthrough
	case strings.HasPrefix(pathValue, "/plain/"):
		plan.Class = kinds.Immutable
		plan.Binary = true
	case strings.HasPrefix(pathValue, "/validate"):
		plan.Class = kinds.Immutable
		plan.Binary = true
		plan.Hooks.Validate = validateHook
	case strings.HasPrefix(pathValue, "/companion"):
		plan.Class = kinds.Immutable
		plan.Binary = true
		plan.Hooks.Companions = func(string) []kinds.Companion {
			return []kinds.Companion{{Suffix: ".sha256", Algo: "sha256"}}
		}
	case strings.HasPrefix(pathValue, "/expect/"):
		plan.Class = kinds.Immutable
		plan.Binary = true
		digest := strings.TrimPrefix(pathValue, "/expect/")
		plan.Hooks.Expect = func(string) []integrity.Digest {
			return []integrity.Digest{{Algo: "sha256", Hex: digest}}
		}
	case pathValue == "/resolve":
		plan.Class = kinds.Immutable
		plan.Binary = true
		plan.Hooks.Resolve = func(ups []kinds.Upstream) ([]kinds.URLCandidate, error) {
			if len(ups) == 0 {
				return nil, fmt.Errorf("no upstream")
			}
			return []kinds.URLCandidate{{URL: strings.TrimSuffix(ups[0].URL, "/") + "/resolved"}}, nil
		}
	case pathValue == "/rewrite":
		plan.Hooks.Rewrite = func(raw []byte, publicBase string) ([]byte, error) {
			return []byte(publicBase + "|" + string(raw)), nil
		}
	case pathValue == "/ingest":
		plan.Hooks.Ingest = ingestHook
	}
	return plan, nil
}

func validateHook(_ context.Context, file *os.File, _ int64) error {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(file, 1<<20))
	if err != nil {
		return err
	}
	if !strings.HasPrefix(string(data), "valid") {
		return fmt.Errorf("invalid test artifact")
	}
	return nil
}

type ingestDocument struct {
	Expect []struct {
		Key    string `json:"key"`
		Algo   string `json:"algo"`
		Digest string `json:"digest"`
	} `json:"expect"`
	Stale []string `json:"stale"`
}

func ingestHook(_ context.Context, _ kinds.ObjectRef, raw io.Reader, sink kinds.Sink) error {
	data, err := io.ReadAll(io.LimitReader(raw, 1<<20))
	if err != nil {
		return err
	}
	var doc ingestDocument
	if err := json.Unmarshal(data, &doc); err != nil {
		sink.Logger().Warn("test_ingest_malformed", "error", err)
		return nil
	}
	for _, expectation := range doc.Expect {
		sink.Expect(expectation.Key, expectation.Algo, expectation.Digest)
	}
	for _, pattern := range doc.Stale {
		sink.MarkStale(pattern)
	}
	return nil
}
