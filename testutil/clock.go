// Package testutil contains shared helpers for hermetic tests.
package testutil

import (
	"sync"
	"time"
)

// Clock is the time source used by components whose behavior depends on time.
type Clock interface {
	Now() time.Time
	After(time.Duration) <-chan time.Time
}

// RealClock is the wall clock.
type RealClock struct{}

// Now returns the current wall-clock time.
func (RealClock) Now() time.Time { return time.Now() }

// After reports the wall clock after d.
func (RealClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

type waiter struct {
	at time.Time
	ch chan time.Time
}

// FakeClock is a deterministic clock for tests.
type FakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []waiter
}

// NewFakeClock returns a fake clock initialized at now.
func NewFakeClock(now time.Time) *FakeClock {
	return &FakeClock{now: now}
}

// Now returns the current fake time.
func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// After returns a channel that receives the fake time after d has elapsed.
func (c *FakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- c.now
		return ch
	}
	c.waiters = append(c.waiters, waiter{at: c.now.Add(d), ch: ch})
	return ch
}

// Advance moves the fake clock forward and fires every due timer.
func (c *FakeClock) Advance(d time.Duration) {
	if d < 0 {
		panic("testutil: cannot advance a fake clock backwards")
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.now = c.now.Add(d)
	c.fireLocked()
}

// Set moves the fake clock to t and fires every due timer.
func (c *FakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.now = t
	c.fireLocked()
}

func (c *FakeClock) fireLocked() {
	remaining := c.waiters[:0]
	for _, w := range c.waiters {
		if w.at.After(c.now) {
			remaining = append(remaining, w)
			continue
		}
		w.ch <- c.now
	}
	c.waiters = remaining
}
