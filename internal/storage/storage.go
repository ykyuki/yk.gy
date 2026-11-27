// Package storage persists short links. Production code uses the Fastly KV
// Store (see kv.go); tests use MemoryStore.
package storage

import (
	"context"
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

// Link is the value stored under a short code. Links never expire.
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
}

// MemoryStore is an in-memory Store for tests.
type MemoryStore struct {
	mu    sync.Mutex
	links map[string]Link
	// CreateErr, if set, is returned by Create (fault injection).
	CreateErr error
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore { return &MemoryStore{links: map[string]Link{}} }

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
