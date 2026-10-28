package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/aloisdeniel/cairn/internal/store"
)

// Comment notifications over ntfy (https://ntfy.sh): each user picks their
// own topic in the gallery (user menu → Notifications…) and subscribes to it
// in the ntfy app. A new comment notifies the artifact's creator, the users
// who pinned it and the earlier participants of the thread, never its
// author. Publishing happens in the background: a slow or failing ntfy never
// fails the comment.

const (
	notifyTimeout   = 10 * time.Second
	notifyBodyRunes = 500
)

// ntfy topics are public names: whoever knows one can read it, so users
// should pick something hard to guess.
var ntfyTopicRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ntfyMessage is ntfy's JSON publish API (POST <ntfy-url>/), which, unlike
// the header-based one, carries UTF-8 titles.
type ntfyMessage struct {
	Topic   string   `json:"topic"`
	Title   string   `json:"title,omitempty"`
	Message string   `json:"message"`
	Click   string   `json:"click,omitempty"`
	Tags    []string `json:"tags,omitempty"`
}

func (s *Server) ntfyPublish(ctx context.Context, m ntfyMessage) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(s.cfg.NtfyURL, "/")+"/", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if s.cfg.NtfyToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.cfg.NtfyToken)
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("ntfy: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

// artifactTitle is the title the gallery shows: the description when it is
// short enough (in UTF-16 units, like the page's JS .length), else the name.
func artifactTitle(a *store.Artifact) string {
	if a.Description != "" && len(utf16.Encode([]rune(a.Description))) <= 90 {
		return a.Description
	}
	return a.Name
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:n-1])) + "…"
}

func userLabel(u *store.User) string {
	if u.Name != "" {
		return u.Name
	}
	return u.Email
}

// notifyComment tells everyone interested in c (but its author) about it, in
// the background. s.notifyWG lets tests wait for the deliveries.
func (s *Server) notifyComment(a *store.Artifact, author *store.User, c *store.Comment) {
	targets, err := s.store.CommentNotifyTargets(a.ID, c.ParentID, author.ID)
	if err != nil {
		s.log.Error("comment notification recipients", "artifact", a.ID, "comment", c.ID, "err", err)
		return
	}
	if len(targets) == 0 {
		return
	}
	m := ntfyMessage{
		Title:   userLabel(author) + " on " + artifactTitle(a),
		Message: truncateRunes(c.Body, notifyBodyRunes),
		Tags:    []string{"speech_balloon"},
	}
	if s.cfg.BaseURL != "" {
		m.Click = strings.TrimRight(s.cfg.BaseURL, "/") + "/shared/" + a.ID + "#comments"
	}
	// One publish per recipient, in parallel, each with its own timeout: a
	// slow one doesn't eat the others' budget.
	for _, t := range targets {
		m := m
		m.Topic = t.Topic
		s.notifyWG.Add(1)
		go func() {
			defer s.notifyWG.Done()
			ctx, cancel := context.WithTimeout(context.Background(), notifyTimeout)
			defer cancel()
			if err := s.ntfyPublish(ctx, m); err != nil {
				s.log.Warn("comment notification failed", "artifact", a.ID, "comment", c.ID, "user", t.UserID, "err", err)
			}
		}()
	}
}

// settings is the signed-in user's own preferences (GET|PATCH
// /api/me/settings). NtfyURL is the server's, so the page can link to the
// topic.
type settings struct {
	NtfyTopic string `json:"ntfyTopic"`
	NtfyURL   string `json:"ntfyUrl"`
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	topic, err := s.store.NtfyTopic(requestUser(r).ID)
	if err != nil {
		s.writeStoreError(w, err, "user")
		return
	}
	writeJSON(w, http.StatusOK, settings{NtfyTopic: topic, NtfyURL: s.cfg.NtfyURL})
}

type settingsRequest struct {
	NtfyTopic *string `json:"ntfyTopic"`
}

// validTopic trims the topic and checks ntfy's charset; empty is valid (no
// notifications).
func validTopic(topic string) (string, bool) {
	topic = strings.TrimSpace(topic)
	return topic, topic == "" || ntfyTopicRe.MatchString(topic)
}

func (s *Server) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	u := requestUser(r)
	var req settingsRequest
	if !readJSON(w, r, &req) {
		return
	}
	if req.NtfyTopic != nil {
		topic, ok := validTopic(*req.NtfyTopic)
		if !ok {
			writeError(w, http.StatusBadRequest, "ntfy topics are 1 to 64 letters, digits, - or _")
			return
		}
		if err := s.store.SetNtfyTopic(u.ID, topic); err != nil {
			s.writeStoreError(w, err, "user")
			return
		}
	}
	s.handleGetSettings(w, r)
}

// handleTestNotification sends a test message, synchronously so the page can
// tell whether it worked: to {ntfyTopic} when given (try before saving),
// else to the saved topic.
func (s *Server) handleTestNotification(w http.ResponseWriter, r *http.Request) {
	u := requestUser(r)
	var req settingsRequest
	// The body is optional.
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil && err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	var topic string
	if req.NtfyTopic != nil {
		var ok bool
		if topic, ok = validTopic(*req.NtfyTopic); !ok {
			writeError(w, http.StatusBadRequest, "ntfy topics are 1 to 64 letters, digits, - or _")
			return
		}
	} else {
		var err error
		if topic, err = s.store.NtfyTopic(u.ID); err != nil {
			s.writeStoreError(w, err, "user")
			return
		}
	}
	if topic == "" {
		writeError(w, http.StatusBadRequest, "no ntfy topic set")
		return
	}
	m := ntfyMessage{
		Topic:   topic,
		Title:   "Cairn notifications work",
		Message: "New comments on your artifacts, the ones you pinned and the threads you wrote in will show up here.",
		Tags:    []string{"speech_balloon"},
	}
	if s.cfg.BaseURL != "" {
		m.Click = strings.TrimRight(s.cfg.BaseURL, "/") + "/"
	}
	ctx, cancel := context.WithTimeout(r.Context(), notifyTimeout)
	defer cancel()
	if err := s.ntfyPublish(ctx, m); err != nil {
		s.log.Warn("test notification failed", "user", u.Email, "err", err)
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
