package storage

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestEncodeDecode(t *testing.T) {
	l := Link{URL: "https://example.com/", CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	b, err := l.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(b)
	if err != nil || got.URL != l.URL || !got.CreatedAt.Equal(l.CreatedAt) || got.Disabled {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := Decode([]byte(`{"created_at":"2026-01-01T00:00:00Z"}`)); err == nil {
		t.Fatal("empty url must be rejected")
	}
	if _, err := Decode([]byte(`not json`)); err == nil {
		t.Fatal("garbage must be rejected")
	}
}

func TestMemoryStoreConditionalCreate(t *testing.T) {
	m := NewMemoryStore()
	ctx := context.Background()
	if err := m.Create(ctx, "abc", Link{URL: "https://a/"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Create(ctx, "abc", Link{URL: "https://b/"}); !errors.Is(err, ErrExists) {
		t.Fatalf("expected ErrExists, got %v", err)
	}
	l, _ := m.Get(ctx, "abc")
	if l.URL != "https://a/" {
		t.Fatal("overwritten")
	}
	if _, err := m.Get(ctx, "zzz"); !errors.Is(err, ErrNotFound) {
		t.Fatal("expected ErrNotFound")
	}
}
