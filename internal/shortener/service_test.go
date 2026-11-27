package shortener

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ykyuki/yk.gy/internal/safebrowsing"
	"github.com/ykyuki/yk.gy/internal/storage"
	"github.com/ykyuki/yk.gy/internal/validation"
)

type fakeChecker struct {
	v     safebrowsing.Verdict
	err   error
	calls int
}

func (f *fakeChecker) Check(context.Context, string) (safebrowsing.Verdict, error) {
	f.calls++
	return f.v, f.err
}

func newSvc(store storage.Store, c safebrowsing.Checker) *Service {
	return &Service{
		Store:      store,
		Checker:    c,
		Validation: validation.Options{ShortHosts: []string{"yk.gy"}},
		Now:        func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) },
	}
}

func TestShortenStoresLink(t *testing.T) {
	st := storage.NewMemoryStore()
	c := &fakeChecker{v: safebrowsing.Verdict{Safe: true}}
	res, err := newSvc(st, c).Shorten(context.Background(), "  HTTPS://Example.COM/a?b=c  ")
	if err != nil {
		t.Fatal(err)
	}
	if !IsValidCode(res.Code) || res.URL != "https://example.com/a?b=c" {
		t.Fatalf("unexpected result %+v", res)
	}
	l, err := st.Get(context.Background(), res.Code)
	if err != nil || l.URL != res.URL || l.CreatedAt.IsZero() {
		t.Fatalf("stored link %+v err %v", l, err)
	}
	if c.calls != 1 {
		t.Fatalf("safe browsing must be called exactly once, got %d", c.calls)
	}
}

func TestShortenRejectsUnsafe(t *testing.T) {
	st := storage.NewMemoryStore()
	_, err := newSvc(st, &fakeChecker{v: safebrowsing.Verdict{Threats: []string{"MALWARE"}}}).Shorten(context.Background(), "https://evil.example.net/")
	var ue *UnsafeError
	if !errors.As(err, &ue) || ue.Threats[0] != "MALWARE" || !errors.Is(err, ErrUnsafe) {
		t.Fatalf("expected UnsafeError, got %v", err)
	}
	if st.Len() != 0 {
		t.Fatal("unsafe url must not be stored")
	}
}

func TestShortenFailsClosed(t *testing.T) {
	st := storage.NewMemoryStore()
	_, err := newSvc(st, &fakeChecker{err: safebrowsing.ErrUnavailable}).Shorten(context.Background(), "https://example.net/")
	if !errors.Is(err, ErrCheckUnavailable) {
		t.Fatalf("expected ErrCheckUnavailable, got %v", err)
	}
	if st.Len() != 0 {
		t.Fatal("must not store when check unavailable")
	}
	// nil checker is also fail-closed
	_, err = newSvc(st, nil).Shorten(context.Background(), "https://example.net/")
	if !errors.Is(err, ErrCheckUnavailable) {
		t.Fatalf("nil checker: expected ErrCheckUnavailable, got %v", err)
	}
}

func TestShortenValidationBeforeCheck(t *testing.T) {
	c := &fakeChecker{v: safebrowsing.Verdict{Safe: true}}
	_, err := newSvc(storage.NewMemoryStore(), c).Shorten(context.Background(), "ftp://example.com/")
	if _, ok := validation.AsError(err); !ok {
		t.Fatalf("expected validation error, got %v", err)
	}
	if c.calls != 0 {
		t.Fatal("invalid URLs must not reach Safe Browsing")
	}
}

// collidingStore reports ErrExists for the first n creates.
type collidingStore struct {
	*storage.MemoryStore
	n int
}

func (c *collidingStore) Create(ctx context.Context, code string, l storage.Link) error {
	if c.n > 0 {
		c.n--
		return storage.ErrExists
	}
	return c.MemoryStore.Create(ctx, code, l)
}

func TestShortenRetriesOnCollision(t *testing.T) {
	st := &collidingStore{MemoryStore: storage.NewMemoryStore(), n: 3}
	res, err := newSvc(st, &fakeChecker{v: safebrowsing.Verdict{Safe: true}}).Shorten(context.Background(), "https://example.com/")
	if err != nil || res.Code == "" || st.Len() != 1 {
		t.Fatalf("res=%+v err=%v len=%d", res, err, st.Len())
	}
}

func TestShortenGivesUpAfterMaxAttempts(t *testing.T) {
	st := &collidingStore{MemoryStore: storage.NewMemoryStore(), n: 100}
	_, err := newSvc(st, &fakeChecker{v: safebrowsing.Verdict{Safe: true}}).Shorten(context.Background(), "https://example.com/")
	if !errors.Is(err, ErrNoFreeCode) {
		t.Fatalf("expected ErrNoFreeCode, got %v", err)
	}
}

func TestShortenNeverOverwrites(t *testing.T) {
	st := storage.NewMemoryStore()
	_ = st.Create(context.Background(), "AAAAAAA", storage.Link{URL: "https://original.example.com/"})
	// Random source that yields "AAAAAAA" first, then "BBBBBBB".
	a := bytes.Repeat([]byte{10}, 16) // 10 -> 'A'
	b := bytes.Repeat([]byte{11}, 16) // 11 -> 'B'
	svc := newSvc(st, &fakeChecker{v: safebrowsing.Verdict{Safe: true}})
	svc.Rand = bytes.NewReader(append(a, b...))
	res, err := svc.Shorten(context.Background(), "https://new.example.com/")
	if err != nil || res.Code != "BBBBBBB" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	l, _ := st.Get(context.Background(), "AAAAAAA")
	if l.URL != "https://original.example.com/" {
		t.Fatal("existing link was overwritten")
	}
}

func TestShortenStorageError(t *testing.T) {
	st := storage.NewMemoryStore()
	st.CreateErr = storage.ErrUnavailable
	_, err := newSvc(st, &fakeChecker{v: safebrowsing.Verdict{Safe: true}}).Shorten(context.Background(), "https://example.com/")
	if !errors.Is(err, ErrStorage) {
		t.Fatalf("expected ErrStorage, got %v", err)
	}
}

func TestResolve(t *testing.T) {
	st := storage.NewMemoryStore()
	_ = st.Create(context.Background(), "Abc1234", storage.Link{URL: "https://example.com/"})
	svc := newSvc(st, nil)
	if l, err := svc.Resolve(context.Background(), "Abc1234"); err != nil || l.URL != "https://example.com/" {
		t.Fatalf("l=%v err=%v", l, err)
	}
	if _, err := svc.Resolve(context.Background(), "../etc"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("invalid code should be not found, got %v", err)
	}
}
