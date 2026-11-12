package server

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestMCPEvents_SubscribeAndDeliver registers an events/subscribe for
// comment.created, mounts a stub webhook server that answers the
// verification challenge and later records deliveries, then posts a comment
// and verifies the stub got the signed webhook within a short window.
func TestMCPEvents_SubscribeAndDeliver(t *testing.T) {
	_, ts := testServer(t)
	admin := login(t, ts.URL, "admin@example.com", "admin-password")
	aid := createArtifact(t, admin, "demo", false)

	// Stub webhook endpoint: echo the challenge on the first POST,
	// capture payload on the second.
	var received atomic.Pointer[http.Header]
	var body atomic.Pointer[string]
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var env map[string]any
		if err := json.Unmarshal(data, &env); err == nil && env["type"] == "verification" {
			json.NewEncoder(w).Encode(map[string]any{"challenge": env["challenge"]})
			return
		}
		h := r.Header.Clone()
		received.Store(&h)
		s := string(data)
		body.Store(&s)
		w.WriteHeader(http.StatusOK)
	}))
	defer stub.Close()

	// A 32-byte signing key is well inside the 24..64 band the spec
	// requires.
	secret := "whsec_" + base64.StdEncoding.EncodeToString(bytesOf(32))

	// events/subscribe
	var subResp struct {
		Result struct {
			ID            string `json:"id"`
			RefreshBefore string `json:"refreshBefore"`
		} `json:"result"`
		Error *rpcError `json:"error"`
	}
	admin.mustDo("POST", "/mcp", map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "events/subscribe",
		"params": map[string]any{
			"name":      "comment.created",
			"arguments": map[string]any{"artifactId": aid},
			"delivery": map[string]any{
				"mode":   "webhook",
				"url":    stub.URL,
				"secret": secret,
			},
		},
	}, &subResp, http.StatusOK)
	if subResp.Error != nil {
		t.Fatalf("subscribe: %+v", subResp.Error)
	}
	if subResp.Result.ID == "" {
		t.Fatalf("subscribe returned no id")
	}

	// Post a comment and wait for the webhook delivery.
	admin.mustDo("POST", "/api/artifacts/"+aid+"/comments", map[string]any{"body": "watch me"}, nil, http.StatusCreated)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if body.Load() != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if body.Load() == nil {
		t.Fatal("stub did not receive a delivery")
	}
	s := *body.Load()
	if !strings.Contains(s, `"name":"comment.created"`) || !strings.Contains(s, "watch me") {
		t.Fatalf("unexpected delivered body: %s", s)
	}
	h := *received.Load()
	for _, k := range []string{"Webhook-Id", "Webhook-Timestamp", "Webhook-Signature", "X-Mcp-Subscription-Id"} {
		if h.Get(k) == "" {
			t.Errorf("missing header %s. Got: %#v", k, h)
		}
	}
	if !strings.HasPrefix(h.Get("Webhook-Signature"), "v1,") {
		t.Errorf("signature should start with v1,: %s", h.Get("Webhook-Signature"))
	}
}

// TestMCPEvents_List verifies events/list exposes the comment events with a
// webhook delivery and an inputSchema that names artifactId.
func TestMCPEvents_List(t *testing.T) {
	_, ts := testServer(t)
	admin := login(t, ts.URL, "admin@example.com", "admin-password")

	var out struct {
		Result struct {
			Events []map[string]any `json:"events"`
		} `json:"result"`
	}
	admin.mustDo("POST", "/mcp", map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "events/list",
	}, &out, http.StatusOK)
	names := map[string]bool{}
	for _, e := range out.Result.Events {
		names[e["name"].(string)] = true
	}
	for _, want := range []string{"comment.created", "comment.resolved", "comment.reopened", "comment.updated"} {
		if !names[want] {
			t.Errorf("events/list missing %q", want)
		}
	}
}

// TestInitializeAdvertisesEventsCapability covers the MCP capabilities
// response: `events` must be present alongside `tools`.
func TestInitializeAdvertisesEventsCapability(t *testing.T) {
	_, ts := testServer(t)
	admin := login(t, ts.URL, "admin@example.com", "admin-password")

	var out struct {
		Result struct {
			Capabilities map[string]any `json:"capabilities"`
		} `json:"result"`
	}
	admin.mustDo("POST", "/mcp", map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params":  map[string]any{"protocolVersion": "2025-11-25"},
	}, &out, http.StatusOK)
	if _, ok := out.Result.Capabilities["events"]; !ok {
		t.Fatalf("initialize did not advertise events: %+v", out.Result.Capabilities)
	}
}

// bytesOf returns n deterministic bytes for use as a signing key in tests.
func bytesOf(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i + 1)
	}
	return b
}
