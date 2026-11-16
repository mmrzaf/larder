package metrics

import (
	"errors"
	"testing"
)

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func TestWriteFailure(t *testing.T) {
	r := NewRegistry()
	if _, err := r.NewCounter("write_total", "x"); err != nil {
		t.Fatal(err)
	}
	if err := r.WritePrometheus(failingWriter{}); err == nil {
		t.Fatal("write failure was ignored")
	}
}
