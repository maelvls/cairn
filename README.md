# Cairn

> **Fork [maelvls/cairn](https://github.com/maelvls/cairn)**, whose main
> branch is `mael-fork` (there is no `main` here; upstream's is
> [aloisdeniel/cairn](https://github.com/aloisdeniel/cairn); `google-auth`,
> the previous main branch, stays for old links). It adds Google
> sign-in, which replaces email/password when a Google OAuth client is
> configured (see "Google sign-in" under [Operations](#operations)), and an
> [MCP endpoint with OAuth 2.1](#mcp) for AI clients. Releases
> are tagged `v<upstream>-google.<n>` and ship the same binaries and
> multi-arch image as upstream:
>
> ```sh
> curl -fsSL https://raw.githubusercontent.com/maelvls/cairn/mael-fork/docs/install.sh | CAIRN_INSTALL_DIR="$HOME/.local/bin" sh
> docker pull ghcr.io/maelvls/cairn:latest
> ```

Cairn is a self-hosted server for uploading, managing and hosting collections
of **artifacts** — single-page web apps with optional shared SQLite data — an
open alternative to Claude Artifacts for small trusted teams and AI-agent
workflows.

One Go binary contains the server, an admin UI, and a client CLI designed to
be driven by AI agents.

```
┌───────────────┐   cairn push    ┌──────────────────────────────┐
│  agent / dev  │ ──────────────▶ │  cairn serve                 │
└───────────────┘                 │  ├─ /artifacts/{id}/{vid}/   │  full screen
        ▲                         │  ├─ /shared/{id}             │  framed + metadata
        │ cairn db query          │  ├─ /admin                   │  admin UI
        └──────────────────────── │  └─ /api/...                 │  JSON APIs
                                  └──────────────────────────────┘
                                     data/ (SQLite + files)
```

## Concepts

- **Artifact** — uuid, name, description, public/private flag, associated
  *resources* (e.g. a Claude session id), and a sequence of versions.
- **Version** — uuid, name, changelog, and a directory with at least an
  `index.html`. Each version also owns a lazily-created SQLite database shared
  by everyone who uses that version, accessed through a raw-SQL HTTP API, and
  a **file storage** where clients upload and download arbitrary files.
  Versions can be re-uploaded in place (work-in-progress iteration) — the
  shared database and file storage survive re-uploads.
- **Users** — created by admins; no email verification, no self-signup. A new
  account has no password: **the user chooses one at first sign-in**.
- **API keys** — created by admins, tied to a user, revocable. For CLIs and
  agents.

### Trust model (read this)

Cairn trusts its users, deliberately: every authenticated user can manage
artifacts and run **raw SQL** against artifact databases, and artifacts are
served same-origin with the APIs. This keeps everything simple and is fine for
a small circle of invited people. Do **not** point untrusted crowds at a Cairn
instance. Guardrails exist (per-query timeout, row caps, read-only access for
anonymous visitors of public artifacts) but they are guardrails, not a sandbox.

## Quick start

```sh
go build ./cmd/cairn

# First run creates the admin account — no password on the command line:
# you choose it in the browser at first sign-in
./cairn serve --data-dir data --admin-email you@example.com

open http://localhost:8787/admin
```

Create users and API keys in the admin UI (or via `/api/admin/...`). Accounts
— the bootstrap admin included — sign in at `/login` and choose their password
on the spot. (Automation can still pre-set the admin password with
`--admin-password` / `CAIRN_ADMIN_PASSWORD`.)

### Push your first artifact

```sh
./cairn login --host http://localhost:8787 --email you@example.com
./cairn push examples/guestbook --artifact guestbook --create --public \
    --name v1 --changelog "first version"
./cairn open guestbook            # prints the URL
```

### Docker

```sh
docker run -p 8787:8787 -v cairn-data:/data \
  -e CAIRN_ADMIN_EMAIL=you@example.com \
  ghcr.io/aloisdeniel/cairn:latest
```

Or `docker compose up -d` with the repo's [`docker-compose.yml`](docker-compose.yml)
(hosted Docker managers can point straight at the GitHub repo). To build the
image yourself instead: `docker build -t cairn .`

## URLs

| URL | Meaning |
|---|---|
| `/artifacts/{id}` | redirects to the latest version |
| `/artifacts/{id}/{versionId}/` | a version, full screen (canonical) |
| `/shared/{id}` / `/shared/{id}/{versionId}` | version embedded in a shell frame with metadata and a version picker |
| `/admin` | admin UI |
| `/login`, `/logout` | session pages |

Private artifacts redirect anonymous visitors to `/login?next=…`. Because a
version is always served under its own directory URL, relative paths inside
the SPA (`./app.js`, `fetch('data.json')`) just work; extension-less paths
fall back to `index.html` for client-side routing, missing assets 404.

## Writing an artifact

A version is a directory with an `index.html`. Include the client library with
a relative script tag — the server injects it into every version's URL space:

```html
<script src="./cairn.js"></script>
<script>
  await cairn.ready();
  const me = await cairn.me();               // null when anonymous
  await cairn.db.migrate('001-schema', [     // run-once, concurrency-safe
    {sql: 'CREATE TABLE notes (id INTEGER PRIMARY KEY, body TEXT)'}
  ]);
  await cairn.db.query('INSERT INTO notes (body) VALUES (?)', ['hi']);
  const res = await cairn.db.query('SELECT * FROM notes');  // {columns, rows}
  const dir = await cairn.users();           // global user directory
</script>
```

`cairn.db.query(sql, params, {version: otherVersionId})` reads another
version's database (read-only) — use it to migrate data forward after
publishing a new version. `cairn.db.batch([...])` runs statements in one
transaction.

Each version also has a **file storage** for binary data that does not belong
in SQLite (images, exports, attachments):

```js
await cairn.files.upload('photos/cat.png', blob);   // Blob/File/string; auth required
await cairn.files.list();                           // [{path, size, modifiedAt}]
const blob = await cairn.files.download('photos/cat.png');  // null when absent
img.src = cairn.files.url('photos/cat.png');        // direct URL (remote mode)
await cairn.files.remove('photos/cat.png');
```

Like the database, file storage is per-version, survives re-uploads, and is
readable anonymously on public artifacts while writes require authentication.
`list`/`download`/`url` accept `{version: otherVersionId}` for read-only
access to a sibling version's files.

**Local debug mode.** Open the same directory without a Cairn server (any
static file server, or `file://`) and `cairn.js` switches to an in-browser
SQLite (sql.js/WebAssembly) persisted in browser storage; `cairn.me()` returns
`{id: 0, name: "Debug"}`. First load fetches sql.js from a CDN and caches it
for offline use; fully offline setups can drop `sql-wasm.js`/`sql-wasm.wasm`
next to `index.html`. See `examples/guestbook`.

More examples in [`examples/`](examples/): [`poll`](examples/poll/) — a
multi-file artifact (CSS, JS, SVG assets, a fetched `config.json`) —
[`drive`](examples/drive/) — a shared file drive built entirely on the
per-version file storage (uploads, folders, thumbnails, downloads) — and
[`todo-react`](examples/todo-react/) — TypeScript + React bundled with esbuild,
where a typed `TodoStore` hides every cairn.js detail from the UI.

## HTTP API

Authentication: `Authorization: Bearer <jwt>` (from `POST /api/auth/login`) or
`Bearer <api-key>`, or the session cookie set at login. Reads of public
artifacts need no auth; **writes always need auth**.

The `{id}` segment of artifact routes (APIs and page URLs alike) accepts
either the artifact id or a **resource reference** — a resource value such as
a Claude session id, or a resource row id. If the reference matches more than
one artifact the request fails with `409 Conflict`; use the artifact id then.

```
POST   /api/auth/login                      {email, password[, confirm]}  (403 with Google sign-in)
GET    /api/auth/config                     {password, google}: sign-in methods
GET    /api/me
GET    /api/users                           directory: id, name, email (any authed user)
GET    /api/artifacts                       ?name= ?limit= ?offset=
POST   /api/artifacts                       {name, description, public}
GET|PATCH|DELETE /api/artifacts/{id}
POST   /api/artifacts/{id}/resources        {type, value}
DELETE /api/artifacts/{id}/resources/{rid}
GET    /api/artifacts/{id}/versions
POST   /api/artifacts/{id}/versions         multipart zip: archive, name, changelog
GET|PATCH|DELETE /api/artifacts/{id}/versions/{vid}
PUT    /api/artifacts/{id}/versions/{vid}   re-upload (data survives)
POST   /api/artifacts/{id}/versions/{vid}/db/query   {sql, params}
POST   /api/artifacts/{id}/versions/{vid}/db/batch   {statements: [{sql, params}]}
GET    /api/artifacts/{id}/versions/{vid}/db/download
GET    /api/artifacts/{id}/versions/{vid}/files      list: [{path, size, modifiedAt}]
GET    /api/artifacts/{id}/versions/{vid}/files/{path}   download (ranges supported)
PUT    /api/artifacts/{id}/versions/{vid}/files/{path}   raw body upload (overwrites)
DELETE /api/artifacts/{id}/versions/{vid}/files/{path}
GET    /api/admin/users|keys ...            admin management
```

Notes on the SQL proxy: one statement per `query` call (use `batch` for
scripts/transactions); write access is enforced at the connection level
(anonymous requests run on a `query_only` pool), never by parsing SQL; BLOBs
are returned base64-encoded; results are capped (`--max-query-rows`).

## CLI for agents

Set two environment variables and every command works headlessly:

```sh
export CAIRN_HOST=https://cairn.example.com
export CAIRN_API_KEY=cairn_xxxxxxxx_yyyyyyyy    # from the admin UI

cairn whoami --json
cairn artifact list --json
cairn push ./dist --artifact my-app --create --json   # prints the URLs
cairn push ./dist --artifact my-app --overwrite latest
cairn db query --artifact my-app --json "SELECT COUNT(*) FROM notes"
cairn files put ./report.pdf --artifact my-app --path exports/report.pdf
cairn files list --artifact my-app --json
cairn files get exports/report.pdf --artifact my-app --out ./report.pdf
```

All commands accept `--json`; errors exit non-zero with a message on stderr.
Associate an agent session with `cairn artifact create --resource
claude-session=<id>` or `POST /api/artifacts/{id}/resources` — afterwards the
session id works anywhere an artifact id or name does:

```sh
cairn push ./dist --artifact <claude-session-id> --overwrite latest
cairn db query --artifact <claude-session-id> "SELECT ..."
```

A ready-made **Claude Code skill** ships in
[`.claude/skills/cairn-artifact/`](.claude/skills/cairn-artifact/SKILL.md): it
teaches Claude the whole build → test locally → publish → iterate workflow and
the `cairn.js` API. It is picked up automatically when working inside this
repo; copy the directory into any other project's `.claude/skills/` (or
`~/.claude/skills/` for global use) to let Claude publish artifacts from
there.

## MCP

`POST /mcp` is an [MCP](https://modelcontextprotocol.io) server (Streamable
HTTP, stateless: plain JSON answers, no SSE stream, so `GET /mcp` is 405).
Add `<mcp-url>` as a connector in claude.ai, grok.com, ChatGPT… and sign in:
nothing to copy.

Clients authenticate with **OAuth 2.1**, Cairn being its own authorization
server (`internal/server/oauth.go`, needs `--base-url`, which is the issuer):

- `POST /mcp` without a token answers 401 with
  `WWW-Authenticate: Bearer resource_metadata="…"`, pointing at the protected
  resource metadata (RFC 9728), which names Cairn as the authorization server
  (`/.well-known/oauth-authorization-server`, RFC 8414).
- Clients register themselves (`POST /oauth/register`, RFC 7591), public
  (`none`) or with a secret. Redirect URIs must be https, http on loopback, or
  a private-use scheme.
- `/oauth/authorize` sends the user through the usual sign-in (Google when
  configured), then a consent page names the client and where it redirects.
  Only existing Cairn accounts can approve, as everywhere else. PKCE S256 is
  mandatory.
- `/oauth/token`: 5-minute single-use codes, 1-hour access tokens, 30-day
  refresh tokens rotated at each use and revoked with the account's sessions
  (disable, password reset). Only hashes are stored.
- Access tokens are bound to the MCP endpoint (`aud`): they are refused
  everywhere else, so a connector can't call `/api` directly.

`--mcp-url` (`CAIRN_MCP_URL`) is the endpoint's external URL when a reverse
proxy serves it from another host or path (default `<base-url>/mcp`). The
proxy forwards that path to `/mcp` and
`/.well-known/oauth-protected-resource<path>` as is; authorization stays on
`<base-url>`, where the sign-in cookie lives. For example, with
`--mcp-url https://mcp.example.com/cairn/mcp`, in Caddy:

```
https://mcp.example.com {
	handle /cairn/mcp {
		rewrite * /mcp
		reverse_proxy cairn:8787
	}
	handle /.well-known/oauth-protected-resource/cairn/mcp {
		reverse_proxy cairn:8787
	}
}
```

API keys and session tokens work on `/mcp` too, for scripts.

| Tool | |
|------|-|
| `list_artifacts`, `get_artifact` | artifacts, versions, page URLs |
| `publish` | create an artifact from inline files (text or base64), add a version, or overwrite one (`replace_version`) |
| `update_artifact`, `delete_artifact` | rename, describe, public/private; delete |
| `read_source` | list or read a version's published files |
| `db_query`, `db_batch` | SQL on a version's shared database |
| `list_files`, `read_file`, `write_file`, `delete_file` | a version's file storage |

`version` defaults to the latest one everywhere. Each tool replays the
matching `/api` route in-process (`internal/server/mcp.go`), so limits and
validation are the API's.

```sh
curl -s https://cairn.example.com/mcp -H "Authorization: Bearer $CAIRN_API_KEY" \
  -H 'Content-Type: application/json' -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}'
```

## Operations

- **Data layout** — everything lives under `--data-dir`: `cairn.db`
  (metadata), `secret.key` (JWT signing), `content/` (extracted version
  uploads), `dbs/` (per-version shared databases), `files/` (per-version file
  storage).
- **Backup** — `cairn backup --data-dir data --out backup/` snapshots live
  SQLite databases with `VACUUM INTO` and copies the rest. Safe while the
  server runs.
- **Config** — flags > `CAIRN_*` env (`CAIRN_ADDR`, `CAIRN_DATA_DIR`,
  `CAIRN_BASE_URL`, `CAIRN_TOKEN_TTL`, `CAIRN_MAX_UPLOAD_MB`, …). Set
  `--base-url https://…` behind TLS so cookies are marked Secure.
- **Password reset** — an admin resets the account; the user picks a new
  password at next sign-in. Tokens are invalidated instantly on reset/disable.
- **Google sign-in** — set `--google-client-id` / `--google-client-secret`
  (`CAIRN_GOOGLE_CLIENT_ID`, `CAIRN_GOOGLE_CLIENT_SECRET`) and `--base-url`,
  and register `<base-url>/auth/google/callback` as an authorized redirect
  URI of the OAuth client. Google then **replaces** email/password: the login
  page only offers Google, `POST /api/auth/login` answers 403, and "reset
  password" disappears from the admin UI. The trust model does not change:
  only accounts an admin created can sign in, matched on the verified Google
  email (the bootstrap admin's email must be a Google account). API keys keep
  working. `cairn login --host <url>` opens the browser: after Google
  sign-in, `/auth/cli` asks to confirm, then hands a token to the CLI waiting
  on `127.0.0.1`.

## Cairn vs. Claude Artifacts

| | Claude Artifacts | Cairn |
|---|---|---|
| Hosting | claude.ai, Anthropic accounts | self-hosted, your domain, your disk |
| Content | single-page, strict CSP | full multi-file SPA dirs, no CSP wall |
| Shared data | capability-gated runtime | first-class SQLite (raw SQL, transactions, cross-version migration, downloadable file) + per-version file storage |
| Versioning | product history | explicit versions + changelogs, stable URLs, re-upload |
| Automation | Claude's Artifact tool | any agent via CLI/API keys — model-agnostic |
| AI runtime | `window.claude` in page | none built in |
| Security | sandboxed for untrusted viewers | trust-based, invited users only |

## Development

```sh
go test ./...        # unit + integration tests
./scripts/e2e.sh     # full end-to-end smoke test against a real server
```
