package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func newServer() (*Server, *[]string) {
	var calls []string
	s := &Server{
		Name: "yk.gy", Title: "t", Version: "test", Instructions: "use shorten_url",
		AllowedOrigin: func(o string) bool { return o == "https://yk.gy" },
		Shorten: func(_ context.Context, u string, c ClientInfo) ShortenResult {
			calls = append(calls, u+"|"+c.Name)
			if strings.Contains(u, "evil") {
				return ShortenResult{ErrCode: "unsafe_url", ErrMsg: "dangerous"}
			}
			return ShortenResult{ShortURL: "https://yk.gy/AbC1234", Code: "AbC1234", URL: u, Created: true}
		},
	}
	return s, &calls
}

func post(s *Server, body string, hdr map[string]string) (Response, map[string]any) {
	h := func(k string) string { return hdr[k] }
	r := s.Handle(context.Background(), Request{Method: http.MethodPost, Header: h, Body: []byte(body)})
	var m map[string]any
	_ = json.Unmarshal(r.Body, &m)
	return r, m
}

func TestLegacyHandshakeFlow(t *testing.T) {
	s, calls := newServer()
	r, m := post(s, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"c","version":"1"}}}`, nil)
	res := m["result"].(map[string]any)
	if r.Status != 200 || res["protocolVersion"] != "2025-06-18" || res["serverInfo"].(map[string]any)["name"] != "yk.gy" {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
	if _, ok := res["capabilities"].(map[string]any)["tools"]; !ok {
		t.Fatal("tools capability missing")
	}
	if r.Header["Mcp-Session-Id"] != "" {
		t.Fatal("stateless server must not mint sessions")
	}
	// Unknown legacy version -> newest legacy version.
	_, m = post(s, `{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":"2024-01-01"}}`, nil)
	if m["result"].(map[string]any)["protocolVersion"] != "2025-11-25" {
		t.Fatalf("%v", m)
	}
	if r, _ := post(s, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, nil); r.Status != 202 || len(r.Body) != 0 {
		t.Fatalf("notification: %d %q", r.Status, r.Body)
	}
	if _, m := post(s, `{"jsonrpc":"2.0","id":3,"method":"ping"}`, nil); m["result"] == nil {
		t.Fatal("ping")
	}
	_, m = post(s, `{"jsonrpc":"2.0","id":4,"method":"tools/list"}`, map[string]string{"MCP-Protocol-Version": "2025-06-18"})
	tools := m["result"].(map[string]any)["tools"].([]any)
	tool := tools[0].(map[string]any)
	if tool["name"] != "shorten_url" || tool["inputSchema"].(map[string]any)["required"].([]any)[0] != "url" {
		t.Fatalf("%v", tool)
	}
	if _, ok := m["result"].(map[string]any)["resultType"]; ok {
		t.Fatal("legacy responses should not carry resultType")
	}
	_, m = post(s, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"shorten_url","arguments":{"url":"https://example.com/a"}}}`, nil)
	res = m["result"].(map[string]any)
	if res["isError"] != false || res["structuredContent"].(map[string]any)["short_url"] != "https://yk.gy/AbC1234" {
		t.Fatalf("%v", m)
	}
	if res["content"].([]any)[0].(map[string]any)["text"] != "https://yk.gy/AbC1234" {
		t.Fatal("first text block should be the bare short URL")
	}
	if len(*calls) != 1 || (*calls)[0] != "https://example.com/a|" {
		t.Fatalf("calls=%v", *calls)
	}
}

func TestStatelessRevision(t *testing.T) {
	s, _ := newServer()
	meta := `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}`
	hdr := map[string]string{"MCP-Protocol-Version": "2026-07-28", "Mcp-Method": "server/discover"}
	r, m := post(s, `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{`+meta+`}}`, hdr)
	res := m["result"].(map[string]any)
	if r.Status != 200 || res["resultType"] != "complete" || res["supportedVersions"].([]any)[0] != "2026-07-28" {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
	if res["_meta"].(map[string]any)[metaServer].(map[string]any)["name"] != "yk.gy" {
		t.Fatal("serverInfo must be in _meta")
	}
	_, m = post(s, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"shorten_url","arguments":{"url":"https://evil.example/"},`+meta+`}}`, hdr)
	res = m["result"].(map[string]any)
	if res["isError"] != true || res["resultType"] != "complete" || !strings.Contains(res["content"].([]any)[0].(map[string]any)["text"].(string), "unsafe_url") {
		t.Fatalf("tool errors must be isError results: %v", m)
	}
	// Unknown method -> 404 in the stateless revision.
	if r, m := post(s, `{"jsonrpc":"2.0","id":3,"method":"resources/list","params":{`+meta+`}}`, hdr); r.Status != 404 || m["error"].(map[string]any)["code"].(float64) != codeMethodNotFound {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
}

func TestProtocolErrors(t *testing.T) {
	s, calls := newServer()
	cases := []struct {
		name, body string
		hdr        map[string]string
		status     int
		code       float64
	}{
		{"parse", `{not json`, nil, 400, codeParse},
		{"batch", `[{"jsonrpc":"2.0","id":1,"method":"ping"}]`, nil, 400, codeInvalidRequest},
		{"no jsonrpc", `{"id":1,"method":"ping"}`, nil, 400, codeInvalidRequest},
		{"unsupported version", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2099-01-01"}}}`, map[string]string{"MCP-Protocol-Version": "2099-01-01"}, 400, codeUnsupportedVer},
		{"header mismatch", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`, map[string]string{"MCP-Protocol-Version": "2025-06-18"}, 400, codeHeaderMismatch},
		{"unknown tool", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"rm_rf","arguments":{}}}`, nil, 200, codeInvalidParams},
		{"bad arguments", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"shorten_url","arguments":"x"}}`, nil, 200, codeInvalidParams},
	}
	for _, c := range cases {
		r, m := post(s, c.body, c.hdr)
		e, _ := m["error"].(map[string]any)
		if r.Status != c.status || e == nil || e["code"].(float64) != c.code {
			t.Errorf("%s: %d %s", c.name, r.Status, r.Body)
		}
	}
	_, m := post(s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"shorten_url","arguments":{}}}`, nil)
	if m["result"].(map[string]any)["isError"] != true {
		t.Fatal("missing url must be a tool error")
	}
	if len(*calls) != 0 {
		t.Fatal("invalid calls must not reach Shorten")
	}
}

func TestTransportRules(t *testing.T) {
	s, _ := newServer()
	for _, m := range []string{http.MethodGet, http.MethodDelete} {
		if r := s.Handle(context.Background(), Request{Method: m, Header: func(string) string { return "" }}); r.Status != 405 {
			t.Errorf("%s: %d", m, r.Status)
		}
	}
	// DNS-rebinding protection: foreign browser Origin is refused.
	if r, _ := post(s, `{"jsonrpc":"2.0","id":1,"method":"ping"}`, map[string]string{"Origin": "https://evil.example"}); r.Status != 403 {
		t.Fatalf("foreign origin: %d", r.Status)
	}
	if r, _ := post(s, `{"jsonrpc":"2.0","id":1,"method":"ping"}`, map[string]string{"Origin": "https://yk.gy"}); r.Status != 200 {
		t.Fatalf("own origin: %d", r.Status)
	}
	if r := s.Handle(context.Background(), Request{Method: http.MethodPost, Header: func(string) string { return "" }}); r.Status != 400 {
		t.Fatalf("empty body: %d", r.Status)
	}
}

func TestClientInfoIsPassedThrough(t *testing.T) {
	s, calls := newServer()
	var inits []string
	s.OnInitialize = func(c ClientInfo, v string) { inits = append(inits, c.Name+"/"+c.Version+"@"+v) }
	post(s, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"openai-mcp","version":"1.2"}}}`, nil)
	if len(inits) != 1 || inits[0] != "openai-mcp/1.2@2025-06-18" {
		t.Fatalf("%v", inits)
	}
	meta := `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientInfo":{"name":"claude-ai\u0000\n","version":"9"}}`
	post(s, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"shorten_url","arguments":{"url":"https://example.com/b"},`+meta+`}}`, map[string]string{"MCP-Protocol-Version": "2026-07-28"})
	if (*calls)[len(*calls)-1] != "https://example.com/b|claude-ai" {
		t.Fatalf("client from _meta (control chars stripped): %v", *calls)
	}
}
