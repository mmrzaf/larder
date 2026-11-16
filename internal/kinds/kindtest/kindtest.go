// Package kindtest provides the shared classification table runner for kinds.
package kindtest

import (
	"errors"
	"net/http"
	"reflect"
	"testing"

	"github.com/mmrzaf/larder/internal/kinds"
)

// Case is one classification expectation.
type Case struct {
	Name       string
	Method     string
	Path       string
	RawQuery   string
	Header     http.Header
	PublicBase string
	Want       kinds.Plan
	WantErr    int
}

// Table runs classification cases for one kind.
func Table(t *testing.T, kind kinds.Kind, cases []Case) {
	t.Helper()
	for _, tc := range cases {
		tc := tc
		name := tc.Name
		if name == "" {
			name = tc.Path
		}
		t.Run(name, func(t *testing.T) {
			method := tc.Method
			if method == "" {
				method = http.MethodGet
			}
			plan, err := kind.Classify(kinds.Request{
				Method:     method,
				Path:       tc.Path,
				RawQuery:   tc.RawQuery,
				Header:     tc.Header,
				PublicBase: tc.PublicBase,
			})
			if tc.WantErr != 0 {
				var status *kinds.StatusError
				if !errors.As(err, &status) || status.Code != tc.WantErr {
					t.Fatalf("error = %v, want status %d", err, tc.WantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Classify: %v", err)
			}
			comparePlan(t, plan, tc.Want)
		})
	}
}

func comparePlan(t *testing.T, got, want kinds.Plan) {
	t.Helper()
	if got.Class != want.Class {
		t.Errorf("Class = %v, want %v", got.Class, want.Class)
	}
	if got.Key != want.Key {
		t.Errorf("Key = %q, want %q", got.Key, want.Key)
	}
	if got.Variant != want.Variant {
		t.Errorf("Variant = %q, want %q", got.Variant, want.Variant)
	}
	if got.UpstreamPath != want.UpstreamPath {
		t.Errorf("UpstreamPath = %q, want %q", got.UpstreamPath, want.UpstreamPath)
	}
	if !reflect.DeepEqual(got.UpstreamHdr, want.UpstreamHdr) {
		t.Errorf("UpstreamHdr = %#v, want %#v", got.UpstreamHdr, want.UpstreamHdr)
	}
	if got.FreshTTL != want.FreshTTL {
		t.Errorf("FreshTTL = %v, want %v", got.FreshTTL, want.FreshTTL)
	}
	if got.Binary != want.Binary {
		t.Errorf("Binary = %v, want %v", got.Binary, want.Binary)
	}
	if got.Redirect != want.Redirect {
		t.Errorf("Redirect = %q, want %q", got.Redirect, want.Redirect)
	}
	if got.AllowPost != want.AllowPost {
		t.Errorf("AllowPost = %v, want %v", got.AllowPost, want.AllowPost)
	}
	if got.Timeout != want.Timeout {
		t.Errorf("Timeout = %v, want %v", got.Timeout, want.Timeout)
	}
	if got.FailureStatus != want.FailureStatus {
		t.Errorf("FailureStatus = %d, want %d", got.FailureStatus, want.FailureStatus)
	}
	if !reflect.DeepEqual(got.Local, want.Local) {
		t.Errorf("Local = %#v, want %#v", got.Local, want.Local)
	}
}
