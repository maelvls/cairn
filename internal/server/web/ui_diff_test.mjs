// Tests for ui.diffLines and ui.hunks (ui.js), the viewer's version diff.
// There is no JS test runner: run it with node.
//
//   node internal/server/web/ui_diff_test.mjs            # assertions
//   node internal/server/web/ui_diff_test.mjs old new    # diff -u of two files
//
// ui.js is a browser script: it runs here in a vm context with a fake window
// and document, enough for its top level.
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';

const noop = () => {};
const ctx = {window: {addEventListener: noop}, document: {addEventListener: noop}};
vm.createContext(ctx);
vm.runInContext(fs.readFileSync(new URL('./ui.js', import.meta.url), 'utf8'), ctx, {filename: 'ui.js'});
const {diffLines, hunks, splitLines} = ctx.window.ui;

// unified renders hunks like diff -u, without the file header.
function unified(a, b, context = 3) {
  const ops = diffLines(a, b);
  if (!ops) return null;
  return hunks(ops, context).map(hk => [hk.header, ...ops.slice(hk.start, hk.end).map(o => o.op + o.text)].join('\n')).join('\n');
}

if (process.argv.length === 4) {
  const [a, b] = process.argv.slice(2).map(p => fs.readFileSync(p, 'utf8'));
  const out = unified(a, b);
  console.log(out === null ? 'files too different to diff' : out || 'identical');
  process.exit(0);
}

let n = 0;
const test = (name, fn) => {
  try { fn(); n++; } catch (e) { console.error('FAIL', name); throw e; }
};

test('splitLines', () => {
  assert.equal(JSON.stringify(splitLines('')), '[]');
  assert.equal(JSON.stringify(splitLines('a\nb\n')), '["a","b"]');
  assert.equal(JSON.stringify(splitLines('a\n\n')), '["a",""]');
});

test('identical texts have no hunks', () => {
  const ops = diffLines('a\nb\nc\n', 'a\nb\nc\n');
  assert.equal(ops.length, 3);
  assert.ok(ops.every(o => o.op === ' '));
  assert.equal(hunks(ops).length, 0);
});

test('one changed line, context clipped at the edges', () => {
  assert.equal(unified('a\nb\nc\n', 'a\nB\nc\n'), '@@ -1,3 +1,3 @@\n a\n-b\n+B\n c');
});

test('line numbers on both sides', () => {
  const ops = diffLines('x\na\nb', 'a\nb\ny');
  assert.equal(ops.map(o => `${o.op}${o.a ?? '_'}:${o.b ?? '_'}`).join(' '), '-1:_  2:1  3:2 +_:3');
});

test('distant changes make separate hunks, close ones merge', () => {
  const a = Array.from({length: 20}, (_, i) => 'l' + (i + 1));
  const b = a.slice();
  b[1] = 'changed 2';
  b[17] = 'changed 18';
  const hs = hunks(diffLines(a, b), 3);
  assert.equal(hs.map(x => x.header).join(' '), '@@ -1,5 +1,5 @@ @@ -15,6 +15,6 @@');
  b[8] = 'changed 9'; // 6 unchanged lines from line 2: one hunk
  assert.equal(hunks(diffLines(a, b), 3).length, 2);
  assert.equal(hunks(diffLines(a, b), 3)[0].header, '@@ -1,12 +1,12 @@');
});

test('empty sides start at the line before', () => {
  assert.equal(unified('', 'a\nb\n'), '@@ -0,0 +1,2 @@\n+a\n+b');
  assert.equal(unified('a\nb\n', ''), '@@ -1,2 +0,0 @@\n-a\n-b');
  assert.equal(unified('a\nb\nc\nd\ne\n', 'a\nb\nc\nd\nX\ne\n', 1), '@@ -4,2 +4,3 @@\n d\n+X\n e');
  assert.equal(unified('a\nb\nc\nd\ne\nf\n', 'a\nb\nc\nf\n', 0), '@@ -4,2 +3,0 @@\n-d\n-e');
});

test('too different returns null', () => {
  const a = Array.from({length: 3000}, (_, i) => 'a' + i);
  const b = Array.from({length: 3000}, (_, i) => 'b' + i);
  assert.equal(diffLines(a, b), null);
  assert.equal(diffLines(a, b, {maxEdits: 6000}).length, 6000);
  assert.equal(diffLines(a, a.concat(['x']), {maxLines: 10}).length, 3001); // head trimmed first
  assert.equal(diffLines(a.concat(['x']), ['y'].concat(a), {maxLines: 10}), null);
});

// Random small inputs: the ops rebuild both sides, number lines correctly,
// and are minimal (as many unchanged lines as the longest common subsequence).
test('random diffs are valid and minimal', () => {
  let seed = 42;
  const rnd = k => { seed = (seed * 1103515245 + 12345) % 2147483648; return seed % k; };
  const lcs = (a, b) => {
    const t = Array.from({length: a.length + 1}, () => new Array(b.length + 1).fill(0));
    for (let i = a.length - 1; i >= 0; i--)
      for (let j = b.length - 1; j >= 0; j--)
        t[i][j] = a[i] === b[j] ? t[i + 1][j + 1] + 1 : Math.max(t[i + 1][j], t[i][j + 1]);
    return t[0][0];
  };
  for (let iter = 0; iter < 3000; iter++) {
    const a = Array.from({length: rnd(25)}, () => 'abcd'[rnd(4)]);
    const b = Array.from({length: rnd(25)}, () => 'abcd'[rnd(4)]);
    const ops = diffLines(a, b);
    const ctxt = `${a.join('')} → ${b.join('')}`;
    assert.equal(ops.filter(o => o.op !== '+').map(o => o.text).join(''), a.join(''), ctxt);
    assert.equal(ops.filter(o => o.op !== '-').map(o => o.text).join(''), b.join(''), ctxt);
    assert.equal(ops.filter(o => o.op !== '+').map(o => o.a).join(','), a.map((_, i) => i + 1).join(','), ctxt);
    assert.equal(ops.filter(o => o.op !== '-').map(o => o.b).join(','), b.map((_, i) => i + 1).join(','), ctxt);
    assert.equal(ops.filter(o => o.op === ' ').length, lcs(a, b), ctxt);
    // Hunks cover every change, in order, without overlapping.
    let prevEnd = 0;
    for (const hk of hunks(ops, rnd(4))) {
      assert.ok(hk.start >= prevEnd && hk.end > hk.start, ctxt);
      prevEnd = hk.end;
    }
    const covered = new Set(hunks(ops, 1).flatMap(hk => ops.slice(hk.start, hk.end)));
    assert.ok(ops.every(o => o.op === ' ' || covered.has(o)), ctxt);
  }
});

console.log(`ok: ${n} diff tests passed`);
