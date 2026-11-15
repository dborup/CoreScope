/**
 * E2E (#733): the DHM credit meets Dataforsyningen's CC BY 4.0 terms in a
 * real browser.
 *
 * Dataforsyningen's terms ("Vilkår for brug af data", v3.1, August 2024,
 * https://dataforsyningen.dk/Vilkaar) require a credit for Klimadatastyrelsen
 * data to name the owner, name the licence and link to it, link to the
 * specific dataset, and state whether the data was changed. The strings live
 * in internal/dhm; the DOM that renders them is what this suite pins.
 *
 * The CI server has no map.dhm config and no Dataforsyningen token, so the
 * overlay and the profile are switched on the way an operator's config
 * would: /api/config/client is intercepted and given the `dhm` block the Go
 * server sends (cmd/server/dhm_proxy.go), the hillshade tiles are a
 * synthetic PNG and /api/terrain/profile a synthetic profile. Nothing here
 * reaches a real network.
 *
 * Asserts:
 *   1. Map and Live: with the hillshade on, the credit control names the
 *      owner, links the dataset and links the licence, every link
 *      target="_blank" with rel="noopener"; the rendered text is exactly
 *      the server's plain credit, and it is visible and uncovered;
 *   2. toggling the overlay off removes that credit and its links;
 *   3. a pre-#733 block (no link fields) still shows the plain credit, with
 *      no dataset or licence link — the degradation path;
 *   4. the profile chart footer carries the same two links plus the
 *      "derived" note, exactly once, with the owner named, and it is the
 *      server's block that says so: a changed wording or link target in
 *      the block shows as sent (#765);
 *   5. 360px portrait: the footer credit is inside the panel and the
 *      viewport, legible, uncovered, and each link's own hit box is at
 *      least 44px tall and hit by itself at its centre;
 *   6. no request reaches dataforsyningen.dk and no console error is logged.
 *
 * Usage:
 *   BASE_URL=http://localhost:13700 node test-issue-733-dhm-credit-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13700';
const DHM_PATH = '/api/map/dhm/wms';
const PROFILE_PATH = '/api/terrain/profile';

// internal/dhm/dhm.go — the one credit definition.
const OWNER = 'Klimadatastyrelsen';
const DATASET_MARK = 'Danmarks Højdemodel';
const DATASET_URL = 'https://dataforsyningen.dk/data/930';
const LICENCE_MARK = 'CC BY 4.0';
const LICENCE_URL = 'https://creativecommons.org/licenses/by/4.0/';
const DERIVED_NOTE = 'data bearbejdet';
const ATTRIBUTION = 'Indeholder data fra ' + OWNER + ', ' + DATASET_MARK + ', ' + LICENCE_MARK;
const ATTRIBUTION_DERIVED = ATTRIBUTION + ' — ' + DERIVED_NOTE;

// The `dhm` block of /api/config/client, field for field (and with the
// profile on, so the same context drives both halves of the suite).
const DHM_BLOCK = {
  enabled: true, url: DHM_PATH, layer: 'dhm_terraen_skyggekort',
  minZoom: 7, maxZoom: 19, profile: true,
  bounds: [[54.4265, 7.99125], [57.778, 15.5995]],
  attribution: ATTRIBUTION,
  datasetName: DATASET_MARK, datasetUrl: DATASET_URL,
  licenseName: LICENCE_MARK, licenseUrl: LICENCE_URL,
  // #765: the profile footer is built from this block.
  attributionDerived: ATTRIBUTION_DERIVED,
};
// The block a server without #733 sends: the same credit text, no links.
const PLAIN_BLOCK = (() => {
  const b = Object.assign({}, DHM_BLOCK);
  delete b.datasetName; delete b.datasetUrl; delete b.licenseName; delete b.licenseUrl;
  delete b.attributionDerived;
  return b;
})();

const PNG_1X1 = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGM4ceIEAAS0AlkWLoFAAAAAAElFTkSuQmCC',
  'base64');

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

// A synthetic profile, same field-for-field shape as the real endpoint. The
// chart only has to render; the footer is what is under test.
function profileBody(q) {
  const D = 9000;
  const samples = [];
  for (let i = 0; i < 90; i++) {
    const d = Math.round((D * i / 89) * 100) / 100;
    samples.push({ d: d, dtm: 30 + 15 * Math.sin(d / 800), dsm: 41 + 15 * Math.sin(d / 800) });
  }
  const end = (p, h) => ({
    lat: 56.1629, lon: 10.2039, kind: /^[0-9a-f]{64}$/.test(p || '') ? 'node' : 'point',
    role: 'repeater', groundM: 40, antennaM: q.get(h) !== null ? Number(q.get(h)) : 10,
    antennaSource: q.get(h) !== null ? 'request' : 'role-default', assumed: q.get(h) === null,
  });
  return {
    a: end(q.get('a'), 'ha'), b: end(q.get('b'), 'hb'),
    distanceM: D, stepM: 11.7, sampleCount: 770,
    model: q.get('model') === 'dtm' ? 'dtm' : 'both',
    coverage: { state: 'full', pct: 100, firstGapM: null },
    samples: samples,
    physics: { freqMHz: 869.618, freqSource: 'firmware', lambdaM: 0.34474, k: 1.3333333333333333, kSource: 'default', f1MaxM: 27.8 },
    verdict: {
      state: 'clear', model: 'dtm', criterion: 0.6, worstClearanceM: 38.4, worstAtM: 4500, worstF1Fraction: 1.3,
      surface: { state: 'clear', worstClearanceM: 26.5, worstAtM: 4500, worstF1Fraction: 1.0 },
    },
    // The profile is computed from DHM, so the API credits derived data.
    attribution: ATTRIBUTION_DERIVED,
  };
}

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #733 DHM credit compliance against ${BASE} ===\n`);

  async function newCtx(o) {
    o = o || {};
    const ctx = await browser.newContext({
      viewport: o.viewport || { width: 1280, height: 860 },
      hasTouch: !!o.touch, isMobile: !!o.touch,
    });
    const rec = { consoleErrors: [], all: [] };
    await ctx.route(BASE + '/api/config/client', async (route) => {
      const response = await route.fetch();
      const config = await response.json();
      config.dhm = o.dhm || (o.plain ? PLAIN_BLOCK : DHM_BLOCK);
      await route.fulfill({ response, json: config });
    });
    await ctx.route(url => new URL(url).pathname === DHM_PATH, async (route) => {
      await route.fulfill({ status: 200, contentType: 'image/png', body: PNG_1X1 });
    });
    await ctx.route(url => new URL(url).pathname === PROFILE_PATH, async (route) => {
      const u = new URL(route.request().url());
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(profileBody(u.searchParams)) });
    });
    // Every third-party image is stubbed; nothing in this suite may reach a
    // real network.
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

  const terrainLabel = '.leaflet-control-layers-overlays label[data-overlay-id="dhm-terrain"]';

  async function gotoMap(page, hash, extra) {
    const qs = 'lat=56.15&lon=10.2&zoom=9' + (extra ? '&' + extra : '');
    await page.goto(`${BASE}/${hash}?${qs}`, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('.leaflet-control-layers', { timeout: 20000 });
    await page.waitForFunction(() => window.MeshConfigReady !== undefined, null, { timeout: 15000 });
    await page.evaluate(() => window.MeshConfigReady).catch(() => {});
    await page.waitForTimeout(800);
  }

  async function setTerrain(page, on) {
    await page.evaluate(({ sel, want }) => {
      const input = document.querySelector(sel + ' input[type="checkbox"]');
      if (!input) throw new Error('no terrain checkbox');
      if (input.checked !== want) input.click();
    }, { sel: terrainLabel, want: on });
    await page.waitForTimeout(800);
  }

  // What the map's credit control says, and the links in it.
  async function mapCredit(page) {
    return page.evaluate(() => {
      const el = document.querySelector('.leaflet-control-attribution');
      if (!el) return null;
      const r = el.getBoundingClientRect();
      return {
        text: el.textContent || '',
        links: Array.from(el.querySelectorAll('a')).map(a => ({
          href: a.getAttribute('href'), rel: a.getAttribute('rel'),
          target: a.getAttribute('target'), text: a.textContent,
        })),
        visible: r.width > 0 && r.height > 0 && r.left >= 0 && r.top >= 0
          && r.right <= innerWidth && r.bottom <= innerHeight,
        covered: (() => {
          const hit = document.elementFromPoint(r.x + r.width / 2, r.y + r.height / 2);
          return !(hit && el.contains(hit));
        })(),
      };
    });
  }

  function creditLink(c, href) { return c.links.filter(l => l.href === href); }

  // ── 1 + 2 + 6: the hillshade credit on Map and Live ───────────────────────
  for (const p of [{ name: 'Map', hash: '#/map' }, { name: 'Live', hash: '#/live' }]) {
    await step(`${p.name}: the hillshade credit names the owner and links the dataset and the licence`, async () => {
      const { ctx, page, rec } = await newCtx({});
      try {
        await gotoMap(page, p.hash);
        await setTerrain(page, true);
        const c = await mapCredit(page);
        assert(c, 'no attribution control');
        // 1 — the owner is named, in the plain text the server sent.
        assert(c.text.includes(ATTRIBUTION), 'credit text: ' + JSON.stringify(c.text));
        // 3 — the dataset is linked.
        const ds = creditLink(c, DATASET_URL);
        assert(ds.length === 1, 'dataset links: ' + JSON.stringify(c.links));
        assert(ds[0].text === DATASET_MARK, 'dataset link text: ' + ds[0].text);
        // 2 — the licence is named and linked.
        const lic = creditLink(c, LICENCE_URL);
        assert(lic.length === 1, 'licence links: ' + JSON.stringify(c.links));
        assert(lic[0].text === LICENCE_MARK, 'licence link text: ' + lic[0].text);
        assert(/\blicense\b/.test(lic[0].rel || ''), 'licence rel: ' + lic[0].rel);
        // Both open safely.
        for (const l of ds.concat(lic)) {
          assert(l.target === '_blank', 'target: ' + JSON.stringify(l));
          assert(/\bnoopener\b/.test(l.rel || ''), 'rel: ' + JSON.stringify(l));
        }
        // 5 — easy to find: in the map corner, on screen and uncovered.
        assert(c.visible, 'the credit is clipped or off-screen');
        assert(!c.covered, 'the credit is covered by other UI');
        // 4 — the tiles are shown as served, so no "changed" claim here.
        assert(!c.text.includes(DERIVED_NOTE), 'the overlay credit claims derived data: ' + c.text);

        // 2 — the whole credit, links included, goes when the overlay does.
        await setTerrain(page, false);
        const off = await mapCredit(page);
        assert(!off.text.includes(OWNER), 'credit left after the overlay went off: ' + off.text);
        assert(creditLink(off, DATASET_URL).length === 0, 'dataset link left after off');
        assert(creditLink(off, LICENCE_URL).length === 0, 'licence link left after off');

        assert(rec.all.filter(u => /dataforsyningen\.dk/i.test(u)).length === 0, 'a request reached dataforsyningen.dk');
        assert(rec.consoleErrors.length === 0, 'console errors: ' + rec.consoleErrors.join(' | '));
      } finally { await ctx.close(); }
    });
  }

  // ── 3: a server without the link fields ──────────────────────────────────
  await step('a pre-#733 dhm block still shows the plain credit, with no links', async () => {
    const { ctx, page, rec } = await newCtx({ plain: true });
    try {
      await gotoMap(page, '#/map');
      await setTerrain(page, true);
      const c = await mapCredit(page);
      assert(c.text.includes(ATTRIBUTION), 'credit text: ' + JSON.stringify(c.text));
      assert(creditLink(c, DATASET_URL).length === 0, 'a dataset link appeared without the config field');
      assert(creditLink(c, LICENCE_URL).length === 0, 'a licence link appeared without the config field');
      assert(rec.consoleErrors.length === 0, 'console errors: ' + rec.consoleErrors.join(' | '));
    } finally { await ctx.close(); }
  });

  // ── 4: the profile chart footer ──────────────────────────────────────────
  const PAIR_LINK = 'profile=' + 'a'.repeat(64) + ',' + 'b'.repeat(64);

  async function footer(page) {
    return page.evaluate(() => {
      const panel = document.querySelector('[data-testid="tp-panel"]');
      const line = panel && panel.querySelector('.tp-credit-line');
      if (!line) return null;
      const cs = getComputedStyle(line);
      const r = line.getBoundingClientRect();
      const panelR = panel.getBoundingClientRect();
      const link = (sel) => {
        const a = line.querySelector(sel);
        if (!a) return null;
        const b = a.getBoundingClientRect();
        const hit = document.elementFromPoint(b.x + b.width / 2, b.y + b.height / 2);
        return {
          href: a.getAttribute('href'), rel: a.getAttribute('rel'),
          target: a.getAttribute('target'), text: a.textContent,
          w: Math.round(b.width * 10) / 10, h: Math.round(b.height * 10) / 10,
          selfHit: !!(hit && (hit === a || a.contains(hit))),
          hitBy: hit ? (hit.getAttribute('data-testid') || hit.getAttribute('class') || hit.tagName) : null,
        };
      };
      return {
        text: line.textContent || '',
        size: parseFloat(cs.fontSize),
        dataset: link('[data-testid="tp-credit-dataset"]'),
        licence: link('[data-testid="tp-credit-link"]'),
        // The panel's credit count: visible text nodes naming the owner.
        // Text nodes, not leaf elements: the shared walker (#765) leaves
        // the text between the links as bare text in the line.
        credits: (() => {
          const w = document.createTreeWalker(panel, NodeFilter.SHOW_TEXT);
          let n = 0;
          while (w.nextNode()) {
            const t = w.currentNode;
            if (t.nodeValue.indexOf('Klimadatastyrelsen') >= 0 && !t.parentElement.closest('[hidden]')) n++;
          }
          return n;
        })(),
        insidePanel: panelR.left - 1 <= r.left && r.right <= panelR.right + 1,
        inViewport: r.top >= 0 && r.bottom <= innerHeight + 1 && r.height > 0,
        covered: (() => {
          const hit = document.elementFromPoint(Math.round(r.x + 8), Math.round(r.y + r.height / 2));
          return !(hit && (hit === line || line.contains(hit)));
        })(),
      };
    });
  }

  await step('the profile footer links the dataset and the licence and says the data is derived', async () => {
    const { ctx, page, rec } = await newCtx({});
    try {
      await gotoMap(page, '#/map', PAIR_LINK);
      await page.waitForSelector('[data-testid="tp-panel"]', { timeout: 20000 });
      await page.waitForTimeout(900);
      const f = await footer(page);
      assert(f, 'no credit line in the profile panel');
      // 1 + 4 — the whole credit, with the statement that the data was
      // changed, as one line.
      assert(f.text === ATTRIBUTION_DERIVED, 'footer text = ' + JSON.stringify(f.text));
      assert(f.text.includes(OWNER), 'the footer does not name the owner');
      assert(f.text.endsWith(DERIVED_NOTE), 'the derived note is not the end of the line: ' + f.text);
      // 3 + 2 — both links, both opening safely.
      assert(f.dataset, 'no dataset link in the footer');
      assert(f.licence, 'no licence link in the footer');
      assert(f.dataset.href === DATASET_URL, 'dataset href = ' + f.dataset.href);
      assert(f.licence.href === LICENCE_URL, 'licence href = ' + f.licence.href);
      assert(f.dataset.text === DATASET_MARK, 'dataset text = ' + f.dataset.text);
      assert(f.licence.text === LICENCE_MARK, 'licence text = ' + f.licence.text);
      for (const l of [f.dataset, f.licence]) {
        assert(l.target === '_blank', 'target: ' + JSON.stringify(l));
        assert(/\bnoopener\b/.test(l.rel || ''), 'rel: ' + JSON.stringify(l));
      }
      assert(/\blicense\b/.test(f.licence.rel), 'licence rel: ' + f.licence.rel);
      // Shown exactly once, as before: the footer is the only credit.
      assert(f.credits === 1, 'the credit appears ' + f.credits + ' times in the panel');
      assert(rec.all.filter(u => /dataforsyningen\.dk/i.test(u)).length === 0, 'a request reached dataforsyningen.dk');
      assert(rec.consoleErrors.length === 0, 'console errors: ' + rec.consoleErrors.join(' | '));
    } finally { await ctx.close(); }
  });

  // #765: the footer comes from the server's block, so a wording or a link
  // target the page has never seen still shows exactly as the server sent
  // it. Nothing in this block is a real address.
  await step('the profile footer follows the server\'s dhm block (#765)', async () => {
    const block = Object.assign({}, DHM_BLOCK, {
      attributionDerived: ATTRIBUTION_DERIVED + ' (#765)',
      datasetUrl: 'https://example.test/dataset-765',
      licenseUrl: 'https://example.test/licence-765',
    });
    const { ctx, page, rec } = await newCtx({ dhm: block });
    try {
      await gotoMap(page, '#/map', PAIR_LINK);
      await page.waitForSelector('[data-testid="tp-panel"]', { timeout: 20000 });
      await page.waitForTimeout(900);
      const f = await footer(page);
      assert(f, 'no credit line in the profile panel');
      assert(f.text === block.attributionDerived, 'footer text = ' + JSON.stringify(f.text));
      assert(f.dataset && f.dataset.href === block.datasetUrl, 'dataset link = ' + JSON.stringify(f.dataset));
      assert(f.licence && f.licence.href === block.licenseUrl, 'licence link = ' + JSON.stringify(f.licence));
      assert(f.credits === 1, 'the credit appears ' + f.credits + ' times in the panel');
      assert(rec.consoleErrors.length === 0, 'console errors: ' + rec.consoleErrors.join(' | '));
    } finally { await ctx.close(); }
  });

  // ── 5: the footer on a 360px phone ───────────────────────────────────────
  await step('360px portrait: the footer credit is legible, uncovered and both links are tappable', async () => {
    const { ctx, page, rec } = await newCtx({ viewport: { width: 360, height: 740 }, touch: true });
    try {
      await gotoMap(page, '#/map', PAIR_LINK);
      await page.waitForSelector('[data-testid="tp-panel"]', { timeout: 20000 });
      await page.waitForTimeout(900);
      const f = await footer(page);
      assert(f, 'no credit line in the profile panel');
      assert(f.text === ATTRIBUTION_DERIVED, 'footer text = ' + JSON.stringify(f.text));
      assert(f.size >= 10, 'the credit renders at ' + f.size + 'px');
      assert(f.insidePanel, 'the credit line runs outside the panel');
      assert(f.inViewport, 'the credit sits outside the viewport');
      assert(!f.covered, 'the credit is covered');
      console.log(`    360px link boxes: dataset ${f.dataset.w}x${f.dataset.h}, licence ${f.licence.w}x${f.licence.h}`);
      for (const [what, l] of [['dataset', f.dataset], ['licence', f.licence]]) {
        assert(l.h >= 44, what + ' link is ' + l.h + 'px tall');
        assert(l.w >= 44, what + ' link is ' + l.w + 'px wide');
        assert(l.selfHit, what + ' link centre is hit by ' + l.hitBy);
      }
      assert(rec.consoleErrors.length === 0, 'console errors: ' + rec.consoleErrors.join(' | '));
    } finally { await ctx.close(); }
  });

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})();
