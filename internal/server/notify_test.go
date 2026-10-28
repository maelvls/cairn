package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/aloisdeniel/cairn/internal/store"
)

// fakeNtfy records what ntfy's JSON publish API receives, or fails.
type fakeNtfy struct {
	mu   sync.Mutex
	got  []ntfyMessage
	fail bool
}

func (f *fakeNtfy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method != http.MethodPost || r.URL.Path != "/" || r.Header.Get("Content-Type") != "application/json" ||
		r.Header.Get("Authorization") != "Bearer tk_test" {
		http.Error(w, "unexpected request", http.StatusBadRequest)
		return
	}
	if f.fail {
		http.Error(w, `{"error":"down"}`, http.StatusInternalServerError)
		return
	}
	var m ntfyMessage
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.got = append(f.got, m)
	w.Write([]byte(`{"id":"x"}`))
}

func (f *fakeNtfy) setFail(fail bool) {
	f.mu.Lock()
	f.fail = fail
	f.mu.Unlock()
}

// take returns the messages received so far, by topic, and forgets them.
func (f *fakeNtfy) take() map[string]ntfyMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]ntfyMessage{}
	for _, m := range f.got {
		out[m.Topic] = m
	}
	f.got = nil
	return out
}

func topics(m map[string]ntfyMessage) string {
	var ts []string
	for t := range m {
		ts = append(ts, t)
	}
	sort.Strings(ts)
	return strings.Join(ts, ",")
}

func ntfyTestServer(t *testing.T) (*Server, *httptest.Server, *fakeNtfy) {
	fake := &fakeNtfy{}
	ntfy := httptest.NewServer(fake)
	t.Cleanup(ntfy.Close)
	s, ts := testServerWith(t, func(c *Config) {
		c.NtfyURL = ntfy.URL + "/" // the trailing slash is not doubled
		c.NtfyToken = "tk_test"
		c.BaseURL = "https://cairn.example"
	})
	s.httpClient = ntfy.Client()
	return s, ts, fake
}

func TestNtfySettings(t *testing.T) {
	_, ts, fake := ntfyTestServer(t)
	admin := login(t, ts.URL, "admin@example.com", "admin-password")
	bob := newUser(t, ts.URL, admin, "bob@example.com", "Bob")

	var st settings
	bob.mustDo("GET", "/api/me/settings", nil, &st, http.StatusOK)
	if st.NtfyTopic != "" || !strings.HasPrefix(st.NtfyURL, "http://127.0.0.1") {
		t.Fatalf("initial settings: %+v", st)
	}
	bob.mustDo("POST", "/api/me/settings/test-notification", map[string]any{}, nil, http.StatusBadRequest)
	for _, bad := range []string{"has space", "slash/no", "é", strings.Repeat("x", 65)} {
		bob.mustDo("PATCH", "/api/me/settings", map[string]any{"ntfyTopic": bad}, nil, http.StatusBadRequest)
	}
	bob.mustDo("PATCH", "/api/me/settings", map[string]any{"ntfyTopic": " bob_Topic-1 "}, &st, http.StatusOK)
	if st.NtfyTopic != "bob_Topic-1" {
		t.Fatalf("saved topic: %+v", st)
	}
	// Topics are private: other users and the directory don't see them.
	admin.mustDo("GET", "/api/me/settings", nil, &st, http.StatusOK)
	if st.NtfyTopic != "" {
		t.Errorf("admin sees topic %q", st.NtfyTopic)
	}
	resp := get(t, ts.URL+"/api/admin/users", admin.token, "")
	if b := body(t, resp); strings.Contains(b, "bob_Topic-1") {
		t.Errorf("admin user list leaks the topic: %s", b)
	}

	// The test goes to the saved topic, or to one being tried.
	bob.mustDo("POST", "/api/me/settings/test-notification", nil, nil, http.StatusOK) // no body
	bob.mustDo("POST", "/api/me/settings/test-notification", map[string]any{"ntfyTopic": "trying"}, nil, http.StatusOK)
	bob.mustDo("POST", "/api/me/settings/test-notification", map[string]any{"ntfyTopic": "no way"}, nil, http.StatusBadRequest)
	got := fake.take()
	if topics(got) != "bob_Topic-1,trying" || got["trying"].Click != "https://cairn.example/" {
		t.Errorf("test notifications: %+v", got)
	}
	fake.setFail(true)
	bob.mustDo("POST", "/api/me/settings/test-notification", map[string]any{}, nil, http.StatusBadGateway)
	fake.setFail(false)

	// Clearing.
	bob.mustDo("PATCH", "/api/me/settings", map[string]any{"ntfyTopic": ""}, &st, http.StatusOK)
	if st.NtfyTopic != "" {
		t.Errorf("cleared topic: %+v", st)
	}
	anon := &testClient{t: t, base: ts.URL}
	anon.mustDo("GET", "/api/me/settings", nil, nil, http.StatusUnauthorized)
}

func TestCommentNotifications(t *testing.T) {
	s, ts, fake := ntfyTestServer(t)
	admin, aid, _ := setupArtifact(t, ts.URL, false) // admin created it
	admin.mustDo("PATCH", "/api/artifacts/"+aid, map[string]any{"description": "Café du coin ☕"}, nil, http.StatusOK)
	users := map[string]*testClient{"admin": admin}
	for _, n := range []string{"Bob", "Carol", "Dave", "Eve", "Frank"} {
		users[strings.ToLower(n)] = newUser(t, ts.URL, admin, strings.ToLower(n)+"@example.com", n)
	}
	for n, c := range users {
		if n != "dave" { // Dave never sets a topic
			c.mustDo("PATCH", "/api/me/settings", map[string]any{"ntfyTopic": n + "-t"}, nil, http.StatusOK)
		}
	}
	for _, n := range []string{"bob", "dave", "frank"} {
		users[n].mustDo("PUT", "/api/artifacts/"+aid+"/pin", nil, nil, http.StatusOK)
	}
	frank, err := s.store.UserByEmail("frank@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.SetUserDisabled(frank.ID, true); err != nil {
		t.Fatal(err)
	}

	base := "/api/artifacts/" + aid + "/comments"
	comment := func(who string, body any, parent string) (store.Comment, map[string]ntfyMessage) {
		t.Helper()
		var c store.Comment
		users[who].mustDo("POST", base, map[string]any{"body": body, "parentId": parent}, &c, http.StatusCreated)
		s.notifyWG.Wait()
		return c, fake.take()
	}

	// A root comment: the creator and the pinners with a topic, not the
	// author, nor Eve who has nothing to do with it.
	long := strings.Repeat("é", 600)
	root, got := comment("carol", long, "")
	if topics(got) != "admin-t,bob-t" {
		t.Fatalf("root comment notified %q", topics(got))
	}
	m := got["bob-t"]
	if m.Title != "Carol on Café du coin ☕" || m.Click != "https://cairn.example/shared/"+aid+"#comments" ||
		len(m.Tags) != 1 || m.Tags[0] != "speech_balloon" {
		t.Errorf("message: %+v", m)
	}
	if r := []rune(m.Message); len(r) != notifyBodyRunes || r[len(r)-1] != '…' {
		t.Errorf("message body: %d runes, ends %q", len(r), string(r[len(r)-1]))
	}

	// Replies also reach the thread's earlier participants.
	reply, got := comment("eve", "agreed", root.ID)
	if topics(got) != "admin-t,bob-t,carol-t" || got["carol-t"].Message != "agreed" || got["carol-t"].Title != "Eve on Café du coin ☕" {
		t.Fatalf("reply notified %q: %+v", topics(got), got)
	}
	if _, got = comment("admin", "fixed", reply.ID); topics(got) != "bob-t,carol-t,eve-t" {
		t.Fatalf("admin's reply notified %q", topics(got))
	}
	// Another thread: Eve and Carol are not part of it.
	if _, got = comment("bob", "and the logo?", ""); topics(got) != "admin-t" {
		t.Fatalf("second thread notified %q", topics(got))
	}

	// A failing ntfy never fails the comment.
	fake.setFail(true)
	if _, got = comment("carol", "still there?", root.ID); len(got) != 0 {
		t.Fatalf("failing ntfy recorded %v", got)
	}
}
