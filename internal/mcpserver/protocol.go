package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"reflect"
	"strings"

	"github.com/AlphaTechini/newgit/internal/auth"
	"github.com/AlphaTechini/newgit/internal/store"
)

// This file implements the subset of the Model Context Protocol that a tool
// server needs, over the Streamable HTTP transport: JSON-RPC 2.0 requests are
// POSTed to /mcp and answered with application/json. The server keeps no
// session state and offers no server-to-client stream (GET returns 405, which
// the transport spec allows).

var supportedVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

type toolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	run         func(ctx context.Context, c *call, args json.RawMessage) (string, error)
}

// add registers a typed tool; its JSON Schema is derived from T's fields.
func add[T any](t *tools, name, desc string, fn func(ctx context.Context, c *call, in T) (string, error)) {
	var zero T
	def := toolDef{
		Name:        name,
		Description: desc,
		InputSchema: schemaFor(reflect.TypeOf(zero)),
		run: func(ctx context.Context, c *call, args json.RawMessage) (string, error) {
			var in T
			if len(args) > 0 && string(args) != "null" {
				dec := json.NewDecoder(bytes.NewReader(args))
				if err := dec.Decode(&in); err != nil {
					return "", fmt.Errorf("invalid arguments: %v", err)
				}
			}
			return fn(ctx, c, in)
		},
	}
	t.list = append(t.list, def)
	t.byName[name] = def
}

func schemaFor(rt reflect.Type) map[string]any {
	switch rt.Kind() {
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int64, reflect.Int32:
		return map[string]any{"type": "integer"}
	case reflect.Slice:
		return map[string]any{"type": "array", "items": schemaFor(rt.Elem())}
	case reflect.Struct:
		props := map[string]any{}
		var required []string
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			tag := f.Tag.Get("json")
			name, opts, _ := strings.Cut(tag, ",")
			if name == "" || name == "-" {
				continue
			}
			p := schemaFor(f.Type)
			if d := f.Tag.Get("jsonschema"); d != "" {
				p["description"] = d
			}
			props[name] = p
			if !strings.Contains(opts, "omitempty") {
				required = append(required, name)
			}
		}
		s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
		if len(required) > 0 {
			s["required"] = required
		}
		return s
	}
	return map[string]any{}
}

// ---------- JSON-RPC ----------

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type textContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type toolResult struct {
	Content []textContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

// Handler returns the MCP endpoint handler.
func Handler(st *store.Store, baseURL, version string) http.Handler {
	t := &tools{st: st, baseURL: strings.TrimRight(baseURL, "/"), version: version, byName: map[string]toolDef{}}
	registerTools(t)
	return http.HandlerFunc(t.serveHTTP)
}

func (t *tools) authenticate(w http.ResponseWriter, r *http.Request) (*call, bool) {
	tok, ok := auth.BasicToken(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="NewGit", error="invalid_request", error_description="create a token on the device page and send it as a Bearer token"`)
		http.Error(w, "missing bearer token", http.StatusUnauthorized)
		return nil, false
	}
	u, tk, ok := t.st.VerifyToken(tok)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="NewGit", error="invalid_token"`)
		http.Error(w, "invalid or revoked token", http.StatusUnauthorized)
		return nil, false
	}
	return &call{user: u, canWrite: tk.CanWrite()}, true
}

func (t *tools) serveHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
	case http.MethodGet:
		w.Header().Set("Allow", "POST")
		http.Error(w, "this server does not offer an SSE stream; POST JSON-RPC messages instead", http.StatusMethodNotAllowed)
		return
	case http.MethodDelete:
		w.WriteHeader(http.StatusOK) // stateless: nothing to tear down
		return
	default:
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	c, ok := t.authenticate(w, r)
	if !ok {
		return
	}
	body, err := readAll(http.MaxBytesReader(w, r.Body, 8<<20))
	if err != nil {
		writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{Code: -32700, Message: "could not read body"}})
		return
	}
	body = bytes.TrimSpace(body)
	if len(body) > 0 && body[0] == '[' {
		var batch []rpcRequest
		if err := json.Unmarshal(body, &batch); err != nil {
			writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{Code: -32700, Message: "parse error"}})
			return
		}
		var out []rpcResponse
		for _, req := range batch {
			if resp, ok := t.handle(r.Context(), c, req); ok {
				out = append(out, resp)
			}
		}
		if len(out) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		writeRPC(w, out)
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{Code: -32700, Message: "parse error"}})
		return
	}
	resp, ok := t.handle(r.Context(), c, req)
	if !ok {
		// notification or client response: acknowledged, nothing to return
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeRPC(w, resp)
}

func readAll(r interface{ Read([]byte) (int, error) }) ([]byte, error) {
	var buf bytes.Buffer
	_, err := buf.ReadFrom(r)
	return buf.Bytes(), err
}

func writeRPC(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// handle processes one message. ok=false means no response is due.
func (t *tools) handle(ctx context.Context, c *call, req rpcRequest) (rpcResponse, bool) {
	if len(req.ID) == 0 || string(req.ID) == "null" {
		return rpcResponse{}, false // notification (e.g. notifications/initialized)
	}
	if req.Method == "" {
		return rpcResponse{}, false // a response from the client; we never send requests
	}
	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		version := supportedVersions[0]
		for _, v := range supportedVersions {
			if v == p.ProtocolVersion {
				version = v
			}
		}
		resp.Result = map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "newgit", "title": "NewGit", "version": t.version},
			"instructions":    instructions,
		}
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		resp.Result = map[string]any{"tools": t.list}
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			resp.Error = &rpcError{Code: -32602, Message: "invalid params"}
			break
		}
		def, ok := t.byName[p.Name]
		if !ok {
			resp.Error = &rpcError{Code: -32602, Message: "unknown tool: " + p.Name}
			break
		}
		out, err := safeRun(ctx, def, c, p.Arguments)
		if err != nil {
			resp.Result = toolResult{Content: []textContent{{Type: "text", Text: err.Error()}}, IsError: true}
		} else {
			resp.Result = toolResult{Content: []textContent{{Type: "text", Text: out}}}
		}
	case "resources/list":
		resp.Result = map[string]any{"resources": []any{}}
	case "prompts/list":
		resp.Result = map[string]any{"prompts": []any{}}
	default:
		resp.Error = &rpcError{Code: -32601, Message: "method not found: " + req.Method}
	}
	return resp, true
}

func safeRun(ctx context.Context, def toolDef, c *call, args json.RawMessage) (out string, err error) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("mcp tool %s panicked: %v", def.Name, r)
			err = fmt.Errorf("internal error in %s", def.Name)
		}
	}()
	return def.run(ctx, c, args)
}
