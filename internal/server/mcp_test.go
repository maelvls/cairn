package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// mcpCall sends one JSON-RPC request to /mcp and returns the decoded
// response.
func mcpCall(t *testing.T, c *testClient, method string, params any) rpcResponse {
	t.Helper()
	var resp struct {
		rpcResponse
		Result json.RawMessage `json:"result"`
	}
	c.mustDo("POST", "/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params}, &resp, http.StatusOK)
	resp.rpcResponse.Result = resp.Result
	return resp.rpcResponse
}

type toolResult struct {
	Content []struct {
		Text string `json:"text"`
	} `json:"content"`
	StructuredContent struct {
		Result json.RawMessage `json:"result"`
	} `json:"structuredContent"`
	IsError bool `json:"isError"`
}

// mcpTool calls a tool and decodes its structured result into out; it fails
// the test on a tool error unless wantErr is set, and returns the text.
func callTool(t *testing.T, c *testClient, name string, args any, out any, wantErr bool) string {
	t.Helper()
	resp := mcpCall(t, c, "tools/call", map[string]any{"name": name, "arguments": args})
	if resp.Error != nil {
		t.Fatalf("%s: JSON-RPC error %v", name, resp.Error)
	}
	var res toolResult
	if err := json.Unmarshal(resp.Result.(json.RawMessage), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Content) != 1 {
		t.Fatalf("%s: content %+v", name, res.Content)
	}
	if res.IsError != wantErr {
		t.Fatalf("%s: isError=%v, want %v: %s", name, res.IsError, wantErr, res.Content[0].Text)
	}
	if out != nil {
		if err := json.Unmarshal(res.StructuredContent.Result, out); err != nil {
			t.Fatalf("%s: decode %s: %v", name, res.StructuredContent.Result, err)
		}
	}
	return res.Content[0].Text
}

func TestMCPHandshake(t *testing.T) {
	_, ts := testServer(t)

	anon := &testClient{t: t, base: ts.URL}
	anon.mustDo("POST", "/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping"}, nil, http.StatusUnauthorized)

	c := login(t, ts.URL, "admin@example.com", "admin-password")
	init := mcpCall(t, c, "initialize", map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}})
	var ir struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name string `json:"name"`
		} `json:"serverInfo"`
	}
	json.Unmarshal(init.Result.(json.RawMessage), &ir)
	if ir.ProtocolVersion != "2025-03-26" || ir.ServerInfo.Name != "cairn" {
		t.Fatalf("initialize: %+v", ir)
	}

	// Unknown protocol versions get the newest one.
	init = mcpCall(t, c, "initialize", map[string]any{"protocolVersion": "1999-01-01"})
	json.Unmarshal(init.Result.(json.RawMessage), &ir)
	if ir.ProtocolVersion != mcpProtocolVersions[0] {
		t.Fatalf("fallback version: %q", ir.ProtocolVersion)
	}

	// Notifications are accepted without a body.
	resp := c.do("POST", "/mcp", map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("notification: status %d", resp.StatusCode)
	}

	var list struct {
		Tools []struct {
			Name        string         `json:"name"`
			InputSchema map[string]any `json:"inputSchema"`
		} `json:"tools"`
	}
	json.Unmarshal(mcpCall(t, c, "tools/list", nil).Result.(json.RawMessage), &list)
	if len(list.Tools) != len(mcpTools) {
		t.Fatalf("tools/list: %d tools, want %d", len(list.Tools), len(mcpTools))
	}
	for _, tool := range list.Tools {
		if tool.InputSchema["type"] != "object" {
			t.Errorf("%s: schema %v", tool.Name, tool.InputSchema)
		}
	}

	if e := mcpCall(t, c, "nope", nil).Error; e == nil || e.Code != -32601 {
		t.Fatalf("unknown method: %+v", e)
	}
	if e := mcpCall(t, c, "tools/call", map[string]any{"name": "nope"}).Error; e == nil || e.Code != -32602 {
		t.Fatalf("unknown tool: %+v", e)
	}

	// Batches answer every request and skip notifications.
	var batch []rpcResponse
	c.mustDo("POST", "/mcp", []map[string]any{
		{"jsonrpc": "2.0", "id": 1, "method": "ping"},
		{"jsonrpc": "2.0", "method": "notifications/initialized"},
		{"jsonrpc": "2.0", "id": 2, "method": "ping"},
	}, &batch, http.StatusOK)
	if len(batch) != 2 {
		t.Fatalf("batch: %+v", batch)
	}

	resp = c.do("GET", "/mcp", nil, nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /mcp: status %d", resp.StatusCode)
	}
}

func TestMCPPublishAndData(t *testing.T) {
	_, ts := testServer(t)
	c := login(t, ts.URL, "admin@example.com", "admin-password")

	// No index.html: rejected, and no empty artifact is left behind.
	callTool(t, c, "publish", map[string]any{
		"name":  "broken",
		"files": []map[string]any{{"path": "app.js", "content": "1"}},
	}, nil, true)
	var arts []map[string]any
	callTool(t, c, "list_artifacts", map[string]any{}, &arts, false)
	if len(arts) != 0 {
		t.Fatalf("artifacts after failed publish: %v", arts)
	}

	var pub struct {
		Artifact struct {
			ID     string `json:"id"`
			Public bool   `json:"public"`
		} `json:"artifact"`
		Version struct {
			ID string `json:"id"`
		} `json:"version"`
		URL string `json:"url"`
	}
	callTool(t, c, "publish", map[string]any{
		"name":   "hello",
		"public": true,
		"files": []map[string]any{
			{"path": "index.html", "content": "<h1>héllo</h1>"},
			{"path": "img/dot.png", "content": "iVBORw0KGgo=", "encoding": "base64"},
		},
		"changelog": "first",
	}, &pub, false)
	if pub.Artifact.ID == "" || !pub.Artifact.Public || !strings.HasSuffix(pub.URL, "/artifacts/"+pub.Artifact.ID+"/"+pub.Version.ID+"/") {
		t.Fatalf("publish: %+v", pub)
	}
	page, err := http.Get(ts.URL + "/artifacts/" + pub.Artifact.ID + "/" + pub.Version.ID + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(page.Body)
	page.Body.Close()
	if string(body) != "<h1>héllo</h1>" {
		t.Fatalf("served page: %q", body)
	}

	var src map[string]any
	callTool(t, c, "read_source", map[string]any{"artifact": pub.Artifact.ID, "path": "img/dot.png"}, &src, false)
	if src["encoding"] != "base64" || src["content"] != "iVBORw0KGgo=" {
		t.Fatalf("read_source binary: %v", src)
	}
	callTool(t, c, "read_source", map[string]any{"artifact": pub.Artifact.ID}, &src, false)
	if files := src["files"].([]any); len(files) != 2 {
		t.Fatalf("read_source list: %v", src)
	}
	callTool(t, c, "read_source", map[string]any{"artifact": pub.Artifact.ID, "path": "../secret.key"}, nil, true)

	// Shared database on the latest version.
	callTool(t, c, "db_batch", map[string]any{"artifact": pub.Artifact.ID, "statements": []map[string]any{
		{"sql": "CREATE TABLE t (x TEXT)"},
		{"sql": "INSERT INTO t VALUES (?)", "params": []any{"a"}},
	}}, nil, false)
	var res struct {
		Rows [][]any `json:"rows"`
	}
	callTool(t, c, "db_query", map[string]any{"artifact": pub.Artifact.ID, "sql": "SELECT x FROM t"}, &res, false)
	if len(res.Rows) != 1 || res.Rows[0][0] != "a" {
		t.Fatalf("db_query: %+v", res)
	}
	if text := callTool(t, c, "db_query", map[string]any{"artifact": pub.Artifact.ID, "sql": "SELECT nope FROM t"}, nil, true); !strings.Contains(text, "no such column") {
		t.Fatalf("db_query error: %s", text)
	}

	// Replacing the version keeps its database.
	callTool(t, c, "publish", map[string]any{
		"artifact":        pub.Artifact.ID,
		"replace_version": pub.Version.ID,
		"files":           []map[string]any{{"path": "index.html", "content": "v1 again"}},
	}, nil, false)
	callTool(t, c, "db_query", map[string]any{"artifact": pub.Artifact.ID, "version": pub.Version.ID, "sql": "SELECT count(*) FROM t"}, &res, false)
	if res.Rows[0][0] != float64(1) {
		t.Fatalf("db after replace: %+v", res)
	}

	// A new version becomes the latest.
	var pub2 struct {
		Version struct {
			ID  string `json:"id"`
			Seq int    `json:"seq"`
		} `json:"version"`
	}
	callTool(t, c, "publish", map[string]any{
		"artifact": pub.Artifact.ID,
		"files":    []map[string]any{{"path": "index.html", "content": "v2"}},
	}, &pub2, false)
	if pub2.Version.Seq != 2 {
		t.Fatalf("second version: %+v", pub2)
	}
	callTool(t, c, "read_source", map[string]any{"artifact": pub.Artifact.ID, "path": "index.html"}, &src, false)
	if src["content"] != "v2" {
		t.Fatalf("latest source: %v", src)
	}

	// File storage.
	callTool(t, c, "write_file", map[string]any{"artifact": pub.Artifact.ID, "path": "notes/a b.txt", "content": "hi"}, nil, false)
	var files struct {
		Files []fileInfo `json:"files"`
	}
	callTool(t, c, "list_files", map[string]any{"artifact": pub.Artifact.ID}, &files, false)
	if len(files.Files) != 1 || files.Files[0].Path != "notes/a b.txt" {
		t.Fatalf("list_files: %+v", files)
	}
	var f map[string]any
	callTool(t, c, "read_file", map[string]any{"artifact": pub.Artifact.ID, "path": "notes/a b.txt"}, &f, false)
	if f["content"] != "hi" {
		t.Fatalf("read_file: %v", f)
	}
	callTool(t, c, "delete_file", map[string]any{"artifact": pub.Artifact.ID, "path": "notes/a b.txt"}, nil, false)
	callTool(t, c, "read_file", map[string]any{"artifact": pub.Artifact.ID, "path": "notes/a b.txt"}, nil, true)

	var got struct {
		Artifact struct {
			Name   string `json:"name"`
			Public bool   `json:"public"`
		} `json:"artifact"`
		Versions []any `json:"versions"`
	}
	callTool(t, c, "update_artifact", map[string]any{"artifact": pub.Artifact.ID, "name": "renamed", "public": false}, nil, false)
	callTool(t, c, "get_artifact", map[string]any{"artifact": pub.Artifact.ID}, &got, false)
	if got.Artifact.Name != "renamed" || got.Artifact.Public || len(got.Versions) != 2 {
		t.Fatalf("get_artifact: %+v", got)
	}

	callTool(t, c, "delete_artifact", map[string]any{"artifact": pub.Artifact.ID}, nil, false)
	callTool(t, c, "get_artifact", map[string]any{"artifact": pub.Artifact.ID}, nil, true)
}
