/**
 * E2E (#738 M2): the mixed-resolution note and the Copernicus credit in the
 * Map page's Path profile panel, in a real browser.
 *
 * The CI server has no map.dhm config, no Dataforsyningen token and no
 * foreign fill, so they are switched on the way an operator's config would:
 * /api/config/client is intercepted and given the `dhm` block the Go server
 * sends with map.dhm.profile.foreignFill on (cmd/server/dhm_proxy.go,
 * DHMFillClientConfig), and /api/terrain/profile is a synthetic profile in
 * the server's shape (#773): Sønderborg → Flensburg is mixed, a path
 * inside Denmark is not. Nothing here reaches a real network.
 *
 * Asserts:
 *   1. Sønderborg → Flensburg: the note says mixed, outside Denmark,
 *      Copernicus 30 m, surface-influenced, surface unknown; the short
 *      credit is the block's, with the dataset and licence linked
 *      (target=_blank, rel=noopener); the full Art. 6(b) line is folded
 *      away until "Full credit" is opened; the DHM footer is still there;
 *      light and dark;
 *   2. on the ½ overview the note says 60 m and the overview;
 *   3. Aarhus → Silkeborg (Denmark only): neither the note nor the credit;
 *      and switching from the mixed link to it takes both away;
 *   4. 360px portrait: the credit links and "Full credit" are tappable;
 *   5. no request reaches AWS, Copernicus or Dataforsyningen, and no
 *      console error is logged.
 *
 * SHOTS_DIR, when set, receives small JPG screenshots of the panel.
 *
 * Usage:
 *   BASE_URL=http://localhost:13581 node test-issue-738-foreign-fill-ui-e2e.js
 */
'use strict';
const path = require('path');
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const SHOTS = process.env.SHOTS_DIR || '';
const PROFILE_PATH = '/api/terrain/profile';
const DHM_PATH = '/api/map/dhm/wms';

// internal/copdem/copdem.go, as cmd/server sends it in dhm.fill.
const COP_DATASET = 'Copernicus DEM GLO-30';
const COP_DATASET_URL = 'https://registry.opendata.aws/copernicus-dem/';
const COP_LICENSE = 'Copernicus DEM licence';
const COP_LICENSE_URL = 'https://documentation.dataspace.copernicus.eu/APIs/SentinelHub/Data/DEM/resources/license/License-COPDEM-30.pdf';
const COP_CREDIT = 'Outside Denmark: produced using ' + COP_DATASET + ', provided under COPERNICUS by the European Union and ESA (' + COP_LICENSE + ')';
const COP_6B = 'produced using Copernicus WorldDEM-30 © DLR e.V. 2010-2014 and © Airbus Defence and Space GmbH 2014-2018 provided under COPERNICUS by the European Union and ESA; all rights reserved';
// internal/dhm/dhm.go.
const DHM_ATTRIBUTION = 'Indeholder data fra Klimadatastyrelsen, Danmarks Højdemodel, CC BY 4.0';
const DHM_DERIVED = DHM_ATTRIBUTION + ' — data bearbejdet';

const DHM_BLOCK = {
  enabled: true, url: DHM_PATH, layer: 'dhm_terraen_skyggekort',
  minZoom: 7, maxZoom: 19, profile: true,
  bounds: [[54.4265, 7.99125], [57.778, 15.5995]],
  attribution: DHM_ATTRIBUTION,
  datasetName: 'Danmarks Højdemodel', datasetUrl: 'https://dataforsyningen.dk/data/930',
  licenseName: 'CC BY 4.0', licenseUrl: 'https://creativecommons.org/licenses/by/4.0/',
  attributionDerived: DHM_DERIVED,
  fill: {
    credit: COP_CREDIT,
    datasetName: COP_DATASET, datasetUrl: COP_DATASET_URL,
    licenseName: COP_LICENSE, licenseUrl: COP_LICENSE_URL,
  },
};

const SONDERBORG = '54.90980,9.79220';
const FLENSBURG = '54.79370,9.44690';
const AARHUS = '56.16290,10.20390';
const SILKEBORG = '56.16970,9.54510';
const MIXED_LINK = 'profile=' + SONDERBORG + ',' + FLENSBURG;
const DK_LINK = 'profile=' + AARHUS + ',' + SILKEBORG;
const NOTE_30 = 'Mixed: outside Denmark Copernicus 30 m, surface-influenced; surface unknown there.';
const NOTE_60 = 'Mixed: outside Denmark Copernicus 60 m (½ overview), surface-influenced; surface unknown there.';

const PNG_1X1 = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGM4ceIEAAS0AlkWLoFAAAAAAElFTkSuQmCC',
  'base64');

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

/* A synthetic profile in the server's shape. Flensburg as B makes it the
 * mixed cross-border path: the last third is filled, the surface verdict
 * is unknown (no surface model abroad) and the response carries the Art.
 * 6(b) line. Anything else is a full DHM profile. */
function profileBody(q, resolutionM) {
  const mixed = (q.get('b') || '') === FLENSBURG;
  const D = mixed ? 24500 : 42300;
  const n = 90, cut = 60;
  const samples = [];
  for (let i = 0; i < n; i++) {
    const d = Math.round((D * i / (n - 1)) * 100) / 100;
    const dtm = 8 + 12 * Math.sin(d / 2500) + (mixed && i >= cut ? 6 : 0);
    const s = { d: d, dtm: Math.round(dtm * 100) / 100 };
    if (!(mixed && i >= cut)) s.dsm = Math.round((dtm + 9) * 100) / 100;
    samples.push(s);
  }
  const ref = (s) => { const [lat, lon] = String(s).split(',').map(Number); return { lat, lon }; };
  const end = (p, h) => Object.assign(ref(p), {
    kind: 'point', groundM: 10, antennaM: q.get(h) !== null ? Number(q.get(h)) : 10,
    antennaSource: q.get(h) !== null ? 'request' : 'point-default', assumed: q.get(h) === null,
  });
  const body = {
    a: end(q.get('a'), 'ha'), b: end(q.get('b'), 'hb'),
    distanceM: D, stepM: 23.9, sampleCount: 1024,
    model: q.get('model') === 'dtm' ? 'dtm' : 'both',
    coverage: { state: 'full', pct: 100, firstGapM: null },
    samples: samples,
    physics: { freqMHz: 869.618, freqSource: 'firmware', lambdaM: 0.34474, k: 1.3333333333333333, kSource: 'default', f1MaxM: 40.1 },
    verdict: {
      state: 'clear', model: 'dtm', criterion: 0.6, worstClearanceM: 14.2, worstAtM: 12000, worstF1Fraction: 1.1,
      surface: mixed ? { state: 'unknown', worstClearanceM: null, worstAtM: null, worstF1Fraction: null }
        : { state: 'clear', worstClearanceM: 6.3, worstAtM: 12000, worstF1Fraction: 1.0 },
    },
    attribution: DHM_DERIVED,
  };
  if (mixed) {
    body.coverage.mixed = true;
    body.coverage.fill = [{ fromM: samples[cut].d, toM: D, source: 'copdem-glo30', resolutionM: resolutionM || 30 }];
    body.attributionFill = COP_6B;
  }
  return body;
}

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #738 M2 mixed-resolution note and Copernicus credit against ${BASE} ===\n`);

  async function newCtx(o) {
    o = o || {};
    const ctx = await browser.newContext({
      viewport: o.viewport || { width: 1280, height: 900 },
      hasTouch: !!o.touch, isMobile: !!o.touch,
    });
    const rec = { consoleErrors: [], all: [], profiles: [] };
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
      rec.profiles.push(u.search);
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(profileBody(u.searchParams, o.resolutionM)) });
    });
    // Every third-party request is stubbed; nothing may reach a real network.
    await ctx.route(/^https?:\/\/(?!localhost|127\.0\.0\.1)/i, async (route) => {
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

  async function openProfile(page, link) {
    await page.goto(`${BASE}/#/map?lat=55.2&lon=9.8&zoom=9&${link}`, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('[data-testid="tp-panel"]', { timeout: 20000 });
    await page.waitForSelector('[data-testid="tp-verdict"][data-state="clear"]', { timeout: 15000 });
    await page.waitForTimeout(400);
  }

  // What the panel shows: visible text only, as a viewer reads it.
  async function panelState(page) {
    return page.evaluate(() => {
      const q = (id) => document.querySelector('[data-testid="' + id + '"]');
      // checkVisibility, not client rects: the body of a closed <details>
      // keeps its rects in Chromium, but is not rendered.
      const shown = (e) => !!e && !e.closest('[hidden]') && e.checkVisibility({ visibilityProperty: true })
        && e.getClientRects().length > 0;
      const link = (e) => e && {
        href: e.getAttribute('href'), rel: e.getAttribute('rel'), target: e.getAttribute('target'), text: e.textContent,
      };
      const note = q('tp-mixed-note'), credit = q('tp-credit-fill'), line = q('tp-credit-fill-line');
      const full = q('tp-credit-fill-full'), text = q('tp-credit-fill-text');
      const title = document.querySelector('[data-testid="tp-chart"] svg title');
      return {
        note: shown(note) ? note.textContent : null,
        credit: shown(credit),
        line: shown(line) ? line.textContent : null,
        dataset: link(q('tp-credit-fill-dataset')),
        license: link(q('tp-credit-fill-license')),
        fullOpen: !!(full && full.open),
        fullText: shown(text) ? text.textContent : null,
        dhmFooter: shown(q('tp-credit-dataset')),
        surfaceChip: shown(q('tp-surface-chip')),
        title: title ? title.textContent : '',
        copernicusVisible: Array.from(document.querySelectorAll('[data-testid="tp-panel"] *'))
          .some(e => e.children.length === 0 && shown(e) && /Copernicus/.test(e.textContent)),
      };
    });
  }

  function noNetwork(rec) {
    const bad = rec.all.filter(u => /amazonaws|copernicus|dataforsyningen\.dk|opendata\.aws/i.test(u));
    assert(bad.length === 0, 'a request reached a real upstream: ' + bad.join(', '));
    assert(rec.consoleErrors.length === 0, 'console errors: ' + rec.consoleErrors.join(' | '));
  }

  async function shot(page, name) {
    if (!SHOTS) return;
    const panel = await page.$('[data-testid="tp-panel"]');
    await panel.screenshot({ path: path.join(SHOTS, name), type: 'jpeg', quality: 60 });
  }

  // ── 1: Sønderborg → Flensburg, light and dark ───────────────────────────
  for (const theme of ['light', 'dark']) {
    await step(`${theme}: Sønderborg → Flensburg shows the note and the Copernicus credit`, async () => {
      const { ctx, page, rec } = await newCtx({ theme });
      try {
        await openProfile(page, MIXED_LINK);
        const s = await panelState(page);
        assert(s.note === NOTE_30, 'note = ' + JSON.stringify(s.note));
        assert(s.credit, 'the Copernicus credit is not shown');
        assert(s.line === COP_CREDIT, 'credit line = ' + JSON.stringify(s.line));
        assert(s.dataset && s.dataset.href === COP_DATASET_URL && s.dataset.text === COP_DATASET, 'dataset link = ' + JSON.stringify(s.dataset));
        assert(s.license && s.license.href === COP_LICENSE_URL && s.license.text === COP_LICENSE, 'licence link = ' + JSON.stringify(s.license));
        for (const l of [s.dataset, s.license]) {
          assert(l.target === '_blank', 'target: ' + JSON.stringify(l));
          assert(/\bnoopener\b/.test(l.rel || ''), 'rel: ' + JSON.stringify(l));
        }
        assert(/\blicense\b/.test(s.license.rel), 'licence rel: ' + s.license.rel);
        // The long line is not inline: folded until asked for.
        assert(!s.fullOpen && s.fullText === null, 'the full Art. 6(b) line is shown inline');
        assert(s.dhmFooter, 'the DHM footer went missing');
        assert(!s.surfaceChip, 'a surface chip is shown on a mixed profile');
        assert(s.title.indexOf(NOTE_30) >= 0, 'the chart title lacks the note: ' + s.title);

        await page.click('[data-testid="tp-credit-fill-full"] summary');
        const open = await panelState(page);
        assert(open.fullOpen && open.fullText === COP_6B, 'full credit = ' + JSON.stringify(open.fullText));
        await shot(page, `738-m2-mixed-${theme}.jpg`);
        noNetwork(rec);
      } finally { await ctx.close(); }
    });
  }

  // ── 2: the ½ overview ───────────────────────────────────────────────────
  await step('on the ½ overview the note says 60 m and the overview', async () => {
    const { ctx, page, rec } = await newCtx({ resolutionM: 60 });
    try {
      await openProfile(page, MIXED_LINK);
      const s = await panelState(page);
      assert(s.note === NOTE_60, 'note = ' + JSON.stringify(s.note));
      assert(s.credit && s.line === COP_CREDIT, 'credit = ' + JSON.stringify(s.line));
      noNetwork(rec);
    } finally { await ctx.close(); }
  });

  // ── 3: a path inside Denmark ────────────────────────────────────────────
  await step('Aarhus → Silkeborg (Denmark only) shows neither the note nor the credit', async () => {
    const { ctx, page, rec } = await newCtx({});
    try {
      await openProfile(page, DK_LINK);
      const s = await panelState(page);
      assert(s.note === null, 'note on a DK-only path: ' + s.note);
      assert(!s.credit && s.line === null, 'Copernicus credit on a DK-only path');
      assert(!s.copernicusVisible, 'the panel names Copernicus on a DK-only path');
      assert(s.dhmFooter, 'no DHM footer');
      assert(s.title.indexOf('Mixed') < 0, 'the chart title is mixed: ' + s.title);
      await shot(page, '738-m2-dk-only-light.jpg');
      noNetwork(rec);
    } finally { await ctx.close(); }
  });

  await step('switching from the mixed link to a DK-only one takes the note and the credit away', async () => {
    const { ctx, page, rec } = await newCtx({});
    try {
      await openProfile(page, MIXED_LINK);
      assert((await panelState(page)).credit, 'setup: no credit on the mixed path');
      const before = rec.profiles.length;
      await page.evaluate((link) => { location.hash = '#/map?lat=55.2&lon=9.8&zoom=9&' + link; }, DK_LINK);
      await page.waitForFunction(() => {
        const t = document.querySelector('[data-testid="tp-chart"] svg title');
        return t && t.textContent.indexOf('42.3 km') >= 0;
      }, null, { timeout: 15000 });
      const s = await panelState(page);
      assert(rec.profiles.length > before, 'no request for the DK-only path');
      assert(s.note === null && !s.credit && !s.copernicusVisible, 'left over: ' + JSON.stringify(s));
      noNetwork(rec);
    } finally { await ctx.close(); }
  });

  // ── 4: a 360px phone ────────────────────────────────────────────────────
  await step('360px portrait: the Copernicus links and "Full credit" are tappable', async () => {
    const { ctx, page, rec } = await newCtx({ viewport: { width: 360, height: 740 }, touch: true });
    try {
      await openProfile(page, MIXED_LINK);
      const boxes = await page.evaluate(() => {
        const box = (sel) => {
          const e = document.querySelector(sel);
          if (!e) return null;
          e.scrollIntoView({ block: 'center' });
          const b = e.getBoundingClientRect();
          const hit = document.elementFromPoint(b.x + b.width / 2, b.y + b.height / 2);
          return {
            w: Math.round(b.width), h: Math.round(b.height),
            selfHit: !!(hit && (hit === e || e.contains(hit))),
            hitBy: hit ? (hit.getAttribute('data-testid') || hit.className || hit.tagName) : null,
            inside: b.left >= 0 && b.right <= innerWidth + 1,
          };
        };
        return {
          dataset: box('[data-testid="tp-credit-fill-dataset"]'),
          license: box('[data-testid="tp-credit-fill-license"]'),
          more: box('[data-testid="tp-credit-fill-full"] summary'),
        };
      });
      console.log('    360px boxes: ' + JSON.stringify(boxes));
      for (const [what, b] of Object.entries(boxes)) {
        assert(b, what + ' missing');
        assert(b.h >= 44, what + ' is ' + b.h + 'px tall');
        assert(b.w >= 44, what + ' is ' + b.w + 'px wide');
        assert(b.selfHit, what + ' centre is hit by ' + b.hitBy);
        assert(b.inside, what + ' runs outside the viewport');
      }
      await shot(page, '738-m2-mixed-360.jpg');
      noNetwork(rec);
    } finally { await ctx.close(); }
  });

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})();
