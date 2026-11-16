/**
 * Unit tests (#812 M3): the View Path share link's browser handoff.
 *
 * Real code only: the handoff script is lifted out of the shipped
 * public/index.html and run against a stubbed location and history, as
 * test-issue-812-packet-share-handoff.js does for /p?packet=.
 *
 * Covered: /p?packet=<16-hex>&viewPath=1 lands on
 * #/packets/<hash>?viewPath=1, the View Path modal's own link; viewPath is
 * carried only as exactly "1" and only with a full hash; nothing else of
 * the request is carried; the packet, node and profile handoffs are
 * unchanged.
 *
 * Usage: node test-issue-812-viewpath-share-handoff.js
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

function handoff(pathname, search) {
  const calls = [];
  const ctx = {
    URLSearchParams,
    location: { pathname, search, replace: (u) => calls.push(u) },
    history: { replaceState: (s, t, u) => calls.push(u) },
  };
  vm.createContext(ctx);
  vm.runInContext(m[1], ctx);
  return calls;
}

console.log('\n=== #812 M3: /p?packet=&viewPath=1 hands off to the View Path modal ===');

test('the handoff script is there', () => { assert.ok(m, 'no <script data-share-handoff>'); });

test('a View Path link lands on #/packets/<hash>?viewPath=1', () => {
  assert.deepStrictEqual(handoff('/p', '?packet=' + H + '&viewPath=1'), ['/#/packets/' + H + '?viewPath=1']);
  assert.deepStrictEqual(handoff('/p', '?viewPath=1&packet=' + H.toUpperCase()), ['/#/packets/' + H + '?viewPath=1']);
  assert.deepStrictEqual(handoff('/app/p', '?packet=' + H + '&viewPath=1'), ['/app/#/packets/' + H + '?viewPath=1']);
});

test('it carries viewPath=1 and nothing else', () => {
  assert.deepStrictEqual(handoff('/p', '?packet=' + H + '&viewPath=1&v=abc&fbclid=x&record=allTime.farthestPing&obs=7&evil=%3Cscript%3E'),
    ['/#/packets/' + H + '?viewPath=1']);
});

test('viewPath other than exactly 1 is dropped', () => {
  for (const bad of ['0', 'true', '', '01', ' 1', '1%22%3E%3Cscript%3E', '%3Cscript%3E']) {
    assert.deepStrictEqual(handoff('/p', '?packet=' + H + '&viewPath=' + bad), ['/#/packets/' + H], bad);
  }
  assert.deepStrictEqual(handoff('/p', '?packet=' + H + '&viewpath=1'), ['/#/packets/' + H]);
});

test('without a full hash, viewPath is not carried', () => {
  for (const bad of ['12345', H.slice(0, 15), '', '%3Cscript%3E']) {
    assert.deepStrictEqual(handoff('/p', '?packet=' + bad + '&viewPath=1'), ['/#/packets'], bad);
  }
});

test('viewPath without a packet is not a packet link', () => {
  assert.deepStrictEqual(handoff('/p', '?viewPath=1'), ['/#/map']);
});

test('the packet, node and profile handoffs are unchanged', () => {
  const pk = '3f2a91c0'.repeat(8), a = 'a'.repeat(64), b = 'b'.repeat(64);
  assert.deepStrictEqual(handoff('/p', '?packet=' + H), ['/#/packets/' + H]);
  assert.deepStrictEqual(handoff('/p', '?node=' + pk + '&viewPath=1'), ['/#/nodes/' + pk]);
  assert.deepStrictEqual(handoff('/p', '?profile=' + a + '%2C' + b + '&ha=1&viewPath=1'), ['/#/map?profile=' + a + '%2C' + b + '&ha=1']);
});

console.log('\n' + passed + ' passed, ' + failed + ' failed\n');
process.exit(failed ? 1 : 0);
