package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/aloisdeniel/cairn/internal/auth"
)

// agedToken signs a session token for the admin that expires in left.
func agedToken(t *testing.T, s *Server, left time.Duration, tweak func(*auth.Claims)) string {
	t.Helper()
	u, err := s.store.UserByEmail("admin@example.com")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	c := auth.Claims{
		UserID:       u.ID,
		IsAdmin:      u.IsAdmin,
		TokenVersion: u.TokenVersion,
		IssuedAt:     now.Add(left - s.cfg.TokenTTL).Unix(),
		ExpiresAt:    now.Add(left).Unix(),
	}
	if tweak != nil {
		tweak(&c)
	}
	tok, err := auth.SignJWT(s.secret, c)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func getMe(t *testing.T, base string, set func(*http.Request)) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", base+"/api/me", nil)
	set(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func bearer(tok string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }
}

func TestSessionRefreshBearer(t *testing.T) {
	s, ts := testServer(t) // TokenTTL = 1h

	// A fresh login is not renewed.
	c := login(t, ts.URL, "admin@example.com", "admin-password")
	if resp := getMe(t, ts.URL, bearer(c.token)); resp.Header.Get(auth.RefreshHeader) != "" {
		t.Fatal("fresh token was renewed")
	}

	// Past half its lifetime, the response carries a new full-length token.
	old := agedToken(t, s, 20*time.Minute, nil)
	resp := getMe(t, ts.URL, bearer(old))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	renewed := resp.Header.Get(auth.RefreshHeader)
	if renewed == "" || renewed == old {
		t.Fatal("aged token not renewed")
	}
	claims, err := auth.VerifyJWT(s.secret, renewed)
	if err != nil {
		t.Fatal(err)
	}
	if left := time.Until(time.Unix(claims.ExpiresAt, 0)); left < 59*time.Minute {
		t.Fatalf("renewed token expires in %v, want ~1h", left)
	}
	if resp := getMe(t, ts.URL, bearer(renewed)); resp.StatusCode != http.StatusOK {
		t.Fatalf("renewed token rejected: %d", resp.StatusCode)
	}
}

func TestSessionRefreshCookie(t *testing.T) {
	s, ts := testServer(t)
	old := agedToken(t, s, 10*time.Minute, nil)
	resp := getMe(t, ts.URL, func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: tokenCookie, Value: old})
	})
	var got *http.Cookie
	for _, ck := range resp.Cookies() {
		if ck.Name == tokenCookie {
			got = ck
		}
	}
	if got == nil || got.Value == "" || got.Value == old {
		t.Fatal("cookie session not renewed")
	}
	if got.MaxAge != int(time.Hour.Seconds()) || !got.HttpOnly {
		t.Fatalf("renewed cookie: MaxAge=%d HttpOnly=%v", got.MaxAge, got.HttpOnly)
	}
}

func TestSessionRefreshSkips(t *testing.T) {
	s, ts := testServer(t)

	// OAuth access tokens (with an audience) are never renewed.
	oauth := agedToken(t, s, time.Minute, func(c *auth.Claims) { c.Audience = s.mcpResource() })
	if resp := getMe(t, ts.URL, bearer(oauth)); resp.Header.Get(auth.RefreshHeader) != "" {
		t.Fatal("OAuth access token renewed")
	}

	// Revoked (stale token_version) and expired tokens are refused, not renewed.
	revoked := agedToken(t, s, time.Minute, func(c *auth.Claims) { c.TokenVersion-- })
	resp := getMe(t, ts.URL, bearer(revoked))
	if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get(auth.RefreshHeader) != "" {
		t.Fatalf("revoked token: status %d, header %q", resp.StatusCode, resp.Header.Get(auth.RefreshHeader))
	}
	expired := agedToken(t, s, -time.Minute, nil)
	resp = getMe(t, ts.URL, bearer(expired))
	if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get(auth.RefreshHeader) != "" {
		t.Fatalf("expired token: status %d", resp.StatusCode)
	}

	// Logging out does not hand back a fresh cookie.
	req, _ := http.NewRequest("POST", ts.URL+"/api/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: tokenCookie, Value: agedToken(t, s, time.Minute, nil)})
	lo, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	lo.Body.Close()
	for _, ck := range lo.Cookies() {
		if ck.Name == tokenCookie && ck.Value != "" {
			t.Fatal("logout renewed the session cookie")
		}
	}
}
