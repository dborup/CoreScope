/**
 * Unit tests (#681 M5): the node-detail entry point into the Path profile
 * tool, and the panel polish (antenna-height source badge, copy link, the
 * CC BY 4.0 credit link).
 *
 * public/terrain-profile.js is loaded with vm.createContext, as
 * test-issue-681-profile-tool.js does; the clipboard path runs the real
 * window.copyToClipboard lifted out of public/roles.js. Nothing here is a
 * copy of production logic.
 *
 * Covered:
 *   1. nodeEntryHash / nodeEntryAction: a node with a fix gets a
 *      #/map?profile=<key> link, a node without one (or with the profile
 *      switched off) gets no action at all;
 *   2. parseStart: the one-endpoint link, and that it never parses as a pair;
 *   3. heightBadge: every server antennaSource, and request split by origin;
 *   4. the panel badge follows where each height came from, and an echoed
 *      default never becomes a "request" height on the next measure;
 *   5. copyLink and shareURL, with a stubbed clipboard that succeeds,
 *      rejects, throws, or is missing;
 *   6. the credit line carries a CC BY 4.0 link.
 *
 * Usage: node test-issue-681-m5-node-entry.js
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

const PK_A = 'a'.repeat(64);
const PK_B = 'b'.repeat(64);
const ON = { enabled: true, profile: true };

// ── Sandbox ────────────────────────────────────────────────────────────────

function makeStorage() {
  const store = {};
  return {
    getItem: k => (Object.prototype.hasOwnProperty.call(store, k) ? store[k] : null),
    setItem: (k, v) => { store[k] = String(v); },
    removeItem: k => { delete store[k]; },
  };
}

/* Elements that remember children, attributes and listeners: enough for
 * create() and nodeEntryAction() to build real structure under test. */
function fakeDom() {
  const made = [];
  function mkEl(tag) {
    const on = {};
    const classes = new Set();
    const e = {
      tagName: String(tag).toUpperCase(), children: [], attrs: {}, style: {},
      className: '', textContent: '', innerHTML: '', value: '', hidden: false,
      disabled: false, parentNode: null,
      classList: {
        add: (c) => classes.add(c),
        remove: (c) => classes.delete(c),
        contains: (c) => classes.has(c),
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
    doc: { createElement: mkEl, body: body, addEventListener() {}, querySelector: () => null, querySelectorAll: () => [] },
    body: body,
    find: (id) => made.filter(e => e.attrs['data-testid'] === id).pop() || null,
  };
}

function load(extra) {
  const dom = fakeDom();
  const win = Object.assign({
    addEventListener() {}, removeEventListener() {}, dispatchEvent() {},
    localStorage: makeStorage(),
    document: dom.doc,
    location: { origin: 'http://localhost:13700', pathname: '/', search: '', hash: '#/map' },
    setTimeout: (fn, ms) => setTimeout(fn, ms),
    clearTimeout: (t) => clearTimeout(t),
  }, extra || {});
  const ctx = {
    window: win, console, Math, Date, JSON, Array, Object, String, Number,
    RegExp, Error, TypeError, Boolean, isNaN, isFinite, parseInt, parseFloat,
    URLSearchParams, Map, Set, Promise, encodeURIComponent, decodeURIComponent,
    setTimeout, clearTimeout, document: dom.doc,
  };
  vm.createContext(ctx);
  vm.runInContext(fs.readFileSync('public/terrain-profile.js', 'utf8'), ctx);
  const TP = ctx.window.TerrainProfile;
  assert.ok(TP, 'terrain-profile.js must define window.TerrainProfile');
  return { TP, dom, win };
}

const { TP } = load();

// ── 1. The node-detail link ────────────────────────────────────────────────

console.log('\n=== #681 M5: node detail → Path profile link ===');

test('a node with a fix links to #/map?profile=<its key>', () => {
  const hash = TP.nodeEntryHash({ public_key: PK_A, lat: 56.1629, lon: 10.2039 }, ON);
  assert.strictEqual(hash, '#/map?profile=' + PK_A);
});

test('the link is lower-cased, like every other profile link', () => {
  const hash = TP.nodeEntryHash({ public_key: PK_A.toUpperCase(), lat: 56.1, lon: 10.2 }, ON);
  assert.strictEqual(hash, '#/map?profile=' + PK_A);
});

test('the link parses back to that node as A with B still to pick', () => {
  const hash = TP.nodeEntryHash({ public_key: PK_A, lat: 56.1, lon: 10.2 }, ON);
  const qs = hash.split('?')[1];
  assert.deepStrictEqual(JSON.parse(JSON.stringify(TP.parseStart(qs))), { kind: 'node', pubkey: PK_A });
  // It must never be mistaken for a complete pair.
  assert.strictEqual(TP.parseState(qs), null);
});

{
  const noFix = [
    ['lat/lon null', { lat: null, lon: null }],
    ['lat/lon missing', {}],
    ['the 0,0 no-lock sentinel', { lat: 0, lon: 0 }],
    ['only a latitude', { lat: 56.1, lon: null }],
    ['NaN strings', { lat: 'abc', lon: 'def' }],
    ['out of range', { lat: 91, lon: 10 }],
    ['only an estimated position', { lat: null, lon: null, estimated_lat: 56.1, estimated_lon: 10.2 }],
  ];
  noFix.forEach(([label, pos]) => {
    test('no action for a node without a fix: ' + label, () => {
      const node = Object.assign({ public_key: PK_A }, pos);
      assert.strictEqual(TP.nodeEntryHash(node, ON), null);
      assert.strictEqual(TP.nodeEntryAction(node, ON), null);
    });
  });
}

test('string coordinates from the API still count as a fix', () => {
  assert.strictEqual(TP.nodeEntryHash({ public_key: PK_A, lat: '56.1', lon: '10.2' }, ON), '#/map?profile=' + PK_A);
});

test('a fix on the equator or the prime meridian alone is still a fix', () => {
  assert.ok(TP.nodeEntryHash({ public_key: PK_A, lat: 0, lon: 10.2 }, ON));
  assert.ok(TP.nodeEntryHash({ public_key: PK_A, lat: 56.1, lon: 0 }, ON));
});

{
  const off = [
    ['no dhm block', null],
    ['dhm off', { enabled: false, profile: true }],
    ['profile flag off', { enabled: true, profile: false }],
    ['profile flag missing', { enabled: true }],
    ['truthy but not true', { enabled: 'yes', profile: 1 }],
  ];
  off.forEach(([label, cfg]) => {
    test('no action while the profile is disabled: ' + label, () => {
      const node = { public_key: PK_A, lat: 56.1, lon: 10.2 };
      assert.strictEqual(TP.offered(cfg), false);
      assert.strictEqual(TP.nodeEntryHash(node, cfg), null);
      assert.strictEqual(TP.nodeEntryAction(node, cfg), null);
    });
  });
}

test('offered() is true only for enabled + profile', () => {
  assert.strictEqual(TP.offered(ON), true);
});

{
  const badKeys = [
    ['missing', undefined],
    ['short', 'a'.repeat(63)],
    ['non-hex', 'z'.repeat(64)],
    ['markup', '"><img src=x onerror=alert(1)>' + 'a'.repeat(34)],
  ];
  badKeys.forEach(([label, pk]) => {
    test('no action for a node with an unusable key: ' + label, () => {
      assert.strictEqual(TP.nodeEntryHash({ public_key: pk, lat: 56.1, lon: 10.2 }, ON), null);
    });
  });
}

test('nodeEntryAction builds a real link with a ≥48 px target class and a test id', () => {
  const { TP: T } = load();
  const a = T.nodeEntryAction({ public_key: PK_A, name: '<b>x</b>', lat: 56.1, lon: 10.2 }, ON);
  assert.ok(a, 'no action for a node with a fix');
  assert.strictEqual(a.tagName, 'A');
  assert.strictEqual(a.attrs.href, '#/map?profile=' + PK_A);
  assert.strictEqual(a.attrs['data-testid'], 'node-terrain-profile');
  assert.ok(/\bnode-terrain-action\b/.test(a.className), 'class = ' + a.className);
  // Text, not markup: the node name never reaches an HTML sink here.
  assert.strictEqual(a.innerHTML.indexOf('<b>'), -1);
  const text = a.children.map(c => c.textContent).join('');
  assert.ok(/Terrain profile to/.test(text), 'label = ' + text);
});

// ── 2. The one-endpoint link ───────────────────────────────────────────────

console.log('\n=== #681 M5: parseStart ===');

{
  const notStart = [
    ['a full pair', 'profile=' + PK_A + ',' + PK_B],
    ['a point, not a node', 'profile=56.1,10.2'],
    ['a key plus junk', 'profile=' + PK_A + ',x'],
    ['short hex', 'profile=' + 'a'.repeat(63)],
    ['markup', 'profile=<script>'],
    ['empty', 'profile='],
    ['no parameter', 'model=dtm'],
    ['nothing', ''],
  ];
  notStart.forEach(([label, qs]) => {
    test('parseStart rejects ' + label, () => assert.strictEqual(TP.parseStart(qs), null));
  });
}

test('parseStart accepts an upper-case key and a URLSearchParams', () => {
  const ref = TP.parseStart(new URLSearchParams('profile=' + PK_A.toUpperCase()));
  assert.strictEqual(ref.pubkey, PK_A);
});

// ── 3. The antenna-height source badge ─────────────────────────────────────

console.log('\n=== #681 M5: antenna-height source badge ===');

{
  const rows = [
    ['role-default', null, 'default'],
    ['point-default', null, 'default'],
    ['operator-node', null, 'operator'],
    ['operator-role', null, 'operator'],
    ['request', 'yours', 'yours'],
    ['request', 'link', 'link'],
    ['request', null, 'link'],
    // The server is the authority on defaults: a remembered height the
    // request did not carry cannot claim the badge.
    ['role-default', 'yours', 'default'],
    ['operator-node', 'yours', 'operator'],
  ];
  rows.forEach(([src, origin, want]) => {
    test('badge ' + src + ' / ' + origin + ' → ' + want, () => {
      const b = TP.heightBadge(src, origin);
      assert.strictEqual(b && b.key, want);
      assert.ok(b.label && b.title, 'badge needs a label and a title');
    });
  });
}

test('an unknown source gets no badge rather than a confident one', () => {
  assert.strictEqual(TP.heightBadge('something-new', null), null);
  assert.strictEqual(TP.heightBadge(undefined, 'yours'), null);
  assert.strictEqual(TP.heightBadge('__proto__', null), null);
});

// ── 4. The badge in the panel ──────────────────────────────────────────────

function deferredFetch() {
  const calls = [];
  const fn = (url) => {
    let settle;
    const p = new Promise((resolve, reject) => { settle = { resolve, reject }; });
    calls.push({ url, settle });
    return p;
  };
  fn.calls = calls;
  return fn;
}

/* An answer the way the server builds it: a height the request carried is
 * "request", anything else is the role default. */
function answerFor(url) {
  const q = new URLSearchParams(url.split('?')[1]);
  const end = (h) => (q.get(h) !== null
    ? { kind: 'node', groundM: 0, antennaM: Number(q.get(h)), antennaSource: 'request', assumed: false }
    : { kind: 'node', groundM: 0, antennaM: 10, antennaSource: 'role-default', assumed: true });
  return {
    a: end('ha'), b: end('hb'), distanceM: 1000, model: 'dtm',
    coverage: { state: 'full', pct: 100, firstGapM: null },
    samples: [{ d: 0, dtm: 0 }, { d: 500, dtm: 0 }, { d: 1000, dtm: 0 }],
    physics: { freqMHz: 869.618, freqSource: 'firmware', lambdaM: 0.34474, k: 1.3333, f1MaxM: 9.28 },
    verdict: { state: 'clear', criterion: 0.6 },
  };
}

function openTool(T, dom, fetchJSON, extra) {
  return T.create(Object.assign({ mount: dom.body, fetchJSON, syncHash: false,
    resolveNode: (pk) => ({ name: pk.slice(0, 4), lat: 56, lon: 10, role: 'repeater' }) }, extra || {}));
}

async function settleLast(fetchJSON) {
  const c = fetchJSON.calls[fetchJSON.calls.length - 1];
  c.settle.resolve(answerFor(c.url));
  await sleep(0);
}

function badgeOf(dom, side) {
  const b = dom.find('tp-hsrc-' + side);
  return b && !b.hidden ? b.attrs['data-source'] : null;
}

console.log('\n=== #681 M5: the badge follows each height ===');

atest('role defaults show "default" on both ends', async () => {
  const { TP: T, dom } = load();
  const f = deferredFetch();
  const tool = openTool(T, dom, f);
  tool.setEndpoint('a', { kind: 'node', pubkey: PK_A });
  tool.setEndpoint('b', { kind: 'node', pubkey: PK_B });
  await settleLast(f);
  assert.strictEqual(badgeOf(dom, 'a'), 'default');
  assert.strictEqual(badgeOf(dom, 'b'), 'default');
});

atest('an echoed default is not sent back as a request height', async () => {
  const { TP: T, dom } = load();
  const f = deferredFetch();
  const tool = openTool(T, dom, f);
  tool.setEndpoint('a', { kind: 'node', pubkey: PK_A });
  tool.setEndpoint('b', { kind: 'node', pubkey: PK_B });
  await settleLast(f);
  dom.find('tp-model-dtm').fire('click');
  const url = f.calls[f.calls.length - 1].url;
  assert.ok(!/[?&]ha=/.test(url) && !/[?&]hb=/.test(url), 'defaults leaked into the request: ' + url);
  await settleLast(f);
  assert.strictEqual(badgeOf(dom, 'a'), 'default', 'a model switch turned a default into a confirmed height');
  assert.strictEqual(dom.find('tp-assumed-chip').hidden, false, 'the assumed caveat vanished on a model switch');
});

atest('a height you type shows "yours"', async () => {
  const { TP: T, dom } = load();
  const f = deferredFetch();
  const tool = openTool(T, dom, f);
  tool.setEndpoint('a', { kind: 'node', pubkey: PK_A });
  tool.setEndpoint('b', { kind: 'node', pubkey: PK_B });
  await settleLast(f);
  dom.find('tp-h-a-plus').fire('click');
  await sleep(450);
  await settleLast(f);
  assert.strictEqual(badgeOf(dom, 'a'), 'yours');
  assert.strictEqual(badgeOf(dom, 'b'), 'default');
});

atest('a height remembered in localStorage shows "yours"', async () => {
  const { TP: T, dom } = load();
  T.rememberHeight({ kind: 'node', pubkey: PK_A }, 18);
  const f = deferredFetch();
  const tool = openTool(T, dom, f);
  tool.setEndpoint('a', { kind: 'node', pubkey: PK_A });
  tool.setEndpoint('b', { kind: 'node', pubkey: PK_B });
  await settleLast(f);
  assert.ok(/[?&]ha=18/.test(f.calls[0].url), 'the remembered height was not sent');
  assert.strictEqual(badgeOf(dom, 'a'), 'yours');
});

atest('a height that came in on a shared link shows "link", not "yours"', async () => {
  const { TP: T, dom } = load();
  const f = deferredFetch();
  const tool = openTool(T, dom, f);
  tool.setState(T.parseState('profile=' + PK_A + ',' + PK_B + '&ha=25'));
  await settleLast(f);
  assert.strictEqual(badgeOf(dom, 'a'), 'link');
  assert.strictEqual(badgeOf(dom, 'b'), 'default');
});

atest('an operator height shows "operator"', async () => {
  const { TP: T, dom } = load();
  const f = deferredFetch();
  const tool = openTool(T, dom, f);
  tool.setEndpoint('a', { kind: 'node', pubkey: PK_A });
  tool.setEndpoint('b', { kind: 'node', pubkey: PK_B });
  const c = f.calls[0];
  const ans = answerFor(c.url);
  ans.a = { kind: 'node', groundM: 0, antennaM: 18, antennaSource: 'operator-node', assumed: false };
  c.settle.resolve(ans);
  await sleep(0);
  assert.strictEqual(badgeOf(dom, 'a'), 'operator');
});

// ── 5. Copy link ───────────────────────────────────────────────────────────

console.log('\n=== #681 M5: copy link ===');

test('shareURL builds an absolute #/map?profile= link from the state alone', () => {
  const st = TP.parseState('profile=' + PK_A + ',56.16290,10.20390&model=dtm&ha=12');
  const url = TP.shareURL(st, { origin: 'http://localhost:13700', pathname: '/' });
  assert.strictEqual(url, 'http://localhost:13700/#/map?profile=' + PK_A + '%2C56.16290%2C10.20390&model=dtm&ha=12');
  const back = TP.parseState(url.split('?')[1]);
  assert.strictEqual(back.a.pubkey, PK_A);
  assert.strictEqual(back.ha, 12);
});

test('shareURL is null until both ends are picked', () => {
  assert.strictEqual(TP.shareURL({ a: { kind: 'node', pubkey: PK_A }, b: null }, { origin: 'x', pathname: '/' }), null);
});

/* The real window.copyToClipboard from public/roles.js, run against a
 * stubbed navigator and document, so copyLink is tested through the very
 * helper the panel uses. */
function realCopier(clipboard, execOk) {
  const src = fs.readFileSync('public/roles.js', 'utf8');
  const start = src.indexOf('window.copyToClipboard = function');
  assert.ok(start >= 0, 'roles.js no longer defines window.copyToClipboard');
  const end = src.indexOf('\n  };\n', start);
  const body = src.slice(start, end + 5);
  const appended = [];
  const ctx = {
    window: {}, navigator: { clipboard },
    document: {
      createElement: () => ({ style: {}, focus() {}, select() {} }),
      body: { appendChild: (e) => appended.push(e), removeChild() {} },
      execCommand: () => { if (execOk === 'throws') throw new Error('denied'); return !!execOk; },
    },
  };
  vm.createContext(ctx);
  vm.runInContext(body, ctx);
  return ctx.window.copyToClipboard;
}

atest('copyLink resolves true when the clipboard takes the text', async () => {
  const seen = [];
  const ok = await TP.copyLink('http://x/#/map?profile=1', realCopier({ writeText: (t) => { seen.push(t); return Promise.resolve(); } }, false));
  assert.strictEqual(ok, true);
  assert.deepStrictEqual(seen, ['http://x/#/map?profile=1']);
});

atest('copyLink resolves false when the clipboard rejects and the fallback fails', async () => {
  const ok = await TP.copyLink('u', realCopier({ writeText: () => Promise.reject(new Error('NotAllowedError')) }, false));
  assert.strictEqual(ok, false);
});

atest('copyLink resolves false when the clipboard throws synchronously', async () => {
  const ok = await TP.copyLink('u', realCopier({ writeText: () => { throw new Error('SecurityError'); } }, 'throws'));
  assert.strictEqual(ok, false);
});

atest('copyLink falls back to execCommand when there is no clipboard API', async () => {
  assert.strictEqual(await TP.copyLink('u', realCopier(undefined, true)), true);
  assert.strictEqual(await TP.copyLink('u', realCopier(undefined, 'throws')), false);
});

atest('copyLink resolves false without any copier at all', async () => {
  assert.strictEqual(await TP.copyLink('u', undefined), false);
});

atest('the panel button copies the profile link and says so', async () => {
  const copied = [];
  const { TP: T, dom } = load({ copyToClipboard: (t, ok) => { copied.push(t); ok(); } });
  const f = deferredFetch();
  const tool = openTool(T, dom, f);
  tool.setEndpoint('a', { kind: 'node', pubkey: PK_A });
  const btn = dom.find('tp-copy-link');
  assert.ok(btn, 'no copy-link button in the panel');
  assert.strictEqual(btn.disabled, true, 'copy is offered with only one end picked');
  tool.setEndpoint('b', { kind: 'node', pubkey: PK_B });
  assert.strictEqual(btn.disabled, false, 'copy stayed disabled with both ends picked');
  btn.fire('click');
  await sleep(0);
  assert.deepStrictEqual(copied, ['http://localhost:13700/#/map?profile=' + PK_A + '%2C' + PK_B]);
  assert.strictEqual(dom.find('tp-copy-status').textContent, 'Link copied');
});

atest('the panel says so when copying fails, and does not throw', async () => {
  const { TP: T, dom } = load({ copyToClipboard: () => { throw new Error('SecurityError'); } });
  const f = deferredFetch();
  const tool = openTool(T, dom, f);
  tool.setEndpoint('a', { kind: 'node', pubkey: PK_A });
  tool.setEndpoint('b', { kind: 'node', pubkey: PK_B });
  dom.find('tp-copy-link').fire('click');
  await sleep(0);
  assert.ok(/Could not copy/.test(dom.find('tp-copy-status').textContent), 'status = ' + dom.find('tp-copy-status').textContent);
});

// ── 6. The credit line ─────────────────────────────────────────────────────

console.log('\n=== #681 M5: CC BY 4.0 credit link ===');

test('ATTRIBUTION_URL is the CC BY 4.0 licence over https', () => {
  assert.strictEqual(TP.ATTRIBUTION_URL, 'https://creativecommons.org/licenses/by/4.0/');
});

test('the panel credit links "CC BY 4.0" to the licence in a new tab', () => {
  const { TP: T, dom } = load();
  openTool(T, dom, deferredFetch()).open();
  const link = dom.find('tp-credit-link');
  assert.ok(link, 'the credit has no licence link');
  assert.strictEqual(link.tagName, 'A');
  assert.strictEqual(link.attrs.href, T.ATTRIBUTION_URL);
  assert.strictEqual(link.textContent, 'CC BY 4.0');
  assert.strictEqual(link.attrs.target, '_blank');
  assert.ok(/noopener/.test(link.attrs.rel || ''), 'rel = ' + link.attrs.rel);
  // The text around the link plus the link reads exactly the shared credit.
  const line = link.parentNode;
  assert.strictEqual(line.children.map(c => c.textContent).join(''), T.ATTRIBUTION);
});

// ── Summary ────────────────────────────────────────────────────────────────

(async () => {
  for (const [name, fn] of asyncTests) {
    try { await fn(); passed++; console.log('  ✅ ' + name); }
    catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
  }
  console.log(`\n#681 M5 unit: ${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})();
