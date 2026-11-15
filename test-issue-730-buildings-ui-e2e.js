/**
 * E2E (#730 M3): buildings versus trees in the Map page's Path profile.
 *
 * Like the other DHM suites, the CI server has no map.dhm config and no
 * token, so /api/config/client is intercepted and given the dhm block the
 * Go server sends with map.dhm.profile.buildings on (dhm.buildings, the
 * footprints' credit parts), and /api/terrain/profile is answered with a
 * synthetic profile in the shape M2 answers. No DHM, INSPIRE or
 * third-party service is ever contacted.
 *
 * Asserts, in light and dark:
 *   1. a Dokk1-like path (#729): the surface chip says "Buildings: Blocked",
 *      the roof is drawn in --terrain-building, the legend names both
 *      classes, the hover readout says "(building)" on the roof, and the
 *      footer carries the footprints' credit with its four links;
 *   2. a forest path: "Trees/other: Marginal", the canopy in
 *      --terrain-vegetation, no building;
 *   3. a partial classification keeps the generic chip and shows the muted
 *      note; an unavailable one shows neither, nor a legend, and the DHM
 *      credit;
 *   4. axe is clean on the panel;
 * and:
 *   5. mobile landscape (740x360, touch): the chart and the chips stay in
 *      view with the panel open, and the credit links are 44 px targets
 *      hit by themselves;
 *   6. no request leaves localhost, and no console error is logged.
 *
 * SHOTS_DIR, when set, receives small JPG screenshots of the panel.
 *
 * Usage:
 *   BASE_URL=http://localhost:13581 node test-issue-730-buildings-ui-e2e.js
 */
'use strict';
const path = require('path');
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const SHOTS = process.env.SHOTS_DIR || '';
const PROFILE_PATH = '/api/terrain/profile';
const DHM_PATH = '/api/map/dhm/wms';
const PANEL = '[data-testid="tp-panel"]';

// internal/dhm/dhm.go and internal/buildings/credit.go, as cmd/server
// sends them.
const DHM_ATTRIBUTION = 'Indeholder data fra Klimadatastyrelsen, Danmarks Højdemodel, CC BY 4.0';
const DHM_DERIVED = DHM_ATTRIBUTION + ' — data bearbejdet';
const BLD_CREDIT = 'Indeholder data fra Klimadatastyrelsen, Danmarks Højdemodel og INSPIRE – Bygninger (GeoDanmark, @geodanmark), CC BY 4.0 — data bearbejdet';
const LINKS = [
  ['tp-credit-dataset', 'Danmarks Højdemodel', 'https://dataforsyningen.dk/data/930'],
  ['tp-credit-buildings-dataset', 'INSPIRE – Bygninger', 'https://dataforsyningen.dk/data/2677'],
  ['tp-credit-buildings-source', 'GeoDanmark', 'https://www.geodanmark.dk/home/vejledninger/vilkaar-for-data-anvendelse/'],
  ['tp-credit-link', 'CC BY 4.0', 'https://creativecommons.org/licenses/by/4.0/'],
];
const DHM_BLOCK = {
  enabled: true, url: DHM_PATH, layer: 'dhm_terraen_skyggekort',
  minZoom: 7, maxZoom: 19, profile: true,
  bounds: [[54.4265, 7.99125], [57.778, 15.5995]],
  attribution: DHM_ATTRIBUTION,
  datasetName: 'Danmarks Højdemodel', datasetUrl: 'https://dataforsyningen.dk/data/930',
  licenseName: 'CC BY 4.0', licenseUrl: 'https://creativecommons.org/licenses/by/4.0/',
  attributionDerived: DHM_DERIVED,
  buildings: {
    credit: BLD_CREDIT,
    datasetName: 'INSPIRE – Bygninger', datasetUrl: 'https://dataforsyningen.dk/data/2677',
    sourceName: 'GeoDanmark', sourceUrl: 'https://www.geodanmark.dk/home/vejledninger/vilkaar-for-data-anvendelse/',
  },
};
const PARTIAL_NOTE = 'Buildings could not be told apart from trees on part of this path.';

// The reference paths. B picks the stub's answer.
const DOKK1_A = '56.15360,10.21410', DOKK1_B = '56.15147,10.21420';
const FOREST_A = '56.80000,9.85000', FOREST_B = '56.80000,9.88280';
const PARTIAL_B = '56.15147,10.21421', UNAVAILABLE_B = '56.15147,10.21422';
const link = (a, b) => 'profile=' + a + ',' + b + '&ha=10&hb=10';

const PNG_1X1 = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGM4ceIEAAS0AlkWLoFAAAAAAElFTkSuQmCC',
  'base64');

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

/* A synthetic profile in the shape of cmd/server TerrainProfileResponse
 * with the classification on. Dokk1-like: 237 m, a 28 m roof at
 * 118-158 m, terrain clear and surface blocked. Forest: 2 km of 18 m
 * trees, surface marginal. The partial and unavailable variants are the
 * Dokk1 path with that classification state (unavailable: no class). */
function profileBody(q) {
  const b = q.get('b');
  const forest = b === FOREST_B;
  const state = b === PARTIAL_B ? 'partial' : b === UNAVAILABLE_B ? 'unavailable' : 'full';
  const D = forest ? 2000 : 237, n = forest ? 201 : 25;
  const samples = [];
  for (let i = 0; i < n; i++) {
    const d = Math.round(D * i / (n - 1) * 100) / 100;
    const s = { d, dtm: forest ? 50 : 5 };
    const up = forest ? d > 100 && d < 1900 : d >= 118 && d <= 158;
    s.dsm = up ? s.dtm + (forest ? 18 : 28) : s.dtm + 0.4;
    if (up && state !== 'unavailable' && !(state === 'partial' && d > 140)) s.cls = forest ? 'v' : 'b';
    samples.push(s);
  }
  const ref = (p) => { const [lat, lon] = String(p).split(',').map(Number); return { lat, lon }; };
  const end = (p, h) => Object.assign(ref(p), {
    kind: 'point', groundM: samples[0].dtm, antennaM: Number(q.get(h) || 10), antennaSource: 'request', assumed: false,
  });
  const surface = forest
    ? { state: 'fresnel60', worstClearanceM: 2.1, worstAtM: 1000, worstF1Fraction: 0.4, cause: 'vegetation' }
    : { state: 'blocked', worstClearanceM: -17.5, worstAtM: 138, worstF1Fraction: -2.1,
      cause: state === 'full' ? 'building' : state === 'partial' ? 'building' : 'unknown' };
  return {
    a: end(q.get('a'), 'ha'), b: end(q.get('b'), 'hb'),
    distanceM: D, stepM: 10, sampleCount: n, model: q.get('model') === 'dtm' ? 'dtm' : 'both',
    coverage: { state: 'full', pct: 100, firstGapM: null },
    samples,
    physics: { freqMHz: 869.618, freqSource: 'firmware', lambdaM: 0.34474, k: 1.3333333333333333, kSource: 'default', f1MaxM: 5.6 },
    verdict: { state: 'clear', model: 'dtm', criterion: 0.6, worstClearanceM: 9.6, worstAtM: forest ? 1000 : 138, worstF1Fraction: 1.7, surface },
    classification: { state, truncated: false },
    attribution: state === 'unavailable' ? DHM_DERIVED : BLD_CREDIT,
  };
}

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #730 M3 buildings versus trees in the Path profile against ${BASE} ===\n`);

  async function newCtx(o) {
    o = o || {};
    const ctx = await browser.newContext({
      viewport: o.viewport || { width: 1280, height: 900 },
      hasTouch: !!o.touch, isMobile: !!o.touch,
    });
    const rec = { consoleErrors: [], all: [], stubbed: new Set() };
    await ctx.route(BASE + '/api/config/client', async (route) => {
      const response = await route.fetch();
      const config = await response.json();
      config.dhm = DHM_BLOCK;
      await route.fulfill({ response, json: config });
    });
    await ctx.route(url => new URL(url).pathname === DHM_PATH, async (route) => {
      await route.fulfill({ status: 200, contentType: 'image/png', body: PNG_1X1 });
    });
    await ctx.route(url => new URL(url).pathname === PROFILE_PATH, async (route) => {
      const u = new URL(route.request().url());
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(profileBody(u.searchParams)) });
    });
    // Every third-party request is stubbed; nothing may reach a real network.
    await ctx.route(/^https?:\/\/(?!localhost|127\.0\.0\.1)/i, async (route) => {
      rec.stubbed.add(route.request().url());
      await route.fulfill({ status: 200, contentType: 'image/png', body: PNG_1X1 });
    });
    await ctx.addInitScript(({ theme }) => {
      try {
        localStorage.setItem('meshcore-theme', theme);
        localStorage.setItem('meshcore-map-clustering', 'false');
        ['row-swipe', 'tab-swipe', 'edge-drawer', 'pull-refresh']
          .forEach(k => localStorage.setItem('meshcore-gesture-hints-' + k, 'seen'));
      } catch (_) {}
      const apply = () => document.documentElement && document.documentElement.setAttribute('data-theme', theme);
      apply();
      document.addEventListener('DOMContentLoaded', apply);
    }, { theme: o.theme || 'light' });
    const page = await ctx.newPage();
    page.on('console', m => { if (m.type() === 'error') rec.consoleErrors.push(m.text()); });
    page.on('pageerror', e => rec.consoleErrors.push(String(e)));
    page.on('request', r => rec.all.push(r.url()));
    return { ctx, page, rec };
  }

  async function openProfile(page, l) {
    await page.goto(`${BASE}/#/map?lat=56.15&lon=10.21&zoom=15&${l}`, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector(PANEL + '.is-open', { timeout: 20000 });
    await page.waitForSelector('[data-testid="tp-verdict"][data-state="clear"]', { timeout: 15000 });
    await page.waitForSelector('[data-testid="tp-chart"] svg.tp-svg', { timeout: 10000 });
    await page.waitForTimeout(300);
  }

  // What the panel shows: visible text only, as a viewer reads it, and
  // the band polygons with their computed fill against the theme tokens.
  async function panelState(page) {
    return page.evaluate(() => {
      const q = (id) => document.querySelector('[data-testid="' + id + '"]');
      const shown = (e) => !!e && !e.closest('[hidden]') && e.checkVisibility({ visibilityProperty: true })
        && e.getClientRects().length > 0;
      const root = getComputedStyle(document.documentElement);
      const rgb = (hex) => {
        const m = String(hex).trim().match(/^#([0-9a-f]{2})([0-9a-f]{2})([0-9a-f]{2})$/i);
        return m ? 'rgb(' + [1, 2, 3].map(i => parseInt(m[i], 16)).join(', ') + ')' : null;
      };
      const bands = (cls) => Array.from(document.querySelectorAll('[data-testid="tp-chart"] polygon.tp-band-' + cls))
        .map(p => getComputedStyle(p).fill);
      const legend = q('tp-legend');
      return {
        chip: shown(q('tp-surface-chip')) ? q('tp-surface-chip').textContent : null,
        note: shown(q('tp-class-note')) ? q('tp-class-note').textContent : null,
        legend: shown(legend) ? Array.from(legend.querySelectorAll('.tp-legend-item')).map(e => e.textContent) : null,
        building: bands('b'), vegetation: bands('v'),
        buildingToken: rgb(root.getPropertyValue('--terrain-building')),
        vegetationToken: rgb(root.getPropertyValue('--terrain-vegetation')),
        credit: q('tp-chart') && document.querySelector('.tp-credit-line').textContent,
        links: Array.from(document.querySelectorAll('.tp-panel .tp-credit-line a')).map(a => [a.getAttribute('data-testid'), a.textContent, a.getAttribute('href'), a.getAttribute('target')]),
        label: document.querySelector('[data-testid="tp-chart"] svg').getAttribute('aria-label'),
      };
    });
  }

  /* The base map's tiles are asked for, and answered by the catch-all
   * stub; nothing else may leave localhost, and nothing at all may be asked
   * of the DHM, INSPIRE or GeoDanmark hosts. */
  function noNetwork(rec) {
    const local = (u) => /^https?:\/\/(localhost|127\.0\.0\.1)[:/]/i.test(u) || /^(data|blob):/.test(u);
    const upstream = rec.all.filter(u => /dataforsyningen|datafordeler|geodanmark|inspire/i.test(new URL(u).hostname));
    assert(upstream.length === 0, 'a request was made to a real upstream: ' + upstream.join(', '));
    const loose = rec.all.filter(u => !local(u) && !rec.stubbed.has(u));
    assert(loose.length === 0, 'a request left localhost unstubbed: ' + loose.slice(0, 5).join(', '));
    assert(rec.consoleErrors.length === 0, 'console errors: ' + rec.consoleErrors.join(' | '));
  }

  async function shot(page, name) {
    if (!SHOTS) return;
    const panel = await page.$(PANEL);
    await panel.screenshot({ path: path.join(SHOTS, name), type: 'jpeg', quality: 55 });
  }

  const AXE_RULES = ['color-contrast', 'label', 'aria-required-attr', 'aria-valid-attr', 'aria-valid-attr-value',
    'button-name', 'aria-command-name', 'link-name', 'duplicate-id', 'nested-interactive', 'aria-allowed-attr'];

  for (const theme of ['light', 'dark']) {
    await step(`${theme}: Dokk1 — "Buildings: Blocked", the roof in the building colour, legend, credit`, async () => {
      const { ctx, page, rec } = await newCtx({ theme });
      try {
        await openProfile(page, link(DOKK1_A, DOKK1_B));
        const s = await panelState(page);
        assert(s.chip === 'Buildings: Blocked', 'chip = ' + JSON.stringify(s.chip));
        assert(s.building.length === 1 && s.vegetation.length === 0, 'bands b ' + s.building.length + ', v ' + s.vegetation.length);
        assert(s.buildingToken && s.building[0] === s.buildingToken, 'roof fill ' + s.building[0] + ', token ' + s.buildingToken);
        assert(JSON.stringify(s.legend) === JSON.stringify(['Building', 'Trees/other']), 'legend ' + JSON.stringify(s.legend));
        assert(s.note === null, 'note on a full classification: ' + s.note);
        assert(s.credit === BLD_CREDIT, 'credit = ' + JSON.stringify(s.credit));
        assert(JSON.stringify(s.links) === JSON.stringify(LINKS.map(l => l.concat('_blank'))), 'links ' + JSON.stringify(s.links));
        assert(/Buildings: Blocked/.test(s.label), 'accessible name ' + s.label);
        await shot(page, `730-m3-dokk1-${theme}.jpg`);

        // The hover readout on the roof (138 m of 237 m).
        const box = await page.$eval('[data-testid="tp-chart"] svg.tp-svg', (svg) => {
          const r = svg.getBoundingClientRect();
          return { left: r.left, top: r.top, width: r.width, height: r.height };
        });
        const x = box.left + box.width * (46 + 758 * 138 / 237) / 820;
        await page.mouse.move(x, box.top + box.height / 2);
        await page.waitForSelector('[data-testid="tp-cursor-readout"]:not([hidden])', { timeout: 5000 });
        const rows = await page.$$eval('[data-testid="tp-cursor-readout"] > div', ds => ds.map(d => d.textContent));
        assert(rows.length === 4 && rows[2] === 'Surface 33.0 m (building)', 'readout ' + JSON.stringify(rows));
        await page.mouse.move(box.left + box.width * (46 + 758 * 30 / 237) / 820, box.top + box.height / 2);
        await page.waitForFunction(() => /^Surface 5\.4 m$/.test(document.querySelectorAll('[data-testid="tp-cursor-readout"] > div')[2].textContent), null, { timeout: 5000 });
        noNetwork(rec);
      } finally { await ctx.close(); }
    });

    await step(`${theme}: forest — "Trees/other: Marginal", the canopy in the trees colour, no building`, async () => {
      const { ctx, page, rec } = await newCtx({ theme });
      try {
        await openProfile(page, link(FOREST_A, FOREST_B));
        const s = await panelState(page);
        assert(s.chip === 'Trees/other: Marginal', 'chip = ' + JSON.stringify(s.chip));
        assert(s.vegetation.length === 1 && s.building.length === 0, 'bands b ' + s.building.length + ', v ' + s.vegetation.length);
        assert(s.vegetationToken && s.vegetation[0] === s.vegetationToken, 'canopy fill ' + s.vegetation[0] + ', token ' + s.vegetationToken);
        assert(s.credit === BLD_CREDIT, 'credit = ' + JSON.stringify(s.credit));
        await shot(page, `730-m3-forest-${theme}.jpg`);
        noNetwork(rec);
      } finally { await ctx.close(); }
    });

    await step(`${theme}: partial keeps the generic chip and adds the note; unavailable adds nothing`, async () => {
      const { ctx, page, rec } = await newCtx({ theme });
      try {
        await openProfile(page, link(DOKK1_A, PARTIAL_B));
        let s = await panelState(page);
        assert(s.chip === 'Buildings/trees: Blocked', 'partial chip = ' + JSON.stringify(s.chip));
        assert(s.note === PARTIAL_NOTE, 'partial note = ' + JSON.stringify(s.note));
        assert(s.building.length === 1, 'the classified part of the roof is still drawn');
        assert(JSON.stringify(s.legend) === JSON.stringify(['Building', 'Trees/other', 'Not told apart']), 'legend ' + JSON.stringify(s.legend));
        await shot(page, `730-m3-partial-${theme}.jpg`);
        await page.evaluate((h) => { location.hash = h; }, `#/map?${link(DOKK1_A, UNAVAILABLE_B)}`);
        await page.waitForFunction(() => !document.querySelector('[data-testid="tp-chart"] polygon.tp-band-b'), null, { timeout: 10000 });
        await page.waitForSelector('[data-testid="tp-verdict"][data-state="clear"]', { timeout: 10000 });
        s = await panelState(page);
        assert(s.chip === 'Buildings/trees: Blocked', 'unavailable chip = ' + JSON.stringify(s.chip));
        assert(s.note === null && s.legend === null, 'note ' + s.note + ', legend ' + JSON.stringify(s.legend));
        assert(s.credit === DHM_DERIVED, 'credit = ' + JSON.stringify(s.credit));
        assert(s.links.map(l => l[0]).join() === 'tp-credit-dataset,tp-credit-link', 'links ' + JSON.stringify(s.links));
        noNetwork(rec);
      } finally { await ctx.close(); }
    });

    await step(`${theme}: axe is clean on the panel, Dokk1 and partial`, async () => {
      const { AxeBuilder } = require('@axe-core/playwright');
      const { ctx, page } = await newCtx({ theme });
      try {
        for (const b of [DOKK1_B, PARTIAL_B]) {
          await openProfile(page, link(DOKK1_A, b));
          const res = await new AxeBuilder({ page }).include(PANEL).withRules(AXE_RULES).analyze();
          const v = res.violations.map(x => x.id + ' x' + x.nodes.length + ' ' + x.nodes.map(n => n.target.join(' ')).join(' | '));
          assert(res.violations.length === 0, 'axe violations: ' + v.join(', '));
        }
      } finally { await ctx.close(); }
    });
  }

  // ── Mobile landscape ─────────────────────────────────────────────────────
  for (const theme of ['light', 'dark']) {
    await step(`${theme}: 740x360 touch — the chart and chips stay in view, the credit links are 44 px targets`, async () => {
      const { ctx, page, rec } = await newCtx({ theme, viewport: { width: 740, height: 360 }, touch: true });
      try {
        await openProfile(page, link(DOKK1_A, DOKK1_B));
        const geo = await page.evaluate(() => {
          const r = (e) => { const b = e.getBoundingClientRect(); return { top: b.top, bottom: b.bottom, left: b.left, right: b.right, h: b.height }; };
          return {
            vh: innerHeight, vw: innerWidth,
            svg: r(document.querySelector('[data-testid="tp-chart"] svg.tp-svg')),
            chip: r(document.querySelector('[data-testid="tp-surface-chip"]')),
            panel: r(document.querySelector('[data-testid="tp-panel"]')),
            scrollW: document.documentElement.scrollWidth,
          };
        });
        for (const k of ['svg', 'chip']) {
          const b = geo[k];
          assert(b.top >= geo.panel.top - 0.5 && b.bottom <= geo.vh + 0.5 && b.left >= -0.5 && b.right <= geo.vw + 0.5,
            k + ' out of view: ' + JSON.stringify(b) + ' in ' + geo.vw + 'x' + geo.vh);
        }
        assert(geo.svg.h >= 100, 'the chart is only ' + geo.svg.h + ' px tall');
        assert(geo.scrollW <= geo.vw, 'the page scrolls sideways: ' + geo.scrollW);
        await shot(page, `730-m3-dokk1-740x360-${theme}.jpg`);
        for (const [id] of LINKS) {
          const t = await page.evaluate((tid) => {
            const a = document.querySelector('[data-testid="' + tid + '"]');
            a.scrollIntoView({ block: 'center' });
            const b = a.getBoundingClientRect();
            const hit = document.elementFromPoint(b.left + b.width / 2, b.top + b.height / 2);
            return { w: b.width, h: b.height, hit: hit === a || a.contains(hit), inView: b.top >= 0 && b.bottom <= innerHeight };
          }, id);
          assert(t.w >= 44 && t.h >= 44, id + ' is ' + t.w + 'x' + t.h);
          assert(t.hit && t.inView, id + ' is covered or out of view: ' + JSON.stringify(t));
        }
        noNetwork(rec);
      } finally { await ctx.close(); }
    });
  }

  await browser.close();
  console.log(`\n#730 M3 E2E: ${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
