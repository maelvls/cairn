// Shared helpers of Cairn's own pages (gallery, viewer), served at
// /_ui/ui.js. Vanilla JS, no build step: everything hangs off window.ui.
(() => {
  'use strict';

  // ------------------------------------------------------------- DOM
  // h('a.btn', {href}, 'text', child) builds an element; attributes starting
  // with "on" become listeners, booleans toggle, null/false skip.
  function h(spec, attrs, ...children) {
    const [tag, ...classes] = spec.split('.');
    const el = document.createElement(tag || 'div');
    if (classes.length) el.className = classes.join(' ');
    if (attrs && (typeof attrs !== 'object' || attrs instanceof Node || Array.isArray(attrs))) {
      children.unshift(attrs);
      attrs = null;
    }
    for (const [k, v] of Object.entries(attrs || {})) {
      if (v == null || v === false) continue;
      if (k.startsWith('on') && typeof v === 'function') el.addEventListener(k.slice(2), v);
      else if (k === 'class') el.className += (el.className ? ' ' : '') + v;
      else if (k === 'style' && typeof v === 'object') Object.assign(el.style, v);
      else if (k in el && typeof v !== 'string') el[k] = v;
      else el.setAttribute(k, v === true ? '' : v);
    }
    append(el, children);
    return el;
  }
  function append(el, children) {
    for (const c of children.flat(Infinity)) {
      if (c == null || c === false) continue;
      el.append(c instanceof Node ? c : document.createTextNode(String(c)));
    }
    return el;
  }
  const $ = (s, el) => (el || document).querySelector(s);

  // ----------------------------------------------------------- icons
  // 24×24 stroke icons, drawn for Cairn.
  const ICONS = {
    search: '<circle cx="11" cy="11" r="6.5"/><path d="m20 20-4.2-4.2"/>',
    pin: '<path d="M9 3.5h6l-.8 5.2 3.3 3.3v1.5H6.5V12l3.3-3.3z"/><path d="M12 13.5V20"/>',
    more: '<circle cx="5.5" cy="12" r="1.2"/><circle cx="12" cy="12" r="1.2"/><circle cx="18.5" cy="12" r="1.2"/>',
    link: '<path d="M10 14a4 4 0 0 0 5.7 0l3-3a4 4 0 0 0-5.7-5.7l-1 1"/><path d="M14 10a4 4 0 0 0-5.7 0l-3 3a4 4 0 0 0 5.7 5.7l1-1"/>',
    external: '<path d="M14 4h6v6"/><path d="M20 4 11 13"/><path d="M18 14v5a1 1 0 0 1-1 1H5a1 1 0 0 1-1-1V7a1 1 0 0 1 1-1h5"/>',
    refresh: '<path d="M19.5 12a7.5 7.5 0 1 1-2.2-5.3"/><path d="M19.5 4.5v4h-4"/>',
    info: '<circle cx="12" cy="12" r="8.5"/><path d="M12 11v5.5"/><path d="M12 7.6v.2"/>',
    comment: '<path d="M4.5 6.5a2 2 0 0 1 2-2h11a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H10l-4 3.5v-3.5h0a1.5 1.5 0 0 1-1.5-1.5z"/>',
    target: '<circle cx="12" cy="12" r="6.5"/><circle cx="12" cy="12" r="1.8"/><path d="M12 2.5v3"/><path d="M12 18.5v3"/><path d="M2.5 12h3"/><path d="M18.5 12h3"/>',
    database: '<ellipse cx="12" cy="6" rx="7" ry="2.5"/><path d="M5 6v12c0 1.4 3.1 2.5 7 2.5s7-1.1 7-2.5V6"/><path d="M5 12c0 1.4 3.1 2.5 7 2.5s7-1.1 7-2.5"/>',
    code: '<path d="m8.5 7.5-4.5 4.5 4.5 4.5"/><path d="m15.5 7.5 4.5 4.5-4.5 4.5"/><path d="m13.5 5-3 14"/>',
    upload: '<path d="M12 15.5V4.5"/><path d="m7.5 9 4.5-4.5L16.5 9"/><path d="M4.5 15v3.5a1 1 0 0 0 1 1h13a1 1 0 0 0 1-1V15"/>',
    download: '<path d="M12 4.5v11"/><path d="m7.5 11 4.5 4.5 4.5-4.5"/><path d="M4.5 15v3.5a1 1 0 0 0 1 1h13a1 1 0 0 0 1-1V15"/>',
    trash: '<path d="M4.5 7h15"/><path d="M9.5 7V4.5h5V7"/><path d="M6.5 7l1 12.5h9l1-12.5"/>',
    edit: '<path d="M4.5 19.5h4l10-10a2.8 2.8 0 0 0-4-4l-10 10z"/><path d="m13 7 4 4"/>',
    check: '<path d="m5 12.5 4.5 4.5L19 7.5"/>',
    x: '<path d="m6 6 12 12"/><path d="M18 6 6 18"/>',
    lock: '<rect x="5" y="10.5" width="14" height="9.5" rx="1.5"/><path d="M8.5 10.5V8a3.5 3.5 0 0 1 7 0v2.5"/>',
    globe: '<circle cx="12" cy="12" r="8.5"/><path d="M3.5 12h17"/><path d="M12 3.5c2.3 2.4 3.5 5.2 3.5 8.5s-1.2 6.1-3.5 8.5c-2.3-2.4-3.5-5.2-3.5-8.5s1.2-6.1 3.5-8.5z"/>',
    history: '<path d="M4.5 12a7.5 7.5 0 1 0 2.2-5.3L4.5 9"/><path d="M4.5 4.5V9H9"/><path d="M12 8v4.5l3 2"/>',
    back: '<path d="M19 12H5"/><path d="m11 6-6 6 6 6"/>',
    plus: '<path d="M12 5v14"/><path d="M5 12h14"/>',
    panel: '<rect x="3.5" y="4.5" width="17" height="15" rx="2"/><path d="M14.5 4.5v15"/>',
    grid: '<rect x="4" y="4" width="6.5" height="6.5" rx="1"/><rect x="13.5" y="4" width="6.5" height="6.5" rx="1"/><rect x="4" y="13.5" width="6.5" height="6.5" rx="1"/><rect x="13.5" y="13.5" width="6.5" height="6.5" rx="1"/>',
    list: '<path d="M9 6.5h11"/><path d="M9 12h11"/><path d="M9 17.5h11"/><path d="M4.5 6.5h.5"/><path d="M4.5 12h.5"/><path d="M4.5 17.5h.5"/>',
    reply: '<path d="M10 8.5 5 13l5 4.5"/><path d="M5 13h9a5 5 0 0 0 5-5V6.5"/>',
    file: '<path d="M7 3.5h6.5L18 8v12.5H7z"/><path d="M13.5 3.5V8H18"/>',
    bell: '<path d="M6.5 16.5V11a5.5 5.5 0 0 1 11 0v5.5l1.5 1.5H5z"/><path d="M10 20.5a2.2 2.2 0 0 0 4 0"/>',
    logout: '<path d="M14 4.5H6.5a1 1 0 0 0-1 1v13a1 1 0 0 0 1 1H14"/><path d="M10.5 12h9"/><path d="m16.5 8.5 3.5 3.5-3.5 3.5"/>',
  };
  function icon(name, attrs) {
    const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    svg.setAttribute('viewBox', '0 0 24 24');
    svg.setAttribute('fill', 'none');
    svg.setAttribute('stroke', 'currentColor');
    svg.setAttribute('stroke-width', '1.6');
    svg.setAttribute('stroke-linecap', 'round');
    svg.setAttribute('stroke-linejoin', 'round');
    svg.setAttribute('aria-hidden', 'true');
    svg.innerHTML = ICONS[name] || '';
    for (const [k, v] of Object.entries(attrs || {})) svg.setAttribute(k, v);
    return svg;
  }

  // ------------------------------------------------------------- API
  class HTTPError extends Error {
    constructor(status, message) { super(message); this.status = status; }
  }
  async function api(path, options = {}) {
    const opts = {...options, headers: {...(options.headers || {})}};
    if (opts.json !== undefined) {
      opts.body = JSON.stringify(opts.json);
      opts.headers['Content-Type'] = 'application/json';
      delete opts.json;
    }
    const resp = await fetch(path, opts);
    if (resp.status === 401 && !options.anonymousOK) {
      location.href = '/login?next=' + encodeURIComponent(location.pathname + location.search + location.hash);
      throw new HTTPError(401, 'authentication required');
    }
    const data = await resp.json().catch(() => ({}));
    if (!resp.ok) throw new HTTPError(resp.status, data.error || ('HTTP ' + resp.status));
    return data;
  }
  const json = method => (path, body) => api(path, {method, json: body ?? {}});

  // ---------------------------------------------------------- toasts
  let toastBox;
  function toast(message, opts = {}) {
    if (!toastBox) toastBox = document.body.appendChild(h('div.toasts', {role: 'status', 'aria-live': 'polite'}));
    const t = h('div.toast' + (opts.error ? '.error' : ''), h('span', message));
    if (opts.action) {
      t.append(h('button.btn.small', {onclick: () => { opts.action.fn(); t.remove(); }}, opts.action.label));
    }
    t.append(h('button.btn.ghost.icon.small', {'aria-label': 'dismiss', onclick: () => t.remove()}, icon('x')));
    toastBox.append(t);
    while (toastBox.children.length > 3) toastBox.firstChild.remove();
    if (!opts.sticky) setTimeout(() => t.remove(), opts.action ? 9000 : 4000);
    return t;
  }
  const fail = err => toast('✗ ' + (err && err.message || err), {error: true});

  // ------------------------------------------------------------ menu
  let openMenu;
  function closeMenu() { if (openMenu) { openMenu.remove(); openMenu = null; } }
  document.addEventListener('click', e => { if (openMenu && !openMenu.contains(e.target)) closeMenu(); }, true);
  document.addEventListener('keydown', e => { if (e.key === 'Escape') closeMenu(); });
  window.addEventListener('blur', closeMenu);
  window.addEventListener('resize', closeMenu);
  // menu(anchor, [{label, icon, fn, href, danger} | '-'])
  function menu(anchor, items) {
    closeMenu();
    const m = h('div.menu', {role: 'menu'});
    for (const it of items) {
      if (!it) continue;
      if (it === '-') { m.append(h('hr')); continue; }
      const attrs = {role: 'menuitem', class: it.danger ? 'danger' : null};
      const el = it.href
        ? h('a', {...attrs, href: it.href, target: it.target || null, rel: it.target ? 'noopener' : null, onclick: closeMenu})
        : h('button', {...attrs, onclick: () => { closeMenu(); it.fn(); }});
      el.append(icon(it.icon || 'more'), it.label);
      m.append(el);
    }
    document.body.append(m);
    const r = anchor.getBoundingClientRect();
    const w = m.offsetWidth, hgt = m.offsetHeight;
    m.style.left = Math.max(8, Math.min(r.right - w, innerWidth - w - 8)) + 'px';
    m.style.top = (r.bottom + 4 + hgt > innerHeight ? Math.max(8, r.top - hgt - 4) : r.bottom + 4) + 'px';
    openMenu = m;
    m.querySelector('button, a')?.focus();
  }

  // ---------------------------------------------------------- dialog
  // ask({title, text, ok, danger, fields:[{name, label, value, type, placeholder}], extra})
  // resolves to the field values (or true), or null when cancelled. text can
  // be nodes (links); extra(form) returns more nodes to put above the
  // buttons (type=button ones don't close the dialog).
  function ask(o) {
    return new Promise(resolve => {
      const form = h('form', {method: 'dialog'});
      form.append(h('h2', o.title));
      if (o.text) form.append(h('p', o.text));
      for (const f of o.fields || []) {
        const input = f.type === 'textarea'
          ? h('textarea.input', {name: f.name, placeholder: f.placeholder || '', rows: 3}, f.value || '')
          : f.type === 'checkbox'
            ? h('input', {type: 'checkbox', name: f.name, checked: !!f.value})
            : h('input.input', {name: f.name, type: f.type || 'text', value: f.value || '', placeholder: f.placeholder || '', required: f.required || null});
        form.append(f.type === 'checkbox'
          ? h('label.check', input, f.label)
          : h('label.field', h('span', f.label), input));
      }
      if (o.extra) append(form, [o.extra(form)]);
      const ok = h('button.btn' + (o.danger ? '.danger' : '.primary'), {value: 'ok'}, o.ok || 'OK');
      if (o.danger) ok.style.cssText = 'color:var(--danger); border-color:var(--danger-dim)';
      form.append(h('div.row', h('button.btn', {value: 'cancel', formnovalidate: true}, 'Cancel'), ok));
      const dlg = h('dialog', form);
      document.body.append(dlg);
      dlg.addEventListener('close', () => {
        const values = {};
        for (const f of o.fields || []) {
          const el = form.elements[f.name];
          values[f.name] = f.type === 'checkbox' ? el.checked : el.value;
        }
        dlg.remove();
        resolve(dlg.returnValue === 'ok' ? (o.fields ? values : true) : null);
      });
      dlg.showModal();
      (form.querySelector('input.input, textarea') || ok).focus();
    });
  }

  // ------------------------------------------------------- formatting
  const rtf = new Intl.RelativeTimeFormat('en', {numeric: 'auto'});
  function ago(iso) {
    if (!iso) return '';
    const s = (new Date(iso) - Date.now()) / 1000;
    const steps = [[60, 'second'], [3600, 'minute', 60], [86400, 'hour', 3600], [86400 * 30, 'day', 86400], [86400 * 365, 'month', 86400 * 30], [Infinity, 'year', 86400 * 365]];
    if (Math.abs(s) < 45) return 'just now';
    for (const [limit, unit, div] of steps) {
      if (Math.abs(s) < limit) return rtf.format(Math.round(s / (div || 1)), unit);
    }
  }
  const fullDate = iso => iso ? new Date(iso).toLocaleString(undefined, {dateStyle: 'medium', timeStyle: 'short'}) : '';
  function bytes(n) {
    if (n < 1024) return n + ' B';
    const u = ['KB', 'MB', 'GB']; let i = -1;
    do { n /= 1024; i++; } while (n >= 1024 && i < u.length - 1);
    return n.toFixed(n < 10 ? 1 : 0) + ' ' + u[i];
  }
  // A stable, pleasant colour per string (artifact or user).
  function hue(s) {
    let x = 0;
    for (const c of s || '') x = (x * 31 + c.charCodeAt(0)) >>> 0;
    return x % 360;
  }
  function avatar(name, title) {
    const label = (name || '?').trim();
    return h('span.avatar', {title: title || label, style: {background: `hsl(${hue(label)} 55% 72%)`}},
      label.charAt(0).toUpperCase() || '?');
  }
  async function copy(text, what) {
    try {
      await navigator.clipboard.writeText(text);
      toast((what || 'Link') + ' copied');
    } catch {
      ask({title: 'Copy ' + (what || 'link').toLowerCase(), fields: [{name: 'v', label: 'select and copy', value: text}]});
    }
  }

  // ------------------------------------------------------ directories
  let usersPromise;
  // users() → Map(id → {id, name, email}); empty for anonymous visitors.
  function users() {
    usersPromise ||= api('/api/users', {anonymousOK: true})
      .then(list => new Map(list.map(u => [u.id, u])))
      .catch(() => new Map());
    return usersPromise;
  }
  const userLabel = u => u ? (u.name || u.email) : '';

  // --------------------------------------------------------- uploads
  // publish(file, {artifact, replace, name, changelog}) uploads an .html page
  // or a .zip archive as a new version (or over `replace`).
  function publish(file, o) {
    const fd = new FormData();
    fd.append('archive', file, file.name);
    if (o.name) fd.append('name', o.name);
    if (o.changelog) fd.append('changelog', o.changelog);
    const path = '/api/artifacts/' + o.artifact + '/versions' + (o.replace ? '/' + o.replace : '');
    return api(path, {method: o.replace ? 'PUT' : 'POST', body: fd});
  }
  const publishable = f => /\.(html?|zip)$/i.test(f.name);

  // onDrop(label, sub, fn): a full-page drop target for .html/.zip files.
  function onDrop(label, sub, fn) {
    let depth = 0, zone;
    const hasFiles = e => [...(e.dataTransfer?.types || [])].includes('Files');
    const hide = () => { depth = 0; zone?.remove(); zone = null; };
    addEventListener('dragenter', e => {
      if (!hasFiles(e)) return;
      e.preventDefault();
      if (depth++ === 0) zone = document.body.appendChild(h('div.dropzone', h('div', icon('upload', {width: 28, height: 28}), h('p', label()), h('small', sub()))));
    });
    addEventListener('dragover', e => { if (hasFiles(e)) e.preventDefault(); });
    addEventListener('dragleave', e => { if (hasFiles(e) && --depth <= 0) hide(); });
    addEventListener('drop', e => {
      if (!hasFiles(e)) return;
      e.preventDefault();
      hide();
      const file = e.dataTransfer.files[0];
      if (!file) return;
      if (!publishable(file)) { fail('Drop an .html page or a .zip archive'); return; }
      fn(file);
    });
  }

  // ------------------------------------------------------------ diffs
  // The viewer's Source tab compares two versions of a file. Tested by
  // ui_diff_test.mjs (node), which loads this file with a fake window.

  // splitLines('a\nb\n') → ['a', 'b']: one final newline ends the last line
  // rather than starting an empty one.
  const splitLines = text => text === '' ? [] : text.replace(/\n$/, '').split('\n');

  // diffLines(a, b) → [{op, a, b, text}], a line-based diff of two texts (or
  // arrays of lines): op is ' ' (in both), '-' (only in a) or '+' (only in
  // b); a and b are 1-based line numbers, null on the side missing the line.
  // Myers' O(ND) algorithm on what is left once the common head and tail are
  // trimmed. Returns null when the texts are too different to be worth it:
  // more than maxLines lines left, or more than maxEdits insertions and
  // deletions (memory grows with maxEdits²).
  function diffLines(a, b, {maxLines = 20000, maxEdits = 1000} = {}) {
    if (typeof a === 'string') a = splitLines(a);
    if (typeof b === 'string') b = splitLines(b);
    let pre = 0, suf = 0;
    while (pre < a.length && pre < b.length && a[pre] === b[pre]) pre++;
    while (suf < a.length - pre && suf < b.length - pre && a[a.length - 1 - suf] === b[b.length - 1 - suf]) suf++;
    const A = a.slice(pre, a.length - suf), B = b.slice(pre, b.length - suf);
    const N = A.length, M = B.length;
    if (N + M > maxLines) return null;

    // V[off + k] is the furthest x reached on diagonal k = x - y; trace[d]
    // keeps V as it was before step d, for diagonals -d-1..d+1.
    const max = Math.min(N + M, maxEdits), off = max + 1;
    const V = new Int32Array(2 * max + 3);
    const trace = [];
    let D = -1;
    search: for (let d = 0; d <= max; d++) {
      trace.push(V.slice(off - d - 1, off + d + 2));
      for (let k = -d; k <= d; k += 2) {
        let x = k === -d || (k !== d && V[off + k - 1] < V[off + k + 1]) ? V[off + k + 1] : V[off + k - 1] + 1;
        let y = x - k;
        while (x < N && y < M && A[x] === B[y]) { x++; y++; }
        V[off + k] = x;
        if (x >= N && y >= M) { D = d; break search; }
      }
    }
    if (D < 0) return null;

    // Walk back from (N, M): each step is a snake of equal lines preceded by
    // one insertion (down) or deletion (right).
    const rev = [];
    let x = N, y = M;
    for (let d = D; d > 0; d--) {
      const v = trace[d], o = d + 1, k = x - y;
      const pk = k === -d || (k !== d && v[o + k - 1] < v[o + k + 1]) ? k + 1 : k - 1;
      const px = v[o + pk], py = px - pk;
      while (x > px && y > py) rev.push([' ', --x, --y]);
      if (x === px) rev.push(['+', -1, --y]); else rev.push(['-', --x, -1]);
    }
    while (x > 0 && y > 0) rev.push([' ', --x, --y]);

    const ops = [];
    for (let i = 0; i < pre; i++) ops.push({op: ' ', a: i + 1, b: i + 1, text: a[i]});
    for (let i = rev.length - 1; i >= 0; i--) {
      const [op, ai, bi] = rev[i];
      ops.push({op, a: ai < 0 ? null : pre + ai + 1, b: bi < 0 ? null : pre + bi + 1, text: ai < 0 ? B[bi] : A[ai]});
    }
    for (let i = suf; i > 0; i--) ops.push({op: ' ', a: a.length - i + 1, b: b.length - i + 1, text: a[a.length - i]});
    return ops;
  }

  // hunks(ops, context) groups diffLines' changes with `context` unchanged
  // lines around them, like `diff -u`: [{start, end, header, oldStart,
  // oldLines, newStart, newLines}] where ops.slice(start, end) are the
  // hunk's lines; what lies between hunks is unchanged. Empty when equal.
  function hunks(ops, context = 3) {
    const out = [];
    let i = 0;
    while (i < ops.length) {
      if (ops[i].op === ' ') { i++; continue; }
      const start = Math.max(0, i - context);
      // Extend over changes separated by at most 2×context unchanged lines.
      let last = i;
      for (let j = i + 1; j < ops.length && j <= last + 2 * context + 1; j++) {
        if (ops[j].op !== ' ') last = j;
      }
      const end = Math.min(ops.length, last + 1 + context);
      const lines = ops.slice(start, end);
      const oldLines = lines.filter(o => o.op !== '+').length;
      const newLines = lines.filter(o => o.op !== '-').length;
      // A side with no lines starts at the line before, as diff -u writes it.
      const before = (side, n) => {
        for (let j = start - 1; j >= 0; j--) if (ops[j][side] != null) return ops[j][side];
        return 0;
      };
      const first = side => lines.find(o => o[side] != null)?.[side];
      const oldStart = oldLines ? first('a') : before('a');
      const newStart = newLines ? first('b') : before('b');
      out.push({start, end, oldStart, oldLines, newStart, newLines,
        header: `@@ -${oldStart},${oldLines} +${newStart},${newLines} @@`});
      i = end;
    }
    return out;
  }

  window.ui = {h, append, $, icon, api, get: p => api(p), post: json('POST'), patch: json('PATCH'), put: json('PUT'), del: p => api(p, {method: 'DELETE'}),
    toast, fail, menu, closeMenu, ask, ago, fullDate, bytes, hue, avatar, copy, users, userLabel, publish, publishable, onDrop, HTTPError,
    splitLines, diffLines, hunks};
})();
