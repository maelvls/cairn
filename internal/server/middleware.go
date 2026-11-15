package server

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aloisdeniel/cairn/internal/auth"
	"github.com/aloisdeniel/cairn/internal/store"
)

const tokenCookie = "cairn_token"

type ctxKey int

const userCtxKey ctxKey = iota

// currentUser resolves the request identity from, in order: the Authorization
// bearer credential (JWT or API key) or the session cookie. It returns
// (nil, nil) for anonymous requests and an error only for credentials that are
// present but invalid.
func (s *Server) currentUser(r *http.Request) (*store.User, error) {
	if u, ok := r.Context().Value(userCtxKey).(*store.User); ok {
		return u, nil
	}
	cred := ""
	if h := r.Header.Get("Authorization"); h != "" {
		var ok bool
		cred, ok = strings.CutPrefix(h, "Bearer ")
		if !ok {
			return nil, errors.New("unsupported Authorization scheme")
		}
	} else if c, err := r.Cookie(tokenCookie); err == nil {
		cred = c.Value
	}
	if cred == "" {
		return nil, nil
	}
	if auth.IsAPIKey(cred) {
		return s.userFromAPIKey(cred)
	}
	return s.userFromJWT(r, cred)
}

func (s *Server) userFromJWT(r *http.Request, token string) (*store.User, error) {
	claims, err := auth.VerifyJWT(s.secret, token)
	if err != nil {
		return nil, err
	}
	// OAuth access tokens are bound to the MCP endpoint: they open nothing
	// else, and only the resource they were issued for.
	if claims.Audience != "" && (r.URL.Path != "/mcp" || claims.Audience != s.mcpResource()) {
		return nil, auth.ErrInvalidToken
	}
	u, err := s.store.UserByID(claims.UserID)
	if err != nil {
		return nil, auth.ErrInvalidToken
	}
	if u.Disabled || u.TokenVersion != claims.TokenVersion {
		return nil, auth.ErrInvalidToken
	}
	return u, nil
}

func (s *Server) userFromAPIKey(token string) (*store.User, error) {
	id, secret, ok := auth.ParseAPIKey(token)
	if !ok {
		return nil, errors.New("malformed API key")
	}
	key, err := s.store.APIKeyByID(id)
	if err != nil {
		return nil, errors.New("unknown API key")
	}
	if key.RevokedAt != "" || key.SecretHash != auth.HashAPIKeySecret(secret) {
		return nil, errors.New("invalid API key")
	}
	u, err := s.store.UserByID(key.UserID)
	if err != nil || u.Disabled {
		return nil, errors.New("invalid API key")
	}
	return u, nil
}

// withUser stores the resolved user in the request context so handlers can
// read it back cheaply via requestUser.
func withUser(r *http.Request, u *store.User) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), userCtxKey, u))
}

// requestUser returns the user attached by requireAuth/requireAdmin (never nil
// inside those handlers).
func requestUser(r *http.Request) *store.User {
	u, _ := r.Context().Value(userCtxKey).(*store.User)
	return u
}

// requireAuth rejects anonymous or invalidly-authenticated API requests.
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, err := s.currentUser(r)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "invalid credentials")
			return
		}
		if u == nil {
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		next(w, withUser(r, u))
	}
}

func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return s.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if !requestUser(r).IsAdmin {
			writeError(w, http.StatusForbidden, "admin access required")
			return
		}
		next(w, r)
	})
}

// setSessionCookie installs the JWT as an HttpOnly cookie so browser page
// loads and same-origin fetches are authenticated with the same login.
func (s *Server) setSessionCookie(w http.ResponseWriter, token string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     tokenCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   int(ttl.Seconds()),
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     tokenCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// safeNext validates a ?next= redirect target: relative paths only.
func safeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return "/"
	}
	if u, err := url.Parse(next); err != nil || u.Host != "" || u.Scheme != "" {
		return "/"
	}
	return next
}

// refreshSession renews session JWTs past half their lifetime: the new token
// goes back in auth.RefreshHeader (bearer clients such as the CLI save it)
// and, when the token came from the cookie, in a fresh cookie. API keys and
// OAuth access tokens (which carry an audience) are left alone. Expired or
// revoked tokens are not renewed: the request fails as usual.
func (s *Server) refreshSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token, fromCookie := s.renewedToken(r); token != "" {
			w.Header().Set(auth.RefreshHeader, token)
			if fromCookie {
				s.setSessionCookie(w, token, s.cfg.TokenTTL)
			}
		}
		next.ServeHTTP(w, r)
	})
}

// renewedToken returns a new session token when the request's one is valid
// and due for renewal, plus whether it was presented as the cookie.
func (s *Server) renewedToken(r *http.Request) (string, bool) {
	if r.URL.Path == "/api/auth/logout" || r.URL.Path == "/logout" {
		return "", false
	}
	cred, fromCookie := "", false
	if h := r.Header.Get("Authorization"); h != "" {
		cred, _ = strings.CutPrefix(h, "Bearer ")
	} else if c, err := r.Cookie(tokenCookie); err == nil {
		cred, fromCookie = c.Value, true
	}
	if cred == "" || auth.IsAPIKey(cred) {
		return "", false
	}
	claims, err := auth.VerifyJWT(s.secret, cred)
	if err != nil || claims.Audience != "" {
		return "", false
	}
	if time.Until(time.Unix(claims.ExpiresAt, 0)) > s.cfg.TokenTTL/2 {
		return "", false
	}
	u, err := s.store.UserByID(claims.UserID)
	if err != nil || u.Disabled || u.TokenVersion != claims.TokenVersion {
		return "", false
	}
	token, err := s.issueToken(u)
	if err != nil {
		s.log.Warn("session refresh failed", "user", u.ID, "err", err)
		return "", false
	}
	return token, fromCookie
}
