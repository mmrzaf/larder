package cache

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/mmrzaf/larder/internal/index"
	"github.com/mmrzaf/larder/internal/kinds"
)

type flightResult struct {
	err        error
	cache      string
	upstream   string
	status     int
	stale      bool
	verified   bool
	goneStatus int
	body       []byte
	object     index.Object
	written    bool
}

type flight struct {
	done   chan struct{}
	result flightResult
}

type flightGroup struct {
	mu sync.Mutex
	m  map[string]*flight
}

func newFlightGroup() *flightGroup {
	return &flightGroup{m: make(map[string]*flight)}
}

func flightKey(repo, key string) string { return repo + "\x00" + key }

var errLeaderFailed = fmt.Errorf("the request that was fetching this object failed")

func (g *flightGroup) begin(repo, key string) (*flight, bool) {
	k := flightKey(repo, key)
	g.mu.Lock()
	defer g.mu.Unlock()
	if existing := g.m[k]; existing != nil {
		return existing, false
	}
	f := &flight{done: make(chan struct{})}
	g.m[k] = f
	return f, true
}

// finish publishes the result to every waiter. Only the first call for a
// flight has an effect, so a deferred call can release waiters if the leader
// panics before it reports.
func (g *flightGroup) finish(repo, key string, f *flight, result flightResult) {
	g.mu.Lock()
	select {
	case <-f.done:
		g.mu.Unlock()
		return
	default:
	}
	f.result = result
	delete(g.m, flightKey(repo, key))
	close(f.done)
	g.mu.Unlock()
}

func (g *flightGroup) wait(ctx context.Context, f *flight, timeout time.Duration, clock Clock) (flightResult, error) {
	if timeout <= 0 {
		timeout = 15 * time.Minute
	}
	select {
	case <-f.done:
		return f.result, nil
	case <-ctx.Done():
		return flightResult{}, ctx.Err()
	case <-clock.After(timeout):
		return flightResult{}, fmt.Errorf("wait timeout")
	}
}

func (e *Engine) waitAndServe(ctx context.Context, rt *repoRuntime, plan kinds.Plan, key string, f *flight, w http.ResponseWriter, r *http.Request, meta *responseMeta) {
	result, err := e.flights.wait(ctx, f, e.cfg.Timeouts.WaitTimeout.Duration(), e.clock)
	if err != nil {
		writeError(w, http.StatusGatewayTimeout, "MISS", "waiting for concurrent fetch timed out")
		return
	}
	e.applyFlight(rt, plan, key, result, w, r, meta, false)
}
