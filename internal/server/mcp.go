package server

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/aloisdeniel/cairn/internal/store"
	"github.com/aloisdeniel/cairn/internal/versiondb"
)

// MCP endpoint (Streamable HTTP, stateless): POST /mcp takes one JSON-RPC
// message (or a batch) and answers with plain JSON, never an SSE stream. It
// sits behind requireAuth like the rest of the API: an OAuth gateway in front
// of Cairn holds an API key and sends it as the bearer token.
//
// Tools don't reimplement anything: each one replays the matching /api route
// in-process with the caller's identity (see mcpAPI), so validation, limits
// and the read/write rules stay those of the HTTP API.

// mcpProtocolVersions are the revisions this server speaks, newest first. The
// client's requested version is echoed back when it is one of them.
var mcpProtocolVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

const mcpInstructions = `Cairn hosts artifacts: single-page web apps (an index.html plus assets), each with versions, a shared SQLite database per version, and a file storage per version.

- publish creates an artifact (or a new version of one) from files given inline; the result's url is the page to open. Artifacts are private (sign-in required) unless public is true.
- Pages can use the shared database and file storage through cairn.js: <script src="./cairn.js"></script> is served inside every version.
- "artifact" arguments take an artifact id (or a resource value associated with it); "version" defaults to the latest version.
- db_query runs one SQL statement; db_batch runs several in one transaction (use it for CREATE TABLE migrations).`

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
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

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.maxUploadBytes()))
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32600, "request too large"}})
		return
	}
	body = bytes.TrimSpace(body)
	if len(body) > 0 && body[0] == '[' {
		var msgs []rpcMessage
		if err := json.Unmarshal(body, &msgs); err != nil {
			writeJSON(w, http.StatusBadRequest, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32700, "parse error"}})
			return
		}
		var out []*rpcResponse
		for _, m := range msgs {
			if resp := s.mcpDispatch(r, m); resp != nil {
				out = append(out, resp)
			}
		}
		if len(out) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	var m rpcMessage
	if err := json.Unmarshal(body, &m); err != nil {
		writeJSON(w, http.StatusBadRequest, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32700, "parse error"}})
		return
	}
	resp := s.mcpDispatch(r, m)
	if resp == nil {
		// Notifications and responses get no answer.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// mcpDispatch answers one JSON-RPC message; nil for notifications and for
// responses sent by the client.
func (s *Server) mcpDispatch(r *http.Request, m rpcMessage) *rpcResponse {
	if len(m.ID) == 0 || m.Method == "" {
		return nil
	}
	resp := &rpcResponse{JSONRPC: "2.0", ID: m.ID}
	switch m.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(m.Params, &p)
		version := mcpProtocolVersions[0]
		for _, v := range mcpProtocolVersions {
			if v == p.ProtocolVersion {
				version = v
			}
		}
		resp.Result = map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "cairn", "title": "Cairn", "version": "1"},
			"instructions":    mcpInstructions,
		}
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		tools := make([]map[string]any, 0, len(mcpTools))
		for _, t := range mcpTools {
			tools = append(tools, map[string]any{
				"name":        t.name,
				"description": t.description,
				"inputSchema": t.schema,
				"annotations": map[string]any{"readOnlyHint": t.readOnly, "destructiveHint": t.destructive},
			})
		}
		resp.Result = map[string]any{"tools": tools}
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(m.Params, &p); err != nil {
			resp.Error = &rpcError{-32602, "invalid params: " + err.Error()}
			return resp
		}
		t := mcpToolByName(p.Name)
		if t == nil {
			resp.Error = &rpcError{-32602, "unknown tool: " + p.Name}
			return resp
		}
		if len(p.Arguments) == 0 || string(p.Arguments) == "null" {
			p.Arguments = json.RawMessage("{}")
		}
		s.log.Info("mcp tool call", "tool", t.name, "by", requestUser(r).Email)
		out, err := t.call(s, r, p.Arguments)
		if err != nil {
			resp.Result = map[string]any{
				"content": []map[string]any{{"type": "text", "text": err.Error()}},
				"isError": true,
			}
			return resp
		}
		text, _ := json.MarshalIndent(out, "", "  ")
		resp.Result = map[string]any{
			"content":           []map[string]any{{"type": "text", "text": string(text)}},
			"structuredContent": map[string]any{"result": out},
		}
	default:
		resp.Error = &rpcError{-32601, "method not found: " + m.Method}
	}
	return resp
}

// mcpAPI replays one /api request in-process with the caller's identity
// (the user is already in r's context, which currentUser reads first) and
// decodes the JSON answer into out. Non-2xx answers become errors carrying
// the API's message.
func (s *Server) mcpAPI(r *http.Request, method, path, contentType string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(r.Context(), method, path, body)
	if err != nil {
		return err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code >= 300 {
		var e apiError
		if json.Unmarshal(rec.Body.Bytes(), &e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		return fmt.Errorf("%s %s: HTTP %d", method, path, rec.Code)
	}
	if out == nil {
		return nil
	}
	if raw, ok := out.(*[]byte); ok {
		*raw = rec.Body.Bytes()
		return nil
	}
	return json.Unmarshal(rec.Body.Bytes(), out)
}

func (s *Server) mcpJSON(r *http.Request, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	return s.mcpAPI(r, method, path, "application/json", body, out)
}

// mcpVersion resolves an artifact reference and an optional version id
// (default: the latest version).
func (s *Server) mcpVersion(artifactRef, versionID string) (*store.Artifact, *store.Version, error) {
	if artifactRef == "" {
		return nil, nil, errors.New("artifact is required")
	}
	a, err := s.resolveArtifactRef(artifactRef)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil, fmt.Errorf("artifact %q not found", artifactRef)
	}
	if err != nil {
		return nil, nil, err
	}
	var v *store.Version
	if versionID == "" {
		v, err = s.store.LatestVersion(a.ID)
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil, fmt.Errorf("artifact %q has no versions yet", a.ID)
		}
	} else {
		v, err = s.store.VersionByID(a.ID, versionID)
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil, fmt.Errorf("version %q of artifact %q not found", versionID, a.ID)
		}
	}
	if err != nil {
		return nil, nil, err
	}
	return a, v, nil
}

// mcpURL is the external URL of an artifact page (latest version when vid
// is empty).
func (s *Server) mcpURL(aid, vid string) string {
	u := strings.TrimRight(s.cfg.BaseURL, "/") + "/artifacts/" + aid
	if vid != "" {
		u += "/" + vid + "/"
	}
	return u
}

// mcpFile is a file passed inline to a tool: text, or base64 for binaries.
type mcpFile struct {
	Path     string `json:"path"`
	Content  string `json:"content"`
	Encoding string `json:"encoding"`
}

func (f mcpFile) bytes() ([]byte, error) {
	switch f.Encoding {
	case "", "utf-8", "text":
		return []byte(f.Content), nil
	case "base64":
		b, err := base64.StdEncoding.DecodeString(f.Content)
		if err != nil {
			return nil, fmt.Errorf("%s: invalid base64: %w", f.Path, err)
		}
		return b, nil
	default:
		return nil, fmt.Errorf("%s: unknown encoding %q (use \"utf-8\" or \"base64\")", f.Path, f.Encoding)
	}
}

// mcpContent returns data as text when it is valid UTF-8, else base64.
func mcpContent(data []byte) map[string]any {
	if utf8.Valid(data) {
		return map[string]any{"encoding": "utf-8", "content": string(data), "size": len(data)}
	}
	return map[string]any{"encoding": "base64", "content": base64.StdEncoding.EncodeToString(data), "size": len(data)}
}

// mcpMaxRead caps what read tools return: the answer lands in a model's
// context.
const mcpMaxRead = 1 << 20

// mcpZip builds the multipart upload that /versions expects from inline
// files.
func mcpZip(files []mcpFile, name, changelog string) (body *bytes.Buffer, contentType string, err error) {
	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	for _, f := range files {
		data, err := f.bytes()
		if err != nil {
			return nil, "", err
		}
		p := strings.TrimPrefix(f.Path, "./")
		if !fs.ValidPath(p) || p == "." {
			return nil, "", fmt.Errorf("invalid file path %q", f.Path)
		}
		fw, err := zw.Create(p)
		if err != nil {
			return nil, "", err
		}
		fw.Write(data)
	}
	if err := zw.Close(); err != nil {
		return nil, "", err
	}
	body = &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	mw.WriteField("name", name)
	mw.WriteField("changelog", changelog)
	fw, err := mw.CreateFormFile("archive", "version.zip")
	if err != nil {
		return nil, "", err
	}
	fw.Write(zbuf.Bytes())
	if err := mw.Close(); err != nil {
		return nil, "", err
	}
	return body, mw.FormDataContentType(), nil
}

type mcpTool struct {
	name        string
	description string
	schema      map[string]any
	readOnly    bool
	destructive bool
	call        func(s *Server, r *http.Request, args json.RawMessage) (any, error)
}

func mcpToolByName(name string) *mcpTool {
	for i := range mcpTools {
		if mcpTools[i].name == name {
			return &mcpTools[i]
		}
	}
	return nil
}

// Schema helpers.

func object(required []string, props map[string]any) map[string]any {
	o := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		o["required"] = required
	}
	return o
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }

var (
	artifactProp = str("Artifact id (or a resource value associated with the artifact).")
	versionProp  = str("Version id. Defaults to the latest version.")
	filesProp    = map[string]any{
		"type":        "array",
		"description": "The version's files. One of them must be index.html at the root.",
		"items": object([]string{"path", "content"}, map[string]any{
			"path":     str("Relative path, e.g. index.html or assets/app.js."),
			"content":  str("File content: text, or base64 when encoding is base64."),
			"encoding": map[string]any{"type": "string", "enum": []string{"utf-8", "base64"}, "description": "Defaults to utf-8."},
		}),
	}
	statementProps = map[string]any{
		"sql":    str("One SQL statement (SQLite)."),
		"params": map[string]any{"type": "array", "description": "Positional parameters for ? placeholders.", "items": map[string]any{}},
	}
)

func decodeArgs[T any](raw json.RawMessage) (T, error) {
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, fmt.Errorf("invalid arguments: %w", err)
	}
	return v, nil
}

type versionArgs struct {
	Artifact string `json:"artifact"`
	Version  string `json:"version"`
}

type fileArgs struct {
	versionArgs
	Path string `json:"path"`
}

// filesPath is the /api path of a version's file storage, with the file path
// escaped segment by segment.
func filesPath(a *store.Artifact, v *store.Version, p string) string {
	base := "/api/artifacts/" + a.ID + "/versions/" + v.ID + "/files"
	if p == "" {
		return base
	}
	segs := strings.Split(strings.TrimPrefix(p, "/"), "/")
	for i, seg := range segs {
		segs[i] = url.PathEscape(seg)
	}
	return base + "/" + strings.Join(segs, "/")
}

var mcpTools = []mcpTool{
	{
		name:        "list_artifacts",
		description: "List the artifacts, most recently updated first, with their page URLs.",
		schema:      object(nil, map[string]any{"name": str("Only artifacts with exactly this name.")}),
		readOnly:    true,
		call: func(s *Server, r *http.Request, raw json.RawMessage) (any, error) {
			args, err := decodeArgs[struct {
				Name string `json:"name"`
			}](raw)
			if err != nil {
				return nil, err
			}
			path := "/api/artifacts"
			if args.Name != "" {
				path += "?name=" + url.QueryEscape(args.Name)
			}
			var as []*store.Artifact
			if err := s.mcpJSON(r, "GET", path, nil, &as); err != nil {
				return nil, err
			}
			out := make([]map[string]any, 0, len(as))
			for _, a := range as {
				out = append(out, map[string]any{"artifact": a, "url": s.mcpURL(a.ID, "")})
			}
			return out, nil
		},
	},
	{
		name:        "get_artifact",
		description: "Show an artifact: metadata, resources, versions (newest first) and page URL.",
		schema:      object([]string{"artifact"}, map[string]any{"artifact": artifactProp}),
		readOnly:    true,
		call: func(s *Server, r *http.Request, raw json.RawMessage) (any, error) {
			args, err := decodeArgs[versionArgs](raw)
			if err != nil {
				return nil, err
			}
			if args.Artifact == "" {
				return nil, errors.New("artifact is required")
			}
			var a store.Artifact
			if err := s.mcpJSON(r, "GET", "/api/artifacts/"+url.PathEscape(args.Artifact), nil, &a); err != nil {
				return nil, err
			}
			var vs []*store.Version
			if err := s.mcpJSON(r, "GET", "/api/artifacts/"+a.ID+"/versions", nil, &vs); err != nil {
				return nil, err
			}
			return map[string]any{"artifact": a, "versions": vs, "url": s.mcpURL(a.ID, "")}, nil
		},
	},
	{
		name: "publish",
		description: "Publish a web page: creates an artifact with its first version, or (with artifact) adds a version to an existing one, " +
			"or (with artifact and replace_version) overwrites a version's files in place, keeping its database and file storage. " +
			"Returns the page URL.",
		schema: object([]string{"files"}, map[string]any{
			"files":           filesProp,
			"artifact":        str("Existing artifact to publish a new version of. Omit to create a new artifact."),
			"name":            str("Name of the new artifact (required when artifact is omitted)."),
			"description":     str("Description of the new artifact."),
			"public":          map[string]any{"type": "boolean", "description": "New artifact only: viewable without signing in. Defaults to false."},
			"version_name":    str("Name of the version, e.g. v2."),
			"changelog":       str("What changed in this version."),
			"replace_version": str("Version id to overwrite instead of adding a version."),
		}),
		call: func(s *Server, r *http.Request, raw json.RawMessage) (any, error) {
			args, err := decodeArgs[struct {
				Files          []mcpFile `json:"files"`
				Artifact       string    `json:"artifact"`
				Name           string    `json:"name"`
				Description    string    `json:"description"`
				Public         bool      `json:"public"`
				VersionName    string    `json:"version_name"`
				Changelog      string    `json:"changelog"`
				ReplaceVersion string    `json:"replace_version"`
			}](raw)
			if err != nil {
				return nil, err
			}
			if len(args.Files) == 0 {
				return nil, errors.New("files is required, with at least index.html")
			}
			body, ctype, err := mcpZip(args.Files, args.VersionName, args.Changelog)
			if err != nil {
				return nil, err
			}

			var a store.Artifact
			created := false
			switch {
			case args.Artifact != "":
				if err := s.mcpJSON(r, "GET", "/api/artifacts/"+url.PathEscape(args.Artifact), nil, &a); err != nil {
					return nil, err
				}
			case args.ReplaceVersion != "":
				return nil, errors.New("replace_version needs artifact")
			case args.Name == "":
				return nil, errors.New("name is required to create an artifact (or pass artifact to add a version)")
			default:
				req := artifactRequest{Name: args.Name, Description: args.Description, Public: &args.Public}
				if err := s.mcpJSON(r, "POST", "/api/artifacts", req, &a); err != nil {
					return nil, err
				}
				created = true
			}

			var v store.Version
			if args.ReplaceVersion != "" {
				err = s.mcpAPI(r, "PUT", "/api/artifacts/"+a.ID+"/versions/"+url.PathEscape(args.ReplaceVersion), ctype, body, &v)
			} else {
				err = s.mcpAPI(r, "POST", "/api/artifacts/"+a.ID+"/versions", ctype, body, &v)
			}
			if err != nil {
				if created {
					// Don't leave an empty artifact behind a rejected upload.
					s.mcpJSON(r, "DELETE", "/api/artifacts/"+a.ID, nil, nil)
				}
				return nil, err
			}
			return map[string]any{"artifact": a, "version": v, "url": s.mcpURL(a.ID, v.ID)}, nil
		},
	},
	{
		name:        "update_artifact",
		description: "Rename an artifact, change its description, or make it public or private.",
		schema: object([]string{"artifact"}, map[string]any{
			"artifact":    artifactProp,
			"name":        str("New name."),
			"description": str("New description."),
			"public":      map[string]any{"type": "boolean", "description": "true: viewable without signing in."},
		}),
		call: func(s *Server, r *http.Request, raw json.RawMessage) (any, error) {
			args, err := decodeArgs[struct {
				Artifact string `json:"artifact"`
				artifactRequest
			}](raw)
			if err != nil {
				return nil, err
			}
			if args.Artifact == "" {
				return nil, errors.New("artifact is required")
			}
			var a store.Artifact
			if err := s.mcpJSON(r, "PATCH", "/api/artifacts/"+url.PathEscape(args.Artifact), args.artifactRequest, &a); err != nil {
				return nil, err
			}
			return map[string]any{"artifact": a, "url": s.mcpURL(a.ID, "")}, nil
		},
	},
	{
		name:        "delete_artifact",
		description: "Delete an artifact with all its versions, databases and stored files. Cannot be undone.",
		schema:      object([]string{"artifact"}, map[string]any{"artifact": artifactProp}),
		destructive: true,
		call: func(s *Server, r *http.Request, raw json.RawMessage) (any, error) {
			args, err := decodeArgs[versionArgs](raw)
			if err != nil {
				return nil, err
			}
			if args.Artifact == "" {
				return nil, errors.New("artifact is required")
			}
			if err := s.mcpJSON(r, "DELETE", "/api/artifacts/"+url.PathEscape(args.Artifact), nil, nil); err != nil {
				return nil, err
			}
			return map[string]any{"ok": true}, nil
		},
	},
	{
		name:        "read_source",
		description: "Read the published files of a version (its index.html and assets). Without path, lists them.",
		schema: object([]string{"artifact"}, map[string]any{
			"artifact": artifactProp,
			"version":  versionProp,
			"path":     str("File to read, e.g. index.html. Omit to list the files."),
		}),
		readOnly: true,
		call: func(s *Server, r *http.Request, raw json.RawMessage) (any, error) {
			args, err := decodeArgs[fileArgs](raw)
			if err != nil {
				return nil, err
			}
			a, v, err := s.mcpVersion(args.Artifact, args.Version)
			if err != nil {
				return nil, err
			}
			root := s.layout.ContentDir(a.ID, v.ContentDir)
			if args.Path == "" {
				files := []fileInfo{}
				err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
					if err != nil || d.IsDir() {
						return err
					}
					info, err := d.Info()
					if err != nil {
						return err
					}
					rel, _ := filepath.Rel(root, p)
					files = append(files, fileInfo{Path: filepath.ToSlash(rel), Size: info.Size(), ModifiedAt: fileModTime(info.ModTime())})
					return nil
				})
				if err != nil {
					return nil, err
				}
				return map[string]any{"artifact": a.ID, "version": v.ID, "files": files}, nil
			}
			p := strings.TrimPrefix(args.Path, "/")
			if !fs.ValidPath(p) || strings.Contains(p, `\`) {
				return nil, fmt.Errorf("invalid path %q", args.Path)
			}
			target := filepath.Join(root, filepath.FromSlash(p))
			st, err := os.Lstat(target)
			if err != nil || !st.Mode().IsRegular() {
				return nil, fmt.Errorf("%s: no such file in version %s", p, v.ID)
			}
			if st.Size() > mcpMaxRead {
				return nil, fmt.Errorf("%s is %d bytes, more than this tool returns (%d); it is served at %s%s", p, st.Size(), mcpMaxRead, s.mcpURL(a.ID, v.ID), p)
			}
			data, err := os.ReadFile(target)
			if err != nil {
				return nil, err
			}
			out := mcpContent(data)
			out["path"] = p
			return out, nil
		},
	},
	{
		name:        "db_query",
		description: "Run one SQL statement against a version's shared SQLite database (created on first use). Returns columns and rows.",
		schema: object([]string{"artifact", "sql"}, map[string]any{
			"artifact": artifactProp,
			"version":  versionProp,
			"sql":      statementProps["sql"],
			"params":   statementProps["params"],
		}),
		call: func(s *Server, r *http.Request, raw json.RawMessage) (any, error) {
			args, err := decodeArgs[struct {
				versionArgs
				versiondb.Statement
			}](raw)
			if err != nil {
				return nil, err
			}
			a, v, err := s.mcpVersion(args.Artifact, args.Version)
			if err != nil {
				return nil, err
			}
			var res versiondb.Result
			if err := s.mcpJSON(r, "POST", "/api/artifacts/"+a.ID+"/versions/"+v.ID+"/db/query", args.Statement, &res); err != nil {
				return nil, err
			}
			return res, nil
		},
	},
	{
		name:        "db_batch",
		description: "Run several SQL statements against a version's shared database in one transaction: all succeed or none do.",
		schema: object([]string{"artifact", "statements"}, map[string]any{
			"artifact":   artifactProp,
			"version":    versionProp,
			"statements": map[string]any{"type": "array", "items": object([]string{"sql"}, statementProps)},
		}),
		call: func(s *Server, r *http.Request, raw json.RawMessage) (any, error) {
			args, err := decodeArgs[struct {
				versionArgs
				batchRequest
			}](raw)
			if err != nil {
				return nil, err
			}
			a, v, err := s.mcpVersion(args.Artifact, args.Version)
			if err != nil {
				return nil, err
			}
			var res []versiondb.Result
			if err := s.mcpJSON(r, "POST", "/api/artifacts/"+a.ID+"/versions/"+v.ID+"/db/batch", args.batchRequest, &res); err != nil {
				return nil, err
			}
			return res, nil
		},
	},
	{
		name:        "list_files",
		description: "List the files in a version's file storage (files that pages or users uploaded, not the page's own source: see read_source).",
		schema:      object([]string{"artifact"}, map[string]any{"artifact": artifactProp, "version": versionProp}),
		readOnly:    true,
		call: func(s *Server, r *http.Request, raw json.RawMessage) (any, error) {
			args, err := decodeArgs[versionArgs](raw)
			if err != nil {
				return nil, err
			}
			a, v, err := s.mcpVersion(args.Artifact, args.Version)
			if err != nil {
				return nil, err
			}
			var files []fileInfo
			if err := s.mcpJSON(r, "GET", filesPath(a, v, ""), nil, &files); err != nil {
				return nil, err
			}
			return map[string]any{"artifact": a.ID, "version": v.ID, "files": files}, nil
		},
	},
	{
		name:        "read_file",
		description: "Read a file from a version's file storage. Text comes back as is, binaries as base64.",
		schema: object([]string{"artifact", "path"}, map[string]any{
			"artifact": artifactProp,
			"version":  versionProp,
			"path":     str("Path in the file storage."),
		}),
		readOnly: true,
		call: func(s *Server, r *http.Request, raw json.RawMessage) (any, error) {
			args, err := decodeArgs[fileArgs](raw)
			if err != nil {
				return nil, err
			}
			a, v, err := s.mcpVersion(args.Artifact, args.Version)
			if err != nil {
				return nil, err
			}
			var data []byte
			if err := s.mcpAPI(r, "GET", filesPath(a, v, args.Path), "", nil, &data); err != nil {
				return nil, err
			}
			if len(data) > mcpMaxRead {
				return nil, fmt.Errorf("%s is %d bytes, more than this tool returns (%d)", args.Path, len(data), mcpMaxRead)
			}
			out := mcpContent(data)
			out["path"] = args.Path
			return out, nil
		},
	},
	{
		name:        "write_file",
		description: "Create or overwrite a file in a version's file storage.",
		schema: object([]string{"artifact", "path", "content"}, map[string]any{
			"artifact": artifactProp,
			"version":  versionProp,
			"path":     str("Path in the file storage."),
			"content":  str("File content: text, or base64 when encoding is base64."),
			"encoding": map[string]any{"type": "string", "enum": []string{"utf-8", "base64"}, "description": "Defaults to utf-8."},
		}),
		call: func(s *Server, r *http.Request, raw json.RawMessage) (any, error) {
			args, err := decodeArgs[struct {
				versionArgs
				mcpFile
			}](raw)
			if err != nil {
				return nil, err
			}
			a, v, err := s.mcpVersion(args.Artifact, args.Version)
			if err != nil {
				return nil, err
			}
			data, err := args.mcpFile.bytes()
			if err != nil {
				return nil, err
			}
			var info fileInfo
			if err := s.mcpAPI(r, "PUT", filesPath(a, v, args.Path), "application/octet-stream", bytes.NewReader(data), &info); err != nil {
				return nil, err
			}
			return info, nil
		},
	},
	{
		name:        "delete_file",
		description: "Delete a file from a version's file storage.",
		schema: object([]string{"artifact", "path"}, map[string]any{
			"artifact": artifactProp,
			"version":  versionProp,
			"path":     str("Path in the file storage."),
		}),
		destructive: true,
		call: func(s *Server, r *http.Request, raw json.RawMessage) (any, error) {
			args, err := decodeArgs[fileArgs](raw)
			if err != nil {
				return nil, err
			}
			a, v, err := s.mcpVersion(args.Artifact, args.Version)
			if err != nil {
				return nil, err
			}
			if err := s.mcpJSON(r, "DELETE", filesPath(a, v, args.Path), nil, nil); err != nil {
				return nil, err
			}
			return map[string]any{"ok": true}, nil
		},
	},
}
