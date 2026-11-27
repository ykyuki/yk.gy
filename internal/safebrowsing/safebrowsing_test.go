package safebrowsing

import (
	"context"
	"errors"
	"net/url"
	"testing"
)

type fakeHTTP struct {
	status int
	body   string
	err    error
	gotURL string
	gotHdr map[string]string
}

func (f *fakeHTTP) Get(_ context.Context, u string, h map[string]string) (int, []byte, error) {
	f.gotURL, f.gotHdr = u, h
	return f.status, []byte(f.body), f.err
}

func TestClientSafe(t *testing.T) {
	h := &fakeHTTP{status: 200, body: `{"cacheDuration":"300s"}`}
	c := &Client{APIKey: "k", HTTP: h}
	v, err := c.Check(context.Background(), "https://example.com/a?b=1&c=2")
	if err != nil || !v.Safe {
		t.Fatalf("v=%+v err=%v", v, err)
	}
	u, _ := url.Parse(h.gotURL)
	if u.Host != "safebrowsing.googleapis.com" || u.Path != "/v5/urls:search" {
		t.Fatalf("wrong endpoint %s", h.gotURL)
	}
	if u.Query().Get("urls") != "https://example.com/a?b=1&c=2" {
		t.Fatalf("url not encoded correctly: %s", h.gotURL)
	}
	if u.Query().Get("alt") != "json" {
		t.Fatalf("alt=json is required, otherwise the API answers in protobuf: %s", h.gotURL)
	}
	if u.Query().Get("key") != "" {
		t.Fatal("api key must not be in the query string")
	}
	if h.gotHdr["X-Goog-Api-Key"] != "k" {
		t.Fatal("api key header missing")
	}
}

func TestClientThreats(t *testing.T) {
	h := &fakeHTTP{status: 200, body: `{"threats":[{"url":"https://evil/","threatTypes":["MALWARE","SOCIAL_ENGINEERING"]},{"url":"https://evil/x","threatTypes":["MALWARE"]}],"cacheDuration":"300s"}`}
	v, err := (&Client{APIKey: "k", HTTP: h}).Check(context.Background(), "https://evil/")
	if err != nil || v.Safe || len(v.Threats) != 2 {
		t.Fatalf("v=%+v err=%v", v, err)
	}
	h.body = `{"threats":[{"url":"https://evil/"}]}`
	v, _ = (&Client{APIKey: "k", HTTP: h}).Check(context.Background(), "https://evil/")
	if v.Safe {
		t.Fatal("a threat match without types must still be unsafe")
	}
}

func TestClientFailsClosed(t *testing.T) {
	cases := map[string]*Client{
		"no key":      {HTTP: &fakeHTTP{status: 200, body: `{}`}},
		"no http":     {APIKey: "k"},
		"net error":   {APIKey: "k", HTTP: &fakeHTTP{err: errors.New("timeout")}},
		"403":         {APIKey: "k", HTTP: &fakeHTTP{status: 403, body: `{}`}},
		"500":         {APIKey: "k", HTTP: &fakeHTTP{status: 500}},
		"429":         {APIKey: "k", HTTP: &fakeHTTP{status: 429}},
		"bad json":    {APIKey: "k", HTTP: &fakeHTTP{status: 200, body: `<html>`}},
		"empty body":  {APIKey: "k", HTTP: &fakeHTTP{status: 200, body: ``}},
		"wrong shape": {APIKey: "k", HTTP: &fakeHTTP{status: 200, body: `{"threats":"x"}`}},
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

func TestSelectModeMockOnlyLocally(t *testing.T) {
	cases := []struct {
		cfg   string
		local bool
		want  Mode
	}{
		{"mock", true, ModeMock},
		{" MOCK ", true, ModeMock},
		{"live", true, ModeLive},
		{"", true, ModeLive},
		{"mock", false, ModeLive}, // production / development can never use the mock
		{"off", false, ModeLive},
		{"disabled", true, ModeLive},
	}
	for _, c := range cases {
		if got := SelectMode(c.cfg, c.local); got != c.want {
			t.Errorf("SelectMode(%q,%v)=%s want %s", c.cfg, c.local, got, c.want)
		}
	}
	if !IsLocalRuntime("localhost") || IsLocalRuntime("cache-nrt-1234") || IsLocalRuntime("") {
		t.Error("IsLocalRuntime mismatch")
	}
}

func TestMock(t *testing.T) {
	m := Mock{}
	unsafe := []string{
		"http://testsafebrowsing.appspot.com/s/malware.html",
		"http://testsafebrowsing.appspot.com/s/phishing.html",
		"http://testsafebrowsing.appspot.com/s/unwanted.html",
		"https://malware.example.com/",
		"https://x.phishing.example.com/",
	}
	for _, u := range unsafe {
		if v, err := m.Check(context.Background(), u); err != nil || v.Safe {
			t.Errorf("%s should be unsafe: %+v %v", u, v, err)
		}
	}
	if v, err := m.Check(context.Background(), "https://example.com/"); err != nil || !v.Safe {
		t.Errorf("example.com should be safe: %+v %v", v, err)
	}
	if _, err := m.Check(context.Background(), "https://sb-unavailable.example.com/"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("outage host should return ErrUnavailable, got %v", err)
	}
}

func TestClientRejectsProtobufBody(t *testing.T) {
	// What the API returns without alt=json: must fail closed, not pass.
	h := &fakeHTTP{status: 200, body: "\x0a\x05threat"}
	if v, err := (&Client{APIKey: "k", HTTP: h}).Check(context.Background(), "https://example.com/"); !errors.Is(err, ErrUnavailable) || v.Safe {
		t.Fatalf("protobuf body must be ErrUnavailable, got v=%+v err=%v", v, err)
	}
}
