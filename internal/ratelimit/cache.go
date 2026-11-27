package ratelimit

import (
	"errors"
	"io"
	"time"

	"github.com/fastly/compute-sdk-go/cache/core"
)

// CoreCacheStore implements SlotStore with Fastly's Core Cache API, which is
// part of Compute (no extra product).
//
// Why insert-only "slots" instead of a counter that is overwritten: measured
// on dev.yk.gy, an overwritten cache object keeps being served in its old
// version by the server that wrote it, so counters written from different
// servers of a POP diverge and never reach the limit. Slot objects are only
// ever created, never modified, so stale local copies cannot be wrong.
// Objects are marked SensitiveData so they are never persisted to disk.
type CoreCacheStore struct{}

// Exists implements SlotStore.
func (CoreCacheStore) Exists(key string) (bool, error) {
	f, err := core.Lookup([]byte(key), core.LookupOptions{})
	if errors.Is(err, core.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	_ = f.Body.Close()
	return f.Usable() && !f.Stale(), nil
}

// Claim implements SlotStore: it creates key only if it does not exist yet.
// The cache transaction collapses concurrent claims of the same key, so only
// one caller wins it.
func (CoreCacheStore) Claim(key string, ttl time.Duration) (bool, error) {
	tx, err := core.NewTransaction([]byte(key), core.LookupOptions{})
	if err != nil {
		return false, err
	}
	defer tx.Close()
	if !tx.MustInsert() {
		return false, nil
	}
	w, err := tx.Insert(core.WriteOptions{TTL: ttl, Length: 1, SensitiveData: true})
	if err != nil {
		return false, err
	}
	if _, err := io.WriteString(w, "1"); err != nil {
		_ = w.Abandon()
		return false, err
	}
	if err := w.Close(); err != nil {
		return false, err
	}
	return true, nil
}
