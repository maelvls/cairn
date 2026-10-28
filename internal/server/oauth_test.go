package server

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// oauthTestServer is testServer with the external URLs OAuth needs; the MCP
// endpoint is served from another host, as behind mcp.pi.vls.dev.
func oauthTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	s, err := New(Config{
		DataDir:       t.TempDir(),
		AdminEmail:    "admin@example.com",
		AdminPassword: "admin-password",
		TokenTTL:      time.Hour,
		BaseURL:       "https://cairn.example",
		MCPURL:        "https://mcp.example/cairn/mcp",
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		ts.Close()
		s.dbs.Close()
		s.store.Close()
	})
	return ts
}

var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func pkce() (verifier, challenge string) {
	verifier = randomToken()
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

type oauthFlow struct {
	t        *testing.T
	base     string
	session  string // the user's cairn_token cookie
	clientID string
	secret   string
	redirect string
}

func (f *oauthFlow) authorizeParams(challenge string) url.Values {
	return url.Values{
		"response_type":         {"code"},
		"client_id":             {f.clientID},
		"redirect_uri":          {f.redirect},
		"state":                 {"st4te"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"resource":              {"https://mcp.example/cairn/mcp"},
	}
}

// approve posts the consent form and returns the redirect back to the client.
func (f *oauthFlow) approve(params url.Values, action string) *url.URL {
	f.t.Helper()
	form := url.Values{"action": {action}}
	for k, v := range params {
		form[k] = v
	}
	req, _ := http.NewRequest("POST", f.base+"/oauth/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: tokenCookie, Value: f.session})
	resp, err := noRedirect.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		f.t.Fatalf("approve: status %d", resp.StatusCode)
	}
	u, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		f.t.Fatal(err)
	}
	return u
}

func (f *oauthFlow) code(challenge string) string {
	f.t.Helper()
	u := f.approve(f.authorizeParams(challenge), "allow")
	q := u.Query()
	if q.Get("state") != "st4te" || q.Get("iss") != "https://cairn.example" || q.Get("code") == "" {
		f.t.Fatalf("redirect: %s", u)
	}
	return q.Get("code")
}

// token calls the token endpoint and returns the status and JSON body.
func (f *oauthFlow) token(form url.Values) (int, map[string]any) {
	f.t.Helper()
	req, _ := http.NewRequest("POST", f.base+"/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if f.secret != "" {
		req.SetBasicAuth(f.clientID, f.secret)
	} else {
		form.Set("client_id", f.clientID)
		req.Body = io.NopCloser(strings.NewReader(form.Encode()))
		req.ContentLength = int64(len(form.Encode()))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func register(t *testing.T, base string, body map[string]any) (int, map[string]any) {
	t.Helper()
	c := &testClient{t: t, base: base}
	var out map[string]any
	resp := c.do("POST", "/oauth/register", body, &out)
	return resp.StatusCode, out
}

func TestOAuthMCPFlow(t *testing.T) {
	ts := oauthTestServer(t)
	admin := login(t, ts.URL, "admin@example.com", "admin-password")

	// Discovery starts with the 401 challenge on /mcp.
	resp := (&testClient{t: t, base: ts.URL}).do("POST", "/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping"}, nil)
	want := `Bearer resource_metadata="https://mcp.example/.well-known/oauth-protected-resource/cairn/mcp"`
	if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") != want {
		t.Fatalf("challenge: %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
	var prm, asm map[string]any
	anon := &testClient{t: t, base: ts.URL}
	anon.mustDo("GET", "/.well-known/oauth-protected-resource/cairn/mcp", nil, &prm, http.StatusOK)
	if prm["resource"] != "https://mcp.example/cairn/mcp" || prm["authorization_servers"].([]any)[0] != "https://cairn.example" {
		t.Fatalf("protected resource metadata: %v", prm)
	}
	anon.mustDo("GET", "/.well-known/oauth-authorization-server", nil, &asm, http.StatusOK)
	if asm["token_endpoint"] != "https://cairn.example/oauth/token" || asm["registration_endpoint"] != "https://cairn.example/oauth/register" {
		t.Fatalf("authorization server metadata: %v", asm)
	}

	if status, _ := register(t, ts.URL, map[string]any{"redirect_uris": []string{"http://evil.example/cb"}}); status != http.StatusBadRequest {
		t.Fatalf("plain http redirect URI accepted: %d", status)
	}
	status, reg := register(t, ts.URL, map[string]any{
		"client_name":                "Test client",
		"redirect_uris":              []string{"https://client.example/cb"},
		"token_endpoint_auth_method": "none",
	})
	if status != http.StatusCreated || reg["client_secret"] != nil {
		t.Fatalf("register: %d %v", status, reg)
	}
	f := &oauthFlow{t: t, base: ts.URL, session: admin.token, clientID: reg["client_id"].(string), redirect: "https://client.example/cb"}
	verifier, challenge := pkce()

	// Signed out: the authorize page sends the user to the login page first.
	authorizeURL := ts.URL + "/oauth/authorize?" + f.authorizeParams(challenge).Encode()
	resp, err := noRedirect.Get(authorizeURL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); resp.StatusCode != http.StatusFound || !strings.HasPrefix(loc, "/login?next=%2Foauth%2Fauthorize") {
		t.Fatalf("anonymous authorize: %d %s", resp.StatusCode, loc)
	}
	req, _ := http.NewRequest("GET", authorizeURL, nil)
	req.AddCookie(&http.Cookie{Name: tokenCookie, Value: admin.token})
	resp, err = noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(page), "Test client") || !strings.Contains(string(page), "client.example") {
		t.Fatalf("consent page: %d", resp.StatusCode)
	}

	// Denying goes back with access_denied.
	if u := f.approve(f.authorizeParams(challenge), "deny"); u.Query().Get("error") != "access_denied" {
		t.Fatalf("deny: %s", u)
	}
	// A resource other than the MCP endpoint is refused.
	params := f.authorizeParams(challenge)
	params.Set("resource", "https://other.example/mcp")
	if u := f.approve(params, "allow"); u.Query().Get("error") != "invalid_target" {
		t.Fatalf("other resource: %s", u)
	}

	// A wrong verifier burns the code.
	code := f.code(challenge)
	if status, out := f.token(url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {f.redirect}, "code_verifier": {randomToken()}}); status != http.StatusBadRequest || out["error"] != "invalid_grant" {
		t.Fatalf("wrong verifier: %d %v", status, out)
	}
	if status, _ := f.token(url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {f.redirect}, "code_verifier": {verifier}}); status != http.StatusBadRequest {
		t.Fatalf("code reused: %d", status)
	}

	code = f.code(challenge)
	status, tok := f.token(url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {f.redirect}, "code_verifier": {verifier}})
	if status != http.StatusOK || tok["token_type"] != "Bearer" || tok["refresh_token"] == nil {
		t.Fatalf("token: %d %v", status, tok)
	}

	// The access token opens /mcp, and nothing else.
	mc := &testClient{t: t, base: ts.URL, token: tok["access_token"].(string)}
	if r := mcpCall(t, mc, "tools/list", nil); r.Error != nil {
		t.Fatalf("tools/list with OAuth token: %v", r.Error)
	}
	mc.mustDo("GET", "/api/me", nil, nil, http.StatusUnauthorized)
	mc.mustDo("GET", "/api/artifacts", nil, nil, http.StatusUnauthorized)

	// Refresh tokens rotate.
	status, tok2 := f.token(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok["refresh_token"].(string)}})
	if status != http.StatusOK || tok2["access_token"] == nil {
		t.Fatalf("refresh: %d %v", status, tok2)
	}
	if status, _ := f.token(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok["refresh_token"].(string)}}); status != http.StatusBadRequest {
		t.Fatalf("refresh token reused: %d", status)
	}
	mc.token = tok2["access_token"].(string)
	if r := mcpCall(t, mc, "ping", nil); r.Error != nil {
		t.Fatalf("ping with refreshed token: %v", r.Error)
	}
}

func TestOAuthConfidentialClient(t *testing.T) {
	ts := oauthTestServer(t)
	admin := login(t, ts.URL, "admin@example.com", "admin-password")
	status, reg := register(t, ts.URL, map[string]any{"redirect_uris": []string{"https://client.example/cb"}})
	if status != http.StatusCreated || reg["client_secret"] == nil || reg["token_endpoint_auth_method"] != "client_secret_basic" {
		t.Fatalf("register: %d %v", status, reg)
	}
	f := &oauthFlow{t: t, base: ts.URL, session: admin.token, clientID: reg["client_id"].(string), redirect: "https://client.example/cb"}
	verifier, challenge := pkce()

	// Without its secret the client is refused, and the code stays usable.
	code := f.code(challenge)
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {verifier}}
	if status, out := f.token(form); status != http.StatusUnauthorized || out["error"] != "invalid_client" {
		t.Fatalf("no secret: %d %v", status, out)
	}
	f.secret = reg["client_secret"].(string)
	if status, out := f.token(form); status != http.StatusOK {
		t.Fatalf("with secret: %d %v", status, out)
	}
}
