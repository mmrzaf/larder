package kinds

import (
	"errors"
	"net/http"
	"testing"

	"github.com/mmrzaf/larder/internal/config"
)

type testKind struct{}

func (testKind) Name() string { return "test" }
func (testKind) Classify(Request) (Plan, error) {
	return Plan{Class: Immutable}, nil
}

func TestClassString(t *testing.T) {
	for class, want := range map[Class]string{
		Immutable: "immutable", Mutable: "mutable", Passthrough: "passthrough",
		Deny: "deny", Local: "local", Class(99): "unknown",
	} {
		if got := class.String(); got != want {
			t.Errorf("Class(%d).String() = %q, want %q", class, got, want)
		}
	}
}

func TestStatusError(t *testing.T) {
	err := Status(http.StatusForbidden, "forbidden")
	var status *StatusError
	if !errors.As(err, &status) || status.Code != http.StatusForbidden || status.Message() != "forbidden" {
		t.Fatalf("status error = %#v", err)
	}
	if got := (&StatusError{Code: http.StatusNotFound}).Message(); got != http.StatusText(http.StatusNotFound) {
		t.Fatalf("empty message = %q", got)
	}
}

func TestRegistry(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Register("test", func(config.Repo) (Kind, error) { return testKind{}, nil }); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register("test", func(config.Repo) (Kind, error) { return testKind{}, nil }); err == nil {
		t.Fatal("duplicate accepted")
	}
	if err := registry.Register("", func(config.Repo) (Kind, error) { return testKind{}, nil }); err == nil {
		t.Fatal("empty name accepted")
	}
	kind, err := registry.New(config.Repo{Kind: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if kind.Name() != "test" {
		t.Fatalf("name = %q", kind.Name())
	}
	if _, err := registry.New(config.Repo{Kind: "missing"}); err == nil {
		t.Fatal("missing kind accepted")
	}
}
