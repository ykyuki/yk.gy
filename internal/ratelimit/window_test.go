package ratelimit

import (
	"errors"
	"testing"
	"time"
)

func newWindow(limit uint32, now *time.Time) (*Window, *MemoryStore) {
	st := &MemoryStore{Now: func() time.Time { return *now }}
	return &Window{Store: st, Policy: Policy{PerMinute: limit, Penalty: 5 * time.Minute}, Prefix: "t:", Now: func() time.Time { return *now }}, st
}

func allowN(t *testing.T, w *Window, key string, n int) int {
	t.Helper()
	passed := 0
	for i := 0; i < n; i++ {
		ok, err := w.Allow(key)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			passed++
		}
	}
	return passed
}

func TestWindowLimitAndPenalty(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	w, _ := newWindow(10, &now)
	if got := allowN(t, w, "a", 15); got != 10 {
		t.Fatalf("passed %d, want 10", got)
	}
	if got := allowN(t, w, "b", 3); got != 3 {
		t.Fatal("clients must be independent")
	}
	now = now.Add(3 * time.Minute) // window fully passed, still in penalty box
	if got := allowN(t, w, "a", 1); got != 0 {
		t.Fatal("penalty box must block")
	}
	now = now.Add(2*time.Minute + time.Second) // penalty (5m) expired
	if got := allowN(t, w, "a", 1); got != 1 {
		t.Fatal("must be allowed after the penalty")
	}
}

func TestWindowSlidingAcrossMinuteBoundary(t *testing.T) {
	// 10 requests at the end of minute 0, then right after the boundary the
	// previous minute still weighs ~100%: a fixed window would allow 10 more.
	now := time.Date(2026, 1, 1, 0, 0, 55, 0, time.UTC)
	w, _ := newWindow(10, &now)
	w.Policy.Penalty = time.Second // isolate the window maths from the penalty box
	if got := allowN(t, w, "a", 9); got != 9 {
		t.Fatal(got)
	}
	now = now.Add(6 * time.Second)             // 00:01:01 -> prev weight = 59/60
	if got := allowN(t, w, "a", 5); got != 2 { // 9*59/60 + cur < 10 -> 2 pass
		t.Fatalf("passed %d right after the boundary, want 2 (fixed window would allow 10)", got)
	}
	now = time.Date(2026, 1, 1, 0, 1, 45, 0, time.UTC) // prev weight 0.25 -> 9*0.25 + 1 = 3.25
	if got := allowN(t, w, "a", 10); got != 6 {
		t.Fatalf("passed %d at :45, want 6", got)
	}
}

func TestWindowFailsOnStoreError(t *testing.T) {
	now := time.Now()
	w, st := newWindow(10, &now)
	st.FailWith = errors.New("cache down")
	if ok, err := w.Allow("a"); ok || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if ok, err := (&Window{Policy: Policy{PerMinute: 1}}).Allow("a"); ok || !errors.Is(err, ErrUnavailable) {
		t.Fatal("nil store must be unavailable")
	}
	if ok, err := (&Window{Store: &MemoryStore{}}).Allow("a"); ok || !errors.Is(err, ErrUnavailable) {
		t.Fatal("zero limit must be unavailable")
	}
}

func TestWindowCorruptCounterCountsAsFull(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 30, 0, time.UTC)
	w, st := newWindow(10, &now)
	_ = st.Set("t:rl:a:"+itoa(now.Unix()/60), "garbage", time.Minute)
	if got := allowN(t, w, "a", 1); got != 0 {
		t.Fatal("corrupt counter must not disable limiting")
	}
}

func itoa(n int64) string {
	b := []byte{}
	if n == 0 {
		return "0"
	}
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestLocalUnsupportedFallback(t *testing.T) {
	unsupported := errors.New("cache: operation not supported")
	now := time.Now()
	w, st := newWindow(10, &now)
	var warned int
	l := LocalUnsupportedFallback{Limiter: w, Unsupported: unsupported, Warn: func(error) { warned++ }}

	st.FailWith = unsupported
	if ok, err := l.Allow("a"); !ok || err != nil || warned != 1 {
		t.Fatalf("unsupported cache locally must allow with a warning: ok=%v err=%v warned=%d", ok, err, warned)
	}
	st.FailWith = errors.New("other failure")
	if ok, err := l.Allow("a"); ok || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("other errors must still fail closed: ok=%v err=%v", ok, err)
	}
	st.FailWith = nil
	if got := allowN(t, l.Limiter.(*Window), "a", 12); got != 10 {
		t.Fatalf("limits still apply when the cache works: %d", got)
	}
}
