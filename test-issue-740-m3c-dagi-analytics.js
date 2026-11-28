/* test-issue-740-m3c-dagi-analytics.js
 *
 * #740 M3c: the existing Analytics tabs honour the DAGI selection.
 *
 * Runs the REAL app.js api(), url-state.js, dagi-filter.js,
 * advert-interval-analytics.js and analytics.js in one vm with a fake fetch
 * and a small fake DOM (the pattern of test-analytics-tab-state-and-query.js).
 * The selection arrives the way the router hands it over: a link's DAGI
 * param, read by DagiFilter.syncFromUrl before the page's init().
 *
 * Covered, one block per requirement:
 *   1. the control is mounted on the Analytics page (and released on leave);
 *   2. every request of a tab that honours Area carries the checked DAGI
 *      param, Channels and the tabs that do not honour Area carry none, and
 *      an unchecked or unknown code is never sent;
 *   3. the hidden-count notice on those tabs, from one request;
 *   4. the tabs that cannot honour it say so instead;
 *   5. the param stays in the hash across a tab and a window change;
 *   6. the client caches tell selections apart (a new selection is a new
 *      request, never the old answer), and the Advert Intervals bucket
 *      panel names the selection.
 */
'use strict';

const vm = require('vm');
const fs = require('fs');
const assert = require('assert');

let passed = 0, failed = 0;
function test(name, fn) {
  return Promise.resolve()
    .then(fn)
    .then(() => { passed++; console.log('  ✅ ' + name); })
    .catch((e) => { failed++; console.log('  ❌ ' + name + ': ' + (e && e.message || e)); });
}

// Tab renders that need more DOM than the fake has fail after their
// requests went out; the requests are what is checked here.
process.on('unhandledRejection', () => {});

const flush = async () => { for (let i = 0; i < 40; i++) await new Promise((r) => setImmediate(r)); };

const REAL = JSON.parse(fs.readFileSync('test-fixtures/analytics-tabs-172.json', 'utf8'));

// GET /api/config/dagi in M2's shape (a small synthetic subset).
const DAGI_CONFIG = {
  enabled: true,
  credit: 'Indeholder data fra Klimadatastyrelsen, DAGI, november 2026, CC BY 4.0 — data bearbejdet',
  source: { licenseUrl: 'https://creativecommons.org/licenses/by/4.0/', termsUrl: 'https://dataforsyningen.dk/Vilkaar' },
  layers: [
    { layer: 'kommune', param: 'kommune', areas: [{ code: '0751', name: 'Aarhus' }, { code: '0860', name: 'Hjørring' }] },
    { layer: 'landsdel', param: 'landsdel', areas: [{ code: '218546', name: 'Østjylland' }] },
    { layer: 'region', param: 'dkregion', areas: [{ code: '1082', name: 'Region Midtjylland' }] },
    { layer: 'postnummer', param: 'postnr', areas: [{ code: '8000', name: 'Aarhus C' }] },
  ],
};
const HIDDEN = 7;

// The tabs that honour Area (and so DAGI), each with the request paths that
// must carry the selection. Channels is honoured by the shared load's other
// endpoints but not on its own, by design (the Area rule, analytics.js).
const HONOURING = {
  overview: ['/api/analytics/hash-sizes', '/api/analytics/hash-collisions', '/api/analytics/rf', '/api/analytics/topology', '/api/analytics/relay-airtime-share'],
  rf: ['/api/analytics/rf'],
  topology: ['/api/analytics/topology'],
  hashsizes: ['/api/analytics/hash-sizes'],
  collisions: ['/api/analytics/hash-collisions'],
  nodes: ['/api/nodes', '/api/nodes/bulk-health'],
  'my-repeaters': ['/api/nodes'],
  'repeater-metrics': ['/api/nodes'],
  'clock-health': ['/api/nodes/clock-skew'],
  'advert-intervals': ['/api/analytics/advert-intervals'],
  'prefix-tool': ['/api/nodes', '/api/analytics/hash-sizes'],
};
const NOT_HONOURING = ['channels', 'subpaths', 'distance', 'neighbor-graph', 'rf-health', 'roles', 'scopes', 'foreign-traffic', 'wardriving', 'areas'];

function fakeClassList(owner) {
  const list = () => String(owner.className || '').split(/\s+/).filter(Boolean);
  return {
    add: (c) => { if (!list().includes(c)) owner.className = list().concat(c).join(' '); },
    remove: (c) => { owner.className = list().filter((x) => x !== c).join(' '); },
    contains: (c) => list().includes(c),
    toggle() {},
  };
}

function fakeEl(id, tag) {
  let html = '';
  const listeners = {};
  const e = {
    id, tagName: String(tag || 'div').toUpperCase(), value: '', style: {}, dataset: {}, parentElement: null, parentNode: null,
    className: '', hidden: false, children: [], attrs: {}, _text: '',
    get innerHTML() { return html; },
    set innerHTML(v) { html = String(v); },
    get textContent() { return this._text + this.children.map((c) => c.textContent).join(''); },
    set textContent(v) { this.children = []; this._text = String(v); },
    addEventListener(type, fn) { (listeners[type] = listeners[type] || []).push(fn); },
    removeEventListener(type, fn) { listeners[type] = (listeners[type] || []).filter((f) => f !== fn); },
    listenerCount: () => Object.values(listeners).reduce((n, l) => n + l.length, 0),
    dispatch(type, ev) { for (const fn of listeners[type] || []) fn(ev); },
    contains: () => true, closest: () => null,
    querySelector: () => null, querySelectorAll: () => [],
    appendChild(c) { c.parentNode = this; this.children.push(c); return c; },
    insertBefore(c) { c.parentNode = this; this.children.push(c); return c; },
    removeChild(c) { this.children = this.children.filter((x) => x !== c); return c; },
    setAttribute(k, v) { this.attrs[k] = String(v); }, getAttribute(k) { return k in this.attrs ? this.attrs[k] : null },
    removeAttribute(k) { delete this.attrs[k]; },
    focus() {}, getBoundingClientRect: () => ({ left: 0, top: 0, right: 0, bottom: 0, width: 0, height: 0 }),
  };
  e.classList = fakeClassList(e);
  return e;
}

function pageEnv(opts) {
  opts = opts || {};
  const fetchLog = [];
  const respond = (body, status) => ({
    ok: !status || status < 400, status: status || 200, headers: { get: () => null },
    json: async () => JSON.parse(JSON.stringify(body)),
  });
  const fetchImpl = async (url) => {
    url = String(url);
    fetchLog.push(url);
    if (url === '/api/config/dagi') {
      if (opts.configGate) await opts.configGate;
      if (opts.configFail) return respond({ error: 'boom' }, 500);
      return opts.dagiOff ? respond({ enabled: false, layers: [] }) : respond(DAGI_CONFIG);
    }
    if (url === '/api/analytics/dagi-unpositioned') return respond({ unpositionedHidden: HIDDEN });
    const m = /\/api\/analytics\/(rf|topology|channels|hash-sizes|hash-collisions)(\?|$)/.exec(url);
    const key = m && { rf: 'rfData', topology: 'topoData', channels: 'chanData', 'hash-sizes': 'hashData', 'hash-collisions': 'collisionData' }[m[1]];
    if (key) return respond(REAL[key]);
    if (/\/api\/analytics\/neighbor-graph/.test(url)) return respond({ error: 'not found' }, 404);
    if (/\/api\/nodes\/clock-skew/.test(url)) return respond([]);
    if (/\/api\/nodes\/bulk-health/.test(url)) return respond([]);
    if (/\/api\/nodes\?/.test(url)) return respond({ nodes: [{ public_key: 'aabbccddeeff', name: 'R1', role: 'repeater' }], total: 1 });
    return respond({});
  };

  let els = {};
  const app = fakeEl('app');
  let appHtml = '';
  Object.defineProperty(app, 'innerHTML', {
    get: () => appHtml,
    set: (v) => { appHtml = String(v); els = {}; buttons = null; },
  });
  let buttons = null;
  const tabButtons = () => {
    if (!buttons) {
      buttons = [];
      const re = /<button class="tab-btn( active)?" data-tab="([^"]+)"/g;
      let m;
      while ((m = re.exec(appHtml))) {
        const b = fakeEl('', 'button');
        b.dataset = { tab: m[2] };
        if (m[1]) b.classList.add('active');
        buttons.push(b);
      }
    }
    return buttons;
  };
  // Elements in the markup init() writes start the way the markup has them.
  const hiddenInMarkup = (id) => new RegExp('id="' + id + '"[^>]*\\shidden[\\s>]').test(appHtml);
  const displayNoneInMarkup = (id) => new RegExp('id="' + id + '"[^>]*style="[^"]*display:\\s*none').test(appHtml);
  const el = (id) => {
    if (id === 'app') return app;
    if (!els[id]) {
      // The DAGI elements exist only when the markup has them (the red
      // case); every other id gets an element, as the tab renders expect.
      if (/^analyticsDagi/.test(id) && !appHtml.includes('id="' + id + '"')) return null;
      els[id] = fakeEl(id);
      if (hiddenInMarkup(id)) els[id].hidden = true;
      if (displayNoneInMarkup(id)) els[id].style.display = 'none';
      if (id === 'analyticsTabs') {
        els[id].querySelectorAll = (sel) => (sel === '.tab-btn' ? tabButtons() : []);
      }
    }
    return els[id];
  };

  const store = Object.assign({}, opts.storage || {});
  const pages = {};
  const ctx = {
    console: { log() {}, warn() {}, error() {} }, Date, Infinity, Math, Array, Object, String, Number, Boolean,
    JSON, RegExp, Error, TypeError, SyntaxError, parseInt, parseFloat, isNaN, isFinite, encodeURIComponent, decodeURIComponent,
    setTimeout: () => 0, clearTimeout() {}, setInterval: () => 0, clearInterval() {},
    requestAnimationFrame: () => 0, cancelAnimationFrame() {},
    fetch: fetchImpl, performance: { now: () => 0 },
    localStorage: {
      getItem: (k) => (k in store ? store[k] : null),
      setItem: (k, v) => { store[k] = String(v); }, removeItem: (k) => { delete store[k]; },
    },
    sessionStorage: { getItem: () => null, setItem() {}, removeItem() {} },
    location: { hash: '#/analytics', pathname: '/', search: '' },
    CustomEvent: class CustomEvent {}, Map, Set, WeakMap, Promise, URLSearchParams, Intl,
    getComputedStyle: () => ({ getPropertyValue: () => '' }),
    matchMedia: () => ({ matches: false, addEventListener() {}, removeEventListener() {} }),
    timeAgo: () => 'x ago', initTabBar() {}, makeColumnsResizable() {},
    onWS() {}, offWS() {}, connectWS() {}, IATA_COORDS_GEO: {},
    addEventListener() {}, removeEventListener() {}, dispatchEvent() {},
    RegionFilter: { init() {}, onChange: (f) => f, offChange() {}, regionQueryString: () => '', getRegionParam: () => '' },
    AreaFilter: { init() {}, onChange: (f) => f, offChange() {}, areaQueryString: () => opts.area || '', getSelectedLabel: () => '' },
  };
  ctx.history = {
    state: null,
    replaceState: (s, t, url) => { ctx.location.hash = String(url).replace(/^[^#]*/, ''); },
    pushState: (s, t, url) => { ctx.location.hash = String(url).replace(/^[^#]*/, ''); },
  };
  ctx.document = {
    readyState: 'complete', body: fakeEl('body'), head: fakeEl('head'),
    createElement: (t) => fakeEl('', t), createElementNS: (ns, t) => fakeEl('', t), getElementById: el,
    addEventListener() {}, removeEventListener() {},
    querySelectorAll: (sel) => (sel === '.tab-btn' ? tabButtons() : []), querySelector: () => null,
    documentElement: fakeEl('html'), activeElement: null,
  };
  ctx.window = ctx;
  vm.createContext(ctx);
  const load = (f) => vm.runInContext(fs.readFileSync(f, 'utf8'), ctx, { filename: f });
  load('public/payload-labels.js');
  load('public/roles.js');
  load('public/url-state.js');
  try { load('public/app.js'); } catch (e) { /* DOM-only tail */ }
  ctx.registerPage = (name, obj) => { pages[name] = obj; };
  load('public/dagi-filter.js');
  load('public/advert-interval-analytics.js');
  load('public/analytics.js');
  const page = pages.analytics;

  return {
    ctx, fetchLog,
    dagi: () => ctx.DagiFilter,
    // What the router does on a navigation (app.js): sync, then init().
    async mount(hash) {
      ctx.location.hash = hash;
      ctx.DagiFilter.syncFromUrl(hash);
      page.init(app).catch((e) => { this.initError = e; });
      await flush();
    },
    destroy: () => page.destroy(),
    clickTab: async (tab) => {
      const btn = tabButtons().find((b) => b.dataset.tab === tab);
      el('analyticsTabs').dispatch('click', { target: { closest: () => btn } });
      await flush();
    },
    setWindow: async (v) => {
      const tw = el('analyticsTimeWindow');
      tw.value = v;
      tw.dispatch('change', {});
      await flush();
    },
    el,
    notice: () => el('analyticsDagiNotice'),
    hash: () => ctx.location.hash,
  };
}

const pathOf = (u) => u.split('?')[0];
const qOf = (u) => new URLSearchParams(u.split('?')[1] || '');
const reqs = (env, path) => env.fetchLog.filter((u) => pathOf(u) === path);

(async () => {
  console.log('\n=== 1. the control on the Analytics page ===');

  await test('init() mounts the DAGI control beside Region/Area and shows it once the config loads', async () => {
    const env = pageEnv();
    await env.mount('#/analytics');
    const c = env.el('analyticsDagiFilter');
    assert.ok(c, 'no #analyticsDagiFilter container in the markup');
    assert.notStrictEqual(c.style.display, 'none', 'the control stayed hidden');
    assert.ok(/Denmark: All/.test(c.textContent), 'trigger label: ' + c.textContent);
    // It sits in the filter bar with Region and Area.
    const bar = /<div class="analytics-filters">([\s\S]*?)<select id="analyticsTimeWindow"/.exec(env.el('app').innerHTML);
    assert.ok(bar && /id="analyticsAreaFilter"[\s\S]*id="analyticsDagiFilter"/.test(bar[1]), 'not after the Area filter in .analytics-filters');
  });

  await test('with DAGI off the control stays hidden and no DAGI request is made', async () => {
    const env = pageEnv({ dagiOff: true });
    await env.mount('#/analytics?tab=rf&kommune=0751');
    assert.strictEqual(env.el('analyticsDagiFilter').style.display, 'none');
    assert.deepStrictEqual(env.fetchLog.filter((u) => /kommune=|dagi-unpositioned/.test(u)), []);
    assert.ok(env.notice().hidden, 'notice shown with DAGI off');
  });

  await test('destroy() releases the control and its change listener', async () => {
    const env = pageEnv();
    await env.mount('#/analytics');
    env.destroy();
    const before = env.fetchLog.length;
    env.dagi().syncFromUrl('#/analytics?kommune=0751');
    await flush();
    assert.deepStrictEqual(env.fetchLog.slice(before).filter((u) => /\/api\/analytics\//.test(u)), [], 'a left page reloaded on a DAGI change');
  });

  console.log('\n=== 2. the checked param on every request of a tab that honours it ===');

  for (const [tab, paths] of Object.entries(HONOURING)) {
    await test('#/analytics?tab=' + tab + '&kommune=0751 sends kommune=0751 on ' + paths.join(', '), async () => {
      const env = pageEnv({ storage: { 'meshcore-favorites': '["aabbccddeeff"]' } });
      await env.mount('#/analytics?tab=' + tab + '&kommune=0751');
      for (const p of paths) {
        const got = reqs(env, p);
        assert.ok(got.length, 'no request to ' + p + ' in ' + env.fetchLog.join(', '));
        const bad = got.filter((u) => qOf(u).get('kommune') !== '0751');
        assert.deepStrictEqual(bad, [], p + ' without kommune=0751');
      }
    });
  }

  await test('Channels keeps its region-only query, as it does for Area', async () => {
    const env = pageEnv();
    await env.mount('#/analytics?kommune=0751');
    const ch = reqs(env, '/api/analytics/channels');
    assert.ok(ch.length);
    assert.ok(ch.every((u) => !/kommune=/.test(u)), ch.join(', '));
  });

  for (const tab of NOT_HONOURING) {
    await test('tab=' + tab + ' sends no DAGI param', async () => {
      const env = pageEnv();
      await env.mount('#/analytics?tab=' + tab + '&kommune=0751');
      const own = env.fetchLog.filter((u) => /\/api\//.test(u) && !/^\/api\/analytics\/(hash-sizes|hash-collisions|rf|topology|relay-airtime-share)(\?|$)/.test(u));
      assert.deepStrictEqual(own.filter((u) => /kommune=/.test(u)), []);
    });
  }

  await test('a linked code waits for the check: the first load already carries it (no unfiltered request first)', async () => {
    const env = pageEnv();
    await env.mount('#/analytics?tab=rf&kommune=0751');
    const rf = reqs(env, '/api/analytics/rf');
    assert.ok(rf.length >= 1);
    assert.deepStrictEqual(rf.filter((u) => !/kommune=0751/.test(u)), [], 'an unfiltered rf request: ' + rf.join(', '));
  });

  await test('an unknown code is never sent and leaves the hash', async () => {
    const env = pageEnv();
    await env.mount('#/analytics?tab=rf&kommune=9999');
    assert.deepStrictEqual(env.fetchLog.filter((u) => /9999/.test(u)), []);
    assert.ok(!/kommune=/.test(env.hash()), env.hash());
    assert.ok(env.notice().hidden, 'notice for an unknown code');
  });

  await test('a config that fails to load: the linked code is never sent unchecked', async () => {
    const env = pageEnv({ configFail: true });
    await env.mount('#/analytics?tab=rf&kommune=0751');
    const rf = reqs(env, '/api/analytics/rf');
    assert.ok(rf.length, 'no rf request');
    assert.deepStrictEqual(env.fetchLog.filter((u) => /kommune=/.test(u)), []);
    assert.ok(env.notice().hidden, 'notice for an unchecked code');
  });

  await test('a page left while the config loads sends nothing when it arrives', async () => {
    let open;
    const env = pageEnv({ configGate: new Promise((r) => { open = r; }) });
    await env.mount('#/analytics?tab=clock-health&kommune=0751');
    assert.deepStrictEqual(env.fetchLog.filter((u) => /\/api\/(analytics\/|nodes)/.test(u)), [], 'requests before the check');
    env.destroy();
    open();
    await flush();
    assert.deepStrictEqual(env.fetchLog.filter((u) => /\/api\/(analytics\/|nodes)/.test(u)), [], 'a left page loaded');
  });

  await test('Area and DAGI together: both params on the request (the server ANDs them)', async () => {
    const env = pageEnv({ area: '&area=north' });
    await env.mount('#/analytics?tab=rf&kommune=0751');
    assert.ok(env.fetchLog.includes('/api/analytics/rf?area=north&kommune=0751'), env.fetchLog.join(', '));
  });

  await test('a pick reloads the open tab with the param (the onChange path)', async () => {
    const env = pageEnv();
    await env.mount('#/analytics?tab=clock-health');
    assert.ok(reqs(env, '/api/nodes/clock-skew').every((u) => !/kommune=/.test(u)));
    env.dagi().syncFromUrl('#/analytics?tab=clock-health&dkregion=1082');
    await flush();
    assert.ok(env.fetchLog.includes('/api/nodes/clock-skew?dkregion=1082'), env.fetchLog.join(', '));
    assert.ok(env.fetchLog.some((u) => /^\/api\/analytics\/rf\?dkregion=1082$/.test(u)), 'shared load not reloaded');
  });

  console.log('\n=== 3. the hidden-count notice ===');

  await test('a tab that honours the selection shows the whole-list count from one request', async () => {
    const env = pageEnv();
    await env.mount('#/analytics?tab=topology&kommune=0751');
    const n = env.notice();
    assert.ok(n && !n.hidden, 'notice hidden');
    assert.strictEqual(n.textContent, env.dagi().hiddenNotice(HIDDEN));
    assert.strictEqual(n.getAttribute('role') || (/id="analyticsDagiNotice"[^>]*role="status"/.test(env.el('app').innerHTML) ? 'status' : null), 'status');
    await env.clickTab('rf');
    await env.clickTab('nodes');
    assert.strictEqual(reqs(env, '/api/analytics/dagi-unpositioned').length, 1, 'the count was asked for more than once');
  });

  await test('no selection: no notice and no count request', async () => {
    const env = pageEnv();
    await env.mount('#/analytics?tab=topology');
    assert.ok(env.notice().hidden);
    assert.strictEqual(reqs(env, '/api/analytics/dagi-unpositioned').length, 0);
  });

  await test('clearing the selection hides the notice', async () => {
    const env = pageEnv();
    await env.mount('#/analytics?tab=rf&kommune=0751');
    assert.ok(!env.notice().hidden);
    // The clear button's path (applySelection(null)), via a link without a code.
    env.ctx.localStorage.removeItem('meshcore-dagi-filter');
    env.dagi().init(env.el('analyticsDagiFilter'));
    await flush();
    const clear = (function find(n) { if (n && /dagi-clear/.test(n.className)) return n; for (const c of (n && n.children) || []) { const f = find(c); if (f) return f; } return null; })(env.el('analyticsDagiFilter'));
    assert.ok(clear, 'no clear button');
    clear.dispatch('click', { stopPropagation() {} });
    await flush();
    assert.ok(env.notice().hidden, 'notice after the clear');
    assert.ok(!/kommune=/.test(env.hash()), env.hash());
  });

  console.log('\n=== 4. tabs that cannot honour it say so ===');

  for (const tab of NOT_HONOURING) {
    await test('tab=' + tab + ' says the Danish area filter does not apply', async () => {
      const env = pageEnv();
      await env.mount('#/analytics?tab=' + tab + '&kommune=0751');
      const n = env.notice();
      assert.ok(!n.hidden, 'no notice');
      assert.strictEqual(n.textContent, env.dagi().notAppliedNotice());
      assert.ok(/Kommune: Aarhus/.test(n.textContent) && /does not apply/.test(n.textContent), n.textContent);
    });
  }

  await test('switching from a tab that honours it to one that does not swaps the notice, and back', async () => {
    const env = pageEnv();
    await env.mount('#/analytics?tab=rf&kommune=0751');
    assert.strictEqual(env.notice().textContent, env.dagi().hiddenNotice(HIDDEN));
    await env.clickTab('channels');
    assert.strictEqual(env.notice().textContent, env.dagi().notAppliedNotice());
    await env.clickTab('topology');
    assert.strictEqual(env.notice().textContent, env.dagi().hiddenNotice(HIDDEN));
  });

  console.log('\n=== 5. the param in the hash ===');

  await test('#/analytics?tab=…&kommune=… keeps kommune across a tab and a window change', async () => {
    const env = pageEnv();
    await env.mount('#/analytics?tab=rf&kommune=0751');
    await env.clickTab('topology');
    assert.ok(/[?&]tab=topology(&|$)/.test(env.hash()) && /[?&]kommune=0751(&|$)/.test(env.hash()), env.hash());
    await env.setWindow('7d');
    assert.ok(/[?&]window=7d(&|$)/.test(env.hash()) && /[?&]kommune=0751(&|$)/.test(env.hash()), env.hash());
    assert.ok(env.fetchLog.includes('/api/analytics/topology?kommune=0751&window=7d'), env.fetchLog.join(', '));
  });

  console.log('\n=== 6. the caches tell selections apart ===');

  await test('a new selection is a new request on every honouring endpoint, never the cached answer', async () => {
    const env = pageEnv();
    await env.mount('#/analytics?tab=rf&kommune=0751');
    for (const p of HONOURING.overview) assert.strictEqual(reqs(env, p).length, 1, p);
    env.dagi().syncFromUrl('#/analytics?tab=rf&kommune=0860');
    await flush();
    for (const p of HONOURING.overview) {
      assert.ok(reqs(env, p).some((u) => qOf(u).get('kommune') === '0860'), p + ' not asked again for 0860: ' + reqs(env, p).join(', '));
    }
    // Back to 0751 is the cached answer of the same URL (api() ttl), so it
    // costs nothing.
    const before = env.fetchLog.length;
    env.dagi().syncFromUrl('#/analytics?tab=rf&kommune=0751');
    await flush();
    assert.deepStrictEqual(env.fetchLog.slice(before).filter((u) => /\/api\/analytics\/rf/.test(u)), []);
  });

  await test('the Advert Intervals bucket panel names the DAGI selection in its scope line', async () => {
    const env = pageEnv();
    const html = env.ctx.AdvertIntervalAnalytics.renderBucketPanel({
      bucket: { cls: 'flood', intervalS: 3600, snapped: true }, status: 'loading',
      chart: { generated_at: '', nodes: 0 },
      scope: { region: '', area: '', dagi: 'Kommune: Aarhus' },
    });
    assert.ok(/All regions · All areas · Kommune: Aarhus/.test(html), html);
    const none = env.ctx.AdvertIntervalAnalytics.renderBucketPanel({
      bucket: { cls: 'flood', intervalS: 3600, snapped: true }, status: 'loading',
      chart: { generated_at: '', nodes: 0 }, scope: { region: '', area: '' },
    });
    assert.ok(/All regions · All areas\./.test(none), none);
  });

  await test('the bucket scope is the checked selection, read where the panel is built', async () => {
    const src = fs.readFileSync('public/analytics.js', 'utf8');
    const view = /function _aiView\([\s\S]*?\n  }\n/.exec(src);
    assert.ok(view && /dagi:\s*_dagiScopeLabel\(\)/.test(view[0]), '_aiView has no dagi scope');
    const filt = /function _aiFilter\(\)[^\n]*/.exec(src);
    assert.ok(filt && /_nodeScopeQs\(\)/.test(filt[0]), '_aiFilter (the bucket reuse key) lacks the DAGI param: ' + (filt && filt[0]));
  });

  await test('no analytics call site reads the Area query alone any more', async () => {
    const src = fs.readFileSync('public/analytics.js', 'utf8');
    const uses = src.split('\n').filter((l) => /AreaFilter\.areaQueryString\(\)/.test(l));
    assert.strictEqual(uses.length, 1, 'expected one use, in _nodeScopeQs: ' + uses.join('\n'));
    assert.ok(/function _nodeScopeQs\(\)/.test(uses[0]), uses[0]);
  });

  console.log('\n=== dagi-filter.js shared helpers ===');

  await test('notAppliedNotice() names the checked selection; empty without one', async () => {
    const env = pageEnv();
    await env.mount('#/analytics?kommune=0751');
    assert.strictEqual(env.dagi().notAppliedNotice(), 'The Danish area filter (Kommune: Aarhus) does not apply to this tab: it shows data from all areas.');
    env.dagi().syncFromUrl('#/analytics?kommune=9999');
    await flush();
    assert.strictEqual(env.dagi().notAppliedNotice(), '');
  });

  await test('hiddenCount() asks once per page session window and returns the count', async () => {
    const env = pageEnv();
    await env.mount('#/analytics');
    const a = await env.dagi().hiddenCount();
    const b = await env.dagi().hiddenCount();
    assert.strictEqual(a, HIDDEN);
    assert.strictEqual(b, HIDDEN);
    assert.strictEqual(reqs(env, '/api/analytics/dagi-unpositioned').length, 1);
  });

  console.log('\n' + passed + ' passed, ' + failed + ' failed');
  process.exit(failed ? 1 : 0);
})();
