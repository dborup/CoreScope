/**
 * E2E (#839): the minor map findings from the rc19 test (CS-Macmini), in a
 * real browser on the Map page at Jelling.
 *
 * Nothing leaves the machine: /api/config/client is intercepted and given a
 * map.tiles block (OSM, Esri and Dataforsyningen on, a dummy token) and the
 * dhm block the Go server sends (hillshade + contours, each crediting its own
 * dataset); /api/map/dhm/wms is a synthetic PNG; every Dataforsyningen GetMap
 * is answered in the browser; every other third-party image is a stub and
 * any other third-party request is refused.
 *
 *   F3  Natur- og Friluftskort: the first GetMap of two tiles is answered the
 *       way the upstream sometimes does — HTTP 200, application/xml, a WMS
 *       ServiceException — which Chrome refuses as an image. Each of those
 *       two tiles is fetched exactly once more, and every Natur tile in the
 *       view ends up drawn (512 px, leaflet-tile-loaded). No tile is fetched
 *       a third time.
 *   F4  The popup's commissioning date in Chromium's Intl: Danish date.
 *   F5  The layer control lists no base-map name twice; the dark twins say
 *       "(dark)", and in the dark theme the "(dark)" Skærmkort grå entry is
 *       the inverted one.
 *   F6  Skærmkort + DHM hillshade + contours at 1440x900: the credit lies
 *       outside the Map Controls panel and is hit by itself at its left,
 *       centre and right. Both DHM credits stay, each linking its own
 *       dataset (#733, #734 Q5): a guard, not a change.
 *   F10 The attribution bar takes --surface-1 / --text-muted: dark in the
 *       dark theme, light in the light theme.
 *
 * Usage:
 *   BASE_URL=http://localhost:13839 node test-issue-839-map-polish-e2e.js
 */
'use strict';
const { chromium } = require('playwright');
const zlib = require('zlib');

const BASE = process.env.BASE_URL || 'http://localhost:13839';
const TOKEN = 'e2e-839-dummy-token';
const JELLING = '#/map?lat=55.7565&lon=9.4195&zoom=15';
const NATUR_BASE = 'https://api.dataforsyningen.dk/wms/natur_friluftskort';
const DHM_PATH = '/api/map/dhm/wms';
const DHM_CREDIT = 'Indeholder data fra Klimadatastyrelsen, Danmarks Højdemodel, CC BY 4.0';
const SK_CREDIT = 'Indeholder data fra Klimadatastyrelsen og GeoDanmark, Skærmkort, CC BY 4.0';
const DATASET = { terrain: 'https://dataforsyningen.dk/data/930', contours: 'https://dataforsyningen.dk/data/926' };
const DHM_BLOCK = {
  enabled: true, url: DHM_PATH, layer: 'dhm_terraen_skyggekort',
  minZoom: 7, maxZoom: 19, bounds: [[54.4265, 7.99125], [57.778, 15.5995]],
  attribution: DHM_CREDIT,
  datasetName: 'Danmarks Højdemodel', datasetUrl: DATASET.terrain,
  licenseName: 'CC BY 4.0', licenseUrl: 'https://creativecommons.org/licenses/by/4.0/',
  overlays: [
    { id: 'terrain', minZoom: 7, maxZoom: 19, datasetName: 'Danmarks Højdemodel', datasetUrl: DATASET.terrain },
    { id: 'contours', minZoom: 14, maxZoom: 19, datasetName: 'Danmarks Højdemodel', datasetUrl: DATASET.contours },
  ],
};
const TILES = {
  providers: {
    osm: { enabled: true }, esri: { enabled: true },
    dataforsyningen: { enabled: true, token: TOKEN },
  },
};
const INVERT = 'invert(1) hue-rotate(180deg) brightness(0.9) contrast(1.05)';
// What the upstream answered in the rc19 test (HTTP 200).
const SERVICE_EXCEPTION = '<?xml version="1.0" encoding="UTF-8"?><ServiceExceptionReport version="1.3.0">' +
  '<ServiceException>msDrawMap(): Image handling error. Failed to draw layer named \'Erm_land\'.</ServiceException></ServiceExceptionReport>';

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
function png(size, rgb) {
  const raw = Buffer.alloc((size * 3 + 1) * size);
  for (let j = 0; j < size; j++) for (let i = 0; i < size; i++) raw.set(rgb, j * (size * 3 + 1) + 1 + i * 3);
  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(size, 0); ihdr.writeUInt32BE(size, 4); ihdr[8] = 8; ihdr[9] = 2;
  return Buffer.concat([Buffer.from('\x89PNG\r\n\x1a\n', 'binary'), chunk('IHDR', ihdr), chunk('IDAT', zlib.deflateSync(raw)), chunk('IEND', Buffer.alloc(0))]);
}
const WMS_512 = png(512, [236, 233, 224]);
const OTHER_256 = png(256, [200, 200, 200]);

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
  console.log(`\n=== #839 map polish against ${BASE} ===\n`);

  // failNatur: how many distinct Natur tiles get the ServiceException on
  // their first GetMap.
  async function newCtx({ theme = 'light', viewport = { width: 1440, height: 900 }, storage = {}, failNatur = 0 } = {}) {
    const ctx = await browser.newContext({ viewport });
    const rec = { natur: new Map(), failed: [], external: [], errors: [] };
    await ctx.route(BASE + '/api/config/client', async (route) => {
      const response = await route.fetch();
      const config = await response.json();
      delete config.tiles; delete config.dhm; delete config.masts; delete config.heritage;
      config.map = Object.assign({}, config.map, { tiles: TILES });
      config.dhm = DHM_BLOCK;
      await route.fulfill({ response, json: config });
    });
    await ctx.route(url => new URL(url).pathname === DHM_PATH, route =>
      route.fulfill({ status: 200, contentType: 'image/png', body: OTHER_256 }));
    await ctx.route(/^https?:\/\/(?!localhost|127\.0\.0\.1)/i, async (route) => {
      const url = route.request().url();
      if (url.indexOf(NATUR_BASE) === 0) {
        const n = (rec.natur.get(url) || 0) + 1;
        rec.natur.set(url, n);
        if (n === 1 && rec.failed.length < failNatur) {
          rec.failed.push(url);
          await route.fulfill({ status: 200, contentType: 'application/xml', body: SERVICE_EXCEPTION });
          return;
        }
        await route.fulfill({ status: 200, contentType: 'image/png', body: WMS_512 });
        return;
      }
      if (url.indexOf('https://api.dataforsyningen.dk/') === 0) {
        await route.fulfill({ status: 200, contentType: 'image/png', body: WMS_512 });
        return;
      }
      if (route.request().resourceType() !== 'image') { rec.external.push(url); await route.abort(); return; }
      await route.fulfill({ status: 200, contentType: 'image/png', body: OTHER_256 });
    });
    await ctx.addInitScript(({ theme: t, storage: st }) => {
      try {
        if (!sessionStorage.getItem('__e2e839')) {
          sessionStorage.setItem('__e2e839', '1');
          localStorage.setItem('meshcore-theme', t);
          for (const k of Object.keys(st)) localStorage.setItem(k, st[k]);
        }
      } catch (_) {}
    }, { theme, storage });
    const page = await ctx.newPage();
    page.on('pageerror', e => rec.errors.push(String(e)));
    await page.goto(`${BASE}/${JELLING}`, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('.leaflet-control-layers', { timeout: 20000 });
    await page.waitForFunction(() => window.MeshConfigReady !== undefined, null, { timeout: 10000 });
    await page.evaluate(() => window.MeshConfigReady).catch(() => {});
    await page.waitForTimeout(900);
    return { ctx, page, rec };
  }
  async function selectBase(page, id) {
    await page.evaluate((tid) => {
      const input = document.querySelector('.leaflet-control-layers-base label[data-tile-id="' + tid + '"] input');
      if (!input) throw new Error('no base layer ' + tid);
      input.click();
    }, id);
    await page.waitForTimeout(900);
  }
  const credit = (page) => page.$eval('.leaflet-control-attribution', el => el.textContent);

  await step('F3 a Natur tile answered with a ServiceException is fetched once more and drawn', async () => {
    const { ctx, page, rec } = await newCtx({ failNatur: 2 });
    try {
      await selectBase(page, 'dataforsyningen-natur-friluft');
      // The retry is 3–5 s after the error.
      await page.waitForTimeout(6500);
      assert(rec.failed.length === 2, 'expected two failed first answers, got ' + rec.failed.length);
      for (const u of rec.failed) assert(rec.natur.get(u) === 2, 'a failed tile was fetched ' + rec.natur.get(u) + ' times: ' + u);
      const most = Math.max(...rec.natur.values());
      assert(most <= 2, 'a Natur tile was fetched ' + most + ' times');
      const tiles = await page.$$eval('.leaflet-tile-pane img.leaflet-tile', (imgs, base) => imgs
        .filter(i => i.src.indexOf(base) === 0)
        .map(i => ({ ok: i.complete && i.naturalWidth === 512 && i.classList.contains('leaflet-tile-loaded'), src: i.src })), NATUR_BASE);
      assert(tiles.length >= 4, 'only ' + tiles.length + ' Natur tiles in the view');
      const blank = tiles.filter(t => !t.ok);
      assert(!blank.length, blank.length + ' of ' + tiles.length + ' Natur tiles stayed blank');
      assert(!rec.errors.length, 'page errors: ' + rec.errors.join(' | '));
    } finally { await ctx.close(); }
  });

  await step('F3 an answer that fails twice is left alone (one retry per tile)', async () => {
    const { ctx, page, rec } = await newCtx({ failNatur: 0 });
    try {
      // Every Natur GetMap fails from now on.
      await ctx.unroute(/^https?:\/\/(?!localhost|127\.0\.0\.1)/i);
      await ctx.route(/^https?:\/\/(?!localhost|127\.0\.0\.1)/i, async (route) => {
        const url = route.request().url();
        if (url.indexOf(NATUR_BASE) === 0) {
          rec.natur.set(url, (rec.natur.get(url) || 0) + 1);
          await route.fulfill({ status: 200, contentType: 'application/xml', body: SERVICE_EXCEPTION });
          return;
        }
        if (route.request().resourceType() !== 'image') { await route.abort(); return; }
        await route.fulfill({ status: 200, contentType: 'image/png', body: OTHER_256 });
      });
      await selectBase(page, 'dataforsyningen-natur-friluft');
      await page.waitForTimeout(12000);
      assert(rec.natur.size >= 4, 'only ' + rec.natur.size + ' Natur tiles asked for');
      const counts = Array.from(rec.natur.values());
      assert(counts.every(n => n === 2), 'fetch counts per tile: ' + counts.join(','));
    } finally { await ctx.close(); }
  });

  await step('F4 the popup date is the Danish date in Chromium', async () => {
    const { ctx, page } = await newCtx();
    try {
      const got = await page.evaluate(() => [
        window.MastsLayer.commissionedDate('2022-02-02T23:00:00Z'),
        window.MastsLayer.commissionedDate('2014-03-29T23:00:00Z'),
        window.MastsLayer.commissionedDate('2024-07-12T22:00:00Z'),
      ]);
      assert(got.join() === '2022-02-03,2014-03-30,2024-07-13', 'dates: ' + got.join());
    } finally { await ctx.close(); }
  });

  for (const theme of ['light', 'dark']) {
    await step(`F5 (${theme}) no base map is listed twice; the dark twins say "(dark)"`, async () => {
      const { ctx, page } = await newCtx({ theme });
      try {
        const rows = await page.$$eval('.leaflet-control-layers-base label[data-tile-id]', ls =>
          ls.map(l => ({ id: l.getAttribute('data-tile-id'), name: l.textContent.trim() })));
        const names = rows.map(r => r.name);
        const dup = names.filter((n, i) => names.indexOf(n) !== i);
        assert(!dup.length, 'listed twice: ' + dup.join(', '));
        const want = {
          'carto-voyager-dark': 'Carto Voyager (dark)', 'positron-dark': 'Carto Positron (dark)',
          'esri-imagery-dark': 'Esri World Imagery (dark)', 'osm-dark': 'OSM Standard (dark)',
          'dataforsyningen-skaermkort-graa-dark': 'Skærmkort grå (Denmark) (dark)',
          'dataforsyningen-skaermkort-graa': 'Skærmkort grå (Denmark)',
        };
        for (const id of Object.keys(want)) {
          const r = rows.find(x => x.id === id);
          assert(r && r.name === want[id], id + ' is listed as ' + (r && r.name));
        }
        if (theme === 'dark') {
          await page.click('.leaflet-control-layers-toggle').catch(() => {});
          await page.getByText('Skærmkort grå (Denmark) (dark)', { exact: true }).click();
          await page.waitForTimeout(900);
          const filter = await page.$eval('.leaflet-tile-pane', el => getComputedStyle(el).filter);
          assert(filter === INVERT, 'the (dark) entry is not inverted: ' + filter);
        }
      } finally { await ctx.close(); }
    });
  }

  // Not deduplicated on purpose: each DHM credit links its own dataset, as
  // the CC BY terms require (#733, #734 Q5). Guard that it stays so.
  await step('F6 guard: Skærmkort + hillshade + contours keep every credit, each DHM dataset linked', async () => {
    const { ctx, page } = await newCtx({ storage: { 'mc-terrain-dhm': '1', 'mc-contours-dhm': '1' } });
    try {
      await selectBase(page, 'dataforsyningen-skaermkort');
      const text = await credit(page);
      assert(text.includes(SK_CREDIT), 'no Skærmkort credit: ' + text);
      const links = await page.$$eval('.leaflet-control-attribution a', as => as.map(a => a.getAttribute('href')));
      assert(links.includes(DATASET.terrain) && links.includes(DATASET.contours), 'dataset links: ' + links.join(' '));
    } finally { await ctx.close(); }
  });

  await step('F6 the credit (Skærmkort + hillshade + contours, 1440x900) lies clear of the Map Controls panel', async () => {
    const { ctx, page } = await newCtx({ storage: { 'mc-terrain-dhm': '1', 'mc-contours-dhm': '1' } });
    try {
      await selectBase(page, 'dataforsyningen-skaermkort');
      const geo = await page.evaluate(() => {
        const a = document.querySelector('#leaflet-map .leaflet-control-attribution');
        const p = document.getElementById('mapControls');
        const ra = a.getBoundingClientRect(), rp = p.getBoundingClientRect();
        const pts = [[ra.left + 4, ra.top + ra.height / 2], [ra.left + ra.width / 2, ra.top + ra.height / 2], [ra.right - 4, ra.top + ra.height / 2], [ra.right - 4, ra.top + 3]];
        return {
          a: { l: ra.left, t: ra.top, r: ra.right, b: ra.bottom }, p: { l: rp.left, t: rp.top, r: rp.right, b: rp.bottom },
          panelOpen: !p.classList.contains('collapsed'),
          hits: pts.map(([x, y]) => { const h = document.elementFromPoint(x, y); return !!h && a.contains(h); }),
        };
      });
      assert(geo.panelOpen, 'the Map Controls panel is not open on desktop');
      const overlap = !(geo.a.r <= geo.p.l || geo.a.l >= geo.p.r || geo.a.b <= geo.p.t || geo.a.t >= geo.p.b);
      assert(!overlap, 'credit ' + JSON.stringify(geo.a) + ' overlaps the panel ' + JSON.stringify(geo.p));
      assert(geo.hits.every(Boolean), 'credit covered at ' + JSON.stringify(geo.hits));
    } finally { await ctx.close(); }
  });

  for (const theme of ['light', 'dark']) {
    await step(`F10 (${theme}) the attribution bar takes the theme's surface and muted text`, async () => {
      const { ctx, page } = await newCtx({ theme });
      try {
        const s = await page.evaluate(() => {
          const a = document.querySelector('#leaflet-map .leaflet-control-attribution');
          const probe = document.createElement('span');
          probe.style.color = 'var(--text-muted)';
          document.body.appendChild(probe);
          const muted = getComputedStyle(probe).color;
          probe.remove();
          const cs = getComputedStyle(a);
          return { bg: cs.backgroundColor, color: cs.color, muted };
        });
        // rgb(r, g, b[, a]) in 0–255, or color-mix's color(srgb r g b / a) in 0–1.
        const scale = /^color\(srgb/.test(s.bg) ? 1 : 255;
        const m = s.bg.replace(/^color\(srgb/, '').match(/[\d.]+/g).map(Number);
        const lum = (0.2126 * m[0] + 0.7152 * m[1] + 0.0722 * m[2]) / scale;
        if (theme === 'dark') assert(lum < 0.3, 'background not dark: ' + s.bg);
        else assert(lum > 0.8, 'background not light: ' + s.bg);
        assert(s.color === s.muted, 'text ' + s.color + ', --text-muted ' + s.muted);
      } finally { await ctx.close(); }
    });
  }

  await browser.close();
  console.log(`\n#839 map polish E2E: ${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
