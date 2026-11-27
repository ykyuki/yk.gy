package privacy

import "testing"

func TestIPAllowed(t *testing.T) {
	for _, c := range []string{"DE", "fr", "IE", "NO", "IS", "LI", "GB", " gb ", "", "**", "--"} {
		if IPAllowed(c) {
			t.Errorf("%q: IP must not be logged", c)
		}
	}
	for _, c := range []string{"JP", "US", "CH", "CA", "KR", "AU"} {
		if !IPAllowed(c) {
			t.Errorf("%q: IP may be logged", c)
		}
	}
	if len(noIPCountries) != 31 {
		t.Fatalf("expected EU27 + EEA3 + UK = 31 countries, got %d", len(noIPCountries))
	}
}

func TestClientIP(t *testing.T) {
	cases := map[string]string{
		"203.0.113.5": "203.0.113.5", "203.0.113.5:443": "203.0.113.5",
		"2001:db8::1": "2001:db8::1", "[2001:db8::1]:443": "2001:db8::1",
		"::ffff:203.0.113.5": "203.0.113.5", "garbage": "",
	}
	for in, want := range cases {
		if got := ClientIP(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}
