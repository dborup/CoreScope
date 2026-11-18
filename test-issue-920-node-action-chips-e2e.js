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
 * Asserts, at 1280x860 and at 375x560 on a touch device:
 *   1. page and pane: both actions exist, carry node-action-chip and are not
 *      the old node-terrain-action, with the labels "Terrain profile" and
 *      "Oblique photo";
 *   2. desktop: each chip is as tall as the Analytics chip beside it (not a
 *      48 px button), with the same font size and background;
 *   3. phone: each chip is at least 44 px tall and wide, inside the screen,
 *      and the element at its centre is the chip itself;
 *   4. real taps (touchscreen) on the phone: "Oblique photo" opens its
 *      dialog, "Terrain profile" opens #/map?profile=<key>;
 *   5. no page errors.
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

  // What a chip looks like and where it is, read in one go.
  async function measure(page, sel) {
    return page.$eval(sel, (e) => {
      e.scrollIntoView({ block: 'center' });
      const r = e.getBoundingClientRect();
      const cs = getComputedStyle(e);
      const hit = document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2);
      return {
        x: r.x, y: r.y, w: r.width, h: r.height, cls: e.className,
        label: (e.lastElementChild || e).textContent.trim(),
        fontSize: cs.fontSize, bg: cs.backgroundColor,
        selfHit: !!hit && (hit === e || e.contains(hit)),
      };
    });
  }

  async function check(label, page, scope, ref, o) {
    const phone = o.viewport.width <= 768;
    const width = o.viewport.width;
    for (const [sel, text] of [[TP, 'Terrain profile'], [SF, 'Oblique photo']]) {
      await page.waitForSelector(`${scope} ${sel}`, { timeout: 10000 });
      const m = await measure(page, `${scope} ${sel}`);
      await step(`${label}: ${text}: one shared chip class, new label`, async () => {
        assert(/\bnode-action-chip\b/.test(m.cls), 'class = ' + m.cls);
        assert(!/\bnode-terrain-action\b/.test(m.cls), 'old class kept: ' + m.cls);
        assert(m.label === text, 'label = ' + m.label);
      });
      const r = await measure(page, ref);
      if (phone) {
        await step(`${label}: ${text}: a 44 px tap target inside the screen, hit by itself`, async () => {
          assert(m.h >= 44 && m.w >= 44, `target ${m.w}x${m.h}`);
          assert(m.x >= 0 && m.x + m.w <= width, 'off screen: ' + JSON.stringify(m));
          assert(m.selfHit, 'the element at its centre is not the chip');
        });
      } else {
        await step(`${label}: ${text}: as compact as Analytics beside it`, async () => {
          assert(Math.abs(m.h - r.h) <= 2, `chip ${m.h}px vs Analytics ${r.h}px`);
          assert(m.h < 44, `chip is ${m.h}px, still a button`);
          assert(m.fontSize === r.fontSize, `font ${m.fontSize} vs ${r.fontSize}`);
          assert(m.bg === r.bg, `background ${m.bg} vs ${r.bg}`);
        });
      }
    }
  }

  async function gotoNode(page) {
    await page.goto(`${BASE}/#/nodes/${PK}`, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('.node-full-card', { timeout: 30000 });
    await page.waitForFunction(() => window.MeshConfigReady !== undefined, null, { timeout: 15000 });
    await page.evaluate(() => window.MeshConfigReady);
    await page.waitForTimeout(300);
  }

  const modes = [
    { name: 'desktop', viewport: { width: 1280, height: 860 }, shot: 'desktop' },
    { name: '375x560 touch', viewport: { width: 375, height: 560 }, touch: true, shot: '375' },
  ];

  for (const o of modes) {
    // ── The full node page ────────────────────────────────────────────────
    {
      const { ctx, page, rec } = await newCtx(o);
      await gotoNode(page);
      await check(`${o.name} node page`, page, '.node-full-actions', '.node-full-actions a[href$="/analytics"]', o);
      if (SHOT_DIR) await page.screenshot({ path: path.join(SHOT_DIR, `920-${o.shot}-detail.png`) });

      if (o.touch) {
        // Real taps, from a cold start of the page.
        await step(`${o.name} node page: a real tap on "Oblique photo" opens the viewer`, async () => {
          const m = await measure(page, `.node-full-actions ${SF}`);
          await page.touchscreen.tap(m.x + m.w / 2, m.y + m.h / 2);
          await page.waitForSelector(DIALOG, { timeout: 5000 });
          await page.keyboard.press('Escape');
          await page.waitForSelector(DIALOG, { state: 'detached', timeout: 5000 }).catch(() => {});
        });
        await step(`${o.name} node page: a real tap on "Terrain profile" opens the Path profile`, async () => {
          const m = await measure(page, `.node-full-actions ${TP}`);
          await page.touchscreen.tap(m.x + m.w / 2, m.y + m.h / 2);
          await page.waitForFunction((pk) => location.hash.indexOf('#/map?profile=' + pk) === 0, PK, { timeout: 5000 });
        });
      }
      await step(`${o.name} node page: no page errors`, async () => {
        assert(rec.errors.length === 0, rec.errors.join(' | '));
      });
      await ctx.close();
    }

    // ── The side pane ─────────────────────────────────────────────────────
    {
      const { ctx, page, rec } = await newCtx(o);
      await page.goto(`${BASE}/#/nodes`, { waitUntil: 'domcontentloaded' });
      const row = `tr[data-key="${node.pk}"]`;
      await page.waitForSelector(row, { timeout: 30000 });
      // The pubkey column is hidden at phone width: use the first visible cell.
      const cell = page.locator(row + ' td:visible').first();
      if (o.touch) await cell.tap(); else await cell.click();
      await page.waitForSelector('#nodesRight .node-detail-actions ' + TP, { timeout: 10000 }).catch(() => {});
      const inPane = await page.$('#nodesRight .node-detail-actions ' + TP);
      if (!inPane && o.touch) {
        // At phone width the list opens the node's own page, which the
        // section above already covered; the pane is a desktop layout.
        console.log(`  - ${o.name} side pane: not shown at this width, the node page covers it`);
      } else {
        await check(`${o.name} side pane`, page, '#nodesRight .node-detail-actions', '#nodesRight .node-detail-role a[href$="/analytics"]', o);
      }
      if (SHOT_DIR) await page.screenshot({ path: path.join(SHOT_DIR, `920-${o.shot}-pane.png`) });
      await step(`${o.name} side pane: no page errors`, async () => {
        assert(rec.errors.length === 0, rec.errors.join(' | '));
      });
      await ctx.close();
    }
  }

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
