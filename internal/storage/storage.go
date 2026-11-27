// Package storage persists short links. Production code uses the Fastly KV
// Store (see kv.go); tests use MemoryStore.
package storage

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

var (
	// ErrNotFound is returned when a code is not registered.
	ErrNotFound = errors.New("storage: not found")
	// ErrExists is returned by Create when the code is already taken.
	ErrExists = errors.New("storage: already exists")
	// ErrUnavailable wraps backend failures.
	ErrUnavailable = errors.New("storage: unavailable")
)

// Link is the value stored under a short code (stored without a TTL).
type Link struct {
	URL       string    `json:"url"`
	CreatedAt time.Time `json:"created_at"`
	// Disabled links answer 410 Gone (manual takedown by an operator).
	Disabled bool `json:"disabled,omitempty"`
}

// Encode serializes a link for storage.
func (l Link) Encode() ([]byte, error) { return json.Marshal(l) }

// Decode parses a stored link.
func Decode(b []byte) (*Link, error) {
	var l Link
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, err
	}
	if l.URL == "" {
		return nil, errors.New("storage: empty url in record")
	}
	return &l, nil
}

// Store is the persistence abstraction used by the shortener.
type Store interface {
	// Get returns the link for code or ErrNotFound.
	Get(ctx context.Context, code string) (*Link, error)
	// Create stores link under code only if code is unused (conditional
	// write). It returns ErrExists when the code is already taken and never
	// overwrites an existing entry.
	Create(ctx context.Context, code string, link Link) error
	// LookupURL returns the code already issued for a normalized URL, or
	// ErrNotFound.
	LookupURL(ctx context.Context, normalizedURL string) (string, error)
	// IndexURL records code as the code for normalizedURL. It never
	// overwrites an existing index entry (the first code wins).
	IndexURL(ctx context.Context, normalizedURL, code string) error
}

// URLIndexKey is the storage key of the URL -> code index entry. Short codes
// are Base62 and never contain ':', so the keys cannot collide.
func URLIndexKey(normalizedURL string) string {
	sum := sha256.Sum256([]byte(normalizedURL))
	return "u:" + base64.RawURLEncoding.EncodeToString(sum[:])
}

// MemoryStore is an in-memory Store for tests.
type MemoryStore struct {
	mu    sync.Mutex
	links map[string]Link
	index map[string]string
	// CreateErr, if set, is returned by Create (fault injection).
	CreateErr error
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{links: map[string]Link{}, index: map[string]string{}}
}

// LookupURL implements Store.
func (m *MemoryStore) LookupURL(_ context.Context, u string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.index[URLIndexKey(u)]
	if !ok {
		return "", ErrNotFound
	}
	return c, nil
}

// IndexURL implements Store.
func (m *MemoryStore) IndexURL(_ context.Context, u, code string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := URLIndexKey(u)
	if _, ok := m.index[k]; ok {
		return ErrExists
	}
	m.index[k] = code
	return nil
}

// Get implements Store.
func (m *MemoryStore) Get(_ context.Context, code string) (*Link, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.links[code]
	if !ok {
		return nil, ErrNotFound
	}
	return &l, nil
}

// Create implements Store.
func (m *MemoryStore) Create(_ context.Context, code string, link Link) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.CreateErr != nil {
		return m.CreateErr
	}
	if _, ok := m.links[code]; ok {
		return ErrExists
	}
	m.links[code] = link
	return nil
}

// Len returns the number of stored links.
func (m *MemoryStore) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.links)
}
