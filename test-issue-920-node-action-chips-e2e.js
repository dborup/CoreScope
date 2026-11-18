/**
 * E2E (#920): "Terrain profile" and "Oblique photo" on a node are compact
 * chips in the style of Details, Analytics and Reach, on the side pane and
 * on the full node page, through one shared class (node-action-chip).
 *
 * The CI server has no map.dhm or map.skraafoto config, so /api/config/client
 * is given a `dhm` block (profile on) and a `skraafoto` block without bounds
 * (no coverage limit, so the fixture's node is offered the photos);
 * /api/skraafoto/search is answered with an empty result. Nothing reaches a
 * real network.
 *
 * Asserts, at 1280x860 and at 375x560 and 360x640 on a touch device:
 *   1. page and pane: both module actions exist, carry node-action-chip and
 *      are not the old node-terrain-action, with the labels "Terrain
 *      profile" and "Oblique photo";
 *   2. parity: every action in the page row (Copy URL, Copy short URL,
 *      Analytics, Reach, Observer →, the two module chips) and in the pane
 *      rows (Details, Analytics, Reach, the two module chips) carries
 *      node-action-chip and has the same rendered height, ±1 px;
 *   3. desktop: the chips are compact (under 44 px), with one font size and
 *      background across the row;
 *   4. phone: every action in the row is at least 44 px tall and wide,
 *      inside the screen, hit by itself at its centre, and the row has no
 *      horizontal overflow;
 *   5. phone, cold start with real taps: Home → bottom-nav More → Nodes →
 *      tap the node's row → its node page (at ≤640 px a row opens the page,
 *      not the side pane); there the parity above holds, "Oblique photo"
 *      opens its dialog and "Terrain profile" opens #/map?profile=<key>;
 *   6. no page errors.
 *
 * Usage:
 *   BASE_URL=http://localhost:13700 node test-issue-920-node-action-chips-e2e.js
 * Optional: SCREENSHOT_DIR=<dir> writes the PR screenshots.
 */
'use strict';
const { chromium } = require('playwright');
const path = require('path');

const BASE = process.env.BASE_URL || 'http://localhost:13700';
const SHOT_DIR = process.env.SCREENSHOT_DIR || '';
const TP = '[data-testid="node-terrain-profile"]';
const SF = '[data-testid="node-skraafoto"]';
const DIALOG = '[data-testid="sf-dialog"]';
const PNG_1X1 = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGM4ceIEAAS0AlkWLoFAAAAAAElFTkSuQmCC',
  'base64');
const DHM_BLOCK = {
  enabled: true, url: '/api/map/dhm/wms', layer: 'dhm_terraen_skyggekort',
  minZoom: 7, maxZoom: 19, profile: true,
  bounds: [[54.4265, 7.99125], [57.778, 15.5995]],
  attribution: 'x', datasetName: 'x', datasetUrl: 'https://dataforsyningen.dk/data/930',
  licenseName: 'CC BY 4.0', licenseUrl: 'https://creativecommons.org/licenses/by/4.0/',
  attributionDerived: 'x',
};
const SKF_BLOCK = {
  enabled: true, searchUrl: '/api/skraafoto/search',
  previewUrl: '/api/skraafoto/preview/{collection}/{item}', maxPx: 1024,
  note: 'x',
  credit: { attribution: 'x', owner: 'x', datasetName: 'x', datasetUrl: 'https://dataforsyningen.dk/data/930',
    licenseName: 'CC BY 4.0', licenseUrl: 'https://creativecommons.org/licenses/by/4.0/' },
};

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
  console.log(`\n=== #920 node action chips against ${BASE} ===\n`);

  async function newCtx(o) {
    const ctx = await browser.newContext({
      viewport: o.viewport, hasTouch: !!o.touch, isMobile: !!o.touch,
    });
    const rec = { errors: [] };
    await ctx.route(BASE + '/api/config/client', async (route) => {
      const response = await route.fetch();
      const config = await response.json();
      config.dhm = DHM_BLOCK;
      config.skraafoto = SKF_BLOCK;
      await route.fulfill({ response, json: config });
    });
    await ctx.route(url => new URL(url).pathname === '/api/skraafoto/search', (route) =>
      route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ photos: [] }) }));
    await ctx.route(/^https?:\/\/(?!localhost|127\.0\.0\.1)/i, (route) =>
      route.fulfill({ status: 200, contentType: 'image/png', body: PNG_1X1 }));
    await ctx.route(url => new URL(url).pathname === '/api/map/dhm/wms', (route) =>
      route.fulfill({ status: 200, contentType: 'image/png', body: PNG_1X1 }));
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

  const node = await (async () => {
    const { ctx, page } = await newCtx({ viewport: { width: 1280, height: 860 } });
    await page.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
    const list = await page.evaluate(async () => {
      const d = await (await fetch('/api/nodes?limit=2000')).json();
      return (d.nodes || d).map(n => ({ pk: n.public_key, name: n.name, lat: n.lat, lon: n.lon }));
    });
    await ctx.close();
    return list.find(n => n.lat != null && n.lon != null && !(n.lat === 0 && n.lon === 0) &&
      /^[0-9a-f]{64}$/i.test(n.pk) && n.name);
  })();
  assert(node, 'the fixture needs a node with a fix');
  const PK = node.pk.toLowerCase();

  // Every action in a row, measured in one go, after the row is in view.
  async function measureRow(page, sel) {
    return page.$$eval(sel, (els) => {
      els[0].scrollIntoView({ block: 'center' });
      return els.map((e) => {
        const r = e.getBoundingClientRect();
        const cs = getComputedStyle(e);
        const hit = document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2);
        return {
          x: r.x, y: r.y, w: r.width, h: r.height, cls: String(e.className),
          label: e.textContent.replace(/\s+/g, ' ').trim(),
          fontSize: cs.fontSize, bg: cs.backgroundColor,
          selfHit: !!hit && (hit === e || e.contains(hit)),
        };
      });
    });
  }
  const show = (items) => items.map(i => `${i.label}=${i.h}`).join(', ');

  // `sel` matches every action of the row(s); `scope` holds the two module chips.
  async function check(label, page, scope, sel, o) {
    const phone = o.viewport.width <= 768;
    const width = o.viewport.width;
    for (const [mod, text] of [[TP, 'Terrain profile'], [SF, 'Oblique photo']]) {
      await page.waitForSelector(`${scope} ${mod}`, { timeout: 10000 });
      const cls = await page.$eval(`${scope} ${mod}`, e => String(e.className));
      const lbl = await page.$eval(`${scope} ${mod}`, e => (e.lastElementChild || e).textContent.trim());
      await step(`${label}: ${text}: one shared chip class, new label`, async () => {
        assert(/\bnode-action-chip\b/.test(cls), 'class = ' + cls);
        assert(!/\bnode-terrain-action\b/.test(cls), 'old class kept: ' + cls);
        assert(lbl === text, 'label = ' + lbl);
      });
    }
    const items = await measureRow(page, sel);
    await step(`${label}: all ${items.length} actions carry node-action-chip`, async () => {
      assert(items.length >= 5, 'only ' + items.length + ' actions: ' + show(items));
      const off = items.filter(i => !/\bnode-action-chip\b/.test(i.cls));
      assert(!off.length, 'without the class: ' + off.map(i => i.label).join(', '));
    });
    await step(`${label}: every action has the same height (±1 px)`, async () => {
      const hs = items.map(i => i.h);
      assert(Math.max(...hs) - Math.min(...hs) <= 1, show(items));
    });
    if (phone) {
      await step(`${label}: every action is a 44 px tap target inside the screen, hit by itself`, async () => {
        for (const m of items) {
          assert(m.h >= 44 && m.w >= 44, `${m.label}: target ${m.w}x${m.h}`);
          assert(m.x >= 0 && m.x + m.w <= width, `${m.label}: off screen ${m.x}+${m.w}`);
          assert(m.selfHit, `${m.label}: the element at its centre is not the action`);
        }
        const overflow = await page.$eval(scope, e => e.scrollWidth - e.clientWidth);
        assert(overflow <= 0, 'the row overflows by ' + overflow + ' px');
      });
    } else {
      await step(`${label}: compact, one font size and background across the row`, async () => {
        for (const m of items) assert(m.h < 44, `${m.label} is ${m.h}px, still a button`);
        const fonts = new Set(items.map(i => i.fontSize)), bgs = new Set(items.map(i => i.bg));
        assert(fonts.size === 1, 'fonts ' + [...fonts].join(' / '));
        assert(bgs.size === 1, 'backgrounds ' + [...bgs].join(' / '));
      });
    }
  }

  const PAGE_ROW = '.node-full-actions';
  const PANE_ROWS = '#nodesRight .node-detail-role > .btn-primary, #nodesRight .node-detail-actions > *';

  async function waitConfig(page) {
    await page.waitForFunction(() => window.MeshConfigReady !== undefined, null, { timeout: 15000 });
    await page.evaluate(() => window.MeshConfigReady);
  }

  // A real touchscreen tap at the element's centre, once it is visible and
  // has stopped moving (the More sheet slides in).
  async function tapOn(page, sel) {
    await page.locator(sel).first().tap({ timeout: 10000 });
  }

  const modes = [
    { name: 'desktop', viewport: { width: 1280, height: 860 }, shot: 'desktop' },
    { name: '375x560 touch', viewport: { width: 375, height: 560 }, touch: true, shot: '375' },
    { name: '360x640 touch', viewport: { width: 360, height: 640 }, touch: true, shot: '360' },
  ];

  for (const o of modes) {
    if (!o.touch) {
      // ── Desktop: the full node page by deep link ────────────────────────
      {
        const { ctx, page, rec } = await newCtx(o);
        await page.goto(`${BASE}/#/nodes/${PK}`, { waitUntil: 'domcontentloaded' });
        await page.waitForSelector('.node-full-card', { timeout: 30000 });
        await waitConfig(page);
        await page.waitForTimeout(300);
        await check(`${o.name} node page`, page, PAGE_ROW, `${PAGE_ROW} > *`, o);
        if (SHOT_DIR) await page.screenshot({ path: path.join(SHOT_DIR, `920-${o.shot}-detail.png`) });
        await step(`${o.name} node page: no page errors`, async () => {
          assert(rec.errors.length === 0, rec.errors.join(' | '));
        });
        await ctx.close();
      }
      // ── Desktop: the side pane ──────────────────────────────────────────
      {
        const { ctx, page, rec } = await newCtx(o);
        await page.goto(`${BASE}/#/nodes`, { waitUntil: 'domcontentloaded' });
        const row = `tr[data-key="${node.pk}"]`;
        await page.waitForSelector(row, { timeout: 30000 });
        await page.locator(row + ' td:visible').first().click();
        await page.waitForSelector('#nodesRight .node-detail-actions ' + SF, { timeout: 10000 });
        await page.waitForTimeout(300);
        await check(`${o.name} side pane`, page, '#nodesRight .node-detail-actions', PANE_ROWS, o);
        if (SHOT_DIR) await page.screenshot({ path: path.join(SHOT_DIR, `920-${o.shot}-pane.png`) });
        await step(`${o.name} side pane: no page errors`, async () => {
          assert(rec.errors.length === 0, rec.errors.join(' | '));
        });
        await ctx.close();
      }
      continue;
    }

    // ── Phone: cold start, real taps all the way to the node page ─────────
    const { ctx, page, rec } = await newCtx(o);
    await page.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
    await waitConfig(page);
    let reached = false;
    await step(`${o.name}: cold start, Home → More → Nodes → row tap opens the node page`, async () => {
      await tapOn(page, '[data-bottom-nav-tab="more"]');
      await tapOn(page, '[data-bottom-nav-more-route="nodes"]');
      await page.waitForFunction(() => location.hash.indexOf('#/nodes') === 0, null, { timeout: 10000 });
      // The pubkey column is hidden at phone width: tap the first visible cell.
      const row = `tr[data-key="${node.pk}"]`;
      await page.waitForSelector(row, { timeout: 30000 });
      await tapOn(page, row + ' td:visible');
      await page.waitForSelector('.node-full-card', { timeout: 30000 });
      const hash = await page.evaluate(() => location.hash);
      assert(hash.toLowerCase().indexOf('#/nodes/' + PK) === 0, 'hash = ' + hash);
      assert(await page.$('#nodesRight .node-detail-actions') === null, 'a side pane opened at phone width');
      reached = true;
    });
    if (reached) {
      await page.waitForTimeout(300);
      await check(`${o.name} node page`, page, PAGE_ROW, `${PAGE_ROW} > *`, o);
      if (SHOT_DIR) await page.screenshot({ path: path.join(SHOT_DIR, `920-${o.shot}-detail.png`) });
      await step(`${o.name} node page: a real tap on "Oblique photo" opens the viewer`, async () => {
        await tapOn(page, `${PAGE_ROW} ${SF}`);
        await page.waitForSelector(DIALOG, { timeout: 5000 });
        await page.keyboard.press('Escape');
        await page.waitForSelector(DIALOG, { state: 'detached', timeout: 5000 }).catch(() => {});
      });
      await step(`${o.name} node page: a real tap on "Terrain profile" opens the Path profile`, async () => {
        await tapOn(page, `${PAGE_ROW} ${TP}`);
        await page.waitForFunction((pk) => location.hash.indexOf('#/map?profile=' + pk) === 0, PK, { timeout: 5000 });
      });
    }
    await step(`${o.name}: no page errors`, async () => {
      assert(rec.errors.length === 0, rec.errors.join(' | '));
    });
    await ctx.close();
  }

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
