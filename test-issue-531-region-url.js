/**
 * #531: a link's ?region= sets the region selection on every view, and it is
 * applied before the view's first region-dependent fetch.
 *
 *   - RegionFilter.syncFromUrl(hash) parses the hash-query `region`
 *     parameter: codes are trimmed, empty entries and repeats are dropped,
 *     and once regions are loaded unknown codes are dropped and the rest take
 *     the configured spelling (the server matches codes case-insensitively).
 *   - A link with codes overrides the saved selection and is saved like a
 *     click. No parameter, an empty one or only commas leave the saved
 *     selection as it is, the same as before.
 *   - Listeners hear a genuine change once; an unchanged selection notifies
 *     nobody.
 *   - On a fresh load the codes are checked when regions load. A link whose
 *     codes are all unknown then falls back to all regions and listeners are
 *     told once; a saved selection with no link keeps today's silent cleanup.
 *   - app.js's router calls syncFromUrl after the old page's destroy() and
 *     before the new page's init().
 *   - "All" in any case is the server's catch-all (#770): a link with only
 *     that selects all regions with no request carrying it; next to real
 *     codes it is dropped.
 *   - A change in the control keeps a `region` parameter already in the hash
 *     in step with the selection (replaceState, other parameters and the
 *     route kept; removed for all regions) before listeners hear of it, so a
 *     stale link value cannot override the choice on Back or reload. A hash
 *     without one is left alone.
 *
 * Usage: node test-issue-531-region-url.js
 */
'use strict';
const assert = require('assert');
const fs = require('fs');
const vm = require('vm');

let passed = 0, failed = 0;
const queue = [];
function test(name, fn) { queue.push({ name, fn }); }
function section(title) { queue.push({ title }); }

const REGIONS = { SJC: 'San Jose', SFO: 'San Francisco', OAK: 'Oakland', MRY: 'Monterey' };

// The real public/region-filter.js in a sandbox. `stored` pre-seeds the saved
// selection. Returns the API, the store and a call log for one listener.
function load(opts) {
  opts = opts || {};
  const store = {};
  if (opts.stored) store['meshcore-region-filter'] = JSON.stringify(opts.stored);
  const session = Object.assign({}, opts.session);
  let fetches = 0;
  const replaced = [];
  const ctx = {
    window: {},
    document: { addEventListener() {}, removeEventListener() {} },
    location: { hash: opts.hash || '' },
    history: {
      state: opts.state === undefined ? null : opts.state,
      // As in a browser: the state is stored as given, and a missing URL
      // keeps the current one. `replaced` logs URL rewrites only.
      replaceState(state, title, url) {
        ctx.history.state = state === undefined ? null : JSON.parse(JSON.stringify(state));
        if (url !== undefined) { replaced.push(url); ctx.location.hash = url; }
      },
      pushState() { throw new Error('pushState must not be used'); },
    },
    URLSearchParams,
    localStorage: {
      getItem: (k) => (k in store ? store[k] : null),
      setItem: (k, v) => { store[k] = String(v); },
      removeItem: (k) => { delete store[k]; },
    },
    sessionStorage: {
      getItem: (k) => (k in session ? session[k] : null),
      setItem: (k, v) => { session[k] = String(v); },
      removeItem: (k) => { delete session[k]; },
    },
    fetch: async () => { fetches++; return { json: async () => REGIONS }; },
    escapeHtml: (s) => String(s),
    console, setTimeout, clearTimeout,
  };
  ctx.window = ctx;
  vm.createContext(ctx);
  vm.runInContext(fs.readFileSync(__dirname + '/public/region-filter.js', 'utf8'), ctx, { filename: 'region-filter.js' });
  vm.runInContext(fs.readFileSync(__dirname + '/public/url-state.js', 'utf8'), ctx, { filename: 'url-state.js' });
  const RF = ctx.RegionFilter;
  const calls = [];
  const hashAtNotify = [];
  RF.onChange((sel) => { calls.push(sel ? Array.from(sel) : null); hashAtNotify.push(ctx.location.hash); });
  const container = { innerHTML: '', style: {}, onclick: null, querySelector: () => ({ setAttribute() {} }) };
  return { RF, store, session, calls, container, ctx, replaced, hashAtNotify, fetches: () => fetches };
}
// A click on a region pill in `container` (the pill bar: four regions).
const click = (container, code) => container.onclick({ target: { closest: () => ({ dataset: { region: code } }) } });
const hashParams = (hash) => new URLSearchParams(String(hash).split('?')[1] || '');
const saved = (store) => (store['meshcore-region-filter'] ? JSON.parse(store['meshcore-region-filter']) : null);
const sync = (RF, hash) => { if (typeof RF.syncFromUrl !== 'function') throw new Error('RegionFilter.syncFromUrl is missing'); return RF.syncFromUrl(hash); };

section('\n=== #531 RegionFilter.syncFromUrl: parsing and precedence ===');

test('a link region overrides the saved selection and is saved', async () => {
  const t = load({ stored: ['SFO'] });
  await t.RF.init(t.container);
  sync(t.RF, '#/nodes?region=SJC');
  assert.deepStrictEqual(Array.from(t.RF.getSelected()), ['SJC']);
  assert.strictEqual(t.RF.regionQueryString(), '&region=SJC');
  assert.deepStrictEqual(saved(t.store), ['SJC']);
});

test('no query, no region parameter: the saved selection stands', async () => {
  for (const hash of ['', '#/nodes', '#/nodes?', '#/nodes?tab=x', '#/nodes?regions=SJC']) {
    const t = load({ stored: ['SFO'] });
    await t.RF.init(t.container);
    assert.strictEqual(sync(t.RF, hash), false, hash);
    assert.deepStrictEqual(Array.from(t.RF.getSelected()), ['SFO'], hash);
    assert.deepStrictEqual(saved(t.store), ['SFO'], hash);
  }
});

test('an empty parameter or only commas/spaces: the saved selection stands', async () => {
  for (const hash of ['#/live?region=', '#/live?region', '#/live?region=,,', '#/live?region=%20,%20']) {
    const t = load({ stored: ['SFO'] });
    await t.RF.init(t.container);
    assert.strictEqual(sync(t.RF, hash), false, hash);
    assert.deepStrictEqual(Array.from(t.RF.getSelected()), ['SFO'], hash);
  }
});

test('an empty parameter with nothing saved keeps all regions', async () => {
  const t = load();
  await t.RF.init(t.container);
  sync(t.RF, '#/live?region=');
  assert.strictEqual(t.RF.getSelected(), null);
  assert.strictEqual(saved(t.store), null);
});

test('comma list: trimmed, empty entries and repeats dropped, order kept', async () => {
  const t = load();
  await t.RF.init(t.container);
  sync(t.RF, '#/packets?region=%20SJC%20,,OAK,SJC');
  assert.deepStrictEqual(Array.from(t.RF.getSelected()), ['SJC', 'OAK']);
});

test('an encoded comma (as packets writes it) splits like a plain one', async () => {
  const t = load();
  await t.RF.init(t.container);
  sync(t.RF, '#/packets?timeWindow=60&region=SJC%2COAK&hash=ab');
  assert.deepStrictEqual(Array.from(t.RF.getSelected()), ['SJC', 'OAK']);
});

test('the first region parameter wins', async () => {
  const t = load();
  await t.RF.init(t.container);
  sync(t.RF, '#/nodes?region=OAK&region=SJC');
  assert.deepStrictEqual(Array.from(t.RF.getSelected()), ['OAK']);
});

test('a malformed escape is kept literally instead of throwing', async () => {
  const t = load();
  sync(t.RF, '#/nodes?region=SJC%E0');
  assert.deepStrictEqual(Array.from(t.RF.getSelected()), ['SJC%E0']);
});

test('regions loaded: unknown codes are dropped', async () => {
  const t = load();
  await t.RF.init(t.container);
  sync(t.RF, '#/nodes?region=SJC,XXX');
  assert.deepStrictEqual(Array.from(t.RF.getSelected()), ['SJC']);
  assert.deepStrictEqual(saved(t.store), ['SJC']);
});

test('regions loaded: only unknown codes fall back to all regions, nothing saved', async () => {
  const t = load({ stored: ['SFO'] });
  await t.RF.init(t.container);
  sync(t.RF, '#/nodes?region=XXX');
  assert.strictEqual(t.RF.getSelected(), null);
  assert.strictEqual(t.RF.regionQueryString(), '');
  assert.strictEqual(saved(t.store), null);
});

test('regions loaded: codes match case-insensitively and take the configured spelling', async () => {
  const t = load();
  await t.RF.init(t.container);
  sync(t.RF, '#/nodes?region=sjc,Oak,SJC');
  assert.deepStrictEqual(Array.from(t.RF.getSelected()), ['SJC', 'OAK']);
});

test('fresh load: the link is the selection before init() and before any fetch', async () => {
  const t = load({ stored: ['SFO'] });
  sync(t.RF, '#/live?region=SJC,OAK');
  assert.strictEqual(t.fetches(), 0, 'syncFromUrl must not fetch');
  assert.strictEqual(t.RF.getRegionParam(), 'SJC,OAK');
  await t.RF.init(t.container);
  assert.strictEqual(t.RF.getRegionParam(), 'SJC,OAK');
});

test('fresh load: repeats differing only in case are dropped before the first request', async () => {
  const t = load();
  sync(t.RF, '#/live?region=SJC,sjc,%20Sjc');
  assert.strictEqual(t.RF.regionQueryString(), '&region=SJC');
});

test('fresh load: unknown codes are dropped when regions load', async () => {
  const t = load();
  sync(t.RF, '#/nodes?region=SJC,XXX');
  await t.RF.init(t.container);
  assert.deepStrictEqual(Array.from(t.RF.getSelected()), ['SJC']);
  assert.deepStrictEqual(saved(t.store), ['SJC']);
});

section('\n=== #531 RegionFilter.syncFromUrl: notifications ===');

test('a genuine change notifies listeners exactly once', async () => {
  const t = load({ stored: ['SFO'] });
  await t.RF.init(t.container);
  assert.strictEqual(sync(t.RF, '#/nodes?region=SJC'), true);
  assert.deepStrictEqual(t.calls, [['SJC']]);
});

test('an unchanged selection (any order) notifies nobody', async () => {
  const t = load({ stored: ['SJC', 'OAK'] });
  await t.RF.init(t.container);
  assert.strictEqual(sync(t.RF, '#/nodes?region=OAK,SJC'), false);
  assert.strictEqual(sync(t.RF, '#/nodes?region=SJC,OAK,SJC'), false);
  assert.deepStrictEqual(t.calls, []);
});

test('repeating the same link notifies once, not twice', async () => {
  const t = load();
  await t.RF.init(t.container);
  sync(t.RF, '#/nodes?region=SJC');
  sync(t.RF, '#/packets?region=SJC');
  assert.deepStrictEqual(t.calls, [['SJC']]);
});

test('only unknown codes with all regions already selected is a no-op', async () => {
  const t = load();
  await t.RF.init(t.container);
  assert.strictEqual(sync(t.RF, '#/nodes?region=XXX'), false);
  assert.deepStrictEqual(t.calls, []);
});

test('a removed listener is not notified', async () => {
  const t = load();
  await t.RF.init(t.container);
  let hits = 0;
  const h = t.RF.onChange(() => { hits++; });
  t.RF.offChange(h);
  sync(t.RF, '#/nodes?region=SJC');
  assert.strictEqual(hits, 0);
});

test('fresh load: link with only unknown codes falls back to all and notifies once when regions load', async () => {
  const t = load({ stored: ['SFO'] });
  sync(t.RF, '#/live?region=XXX');
  t.calls.length = 0; // the link itself changed SFO -> XXX
  await t.RF.init(t.container);
  assert.strictEqual(t.RF.getSelected(), null);
  assert.deepStrictEqual(t.calls, [null]);
  await t.RF.init(t.container);
  assert.deepStrictEqual(t.calls, [null], 'a later init() must not notify again');
});

test('fresh load: dropping some unknown codes does not notify (the request already matched the same observers)', async () => {
  const t = load();
  sync(t.RF, '#/nodes?region=SJC,XXX');
  t.calls.length = 0;
  await t.RF.init(t.container);
  assert.deepStrictEqual(t.calls, []);
});

test('fresh load: a valid link does not notify when regions load', async () => {
  const t = load();
  sync(t.RF, '#/nodes?region=SJC');
  t.calls.length = 0;
  await t.RF.init(t.container);
  assert.deepStrictEqual(t.calls, []);
});

test('no link: a saved selection whose codes are gone is cleaned up silently, as before', async () => {
  const t = load({ stored: ['XXX'] });
  sync(t.RF, '#/nodes');
  await t.RF.init(t.container);
  assert.strictEqual(t.RF.getSelected(), null);
  assert.deepStrictEqual(t.calls, []);
});

test('a click in the control still notifies once', async () => {
  const t = load();
  const container = {
    innerHTML: '', style: {}, onclick: null,
    querySelector: () => ({ setAttribute() {} }),
  };
  await t.RF.init(container);
  container.onclick({ target: { closest: () => ({ dataset: { region: 'SJC' } }) } });
  assert.deepStrictEqual(t.calls, [['SJC']]);
});

section('\n=== #531 ?region=All (the server\'s #770 sentinel) ===');

test('?region=All in any case means all regions: the saved selection is cleared, listeners told once', async () => {
  for (const v of ['All', 'ALL', 'all', '%20aLl%20', 'All,all']) {
    const t = load({ stored: ['SFO'] });
    await t.RF.init(t.container);
    assert.strictEqual(sync(t.RF, '#/nodes?region=' + v), true, v);
    assert.strictEqual(t.RF.getSelected(), null, v);
    assert.strictEqual(t.RF.regionQueryString(), '', v);
    assert.strictEqual(saved(t.store), null, v);
    assert.deepStrictEqual(t.calls, [null], v);
  }
});

test('fresh load: ?region=All in any case sends no region and notifies nobody when regions load', async () => {
  for (const v of ['All', 'ALL', 'all', '%20aLl%20']) {
    const t = load({ stored: ['SFO'] });
    sync(t.RF, '#/nodes?region=' + v);
    assert.strictEqual(t.RF.regionQueryString(), '', v + ': before regions load');
    t.calls.length = 0;
    await t.RF.init(t.container);
    assert.strictEqual(t.RF.getSelected(), null, v);
    assert.deepStrictEqual(t.calls, [], v + ': no second load after regions load');
  }
});

test('?region=All with all regions already selected is a no-op', async () => {
  const t = load();
  await t.RF.init(t.container);
  assert.strictEqual(sync(t.RF, '#/nodes?region=all'), false);
  assert.deepStrictEqual(t.calls, []);
});

test('All next to real codes is dropped, as the server does', async () => {
  const t = load({ stored: ['SFO'] });
  sync(t.RF, '#/nodes?region=SJC,All,OAK');
  assert.strictEqual(t.RF.regionQueryString(), '&region=' + encodeURIComponent('SJC,OAK'));
});

section('\n=== #531 a change in the control keeps the hash ?region= in step ===');

test('a change rewrites the hash region with replaceState and keeps the other parameters', async () => {
  const t = load({ stored: ['SFO'], hash: '#/analytics?tab=topology&region=SJC&window=24h' });
  sync(t.RF, t.ctx.location.hash);
  await t.RF.init(t.container);
  click(t.container, 'OAK');
  const p = hashParams(t.ctx.location.hash);
  assert.ok(t.ctx.location.hash.startsWith('#/analytics?'), t.ctx.location.hash);
  assert.strictEqual(p.get('region'), 'SJC,OAK');
  assert.strictEqual(p.get('tab'), 'topology');
  assert.strictEqual(p.get('window'), '24h');
  assert.strictEqual(t.replaced.length, 1, 'one replaceState');
  click(t.container, 'SJC');
  assert.strictEqual(hashParams(t.ctx.location.hash).get('region'), 'OAK');
  // A reload reads the hash again: the choice stands.
  assert.strictEqual(sync(t.RF, t.ctx.location.hash), false);
  assert.deepStrictEqual(Array.from(t.RF.getSelected()), ['OAK']);
});

test('a change keeps the route subpath', async () => {
  const t = load({ hash: '#/nodes/abc%20def?region=SJC' });
  sync(t.RF, t.ctx.location.hash);
  await t.RF.init(t.container);
  click(t.container, 'OAK');
  assert.strictEqual(t.ctx.location.hash.split('?')[0], '#/nodes/abc%20def');
  assert.strictEqual(hashParams(t.ctx.location.hash).get('region'), 'SJC,OAK');
});

test('a change to all regions removes region from the hash', async () => {
  const t = load({ stored: ['SFO'], hash: '#/live?node=abc&region=SJC' });
  sync(t.RF, t.ctx.location.hash);
  await t.RF.init(t.container);
  click(t.container, '__all__');
  assert.strictEqual(t.ctx.location.hash, '#/live?node=abc');
  assert.strictEqual(sync(t.RF, t.ctx.location.hash), false, 'a reload keeps all regions');
  assert.strictEqual(t.RF.getSelected(), null);
});

test('repeated, empty or unknown-only region parameters are replaced too', async () => {
  for (const hash of ['#/nodes?region=SJC&region=MRY', '#/nodes?region=', '#/nodes?region=XXX']) {
    const t = load({ hash });
    sync(t.RF, hash);
    await t.RF.init(t.container);
    click(t.container, 'OAK');
    assert.deepStrictEqual(hashParams(t.ctx.location.hash).getAll('region'), [t.RF.getRegionParam()], hash);
  }
});

test('listeners see the updated hash', async () => {
  const t = load({ hash: '#/nodes?region=SJC' });
  sync(t.RF, t.ctx.location.hash);
  await t.RF.init(t.container);
  t.hashAtNotify.length = 0;
  click(t.container, 'OAK');
  assert.deepStrictEqual(t.hashAtNotify.map((h) => hashParams(h).get('region')), ['SJC,OAK']);
});

test('a hash without a region parameter is left alone', async () => {
  for (const hash of ['#/nodes', '#/channels/abc', '#/analytics?tab=topology']) {
    const t = load({ hash });
    await t.RF.init(t.container);
    click(t.container, 'OAK');
    assert.strictEqual(t.ctx.location.hash, hash);
    assert.deepStrictEqual(t.replaced, [], hash);
  }
});

test('#556: a change keeps the entry\'s history.state', async () => {
  const t = load({ hash: '#/analytics?tab=topology&region=SJC', state: { analyticsView: { tab: 'topology' } } });
  sync(t.RF, t.ctx.location.hash);
  await t.RF.init(t.container);
  click(t.container, 'OAK');
  assert.strictEqual(hashParams(t.ctx.location.hash).get('region'), 'SJC,OAK');
  assert.ok(t.ctx.history.state, 'history.state was dropped');
  assert.deepStrictEqual(t.ctx.history.state.analyticsView, { tab: 'topology' });
});

section('\n=== #556 Back/Forward: a link\'s region does not override a later choice ===');

// The router's sync on entering a history entry: `state` is that entry's
// history.state (null for a new entry, the saved object on Back/Forward).
function enter(t, hash, state) {
  t.ctx.location.hash = hash;
  t.ctx.history.state = state === undefined ? null : state;
  return sync(t.RF, hash);
}
const entryState = (t) => JSON.parse(JSON.stringify(t.ctx.history.state));

test('link, other entry, choice in the control, Back to the link: the choice stands', async () => {
  const t = load({ stored: ['SFO'] });
  await t.RF.init(t.container);
  assert.strictEqual(enter(t, '#/nodes?region=SJC'), true, 'the link applies on a new entry');
  const linkEntry = entryState(t);
  enter(t, '#/channels');
  click(t.container, 'OAK');
  click(t.container, 'SJC');
  assert.deepStrictEqual(Array.from(t.RF.getSelected()), ['OAK']);
  assert.strictEqual(enter(t, '#/nodes?region=SJC', linkEntry), false, 'Back must not reapply SJC');
  assert.deepStrictEqual(Array.from(t.RF.getSelected()), ['OAK']);
  assert.deepStrictEqual(saved(t.store), ['OAK']);
  // Forward and Back again, and a reload of the entry, keep it.
  enter(t, '#/channels', null);
  assert.strictEqual(enter(t, '#/nodes?region=SJC', linkEntry), false);
  assert.deepStrictEqual(Array.from(t.RF.getSelected()), ['OAK']);
});

test('the choice survives a reload of the tab (it is kept per tab)', async () => {
  const t = load({ stored: ['SFO'] });
  await t.RF.init(t.container);
  enter(t, '#/nodes?region=SJC');
  const linkEntry = entryState(t);
  enter(t, '#/channels');
  click(t.container, 'OAK');
  click(t.container, 'SJC');
  // Reload on #/channels, then Back to the link's entry.
  const r = load({ stored: saved(t.store), session: t.session });
  await r.RF.init(r.container);
  assert.strictEqual(enter(r, '#/nodes?region=SJC', linkEntry), false);
  assert.deepStrictEqual(Array.from(r.RF.getSelected()), ['OAK']);
});

test('Back and Forward between links with no choice in between still apply each link', async () => {
  const t = load({ stored: ['SFO'] });
  await t.RF.init(t.container);
  enter(t, '#/nodes?region=SJC');
  const first = entryState(t);
  enter(t, '#/channels?region=OAK');
  const second = entryState(t);
  assert.strictEqual(enter(t, '#/nodes?region=SJC', first), true, 'Back');
  assert.deepStrictEqual(Array.from(t.RF.getSelected()), ['SJC']);
  assert.strictEqual(enter(t, '#/channels?region=OAK', second), true, 'Forward');
  assert.deepStrictEqual(Array.from(t.RF.getSelected()), ['OAK']);
});

test('a link on a new entry wins after earlier choices; so does a first load', async () => {
  const t = load({ stored: ['SFO'] });
  await t.RF.init(t.container);
  click(t.container, 'OAK');
  assert.strictEqual(enter(t, '#/nodes?region=MRY'), true, 'a new entry after a choice');
  assert.deepStrictEqual(Array.from(t.RF.getSelected()), ['MRY']);
  const fresh = load({ stored: ['SFO'], session: t.session, hash: '#/nodes?region=SJC' });
  assert.strictEqual(sync(fresh.RF, fresh.ctx.location.hash), true, 'deep link on first load');
  assert.deepStrictEqual(Array.from(fresh.RF.getSelected()), ['SJC']);
});

test('the mark keeps the entry\'s other history.state keys and leaves the URL alone', async () => {
  const t = load();
  await t.RF.init(t.container);
  enter(t, '#/analytics?tab=topology&region=SJC', { analyticsView: { tab: 'topology' } });
  assert.deepStrictEqual(t.ctx.history.state.analyticsView, { tab: 'topology' });
  assert.strictEqual(t.ctx.location.hash, '#/analytics?tab=topology&region=SJC');
  assert.deepStrictEqual(t.replaced, []);
});

test('Clear in Packets (setSelected) counts as a choice', async () => {
  const t = load({ stored: ['SFO'] });
  await t.RF.init(t.container);
  enter(t, '#/nodes?region=SJC');
  const linkEntry = entryState(t);
  enter(t, '#/packets');
  t.RF.setSelected([]);
  assert.strictEqual(enter(t, '#/nodes?region=SJC', linkEntry), false);
  assert.strictEqual(t.RF.getSelected(), null);
});

test('storage blocked: Back still keeps a choice made in this page', async () => {
  const t = load({ stored: ['SFO'] });
  t.ctx.sessionStorage.getItem = () => { throw new Error('blocked'); };
  t.ctx.sessionStorage.setItem = () => { throw new Error('blocked'); };
  await t.RF.init(t.container);
  enter(t, '#/nodes?region=SJC');
  const linkEntry = entryState(t);
  enter(t, '#/channels');
  click(t.container, 'OAK');
  click(t.container, 'SJC');
  assert.strictEqual(enter(t, '#/nodes?region=SJC', linkEntry), false);
  assert.deepStrictEqual(Array.from(t.RF.getSelected()), ['OAK']);
});

section('\n=== #531 app.js router: sync between destroy() and init() ===');

function loadApp() {
  const order = [];
  const el = () => ({
    classList: { add() {}, remove() {}, toggle() {}, contains: () => false },
    setAttribute() {}, removeAttribute() {}, focus() {}, querySelector: () => null,
    querySelectorAll: () => [], appendChild() {}, style: {}, innerHTML: '', offsetWidth: 0,
  });
  const app = el();
  const ctx = {
    console, Date, Math, JSON, Promise, Set, Map,
    setTimeout: () => 0, clearTimeout: () => {}, setInterval: () => 0, clearInterval: () => {},
    requestAnimationFrame: () => 0,
    performance: { now: () => 0 },
    location: { hash: '#/packets', pathname: '/', search: '' },
    history: { replaceState() {} },
    navigator: { userAgent: '' },
    localStorage: { getItem: () => null, setItem() {}, removeItem() {} },
    sessionStorage: { getItem: () => null, setItem() {}, removeItem() {} },
    fetch: () => new Promise(() => {}),
    matchMedia: () => ({ matches: false, addEventListener() {}, addListener() {} }),
    document: {
      readyState: 'loading', body: el(), documentElement: el(), head: el(),
      getElementById: (id) => (id === 'app' ? app : null),
      querySelector: () => null, querySelectorAll: () => [],
      createElement: el, addEventListener() {}, removeEventListener() {},
    },
    addEventListener() {}, removeEventListener() {}, dispatchEvent() {},
    RegionFilter: { syncFromUrl: (hash) => order.push('sync ' + hash) },
  };
  ctx.window = ctx;
  vm.createContext(ctx);
  vm.runInContext(fs.readFileSync(__dirname + '/public/app.js', 'utf8'), ctx, { filename: 'app.js' });
  ctx.registerPage('packets', { init: () => order.push('init packets'), destroy: () => order.push('destroy packets') });
  ctx.registerPage('nodes', { init: () => order.push('init nodes'), destroy: () => order.push('destroy nodes') });
  return { ctx, order };
}

test('navigate(): old destroy(), then syncFromUrl(location.hash), then new init()', () => {
  const { ctx, order } = loadApp();
  ctx.location.hash = '#/packets';
  ctx.navigate();
  order.length = 0;
  ctx.location.hash = '#/nodes?region=SJC';
  ctx.navigate();
  assert.deepStrictEqual(order, ['destroy packets', 'sync #/nodes?region=SJC', 'init nodes']);
});

test('navigate(): the first navigation syncs before the first init()', () => {
  const { ctx, order } = loadApp();
  ctx.location.hash = '#/live?region=SJC';
  ctx.navigate();
  assert.strictEqual(order[0], 'sync #/live?region=SJC');
});

(async () => {
  for (const item of queue) {
    if (item.title) { console.log(item.title); continue; }
    try { await item.fn(); passed++; console.log('  ✅ ' + item.name); }
    catch (e) { failed++; console.log('  ❌ ' + item.name + ': ' + e.message); }
  }
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})();
