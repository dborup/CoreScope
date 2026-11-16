/**
 * E2E (#728 M3): the "Aerial (Denmark)" orthophoto base entry.
 *
 * The CI server has no DHM token, so /api/config/client is intercepted
 * and given the dhm block a server with map.dhm.ortho.enabled sends, with
 * the real foreign-tile mask (test-fixtures/dhm-ortho-foreign-mask.json,
 * kept equal to internal/dhm's by TestOrthoForeignMaskFixture). The
 * server's ortho route is stubbed in the browser, and so is every
 * third-party tile: nothing reaches Dataforsyningen, Esri or OSM.
 *
 * Asserts, on Map and Live:
 *   1. Not offered without dhm.ortho; offered with it, on both themes,
 *      and zero ortho requests until it is picked (incl. pan and zoom).
 *   2. Picked with Esri off: only same-origin /api/map/dhm/ortho/{z}/{x}/{y}
 *      requests (no query, z 7-20), tiles drawn, the exact credit with the
 *      dataset and licence linked, no Esri request, no filter.
 *   3. Esri on: Esri World Imagery loads under the ortho layer, both
 *      credits are shown.
 *   4. Dark theme: never inverted (Auto's osm-dark is, as a probe check).
 *   5. The pick persists: ?ortho=1 and storage, a reload and Live restore
 *      it; picking Auto clears it and a reload keeps Auto.
 *   6. Outside Denmark (Esri on): inland Skåne and Schleswig, inside the
 *      coverage rectangle, and Hamburg, outside it, show Esri tiles and
 *      request no ortho tile.
 *   7. Over the whole run, no browser request carries "token" or goes to
 *      a dataforsyningen or datafordeler host.
 *   8. #728 N3: with the stub refusing cold tiles beyond a budget the way
 *      the proxy's admission bound does (busy: 1×1 PNG, no-store), each
 *      busy ortho/hillshade tile in view is fetched once more after 3-5 s
 *      and the view ends with no blank tile; no tile is fetched more than
 *      twice; a busy tile panned out of view is not fetched again.
 *
 * Usage:
 *   BASE_URL=http://localhost:13728 node test-issue-728-ortho-base-e2e.js
 * Optional: SCREENSHOT_DIR=<dir> writes small JPEG screenshots (stub tiles).
 */
'use strict';
const { chromium } = require('playwright');
const zlib = require('zlib');
const fs = require('fs');
const path = require('path');

const BASE = process.env.BASE_URL || 'http://localhost:13800';
const SHOT_DIR = process.env.SCREENSHOT_DIR || '';
const ORTHO_PATH = '/api/map/dhm/ortho/';
const ORTHO_RE = /^\/api\/map\/dhm\/ortho\/(\d+)\/(\d+)\/(\d+)$/;
const IMAGERY_RE = /World_Imagery/;
const MASK = JSON.parse(fs.readFileSync(path.join(__dirname, 'test-fixtures', 'dhm-ortho-foreign-mask.json'), 'utf8'));
// What cmd/server/dhm_proxy.go sends with map.dhm.ortho.enabled.
const ORTHO_BLOCK = {
  url: '/api/map/dhm/ortho/{z}/{x}/{y}', minZoom: 7, maxZoom: 20,
  bounds: [[54.4265, 7.99125], [57.778, 15.5995]],
  attribution: 'Indeholder data fra GeoDanmark, Ortofoto forår, CC BY 4.0',
  datasetName: 'Ortofoto forår', datasetUrl: 'https://dataforsyningen.dk/data/981',
  licenseName: 'CC BY 4.0', licenseUrl: 'https://creativecommons.org/licenses/by/4.0/',
  foreignMask: MASK,
};
const DHM_BLOCK = {
  enabled: true, url: '/api/map/dhm/wms', layer: 'dhm_terraen_skyggekort',
  minZoom: 7, maxZoom: 19, bounds: [[54.4265, 7.99125], [57.778, 15.5995]],
  attribution: 'Indeholder data fra Klimadatastyrelsen, Danmarks Højdemodel, CC BY 4.0',
  datasetName: 'Danmarks Højdemodel', datasetUrl: 'https://dataforsyningen.dk/data/930',
  licenseName: 'CC BY 4.0', licenseUrl: 'https://creativecommons.org/licenses/by/4.0/',
  profile: false, share: false,
};
const ESRI_CREDIT = 'Powered by Esri | Tiles © Esri — Source: Esri, Vantor, Earthstar Geographics, and the GIS User Community';
const ESRI_ON = { providers: { esri: { enabled: true } } };
const VIEWS = {
  copenhagen: { lat: 55.68, lon: 12.57, zoom: 12 },
  // Zealand west, the Øresund, Skåne east: at z10 the inland Skåne tiles
  // are wholly marked, so Esri shows there and ortho over Denmark.
  oresund: { lat: 55.75, lon: 13.0, zoom: 10 },
  // Inland, so every tile in a 900x600 view is marked in the mask.
  skane: { lat: 55.85, lon: 13.66, zoom: 12 },
  schleswig: { lat: 54.665, lon: 9.40, zoom: 12 },
  hamburg: { lat: 53.55, lon: 9.99, zoom: 10 },
};

// ── Stub tiles ──────────────────────────────────────────────────────────────
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
// W×H RGB PNG; fn(i, j) -> [r, g, b].
function png(fn, W = 256, H = 256) {
  const raw = Buffer.alloc((W * 3 + 1) * H);
  for (let j = 0; j < H; j++) {
    raw[j * (W * 3 + 1)] = 0;
    for (let i = 0; i < W; i++) {
      const v = fn(i, j), o = j * (W * 3 + 1) + 1 + i * 3;
      raw[o] = v[0]; raw[o + 1] = v[1]; raw[o + 2] = v[2];
    }
  }
  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(W, 0); ihdr.writeUInt32BE(H, 4); ihdr[8] = 8; ihdr[9] = 2;
  return Buffer.concat([Buffer.from('\x89PNG\r\n\x1a\n', 'binary'), chunk('IHDR', ihdr), chunk('IDAT', zlib.deflateSync(raw)), chunk('IEND', Buffer.alloc(0))]);
}
// Ortho stand-in: warm fields with hedges, so it reads apart from Esri's.
const ORTHO_STUB = png((i, j) => {
  if (i % 64 < 2 || j % 64 < 2) return [60, 82, 44];
  const f = ((i >> 6) * 5 + (j >> 6) * 3) % 4;
  return [[176, 160, 96], [132, 150, 78], [196, 182, 120], [110, 132, 70]][f];
});
// Esri stand-in: cool grey-blue patchwork.
const ESRI_STUB = png((i, j) => {
  const f = ((i >> 5) * 7 + (j >> 5) * 13) % 3;
  return [[92, 108, 128], [110, 124, 140], [80, 96, 116]][f];
});
const GREY_STUB = png(() => [200, 200, 200]);
const TERRAIN_STUB = png((i, j) => { const v = 150 + Math.round(70 * Math.sin(i / 19) * Math.cos(j / 23)); return [v, v, v]; });

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

// Every request of the whole run, for the token and host checks (7).
const allRequests = [];

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #728 M3 ortho base entry against ${BASE} ===\n`);

  // ortho: false leaves dhm.ortho out (map.dhm on, map.dhm.ortho off).
  async function newCtx({ tiles = {}, ortho = true, theme = 'light', storage = {}, viewport = { width: 900, height: 600 } } = {}) {
    const ctx = await browser.newContext({ viewport });
    const rec = { ortho: [], imagery: [], osm: [], terrain: [], errors: [] };
    await ctx.route(BASE + '/api/config/client', async (route) => {
      const response = await route.fetch();
      const config = await response.json();
      delete config.tiles;
      config.map = Object.assign({}, config.map, { tiles });
      config.dhm = Object.assign({}, DHM_BLOCK, ortho ? { ortho: ORTHO_BLOCK } : {});
      await route.fulfill({ response, json: config });
    });
    await ctx.route(url => new URL(url).pathname.startsWith(ORTHO_PATH), async (route) => {
      rec.ortho.push(new URL(route.request().url()));
      await route.fulfill({ status: 200, contentType: 'image/png', headers: { 'X-CoreScope-Tile': 'miss' }, body: ORTHO_STUB });
    });
    await ctx.route(url => new URL(url).pathname === DHM_BLOCK.url, async (route) => {
      rec.terrain.push(route.request().url());
      await route.fulfill({ status: 200, contentType: 'image/png', body: TERRAIN_STUB });
    });
    await ctx.route(/^https?:\/\/(?!localhost|127\.0\.0\.1)/i, async (route) => {
      const url = route.request().url();
      if (route.request().resourceType() !== 'image') { await route.abort(); return; }
      if (IMAGERY_RE.test(url)) rec.imagery.push(url);
      if (/tile\.openstreetmap\.org/.test(url)) rec.osm.push(url);
      await route.fulfill({ status: 200, contentType: 'image/png', body: IMAGERY_RE.test(url) ? ESRI_STUB : GREY_STUB });
    });
    await ctx.addInitScript(({ theme: t, storage: st }) => {
      try {
        if (!sessionStorage.getItem('__e2e728m3')) {
          sessionStorage.setItem('__e2e728m3', '1');
          localStorage.setItem('meshcore-theme', t);
          for (const k of Object.keys(st)) localStorage.setItem(k, st[k]);
        }
      } catch (_) {}
      const apply = () => document.documentElement && document.documentElement.setAttribute('data-theme', t);
      apply();
      document.addEventListener('DOMContentLoaded', apply);
    }, { theme, storage });
    const page = await ctx.newPage();
    page.on('request', r => allRequests.push(r.url()));
    page.on('console', m => { if (m.type() === 'error') rec.errors.push(m.text()); });
    page.on('pageerror', e => rec.errors.push(String(e)));
    return { ctx, page, rec };
  }

  async function waitReady(page) {
    await page.waitForSelector('.leaflet-control-layers', { timeout: 20000 });
    await page.waitForFunction(() => window.MeshConfigReady !== undefined, null, { timeout: 10000 });
    await page.evaluate(() => window.MeshConfigReady).catch(() => {});
    await page.waitForTimeout(1200);
  }
  async function gotoMap(page, hash, view = VIEWS.copenhagen, extra = '') {
    await page.goto(`${BASE}/${hash}?lat=${view.lat}&lon=${view.lon}&zoom=${view.zoom}${extra}`, { waitUntil: 'domcontentloaded' });
    await waitReady(page);
  }
  const offeredIds = (page) => page.$$eval('.leaflet-control-layers-base label[data-tile-id]', ls => ls.map(l => l.getAttribute('data-tile-id')));
  // #845: the base maps sit in two lists (the base list and the Denmark
  // group's), one radio group across both.
  const checkedId = (page) => page.evaluate(() => {
    const i = document.querySelector('.leaflet-control-layers-base input:checked');
    const l = i && i.closest('label');
    return l ? l.getAttribute('data-tile-id') : null;
  });
  async function selectBase(page, id) {
    await page.evaluate((tid) => {
      const input = document.querySelector('.leaflet-control-layers-base label[data-tile-id="' + tid + '"] input');
      if (!input) throw new Error('no base layer ' + tid);
      input.click();
    }, id);
    await page.waitForTimeout(1200);
  }
  async function state(page) {
    return page.evaluate(() => {
      const pane = document.querySelector('.leaflet-tile-pane');
      const attr = document.querySelector('.leaflet-control-attribution');
      const loaded = (sel) => Array.from(document.querySelectorAll(sel)).filter(i => i.complete && i.naturalWidth === 256);
      const zOf = (el) => (el ? Number(getComputedStyle(el).zIndex) : null);
      const esriTile = Array.from(document.querySelectorAll('.leaflet-tile-pane img.leaflet-tile')).find(i => /World_Imagery/.test(i.src));
      const orthoTile = document.querySelector('.mc-ortho-tiles img.leaflet-tile');
      return {
        filter: pane ? getComputedStyle(pane).filter : null,
        orthoTiles: loaded('.mc-ortho-tiles img.leaflet-tile').length,
        imageryTiles: loaded('.leaflet-tile-pane img.leaflet-tile').filter(i => /World_Imagery/.test(i.src)).length,
        // Stacking is by pane: the ortho pane must hold the ortho tiles and
        // sit above the tile pane that holds Esri's, and below the hillshade.
        zOrtho: orthoTile && orthoTile.closest('.mc-ortho-pane') ? zOf(orthoTile.closest('.mc-ortho-pane')) : null,
        zEsri: esriTile && esriTile.closest('.leaflet-tile-pane') ? zOf(esriTile.closest('.leaflet-tile-pane')) : null,
        zTerrain: zOf(document.querySelector('.mc-terrain-pane')),
        attribution: attr ? attr.textContent : '',
        links: attr ? Array.from(attr.querySelectorAll('a')).map(a => ({ text: a.textContent, href: a.getAttribute('href'), target: a.getAttribute('target'), rel: a.getAttribute('rel') })) : [],
        label: (() => { const l = document.querySelector('.leaflet-control-layers-base label[data-tile-id="dhm-ortho"]'); return l ? l.textContent.trim() : null; })(),
        hash: location.hash,
        stored: localStorage.getItem('mc-ortho-base'),
      };
    });
  }
  async function pan(page, dx, dy) {
    const b = await page.$eval('.leaflet-container', el => { const r = el.getBoundingClientRect(); return { x: r.x + r.width / 2, y: r.y + r.height / 2 }; });
    await page.mouse.move(b.x, b.y);
    await page.mouse.down();
    await page.mouse.move(b.x + dx, b.y + dy, { steps: 8 });
    await page.mouse.up();
    await page.waitForTimeout(800);
  }
  async function zoomIn(page) { await page.click('.leaflet-control-zoom-in'); await page.waitForTimeout(1000); }
  async function shot(page, name) {
    if (!SHOT_DIR) return;
    await page.screenshot({ path: path.join(SHOT_DIR, name), type: 'jpeg', quality: 55 });
  }
  // Every ortho request is the same-origin path shape, nothing else.
  function checkOrthoUrls(urls) {
    for (const u of urls) {
      assert(u.origin === new URL(BASE).origin, 'ortho request to another origin: ' + u.href);
      assert(u.search === '', 'ortho request with a query: ' + u.href);
      const m = ORTHO_RE.exec(u.pathname);
      assert(m, 'unexpected ortho path: ' + u.pathname);
      const z = +m[1], x = +m[2], y = +m[3];
      assert(z >= 7 && z <= 20 && x < 2 ** z && y < 2 ** z, 'ortho tile out of range: ' + u.pathname);
    }
  }
  function assertOrthoCredit(s, label) {
    assert(s.attribution.includes(ORTHO_BLOCK.attribution), label + ': ortho credit missing: ' + JSON.stringify(s.attribution));
    const ds = s.links.filter(l => l.href === ORTHO_BLOCK.datasetUrl);
    const lic = s.links.filter(l => l.href === ORTHO_BLOCK.licenseUrl);
    assert(ds.length === 1 && ds[0].text === ORTHO_BLOCK.datasetName, label + ': dataset link ' + JSON.stringify(ds));
    assert(lic.length === 1 && lic[0].text === ORTHO_BLOCK.licenseName, label + ': licence link ' + JSON.stringify(lic));
    for (const l of ds.concat(lic)) assert(l.target === '_blank' && /noopener/.test(l.rel || ''), label + ': link attrs ' + JSON.stringify(l));
  }

  // 1 — offered only with dhm.ortho; nothing requested until picked
  for (const p of [{ name: 'Map', hash: '#/map' }, { name: 'Live', hash: '#/live' }]) {
    await step(`${p.name}: map.dhm.ortho off → no Aerial entry and no ortho request`, async () => {
      const { ctx, page, rec } = await newCtx({ ortho: false });
      try {
        await gotoMap(page, p.hash);
        const ids = await offeredIds(page);
        assert(ids.includes('__auto__'), 'control not built: ' + ids.join(','));
        assert(!ids.includes('dhm-ortho'), 'Aerial offered without dhm.ortho');
        assert(rec.ortho.length === 0, 'ortho requests: ' + rec.ortho.length);
      } finally { await ctx.close(); }
    });
    for (const theme of ['light', 'dark']) {
      await step(`${p.name} (${theme}): offered as "Aerial photo" (#845), zero ortho requests until picked (pan + zoom)`, async () => {
        const { ctx, page, rec } = await newCtx({ theme, tiles: ESRI_ON });
        try {
          await gotoMap(page, p.hash);
          const ids = await offeredIds(page);
          assert(ids.filter(id => id === 'dhm-ortho').length === 1, 'entries: ' + ids.join(','));
          assert((await state(page)).label === 'Aerial photo', 'label ' + (await state(page)).label);
          await pan(page, 220, 0);
          await zoomIn(page);
          await pan(page, -120, 80);
          assert(rec.ortho.length === 0, 'ortho requests while not picked: ' + rec.ortho.length);
          assert(rec.imagery.length === 0, 'Esri imagery requests while not picked: ' + rec.imagery.length);
          assert(rec.errors.length === 0, 'console errors: ' + rec.errors.join(' | '));
        } finally { await ctx.close(); }
      });
    }
  }

  // 2 — picked, Esri off: ortho alone, same-origin, credited, not inverted
  for (const p of [{ name: 'Map', hash: '#/map' }, { name: 'Live', hash: '#/live' }]) {
    await step(`${p.name}: picked with Esri off → same-origin ortho tiles only, exact credit, no Esri`, async () => {
      const { ctx, page, rec } = await newCtx();
      try {
        await gotoMap(page, p.hash);
        await selectBase(page, 'dhm-ortho');
        await pan(page, 150, 60);
        await zoomIn(page);
        const s = await state(page);
        assert(rec.ortho.length > 0, 'no ortho tile requested');
        checkOrthoUrls(rec.ortho);
        assert(s.orthoTiles > 0, 'no ortho tile drawn');
        assert(rec.imagery.length === 0, 'Esri imagery requested with Esri off: ' + rec.imagery.length);
        assertOrthoCredit(s, p.name);
        assert(!/Esri/.test(s.attribution), 'Esri credit with Esri off: ' + s.attribution);
        assert(s.filter === 'none', 'tile pane filter ' + s.filter);
        assert(rec.errors.length === 0, 'console errors: ' + rec.errors.join(' | '));
      } finally { await ctx.close(); }
    });
  }

  // 3 — Esri on: Esri under ortho, both credits
  await step('Map: picked with Esri on → Esri imagery under the ortho layer, both credits', async () => {
    const { ctx, page, rec } = await newCtx({ tiles: ESRI_ON });
    try {
      await gotoMap(page, '#/map');
      await selectBase(page, 'dhm-ortho');
      const s = await state(page);
      assert(rec.ortho.length > 0 && rec.imagery.length > 0, 'requests: ortho ' + rec.ortho.length + ', Esri ' + rec.imagery.length);
      checkOrthoUrls(rec.ortho);
      assert(s.orthoTiles > 0 && s.imageryTiles > 0, 'drawn: ortho ' + s.orthoTiles + ', Esri ' + s.imageryTiles);
      assert(s.zOrtho > s.zEsri, 'ortho layer z ' + s.zOrtho + ' not above Esri ' + s.zEsri);
      assertOrthoCredit(s, 'Map');
      assert(s.attribution.includes(ESRI_CREDIT), 'Esri credit missing: ' + s.attribution);
    } finally { await ctx.close(); }
  });

  // 4 — dark theme: never inverted
  for (const p of [{ name: 'Map', hash: '#/map' }, { name: 'Live', hash: '#/live' }]) {
    await step(`${p.name} (dark): Auto is inverted, Aerial is not`, async () => {
      const { ctx, page } = await newCtx({ theme: 'dark', tiles: ESRI_ON });
      try {
        await gotoMap(page, p.hash);
        const before = await state(page);
        assert(/invert/.test(before.filter), 'probe: osm-dark should be inverted, filter ' + before.filter);
        await selectBase(page, 'dhm-ortho');
        assert((await state(page)).filter === 'none', 'filter with Aerial: ' + (await state(page)).filter);
        // A theme flip while Aerial is on keeps it uninverted.
        await page.evaluate(() => document.documentElement.setAttribute('data-theme', 'light'));
        await page.evaluate(() => document.documentElement.setAttribute('data-theme', 'dark'));
        await page.waitForTimeout(300);
        assert((await state(page)).filter === 'none', 'filter after a theme flip: ' + (await state(page)).filter);
      } finally { await ctx.close(); }
    });
  }

  // 5 — the pick persists per viewer and in the link
  await step('Map → reload → Live: the pick is stored, linked, restored; Auto clears it', async () => {
    const { ctx, page, rec } = await newCtx({ tiles: ESRI_ON });
    try {
      await gotoMap(page, '#/map');
      await selectBase(page, 'dhm-ortho');
      let s = await state(page);
      assert(s.stored === '1', 'stored ' + s.stored);
      assert(/[?&]ortho=1(&|$)/.test(s.hash), 'hash ' + s.hash);
      await page.reload({ waitUntil: 'domcontentloaded' });
      await waitReady(page);
      rec.ortho.length = 0;
      await pan(page, 100, 0);
      assert((await checkedId(page)) === 'dhm-ortho', 'after reload checked: ' + (await checkedId(page)));
      assert(rec.ortho.length > 0, 'no ortho request after reload');
      assert((await state(page)).orthoTiles > 0, 'no ortho tile drawn after reload');
      // Live, in the same browser: restored from storage.
      await gotoMap(page, '#/live');
      assert((await checkedId(page)) === 'dhm-ortho', 'Live checked: ' + (await checkedId(page)));
      assert((await state(page)).orthoTiles > 0, 'no ortho tile on Live');
      // Auto clears it, and a reload stays on Auto.
      await selectBase(page, '__auto__');
      s = await state(page);
      assert(s.stored === '0' && !/ortho=/.test(s.hash), 'after Auto: stored ' + s.stored + ', hash ' + s.hash);
      await page.reload({ waitUntil: 'domcontentloaded' });
      await waitReady(page);
      rec.ortho.length = 0;
      await pan(page, 100, 0);
      assert((await checkedId(page)) === '__auto__', 'after reload checked: ' + (await checkedId(page)));
      assert(rec.ortho.length === 0, 'ortho requests on Auto after reload: ' + rec.ortho.length);
    } finally { await ctx.close(); }
  });

  await step('A shared link with ?ortho=1 opens on Aerial for a new viewer', async () => {
    const { ctx, page } = await newCtx({ tiles: ESRI_ON });
    try {
      await gotoMap(page, '#/map', VIEWS.copenhagen, '&ortho=1');
      assert((await checkedId(page)) === 'dhm-ortho', 'checked ' + (await checkedId(page)));
      assert((await state(page)).stored === '1', 'not saved');
    } finally { await ctx.close(); }
  });

  // 6 — outside Denmark: Esri shows, no ortho tile is requested
  for (const [where, view] of [['inland Skåne', VIEWS.skane], ['inland Schleswig', VIEWS.schleswig], ['Hamburg (outside the bounds)', VIEWS.hamburg]]) {
    for (const p of [{ name: 'Map', hash: '#/map' }, { name: 'Live', hash: '#/live' }]) {
      await step(`${p.name}, ${where}: Esri imagery drawn, zero ortho requests`, async () => {
        const { ctx, page, rec } = await newCtx({ tiles: ESRI_ON, storage: { 'mc-ortho-base': '1' } });
        try {
          await gotoMap(page, p.hash, view);
          assert((await checkedId(page)) === 'dhm-ortho', 'Aerial not restored: ' + (await checkedId(page)));
          // Picked again in place, and moved a little within the region.
          await selectBase(page, '__auto__');
          await selectBase(page, 'dhm-ortho');
          await pan(page, 40, -30);
          const s = await state(page);
          assert(s.imageryTiles > 0, 'no Esri tile drawn');
          assert(rec.ortho.length === 0, rec.ortho.length + ' ortho requests, e.g. ' + rec.ortho.slice(0, 3).map(u => u.pathname).join(' '));
        } finally { await ctx.close(); }
      });
    }
  }

  await step('Øresund edge: ortho over Denmark, Esri over inland Skåne (screenshots light and dark)', async () => {
    for (const theme of ['light', 'dark']) {
      const { ctx, page, rec } = await newCtx({ theme, tiles: ESRI_ON, storage: { 'mc-ortho-base': '1', 'mc-terrain-dhm': '1' }, viewport: { width: 960, height: 540 } });
      try {
        await gotoMap(page, '#/map', VIEWS.oresund);
        await page.click('#mapControlsToggle').catch(() => {});
        await page.waitForTimeout(400);
        const s = await state(page);
        assert(s.orthoTiles > 0 && s.imageryTiles > 0, theme + ': ortho ' + s.orthoTiles + ', Esri ' + s.imageryTiles);
        assert(s.zEsri < s.zOrtho && s.zOrtho < s.zTerrain, theme + ': panes Esri ' + s.zEsri + ' < ortho ' + s.zOrtho + ' < terrain ' + s.zTerrain);
        // No requested ortho tile is one the mask marks.
        for (const u of rec.ortho) {
          const [z, x, y] = ORTHO_RE.exec(u.pathname).slice(1).map(Number);
          if (z < MASK.z) continue;
          const c = (x >> (z - MASK.z)) - MASK.x0, r = (y >> (z - MASK.z)) - MASK.y0;
          if (c < 0 || r < 0 || c >= MASK.cols || r >= MASK.rows) continue;
          const i = r * MASK.cols + c;
          assert(!((Buffer.from(MASK.bits, 'base64')[i >> 3] >> (i & 7)) & 1), 'a marked tile was requested: ' + u.pathname);
        }
        await shot(page, 'ortho-oresund-' + theme + '.jpg');
      } finally { await ctx.close(); }
    }
  });

  // 8 — #728 N3: busy tiles are retried once, in view, without a storm.
  // The stub plays the proxy's admission bound (#744): the cold tiles
  // `refuse(i)` picks (i counts first requests, ortho and hillshade
  // together) come back busy (1×1 transparent PNG, no-store), as the
  // server answers them. Retries are answered with real tiles. A 1100×720
  // view at z12 is 24 tiles per layer.
  const TRANSPARENT_1PX = Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==', 'base64');
  async function busyCtx(opts, refuse) {
    const c = await newCtx(opts);
    const log = []; // { path, t, busy }
    const seen = new Map();
    let cold = 0;
    const t0 = Date.now();
    const answer = async (route, body) => {
      const p = new URL(route.request().url()).pathname + new URL(route.request().url()).search;
      const n = (seen.get(p) || 0) + 1;
      seen.set(p, n);
      const busy = n === 1 && refuse(cold++);
      log.push({ path: p, t: Date.now() - t0, busy, n });
      if (busy) await route.fulfill({ status: 200, contentType: 'image/png', headers: { 'Cache-Control': 'no-store', 'X-CoreScope-Tile': 'busy' }, body: TRANSPARENT_1PX });
      else await route.fulfill({ status: 200, contentType: 'image/png', headers: { 'Cache-Control': 'public, max-age=86400', 'X-CoreScope-Tile': 'miss' }, body });
    };
    // Registered after newCtx's routes, so these win.
    await c.ctx.route(url => new URL(url).pathname.startsWith(ORTHO_PATH), route => answer(route, ORTHO_STUB));
    await c.ctx.route(url => new URL(url).pathname === DHM_BLOCK.url, route => answer(route, TERRAIN_STUB));
    return Object.assign(c, { log, seen, t0 });
  }
  const placeholders = (page) => page.evaluate(() =>
    Array.from(document.querySelectorAll('.mc-ortho-tiles img.leaflet-tile, .mc-terrain-tiles img.leaflet-tile'))
      .filter(i => i.complete && i.naturalWidth === 1).length);

  await step('N3: ortho + hillshade cold, some tiles busy → each retried once after ≥3 s, all real after, no storm', async () => {
    // Every second cold tile, so both layers have busy tiles (about half
    // the view, as with ~120 tiles against 64 slots).
    const { ctx, page, log } = await busyCtx({ tiles: ESRI_ON, storage: { 'mc-ortho-base': '1', 'mc-terrain-dhm': '1' }, viewport: { width: 1100, height: 720 } }, i => i % 2 === 1);
    try {
      await gotoMap(page, '#/map', VIEWS.copenhagen);
      const busy = log.filter(e => e.busy);
      const busyOrtho = busy.filter(e => e.path.startsWith(ORTHO_PATH)).length;
      assert(busyOrtho > 0 && busy.length - busyOrtho > 0, 'busy tiles: ortho ' + busyOrtho + ', hillshade ' + (busy.length - busyOrtho));
      const before = await placeholders(page);
      assert(before > 0, 'no blank tile before the retries');
      await page.waitForTimeout(5500);
      const after = await placeholders(page);
      const first = new Map();
      for (const e of log) if (e.n === 1) first.set(e.path, e.t);
      const retries = log.filter(e => e.n === 2);
      const counts = Array.from(new Set(log.map(e => e.path))).map(p => log.filter(e => e.path === p).length);
      const gaps = retries.map(e => e.t - first.get(e.path));
      const window = retries.length ? Math.max(...retries.map(e => e.t)) - Math.min(...retries.map(e => e.t)) : 0;
      console.log(`       ${first.size} tiles, ${busy.length} busy (${busyOrtho} ortho, ${busy.length - busyOrtho} hillshade), ${retries.length} retries, max ${Math.max(...counts)} requests per tile, retry gap ${Math.min(...gaps)}–${Math.max(...gaps)} ms, retries spread over ${window} ms, blank ${before} → ${after}`);
      assert(Math.max(...counts) <= 2, 'a tile was requested ' + Math.max(...counts) + ' times');
      assert(retries.length <= busy.length, retries.length + ' retries for ' + busy.length + ' busy tiles');
      assert(retries.every(e => first.has(e.path) && log.find(f => f.path === e.path && f.n === 1).busy), 'a real tile was retried');
      assert(Math.min(...gaps) >= 2900, 'a retry came after ' + Math.min(...gaps) + ' ms');
      assert(Math.max(...gaps) <= 5600, 'a retry came after ' + Math.max(...gaps) + ' ms');
      assert(after === 0, after + ' tiles still blank after the retries');
    } finally { await ctx.close(); }
  });

  await step('N3: a busy tile that left the view before the delay is not retried; one still in view is', async () => {
    const { ctx, page, log } = await busyCtx({ tiles: ESRI_ON, storage: { 'mc-ortho-base': '1' }, viewport: { width: 1100, height: 720 } }, i => i >= 8);
    try {
      await gotoMap(page, '#/map', VIEWS.copenhagen);
      const busyBefore = new Set(log.filter(e => e.busy).map(e => e.path));
      assert(busyBefore.size > 0, 'the stub refused no tile');
      // About two tiles west, at once, held still before the release so
      // Leaflet adds no inertia: busy tiles on the east side leave the view
      // but stay loaded (Leaflet's keepBuffer), so only the in-view test
      // stops them; the tiles coming in from the west are refused too.
      const b = await page.$eval('.leaflet-container', el => { const r = el.getBoundingClientRect(); return { x: r.x + r.width / 2, y: r.y + r.height / 2 }; });
      await page.mouse.move(b.x, b.y);
      await page.mouse.down();
      await page.mouse.move(b.x + 560, b.y, { steps: 20 });
      await page.waitForTimeout(400);
      await page.mouse.up();
      await page.waitForTimeout(5500);
      const busyPaths = new Set(log.filter(e => e.busy).map(e => e.path));
      const inView = await page.evaluate((paths) => {
        const box = document.querySelector('.leaflet-container').getBoundingClientRect();
        const out = {};
        for (const img of document.querySelectorAll('.mc-ortho-tiles img.leaflet-tile')) {
          const p = new URL(img.src).pathname;
          if (!paths.includes(p)) continue;
          const r = img.getBoundingClientRect();
          out[p] = r.right > box.left && r.left < box.right && r.bottom > box.top && r.top < box.bottom;
        }
        return out;
      }, Array.from(busyPaths));
      const retried = new Set(log.filter(e => e.n === 2).map(e => e.path));
      const outOfView = Array.from(busyPaths).filter(p => !inView[p]);
      console.log(`       ${busyPaths.size} busy (${busyBefore.size} before the pan), ${outOfView.length} out of view after it (${outOfView.filter(p => p in inView).length} still loaded), ${retried.size} retried`);
      assert(outOfView.filter(p => p in inView).length > 0, 'no busy tile left the view while staying loaded');
      for (const p of outOfView) assert(!retried.has(p), 'out-of-view tile retried: ' + p);
      assert(Array.from(retried).every(p => inView[p]), 'a retried tile is not in view');
      assert(retried.size > 0, 'no in-view busy tile was retried');
    } finally { await ctx.close(); }
  });

  // 7 — over the whole run: no token, no Dataforsyningen/Datafordeler host
  await step('No browser request carried "token" or went to dataforsyningen / datafordeler', async () => {
    assert(allRequests.length > 100, 'too few requests recorded: ' + allRequests.length);
    const bad = allRequests.filter(u => /token/i.test(u) || /dataforsyningen|datafordeler/i.test(new URL(u).hostname));
    assert(bad.length === 0, bad.length + ' requests, e.g. ' + bad.slice(0, 3).join(' '));
  });

  await browser.close();
  console.log(`\n=== #728 M3 ortho base: ${passed} passed, ${failed} failed ===`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
