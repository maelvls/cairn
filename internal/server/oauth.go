package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aloisdeniel/cairn/internal/auth"
	"github.com/aloisdeniel/cairn/internal/store"
)

// OAuth 2.1 authorization server, so that MCP clients (claude.ai, grok.com…)
// connect to /mcp the way the MCP spec expects: protected resource metadata
// (RFC 9728) pointing at this server's metadata (RFC 8414), dynamic client
// registration (RFC 7591), authorization code with mandatory PKCE S256, and
// rotating refresh tokens. The user signs in with Cairn's usual login (Google
// when configured) and approves the client on a consent page; only existing
// Cairn accounts can, as everywhere else.
//
// Access tokens are Cairn JWTs carrying an audience: the MCP endpoint URL.
// userFromJWT refuses them anywhere but /mcp. The issuer is BaseURL, where
// the session cookie lives; the MCP endpoint itself may be served from
// another host (MCPURL) by a reverse proxy.

const (
	oauthCodeTTL    = 5 * time.Minute
	oauthAccessTTL  = time.Hour
	oauthRefreshTTL = 30 * 24 * time.Hour
)

// oauthEnabled: the issuer URL comes from BaseURL.
func (s *Server) oauthEnabled() bool { return s.cfg.BaseURL != "" }

func (s *Server) issuer() string { return strings.TrimRight(s.cfg.BaseURL, "/") }

// mcpResource is the MCP endpoint's external URL, the audience of access
// tokens.
func (s *Server) mcpResource() string {
	if s.cfg.MCPURL != "" {
		return strings.TrimRight(s.cfg.MCPURL, "/")
	}
	return s.issuer() + "/mcp"
}

// protectedResourcePath is where RFC 9728 puts the resource's metadata on
// its own host: the well-known prefix, then the resource's path.
func (s *Server) protectedResourcePath() string {
	p := "/mcp"
	if u, err := url.Parse(s.mcpResource()); err == nil && u.Path != "" && u.Path != "/" {
		p = u.Path
	}
	return "/.well-known/oauth-protected-resource" + p
}

func (s *Server) protectedResourceMetadataURL() string {
	u, err := url.Parse(s.mcpResource())
	if err != nil {
		return s.protectedResourcePath()
	}
	return u.Scheme + "://" + u.Host + s.protectedResourcePath()
}

// oauthCORS lets browser-based MCP clients reach the metadata, registration,
// token and MCP endpoints. None of them rely on cookies across origins: the
// session cookie is SameSite=Lax and a wildcard origin never carries
// credentials.
func oauthCORS(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Mcp-Protocol-Version, Mcp-Session-Id")
	h.Set("Access-Control-Expose-Headers", "WWW-Authenticate, Mcp-Session-Id")
}

func (s *Server) handleOAuthPreflight(w http.ResponseWriter, r *http.Request) {
	oauthCORS(w)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAuthServerMetadata(w http.ResponseWriter, r *http.Request) {
	if !s.oauthEnabled() {
		http.NotFound(w, r)
		return
	}
	oauthCORS(w)
	iss := s.issuer()
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                         iss,
		"authorization_endpoint":                         iss + "/oauth/authorize",
		"token_endpoint":                                 iss + "/oauth/token",
		"registration_endpoint":                          iss + "/oauth/register",
		"response_types_supported":                       []string{"code"},
		"grant_types_supported":                          []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":               []string{"S256"},
		"token_endpoint_auth_methods_supported":          []string{"none", "client_secret_basic", "client_secret_post"},
		"authorization_response_iss_parameter_supported": true,
	})
}

func (s *Server) handleProtectedResourceMetadata(w http.ResponseWriter, r *http.Request) {
	if !s.oauthEnabled() {
		http.NotFound(w, r)
		return
	}
	oauthCORS(w)
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":                 s.mcpResource(),
		"authorization_servers":    []string{s.issuer()},
		"bearer_methods_supported": []string{"header"},
		"resource_name":            "Cairn",
	})
}

// requireMCPAuth is requireAuth for /mcp: a missing or invalid token gets
// the WWW-Authenticate challenge that starts an MCP client's OAuth flow.
func (s *Server) requireMCPAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		oauthCORS(w)
		u, err := s.currentUser(r)
		if err != nil || u == nil {
			if s.oauthEnabled() {
				challenge := fmt.Sprintf(`Bearer resource_metadata=%q`, s.protectedResourceMetadataURL())
				if err != nil {
					challenge += `, error="invalid_token"`
				}
				w.Header().Set("WWW-Authenticate", challenge)
			}
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		next(w, withUser(r, u))
	}
}

// Dynamic client registration (RFC 7591). Open to anyone, like everywhere
// else: a client gets nothing until a signed-in user approves it.

type oauthError struct {
	Error       string `json:"error"`
	Description string `json:"error_description,omitempty"`
}

func writeOAuthError(w http.ResponseWriter, status int, code, desc string) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, oauthError{Error: code, Description: desc})
}

// validRedirectURI accepts https URLs, http on loopback (native apps and
// local tools) and private-use schemes (RFC 8252), never fragments or
// schemes that run code in the browser.
func validRedirectURI(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Fragment != "" || len(raw) > 2000 {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return u.Host != ""
	case "http":
		h := u.Hostname()
		return h == "localhost" || h == "127.0.0.1" || h == "::1"
	case "javascript", "data", "file", "vbscript", "about", "blob":
		return false
	}
	return true
}

func (s *Server) handleOAuthRegister(w http.ResponseWriter, r *http.Request) {
	oauthCORS(w)
	if !s.oauthEnabled() {
		http.NotFound(w, r)
		return
	}
	var req struct {
		RedirectURIs            []string `json:"redirect_uris"`
		ClientName              string   `json:"client_name"`
		TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "invalid JSON body")
		return
	}
	if len(req.RedirectURIs) == 0 || len(req.RedirectURIs) > 10 {
		writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri", "between 1 and 10 redirect_uris are required")
		return
	}
	for _, u := range req.RedirectURIs {
		if !validRedirectURI(u) {
			writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri", "unsupported redirect URI: "+u)
			return
		}
	}
	if len(req.ClientName) > 200 {
		req.ClientName = req.ClientName[:200]
	}
	c := &store.OAuthClient{ID: randomToken(), Name: req.ClientName, RedirectURIs: req.RedirectURIs}
	secret := ""
	switch req.TokenEndpointAuthMethod {
	case "none":
	case "", "client_secret_basic", "client_secret_post":
		// RFC 7591's default is client_secret_basic.
		if req.TokenEndpointAuthMethod == "" {
			req.TokenEndpointAuthMethod = "client_secret_basic"
		}
		secret = randomToken()
		c.SecretHash = auth.HashAPIKeySecret(secret)
	default:
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "unsupported token_endpoint_auth_method")
		return
	}
	if err := s.store.CreateOAuthClient(c); err != nil {
		s.writeStoreError(w, err, "client")
		return
	}
	s.log.Info("oauth client registered", "client", c.Name, "redirect_uris", strings.Join(c.RedirectURIs, " "))
	resp := map[string]any{
		"client_id":                  c.ID,
		"client_id_issued_at":        time.Now().Unix(),
		"client_name":                c.Name,
		"redirect_uris":              c.RedirectURIs,
		"token_endpoint_auth_method": req.TokenEndpointAuthMethod,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
	}
	if secret != "" {
		resp["client_secret"] = secret
		resp["client_secret_expires_at"] = 0
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, resp)
}

// Authorization endpoint

type authzRequest struct {
	Client      *store.OAuthClient
	RedirectURI string
	State       string
	Challenge   string
	Resource    string
}

// parseAuthorize validates an authorization request. Until the client and
// redirect URI are known good, errors are shown to the user (redirect is
// false); after that they go back to the client.
func (s *Server) parseAuthorize(v url.Values) (req *authzRequest, redirect bool, code, desc string) {
	c, err := s.store.OAuthClientByID(v.Get("client_id"))
	if err != nil {
		return nil, false, "invalid_client", "Unknown client. Remove the connector and add it again."
	}
	req = &authzRequest{Client: c, RedirectURI: v.Get("redirect_uri"), State: v.Get("state")}
	if req.RedirectURI == "" && len(c.RedirectURIs) == 1 {
		req.RedirectURI = c.RedirectURIs[0]
	}
	registered := false
	for _, u := range c.RedirectURIs {
		registered = registered || u == req.RedirectURI
	}
	if !registered {
		return nil, false, "invalid_request", "This redirect URI was not registered by the client."
	}
	switch {
	case v.Get("response_type") != "code":
		return req, true, "unsupported_response_type", "only response_type=code is supported"
	case v.Get("code_challenge") == "" || v.Get("code_challenge_method") != "S256":
		return req, true, "invalid_request", "PKCE with code_challenge_method=S256 is required"
	}
	req.Challenge = v.Get("code_challenge")
	req.Resource = s.mcpResource()
	if res := strings.TrimRight(v.Get("resource"), "/"); res != "" && res != req.Resource {
		return req, true, "invalid_target", "the only resource here is " + req.Resource
	}
	return req, false, "", ""
}

// redirectToClient sends the browser back to the client with the given
// parameters plus iss (RFC 9207).
func (s *Server) redirectToClient(w http.ResponseWriter, r *http.Request, req *authzRequest, params url.Values) {
	u, _ := url.Parse(req.RedirectURI)
	q := u.Query()
	for k, vs := range params {
		q[k] = vs
	}
	if req.State != "" {
		q.Set("state", req.State)
	}
	q.Set("iss", s.issuer())
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusSeeOther)
}

func (s *Server) authorizeError(w http.ResponseWriter, r *http.Request, req *authzRequest, redirect bool, code, desc string) {
	if redirect {
		s.redirectToClient(w, r, req, url.Values{"error": {code}, "error_description": {desc}})
		return
	}
	http.Error(w, desc, http.StatusBadRequest)
}

// handleOAuthAuthorize shows the consent page, after sign-in.
func (s *Server) handleOAuthAuthorize(w http.ResponseWriter, r *http.Request) {
	if !s.oauthEnabled() {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	req, redirect, code, desc := s.parseAuthorize(q)
	if code != "" {
		s.authorizeError(w, r, req, redirect, code, desc)
		return
	}
	u, err := s.currentUser(r)
	if err != nil || u == nil {
		http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
		return
	}
	name := req.Client.Name
	if name == "" {
		name = "An unnamed application"
	}
	dest := req.RedirectURI
	if ru, err := url.Parse(req.RedirectURI); err == nil && ru.Host != "" {
		dest = ru.Host
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
	if err := s.templates().ExecuteTemplate(w, "oauth.html", map[string]any{
		"Email":    u.Email,
		"Client":   name,
		"Dest":     dest,
		"Resource": req.Resource,
		"Params":   q,
	}); err != nil {
		s.log.Error("render oauth", "err", err)
	}
}

// handleOAuthApprove issues the authorization code once the user clicked
// Allow. The session cookie is SameSite=Lax, so a cross-site form cannot
// submit this with the user's session.
func (s *Server) handleOAuthApprove(w http.ResponseWriter, r *http.Request) {
	if !s.oauthEnabled() {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	req, redirect, code, desc := s.parseAuthorize(r.PostForm)
	if code != "" {
		s.authorizeError(w, r, req, redirect, code, desc)
		return
	}
	u, err := s.currentUser(r)
	if err != nil || u == nil {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	if r.PostForm.Get("action") != "allow" {
		s.redirectToClient(w, r, req, url.Values{"error": {"access_denied"}})
		return
	}
	authCode := randomToken()
	err = s.store.CreateOAuthCode(auth.HashAPIKeySecret(authCode), store.OAuthGrant{
		ClientID:      req.Client.ID,
		UserID:        u.ID,
		RedirectURI:   req.RedirectURI,
		CodeChallenge: req.Challenge,
		Resource:      req.Resource,
	}, oauthCodeTTL)
	if err != nil {
		s.writeStoreError(w, err, "code")
		return
	}
	s.log.Info("oauth client approved", "client", req.Client.Name, "email", u.Email)
	s.redirectToClient(w, r, req, url.Values{"code": {authCode}})
}

// Token endpoint

func (s *Server) handleOAuthToken(w http.ResponseWriter, r *http.Request) {
	oauthCORS(w)
	if !s.oauthEnabled() {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "invalid form body")
		return
	}
	f := r.PostForm

	// Client authentication: HTTP Basic (form-urlencoded parts, RFC 6749
	// §2.3.1), or client_id/client_secret in the body.
	clientID, secret := f.Get("client_id"), f.Get("client_secret")
	if id, sec, ok := r.BasicAuth(); ok {
		clientID, _ = url.QueryUnescape(id)
		secret, _ = url.QueryUnescape(sec)
	}
	client, err := s.store.OAuthClientByID(clientID)
	if err != nil {
		writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "unknown client")
		return
	}
	if client.SecretHash != "" && subtle.ConstantTimeCompare([]byte(auth.HashAPIKeySecret(secret)), []byte(client.SecretHash)) != 1 {
		writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "bad client secret")
		return
	}

	var g *store.OAuthGrant
	switch f.Get("grant_type") {
	case "authorization_code":
		g, err = s.store.ConsumeOAuthCode(auth.HashAPIKeySecret(f.Get("code")))
		if err != nil || g.ClientID != client.ID {
			writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "unknown, expired or already used code")
			return
		}
		if ru := f.Get("redirect_uri"); ru != "" && ru != g.RedirectURI {
			writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri does not match")
			return
		}
		verifier := f.Get("code_verifier")
		sum := sha256.Sum256([]byte(verifier))
		if len(verifier) < 43 || len(verifier) > 128 ||
			subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(sum[:])), []byte(g.CodeChallenge)) != 1 {
			writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "PKCE verification failed")
			return
		}
	case "refresh_token":
		g, err = s.store.ConsumeOAuthRefreshToken(auth.HashAPIKeySecret(f.Get("refresh_token")))
		if err != nil || g.ClientID != client.ID {
			writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "unknown or expired refresh token")
			return
		}
	default:
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type", "use authorization_code or refresh_token")
		return
	}
	if res := strings.TrimRight(f.Get("resource"), "/"); res != "" && res != g.Resource {
		writeOAuthError(w, http.StatusBadRequest, "invalid_target", "the only resource here is "+g.Resource)
		return
	}
	u, err := s.store.UserByID(g.UserID)
	if err != nil || u.Disabled || (g.TokenVersion != 0 && g.TokenVersion != u.TokenVersion) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "the account was disabled or its sessions revoked")
		return
	}

	nowT := time.Now()
	access, err := auth.SignJWT(s.secret, auth.Claims{
		UserID:       u.ID,
		IsAdmin:      u.IsAdmin,
		TokenVersion: u.TokenVersion,
		IssuedAt:     nowT.Unix(),
		ExpiresAt:    nowT.Add(oauthAccessTTL).Unix(),
		Audience:     g.Resource,
	})
	if err != nil {
		s.writeStoreError(w, err, "token")
		return
	}
	refresh := randomToken()
	err = s.store.CreateOAuthRefreshToken(auth.HashAPIKeySecret(refresh), store.OAuthGrant{
		ClientID:     client.ID,
		UserID:       u.ID,
		Resource:     g.Resource,
		TokenVersion: u.TokenVersion,
	}, oauthRefreshTTL)
	if err != nil {
		s.writeStoreError(w, err, "token")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":  access,
		"token_type":    "Bearer",
		"expires_in":    int(oauthAccessTTL.Seconds()),
		"refresh_token": refresh,
	})
}
