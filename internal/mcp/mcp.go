// Package mcp implements a minimal, stateless remote MCP server (Streamable
// HTTP, JSON responses only) exposing a single tool, shorten_url.
//
// It follows the stateless 2026-07-28 revision (no initialize handshake;
// protocol version in params._meta and the MCP-Protocol-Version header;
// server/discover) and also answers the older handshake-based revisions
// (initialize / notifications/initialized / ping) that many clients still use.
// No session IDs are issued. Spec: https://modelcontextprotocol.io/specification
package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
)

// LatestVersion is the newest (stateless) protocol revision.
const LatestVersion = "2026-07-28"

// legacyVersions are handshake-based revisions answered via initialize.
var legacyVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26"}

// SupportedVersions lists every revision this server answers.
var SupportedVersions = append([]string{LatestVersion}, legacyVersions...)

const (
	metaVersion  = "io.modelcontextprotocol/protocolVersion"
	metaServer   = "io.modelcontextprotocol/serverInfo"
	toolName     = "shorten_url"
	maxURLLength = 2048
)

// JSON-RPC / MCP error codes.
const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeHeaderMismatch = -32020
	codeUnsupportedVer = -32022
)

// ShortenResult is what the tool returns; either ShortURL or ErrCode is set.
type ShortenResult struct {
	ShortURL, Code, URL string
	Created             bool
	ErrCode, ErrMsg     string
}

// Server is a stateless MCP server.
type Server struct {
	Name, Title, Version, Instructions string
	// AllowedOrigin validates a browser Origin header (DNS-rebinding
	// protection). Requests without Origin (server-side clients) pass.
	AllowedOrigin func(origin string) bool
	// Shorten performs the tool call.
	Shorten func(ctx context.Context, url string) ShortenResult
}

// Request is the transport-independent HTTP request.
type Request struct {
	Method string
	Header func(string) string
	Body   []byte
}

// Response is the HTTP response to send.
type Response struct {
	Status      int
	ContentType string
	Header      map[string]string
	Body        []byte
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// Handle processes one HTTP request to the MCP endpoint.
func (s *Server) Handle(ctx context.Context, req Request) Response {
	if o := req.Header("Origin"); o != "" && (s.AllowedOrigin == nil || !s.AllowedOrigin(o)) {
		return errorResponse(http.StatusForbidden, nil, codeInvalidRequest, "Forbidden origin", nil)
	}
	if req.Method != http.MethodPost {
		// No server-initiated SSE stream and no sessions to delete.
		r := Response{Status: http.StatusMethodNotAllowed, Header: map[string]string{"Allow": "POST"}}
		return r
	}
	if len(req.Body) == 0 {
		return errorResponse(http.StatusBadRequest, nil, codeInvalidRequest, "Empty or oversized body", nil)
	}
	trimmed := strings.TrimSpace(string(req.Body))
	if strings.HasPrefix(trimmed, "[") {
		return errorResponse(http.StatusBadRequest, nil, codeInvalidRequest, "Batch requests are not supported", nil)
	}
	var rr rpcRequest
	if err := json.Unmarshal(req.Body, &rr); err != nil {
		return errorResponse(http.StatusBadRequest, nil, codeParse, "Parse error", nil)
	}
	if rr.JSONRPC != "2.0" || rr.Method == "" {
		return errorResponse(http.StatusBadRequest, rr.ID, codeInvalidRequest, "Invalid request", nil)
	}
	isNotification := len(rr.ID) == 0 || string(rr.ID) == "null"
	if isNotification {
		// e.g. notifications/initialized from handshake-based clients.
		return Response{Status: http.StatusAccepted}
	}

	var params struct {
		Meta            map[string]any  `json:"_meta"`
		ProtocolVersion string          `json:"protocolVersion"` // initialize (legacy)
		Name            string          `json:"name"`
		Arguments       json.RawMessage `json:"arguments"`
	}
	if len(rr.Params) > 0 {
		if err := json.Unmarshal(rr.Params, &params); err != nil {
			return errorResponse(http.StatusBadRequest, rr.ID, codeInvalidParams, "Invalid params", nil)
		}
	}

	// Per-request version (stateless revision): _meta and header must agree.
	metaVer, _ := params.Meta[metaVersion].(string)
	headerVer := req.Header("MCP-Protocol-Version")
	if metaVer != "" && headerVer != "" && metaVer != headerVer {
		return errorResponse(http.StatusBadRequest, rr.ID, codeHeaderMismatch, "MCP-Protocol-Version header does not match _meta", nil)
	}
	version := metaVer
	if version == "" {
		version = headerVer
	}
	if version != "" && !slices.Contains(SupportedVersions, version) && rr.Method != "initialize" {
		return errorResponse(http.StatusBadRequest, rr.ID, codeUnsupportedVer, "Unsupported protocol version",
			map[string]any{"supported": SupportedVersions, "requested": version})
	}
	stateless := version == LatestVersion

	switch rr.Method {
	case "initialize": // handshake-based revisions
		v := params.ProtocolVersion
		if !slices.Contains(legacyVersions, v) {
			v = legacyVersions[0]
		}
		return s.result(rr.ID, false, map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      s.serverInfo(),
			"instructions":    s.Instructions,
		})
	case "ping":
		return s.result(rr.ID, false, map[string]any{})
	case "server/discover":
		return s.result(rr.ID, true, map[string]any{
			"supportedVersions": SupportedVersions,
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"instructions":      s.Instructions,
			"_meta":             map[string]any{metaServer: s.serverInfo()},
		})
	case "tools/list":
		return s.result(rr.ID, stateless, map[string]any{"tools": []any{s.tool()}})
	case "tools/call":
		if params.Name != toolName {
			return errorResponse(http.StatusOK, rr.ID, codeInvalidParams, "Unknown tool: "+params.Name, nil)
		}
		var args struct {
			URL string `json:"url"`
		}
		if len(params.Arguments) > 0 {
			if err := json.Unmarshal(params.Arguments, &args); err != nil {
				return errorResponse(http.StatusOK, rr.ID, codeInvalidParams, "arguments must be an object with a string \"url\"", nil)
			}
		}
		if strings.TrimSpace(args.URL) == "" {
			return s.result(rr.ID, stateless, toolError("missing_url", "The \"url\" argument is required (an http:// or https:// URL)."))
		}
		if len(args.URL) > maxURLLength*3 {
			return s.result(rr.ID, stateless, toolError("too_long", "The URL is too long (max 2048 characters)."))
		}
		r := s.Shorten(ctx, args.URL)
		if r.ShortURL == "" {
			return s.result(rr.ID, stateless, toolError(r.ErrCode, r.ErrMsg))
		}
		structured := map[string]any{"short_url": r.ShortURL, "code": r.Code, "url": r.URL, "created": r.Created}
		js, _ := json.Marshal(structured)
		return s.result(rr.ID, stateless, map[string]any{
			"content": []any{
				map[string]any{"type": "text", "text": r.ShortURL},
				map[string]any{"type": "text", "text": string(js)},
			},
			"structuredContent": structured,
			"isError":           false,
		})
	default:
		status := http.StatusOK
		if stateless {
			status = http.StatusNotFound
		}
		return errorResponse(status, rr.ID, codeMethodNotFound, "Method not found: "+rr.Method, nil)
	}
}

func (s *Server) serverInfo() map[string]any {
	return map[string]any{"name": s.Name, "title": s.Title, "version": s.Version}
}

func (s *Server) tool() map[string]any {
	return map[string]any{
		"name":  toolName,
		"title": "Shorten URL (yk.gy)",
		"description": "Create a short URL on yk.gy for an http(s) URL and return it. " +
			"The same URL always returns the same short URL. URLs flagged by Google Safe Browsing, " +
			"localhost/private addresses and other URL shorteners are refused.",
		"inputSchema": map[string]any{
			"type":                 "object",
			"properties":           map[string]any{"url": map[string]any{"type": "string", "description": "The http:// or https:// URL to shorten", "maxLength": maxURLLength}},
			"required":             []string{"url"},
			"additionalProperties": false,
		},
		"outputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"short_url": map[string]any{"type": "string"},
				"code":      map[string]any{"type": "string"},
				"url":       map[string]any{"type": "string"},
				"created":   map[string]any{"type": "boolean"},
			},
			"required": []string{"short_url", "code", "url", "created"},
		},
		"annotations": map[string]any{
			"readOnlyHint":    false,
			"destructiveHint": false,
			"idempotentHint":  true,
			"openWorldHint":   true,
		},
	}
}

func toolError(code, msg string) map[string]any {
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": "Error (" + code + "): " + msg}},
		"isError": true,
	}
}

func (s *Server) result(id json.RawMessage, stateless bool, result map[string]any) Response {
	if stateless {
		result["resultType"] = "complete"
	}
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	return Response{Status: http.StatusOK, ContentType: "application/json", Body: b}
}

func errorResponse(status int, id json.RawMessage, code int, msg string, data any) Response {
	m := map[string]any{"jsonrpc": "2.0", "error": rpcError{Code: code, Message: msg, Data: data}}
	if len(id) > 0 {
		m["id"] = id
	}
	b, _ := json.Marshal(m)
	return Response{Status: status, ContentType: "application/json", Body: b}
}
