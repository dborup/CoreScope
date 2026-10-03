/* test-analytics-tab-state-and-query.js
 *
 * #179: the Prefix Tool fetched `/analytics/hash-sizes&region=…` (no `?`)
 *       whenever a region or area filter was active, and got the SPA HTML.
 * #183: leaving #/analytics and coming back without ?tab= marked Overview
 *       active but rendered the tab selected before leaving.
 *
 * Runs the REAL app.js api() and the REAL analytics page in one vm with a
 * fake fetch and a small fake DOM whose tab buttons come from the markup
 * init() writes, so the active button is whatever the page set.
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

const flush = async () => { for (let i = 0; i < 30; i++) await new Promise((r) => setImmediate(r)); };

// Real responses of the CI fixture server for the shared endpoints (#172).
const REAL = JSON.parse(fs.readFileSync('test-fixtures/analytics-tabs-172.json', 'utf8'));
const MARKERS = { overview: /Total Transmissions/, topology: /Per-Observer Reachability/ };

function fakeClassList(initial) {
  const s = new Set(initial);
  return { add: (c) => s.add(c), remove: (c) => s.delete(c), contains: (c) => s.has(c), toggle() {} };
}

function fakeEl(id) {
  let html = '';
  const listeners = {};
  return {
    id, value: '', style: {}, dataset: {}, parentElement: null,
    get innerHTML() { return html; },
    set innerHTML(v) { html = String(v); },
    classList: fakeClassList([]),
    addEventListener(type, fn) { (listeners[type] = listeners[type] || []).push(fn); }, removeEventListener() {},
    dispatch(type, ev) { for (const fn of listeners[type] || []) fn(ev); },
    contains: () => true,
    querySelector: () => null, querySelectorAll: () => [],
    appendChild: (c) => c, insertBefore: (c) => c, setAttribute() {}, getAttribute: () => null,
  };
}

function pageEnv() {
  const fetchLog = [];
  let region = '', area = '';
  const respond = (body) => ({
    ok: true, status: 200, headers: { get: () => null },
    json: async () => JSON.parse(JSON.stringify(body)),
  });
  const fetchImpl = async (url) => {
    fetchLog.push(url);
    const m = /\/api\/analytics\/(rf|topology|channels|hash-sizes|hash-collisions)(\?|$)/.exec(url);
    const key = m && { rf: 'rfData', topology: 'topoData', channels: 'chanData', 'hash-sizes': 'hashData', 'hash-collisions': 'collisionData' }[m[1]];
    if (key) return respond(REAL[key]);
    if (/\/api\/nodes\?/.test(url)) return respond({ nodes: [{ public_key: 'aabbccddeeff', name: 'R1', role: 'repeater' }] });
    return respond({});
  };

  // A new app.innerHTML is a new page: every element below it is new, so
  // listeners of the previous mount are gone, as in a browser.
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
        buttons.push({ dataset: { tab: m[2] }, classList: fakeClassList(m[1] ? ['active'] : []), setAttribute() {} });
      }
    }
    return buttons;
  };
  const el = (id) => {
    if (id === 'app') return app;
    if (!els[id]) {
      els[id] = fakeEl(id);
      if (id === 'analyticsTabs') {
        els[id].querySelectorAll = (sel) => (sel === '.tab-btn' ? tabButtons() : []);
        els[id].querySelector = (sel) => {
          const m = /^\[data-tab="([^"]+)"\]$/.exec(sel);
          return m ? tabButtons().find((b) => b.dataset.tab === m[1]) || null : null;
        };
      }
    }
    return els[id];
  };

  const pages = {};
  const ctx = {
    window: { addEventListener() {}, removeEventListener() {}, dispatchEvent() {} },
    document: {
      readyState: 'complete', body: { appendChild() {} }, head: { appendChild() {} },
      createElement: () => fakeEl(''), getElementById: el, addEventListener() {},
      querySelectorAll: (sel) => (sel === '.tab-btn' ? tabButtons() : []), querySelector: () => null,
      documentElement: fakeEl('html'),
    },
    console: { log() {}, warn() {}, error: console.error }, Date, Infinity, Math, Array, Object, String, Number,
    JSON, RegExp, Error, TypeError, parseInt, parseFloat, isNaN, isFinite, encodeURIComponent, decodeURIComponent,
    setTimeout: () => 0, clearTimeout() {}, setInterval: () => 0, clearInterval() {},
    requestAnimationFrame: () => 0, cancelAnimationFrame() {},
    fetch: fetchImpl, performance: { now: () => 0 },
    localStorage: { getItem: () => null, setItem() {}, removeItem() {} },
    location: { hash: '#/analytics' }, history: { replaceState() {} },
    CustomEvent: class CustomEvent {}, Map, Set, Promise, URLSearchParams,
    getComputedStyle: () => ({ getPropertyValue: () => '' }),
    timeAgo: () => 'x ago', initTabBar() {}, makeColumnsResizable() {},
    onWS() {}, offWS() {}, connectWS() {}, invalidateApiCache() {}, IATA_COORDS_GEO: {},
    RegionFilter: { init() {}, onChange() {}, regionQueryString: () => region },
    AreaFilter: { init() {}, onChange() {}, areaQueryString: () => area },
  };
  vm.createContext(ctx);
  const load = (f) => { vm.runInContext(fs.readFileSync(f, 'utf8'), ctx); for (const k of Object.keys(ctx.window)) ctx[k] = ctx.window[k]; };
  load('public/payload-labels.js');
  load('public/roles.js');
  try { load('public/app.js'); } catch (e) { /* DOM-only tail */ }
  ctx.registerPage = (name, obj) => { pages[name] = obj; };   // app.js defines its own
  load('public/analytics.js');
  const page = pages.analytics;

  return {
    fetchLog,
    setFilters: (r, a) => { region = r ? '&region=' + r : ''; area = a ? '&area=' + a : ''; },
    async mount(hash) { ctx.location.hash = hash; page.init(app); await flush(); },
    destroy: () => page.destroy(),
    clickTab: async (tab) => {
      const btn = tabButtons().find((b) => b.dataset.tab === tab);
      el('analyticsTabs').dispatch('click', { target: { closest: () => btn } });
      await flush();
    },
    activeTabs: () => tabButtons().filter((b) => b.classList.contains('active')).map((b) => b.dataset.tab),
    content: () => el('analyticsContent').innerHTML,
  };
}

(async () => {
  console.log('\n=== #179: Prefix Tool hash-sizes query ===');

  const hashSizesUrls = (env) => env.fetchLog.filter((u) => /\/api\/analytics\/hash-sizes/.test(u));

  await test('with a region and an area, the Prefix Tool asks for hash-sizes?region=…&area=…', async () => {
    const env = pageEnv();
    env.setFilters('SJC', 'north');
    await env.mount('#/analytics?tab=prefix-tool');
    const urls = hashSizesUrls(env);
    const bad = urls.filter((u) => !/\/api\/analytics\/hash-sizes(\?|$)/.test(u));
    assert.deepStrictEqual(bad, [], 'malformed hash-sizes URL(s)');
    assert.ok(urls.some((u) => /\/api\/analytics\/hash-sizes\?region=SJC&area=north$/.test(u)), 'no hash-sizes?region=SJC&area=north: ' + urls.join(', '));
  });

  await test('with only an area, the query still starts with ?', async () => {
    const env = pageEnv();
    env.setFilters('', 'north');
    await env.mount('#/analytics?tab=prefix-tool');
    const bad = hashSizesUrls(env).filter((u) => !/\/api\/analytics\/hash-sizes(\?|$)/.test(u));
    assert.deepStrictEqual(bad, [], 'malformed hash-sizes URL(s)');
  });

  await test('without filters, the Prefix Tool asks for plain /analytics/hash-sizes', async () => {
    const env = pageEnv();
    await env.mount('#/analytics?tab=prefix-tool');
    const urls = hashSizesUrls(env);
    assert.ok(urls.length > 0, 'no hash-sizes fetch');
    assert.ok(urls.every((u) => /\/api\/analytics\/hash-sizes$/.test(u)), 'unexpected hash-sizes URL(s): ' + urls.join(', '));
  });

  // The same helper now builds every analytics query; these pin the URLs
  // the other callers sent before it, so the refactor changes none of them.
  await test('the shared loads keep their query strings (region + area + window)', async () => {
    const env = pageEnv();
    env.setFilters('SJC', 'north');
    await env.mount('#/analytics?window=7d');
    for (const want of [
      '/api/analytics/hash-sizes?region=SJC&area=north',
      '/api/analytics/hash-collisions?region=SJC&area=north',
      '/api/analytics/rf?region=SJC&area=north&window=7d',
      '/api/analytics/topology?region=SJC&area=north&window=7d',
      '/api/analytics/channels?region=SJC&window=7d',
      '/api/analytics/relay-airtime-share?region=SJC&area=north&window=7d',
    ]) assert.ok(env.fetchLog.includes(want), 'missing ' + want + ' in ' + env.fetchLog.join(', '));
  });

  await test('the shared loads without filters have no query string', async () => {
    const env = pageEnv();
    await env.mount('#/analytics');
    for (const ep of ['hash-sizes', 'hash-collisions', 'rf', 'topology', 'channels', 'relay-airtime-share']) {
      assert.ok(env.fetchLog.includes('/api/analytics/' + ep), 'missing plain /api/analytics/' + ep);
    }
  });

  await test('the distance tab asks for distance?region=… and plain distance', async () => {
    const env = pageEnv();
    env.setFilters('SJC');
    await env.mount('#/analytics?tab=distance');
    assert.ok(env.fetchLog.includes('/api/analytics/distance?region=SJC'), env.fetchLog.join(', '));
    const env2 = pageEnv();
    await env2.mount('#/analytics?tab=distance');
    assert.ok(env2.fetchLog.includes('/api/analytics/distance'), env2.fetchLog.join(', '));
  });

  console.log('\n=== #183: tab state across leave and return ===');

  await test('select Topology, leave, return without ?tab=: button and content are Overview', async () => {
    const env = pageEnv();
    await env.mount('#/analytics');
    await env.clickTab('topology');
    assert.ok(MARKERS.topology.test(env.content()), 'Topology did not render on click');
    env.destroy();
    await env.mount('#/analytics');
    assert.deepStrictEqual(env.activeTabs(), ['overview'], 'active button');
    assert.ok(MARKERS.overview.test(env.content()), 'content is not Overview: ' + env.content().slice(0, 200));
    assert.ok(!MARKERS.topology.test(env.content()), 'content is still Topology');
  });

  await test('select Topology, leave, return with ?tab=topology: button and content are Topology', async () => {
    const env = pageEnv();
    await env.mount('#/analytics');
    await env.clickTab('topology');
    env.destroy();
    await env.mount('#/analytics?tab=topology');
    assert.deepStrictEqual(env.activeTabs(), ['topology'], 'active button');
    assert.ok(MARKERS.topology.test(env.content()), 'content is not Topology: ' + env.content().slice(0, 200));
  });

  await test('select Topology, then (without destroy) mount with an unknown ?tab=: Overview', async () => {
    const env = pageEnv();
    await env.mount('#/analytics');
    await env.clickTab('topology');
    await env.mount('#/analytics?tab=no-such-tab');
    assert.deepStrictEqual(env.activeTabs(), ['overview'], 'active button');
    assert.ok(MARKERS.overview.test(env.content()), 'content is not Overview: ' + env.content().slice(0, 200));
  });

  console.log('\n' + passed + ' passed, ' + failed + ' failed');
  if (failed > 0) process.exit(1);
})();
