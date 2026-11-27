//go:build wasip1

// Integration tests that run inside the real Fastly Compute runtime emulator
// (Viceroy) against a local KV Store. Run with `make test-integration`.
package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fastly/compute-sdk-go/cache/core"

	"github.com/ykyuki/yk.gy/internal/ratelimit"
	"github.com/ykyuki/yk.gy/internal/safebrowsing"
	"github.com/ykyuki/yk.gy/internal/shortener"
	"github.com/ykyuki/yk.gy/internal/storage"
)

func TestKVConditionalInsert(t *testing.T) {
	ctx := context.Background()
	for _, ttl := range []time.Duration{0, time.Minute} {
		s := &storage.KVStore{Name: "links", CacheTTL: ttl}
		if err := s.Ping(); err != nil {
			t.Fatal(err)
		}
		l, err := s.Get(ctx, "Existed")
		if err != nil || l.URL != "https://example.com/" {
			t.Fatalf("ttl=%v get existing: %+v %v", ttl, l, err)
		}
		// Conditional write must refuse to overwrite.
		err = s.Create(ctx, "Existed", storage.Link{URL: "https://attacker.example.net/"})
		if !errors.Is(err, storage.ErrExists) {
			t.Fatalf("ttl=%v expected ErrExists, got %v", ttl, err)
		}
		l, _ = s.Get(ctx, "Existed")
		if l.URL != "https://example.com/" {
			t.Fatal("existing entry was overwritten")
		}
		if _, err := s.Get(ctx, "Missing"); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("ttl=%v expected ErrNotFound, got %v", ttl, err)
		}
	}
}

func TestShortenEndToEndOnKV(t *testing.T) {
	ctx := context.Background()
	kv := &storage.KVStore{Name: "links"}
	svc := &shortener.Service{Store: kv, Checker: safebrowsing.Mock{}}
	res, err := svc.Shorten(ctx, "https://www.fastly.com/")
	if err != nil {
		t.Fatal(err)
	}
	l, err := svc.Resolve(ctx, res.Code)
	if err != nil || l.URL != "https://www.fastly.com/" {
		t.Fatalf("%+v %v", l, err)
	}
	if err := kv.Create(ctx, res.Code, storage.Link{URL: "https://other.example/"}); !errors.Is(err, storage.ErrExists) {
		t.Fatalf("second insert of a fresh code must fail, got %v", err)
	}
}

func TestMissingStoreFailsClosed(t *testing.T) {
	kv := &storage.KVStore{Name: "not_linked"}
	if err := kv.Create(context.Background(), "Abcdefg", storage.Link{URL: "https://a.example/"}); !errors.Is(err, storage.ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable, got %v", err)
	}
}

// TestCacheRateLimiter runs the real limiter against Viceroy's Core Cache.
func TestCacheRateLimiter(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 10, 0, time.UTC)
	w := &ratelimit.Window{
		Store:  ratelimit.CoreCacheStore{},
		Policy: ratelimit.Policy{PerMinute: 5, Penalty: time.Minute},
		Prefix: "itest:" + time.Now().Format("150405.000000") + ":",
		Now:    func() time.Time { return now },
	}
	for i := 0; i < 5; i++ {
		ok, err := w.Allow("203.0.113.1")
		if errors.Is(err, core.ErrUnsupported) {
			// Viceroy cannot read Core Cache entries from the Go SDK; the
			// limiter is verified on Fastly by the dev smoke test instead.
			t.Skipf("Core Cache lookups unsupported in this Viceroy: %v", err)
		}
		if err != nil || !ok {
			t.Fatalf("request %d: ok=%v err=%v", i, ok, err)
		}
	}
	if ok, err := w.Allow("203.0.113.1"); err != nil || ok {
		t.Fatalf("6th request must be limited: ok=%v err=%v", ok, err)
	}
	if ok, err := w.Allow("203.0.113.2"); err != nil || !ok {
		t.Fatalf("other client must pass: ok=%v err=%v", ok, err)
	}
	// Still blocked by the penalty box even though the logical minute moved on.
	now = now.Add(2 * time.Minute)
	if ok, _ := w.Allow("203.0.113.1"); ok {
		t.Fatal("penalty box must still block (real cache TTL not expired)")
	}
	// Slots are insert-only: a second claim of the same key must lose.
	st := ratelimit.CoreCacheStore{}
	if ok, err := st.Claim(w.Prefix+"x", time.Minute); err != nil || !ok {
		t.Fatalf("first claim: ok=%v err=%v", ok, err)
	}
	if ok, err := st.Claim(w.Prefix+"x", time.Minute); err != nil || ok {
		t.Fatalf("second claim must lose: ok=%v err=%v", ok, err)
	}
}
