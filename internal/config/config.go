// Package config loads per-environment settings from the Fastly Config
// Store linked under the logical name "app_config". The same Go code runs in
// every environment; only the store contents differ.
package config

import (
	"strconv"
	"strings"
	"time"
)

// Logical resource names. These are identical in every environment; each
// Fastly service links its own (environment specific) store under them.
const (
	ConfigStoreName = "app_config"
	KVStoreName     = "links"
	SecretStoreName = "secrets"
	SecretSBKey     = "safebrowsing_api_key"
	BackendSB       = "safebrowsing"
	LogEndpoint     = "app_log"
	RateCounter     = "shorten_rc"
	PenaltyBox      = "shorten_pb"
)

// DefaultBlockedHosts are well known third-party shorteners. Chaining
// shorteners hides the real destination from Safe Browsing.
var DefaultBlockedHosts = []string{
	"bit.ly", "bitly.com", "j.mp", "t.co", "tinyurl.com", "goo.gl", "ow.ly",
	"is.gd", "v.gd", "buff.ly", "rebrand.ly", "cutt.ly", "shorturl.at",
	"rb.gy", "t.ly", "tiny.cc", "lnkd.in", "s.id", "x.gd", "amzn.to",
}

// Config holds runtime settings.
type Config struct {
	Environment       string // production | development | local
	BaseURL           string // e.g. https://yk.gy (no trailing slash)
	ShortHosts        []string
	BlockedHosts      []string
	SafeBrowsingMode  string // "mock" is honoured only on the local server
	RatePerMinute     uint32
	RatePenalty       time.Duration
	RateLimitFailOpen bool // default false: limiter errors reject requests
	RedirectCacheTTL  time.Duration
}

// Getter reads one key; ok=false when missing.
type Getter func(key string) (value string, ok bool)

// Load builds a Config from g, applying safe defaults. Unknown or invalid
// values fall back to defaults rather than weakening security.
func Load(g Getter) Config {
	get := func(k, def string) string {
		if g == nil {
			return def
		}
		if v, ok := g(k); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
		return def
	}
	c := Config{
		Environment:       get("environment", "unknown"),
		BaseURL:           strings.TrimRight(get("base_url", ""), "/"),
		ShortHosts:        splitList(get("short_hosts", "yk.gy")),
		BlockedHosts:      splitList(get("blocked_hosts", strings.Join(DefaultBlockedHosts, ","))),
		SafeBrowsingMode:  get("safebrowsing_mode", "live"),
		RatePerMinute:     uint32(atoiRange(get("ratelimit_per_minute", "10"), 10, 1, 1000)),
		RatePenalty:       time.Duration(atoiRange(get("ratelimit_penalty_seconds", "300"), 300, 60, 3600)) * time.Second,
		RateLimitFailOpen: get("ratelimit_on_error", "deny") == "allow",
		RedirectCacheTTL:  time.Duration(atoiRange(get("redirect_cache_ttl_seconds", "0"), 0, 0, 86400)) * time.Second,
	}
	return c
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.ToLower(strings.TrimSpace(p)); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func atoiRange(s string, def, lo, hi int) int {
	n, err := strconv.Atoi(s)
	if err != nil || n < lo || n > hi {
		return def
	}
	return n
}
