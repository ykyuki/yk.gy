// Command shortener is the Fastly Compute entry point. It wires the
// environment-independent handler to Fastly resources that are referenced
// only by logical names (see internal/config). Which physical KV / Secret /
// Config store and log endpoint those names resolve to is decided by the
// Fastly service the package is deployed to (production or development),
// or by [local_server] in fastly.toml when running locally.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"runtime/debug"
	"time"

	"github.com/fastly/compute-sdk-go/cache/core"
	"github.com/fastly/compute-sdk-go/configstore"
	"github.com/fastly/compute-sdk-go/fsthttp"
	"github.com/fastly/compute-sdk-go/geo"
	"github.com/fastly/compute-sdk-go/rtlog"
	"github.com/fastly/compute-sdk-go/secretstore"

	"github.com/ykyuki/yk.gy/internal/config"
	"github.com/ykyuki/yk.gy/internal/handler"
	"github.com/ykyuki/yk.gy/internal/ratelimit"
	"github.com/ykyuki/yk.gy/internal/safebrowsing"
	"github.com/ykyuki/yk.gy/internal/shortener"
	"github.com/ykyuki/yk.gy/internal/storage"
	"github.com/ykyuki/yk.gy/internal/validation"
	"github.com/ykyuki/yk.gy/web"
)

var assets = web.Assets()

func main() {
	fsthttp.ServeFunc(func(ctx context.Context, w fsthttp.ResponseWriter, r *fsthttp.Request) {
		buildHandler().ServeHTTP(ctx, w, r)
	})
}

func buildHandler() *handler.Handler {
	local := safebrowsing.IsLocalRuntime(os.Getenv("FASTLY_HOSTNAME"))
	cfg := config.Load(configGetter())
	logger := newLogger()

	var checker safebrowsing.Checker
	switch safebrowsing.SelectMode(cfg.SafeBrowsingMode, local) {
	case safebrowsing.ModeMock:
		checker = safebrowsing.Mock{}
	default:
		if !local && cfg.SafeBrowsingMode == string(safebrowsing.ModeMock) {
			logger(map[string]any{"event": "config_warning", "msg": "safebrowsing_mode=mock ignored outside the local server"})
		}
		key, _ := secretstore.Plaintext(config.SecretStoreName, config.SecretSBKey) // missing key => fail closed in Check
		checker = &safebrowsing.Client{APIKey: string(key), HTTP: fastlyHTTP{backend: config.BackendSB}}
	}

	kv := &storage.KVStore{Name: config.KVStoreName, CacheTTL: cfg.RedirectCacheTTL}
	return &handler.Handler{
		Config: cfg,
		Service: &shortener.Service{
			Store:      kv,
			Checker:    checker,
			Validation: validation.Options{ShortHosts: cfg.ShortHosts, BlockedHosts: cfg.BlockedHosts},
		},
		Limiter: newLimiter(cfg, local, logger),
		Health: func() map[string]error {
			_, cerr := configstore.Open(config.ConfigStoreName)
			_, serr := secretstore.Open(config.SecretStoreName)
			return map[string]error{"kv_store": kv.Ping(), "config_store": cerr, "secret_store": serr}
		},
		Log:            logger,
		Commit:         commit(),
		ServiceVersion: os.Getenv("FASTLY_SERVICE_VERSION"),
		LocalRuntime:   local,
		Assets:         assets,
		Country:        countryOf,
	}
}

// newLimiter builds the cache-based rate limiter. Only on the local server
// (Viceroy, which cannot read Core Cache entries from the Go SDK) an
// "unsupported" cache error lets the request through with a warning; on
// Fastly every cache error fails closed.
func newLimiter(cfg config.Config, local bool, logger handler.Logger) ratelimit.Limiter {
	w := &ratelimit.Window{
		Store:  ratelimit.CoreCacheStore{},
		Policy: ratelimit.Policy{PerMinute: cfg.RatePerMinute, Penalty: cfg.RatePenalty},
		Prefix: config.RateLimitPrefix,
	}
	if !local {
		return w
	}
	return ratelimit.LocalUnsupportedFallback{
		Limiter:     w,
		Unsupported: core.ErrUnsupported,
		Warn: func(err error) {
			logger(map[string]any{"event": "ratelimit_local_unsupported", "error": err.Error(), "msg": "rate limiting skipped on the local server only"})
		},
	}
}

// countryOf returns the country code from Fastly's geolocation ("" if
// unknown or on error).
func countryOf(ip string) string {
	g, err := geo.Lookup(net.ParseIP(ip))
	if err != nil || g == nil {
		return ""
	}
	return g.CountryCode
}

func configGetter() config.Getter {
	st, err := configstore.Open(config.ConfigStoreName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config store %q unavailable: %v\n", config.ConfigStoreName, err)
		return nil
	}
	return func(k string) (string, bool) {
		v, err := st.Get(k)
		return v, err == nil
	}
}

// newLogger writes JSON lines to the environment's real-time log endpoint
// (logical name "app_log") and to stdout (visible in `fastly log-tail` and
// locally in the terminal).
func newLogger() handler.Logger {
	ep := rtlog.Open(config.LogEndpoint)
	return func(ev map[string]any) {
		ev["ts"] = time.Now().UTC().Format(time.RFC3339Nano)
		b, err := json.Marshal(ev)
		if err != nil {
			return
		}
		b = append(b, '\n')
		_, _ = os.Stdout.Write(b)
		_, _ = ep.Write(b) // endpoint may be absent (e.g. locally): ignore
	}
}

func commit() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		rev, dirty := "", false
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.modified":
				dirty = s.Value == "true"
			}
		}
		if rev != "" {
			if dirty {
				rev += "-dirty"
			}
			return rev
		}
	}
	return "unknown"
}

// fastlyHTTP sends requests to a static Fastly backend, bypassing the cache.
type fastlyHTTP struct{ backend string }

func (f fastlyHTTP) Get(ctx context.Context, url string, header map[string]string) (safebrowsing.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := fsthttp.NewRequest(fsthttp.MethodGet, url, nil)
	if err != nil {
		return safebrowsing.Response{}, err
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	req.CacheOptions.Pass = true
	resp, err := req.Send(ctx, f.backend)
	if err != nil {
		return safebrowsing.Response{}, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20+1))
	if err != nil {
		return safebrowsing.Response{}, err
	}
	return safebrowsing.Response{Status: resp.StatusCode, ContentType: resp.Header.Get("Content-Type"), Body: b}, nil
}
