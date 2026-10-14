/**
 * #217 (port of upstream `Kpa-clawbot/CoreScope` PR 2090, adapted): unit tests
 * for the packets "Full Names" toggle and the full-path popover body.
 *
 *   1. Observer cells keep their 16/10 character cut unless Full Names is on,
 *      and are escaped either way.
 *   2. buildPacketsQuery carries fullNames=1 while the view shows full names,
 *      and fullNames=0 when the saved choice is "on" but this view is off,
 *      so a reload of the URL shows what the link showed.
 *   3. The popover lists the hops one per line, numbered, and its title
 *      counts hops, not arrows, wrappers or the path's ambiguity summary.
 *   4. style.css lifts the 120px hop-chip cap only under .pkt-full-names, and
 *      pins the +N pill to the right edge of .path-hops.
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

function makeSandbox() {
  const ctx = {
    window: { addEventListener() {}, removeEventListener() {}, dispatchEvent() {}, innerWidth: 1200, PacketFilter: null },
    document: {
      readyState: 'complete',
      createElement: (tag) => ({ tagName: tag.toUpperCase(), style: {}, appendChild() {}, setAttribute() {},
        addEventListener() {}, querySelectorAll: () => [], querySelector: () => null,
        classList: { add() {}, remove() {}, contains: () => false } }),
      head: { appendChild() {} }, getElementById: () => null, addEventListener() {}, removeEventListener() {},
      querySelectorAll: () => [], querySelector: () => null, body: { appendChild() {} },
    },
    console, Date, Infinity, Math, Array, Object, String, Number, JSON, RegExp, Error, TypeError, RangeError,
    parseInt, parseFloat, isNaN, isFinite, encodeURIComponent, decodeURIComponent,
    setTimeout() {}, clearTimeout() {}, setInterval() {}, clearInterval() {},
    fetch: () => Promise.resolve({ ok: true, json: () => Promise.resolve({}) }),
    performance: { now: () => Date.now() },
    localStorage: (() => { const s = {}; return { getItem: k => (k in s ? s[k] : null), setItem: (k, v) => { s[k] = String(v); }, removeItem: k => { delete s[k]; } }; })(),
    location: { hash: '' }, history: { replaceState() {} }, CustomEvent: class CustomEvent {},
    Map, Set, Promise, URLSearchParams,
    addEventListener() {}, removeEventListener() {}, dispatchEvent() {},
    requestAnimationFrame: (cb) => setTimeout(cb, 0),
    registerPage() {},
  };
  vm.createContext(ctx);
  return ctx;
}
function loadInCtx(ctx, file) {
  vm.runInContext(fs.readFileSync(file, 'utf8'), ctx, { filename: file });
  for (const k of Object.keys(ctx.window)) ctx[k] = ctx.window[k];
}
function loadApi() {
  const ctx = makeSandbox();
  for (const f of ['payload-labels', 'roles', 'app', 'packet-helpers']) loadInCtx(ctx, `public/${f}.js`);
  vm.runInContext(`window.HopDisplay = { renderHop: function (h) { return '<span class="hop">' + h + '</span>'; }, _showFromBtn: function () {} };`, ctx);
  loadInCtx(ctx, 'public/packets.js');
  return ctx._packetsTestAPI;
}

const LONG = 'Key Route Inspector-BRK long observer';
const EVIL = '<img src=x onerror=alert(1)>' + 'x'.repeat(30);
const flat = (id) => ({ id: 1, hash: 'abcd1234', observer_id: id, payload_type: 5, route_type: 1,
  timestamp: '2026-01-01T00:00:00Z', first_seen: '2026-01-01T00:00:00Z', raw_hex: '15', decoded_json: '{}' });
const obsCell = (html) => (/<td class="col-observer"[^>]*>([\s\S]*?)<\/td>/.exec(html) || [])[1];

console.log('\n=== #217: Full Names observer cell ===');
{
  const api = loadApi();
  api._setObservers([{ id: 'o1', name: LONG }, { id: 'o2', name: EVIL }]);
  api._setFullNames(false);
  test('off: a flat row cuts the observer to 16 characters', () => {
    const c = obsCell(api.buildFlatRowHtml(flat('o1')));
    assert.ok(!c.includes(LONG), c);
    assert.ok(c.startsWith(LONG.slice(0, 15)), c);
  });
  api._setFullNames(true);
  test('on: a flat row shows the whole observer name', () => {
    assert.ok(obsCell(api.buildFlatRowHtml(flat('o1'))).includes(LONG));
  });
  test('on: a group header row shows the whole observer name', () => {
    const g = { hash: 'abcd1234', observer_id: 'o1', payload_type: 5, route_type: 1, latest: '2026-01-01T00:00:00Z',
      count: 2, observation_count: 2, observer_count: 2, _children: [], path_json: '[]' };
    assert.ok(obsCell(api.buildGroupRowHtml(g)).includes(LONG));
  });
  test('on: the name is still escaped', () => {
    const c = obsCell(api.buildFlatRowHtml(flat('o2')));
    assert.ok(!c.includes('<img'), c);
    assert.ok(c.includes('&lt;img'), c);
  });
  api._setFullNames(false);
  test('off: the name is still escaped', () => {
    assert.ok(!obsCell(api.buildFlatRowHtml(flat('o2'))).includes('<img'));
  });
}

console.log('\n=== #217: fullNames in the packets URL ===');
{
  const api = loadApi();
  test('off and never saved: no fullNames parameter', () => {
    api._setFullNames(false, false);
    assert.ok(!/fullNames/.test(api.buildPacketsQuery(15, '', false)));
  });
  test('on: fullNames=1', () => {
    api._setFullNames(true, true);
    assert.ok(/[?&]fullNames=1(&|$)/.test(api.buildPacketsQuery(15, '', false)));
  });
  test('saved on but this view off (link override): fullNames=0 survives', () => {
    api._setFullNames(false, true);
    assert.ok(/[?&]fullNames=0(&|$)/.test(api.buildPacketsQuery(15, '', false)));
  });
}

console.log('\n=== #217: path popover body ===');
{
  const api = loadApi();
  const kid = (cls, html) => ({ classList: { contains: c => cls.split(' ').includes(c) }, outerHTML: html });
  const host = { children: [
    kid('hop hop-named', '<a class="hop hop-named">Alpha</a>'), kid('arrow', '<span class="arrow"> → </span>'),
    kid('hop-group', '<span class="hop-group"><a class="hop">Bravo</a><button>!</button></span>'), kid('arrow', '<span class="arrow"> → </span>'),
    kid('hop', '<span class="hop">Charlie</span>'),
    kid('hop-path-warn', '<span class="hop-path-warn">2 ambiguous</span>'),
    kid('path-overflow-pill', '<span class="path-overflow-pill">+2</span>'),
  ] };
  const html = api._pathPopoverHtml(host);
  test('title counts hops, not arrows, wrappers, warnings or the pill', () => {
    assert.ok(/Full path[^<]*3 hops/.test(html), html);
  });
  test('hops are numbered list items in path order', () => {
    const items = html.match(/<li>[\s\S]*?<\/li>/g) || [];
    assert.strictEqual(items.length, 3, html);
    assert.ok(/>1<[\s\S]*Alpha/.test(items[0]) && />2<[\s\S]*Bravo/.test(items[1]) && />3<[\s\S]*Charlie/.test(items[2]));
    assert.ok(!/path-overflow-pill|hop-path-warn|class="arrow"/.test(html));
  });
  test('a single hop is "1 hop"', () => {
    assert.ok(/1 hop(?!s)/.test(api._pathPopoverHtml({ children: [host.children[0]] })));
  });
}

console.log('\n=== #217: style.css ===');
{
  const css = fs.readFileSync('public/style.css', 'utf8');
  test('Full Names lifts the hop chip cap, only under .pkt-full-names', () => {
    assert.ok(/#pktTable\.pkt-full-names\s+\.path-hops\s+\.hop-named\s*\{[^}]*max-width:\s*none/.test(css));
  });
  test('the +N pill sticks to the right edge of .path-hops', () => {
    const m = /\.path-overflow-pill\s*\{([^}]*)\}/.exec(css);
    assert.ok(m && /position:\s*sticky/.test(m[1]) && /right:\s*0/.test(m[1]), m && m[1]);
  });
  test('the popover list has its own style and no hard-coded colour', () => {
    assert.ok(/\.path-popover-list\s*\{/.test(css));
    const blk = css.slice(css.indexOf('.path-popover-list'), css.indexOf('.fux-popover-header'));
    assert.ok(!/#[0-9a-fA-F]{3,8}\b/.test(blk), blk);
  });
}

console.log(`\n${passed} passed, ${failed} failed`);
process.exit(failed ? 1 : 0);
