package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestDBStream_LiveReflectsWrite checks the db/stream endpoint end to end: a
// signed-in client opens the stream, another request writes to the artifact's
// shared database, and the first stream receives a `db.result` event with the
// re-executed query.
func TestDBStream_LiveReflectsWrite(t *testing.T) {
	_, ts := testServer(t)
	admin := login(t, ts.URL, "admin@example.com", "admin-password")
	aid := createArtifact(t, admin, "demo", false)
	vid := uploadEmptyVersion(t, admin, aid)

	// Seed the shared DB with a table.
	admin.mustDo("POST", "/api/artifacts/"+aid+"/versions/"+vid+"/db/query",
		map[string]any{"sql": "CREATE TABLE notes (id INTEGER PRIMARY KEY, body TEXT)"},
		nil, http.StatusOK)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	body, _ := json.Marshal(map[string]any{
		"sql":        "SELECT body FROM notes ORDER BY id",
		"coalesceMs": 10,
	})
	req, err := http.NewRequestWithContext(ctx, "POST",
		ts.URL+"/api/artifacts/"+aid+"/versions/"+vid+"/db/stream",
		strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+admin.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream: HTTP %d", resp.StatusCode)
	}

	// First frame is the init: empty result.
	init := readOneFrame(t, resp.Body, 3*time.Second)
	if !strings.Contains(init, "event: db.init") {
		t.Fatalf("expected db.init frame, got:\n%s", init)
	}
	if !strings.Contains(init, `"rows":[]`) {
		t.Fatalf("expected empty rows in init frame, got:\n%s", init)
	}

	// Write a row and expect a db.result frame carrying it.
	admin.mustDo("POST", "/api/artifacts/"+aid+"/versions/"+vid+"/db/query",
		map[string]any{"sql": "INSERT INTO notes (body) VALUES ('hi')"},
		nil, http.StatusOK)

	result := readOneFrame(t, resp.Body, 3*time.Second)
	if !strings.Contains(result, "event: db.result") {
		t.Fatalf("expected db.result frame, got:\n%s", result)
	}
	if !strings.Contains(result, `"hi"`) {
		t.Fatalf("expected inserted row in result frame, got:\n%s", result)
	}
}

// TestDBStream_RequiresAuth makes sure anonymous callers cannot open a
// long-lived DB stream even on a public artifact.
func TestDBStream_RequiresAuth(t *testing.T) {
	_, ts := testServer(t)
	admin := login(t, ts.URL, "admin@example.com", "admin-password")
	aid := createArtifact(t, admin, "demo", true) // public
	vid := uploadEmptyVersion(t, admin, aid)

	body, _ := json.Marshal(map[string]any{"sql": "SELECT 1"})
	resp, err := http.Post(ts.URL+"/api/artifacts/"+aid+"/versions/"+vid+"/db/stream",
		"application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous db/stream: HTTP %d, want 401", resp.StatusCode)
	}
}

// TestDBStream_RejectsMissingSQL covers the obvious bad input.
func TestDBStream_RejectsMissingSQL(t *testing.T) {
	_, ts := testServer(t)
	admin := login(t, ts.URL, "admin@example.com", "admin-password")
	aid := createArtifact(t, admin, "demo", false)
	vid := uploadEmptyVersion(t, admin, aid)

	body, _ := json.Marshal(map[string]any{"sql": "   "})
	req, _ := http.NewRequest("POST",
		ts.URL+"/api/artifacts/"+aid+"/versions/"+vid+"/db/stream",
		strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+admin.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty sql: HTTP %d, want 400", resp.StatusCode)
	}
}
