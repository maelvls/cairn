package server

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/aloisdeniel/cairn/internal/store"
)

// Thumbnails: the viewer captures what a signed-in user sees (html2canvas,
// in the browser) and uploads it, so the gallery shows an image instead of
// running every artifact in a live iframe. One image per version, stored at
// thumbs/{artifactID}/{versionID}.{ext}; a re-upload of the version deletes
// it, and one older than the version's updatedAt counts as missing.

const maxThumbBytes = 1 << 20

// thumbFormats are the accepted image types, in lookup order on disk
// (normally only one file exists).
var thumbFormats = []struct{ mime, ext string }{
	{"image/webp", ".webp"},
	{"image/jpeg", ".jpg"},
	{"image/png", ".png"},
}

// freshThumb returns the version's thumbnail file when there is one at least
// as recent as the version's content. One directory read per artifact: most
// have no thumbnail directory at all until someone views them.
func (s *Server) freshThumb(v *store.Version) (string, fs.FileInfo, bool) {
	dir := s.layout.ArtifactThumbsRoot(v.ArtifactID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", nil, false
	}
	names := make(map[string]fs.DirEntry, len(entries))
	for _, e := range entries {
		names[e.Name()] = e
	}
	for _, f := range thumbFormats {
		e, ok := names[v.ID+f.ext]
		if !ok || !e.Type().IsRegular() {
			continue
		}
		st, err := e.Info()
		if err != nil {
			continue
		}
		// Second precision on both sides: Last-Modified, which the viewer
		// sees, has no finer resolution.
		if updated, err := time.Parse(time.RFC3339Nano, v.UpdatedAt); err == nil &&
			st.ModTime().Truncate(time.Second).Before(updated.Truncate(time.Second)) {
			return "", nil, false
		}
		return filepath.Join(dir, e.Name()), st, true
	}
	return "", nil, false
}

// thumbURL is the thumbnail's API URL, cache-busted by its modification time.
func thumbURL(v *store.Version, st fs.FileInfo) string {
	return fmt.Sprintf("/api/artifacts/%s/versions/%s/thumbnail?v=%d", v.ArtifactID, v.ID, st.ModTime().UnixMilli())
}

// removeThumbs deletes a version's thumbnail files, whatever their type,
// except the one with extension keep ("" removes them all).
func (s *Server) removeThumbs(artifactID, versionID, keep string) {
	for _, f := range thumbFormats {
		if f.ext != keep {
			os.Remove(s.layout.VersionThumb(artifactID, versionID, f.ext))
		}
	}
}

func (s *Server) handleThumbnailGet(w http.ResponseWriter, r *http.Request) {
	v, err := s.store.VersionByID(requestArtifact(r).ID, r.PathValue("vid"))
	if err != nil {
		s.writeStoreError(w, err, "version")
		return
	}
	p, st, ok := s.freshThumb(v)
	var f *os.File
	if ok {
		f, err = os.Open(p)
	}
	if !ok || err != nil {
		writeError(w, http.StatusNotFound, "no thumbnail for this version")
		return
	}
	defer f.Close()
	// Cache-busted URLs (?v=mtime, from the artifact list) can be kept;
	// bare ones are revalidated so a new capture shows up at once.
	if r.URL.Query().Has("v") {
		w.Header().Set("Cache-Control", "private, max-age=86400")
	} else {
		w.Header().Set("Cache-Control", "private, no-cache")
	}
	w.Header().Set("ETag", fmt.Sprintf(`"%x-%x"`, st.ModTime().UnixNano(), st.Size()))
	w.Header().Set("Content-Type", mime.TypeByExtension(filepath.Ext(p)))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, "", st.ModTime(), f)
}

// handleThumbnailPut stores the raw body as the version's thumbnail. The
// declared Content-Type must be an accepted image type and match the bytes.
// ?updatedAt= (the version's updatedAt the capture was taken from) guards
// against a capture of replaced content landing after the re-upload: 409.
func (s *Server) handleThumbnailPut(w http.ResponseWriter, r *http.Request) {
	v, err := s.store.VersionByID(requestArtifact(r).ID, r.PathValue("vid"))
	if err != nil {
		s.writeStoreError(w, err, "version")
		return
	}
	if at := r.URL.Query().Get("updatedAt"); at != "" && at != v.UpdatedAt {
		writeError(w, http.StatusConflict, "the version's content changed since this capture")
		return
	}
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	ext := ""
	for _, f := range thumbFormats {
		if f.mime == ct {
			ext = f.ext
		}
	}
	if ext == "" {
		writeError(w, http.StatusUnsupportedMediaType, "thumbnail must be image/webp, image/jpeg or image/png")
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxThumbBytes))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "thumbnail exceeds 1 MB")
			return
		}
		writeError(w, http.StatusBadRequest, "failed to read thumbnail")
		return
	}
	if http.DetectContentType(data) != ct {
		writeError(w, http.StatusUnsupportedMediaType, "thumbnail content is not "+ct)
		return
	}
	// Stage in tmp/ and rename into place (same volume): readers never see
	// a partial image.
	tmp, err := os.CreateTemp(s.layout.TmpRoot(), "thumb-*")
	if err != nil {
		s.log.Error("stage thumbnail", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(data)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	target := s.layout.VersionThumb(v.ArtifactID, v.ID, ext)
	if err == nil {
		os.Chmod(tmp.Name(), 0o644)
		err = os.MkdirAll(filepath.Dir(target), 0o755)
	}
	if err == nil {
		// A previous capture in another format would shadow this one.
		// Removed before the rename, so concurrent uploads in different
		// formats can leave two files but never none.
		s.removeThumbs(v.ArtifactID, v.ID, ext)
		err = os.Rename(tmp.Name(), target)
	}
	if err != nil {
		s.log.Error("store thumbnail", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	st, err := os.Stat(target)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"thumbnail": thumbURL(v, st), "size": len(data)})
}
