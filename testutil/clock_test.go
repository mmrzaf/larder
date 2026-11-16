package testutil

import (
	"os"
	"testing"
	"time"
)

func TestFakeClock(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	clock := NewFakeClock(start)
	if !clock.Now().Equal(start) {
		t.Fatalf("Now = %v", clock.Now())
	}

	first := clock.After(time.Second)
	second := clock.After(2 * time.Second)
	zero := clock.After(0)
	select {
	case got := <-zero:
		if !got.Equal(start) {
			t.Fatalf("zero timer = %v", got)
		}
	default:
		t.Fatal("zero timer did not fire")
	}

	clock.Advance(time.Second)
	select {
	case got := <-first:
		if !got.Equal(start.Add(time.Second)) {
			t.Fatalf("first timer = %v", got)
		}
	default:
		t.Fatal("first timer did not fire")
	}
	select {
	case <-second:
		t.Fatal("second timer fired early")
	default:
	}

	clock.Set(start.Add(5 * time.Second))
	select {
	case got := <-second:
		if !got.Equal(start.Add(5 * time.Second)) {
			t.Fatalf("second timer = %v", got)
		}
	default:
		t.Fatal("second timer did not fire after Set")
	}
}

func TestFakeClockNegativeAdvancePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("negative advance did not panic")
		}
	}()
	NewFakeClock(time.Now()).Advance(-time.Second)
}

func TestRealClock(t *testing.T) {
	clock := RealClock{}
	if clock.Now().IsZero() {
		t.Fatal("real clock returned zero time")
	}
	select {
	case <-clock.After(0):
	case <-time.After(time.Second):
		t.Fatal("real clock timer did not fire")
	}
}

func TestTempDataDir(t *testing.T) {
	dir := TempDataDir(t)
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		t.Fatal("temp data dir is not a directory")
	}
	if info.Mode().Perm() != 0o750 {
		t.Fatalf("mode = %o, want 750", info.Mode().Perm())
	}
}
