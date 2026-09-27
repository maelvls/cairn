package server

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

const testGoogleClientID = "client-id.apps.googleusercontent.com"

// fakeGoogle stands in for Google's token endpoint. It checks the PKCE
// verifier against the challenge sent to the consent page and answers with an
// ID token carrying the claims the test sets.
type fakeGoogle struct {
	t         *testing.T
	challenge string
	claims    map[string]any
}

func (f *fakeGoogle) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != f.challenge ||
		r.PostForm.Get("code") != "the-code" ||
		r.PostForm.Get("client_secret") != "client-secret" ||
		r.PostForm.Get("redirect_uri") != "https://cairn.test/auth/google/callback" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
		return
	}
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	idToken := enc(map[string]string{"alg": "RS256"}) + "." + enc(f.claims) + ".sig"
	json.NewEncoder(w).Encode(map[string]string{"id_token": idToken})
}

func googleClaims(email string) map[string]any {
	return map[string]any{
		"iss":            "https://accounts.google.com",
		"aud":            testGoogleClientID,
		"exp":            time.Now().Add(time.Hour).Unix(),
		"email":          email,
		"email_verified": true,
		"name":           "Ada Admin",
	}
}

func googleTestServer(t *testing.T) (*Server, *httptest.Server, *fakeGoogle) {
	t.Helper()
	s, err := New(Config{
		DataDir:            t.TempDir(),
		BaseURL:            "https://cairn.test",
		AdminEmail:         "admin@example.com",
		TokenTTL:           time.Hour,
		Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		GoogleClientID:     testGoogleClientID,
		GoogleClientSecret: "client-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Test traffic is plain HTTP: cookies must not be Secure-only.
	s.secure = false
	fake := &fakeGoogle{t: t, claims: googleClaims("admin@example.com")}
	gs := httptest.NewServer(fake)
	s.googleTokenURL = gs.URL
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		ts.Close()
		gs.Close()
		s.dbs.Close()
		s.store.Close()
	})
	return s, ts, fake
}

// browser is an HTTP client with a cookie jar that does not follow redirects.
func browser(t *testing.T) *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{
		Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func browserGet(t *testing.T, c *http.Client, u string) *http.Response {
	t.Helper()
	resp, err := c.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

// startGoogle follows /auth/google and returns the state sent to Google.
func startGoogle(t *testing.T, c *http.Client, ts *httptest.Server, fake *fakeGoogle, next string) string {
	t.Helper()
	resp := browserGet(t, c, ts.URL+"/auth/google?next="+url.QueryEscape(next))
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("start: status %d", resp.StatusCode)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if loc.Host != "accounts.google.com" || loc.Query().Get("client_id") != testGoogleClientID {
		t.Fatalf("start: unexpected redirect %s", loc)
	}
	fake.challenge = loc.Query().Get("code_challenge")
	return loc.Query().Get("state")
}

func TestGoogleSignIn(t *testing.T) {
	_, ts, fake := googleTestServer(t)
	c := browser(t)
	state := startGoogle(t, c, ts, fake, "/admin")

	resp := browserGet(t, c, ts.URL+"/auth/google/callback?code=the-code&state="+state)
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/admin" {
		t.Fatalf("callback: status %d, location %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	me, err := c.Get(ts.URL + "/api/me")
	if err != nil {
		t.Fatal(err)
	}
	defer me.Body.Close()
	var u struct{ Email, Name string }
	json.NewDecoder(me.Body).Decode(&u)
	if me.StatusCode != http.StatusOK || u.Email != "admin@example.com" {
		t.Fatalf("me: status %d, user %+v", me.StatusCode, u)
	}
	if u.Name != "Ada Admin" {
		t.Errorf("name not taken from Google: %q", u.Name)
	}
}

func TestGoogleSignInRefusals(t *testing.T) {
	cases := []struct {
		name   string
		claims func(map[string]any)
		state  string // overrides the real state when set
		want   int
	}{
		{name: "state mismatch", state: "forged", want: http.StatusBadRequest},
		{name: "unknown email", claims: func(c map[string]any) { c["email"] = "stranger@example.com" }, want: http.StatusForbidden},
		{name: "unverified email", claims: func(c map[string]any) { c["email_verified"] = false }, want: http.StatusUnauthorized},
		{name: "other audience", claims: func(c map[string]any) { c["aud"] = "someone-else" }, want: http.StatusUnauthorized},
		{name: "expired", claims: func(c map[string]any) { c["exp"] = time.Now().Add(-time.Minute).Unix() }, want: http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ts, fake := googleTestServer(t)
			if tc.claims != nil {
				tc.claims(fake.claims)
			}
			c := browser(t)
			state := startGoogle(t, c, ts, fake, "/admin")
			if tc.state != "" {
				state = tc.state
			}
			resp := browserGet(t, c, ts.URL+"/auth/google/callback?code=the-code&state="+state)
			if resp.StatusCode != tc.want {
				t.Fatalf("status %d, want %d", resp.StatusCode, tc.want)
			}
			if me := browserGet(t, c, ts.URL+"/api/me"); me.StatusCode != http.StatusUnauthorized {
				t.Fatalf("session opened anyway: /api/me status %d", me.StatusCode)
			}
		})
	}
}

func TestGoogleDisabledAccount(t *testing.T) {
	s, ts, fake := googleTestServer(t)
	u, err := s.store.CreateUser("off@example.com", "off", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.SetUserDisabled(u.ID, true); err != nil {
		t.Fatal(err)
	}
	fake.claims["email"] = "off@example.com"
	c := browser(t)
	state := startGoogle(t, c, ts, fake, "/")
	if resp := browserGet(t, c, ts.URL+"/auth/google/callback?code=the-code&state="+state); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status %d, want 403", resp.StatusCode)
	}
}

func TestGoogleReplacesPassword(t *testing.T) {
	_, ts, _ := googleTestServer(t)
	c := &testClient{t: t, base: ts.URL}
	c.mustDo("POST", "/api/auth/login", map[string]string{"email": "admin@example.com", "password": "whatever-password", "confirm": "whatever-password"}, nil, http.StatusForbidden)

	var cfg map[string]bool
	c.mustDo("GET", "/api/auth/config", nil, &cfg, http.StatusOK)
	if !cfg["google"] || cfg["password"] {
		t.Fatalf("auth config %v", cfg)
	}

	resp, err := http.Get(ts.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "/auth/google") || strings.Contains(string(body), `type="password"`) {
		t.Fatal("login page still offers the password form")
	}
}

func TestPasswordServerAuthConfig(t *testing.T) {
	_, ts := testServer(t)
	c := &testClient{t: t, base: ts.URL}
	var cfg map[string]bool
	c.mustDo("GET", "/api/auth/config", nil, &cfg, http.StatusOK)
	if cfg["google"] || !cfg["password"] {
		t.Fatalf("auth config %v", cfg)
	}
	if resp := browserGet(t, browser(t), ts.URL+"/auth/google"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("/auth/google on a password server: status %d", resp.StatusCode)
	}
}

func TestGoogleNeedsBaseURL(t *testing.T) {
	_, err := New(Config{
		DataDir:            t.TempDir(),
		AdminEmail:         "admin@example.com",
		Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		GoogleClientID:     testGoogleClientID,
		GoogleClientSecret: "client-secret",
	})
	if err == nil {
		t.Fatal("New accepted Google sign-in without a base URL")
	}
}

func TestCLISignIn(t *testing.T) {
	_, ts, fake := googleTestServer(t)
	c := browser(t)
	const state = "0123456789abcdef0123"
	cliPath := "/auth/cli?port=5555&state=" + state

	// Anonymous: sent to sign in first, coming back here afterwards.
	resp := browserGet(t, c, ts.URL+cliPath)
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/login?next="+url.QueryEscape(cliPath) {
		t.Fatalf("anonymous: status %d, location %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp := browserGet(t, c, ts.URL+"/auth/cli?port=80&state="+state); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("privileged port accepted: status %d", resp.StatusCode)
	}

	s := startGoogle(t, c, ts, fake, cliPath)
	browserGet(t, c, ts.URL+"/auth/google/callback?code=the-code&state="+s)
	if resp := browserGet(t, c, ts.URL+cliPath); resp.StatusCode != http.StatusOK {
		t.Fatalf("confirmation page: status %d", resp.StatusCode)
	}

	resp, err := c.PostForm(ts.URL+"/auth/cli", url.Values{"port": {"5555"}, "state": {state}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusFound || loc.Host != "127.0.0.1:5555" || loc.Path != "/callback" || loc.Query().Get("state") != state {
		t.Fatalf("confirm: status %d, location %s", resp.StatusCode, loc)
	}
	api := &testClient{t: t, base: ts.URL, token: loc.Query().Get("token")}
	api.mustDo("GET", "/api/me", nil, nil, http.StatusOK)

	// Without a session, the confirmation issues nothing.
	resp, err = http.PostForm(ts.URL+"/auth/cli", url.Values{"port": {"5555"}, "state": {state}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous confirm: status %d", resp.StatusCode)
	}
}
