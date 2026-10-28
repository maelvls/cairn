package store

import (
	"encoding/json"
	"time"
)

// OAuthClient is a dynamically registered OAuth client (RFC 7591).
type OAuthClient struct {
	ID           string
	SecretHash   string // empty for public clients
	Name         string
	RedirectURIs []string
	CreatedAt    string
}

// OAuthGrant is what an authorization code or a refresh token stands for.
type OAuthGrant struct {
	ClientID      string
	UserID        string
	RedirectURI   string // codes only
	CodeChallenge string // codes only
	Resource      string
	TokenVersion  int // refresh tokens only
}

func (s *Store) CreateOAuthClient(c *OAuthClient) error {
	uris, err := json.Marshal(c.RedirectURIs)
	if err != nil {
		return err
	}
	c.CreatedAt = now()
	_, err = s.db.Exec(`INSERT INTO oauth_clients (id, secret_hash, name, redirect_uris, created_at) VALUES (?, ?, ?, ?, ?)`,
		c.ID, c.SecretHash, c.Name, string(uris), c.CreatedAt)
	return err
}

func (s *Store) OAuthClientByID(id string) (*OAuthClient, error) {
	var c OAuthClient
	var uris string
	err := s.db.QueryRow(`SELECT id, secret_hash, name, redirect_uris, created_at FROM oauth_clients WHERE id = ?`, id).
		Scan(&c.ID, &c.SecretHash, &c.Name, &uris, &c.CreatedAt)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(uris), &c.RedirectURIs); err != nil {
		return nil, err
	}
	return &c, nil
}

// CreateOAuthCode stores an authorization code (by hash) and drops expired
// ones.
func (s *Store) CreateOAuthCode(codeHash string, g OAuthGrant, ttl time.Duration) error {
	t := time.Now().Unix()
	s.db.Exec(`DELETE FROM oauth_codes WHERE expires_at < ?`, t)
	_, err := s.db.Exec(`INSERT INTO oauth_codes (code_hash, client_id, user_id, redirect_uri, code_challenge, resource, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		codeHash, g.ClientID, g.UserID, g.RedirectURI, g.CodeChallenge, g.Resource, t+int64(ttl.Seconds()))
	return err
}

// ConsumeOAuthCode deletes and returns an unexpired code: codes are single
// use even when the exchange then fails. ErrNotFound when unknown or expired.
func (s *Store) ConsumeOAuthCode(codeHash string) (*OAuthGrant, error) {
	var g OAuthGrant
	var exp int64
	err := s.db.QueryRow(`DELETE FROM oauth_codes WHERE code_hash = ? RETURNING client_id, user_id, redirect_uri, code_challenge, resource, expires_at`, codeHash).
		Scan(&g.ClientID, &g.UserID, &g.RedirectURI, &g.CodeChallenge, &g.Resource, &exp)
	if err != nil {
		return nil, err
	}
	if exp < time.Now().Unix() {
		return nil, ErrNotFound
	}
	return &g, nil
}

func (s *Store) CreateOAuthRefreshToken(tokenHash string, g OAuthGrant, ttl time.Duration) error {
	t := time.Now().Unix()
	s.db.Exec(`DELETE FROM oauth_refresh_tokens WHERE expires_at < ?`, t)
	_, err := s.db.Exec(`INSERT INTO oauth_refresh_tokens (token_hash, client_id, user_id, resource, token_version, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		tokenHash, g.ClientID, g.UserID, g.Resource, g.TokenVersion, now(), t+int64(ttl.Seconds()))
	return err
}

// ConsumeOAuthRefreshToken deletes and returns an unexpired refresh token
// (they rotate on every use). ErrNotFound when unknown or expired.
func (s *Store) ConsumeOAuthRefreshToken(tokenHash string) (*OAuthGrant, error) {
	var g OAuthGrant
	var exp int64
	err := s.db.QueryRow(`DELETE FROM oauth_refresh_tokens WHERE token_hash = ? RETURNING client_id, user_id, resource, token_version, expires_at`, tokenHash).
		Scan(&g.ClientID, &g.UserID, &g.Resource, &g.TokenVersion, &exp)
	if err != nil {
		return nil, err
	}
	if exp < time.Now().Unix() {
		return nil, ErrNotFound
	}
	return &g, nil
}
