package handler

import (
	"context"
	"encoding/json"
	"io"
	"net/url"
	"strings"
	"testing"

	"github.com/fastly/compute-sdk-go/fsthttp"
	"github.com/fastly/compute-sdk-go/fsttest"

	"github.com/ykyuki/yk.gy/internal/config"
	"github.com/ykyuki/yk.gy/internal/ratelimit"
	"github.com/ykyuki/yk.gy/internal/safebrowsing"
	"github.com/ykyuki/yk.gy/internal/shortener"
	"github.com/ykyuki/yk.gy/internal/storage"
	"github.com/ykyuki/yk.gy/internal/validation"
)

type env struct {
	h     *Handler
	store *storage.MemoryStore
	logs  []map[string]any
}

func newEnv(t *testing.T, checker safebrowsing.Checker) *env {
	t.Helper()
	e := &env{store: storage.NewMemoryStore()}
	_ = e.store.Create(context.Background(), "Example", storage.Link{URL: "https://example.com/"})
	_ = e.store.Create(context.Background(), "Disabld", storage.Link{URL: "https://example.com/x", Disabled: true})
	_ = e.store.Create(context.Background(), "Tampers", storage.Link{URL: "javascript:alert(1)"})
	cfg := config.Config{Environment: "production", BaseURL: "https://yk.gy", ShortHosts: []string{"yk.gy", "dev.yk.gy"}, RatePerMinute: 3, RatePenalty: 300e9}
	e.h = &Handler{
		Config:  cfg,
		Service: &shortener.Service{Store: e.store, Checker: checker, Validation: validation.Options{ShortHosts: cfg.ShortHosts, BlockedHosts: []string{"bit.ly"}}},
		Limiter: &ratelimit.Window{Store: &ratelimit.MemoryStore{}, Policy: ratelimit.Policy{PerMinute: 3, Penalty: 300e9}},
		Log:     func(ev map[string]any) { e.logs = append(e.logs, ev) },
		Commit:  "abc123", ServiceVersion: "7",
		Assets: map[string]Asset{"index.html": {ContentType: "text/html; charset=utf-8", Body: []byte("<h1>hi</h1>")}},
	}
	return e
}

func do(h *Handler, method, target, ctype, body string, hdr map[string]string) *fsttest.ResponseRecorder {
	u, _ := url.Parse("https://yk.gy" + target)
	r := &fsthttp.Request{Method: method, URL: u, Header: fsthttp.NewHeader(), Host: "yk.gy", RemoteAddr: "203.0.113.5", Body: io.NopCloser(strings.NewReader(body))}
	if ctype != "" {
		r.Header.Set("Content-Type", ctype)
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := fsttest.NewRecorder()
	h.ServeHTTP(context.Background(), w, r)
	return w
}

func post(h *Handler, body string) *fsttest.ResponseRecorder {
	return do(h, "POST", "/api/shorten", "application/json", body, map[string]string{"Origin": "https://yk.gy"})
}

func errCode(t *testing.T, w *fsttest.ResponseRecorder) string {
	t.Helper()
	var e struct{ Error struct{ Code string } }
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
		t.Fatalf("bad json %q: %v", w.Body.String(), err)
	}
	return e.Error.Code
}

func TestRedirect302(t *testing.T) {
	e := newEnv(t, safebrowsing.Mock{})
	w := do(e.h, "GET", "/Example", "", "", nil)
	if w.Code != 302 || w.HeaderMap.Get("Location") != "https://example.com/" {
		t.Fatalf("code=%d loc=%q", w.Code, w.HeaderMap.Get("Location"))
	}
	if w := do(e.h, "HEAD", "/Example", "", "", nil); w.Code != 302 {
		t.Fatalf("HEAD: %d", w.Code)
	}
}

func TestRedirectEdgeCases(t *testing.T) {
	e := newEnv(t, safebrowsing.Mock{})
	cases := map[string]int{"/Nope123": 404, "/Disabld": 410, "/Tampers": 410, "/toolongcode": 404, "/../etc/passwd": 404}
	for p, want := range cases {
		if w := do(e.h, "GET", p, "", "", nil); w.Code != want {
			t.Errorf("%s: got %d want %d", p, w.Code, want)
		}
	}
	if w := do(e.h, "DELETE", "/Example", "", "", nil); w.Code != 405 {
		t.Errorf("DELETE should be 405, got %d", w.Code)
	}
}

func TestShortenSuccess(t *testing.T) {
	e := newEnv(t, safebrowsing.Mock{})
	w := post(e.h, `{"url":"https://www.example.org/path"}`)
	if w.Code != 201 {
		t.Fatalf("code %d body %s", w.Code, w.Body.String())
	}
	var res shortenResponse
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if !shortener.IsValidCode(res.Code) || res.ShortURL != "https://yk.gy/"+res.Code || res.URL != "https://www.example.org/path" {
		t.Fatalf("%+v", res)
	}
	if w := do(e.h, "GET", "/"+res.Code, "", "", nil); w.Code != 302 || w.HeaderMap.Get("Location") != res.URL {
		t.Fatalf("redirect of new code failed: %d", w.Code)
	}
}

func TestShortenErrors(t *testing.T) {
	cases := []struct {
		name, body string
		status     int
		code       string
	}{
		{"malware", `{"url":"http://testsafebrowsing.appspot.com/s/malware.html"}`, 422, "unsafe_url"},
		{"sb outage", `{"url":"https://sb-unavailable.example.com/"}`, 503, "safety_check_unavailable"},
		{"scheme", `{"url":"javascript:alert(1)"}`, 400, "unsupported_scheme"},
		{"private", `{"url":"http://10.1.2.3/"}`, 400, "private_host"},
		{"recursive", `{"url":"https://yk.gy/Example"}`, 400, "recursive"},
		{"blocked", `{"url":"https://bit.ly/x"}`, 400, "blocked_host"},
		{"bad json", `{"url":`, 400, "bad_json"},
		{"unknown field", `{"url":"https://a.com","x":1}`, 400, "bad_json"},
		{"empty", `{"url":""}`, 400, "empty"},
	}
	for _, c := range cases {
		e := newEnv(t, safebrowsing.Mock{})
		w := post(e.h, c.body)
		if w.Code != c.status || errCode(t, w) != c.code {
			t.Errorf("%s: got %d %s want %d %s", c.name, w.Code, errCode(t, w), c.status, c.code)
		}
		if e.store.Len() != 3 {
			t.Errorf("%s: nothing must be stored", c.name)
		}
	}
}

func TestShortenRejectsCrossSite(t *testing.T) {
	e := newEnv(t, safebrowsing.Mock{})
	if w := do(e.h, "POST", "/api/shorten", "application/x-www-form-urlencoded", "url=https://a.com", nil); w.Code != 415 {
		t.Errorf("form post: %d", w.Code)
	}
	if w := do(e.h, "POST", "/api/shorten", "application/json", `{"url":"https://a.com"}`, map[string]string{"Origin": "https://evil.example"}); w.Code != 403 {
		t.Errorf("foreign origin: %d", w.Code)
	}
	if w := do(e.h, "GET", "/api/shorten", "", "", nil); w.Code != 405 {
		t.Errorf("GET api: %d", w.Code)
	}
	big := `{"url":"https://a.com/` + strings.Repeat("a", 9000) + `"}`
	if w := post(e.h, big); w.Code != 413 {
		t.Errorf("big body: %d", w.Code)
	}
}

func TestRateLimit(t *testing.T) {
	e := newEnv(t, safebrowsing.Mock{})
	for i := 0; i < 3; i++ {
		if w := post(e.h, `{"url":"https://example.org/`+string(rune('a'+i))+`"}`); w.Code != 201 {
			t.Fatalf("request %d: %d", i, w.Code)
		}
	}
	w := post(e.h, `{"url":"https://example.org/z"}`)
	if w.Code != 429 || w.HeaderMap.Get("Retry-After") != "300" {
		t.Fatalf("expected 429, got %d", w.Code)
	}
}

type brokenLimiter struct{}

func (brokenLimiter) Allow(string) (bool, error) { return false, ratelimit.ErrUnavailable }

func TestRateLimiterErrorFailsClosedByDefault(t *testing.T) {
	e := newEnv(t, safebrowsing.Mock{})
	e.h.Limiter = brokenLimiter{}
	if w := post(e.h, `{"url":"https://example.org/"}`); w.Code != 503 {
		t.Fatalf("expected 503, got %d", w.Code)
	}
	e.h.Config.RateLimitFailOpen = true
	if w := post(e.h, `{"url":"https://example.org/"}`); w.Code != 201 {
		t.Fatalf("fail-open config: expected 201, got %d", w.Code)
	}
}

func TestSecurityHeadersAndIndex(t *testing.T) {
	e := newEnv(t, safebrowsing.Mock{})
	w := do(e.h, "GET", "/", "", "", nil)
	if w.Code != 200 || w.Body.String() != "<h1>hi</h1>" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	for _, h := range []string{"Content-Security-Policy", "X-Content-Type-Options", "X-Frame-Options", "Referrer-Policy", "Strict-Transport-Security"} {
		if w.HeaderMap.Get(h) == "" {
			t.Errorf("missing %s", h)
		}
	}
	if !strings.Contains(w.HeaderMap.Get("Content-Security-Policy"), "script-src 'self'") {
		t.Error("CSP must restrict scripts")
	}
}

func TestErrorPagesEscapeHTML(t *testing.T) {
	e := newEnv(t, safebrowsing.Mock{})
	w := do(e.h, "GET", "/<script>", "", "", nil)
	if strings.Contains(w.Body.String(), "<script>") {
		t.Fatal("path reflected unescaped")
	}
	// JSON responses escape HTML metacharacters.
	e.h.Service.Checker = safebrowsing.Mock{}
	w = post(e.h, `{"url":"https://example.org/<script>"}`)
	if strings.Contains(w.Body.String(), "<script>") {
		t.Fatalf("json must escape <: %s", w.Body.String())
	}
}

func TestHealth(t *testing.T) {
	e := newEnv(t, safebrowsing.Mock{})
	e.h.Health = func() map[string]error { return map[string]error{"kv_store": nil} }
	w := do(e.h, "GET", "/healthz", "", "", nil)
	var hr healthResponse
	_ = json.Unmarshal(w.Body.Bytes(), &hr)
	if w.Code != 200 || hr.Status != "ok" || hr.Commit != "abc123" || hr.ServiceVersion != "7" || hr.SafeBrowsing != "live" {
		t.Fatalf("%d %+v", w.Code, hr)
	}
	e.h.Health = func() map[string]error { return map[string]error{"kv_store": storage.ErrUnavailable} }
	if w := do(e.h, "GET", "/healthz", "", "", nil); w.Code != 503 {
		t.Fatalf("degraded health should be 503, got %d", w.Code)
	}
}

func TestMockNeverReportedOutsideLocal(t *testing.T) {
	e := newEnv(t, safebrowsing.Mock{})
	e.h.Config.SafeBrowsingMode = "mock"
	w := do(e.h, "GET", "/healthz", "", "", nil)
	if !strings.Contains(w.Body.String(), `"safebrowsing_mode":"live"`) {
		t.Fatalf("non-local runtime must report live: %s", w.Body.String())
	}
}

type failingChecker struct{}

func (failingChecker) Check(context.Context, string) (safebrowsing.Verdict, error) {
	return safebrowsing.Verdict{}, safebrowsing.ErrUnavailable
}

func TestDeepHealthChecksSafeBrowsingWithoutWriting(t *testing.T) {
	e := newEnv(t, safebrowsing.Mock{})
	w := do(e.h, "GET", "/healthz?deep=1", "", "", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"safebrowsing":"ok"`) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	e.h.Service.Checker = failingChecker{}
	if w := do(e.h, "GET", "/healthz?deep=1", "", "", nil); w.Code != 503 {
		t.Fatalf("expected 503 when Safe Browsing is down, got %d", w.Code)
	}
	if e.store.Len() != 3 {
		t.Fatal("health checks must never write")
	}
}
