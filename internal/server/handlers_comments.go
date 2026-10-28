package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/aloisdeniel/cairn/internal/store"
)

// Comments: threads on an artifact, for the people looking at it and the
// agents iterating on it (MCP list_comments/add_comment, `cairn comments`).
// Like everything authenticated, any user can comment, reply and resolve;
// editing a comment is its author's, deleting it its author's or an admin's.
// Anonymous visitors of public artifacts don't see comments.

const maxCommentRunes = 20_000

const viaCtxKey ctxKey = 101

// withVia marks requests replayed by the MCP endpoint so comments record
// that an agent wrote them.
func withVia(r *http.Request, via string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), viaCtxKey, via))
}

// requestVia is how a request reached the API: "mcp" (an MCP tool), "api"
// (a bearer token: the CLI, scripts) or "web" (the session cookie).
func requestVia(r *http.Request) string {
	if v, ok := r.Context().Value(viaCtxKey).(string); ok {
		return v
	}
	if r.Header.Get("Authorization") != "" {
		return "api"
	}
	return "web"
}

func (s *Server) handleListComments(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	switch status {
	case "", "all", "open", "resolved":
	default:
		writeError(w, http.StatusBadRequest, "status must be open, resolved or all")
		return
	}
	cs, err := s.store.ListComments(requestArtifact(r).ID, status)
	if err != nil {
		s.writeStoreError(w, err, "comments")
		return
	}
	writeJSON(w, http.StatusOK, cs)
}

type commentRequest struct {
	Body      *string `json:"body"`
	ParentID  string  `json:"parentId"`
	VersionID string  `json:"versionId"`
	Resolved  *bool   `json:"resolved"`
}

func validCommentBody(body *string) (string, string) {
	if body == nil || strings.TrimSpace(*body) == "" {
		return "", "body is required"
	}
	if utf8.RuneCountInString(*body) > maxCommentRunes {
		return "", "body is too long"
	}
	return strings.TrimSpace(*body), ""
}

func (s *Server) handleCreateComment(w http.ResponseWriter, r *http.Request) {
	a, u := requestArtifact(r), requestUser(r)
	var req commentRequest
	if !readJSON(w, r, &req) {
		return
	}
	body, msg := validCommentBody(req.Body)
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	c := &store.Comment{ArtifactID: a.ID, UserID: u.ID, Via: requestVia(r), Body: body}
	if req.ParentID != "" {
		parent, err := s.store.CommentByID(a.ID, req.ParentID)
		if err != nil {
			s.writeStoreError(w, err, "parent comment")
			return
		}
		// Threads are flat: a reply to a reply joins the root's thread.
		c.ParentID = parent.ID
		if parent.ParentID != "" {
			c.ParentID = parent.ParentID
		}
	}
	if req.VersionID != "" {
		if _, err := s.store.VersionByID(a.ID, req.VersionID); err != nil {
			s.writeStoreError(w, err, "version")
			return
		}
		c.VersionID = req.VersionID
	}
	c, err := s.store.CreateComment(c)
	if err != nil {
		s.writeStoreError(w, err, "comment")
		return
	}
	if c.ParentID != "" {
		// A reply reopens a resolved thread: there is something new to read.
		s.store.ResolveComment(a.ID, c.ParentID, "")
	}
	s.log.Info("comment added", "artifact", a.ID, "comment", c.ID, "via", c.Via, "by", u.Email)
	s.notifyComment(a, u, c)
	writeJSON(w, http.StatusCreated, c)
}

func (s *Server) handleUpdateComment(w http.ResponseWriter, r *http.Request) {
	a, u := requestArtifact(r), requestUser(r)
	c, err := s.store.CommentByID(a.ID, r.PathValue("cid"))
	if err != nil {
		s.writeStoreError(w, err, "comment")
		return
	}
	var req commentRequest
	if !readJSON(w, r, &req) {
		return
	}
	if req.Body != nil {
		if c.UserID != u.ID {
			writeError(w, http.StatusForbidden, "only the author can edit a comment")
			return
		}
		body, msg := validCommentBody(req.Body)
		if msg != "" {
			writeError(w, http.StatusBadRequest, msg)
			return
		}
		if err := s.store.UpdateCommentBody(a.ID, c.ID, body); err != nil {
			s.writeStoreError(w, err, "comment")
			return
		}
	}
	if req.Resolved != nil {
		root := c.ID
		if c.ParentID != "" {
			root = c.ParentID
		}
		by := ""
		if *req.Resolved {
			by = u.ID
		}
		if err := s.store.ResolveComment(a.ID, root, by); err != nil {
			s.writeStoreError(w, err, "comment")
			return
		}
	}
	c, err = s.store.CommentByID(a.ID, c.ID)
	if err != nil {
		s.writeStoreError(w, err, "comment")
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) handleDeleteComment(w http.ResponseWriter, r *http.Request) {
	a, u := requestArtifact(r), requestUser(r)
	c, err := s.store.CommentByID(a.ID, r.PathValue("cid"))
	if err != nil {
		s.writeStoreError(w, err, "comment")
		return
	}
	if c.UserID != u.ID && !u.IsAdmin {
		writeError(w, http.StatusForbidden, "only the author or an admin can delete a comment")
		return
	}
	if err := s.store.DeleteComment(a.ID, c.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.writeStoreError(w, err, "comment")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
