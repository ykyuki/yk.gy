package ratelimit

import (
	"testing"
	"time"
)

func TestMemoryLimiter(t *testing.T) {
	now := time.Unix(1000, 0)
	m := &Memory{Policy: Policy{PerMinute: 3, Penalty: 5 * time.Minute}, Now: func() time.Time { return now }}
	for i := 0; i < 3; i++ {
		if ok, _ := m.Allow("a"); !ok {
			t.Fatalf("request %d should pass", i)
		}
	}
	if ok, _ := m.Allow("a"); ok {
		t.Fatal("4th request must be limited")
	}
	if ok, _ := m.Allow("b"); !ok {
		t.Fatal("other clients are independent")
	}
	now = now.Add(2 * time.Minute) // window passed but still in penalty box
	if ok, _ := m.Allow("a"); ok {
		t.Fatal("penalty box must still block")
	}
	now = now.Add(4 * time.Minute)
	if ok, _ := m.Allow("a"); !ok {
		t.Fatal("should be allowed after penalty")
	}
}

func TestClientKey(t *testing.T) {
	cases := map[string]string{
		"203.0.113.9":           "203.0.113.9",
		"203.0.113.9:1234":      "203.0.113.9",
		"::ffff:203.0.113.9":    "203.0.113.9",
		"2001:db8:1:2:3:4:5:6":  "2001:db8:1:2::/64",
		"[2001:db8:1:2::9]:443": "2001:db8:1:2::/64",
		"garbage":               "unknown",
	}
	for in, want := range cases {
		if got := ClientKey(in); got != want {
			t.Errorf("ClientKey(%q)=%q want %q", in, got, want)
		}
	}
}
