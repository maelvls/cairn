package server

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestCommentsStream_Live checks the SSE endpoint end to end: a signed-in
// client opens the stream, another request posts a comment on the same
// artifact, and the first stream delivers it. The stream is cut from the
// client side (ctx cancellation) once the event is observed.
func TestCommentsStream_Live(t *testing.T) {
	_, ts := testServer(t)
	admin := login(t, ts.URL, "admin@example.com", "admin-password")
	aid := createArtifact(t, admin, "demo", false)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", ts.URL+"/api/artifacts/"+aid+"/comments/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+admin.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream: HTTP %d", resp.StatusCode)
	}

	// Post a comment on the same artifact. Give the server a moment to open
	// its subscription before the publish happens.
	time.Sleep(50 * time.Millisecond)
	admin.mustDo("POST", "/api/artifacts/"+aid+"/comments", map[string]any{"body": "hello"}, nil, http.StatusCreated)

	// Read one SSE frame.
	frame := readOneFrame(t, resp.Body, 3*time.Second)
	if !strings.Contains(frame, "event: comment.created") {
		t.Fatalf("stream missing comment.created event:\n%s", frame)
	}
	if !strings.Contains(frame, "\"body\":\"hello\"") {
		t.Fatalf("stream missing the comment body:\n%s", frame)
	}
}

// readOneFrame reads SSE frames until it finds one that has a data: line
// (skipping heartbeat ": ping" and the initial ": watching ..." comments),
// or fails the test after timeout.
func readOneFrame(t *testing.T, body io.Reader, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	br := bufio.NewReader(body)
	var frame strings.Builder
	for time.Now().Before(deadline) {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("stream read: %v", err)
		}
		frame.WriteString(line)
		if line == "\n" || line == "\r\n" {
			s := frame.String()
			if strings.Contains(s, "data:") {
				return s
			}
			frame.Reset()
		}
	}
	t.Fatalf("no data frame within %s. last partial frame:\n%s", timeout, frame.String())
	return ""
}

// TestSSE_FrameFormat sanity-checks the event frame layout the server
// writes: id/event/data lines, in that order, separated from the next event
// by a blank line.
func TestSSE_FrameFormat(t *testing.T) {
	rr := httptest.NewRecorder()
	if err := writeSSEEvent(rr, Event{ID: "42", Name: "comment.created", ArtifactID: "a1", OccurredAt: time.Unix(1700000000, 0).UTC()}); err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(strings.NewReader(rr.Body.String()))
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if len(lines) < 3 {
		t.Fatalf("expected at least 3 lines, got %d: %v", len(lines), lines)
	}
	if lines[0] != "id: 42" || lines[1] != "event: comment.created" || !strings.HasPrefix(lines[2], "data: ") {
		t.Fatalf("unexpected frame layout: %v", lines)
	}
}
