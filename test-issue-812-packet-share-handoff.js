/**
 * Unit tests (#812 M2): the /p?packet= browser handoff.
 *
 * Real code only: the handoff script is lifted out of the shipped
 * public/index.html and run against a stubbed location and history, as
 * test-issue-811-node-share-handoff.js does for /p?node=.
 *
 * Covered: /p?packet=<16-hex> lands on #/packets/<hash> (lower-cased) with
 * replaceState, carrying the hash and nothing else; anything that is not a
 * full hash (a numeric id, 15/17 characters, non-hex, markup) lands on the
 * #/packets list with nothing from the request; a packet link never becomes
 * a map or node link; the node and profile handoffs are unchanged.
 *
 * Usage: node test-issue-812-packet-share-handoff.js
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

const H = '3f2a91c00b1e5d7a';
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

console.log('\n=== #812: /p?packet= hands off to #/packets/<hash> ===');

test('the handoff script is there', () => { assert.ok(m, 'no <script data-share-handoff>'); });

test('a full hash lands on the packet page, in place', () => {
  const c = handoff('/p', '?packet=' + H);
  assert.deepStrictEqual(c.replaceState, ['/#/packets/' + H]);
  assert.deepStrictEqual(c.replace, []);
});

test('an upper-case hash is lower-cased', () => {
  assert.deepStrictEqual(handoff('/p', '?packet=' + H.toUpperCase()).replaceState, ['/#/packets/' + H]);
});

test('packet= carries the hash and nothing else', () => {
  const c = handoff('/p', '?packet=' + H + '&v=abc&fbclid=x&utm_source=discord&profile=a,b&model=dtm&evil=%3Cscript%3E');
  assert.deepStrictEqual(c.replaceState, ['/#/packets/' + H]);
});

test('anything but a full hash lands on the packet list, carrying nothing', () => {
  for (const bad of ['12345', H.slice(0, 15), H + 'a', 'g'.repeat(16), '', '%3Cscript%3Ealert(1)%3C%2Fscript%3E',
    encodeURIComponent('../../#/map'), H.slice(0, 14) + '%0A%0D', 'a'.repeat(64)]) {
    const c = handoff('/p', '?packet=' + bad);
    assert.deepStrictEqual(c.replaceState, ['/#/packets'], bad);
  }
});

test('a duplicated key uses its first value', () => {
  assert.deepStrictEqual(handoff('/p', '?packet=' + H + '&packet=' + 'b'.repeat(16)).replaceState, ['/#/packets/' + H]);
});

test('under a sub-path, /app/p moves to /app/#/packets/<hash>', () => {
  assert.deepStrictEqual(handoff('/app/p', '?packet=' + H).replaceState, ['/app/#/packets/' + H]);
});

test('without a usable history API it falls back to location.replace', () => {
  assert.deepStrictEqual(handoff('/p', '?packet=' + H, { throws: true }).replace, ['/#/packets/' + H]);
});

test('other pages and // paths are left alone', () => {
  for (const p of ['/', '/ap', '/p/x', '//p', '//evil.example/p']) {
    const c = handoff(p, '?packet=' + H);
    assert.strictEqual(c.replaceState.length + c.replace.length, 0, p);
  }
});

test('the node and profile handoffs are unchanged', () => {
  const pk = '3f2a91c0'.repeat(8), a = 'a'.repeat(64), b = 'b'.repeat(64);
  assert.deepStrictEqual(handoff('/p', '?node=' + pk).replaceState, ['/#/nodes/' + pk]);
  assert.deepStrictEqual(handoff('/p', '?node=' + pk + '&packet=' + H).replaceState, ['/#/nodes/' + pk]);
  assert.deepStrictEqual(handoff('/p', '?profile=' + a + '%2C' + b + '&ha=1&v=x').replaceState, ['/#/map?profile=' + a + '%2C' + b + '&ha=1']);
  assert.deepStrictEqual(handoff('/p', '').replaceState, ['/#/map']);
});

console.log('\n' + passed + ' passed, ' + failed + ' failed\n');
process.exit(failed ? 1 : 0);
