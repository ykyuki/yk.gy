package handler

import (
	"context"
	"encoding/json"
	"io"
	"strconv"
	"strings"

	"github.com/fastly/compute-sdk-go/fsthttp"

	"github.com/ykyuki/yk.gy/internal/mcp"
)

// AI-agent friendly entry points:
//
//   - GET /api/shorten?url=...   one-shot creation that any HTTP tool (ChatGPT
//     browsing, curl, coding agents) can call; same URL -> same short URL
//   - POST /mcp                  remote MCP server with a shorten_url tool
//   - GET /openapi.json          OpenAPI 3.1 for ChatGPT custom GPT Actions
//   - GET /llms.txt              usage instructions for LLMs (llmstxt.org)

// shortenGet handles GET /api/shorten?url=<url>[&format=text].
func (h *Handler) shortenGet(ctx context.Context, w fsthttp.ResponseWriter, r *fsthttp.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	text := r.URL.Query().Get("format") == "text"

	if r.Method == fsthttp.MethodHead {
		// HEAD must never have side effects.
		w.Header().Set("Allow", "GET, POST")
		h.jsonError(w, fsthttp.StatusMethodNotAllowed, "method_not_allowed", "GET または POST を使用してください")
		return
	}
	// A GET that creates data is reachable from <img>/<script>/links on other
	// sites. Browsers label such loads with Sec-Fetch-Site; refuse them so
	// third-party pages cannot make their visitors' browsers create links.
	// AI agents and HTTP clients do not send this header; a user typing the
	// URL into the address bar sends "none", our own page "same-origin".
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "none", "same-origin":
	default:
		h.writeGetOutcome(w, text, outcome{Status: fsthttp.StatusForbidden, ErrCode: "cross_site_request", ErrMsg: "他のサイトから埋め込まれたリクエストは受け付けません"})
		return
	}

	raw := r.URL.Query().Get("url")
	if strings.TrimSpace(raw) == "" {
		h.writeGetOutcome(w, text, outcome{Status: fsthttp.StatusBadRequest, ErrCode: "missing_url",
			ErrMsg: "url パラメータに短縮したいURLを指定してください。例: " + h.baseURL(r) + "/api/shorten?url=https%3A%2F%2Fexample.com%2F"})
		return
	}
	h.writeGetOutcome(w, text, h.shortenURL(ctx, r, raw))
}

func (h *Handler) writeGetOutcome(w fsthttp.ResponseWriter, text bool, o outcome) {
	if !text {
		h.writeOutcome(w, o)
		return
	}
	if o.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(o.RetryAfter))
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(o.Status)
	if o.Resp != nil {
		_, _ = io.WriteString(w, o.Resp.ShortURL+"\n")
		return
	}
	_, _ = io.WriteString(w, "error: "+o.ErrCode+": "+o.ErrMsg+"\n")
}

// mcp serves the remote MCP endpoint.
func (h *Handler) mcp(ctx context.Context, w fsthttp.ResponseWriter, r *fsthttp.Request) {
	var body []byte
	if r.Method == fsthttp.MethodPost {
		b, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
		if err != nil || len(b) > maxBodyBytes {
			b = nil
		}
		body = b
	}
	srv := &mcp.Server{
		Name:    "yk.gy",
		Title:   "yk.gy URL shortener",
		Version: shortCommit(h.Commit),
		Instructions: "Use the shorten_url tool when the user asks to shorten a URL (e.g. \"shorten this with yk.gy\"). " +
			"Return the short_url to the user. The same URL always returns the same short URL. " +
			"Only http/https URLs are accepted; URLs flagged by Google Safe Browsing, private/localhost addresses and other URL shorteners are refused.",
		AllowedOrigin: func(origin string) bool { return h.sameOrigin(origin, r) },
		Shorten: func(ctx context.Context, url string) mcp.ShortenResult {
			o := h.shortenURL(ctx, r, url)
			if o.Resp == nil {
				return mcp.ShortenResult{ErrCode: o.ErrCode, ErrMsg: o.ErrMsg}
			}
			return mcp.ShortenResult{ShortURL: o.Resp.ShortURL, Code: o.Resp.Code, URL: o.Resp.URL, Created: o.Resp.Created}
		},
	}
	resp := srv.Handle(ctx, mcp.Request{
		Method: r.Method,
		Header: func(k string) string { return r.Header.Get(k) },
		Body:   body,
	})
	for k, v := range resp.Header {
		w.Header().Set(k, v)
	}
	w.Header().Set("Cache-Control", "no-store")
	if resp.ContentType != "" {
		w.Header().Set("Content-Type", resp.ContentType)
	}
	w.WriteHeader(resp.Status)
	_, _ = w.Write(resp.Body)
}

func shortCommit(c string) string {
	if len(c) > 12 {
		return c[:12]
	}
	if c == "" {
		return "dev"
	}
	return c
}

// llmsTxt serves /llms.txt (https://llmstxt.org/) with absolute URLs for
// the current environment.
func (h *Handler) llmsTxt(w fsthttp.ResponseWriter, r *fsthttp.Request) {
	base := h.baseURL(r)
	host := strings.TrimPrefix(strings.TrimPrefix(base, "https://"), "http://")
	body := `# ` + host + `

> ` + host + ` is a free URL shortener. Give it a long http(s) URL and it returns a 7-character short URL (` + base + `/AbC1234) that redirects with HTTP 302. No account or API key is needed.

To shorten a URL for a user, make ONE HTTP GET request and read the JSON response:

    GET ` + base + `/api/shorten?url=<percent-encoded URL>

Example: ` + base + `/api/shorten?url=https%3A%2F%2Fexample.com%2Fsome%2Flong%2Fpage

Response (JSON): {"code":"AbC1234","short_url":"` + base + `/AbC1234","url":"https://example.com/some/long/page","created":true}

Give the user the value of "short_url". Add "&format=text" to get only the short URL as plain text.

Rules:
- The "url" value must be percent-encoded (encode "&", "?", "#", "=" inside it).
- Only http:// and https:// URLs are accepted.
- Every new URL is checked with Google Safe Browsing; dangerous URLs are refused (HTTP 422, "unsafe_url").
- Localhost, private IP addresses, this service's own short URLs and other URL shorteners are refused (HTTP 400).
- The same URL always returns the same short URL ("created": false), so retrying is safe.
- Rate limited per client; on HTTP 429 wait for the Retry-After seconds.
- Errors are JSON: {"error":{"code":"...","message":"..."}} (messages are in Japanese).
- This service is in alpha: it may be unstable, and short URLs and data may be deleted without notice.

## API

- [GET /api/shorten](` + base + `/api/shorten?url=https%3A%2F%2Fexample.com%2F): create (or reuse) a short URL; query parameters "url" (required) and "format" ("json" default, or "text")
- [POST /api/shorten](` + base + `/openapi.json): same, with a JSON body {"url":"..."} and Content-Type: application/json
- [OpenAPI 3.1 schema](` + base + `/openapi.json): for ChatGPT custom GPT Actions and other tool frameworks
- [MCP server](` + base + `/mcp): remote MCP (Streamable HTTP, no authentication) with one tool, "shorten_url"; add it as a custom connector in ChatGPT or Claude

## Optional

- [Web UI](` + base + `/): Japanese single-page UI for humans
`
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.WriteHeader(fsthttp.StatusOK)
	if r.Method != fsthttp.MethodHead {
		_, _ = io.WriteString(w, body)
	}
}

// openAPI serves an OpenAPI 3.1 description of the shorten API.
func (h *Handler) openAPI(w fsthttp.ResponseWriter, r *fsthttp.Request) {
	base := h.baseURL(r)
	result := map[string]any{
		"type":     "object",
		"required": []string{"code", "short_url", "url", "created"},
		"properties": map[string]any{
			"code":      map[string]any{"type": "string", "pattern": "^[0-9A-Za-z]{7}$", "description": "7-character short code"},
			"short_url": map[string]any{"type": "string", "format": "uri", "description": "The short URL to give to the user"},
			"url":       map[string]any{"type": "string", "format": "uri", "description": "Normalized destination URL"},
			"created":   map[string]any{"type": "boolean", "description": "false if this URL had already been shortened"},
		},
	}
	errSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{"error": map[string]any{
			"type":       "object",
			"properties": map[string]any{"code": map[string]any{"type": "string"}, "message": map[string]any{"type": "string"}},
		}},
	}
	jsonContent := func(schema any) map[string]any {
		return map[string]any{"application/json": map[string]any{"schema": schema}}
	}
	responses := map[string]any{
		"200": map[string]any{"description": "Existing short URL for this URL", "content": jsonContent(map[string]any{"$ref": "#/components/schemas/ShortenResult"})},
		"201": map[string]any{"description": "New short URL created", "content": jsonContent(map[string]any{"$ref": "#/components/schemas/ShortenResult"})},
		"400": map[string]any{"description": "Invalid or disallowed URL", "content": jsonContent(map[string]any{"$ref": "#/components/schemas/Error"})},
		"422": map[string]any{"description": "Flagged as unsafe by Google Safe Browsing", "content": jsonContent(map[string]any{"$ref": "#/components/schemas/Error"})},
		"429": map[string]any{"description": "Rate limited; retry after the Retry-After header", "content": jsonContent(map[string]any{"$ref": "#/components/schemas/Error"})},
		"503": map[string]any{"description": "Temporarily unavailable (safety check or storage)", "content": jsonContent(map[string]any{"$ref": "#/components/schemas/Error"})},
	}
	doc := map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title":       "yk.gy URL shortener",
			"version":     "1.0.0",
			"description": "Create 7-character short URLs. No authentication. The same URL always returns the same short URL. Dangerous URLs (Google Safe Browsing), private addresses and other shorteners are refused.",
		},
		"servers": []any{map[string]any{"url": base}},
		"paths": map[string]any{
			"/api/shorten": map[string]any{
				"get": map[string]any{
					"operationId": "shortenUrl",
					"summary":     "Shorten a URL",
					"description": "Returns a short URL for the given http(s) URL. Call this when the user asks to shorten a link.",
					"parameters": []any{
						map[string]any{"name": "url", "in": "query", "required": true, "description": "The http(s) URL to shorten", "schema": map[string]any{"type": "string", "maxLength": 2048}},
						map[string]any{"name": "format", "in": "query", "required": false, "description": "Response format", "schema": map[string]any{"type": "string", "enum": []string{"json", "text"}, "default": "json"}},
					},
					"responses": responses,
				},
				"post": map[string]any{
					"operationId": "shortenUrlPost",
					"summary":     "Shorten a URL (JSON body)",
					"requestBody": map[string]any{"required": true, "content": jsonContent(map[string]any{
						"type": "object", "required": []string{"url"}, "additionalProperties": false,
						"properties": map[string]any{"url": map[string]any{"type": "string", "maxLength": 2048}},
					})},
					"responses": responses,
				},
			},
		},
		"components": map[string]any{"schemas": map[string]any{"ShortenResult": result, "Error": errSchema}},
	}
	b, _ := json.MarshalIndent(doc, "", "  ")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.WriteHeader(fsthttp.StatusOK)
	if r.Method != fsthttp.MethodHead {
		_, _ = w.Write(b)
	}
}
