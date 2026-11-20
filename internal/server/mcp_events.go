package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// MCP events methods, per the Triggers & Events working group's design
// sketch (github.com/modelcontextprotocol/experimental-ext-triggers-events,
// SEP-3415), with its three delivery modes:
//
//   - poll: events/poll, request/response;
//   - push: events/stream, a long POST answered with an SSE stream;
//   - webhook: events/subscribe and events/unsubscribe (webhooks.go), the
//     only mode ChatGPT uses.
//
// Poll and push read the bus's log by cursor (eventbus.go). The log is in
// memory: after a restart a client's cursor is from another log, it is told
// so (truncated) and goes on from now, re-reading the state it watches.

const (
	// The sketch's error codes, provisional until SEP-3415 is accepted.
	rpcEventNotFound    = -32023
	rpcEventUnsupported = -32026

	mcpEventsPollDefault = 50
	mcpEventsPollMax     = 200
	mcpEventsNextPollMs  = 15_000
	// mcpEventsHeartbeat: the sketch asks for one at least every 30 s.
	mcpEventsHeartbeat = 25 * time.Second
	// mcpSubscriptionIDKey routes a stream's notifications (SEP-2575).
	mcpSubscriptionIDKey = "io.modelcontextprotocol/subscriptionId"
)

func (s *Server) mcpEventsList(_ json.RawMessage) (any, error) {
	events := make([]map[string]any, 0, len(mcpEvents))
	for _, e := range mcpEvents {
		schema := commentPayloadSchema
		if e.name == "db.write" {
			schema = dbWritePayloadSchema
		}
		events = append(events, map[string]any{
			"name":        e.name,
			"description": e.description,
			"delivery":    []string{"poll", "push", "webhook"},
			"inputSchema": object(nil, map[string]any{
				"artifactId": str("Only deliver events for this artifact. Omit to receive every artifact's events."),
			}),
			"payloadSchema": schema,
		})
	}
	return map[string]any{"events": events}, nil
}

// dbWritePayloadSchema matches the small payload the bus publishes on
// db.write: enough for a subscriber to know which version to re-query.
var dbWritePayloadSchema = object([]string{"artifactId", "versionId"}, map[string]any{
	"artifactId": str("Artifact whose database was written to."),
	"versionId":  str("Version whose database was written to."),
})

// commentPayloadSchema matches the Comment JSON delivered in the data field.
var commentPayloadSchema = object([]string{"id", "artifactId", "body"}, map[string]any{
	"id":         str("Comment id."),
	"artifactId": str("Artifact the comment belongs to."),
	"parentId":   str("Parent comment id, when this is a reply."),
	"versionId":  str("Version the author was looking at."),
	"userId":     str("Author's user id."),
	"author":     str("Author's name (or email)."),
	"via":        str("How the comment was posted: web, api or mcp."),
	"body":       str("Comment body (plain text)."),
	"anchor": map[string]any{
		"type":        "object",
		"description": "Element of the page the thread is about (optional).",
		"properties": map[string]any{
			"selector": str("CSS selector."),
			"quote":    str("Element text."),
			"path":     str("Page inside the version."),
		},
	},
	"resolvedAt": str("RFC3339 timestamp when the thread was resolved."),
	"resolvedBy": str("User id of the resolver."),
	"createdAt":  str("RFC3339 creation timestamp."),
	"updatedAt":  str("RFC3339 last-update timestamp."),
})

func (s *Server) mcpEventsSubscribe(r *http.Request, raw json.RawMessage) (any, *rpcError) {
	var in subscribeInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, &rpcError{-32602, "invalid params: " + err.Error()}
	}
	u := requestUser(r)
	if u == nil {
		return nil, &rpcError{-32001, "authentication required"}
	}
	out, code, err := s.webhooks.subscribe(r.Context(), u.ID, in)
	if err != nil {
		if code == 0 {
			code = -32603
		}
		return nil, &rpcError{code, err.Error()}
	}
	return out, nil
}

func (s *Server) mcpEventsUnsubscribe(r *http.Request, raw json.RawMessage) (any, *rpcError) {
	var in struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
		Delivery  struct {
			URL string `json:"url"`
		} `json:"delivery"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, &rpcError{-32602, "invalid params: " + err.Error()}
	}
	u := requestUser(r)
	if u == nil {
		return nil, &rpcError{-32001, "authentication required"}
	}
	if in.Name == "" || in.Delivery.URL == "" {
		return nil, &rpcError{-32602, "name and delivery.url are required"}
	}
	s.webhooks.unsubscribe(u.ID, in.Name, in.Arguments, in.Delivery.URL)
	return map[string]any{}, nil
}

func (s *Server) mcpEventsListSubscriptions(r *http.Request) (any, *rpcError) {
	u := requestUser(r)
	if u == nil {
		return nil, &rpcError{-32001, "authentication required"}
	}
	subs := s.webhooks.list(u.ID)
	out := make([]map[string]any, 0, len(subs))
	for _, sub := range subs {
		out = append(out, map[string]any{
			"id":            sub.id,
			"name":          sub.eventName,
			"arguments":     sub.args,
			"callbackUrl":   sub.callbackURL,
			"createdAt":     sub.createdAt.UTC().Format("2006-01-02T15:04:05Z"),
			"refreshBefore": sub.expiresAt.UTC().Format("2006-01-02T15:04:05Z"),
		})
	}
	return map[string]any{"subscriptions": out}, nil
}

// errRPC returns a JSON-RPC error with the given code, or -32603 (internal)
// when err is nil-safe.
func errRPC(code int, err error) *rpcError {
	if err == nil {
		err = errors.New(fmt.Sprintf("error code %d", code))
	}
	return &rpcError{code, err.Error()}
}

// eventsReadParams are the parameters events/poll and events/stream share.
type eventsReadParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Cursor    *string         `json:"cursor"`
	MaxAgeMs  *int64          `json:"maxAgeMs"`
	MaxEvents int             `json:"maxEvents"`
}

func (p eventsReadParams) maxAge() time.Duration {
	if p.MaxAgeMs == nil || *p.MaxAgeMs <= 0 {
		return 0
	}
	return time.Duration(*p.MaxAgeMs) * time.Millisecond
}

// parseEventsRead checks who asks and what for, and returns the filter. Any
// signed-in user may read every artifact's events, as with the webhooks and
// the rest of the API.
func parseEventsRead(r *http.Request, raw json.RawMessage) (eventsReadParams, func(Event) bool, *rpcError) {
	var p eventsReadParams
	if requestUser(r) == nil {
		return p, nil, &rpcError{-32001, "authentication required"}
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return p, nil, &rpcError{-32602, "invalid params: " + err.Error()}
		}
	}
	def := mcpEventByName(p.Name)
	if def == nil {
		return p, nil, &rpcError{rpcEventNotFound, fmt.Sprintf("unknown event name: %q", p.Name)}
	}
	args := map[string]any{}
	if len(p.Arguments) > 0 && string(p.Arguments) != "null" {
		if err := json.Unmarshal(p.Arguments, &args); err != nil {
			return p, nil, &rpcError{-32602, "arguments must be an object"}
		}
	}
	for k, v := range args {
		if _, ok := v.(string); k != "artifactId" || !ok {
			return p, nil, &rpcError{-32602, fmt.Sprintf("%s takes one argument, artifactId (a string), not %q", def.name, k)}
		}
	}
	name := def.name
	return p, func(e Event) bool { return e.Name == name && matchArgs(args, e) }, nil
}

// mcpOccurrence is an event as the sketch's EventOccurrence.
func (s *Server) mcpOccurrence(e Event, withCursor bool) map[string]any {
	o := map[string]any{
		"eventId":   s.bus.eventID(e),
		"name":      e.Name,
		"timestamp": e.OccurredAt.UTC().Format(time.RFC3339Nano),
		"data":      json.RawMessage(e.Data),
	}
	if withCursor {
		if seq, ok := eventSeq(e); ok {
			o["cursor"] = s.bus.cursor(seq)
		}
	}
	return o
}

func (s *Server) mcpEventsPoll(r *http.Request, raw json.RawMessage) (any, *rpcError) {
	p, match, rerr := parseEventsRead(r, raw)
	if rerr != nil {
		return nil, rerr
	}
	limit := p.MaxEvents
	if limit <= 0 || limit > mcpEventsPollMax {
		limit = mcpEventsPollDefault
	}
	pos, truncated := s.bus.start(p.Cursor, p.maxAge())
	evs, next, more := s.bus.after(pos, match, limit)
	out := make([]map[string]any, 0, len(evs))
	for _, e := range evs {
		out = append(out, s.mcpOccurrence(e, false))
	}
	return map[string]any{
		"events": out, "cursor": s.bus.cursor(next), "truncated": truncated,
		"hasMore": more, "nextPollMs": mcpEventsNextPollMs,
	}, nil
}

// mcpEventsStream is events/stream: the events as an SSE stream of
// notifications/events/*, until the client goes away or the server stops.
func (s *Server) mcpEventsStream(w http.ResponseWriter, r *http.Request, m rpcMessage) {
	fail := func(rerr *rpcError) {
		writeJSON(w, http.StatusOK, rpcResponse{JSONRPC: "2.0", ID: m.ID, Error: rerr})
	}
	p, match, rerr := parseEventsRead(r, m.Params)
	if rerr != nil {
		fail(rerr)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		fail(&rpcError{rpcEventUnsupported, "streaming unsupported"})
		return
	}
	// Listening before reading the log: nothing falls in between.
	wake := s.bus.listen()
	defer s.bus.unlisten(wake)
	send := func(method string, params map[string]any) bool {
		params["_meta"] = map[string]any{mcpSubscriptionIDKey: m.ID}
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
		if _, err := fmt.Fprintf(w, "event: message\ndata: %s\n\n", b); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	pos, truncated := s.bus.start(p.Cursor, p.maxAge())
	if !send("notifications/events/active", map[string]any{"cursor": s.bus.cursor(pos), "truncated": truncated}) {
		return
	}
	beat := time.NewTicker(s.eventsHeartbeat)
	defer beat.Stop()
	for {
		evs, next, _ := s.bus.after(pos, match, 0)
		for _, e := range evs {
			if !send("notifications/events/event", s.mcpOccurrence(e, true)) {
				return
			}
		}
		pos = next
		select {
		case <-r.Context().Done():
			return
		case <-s.webhooks.stopCh:
			// The server stops: the final frame (StreamEventsResult).
			b, _ := json.Marshal(rpcResponse{JSONRPC: "2.0", ID: m.ID, Result: map[string]any{"_meta": map[string]any{}}})
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", b)
			flusher.Flush()
			return
		case <-wake:
		case <-beat.C:
			if !send("notifications/events/heartbeat", map[string]any{"cursor": s.bus.cursor(pos)}) {
				return
			}
		}
	}
}
