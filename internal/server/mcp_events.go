package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// MCP events methods (events/list, events/subscribe, events/unsubscribe),
// per OpenAI's MCP events spec (webhook flavor).

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
			"delivery":    []map[string]any{{"mode": "webhook"}},
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
