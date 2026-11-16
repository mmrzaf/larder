package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mmrzaf/larder/testutil"
)

func TestJobManagerLimitsAndRetention(t *testing.T) {
	clock := testutil.NewFakeClock(time.Unix(1_700_000_000, 0))
	manager := NewJobManager(clock, time.Hour)
	started := make(chan struct{})
	release := make(chan struct{})
	job, err := manager.Start("gc", func(_ context.Context, job *Job) error {
		job.AddProgress(2, 10)
		close(started)
		<-release
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if _, err := manager.Start("gc", func(context.Context, *Job) error { return nil }); !errors.Is(err, ErrJobConflict) {
		t.Fatalf("conflict error = %v", err)
	}
	if _, err := manager.Start("evict", func(context.Context, *Job) error { return nil }); err != nil {
		t.Fatalf("different kind rejected: %v", err)
	}
	close(release)
	<-job.Done()
	view, err := manager.Get(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.State != "done" || view.Progress.Items != 2 || view.Progress.Bytes != 10 {
		t.Fatalf("view = %#v", view)
	}
	clock.Advance(2 * time.Hour)
	if _, err := manager.Get(job.ID); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("retention error = %v", err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestJobManagerFailureAndClose(t *testing.T) {
	clock := testutil.NewFakeClock(time.Unix(1_700_000_000, 0))
	manager := NewJobManager(clock, time.Hour)
	failed, err := manager.Start("verify", func(context.Context, *Job) error { return errors.New("boom") })
	if err != nil {
		t.Fatal(err)
	}
	<-failed.Done()
	view, err := manager.Get(failed.ID)
	if err != nil || view.State != "failed" || view.Error != "boom" {
		t.Fatalf("failed view = %#v, %v", view, err)
	}
	started := make(chan struct{})
	blocked, err := manager.Start("gc", func(ctx context.Context, _ *Job) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	<-blocked.Done()
	if _, err := manager.Start("gc", func(context.Context, *Job) error { return nil }); err == nil {
		t.Fatal("started a job after close")
	}
}
