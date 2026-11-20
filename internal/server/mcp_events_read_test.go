package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

type pollResult struct {
	Result struct {
		Events []struct {
			EventID   string         `json:"eventId"`
			Name      string         `json:"name"`
			Timestamp string         `json:"timestamp"`
			Data      map[string]any `json:"data"`
		} `json:"events"`
		Cursor     string `json:"cursor"`
		Truncated  bool   `json:"truncated"`
		HasMore    bool   `json:"hasMore"`
		NextPollMs int    `json:"nextPollMs"`
	} `json:"result"`
	Error *rpcError `json:"error"`
}

func poll(t *testing.T, c *testClient, params map[string]any) pollResult {
	t.Helper()
	var out pollResult
	c.mustDo("POST", "/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "events/poll", "params": params}, &out, http.StatusOK)
	return out
}

func comment(t *testing.T, c *testClient, aid, body string) {
	t.Helper()
	c.mustDo("POST", "/api/artifacts/"+aid+"/comments", map[string]any{"body": body}, nil, http.StatusCreated)
}

// TestMCPEvents_Poll walks a cursor through comments on two artifacts.
func TestMCPEvents_Poll(t *testing.T) {
	_, ts := testServer(t)
	admin := login(t, ts.URL, "admin@example.com", "admin-password")
	a1 := createArtifact(t, admin, "one", false)
	a2 := createArtifact(t, admin, "two", false)

	comment(t, admin, a1, "before")

	// A null cursor starts from now: nothing, and a cursor to come back with.
	first := poll(t, admin, map[string]any{"name": "comment.created", "cursor": nil})
	if first.Error != nil || len(first.Result.Events) != 0 || first.Result.Cursor == "" || first.Result.Truncated {
		t.Fatalf("first poll: %+v", first)
	}
	if first.Result.NextPollMs <= 0 {
		t.Errorf("nextPollMs = %d", first.Result.NextPollMs)
	}
	c0 := first.Result.Cursor

	comment(t, admin, a1, "one-a")
	comment(t, admin, a2, "two-a")
	comment(t, admin, a1, "one-b")

	all := poll(t, admin, map[string]any{"name": "comment.created", "cursor": c0})
	if len(all.Result.Events) != 3 || all.Result.HasMore || all.Result.Truncated {
		t.Fatalf("poll after three comments: %+v", all)
	}
	seen := map[string]bool{}
	for _, e := range all.Result.Events {
		if e.Name != "comment.created" || !strings.HasPrefix(e.EventID, "evt_") || seen[e.EventID] {
			t.Errorf("event %+v", e)
		}
		seen[e.EventID] = true
		if _, err := time.Parse(time.RFC3339Nano, e.Timestamp); err != nil {
			t.Errorf("timestamp %q: %v", e.Timestamp, err)
		}
	}
	if got := all.Result.Events[2].Data["body"]; got != "one-b" {
		t.Errorf("last event's body = %v", got)
	}
	// Up to date: nothing more, the same cursor.
	if again := poll(t, admin, map[string]any{"name": "comment.created", "cursor": all.Result.Cursor}); len(again.Result.Events) != 0 || again.Result.Cursor != all.Result.Cursor {
		t.Errorf("poll at the head: %+v", again)
	}

	// The artifactId filter.
	only := poll(t, admin, map[string]any{"name": "comment.created", "arguments": map[string]any{"artifactId": a2}, "cursor": c0})
	if len(only.Result.Events) != 1 || only.Result.Events[0].Data["body"] != "two-a" {
		t.Errorf("filtered poll: %+v", only)
	}

	// maxEvents: a page, then the rest from its cursor.
	page := poll(t, admin, map[string]any{"name": "comment.created", "cursor": c0, "maxEvents": 2})
	if len(page.Result.Events) != 2 || !page.Result.HasMore {
		t.Fatalf("first page: %+v", page)
	}
	rest := poll(t, admin, map[string]any{"name": "comment.created", "cursor": page.Result.Cursor, "maxEvents": 2})
	if len(rest.Result.Events) != 1 || rest.Result.HasMore || rest.Result.Events[0].Data["body"] != "one-b" {
		t.Errorf("second page: %+v", rest)
	}

	// Another event name sees none of them.
	if other := poll(t, admin, map[string]any{"name": "comment.resolved", "cursor": c0}); len(other.Result.Events) != 0 {
		t.Errorf("comment.resolved poll: %+v", other)
	}

	// A cursor from another log (a restart): truncated, and on from now.
	stale := poll(t, admin, map[string]any{"name": "comment.created", "cursor": "deadbeef.1"})
	if !stale.Result.Truncated || len(stale.Result.Events) != 0 || stale.Result.Cursor != all.Result.Cursor {
		t.Errorf("stale cursor: %+v", stale)
	}

	for name, params := range map[string]map[string]any{
		"unknown event":    {"name": "comment.exploded"},
		"unknown argument": {"name": "comment.created", "arguments": map[string]any{"color": "red"}},
		"argument type":    {"name": "comment.created", "arguments": map[string]any{"artifactId": 3}},
	} {
		if out := poll(t, admin, params); out.Error == nil {
			t.Errorf("%s: no error", name)
		} else if name == "unknown event" && out.Error.Code != rpcEventNotFound {
			t.Errorf("%s: code %d", name, out.Error.Code)
		} else if name != "unknown event" && out.Error.Code != -32602 {
			t.Errorf("%s: code %d", name, out.Error.Code)
		}
	}
}

// TestEventBus_LogTruncates covers a cursor older than what the log keeps.
func TestEventBus_LogTruncates(t *testing.T) {
	b := NewEventBus()
	b.logKeep = 3
	all := func(Event) bool { return true }
	b.Publish(Event{Name: "x"}, allEventsTopic)
	c1 := b.cursor(1)
	b.nextEventID() // an id the SSE streams took: a gap, not a loss
	b.Publish(Event{Name: "x"}, allEventsTopic)
	b.Publish(Event{Name: "x"}, allEventsTopic)
	if pos, truncated := b.start(&c1, 0); truncated || pos != 1 {
		t.Errorf("nothing dropped yet: pos %d truncated %v", pos, truncated)
	}
	if evs, next, more := b.after(1, all, 0); len(evs) != 2 || next != 4 || more {
		t.Errorf("after 1: %d events, next %d, more %v", len(evs), next, more)
	}
	b.Publish(Event{Name: "x"}, allEventsTopic) // drops event 1
	if pos, truncated := b.start(&c1, 0); truncated || pos != 1 {
		t.Errorf("event 1 dropped, the cursor is after it: pos %d truncated %v", pos, truncated)
	}
	b.Publish(Event{Name: "x"}, allEventsTopic) // drops event 3
	pos, truncated := b.start(&c1, 0)
	if !truncated || pos != 3 {
		t.Errorf("event 3 dropped: pos %d truncated %v", pos, truncated)
	}
	if evs, _, _ := b.after(pos, all, 0); len(evs) != 3 {
		t.Errorf("after the truncation: %d events", len(evs))
	}
	// Events published on an artifact's topic alone aren't logged.
	b.Publish(Event{Name: "y"}, artifactTopic("a"))
	if evs, _, _ := b.after(0, func(e Event) bool { return e.Name == "y" }, 0); len(evs) != 0 {
		t.Errorf("unlogged event came back")
	}
	// maxAge skips what is older.
	old := NewEventBus()
	old.Publish(Event{Name: "x", OccurredAt: time.Now().Add(-time.Hour)}, allEventsTopic)
	old.Publish(Event{Name: "x"}, allEventsTopic)
	c0 := old.cursor(0)
	if pos, truncated := old.start(&c0, time.Minute); !truncated || pos != 1 {
		t.Errorf("maxAge: pos %d truncated %v", pos, truncated)
	}
}

type streamFrame struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params struct {
		Cursor    string         `json:"cursor"`
		Truncated bool           `json:"truncated"`
		Name      string         `json:"name"`
		EventID   string         `json:"eventId"`
		Data      map[string]any `json:"data"`
		Meta      map[string]any `json:"_meta"`
	} `json:"params"`
}

// TestMCPEvents_Stream opens events/stream and reads the active frame, an
// event, a heartbeat, and resumes from a cursor.
func TestMCPEvents_Stream(t *testing.T) {
	s, ts := testServer(t)
	s.eventsHeartbeat = 100 * time.Millisecond
	admin := login(t, ts.URL, "admin@example.com", "admin-password")
	a1 := createArtifact(t, admin, "one", false)
	a2 := createArtifact(t, admin, "two", false)

	open := func(ctx context.Context, params map[string]any) *bufio.Scanner {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 7, "method": "events/stream", "params": params})
		req, _ := http.NewRequestWithContext(ctx, "POST", ts.URL+"/mcp", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+admin.token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
			t.Fatalf("content type %q", ct)
		}
		return bufio.NewScanner(resp.Body)
	}
	next := func(sc *bufio.Scanner) streamFrame {
		t.Helper()
		for sc.Scan() {
			if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				var f streamFrame
				if err := json.Unmarshal([]byte(data), &f); err != nil {
					t.Fatalf("frame %q: %v", data, err)
				}
				if f.Method != "" && f.Params.Meta[mcpSubscriptionIDKey] != float64(7) {
					t.Errorf("frame without the subscription id: %s", data)
				}
				return f
			}
		}
		t.Fatalf("stream ended: %v", sc.Err())
		return streamFrame{}
	}
	nextEvent := func(sc *bufio.Scanner) streamFrame {
		t.Helper()
		for {
			if f := next(sc); f.Method != "notifications/events/heartbeat" {
				return f
			}
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sc := open(ctx, map[string]any{"name": "comment.created", "arguments": map[string]any{"artifactId": a1}})
	active := next(sc)
	if active.Method != "notifications/events/active" || active.Params.Cursor == "" || active.Params.Truncated {
		t.Fatalf("first frame: %+v", active)
	}
	comment(t, admin, a2, "not this one")
	comment(t, admin, a1, "this one")
	ev := nextEvent(sc)
	if ev.Method != "notifications/events/event" || ev.Params.Name != "comment.created" || ev.Params.Data["body"] != "this one" || ev.Params.Cursor == "" {
		t.Fatalf("event frame: %+v", ev)
	}
	if hb := next(sc); hb.Method != "notifications/events/heartbeat" || hb.Params.Cursor == "" {
		t.Errorf("heartbeat frame: %+v", hb)
	}

	// A second stream from the first one's start replays the event.
	sc2 := open(ctx, map[string]any{"name": "comment.created", "cursor": active.Params.Cursor})
	if f := next(sc2); f.Method != "notifications/events/active" {
		t.Fatalf("second stream's first frame: %+v", f)
	}
	if f := nextEvent(sc2); f.Params.Data["body"] != "not this one" {
		t.Errorf("replay, first: %+v", f)
	}
	if f := nextEvent(sc2); f.Params.EventID != ev.Params.EventID {
		t.Errorf("replay, second: %+v, want event %s", f, ev.Params.EventID)
	}

	// An invalid subscription is a plain JSON-RPC error, no stream.
	var out struct {
		Error *rpcError `json:"error"`
	}
	admin.mustDo("POST", "/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "events/stream", "params": map[string]any{"name": "nope"}}, &out, http.StatusOK)
	if out.Error == nil || out.Error.Code != rpcEventNotFound {
		t.Errorf("unknown event on a stream: %+v", out.Error)
	}
}

// TestMCPEvents_ListDelivery: every event offers the three delivery modes,
// as a list of names.
func TestMCPEvents_ListDelivery(t *testing.T) {
	_, ts := testServer(t)
	admin := login(t, ts.URL, "admin@example.com", "admin-password")
	var out struct {
		Result struct {
			Events []struct {
				Name     string   `json:"name"`
				Delivery []string `json:"delivery"`
			} `json:"events"`
		} `json:"result"`
	}
	admin.mustDo("POST", "/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "events/list"}, &out, http.StatusOK)
	if len(out.Result.Events) == 0 {
		t.Fatal("no events")
	}
	for _, e := range out.Result.Events {
		if strings.Join(e.Delivery, ",") != "poll,push,webhook" {
			t.Errorf("%s: delivery %v", e.Name, e.Delivery)
		}
	}
}
