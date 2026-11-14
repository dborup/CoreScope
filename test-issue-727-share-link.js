/**
 * Unit tests (#727 M1): shareable /p links for the Path profile.
 *
 * Real code only: public/terrain-profile.js is loaded with vm.createContext
 * (as test-issue-681-*.js do), and the /p handoff script is lifted out of
 * the shipped public/index.html and run against a stubbed location and
 * history.
 *
 * Covered:
 *   1. parseProfilePair answers the shared fixture table
 *      cmd/server/testdata/profile-param-cases.json exactly as the Go
 *      parser does (TestShareProfile_ParamFixture reads the same file);
 *   2. shareOffered: the dhm.share flag, and only with the profile on;
 *   3. shareURL with the route on (a /p link) and off (the #/map link,
 *      unchanged), the round trip back through parseState, and that no
 *      customizer default rides either;
 *   4. the copy button hands out whichever link the host's shareRoute says;
 *   5. the index.html handoff: /p moves to #/map with an allow-list, no
 *      reload, no history entry, before any asset loads.
 *
 * Usage: node test-issue-727-share-link.js
 */
'use strict';
const vm = require('vm');
const fs = require('fs');
const assert = require('assert');

let passed = 0, failed = 0;
const asyncTests = [];
function test(name, fn) {
  try { fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}
function atest(name, fn) { asyncTests.push([name, fn]); }
const sleep = (ms) => new Promise(r => setTimeout(r, ms));
/* Values from the vm realm do not share this realm's prototypes. */
function eq(actual, expected, msg) {
  assert.deepStrictEqual(JSON.parse(JSON.stringify(actual === undefined ? null : actual)), expected, msg);
}

const PK_A = 'a'.repeat(64);
const PK_B = 'b'.repeat(64);
const ON = { enabled: true, profile: true, share: true };

// ── Sandbox ────────────────────────────────────────────────────────────────

/* Elements that remember children, attributes and listeners: enough for
 * create() to build the panel and fire its copy button. */
function fakeDom() {
  const made = [];
  function mkEl(tag) {
    const on = {};
    const classes = new Set();
    const e = {
      tagName: String(tag).toUpperCase(), children: [], attrs: {}, style: {},
      className: '', textContent: '', innerHTML: '', value: '', hidden: false, disabled: false, parentNode: null,
      classList: {
        add: (c) => classes.add(c), remove: (c) => classes.delete(c), contains: (c) => classes.has(c),
        toggle: (c, want) => { const v = want === undefined ? !classes.has(c) : !!want; if (v) classes.add(c); else classes.delete(c); },
      },
      setAttribute: (k, v) => { e.attrs[k] = String(v); },
      getAttribute: (k) => (Object.prototype.hasOwnProperty.call(e.attrs, k) ? e.attrs[k] : null),
      appendChild: (c) => { e.children.push(c); c.parentNode = e; return c; },
      removeChild: (c) => { e.children = e.children.filter(x => x !== c); c.parentNode = null; return c; },
      addEventListener: (t, fn) => { (on[t] || (on[t] = [])).push(fn); },
      removeEventListener: (t, fn) => { on[t] = (on[t] || []).filter(f => f !== fn); },
      fire: (t) => { (on[t] || []).slice().forEach(fn => fn({ target: e, preventDefault() {} })); },
    };
    made.push(e);
    return e;
  }
  const body = mkEl('body');
  return {
    doc: { createElement: mkEl, body, addEventListener() {}, querySelector: () => null, querySelectorAll: () => [] },
    body,
    find: (id) => made.filter(e => e.attrs['data-testid'] === id).pop() || null,
  };
}

function load() {
  const dom = fakeDom();
  const store = {};
  const win = {
    addEventListener() {}, removeEventListener() {}, dispatchEvent() {},
    localStorage: { getItem: k => (k in store ? store[k] : null), setItem: (k, v) => { store[k] = String(v); }, removeItem: k => { delete store[k]; } },
    document: dom.doc,
    location: { origin: 'http://localhost:13700', pathname: '/', search: '', hash: '#/map' },
    setTimeout: (fn, ms) => setTimeout(fn, ms), clearTimeout: (t) => clearTimeout(t),
  };
  const ctx = {
    window: win, console, Math, Date, JSON, Array, Object, String, Number, RegExp, Error, TypeError, Boolean,
    isNaN, isFinite, parseInt, parseFloat, URLSearchParams, Map, Set, Promise, encodeURIComponent,
    decodeURIComponent, setTimeout, clearTimeout, document: dom.doc,
  };
  vm.createContext(ctx);
  vm.runInContext(fs.readFileSync('public/antenna-height.js', 'utf8'), ctx);
  vm.runInContext(fs.readFileSync('public/terrain-profile.js', 'utf8'), ctx);
  return { TP: ctx.window.TerrainProfile, dom };
}

const { TP } = load();

// ── 1. One grammar, two parsers ────────────────────────────────────────────

console.log('\n=== #727: profile= fixture shared with the Go parser ===');

const fixture = JSON.parse(fs.readFileSync('cmd/server/testdata/profile-param-cases.json', 'utf8')).cases;

test('the fixture table is substantial and has both outcomes', () => {
  assert.ok(fixture.length >= 30, fixture.length + ' cases');
  assert.ok(fixture.some(c => c.valid) && fixture.some(c => !c.valid));
});

for (const c of fixture) {
  test('parseProfilePair: ' + c.name, () => {
    const got = TP.parseProfilePair(c.input);
    if (!c.valid) { assert.strictEqual(got, null, JSON.stringify(got)); return; }
    eq(got, c.refs);
  });
}

// ── 2. The flag ────────────────────────────────────────────────────────────

console.log('\n=== #727: dhm.share ===');

test('shareOffered needs dhm.share and the profile itself', () => {
  assert.strictEqual(TP.shareOffered(ON), true);
  assert.strictEqual(TP.shareOffered({ enabled: true, profile: true }), false);
  assert.strictEqual(TP.shareOffered({ enabled: true, profile: true, share: 'true' }), false);
  assert.strictEqual(TP.shareOffered({ enabled: true, profile: false, share: true }), false);
  assert.strictEqual(TP.shareOffered({ enabled: false, profile: true, share: true }), false);
  assert.strictEqual(TP.shareOffered(null), false);
  assert.strictEqual(TP.shareOffered(undefined), false);
});

// ── 3. shareURL ────────────────────────────────────────────────────────────

console.log('\n=== #727: shareURL ===');

const LOC = { origin: 'http://h', pathname: '/', search: '?lang=da' };

test('route on: a /p link next to index.html, the same keys as the hash link', () => {
  const st = TP.parseState('profile=' + PK_A + ',56.16290,10.20390&model=dtm&ha=12&k=1.5');
  assert.strictEqual(TP.shareURL(st, LOC, undefined, true),
    'http://h/p?profile=' + PK_A + '%2C56.16290%2C10.20390&model=dtm&ha=12&k=1.5');
});

test('route off (or not exactly true): the #/map link, unchanged from before #727', () => {
  const st = TP.parseState('profile=' + PK_A + ',' + PK_B);
  const hash = 'http://h/?lang=da#/map?profile=' + PK_A + '%2C' + PK_B;
  assert.strictEqual(TP.shareURL(st, LOC), hash);
  assert.strictEqual(TP.shareURL(st, LOC, undefined, false), hash);
  assert.strictEqual(TP.shareURL(st, LOC, undefined, 'true'), hash);
});

test('route on under a sub-path: /p sits next to the page, not at the host root', () => {
  const st = TP.parseState('profile=' + PK_A + ',' + PK_B);
  assert.strictEqual(TP.shareURL(st, { origin: 'http://h', pathname: '/app/', search: '' }, undefined, true),
    'http://h/app/p?profile=' + PK_A + '%2C' + PK_B);
  assert.strictEqual(TP.shareURL(st, { origin: 'http://h', pathname: '/app/index.html' }, undefined, true),
    'http://h/app/p?profile=' + PK_A + '%2C' + PK_B);
});

test('a /p link parses back to the same state', () => {
  const st = TP.parseState('profile=56.10000,10.10000,' + PK_B + '&model=dtm&ha=9&hb=4.5&freq=868.1');
  const url = TP.shareURL(st, LOC, undefined, true);
  eq(TP.parseState(url.split('?')[1]), JSON.parse(JSON.stringify(st)));
});

test('a customizer height never rides a /p link', () => {
  const st = TP.parseState('profile=' + PK_A + ',' + PK_B);
  const defaults = TP.themeDefaults({ heightRepeater: '25', heightPoint: '3', model: 'both' });
  const url = TP.shareURL(st, LOC, defaults, true);
  assert.ok(!/[?&](ha|hb)=/.test(url), url);
});

test('null until both ends are picked, route or not', () => {
  assert.strictEqual(TP.shareURL({ a: { kind: 'node', pubkey: PK_A }, b: null }, LOC, undefined, true), null);
});

// ── 4. The copy button ─────────────────────────────────────────────────────

console.log('\n=== #727: the copy button ===');

function copiedWith(extra) {
  const { TP: T, dom } = load();
  const copied = [];
  const tool = T.create(Object.assign({
    mount: dom.body, syncHash: false,
    fetchJSON: () => new Promise(() => {}),
    resolveNode: (pk) => ({ name: pk.slice(0, 4), lat: 56, lon: 10, role: 'repeater' }),
    copyText: (t, ok) => { copied.push(t); ok(); },
  }, extra));
  tool.setEndpoint('a', { kind: 'node', pubkey: PK_A });
  tool.setEndpoint('b', { kind: 'point', lat: 56.1629, lon: 10.2039 });
  dom.find('tp-copy-link').fire('click');
  return sleep(0).then(() => copied);
}

atest('shareRoute: true copies the /p link', async () => {
  const copied = await copiedWith({ shareRoute: true });
  eq(copied, ['http://localhost:13700/p?profile=' + PK_A + '%2C56.16290%2C10.20390']);
});

atest('without shareRoute the copy button keeps the #/map link', async () => {
  const copied = await copiedWith({});
  eq(copied, ['http://localhost:13700/#/map?profile=' + PK_A + '%2C56.16290%2C10.20390']);
});

test('map.js passes the server flag through shareOffered', () => {
  const src = fs.readFileSync('public/map.js', 'utf8');
  assert.ok(/shareRoute:\s*window\.TerrainProfile\.shareOffered\(window\.MC_DHM_CFG\)/.test(src));
});

// ── 5. The index.html handoff ──────────────────────────────────────────────

console.log('\n=== #727: /p hands off to #/map ===');

const html = fs.readFileSync('public/index.html', 'utf8');
const handoffMatch = html.match(/<script data-share-handoff>([\s\S]*?)<\/script>/);

test('index.html carries exactly one handoff script, before any asset', () => {
  assert.ok(handoffMatch, 'no <script data-share-handoff>');
  assert.strictEqual(html.split('data-share-handoff').length, 2);
  const at = html.indexOf('data-share-handoff');
  const firstAsset = Math.min(html.indexOf('<link rel="stylesheet"'), html.indexOf('<script src'));
  assert.ok(at > 0 && at < firstAsset, 'the handoff must run before the first stylesheet or script');
});

/* Runs the shipped script against a stubbed location/history and returns
 * what it did. */
function handoff(pathname, search, opts) {
  opts = opts || {};
  const calls = { replaceState: [], replace: [] };
  const ctx = {
    URLSearchParams,
    location: { pathname, search, replace: (u) => calls.replace.push(u) },
    history: {
      replaceState: (s, t, u) => {
        if (opts.throws) throw new Error('SecurityError');
        calls.replaceState.push({ s, u });
      },
    },
  };
  vm.createContext(ctx);
  vm.runInContext(handoffMatch[1], ctx);
  return calls;
}

test('/p moves to #/map with the profile keys, in place', () => {
  const c = handoff('/p', '?profile=' + PK_A + '%2C' + PK_B + '&model=dtm&ha=9&hb=4&k=1.5&freq=868.1');
  eq(c.replaceState, [{ s: null, u: '/#/map?profile=' + PK_A + '%2C' + PK_B + '&model=dtm&ha=9&hb=4&k=1.5&freq=868.1' }]);
  eq(c.replace, []);
});

test('the allow-list drops every other key, v included', () => {
  const c = handoff('/p', '?evil=%3Cscript%3E&profile=' + PK_A + '%2C' + PK_B + '&v=abc123&lat=1&zoom=3&__proto__=x');
  eq(c.replaceState.map(x => x.u), ['/#/map?profile=' + PK_A + '%2C' + PK_B]);
});

test('a duplicated key keeps its first value only', () => {
  const c = handoff('/p', '?profile=' + PK_A + '%2C' + PK_B + '&ha=1&ha=99');
  eq(c.replaceState.map(x => x.u), ['/#/map?profile=' + PK_A + '%2C' + PK_B + '&ha=1']);
});

test('the result is a link parseState reads like the /p query', () => {
  const q = '?profile=56.10000%2C10.10000%2C' + PK_B + '&ha=9&model=dtm';
  const u = handoff('/p', q).replaceState[0].u;
  eq(TP.parseState(u.split('?')[1]), JSON.parse(JSON.stringify(TP.parseState(q.slice(1)))));
});

test('/p without a query still lands on the map', () => {
  eq(handoff('/p', '').replaceState.map(x => x.u), ['/#/map']);
});

test('under a sub-path, /app/p moves to /app/#/map', () => {
  eq(handoff('/app/p', '?profile=' + PK_A + '%2C' + PK_B).replaceState.map(x => x.u), ['/app/#/map?profile=' + PK_A + '%2C' + PK_B]);
});

test('every other page is left alone', () => {
  for (const p of ['/', '/index.html', '/ap', '/p/x', '/pp', '/P']) {
    const c = handoff(p, '?profile=' + PK_A + '%2C' + PK_B);
    assert.strictEqual(c.replaceState.length + c.replace.length, 0, p);
  }
});

test('without a usable history API it falls back to location.replace', () => {
  const c = handoff('/p', '?profile=' + PK_A + '%2C' + PK_B, { throws: true });
  eq(c.replace, ['/#/map?profile=' + PK_A + '%2C' + PK_B]);
});

// ── Summary ────────────────────────────────────────────────────────────────

(async () => {
  for (const [name, fn] of asyncTests) {
    try { await fn(); passed++; console.log('  ✅ ' + name); }
    catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
  }
  console.log('\n' + passed + ' passed, ' + failed + ' failed\n');
  process.exit(failed ? 1 : 0);
})();
