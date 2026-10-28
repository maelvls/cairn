package server

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/aloisdeniel/cairn/internal/store"
)

// newUser creates a non-admin account and signs it in.
func newUser(t *testing.T, base string, admin *testClient, email, name string) *testClient {
	t.Helper()
	admin.mustDo("POST", "/api/admin/users", map[string]any{"email": email, "name": name}, nil, http.StatusCreated)
	u := &testClient{t: t, base: base}
	var out struct {
		Token string `json:"token"`
	}
	u.mustDo("POST", "/api/auth/login", map[string]string{"email": email, "password": "password123", "confirm": "password123"}, &out, http.StatusOK)
	u.token = out.Token
	return u
}

func uploadFile(t *testing.T, c *testClient, path, filename, content string) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("archive", filename)
	fw.Write([]byte(content))
	mw.Close()
	req, _ := http.NewRequest("POST", c.base+path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestGalleryListAndPins(t *testing.T) {
	_, ts := testServer(t)
	admin, older, _ := setupArtifact(t, ts.URL, false)
	bob := newUser(t, ts.URL, admin, "bob@example.com", "Bob")
	newer := createArtifact(t, bob, "empty", false)

	var list []*store.ArtifactSummary
	bob.mustDo("GET", "/api/artifacts", nil, &list, http.StatusOK)
	if len(list) != 2 || list[0].ID != newer || list[1].ID != older {
		t.Fatalf("want most recently updated first, got %+v", list)
	}
	if list[0].LatestVersion != nil || list[0].VersionCount != 0 || list[0].CreatedBy == "" {
		t.Errorf("empty artifact summary: %+v", list[0])
	}
	if list[1].LatestVersion == nil || list[1].LatestVersion.Seq != 1 || list[1].VersionCount != 1 {
		t.Errorf("summary of the artifact with a version: %+v", list[1])
	}

	// Pins are per user.
	bob.mustDo("PUT", "/api/artifacts/"+older+"/pin", nil, nil, http.StatusOK)
	bob.mustDo("PUT", "/api/artifacts/"+older+"/pin", nil, nil, http.StatusOK) // idempotent
	bob.mustDo("GET", "/api/artifacts", nil, &list, http.StatusOK)
	if !list[1].Pinned {
		t.Error("bob's pin missing")
	}
	admin.mustDo("GET", "/api/artifacts", nil, &list, http.StatusOK)
	if list[1].Pinned {
		t.Error("bob's pin leaked to admin")
	}
	bob.mustDo("DELETE", "/api/artifacts/"+older+"/pin", nil, nil, http.StatusOK)
	bob.mustDo("GET", "/api/artifacts", nil, &list, http.StatusOK)
	if list[1].Pinned {
		t.Error("unpin did not stick")
	}
	anon := &testClient{t: t, base: ts.URL}
	anon.mustDo("PUT", "/api/artifacts/"+older+"/pin", nil, nil, http.StatusUnauthorized)

	// The gallery is every signed-in user's home, not only admins'.
	resp := get(t, ts.URL+"/", bob.token, "text/html")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body(t, resp), "/_ui/ui.js") {
		t.Errorf("gallery for a non-admin: %d", resp.StatusCode)
	}
	resp = get(t, ts.URL+"/", "", "text/html")
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/login" {
		t.Errorf("anonymous root: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	for _, f := range []string{"ui.css", "ui.js"} {
		resp = get(t, ts.URL+"/_ui/"+f, "", "")
		if resp.StatusCode != http.StatusOK {
			t.Errorf("/_ui/%s: %d", f, resp.StatusCode)
		}
		resp.Body.Close()
	}
	resp = get(t, ts.URL+"/_ui/nope.js", "", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("/_ui/nope.js: %d", resp.StatusCode)
	}
}

func TestComments(t *testing.T) {
	_, ts := testServer(t)
	admin, aid, vid := setupArtifact(t, ts.URL, true)
	bob := newUser(t, ts.URL, admin, "bob@example.com", "Bob")
	base := "/api/artifacts/" + aid + "/comments"

	var root store.Comment
	bob.mustDo("POST", base, map[string]any{"body": "  too pale  ", "versionId": vid}, &root, http.StatusCreated)
	if root.Body != "too pale" || root.Author != "Bob" || root.Via != "api" || root.VersionID != vid {
		t.Fatalf("root comment: %+v", root)
	}
	bob.mustDo("POST", base, map[string]any{"body": " "}, nil, http.StatusBadRequest)
	bob.mustDo("POST", base, map[string]any{"body": "x", "versionId": "nope"}, nil, http.StatusNotFound)
	bob.mustDo("POST", base, map[string]any{"body": "x", "parentId": "nope"}, nil, http.StatusNotFound)

	// A reply to a reply joins the root's thread.
	var reply, reply2 store.Comment
	admin.mustDo("POST", base, map[string]any{"body": "fixed", "parentId": root.ID}, &reply, http.StatusCreated)
	bob.mustDo("POST", base, map[string]any{"body": "thanks", "parentId": reply.ID}, &reply2, http.StatusCreated)
	if reply2.ParentID != root.ID {
		t.Errorf("nested reply parent = %q, want the root", reply2.ParentID)
	}

	// Resolving a reply resolves its thread; only roots carry the state.
	admin.mustDo("PATCH", base+"/"+reply.ID, map[string]any{"resolved": true}, nil, http.StatusOK)
	var open, resolved, all []store.Comment
	bob.mustDo("GET", base+"?status=open", nil, &open, http.StatusOK)
	bob.mustDo("GET", base+"?status=resolved", nil, &resolved, http.StatusOK)
	bob.mustDo("GET", base, nil, &all, http.StatusOK)
	if len(open) != 0 || len(resolved) != 3 || len(all) != 3 || all[0].ResolvedAt == "" {
		t.Fatalf("open %d, resolved %d, all %d (%+v)", len(open), len(resolved), len(all), all)
	}
	bob.mustDo("GET", base+"?status=bogus", nil, nil, http.StatusBadRequest)

	var list []*store.ArtifactSummary
	bob.mustDo("GET", "/api/artifacts", nil, &list, http.StatusOK)
	if list[0].OpenComments != 0 {
		t.Errorf("openComments after resolve = %d", list[0].OpenComments)
	}
	// A new reply reopens the thread.
	bob.mustDo("POST", base, map[string]any{"body": "still pale", "parentId": root.ID}, nil, http.StatusCreated)
	bob.mustDo("GET", "/api/artifacts", nil, &list, http.StatusOK)
	if list[0].OpenComments != 1 {
		t.Errorf("openComments after a reply = %d", list[0].OpenComments)
	}

	// Editing is the author's, deleting the author's or an admin's.
	admin.mustDo("PATCH", base+"/"+root.ID, map[string]any{"body": "hijack"}, nil, http.StatusForbidden)
	bob.mustDo("PATCH", base+"/"+root.ID, map[string]any{"body": "much too pale"}, &root, http.StatusOK)
	if root.Body != "much too pale" {
		t.Errorf("edit: %+v", root)
	}
	bob.mustDo("DELETE", base+"/"+reply.ID, nil, nil, http.StatusForbidden)
	admin.mustDo("DELETE", base+"/"+root.ID, nil, nil, http.StatusOK)
	bob.mustDo("GET", base, nil, &all, http.StatusOK)
	if len(all) != 0 {
		t.Errorf("deleting the root should delete its replies, %d left", len(all))
	}

	// Anonymous visitors of a public artifact don't see comments.
	anon := &testClient{t: t, base: ts.URL}
	anon.mustDo("GET", base, nil, nil, http.StatusUnauthorized)
}

func TestMCPComments(t *testing.T) {
	_, ts := testServer(t)
	admin, aid, vid := setupArtifact(t, ts.URL, false)
	var root store.Comment
	admin.mustDo("POST", "/api/artifacts/"+aid+"/comments", map[string]any{"body": "bigger title please"}, &root, http.StatusCreated)

	var threads []commentThread
	callTool(t, admin, "list_comments", map[string]any{"artifact": aid}, &threads, false)
	if len(threads) != 1 || threads[0].Body != "bigger title please" || len(threads[0].Replies) != 0 {
		t.Fatalf("threads: %+v", threads)
	}
	var reply store.Comment
	callTool(t, admin, "add_comment", map[string]any{"artifact": aid, "body": "done in v2", "reply_to": root.ID, "resolve": true}, &reply, false)
	if reply.Via != "mcp" || reply.VersionID != vid || reply.ParentID != root.ID {
		t.Errorf("agent reply: %+v", reply)
	}
	callTool(t, admin, "list_comments", map[string]any{"artifact": aid}, &threads, false)
	if len(threads) != 0 {
		t.Errorf("thread should be resolved, open: %+v", threads)
	}
	callTool(t, admin, "list_comments", map[string]any{"artifact": aid, "status": "all"}, &threads, false)
	if len(threads) != 1 || len(threads[0].Replies) != 1 || threads[0].ResolvedAt == "" {
		t.Errorf("all threads: %+v", threads)
	}
	var c store.Comment
	callTool(t, admin, "resolve_comment", map[string]any{"artifact": aid, "comment": root.ID, "reopen": true}, &c, false)
	if c.ResolvedAt != "" {
		t.Errorf("reopen: %+v", c)
	}
}

func TestViewerData(t *testing.T) {
	s, ts := testServer(t)
	admin, aid, vid := setupArtifact(t, ts.URL, true)
	anon := &testClient{t: t, base: ts.URL}
	vbase := "/api/artifacts/" + aid + "/versions/" + vid

	var files []fileInfo
	anon.mustDo("GET", vbase+"/source", nil, &files, http.StatusOK)
	if len(files) != 3 || files[0].Path != "app.js" || files[2].Path != "sub/index.html" {
		t.Errorf("source: %+v", files)
	}

	// Describing a database that was never written must not create it.
	var tables []tableInfo
	admin.mustDo("GET", vbase+"/db/tables", nil, &tables, http.StatusOK)
	if len(tables) != 0 {
		t.Errorf("tables of a fresh version: %+v", tables)
	}
	if _, err := os.Stat(s.layout.VersionDB(aid, vid)); !os.IsNotExist(err) {
		t.Errorf("db/tables created the database: %v", err)
	}
	admin.mustDo("POST", vbase+"/db/batch", map[string]any{"statements": []map[string]any{
		{"sql": `CREATE TABLE "odd ""name""" (id INTEGER PRIMARY KEY, body TEXT)`},
		{"sql": `INSERT INTO "odd ""name""" (body) VALUES ('a'), ('b')`},
		{"sql": `CREATE VIEW v AS SELECT body FROM "odd ""name"""`},
	}}, nil, http.StatusOK)
	anon.mustDo("GET", vbase+"/db/tables", nil, &tables, http.StatusOK)
	if len(tables) != 2 || tables[0].Name != `odd "name"` || tables[0].Rows != 2 || len(tables[0].Columns) != 2 || tables[1].Type != "view" {
		t.Errorf("tables: %+v", tables)
	}

	// The viewer knows whether to follow the latest version.
	resp := get(t, ts.URL+"/shared/"+aid, "", "text/html")
	if html := body(t, resp); !strings.Contains(html, `"follow":true`) {
		t.Errorf("viewer of the latest version should follow it")
	}
	resp = get(t, ts.URL+"/shared/"+aid+"/"+vid, "", "text/html")
	if html := body(t, resp); !strings.Contains(html, `"follow":false`) || !strings.Contains(html, `"user":null`) {
		t.Errorf("viewer of a given version, anonymously")
	}
}

func TestSingleHTMLUploadAndTitle(t *testing.T) {
	_, ts := testServer(t)
	admin := login(t, ts.URL, "admin@example.com", "admin-password")
	aid := createArtifact(t, admin, "page", false)
	resp := uploadFile(t, admin, "/api/artifacts/"+aid+"/versions", "Report.HTML",
		"<!doctype html><html><head><title>\n  Plan &amp; budget\n</title></head><body>hi</body></html>")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("html upload: %d %s", resp.StatusCode, body(t, resp))
	}
	v := decode[store.Version](t, resp)
	if v.CreatedBy == "" {
		t.Error("version has no author")
	}
	page := get(t, ts.URL+"/artifacts/"+aid+"/"+v.ID+"/", admin.token, "text/html")
	if !strings.Contains(body(t, page), "<body>hi</body>") {
		t.Error("the page is not served as index.html")
	}
	var a store.Artifact
	admin.mustDo("GET", "/api/artifacts/"+aid, nil, &a, http.StatusOK)
	if a.Description != "Plan & budget" {
		t.Errorf("description from <title> = %q", a.Description)
	}

	// An explicit description is never overwritten.
	admin.mustDo("PATCH", "/api/artifacts/"+aid, map[string]any{"description": "Mine"}, nil, http.StatusOK)
	uploadFile(t, admin, "/api/artifacts/"+aid+"/versions", "x.html", "<title>Other</title>").Body.Close()
	admin.mustDo("GET", "/api/artifacts/"+aid, nil, &a, http.StatusOK)
	if a.Description != "Mine" {
		t.Errorf("description overwritten: %q", a.Description)
	}

	// Anything else named .html must still be a valid page or zip.
	resp = uploadFile(t, admin, "/api/artifacts/"+aid+"/versions", "notes.txt", "plain")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("non-zip, non-html upload: %d", resp.StatusCode)
	}
	resp.Body.Close()
}
