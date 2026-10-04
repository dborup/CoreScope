/* test-analytics-subtab-deeplinks-205.js
 *
 * #205: the Scopes tab's sub-tab (Overview / Hop Depth / Regions / Hygiene)
 * and its 1h/24h/7d window, and the Wardriving tab's window, lived only in
 * sessionStorage, so a link to e.g. Hop Depth could not be shared. They are
 * now in the hash next to ?tab=:
 *
 *   sub=<overview|hopdepth|regions|hygiene>   Scopes sub-tab
 *   swin=<1h|24h|7d>                          Scopes window
 *   wdwin=<1h|24h|7d>                         Wardriving window
 *
 * (?window= stays the global analytics time picker; it has other values and
 * drives the shared loads, so the tabs get keys of their own.)
 *
 * Contract pinned here:
 * - a URL value wins over sessionStorage, and is stored for the next plain
 *   visit of the tab;
 * - without a URL value the stored value is used and written to the URL;
 * - an unknown or hostile URL value falls back to the default (never to the
 *   stored value), never throws and never reaches a selector (#193/#194);
 * - the default view keeps today's URL (no sub/swin/wdwin);
 * - clicks write the URL; switching to another tab drops these keys.
 *
 * Runs the REAL app.js api(), url-state.js and analytics page in one vm with
 * a fake fetch and a small fake DOM whose buttons come from the markup the
 * page writes.
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

// Real responses of the CI fixture server for the shared endpoints (#172).
const REAL = JSON.parse(fs.readFileSync('test-fixtures/analytics-tabs-172.json', 'utf8'));

const flush = async () => { for (let i = 0; i < 30; i++) await new Promise((r) => setImmediate(r)); };

function fakeClassList(initial) {
  const s = new Set(initial);
  return {
    add: (c) => s.add(c), remove: (c) => s.delete(c), contains: (c) => s.has(c),
    toggle: (c, on) => { if (on === undefined ? !s.has(c) : on) s.add(c); else s.delete(c); },
  };
}

const camel = (s) => s.replace(/-([a-z])/g, (_, c) => c.toUpperCase());

// A fake element whose querySelector('#id') / querySelectorAll('[data-x]')
// answer from the markup written to it, so the listeners the page attaches
// are kept on the same button objects the test clicks.
function fakeEl(id, byId) {
  let html = '';
  let listCache = {};
  const listeners = {};
  const self = {
    id, value: '', style: {}, dataset: {}, parentElement: null,
    get innerHTML() { return html; },
    set innerHTML(v) { html = String(v); listCache = {}; },
    classList: fakeClassList([]),
    addEventListener(type, fn) { (listeners[type] = listeners[type] || []).push(fn); }, removeEventListener() {},
    dispatch(type, ev) { for (const fn of listeners[type] || []) fn(ev); },
    contains: () => true, closest: () => null,
    querySelector(sel) {
      const m = /^#([\w-]+)$/.exec(sel);
      if (m && html.indexOf('id="' + m[1] + '"') >= 0) return byId(m[1]);
      return null;
    },
    querySelectorAll(sel) {
      const m = /^\[(data-[\w-]+)\]$/.exec(sel);
      if (!m) return [];
      if (!listCache[sel]) {
        const re = new RegExp('<button class="tab-btn( active)?" ' + m[1] + '="([^"]*)"', 'g');
        const out = [];
        let r;
        while ((r = re.exec(html))) {
          const b = fakeEl('', byId);
          b.dataset[camel(m[1].slice(5))] = r[2];
          b.classList = fakeClassList(r[1] ? ['active'] : []);
          b.closest = () => b;
          out.push(b);
        }
        listCache[sel] = out;
      }
      return listCache[sel];
    },
    appendChild: (c) => c, insertBefore: (c) => c, setAttribute() {}, getAttribute: () => null, removeAttribute() {},
  };
  return self;
}

function storage(initial) {
  const s = Object.assign({}, initial || {});
  return {
    data: s,
    getItem: (k) => (Object.prototype.hasOwnProperty.call(s, k) ? s[k] : null),
    setItem: (k, v) => { s[k] = String(v); },
    removeItem: (k) => { delete s[k]; },
  };
}

function pageEnv(opts) {
  opts = opts || {};
  const fetchLog = [];
  const respond = (body) => ({
    ok: true, status: 200, headers: { get: () => null },
    json: async () => JSON.parse(JSON.stringify(body)),
  });
  const fetchImpl = async (url) => {
    fetchLog.push(url);
    const m = /\/api\/analytics\/(rf|topology|channels|hash-sizes|hash-collisions)(\?|$)/.exec(url);
    const key = m && { rf: 'rfData', topology: 'topoData', channels: 'chanData', 'hash-sizes': 'hashData', 'hash-collisions': 'collisionData' }[m[1]];
    return respond(key ? REAL[key] : {});
  };

  let els = {};
  const byId = (id) => el(id);
  const app = fakeEl('app', byId);
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
  function el(id) {
    if (id === 'app') return app;
    if (!els[id]) {
      els[id] = fakeEl(id, byId);
      if (id === 'analyticsTabs') els[id].querySelectorAll = (sel) => (sel === '.tab-btn' ? tabButtons() : []);
    }
    return els[id];
  }

  const hashLog = [];
  const pages = {};
  let initError = null;
  const ctx = {
    window: { addEventListener() {}, removeEventListener() {}, dispatchEvent() {} },
    document: {
      readyState: 'complete', body: { appendChild() {} }, head: { appendChild() {} },
      createElement: () => fakeEl('', byId), getElementById: el, addEventListener() {},
      querySelectorAll: (sel) => (sel === '.tab-btn' ? tabButtons() : []), querySelector: () => null,
      documentElement: fakeEl('html', byId),
    },
    console: { log() {}, warn() {}, error() {} }, Date, Infinity, Math, Array, Object, String, Number,
    JSON, RegExp, Error, TypeError, parseInt, parseFloat, isNaN, isFinite, encodeURIComponent, decodeURIComponent,
    setTimeout: () => 0, clearTimeout() {}, setInterval: () => 0, clearInterval() {},
    requestAnimationFrame: () => 0, cancelAnimationFrame() {},
    fetch: fetchImpl, performance: { now: () => 0 },
    localStorage: storage(),
    sessionStorage: storage(opts.session),
    location: { hash: '#/analytics' },
    history: { replaceState(_s, _t, url) { hashLog.push(url); ctx.location.hash = url; } },
    CustomEvent: class CustomEvent {}, Map, Set, Promise, URLSearchParams,
    getComputedStyle: () => ({ getPropertyValue: () => '' }),
    timeAgo: () => 'x ago', initTabBar() {}, makeColumnsResizable() {},
    onWS() {}, offWS() {}, connectWS() {}, invalidateApiCache() {}, IATA_COORDS_GEO: {},
    fetchAllNodes: async () => ({ nodes: [] }),
    RegionFilter: { init() {}, onChange() {}, regionQueryString: () => '' },
    AreaFilter: { init() {}, onChange() {}, areaQueryString: () => '' },
  };
  vm.createContext(ctx);
  const load = (f) => { vm.runInContext(fs.readFileSync(f, 'utf8'), ctx); for (const k of Object.keys(ctx.window)) ctx[k] = ctx.window[k]; };
  load('public/payload-labels.js');
  load('public/roles.js');
  load('public/url-state.js');
  try { load('public/app.js'); } catch (e) { /* DOM-only tail */ }
  ctx.registerPage = (name, obj) => { pages[name] = obj; };   // app.js defines its own
  ctx.fetchAllNodes = async () => ({ nodes: [] });
  load('public/analytics.js');
  const page = pages.analytics;

  const content = () => el('analyticsContent');
  const activeOf = (attr) => {
    const re = new RegExp('<button class="tab-btn active" ' + attr + '="([^"]*)"', 'g');
    const out = [];
    let m;
    while ((m = re.exec(content().innerHTML))) out.push(m[1]);
    return out;
  };

  return {
    ctx, fetchLog, hashLog,
    session: ctx.sessionStorage.data,
    async mount(hash) {
      ctx.location.hash = hash;
      hashLog.length = 0;
      initError = null;
      page.init(app).catch((e) => { initError = e; });
      await flush();
    },
    initError: () => initError,
    destroy: () => page.destroy(),
    hash: () => ctx.location.hash,
    params: () => Object.fromEntries(new URLSearchParams(ctx.location.hash.split('?')[1] || '')),
    clickTab: async (tab) => {
      const btn = tabButtons().find((b) => b.dataset.tab === tab);
      el('analyticsTabs').dispatch('click', { target: { closest: () => btn } });
      await flush();
    },
    // Scopes: the active sub-tab button(s), the visible panel(s), the active window button(s).
    activeSubtabs: () => activeOf('data-subtab'),
    visiblePanels: () => {
      const re = /id="scopes-panel-([\w-]+)" style="display:([^"]*)"/g;
      const out = [];
      let m;
      while ((m = re.exec(content().innerHTML))) if (m[2] === '') out.push(m[1]);
      return out;
    },
    activeScopesWindows: () => activeOf('data-win'),
    activeWardrivingWindows: () => activeOf('data-wdwin'),
    clickSubtab: async (key) => {
      const bar = content().querySelector('#scopesSubtabs');
      assert.ok(bar, 'no #scopesSubtabs');
      const btn = bar.querySelectorAll('[data-subtab]').find((b) => b.dataset.subtab === key)
        || { dataset: { subtab: key }, closest() { return this; } };
      // The bar's buttons live in the content markup; the listener sits on the bar.
      bar.dispatch('click', { target: { closest: () => btn } });
      await flush();
    },
    clickScopesWindow: async (w) => {
      const btn = content().querySelectorAll('[data-win]').find((b) => b.dataset.win === w);
      assert.ok(btn, 'no [data-win="' + w + '"] button');
      btn.dispatch('click', {});
      await flush();
    },
    clickWardrivingWindow: async (w) => {
      const btn = content().querySelectorAll('[data-wdwin]').find((b) => b.dataset.wdwin === w);
      assert.ok(btn, 'no [data-wdwin="' + w + '"] button');
      btn.dispatch('click', {});
      await flush();
    },
    scopeStatsUrls: () => fetchLog.filter((u) => /\/api\/scope-stats/.test(u)),
    wardrivingUrls: () => fetchLog.filter((u) => /\/api\/analytics\/wardriving/.test(u)),
    globalWindow: () => el('analyticsTimeWindow').value,
    content: () => content().innerHTML,
    resolveViewParam: ctx._analyticsResolveViewParam,
  };
}

(async () => {
  console.log('\n=== #205: resolveViewParam(urlValue, storedValue, allowed, dflt) ===');

  await test('resolveViewParam is exposed for the test', async () => {
    assert.strictEqual(typeof pageEnv().resolveViewParam, 'function', 'window._analyticsResolveViewParam missing');
  });

  const SUBS = ['overview', 'hopdepth', 'regions', 'hygiene'];
  const RV = [
    // urlValue, storedValue, expected
    [null, null, 'overview'],
    [null, 'regions', 'regions'],
    [null, 'bogus', 'overview'],
    [null, '', 'overview'],
    ['hopdepth', null, 'hopdepth'],
    ['hopdepth', 'regions', 'hopdepth'],
    ['bogus', 'regions', 'overview'],
    ['', 'regions', 'overview'],
    ['x"]', 'regions', 'overview'],
    ['x"],[data-subtab="hygiene', null, 'overview'],
    ['__proto__', null, 'overview'],
    ['constructor', null, 'overview'],
    ['HOPDEPTH', null, 'overview'],
    [' hopdepth', null, 'overview'],
  ];
  for (const [u, s, want] of RV) {
    await test('resolveViewParam(' + JSON.stringify(u) + ', ' + JSON.stringify(s) + ') = ' + JSON.stringify(want), async () => {
      assert.strictEqual(pageEnv().resolveViewParam(u, s, SUBS, 'overview'), want);
    });
  }

  console.log('\n=== #205: Scopes sub-tab from the URL ===');

  for (const sub of SUBS) {
    await test('#/analytics?tab=scopes&sub=' + sub + ' opens ' + sub, async () => {
      const env = pageEnv();
      await env.mount('#/analytics?tab=scopes&sub=' + sub);
      assert.strictEqual(env.initError(), null, 'init() threw');
      assert.deepStrictEqual(env.activeSubtabs(), [sub], 'active sub-tab button');
      assert.deepStrictEqual(env.visiblePanels(), [sub], 'visible panel');
    });
  }

  await test('the URL sub-tab wins over sessionStorage and is stored', async () => {
    const env = pageEnv({ session: { scopes_subtab: 'regions' } });
    await env.mount('#/analytics?tab=scopes&sub=hygiene');
    assert.deepStrictEqual(env.activeSubtabs(), ['hygiene']);
    assert.deepStrictEqual(env.visiblePanels(), ['hygiene']);
    assert.strictEqual(env.session.scopes_subtab, 'hygiene', 'sessionStorage not updated');
    assert.strictEqual(env.params().sub, 'hygiene', 'sub= lost');
  });

  await test('without sub=, the stored sub-tab opens and is written to the URL', async () => {
    const env = pageEnv({ session: { scopes_subtab: 'regions' } });
    await env.mount('#/analytics?tab=scopes');
    assert.deepStrictEqual(env.activeSubtabs(), ['regions']);
    assert.strictEqual(env.hash(), '#/analytics?tab=scopes&sub=regions');
  });

  const hostileSubs = ['x"', 'x"],[data-subtab="hygiene', 'hopdepth"]', '\\', '', '__proto__', 'constructor', 'toString', 'Overview', '<img src=x onerror=alert(1)>'];
  for (const value of hostileSubs) {
    await test('?sub=' + JSON.stringify(value) + ' falls back to Overview (not the stored sub-tab), no exception', async () => {
      const env = pageEnv({ session: { scopes_subtab: 'regions' } });
      await env.mount('#/analytics?tab=scopes&sub=' + encodeURIComponent(value));
      assert.strictEqual(env.initError(), null, 'init() threw: ' + (env.initError() && env.initError().message));
      assert.deepStrictEqual(env.activeSubtabs(), ['overview'], 'active sub-tab button');
      assert.deepStrictEqual(env.visiblePanels(), ['overview'], 'visible panel');
      assert.strictEqual(env.hash(), '#/analytics?tab=scopes', 'URL not canonical');
      assert.ok(env.content().indexOf('onerror') < 0, 'URL value reached the markup');
    });
  }

  await test('a garbage stored sub-tab falls back to Overview', async () => {
    const env = pageEnv({ session: { scopes_subtab: 'x"]' } });
    await env.mount('#/analytics?tab=scopes');
    assert.deepStrictEqual(env.activeSubtabs(), ['overview']);
    assert.deepStrictEqual(env.visiblePanels(), ['overview']);
    assert.strictEqual(env.hash(), '#/analytics?tab=scopes');
  });

  console.log('\n=== #205: Scopes window from the URL (swin=) ===');

  for (const w of ['1h', '24h', '7d']) {
    await test('#/analytics?tab=scopes&swin=' + w + ' selects ' + w + ' and loads scope-stats?window=' + w, async () => {
      const env = pageEnv();
      await env.mount('#/analytics?tab=scopes&swin=' + w);
      assert.deepStrictEqual(env.activeScopesWindows(), [w, w], 'active window buttons (Overview + Hop Depth copies)');
      assert.deepStrictEqual(env.scopeStatsUrls(), ['/api/scope-stats?window=' + w]);
    });
  }

  await test('the URL window wins over sessionStorage and is stored', async () => {
    const env = pageEnv({ session: { scopes_window: '1h' } });
    await env.mount('#/analytics?tab=scopes&swin=7d');
    assert.deepStrictEqual(env.scopeStatsUrls(), ['/api/scope-stats?window=7d']);
    assert.strictEqual(env.session.scopes_window, '7d');
  });

  await test('without swin=, the stored window is used and written to the URL', async () => {
    const env = pageEnv({ session: { scopes_window: '1h' } });
    await env.mount('#/analytics?tab=scopes');
    assert.deepStrictEqual(env.scopeStatsUrls(), ['/api/scope-stats?window=1h']);
    assert.strictEqual(env.hash(), '#/analytics?tab=scopes&swin=1h');
  });

  for (const value of ['30d', '', 'x"]', '__proto__', '24H', '1h&sub=hygiene']) {
    await test('?swin=' + JSON.stringify(value) + ' falls back to 24h, no exception', async () => {
      const env = pageEnv({ session: { scopes_window: '1h' } });
      await env.mount('#/analytics?tab=scopes&swin=' + encodeURIComponent(value));
      assert.strictEqual(env.initError(), null, 'init() threw');
      assert.deepStrictEqual(env.activeScopesWindows(), ['24h', '24h']);
      assert.deepStrictEqual(env.scopeStatsUrls(), ['/api/scope-stats?window=24h']);
      assert.strictEqual(env.hash(), '#/analytics?tab=scopes');
    });
  }

  // Writing one key rebuilds the hash and drops an empty key the next read
  // would still see, so every key of the tab is read before any is written.
  for (const hash of ['#/analytics?tab=scopes&swin=&sub=hopdepth', '#/analytics?tab=scopes&sub=hopdepth&swin=', '#/analytics?tab=scopes&swin=7d&sub=']) {
    await test(hash + ': an empty key does not disturb the other one', async () => {
      const env = pageEnv({ session: { scopes_subtab: 'regions', scopes_window: '1h' } });
      await env.mount(hash);
      const q = new URLSearchParams(hash.split('?')[1]);
      const wantSub = q.get('sub') || 'overview', wantWin = q.get('swin') || '24h';
      assert.deepStrictEqual(env.activeSubtabs(), [wantSub], 'sub-tab');
      assert.deepStrictEqual(env.scopeStatsUrls(), ['/api/scope-stats?window=' + wantWin], 'window');
    });
  }

  await test('?window= stays the global picker: window=7d&swin=1h sets each on its own', async () => {
    const env = pageEnv();
    await env.mount('#/analytics?tab=scopes&window=7d&swin=1h');
    assert.strictEqual(env.globalWindow(), '7d', 'global picker');
    assert.deepStrictEqual(env.scopeStatsUrls(), ['/api/scope-stats?window=1h']);
    assert.deepStrictEqual(env.params(), { tab: 'scopes', window: '7d', swin: '1h' });
  });

  console.log('\n=== #205: default URLs do not change ===');

  for (const hash of ['#/analytics?tab=scopes', '#/analytics?tab=wardriving', '#/analytics', '#/analytics?tab=topology&window=7d']) {
    await test(hash + ' with nothing stored is left as it is', async () => {
      const env = pageEnv();
      await env.mount(hash);
      assert.strictEqual(env.hash(), hash);
      assert.deepStrictEqual(env.hashLog.filter((h) => h !== hash), [], 'URL rewritten');
    });
  }

  await test('the explicit defaults (sub=overview&swin=24h) are dropped from the URL', async () => {
    const env = pageEnv();
    await env.mount('#/analytics?tab=scopes&sub=overview&swin=24h');
    assert.deepStrictEqual(env.activeSubtabs(), ['overview']);
    assert.strictEqual(env.hash(), '#/analytics?tab=scopes');
  });

  console.log('\n=== #205: clicks write the URL ===');

  await test('clicking a sub-tab writes sub=, clicking Overview drops it', async () => {
    const env = pageEnv();
    await env.mount('#/analytics?tab=scopes');
    await env.clickSubtab('hopdepth');
    assert.strictEqual(env.hash(), '#/analytics?tab=scopes&sub=hopdepth');
    assert.strictEqual(env.session.scopes_subtab, 'hopdepth');
    await env.clickSubtab('regions');
    assert.strictEqual(env.hash(), '#/analytics?tab=scopes&sub=regions');
    await env.clickSubtab('overview');
    assert.strictEqual(env.hash(), '#/analytics?tab=scopes');
  });

  await test('clicking a window writes swin=, clicking 24h drops it; sub= is kept', async () => {
    const env = pageEnv();
    await env.mount('#/analytics?tab=scopes&sub=hopdepth');
    await env.clickScopesWindow('7d');
    assert.strictEqual(env.hash(), '#/analytics?tab=scopes&sub=hopdepth&swin=7d');
    assert.strictEqual(env.session.scopes_window, '7d');
    assert.ok(env.scopeStatsUrls().includes('/api/scope-stats?window=7d'), env.scopeStatsUrls().join(', '));
    await env.clickScopesWindow('24h');
    assert.strictEqual(env.hash(), '#/analytics?tab=scopes&sub=hopdepth');
  });

  await test('a click, leave and a remount from the written URL (reload / back / forward) restores the view', async () => {
    const env = pageEnv();
    await env.mount('#/analytics?tab=scopes');
    await env.clickSubtab('hygiene');
    await env.clickScopesWindow('1h');
    const url = env.hash();
    env.destroy();
    await env.mount('#/packets');
    const env2 = pageEnv();   // fresh document, nothing in sessionStorage
    await env2.mount(url);
    assert.deepStrictEqual(env2.activeSubtabs(), ['hygiene']);
    assert.deepStrictEqual(env2.visiblePanels(), ['hygiene']);
    assert.deepStrictEqual(env2.scopeStatsUrls(), ['/api/scope-stats?window=1h']);
    assert.strictEqual(env2.hash(), url);
  });

  await test('switching from Scopes to another tab drops sub= and swin=, keeps window=', async () => {
    const env = pageEnv();
    await env.mount('#/analytics?tab=scopes&sub=hopdepth&swin=7d&window=24h');
    await env.clickTab('topology');
    assert.strictEqual(env.hash(), '#/analytics?tab=topology&window=24h');
  });

  await test('switching back to Scopes restores the stored view into the URL', async () => {
    const env = pageEnv();
    await env.mount('#/analytics?tab=scopes&sub=hopdepth&swin=7d');
    await env.clickTab('topology');
    await env.clickTab('scopes');
    assert.deepStrictEqual(env.params(), { tab: 'scopes', sub: 'hopdepth', swin: '7d' });
  });

  await test('rf-health keys are still dropped when leaving rf-health (unchanged)', async () => {
    const env = pageEnv();
    await env.mount('#/analytics?tab=rf-health&range=7d&observer=X');
    await env.clickTab('scopes');
    assert.strictEqual(env.hash(), '#/analytics?tab=scopes');
  });

  console.log('\n=== #205: Wardriving window (wdwin=) ===');

  for (const w of ['1h', '24h', '7d']) {
    await test('#/analytics?tab=wardriving&wdwin=' + w + ' selects ' + w, async () => {
      const env = pageEnv();
      await env.mount('#/analytics?tab=wardriving&wdwin=' + w);
      assert.deepStrictEqual(env.activeWardrivingWindows(), [w]);
      assert.ok(env.wardrivingUrls().length > 0 && env.wardrivingUrls().every((u) => u.indexOf('window=' + w) >= 0), env.wardrivingUrls().join(', '));
    });
  }

  await test('the URL wardriving window wins over sessionStorage', async () => {
    const env = pageEnv({ session: { wardriving_window: '1h' } });
    await env.mount('#/analytics?tab=wardriving&wdwin=7d');
    assert.deepStrictEqual(env.activeWardrivingWindows(), ['7d']);
    assert.strictEqual(env.session.wardriving_window, '7d');
  });

  await test('without wdwin=, the stored wardriving window is written to the URL', async () => {
    const env = pageEnv({ session: { wardriving_window: '1h' } });
    await env.mount('#/analytics?tab=wardriving');
    assert.deepStrictEqual(env.activeWardrivingWindows(), ['1h']);
    assert.strictEqual(env.hash(), '#/analytics?tab=wardriving&wdwin=1h');
  });

  for (const value of ['30d', 'x"]', '__proto__', '']) {
    await test('?wdwin=' + JSON.stringify(value) + ' falls back to 24h, no exception', async () => {
      const env = pageEnv({ session: { wardriving_window: '1h' } });
      await env.mount('#/analytics?tab=wardriving&wdwin=' + encodeURIComponent(value));
      assert.strictEqual(env.initError(), null, 'init() threw');
      assert.deepStrictEqual(env.activeWardrivingWindows(), ['24h']);
      assert.strictEqual(env.hash(), '#/analytics?tab=wardriving');
    });
  }

  await test('clicking a wardriving window writes wdwin=, 24h drops it; leaving the tab drops it', async () => {
    const env = pageEnv();
    await env.mount('#/analytics?tab=wardriving');
    await env.clickWardrivingWindow('7d');
    assert.strictEqual(env.hash(), '#/analytics?tab=wardriving&wdwin=7d');
    await env.clickWardrivingWindow('24h');
    assert.strictEqual(env.hash(), '#/analytics?tab=wardriving');
    await env.clickWardrivingWindow('1h');
    await env.clickTab('scopes');
    assert.strictEqual(env.hash(), '#/analytics?tab=scopes');
  });

  await test('Scopes and Wardriving windows are independent', async () => {
    const env = pageEnv();
    await env.mount('#/analytics?tab=scopes&swin=7d');
    await env.clickTab('wardriving');
    assert.deepStrictEqual(env.activeWardrivingWindows(), ['24h']);
    assert.strictEqual(env.hash(), '#/analytics?tab=wardriving');
  });

  console.log('\n' + passed + ' passed, ' + failed + ' failed');
  if (failed > 0) process.exit(1);
})();
