/* test-issue-96-hide-control.js — #96
 *
 * Optional "Hide CONTROL packets" display filter on the packets page.
 *
 * Decisions under test:
 * - readHideControlPref(urlValue, stored): off unless something turns it on.
 *   The URL (#/packets?hideControl=1|0) wins over the saved choice; without a
 *   URL value the saved choice applies, and an explicit saved '0' stays off.
 * - filterHiddenControl(list, hide): drops only CONTROL (payload type 11),
 *   keeps the order of everything else, and returns the same array when off
 *   (no copy on the 30K-row default path).
 * - buildPacketsQuery() puts hideControl=1 in the URL hash while it is on and
 *   omits it when off (the default), like the other packets filters.
 *
 * Runs the REAL functions from public/packets.js inside a vm sandbox
 * (extracted by name, as test-issue-147-packets-url-detail-params.js does).
 * A function that does not exist fails the tests that call it.
 */
'use strict';

const vm = require('vm');
const fs = require('fs');
const path = require('path');
const assert = require('assert');

console.log('--- test-issue-96-hide-control.js ---');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log(`  ✅ ${name}`); }
  catch (e) { failed++; console.log(`  ❌ ${name}: ${e.message}`); }
}

const SRC = fs.readFileSync(path.join(__dirname, 'public/packets.js'), 'utf8');

function extractFunction(marker) {
  const idx = SRC.indexOf(marker);
  if (idx === -1) return '';
  const open = SRC.indexOf('{', idx);
  let depth = 0;
  for (let i = open; i < SRC.length; i++) {
    if (SRC[i] === '{') depth++;
    else if (SRC[i] === '}' && --depth === 0) return SRC.substring(idx, i + 1);
  }
  throw new Error('unbalanced braces after ' + marker);
}

function constLine(name) {
  const m = SRC.match(new RegExp('const ' + name + ' = [^;]+;'));
  return m ? m[0] : '';
}

function makeSandbox() {
  const ctx = {
    filters: {},
    hideControl: false,
    _packetSortColumn: null,
    _packetSortDirection: 'desc',
    DEFAULT_TIME_WINDOW: 15,
    window: {},
    encodeURIComponent,
  };
  vm.createContext(ctx);
  vm.runInContext([
    constLine('PAYLOAD_TYPE_CONTROL'),
    extractFunction('function readHideControlPref('),
    extractFunction('function filterHiddenControl('),
    extractFunction('function buildPacketsQuery('),
    // const declarations are not sandbox properties; expose what the tests read.
    'this.__fns = {' +
      ' read: typeof readHideControlPref === "function" ? readHideControlPref : null,' +
      ' filter: typeof filterHiddenControl === "function" ? filterHiddenControl : null,' +
      ' query: typeof buildPacketsQuery === "function" ? buildPacketsQuery : null,' +
      ' control: typeof PAYLOAD_TYPE_CONTROL === "number" ? PAYLOAD_TYPE_CONTROL : null };',
  ].join('\n'), ctx);
  return ctx;
}

function fn(ctx, name) {
  const f = ctx.__fns[name];
  assert(f, name + ' is not defined in public/packets.js');
  return f;
}

console.log('\n=== readHideControlPref: default off, saved choice, URL wins ===');
{
  const ctx = makeSandbox();
  test('no URL value and nothing saved: unchecked (current behaviour kept)', () => {
    assert.strictEqual(fn(ctx, 'read')(null, null), false);
  });
  test('saved "1": checked after a reload', () => {
    assert.strictEqual(fn(ctx, 'read')(null, '1'), true);
  });
  test('saved "0" (explicit unchecked choice): stays unchecked after a reload', () => {
    assert.strictEqual(fn(ctx, 'read')(null, '0'), false);
  });
  test('an unknown saved value is treated as unchecked', () => {
    assert.strictEqual(fn(ctx, 'read')(null, 'true'), false);
  });
  test('URL hideControl=1 wins over a saved "0"', () => {
    assert.strictEqual(fn(ctx, 'read')('1', '0'), true);
  });
  test('URL hideControl=0 wins over a saved "1"', () => {
    assert.strictEqual(fn(ctx, 'read')('0', '1'), false);
  });
  test('a URL value that is neither 1 nor 0 is ignored: the saved choice applies', () => {
    assert.strictEqual(fn(ctx, 'read')('yes', '1'), true);
    assert.strictEqual(fn(ctx, 'read')('yes', null), false);
  });
}

console.log('\n=== filterHiddenControl: only CONTROL, order kept, no copy when off ===');
{
  const ctx = makeSandbox();
  test('CONTROL is payload type 11 (firmware PAYLOAD_TYPE_CONTROL)', () => {
    assert.strictEqual(ctx.__fns.control, 11);
  });
  const list = [
    { hash: 'a', payload_type: 4 },
    { hash: 'b', payload_type: 11 },
    { hash: 'c', payload_type: 5 },
    { hash: 'd', payload_type: 11 },
    { hash: 'e' },
  ];
  test('on: removes every CONTROL packet and keeps the others in order', () => {
    const out = fn(ctx, 'filter')(list, true);
    assert.deepStrictEqual(out.map((p) => p.hash), ['a', 'c', 'e']);
    assert.strictEqual(list.length, 5, 'the input array is not modified');
  });
  test('off: returns the same array (no copy, nothing hidden)', () => {
    assert.strictEqual(fn(ctx, 'filter')(list, false), list);
  });
  test('on, with no CONTROL packets: nothing is removed', () => {
    const plain = [{ hash: 'x', payload_type: 1 }, { hash: 'y', payload_type: 2 }];
    assert.deepStrictEqual(fn(ctx, 'filter')(plain, true).map((p) => p.hash), ['x', 'y']);
  });
}

console.log('\n=== buildPacketsQuery: hideControl=1 in the URL hash while on ===');
{
  test('off (default): no hideControl param', () => {
    const ctx = makeSandbox();
    ctx.hideControl = false;
    assert.strictEqual(fn(ctx, 'query')(15, '', false), '');
  });
  test('on: hideControl=1', () => {
    const ctx = makeSandbox();
    ctx.hideControl = true;
    assert.strictEqual(fn(ctx, 'query')(15, '', false), '?hideControl=1');
  });
  test('on, with other filters: kept alongside them', () => {
    const ctx = makeSandbox();
    ctx.hideControl = true;
    ctx.filters = { observer: 'obs1' };
    const q = fn(ctx, 'query')(60, 'SJC', false);
    const params = new URLSearchParams(q.slice(1));
    assert.strictEqual(params.get('hideControl'), '1');
    assert.strictEqual(params.get('observer'), 'obs1');
    assert.strictEqual(params.get('timeWindow'), '60');
    assert.strictEqual(params.get('region'), 'SJC');
  });
}

console.log(`\n${passed} passed, ${failed} failed`);
process.exit(failed > 0 ? 1 : 0);
