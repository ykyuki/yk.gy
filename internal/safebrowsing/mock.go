package safebrowsing

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// Mode selects the checker implementation.
type Mode string

const (
	// ModeLive calls the real Google Safe Browsing API.
	ModeLive Mode = "live"
	// ModeMock uses the deterministic Mock. Only honoured on the local
	// development server (see SelectMode).
	ModeMock Mode = "mock"
)

// SelectMode decides which checker to use.
//
// The mock can ONLY be selected when the code runs on the local Fastly
// development server (FASTLY_HOSTNAME == "localhost", which is set by
// Viceroy and can never be the value on Fastly's edge). On Production and
// Development services the configured value is ignored and the live API is
// always used, so the safety check cannot be disabled by configuration.
func SelectMode(configured string, localRuntime bool) Mode {
	if localRuntime && Mode(strings.ToLower(strings.TrimSpace(configured))) == ModeMock {
		return ModeMock
	}
	return ModeLive
}

// IsLocalRuntime reports whether FASTLY_HOSTNAME identifies the local
// development server.
func IsLocalRuntime(fastlyHostname string) bool { return fastlyHostname == "localhost" }

// Mock is a deterministic local-only checker.
//
// Rules (also documented in README):
//   - testsafebrowsing.appspot.com/s/malware.html      -> MALWARE
//   - testsafebrowsing.appspot.com/s/phishing.html     -> SOCIAL_ENGINEERING
//   - testsafebrowsing.appspot.com/s/unwanted.html     -> UNWANTED_SOFTWARE
//   - host malware.example.com (and subdomains)        -> MALWARE
//   - host phishing.example.com (and subdomains)       -> SOCIAL_ENGINEERING
//   - host sb-unavailable.example.com                  -> ErrUnavailable (fail-closed test)
//   - everything else                                  -> safe
type Mock struct{}

// Check implements Checker.
func (Mock) Check(_ context.Context, rawURL string) (Verdict, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return Verdict{}, fmt.Errorf("%w: mock parse: %v", ErrUnavailable, err)
	}
	host := strings.ToLower(u.Hostname())
	path := strings.ToLower(u.EscapedPath())
	is := func(h string) bool { return host == h || strings.HasSuffix(host, "."+h) }
	switch {
	case host == "sb-unavailable.example.com":
		return Verdict{}, fmt.Errorf("%w: mock outage", ErrUnavailable)
	case host == "testsafebrowsing.appspot.com" && strings.HasPrefix(path, "/s/malware"):
		return Verdict{Threats: []string{"MALWARE"}}, nil
	case host == "testsafebrowsing.appspot.com" && strings.HasPrefix(path, "/s/phishing"):
		return Verdict{Threats: []string{"SOCIAL_ENGINEERING"}}, nil
	case host == "testsafebrowsing.appspot.com" && strings.HasPrefix(path, "/s/unwanted"):
		return Verdict{Threats: []string{"UNWANTED_SOFTWARE"}}, nil
	case is("malware.example.com"):
		return Verdict{Threats: []string{"MALWARE"}}, nil
	case is("phishing.example.com"):
		return Verdict{Threats: []string{"SOCIAL_ENGINEERING"}}, nil
	}
	return Verdict{Safe: true}, nil
}
