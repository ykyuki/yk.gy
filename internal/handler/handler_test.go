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
	if w := do(e.h, "DELETE", "/api/shorten", "", "", nil); w.Code != 405 {
		t.Errorf("DELETE api: %d", w.Code)
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

func TestGetAPI(t *testing.T) {
	e := newEnv(t, safebrowsing.Mock{})
	e.h.Limiter = &ratelimit.Window{Store: &ratelimit.MemoryStore{}, Policy: ratelimit.Policy{PerMinute: 100, Penalty: 300e9}}
	u := "/api/shorten?url=" + url.QueryEscape("https://www.example.org/a?b=1&c=2")
	w := do(e.h, "GET", u, "", "", nil)
	var res shortenResponse
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if w.Code != 201 || !res.Created || res.URL != "https://www.example.org/a?b=1&c=2" || w.HeaderMap.Get("X-Robots-Tag") == "" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	// Same URL again: 200 with the same code, nothing new stored.
	n := e.store.Len()
	w = do(e.h, "GET", u, "", "", nil)
	var again shortenResponse
	_ = json.Unmarshal(w.Body.Bytes(), &again)
	if w.Code != 200 || again.Created || again.Code != res.Code || e.store.Len() != n {
		t.Fatalf("dedupe: %d %s", w.Code, w.Body.String())
	}
	// Plain text format.
	w = do(e.h, "GET", u+"&format=text", "", "", nil)
	if w.Code != 200 || w.Body.String() != "https://yk.gy/"+res.Code+"\n" || !strings.HasPrefix(w.HeaderMap.Get("Content-Type"), "text/plain") {
		t.Fatalf("text: %d %q", w.Code, w.Body.String())
	}
	w = do(e.h, "GET", "/api/shorten?format=text&url="+url.QueryEscape("javascript:alert(1)"), "", "", nil)
	if w.Code != 400 || !strings.HasPrefix(w.Body.String(), "error: unsupported_scheme") {
		t.Fatalf("text error: %d %q", w.Code, w.Body.String())
	}
	if w := do(e.h, "GET", "/api/shorten", "", "", nil); w.Code != 400 || errCode(t, w) != "missing_url" {
		t.Fatalf("missing url: %d", w.Code)
	}
}

func TestGetAPIRefusesCrossSiteAndHead(t *testing.T) {
	e := newEnv(t, safebrowsing.Mock{})
	e.h.Limiter = &ratelimit.Window{Store: &ratelimit.MemoryStore{}, Policy: ratelimit.Policy{PerMinute: 100, Penalty: 300e9}}
	u := "/api/shorten?url=" + url.QueryEscape("https://www.example.org/x")
	for _, site := range []string{"cross-site", "same-site"} {
		w := do(e.h, "GET", u, "", "", map[string]string{"Sec-Fetch-Site": site, "Sec-Fetch-Dest": "image"})
		if w.Code != 403 || errCode(t, w) != "cross_site_request" {
			t.Fatalf("%s: %d", site, w.Code)
		}
	}
	if w := do(e.h, "HEAD", u, "", "", nil); w.Code != 405 {
		t.Fatalf("HEAD must not create: %d", w.Code)
	}
	if e.store.Len() != 3 {
		t.Fatal("refused requests must not store anything")
	}
	for _, site := range []string{"none", "same-origin"} {
		if w := do(e.h, "GET", u, "", "", map[string]string{"Sec-Fetch-Site": site}); w.Code != 201 && w.Code != 200 {
			t.Fatalf("%s: %d", site, w.Code)
		}
	}
}

func TestGetAPIIsRateLimited(t *testing.T) {
	e := newEnv(t, safebrowsing.Mock{})
	for i := 0; i < 3; i++ {
		do(e.h, "GET", "/api/shorten?url="+url.QueryEscape("https://example.org/"+string(rune('a'+i))), "", "", nil)
	}
	if w := do(e.h, "GET", "/api/shorten?url="+url.QueryEscape("https://example.org/z"), "", "", nil); w.Code != 429 {
		t.Fatalf("expected 429, got %d", w.Code)
	}
}

func TestDocsForAI(t *testing.T) {
	e := newEnv(t, safebrowsing.Mock{})
	w := do(e.h, "GET", "/llms.txt", "", "", nil)
	body := w.Body.String()
	if w.Code != 200 || !strings.HasPrefix(body, "# yk.gy\n") || !strings.Contains(body, "https://yk.gy/api/shorten?url=") || !strings.Contains(w.HeaderMap.Get("Content-Type"), "markdown") {
		t.Fatalf("llms.txt: %d %s", w.Code, body)
	}
	w = do(e.h, "GET", "/openapi.json", "", "", nil)
	var doc map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil || doc["openapi"] != "3.1.0" {
		t.Fatalf("openapi: %v %s", err, w.Body.String())
	}
	if doc["servers"].([]any)[0].(map[string]any)["url"] != "https://yk.gy" {
		t.Fatal("servers must use the environment's base URL")
	}
	get := doc["paths"].(map[string]any)["/api/shorten"].(map[string]any)["get"].(map[string]any)
	if get["operationId"] != "shortenUrl" {
		t.Fatal("operationId")
	}
}

func TestMCPEndToEnd(t *testing.T) {
	e := newEnv(t, safebrowsing.Mock{})
	call := `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"shorten_url","arguments":{"url":"https://www.example.org/mcp"}}}`
	w := do(e.h, "POST", "/mcp", "application/json", call, map[string]string{"Accept": "application/json, text/event-stream"})
	var m struct {
		Result struct {
			IsError           bool              `json:"isError"`
			StructuredContent map[string]any    `json:"structuredContent"`
			Content           []json.RawMessage `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil || w.Code != 200 || m.Result.IsError {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	short, _ := m.Result.StructuredContent["short_url"].(string)
	if !strings.HasPrefix(short, "https://yk.gy/") || m.Result.StructuredContent["created"] != true {
		t.Fatalf("%v", m.Result.StructuredContent)
	}
	// Tool errors flow through as isError results (Safe Browsing mock).
	bad := `{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"shorten_url","arguments":{"url":"http://testsafebrowsing.appspot.com/s/malware.html"}}}`
	w = do(e.h, "POST", "/mcp", "application/json", bad, nil)
	if !strings.Contains(w.Body.String(), `"isError":true`) || !strings.Contains(w.Body.String(), "unsafe_url") {
		t.Fatalf("%s", w.Body.String())
	}
	if w := do(e.h, "GET", "/mcp", "", "", nil); w.Code != 405 {
		t.Fatalf("GET /mcp: %d", w.Code)
	}
	if w := do(e.h, "POST", "/mcp", "application/json", call, map[string]string{"Origin": "https://evil.example"}); w.Code != 403 {
		t.Fatalf("foreign origin: %d", w.Code)
	}
}

func TestNoindexOutsideProduction(t *testing.T) {
	paths := []string{"/", "/llms.txt", "/openapi.json", "/Example", "/Nope123", "/healthz"}
	e := newEnv(t, safebrowsing.Mock{})
	e.h.Assets["robots.txt"] = Asset{ContentType: "text/plain", Body: []byte("User-agent: *")}
	for _, env := range []string{"development", "local", "unknown", ""} {
		e.h.Config.Environment = env
		for _, p := range paths {
			if w := do(e.h, "GET", p, "", "", nil); !strings.Contains(w.HeaderMap.Get("X-Robots-Tag"), "noindex") {
				t.Errorf("env=%q %s: missing noindex", env, p)
			}
		}
	}
	e.h.Config.Environment = "production"
	for _, p := range []string{"/", "/llms.txt", "/openapi.json"} {
		if got := do(e.h, "GET", p, "", "", nil).HeaderMap.Get("X-Robots-Tag"); got != "" {
			t.Errorf("production %s must be indexable, got %q", p, got)
		}
	}
	// The API stays noindex everywhere.
	if got := do(e.h, "GET", "/api/shorten", "", "", nil).HeaderMap.Get("X-Robots-Tag"); !strings.Contains(got, "noindex") {
		t.Errorf("production API must stay noindex, got %q", got)
	}
}

func lastEvent(t *testing.T, e *env, name string) map[string]any {
	t.Helper()
	for i := len(e.logs) - 1; i >= 0; i-- {
		if e.logs[i]["event"] == name {
			return e.logs[i]
		}
	}
	t.Fatalf("no %q event in %v", name, e.logs)
	return nil
}

func TestAccessLogsCarryIPExceptEUAndUK(t *testing.T) {
	cases := []struct {
		country string
		wantIP  any
	}{
		{"JP", "203.0.113.5"},
		{"US", "203.0.113.5"},
		{"CH", "203.0.113.5"},
		{"DE", nil},
		{"FR", nil},
		{"NO", nil}, // EEA
		{"GB", nil}, // UK GDPR
		{"", nil},   // unknown -> treated as EU
	}
	for _, c := range cases {
		e := newEnv(t, safebrowsing.Mock{})
		country := c.country
		e.h.Country = func(string) string { return country }

		do(e.h, "GET", "/", "", "", nil)
		do(e.h, "GET", "/Example", "", "", nil)
		do(e.h, "GET", "/Nope123", "", "", nil)
		post(e.h, `{"url":"https://www.example.org/log"}`)

		for _, name := range []string{"page_view", "redirect", "link_created"} {
			ev := lastEvent(t, e, name)
			if ev["ip"] != c.wantIP {
				t.Errorf("country=%q %s: ip=%v want %v", c.country, name, ev["ip"], c.wantIP)
			}
			wantCountry := any(c.country)
			if c.country == "" {
				wantCountry = nil
			}
			if ev["country"] != wantCountry {
				t.Errorf("country=%q %s: country=%v", c.country, name, ev["country"])
			}
		}
	}
}

func TestPageViewAndRedirectEvents(t *testing.T) {
	e := newEnv(t, safebrowsing.Mock{})
	e.h.Country = func(string) string { return "JP" }
	do(e.h, "GET", "/", "", "", nil)
	if ev := lastEvent(t, e, "page_view"); ev["path"] != "/" || ev["env"] != "production" {
		t.Fatalf("%v", ev)
	}
	do(e.h, "GET", "/Example", "", "", nil)
	ev := lastEvent(t, e, "redirect")
	if ev["code"] != "Example" || ev["status"] != 302 || ev["target_host"] != "example.com" {
		t.Fatalf("302: %v", ev)
	}
	do(e.h, "GET", "/Nope123", "", "", nil)
	if ev := lastEvent(t, e, "redirect"); ev["status"] != 404 || ev["code"] != "Nope123" {
		t.Fatalf("404: %v", ev)
	}
	do(e.h, "GET", "/Disabld", "", "", nil)
	if ev := lastEvent(t, e, "redirect"); ev["status"] != 410 {
		t.Fatalf("410: %v", ev)
	}
	// No events for HEAD on the page or for static assets.
	n := len(e.logs)
	do(e.h, "HEAD", "/", "", "", nil)
	e.h.Assets["style.css"] = Asset{ContentType: "text/css", Body: []byte("x")}
	do(e.h, "GET", "/style.css", "", "", nil)
	if len(e.logs) != n {
		t.Fatalf("unexpected events: %v", e.logs[n:])
	}
}

func TestRateLimitedEventUsesSameIPRule(t *testing.T) {
	for country, want := range map[string]any{"JP": "203.0.113.5", "IE": nil} {
		e := newEnv(t, safebrowsing.Mock{})
		c := country
		e.h.Country = func(string) string { return c }
		for i := 0; i < 4; i++ {
			post(e.h, `{"url":"https://example.org/`+string(rune('a'+i))+`"}`)
		}
		ev := lastEvent(t, e, "rate_limited")
		if ev["ip"] != want || ev["rate_key"] != want {
			t.Errorf("%s: rate_key must follow the IP rule: %v", country, ev)
		}
		if _, ok := ev["client"]; ok {
			t.Error("old client field must be gone")
		}
	}
}

func TestPrivacyPage(t *testing.T) {
	e := newEnv(t, safebrowsing.Mock{})
	e.h.Assets["privacy.html"] = Asset{ContentType: "text/html; charset=utf-8", Body: []byte("<h1>プライバシーポリシー</h1>")}
	for _, p := range []string{"/privacy", "/privacy.html"} {
		w := do(e.h, "GET", p, "", "", nil)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "プライバシーポリシー") || w.HeaderMap.Get("Cache-Control") != "no-cache" {
			t.Fatalf("%s: %d %q", p, w.Code, w.HeaderMap.Get("Cache-Control"))
		}
	}
	// "privacy" is 7 characters: it must not be treated as a short code.
	if w := do(e.h, "GET", "/privacy", "", "", nil); w.HeaderMap.Get("Location") != "" {
		t.Fatal("/privacy must not redirect")
	}
}

func TestShortenLogsCarryEntryPoint(t *testing.T) {
	e := newEnv(t, safebrowsing.Mock{})
	e.h.Limiter = &ratelimit.Window{Store: &ratelimit.MemoryStore{}, Policy: ratelimit.Policy{PerMinute: 100, Penalty: 300e9}}
	e.h.Country = func(string) string { return "JP" }

	post(e.h, `{"url":"https://www.example.org/via"}`)
	if ev := lastEvent(t, e, "link_created"); ev["via"] != "post" {
		t.Fatalf("post: %v", ev)
	}
	do(e.h, "GET", "/api/shorten?url="+url.QueryEscape("https://www.example.org/via"), "", "", nil)
	ev := lastEvent(t, e, "link_reused")
	if ev["via"] != "get" || ev["code"] == nil || ev["ip"] != "203.0.113.5" {
		t.Fatalf("get reuse: %v", ev)
	}
	if _, ok := ev["mcp_client"]; ok {
		t.Fatal("mcp fields only for MCP requests")
	}
	// Page views are not shorten requests: no via.
	do(e.h, "GET", "/", "", "", nil)
	if _, ok := lastEvent(t, e, "page_view")["via"]; ok {
		t.Fatal("page_view must not carry via")
	}
}

func TestMCPLogsClientInfo(t *testing.T) {
	e := newEnv(t, safebrowsing.Mock{})
	e.h.Country = func(string) string { return "US" }
	ua := map[string]string{"User-Agent": "openai-mcp/1.0 (+https://openai.com/bot)"}

	// Legacy initialize: name recorded on mcp_initialize.
	do(e.h, "POST", "/mcp", "application/json", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"openai-mcp","version":"1.0"}}}`, ua)
	ev := lastEvent(t, e, "mcp_initialize")
	if ev["mcp_client"] != "openai-mcp" || ev["mcp_client_version"] != "1.0" || ev["protocol_version"] != "2025-06-18" || ev["via"] != "mcp" || ev["user_agent"] != ua["User-Agent"] {
		t.Fatalf("initialize: %v", ev)
	}

	// Legacy tools/call has no clientInfo: name is null, User-Agent kept.
	do(e.h, "POST", "/mcp", "application/json", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"shorten_url","arguments":{"url":"https://www.example.org/mcp-a"}}}`, ua)
	ev = lastEvent(t, e, "link_created")
	if ev["via"] != "mcp" || ev["mcp_client"] != nil || ev["user_agent"] != ua["User-Agent"] {
		t.Fatalf("legacy call: %v", ev)
	}

	// Stateless revision: clientInfo in _meta; same URL -> link_reused.
	meta := `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientInfo":{"name":"claude-ai","version":"2"}}`
	do(e.h, "POST", "/mcp", "application/json", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"shorten_url","arguments":{"url":"https://www.example.org/mcp-a"},`+meta+`}}`, map[string]string{"MCP-Protocol-Version": "2026-07-28"})
	ev = lastEvent(t, e, "link_reused")
	if ev["via"] != "mcp" || ev["mcp_client"] != "claude-ai" || ev["mcp_client_version"] != "2" || ev["user_agent"] != nil {
		t.Fatalf("stateless call: %v", ev)
	}
}
