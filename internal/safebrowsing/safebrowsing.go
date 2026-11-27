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
		return Verdict{}, fmt.Errorf("%w: http status %d", ErrUnavailable, status)
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
