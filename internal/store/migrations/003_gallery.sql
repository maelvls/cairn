-- Gallery and viewer: who published what, per-user pins, and comment threads
-- on artifacts (see internal/server/handlers_comments.go).

-- User ids; empty for rows written before this migration.
ALTER TABLE artifacts ADD COLUMN created_by TEXT NOT NULL DEFAULT '';
ALTER TABLE versions ADD COLUMN created_by TEXT NOT NULL DEFAULT '';

CREATE TABLE pins (
    user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    artifact_id TEXT NOT NULL REFERENCES artifacts(id) ON DELETE CASCADE,
    created_at  TEXT NOT NULL,
    PRIMARY KEY (user_id, artifact_id)
);

CREATE INDEX pins_artifact ON pins(artifact_id);

-- A thread is a root comment (parent_id NULL) and its replies. Resolving
-- applies to the root. version_id records which version the author was
-- looking at; deleting the version keeps the comment.
CREATE TABLE comments (
    id          TEXT PRIMARY KEY,
    artifact_id TEXT NOT NULL REFERENCES artifacts(id) ON DELETE CASCADE,
    version_id  TEXT REFERENCES versions(id) ON DELETE SET NULL,
    parent_id   TEXT REFERENCES comments(id) ON DELETE CASCADE,
    user_id     TEXT REFERENCES users(id) ON DELETE SET NULL,
    via         TEXT NOT NULL DEFAULT 'web', -- web, api or mcp
    body        TEXT NOT NULL,
    resolved_at TEXT,
    resolved_by TEXT,
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);

CREATE INDEX comments_artifact ON comments(artifact_id, created_at);
CREATE INDEX comments_parent ON comments(parent_id);
