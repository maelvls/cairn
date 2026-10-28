# cairn.js API reference

Include with `<script src="./cairn.js"></script>` (relative path — the server
injects the file into every version's URL space; a copy dropped next to
`index.html` takes precedence and enables fully-offline debug mode).

All functions return promises. The library picks its backend automatically:

- **remote** — page served by Cairn under `/artifacts/{id}/{vid}/`; calls hit
  the server APIs, authentication travels with the session cookie.
- **debug** — page served anywhere else (local dev server, `file://`);
  data lives in an in-browser SQLite (sql.js/WASM) persisted to IndexedDB.
  First load fetches sql.js from a CDN and caches it for offline reuse.

## Context

```js
cairn.mode        // 'remote' | 'debug'
cairn.artifactId  // from the URL; 'debug-artifact' in debug mode
cairn.versionId   // from the URL; 'debug-version' in debug mode
await cairn.ready()   // resolves when the backend is usable; call first
```

## Users

```js
const me = await cairn.me();
// remote: {id, email, name, isAdmin, ...} or null when anonymous
// debug:  {id: 0, name: 'Debug', email: 'debug@localhost', isAdmin: true}

const users = await cairn.users();   // [{id, name, email}] — whole directory,
                                     // any authenticated user may read it
cairn.login();                       // redirect to /login and back
```

Anonymous visitors (public artifacts) can read but not write. Gate write UI on
`me !== null` and call `cairn.login()` when an anonymous user tries to write.

## Metadata

```js
await cairn.artifact();   // {id, name, description, public, resources: [{type, value}]}
await cairn.versions();   // [{id, seq, name, changelog, createdAt}], newest first
```

## Shared database

```js
const res = await cairn.db.query(sql, params);
// res = {columns: [...], types: [...], rows: [[...], ...],
//        rowsAffected, lastInsertId, truncated?}
// rows are arrays in column order; BLOBs arrive base64-encoded.

await cairn.db.query('SELECT * FROM t WHERE id = ?', [42]);
await cairn.db.query(sql, params, {version: otherVersionId});  // read-only
                                                               // sibling-version access
await cairn.db.batch([{sql, params}, ...]);   // one atomic transaction
cairn.db.downloadURL;                          // raw .db download (remote only)
```

Constraints (server-enforced): exactly one statement per `query` call;
per-query timeout; row cap (`truncated: true` when hit); writes require
authentication.

## Migrations

```js
const applied = await cairn.db.migrate('002-add-votes', [
  {sql: 'ALTER TABLE items ADD COLUMN votes INTEGER NOT NULL DEFAULT 0'},
]);
// true  → this client applied it
// false → another client already had; safe to continue either way
```

The claim insert and the statements run in one transaction, so concurrent
viewers cannot double-apply. Use ascending prefixed names ('001-…', '002-…')
and never edit a shipped migration — add a new one.

## File storage

Each version also owns a file storage for binary data that does not belong in
SQLite (images, exports, attachments). Like the database it is per-version,
survives re-uploads, is readable anonymously on public artifacts, and writes
require authentication.

```js
await cairn.files.upload('photos/cat.png', blob);  // Blob/File/string
                                                   // → {path, size, modifiedAt}
await cairn.files.list();                          // [{path, size, modifiedAt}]
const blob = await cairn.files.download('photos/cat.png');  // null when absent
await cairn.files.remove('photos/cat.png');        // throws when absent

img.src = cairn.files.url('photos/cat.png');
// direct download URL — remote mode only, returns null in debug mode;
// use download() + URL.createObjectURL(blob) to work in both.
```

Paths are relative slash-separated names (`a/b/c.png` — no leading `/`, no
`..`); uploading to an existing path overwrites it. `list`, `download` and
`url` accept `{version: otherVersionId}` for read-only access to a sibling
version's files. In debug mode files persist to IndexedDB.

Cross-version data migration pattern (new version pulling from the old).
Read the old data first, then create schema and import in a single guarded
migration so exactly one client performs the copy, atomically:

```js
const old = await cairn.db.query('SELECT body FROM items', [], {version: OLD_VID});
await cairn.db.migrate('001-import-v1', [
  {sql: 'CREATE TABLE items (id INTEGER PRIMARY KEY, body TEXT NOT NULL)'},
  ...old.rows.map(([body]) => (
    {sql: 'INSERT INTO items (body) VALUES (?)', params: [body]})),
]);
```

## AI (Claude)

When the server runs with an Anthropic API key, pages can call Claude (the
Cairn counterpart of `window.claude` in Claude Artifacts). The request goes
through the server, which holds the key.

```js
const st = await cairn.ai.status();
// {enabled, model, models, maxTokens, dailyLimit, remaining}
// remaining: -1 = unlimited; null when signed out; {enabled: false} in debug
// mode or when the server has no key

const text = await cairn.ai.ask('Summarize these notes:\n' + notes, {
  system: 'Answer in one short paragraph.',  // optional
  maxTokens: 500,                            // optional, clamped by the server
  model: 'claude-haiku-5-5',                 // optional, one of st.models
});

const reply = await cairn.ai.chat([         // multi-turn: first role is 'user'
  {role: 'user', content: 'Suggest a name for a book club.'},
  {role: 'assistant', content: 'The Page Turners.'},
  {role: 'user', content: 'Something shorter?'},
], {system: 'Be playful.'});

const full = await cairn.ai.complete({prompt: 'Hi', max_tokens: 100});
// {text, model, usage: {input_tokens, output_tokens}, stop_reason}
```

`content` may also be an array of Messages API content blocks (`text`,
`image`, `document`). `ask`/`chat` resolve to the concatenated text; use
`complete` to see `stop_reason` (`max_tokens` = cut short, `refusal` =
declined).

Errors reject with an `Error` whose `.status` tells why: 401 signed out (call
`cairn.login()`), 429 daily quota reached, 503 AI not configured on the
server, 502 Anthropic error (message included), 400/413 invalid or too large
(200 KB) request. In debug mode every call rejects with "cairn.ai needs a
Cairn server".

```js
const st = await cairn.ai.status();
button.hidden = !(st && st.enabled);
button.onclick = async () => {
  out.textContent = '…';
  try {
    out.textContent = await cairn.ai.ask(input.value);
  } catch (e) {
    out.textContent = e.status === 401 ? 'Sign in to ask Claude.' : e.message;
  }
};
```

Cautions:
- Signed-in viewers only: anonymous visitors of a public artifact cannot use
  it. Gate the UI on `status()` and keep the page useful without AI.
- Each call spends one request of the viewer's daily quota (`remaining`);
  avoid calling on page load or in loops.
- The prompt goes to Anthropic, and the page (hence every viewer) sees it:
  never embed secrets or data the viewer should not see in prompts or
  system prompts. Treat the answer as untrusted text (render with
  `textContent`, not `innerHTML`).
