/* test-issue-124-rx-coverage-viewport.js — RX Coverage initial viewport and
 * lifecycle (#124).
 *
 * Loads the REAL public/rx-coverage.js in a vm with a fake Leaflet map that
 * records setView/fitBounds, a fetch whose responses the test releases, fake
 * localStorage/location/history and a manual clock.
 *
 * Viewport precedence: valid URL lat/lon/zoom, then a valid
 * `rx-coverage-view`, then /api/config/map, then the documented offline
 * fallback (51.0, 4.8, zoom 8). Invalid, partial or out-of-range values fall
 * through. Only `rx-coverage-view` is read or written, never the main map's
 * `map-view`. days/rx stay in the URL as the viewport changes, an
 * observer-only rx= link still fits that observer, and delayed responses
 * never touch a destroyed or replaced page.
 */
'use strict';
const vm = require('vm');
const fs = require('fs');
const assert = require('assert');

const SRC = fs.readFileSync(__dirname + '/public/rx-coverage.js', 'utf8');
const APP = fs.readFileSync(__dirname + '/public/app.js', 'utf8');
const FALLBACK = { lat: 51.0, lon: 4.8, zoom: 8 };

let passed = 0, failed = 0;
async function test(name, fn) {
  try { await fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + (e && e.message || e)); }
}
const flush = async () => { for (let i = 0; i < 20; i++) await new Promise((r) => setImmediate(r)); };

function makeEnv(opts) {
  opts = opts || {};
  const storage = Object.assign({}, opts.storage || {});
  const pending = [];                        // unreleased fetches
  const fetchLog = [];
  const timers = [];
  const maps = [];
  const location = { hash: opts.hash || '#/rx-coverage' };

  function makeMap() {
    const handlers = {};
    const m = {
      removed: false, views: [], fits: [], center: null, zoom: null,
      setView(c, z) { m.views.push({ lat: c[0], lon: c[1], zoom: z }); m.center = { lat: c[0], lng: c[1] }; m.zoom = z; return m; },
      fitBounds(b) { m.fits.push(b); return m; },
      on(ev, fn) { ev.split(' ').forEach((e) => { (handlers[e] = handlers[e] || []).push(fn); }); return m; },
      fire(ev) { (handlers[ev] || []).forEach((fn) => fn()); },
      getCenter() { return m.center; }, getZoom() { return m.zoom; },
      getBounds() { return { getSouth: () => 0, getWest: () => 0, getNorth: () => 1, getEast: () => 1 }; },
      invalidateSize() {}, remove() { m.removed = true; },
      panTo(ll) { m.center = { lat: ll[0], lng: ll[1] }; },
    };
    maps.push(m);
    return m;
  }
  const layer = () => ({ addTo() { return this; }, clearLayers() {}, bindTooltip() { return this; } });
  const el = () => ({ innerHTML: '', addEventListener() {}, querySelectorAll: () => [], dataset: {} });
  const els = {};
  const sandbox = {
    console: { log() {}, warn() {}, error() {} },
    Promise, Math, JSON, Number, String, Array, Object, Date, isFinite, parseFloat, parseInt, URLSearchParams,
    window: { MeshConfigReady: Promise.resolve(), MC_CLIENT_RX_COVERAGE: true },
    document: { getElementById: (id) => els[id] || (els[id] = el()), documentElement: {} },
    location,
    history: { replaceState: (_s, _t, url) => { location.hash = url; } },
    localStorage: {
      getItem: (k) => (k in storage ? storage[k] : null),
      setItem: (k, v) => { storage[k] = String(v); },
      removeItem: (k) => { delete storage[k]; },
    },
    getComputedStyle: () => ({ getPropertyValue: () => '' }),
    L: { map: () => makeMap(), tileLayer: () => layer(), layerGroup: () => layer(), polygon: () => layer() },
    escapeHtml: (s) => String(s),
    debounce: (fn) => fn,
    setTimeout: (fn, ms) => { timers.push({ fn, ms }); return timers.length; },
    clearTimeout: () => {},
    fetch: (url) => {
      fetchLog.push(url);
      return new Promise((resolve, reject) => { pending.push({ url, resolve, reject }); });
    },
  };
  sandbox.getHashParams = () => new URLSearchParams((location.hash.split('?')[1] || ''));
  vm.createContext(sandbox);
  // parseViewportHash and friends come from app.js in the browser.
  const m = APP.match(/function parseViewportHash[\s\S]*?\n}\n/);
  if (m) vm.runInContext(m[0], sandbox);
  let page;
  sandbox.registerPage = (name, obj) => { page = obj; };
  vm.runInContext(SRC, sandbox);

  const respond = (re, body, ok = true) => {
    const i = pending.findIndex((p) => re.test(p.url));
    if (i < 0) return false;
    const p = pending.splice(i, 1)[0];
    p.resolve({ ok, json: async () => body });
    return true;
  };
  const failFetch = (re) => {
    const i = pending.findIndex((p) => re.test(p.url));
    if (i < 0) return false;
    pending.splice(i, 1)[0].reject(new Error('offline'));
    return true;
  };
  const runTimers = () => { while (timers.length) timers.shift().fn(); };
  return { sandbox, storage, location, maps, fetchLog, pending, respond, failFetch, runTimers, page: () => page };
}

// Mounts the page and lets it reach its first map (answering /api/config/map
// with cfg, or failing it, when it is requested).
async function mount(env, cfg) {
  env.page().init({ innerHTML: '' });
  await flush();
  if (cfg === 'fail') env.failFetch(/\/api\/config\/map/);
  else if (cfg) env.respond(/\/api\/config\/map/, cfg);
  await flush();
}
const lastView = (env) => { const m = env.maps[env.maps.length - 1]; return m && m.views[0]; };
function assertView(env, want, tag) {
  const v = lastView(env);
  assert(v, 'no map view set (' + tag + ')');
  assert(Math.abs(v.lat - want.lat) < 1e-9 && Math.abs(v.lon - want.lon) < 1e-9 && v.zoom === want.zoom,
    tag + ': view ' + JSON.stringify(v) + ', want ' + JSON.stringify(want));
}

(async () => {
  console.log('--- test-issue-124-rx-coverage-viewport.js ---');

  await test('1. a valid URL viewport wins over storage and config', async () => {
    const env = makeEnv({ hash: '#/rx-coverage?lat=55.5&lon=10.25&zoom=11', storage: { 'rx-coverage-view': JSON.stringify({ lat: 1, lng: 2, zoom: 5 }) } });
    await mount(env, { center: [3, 4], zoom: 6 });
    assertView(env, { lat: 55.5, lon: 10.25, zoom: 11 }, 'URL');
  });

  await test('2. a valid rx-coverage-view wins over config', async () => {
    const env = makeEnv({ storage: { 'rx-coverage-view': JSON.stringify({ lat: 56.1, lng: 9.9, zoom: 10 }) } });
    await mount(env, { center: [3, 4], zoom: 6 });
    assertView(env, { lat: 56.1, lon: 9.9, zoom: 10 }, 'storage');
  });

  await test('3. /api/config/map when there is no URL viewport and nothing saved', async () => {
    const env = makeEnv({});
    await mount(env, { center: [55.68, 12.57], zoom: 9 });
    assertView(env, { lat: 55.68, lon: 12.57, zoom: 9 }, 'config');
  });

  await test('4. the documented offline fallback when config fails or is invalid', async () => {
    for (const cfg of ['fail', { center: 'x', zoom: 9 }, { center: [999, 0], zoom: 9 }, {}]) {
      const env = makeEnv({});
      await mount(env, cfg);
      assertView(env, FALLBACK, 'fallback for ' + JSON.stringify(cfg));
    }
  });

  await test('5. invalid, partial or out-of-range URL and storage values fall through', async () => {
    const badHashes = ['?lat=55&lon=10', '?lat=95&lon=10&zoom=9', '?lat=55&lon=190&zoom=9', '?lat=abc&lon=10&zoom=9', '?lat=55&lon=10&zoom=99', '?lat=55&lon=10&zoom=0'];
    for (const h of badHashes) {
      const env = makeEnv({ hash: '#/rx-coverage' + h, storage: { 'rx-coverage-view': JSON.stringify({ lat: 56.1, lng: 9.9, zoom: 10 }) } });
      await mount(env, { center: [3, 4], zoom: 6 });
      assertView(env, { lat: 56.1, lon: 9.9, zoom: 10 }, 'URL ' + h + ' falls through to storage');
    }
    const badSaved = ['{"lat":56}', '{"lat":95,"lng":9,"zoom":10}', '{"lat":56,"lng":9,"zoom":99}', 'not json', '{"lat":"x","lng":9,"zoom":10}', 'null'];
    for (const s of badSaved) {
      const env = makeEnv({ storage: { 'rx-coverage-view': s } });
      await mount(env, { center: [55.68, 12.57], zoom: 9 });
      assertView(env, { lat: 55.68, lon: 12.57, zoom: 9 }, 'saved ' + s + ' falls through to config');
    }
  });

  await test('6. the main map key map-view is never read or written', async () => {
    const env = makeEnv({ storage: { 'map-view': JSON.stringify({ lat: 40, lng: -74, zoom: 13 }) } });
    await mount(env, { center: [55.68, 12.57], zoom: 9 });
    assertView(env, { lat: 55.68, lon: 12.57, zoom: 9 }, 'map-view ignored');
    const m = env.maps[0];
    m.setView([56.2, 10.1], 12);
    m.fire('moveend');
    assert.strictEqual(env.storage['map-view'], JSON.stringify({ lat: 40, lng: -74, zoom: 13 }), 'map-view was changed');
    const saved = JSON.parse(env.storage['rx-coverage-view'] || 'null');
    assert(saved && saved.lat === 56.2 && saved.lng === 10.1 && saved.zoom === 12, 'rx-coverage-view not saved: ' + env.storage['rx-coverage-view']);
  });

  await test('7. days and rx stay in the URL as the viewport changes', async () => {
    const env = makeEnv({ hash: '#/rx-coverage?days=14&rx=abcdef&lat=55.5&lon=10.25&zoom=11' });
    await mount(env, null);
    const m = env.maps[0];
    m.setView([56.25, 10.5], 12);
    m.fire('moveend');
    const q = new URLSearchParams(env.location.hash.split('?')[1]);
    assert.strictEqual(q.get('days'), '14', 'days lost: ' + env.location.hash);
    assert.strictEqual(q.get('rx'), 'abcdef', 'rx lost: ' + env.location.hash);
    assert.strictEqual(Number(q.get('lat')), 56.25, 'lat not in URL: ' + env.location.hash);
    assert.strictEqual(Number(q.get('lon')), 10.5);
    assert.strictEqual(q.get('zoom'), '12');
  });

  await test('8. an observer-only rx= link still fits that observer; an explicit viewport does not', async () => {
    const env = makeEnv({ hash: '#/rx-coverage?rx=abcdef' });
    await mount(env, { center: [55.68, 12.57], zoom: 9 });
    env.runTimers();
    assert(env.respond(/bbox=-90,-180,90,180.*rx=abcdef/, { features: [{ geometry: { coordinates: [[[10, 55], [11, 56]]] } }] }), 'no extent request for the observer');
    await flush();
    assert.strictEqual(env.maps[0].fits.length, 1, 'observer link did not fit the observer');

    const env2 = makeEnv({ hash: '#/rx-coverage?rx=abcdef&lat=55.5&lon=10.25&zoom=11' });
    await mount(env2, null);
    env2.runTimers();
    assert(!env2.pending.some((p) => /bbox=-90,-180,90,180/.test(p.url)), 'an explicit viewport should not be replaced by a fit');
  });

  await test('9. a config response after destroy creates no map', async () => {
    const env = makeEnv({});
    env.page().init({ innerHTML: '' });
    await flush();
    env.page().destroy();
    env.respond(/\/api\/config\/map/, { center: [55.68, 12.57], zoom: 9 });
    await flush();
    assert.strictEqual(env.maps.length, 0, 'a map was created for a destroyed page');
  });

  await test('10. after a quick remount, the old mount\'s config, extent, coverage and leaderboard responses are ignored', async () => {
    const env = makeEnv({ hash: '#/rx-coverage?rx=abcdef' });
    await mount(env, { center: [55.68, 12.57], zoom: 9 });
    env.runTimers(); // old mount: extent + leaderboard requests pending
    env.page().destroy();
    env.location.hash = '#/rx-coverage';
    env.page().init({ innerHTML: '' });
    await flush();
    const newMaps = () => env.maps.slice(1);
    // old extent response arrives now
    env.respond(/bbox=-90,-180,90,180.*rx=abcdef/, { features: [{ geometry: { coordinates: [[[10, 55], [11, 56]]] } }] });
    await flush();
    env.respond(/\/api\/config\/map/, { center: [50, 5], zoom: 7 });
    await flush();
    assert.strictEqual(newMaps().length, 1, 'expected exactly one map for the new mount, got ' + newMaps().length);
    assert.strictEqual(newMaps()[0].fits.length, 0, 'the old observer extent fitted the new map');
    // old leaderboard response for the old mount
    const board = env.sandbox.document.getElementById('rxBoard');
    board.innerHTML = 'NEW';
    env.respond(/rx-leaderboard/, { observers: [{ pubkey: 'old', name: 'OLD', score: 1, cells: 1, nodes: 1, receptions: 1 }] });
    await flush();
    assert(!/OLD/.test(board.innerHTML), 'the old leaderboard rendered into the new page');
  });

  await test('12. after a remount with a saved view (the new map exists at once), the old extent and coverage responses are ignored', async () => {
    const saved = { 'rx-coverage-view': JSON.stringify({ lat: 56.1, lng: 9.9, zoom: 10 }) };
    const env = makeEnv({ hash: '#/rx-coverage?rx=abcdef', storage: saved });
    // record what lands on each coverage layer
    const layers = [];
    env.sandbox.L.layerGroup = () => {
      const l = { cleared: 0, added: 0, addTo() { return l; }, clearLayers() { l.cleared++; } };
      layers.push(l);
      return l;
    };
    env.sandbox.L.polygon = () => {
      const pg = { addTo(l) { l.added++; return pg; }, bindTooltip() { return pg; } };
      return pg;
    };
    await mount(env, null);
    assert.strictEqual(env.maps.length, 1, 'the saved view should create the first map without a config fetch');
    env.runTimers();          // old mount: settle timer -> observer extent request
    env.maps[0].fire('moveend'); // old mount: coverage request
    assert(env.pending.some((p) => /bbox=-90,-180,90,180.*rx=abcdef/.test(p.url)), 'no old extent request pending');
    assert(env.pending.some((p) => /^\/api\/rx-coverage\?bbox=0,0,1,1/.test(p.url)), 'no old coverage request pending');

    env.page().destroy();
    env.location.hash = '#/rx-coverage';
    env.page().init({ innerHTML: '' });
    await flush();
    assert.strictEqual(env.maps.length, 2, 'the remount should create its map at once from the saved view');
    const newMap = env.maps[1], newLayer = layers[1];
    assert(newLayer, 'the remount has no coverage layer');

    // the old mount's extent response arrives while the new map exists
    assert(env.respond(/bbox=-90,-180,90,180.*rx=abcdef/, { features: [{ geometry: { coordinates: [[[10, 55], [11, 56]]] } }] }));
    await flush();
    assert.strictEqual(newMap.fits.length, 0, 'the old observer extent fitted the new map');

    // the old mount's coverage response arrives while the new layer exists
    assert(env.respond(/^\/api\/rx-coverage\?bbox=0,0,1,1/, { features: [{ properties: {}, geometry: { coordinates: [[[10, 55], [11, 56], [10, 56]]] } }] }));
    await flush();
    assert.strictEqual(newLayer.cleared + newLayer.added, 0, 'the old coverage response was drawn on the new layer (cleared ' + newLayer.cleared + ', added ' + newLayer.added + ')');
  });

  await test('11. coverage filtering and leaderboard requests are unchanged', async () => {
    const env = makeEnv({ hash: '#/rx-coverage?days=14' });
    await mount(env, { center: [55.68, 12.57], zoom: 9 });
    env.runTimers();
    assert(env.fetchLog.some((u) => /^\/api\/rx-leaderboard\?days=14&limit=25$/.test(u)), 'leaderboard request changed: ' + env.fetchLog);
    assert(env.fetchLog.some((u) => /^\/api\/rx-coverage\?bbox=0,0,1,1&z=\d+&days=14$/.test(u)), 'coverage request changed: ' + env.fetchLog);
  });

  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})();
