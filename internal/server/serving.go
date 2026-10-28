package server

import (
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/aloisdeniel/cairn/internal/server/web"
	"github.com/aloisdeniel/cairn/internal/store"
)

// pageAuth gates artifact page loads: public artifacts are open, private ones
// redirect browsers to the login page.
func (s *Server) pageAuth(w http.ResponseWriter, r *http.Request, a *store.Artifact) bool {
	if s.canRead(r, a) {
		return true
	}
	next := url.QueryEscape(r.URL.RequestURI())
	http.Redirect(w, r, "/login?next="+next, http.StatusFound)
	return false
}

func artifactPageHeaders(w http.ResponseWriter) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Allow embedding only by our own shell frame.
	w.Header().Set("Content-Security-Policy", "frame-ancestors 'self'")
}

// resolvePageArtifact resolves the {id} segment for page routes (artifact id
// or resource reference), writing plain-text errors.
func (s *Server) resolvePageArtifact(w http.ResponseWriter, r *http.Request) *store.Artifact {
	a, err := s.resolveArtifactRef(r.PathValue("id"))
	if err != nil {
		var ambiguous errAmbiguousResource
		if errors.As(err, &ambiguous) {
			http.Error(w, ambiguous.Error(), http.StatusConflict)
			return nil
		}
		http.NotFound(w, r)
		return nil
	}
	return a
}

// handleArtifactRedirect sends /artifacts/{id} to the latest version's
// canonical URL so every relative asset resolves inside one version. The {id}
// may be a resource reference (e.g. a Claude session id); the redirect
// canonicalizes it to the artifact id.
func (s *Server) handleArtifactRedirect(w http.ResponseWriter, r *http.Request) {
	a := s.resolvePageArtifact(w, r)
	if a == nil {
		return
	}
	if !s.pageAuth(w, r, a) {
		return
	}
	v, err := s.store.LatestVersion(a.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "this artifact has no versions yet", http.StatusNotFound)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/artifacts/"+a.ID+"/"+v.ID+"/", http.StatusFound)
}

// handleVersionPage serves the files of one version. The path shape is
// /artifacts/{id}/{vid}/{path...} — a trailing-slash canonical base URL, so
// relative routing inside the SPA needs no rewriting at all.
func (s *Server) handleVersionPage(w http.ResponseWriter, r *http.Request) {
	a := s.resolvePageArtifact(w, r)
	if a == nil {
		return
	}
	if !s.pageAuth(w, r, a) {
		return
	}
	v, err := s.store.VersionByID(a.ID, r.PathValue("vid"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	rel := r.PathValue("path")
	artifactPageHeaders(w)
	s.serveVersionFile(w, r, a, v, rel)
}

// handleVersionNoSlash canonicalizes /artifacts/{id}/{vid} (no trailing
// slash) so relative asset paths resolve inside the version.
func (s *Server) handleVersionNoSlash(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, r.URL.Path+"/", http.StatusMovedPermanently)
}

func (s *Server) serveVersionFile(w http.ResponseWriter, r *http.Request, a *store.Artifact, v *store.Version, rel string) {
	root := s.layout.ContentDir(a.ID, v.ContentDir)
	clean, ok := cleanRequestPath(rel)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if clean == "" {
		clean = "index.html"
	}
	// cairn.js is always available inside a version's URL space so artifacts
	// can load it with a relative <script src="./cairn.js"> that also works
	// when the directory is opened locally (drop the file next to index.html).
	if clean == "cairn.js" {
		if _, err := os.Stat(filepath.Join(root, "cairn.js")); err != nil {
			s.serveCairnJS(w, r)
			return
		}
	}
	target := filepath.Join(root, filepath.FromSlash(clean))
	st, err := os.Stat(target)
	if err == nil && st.IsDir() {
		idx := filepath.Join(target, "index.html")
		if _, ierr := os.Stat(idx); ierr == nil {
			// Directories need a trailing slash for relative resolution.
			if !strings.HasSuffix(r.URL.Path, "/") {
				http.Redirect(w, r, r.URL.Path+"/", http.StatusMovedPermanently)
				return
			}
			target, st, err = idx, nil, nil
			if st, err = os.Stat(idx); err != nil {
				http.NotFound(w, r)
				return
			}
		} else {
			http.NotFound(w, r)
			return
		}
	}
	if err != nil {
		// SPA fallback: extension-less paths negotiated as HTML get
		// index.html; real assets 404 so missing files fail loudly.
		if path.Ext(clean) == "" && acceptsHTML(r) {
			http.ServeFile(w, r, filepath.Join(root, "index.html"))
			return
		}
		http.NotFound(w, r)
		return
	}
	if !st.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	// Defense in depth: never follow a path that escapes the version dir.
	if resolved, err := filepath.EvalSymlinks(target); err != nil || !strings.HasPrefix(resolved+string(filepath.Separator), mustEval(root)+string(filepath.Separator)) {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, target)
}

func mustEval(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

func acceptsHTML(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// Viewer: /shared/{id}[/{vid}] frames the fullscreen page with a toolbar
// and a side panel (details and versions, comments, data, source). Without
// {vid} it follows the latest version, switching as new ones are published.

type shellData struct {
	Artifact *store.Artifact
	Version  *store.Version
	Boot     map[string]any // the page script's initial state
}

// pageUser is what Cairn's pages know about the signed-in user.
func pageUser(u *store.User) map[string]any {
	if u == nil {
		return nil
	}
	return map[string]any{"id": u.ID, "name": u.Name, "email": u.Email, "isAdmin": u.IsAdmin}
}

func (s *Server) handleShared(w http.ResponseWriter, r *http.Request) {
	a := s.resolvePageArtifact(w, r)
	if a == nil {
		return
	}
	if !s.pageAuth(w, r, a) {
		return
	}
	versions, err := s.store.ListVersions(a.ID)
	if err != nil || len(versions) == 0 {
		http.Error(w, "this artifact has no versions yet", http.StatusNotFound)
		return
	}
	current := versions[0]
	if vid := r.PathValue("vid"); vid != "" {
		found := false
		for _, v := range versions {
			if v.ID == vid {
				current, found = v, true
				break
			}
		}
		if !found {
			http.NotFound(w, r)
			return
		}
	}
	u, _ := s.currentUser(r)
	a = s.withResources(a)
	boot := map[string]any{
		"artifact": a,
		"version":  current,
		"versions": versions,
		"follow":   r.PathValue("vid") == "",
		"user":     pageUser(u),
		"pinned":   u != nil && s.store.IsPinned(u.ID, a.ID),
	}
	data := shellData{Artifact: a, Version: current, Boot: boot}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if err := s.templates().ExecuteTemplate(w, "shell.html", data); err != nil {
		s.log.Error("render shell", "err", err)
	}
}

// Login / logout pages

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	// Already logged in? Straight through.
	if u, err := s.currentUser(r); err == nil && u != nil {
		http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusFound)
		return
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if err := s.templates().ExecuteTemplate(w, "login.html", map[string]any{
		"Next":   safeNext(r.URL.Query().Get("next")),
		"Google": s.googleEnabled(),
	}); err != nil {
		s.log.Error("render login", "err", err)
	}
}

func (s *Server) handleLogoutPage(w http.ResponseWriter, r *http.Request) {
	s.clearSessionCookie(w)
	http.Redirect(w, r, "/login", http.StatusFound)
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	u, err := s.currentUser(r)
	if err != nil || u == nil {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	// The gallery: every signed-in user's home, admin or not.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if err := s.templates().ExecuteTemplate(w, "gallery.html", map[string]any{
		"User": u,
		"Boot": map[string]any{"user": pageUser(u), "mcpUrl": s.mcpResource()},
	}); err != nil {
		s.log.Error("render gallery", "err", err)
	}
}

// serveUIAsset serves the stylesheet and helpers Cairn's pages share.
func (s *Server) serveUIAsset(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	data, err := web.UI.ReadFile(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	switch path.Ext(name) {
	case ".css":
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case ".js":
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if name == "html2canvas.min.js" {
		// Vendored at a fixed version: it only changes with Cairn itself.
		w.Header().Set("Cache-Control", "public, max-age=604800")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	w.Write(data)
}

// handleAdminPage serves the admin UI; the page itself talks to the JSON
// APIs. Anonymous visitors are sent to login, non-admins get a 403.
func (s *Server) handleAdminPage(w http.ResponseWriter, r *http.Request) {
	u, err := s.currentUser(r)
	if err != nil || u == nil {
		http.Redirect(w, r, "/login?next=/admin", http.StatusFound)
		return
	}
	if !u.IsAdmin {
		http.Error(w, "admin access required", http.StatusForbidden)
		return
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if err := s.templates().ExecuteTemplate(w, "admin.html", map[string]any{
		"Google": s.googleEnabled(),
	}); err != nil {
		s.log.Error("render admin", "err", err)
	}
}

func (s *Server) serveCairnJS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(web.CairnJS)
}

// templates parses the embedded HTML templates once.
func (s *Server) templates() *template.Template {
	s.tmplOnce.Do(func() {
		s.tmpl = template.Must(template.ParseFS(web.Templates, "*.html"))
	})
	return s.tmpl
}
