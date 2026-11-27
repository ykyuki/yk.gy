package ratelimit

import "testing"

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
