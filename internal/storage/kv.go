package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/fastly/compute-sdk-go/cache/simple"
	"github.com/fastly/compute-sdk-go/kvstore"
)

// maxRecordSize bounds how much of a KV value is read.
const maxRecordSize = 16 << 10

// KVStore stores links in a Fastly KV Store. The store is referenced by its
// logical resource-link name (e.g. "links"); which physical store that name
// points to is decided per service (production / development) in Fastly,
// and locally by [local_server.kv_stores] in fastly.toml.
type KVStore struct {
	Name string
	// CacheTTL > 0 enables a per-POP read-through cache of successful
	// lookups using the Simple Cache API. Links are immutable, so this is
	// safe; it only delays operator takedowns by at most CacheTTL.
	CacheTTL time.Duration
}

func (s *KVStore) open() (*kvstore.Store, error) {
	st, err := kvstore.Open(s.Name)
	if err != nil {
		return nil, fmt.Errorf("%w: open %q: %v", ErrUnavailable, s.Name, err)
	}
	return st, nil
}

// Ping checks that the store is linked to this service.
func (s *KVStore) Ping() error {
	_, err := s.open()
	return err
}

// Get implements Store.
func (s *KVStore) Get(ctx context.Context, code string) (*Link, error) {
	var b []byte
	var err error
	if s.CacheTTL > 0 {
		b, err = s.cachedRead(code)
	} else {
		b, err = s.read(code)
	}
	if err != nil {
		return nil, err
	}
	return Decode(b)
}

// cachedRead reads key through the per-POP Simple Cache (successful reads
// only; misses are not cached). Any cache failure falls back to KV.
func (s *KVStore) cachedRead(key string) ([]byte, error) {
	rc, err := simple.GetOrSet([]byte("kv:v1:"+key), func() (simple.CacheEntry, error) {
		b, err := s.read(key)
		if err != nil {
			return simple.CacheEntry{}, err
		}
		return simple.CacheEntry{Body: bytes.NewReader(b), TTL: s.CacheTTL}, nil
	})
	if err == nil {
		defer rc.Close()
		if b, rerr := io.ReadAll(io.LimitReader(rc, maxRecordSize)); rerr == nil {
			return b, nil
		}
	}
	if errors.Is(err, ErrNotFound) {
		return nil, ErrNotFound
	}
	return s.read(key)
}

func (s *KVStore) read(code string) ([]byte, error) {
	st, err := s.open()
	if err != nil {
		return nil, err
	}
	e, err := st.Lookup(code)
	if err != nil {
		if errors.Is(err, kvstore.ErrKeyNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("%w: lookup: %v", ErrUnavailable, err)
	}
	b, err := io.ReadAll(io.LimitReader(e, maxRecordSize))
	if err != nil {
		return nil, fmt.Errorf("%w: read: %v", ErrUnavailable, err)
	}
	return b, nil
}

// Create implements Store using InsertModeAdd, which makes the KV Store
// reject the write if the key already exists (conditional write), so an
// existing link can never be overwritten.
func (s *KVStore) Create(ctx context.Context, code string, link Link) error {
	st, err := s.open()
	if err != nil {
		return err
	}
	b, err := link.Encode()
	if err != nil {
		return err
	}
	err = st.InsertWithConfig(code, bytes.NewReader(b), &kvstore.InsertConfig{Mode: kvstore.InsertModeAdd})
	switch {
	case err == nil:
		return nil
	case errors.Is(err, kvstore.ErrPreconditionFailed):
		return ErrExists
	default:
		return fmt.Errorf("%w: insert: %v", ErrUnavailable, err)
	}
}

// LookupURL implements Store. Index entries are immutable, so they share the
// per-POP read cache with links.
func (s *KVStore) LookupURL(ctx context.Context, normalizedURL string) (string, error) {
	key := URLIndexKey(normalizedURL)
	var b []byte
	var err error
	if s.CacheTTL > 0 {
		b, err = s.cachedRead(key)
	} else {
		b, err = s.read(key)
	}
	if err != nil {
		return "", err
	}
	return string(bytes.TrimSpace(b)), nil
}

// IndexURL implements Store with InsertModeAdd (never overwrites).
func (s *KVStore) IndexURL(ctx context.Context, normalizedURL, code string) error {
	st, err := s.open()
	if err != nil {
		return err
	}
	err = st.InsertWithConfig(URLIndexKey(normalizedURL), strings.NewReader(code), &kvstore.InsertConfig{Mode: kvstore.InsertModeAdd})
	switch {
	case err == nil:
		return nil
	case errors.Is(err, kvstore.ErrPreconditionFailed):
		return ErrExists
	default:
		return fmt.Errorf("%w: index insert: %v", ErrUnavailable, err)
	}
}
