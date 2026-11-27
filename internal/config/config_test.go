package config

import (
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	c := Load(nil)
	if c.SafeBrowsingMode != "live" || c.RatePerMinute != 10 || c.RatePenalty != 5*time.Minute || c.RateLimitFailOpen || c.RedirectCacheTTL != 0 {
		t.Fatalf("bad defaults %+v", c)
	}
	if len(c.BlockedHosts) == 0 || c.ShortHosts[0] != "yk.gy" {
		t.Fatalf("bad host defaults %+v", c)
	}
}

func TestLoadValues(t *testing.T) {
	m := map[string]string{
		"environment": "development", "base_url": "https://dev.yk.gy/",
		"short_hosts": " yk.gy, DEV.yk.gy ,", "ratelimit_per_minute": "5",
		"ratelimit_penalty_seconds": "120", "ratelimit_on_error": "allow",
		"redirect_cache_ttl_seconds": "60",
	}
	c := Load(func(k string) (string, bool) { v, ok := m[k]; return v, ok })
	if c.Environment != "development" || c.BaseURL != "https://dev.yk.gy" || len(c.ShortHosts) != 2 || c.ShortHosts[1] != "dev.yk.gy" {
		t.Fatalf("%+v", c)
	}
	if c.RatePerMinute != 5 || c.RatePenalty != 2*time.Minute || !c.RateLimitFailOpen || c.RedirectCacheTTL != time.Minute {
		t.Fatalf("%+v", c)
	}
}

func TestLoadInvalidFallsBack(t *testing.T) {
	m := map[string]string{"ratelimit_per_minute": "0", "ratelimit_penalty_seconds": "abc", "redirect_cache_ttl_seconds": "-5", "ratelimit_on_error": "yes"}
	c := Load(func(k string) (string, bool) { v, ok := m[k]; return v, ok })
	if c.RatePerMinute != 10 || c.RatePenalty != 5*time.Minute || c.RedirectCacheTTL != 0 || c.RateLimitFailOpen {
		t.Fatalf("%+v", c)
	}
}
