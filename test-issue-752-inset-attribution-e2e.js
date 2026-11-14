/**
 * E2E (#752): the inset maps credit the base map they render.
 *
 * The inset maps built on `_applyTilesToNodeMap` (node detail, node-reach,
 * packet-path, area-nodes, rx-coverage and the Analytics subpath minimap)
 * are created with `attributionControl: false`, so until #752 they rendered
 * the viewer's active provider with no credit at all: with `esri-imagery`
 * picked, the node-detail inset showed 12 World_Imagery tiles and 0
 * attribution controls. Esri requires a visible "Powered by Esri" and
 * source credit; OSM requires its credit too.
 *
 * Drives a real browser over three of those insets — node detail,
 * area-nodes and the Analytics subpath minimap — with Esri World Imagery
 * and with the keyless OSM default, and asserts per inset:
 *
 *   1. exactly one attribution control inside that inset, carrying the
 *      registry's compact class;
 *   2. the provider's exact credit text, and with Esri the single link to
 *      https://www.esri.com;
 *   3. the inset really rendered that provider's tiles, so "a credit is
 *      shown" is not a statement about an empty map;
 *   4. the credit is legible (>= 10px, nothing painted over its text,
 *      scrollable rather than clipped) and does not dominate the inset
 *      (inside the CSS cap, and no wider than the cap's share), including
 *      at 390x844 where the node-detail QR overlay shares its corner.
 *
 * It also pins the `- var(--live-credit-height)` term in `.live-feed`'s
 * max-height (#735 review N2) differentially: with the credit's published
 * height forced to 0 the cap must grow by exactly that height.
 *
 * #756 (follow-ups from the #754 review) adds two more checks:
 *   - every credit's scrollHeight must not exceed its clientHeight — the
 *     "clipped" assertion above tolerates a scrollable overflow, which is
 *     exactly how the review's 2px-short cap slipped through;
 *   - the Home My-Mesh health minimap (home.js), left out of #752's scope
 *     because it hardcodes OSM instead of the registry's provider, now
 *     gets the same compact credit through MC_ensureAttribution.
 *
 * The CI server has no map.tiles config, so /api/config/client is
 * intercepted and given the map.tiles block an operator would set, as the
 * #728 suite does. Every third-party tile is stubbed; nothing reaches Esri
 * or OSM.
 *
 * Usage:
 *   BASE_URL=http://localhost:13800 node test-issue-752-inset-attribution-e2e.js
 */
'use strict';
const { chromium } = require('playwright');
const zlib = require('zlib');

const BASE = process.env.BASE_URL || 'http://localhost:13800';
// The credit as it renders (the "Esri" of "Powered by Esri" is a link).
const ESRI_CREDIT = 'Powered by Esri | Tiles © Esri — Source: Esri, Vantor, Earthstar Geographics, and the GIS User Community';
const ESRI_HOME = 'https://www.esri.com';
const OSM_CREDIT = '© OpenStreetMap contributors';
const COMPACT_CLASS = 'mc-credit-compact';
const ESRI_TILES = { providers: { esri: { enabled: true } } };
// A node with lat/lon in test-fixtures/e2e-fixture.db, so the detail page
// mounts its inset map. Discovered at runtime; this is the fallback.
const NODE_KEY = process.env.NODE_KEY ||
  '988396af0ff369c2178a8c2a303661bf26193257136ed4452b6eadd9547011e9';

// ── stub tiles (256x256 PNGs; nothing third-party leaves the browser) ──────
const CRC_TABLE = (() => {
  const t = new Int32Array(256);
  for (let n = 0; n < 256; n++) { let c = n; for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1; t[n] = c; }
  return t;
})();
function crc32(buf) { let c = -1; for (const b of buf) c = CRC_TABLE[(c ^ b) & 0xff] ^ (c >>> 8); return (c ^ -1) >>> 0; }
function chunk(type, data) {
  const len = Buffer.alloc(4); len.writeUInt32BE(data.length);
  const td = Buffer.concat([Buffer.from(type), data]);
  const crc = Buffer.alloc(4); crc.writeUInt32BE(crc32(td));
  return Buffer.concat([len, td, crc]);
}
function png(grey) {
  const W = 256, H = 256, raw = Buffer.alloc((W + 1) * H);
  for (let j = 0; j < H; j++) { raw[j * (W + 1)] = 0; for (let i = 0; i < W; i++) raw[j * (W + 1) + 1 + i] = grey; }
  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(W, 0); ihdr.writeUInt32BE(H, 4); ihdr[8] = 8; ihdr[9] = 0;
  return Buffer.concat([Buffer.from('\x89PNG\r\n\x1a\n', 'binary'), chunk('IHDR', ihdr), chunk('IDAT', zlib.deflateSync(raw)), chunk('IEND', Buffer.alloc(0))]);
}
const TILE_STUB = png(180);

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
  console.log(`\n=== #752 inset map attribution against ${BASE} ===\n`);

  // tiles: the map.tiles block to serve (null leaves the server's own, i.e.
  // the keyless OSM default). storage: localStorage seeded before load.
  async function newCtx({ tiles = null, storage = {}, theme = 'light', viewport = { width: 1280, height: 900 } } = {}) {
    const ctx = await browser.newContext({ viewport });
    const rec = { imagery: [], osm: [], errors: [] };
    await ctx.route(BASE + '/api/config/client', async (route) => {
      const response = await route.fetch();
      const config = await response.json();
      delete config.tiles;
      config.map = Object.assign({}, config.map, { tiles: tiles || {} });
      await route.fulfill({ response, json: config });
    });
    await ctx.route(/^https?:\/\/(?!localhost|127\.0\.0\.1)/i, async (route) => {
      const url = route.request().url();
      if (route.request().resourceType() !== 'image') { await route.abort(); return; }
      if (/World_Imagery/.test(url)) rec.imagery.push(url);
      if (/tile\.openstreetmap\.org/.test(url)) rec.osm.push(url);
      await route.fulfill({ status: 200, contentType: 'image/png', body: TILE_STUB });
    });
    await ctx.addInitScript(({ theme: t, storage: st }) => {
      try {
        localStorage.setItem('meshcore-theme', t);
        for (const k of Object.keys(st)) localStorage.setItem(k, st[k]);
      } catch (_) {}
      const apply = () => document.documentElement && document.documentElement.setAttribute('data-theme', t);
      apply();
      document.addEventListener('DOMContentLoaded', apply);
    }, { theme, storage });
    const page = await ctx.newPage();
    page.setDefaultTimeout(20000);
    page.on('pageerror', (e) => rec.errors.push(String(e)));
    page.on('console', (m) => { if (m.type() === 'error') rec.errors.push(m.text()); });
    return { ctx, page, rec };
  }

  // Everything the credit contract needs, read from the inset itself.
  async function creditState(page, sel) {
    return page.evaluate((s) => {
      const box = document.querySelector(s);
      if (!box) return { box: false };
      const ctls = box.querySelectorAll('.leaflet-control-attribution');
      const ctl = ctls[0] || null;
      const br = box.getBoundingClientRect();
      if (!ctl) return { box: true, controls: ctls.length, insetHeight: br.height };
      const r = ctl.getBoundingClientRect();
      const cs = getComputedStyle(ctl);
      const tiles = Array.from(box.querySelectorAll('img.leaflet-tile'))
        .filter((i) => i.complete && i.naturalWidth === 256);
      // Points of the credit's text painted over by something else.
      const covered = [];
      const range = document.createRange();
      range.selectNodeContents(ctl);
      for (const rc of range.getClientRects()) {
        for (const y of [rc.top + 2, rc.bottom - 2]) {
          for (let x = rc.left + 1; x < rc.right - 1; x += 6) {
            const hit = document.elementFromPoint(x, y);
            if (!hit || !ctl.contains(hit)) covered.push((hit ? (hit.id || hit.className || hit.tagName) : 'off-screen') + '@' + Math.round(x) + ',' + Math.round(y));
          }
        }
      }
      const qr = box.closest('.node-top-row, .node-map-qr-wrap');
      const overlay = qr ? qr.querySelector('.node-qr-wrap, .node-map-qr-overlay') : null;
      const or = overlay ? overlay.getBoundingClientRect() : null;
      return {
        box: true,
        controls: ctls.length,
        text: ctl.textContent.trim(),
        compact: ctl.classList.contains('mc-credit-compact'),
        esriLinks: Array.from(ctl.querySelectorAll('a')).filter((a) => a.getAttribute('href') === 'https://www.esri.com').map((a) => a.textContent),
        links: Array.from(ctl.querySelectorAll('a')).map((a) => a.getAttribute('href')),
        inCorner: !!ctl.closest('.leaflet-bottom.leaflet-right'),
        rect: { top: r.top, left: r.left, right: r.right, bottom: r.bottom, width: r.width, height: r.height },
        inset: { top: br.top, left: br.left, right: br.right, bottom: br.bottom, width: br.width, height: br.height },
        inside: r.width > 0 && r.height > 0 && r.left >= br.left - 1 && r.right <= br.right + 1 && r.top >= br.top - 1 && r.bottom <= br.bottom + 1,
        inViewport: r.top >= 0 && r.left >= 0 && r.bottom <= innerHeight && r.right <= innerWidth,
        fontSize: parseFloat(cs.fontSize),
        maxHeight: parseFloat(cs.maxHeight),
        overflowY: cs.overflowY,
        scrollHeight: ctl.scrollHeight,
        clientHeight: ctl.clientHeight,
        clipped: ctl.scrollHeight > ctl.clientHeight + 1 && cs.overflowY !== 'auto' && cs.overflowY !== 'scroll',
        covered,
        imageryTiles: tiles.filter((i) => /World_Imagery/.test(i.src)).length,
        osmTiles: tiles.filter((i) => /tile\.openstreetmap\.org/.test(i.src)).length,
        qrOverlap: or ? (Math.min(r.right, or.right) > Math.max(r.left, or.left) && Math.min(r.bottom, or.bottom) > Math.max(r.top, or.top)) : null,
      };
    }, sel);
  }

  // The contract every inset must meet, whatever provider it renders.
  function assertCredit(s, { label, credit, esri, tileKind }) {
    assert(s.box, label + ': the inset container never mounted');
    assert(s.controls === 1, label + ': attribution controls inside the inset: ' + s.controls);
    assert(s.compact, label + ': the credit is not the registry\'s compact control');
    assert(s.inCorner, label + ': the credit is not in the bottom-right corner');
    assert(s.text === credit, label + ': credit text\n  want: ' + credit + '\n  got:  ' + s.text);
    if (esri) {
      assert(s.esriLinks.length === 1 && s.esriLinks[0] === 'Esri',
        label + ': expected exactly one "Esri" link to ' + ESRI_HOME + ' — got ' + JSON.stringify(s.links));
    } else {
      assert(s.esriLinks.length === 0, label + ': an Esri link with no Esri provider: ' + JSON.stringify(s.links));
    }
    const rendered = tileKind === 'imagery' ? s.imageryTiles : s.osmTiles;
    assert(rendered > 0, label + ': the inset rendered 0 ' + tileKind + ' tiles, so its credit proves nothing');
    // Legible: not tiny, nothing painted over the text, nothing cut away.
    assert(s.fontSize >= 10, label + ': credit font-size ' + s.fontSize + 'px');
    assert(s.inside, label + ': the credit is not inside the inset: ' + JSON.stringify({ rect: s.rect, inset: s.inset }));
    assert(s.inViewport, label + ': the credit is outside the viewport: ' + JSON.stringify(s.rect));
    assert(s.covered.length === 0, label + ': credit covered at ' + s.covered.length + ' points, e.g. ' + s.covered.slice(0, 4).join(' '));
    assert(!s.clipped, label + ': the credit is clipped instead of scrollable (overflow-y: ' + s.overflowY + ')');
    // #756 (review of #754, finding 1): border-box with vertical padding ate
    // into the content height the cap budgeted, so a two-line credit (Esri
    // World Imagery) scrolled by exactly the padding — allowed by `clipped`
    // above, which only cares whether an existing clip is scrollable at all.
    assert(s.scrollHeight <= s.clientHeight, label + ': the credit scrolls — scrollHeight ' +
      s.scrollHeight + 'px > clientHeight ' + s.clientHeight + 'px (the cap does not count the vertical padding)');
    // Compact: inside the CSS cap, and a minority of a small inset.
    assert(Number.isFinite(s.maxHeight) && s.maxHeight > 0,
      label + ': the compact credit has no height cap (max-height: ' + s.maxHeight + ') — a long credit would grow without bound');
    assert(s.maxHeight <= 40, label + ': the cap is ' + s.maxHeight + 'px, too tall to call compact');
    assert(s.rect.height <= s.maxHeight + 6, label + ': credit ' + s.rect.height + 'px exceeds the cap ' + s.maxHeight + 'px');
    assert(s.rect.height <= s.inset.height * 0.25, label + ': credit takes ' +
      Math.round((s.rect.height / s.inset.height) * 100) + '% of the inset height');
    assert(s.rect.width <= s.inset.width * 0.75, label + ': credit takes ' +
      Math.round((s.rect.width / s.inset.width) * 100) + '% of the inset width');
    if (s.qrOverlap !== null) assert(s.qrOverlap === false, label + ': the QR overlay shares the credit\'s box');
  }

  async function openNodeDetail(page) {
    const key = await (async () => {
      try {
        const found = await page.evaluate(async () => {
          const r = await fetch('/api/nodes?limit=200');
          const d = await r.json();
          const list = Array.isArray(d) ? d : (d.nodes || []);
          const n = list.find((x) => x.lat != null && x.lon != null && x.public_key);
          return n ? n.public_key : null;
        });
        return found || NODE_KEY;
      } catch (_) { return NODE_KEY; }
    })();
    await page.goto(BASE + '/#/nodes/' + key, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#nodeFullMap .leaflet-tile-pane, #nodeMap .leaflet-tile-pane', { state: 'attached' });
    await page.waitForTimeout(1500);
    return (await page.$('#nodeFullMap')) ? '#nodeFullMap' : '#nodeMap';
  }

  // ── node detail ──────────────────────────────────────────────────────────
  for (const v of [
    { name: 'Esri World Imagery', tiles: ESRI_TILES, storage: { 'mc-light-tile-provider': 'esri-imagery' }, credit: ESRI_CREDIT, esri: true, tileKind: 'imagery' },
    { name: 'keyless OSM default', tiles: null, storage: {}, credit: OSM_CREDIT, esri: false, tileKind: 'osm' },
  ]) {
    await step(`node detail inset credits ${v.name}`, async () => {
      const { ctx, page, rec } = await newCtx({ tiles: v.tiles, storage: v.storage });
      try {
        const sel = await openNodeDetail(page);
        const s = await creditState(page, sel);
        assertCredit(s, { label: 'node detail ' + sel, credit: v.credit, esri: v.esri, tileKind: v.tileKind });
        assert(rec.errors.length === 0, 'console errors: ' + rec.errors.join(' | '));
      } finally { await ctx.close(); }
    });
  }

  // The full node page overlays the QR on the map at <=640px, in the same
  // corner as the credit (#1243); it has to clear it.
  await step('node detail inset at 390x844: the credit clears the QR overlay', async () => {
    const { ctx, page, rec } = await newCtx({
      tiles: ESRI_TILES, storage: { 'mc-light-tile-provider': 'esri-imagery' },
      viewport: { width: 390, height: 844 },
    });
    try {
      const sel = await openNodeDetail(page);
      const s = await creditState(page, sel);
      assertCredit(s, { label: 'node detail 390x844 ' + sel, credit: ESRI_CREDIT, esri: true, tileKind: 'imagery' });
      assert(rec.errors.length === 0, 'console errors: ' + rec.errors.join(' | '));
    } finally { await ctx.close(); }
  });

  // ── area-nodes map ───────────────────────────────────────────────────────
  // Driven through its own public entry point, as the #332 suite does: the
  // committed fixture has no estimated positions to click a row for, and the
  // tile-attach path is identical either way.
  for (const v of [
    { name: 'Esri World Imagery', tiles: ESRI_TILES, storage: { 'mc-light-tile-provider': 'esri-imagery' }, credit: ESRI_CREDIT, esri: true, tileKind: 'imagery' },
    { name: 'keyless OSM default', tiles: null, storage: {}, credit: OSM_CREDIT, esri: false, tileKind: 'osm' },
  ]) {
    await step(`area-nodes inset credits ${v.name}`, async () => {
      const { ctx, page, rec } = await newCtx({ tiles: v.tiles, storage: v.storage });
      try {
        await page.goto(BASE + '/#/tools/position-gaps', { waitUntil: 'domcontentloaded' });
        await page.waitForFunction(() => !!(window.AreaNodesMap && window.L));
        await page.evaluate(() => window.AreaNodesMap.open('#752 E2E area', [
          { name: 'A', lat: 56.1, lon: 10.2, approximated: true },
          { name: 'B', lat: 56.3, lon: 10.4, approximated: true },
        ]));
        await page.waitForSelector('#areaNodesMapContainer .leaflet-tile-pane', { state: 'attached' });
        await page.waitForTimeout(1500);
        const s = await creditState(page, '#areaNodesMapContainer');
        assertCredit(s, { label: 'area-nodes', credit: v.credit, esri: v.esri, tileKind: v.tileKind });
        assert(rec.errors.length === 0, 'console errors: ' + rec.errors.join(' | '));
      } finally { await ctx.close(); }
    });
  }

  // ── Analytics subpath minimap (the smallest inset, 200px) ────────────────
  await step('Analytics subpath minimap credits Esri World Imagery', async () => {
    const { ctx, page, rec } = await newCtx({
      tiles: ESRI_TILES, storage: { 'mc-light-tile-provider': 'esri-imagery' },
    });
    try {
      await page.goto(BASE + '/#/analytics', { waitUntil: 'domcontentloaded' });
      const tab = page.locator('.tab-btn[data-tab="subpaths"]');
      await tab.waitFor();
      await tab.click();
      await page.waitForSelector('tr[data-hops]');
      const rows = page.locator('tr[data-hops]');
      const total = Math.min(await rows.count(), 15);
      assert(total > 0, 'the fixture served no subpath rows to open');
      let mounted = false;
      for (let i = 0; i < total && !mounted; i++) {
        await rows.nth(i).click();
        try {
          await page.waitForSelector('#subpathMap .leaflet-tile-pane', { state: 'attached', timeout: 3000 });
          mounted = true;
        } catch (_) { /* this route has <2 located nodes — try the next */ }
      }
      assert(mounted, `no subpath route out of ${total} mounted #subpathMap`);
      await page.waitForTimeout(1500);
      const s = await creditState(page, '#subpathMap');
      assertCredit(s, { label: 'analytics subpath', credit: ESRI_CREDIT, esri: true, tileKind: 'imagery' });
      assert(rec.errors.length === 0, 'console errors: ' + rec.errors.join(' | '));
    } finally { await ctx.close(); }
  });

  // ── #756: the Home My-Mesh minimap (out of #752's scope, still uncredited
  // until now) ───────────────────────────────────────────────────────────
  // home.js hardcodes the OSM tile layer rather than going through
  // nodes.js's provider-aware helper, so it always shows the OSM credit
  // regardless of the configured provider — unlike every other inset above.
  await step('Home My-Mesh minimap credits OSM (#756)', async () => {
    const { ctx, page, rec } = await newCtx({ tiles: ESRI_TILES, storage: { 'mc-light-tile-provider': 'esri-imagery' } });
    try {
      const key = await (async () => {
        try {
          const found = await page.evaluate(async () => {
            const r = await fetch('/api/nodes?limit=200');
            const d = await r.json();
            const list = Array.isArray(d) ? d : (d.nodes || []);
            const n = list.find((x) => x.lat != null && x.lon != null && x.public_key);
            return n ? n.public_key : null;
          });
          return found || NODE_KEY;
        } catch (_) { return NODE_KEY; }
      })();
      await page.goto(BASE + '/#/home', { waitUntil: 'domcontentloaded' });
      await page.evaluate((pk) => {
        localStorage.setItem('meshcore-user-level', 'experienced');
        localStorage.setItem('meshcore-my-nodes', JSON.stringify([{ pubkey: pk, name: 'Attr756', addedAt: new Date().toISOString() }]));
      }, key);
      await page.reload({ waitUntil: 'domcontentloaded' });
      // home.js closes an open health panel on 'theme-refresh' (a full
      // renderHome), and every page load schedules exactly one of those
      // ~300ms after the theme customizer's init dispatches
      // 'theme-changed' (app.js). Clearing that one-shot window before
      // opening the panel keeps the assertions below from racing it.
      await page.waitForTimeout(1000);
      await page.waitForSelector('.my-node-card', { timeout: 10000 });
      await page.click('.my-node-card');
      await page.waitForSelector('#healthMap .leaflet-tile-pane', { state: 'attached', timeout: 8000 });
      await page.waitForTimeout(1200);
      const s = await creditState(page, '#healthMap');
      assertCredit(s, { label: 'Home My-Mesh minimap', credit: OSM_CREDIT, esri: false, tileKind: 'osm' });
      assert(rec.errors.length === 0, 'console errors: ' + rec.errors.join(' | '));
    } finally { await ctx.close(); }
  });

  // ── #735 review N2: the Live feed cap reserves the credit's height ───────
  // Differential, so it cannot pass by restating the stylesheet: forcing the
  // published credit height to 0 must raise the cap by exactly that height.
  await step('Live feed max-height reserves the credit height (740x360)', async () => {
    const { ctx, page, rec } = await newCtx({
      tiles: ESRI_TILES, storage: { 'mc-light-tile-provider': 'esri-imagery' },
      viewport: { width: 740, height: 360 },
    });
    try {
      await page.goto(BASE + '/#/live', { waitUntil: 'domcontentloaded' });
      await page.waitForSelector('#liveFeed');
      await page.waitForSelector('#liveMap .leaflet-control-attribution');
      await page.waitForTimeout(1200);
      const m = await page.evaluate(() => {
        const feed = document.getElementById('liveFeed');
        const root = document.querySelector('.live-page');
        const read = () => parseFloat(getComputedStyle(feed).maxHeight);
        const credit = parseFloat(getComputedStyle(root).getPropertyValue('--live-credit-height')) || 0;
        const measured = document.querySelector('#liveMap .leaflet-control-attribution').getBoundingClientRect().height;
        const before = read();
        // live.js publishes the measured height as an inline custom
        // property on this element, so put its own value back afterwards.
        const inline = root.style.getPropertyValue('--live-credit-height');
        root.style.setProperty('--live-credit-height', '0px');
        const after = read();
        if (inline) root.style.setProperty('--live-credit-height', inline);
        else root.style.removeProperty('--live-credit-height');
        return { credit, measured, before, after, inline, restored: read() };
      });
      assert(m.credit > 0, 'Live published --live-credit-height ' + m.credit + 'px — nothing to reserve');
      assert(Math.abs(m.credit - Math.ceil(m.measured)) <= 1, 'published ' + m.credit + 'px for a ' + m.measured + 'px credit');
      assert(m.before < 340, 'the feed cap is clamped at 340px here, so the credit term cannot be measured: ' + m.before);
      assert(Math.abs((m.after - m.before) - m.credit) <= 1,
        '.live-feed max-height does not reserve the credit: ' + m.before + 'px with a ' + m.credit +
        'px credit, ' + m.after + 'px without it (expected +' + m.credit + ')');
      assert(Math.abs(m.restored - m.before) <= 1, 'the probe left the cap changed: ' + m.restored);
      assert(rec.errors.length === 0, 'console errors: ' + rec.errors.join(' | '));
    } finally { await ctx.close(); }
  });

  await browser.close();
  console.log(`\n#752 inset attribution E2E: ${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
