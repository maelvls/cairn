# Cairn — developer notes

Self-hosted artifact server ("open Claude Artifacts"): one Go binary =
server + admin UI + agent-oriented CLI. See README.md for user-facing docs.

## Commands

```sh
go build ./cmd/cairn     # build the binary
go test ./...            # unit + integration tests (httptest, temp dirs)
./scripts/e2e.sh         # end-to-end smoke test against a real server
gofmt -l . && go vet ./...
node internal/server/web/ui_diff_test.mjs   # ui.diffLines/ui.hunks (viewer source diff)
```

## Map

- `cmd/cairn/` — subcommand dispatch. `serve.go` (server), `backup.go`,
  `client_cmds.go` (login/artifact/push/db/open), `config.go` (CLI state at
  `~/.config/cairn/config.json`, override with `CAIRN_CONFIG`; headless via
  `CAIRN_HOST` + `CAIRN_API_KEY`).
- `internal/store/` — metadata SQLite (`modernc.org/sqlite`, pure Go).
  Embedded migrations in `migrations/*.sql` run at open. `layout.go` defines
  the data dir: `content/{artifactID}/{contentDir}/` for extracted uploads,
  `dbs/{artifactID}/{versionID}.db` for shared databases,
  `files/{artifactID}/{versionID}/` for per-version file storage —
  deliberately separate so re-uploads never touch data.
- `internal/auth/` — bcrypt passwords, hand-rolled HS256 JWT (claims include
  `tkv` = token_version for instant revocation), API keys
  `cairn_<keyid>_<secret>` stored as SHA-256.
- `internal/versiondb/` — per-version DB manager: refcounted handle cache,
  **read/write enforced by connection pools** (anonymous → `query_only` pool),
  single-statement scanner (the driver executes multi-statement input
  otherwise!), batch = one transaction.
- `internal/server/` — HTTP. `routes.go` is the route table. Serving model:
  `/artifacts/{id}` 302→ `/artifacts/{id}/{vid}/` (canonical, trailing slash)
  so relative paths resolve; SPA fallback only for extension-less
  `Accept: text/html` requests. `web/` holds embedded assets: `login.html`,
  `gallery.html` (`/`, every signed-in user's home: cards with live iframe
  previews, pins, drop-to-publish), `shell.html` (the viewer at `/shared/…`:
  toolbar + side panel with details/versions, comments, data, source; polls
  versions to follow new pushes), `admin.html` (users and API keys),
  `ui.css`/`ui.js` (shared chrome and helpers, served at `/_ui/`; vanilla JS,
  no build step, templates pass state as `{{.Boot}}`; `ui.diffLines` and
  `ui.hunks` are the Source tab's version diff, tested with node), `cairn.js` (client
  lib; falls back to sql.js-in-browser with user `{id: 0, name: "Debug"}`
  when not served by Cairn).
- `internal/server/handlers_comments.go` — comment threads (flat: replies
  point at the root, resolution lives on the root; `via` records web, api or
  mcp). `handlers_source.go` — read-only views for the viewer: published
  files, and `db/tables` (query_only pool, never creates a database).
- `internal/server/mcp.go` — MCP endpoint (`POST /mcp`, stateless
  Streamable HTTP, hand-rolled JSON-RPC). Tools replay `/api` routes
  in-process through `s.mux` with the caller's user in the context
  (`mcpAPI`); add a tool by calling the API, not the store, so the API's
  rules apply. `read_source` is the exception (no API serves raw sources).
- `internal/server/oauth.go` — OAuth 2.1 authorization server for MCP
  clients (RFC 9728/8414/7591, PKCE S256, rotating refresh tokens; tables in
  `migrations/002_oauth.sql`, hashes only). Access tokens are JWTs with `aud`
  = the MCP URL; `userFromJWT` refuses any `aud` token outside `/mcp`. Keep
  it that way: an MCP connector must not become a full API session.
- `internal/client/` — Go API client used by the CLI (zips + multipart push).
- `examples/` — reference artifacts: `guestbook` (single file, exercised by
  e2e), `poll` (multi-file + assets + config fetch), `drive` (file storage as
  a shared drive; no database), `todo-react` (TypeScript + React + esbuild;
  push its `dist/`, `src/store.ts` wraps cairn.js).
- `.claude/skills/cairn-artifact/` — Claude Code skill for building and
  publishing artifacts with the CLI; keep it in sync when CLI flags or the
  cairn.js API change.

## Invariants worth keeping

- Versions are re-uploadable; replacement is a content-dir pointer swap
  (`SwapVersionContent`) + RemoveAll of the old dir. Never write into a live
  content dir.
- Never classify SQL as read/write by parsing — connection-level only.
- Uploaded zips: `fs.ValidPath` names only, no symlinks, decompressed-size
  budget, `index.html` required at root (single wrapping top dir is stripped).
  A lone `.html` upload (no zip signature) becomes `index.html`.
- A version's `updated_at` moves only when its files change (re-upload):
  viewers reload the page on it. Metadata edits leave it alone. Timestamps
  are fixed-width RFC 3339 with milliseconds (`store.now`), so they sort as
  strings.
- Google sign-in (`internal/server/google.go`) replaces passwords when a
  client id is configured; it only ever signs in to existing accounts, by
  verified email. The ID token comes from Google's token endpoint directly, so
  its claims are checked but not its signature — keep it that way only as
  long as it is never read from the browser.
- First-login-sets-password is the intended trust model (no invite tokens);
  `confirm` field guards typos. Admin "reset password" returns the account to
  the unclaimed state.
- Users are trusted with everything authenticated; anonymous gets read-only
  access to public artifacts. Keep new endpoints on that line.
- The `{id}` segment of artifact routes is a *reference*: artifact id first,
  else resource value/row id (409 on ambiguity). Handlers must use
  `requestArtifact(r)` (set by `withArtifact`/`publicAware`) — never
  `r.PathValue("id")` — as the artifact id for storage paths and queries.
