/**
 * E2E (#817): the half-profile guard is released only by the moveend of
 * the guarded pan, never by a foreign moveend that arrives first.
 *
 * #794 keeps the half-profile link's pan to A out of the viewer's stored
 * view (localStorage["map-view"]) until that pan has ended. Another
 * moveend can arrive before the pan's own: an animation still running
 * when panTo is called (drag inertia: setView -> _stop() ends it with a
 * moveend inside the call), an invalidateSize() after a resize mid-pan,
 * or any other moveend. If that moveend releases the guard, the pan's own
 * end is saved after all.
 *
 * Same harness as test-issue-794-half-profile-saved-view-e2e.js. Each
 * trigger runs at 1440x900 (mouse) and on a 375x560 touch phone, entering
 * through the real node-detail action:
 *   - foreign: a moveend fired on the map mid-pan;
 *   - resize:  the map container shrinks mid-pan and invalidateSize() runs;
 *   - inertia: an animated pan is still running when panTo is called
 *              (from a stored view near A, and far from it).
 * Asserts, per case:
 *   1. the trigger really fired (mid-pan, or stopped inside panTo) and
 *      produced an extra moveend;
 *   2. once the pan to A has ended, the stored view is untouched;
 *   3. a pan by hand right after IS saved (the guard was released).
 *
 * Usage:
 *   BASE_URL=http://localhost:13800 node test-issue-817-half-profile-guard-moveend-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13800';
const DHM_BLOCK = {
  enabled: true, url: '/api/map/dhm/wms', layer: 'dhm_terraen_skyggekort',
  minZoom: 7, maxZoom: 19, profile: true,
  bounds: [[54.4265, 7.99125], [57.778, 15.5995]],
  attribution: 'Indeholder data fra Klimadatastyrelsen, Danmarks Højdemodel, CC BY 4.0',
};
const PNG_1X1 = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGM4ceIEAAS0AlkWLoFAAAAAAElFTkSuQmCC',
  'base64');
const ACTION = '.node-full-card [data-testid="node-terrain-profile"]';
const PANEL = '[data-testid="tp-panel"]';

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #817 half-profile guard ignores a foreign moveend against ${BASE} ===\n`);

  async function newCtx(o) {
    o = o || {};
    const ctx = await browser.newContext({
      viewport: o.viewport || { width: 1440, height: 900 },
      hasTouch: !!o.touch, isMobile: !!o.touch,
    });
    const rec = { profile: 0, errors: [] };
    await ctx.route(BASE + '/api/config/client', async (route) => {
      const response = await route.fetch();
      const config = await response.json();
      config.dhm = DHM_BLOCK;
      await route.fulfill({ response, json: config });
    });
    await ctx.route(url => new URL(url).pathname === '/api/terrain/profile', (route) => {
      rec.profile++;
      return route.fulfill({ status: 503, contentType: 'application/json', body: '{"error":"not in this test"}' });
    });
    await ctx.route(/^https?:\/\/(?!localhost|127\.0\.0\.1)/i, (route) =>
      route.fulfill({ status: 200, contentType: 'image/png', body: PNG_1X1 }));
    await ctx.route(url => new URL(url).pathname === '/api/map/dhm/wms', (route) =>
      route.fulfill({ status: 200, contentType: 'image/png', body: PNG_1X1 }));
    // Seeded on every load, but never map-view: the stored view is written
    // once by the case itself, so a reload reads whatever the page saved.
    await ctx.addInitScript(() => {
      try {
        localStorage.setItem('meshcore-theme', 'light');
        localStorage.setItem('meshcore-map-clustering', 'false');
        ['row-swipe', 'tab-swipe', 'edge-drawer', 'pull-refresh']
          .forEach(k => localStorage.setItem('meshcore-gesture-hints-' + k, 'seen'));
      } catch (_) {}
    });
    const page = await ctx.newPage();
    page.on('pageerror', e => rec.errors.push(String(e)));
    return { ctx, page, rec };
  }

  // A fixture node with a real fix, picked from the API.
  const node = await (async () => {
    const { ctx, page } = await newCtx();
    await page.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
    const list = await page.evaluate(async () => {
      const d = await (await fetch('/api/nodes?limit=2000')).json();
      return (d.nodes || d).map(n => ({ pk: n.public_key, name: n.name, lat: n.lat, lon: n.lon }));
    });
    await ctx.close();
    return list.find(n => n.lat != null && n.lon != null && !(n.lat === 0 && n.lon === 0)
      && /^[0-9a-f]{64}$/i.test(n.pk) && n.name);
  })();
  assert(node, 'the fixture needs a node with a fix');
  const PK = node.pk.toLowerCase();

  async function gotoNode(page) {
    await page.goto(`${BASE}/#/nodes/${PK}`, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector(ACTION, { timeout: 30000 });
    await page.evaluate(() => window.MeshConfigReady);
  }

  async function storedView(page) {
    return page.evaluate(() => localStorage.getItem('map-view'));
  }
  async function liveView(page) {
    return page.evaluate(() => {
      const m = window.__mc_map, c = m.getCenter();
      return { lat: c.lat, lng: c.lng, zoom: m.getZoom() };
    });
  }
  // A live center comes back through Leaflet's pixel rounding, so views
  // are compared in Web Mercator pixels at their zoom: within 2 px.
  const px = (v, z) => {
    const s = 256 * Math.pow(2, z), r = v.lat * Math.PI / 180;
    return [s * (v.lng + 180) / 360, s * (1 - Math.log(Math.tan(r) + 1 / Math.cos(r)) / Math.PI) / 2];
  };
  const samePlace = (a, b, z) => { const p = px(a, z), q = px(b, z); return Math.hypot(p[0] - q[0], p[1] - q[1]) <= 2; };
  const near = (a, b) => !!a && !!b && a.zoom === b.zoom && samePlace(a, b, a.zoom);

  /* Enters the half profile through the node-detail action with a foreign
   * moveend source armed (see the header), and waits until the pan to A
   * has ended. panTo is wrapped to set the trigger off and record what
   * happened. */
  async function enterWithTrigger(page, touch, trigger) {
    await gotoNode(page);
    await page.evaluate((trigger) => {
      window.__pans = [];
      const orig = window.__origPanTo || (window.__origPanTo = L.Map.prototype.panTo);
      L.Map.prototype.panTo = function () {
        const map = this;
        const rec = { trigger, moveends: 0, animated: false, fired: false, midPan: false, ended: false };
        window.__pans.push(rec);
        const anim = () => !!(map._panAnim && map._panAnim._inProgress);
        const count = () => { rec.moveends++; };
        map.on('moveend', count);
        if (trigger === 'inertia') {
          // An animated pan still running, as drag inertia leaves it.
          map.panBy([60, 0], { animate: true, duration: 1 });
          rec.fired = anim();
        }
        const r = orig.apply(map, arguments);
        rec.animated = anim();
        if (trigger === 'inertia') rec.midPan = rec.moveends >= 1; // _stop() ended it inside panTo
        if (rec.animated && trigger === 'foreign') {
          setTimeout(() => { rec.midPan = anim(); rec.fired = true; map.fire('moveend'); }, 60);
        }
        if (rec.animated && trigger === 'resize') {
          setTimeout(() => {
            rec.midPan = anim();
            const el = map.getContainer(), before = map.getSize().y;
            el.style.height = (el.clientHeight - 40) + 'px';
            map.invalidateSize();
            rec.fired = map.getSize().y !== before;
          }, 60);
        }
        const done = () => {
          if (anim() || (rec.animated && (trigger === 'foreign' || trigger === 'resize') && !rec.fired)) return setTimeout(done, 20);
          rec.ended = true; map.off('moveend', count);
        };
        setTimeout(done, 20);
        return r;
      };
    }, trigger);
    if (touch) await page.tap(ACTION); else await page.click(ACTION);
    await page.waitForSelector('#leaflet-map[data-loaded="true"]', { timeout: 30000 });
    await page.waitForSelector(PANEL + '.is-open', { timeout: 15000 });
    await page.waitForFunction(() => window.__pans.length > 0 && window.__pans.every(p => p.ended), null, { timeout: 10000 });
    await page.waitForTimeout(300);
    // Later pans (the hand pan below) are plain.
    await page.evaluate(() => { L.Map.prototype.panTo = window.__origPanTo; });
    return page.evaluate(() => window.__pans.slice());
  }

  /* An empty spot in the visible upper map, away from markers and controls. */
  async function emptySpot(page) {
    const pt = await page.evaluate(() => {
      const m = document.getElementById('leaflet-map').getBoundingClientRect();
      const icons = Array.from(document.querySelectorAll('.leaflet-marker-icon')).map(e => e.getBoundingClientRect());
      // On a 375x560 phone the open panel leaves only a strip of map above
      // it, so the search walks down from the map's top edge pixel by pixel.
      for (let y = m.y + 6; y < m.y + m.height * 0.9; y += 3) {
        for (let fx = 0.4; fx < 0.9; fx += 0.05) {
          const x = m.x + m.width * fx;
          const hit = document.elementFromPoint(x, y);
          if (!hit || !hit.closest('.leaflet-container') || hit.closest('.leaflet-marker-icon, .leaflet-control, .map-profile-toggle, .map-profile-hint, .tp-panel')) continue;
          if (icons.every(r => x < r.left - 12 || x > r.right + 12 || y < r.top - 12 || y > r.bottom + 12)) return { x: Math.round(x), y: Math.round(y) };
        }
      }
      return null;
    });
    assert(pt, 'no empty map spot to drag from');
    return pt;
  }

  /* A pan by hand: a mouse drag on desktop, a one-finger touch drag on the
   * phone (raw CDP touch events, as a finger produces them). */
  async function dragMap(page, touch) {
    const from = await emptySpot(page);
    const to = { x: from.x - 120, y: from.y };
    const steps = 12;
    if (!touch) {
      await page.mouse.move(from.x, from.y);
      await page.mouse.down();
      for (let i = 1; i <= steps; i++) await page.mouse.move(from.x + (to.x - from.x) * i / steps, from.y + (to.y - from.y) * i / steps);
      await page.mouse.up();
    } else {
      const cdp = await page.context().newCDPSession(page);
      const at = (i) => [{ x: from.x + (to.x - from.x) * i / steps, y: from.y + (to.y - from.y) * i / steps, id: 1 }];
      await cdp.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: at(0) });
      for (let i = 1; i <= steps; i++) {
        await cdp.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: at(i) });
        await page.waitForTimeout(16);
      }
      await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
    }
    // Leaflet's drag inertia ends with its own moveend.
    await page.waitForTimeout(1200);
  }

  async function runCase(label, o) {
    const V = o.near
      ? { lat: node.lat + 0.02, lng: node.lon + 0.03, zoom: 12 }
      : { lat: node.lat > 0 ? node.lat - 20 : node.lat + 20, lng: node.lon > 0 ? node.lon - 40 : node.lon + 40, zoom: 6 };
    const VS = JSON.stringify(V);
    const { ctx, page, rec } = await newCtx(o);
    await page.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
    await page.evaluate((vs) => localStorage.setItem('map-view', vs), VS);

    const pans = await enterWithTrigger(page, o.touch, o.trigger);
    await step(`${label}: the ${o.trigger} trigger fired during the pan to A`, async () => {
      const p = pans[0];
      assert(pans.length === 1, 'pans = ' + JSON.stringify(pans));
      assert(p.animated === !!o.near, 'animated = ' + p.animated + ', want ' + !!o.near);
      assert(p.fired && p.midPan, 'trigger did not land mid-pan: ' + JSON.stringify(p));
      assert(p.moveends >= 2, 'want the foreign moveend and the pan\'s own: ' + JSON.stringify(p));
      const v = await liveView(page);
      assert(samePlace(v, { lat: node.lat, lng: node.lon }, v.zoom) || o.trigger === 'resize',
        'map at ' + JSON.stringify(v) + ', A at ' + [node.lat, node.lon]);
    });
    await step(`${label}: the pan to A is not saved over the stored view`, async () => {
      const s = await storedView(page);
      assert(s === VS, 'stored view = ' + s + ', want ' + VS);
    });
    await step(`${label}: a pan by hand right after IS saved`, async () => {
      const before = await liveView(page);
      await dragMap(page, o.touch);
      const hash = await page.evaluate(() => location.hash);
      assert(hash === '#/map?profile=' + PK, 'the drag left the half profile: hash = ' + hash);
      const after = await liveView(page);
      assert(!near(after, before), 'the drag did not move the map: ' + JSON.stringify({ before, after }));
      const s = JSON.parse(await storedView(page));
      assert(near(s, after), 'stored view = ' + JSON.stringify(s) + ', map at ' + JSON.stringify(after));
      assert(rec.profile === 0, 'the drag picked B: ' + rec.profile + ' profile request(s)');
    });
    await step(`${label}: no page errors`, async () => {
      assert(rec.errors.length === 0, rec.errors.join(' | '));
    });
    await ctx.close();
  }

  const CASES = [
    { trigger: 'foreign', near: true },
    { trigger: 'resize', near: true },
    { trigger: 'inertia', near: true },
    { trigger: 'inertia', near: false },
  ];
  for (const c of CASES) {
    const where = c.near ? 'near' : 'far';
    await runCase(`1440px, ${c.trigger}, stored view ${where}`, Object.assign({ viewport: { width: 1440, height: 900 } }, c));
    await runCase(`375x560 touch, ${c.trigger}, stored view ${where}`, Object.assign({ viewport: { width: 375, height: 560 }, touch: true }, c));
  }

  await browser.close();
  console.log(`\n#817 E2E: ${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
