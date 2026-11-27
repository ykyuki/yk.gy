package shortener

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/ykyuki/yk.gy/internal/safebrowsing"
	"github.com/ykyuki/yk.gy/internal/storage"
	"github.com/ykyuki/yk.gy/internal/validation"
)

// DefaultMaxAttempts is how many codes are tried before giving up.
// With 62^7 codes a collision is astronomically unlikely; retries exist for
// correctness, not throughput.
const DefaultMaxAttempts = 5

var (
	// ErrUnsafe means Safe Browsing flagged the URL.
	ErrUnsafe = errors.New("shortener: url flagged as unsafe")
	// ErrCheckUnavailable means Safe Browsing could not be consulted; the
	// registration is refused (fail closed).
	ErrCheckUnavailable = errors.New("shortener: safety check unavailable")
	// ErrStorage means the link could not be stored.
	ErrStorage = errors.New("shortener: storage error")
	// ErrNoFreeCode means every attempted code collided.
	ErrNoFreeCode = errors.New("shortener: could not allocate a code")
	// ErrDisabled means the URL was taken down by an operator; it cannot be
	// shortened again under a new code.
	ErrDisabled = errors.New("shortener: url was disabled by an operator")
)

// UnsafeError carries the threat types returned by Safe Browsing.
type UnsafeError struct{ Threats []string }

func (e *UnsafeError) Error() string { return fmt.Sprintf("%v: %v", ErrUnsafe, e.Threats) }
func (e *UnsafeError) Unwrap() error { return ErrUnsafe }

// Service registers and resolves short links.
type Service struct {
	Store       storage.Store
	Checker     safebrowsing.Checker
	Validation  validation.Options
	Rand        io.Reader        // nil => crypto/rand
	Now         func() time.Time // nil => time.Now
	MaxAttempts int              // 0 => DefaultMaxAttempts
}

// Result is a successful registration.
type Result struct {
	Code string
	URL  string
	// Existing is true when the URL had been shortened before and its
	// existing code was returned (no Safe Browsing call, no write).
	Existing bool
}

// Shorten validates rawURL, checks it with Safe Browsing (always, fail
// closed) and stores it under a fresh random code using a conditional write.
func (s *Service) Shorten(ctx context.Context, rawURL string) (Result, error) {
	normalized, err := validation.Normalize(rawURL, s.Validation)
	if err != nil {
		return Result{}, err
	}

	// Same URL -> same code. This keeps repeated or automated requests (AI
	// agents, GET API, MCP) from creating duplicates, writing to storage or
	// spending Safe Browsing quota. Lookup failures just skip the shortcut.
	if code, err := s.Store.LookupURL(ctx, normalized); err == nil && IsValidCode(code) {
		if l, err := s.Store.Get(ctx, code); err == nil && l.URL == normalized {
			if l.Disabled {
				return Result{}, ErrDisabled
			}
			return Result{Code: code, URL: normalized, Existing: true}, nil
		}
	}

	if s.Checker == nil {
		return Result{}, ErrCheckUnavailable
	}
	v, err := s.Checker.Check(ctx, normalized)
	if err != nil {
		return Result{}, errors.Join(ErrCheckUnavailable, err)
	}
	if !v.Safe {
		return Result{}, &UnsafeError{Threats: v.Threats}
	}

	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	attempts := s.MaxAttempts
	if attempts <= 0 {
		attempts = DefaultMaxAttempts
	}
	link := storage.Link{URL: normalized, CreatedAt: now().UTC()}
	for i := 0; i < attempts; i++ {
		code, err := NewCode(s.Rand)
		if err != nil {
			return Result{}, errors.Join(ErrStorage, err)
		}
		err = s.Store.Create(ctx, code, link)
		switch {
		case err == nil:
			// Best effort: if the index write fails the link still works; a
			// later request for the same URL may then get a second code.
			_ = s.Store.IndexURL(ctx, normalized, code)
			return Result{Code: code, URL: normalized}, nil
		case errors.Is(err, storage.ErrExists):
			continue // collision: generate another code
		default:
			return Result{}, errors.Join(ErrStorage, err)
		}
	}
	return Result{}, ErrNoFreeCode
}

// Resolve returns the link registered for code.
func (s *Service) Resolve(ctx context.Context, code string) (*storage.Link, error) {
	if !IsValidCode(code) {
		return nil, storage.ErrNotFound
	}
	return s.Store.Get(ctx, code)
}
