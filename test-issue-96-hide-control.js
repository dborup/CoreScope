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
 *   omits it when off (the default), like the other packets filters. Off is
 *   kept as hideControl=0 while the saved choice is on, so the next URL
 *   rewrite does not drop an explicit hideControl=0 (#211 review).
 * - The checkbox's change (setHideControl) filters and updates the URL even
 *   when localStorage throws; the saved choice is best effort (#211 review).
 * - controlHidingEmptiedList: the empty-list note blames CONTROL only when a
 *   hidden CONTROL packet would pass the later filters (#211 review).
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

// storage: the localStorage the sandbox sees (default: an in-memory one).
function makeSandbox(storage) {
  const store = {};
  const ctx = {
    localStorage: storage || {
      getItem: (k) => (Object.prototype.hasOwnProperty.call(store, k) ? store[k] : null),
      setItem: (k, v) => { store[k] = String(v); },
    },
    calls: [],
    updatePacketsUrl() { ctx.calls.push('url'); },
    renderTableRows() { ctx.calls.push('render'); },
    filters: {},
    hideControl: false,
    savedHideControl: false,
    _packetSortColumn: null,
    _packetSortDirection: 'desc',
    DEFAULT_TIME_WINDOW: 15,
    window: {},
    encodeURIComponent,
  };
  vm.createContext(ctx);
  vm.runInContext([
    constLine('PAYLOAD_TYPE_CONTROL'),
    constLine('HIDE_CONTROL_KEY'),
    extractFunction('function readHideControlPref('),
    extractFunction('function readStoredHideControl('),
    extractFunction('function saveHideControlPref('),
    extractFunction('function filterHiddenControl('),
    extractFunction('function controlHidingEmptiedList('),
    extractFunction('function setHideControl('),
    extractFunction('function buildPacketsQuery('),
    // const declarations are not sandbox properties; expose what the tests read.
    'this.__fns = {' +
      ' read: typeof readHideControlPref === "function" ? readHideControlPref : null,' +
      ' filter: typeof filterHiddenControl === "function" ? filterHiddenControl : null,' +
      ' query: typeof buildPacketsQuery === "function" ? buildPacketsQuery : null,' +
      ' readStored: typeof readStoredHideControl === "function" ? readStoredHideControl : null,' +
      ' save: typeof saveHideControlPref === "function" ? saveHideControlPref : null,' +
      ' set: typeof setHideControl === "function" ? setHideControl : null,' +
      ' emptied: typeof controlHidingEmptiedList === "function" ? controlHidingEmptiedList : null,' +
      ' key: typeof HIDE_CONTROL_KEY === "string" ? HIDE_CONTROL_KEY : null,' +
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

console.log('\n=== #211 review, nit 1: a throwing localStorage does not stop the checkbox ===');
{
  const throwing = {
    getItem() { throw new Error('SecurityError: storage blocked'); },
    setItem() { throw new Error('QuotaExceededError'); },
  };
  test('readStoredHideControl: a throwing getItem reads as no saved choice', () => {
    const ctx = makeSandbox(throwing);
    assert.strictEqual(fn(ctx, 'readStored')(), null);
  });
  test('saveHideControlPref: a throwing setItem is caught and reported as not saved', () => {
    const ctx = makeSandbox(throwing);
    assert.strictEqual(fn(ctx, 'save')(true), false);
  });
  test('saveHideControlPref: saves "1"/"0" under the key and reports success', () => {
    const ctx = makeSandbox();
    assert.strictEqual(fn(ctx, 'save')(true), true);
    assert.strictEqual(ctx.localStorage.getItem(ctx.__fns.key), '1');
    assert.strictEqual(fn(ctx, 'save')(false), true);
    assert.strictEqual(ctx.localStorage.getItem(ctx.__fns.key), '0');
  });
  test('setHideControl with a throwing setItem: still filters and updates the URL', () => {
    const ctx = makeSandbox(throwing);
    fn(ctx, 'set')(true);
    assert.strictEqual(ctx.hideControl, true, 'hideControl not set');
    assert.deepStrictEqual(ctx.calls, ['url', 'render'], 'URL update and re-render must still run');
  });
  test('setHideControl: saves the choice, then updates the URL, then re-renders', () => {
    const ctx = makeSandbox();
    fn(ctx, 'set')(true);
    assert.strictEqual(ctx.localStorage.getItem(ctx.__fns.key), '1');
    assert.strictEqual(ctx.savedHideControl, true);
    assert.deepStrictEqual(ctx.calls, ['url', 'render']);
  });
}

console.log('\n=== #211 review, nit 2: an explicit hideControl=0 survives a URL rewrite ===');
{
  test('off while the saved choice is on (URL override): hideControl=0 stays in the URL', () => {
    const ctx = makeSandbox();
    ctx.hideControl = false;
    ctx.savedHideControl = true;
    assert.strictEqual(fn(ctx, 'query')(15, '', false), '?hideControl=0');
  });
  test('off and saved off: still omitted (the default)', () => {
    const ctx = makeSandbox();
    ctx.hideControl = false;
    ctx.savedHideControl = false;
    assert.strictEqual(fn(ctx, 'query')(15, '', false), '');
  });
  test('on while the saved choice is off: hideControl=1 (unchanged)', () => {
    const ctx = makeSandbox();
    ctx.hideControl = true;
    ctx.savedHideControl = false;
    assert.strictEqual(fn(ctx, 'query')(15, '', false), '?hideControl=1');
  });
  test('unchecking with a saved "1": saved becomes "0", so the param is dropped', () => {
    const ctx = makeSandbox();
    ctx.localStorage.setItem(ctx.__fns.key, '1');
    ctx.hideControl = true;
    ctx.savedHideControl = true;
    fn(ctx, 'set')(false);
    assert.strictEqual(ctx.savedHideControl, false);
    assert.strictEqual(fn(ctx, 'query')(15, '', false), '');
  });
  test('unchecking when the save fails: the saved "1" still applies, so hideControl=0 stays', () => {
    const ctx = makeSandbox({ getItem: () => '1', setItem() { throw new Error('QuotaExceededError'); } });
    ctx.hideControl = true;
    ctx.savedHideControl = true;
    fn(ctx, 'set')(false);
    assert.strictEqual(ctx.savedHideControl, true, 'a failed save must not change what is saved');
    assert.strictEqual(fn(ctx, 'query')(15, '', false), '?hideControl=0');
  });
}

console.log('\n=== #211 review, nit 3: the empty-list note blames CONTROL only when it is the cause ===');
{
  const ctx = makeSandbox();
  const beforeHide = [
    { hash: 'a', payload_type: 4, snr: 1 },
    { hash: 'b', payload_type: 11, snr: 9 },
    { hash: 'c', payload_type: 11, snr: 2 },
  ];
  test('a hidden CONTROL packet passes the later filters: hiding CONTROL emptied the list', () => {
    assert.strictEqual(fn(ctx, 'emptied')(beforeHide, (l) => l.filter((p) => p.snr > 5)), true);
  });
  test('no hidden CONTROL packet passes the later filters: another filter emptied it', () => {
    assert.strictEqual(fn(ctx, 'emptied')(beforeHide, (l) => l.filter((p) => p.snr > 50)), false);
  });
  test('only CONTROL packets are run through the later filters', () => {
    const seen = [];
    fn(ctx, 'emptied')(beforeHide, (l) => { seen.push(...l.map((p) => p.hash)); return []; });
    assert.deepStrictEqual(seen, ['b', 'c']);
  });
  test('no CONTROL packet before hiding: not blamed, later filters not run', () => {
    let ran = false;
    assert.strictEqual(fn(ctx, 'emptied')([{ hash: 'a', payload_type: 4 }], (l) => { ran = true; return l; }), false);
    assert.strictEqual(ran, false);
  });
}

console.log(`\n${passed} passed, ${failed} failed`);
process.exit(failed > 0 ? 1 : 0);
