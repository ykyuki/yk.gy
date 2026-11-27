package ratelimit

import (
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"
)

// SlotStore is an insert-only key store with per-entry TTL. On Fastly it is
// backed by the POP-local Core Cache (cache.go); tests use MemoryStore.
// Entries are never modified after creation (see CoreCacheStore for why).
type SlotStore interface {
	// Exists reports whether key exists and has not expired.
	Exists(key string) (bool, error)
	// Claim creates key for ttl if it does not exist and reports whether
	// this caller created it.
	Claim(key string, ttl time.Duration) (bool, error)
}

// slotTTL keeps a minute's slots alive while they serve as the "previous"
// minute of the sliding window.
const slotTTL = 2*time.Minute + 5*time.Second

// maxClaimAttempts bounds how far a request moves on when concurrent
// requests take the slot it tried to claim.
const maxClaimAttempts = 4

// Window is a sliding-window limiter with a penalty box built on a
// SlotStore. It needs no paid product: on Fastly it only uses the cache
// included in Compute.
//
// Requests of a client in minute m are recorded as slots rl:<client>:<m>:1,
// :2, ... (created, never modified). The count of a minute is the number of
// filled slots, found by binary search (slots are filled in order).
//
//   - if pb:<client> exists -> blocked (penalty box)
//   - estimate = count(m) + count(m-1) * (1 - elapsed/60s)
//   - if estimate >= PerMinute -> claim pb:<client> for Penalty, blocked
//   - else claim slot count(m)+1 -> allowed
type Window struct {
	Store  SlotStore
	Policy Policy
	// Prefix namespaces keys (e.g. per purpose / version).
	Prefix string
	Now    func() time.Time
}

// Allow implements Limiter.
func (w *Window) Allow(key string) (bool, error) {
	if w.Store == nil {
		return false, fmt.Errorf("%w: no store", ErrUnavailable)
	}
	limit := int(w.Policy.PerMinute)
	if limit <= 0 {
		return false, fmt.Errorf("%w: limit is zero", ErrUnavailable)
	}
	now := time.Now()
	if w.Now != nil {
		now = w.Now()
	}
	pbKey := w.Prefix + "pb:" + key

	boxed, err := w.Store.Exists(pbKey)
	if err != nil {
		return false, fmt.Errorf("%w: penalty lookup: %w", ErrUnavailable, err)
	}
	if boxed {
		return false, nil
	}

	minute := now.Unix() / 60
	cur, err := w.count(key, minute, limit)
	if err != nil {
		return false, err
	}
	prev, err := w.count(key, minute-1, limit)
	if err != nil {
		return false, err
	}
	elapsed := float64(now.Unix()%60) + float64(now.Nanosecond())/1e9
	estimate := float64(cur) + float64(prev)*(1-elapsed/60)

	if estimate >= float64(limit) {
		if _, err := w.Store.Claim(pbKey, w.Policy.Penalty); err != nil {
			return false, fmt.Errorf("%w: penalty set: %w", ErrUnavailable, err)
		}
		return false, nil
	}
	for n := cur + 1; n <= limit && n <= cur+maxClaimAttempts; n++ {
		ok, err := w.Store.Claim(w.slotKey(key, minute, n), slotTTL)
		if err != nil {
			return false, fmt.Errorf("%w: count set: %w", ErrUnavailable, err)
		}
		if ok {
			return true, nil
		}
		// Someone took slot n concurrently: that request counted, try the next.
	}
	// All nearby slots were taken concurrently: the minute is (nearly) full.
	return false, nil
}

func (w *Window) slotKey(key string, minute int64, n int) string {
	return w.Prefix + "rl:" + key + ":" + strconv.FormatInt(minute, 10) + ":" + strconv.Itoa(n)
}

// count returns how many slots of minute are filled (0..limit), assuming
// they are filled in order, with O(log limit) lookups.
func (w *Window) count(key string, minute int64, limit int) (int, error) {
	lo, hi := 0, limit // invariant: slots 1..lo filled; answer in [lo, hi]
	for lo < hi {
		mid := (lo + hi + 1) / 2
		ok, err := w.Store.Exists(w.slotKey(key, minute, mid))
		if err != nil {
			return 0, fmt.Errorf("%w: count lookup: %w", ErrUnavailable, err)
		}
		if ok {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo, nil
}

// MemoryStore is an in-memory SlotStore for tests.
type MemoryStore struct {
	Now func() time.Time
	// FailWith, if set, is returned by every operation (fault injection).
	FailWith error

	mu      sync.Mutex
	entries map[string]time.Time // key -> expiry
	Lookups int                  // number of Exists calls (cost checks)
}

// Exists implements SlotStore.
func (m *MemoryStore) Exists(key string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Lookups++
	if m.FailWith != nil {
		return false, m.FailWith
	}
	exp, ok := m.entries[key]
	return ok && m.now().Before(exp), nil
}

// Claim implements SlotStore.
func (m *MemoryStore) Claim(key string, ttl time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailWith != nil {
		return false, m.FailWith
	}
	if ttl <= 0 {
		return false, errors.New("ttl must be positive")
	}
	if m.entries == nil {
		m.entries = map[string]time.Time{}
	}
	if exp, ok := m.entries[key]; ok && m.now().Before(exp) {
		return false, nil
	}
	m.entries[key] = m.now().Add(ttl)
	return true, nil
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
