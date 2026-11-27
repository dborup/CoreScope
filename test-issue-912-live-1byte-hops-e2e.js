/**
 * E2E (#912 M1): the Live map follows the 1-byte hop setting (Show / Mark /
 * Hide) of the customizer (public/hop-filter.js, #674).
 *
 * Packets are fed through the real WS handler (window._liveWSHandler(), the
 * function app.js calls per frame). A route is sender A (GPS from its advert)
 * → B as a 1-byte hop → C as a 2-byte hop, with the server's resolved_path,
 * between three fixture nodes with distinct positions. Every L.polyline the
 * Live page creates after the injection is recorded (the fading trail of each
 * segment and the clickable route), so the checks read what was drawn:
 *
 *   1. 1440x900, the mode switched through the customizer's own select and no
 *      reload between modes (mc-hide-1byte-hops-changed):
 *      - Show: a vertex sits on B and no segment is marked (control);
 *      - Hide: no polyline vertex sits on B, and A→C is drawn dashed and
 *        lighter (class live-path-uncertain), since a hop was dropped;
 *      - Mark: B stays, A→B and B→C carry the dashed class, and a route
 *        with only multi-byte hops carries none;
 *   2. a 375x560 touch phone from a cold start: More → Customize → Display by
 *      real taps, Hide picked, Live by a real tap; no vertex on B.
 *
 * Usage: BASE_URL=http://localhost:13581 node test-issue-912-live-1byte-hops-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

async function open(browser, { phone = false } = {}) {
  const ctx = await browser.newContext(phone
    ? { viewport: { width: 375, height: 560 }, isMobile: true, hasTouch: true }
    : { viewport: { width: 1440, height: 900 } });
  await ctx.addInitScript(() => {
    try {
      if (sessionStorage.getItem('__e2e912_seeded')) return; // a cold start, once per tab
      sessionStorage.setItem('__e2e912_seeded', '1');
      localStorage.removeItem('meshcore-1byte-hops-mode');
      localStorage.removeItem('meshcore-hide-1byte-hops');
      localStorage.removeItem('live-multibyte-only');
      localStorage.removeItem('live-matrix-mode');
      localStorage.removeItem('live-realistic-propagation');
    } catch (_e) { /* storage unavailable */ }
  });
  const page = await ctx.newPage();
  page.setDefaultTimeout(30000);
  const errors = [];
  page.on('pageerror', (e) => errors.push(String(e.message)));
  return { ctx, page, errors };
}

// Waits for Live to have its nodes and for the replayed fixture packets to
// finish animating, then records every polyline created from here on.
async function readyLive(page) {
  await page.waitForFunction(() => window._liveNodeData && window._liveWSHandler && window._liveWSHandler() &&
    window.HopResolver && window.HopResolver.ready() &&
    Object.values(window._liveNodeData()).filter((n) => n.lat && n.lon).length >= 4, null, { timeout: 30000 });
  await page.waitForFunction(() => window._liveTestSeams.getAnimCount() === 0 &&
    document.getElementById('liveAnimCount').textContent === '0', null, { timeout: 30000 });
  await page.evaluate(() => {
    if (window.__e2e912Polys) return;
    window.__e2e912Polys = [];
    const orig = L.polyline;
    L.polyline = function (latlngs, options) {
      const p = orig.apply(this, arguments);
      window.__e2e912Polys.push({
        latlngs: (latlngs || []).map((ll) => { const x = L.latLng(ll); return [x.lat, x.lng]; }),
        className: (options && options.className) || '',
        dashArray: (options && options.dashArray) || null,
        opacity: options && options.opacity,
        weight: options && options.weight,
      });
      return p;
    };
  });
}

// Four fixture nodes with GPS and pairwise distinct positions.
function pickNodes(page) {
  return page.evaluate(() => {
    const nd = window._liveNodeData();
    const out = [];
    for (const k of Object.keys(nd).sort()) {
      const n = nd[k];
      if (!n.lat || !n.lon) continue;
      if (out.some((o) => Math.abs(o.lat - n.lat) < 0.01 && Math.abs(o.lon - n.lon) < 0.01)) continue;
      if (out.some((o) => o.key.slice(0, 6) === k.slice(0, 6))) continue;
      out.push({ key: k, name: n.name, lat: n.lat, lon: n.lon });
      if (out.length === 4) break;
    }
    return out;
  });
}

// Sends one ADVERT from `a` over `hops` ([{ node, bytes }]) and waits until
// every animation is done. Returns the polylines it drew. Headless Chromium
// without a GPU paints Live at a few frames a second and a segment's progress
// per frame is capped (tickDt), so a 3-segment route can take ~15 s.
async function send(page, id, a, hops) {
  return page.evaluate(async ({ id, a, hops }) => {
    window.__e2e912Polys.length = 0;
    window._liveWSHandler()({ type: 'packet', data: {
      id: 912000000 + id,
      hash: (0x912000000000 + id).toString(16).padStart(16, '0'),
      timestamp: new Date().toISOString(),
      observer_id: 'E2E912', observer_name: 'e2e-912',
      path_json: JSON.stringify(hops.map((h) => h.node.key.slice(0, h.bytes * 2).toUpperCase())),
      resolved_path: JSON.stringify(hops.map((h) => h.node.key)),
      decoded: { header: { payloadTypeName: 'ADVERT' },
        payload: { type: 'ADVERT', pubKey: a.key, name: a.name, lat: a.lat, lon: a.lon } },
    } });
    const t0 = Date.now();
    await new Promise((r) => setTimeout(r, 100));
    while (window._liveTestSeams.getAnimCount() > 0 || document.getElementById('liveAnimCount').textContent !== '0') {
      if (Date.now() - t0 > 60000) {
        throw new Error('animation did not finish: anims=' + window._liveTestSeams.getAnimCount() +
          ' counter=' + document.getElementById('liveAnimCount').textContent);
      }
      await new Promise((r) => setTimeout(r, 50));
    }
    return window.__e2e912Polys.slice();
  }, { id, a, hops });
}

const at = (ll, n) => Math.abs(ll[0] - n.lat) < 1e-9 && Math.abs(ll[1] - n.lon) < 1e-9;
const hasVertex = (polys, n) => polys.some((p) => p.latlngs.some((ll) => at(ll, n)));
// The fading core line of the segment from → to: not its glow (weight 6) and
// not the invisible clickable route (opacity 0, weight 12).
const segment = (polys, from, to) => polys.find((p) => p.latlngs.length === 2 && p.opacity > 0 &&
  p.weight <= 2 && at(p.latlngs[0], from) && at(p.latlngs[1], to));
const uncertain = (p) => !!p && /\blive-path-uncertain\b/.test(p.className) && p.dashArray === '4 6';

async function pickMode(page, mode, phone) {
  if (!(await page.$('.cust-overlay:not(.hidden)'))) {
    if (phone) {
      await page.tap('[data-bottom-nav-tab="more"]');
      await page.waitForSelector('[data-mpa-mirror="customizeToggle"]', { state: 'visible', timeout: 5000 });
      await page.tap('[data-mpa-mirror="customizeToggle"]');
    } else {
      // Live hides the top bar after 4 s; a mouse move over the page brings it back.
      await page.mouse.move(700, 450);
      await page.mouse.move(720, 460);
      await page.waitForSelector('#customizeToggle', { state: 'visible', timeout: 5000 });
      await page.click('#customizeToggle');
    }
    await page.waitForSelector('.cust-overlay:not(.hidden)', { timeout: 5000 });
  }
  if (phone) await page.tap('.cust-tab[data-tab="display"]'); else await page.click('.cust-tab[data-tab="display"]');
  await page.waitForSelector('#cv2-hide-1byte-hops', { state: 'visible' });
  if (phone) await page.tap('#cv2-hide-1byte-hops');
  await page.selectOption('#cv2-hide-1byte-hops', mode);
  if (phone) await page.tap('.cust-close'); else await page.click('.cust-close');
  await page.waitForSelector('.cust-overlay:not(.hidden)', { state: 'detached', timeout: 5000 }).catch(() => {});
  assert(await page.evaluate(() => window.MC_get1ByteHopsMode()) === mode, 'the select did not set ' + mode);
}

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  try {
    console.log('\n=== #912 M1 Live 1-byte hop modes E2E against ' + BASE + ' ===');

    {
      const h = await open(browser);
      await h.page.goto(BASE + '/#/live', { waitUntil: 'load' });
      await readyLive(h.page);
      const [A, B, C, D] = await pickNodes(h.page);
      assert(D, 'the fixture needs four nodes with distinct GPS positions');
      const route = [{ node: B, bytes: 1 }, { node: C, bytes: 2 }];
      let id = 0;

      await step('Show (control): the 1-byte hop is drawn, nothing is marked', async () => {
        await pickMode(h.page, 'show', false);
        const polys = await send(h.page, ++id, A, route);
        assert(hasVertex(polys, B), 'Show must draw a vertex on B: ' + JSON.stringify(polys));
        assert(segment(polys, A, B) && segment(polys, B, C), 'A→B and B→C must be drawn');
        assert(!polys.some(uncertain), 'Show marks nothing');
      });

      await step('Hide: no Live polyline vertex sits on the 1-byte hop, A→C is dashed and lighter', async () => {
        await pickMode(h.page, 'hide', false);
        const polys = await send(h.page, ++id, A, route);
        assert(polys.length > 0, 'the route must still be drawn');
        assert(!hasVertex(polys, B), 'a vertex sits on the hidden hop B: ' + JSON.stringify(polys));
        const ac = segment(polys, A, C);
        assert(ac, 'A→C must be drawn: ' + JSON.stringify(polys));
        assert(uncertain(ac), 'A→C must carry live-path-uncertain + 4 6 dashes: ' + JSON.stringify(ac));
        assert(ac.opacity < 0.8, 'A→C must be lighter than a certain segment (0.8): ' + ac.opacity);
      });

      await step('Hide: a route without 1-byte hops is drawn unmarked', async () => {
        const polys = await send(h.page, ++id, A, [{ node: C, bytes: 2 }, { node: D, bytes: 3 }]);
        assert(segment(polys, A, C) && segment(polys, C, D), 'A→C and C→D must be drawn');
        assert(!polys.some(uncertain), 'nothing was dropped, so nothing is marked');
      });

      await step('Mark: the 1-byte hop stays; A→B and B→C carry the dashed class, C→D not', async () => {
        await pickMode(h.page, 'mark', false);
        const polys = await send(h.page, ++id, A, [{ node: B, bytes: 1 }, { node: C, bytes: 2 }, { node: D, bytes: 3 }]);
        assert(hasVertex(polys, B), 'Mark keeps B');
        const ab = segment(polys, A, B), bc = segment(polys, B, C), cd = segment(polys, C, D);
        assert(uncertain(ab), 'A→B must be marked: ' + JSON.stringify(ab));
        assert(uncertain(bc), 'B→C must be marked: ' + JSON.stringify(bc));
        assert(cd && !uncertain(cd), 'C→D is certain: ' + JSON.stringify(cd));
        assert(ab.opacity < cd.opacity, 'a marked segment is lighter: ' + ab.opacity + ' vs ' + cd.opacity);
      });

      await step('no page errors', async () => {
        assert(h.errors.length === 0, h.errors.join(' | '));
      });
      await h.ctx.close();
    }

    await step('375x560 touch phone, cold start, real taps: Hide leaves no vertex on the 1-byte hop', async () => {
      const h = await open(browser, { phone: true });
      await h.page.goto(BASE + '/', { waitUntil: 'load' });
      await pickMode(h.page, 'hide', true);
      await h.page.tap('[data-bottom-nav-tab="live"]');
      await h.page.waitForFunction(() => location.hash.indexOf('#/live') === 0);
      await readyLive(h.page);
      const [A, B, C] = await pickNodes(h.page);
      const polys = await send(h.page, 99, A, [{ node: B, bytes: 1 }, { node: C, bytes: 2 }]);
      assert(polys.length > 0 && !hasVertex(polys, B), 'a vertex sits on the hidden hop B: ' + JSON.stringify(polys));
      assert(uncertain(segment(polys, A, C)), 'A→C must be dashed: ' + JSON.stringify(polys));
      const sw = await h.page.evaluate(() => document.documentElement.scrollWidth);
      assert(sw <= 375, 'no sideways scroll at 375px: ' + sw);
      assert(h.errors.length === 0, h.errors.join(' | '));
      await h.ctx.close();
    });
  } finally {
    await browser.close();
  }
  console.log('\n' + passed + ' passed, ' + failed + ' failed');
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
