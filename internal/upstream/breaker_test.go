package upstream

import (
	"testing"
	"time"

	"github.com/mmrzaf/larder/internal/config"
	"github.com/mmrzaf/larder/testutil"
)

func TestBreakerTransitions(t *testing.T) {
	clock := testutil.NewFakeClock(time.Unix(1_700_000_000, 0))
	b := newBreaker(config.Breaker{
		FailureThreshold: 3,
		OpenFor:          config.Duration(30 * time.Second),
		OpenMax:          config.Duration(5 * time.Minute),
	}, clock)

	if allowed, probe := b.allow(); !allowed || probe {
		t.Fatal("closed breaker did not allow a normal request")
	}
	b.failure()
	b.failure()
	if open, _, _, failures := b.state(); open || failures != 2 {
		t.Fatalf("state after two failures = open %v, failures %d", open, failures)
	}
	if opened := b.failure(); !opened {
		t.Fatal("third failure did not open the circuit")
	}
	if allowed, _ := b.allow(); allowed {
		t.Fatal("open circuit allowed a request before the open period")
	}

	clock.Advance(30 * time.Second)
	if allowed, probe := b.allow(); !allowed || !probe {
		t.Fatal("half-open circuit did not allow one probe")
	}
	if allowed, _ := b.allow(); allowed {
		t.Fatal("half-open circuit allowed a second probe")
	}
	if closed := b.success(); !closed {
		t.Fatal("success did not report a closed circuit")
	}
	if allowed, probe := b.allow(); !allowed || probe {
		t.Fatal("closed circuit did not allow after success")
	}

	b.failure()
	b.failure()
	b.failure()
	clock.Advance(30 * time.Second)
	b.allow()
	b.failure()
	open, _, period, _ := b.state()
	if !open || period != 60*time.Second {
		t.Fatalf("probe failure state = open %v, period %v", open, period)
	}
	clock.Advance(60 * time.Second)
	b.allow()
	b.failure()
	_, _, period, _ = b.state()
	if period != 120*time.Second {
		t.Fatalf("doubled period = %v", period)
	}
	clock.Advance(120 * time.Second)
	b.allow()
	b.failure()
	_, _, period, _ = b.state()
	if period != 240*time.Second {
		t.Fatalf("doubled period = %v", period)
	}
	clock.Advance(240 * time.Second)
	b.allow()
	b.failure()
	_, _, period, _ = b.state()
	if period != 5*time.Minute {
		t.Fatalf("capped period = %v", period)
	}
}

func TestBreakerOpenFor(t *testing.T) {
	clock := testutil.NewFakeClock(time.Unix(1_700_000_000, 0))
	b := newBreaker(config.Breaker{
		FailureThreshold: 3,
		OpenFor:          config.Duration(30 * time.Second),
		OpenMax:          config.Duration(5 * time.Minute),
	}, clock)
	b.openFor(10 * time.Second)
	if allowed, _ := b.allow(); allowed {
		t.Fatal("openFor did not open the circuit")
	}
	clock.Advance(10 * time.Second)
	if allowed, probe := b.allow(); !allowed || !probe {
		t.Fatal("openFor did not allow a probe after the stated time")
	}
}
