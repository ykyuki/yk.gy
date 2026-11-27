package validation

import (
	"strings"
	"testing"
)

var opts = Options{
	ShortHosts:   []string{"yk.gy", "dev.yk.gy"},
	BlockedHosts: []string{"bit.ly", "t.co"},
}

func TestNormalizeAccepts(t *testing.T) {
	cases := map[string]string{
		"https://example.com":                     "https://example.com/",
		"HTTPS://EXAMPLE.com/Path?Q=1#Frag":       "https://example.com/Path?Q=1#Frag",
		"http://example.com:80/a":                 "http://example.com/a",
		"https://example.com:443/a":               "https://example.com/a",
		"https://example.com:8443/a":              "https://example.com:8443/a",
		"https://example.com./":                   "https://example.com/",
		"https://日本語.jp/パス":                       "https://xn--wgv71a119e.jp/%E3%83%91%E3%82%B9",
		"https://8.8.8.8/":                        "https://8.8.8.8/",
		"https://[2001:4860:4860::8888]/dns":      "https://[2001:4860:4860::8888]/dns",
		"https://sub.example.co.uk/a%20b":         "https://sub.example.co.uk/a%20b",
		"https://notyk.gy/":                       "https://notyk.gy/",
		"https://example.com/?u=https://yk.gy/xx": "https://example.com/?u=https://yk.gy/xx",
	}
	for in, want := range cases {
		got, err := Normalize(in, opts)
		if err != nil {
			t.Errorf("%q: unexpected error %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}

func TestNormalizeRejects(t *testing.T) {
	cases := map[string]*Error{
		"":    ErrEmpty,
		"   ": ErrEmpty,
		"https://example.com/" + strings.Repeat("a", 2100): ErrTooLong,
		"example.com":                                  ErrScheme,
		"//example.com/":                               ErrScheme,
		"javascript:alert(1)":                          ErrScheme,
		"JaVaScRiPt:alert(1)":                          ErrScheme,
		"data:text/html,<script>":                      ErrScheme,
		"file:///etc/passwd":                           ErrScheme,
		"ftp://example.com/":                           ErrScheme,
		"http:example.com":                             ErrInvalid,
		"https:///path":                                ErrInvalid,
		"https://user:pass@example.com/":               ErrUserinfo,
		"https://google.com@evil.example/":             ErrUserinfo,
		"http://localhost/":                            ErrPrivateHost,
		"http://LOCALHOST:8080/":                       ErrPrivateHost,
		"http://foo.localhost/":                        ErrPrivateHost,
		"http://intranet/":                             ErrPrivateHost,
		"http://printer.local/":                        ErrPrivateHost,
		"http://db.internal/":                          ErrPrivateHost,
		"http://router.home.arpa/":                     ErrPrivateHost,
		"http://10.0.0.1/":                             ErrPrivateHost,
		"http://172.16.5.4/":                           ErrPrivateHost,
		"http://192.168.0.1/":                          ErrPrivateHost,
		"http://127.0.0.1/":                            ErrPrivateHost,
		"http://127.1/":                                ErrPrivateHost,
		"http://0/":                                    ErrPrivateHost,
		"http://0.0.0.0/":                              ErrPrivateHost,
		"http://2130706433/":                           ErrPrivateHost,
		"http://0x7f000001/":                           ErrPrivateHost,
		"http://0177.0.0.1/":                           ErrPrivateHost,
		"http://0x7f.1/":                               ErrPrivateHost,
		"http://169.254.169.254/latest/":               ErrPrivateHost,
		"http://100.64.1.1/":                           ErrPrivateHost,
		"http://[::1]/":                                ErrPrivateHost,
		"http://[::]/":                                 ErrPrivateHost,
		"http://[fe80::1]/":                            ErrPrivateHost,
		"http://[fd00::1]/":                            ErrPrivateHost,
		"http://[::ffff:127.0.0.1]/":                   ErrPrivateHost,
		"http://[::ffff:10.0.0.1]/":                    ErrPrivateHost,
		"http://[fe80::1%25eth0]/":                     ErrPrivateHost,
		"http://127.0.0.1.nip.io/":                     ErrPrivateHost,
		"http://10.0.0.1.sslip.io/":                    ErrPrivateHost,
		"http://localtest.me/":                         ErrPrivateHost,
		"http://224.0.0.1/":                            ErrPrivateHost,
		"https://yk.gy/abcdefg":                        ErrRecursive,
		"https://YK.GY/abcdefg":                        ErrRecursive,
		"https://dev.yk.gy/abcdefg":                    ErrRecursive,
		"https://www.yk.gy/":                           ErrRecursive,
		"https://yk.gy./abcdefg":                       ErrRecursive,
		"https://bit.ly/x":                             ErrBlockedHost,
		"https://www.bit.ly/x":                         ErrBlockedHost,
		"https://t.co/x":                               ErrBlockedHost,
		"https://example.com:0/":                       ErrInvalidPort,
		"https://example.com:70000/":                   ErrInvalidPort,
		"https://example.com:/":                        ErrInvalidPort,
		"https://exa mple.com/":                        ErrControlChars,
		"https://example.com/a\nb":                     ErrControlChars,
		"https://example.com/a\tb":                     ErrControlChars,
		"https://example.com/a\u2028b":                 ErrControlChars,
		"https://ex<ample.com/":                        ErrInvalidHost,
		"https://-bad-.com/":                           ErrInvalidHost,
		"https://a..b.com/":                            ErrInvalidHost,
		"https://example.123/":                         ErrInvalidHost,
		"https://" + strings.Repeat("a", 64) + ".com/": ErrInvalidHost,
	}
	for in, want := range cases {
		_, err := Normalize(in, opts)
		ve, ok := AsError(err)
		if !ok {
			t.Errorf("%q: expected %s, got %v", in, want.Code, err)
			continue
		}
		if ve != want {
			t.Errorf("%q: got %s want %s", in, ve.Code, want.Code)
		}
	}
}

func TestParseLegacyIPv4(t *testing.T) {
	cases := map[string]string{
		"1.2.3.4": "1.2.3.4", "16909060": "1.2.3.4", "0x01020304": "1.2.3.4",
		"1.0x2.3.4": "1.2.3.4", "01.02.03.04": "1.2.3.4", "1.2.772": "1.2.3.4", "1.131844": "1.2.3.4",
	}
	for in, want := range cases {
		a, ok := parseLegacyIPv4(in)
		if !ok || a.String() != want {
			t.Errorf("%q: got %v %v want %s", in, a, ok, want)
		}
	}
	for _, in := range []string{"1.2.3.4.5", "256.1.1.1", "example.com", "1.2.3.", "4294967296", "08.1.1.1"} {
		if _, ok := parseLegacyIPv4(in); ok {
			t.Errorf("%q should not parse", in)
		}
	}
}

func FuzzNormalize(f *testing.F) {
	for _, s := range []string{"https://example.com", "http://127.0.0.1", "javascript:x", "https://日本.jp"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		out, err := Normalize(s, opts)
		if err != nil {
			return
		}
		if !strings.HasPrefix(out, "http://") && !strings.HasPrefix(out, "https://") {
			t.Fatalf("accepted non-http url %q -> %q", s, out)
		}
		// Normalization must be idempotent.
		again, err := Normalize(out, opts)
		if err != nil || again != out {
			t.Fatalf("not idempotent: %q -> %q -> %q (%v)", s, out, again, err)
		}
	})
}
