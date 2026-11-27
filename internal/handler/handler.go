// Package handler implements the HTTP API, static page and redirects.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"html"
	"io"
	"mime"
	"net/url"
	"strconv"
	"strings"

	"github.com/fastly/compute-sdk-go/fsthttp"

	"github.com/ykyuki/yk.gy/internal/config"
	"github.com/ykyuki/yk.gy/internal/privacy"
	"github.com/ykyuki/yk.gy/internal/ratelimit"
	"github.com/ykyuki/yk.gy/internal/safebrowsing"
	"github.com/ykyuki/yk.gy/internal/shortener"
	"github.com/ykyuki/yk.gy/internal/storage"
	"github.com/ykyuki/yk.gy/internal/validation"
)

// maxBodyBytes bounds the size of an API request body.
const maxBodyBytes = 8 << 10

// Asset is an embedded static file.
type Asset struct {
	ContentType string
	Body        []byte
}

// Logger receives structured events. Implementations must not block.
type Logger func(event map[string]any)

// Handler serves every request.
type Handler struct {
	Config  config.Config
	Service *shortener.Service
	Limiter ratelimit.Limiter
	// Health returns the status of each dependency (nil error = ok).
	Health func() map[string]error
	Log    Logger
	// Commit / ServiceVersion identify the running release.
	Commit         string
	ServiceVersion string
	// LocalRuntime is true only on the local development server.
	LocalRuntime bool
	Assets       map[string]Asset
	// Country returns the ISO 3166-1 alpha-2 country of an IP address ("" if
	// unknown). On Fastly it uses the built-in geolocation.
	Country func(ip string) string

	// Per-request client info for logging (the handler is built per request).
	clientIP      string
	clientCountry string
	countryLooked bool
}

// ServeHTTP implements fsthttp.Handler.
func (h *Handler) ServeHTTP(ctx context.Context, w fsthttp.ResponseWriter, r *fsthttp.Request) {
	h.clientIP, h.clientCountry, h.countryLooked = privacy.ClientIP(r.RemoteAddr), "", false
	h.securityHeaders(w)
	path := r.URL.Path

	switch {
	case path == "/api/shorten":
		switch r.Method {
		case fsthttp.MethodPost:
			h.shorten(ctx, w, r)
		case fsthttp.MethodGet, fsthttp.MethodHead:
			h.shortenGet(ctx, w, r)
		default:
			w.Header().Set("Allow", "GET, POST")
			h.jsonError(w, fsthttp.StatusMethodNotAllowed, "method_not_allowed", "GET または POST を使用してください")
		}
		return
	case path == "/mcp":
		h.mcp(ctx, w, r)
		return
	case path == "/healthz":
		h.health(ctx, w, r)
		return
	}

	if r.Method != fsthttp.MethodGet && r.Method != fsthttp.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		h.textError(w, fsthttp.StatusMethodNotAllowed, "Method Not Allowed")
		return
	}

	switch path {
	case "/", "/index.html":
		if r.Method == fsthttp.MethodGet {
			h.log(map[string]any{"event": "page_view", "path": path})
		}
		h.asset(w, r, "index.html")
		return
	case "/privacy", "/privacy.html":
		h.asset(w, r, "privacy.html")
		return
	case "/style.css", "/app.js", "/favicon.svg", "/robots.txt":
		h.asset(w, r, strings.TrimPrefix(path, "/"))
		return
	case "/llms.txt":
		h.llmsTxt(w, r)
		return
	case "/openapi.json", "/.well-known/openapi.json":
		h.openAPI(w, r)
		return
	case "/favicon.ico":
		w.WriteHeader(fsthttp.StatusNoContent)
		return
	}

	code := strings.TrimPrefix(path, "/")
	if shortener.IsValidCode(code) {
		h.redirect(ctx, w, r, code)
		return
	}
	h.notFound(w, r)
}

func (h *Handler) securityHeaders(w fsthttp.ResponseWriter) {
	hd := w.Header()
	hd.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("X-Frame-Options", "DENY")
	hd.Set("Referrer-Policy", "no-referrer")
	hd.Set("Cross-Origin-Opener-Policy", "same-origin")
	hd.Set("Cross-Origin-Resource-Policy", "same-origin")
	hd.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), interest-cohort=()")
	if h.Config.Environment == "production" || h.Config.Environment == "development" {
		hd.Set("Strict-Transport-Security", "max-age=31536000")
	}
	if h.Config.Environment != "" {
		hd.Set("X-Environment", h.Config.Environment)
	}
	// Only production may be indexed by search engines. Development and
	// local copies are noindex on every response; robots.txt keeps allowing
	// crawlers to fetch them so they can actually see this header.
	if h.Config.Environment != "production" {
		hd.Set("X-Robots-Tag", "noindex, nofollow")
	}
}

func (h *Handler) asset(w fsthttp.ResponseWriter, r *fsthttp.Request, name string) {
	a, ok := h.Assets[name]
	if !ok {
		h.notFound(w, r)
		return
	}
	w.Header().Set("Content-Type", a.ContentType)
	if strings.HasSuffix(name, ".html") {
		w.Header().Set("Cache-Control", "no-cache")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=300")
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(a.Body)))
	w.WriteHeader(fsthttp.StatusOK)
	if r.Method != fsthttp.MethodHead {
		_, _ = w.Write(a.Body)
	}
}

type shortenRequest struct {
	URL string `json:"url"`
}

type shortenResponse struct {
	Code     string `json:"code"`
	ShortURL string `json:"short_url"`
	URL      string `json:"url"`
	// Created is false when the URL had already been shortened and the
	// existing short URL is returned.
	Created bool `json:"created"`
}

// outcome is the transport-independent result of a shorten request, shared
// by the HTML form API (POST), the GET API and the MCP tool.
type outcome struct {
	Status     int
	Resp       *shortenResponse
	ErrCode    string
	ErrMsg     string
	RetryAfter int
}

// shortenURL applies the rate limit, then validates, checks and stores
// rawURL. Every entry point goes through here.
func (h *Handler) shortenURL(ctx context.Context, r *fsthttp.Request, rawURL string) outcome {
	key := ratelimit.ClientKey(r.RemoteAddr)
	if h.Limiter != nil {
		ok, err := h.Limiter.Allow(key)
		if err != nil {
			h.log(map[string]any{"event": "ratelimit_error", "error": err.Error()})
			if !h.Config.RateLimitFailOpen {
				return outcome{Status: fsthttp.StatusServiceUnavailable, ErrCode: "ratelimit_unavailable", ErrMsg: "現在URLを発行できません。しばらくしてから再度お試しください"}
			}
		} else if !ok {
			h.log(map[string]any{"event": "rate_limited", "rate_key": key})
			return outcome{Status: fsthttp.StatusTooManyRequests, ErrCode: "rate_limited", ErrMsg: "リクエストが多すぎます。しばらく待ってから再度お試しください", RetryAfter: int(h.Config.RatePenalty.Seconds())}
		}
	}

	svc := *h.Service
	svc.Validation.ShortHosts = append(append([]string{}, svc.Validation.ShortHosts...), hostOnly(r.Host))
	res, err := svc.Shorten(ctx, rawURL)
	if err != nil {
		status, code, msg := h.classifyShortenError(err)
		return outcome{Status: status, ErrCode: code, ErrMsg: msg}
	}
	short := h.baseURL(r) + "/" + res.Code
	status := fsthttp.StatusCreated
	if res.Existing {
		status = fsthttp.StatusOK
	} else {
		h.log(map[string]any{"event": "link_created", "code": res.Code, "target_host": hostOf(res.URL)})
	}
	return outcome{Status: status, Resp: &shortenResponse{Code: res.Code, ShortURL: short, URL: res.URL, Created: !res.Existing}}
}

func (h *Handler) writeOutcome(w fsthttp.ResponseWriter, o outcome) {
	if o.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(o.RetryAfter))
	}
	if o.Resp != nil {
		h.writeJSON(w, o.Status, o.Resp)
		return
	}
	h.jsonError(w, o.Status, o.ErrCode, o.ErrMsg)
}

func (h *Handler) shorten(ctx context.Context, w fsthttp.ResponseWriter, r *fsthttp.Request) {
	w.Header().Set("Cache-Control", "no-store")

	// CSRF / cross-site abuse: require JSON (forces a CORS preflight from
	// other origins, which we never answer) and, when present, a same-origin
	// Origin header.
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mt != "application/json" {
		h.jsonError(w, fsthttp.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type は application/json を指定してください")
		return
	}
	if o := r.Header.Get("Origin"); o != "" && !h.sameOrigin(o, r) {
		h.jsonError(w, fsthttp.StatusForbidden, "forbidden_origin", "許可されていないオリジンからのリクエストです")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		h.jsonError(w, fsthttp.StatusBadRequest, "bad_request", "リクエストを読み取れませんでした")
		return
	}
	if len(body) > maxBodyBytes {
		h.jsonError(w, fsthttp.StatusRequestEntityTooLarge, "too_large", "リクエストが大きすぎます")
		return
	}
	var req shortenRequest
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		h.jsonError(w, fsthttp.StatusBadRequest, "bad_json", "JSONの形式が正しくありません")
		return
	}

	h.writeOutcome(w, h.shortenURL(ctx, r, req.URL))
}

func (h *Handler) classifyShortenError(err error) (status int, code, msg string) {
	if ve, ok := validation.AsError(err); ok {
		return fsthttp.StatusBadRequest, ve.Code, ve.Msg
	}
	var ue *shortener.UnsafeError
	switch {
	case errors.As(err, &ue):
		h.log(map[string]any{"event": "unsafe_rejected", "threats": ue.Threats})
		return fsthttp.StatusUnprocessableEntity, "unsafe_url", "このURLはGoogle Safe Browsingにより危険なサイトと判定されたため登録できません"
	case errors.Is(err, shortener.ErrDisabled):
		return fsthttp.StatusGone, "disabled_url", "このURLは運営者により無効化されているため登録できません"
	case errors.Is(err, shortener.ErrCheckUnavailable):
		h.log(map[string]any{"event": "safebrowsing_unavailable", "error": err.Error()})
		return fsthttp.StatusServiceUnavailable, "safety_check_unavailable", "安全性の確認ができないため、現在URLを登録できません。しばらくしてから再度お試しください"
	default:
		h.log(map[string]any{"event": "shorten_failed", "error": err.Error()})
		return fsthttp.StatusServiceUnavailable, "storage_unavailable", "URLを保存できませんでした。しばらくしてから再度お試しください"
	}
}

func (h *Handler) redirect(ctx context.Context, w fsthttp.ResponseWriter, r *fsthttp.Request, code string) {
	link, err := h.Service.Resolve(ctx, code)
	switch {
	case errors.Is(err, storage.ErrNotFound):
		h.log(map[string]any{"event": "redirect", "code": code, "status": fsthttp.StatusNotFound})
		h.notFound(w, r)
		return
	case err != nil:
		h.log(map[string]any{"event": "resolve_failed", "code": code, "error": err.Error()})
		h.textError(w, fsthttp.StatusServiceUnavailable, "一時的にご利用いただけません")
		return
	case link.Disabled:
		h.log(map[string]any{"event": "redirect", "code": code, "status": fsthttp.StatusGone})
		h.textError(w, fsthttp.StatusGone, "このリンクは無効化されています")
		return
	}
	// Defence in depth: never emit a redirect to a non-http(s) target even
	// if the stored record was tampered with.
	if u, perr := url.Parse(link.URL); perr != nil || (u.Scheme != "http" && u.Scheme != "https") {
		h.log(map[string]any{"event": "redirect", "code": code, "status": fsthttp.StatusGone, "reason": "invalid_target"})
		h.textError(w, fsthttp.StatusGone, "このリンクは無効です")
		return
	}
	h.log(map[string]any{"event": "redirect", "code": code, "status": fsthttp.StatusFound, "target_host": hostOf(link.URL)})
	w.Header().Set("Location", link.URL)
	w.Header().Set("Cache-Control", "private, max-age=90")
	w.WriteHeader(fsthttp.StatusFound)
}

type healthResponse struct {
	Status         string            `json:"status"`
	Environment    string            `json:"environment"`
	Commit         string            `json:"commit"`
	ServiceVersion string            `json:"service_version"`
	SafeBrowsing   string            `json:"safebrowsing_mode"`
	Checks         map[string]string `json:"checks"`
}

// deepCheckURL is a known-good URL used by /healthz?deep=1 to verify, without
// writing anything, that the Safe Browsing API is reachable and the API key
// in the Secret Store is valid.
const deepCheckURL = "https://example.com/"

func (h *Handler) health(ctx context.Context, w fsthttp.ResponseWriter, r *fsthttp.Request) {
	w.Header().Set("Cache-Control", "no-store")
	resp := healthResponse{Status: "ok", Environment: h.Config.Environment, Commit: h.Commit, ServiceVersion: h.ServiceVersion, Checks: map[string]string{}}
	resp.SafeBrowsing = string(safebrowsing.SelectMode(h.Config.SafeBrowsingMode, h.LocalRuntime))
	status := fsthttp.StatusOK
	if h.Health != nil {
		for name, err := range h.Health() {
			if err != nil {
				resp.Checks[name] = "error"
				resp.Status = "degraded"
				status = fsthttp.StatusServiceUnavailable
			} else {
				resp.Checks[name] = "ok"
			}
		}
	}
	if r.URL.Query().Get("deep") == "1" {
		// Deep checks call an external API, so they share the rate limit.
		if h.Limiter != nil {
			if ok, err := h.Limiter.Allow("health:" + ratelimit.ClientKey(r.RemoteAddr)); err != nil || !ok {
				h.jsonError(w, fsthttp.StatusTooManyRequests, "rate_limited", "too many deep health checks")
				return
			}
		}
		if h.Service == nil || h.Service.Checker == nil {
			resp.Checks["safebrowsing"] = "error"
		} else if v, err := h.Service.Checker.Check(ctx, deepCheckURL); err != nil || !v.Safe {
			resp.Checks["safebrowsing"] = "error"
			if err != nil {
				// Non-sensitive classification only (no key, no URL).
				resp.Checks["safebrowsing"] = "error: " + safebrowsing.Reason(err)
				h.log(map[string]any{"event": "deep_health_safebrowsing_error", "error": err.Error()})
			}
		} else {
			resp.Checks["safebrowsing"] = "ok"
		}
		if strings.HasPrefix(resp.Checks["safebrowsing"], "error") {
			resp.Status = "degraded"
			status = fsthttp.StatusServiceUnavailable
		}
	}
	h.writeJSON(w, status, resp)
}

func (h *Handler) notFound(w fsthttp.ResponseWriter, r *fsthttp.Request) {
	h.textError(w, fsthttp.StatusNotFound, "指定された短縮URLは存在しません")
}

func (h *Handler) textError(w fsthttp.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if w.Header().Get("Cache-Control") == "" {
		w.Header().Set("Cache-Control", "no-store")
	}
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `<!doctype html><html lang="ja"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>`+
		strconv.Itoa(status)+`</title><link rel="stylesheet" href="/style.css"><main class="container"><h1>`+
		strconv.Itoa(status)+`</h1><p>`+html.EscapeString(msg)+`</p><p><a href="/">トップへ戻る</a></p></main></html>`)
}

type apiError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (h *Handler) jsonError(w fsthttp.ResponseWriter, status int, code, msg string) {
	var e apiError
	e.Error.Code, e.Error.Message = code, msg
	h.writeJSON(w, status, e)
}

func (h *Handler) writeJSON(w fsthttp.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v) // encoding/json escapes <, >, & — safe against HTML injection
	if err != nil {
		status, b = fsthttp.StatusInternalServerError, []byte(`{"error":{"code":"internal","message":"internal error"}}`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if w.Header().Get("Cache-Control") == "" {
		w.Header().Set("Cache-Control", "no-store")
	}
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// log emits one structured event. Every event carries the client's full IP
// address and country, except that the IP is null for visitors from the
// EU/EEA/UK (and when the country is unknown), see internal/privacy.
func (h *Handler) log(ev map[string]any) {
	if h.Log == nil {
		return
	}
	ev["env"] = h.Config.Environment
	ev["service_version"] = h.ServiceVersion
	country := h.country()
	if country != "" {
		ev["country"] = country
	} else {
		ev["country"] = nil
	}
	if h.clientIP != "" && privacy.IPAllowed(country) {
		ev["ip"] = h.clientIP
	} else {
		ev["ip"] = nil
		// Other fields derived from the IP must not leak it either.
		if _, ok := ev["rate_key"]; ok {
			ev["rate_key"] = nil
		}
	}
	h.Log(ev)
}

func (h *Handler) country() string {
	if !h.countryLooked {
		h.countryLooked = true
		if h.Country != nil && h.clientIP != "" {
			h.clientCountry = strings.ToUpper(strings.TrimSpace(h.Country(h.clientIP)))
		}
	}
	return h.clientCountry
}

func (h *Handler) baseURL(r *fsthttp.Request) string {
	if h.Config.BaseURL != "" {
		return h.Config.BaseURL
	}
	scheme := "https"
	if h.LocalRuntime {
		scheme = "http"
	}
	return scheme + "://" + r.Host
}

func (h *Handler) sameOrigin(origin string, r *fsthttp.Request) bool {
	o, err := url.Parse(origin)
	if err != nil || o.Host == "" {
		return false
	}
	if strings.EqualFold(o.Host, r.Host) {
		return true
	}
	if b, err := url.Parse(h.Config.BaseURL); err == nil && b.Host != "" && strings.EqualFold(o.Host, b.Host) {
		return true
	}
	return false
}

func hostOnly(hostport string) string {
	if i := strings.LastIndex(hostport, ":"); i > 0 && !strings.Contains(hostport[i:], "]") {
		return strings.ToLower(hostport[:i])
	}
	return strings.ToLower(hostport)
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}
