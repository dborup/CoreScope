/**
 * E2E (#979 items 2 and 3): the full node page on a phone.
 *
 * Item 2: at <=640 px the page's QR box is pinned over the bottom-right of
 * the node map (#1243). A node that sends no position has no map, so the
 * pinned box had nothing under it and floated up over the action chips
 * (82x82 over "Observer →" at 375 px). Asserts, for a repeater with a
 * position and one with no position (nor an estimated one, so no map), in
 * light and dark, at 320x560, 375x560,
 * 414x800 and 640x360:
 *   1. the QR box overlaps no action chip;
 *   2. every chip is at least 44 px tall, inside the screen and hit by
 *      itself at its centre and near its four corners;
 *   3. with a map the QR box stays pinned over it (position: absolute,
 *      inside the map); without one it is in the page flow;
 *   and at 1440x900 the desktop layout: the QR box in the flow, beside the
 *   map or centred without one, no overlap, compact (<44 px) chips.
 *
 * Item 3: the owner section asked GET /api/nodes/{pk}/antenna and /station
 * for a node without a position, which the server answers with its #743
 * "Not found", so every such node page logged two console errors. The CI
 * server runs with antennaClaims off, so /api/antenna-claims/config is
 * answered here with the feature (and station details) on; /antenna and
 * /station are the real server's. Asserts:
 *   4. a position-less repeater's page shows the "sends no position" text,
 *      asks neither /antenna nor /station, and logs no console error or
 *      failed /api/ response (a node with clock-skew data is used, because
 *      /clock-skew's own 404 for a node without one is out of scope);
 *   5. a positioned repeater's page still asks both, and /station answers
 *      200 (the server contract is unchanged).
 *
 * Usage:
 *   BASE_URL=http://localhost:13581 node test-issue-979-node-page-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const PHONES = [[320, 560], [375, 560], [414, 800], [640, 360]];
const NO_POSITION = 'This node sends no position in its adverts';
const CLAIMS_CONFIG = {
  enabled: true, detailsEnabled: true, pollInitialMs: 2000, pollBackoff: 1.5,
  pollMaxMs: 30000, pollMaxAttempts: 60, adminDefaultFilter: 'proven',
};

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

// The same test as the server's isValidNodePosition (cmd/server/db.go).
function positioned(n) {
  const lat = n.lat, lon = n.lon;
  return typeof lat === 'number' && typeof lon === 'number' && isFinite(lat) && isFinite(lon) &&
    !(lat === 0 && lon === 0) && lat >= -90 && lat <= 90 && lon >= -180 && lon <= 180;
}

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #979 node page QR and owner-section requests against ${BASE} ===\n`);

  // ── Pick the nodes through the API (no page, so no console noise) ──
  const probe = await browser.newContext();
  const nodes = ((await (await probe.request.get(BASE + '/api/nodes?limit=2000')).json()).nodes) || [];
  const repeaters = nodes.filter((n) => n.role === 'repeater');
  const withPos = repeaters.find(positioned);
  const noPosAll = repeaters.filter((n) => !positioned(n));
  let noPos = null;
  for (const n of noPosAll) {
    const r = await probe.request.get(BASE + '/api/nodes/' + encodeURIComponent(n.public_key) + '/clock-skew');
    if (r.ok()) { noPos = n; break; }
  }
  // A node without a position of its own may still get a map from an
  // estimated position; the overlap needs one with no map at all.
  const noMap = noPosAll.find((n) => n.estimated_lat == null || n.estimated_lon == null);
  await probe.close();
  assert(withPos, 'the fixture has no positioned repeater');
  assert(noMap, 'the fixture has no repeater without a position or an estimated one');
  assert(noPosAll.length, 'the fixture has no position-less repeater');
  assert(noPos, 'the fixture has no position-less repeater with clock-skew data');
  console.log('  → positioned ' + withPos.public_key.slice(0, 12) + '…, no map ' + noMap.public_key.slice(0, 12) +
    '…, position-less with clock skew ' + noPos.public_key.slice(0, 12) + '…');

  async function newCtx(viewport, theme, touch) {
    const ctx = await browser.newContext({ viewport, hasTouch: !!touch, isMobile: !!touch });
    await ctx.addInitScript((t) => { try { localStorage.setItem('meshcore-theme', t); } catch (_) {} }, theme);
    return ctx;
  }

  async function openNode(page, pk) {
    await page.goto(BASE + '/#/nodes/' + encodeURIComponent(pk), { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#nodeFullQrCode svg', { timeout: 15000 });
    await page.waitForSelector('.node-full-actions .node-action-chip', { timeout: 15000 });
    // The map (when there is one) and the QR settle their size after paint.
    await page.waitForTimeout(300);
  }

  // Every chip against the QR box and the screen. Each chip is scrolled to
  // the middle of the screen before it is hit-tested, so the header and the
  // bottom nav cannot cover it.
  async function layout(page) {
    const n = await page.locator('.node-full-actions .node-action-chip').count();
    const chips = [];
    for (let i = 0; i < n; i++) {
      chips.push(await page.evaluate((i) => {
        const c = document.querySelectorAll('.node-full-actions .node-action-chip')[i];
        c.scrollIntoView({ block: 'center', inline: 'nearest' });
        const r = c.getBoundingClientRect();
        const q = document.querySelector('.node-top-row .node-qr-wrap').getBoundingClientRect();
        const ox = Math.max(0, Math.min(r.right, q.right) - Math.max(r.left, q.left));
        const oy = Math.max(0, Math.min(r.bottom, q.bottom) - Math.max(r.top, q.top));
        const pts = [[0.5, 0.5], [0.1, 0.15], [0.9, 0.15], [0.1, 0.85], [0.9, 0.85]];
        const missed = pts.filter(([fx, fy]) => {
          const hit = document.elementFromPoint(r.left + r.width * fx, r.top + r.height * fy);
          return !hit || !c.contains(hit);
        }).map(([fx, fy]) => fx + ',' + fy);
        return {
          label: c.textContent.trim(), w: Math.round(r.width), h: Math.round(r.height),
          overlap: Math.round(ox) + 'x' + Math.round(oy), overlapArea: ox * oy,
          inside: r.left >= 0 && r.right <= window.innerWidth, missed,
        };
      }, i));
    }
    const qr = await page.evaluate(() => {
      const wrap = document.querySelector('.node-top-row .node-qr-wrap');
      const map = document.querySelector('.node-top-row .node-map-wrap');
      const q = wrap.getBoundingClientRect();
      const m = map && map.getBoundingClientRect();
      return {
        position: getComputedStyle(wrap).position,
        hasMap: !!map,
        insideMap: !!m && q.left >= m.left - 1 && q.right <= m.right + 1 && q.top >= m.top - 1 && q.bottom <= m.bottom + 1,
        besideMap: !!m && q.left >= m.right - 1 && Math.abs(q.top - m.top) <= 1,
        centred: Math.abs((q.left + q.right) / 2 - (wrap.parentElement.getBoundingClientRect().left + wrap.parentElement.getBoundingClientRect().right) / 2) <= 2,
        w: Math.round(q.width), h: Math.round(q.height),
      };
    });
    return { chips, qr };
  }

  // ── Item 2 on phones ──
  for (const theme of ['light', 'dark']) {
    for (const [w, h] of PHONES) {
      const ctx = await newCtx({ width: w, height: h }, theme, true);
      const page = await ctx.newPage();
      for (const [kind, node] of [['positioned', withPos], ['map-less', noMap]]) {
        await step(`${w}x${h} ${theme}, ${kind} repeater: the QR box covers no chip; every chip >= 44 px, on screen, hit by itself`, async () => {
          await openNode(page, node.public_key);
          const { chips, qr } = await layout(page);
          assert(chips.length >= 5, 'expected at least the 5 built-in actions, got ' + chips.length);
          const bad = chips.filter((c) => c.overlapArea > 0.5 || c.h < 44 || !c.inside || c.missed.length);
          assert(!bad.length, 'chips: ' + JSON.stringify(bad));
          if (kind === 'positioned') {
            assert(qr.hasMap && qr.position === 'absolute' && qr.insideMap,
              'with a map the QR box stays pinned over it (#1243): ' + JSON.stringify(qr));
          } else {
            assert(!qr.hasMap && qr.position !== 'absolute',
              'without a map the QR box is in the page flow: ' + JSON.stringify(qr));
          }
        });
      }
      await ctx.close();
    }
  }

  // ── Item 2 on the desktop: unchanged ──
  {
    const ctx = await newCtx({ width: 1440, height: 900 }, 'light', false);
    const page = await ctx.newPage();
    for (const [kind, node] of [['positioned', withPos], ['map-less', noMap]]) {
      await step(`1440x900, ${kind} repeater: the QR box is in the flow, ${kind === 'positioned' ? 'beside the map' : 'centred'}; compact chips, no overlap`, async () => {
        await openNode(page, node.public_key);
        const { chips, qr } = await layout(page);
        assert(qr.position === 'static', 'QR box position ' + qr.position);
        assert(kind === 'positioned' ? qr.besideMap : qr.centred, 'QR box placement: ' + JSON.stringify(qr));
        const bad = chips.filter((c) => c.overlapArea > 0.5 || c.h >= 44 || c.missed.length);
        assert(!bad.length, 'chips: ' + JSON.stringify(bad));
      });
    }
    await ctx.close();
  }

  // ── Item 3: no request the client can tell is pointless ──
  async function watch(pk) {
    const ctx = await newCtx({ width: 375, height: 560 }, 'light', true);
    await ctx.route((u) => new URL(u).pathname === '/api/antenna-claims/config',
      (r) => r.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(CLAIMS_CONFIG) }));
    const page = await ctx.newPage();
    const log = { errors: [], failed: [], asked: {}, status: {} };
    page.on('console', (m) => { if (m.type() === 'error') log.errors.push(m.text()); });
    page.on('pageerror', (e) => log.errors.push('pageerror: ' + e.message));
    const owner = new RegExp('^/api/nodes/' + pk + '/(antenna|station)$', 'i');
    page.on('request', (rq) => { const m = new URL(rq.url()).pathname.match(owner); if (m) log.asked[m[1]] = true; });
    page.on('response', (rs) => {
      const p = new URL(rs.url()).pathname;
      const m = p.match(owner);
      if (m) log.status[m[1]] = rs.status();
      if (p.startsWith('/api/') && rs.status() >= 400) log.failed.push(rs.status() + ' ' + p);
    });
    await page.goto(BASE + '/#/nodes/' + encodeURIComponent(pk), { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('[data-testid="antenna-claim"]:not([hidden])', { timeout: 15000 });
    await page.waitForLoadState('networkidle').catch(() => {});
    await page.waitForTimeout(500);
    const text = await page.locator('[data-testid="antenna-claim"]').innerText();
    await ctx.close();
    return Object.assign(log, { text });
  }

  await step('position-less repeater: no /antenna or /station request, a clean console, the "no position" text', async () => {
    const log = await watch(noPos.public_key);
    assert(log.text.includes(NO_POSITION), 'owner section text: ' + log.text.slice(0, 200));
    assert(!log.asked.antenna && !log.asked.station, 'asked: ' + JSON.stringify(log.asked) + ' answered: ' + JSON.stringify(log.status));
    assert(!log.failed.length, 'failed /api/ responses: ' + JSON.stringify(log.failed));
    assert(!log.errors.length, 'console errors: ' + JSON.stringify(log.errors));
  });

  await step('positioned repeater: /antenna and /station are still asked; /station answers 200', async () => {
    const log = await watch(withPos.public_key);
    assert(log.asked.antenna && log.asked.station, 'asked: ' + JSON.stringify(log.asked));
    assert(log.status.station === 200, '/station answered ' + log.status.station);
    assert(!log.text.includes(NO_POSITION), 'a positioned node shows the "no position" text');
  });

  await browser.close();
  console.log('\n' + passed + '/' + (passed + failed) + ' tests passed' + (failed ? ', ' + failed + ' failed' : ''));
  process.exit(failed > 0 ? 1 : 0);
})().catch((err) => { console.error('Fatal:', err); process.exit(1); });
