package upstream

import (
	"sync"
	"time"

	"github.com/mmrzaf/larder/internal/config"
)

// Clock is the time source used by circuit breakers and tests.
type Clock interface {
	Now() time.Time
}

type breaker struct {
	mu        sync.Mutex
	threshold int
	base      time.Duration
	max       time.Duration
	clock     Clock

	failures int
	open     bool
	probing  bool
	period   time.Duration
	until    time.Time
}

func newBreaker(cfg config.Breaker, clock Clock) *breaker {
	if clock == nil {
		clock = realClock{}
	}
	return &breaker{
		threshold: cfg.FailureThreshold,
		base:      cfg.OpenFor.Duration(),
		max:       cfg.OpenMax.Duration(),
		clock:     clock,
	}
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// allow reports whether a request may run and whether it is the half-open probe.
func (b *breaker) allow() (allowed bool, probe bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.clock.Now()
	if !b.open {
		return true, false
	}
	if now.Before(b.until) || b.probing {
		return false, false
	}
	b.probing = true
	return true, true
}

// success closes the circuit.
func (b *breaker) success() (closed bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	wasOpen := b.open
	b.open = false
	b.probing = false
	b.failures = 0
	b.period = b.base
	return wasOpen
}

// failure records one failed attempt and returns whether the circuit opened.
func (b *breaker) failure() (opened bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.clock.Now()
	if b.open {
		b.period = b.nextPeriod()
		b.until = now.Add(b.period)
		b.probing = false
		return true
	}
	b.failures++
	if b.failures < b.threshold {
		return false
	}
	b.open = true
	b.probing = false
	b.period = b.base
	b.until = now.Add(b.period)
	return true
}

// openFor opens the circuit for the stated duration, capped at open_max.
func (b *breaker) openFor(d time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if d <= 0 {
		return
	}
	if d > b.max {
		d = b.max
	}
	b.open = true
	b.probing = false
	b.failures = b.threshold
	b.period = d
	b.until = b.clock.Now().Add(d)
}

func (b *breaker) cancelProbe() {
	b.mu.Lock()
	b.probing = false
	b.mu.Unlock()
}

func (b *breaker) isOpen() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.open
}

func (b *breaker) readyLocked(now time.Time) bool {
	return b.open && !b.probing && !now.Before(b.until)
}

// ready reports whether the open period has elapsed and a probe may run.
func (b *breaker) ready() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.readyLocked(b.clock.Now())
}

func (b *breaker) nextPeriod() time.Duration {
	if b.period <= 0 {
		return b.base
	}
	next := b.period * 2
	if next > b.max {
		next = b.max
	}
	return next
}

func (b *breaker) state() (open bool, until time.Time, period time.Duration, failures int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.open, b.until, b.period, b.failures
}
