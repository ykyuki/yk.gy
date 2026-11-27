package safebrowsing

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
)

const protoCT = "application/x-protobuf"

type fakeHTTP struct {
	resp   Response
	err    error
	gotURL string
	gotHdr map[string]string
}

func (f *fakeHTTP) Get(_ context.Context, u string, h map[string]string) (Response, error) {
	f.gotURL, f.gotHdr = u, h
	return f.resp, f.err
}

// --- protobuf fixture builders (SearchUrlsResponse / google.rpc.Status) ---

func threatURL(u string, types []uint64, packed bool) []byte {
	var m []byte
	m = protowire.AppendTag(m, 1, protowire.BytesType)
	m = protowire.AppendString(m, u)
	if packed {
		var p []byte
		for _, t := range types {
			p = protowire.AppendVarint(p, t)
		}
		m = protowire.AppendTag(m, 2, protowire.BytesType)
		m = protowire.AppendBytes(m, p)
	} else {
		for _, t := range types {
			m = protowire.AppendTag(m, 2, protowire.VarintType)
			m = protowire.AppendVarint(m, t)
		}
	}
	return m
}

func searchResponse(threats ...[]byte) []byte {
	var b []byte
	for _, t := range threats {
		b = protowire.AppendTag(b, 1, protowire.BytesType)
		b = protowire.AppendBytes(b, t)
	}
	// cache_duration { seconds: 300 }
	var d []byte
	d = protowire.AppendTag(d, 1, protowire.VarintType)
	d = protowire.AppendVarint(d, 300)
	b = protowire.AppendTag(b, 2, protowire.BytesType)
	b = protowire.AppendBytes(b, d)
	return b
}

func rpcStatus(code uint64, msg, reason string) []byte {
	var info []byte
	info = protowire.AppendTag(info, 1, protowire.BytesType)
	info = protowire.AppendString(info, reason)
	var anyMsg []byte
	anyMsg = protowire.AppendTag(anyMsg, 1, protowire.BytesType)
	anyMsg = protowire.AppendString(anyMsg, "type.googleapis.com/google.rpc.ErrorInfo")
	anyMsg = protowire.AppendTag(anyMsg, 2, protowire.BytesType)
	anyMsg = protowire.AppendBytes(anyMsg, info)
	var b []byte
	b = protowire.AppendTag(b, 1, protowire.VarintType)
	b = protowire.AppendVarint(b, code)
	b = protowire.AppendTag(b, 2, protowire.BytesType)
	b = protowire.AppendString(b, msg)
	if reason != "" {
		b = protowire.AppendTag(b, 3, protowire.BytesType)
		b = protowire.AppendBytes(b, anyMsg)
	}
	return b
}

// --- tests ---

func TestClientRequestShape(t *testing.T) {
	h := &fakeHTTP{resp: Response{Status: 200, ContentType: protoCT}}
	c := &Client{APIKey: " k \n", HTTP: h}
	v, err := c.Check(context.Background(), "https://example.com/a?b=1&c=2")
	if err != nil || !v.Safe {
		t.Fatalf("empty protobuf body = no threats: v=%+v err=%v", v, err)
	}
	u, _ := url.Parse(h.gotURL)
	if u.Host != "safebrowsing.googleapis.com" || u.Path != "/v5/urls:search" {
		t.Fatalf("wrong endpoint %s", h.gotURL)
	}
	if u.Query().Get("urls") != "https://example.com/a?b=1&c=2" {
		t.Fatalf("url not encoded correctly: %s", h.gotURL)
	}
	if u.Query().Has("alt") {
		t.Fatal("v5 rejects alt=json with 400 Unsupported Output Format")
	}
	if u.Query().Get("key") != "" {
		t.Fatal("api key must not be in the query string")
	}
	if h.gotHdr["X-Goog-Api-Key"] != "k" {
		t.Fatalf("api key header must be trimmed, got %q", h.gotHdr["X-Goog-Api-Key"])
	}
}

func TestClientSafeWithCacheDurationOnly(t *testing.T) {
	h := &fakeHTTP{resp: Response{Status: 200, ContentType: protoCT, Body: searchResponse()}}
	v, err := (&Client{APIKey: "k", HTTP: h}).Check(context.Background(), "https://example.com/")
	if err != nil || !v.Safe || len(v.Threats) != 0 {
		t.Fatalf("v=%+v err=%v", v, err)
	}
}

func TestClientThreats(t *testing.T) {
	cases := map[string]struct {
		body []byte
		want []string
	}{
		"unpacked": {searchResponse(threatURL("https://evil/", []uint64{1, 2}, false)), []string{"MALWARE", "SOCIAL_ENGINEERING"}},
		"packed":   {searchResponse(threatURL("https://evil/", []uint64{3}, true)), []string{"UNWANTED_SOFTWARE"}},
		"dedup":    {searchResponse(threatURL("https://evil/", []uint64{1}, false), threatURL("https://evil/x", []uint64{1, 4}, true)), []string{"MALWARE", "POTENTIALLY_HARMFUL_APPLICATION"}},
		"no types": {searchResponse(threatURL("https://evil/", nil, false)), []string{"THREAT_TYPE_UNSPECIFIED"}},
		"unknown":  {searchResponse(threatURL("https://evil/", []uint64{99}, false)), []string{"THREAT_TYPE_99"}},
	}
	for name, c := range cases {
		h := &fakeHTTP{resp: Response{Status: 200, ContentType: protoCT + "; charset=binary", Body: c.body}}
		v, err := (&Client{APIKey: "k", HTTP: h}).Check(context.Background(), "https://evil/")
		if err != nil || v.Safe || strings.Join(v.Threats, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s: v=%+v err=%v want %v", name, v, err, c.want)
		}
	}
}

func TestClientFailsClosed(t *testing.T) {
	cases := map[string]*Client{
		"no key":       {HTTP: &fakeHTTP{resp: Response{Status: 200, ContentType: protoCT}}},
		"blank key":    {APIKey: "  ", HTTP: &fakeHTTP{resp: Response{Status: 200, ContentType: protoCT}}},
		"no http":      {APIKey: "k"},
		"net error":    {APIKey: "k", HTTP: &fakeHTTP{err: errors.New("timeout")}},
		"403":          {APIKey: "k", HTTP: &fakeHTTP{resp: Response{Status: 403, ContentType: protoCT, Body: rpcStatus(7, "denied", "SERVICE_DISABLED")}}},
		"500 html":     {APIKey: "k", HTTP: &fakeHTTP{resp: Response{Status: 500, ContentType: "text/html", Body: []byte("<html>")}}},
		"200 html":     {APIKey: "k", HTTP: &fakeHTTP{resp: Response{Status: 200, ContentType: "text/html", Body: []byte("<html>")}}},
		"200 json":     {APIKey: "k", HTTP: &fakeHTTP{resp: Response{Status: 200, ContentType: "application/json", Body: []byte(`{}`)}}},
		"200 no ctype": {APIKey: "k", HTTP: &fakeHTTP{resp: Response{Status: 200}}},
		"truncated":    {APIKey: "k", HTTP: &fakeHTTP{resp: Response{Status: 200, ContentType: protoCT, Body: searchResponse(threatURL("https://evil/", []uint64{1}, false))[:5]}}},
		"bad wiretype": {APIKey: "k", HTTP: &fakeHTTP{resp: Response{Status: 200, ContentType: protoCT, Body: protowire.AppendVarint(protowire.AppendTag(nil, 1, protowire.VarintType), 1)}}},
		"too large":    {APIKey: "k", HTTP: &fakeHTTP{resp: Response{Status: 200, ContentType: protoCT, Body: make([]byte, maxResponseBytes+1)}}},
	}
	for name, c := range cases {
		v, err := c.Check(context.Background(), "https://example.com/")
		if !errors.Is(err, ErrUnavailable) || v.Safe {
			t.Errorf("%s: expected ErrUnavailable and not safe, got v=%+v err=%v", name, v, err)
		}
	}
	var nilClient *Client
	if _, err := nilClient.Check(context.Background(), "https://example.com/"); !errors.Is(err, ErrUnavailable) {
		t.Error("nil client must fail closed")
	}
}

func TestReason(t *testing.T) {
	check := func(r Response) error {
		_, err := (&Client{APIKey: "secret-key", HTTP: &fakeHTTP{resp: r}}).Check(context.Background(), "https://example.com/")
		return err
	}
	err := check(Response{Status: 403, ContentType: protoCT, Body: rpcStatus(7, "Safe Browsing API has not been used in project 123", "SERVICE_DISABLED")})
	if got := Reason(err); got != "http_status_403_SERVICE_DISABLED" {
		t.Fatalf("got %q", got)
	}
	if !strings.Contains(err.Error(), "has not been used") || strings.Contains(err.Error(), "secret-key") {
		t.Fatalf("log message should carry Google's message but never the key: %v", err)
	}
	// The exact failure seen in production before this fix.
	err = check(Response{Status: 400, ContentType: protoCT, Body: rpcStatus(3, "Unsupported Output Format", "")})
	if got := Reason(err); got != "http_status_400" || !strings.Contains(err.Error(), "Unsupported Output Format") {
		t.Fatalf("got %q / %v", got, err)
	}
	err = check(Response{Status: 400, ContentType: "application/json", Body: []byte(`{"error":{"status":"INVALID_ARGUMENT","message":"x","details":[{"reason":"API_KEY_INVALID"}]}}`)})
	if got := Reason(err); got != "http_status_400_API_KEY_INVALID" {
		t.Fatalf("json error: got %q", got)
	}
	if r := Reason(check(Response{Status: 500, ContentType: "text/html", Body: []byte("<html>")})); r != "http_status_500" {
		t.Fatalf("got %q", r)
	}
	if r := Reason(check(Response{Status: 200, ContentType: "text/html"})); r != "malformed_response" {
		t.Fatalf("got %q", r)
	}
	_, err = (&Client{HTTP: &fakeHTTP{}}).Check(context.Background(), "https://example.com/")
	if Reason(err) != "api_key_missing" {
		t.Fatal(Reason(err))
	}
	_, err = (&Client{APIKey: "k", HTTP: &fakeHTTP{err: errors.New("dial tcp: secret-key")}}).Check(context.Background(), "https://example.com/")
	if r := Reason(err); r != "request_failed" {
		t.Fatalf("got %q", r)
	}
	if Reason(nil) != "" {
		t.Fatal("nil")
	}
}

// netHTTP adapts net/http for the opt-in live test.
type netHTTP struct{}

func (netHTTP) Get(ctx context.Context, u string, h map[string]string) (Response, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	for k, v := range h {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return Response{Status: resp.StatusCode, ContentType: resp.Header.Get("Content-Type"), Body: b}, err
}

// TestLiveAPI talks to the real API. Opt-in:
//
//	SB_LIVE_TEST=1 go test ./internal/safebrowsing -run TestLiveAPI -v
//
// Without SAFE_BROWSING_API_KEY it only checks that a bogus key is reported
// as API_KEY_INVALID (decoding of the real protobuf error).
func TestLiveAPI(t *testing.T) {
	if os.Getenv("SB_LIVE_TEST") != "1" {
		t.Skip("set SB_LIVE_TEST=1 to call the real Safe Browsing API")
	}
	key := os.Getenv("SAFE_BROWSING_API_KEY")
	if key == "" {
		_, err := (&Client{APIKey: "bogus-key", HTTP: netHTTP{}}).Check(context.Background(), "https://example.com/")
		if r := Reason(err); r != "http_status_400_API_KEY_INVALID" {
			t.Fatalf("got %q (%v)", r, err)
		}
		return
	}
	c := &Client{APIKey: key, HTTP: netHTTP{}}
	v, err := c.Check(context.Background(), "https://example.com/")
	if err != nil || !v.Safe {
		t.Fatalf("example.com: v=%+v err=%v", v, err)
	}
	v, err = c.Check(context.Background(), "http://testsafebrowsing.appspot.com/s/malware.html")
	t.Logf("google test malware url: v=%+v err=%v", v, err)
}
