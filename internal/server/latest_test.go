package server

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type versionID struct {
	ID string `json:"id"`
}

// pushVersion uploads a one-page version and returns its id.
func pushVersion(t *testing.T, c *testClient, aid, html string) string {
	t.Helper()
	resp := c.upload("POST", "/api/artifacts/"+aid+"/versions", zipFrom(t, map[string]string{"index.html": html}), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("push version: %d", resp.StatusCode)
	}
	return decode[versionID](t, resp).ID
}

// getWith is get() plus extra request headers.
func getWith(t *testing.T, url string, headers map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Accept", "text/html")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// The "latest" page URL is served in place (no redirect to a version id) and
// keeps following the newest version across pushes and deletes.
func TestLatestAliasPages(t *testing.T) {
	_, ts := testServer(t)
	admin, aid, v1 := setupArtifact(t, ts.URL, true)
	latest := ts.URL + "/artifacts/" + aid + "/latest/"

	resp := get(t, latest, "", "text/html")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body(t, resp), "hello v1") {
		t.Fatalf("latest page: %d", resp.StatusCode)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("latest page Cache-Control: %q", cc)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors") {
		t.Errorf("latest page missing CSP: %q", csp)
	}
	etagV1 := resp.Header.Get("ETag")
	if etagV1 == "" {
		t.Fatal("latest page has no ETag")
	}

	// Assets, the injected cairn.js and the SPA fallback all resolve under it
	for _, p := range []string{"app.js", "cairn.js", "sub/", "some/route"} {
		resp = get(t, latest+p, "", "text/html")
		if resp.StatusCode != http.StatusOK {
			t.Errorf("latest/%s: %d", p, resp.StatusCode)
		}
		resp.Body.Close()
	}

	// Missing trailing slash is canonicalized like any version
	resp = get(t, ts.URL+"/artifacts/"+aid+"/latest", "", "text/html")
	if resp.StatusCode != http.StatusMovedPermanently || resp.Header.Get("Location") != "/artifacts/"+aid+"/latest/" {
		t.Errorf("latest no-slash: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp.Body.Close()

	// Unchanged content revalidates as a 304
	resp = getWith(t, latest, map[string]string{"If-None-Match": etagV1})
	if resp.StatusCode != http.StatusNotModified {
		t.Errorf("revalidation of unchanged latest: %d", resp.StatusCode)
	}
	resp.Body.Close()

	// A pinned URL shares the validator but is not forced to revalidate
	resp = get(t, ts.URL+"/artifacts/"+aid+"/"+v1+"/", "", "text/html")
	if resp.Header.Get("ETag") != etagV1 {
		t.Errorf("pinned ETag %q, want %q", resp.Header.Get("ETag"), etagV1)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "" {
		t.Errorf("pinned page Cache-Control: %q", cc)
	}
	resp.Body.Close()

	// A new push: same URL, new content, and the old validator no longer matches
	v2 := pushVersion(t, admin, aid, "<h1>hello v2</h1>")
	resp = getWith(t, latest, map[string]string{"If-None-Match": etagV1})
	if resp.StatusCode != http.StatusOK || !strings.Contains(body(t, resp), "hello v2") {
		t.Fatalf("latest after push: %d", resp.StatusCode)
	}
	etagV2 := resp.Header.Get("ETag")
	if etagV2 == "" || etagV2 == etagV1 {
		t.Errorf("ETag did not change with the version: %q", etagV2)
	}
	// ...while the pinned URL still serves the old version
	resp = get(t, ts.URL+"/artifacts/"+aid+"/"+v1+"/", "", "text/html")
	if !strings.Contains(body(t, resp), "hello v1") {
		t.Errorf("pinned v1 changed after a push")
	}

	// Deleting the newest version rolls "latest" back to older files. Their
	// mtimes predate the cached copy, so only the ETag keeps this from being a
	// stale 304.
	admin.mustDo("DELETE", "/api/artifacts/"+aid+"/versions/"+v2, nil, nil, http.StatusOK)
	resp = getWith(t, latest, map[string]string{
		"If-None-Match":     etagV2,
		"If-Modified-Since": "Fri, 01 Jan 2100 00:00:00 GMT",
	})
	if resp.StatusCode != http.StatusOK || !strings.Contains(body(t, resp), "hello v1") {
		t.Errorf("latest after rollback: %d", resp.StatusCode)
	}

	// Replacing the latest version's content in place changes the validator too
	r := admin.upload("PUT", "/api/artifacts/"+aid+"/versions/"+v1, zipFrom(t, map[string]string{"index.html": "<h1>hello v1b</h1>"}), nil)
	r.Body.Close()
	resp = getWith(t, latest, map[string]string{"If-None-Match": etagV1})
	if resp.StatusCode != http.StatusOK || !strings.Contains(body(t, resp), "hello v1b") {
		t.Errorf("latest after overwrite: %d", resp.StatusCode)
	}
}

func TestLatestAliasWithoutVersions(t *testing.T) {
	_, ts := testServer(t)
	admin := login(t, ts.URL, "admin@example.com", "admin-password")
	aid := createArtifact(t, admin, "empty", true)

	resp := get(t, ts.URL+"/artifacts/"+aid+"/latest/", "", "text/html")
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(body(t, resp), "no versions yet") {
		t.Errorf("latest page without versions: %d", resp.StatusCode)
	}
	resp = get(t, ts.URL+"/artifacts/"+aid, "", "text/html")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("artifact redirect without versions: %d", resp.StatusCode)
	}
	resp.Body.Close()
	admin.mustDo("GET", "/api/artifacts/"+aid+"/versions/latest", nil, nil, http.StatusNotFound)
	admin.mustDo("POST", "/api/artifacts/"+aid+"/versions/latest/db/query", map[string]any{"sql": "SELECT 1"}, nil, http.StatusNotFound)
}

// The API accepts "latest" wherever it takes a version id, and resolves it
// before anything touches storage: data lands in the real version's database
// and files dir, never in one named "latest".
func TestLatestAliasAPI(t *testing.T) {
	s, ts := testServer(t)
	admin := login(t, ts.URL, "admin@example.com", "admin-password")
	aid := createArtifact(t, admin, "demo", true)
	v1 := pushVersion(t, admin, aid, "v1")
	v2 := pushVersion(t, admin, aid, "v2")
	alias := "/api/artifacts/" + aid + "/versions/latest"
	real := func(vid string) string { return "/api/artifacts/" + aid + "/versions/" + vid }

	var got versionID
	admin.mustDo("GET", alias, nil, &got, http.StatusOK)
	if got.ID != v2 {
		t.Fatalf("versions/latest = %s, want %s", got.ID, v2)
	}

	// Database: written through the alias, read back through the real id
	admin.mustDo("POST", alias+"/db/batch", map[string]any{"statements": []map[string]any{
		{"sql": "CREATE TABLE notes (body TEXT)"},
		{"sql": "INSERT INTO notes VALUES (?)", "params": []any{"via alias"}},
	}}, nil, http.StatusOK)
	var res struct {
		Rows [][]any `json:"rows"`
	}
	admin.mustDo("POST", real(v2)+"/db/query", map[string]any{"sql": "SELECT body FROM notes"}, &res, http.StatusOK)
	if len(res.Rows) != 1 || res.Rows[0][0] != "via alias" {
		t.Errorf("alias write not visible on %s: %+v", v2, res.Rows)
	}
	// ...and the older version's database was left alone
	if r := admin.do("POST", real(v1)+"/db/query", map[string]any{"sql": "SELECT body FROM notes"}, nil); r.StatusCode == http.StatusOK {
		t.Errorf("alias write leaked into %s", v1)
	}
	// Anonymous reads work through the alias on a public artifact
	anon := &testClient{t: t, base: ts.URL}
	anon.mustDo("POST", alias+"/db/query", map[string]any{"sql": "SELECT body FROM notes"}, &res, http.StatusOK)
	if len(res.Rows) != 1 {
		t.Errorf("anonymous alias read: %+v", res.Rows)
	}
	if r := admin.doRaw("GET", alias+"/db/download", nil); r.StatusCode != http.StatusOK {
		t.Errorf("alias db download: %d", r.StatusCode)
	} else {
		r.Body.Close()
	}

	// Files: same story
	r := admin.doRaw("PUT", alias+"/files/notes/hello.txt", []byte("hello"))
	if r.StatusCode != http.StatusOK {
		t.Fatalf("alias file upload: %d", r.StatusCode)
	}
	r.Body.Close()
	var list []fileInfo
	admin.mustDo("GET", real(v2)+"/files", nil, &list, http.StatusOK)
	if len(list) != 1 || list[0].Path != "notes/hello.txt" {
		t.Errorf("alias upload not visible on %s: %+v", v2, list)
	}
	admin.mustDo("GET", alias+"/files", nil, &list, http.StatusOK)
	if len(list) != 1 {
		t.Errorf("alias file list: %+v", list)
	}
	if r := admin.doRaw("GET", alias+"/files/notes/hello.txt", nil); r.StatusCode != http.StatusOK || body(t, r) != "hello" {
		t.Errorf("alias file download: %d", r.StatusCode)
	}

	// Nothing on disk is named after the alias
	for _, p := range []string{
		s.layout.VersionDB(aid, latestVersionAlias),
		s.layout.VersionFilesDir(aid, latestVersionAlias),
	} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s exists: the alias reached storage unresolved", p)
		}
	}
	if _, err := os.Stat(s.layout.VersionDB(aid, v2)); err != nil {
		t.Errorf("real version database missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.layout.VersionFilesDir(aid, v2), "notes", "hello.txt")); err != nil {
		t.Errorf("real version file missing: %v", err)
	}

	// A newer version takes the alias over, starting from its own empty data
	v3 := pushVersion(t, admin, aid, "v3")
	admin.mustDo("GET", alias, nil, &got, http.StatusOK)
	if got.ID != v3 {
		t.Errorf("versions/latest after push = %s, want %s", got.ID, v3)
	}
	admin.mustDo("GET", alias+"/files", nil, &list, http.StatusOK)
	if len(list) != 0 {
		t.Errorf("new latest version should start without files: %+v", list)
	}
}

// Private artifacts: the alias opens nothing the real id would not.
func TestLatestAliasPrivate(t *testing.T) {
	_, ts := testServer(t)
	admin, aid, _ := setupArtifact(t, ts.URL, false)
	anon := &testClient{t: t, base: ts.URL}

	resp := get(t, ts.URL+"/artifacts/"+aid+"/latest/", "", "text/html")
	if resp.StatusCode != http.StatusFound || !strings.HasPrefix(resp.Header.Get("Location"), "/login?next=") {
		t.Errorf("private latest page: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp.Body.Close()
	resp = get(t, ts.URL+"/artifacts/"+aid+"/latest/", admin.token, "text/html")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("authed private latest page: %d", resp.StatusCode)
	}
	resp.Body.Close()

	alias := fmt.Sprintf("/api/artifacts/%s/versions/latest", aid)
	anon.mustDo("GET", alias, nil, nil, http.StatusUnauthorized)
	anon.mustDo("POST", alias+"/db/query", map[string]any{"sql": "SELECT 1"}, nil, http.StatusUnauthorized)
	anon.mustDo("GET", alias+"/files", nil, nil, http.StatusUnauthorized)
	// Writes through the alias need auth like any other write
	anon.mustDo("POST", alias+"/db/batch", map[string]any{"statements": []map[string]any{{"sql": "SELECT 1"}}}, nil, http.StatusUnauthorized)
}
