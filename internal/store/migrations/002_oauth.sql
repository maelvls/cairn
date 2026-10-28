-- OAuth 2.1 authorization server for MCP clients (see internal/server/oauth.go).
-- Only hashes of codes, secrets and refresh tokens are stored.

CREATE TABLE oauth_clients (
    id            TEXT PRIMARY KEY,
    secret_hash   TEXT NOT NULL DEFAULT '', -- empty: public client (PKCE only)
    name          TEXT NOT NULL DEFAULT '',
    redirect_uris TEXT NOT NULL,            -- JSON array
    created_at    TEXT NOT NULL
);

CREATE TABLE oauth_codes (
    code_hash      TEXT PRIMARY KEY,
    client_id      TEXT NOT NULL REFERENCES oauth_clients(id) ON DELETE CASCADE,
    user_id        TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    redirect_uri   TEXT NOT NULL,
    code_challenge TEXT NOT NULL,
    resource       TEXT NOT NULL,
    expires_at     INTEGER NOT NULL
);

CREATE TABLE oauth_refresh_tokens (
    token_hash    TEXT PRIMARY KEY,
    client_id     TEXT NOT NULL REFERENCES oauth_clients(id) ON DELETE CASCADE,
    user_id       TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    resource      TEXT NOT NULL,
    token_version INTEGER NOT NULL, -- the user's, at issue time: resets revoke
    created_at    TEXT NOT NULL,
    expires_at    INTEGER NOT NULL
);

CREATE INDEX oauth_refresh_tokens_user ON oauth_refresh_tokens(user_id);
