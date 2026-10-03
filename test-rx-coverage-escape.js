/* test-rx-coverage-escape.js — the RX coverage leaderboard must HTML-escape
 * observer names and pubkeys, and the hex tooltip must escape node names and
 * prefixes (XSS guard, #14 / #170 / #174).
 *
 * Loads the REAL public/rx-coverage.js in a vm (like
 * test-issue-124-rx-coverage-viewport.js) with the real escapeHtml from
 * public/app.js, mounts the page, answers /api/rx-leaderboard with hostile
 * observers and checks the rendered #rxBoard markup. Nothing from the row
 * builder is copied or sliced out of the source, so removing escaping from
 * any interpolation in the row builder turns this test red.
 */
'use strict';
const vm = require('vm');
const fs = require('fs');
const assert = require('assert');

const SRC = fs.readFileSync(__dirname + '/public/rx-coverage.js', 'utf8');
const APP = fs.readFileSync(__dirname + '/public/app.js', 'utf8');
const ESC = APP.match(/function escapeHtml\(s\) \{[\s\S]*?\n\}\n/);
assert(ESC, 'could not find escapeHtml in public/app.js');

let passed = 0, failed = 0;
async function test(name, fn) {
  try { await fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + (e && e.message || e)); }
}
const flush = async () => { for (let i = 0; i < 20; i++) await new Promise((r) => setImmediate(r)); };

// Mounts the real page in a vm. Map timers are collected, not run, and every
// bound tooltip is recorded.
async function mountPage(hash) {
  const pending = [];
  const timers = [];
  const tooltips = [];
  const els = {};
  const el = () => ({ innerHTML: '', addEventListener() {}, querySelectorAll: () => [], dataset: {} });
  const layer = () => ({ addTo() { return this; }, clearLayers() {}, bindTooltip(t) { tooltips.push(t); return this; } });
  const location = { hash: hash || '#/rx-coverage' };
  const sandbox = {
    console: { log() {}, warn() {}, error() {} },
    Promise, Math, JSON, Number, String, Array, Object, Date, isFinite, parseFloat, parseInt, URLSearchParams,
    window: { MeshConfigReady: Promise.resolve(), MC_CLIENT_RX_COVERAGE: true },
    document: { getElementById: (id) => els[id] || (els[id] = el()), documentElement: {} },
    location,
    history: { replaceState: (_s, _t, url) => { location.hash = url; } },
    localStorage: { getItem: () => null, setItem() {}, removeItem() {} },
    getComputedStyle: () => ({ getPropertyValue: () => '' }),
    L: { map: () => ({ setView() { return this; }, on() { return this; }, invalidateSize() {}, remove() {},
      getZoom: () => 12, getCenter: () => ({ lat: 56, lng: 10 }),
      getBounds: () => ({ getSouth: () => 55, getWest: () => 9, getNorth: () => 57, getEast: () => 11 }) }),
      tileLayer: () => layer(), layerGroup: () => layer(), polygon: () => layer() },
    debounce: (fn) => fn,
    setTimeout: (fn) => { timers.push(fn); return 0; },
    clearTimeout: () => {},
    fetch: (url) => new Promise((resolve, reject) => { pending.push({ url, resolve, reject }); }),
  };
  sandbox.getHashParams = () => new URLSearchParams((location.hash.split('?')[1] || ''));
  vm.createContext(sandbox);
  vm.runInContext(ESC[0], sandbox);
  let page;
  sandbox.registerPage = (_name, obj) => { page = obj; };
  vm.runInContext(SRC, sandbox);

  page.init({ innerHTML: '' });
  await flush();
  const answer = (re, body) => {
    const i = pending.findIndex((p) => re.test(p.url));
    assert(i >= 0, 'the page did not request ' + re);
    pending.splice(i, 1)[0].resolve({ ok: true, json: async () => body });
  };
  return { sandbox, timers, tooltips, answer, esc: sandbox.escapeHtml };
}

// Renders the leaderboard from `observers`.
// Returns the #rxBoard innerHTML and the sandbox's escapeHtml.
async function renderBoard(observers, hash) {
  const { sandbox, answer, esc } = await mountPage(hash);
  answer(/^\/api\/rx-leaderboard\?/, { observers });
  await flush();
  return { html: sandbox.document.getElementById('rxBoard').innerHTML, esc };
}

// Draws one coverage hex whose properties.nodes are `nodes` and returns its
// tooltip HTML (built by the real coverageNodesHtml / coverageNodeRow).
async function renderHexTooltip(nodes) {
  const { timers, tooltips, answer, esc } = await mountPage('#/rx-coverage?lat=56&lon=10&zoom=12');
  answer(/^\/api\/rx-leaderboard\?/, { observers: [] });
  await flush();
  timers.splice(0).forEach((fn) => fn()); // createMap's deferred first drawCoverage()
  answer(/^\/api\/rx-coverage\?/, { features: [{
    geometry: { coordinates: [[[10, 56], [10.01, 56], [10.01, 56.01], [10, 56]]] },
    properties: { count: nodes.length, has_sig: true, best_snr: -3, nodes },
  }] });
  await flush();
  assert.strictEqual(tooltips.length, 1, 'expected one hex tooltip, got ' + tooltips.length);
  return { html: tooltips[0], esc };
}

// Strictly parses every leaderboard data row: the opening tag must consist of
// exactly the expected double-quoted attributes, with no raw <, > or " inside
// a value, followed by the rank and name spans. Anything an unescaped value
// could inject (an extra attribute, a closed tag, a new element) fails here.
const ROW_ATTRS = ['class', 'data-rx', 'data-name', 'role', 'tabindex', 'aria-pressed', 'aria-label'];
function parseRows(html) {
  const rows = [];
  let at = 0;
  for (;;) {
    const start = html.indexOf('<div class="rxb-row', at);
    if (start < 0) break;
    let pos = start + '<div'.length;
    const attrs = {};
    const attrRe = /\s+([a-z-]+)="([^"<>]*)"/y;
    for (;;) {
      attrRe.lastIndex = pos;
      const m = attrRe.exec(html);
      if (!m) break;
      assert(!(m[1] in attrs), 'duplicate attribute ' + m[1] + ' in ' + html.slice(start, start + 300));
      attrs[m[1]] = m[2];
      pos = attrRe.lastIndex;
    }
    assert.strictEqual(html[pos], '>', 'row opening tag is malformed (injected markup?): ' + html.slice(start, start + 300));
    at = pos + 1;
    if (attrs.class === 'rxb-row rxb-head') continue;
    assert.deepStrictEqual(Object.keys(attrs), ROW_ATTRS, 'unexpected row attributes: ' + html.slice(start, pos + 1));
    const body = /^<span class="rxb-rank">\d+<\/span><span class="rxb-name">([^<>"']*)<\/span>/.exec(html.slice(at));
    assert(body, 'row body is malformed (injected markup in the name?): ' + html.slice(at, at + 200));
    rows.push({ attrs, label: body[1] });
  }
  return rows;
}
// Every & in the markup must start one of escapeHtml's entities.
function assertNoBareAmp(html) {
  const bad = /&(?!(?:amp|lt|gt|quot|#39);)/.exec(html);
  assert(!bad, 'raw & in markup: ' + html.slice(Math.max(0, bad && bad.index - 40), (bad && bad.index) + 40));
}

const HOSTILE_NAMES = [
  '<script>alert(1)</script>',
  'Mob "quoted" name',
  "O'Brien's van",
  'Tom & Jerry',
  '"><img src=x onerror=alert(1)>',
  "'><svg onload=alert(1)>",
];
const obs = (pubkey, name, n) => ({ pubkey, name, score: n, cells: n, nodes: n, receptions: n });

(async () => {
  console.log('--- test-rx-coverage-escape.js ---');

  await test('1. hostile observer names are escaped in the label, data-name and aria-label', async () => {
    const observers = HOSTILE_NAMES.map((nm, i) => obs('aabbccddee' + i, nm, 10 - i));
    const { html, esc } = await renderBoard(observers);
    assert(!/<script|<img|<svg/i.test(html), 'raw tag injected into the leaderboard: ' + html);
    assertNoBareAmp(html);
    const rows = parseRows(html);
    assert.strictEqual(rows.length, HOSTILE_NAMES.length, 'expected one row per observer');
    rows.forEach((r) => {
      const o = observers.find((x) => x.pubkey === r.attrs['data-rx']);
      assert(o, 'row has no matching observer: ' + JSON.stringify(r.attrs));
      assert.strictEqual(r.label, esc(o.name), 'label not escaped');
      assert.strictEqual(r.attrs['data-name'], esc(o.name), 'data-name not escaped');
      assert.strictEqual(r.attrs['aria-label'], 'Show coverage for ' + esc(o.name), 'aria-label not escaped');
      assert(!/['"<>]/.test(r.label + r.attrs['data-name'] + r.attrs['aria-label']), 'raw quote or bracket survived: ' + JSON.stringify(r));
    });
    // spot-check the exact escaped forms
    assert(html.includes('&lt;script&gt;alert(1)&lt;/script&gt;'), '<script> name not rendered as &lt;script&gt;');
    assert(html.includes('Mob &quot;quoted&quot; name'), '" not rendered as &quot;');
    assert(html.includes('O&#39;Brien&#39;s van'), "' not rendered as &#39;");
    assert(html.includes('Tom &amp; Jerry'), '& not rendered as &amp;');
    assert(html.includes('&quot;&gt;&lt;img src=x onerror=alert(1)&gt;'), '<img onerror> name not escaped');
  });

  await test('2. a hostile pubkey is escaped in data-rx and in the unnamed label fallback', async () => {
    const evil = '"><img src=x onerror=alert(1)>';
    const observers = [obs(evil, '', 2), obs("x'&<b>", 'Named', 1)];
    const { html, esc } = await renderBoard(observers);
    assert(!/<img|<b>/i.test(html), 'raw tag injected into the leaderboard: ' + html);
    assertNoBareAmp(html);
    const rows = parseRows(html);
    assert.strictEqual(rows.length, 2);
    const unnamed = rows.find((r) => r.attrs['data-rx'] === esc(evil));
    assert(unnamed, 'data-rx does not carry the escaped pubkey: ' + html);
    assert.strictEqual(unnamed.label, esc(evil.slice(0, 10)) + '…', 'unnamed label fallback not escaped');
    assert.strictEqual(unnamed.attrs['aria-label'], 'Show coverage for ' + esc(evil.slice(0, 10)), 'aria-label pubkey fallback not escaped');
    const named = rows.find((r) => r.attrs['data-name'] === 'Named');
    assert(named && named.attrs['data-rx'] === esc("x'&<b>"), 'named row data-rx not escaped: ' + html);
  });

  await test('3. a selected hostile observer (rx= in the URL) is still escaped', async () => {
    const pk = 'abc"><img src=x onerror=alert(1)>';
    const { html, esc } = await renderBoard([obs(pk.toLowerCase(), '<b>sel</b>', 1)], '#/rx-coverage?rx=' + encodeURIComponent(pk));
    assert(!/<img|<b>/i.test(html), 'raw tag injected into the selected row: ' + html);
    const rows = parseRows(html);
    assert.strictEqual(rows.length, 1);
    assert.strictEqual(rows[0].attrs.class, 'rxb-row sel', 'the observer from the URL is not selected');
    assert.strictEqual(rows[0].attrs['aria-pressed'], 'true');
    assert.strictEqual(rows[0].label, esc('<b>sel</b>'));
  });

  await test('4. rows stay keyboard-operable (role, tabindex, aria-pressed)', async () => {
    const { html } = await renderBoard([obs('aabbccddee', 'Mob', 1)]);
    const r = parseRows(html)[0];
    assert.strictEqual(r.attrs.role, 'button');
    assert.strictEqual(r.attrs.tabindex, '0');
    assert.strictEqual(r.attrs['aria-pressed'], 'false');
  });

  await test('5. hex tooltip: hostile node names and prefixes are escaped (coverageNodeRow)', async () => {
    const nodes = HOSTILE_NAMES.map((nm, i) => ({ prefix: 'ab' + i, name: nm, snr: -4 - i, count: i + 1 }))
      .concat([{ prefix: '"><img src=x onerror=alert(1)>', name: '', snr: null, count: 1 }]);
    const { html, esc } = await renderHexTooltip(nodes);
    assert(!/<script|<img|<svg/i.test(html), 'raw tag injected into the hex tooltip: ' + html);
    assertNoBareAmp(html);
    // Each row: <span>label</span>; the label is the escaped name, or the
    // escaped prefix in <code> when the name is unresolved.
    const labels = [...html.matchAll(/<div style="display:flex[^"]*"><span>([\s\S]*?)<\/span><span /g)].map((m) => m[1]);
    assert.deepStrictEqual(labels, HOSTILE_NAMES.map((nm) => esc(nm))
      .concat(['<code>' + esc('"><img src=x onerror=alert(1)>') + '</code>']));
    assert(html.includes('&lt;script&gt;alert(1)&lt;/script&gt;'), '<script> name not rendered as &lt;script&gt;');
    assert(html.includes('O&#39;Brien&#39;s van'), "' not rendered as &#39;");
    assert(html.includes('Tom &amp; Jerry'), '& not rendered as &amp;');
    assert(html.includes('7 nodes heard here'), 'tooltip header missing: ' + html);
  });

  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})();
