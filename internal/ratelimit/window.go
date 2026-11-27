package ratelimit

import (
	"errors"
	"fmt"
	"strconv"
	"time"
)

// CounterStore is a tiny key/value store with per-entry TTL. On Fastly it is
// backed by the POP-local Core Cache (see cache.go); tests use MemoryStore.
// Writes are last-writer-wins: concurrent requests may under-count slightly,
// which is acceptable for abuse prevention.
type CounterStore interface {
	// Get returns the stored value, or ok=false if absent or expired.
	Get(key string) (value string, ok bool, err error)
	// Set stores value under key for ttl, overwriting any previous value.
	Set(key, value string, ttl time.Duration) error
}

// Window is a sliding-window limiter with a penalty box, built on a
// CounterStore. It needs no paid product: on Fastly it only uses the cache
// that is part of Compute.
//
// Algorithm (per client key):
//   - if pb:<key> exists -> blocked (penalty box)
//   - estimate = count(current minute) + count(previous minute) * (1 - elapsed/60s)
//   - if estimate >= PerMinute -> put the client in the penalty box, blocked
//   - else count(current minute)++ -> allowed
type Window struct {
	Store  CounterStore
	Policy Policy
	// Prefix namespaces keys (e.g. per environment / purpose).
	Prefix string
	Now    func() time.Time
}

// Allow implements Limiter.
func (w *Window) Allow(key string) (bool, error) {
	if w.Store == nil {
		return false, fmt.Errorf("%w: no store", ErrUnavailable)
	}
	if w.Policy.PerMinute == 0 {
		return false, fmt.Errorf("%w: limit is zero", ErrUnavailable)
	}
	now := time.Now()
	if w.Now != nil {
		now = w.Now()
	}
	pbKey := w.Prefix + "pb:" + key

	if _, boxed, err := w.Store.Get(pbKey); err != nil {
		return false, fmt.Errorf("%w: penalty lookup: %w", ErrUnavailable, err)
	} else if boxed {
		return false, nil
	}

	minute := now.Unix() / 60
	curKey := w.Prefix + "rl:" + key + ":" + strconv.FormatInt(minute, 10)
	prevKey := w.Prefix + "rl:" + key + ":" + strconv.FormatInt(minute-1, 10)

	cur, err := w.count(curKey)
	if err != nil {
		return false, err
	}
	prev, err := w.count(prevKey)
	if err != nil {
		return false, err
	}
	elapsed := float64(now.Unix()%60) + float64(now.Nanosecond())/1e9
	estimate := float64(cur) + float64(prev)*(1-elapsed/60)

	if estimate >= float64(w.Policy.PerMinute) {
		if err := w.Store.Set(pbKey, "1", w.Policy.Penalty); err != nil {
			return false, fmt.Errorf("%w: penalty set: %w", ErrUnavailable, err)
		}
		return false, nil
	}
	// The window key must outlive the next minute so it can serve as "prev".
	if err := w.Store.Set(curKey, strconv.FormatUint(cur+1, 10), 2*time.Minute+5*time.Second); err != nil {
		return false, fmt.Errorf("%w: count set: %w", ErrUnavailable, err)
	}
	return true, nil
}

func (w *Window) count(key string) (uint64, error) {
	v, ok, err := w.Store.Get(key)
	if err != nil {
		return 0, fmt.Errorf("%w: count lookup: %w", ErrUnavailable, err)
	}
	if !ok {
		return 0, nil
	}
	n, err := strconv.ParseUint(v, 10, 32)
	if err != nil {
		// A corrupt counter must not disable limiting: treat as full.
		return uint64(w.Policy.PerMinute), nil
	}
	return n, nil
}

// MemoryStore is an in-memory CounterStore for tests.
type MemoryStore struct {
	Now     func() time.Time
	entries map[string]memEntry
	// FailWith, if set, is returned by every operation (fault injection).
	FailWith error
}

type memEntry struct {
	v   string
	exp time.Time
}

// Get implements CounterStore.
func (m *MemoryStore) Get(key string) (string, bool, error) {
	if m.FailWith != nil {
		return "", false, m.FailWith
	}
	e, ok := m.entries[key]
	if !ok || !m.now().Before(e.exp) {
		return "", false, nil
	}
	return e.v, true, nil
}

// Set implements CounterStore.
func (m *MemoryStore) Set(key, value string, ttl time.Duration) error {
	if m.FailWith != nil {
		return m.FailWith
	}
	if ttl <= 0 {
		return errors.New("ttl must be positive")
	}
	if m.entries == nil {
		m.entries = map[string]memEntry{}
	}
	m.entries[key] = memEntry{v: value, exp: m.now().Add(ttl)}
	return nil
}

func (m *MemoryStore) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// LocalUnsupportedFallback wraps a Limiter for the LOCAL development server
// only: Viceroy cannot read Core Cache entries from the Go SDK
// ("operation not supported"), so there the request is allowed and a warning
// is logged. Any other error is returned unchanged. It must never be used on
// Fastly (cmd/shortener only installs it when FASTLY_HOSTNAME == "localhost"),
// where cache errors keep failing closed.
type LocalUnsupportedFallback struct {
	Limiter     Limiter
	Unsupported error // the cache package's "unsupported" sentinel
	Warn        func(err error)
}

// Allow implements Limiter.
func (l LocalUnsupportedFallback) Allow(key string) (bool, error) {
	ok, err := l.Limiter.Allow(key)
	if err != nil && l.Unsupported != nil && errors.Is(err, l.Unsupported) {
		if l.Warn != nil {
			l.Warn(err)
		}
		return true, nil
	}
	return ok, err
}
