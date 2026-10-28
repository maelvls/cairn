package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const testAnthropicKey = "sk-ant-test-secret-key"

// fakeAnthropic stands in for the Messages API. It checks the headers, keeps
// the last request body and answers with a canned message, or with an error
// when status is set.
type fakeAnthropic struct {
	t      *testing.T
	mu     sync.Mutex
	last   map[string]any
	header http.Header
	calls  int
	status int    // non-zero: answer with this error status
	errMsg string // error message for status
}

func (f *fakeAnthropic) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.header = r.Header.Clone()
	if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
		f.t.Errorf("fake Anthropic: %s %s", r.Method, r.URL.Path)
	}
	if r.Header.Get("x-api-key") != testAnthropicKey || r.Header.Get("anthropic-version") != anthropicVersion {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`)
		return
	}
	f.last = nil
	json.NewDecoder(r.Body).Decode(&f.last)
	if f.status != 0 {
		w.WriteHeader(f.status)
		json.NewEncoder(w).Encode(map[string]any{
			"type":  "error",
			"error": map[string]string{"type": "overloaded_error", "message": f.errMsg},
		})
		return
	}
	json.NewEncoder(w).Encode(map[string]any{
		"id":    "msg_1",
		"type":  "message",
		"role":  "assistant",
		"model": f.last["model"],
		"content": []map[string]any{
			{"type": "thinking", "thinking": "", "signature": "sig"},
			{"type": "text", "text": "Hello, "},
			{"type": "text", "text": "world."},
		},
		"stop_reason": "end_turn",
		"usage":       map[string]int{"input_tokens": 12, "output_tokens": 5},
	})
}

// seen returns the last request body and headers and the call count.
func (f *fakeAnthropic) seen() (map[string]any, http.Header, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last, f.header, f.calls
}

func (f *fakeAnthropic) fail(status int, msg string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.errMsg = status, msg
}

func aiTestServer(t *testing.T, key string, limit int) (*httptest.Server, *fakeAnthropic) {
	t.Helper()
	s, err := New(Config{
		DataDir:         t.TempDir(),
		AdminEmail:      "admin@example.com",
		AdminPassword:   "admin-password",
		TokenTTL:        time.Hour,
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		AnthropicAPIKey: key,
		AIDailyLimit:    limit,
		AIMaxTokens:     1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeAnthropic{t: t}
	fs := httptest.NewServer(fake)
	s.anthropicURL = fs.URL + "/v1/messages"
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		ts.Close()
		fs.Close()
		s.dbs.Close()
		s.store.Close()
	})
	return ts, fake
}

func TestAIProxy(t *testing.T) {
	ts, fake := aiTestServer(t, testAnthropicKey, 3)
	admin, aid, _ := setupArtifact(t, ts.URL, true)

	var out struct {
		Text       string `json:"text"`
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	admin.mustDo("POST", "/api/artifacts/"+aid+"/ai", map[string]any{
		"prompt": "Say hello", "system": "Be brief.", "max_tokens": 50000,
	}, &out, http.StatusOK)
	if out.Text != "Hello, world." || out.Model != defaultAIModel || out.StopReason != "end_turn" ||
		out.Usage.InputTokens != 12 || out.Usage.OutputTokens != 5 {
		t.Fatalf("response: %+v", out)
	}
	// prompt → one user message; system forwarded; max_tokens clamped to
	// the cap; fallbacks on for Sonnet 5.5.
	last, hdr, _ := fake.seen()
	msgs, _ := last["messages"].([]any)
	if len(msgs) != 1 || last["system"] != "Be brief." || last["max_tokens"] != float64(1000) ||
		last["model"] != defaultAIModel || last["fallbacks"] != "default" {
		t.Fatalf("upstream body: %v", last)
	}
	if m := msgs[0].(map[string]any); m["role"] != "user" || m["content"] != "Say hello" {
		t.Fatalf("upstream message: %v", m)
	}
	if _, ok := last["temperature"]; ok {
		t.Fatal("temperature must not be sent")
	}
	if hdr.Get("anthropic-beta") != fallbackBetaHdr || hdr.Get("Content-Type") != "application/json" {
		t.Fatalf("upstream headers: %v", hdr)
	}

	// messages, smaller max_tokens kept, Haiku: no fallbacks.
	admin.mustDo("POST", "/api/artifacts/"+aid+"/ai", map[string]any{
		"messages": []map[string]any{
			{"role": "user", "content": "Hi"},
			{"role": "assistant", "content": "Hello"},
			{"role": "user", "content": []map[string]string{{"type": "text", "text": "Again"}}},
		},
		"max_tokens": 64, "model": "claude-haiku-5-5",
	}, &out, http.StatusOK)
	last, hdr, _ = fake.seen()
	if len(last["messages"].([]any)) != 3 || last["max_tokens"] != float64(64) || last["model"] != "claude-haiku-5-5" {
		t.Fatalf("upstream body: %v", last)
	}
	if _, ok := last["fallbacks"]; ok || hdr.Get("anthropic-beta") != "" {
		t.Fatal("fallbacks sent to a model without server-side fallback")
	}

	// Status: 2 of 3 used.
	var st aiStatus
	admin.mustDo("GET", "/api/me/ai", nil, &st, http.StatusOK)
	if !st.Enabled || st.Model != defaultAIModel || st.Remaining != 1 || st.DailyLimit != 3 || st.MaxTokens != 1000 {
		t.Fatalf("status: %+v", st)
	}

	// Validation errors are 400s and spend nothing.
	_, _, calls := fake.seen()
	for _, bad := range []map[string]any{
		{},
		{"prompt": "  "},
		{"prompt": "a", "messages": []map[string]any{{"role": "user", "content": "b"}}},
		{"messages": []map[string]any{{"role": "system", "content": "b"}}},
		{"messages": []map[string]any{{"role": "assistant", "content": "b"}}},
		{"messages": []map[string]any{{"role": "user", "content": []map[string]string{{"type": "tool_use"}}}}},
		{"prompt": "a", "model": "gpt-4"},
		{"prompt": "a", "temperature": 0},
		{"prompt": "a", "maxTokens": 10},
	} {
		resp := admin.do("POST", "/api/artifacts/"+aid+"/ai", bad, nil)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%v: status %d, want 400", bad, resp.StatusCode)
		}
	}
	big := strings.Repeat("x", aiMaxBody)
	if resp := admin.do("POST", "/api/artifacts/"+aid+"/ai", map[string]any{"prompt": big}, nil); resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("big body: status %d, want 413", resp.StatusCode)
	}
	if _, _, n := fake.seen(); n != calls {
		t.Fatal("invalid requests reached the API")
	}

	// Anonymous visitors of a public artifact cannot spend the key.
	anon := &testClient{t: t, base: ts.URL}
	anon.mustDo("POST", "/api/artifacts/"+aid+"/ai", map[string]any{"prompt": "hi"}, nil, http.StatusUnauthorized)
	anon.mustDo("GET", "/api/me/ai", nil, nil, http.StatusUnauthorized)

	// Third request uses the last one; the fourth is over the limit, for
	// this user only.
	admin.mustDo("POST", "/api/artifacts/"+aid+"/ai", map[string]any{"prompt": "3"}, nil, http.StatusOK)
	var e apiError
	admin.mustDo("POST", "/api/artifacts/"+aid+"/ai", map[string]any{"prompt": "4"}, &e, http.StatusTooManyRequests)
	if !strings.Contains(e.Error, "daily AI limit") {
		t.Fatalf("429 error: %q", e.Error)
	}
	bob := newUser(t, ts.URL, admin, "bob@example.com", "Bob")
	bob.mustDo("POST", "/api/artifacts/"+aid+"/ai", map[string]any{"prompt": "hi"}, nil, http.StatusOK)
}

func TestAIUpstreamError(t *testing.T) {
	ts, fake := aiTestServer(t, testAnthropicKey, 1)
	admin, aid, _ := setupArtifact(t, ts.URL, false)
	fake.fail(529, "Overloaded (key "+testAnthropicKey+")")

	var e apiError
	resp := admin.do("POST", "/api/artifacts/"+aid+"/ai", map[string]any{"prompt": "hi"}, &e)
	msg := e.Error
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", resp.StatusCode)
	}
	if !strings.Contains(msg, "Overloaded") || !strings.Contains(msg, "529") {
		t.Fatalf("error lacks the upstream message: %s", msg)
	}
	if strings.Contains(msg, testAnthropicKey) {
		t.Fatalf("error leaks the API key: %s", msg)
	}
	// A failed call does not count against the quota (limit 1).
	fake.fail(0, "")
	admin.mustDo("POST", "/api/artifacts/"+aid+"/ai", map[string]any{"prompt": "hi"}, nil, http.StatusOK)
}

func TestAIDisabled(t *testing.T) {
	ts, fake := aiTestServer(t, "", 10)
	admin, aid, _ := setupArtifact(t, ts.URL, true)
	var e apiError
	admin.mustDo("POST", "/api/artifacts/"+aid+"/ai", map[string]any{"prompt": "hi"}, &e, http.StatusServiceUnavailable)
	if e.Error != "AI is not configured on this server" {
		t.Fatalf("error: %q", e.Error)
	}
	var st aiStatus
	admin.mustDo("GET", "/api/me/ai", nil, &st, http.StatusOK)
	if st.Enabled || st.Model != "" {
		t.Fatalf("status: %+v", st)
	}
	if _, _, n := fake.seen(); n != 0 {
		t.Fatal("disabled server called the API")
	}
}

func TestCairnJSHasAI(t *testing.T) {
	_, ts := testServer(t)
	js := body(t, get(t, ts.URL+"/cairn.js", "", ""))
	for _, want := range []string{"ai: {", "ask:", "chat:", "complete:", "status:", "cairn.ai needs a Cairn server"} {
		if !strings.Contains(js, want) {
			t.Errorf("cairn.js lacks %q", want)
		}
	}
}
