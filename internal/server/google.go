package server

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/aloisdeniel/cairn/internal/store"
)

// Google sign-in replaces email/password when a Google OAuth client is
// configured. The trust model does not change: only accounts an admin created
// can sign in, matched on the verified Google email. API keys keep working.

const (
	googleAuthURL  = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenURL = "https://oauth2.googleapis.com/token"
	oauthCookie    = "cairn_oauth"
	oauthCookieTTL = 10 * time.Minute
)

func (s *Server) googleEnabled() bool { return s.cfg.GoogleClientID != "" }

func (s *Server) googleRedirectURI() string {
	return strings.TrimRight(s.cfg.BaseURL, "/") + "/auth/google/callback"
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// oauthState is kept in a short-lived HttpOnly cookie between the redirect
// to Google and the callback.
type oauthState struct {
	State    string `json:"s"`
	Verifier string `json:"v"`
	Next     string `json:"n"`
}

// handleGoogleStart sends the browser to Google's consent page, with a state
// value (CSRF) and a PKCE challenge.
func (s *Server) handleGoogleStart(w http.ResponseWriter, r *http.Request) {
	if !s.googleEnabled() {
		http.NotFound(w, r)
		return
	}
	st := oauthState{
		State:    randomToken(),
		Verifier: randomToken(),
		Next:     safeNext(r.URL.Query().Get("next")),
	}
	raw, _ := json.Marshal(st)
	http.SetCookie(w, &http.Cookie{
		Name:     oauthCookie,
		Value:    base64.RawURLEncoding.EncodeToString(raw),
		Path:     "/auth/google",
		MaxAge:   int(oauthCookieTTL.Seconds()),
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
	})
	challenge := sha256.Sum256([]byte(st.Verifier))
	q := url.Values{
		"client_id":             {s.cfg.GoogleClientID},
		"redirect_uri":          {s.googleRedirectURI()},
		"response_type":         {"code"},
		"scope":                 {"openid email profile"},
		"state":                 {st.State},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"code_challenge_method": {"S256"},
		"prompt":                {"select_account"},
	}
	http.Redirect(w, r, s.googleAuthURL+"?"+q.Encode(), http.StatusFound)
}

// handleGoogleCallback exchanges the code, maps the Google identity to an
// existing Cairn account and opens a session.
func (s *Server) handleGoogleCallback(w http.ResponseWriter, r *http.Request) {
	if !s.googleEnabled() {
		http.NotFound(w, r)
		return
	}
	var st oauthState
	c, err := r.Cookie(oauthCookie)
	if err == nil {
		raw, derr := base64.RawURLEncoding.DecodeString(c.Value)
		if derr == nil {
			err = json.Unmarshal(raw, &st)
		} else {
			err = derr
		}
	}
	http.SetCookie(w, &http.Cookie{Name: oauthCookie, Path: "/auth/google", MaxAge: -1, HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode})
	q := r.URL.Query()
	if err != nil || st.State == "" || subtle.ConstantTimeCompare([]byte(st.State), []byte(q.Get("state"))) != 1 {
		s.renderLoginError(w, http.StatusBadRequest, st.Next, "Sign-in expired or was started elsewhere. Try again.")
		return
	}
	if e := q.Get("error"); e != "" {
		s.renderLoginError(w, http.StatusUnauthorized, st.Next, "Google sign-in was cancelled ("+e+").")
		return
	}
	email, name, err := s.exchangeGoogleCode(r, q.Get("code"), st.Verifier)
	if err != nil {
		s.log.Warn("google sign-in failed", "err", err)
		s.renderLoginError(w, http.StatusUnauthorized, st.Next, "Google sign-in failed.")
		return
	}
	u, err := s.store.UserByEmail(email)
	if errors.Is(err, store.ErrNotFound) {
		s.log.Info("google sign-in refused: no account", "email", email)
		s.renderLoginError(w, http.StatusForbidden, st.Next, "No Cairn account for "+email+". Ask an admin to create one.")
		return
	}
	if err != nil {
		s.writeStoreError(w, err, "user")
		return
	}
	if u.Disabled {
		s.renderLoginError(w, http.StatusForbidden, st.Next, "This account is disabled.")
		return
	}
	// Accounts created by an admin are named after their email; take the
	// Google display name the first time.
	if name != "" && u.Name == adminNameFromEmail(u.Email) {
		if err := s.store.UpdateUser(u.ID, name, u.IsAdmin); err == nil {
			u.Name = name
		}
	}
	token, err := s.issueToken(u)
	if err != nil {
		s.writeStoreError(w, err, "token")
		return
	}
	s.setSessionCookie(w, token, s.cfg.TokenTTL)
	s.log.Info("google sign-in", "email", u.Email)
	http.Redirect(w, r, st.Next, http.StatusFound)
}

// exchangeGoogleCode trades the authorization code for an ID token and
// returns its verified email. The ID token comes straight from Google's token
// endpoint over TLS, authenticated with the client secret, so its signature
// does not need checking (OpenID Connect Core §3.1.3.7); its claims still do.
func (s *Server) exchangeGoogleCode(r *http.Request, code, verifier string) (email, name string, err error) {
	if code == "" {
		return "", "", errors.New("missing code")
	}
	form := url.Values{
		"code":          {code},
		"client_id":     {s.cfg.GoogleClientID},
		"client_secret": {s.cfg.GoogleClientSecret},
		"redirect_uri":  {s.googleRedirectURI()},
		"grant_type":    {"authorization_code"},
		"code_verifier": {verifier},
	}
	req, err := http.NewRequestWithContext(r.Context(), "POST", s.googleTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	var tok struct {
		IDToken string `json:"id_token"`
		Error   string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return "", "", fmt.Errorf("token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK || tok.IDToken == "" {
		return "", "", fmt.Errorf("token endpoint: status %d, error %q", resp.StatusCode, tok.Error)
	}
	parts := strings.Split(tok.IDToken, ".")
	if len(parts) != 3 {
		return "", "", errors.New("malformed id_token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", "", fmt.Errorf("id_token payload: %w", err)
	}
	var claims struct {
		Iss           string `json:"iss"`
		Aud           string `json:"aud"`
		Exp           int64  `json:"exp"`
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Name          string `json:"name"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", "", fmt.Errorf("id_token claims: %w", err)
	}
	switch {
	case claims.Iss != "https://accounts.google.com" && claims.Iss != "accounts.google.com":
		return "", "", fmt.Errorf("unexpected issuer %q", claims.Iss)
	case claims.Aud != s.cfg.GoogleClientID:
		return "", "", errors.New("id_token audience mismatch")
	case time.Now().Unix() > claims.Exp:
		return "", "", errors.New("id_token expired")
	case claims.Email == "" || !claims.EmailVerified:
		return "", "", errors.New("Google email missing or not verified")
	}
	return claims.Email, claims.Name, nil
}

func (s *Server) renderLoginError(w http.ResponseWriter, status int, next, msg string) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	if err := s.templates().ExecuteTemplate(w, "login.html", map[string]any{
		"Next":   next,
		"Google": s.googleEnabled(),
		"Error":  msg,
	}); err != nil {
		s.log.Error("render login", "err", err)
	}
}

// handleAuthConfig tells clients (the CLI) which sign-in methods the server
// accepts.
func (s *Server) handleAuthConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{
		"password": !s.googleEnabled(),
		"google":   s.googleEnabled(),
	})
}

// cliLoopback validates the port and state the CLI passes when it waits for
// a token on 127.0.0.1.
func cliLoopback(q url.Values) (port int, state string, ok bool) {
	port, err := strconv.Atoi(q.Get("port"))
	state = q.Get("state")
	if err != nil || port < 1024 || port > 65535 || len(state) < 16 || len(state) > 128 {
		return 0, "", false
	}
	return port, state, true
}

// handleCLIAuthPage asks a signed-in user to confirm handing a session to the
// CLI waiting on this machine. Anonymous visitors sign in first.
func (s *Server) handleCLIAuthPage(w http.ResponseWriter, r *http.Request) {
	port, state, ok := cliLoopback(r.URL.Query())
	if !ok {
		http.Error(w, "invalid CLI login request", http.StatusBadRequest)
		return
	}
	u, err := s.currentUser(r)
	if err != nil || u == nil {
		http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
		return
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if err := s.templates().ExecuteTemplate(w, "cli.html", map[string]any{
		"Email": u.Email,
		"Port":  port,
		"State": state,
	}); err != nil {
		s.log.Error("render cli", "err", err)
	}
}

// handleCLIAuthConfirm issues a token and hands it to the CLI's loopback
// listener. The session cookie is SameSite=Lax, so a cross-site form cannot
// trigger this with the user's session.
func (s *Server) handleCLIAuthConfirm(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	port, state, ok := cliLoopback(r.PostForm)
	if !ok {
		http.Error(w, "invalid CLI login request", http.StatusBadRequest)
		return
	}
	u, err := s.currentUser(r)
	if err != nil || u == nil {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	token, err := s.issueToken(u)
	if err != nil {
		s.writeStoreError(w, err, "token")
		return
	}
	s.log.Info("cli sign-in", "email", u.Email)
	q := url.Values{"state": {state}, "token": {token}}
	http.Redirect(w, r, fmt.Sprintf("http://127.0.0.1:%d/callback?%s", port, q.Encode()), http.StatusFound)
}
