# Cairn

> **Fork [maelvls/cairn](https://github.com/maelvls/cairn)**, whose main
> branch is `mael-fork` (there is no `main` here; upstream's is
> [aloisdeniel/cairn](https://github.com/aloisdeniel/cairn); `google-auth`,
> the previous main branch, stays for old links). It adds Google
> sign-in, which replaces email/password when a Google OAuth client is
> configured (see "Google sign-in" under [Operations](#operations)), an
> [MCP endpoint with OAuth 2.1](#mcp) for AI clients, and a
> [gallery and viewer](#gallery-and-viewer) closer to Claude Artifacts:
> live previews, pins, comment threads that agents answer, a data and source
> panel, and a viewer that follows new versions as they are pushed. Releases
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
| `/` | gallery of artifacts (any signed-in user) |
| `/artifacts/{id}` | redirects to the latest version |
| `/artifacts/{id}/{versionId}/` | a version, full screen (canonical) |
| `/shared/{id}` | viewer, following the latest version |
| `/shared/{id}/{versionId}` | viewer, on one version |
| `/admin` | admin UI: users and API keys |
| `/login`, `/logout` | session pages |

Private artifacts redirect anonymous visitors to `/login?next=…`. Because a
version is always served under its own directory URL, relative paths inside
the SPA (`./app.js`, `fetch('data.json')`) just work; extension-less paths
fall back to `index.html` for client-side routing, missing assets 404.

### Gallery and viewer

The **gallery** (`/`) shows every artifact as a card with a preview of its
latest version, most recently updated first: search (`/`), filters (pinned,
mine, public, private, open comments), sort, grid or list. Each user pins
their own artifacts to the top. Drop an `.html` page or a `.zip` on the page
(or use "New artifact") to publish one without the CLI.

Previews are **captured thumbnails**: when a signed-in user opens a version
that has none, the viewer renders the page as a 1280×800 desktop viewport in
the browser (with a vendored copy of
[html2canvas](https://html2canvas.hertzen.com) 1.4.1, MIT, served at
`/_ui/html2canvas.min.js`) and uploads a 640×400 WebP or JPEG. Re-uploading
the version deletes its thumbnail, so the next visit captures it again. Cards
without one fall back to a live, scaled-down iframe of the page.

The **viewer** (`/shared/{id}`) frames the artifact with a toolbar (version
picker, copy link, full screen) and a side panel:

- **Details** (`i`): title, visibility, pin, links, version history (rename,
  replace files, delete), publish a new version by dropping a file.
- **Comments** (`c`): threads on the artifact for signed-in users, each
  remembering the version it was written on; replies, resolve, reopen.
  "Comment on an element" picks an element of the page (hover outlines it,
  `Esc` cancels): the thread quotes it, and clicking the quote scrolls to it
  and flashes it (found by CSS path, else by its text). Agents read and
  answer them (MCP `list_comments`/`add_comment`,
  `cairn comments`), and their comments are marked as such. New comments
  can push a notification to your phone over [ntfy](https://ntfy.sh): click
  your name in the gallery header → Notifications…
- **Data** (`d`): the version's tables with row counts, a table browser, a
  SQL console, the database download and the file storage.
- **Source** (`s`): the version's published files. "Compare with" picks
  another version (by default the one before): files are marked added (+),
  removed (−) or changed (~), with a summary and an "only changed files"
  filter, and a changed text file opens as a unified diff (3 lines of
  context, old and new line numbers; binaries compare by size).

Without a version in the URL the viewer follows the latest one: when an agent
pushes a new version, or overwrites the current one, the page reloads by
itself. Anonymous visitors of public artifacts get the viewer without
comments and without write actions.

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
GET|PATCH /api/me/settings                  {ntfyTopic} (empty: none), plus the server's ntfyUrl
POST   /api/me/settings/test-notification   [{ntfyTopic}] sends a test to it, else to the saved topic
GET    /api/users                           directory: id, name, email (any authed user)
GET    /api/artifacts                       ?name= ?limit= ?offset=  most recently updated first, with
                                            latestVersion (+ thumbnail URL when captured), versionCount,
                                            openComments, pinned
POST   /api/artifacts                       {name, description, public}
GET|PATCH|DELETE /api/artifacts/{id}
PUT|DELETE /api/artifacts/{id}/pin          pin for the calling user
POST   /api/artifacts/{id}/resources        {type, value}
DELETE /api/artifacts/{id}/resources/{rid}
GET    /api/artifacts/{id}/comments         ?status=open|resolved (default: all), oldest first
POST   /api/artifacts/{id}/comments         {body, parentId, versionId, anchor}
                                            anchor (thread roots only): {selector, quote ≤ 300 chars, path}
PATCH  /api/artifacts/{id}/comments/{cid}   {body} (author) and/or {resolved}
DELETE /api/artifacts/{id}/comments/{cid}   author or admin; a root takes its replies
GET    /api/artifacts/{id}/versions
POST   /api/artifacts/{id}/versions         multipart: archive (zip, or a single .html page), name, changelog
GET|PATCH|DELETE /api/artifacts/{id}/versions/{vid}
PUT    /api/artifacts/{id}/versions/{vid}   re-upload (data survives)
GET    /api/artifacts/{id}/versions/{vid}/source     published files: [{path, size, modifiedAt}]
GET    /api/artifacts/{id}/versions/{vid}/thumbnail  captured preview image (404 when none or stale); signed-in
                                                     users only, even on public artifacts: it can show the viewer
PUT    /api/artifacts/{id}/versions/{vid}/thumbnail  raw body: image/webp, image/jpeg or image/png, ≤ 1 MB;
                                            ?updatedAt= of the captured content (409 if replaced since)
GET    /api/artifacts/{id}/versions/{vid}/db/tables  tables and views, columns, row counts
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
cairn push ./my-page            # same directory, same artifact (new one the first time)
cairn comments list my-app      # open threads left in the viewer
cairn comments add my-app "Fixed in v3." --reply <comment-id> --resolve
cairn comments add my-app "Too small" --selector "main > h1" --quote "Results"   # about one element
cairn db query --artifact my-app --json "SELECT COUNT(*) FROM notes"
cairn files put ./report.pdf --artifact my-app --path exports/report.pdf
cairn files list --artifact my-app --json
cairn files get exports/report.pdf --artifact my-app --out ./report.pdf
```

All commands accept `--json`; errors exit non-zero with a message on stderr.
`cairn push` without `--artifact` publishes to the artifact the directory was
last pushed to from this machine (remembered in the CLI config), or creates a
private one named after the directory, like re-publishing the same file in
Claude keeps the same artifact. Artifacts without a description take the
`<title>` of their `index.html`.
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
| `list_comments`, `add_comment`, `resolve_comment` | feedback threads from the viewer: read them (an `anchor` quotes the element a thread is about, with its CSS selector), reply (and resolve) after acting on them |

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
  storage), `thumbs/` (gallery thumbnails; derived, safe to delete).
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
- **Comment notifications** — each user sets their own [ntfy](https://ntfy.sh)
  topic (gallery → their name → Notifications…, with a test button) and
  subscribes to it in the ntfy app. A new comment or reply notifies the
  artifact's creator, the users who pinned it and the people who already
  wrote in the thread — never its author, nor disabled users. Messages go to
  `--ntfy-url` (`CAIRN_NTFY_URL`, default `https://ntfy.sh`; point it at a
  self-hosted ntfy to keep comments off ntfy.sh, with `CAIRN_NTFY_TOKEN` if
  it requires an access token) through ntfy's JSON publish
  API, in the background (10 s timeout, failures logged, never failing the
  comment). With `--base-url` set, tapping a notification opens the
  artifact's comments. ntfy topics are readable by whoever knows their name:
  pick hard-to-guess ones.

## Cairn vs. Claude Artifacts

| | Claude Artifacts | Cairn |
|---|---|---|
| Hosting | claude.ai, Anthropic accounts | self-hosted, your domain, your disk |
| Content | single-page, strict CSP | full multi-file SPA dirs, no CSP wall |
| Shared data | capability-gated runtime | first-class SQLite (raw SQL, transactions, cross-version migration, downloadable file) + per-version file storage |
| Versioning | product history | explicit versions + changelogs, stable URLs, re-upload; the viewer follows the latest live |
| Feedback | comments on artifacts | comment threads per artifact, read and answered by agents (MCP, CLI) |
| Automation | Claude's Artifact tool | any agent via CLI/API keys — model-agnostic |
| AI runtime | `window.claude` in page | none built in |
| Security | sandboxed for untrusted viewers | trust-based, invited users only |

## Development

```sh
go test ./...        # unit + integration tests
./scripts/e2e.sh     # full end-to-end smoke test against a real server
```
