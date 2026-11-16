package upstream

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"
)

func waitQueue(t *testing.T, l *limiter, want int) {
	t.Helper()
	for {
		l.mu.Lock()
		queued := len(l.queue)
		l.mu.Unlock()
		if queued == want {
			return
		}
		runtime.Gosched()
	}
}

func TestLimiterFIFOAndCancel(t *testing.T) {
	l := newLimiter(1)
	if err := l.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := make(chan struct{})
	second := make(chan struct{})
	go func() { _ = l.acquire(context.Background()); close(first) }()
	waitQueue(t, l, 1)
	go func() { _ = l.acquire(context.Background()); close(second) }()
	waitQueue(t, l, 2)
	l.release()
	select {
	case <-first:
	case <-time.After(time.Second):
		t.Fatal("first waiter did not acquire")
	}
	l.release()
	select {
	case <-second:
	case <-time.After(time.Second):
		t.Fatal("second waiter did not acquire")
	}
	l.release()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := l.acquire(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled acquire = %v", err)
	}
}
