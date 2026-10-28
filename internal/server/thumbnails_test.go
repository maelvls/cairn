package server

import (
	"bytes"
	"image"
	"image/png"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aloisdeniel/cairn/internal/store"
)

func putThumb(t *testing.T, c *testClient, path, contentType string, data []byte) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("PUT", c.base+path, bytes.NewReader(data))
	req.Header.Set("Content-Type", contentType)
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func tinyPNG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 4, 4))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// latestThumb returns the thumbnail URL the artifact list gives aid.
func latestThumb(t *testing.T, c *testClient, aid string) string {
	t.Helper()
	var list []*store.ArtifactSummary
	c.mustDo("GET", "/api/artifacts", nil, &list, http.StatusOK)
	for _, a := range list {
		if a.ID == aid && a.LatestVersion != nil {
			return a.LatestVersion.Thumbnail
		}
	}
	t.Fatalf("artifact %s not listed", aid)
	return ""
}

func TestThumbnails(t *testing.T) {
	s, ts := testServer(t)
	admin, pub, pubV := setupArtifact(t, ts.URL, true)
	anon := &testClient{t: t, base: ts.URL}
	thumb := "/api/artifacts/" + pub + "/versions/" + pubV + "/thumbnail"
	img := tinyPNG(t)

	// Absent at first.
	if resp := get(t, ts.URL+thumb, admin.token, ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing thumbnail: %d", resp.StatusCode)
	}
	if u := latestThumb(t, admin, pub); u != "" {
		t.Fatalf("summary before capture: %q", u)
	}

	// Writes need a signed-in user; types and sizes are checked.
	if resp := putThumb(t, anon, thumb, "image/png", img); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous PUT: %d", resp.StatusCode)
	}
	if resp := putThumb(t, admin, thumb, "image/gif", []byte("GIF89a")); resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("gif: %d", resp.StatusCode)
	}
	if resp := putThumb(t, admin, thumb, "image/jpeg", img); resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("PNG bytes declared as JPEG: %d", resp.StatusCode)
	}
	if resp := putThumb(t, admin, thumb, "image/png", []byte("<svg></svg>")); resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("not an image: %d", resp.StatusCode)
	}
	big := append(append([]byte{}, img...), make([]byte, maxThumbBytes)...)
	if resp := putThumb(t, admin, thumb, "image/png", big); resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized: %d", resp.StatusCode)
	}
	if resp := putThumb(t, admin, "/api/artifacts/"+pub+"/versions/nope/thumbnail", "image/png", img); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown version: %d", resp.StatusCode)
	}

	// A capture of content that was replaced since is refused.
	if resp := putThumb(t, admin, thumb+"?updatedAt=2000-01-01T00:00:00.000Z", "image/png", img); resp.StatusCode != http.StatusConflict {
		t.Errorf("capture of replaced content: %d", resp.StatusCode)
	}

	// A non-admin user can capture too. Reading needs a session even on a
	// public artifact: the capture can show who was viewing it.
	bob := newUser(t, ts.URL, admin, "bob@example.com", "Bob")
	var v store.Version
	bob.mustDo("GET", "/api/artifacts/"+pub+"/versions/"+pubV, nil, &v, http.StatusOK)
	if resp := putThumb(t, bob, thumb+"?updatedAt="+url.QueryEscape(v.UpdatedAt), "image/png; charset=binary", img); resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT: %d", resp.StatusCode)
	}
	if resp := get(t, ts.URL+thumb, "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous GET on public: %d", resp.StatusCode)
	}
	resp := get(t, ts.URL+thumb, admin.token, "")
	if got := body(t, resp); resp.StatusCode != http.StatusOK || got != string(img) || resp.Header.Get("Content-Type") != "image/png" {
		t.Fatalf("GET on public: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.HasPrefix(cc, "private") || resp.Header.Get("ETag") == "" || resp.Header.Get("Last-Modified") == "" {
		t.Errorf("cache headers: %q etag=%q", cc, resp.Header.Get("ETag"))
	}
	u := latestThumb(t, bob, pub)
	if !strings.HasPrefix(u, thumb+"?v=") {
		t.Fatalf("summary thumbnail: %q", u)
	}
	if resp := get(t, ts.URL+u, bob.token, ""); resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Cache-Control"), "max-age=86400") {
		t.Errorf("cache-busted GET: %d %q", resp.StatusCode, resp.Header.Get("Cache-Control"))
	}

	// A new capture in another format replaces the previous one.
	webp := append([]byte("RIFF\x10\x00\x00\x00WEBPVP8 "), make([]byte, 16)...)
	if resp := putThumb(t, admin, thumb, "image/webp", webp); resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT webp: %d", resp.StatusCode)
	}
	if _, err := os.Stat(s.layout.VersionThumb(pub, pubV, ".png")); !os.IsNotExist(err) {
		t.Errorf("previous png kept: %v", err)
	}
	if resp := get(t, ts.URL+thumb, bob.token, ""); resp.Header.Get("Content-Type") != "image/webp" || body(t, resp) != string(webp) {
		t.Errorf("webp GET: %s", resp.Header.Get("Content-Type"))
	}

	// A thumbnail older than the version's content is not served.
	old := time.Now().Add(-time.Hour)
	os.Chtimes(s.layout.VersionThumb(pub, pubV, ".webp"), old, old)
	if resp := get(t, ts.URL+thumb, bob.token, ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("stale thumbnail: %d", resp.StatusCode)
	}
	if u := latestThumb(t, admin, pub); u != "" {
		t.Errorf("stale thumbnail listed: %q", u)
	}

	// Re-uploading the version deletes it.
	putThumb(t, admin, thumb, "image/png", img)
	resp = admin.upload("PUT", "/api/artifacts/"+pub+"/versions/"+pubV, zipFrom(t, map[string]string{"index.html": "<h1>v1 again</h1>"}), nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("replace: %d", resp.StatusCode)
	}
	if resp := get(t, ts.URL+thumb, bob.token, ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("thumbnail after replace: %d", resp.StatusCode)
	}

	// Private artifacts: anonymous reads are refused.
	priv := createArtifact(t, admin, "private", false)
	resp = admin.upload("POST", "/api/artifacts/"+priv+"/versions", zipFrom(t, map[string]string{"index.html": "<h1>p</h1>"}), nil)
	privV := decode[struct {
		ID string `json:"id"`
	}](t, resp).ID
	privThumb := "/api/artifacts/" + priv + "/versions/" + privV + "/thumbnail"
	putThumb(t, admin, privThumb, "image/png", img)
	if resp := get(t, ts.URL+privThumb, "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous GET on private: %d", resp.StatusCode)
	}
	if resp := get(t, ts.URL+privThumb, bob.token, ""); resp.StatusCode != http.StatusOK {
		t.Errorf("signed-in GET on private: %d", resp.StatusCode)
	}

	// Deleting the version, then the artifact, deletes the thumbnails.
	putThumb(t, admin, thumb, "image/png", img)
	admin.mustDo("DELETE", "/api/artifacts/"+pub+"/versions/"+pubV, nil, nil, http.StatusOK)
	if _, err := os.Stat(s.layout.VersionThumb(pub, pubV, ".png")); !os.IsNotExist(err) {
		t.Errorf("thumbnail survived its version: %v", err)
	}
	admin.mustDo("DELETE", "/api/artifacts/"+priv, nil, nil, http.StatusOK)
	if _, err := os.Stat(s.layout.ArtifactThumbsRoot(priv)); !os.IsNotExist(err) {
		t.Errorf("thumbnails survived their artifact: %v", err)
	}

	// The vendored capture library is served.
	resp = get(t, ts.URL+"/_ui/html2canvas.min.js", "", "")
	if got := body(t, resp); resp.StatusCode != http.StatusOK || !strings.Contains(got, "html2canvas 1.4.1") {
		t.Errorf("html2canvas: %d", resp.StatusCode)
	}
}
