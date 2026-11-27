package ratelimit

import (
	"errors"
	"io"
	"strings"
	"time"

	"github.com/fastly/compute-sdk-go/cache/core"
)

// maxCounterBytes bounds how much of a cached counter is read.
const maxCounterBytes = 32

// CoreCacheStore implements CounterStore with Fastly's Core Cache API. The
// cache is local to each POP (and shared by all servers in it), so limits
// are enforced per POP, like Fastly's own rate limiting. It is included in
// Compute and needs no extra product. Counters are marked SensitiveData so
// they are never persisted to disk.
type CoreCacheStore struct{}

// Get implements CounterStore.
func (CoreCacheStore) Get(key string) (string, bool, error) {
	f, err := core.Lookup([]byte(key), core.LookupOptions{})
	if errors.Is(err, core.ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	defer f.Body.Close()
	if !f.Usable() || f.Stale() {
		return "", false, nil
	}
	b, err := io.ReadAll(io.LimitReader(f.Body, maxCounterBytes))
	if err != nil {
		return "", false, err
	}
	return strings.TrimSpace(string(b)), true, nil
}

// Set implements CounterStore (unconditional overwrite).
func (CoreCacheStore) Set(key, value string, ttl time.Duration) error {
	w, err := core.Insert([]byte(key), core.WriteOptions{
		TTL:           ttl,
		Length:        uint64(len(value)),
		SensitiveData: true,
	})
	if err != nil {
		return err
	}
	if _, err := io.WriteString(w, value); err != nil {
		_ = w.Abandon()
		return err
	}
	return w.Close()
}
