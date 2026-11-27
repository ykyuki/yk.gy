package ratelimit

import (
	"errors"
	"fmt"
	"io"
	"strings"
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

// Trace, when non-nil, receives one line per cache operation (diagnostics on
// non-production environments only).
var Trace func(string)

func trace(format string, a ...any) {
	if Trace != nil {
		Trace(fmt.Sprintf(format, a...))
	}
}

// Exists implements SlotStore.
func (CoreCacheStore) Exists(key string) (bool, error) {
	f, err := core.Lookup([]byte(key), core.LookupOptions{})
	if errors.Is(err, core.ErrNotFound) {
		trace("has %s=no", shortKey(key))
		return false, nil
	}
	if err != nil {
		trace("has %s err=%v", shortKey(key), err)
		return false, err
	}
	_ = f.Body.Close()
	ok := f.Usable() && !f.Stale()
	trace("has %s=%v", shortKey(key), ok)
	return ok, nil
}

// Claim implements SlotStore: it creates key only if it does not exist yet.
// The cache transaction collapses concurrent claims of the same key, so only
// one caller wins it.
func (CoreCacheStore) Claim(key string, ttl time.Duration) (bool, error) {
	tx, err := core.NewTransaction([]byte(key), core.LookupOptions{})
	if err != nil {
		trace("claim %s err=%v", shortKey(key), err)
		return false, err
	}
	defer tx.Close()
	if !tx.MustInsert() {
		trace("claim %s=taken", shortKey(key))
		return false, nil
	}
	w, err := tx.Insert(core.WriteOptions{TTL: ttl, Length: 1, SensitiveData: true})
	if err != nil {
		trace("claim %s insert err=%v", shortKey(key), err)
		return false, err
	}
	if _, err := io.WriteString(w, "1"); err != nil {
		_ = w.Abandon()
		return false, err
	}
	if err := w.Close(); err != nil {
		trace("claim %s close err=%v", shortKey(key), err)
		return false, err
	}
	trace("claim %s=ok", shortKey(key))
	return true, nil
}

// shortKey drops the configurable prefix for readable traces.
func shortKey(k string) string {
	if i := strings.Index(k, "pb:"); i >= 0 {
		return k[i:]
	}
	if i := strings.Index(k, "rl:"); i >= 0 {
		return k[i:]
	}
	return k
}
