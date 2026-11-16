package cache

import (
	"sync"
	"time"
)

type negativeEntry struct {
	expires time.Time
	status  int
	body    []byte
}

type negativeCache struct {
	mu      sync.Mutex
	clock   Clock
	max     int
	entries map[string]negativeEntry
	order   []string
}

func newNegativeCache(clock Clock, max int) *negativeCache {
	if max <= 0 {
		max = 10000
	}
	return &negativeCache{
		clock:   clock,
		max:     max,
		entries: make(map[string]negativeEntry),
	}
}

func (n *negativeCache) hit(key string) (negativeEntry, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	entry, ok := n.entries[key]
	if !ok {
		return negativeEntry{}, false
	}
	if !n.clock.Now().Before(entry.expires) {
		delete(n.entries, key)
		n.removeOrder(key)
		return negativeEntry{}, false
	}
	return entry, true
}

func (n *negativeCache) put(key string, ttl time.Duration, status int, body []byte) {
	if ttl <= 0 {
		return
	}
	entry := negativeEntry{expires: n.clock.Now().Add(ttl), status: status, body: append([]byte(nil), body...)}
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, exists := n.entries[key]; exists {
		n.entries[key] = entry
		return
	}
	for len(n.entries) >= n.max && len(n.order) > 0 {
		oldest := n.order[0]
		n.order = n.order[1:]
		delete(n.entries, oldest)
	}
	n.entries[key] = entry
	n.order = append(n.order, key)
}

func (n *negativeCache) removeOrder(key string) {
	for i, item := range n.order {
		if item == key {
			n.order = append(n.order[:i], n.order[i+1:]...)
			return
		}
	}
}

func (n *negativeCache) len() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.entries)
}
