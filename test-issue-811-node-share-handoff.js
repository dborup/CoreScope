/**
 * Unit tests (#811 M3): the /p?node= browser handoff.
 *
 * Real code only: the handoff script is lifted out of the shipped
 * public/index.html and run against a stubbed location and history, as
 * test-issue-727-share-link.js does for /p?profile=.
 *
 * Covered: /p?node=<64-hex> lands on #/nodes/<key> (lower-cased) with
 * replaceState, carrying the key and nothing else; anything that is not a
 * full key (a #772 prefix, 63/65 characters, non-hex, markup) lands on the
 * #/nodes list with nothing from the request; a node link never becomes a
 * map link; the profile handoff is unchanged.
 *
 * Usage: node test-issue-811-node-share-handoff.js
 */
'use strict';
const vm = require('vm');
const fs = require('fs');
const assert = require('assert');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}

const PK = '3f2a91c0'.repeat(8);
const html = fs.readFileSync('public/index.html', 'utf8');
const m = html.match(/<script data-share-handoff>([\s\S]*?)<\/script>/);

function handoff(pathname, search, opts) {
  opts = opts || {};
  const calls = { replaceState: [], replace: [] };
  const ctx = {
    URLSearchParams,
    location: { pathname, search, replace: (u) => calls.replace.push(u) },
    history: {
      replaceState: (s, t, u) => {
        if (opts.throws) throw new Error('SecurityError');
        calls.replaceState.push(u);
      },
    },
  };
  vm.createContext(ctx);
  vm.runInContext(m[1], ctx);
  return calls;
}

console.log('\n=== #811: /p?node= hands off to #/nodes/<key> ===');

test('the handoff script is there', () => { assert.ok(m, 'no <script data-share-handoff>'); });

test('a full key lands on the node page, in place', () => {
  const c = handoff('/p', '?node=' + PK);
  assert.deepStrictEqual(c.replaceState, ['/#/nodes/' + PK]);
  assert.deepStrictEqual(c.replace, []);
});

test('an upper-case key is lower-cased', () => {
  assert.deepStrictEqual(handoff('/p', '?node=' + PK.toUpperCase()).replaceState, ['/#/nodes/' + PK]);
});

test('node= carries the key and nothing else', () => {
  const c = handoff('/p', '?node=' + PK + '&v=abc&fbclid=x&utm_source=discord&profile=a,b&model=dtm&evil=%3Cscript%3E');
  assert.deepStrictEqual(c.replaceState, ['/#/nodes/' + PK]);
});

test('anything but a full key lands on the node list, carrying nothing', () => {
  for (const bad of [PK.slice(0, 8), PK.slice(0, 63), PK + 'a', 'g'.repeat(64), '', '%3Cscript%3Ealert(1)%3C%2Fscript%3E',
    encodeURIComponent('../../#/map'), PK.slice(0, 60) + '%0A%0D']) {
    const c = handoff('/p', '?node=' + bad);
    assert.deepStrictEqual(c.replaceState, ['/#/nodes'], bad);
  }
});

test('a duplicated key uses its first value', () => {
  assert.deepStrictEqual(handoff('/p', '?node=' + PK + '&node=' + 'b'.repeat(64)).replaceState, ['/#/nodes/' + PK]);
});

test('under a sub-path, /app/p moves to /app/#/nodes/<key>', () => {
  assert.deepStrictEqual(handoff('/app/p', '?node=' + PK).replaceState, ['/app/#/nodes/' + PK]);
});

test('without a usable history API it falls back to location.replace', () => {
  assert.deepStrictEqual(handoff('/p', '?node=' + PK, { throws: true }).replace, ['/#/nodes/' + PK]);
});

test('other pages and // paths are left alone', () => {
  for (const p of ['/', '/ap', '/p/x', '//p', '//evil.example/p']) {
    const c = handoff(p, '?node=' + PK);
    assert.strictEqual(c.replaceState.length + c.replace.length, 0, p);
  }
});

test('the profile handoff is unchanged', () => {
  const a = 'a'.repeat(64), b = 'b'.repeat(64);
  assert.deepStrictEqual(handoff('/p', '?profile=' + a + '%2C' + b + '&ha=1&v=x').replaceState, ['/#/map?profile=' + a + '%2C' + b + '&ha=1']);
  assert.deepStrictEqual(handoff('/p', '').replaceState, ['/#/map']);
});

console.log('\n' + passed + ' passed, ' + failed + ' failed\n');
process.exit(failed ? 1 : 0);
