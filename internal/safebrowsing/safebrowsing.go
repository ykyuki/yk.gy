// Package safebrowsing checks URLs against the Google Safe Browsing API v5
// (urls:search). Any failure is reported as an error so that callers can
// fail closed.
package safebrowsing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// Endpoint is the Safe Browsing v5 urls:search method.
const Endpoint = "https://safebrowsing.googleapis.com/v5/urls:search"

// maxResponseBytes bounds the size of an accepted API response.
const maxResponseBytes = 1 << 20

var (
	// ErrUnavailable means the verdict could not be obtained (network
	// error, non-200, malformed response, missing API key, ...). Callers
	// MUST treat it as "not safe" (fail closed).
	ErrUnavailable = errors.New("safebrowsing: unavailable")
)

// Verdict is the result of a check.
type Verdict struct {
	Safe    bool
	Threats []string // e.g. MALWARE, SOCIAL_ENGINEERING
}

// Checker checks a single URL.
type Checker interface {
	Check(ctx context.Context, rawURL string) (Verdict, error)
}

// HTTPDoer performs a GET request. It is implemented with fsthttp on
// Fastly (cmd/shortener) and with fakes in tests.
type HTTPDoer interface {
	Get(ctx context.Context, url string, header map[string]string) (status int, body []byte, err error)
}

// Client is the live Safe Browsing v5 client.
type Client struct {
	APIKey   string
	HTTP     HTTPDoer
	Endpoint string // optional override (tests)
}

type searchResponse struct {
	Threats []struct {
		URL         string   `json:"url"`
		ThreatTypes []string `json:"threatTypes"`
	} `json:"threats"`
	CacheDuration string `json:"cacheDuration"`
}

// Check implements Checker.
func (c *Client) Check(ctx context.Context, rawURL string) (Verdict, error) {
	if c == nil || c.HTTP == nil {
		return Verdict{}, fmt.Errorf("%w: client not configured", ErrUnavailable)
	}
	if strings.TrimSpace(c.APIKey) == "" {
		return Verdict{}, fmt.Errorf("%w: api key missing", ErrUnavailable)
	}
	ep := c.Endpoint
	if ep == "" {
		ep = Endpoint
	}
	q := url.Values{}
	q.Set("urls", rawURL)
	// Google APIs answer in protobuf (application/x-protobuf) by default and
	// ignore the Accept header; alt=json selects the JSON encoding.
	q.Set("alt", "json")
	// The API key is sent as a header rather than a query parameter so it
	// does not end up in URL-based logs.
	status, body, err := c.HTTP.Get(ctx, ep+"?"+q.Encode(), map[string]string{
		"X-Goog-Api-Key": c.APIKey,
		"Accept":         "application/json",
	})
	if err != nil {
		return Verdict{}, fmt.Errorf("%w: request failed: %v", ErrUnavailable, err)
	}
	if status != 200 {
		return Verdict{}, fmt.Errorf("%w: http status %d: %s", ErrUnavailable, status, googleErrorReason(body))
	}
	if len(body) > maxResponseBytes {
		return Verdict{}, fmt.Errorf("%w: response too large", ErrUnavailable)
	}
	var r searchResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return Verdict{}, fmt.Errorf("%w: malformed response: %v", ErrUnavailable, err)
	}
	return verdictFrom(r), nil
}

func verdictFrom(r searchResponse) Verdict {
	seen := map[string]bool{}
	var threats []string
	for _, t := range r.Threats {
		for _, tt := range t.ThreatTypes {
			if !seen[tt] {
				seen[tt] = true
				threats = append(threats, tt)
			}
		}
		if len(t.ThreatTypes) == 0 && !seen["THREAT_TYPE_UNSPECIFIED"] {
			// A match without a type is still a match.
			seen["THREAT_TYPE_UNSPECIFIED"] = true
			threats = append(threats, "THREAT_TYPE_UNSPECIFIED")
		}
	}
	return Verdict{Safe: len(threats) == 0, Threats: threats}
}

// googleErrorReason extracts the machine-readable reason (e.g.
// API_KEY_INVALID, SERVICE_DISABLED) from a Google API error body.
func googleErrorReason(body []byte) string {
	var e struct {
		Error struct {
			Status  string `json:"status"`
			Details []struct {
				Reason string `json:"reason"`
			} `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) != nil {
		return "unknown"
	}
	for _, d := range e.Error.Details {
		if d.Reason != "" {
			return d.Reason
		}
	}
	if e.Error.Status != "" {
		return e.Error.Status
	}
	return "unknown"
}

// Reason returns a short, non-sensitive classification of a check error,
// suitable for health endpoints (never includes the API key or the URL).
func Reason(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "api key missing"):
		return "api_key_missing"
	case strings.Contains(msg, "client not configured"):
		return "client_not_configured"
	case strings.Contains(msg, "request failed"):
		return "request_failed"
	case strings.Contains(msg, "http status"):
		var code int
		var reason string
		if i := strings.Index(msg, "http status "); i >= 0 {
			rest := msg[i+len("http status "):]
			fmt.Sscanf(rest, "%d", &code)
			if j := strings.Index(rest, ": "); j >= 0 {
				reason = rest[j+2:]
			}
		}
		r := fmt.Sprintf("http_status_%d", code)
		if reason != "" && reason != "unknown" && isToken(reason) {
			r += "_" + reason
		}
		return r
	case strings.Contains(msg, "malformed response"):
		return "malformed_response"
	case strings.Contains(msg, "response too large"):
		return "response_too_large"
	}
	return "unavailable"
}

func isToken(s string) bool {
	if len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}
