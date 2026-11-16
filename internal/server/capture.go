package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/mmrzaf/larder/internal/index"
)

const (
	captureLifetime = time.Hour
	captureMaxKeys  = 200000
)

type captureSession struct {
	token   string
	set     string
	from    []*net.IPNet
	created time.Time
	expires time.Time
	keys    map[string]index.ObjectRef
}

type captureStore struct {
	mu       sync.Mutex
	clock    interface{ Now() time.Time }
	sessions map[string]*captureSession
}

func newCaptureStore(clock interface{ Now() time.Time }) *captureStore {
	if clock == nil {
		clock = realClock{}
	}
	return &captureStore{clock: clock, sessions: make(map[string]*captureSession)}
}

func (c *captureStore) start(set string, from []string) (string, error) {
	if set == "" {
		return "", fmt.Errorf("capture: pin set is required")
	}
	networks, err := parseCaptureCIDRs(from)
	if err != nil {
		return "", err
	}
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("capture: generate token: %w", err)
	}
	now := c.clock.Now()
	session := &captureSession{
		token: hex.EncodeToString(raw[:]), set: set, from: networks,
		created: now, expires: now.Add(captureLifetime),
		keys: make(map[string]index.ObjectRef),
	}
	c.mu.Lock()
	c.cleanupLocked(now)
	if len(c.sessions) >= 64 {
		c.mu.Unlock()
		return "", fmt.Errorf("capture: too many active sessions")
	}
	c.sessions[session.token] = session
	c.mu.Unlock()
	return session.token, nil
}

func parseCaptureCIDRs(values []string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if !strings.Contains(value, "/") {
			ip := net.ParseIP(value)
			if ip == nil {
				return nil, fmt.Errorf("capture: invalid address %q", value)
			}
			if ip.To4() != nil {
				value += "/32"
			} else {
				value += "/128"
			}
		}
		_, network, err := net.ParseCIDR(value)
		if err != nil {
			return nil, fmt.Errorf("capture: invalid CIDR %q", value)
		}
		out = append(out, network)
	}
	return out, nil
}

func (c *captureStore) collect(clientIP, token, keyHeader string, status int) {
	if keyHeader == "" || status != 200 && status != 206 && status != 304 {
		return
	}
	repo, key, ok := strings.Cut(keyHeader, ":")
	if !ok || repo == "" || key == "" {
		return
	}
	now := c.clock.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cleanupLocked(now)
	for _, session := range c.sessions {
		if token != "" && session.token == token || token == "" && session.matches(clientIP) {
			mapKey := repo + "\x00" + key
			if _, exists := session.keys[mapKey]; exists {
				continue
			}
			if len(session.keys) >= captureMaxKeys {
				continue
			}
			session.keys[mapKey] = index.ObjectRef{Repo: repo, Key: key}
		}
	}
}

func (s *captureSession) matches(clientIP string) bool {
	if len(s.from) == 0 {
		return false
	}
	ip := net.ParseIP(clientIP)
	if ip == nil {
		return false
	}
	for _, network := range s.from {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

func (c *captureStore) stop(token string, pin bool, pinner func(context.Context, string, index.ObjectRef) error) (string, int, int, error) {
	now := c.clock.Now()
	c.mu.Lock()
	c.cleanupLocked(now)
	session := c.sessions[token]
	if session == nil {
		c.mu.Unlock()
		return "", 0, 0, fmt.Errorf("capture: session not found")
	}
	delete(c.sessions, token)
	keys := make([]index.ObjectRef, 0, len(session.keys))
	for _, key := range session.keys {
		keys = append(keys, key)
	}
	c.mu.Unlock()
	pinned := 0
	if pin && pinner != nil {
		for _, key := range keys {
			if err := pinner(context.Background(), session.set, key); err != nil {
				continue
			}
			pinned++
		}
	}
	return session.set, len(keys), pinned, nil
}

func (c *captureStore) cleanupLocked(now time.Time) {
	for token, session := range c.sessions {
		if !now.Before(session.expires) {
			delete(c.sessions, token)
		}
	}
}
