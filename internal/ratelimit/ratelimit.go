// Package ratelimit limits how often a client may call the URL issuing API.
package ratelimit

import (
	"errors"
	"net/netip"
	"sync"
	"time"
)

// ErrUnavailable means the limiter backend failed.
var ErrUnavailable = errors.New("ratelimit: unavailable")

// Limiter decides whether a client may proceed.
type Limiter interface {
	// Allow records one request for key and reports whether it is allowed.
	Allow(key string) (bool, error)
}

// Policy configures a fixed-count-per-minute limit with a penalty box.
type Policy struct {
	PerMinute uint32        // max requests per client per rolling minute
	Penalty   time.Duration // how long a client that exceeded the limit is blocked
}

// ClientKey derives the rate-limit key from the client address. IPv6
// clients are grouped by /64 so a single host cannot trivially rotate
// addresses inside its own prefix.
func ClientKey(remoteAddr string) string {
	a, err := netip.ParseAddr(remoteAddr)
	if err != nil {
		if ap, err2 := netip.ParseAddrPort(remoteAddr); err2 == nil {
			a = ap.Addr()
		} else {
			return "unknown"
		}
	}
	a = a.Unmap()
	if a.Is6() {
		p, _ := a.Prefix(64)
		return p.String()
	}
	return a.String()
}

// Memory is an in-process limiter used by tests.
type Memory struct {
	Policy Policy
	Now    func() time.Time

	mu      sync.Mutex
	hits    map[string][]time.Time
	blocked map[string]time.Time
}

// Allow implements Limiter.
func (m *Memory) Allow(key string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	if m.Now != nil {
		now = m.Now()
	}
	if m.hits == nil {
		m.hits = map[string][]time.Time{}
		m.blocked = map[string]time.Time{}
	}
	if until, ok := m.blocked[key]; ok && now.Before(until) {
		return false, nil
	}
	cutoff := now.Add(-time.Minute)
	h := m.hits[key][:0]
	for _, t := range m.hits[key] {
		if t.After(cutoff) {
			h = append(h, t)
		}
	}
	if uint32(len(h)) >= m.Policy.PerMinute {
		m.blocked[key] = now.Add(m.Policy.Penalty)
		m.hits[key] = h
		return false, nil
	}
	m.hits[key] = append(h, now)
	return true, nil
}
