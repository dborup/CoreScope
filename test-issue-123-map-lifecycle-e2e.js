/**
 * E2E (#123): map page lifecycle and Path Inspector layout.
 *
 * 1. Layout: at tablet/desktop widths the Map Controls panel and its toggle
 *    stay inside the Leaflet area, clear of the Path Inspector pane, and the
 *    pane toggle receives a real click, collapsed and expanded, also after a
 *    node reload (one click = one toggle).
 * 2. Stale async work: responses and timers from a destroyed mount, and the
 *    older of two same-instance node loads, change nothing on the current
 *    page: navigation during the config fetch, a quick return, navigation
 *    right after init (the 100 ms invalidateSize timer), a reverse-resolution
 *    (/api/resolve-hops) response after leaving, and out-of-order node loads.
 *
 * Usage: BASE_URL=http://localhost:13581 node test-issue-123-map-lifecycle-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const ORIGIN = new URL(BASE).origin;
let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

async function newPage(browser, errors, viewport) {
  const ctx = await browser.newContext({ viewport: viewport || { width: 1440, height: 900 } });
  const page = await ctx.newPage();
  page.setDefaultTimeout(20000);
  page.on('pageerror', (e) => errors.push('pageerror: ' + e.message));
  page.on('console', (m) => {
    if (m.type() !== 'error') return;
    const url = (m.location() && m.location().url) || '';
    if (url && !url.startsWith(ORIGIN)) return; // tile/CDN noise
    errors.push('console.error: ' + m.text());
  });
  await page.addInitScript(() => {
    window.addEventListener('unhandledrejection', (e) => {
      console.error('unhandledrejection: ' + (e.reason && e.reason.message || e.reason));
    });
    try { localStorage.removeItem('map-view'); } catch (_) {}
  });
  return { ctx, page };
}

// Holds the next request matching `pattern` until release() is called.
async function hold(page, pattern, times) {
  let release, hit;
  const released = new Promise((r) => { release = r; });
  const reached = new Promise((r) => { hit = r; });
  await page.route(pattern, async (route) => {
    hit();
    await released;
    await route.continue().catch(() => {});
  }, { times: times || 1 });
  return { release, reached: () => reached };
}

const mapLoaded = (page) => page.waitForSelector('#leaflet-map[data-loaded="true"]');
const settle = (page, ms) => page.waitForTimeout(ms || 400);

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  const errors = [];
  console.log('\n=== #123 map lifecycle and inspector layout against ' + BASE + ' ===');

  for (const [w, h] of [[1440, 900], [1280, 600], [1024, 768], [800, 900], [641, 800]]) {
    await step(`${w}x${h}: controls stay clear of the Path Inspector, and its toggle takes real clicks`, async () => {
      const { ctx, page } = await newPage(browser, errors, { width: w, height: h });
      try {
        await page.goto(BASE + '/#/map');
        await mapLoaded(page);
        if (await page.locator('#mapControls').isHidden()) await page.click('#mapControlsToggle');
        for (const expanded of [false, true, false]) {
          const isExpanded = await page.evaluate(() => document.getElementById('mapSidePane').classList.contains('expanded'));
          if (isExpanded !== expanded) await page.click('#mapPaneToggle'); // a real, unforced click
          await settle(page, 350);
          const r = await page.evaluate(() => {
            const box = (id) => document.getElementById(id).getBoundingClientRect();
            const map = box('leaflet-map'), pane = box('mapSidePane');
            const overlaps = (a, b) => a.left < b.right && b.left < a.right && a.top < b.bottom && b.top < a.bottom;
            const out = { expanded: document.getElementById('mapSidePane').classList.contains('expanded') };
            for (const id of ['mapControls', 'mapControlsToggle']) {
              const b = box(id);
              out[id] = { inside: b.left >= map.left - 1 && b.right <= map.right + 1, overPane: overlaps(b, pane) };
            }
            const t = box('mapPaneToggle');
            const hitEl = document.elementFromPoint(t.left + t.width / 2, t.top + t.height / 2);
            out.paneToggleHit = document.getElementById('mapPaneToggle').contains(hitEl);
            return out;
          });
          assert(r.expanded === expanded, `pane should be ${expanded ? 'expanded' : 'collapsed'} after a real click`);
          for (const id of ['mapControls', 'mapControlsToggle']) {
            assert(r[id].inside && !r[id].overPane, `${id} overlaps the Path Inspector (${expanded ? 'expanded' : 'collapsed'}): ${JSON.stringify(r[id])}`);
          }
          assert(r.paneToggleHit, 'the Path Inspector toggle is covered (' + (expanded ? 'expanded' : 'collapsed') + ')');
        }
      } finally { await ctx.close(); }
    });
  }

  await step('after node reloads the Path Inspector toggle still toggles once per click', async () => {
    const { ctx, page } = await newPage(browser, errors);
    try {
      await page.goto(BASE + '/#/map');
      await mapLoaded(page);
      for (const v of ['7d', '24h', '30d']) {
        await page.evaluate(() => document.getElementById('leaflet-map').removeAttribute('data-loaded'));
        await page.selectOption('#mcLastHeard', v);
        await mapLoaded(page);
      }
      const before = await page.evaluate(() => document.getElementById('mapSidePane').classList.contains('expanded'));
      await page.evaluate(() => document.getElementById('mapPaneToggle').click());
      const after = await page.evaluate(() => document.getElementById('mapSidePane').classList.contains('expanded'));
      assert(after !== before, 'one click after 3 node reloads did not toggle the pane (listeners piled up)');
    } finally { await ctx.close(); }
  });

  await step('navigating away during the config fetch leaves no map and no errors', async () => {
    const { ctx, page } = await newPage(browser, errors);
    const errsBefore = errors.length;
    try {
      const cfg = await hold(page, '**/api/config/map');
      await page.goto(BASE + '/#/map');
      await cfg.reached();
      await page.evaluate(() => { location.hash = '#/packets'; });
      await page.waitForSelector('#pktTable', { state: 'attached' });
      cfg.release();
      await settle(page, 800);
      assert(await page.locator('.leaflet-container').count() === 0, 'a map was created after leaving the page');
      assert(errors.length === errsBefore, errors.slice(errsBefore).join(' | '));
    } finally { await ctx.close(); }
  });

  await step('a quick return: the old mount\'s late config cannot create a second map', async () => {
    const { ctx, page } = await newPage(browser, errors);
    const errsBefore = errors.length;
    try {
      const first = await hold(page, '**/api/config/map');
      await page.goto(BASE + '/#/map');
      await first.reached();
      await page.evaluate(() => { location.hash = '#/packets'; });
      await page.waitForSelector('#pktTable', { state: 'attached' });
      await page.evaluate(() => { location.hash = '#/map'; });
      await mapLoaded(page);
      const mapBefore = await page.evaluate(() => { window.__t123Map = window.__mc_map; return !!window.__mc_map; });
      assert(mapBefore, 'fixture: the second mount has no map');
      first.release();
      await settle(page, 1000);
      const r = await page.evaluate(() => ({
        containers: document.querySelectorAll('.leaflet-container').length,
        same: window.__mc_map === window.__t123Map,
      }));
      assert(r.containers === 1, r.containers + ' Leaflet containers after the old config arrived');
      assert(r.same, 'the old mount replaced the current map');
      assert(errors.length === errsBefore, errors.slice(errsBefore).join(' | '));
    } finally { await ctx.close(); }
  });

  await step('leaving right after init: the delayed invalidateSize is inert', async () => {
    const { ctx, page } = await newPage(browser, errors);
    const errsBefore = errors.length;
    try {
      await page.goto(BASE + '/#/packets');
      await page.waitForSelector('#pktTable', { state: 'attached' });
      await page.evaluate(() => new Promise((resolve) => {
        location.hash = '#/map';
        const t0 = Date.now();
        (function poll() {
          if (document.querySelector('#leaflet-map.leaflet-container')) { location.hash = '#/packets'; return resolve(); }
          if (Date.now() - t0 > 10000) return resolve();
          setTimeout(poll, 5);
        })();
      }));
      await settle(page, 800);
      assert(errors.length === errsBefore, errors.slice(errsBefore).join(' | '));
    } finally { await ctx.close(); }
  });

  await step('a reverse-resolution response after leaving touches nothing', async () => {
    const { ctx, page } = await newPage(browser, errors);
    const errsBefore = errors.length;
    try {
      await page.goto(BASE + '/#/map');
      await mapLoaded(page);
      const rh = await hold(page, '**/api/resolve-hops**');
      await page.evaluate(() => { window.drawPacketRouteMulti([{ path: ['aa', 'bb'] }], null); });
      await rh.reached();
      await page.evaluate(() => { location.hash = '#/packets'; });
      await page.waitForSelector('#pktTable', { state: 'attached' });
      rh.release();
      await settle(page, 800);
      assert(errors.length === errsBefore, errors.slice(errsBefore).join(' | '));
    } finally { await ctx.close(); }
  });

  await step('out-of-order node loads: only the newest one renders', async () => {
    const { ctx, page } = await newPage(browser, errors);
    try {
      const node = (pk, lat, lon) => ({ public_key: pk, name: 'n-' + pk.slice(0, 4), role: 'repeater', lat, lon, last_seen: new Date().toISOString(), advert_count: 1 });
      const OLD = { nodes: [node('a'.repeat(64), 55.1, 12.1)], total: 1, counts: {} };
      const NEW = { nodes: [node('b'.repeat(64), 55.2, 12.2), node('c'.repeat(64), 55.3, 12.3)], total: 2, counts: {} };
      await page.goto(BASE + '/#/map');
      await mapLoaded(page);
      let releaseOld;
      const oldHeld = new Promise((r) => { releaseOld = r; });
      await page.route(/\/api\/nodes\?limit=\d+&offset=0&lastHeard=7d/, async (route) => {
        await oldHeld;
        await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(OLD) });
      });
      await page.route(/\/api\/nodes\?limit=\d+&offset=0&lastHeard=24h/, (route) =>
        route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(NEW) }));
      await page.selectOption('#mcLastHeard', '7d');   // older load, held
      await page.selectOption('#mcLastHeard', '24h');  // newer load, answered at once
      await page.waitForFunction(() => (window.__mc_nodes || []).length === 2);
      releaseOld();
      await settle(page, 800);
      const keys = await page.evaluate(() => (window.__mc_nodes || []).map((n) => n.public_key[0]).join(''));
      assert(keys === 'bc', 'the older node load overwrote the newer one (nodes: ' + keys + ')');
    } finally { await ctx.close(); }
  });

  // #123 review round: leaving the map with an open route must not run the
  // route view's delayed invalidateSize on the removed map (it threw
  // "Cannot read properties of undefined (reading '_leaflet_pos')").
  const ROUTE = [
    { pubkey: 'aa00aa00aa00aa00', name: 'Origin', role: 'companion', lat: 55.60, lon: 12.50, isOrigin: true },
    { pubkey: 'bb11bb11bb11bb11', name: 'Relay', role: 'repeater', lat: 55.70, lon: 12.60, resolved: true },
    { pubkey: 'cc22cc22cc22cc22', name: 'Dest', role: 'repeater', lat: 55.80, lon: 12.40, resolved: true, isDest: true },
  ];
  for (const [label, w, h] of [['desktop', 1440, 900], ['tablet', 1024, 768]]) {
    await step(`${label} ${w}x${h}: leaving the map with an open route, the route view's delayed invalidateSize is inert`, async () => {
      const { ctx, page } = await newPage(browser, errors, { width: w, height: h });
      const errsBefore = errors.length;
      try {
        await page.goto(BASE + '/#/map?route=1');
        await mapLoaded(page);
        await page.evaluate((positions) => {
          window.MeshRouteView.render(window.__mc_map, window.__mc_routeLayer, positions, { timestamp: Date.now() });
        }, ROUTE);
        await page.waitForSelector('.mc-rt-sidebar');
        await settle(page, 700); // the render's own timers run on the live map
        await page.evaluate(() => { location.hash = '#/packets'; });
        await page.waitForSelector('#pktTable', { state: 'attached' });
        await settle(page, 800);
        assert(errors.length === errsBefore, errors.slice(errsBefore).join(' | '));
      } finally { await ctx.close(); }
    });
  }

  // #345 CI: Leaflet 1.9.4 ends an animated zoom from a 250 ms setTimeout that
  // map.remove() does not cancel; leaving mid-zoom threw "_leaflet_pos" (e.g.
  // right after a route's fitBounds).
  await step('leaving during an animated zoom: Leaflet\'s transition-end timer is inert', async () => {
    const { ctx, page } = await newPage(browser, errors);
    const errsBefore = errors.length;
    try {
      await page.goto(BASE + '/#/map');
      await mapLoaded(page);
      const animating = await page.evaluate(() => new Promise((resolve) => {
        const m = window.__mc_map;
        m.setZoom(m.getZoom() + 1, { animate: true });
        const t0 = Date.now();
        (function poll() {
          if (m._animatingZoom) { location.hash = '#/packets'; return resolve(true); }
          if (Date.now() - t0 > 200) return resolve(false);
          requestAnimationFrame(poll);
        })();
      }));
      assert(animating, 'fixture: the zoom did not animate');
      await page.waitForSelector('#pktTable', { state: 'attached' });
      await settle(page, 800);
      assert(errors.length === errsBefore, errors.slice(errsBefore).join(' | '));
    } finally { await ctx.close(); }
  });

  // #123 review round: destroy() must drop the tile-provider listener and the
  // theme MutationObserver of its mount. Counts only map.js's own callbacks
  // (they call _syncDarkTiles; live.js has its own tile listener).
  await step('six mounts leave one tile-provider listener and one theme observer; theme switches hit one map', async () => {
    const { ctx, page } = await newPage(browser, errors);
    await page.addInitScript(() => {
      const own = (fn) => typeof fn === 'function' && /_syncDarkTiles/.test(String(fn));
      const live = new Set();
      const add = window.addEventListener, rem = window.removeEventListener;
      window.addEventListener = function (type, fn, o) {
        if (type === 'mc-tile-provider-changed' && own(fn)) live.add(fn);
        return add.call(this, type, fn, o);
      };
      window.removeEventListener = function (type, fn, o) {
        if (type === 'mc-tile-provider-changed') live.delete(fn);
        return rem.call(this, type, fn, o);
      };
      const MO = window.MutationObserver, observing = new Set();
      window.MutationObserver = class extends MO {
        constructor(cb) { super(cb); this.__own = own(cb); }
        observe(t, o) { if (this.__own) observing.add(this); return super.observe(t, o); }
        disconnect() { observing.delete(this); return super.disconnect(); }
      };
      window.__t123 = { listeners: () => live.size, observers: () => observing.size };
    });
    try {
      const mountMap = async () => {
        await page.evaluate(() => { location.hash = '#/map'; });
        await mapLoaded(page);
      };
      const leave = async () => {
        await page.evaluate(() => { location.hash = '#/packets'; });
        await page.waitForSelector('#pktTable', { state: 'attached' });
      };
      // setUrl calls caused by two theme switches and one provider change
      const setUrlCalls = () => page.evaluate(async () => {
        const proto = L.TileLayer.prototype, orig = proto.setUrl;
        let n = 0;
        proto.setUrl = function () { n++; return orig.apply(this, arguments); };
        const tick = () => new Promise((r) => setTimeout(r, 50));
        const root = document.documentElement, before = root.getAttribute('data-theme');
        root.setAttribute('data-theme', 'dark'); await tick();
        root.setAttribute('data-theme', 'light'); await tick();
        window.dispatchEvent(new CustomEvent('mc-tile-provider-changed', { detail: {} })); await tick();
        if (before == null) root.removeAttribute('data-theme'); else root.setAttribute('data-theme', before);
        await tick();
        proto.setUrl = orig;
        return n;
      });
      await page.goto(BASE + '/#/packets');
      await page.waitForSelector('#pktTable', { state: 'attached' });
      await mountMap();
      const one = await setUrlCalls();
      for (let i = 0; i < 5; i++) { await leave(); await mountMap(); }
      const counts = await page.evaluate(() => ({ l: window.__t123.listeners(), o: window.__t123.observers() }));
      const six = await setUrlCalls();
      assert(counts.l === 1, counts.l + ' map tile-provider listeners after 6 mounts, want 1');
      assert(counts.o === 1, counts.o + ' map theme observers after 6 mounts, want 1');
      assert(one > 0 && six === one, 'theme and provider changes caused ' + six + ' setUrl calls after 6 mounts, ' + one + ' after 1');
      await leave();
      const after = await page.evaluate(() => ({ l: window.__t123.listeners(), o: window.__t123.observers() }));
      assert(after.l === 0 && after.o === 0, 'after leaving: ' + after.l + ' listeners, ' + after.o + ' observers, want 0');
    } finally { await ctx.close(); }
  });

  await step('no page errors, console errors or unhandled rejections overall', async () => {
    assert(errors.length === 0, errors.slice(0, 5).join(' | '));
  });

  await browser.close();
  console.log('\n--- ' + passed + ' passed, ' + failed + ' failed ---');
  process.exit(failed ? 1 : 0);
})();
