package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// Server-Sent Events stream of comment activity on one artifact:
// GET /api/artifacts/{id}/comments/stream. Subscribers see the events the
// bus publishes (comment.created, comment.resolved, comment.reopened,
// comment.updated) as they happen. It is meant to power `cairn comments
// watch` — a CLI replacement for polling or /loop.
//
// Transport: plain SSE (`text/event-stream`). The server sends a comment on
// its own ("init") so the client knows the stream is live; after that each
// event is a `comment.<kind>` message carrying the Comment JSON as data.
// A 15-second comment line keeps intermediaries from timing the connection
// out. On a reconnect the client may send Last-Event-ID — the id is
// process-local, so this skips events published since connection time but
// not those produced by a previous server process (which is fine: the state
// stays canonical in the HTTP API, a client that cares reconciles there).

const sseHeartbeatInterval = 15 * time.Second

func (s *Server) handleCommentsStream(w http.ResponseWriter, r *http.Request) {
	a := requestArtifact(r)
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no") // disable proxy buffering (nginx, caddy)
	w.WriteHeader(http.StatusOK)

	fmt.Fprintf(w, ": watching %s\n\n", a.ID)
	flusher.Flush()

	ch, cancel := s.bus.Subscribe(artifactTopic(a.ID))
	defer cancel()

	// Last-Event-ID replay would read from a persistent event log; the bus
	// is in-memory, so we honor it only to skip events older than the
	// cursor the client already saw this session (same process). Every
	// event's id is a monotonically-increasing counter.
	lastSeen := uint64(0)
	if h := r.Header.Get("Last-Event-ID"); h != "" {
		if n, err := strconv.ParseUint(h, 10, 64); err == nil {
			lastSeen = n
		}
	}

	heartbeat := time.NewTicker(sseHeartbeatInterval)
	defer heartbeat.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case e, ok := <-ch:
			if !ok {
				return
			}
			if n, err := strconv.ParseUint(e.ID, 10, 64); err == nil && n <= lastSeen {
				continue
			}
			if err := writeSSEEvent(w, e); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// writeSSEEvent writes one event frame. data is the raw payload (a Comment
// JSON today); we re-marshal a small envelope so the CLI can pull `name`,
// `id` and `artifactId` without parsing headers.
func writeSSEEvent(w http.ResponseWriter, e Event) error {
	envelope := map[string]any{
		"id":         e.ID,
		"name":       e.Name,
		"artifactId": e.ArtifactID,
		"occurredAt": e.OccurredAt,
		"data":       json.RawMessage(e.Data),
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", e.ID, e.Name, payload); err != nil {
		return err
	}
	return nil
}
