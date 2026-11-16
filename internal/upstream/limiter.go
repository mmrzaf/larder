package upstream

import (
	"context"
	"sync"
)

type limiterWaiter struct {
	ch      chan struct{}
	granted bool
}

type limiter struct {
	mu       sync.Mutex
	capacity int
	active   int
	queue    []*limiterWaiter
}

func newLimiter(capacity int) *limiter {
	if capacity <= 0 {
		capacity = 1
	}
	return &limiter{capacity: capacity}
}

func (l *limiter) acquire(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	l.mu.Lock()
	if l.active < l.capacity {
		l.active++
		l.mu.Unlock()
		return nil
	}
	waiter := &limiterWaiter{ch: make(chan struct{})}
	l.queue = append(l.queue, waiter)
	l.mu.Unlock()

	select {
	case <-waiter.ch:
		return nil
	case <-ctx.Done():
		l.mu.Lock()
		defer l.mu.Unlock()
		if waiter.granted {
			return nil
		}
		l.remove(waiter)
		return ctx.Err()
	}
}

func (l *limiter) release() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.queue) > 0 {
		next := l.queue[0]
		l.queue = l.queue[1:]
		next.granted = true
		close(next.ch)
		return
	}
	if l.active > 0 {
		l.active--
	}
}

func (l *limiter) remove(waiter *limiterWaiter) {
	for i, item := range l.queue {
		if item == waiter {
			l.queue = append(l.queue[:i], l.queue[i+1:]...)
			return
		}
	}
}
