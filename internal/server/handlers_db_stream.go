package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aloisdeniel/cairn/internal/versiondb"
)

// Server-Sent Events stream of one SQL query's result:
//   POST /api/artifacts/{id}/versions/{vid}/db/stream
// Body: {"sql": "...", "params": [...], "coalesceMs": 50}. Response is plain
// SSE: a first `db.init` event carries the query's initial result, then one
// `db.result` event per write committed to this version's database. Writes
// that land within coalesceMs of each other are merged into a single
// re-execution, so a batch that touches many rows produces one event, not
// one per row. A 15-second comment line keeps intermediaries from timing the
// connection out.
//
// SELECT read-only queries are the intended use. The stream itself is
// signed-in users only (SSE eats a goroutine for its whole lifetime so we
// never open that door to anonymous callers), but inside each tick the query
// is run on the read-write pool — same semantics as `cairn db query` for an
// authenticated caller. There is no SQL parsing anywhere in this path; if a
// caller wants to UPDATE from inside the watch, they can.
//
// Last-Event-ID: the bus is in-memory, so it skips events older than the
// cursor the client already saw this session (same process), just like
// handleCommentsStream.

const (
	defaultCoalesce = 50 * time.Millisecond
	maxCoalesce     = 5 * time.Second
	sseMaxSQLBytes  = 32 * 1024 // same order as CLI flag; avoids pathological queries
)

type dbStreamRequest struct {
	SQL        string          `json:"sql"`
	Params     json.RawMessage `json:"params"` // passed through to versiondb.Statement
	CoalesceMs *int64          `json:"coalesceMs"`
}

func (s *Server) handleDBStream(w http.ResponseWriter, r *http.Request) {
	aid, ok := s.resolveVersion(w, r)
	if !ok {
		return
	}
	vid := r.PathValue("vid")

	var req dbStreamRequest
	if !readJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.SQL) == "" {
		writeError(w, http.StatusBadRequest, "sql is required")
		return
	}
	if len(req.SQL) > sseMaxSQLBytes {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("sql is %d bytes; limit is %d", len(req.SQL), sseMaxSQLBytes))
		return
	}
	stmt := versiondb.Statement{SQL: req.SQL}
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &stmt.Params); err != nil {
			writeError(w, http.StatusBadRequest, "invalid params: "+err.Error())
			return
		}
	}
	coalesce := defaultCoalesce
	if req.CoalesceMs != nil {
		if *req.CoalesceMs < 0 {
			writeError(w, http.StatusBadRequest, "coalesceMs must be >= 0")
			return
		}
		coalesce = time.Duration(*req.CoalesceMs) * time.Millisecond
		if coalesce > maxCoalesce {
			coalesce = maxCoalesce
		}
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	fmt.Fprintf(w, ": watching %s/%s\n\n", aid, vid)
	flusher.Flush()

	ctx := r.Context()

	// Run the query once and emit the initial state as a db.init event.
	// Runs on the read-only pool so the watch itself does not fire the write
	// hook (which would loop: re-query → hook → re-query). A caller who
	// wants to UPDATE from inside the loop can just call /db/query.
	runAndEmit := func(name string) bool {
		res, err := s.dbs.Exec(ctx, aid, vid, false, stmt)
		if err != nil {
			s.emitDBError(w, flusher, err)
			return false
		}
		return s.emitDBResult(w, flusher, name, aid, vid, res)
	}
	if !runAndEmit("db.init") {
		return
	}

	ch, cancel := s.bus.Subscribe(artifactTopic(aid))
	defer cancel()

	lastSeen := uint64(0)
	if hv := r.Header.Get("Last-Event-ID"); hv != "" {
		if n, err := strconv.ParseUint(hv, 10, 64); err == nil {
			lastSeen = n
		}
	}

	heartbeat := time.NewTicker(sseHeartbeatInterval)
	defer heartbeat.Stop()

	// Coalescing timer: starts on the first write event, re-runs the query
	// when it fires. Writes that arrive while it is armed are absorbed.
	var coalesceTimer *time.Timer
	var coalesceFire <-chan time.Time
	armCoalesce := func() {
		if coalesceTimer != nil {
			return
		}
		coalesceTimer = time.NewTimer(coalesce)
		coalesceFire = coalesceTimer.C
	}
	disarm := func() {
		if coalesceTimer != nil {
			coalesceTimer.Stop()
			coalesceTimer = nil
			coalesceFire = nil
		}
	}
	defer disarm()

	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-coalesceFire:
			disarm()
			if !runAndEmit("db.result") {
				return
			}
		case e, okCh := <-ch:
			if !okCh {
				return
			}
			if e.Name != "db.write" {
				continue
			}
			if !dbEventMatchesVersion(e, vid) {
				continue
			}
			if n, err := strconv.ParseUint(e.ID, 10, 64); err == nil && n <= lastSeen {
				continue
			}
			armCoalesce()
		}
	}
}

// dbEventMatchesVersion returns true when the event's versionId matches vid,
// or when the event carries no version metadata (be permissive on legacy
// publishers).
func dbEventMatchesVersion(e Event, vid string) bool {
	if len(e.Data) == 0 {
		return true
	}
	var p struct {
		VersionID string `json:"versionId"`
	}
	if err := json.Unmarshal(e.Data, &p); err != nil {
		return true
	}
	return p.VersionID == "" || p.VersionID == vid
}

// emitDBResult writes one `name` event with the query result as data.
func (s *Server) emitDBResult(w http.ResponseWriter, flusher http.Flusher, name, aid, vid string, res *versiondb.Result) bool {
	id := s.bus.nextEventID()
	envelope := map[string]any{
		"id":         id,
		"name":       name,
		"artifactId": aid,
		"versionId":  vid,
		"occurredAt": time.Now().UTC(),
		"result":     res,
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		return false
	}
	if _, err := fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", id, name, payload); err != nil {
		return false
	}
	flusher.Flush()
	return true
}

// emitDBError writes an `error` event and keeps the stream open so a later
// write that fixes the state can recover without the client reconnecting.
func (s *Server) emitDBError(w http.ResponseWriter, flusher http.Flusher, err error) {
	id := s.bus.nextEventID()
	payload, _ := json.Marshal(map[string]any{
		"id":    id,
		"name":  "error",
		"error": err.Error(),
	})
	fmt.Fprintf(w, "id: %s\nevent: error\ndata: %s\n\n", id, payload)
	flusher.Flush()
}
