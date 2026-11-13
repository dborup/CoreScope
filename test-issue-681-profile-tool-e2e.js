/**
 * E2E (#681 M4): the "Path profile" measure tool on the Map page.
 *
 * The CI server has no map.dhm config and no Dataforsyningen token, so the
 * tool is switched on the way an operator's config would: /api/config/client
 * is intercepted and given the `dhm` block the Go server sends when the
 * proxy and the profile are both active, and /api/terrain/profile is
 * answered with a synthetic profile. The real endpoint (validation, token,
 * coverage gating, physics, cache) is covered by
 * cmd/server/terrain_profile_test.go against a fake upstream; the shapes
 * stubbed here were copied from a live curl of that endpoint.
 *
 * Asserts:
 *   1. the tool is hidden entirely without the profile flag, and makes no
 *      request at all;
 *   2. it opens and closes (toggle, close button, Escape);
 *   3. picking two node markers, and a node plus a free map point;
 *   4. the deep link restores the exact state, and survives a reload;
 *   5. partial coverage draws a gap and shows no verdict;
 *   6. `none` renders the empty state with no console errors;
 *   7. no token in any request the browser issues, and zero requests to
 *      dataforsyningen.dk;
 *   8. exactly one /api/terrain/profile call per profile;
 *   9. 360px portrait, 568x320 and 740x360 keep every control reachable and
 *      at least 48px, and the panel never covers the bottom nav;
 *  10. axe clean on the panel in light and dark.
 *
 * Usage:
 *   BASE_URL=http://localhost:13700 node test-issue-681-profile-tool-e2e.js
 * Optional: SCREENSHOT_DIR=<dir> writes the PR screenshots.
 */
'use strict';
const { chromium } = require('playwright');
const path = require('path');

const BASE = process.env.BASE_URL || 'http://localhost:13700';
const SHOT_DIR = process.env.SCREENSHOT_DIR || '';
const PROFILE_PATH = '/api/terrain/profile';
const ATTRIBUTION = 'Indeholder data fra Klimadatastyrelsen, Danmarks Højdemodel, CC BY 4.0';
const DHM_BLOCK = {
  enabled: true, url: '/api/map/dhm/wms', layer: 'dhm_terraen_skyggekort',
  minZoom: 7, maxZoom: 19, profile: true,
  bounds: [[54.4265, 7.99125], [57.778, 15.5995]],
  attribution: ATTRIBUTION,
};
// A single neutral-grey pixel stands in for every base-map tile: this suite
// must not reach any network, and a grey ground keeps the screenshots legible.
const PNG_1X1 = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGM4ceIEAAS0AlkWLoFAAAAAAElFTkSuQmCC',
  'base64');

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

// ── Synthetic profiles ──────────────────────────────────────────────────────
// Same field-for-field shape as a real GET /api/terrain/profile response.

function samples(n, D, fn) {
  const out = [];
  for (let i = 0; i < n; i++) {
    const d = Math.round((D * i / (n - 1)) * 100) / 100;
    out.push(Object.assign({ d: d }, fn(d, i)));
  }
  return out;
}

function baseProfile(q, over) {
  const D = 12000;
  const end = (p, hParam) => {
    const node = /^[0-9a-f]{64}$/.test(p || '');
    const asked = q.get(hParam);
    return {
      lat: 56.1629, lon: 10.2039, kind: node ? 'node' : 'point',
      role: node ? 'repeater' : undefined,
      groundM: 40,
      antennaM: asked !== null ? Number(asked) : (node ? 10 : 2),
      antennaSource: asked !== null ? 'request' : (node ? 'role-default' : 'point-default'),
      assumed: asked === null,
    };
  };
  return Object.assign({
    a: end(q.get('a'), 'ha'), b: end(q.get('b'), 'hb'),
    distanceM: D, stepM: 11.7, sampleCount: 1025, model: q.get('model') === 'dtm' ? 'dtm' : 'both',
    coverage: { state: 'full', pct: 100, firstGapM: null },
    samples: samples(120, D, (d) => ({ dtm: 30 + 18 * Math.sin(d / 900), dsm: 42 + 18 * Math.sin(d / 900) })),
    physics: { freqMHz: 869.618, freqSource: 'firmware', lambdaM: 0.34474, k: 1.3333333333333333, kSource: 'default', f1MaxM: 32.14 },
    verdict: { state: 'clear', model: 'dtm', criterion: 0.6, worstClearanceM: 41.2, worstAtM: 5000, worstF1Fraction: 1.4 },
    attribution: ATTRIBUTION,
  }, over || {});
}

function profileFor(scenario, q) {
  const D = 12000;
  if (scenario === 'blocked') {
    return baseProfile(q, {
      samples: samples(120, D, (d) => {
        const ridge = Math.abs(d - 6200) < 700 ? 130 * (1 - Math.abs(d - 6200) / 700) : 0;
        return { dtm: 18 + 6 * Math.sin(d / 900) + ridge, dsm: 27 + 6 * Math.sin(d / 900) + ridge };
      }),
      verdict: { state: 'blocked', model: 'dtm', criterion: 0.6, worstClearanceM: -72.4, worstAtM: 6200, worstF1Fraction: -2.3 },
    });
  }
  if (scenario === 'partial') {
    return baseProfile(q, {
      coverage: { state: 'partial', pct: 62.5, firstGapM: 7500 },
      b: Object.assign(baseProfile(q).b, { groundM: null }),
      samples: samples(120, D, (d) => (d >= 7500
        ? { dtm: null, dsm: null }
        : { dtm: 30 + 18 * Math.sin(d / 900), dsm: 42 + 18 * Math.sin(d / 900) })),
      verdict: { state: 'unknown', model: 'dtm', criterion: 0.6, worstClearanceM: null, worstAtM: null, worstF1Fraction: null },
    });
  }
  if (scenario === 'none') {
    return baseProfile(q, {
      a: Object.assign(baseProfile(q).a, { groundM: null }),
      b: Object.assign(baseProfile(q).b, { groundM: null }),
      coverage: { state: 'none', pct: 0, firstGapM: 0 },
      samples: [],
      verdict: { state: 'unknown', model: 'dtm', criterion: 0.6, worstClearanceM: null, worstAtM: null, worstF1Fraction: null },
    });
  }
  return baseProfile(q);
}

// ── Harness ─────────────────────────────────────────────────────────────────

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #681 M4 Path profile tool against ${BASE} ===\n`);

  async function newCtx(o) {
    o = o || {};
    const ctx = await browser.newContext({
      viewport: o.viewport || { width: 1280, height: 860 },
      hasTouch: !!o.touch,
      isMobile: !!o.touch,
    });
    const rec = { profile: [], consoleErrors: [], all: [], scenario: o.scenario || 'clear' };
    await ctx.route(BASE + '/api/config/client', async (route) => {
      const response = await route.fetch();
      const config = await response.json();
      delete config.dhm;
      if (o.dhm === 'no-profile') config.dhm = Object.assign({}, DHM_BLOCK, { profile: false });
      else if (o.dhm !== 'off') config.dhm = DHM_BLOCK;
      await route.fulfill({ response, json: config });
    });
    await ctx.route(url => new URL(url).pathname === PROFILE_PATH, async (route) => {
      const u = new URL(route.request().url());
      rec.profile.push(u);
      await route.fulfill({ status: 200, contentType: 'application/json',
        body: JSON.stringify(profileFor(rec.scenario, u.searchParams)) });
    });
    // Hillshade tiles and every third-party image are stubbed; nothing in
    // this suite may reach a real network.
    await ctx.route(/^https?:\/\/(?!localhost|127\.0\.0\.1)/i, async (route) => {
      await route.fulfill({ status: 200, contentType: 'image/png', body: PNG_1X1 });
    });
    await ctx.route(url => new URL(url).pathname === '/api/map/dhm/wms', async (route) => {
      await route.fulfill({ status: 200, contentType: 'image/png', body: PNG_1X1 });
    });
    await ctx.addInitScript(({ theme }) => {
      try {
        localStorage.setItem('meshcore-theme', theme);
        // Clustering is on by default; the picker needs individual markers.
        localStorage.setItem('meshcore-map-clustering', 'false');
        // The one-off gesture hints are a touch overlay from another feature;
        // mark them seen so they do not sit on top of the profile panel.
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

  // The committed fixture's nodes are in the Bay Area; the stubbed profile
// answers for any pair, so the viewport only has to put markers on screen.
const VIEW = { lat: 37.62, lon: -122.1, zoom: 10 };
  async function gotoMap(page, extra) {
    const qs = `lat=${VIEW.lat}&lon=${VIEW.lon}&zoom=${VIEW.zoom}` + (extra ? '&' + extra : '');
    await page.goto(`${BASE}/#/map?${qs}`, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#leaflet-map[data-loaded="true"]', { timeout: 30000 });
    await page.waitForFunction(() => window.MeshConfigReady !== undefined, null, { timeout: 15000 });
    await page.waitForTimeout(900);
  }

  const TOGGLE = '[data-testid="map-profile-toggle"]';
  const PANEL = '[data-testid="tp-panel"]';
  const CHART = '[data-testid="tp-chart"]';

  async function nodeKeys(page) {
    return page.evaluate(() => (window.__mc_nodes || [])
      .filter(n => n.public_key && n.lat && n.lon)
      .slice(0, 4).map(n => n.public_key.toLowerCase()));
  }

  // Leaflet keeps every marker in the DOM, including the ones off screen, and
  // the panel covers the lower part of the map once it opens. Pick markers
  // from the visible upper half, far enough apart that the second click
  // cannot land on the first one.
  async function clickMarkers(page, count) {
    const pts = await page.evaluate(() => {
      const box = document.getElementById('leaflet-map').getBoundingClientRect();
      return Array.from(document.querySelectorAll('.leaflet-marker-icon')).map((el) => {
        const r = el.getBoundingClientRect();
        return { x: Math.round(r.x + r.width / 2), y: Math.round(r.y + r.height / 2) };
      }).filter(p => p.x > box.x + 24 && p.x < box.right - 24
        && p.y > box.y + 24 && p.y < box.y + box.height * 0.5);
    });
    const chosen = [];
    for (const p of pts) {
      if (chosen.every(c => Math.abs(c.x - p.x) + Math.abs(c.y - p.y) > 48)) chosen.push(p);
      if (chosen.length === count) break;
    }
    assert(chosen.length === count, `need ${count} separated on-screen markers, found ${chosen.length} of ${pts.length}`);
    for (const c of chosen) {
      await page.mouse.click(c.x, c.y);
      await page.waitForTimeout(400);
    }
    return chosen;
  }

  async function panelState(page) {
    return page.evaluate(() => {
      const q = (s) => document.querySelector(s);
      const panel = q('[data-testid="tp-panel"]');
      const chip = q('[data-testid="tp-verdict"]');
      const ha = q('[data-testid="tp-h-a"]');
      const hb = q('[data-testid="tp-h-b"]');
      const svg = panel && panel.querySelector('svg.tp-svg');
      return {
        present: !!panel,
        open: !!(panel && panel.classList.contains('is-open')),
        chip: chip ? chip.textContent : null,
        chipState: chip ? chip.getAttribute('data-state') : null,
        detail: (q('[data-testid="tp-detail"]') || {}).textContent || '',
        meta: (q('[data-testid="tp-meta"]') || {}).textContent || '',
        nameA: (q('[data-testid="tp-name-a"]') || {}).textContent || '',
        nameB: (q('[data-testid="tp-name-b"]') || {}).textContent || '',
        assumedA: (q('[data-testid="tp-assumed-a"]') || {}).textContent || '',
        ha: ha ? ha.value : null,
        hb: hb ? hb.value : null,
        model: ['both', 'dtm'].filter(m => {
          const b = q('[data-testid="tp-model-' + m + '"]');
          return b && b.classList.contains('active');
        })[0] || null,
        svgTitle: svg ? (svg.querySelector('title') || {}).textContent || '' : null,
        svgRole: svg ? svg.getAttribute('role') : null,
        status: (q('[data-testid="tp-status"]') || {}).textContent || '',
        credit: (panel && panel.textContent.indexOf('Klimadatastyrelsen') >= 0) || false,
        // Counted over visible leaf elements, so the chart footer and the
        // empty-state paragraph never both show the credit.
        credits: panel ? Array.from(panel.querySelectorAll('text, p, span, div')).filter(e =>
          e.children.length === 0
          && (e.textContent || '').indexOf('Klimadatastyrelsen') >= 0
          && !e.closest('[hidden]')).length : 0,
        hash: location.hash,
      };
    });
  }

  // ── 1. hidden without the flag ────────────────────────────────────────────
  {
    const { ctx, page, rec } = await newCtx({ dhm: 'no-profile' });
    await gotoMap(page);
    await step('the tool is hidden when map.dhm.profile is off', async () => {
      assert(await page.$(TOGGLE) === null, 'toggle rendered without the profile flag');
      assert(await page.$(PANEL) === null, 'panel built without the profile flag');
      assert(rec.profile.length === 0, 'requests made with the tool disabled');
    });
    await ctx.close();
  }
  {
    const { ctx, page, rec } = await newCtx({ dhm: 'off' });
    await gotoMap(page);
    await step('the tool is hidden with no DHM block at all', async () => {
      assert(await page.$(TOGGLE) === null, 'toggle rendered without a dhm block');
      assert(rec.profile.length === 0, 'requests made without a dhm block');
    });
    await ctx.close();
  }

  // ── 2/3/8. open, pick, close ──────────────────────────────────────────────
  {
    const { ctx, page, rec } = await newCtx({});
    await gotoMap(page);
    await step('the toggle is offered, and nothing is fetched before it is pressed', async () => {
      assert(await page.$(TOGGLE) !== null, 'no toggle');
      assert((await panelState(page)).present === false, 'panel built before arming');
      assert(rec.profile.length === 0, 'request before arming');
    });

    await step('pressing the toggle arms the picker', async () => {
      await page.click(TOGGLE);
      await page.waitForTimeout(200);
      assert(await page.getAttribute(TOGGLE, 'aria-pressed') === 'true', 'toggle not armed');
      const hint = await page.$('[data-testid="map-profile-hint"]:not([hidden])');
      assert(hint, 'no picking hint');
      assert(rec.profile.length === 0, 'arming alone issued a request');
    });

    const keys = await nodeKeys(page);
    await step('picking two node markers measures exactly one profile', async () => {
      await clickMarkers(page, 2);
      await page.waitForTimeout(900);
      const st = await panelState(page);
      assert(st.open, 'panel did not open');
      assert(st.chip === 'Clear', 'chip = ' + st.chip);
      assert(st.svgRole === 'img', 'chart is not role=img');
      assert(/Clear/i.test(st.svgTitle), 'verdict missing from the chart title: ' + st.svgTitle);
      assert(st.credit, 'no CC BY 4.0 credit in the panel');
      assert(/869\.618 MHz/.test(st.meta), 'the frequency used is not shown: ' + st.meta);
      assert(/MeshCore firmware default/.test(st.meta), 'the frequency source is not shown: ' + st.meta);
      assert(/k = 1\.33/.test(st.meta), 'the k factor is not shown: ' + st.meta);
      assert(st.credits === 1, 'the CC BY 4.0 credit appears ' + st.credits + ' times');
      assert(rec.profile.length === 1, 'profile requests = ' + rec.profile.length);
      const q = rec.profile[0].searchParams;
      assert(/^[0-9a-f]{64}$/.test(q.get('a')), 'endpoint a is not a public key: ' + q.get('a'));
      assert(/^[0-9a-f]{64}$/.test(q.get('b')), 'endpoint b is not a public key: ' + q.get('b'));
      assert(keys.indexOf(q.get('a')) >= 0, 'endpoint a is not one of the map nodes');
    });

    await step('the deep link mirrors the picked pair', async () => {
      const st = await panelState(page);
      const params = new URLSearchParams(st.hash.split('?')[1] || '');
      const pair = (params.get('profile') || '').split(',');
      assert(pair.length === 2 && /^[0-9a-f]{64}$/.test(pair[0]), 'hash profile = ' + params.get('profile'));
      assert(params.get('lat') === String(VIEW.lat), 'the viewport deep link was dropped');
    });

    await step('the measured path is drawn on the map', async () => {
      const n = await page.evaluate(() => document.querySelectorAll('path.tp-path-line').length);
      assert(n === 1, 'tp-path-line count = ' + n);
    });

    await step('changing the model re-measures once', async () => {
      const before = rec.profile.length;
      await page.click('[data-testid="tp-model-dtm"]');
      await page.waitForTimeout(800);
      assert(rec.profile.length === before + 1, 'model change issued ' + (rec.profile.length - before) + ' requests');
      assert(rec.profile[rec.profile.length - 1].searchParams.get('model') === 'dtm', 'model not sent');
      assert((await panelState(page)).model === 'dtm', 'model button not active');
    });

    await step('the height steppers debounce into a single re-measure', async () => {
      const before = rec.profile.length;
      for (let i = 0; i < 4; i++) await page.click('[data-testid="tp-h-a-plus"]');
      await page.waitForTimeout(1200);
      const added = rec.profile.length - before;
      assert(added === 1, 'four stepper taps issued ' + added + ' requests');
      const st = await panelState(page);
      assert(Number(st.ha) >= 4, 'height A did not rise: ' + st.ha);
    });

    await step('Escape closes the panel and clears the path', async () => {
      await page.keyboard.press('Escape');
      await page.waitForTimeout(300);
      const st = await panelState(page);
      assert(!st.open, 'panel still open after Escape');
      const n = await page.evaluate(() => document.querySelectorAll('path.tp-path-line').length);
      assert(n === 0, 'path line left behind: ' + n);
      assert((st.hash.indexOf('profile=') === -1), 'profile left in the hash after close');
    });

    await step('the close button closes the panel too', async () => {
      await page.click(TOGGLE);
      await clickMarkers(page, 2);
      await page.waitForTimeout(700);
      assert((await panelState(page)).open, 'panel did not reopen');
      await page.click('[data-testid="tp-close"]');
      await page.waitForTimeout(250);
      assert(!(await panelState(page)).open, 'close button did not close the panel');
    });

    // Leaflet keeps a marker's handlers in _events. An idle marker carries
    // exactly one click handler, the one bindPopup installs; an armed one
    // carries the picker instead. Two means a picker was left behind.
    async function markerClickHandlers(page) {
      return page.evaluate(() => {
        const map = window.__mc_map;
        let max = 0, markers = 0;
        map.eachLayer((l) => {
          if (!(l instanceof window.L.Marker)) return;
          markers++;
          max = Math.max(max, ((l._events && l._events.click) || []).length);
        });
        return { max: max, markers: markers };
      });
    }

    await step('Escape while armed only disarms, and leaves no picker behind', async () => {
      await page.click(TOGGLE);
      await page.waitForTimeout(150);
      const armed = await markerClickHandlers(page);
      assert(armed.markers > 0, 'no markers on the map to check');
      await page.keyboard.press('Escape');
      await page.waitForTimeout(250);
      assert(await page.getAttribute(TOGGLE, 'aria-pressed') === 'false', 'still armed after Escape');
      const hint = await page.$('[data-testid="map-profile-hint"]:not([hidden])');
      assert(!hint, 'hint still visible after Escape');
      const idle = await markerClickHandlers(page);
      assert(idle.max <= 1, 'a marker kept ' + idle.max + ' click handlers after disarming');
    });

    await step('no token in any request, and nothing reaches dataforsyningen.dk', async () => {
      const bad = rec.all.filter(u => /dataforsyningen\.dk/i.test(u));
      assert(bad.length === 0, 'requests to the upstream service: ' + bad.join(', '));
      const tok = rec.all.filter(u => /[?&](token|ticket|api[_-]?key)=/i.test(u));
      assert(tok.length === 0, 'credential in a browser request: ' + tok.join(', '));
    });

    await step('no console errors through the whole flow', async () => {
      assert(rec.consoleErrors.length === 0, rec.consoleErrors.join(' | '));
    });
    await ctx.close();
  }

  // ── 3b. node plus a free map point ────────────────────────────────────────
  {
    const { ctx, page, rec } = await newCtx({});
    await gotoMap(page);
    await step('picking a node and then a free map point', async () => {
      await page.click(TOGGLE);
      await clickMarkers(page, 1);
      // Upper-right of the canvas, clear of every marker and of the panel.
      const spot = await page.evaluate(() => {
        const box = document.getElementById('leaflet-map').getBoundingClientRect();
        const marks = Array.from(document.querySelectorAll('.leaflet-marker-icon')).map((el) => el.getBoundingClientRect());
        const BLOCKERS = '.leaflet-control, .map-controls, .map-controls-toggle, .map-profile-toggle, .map-profile-hint, .tp-panel, .leaflet-marker-icon';
        for (let dx = 60; dx < box.width * 0.8; dx += 20) {
          for (let dy = 60; dy < box.height / 2; dy += 20) {
            const x = Math.round(box.right - dx), y = Math.round(box.y + dy);
            if (!marks.every(r => x < r.x - 30 || x > r.right + 30 || y < r.y - 30 || y > r.bottom + 30)) continue;
            const hit = document.elementFromPoint(x, y);
            if (hit && !hit.closest(BLOCKERS)) return { x: x, y: y };
          }
        }
        return null;
      });
      assert(spot, 'no free spot on the map');
      await page.mouse.click(spot.x, spot.y);
      await page.waitForTimeout(900);
      const st = await panelState(page);
      assert(st.open, 'panel did not open');
      assert(rec.profile.length === 1, 'profile requests = ' + rec.profile.length);
      const q = rec.profile[0].searchParams;
      assert(/^[0-9a-f]{64}$/.test(q.get('a')), 'a is not a node: ' + q.get('a'));
      assert(/^-?\d+(\.\d+)?,-?\d+(\.\d+)?$/.test(q.get('b')), 'b is not a lat,lon point: ' + q.get('b'));
      assert(/\d+\.\d+, -?\d+\.\d+/.test(st.nameB), 'B is not labelled as a point: ' + st.nameB);
      assert(/^Assumed/.test(st.assumedA) && /role default/.test(st.assumedA),
        'the role default is not labelled assumed: ' + st.assumedA);
    });
    await ctx.close();
  }

  // ── 4. deep link ──────────────────────────────────────────────────────────
  {
    const { ctx, page, rec } = await newCtx({});
    const PK = 'a'.repeat(64);
    const link = `profile=${PK},56.16290,10.20390&model=dtm&ha=12&hb=4&k=1.2&freq=868.5`;
    await gotoMap(page, link);
    await page.waitForTimeout(900);
    await step('a deep link restores the exact state with one request', async () => {
      const st = await panelState(page);
      assert(st.open, 'panel not opened from the deep link');
      assert(st.ha === '12', 'ha = ' + st.ha);
      assert(st.hb === '4', 'hb = ' + st.hb);
      assert(st.model === 'dtm', 'model = ' + st.model);
      assert(rec.profile.length === 1, 'profile requests = ' + rec.profile.length);
      const q = rec.profile[0].searchParams;
      assert(q.get('a') === PK, 'a = ' + q.get('a'));
      assert(q.get('b') === '56.16290,10.20390', 'b = ' + q.get('b'));
      assert(q.get('model') === 'dtm' && q.get('ha') === '12' && q.get('hb') === '4'
        && q.get('k') === '1.2' && q.get('freq') === '868.5', 'controls not forwarded: ' + q.toString());
    });
    await step('the hash still carries the same link after the tool rewrites it', async () => {
      const st = await panelState(page);
      const p = new URLSearchParams(st.hash.split('?')[1] || '');
      assert(p.get('profile') === `${PK},56.16290,10.20390`, 'profile = ' + p.get('profile'));
      assert(p.get('model') === 'dtm' && p.get('ha') === '12' && p.get('hb') === '4'
        && p.get('k') === '1.2' && p.get('freq') === '868.5', 'hash lost a control: ' + p.toString());
    });
    await step('a reload of that hash reproduces the same state', async () => {
      const before = rec.profile.length;
      await page.reload({ waitUntil: 'domcontentloaded' });
      await page.waitForSelector('#leaflet-map[data-loaded="true"]', { timeout: 30000 });
      await page.waitForTimeout(1200);
      const st = await panelState(page);
      assert(st.open && st.ha === '12' && st.hb === '4' && st.model === 'dtm',
        'reload lost the state: ' + JSON.stringify(st));
      assert(rec.profile.length === before + 1, 'reload issued ' + (rec.profile.length - before) + ' requests');
    });
    await step('a hostile deep link is ignored without opening anything', async () => {
      await gotoMap(page, 'profile=' + encodeURIComponent('<script>alert(1)</script>'));
      await page.waitForTimeout(600);
      const st = await panelState(page);
      assert(!st.open, 'a junk profile link opened the panel');
      assert(rec.consoleErrors.length === 0, rec.consoleErrors.join(' | '));
    });
    await ctx.close();
  }

  // ── 5. partial coverage ───────────────────────────────────────────────────
  {
    const { ctx, page, rec } = await newCtx({ scenario: 'partial' });
    await gotoMap(page);
    await step('partial coverage draws a gap and refuses a verdict', async () => {
      await page.click(TOGGLE);
      await clickMarkers(page, 2);
      await page.waitForTimeout(900);
      const st = await panelState(page);
      assert(st.chip === 'No verdict', 'chip = ' + st.chip);
      assert(st.chipState === 'unknown', 'chip state = ' + st.chipState);
      assert(/leaves Denmark/i.test(st.detail), 'detail = ' + st.detail);
      assert(/62\.5\s?%/.test(st.detail), 'coverage percentage missing: ' + st.detail);
      const gap = await page.evaluate(() => {
        const svg = document.querySelector('[data-testid="tp-chart"] svg');
        if (!svg) return null;
        const texts = Array.from(svg.querySelectorAll('text')).map(t => t.textContent);
        return { noData: texts.filter(t => t === 'no data').length, polygons: svg.querySelectorAll('polygon').length };
      });
      assert(gap && gap.noData >= 1, 'no gap marked in the chart: ' + JSON.stringify(gap));
      assert(!/Clear|Blocked|Marginal|Usable/.test(st.chip), 'a verdict was shown on a partial profile');
      assert(rec.consoleErrors.length === 0, rec.consoleErrors.join(' | '));
    });
    await ctx.close();
  }

  // ── 6. no coverage ────────────────────────────────────────────────────────
  {
    const { ctx, page, rec } = await newCtx({ scenario: 'none' });
    await gotoMap(page);
    await step('a path outside Denmark renders the empty state, with no console errors', async () => {
      await page.click(TOGGLE);
      await clickMarkers(page, 2);
      await page.waitForTimeout(900);
      const st = await panelState(page);
      assert(st.open, 'panel did not open');
      assert(st.chip === 'No verdict', 'chip = ' + st.chip);
      assert(/Denmark only/i.test(st.status), 'empty state = ' + st.status);
      const svgs = await page.evaluate(() => document.querySelectorAll('[data-testid="tp-chart"] svg').length);
      assert(svgs === 0, 'a chart was drawn with no samples');
      assert(rec.consoleErrors.length === 0, rec.consoleErrors.join(' | '));
    });
    await ctx.close();
  }

  // ── 9. small viewports ────────────────────────────────────────────────────
  const SMALL = [
    { name: '360x740 portrait', viewport: { width: 360, height: 740 } },
    { name: '568x320 landscape', viewport: { width: 568, height: 320 } },
    { name: '740x360 landscape', viewport: { width: 740, height: 360 } },
  ];
  const PAIR_LINK = 'profile=' + 'a'.repeat(64) + ',' + 'b'.repeat(64);
  for (const vp of SMALL) {
    const { ctx, page, rec } = await newCtx({ viewport: vp.viewport, touch: true });
    await gotoMap(page, PAIR_LINK);
    await step(vp.name + ': every control is reachable and at least 48px', async () => {
      await page.waitForTimeout(900);
      assert((await panelState(page)).open, 'panel did not open');
      const report = await page.evaluate(() => {
        const panel = document.querySelector('[data-testid="tp-panel"]');
        const body = document.querySelector('[data-testid="tp-body"]');
        const ids = ['tp-close', 'tp-h-a-minus', 'tp-h-a', 'tp-h-a-plus', 'tp-h-b-minus', 'tp-h-b', 'tp-h-b-plus', 'tp-model-both', 'tp-model-dtm'];
        const out = { scrollable: false, scrollBox: 0, panelBottom: 0, viewport: window.innerHeight, controls: [] };
        if (!panel || !body) return out;
        const cs = getComputedStyle(body);
        out.scrollable = cs.overflowY === 'auto' || cs.overflowY === 'scroll';
        out.scrollBox = Math.round(body.clientHeight);
        out.panelBottom = Math.round(panel.getBoundingClientRect().bottom);
        for (const id of ids) {
          const el = document.querySelector('[data-testid="' + id + '"]');
          if (!el) { out.controls.push({ id: id, missing: true }); continue; }
          el.scrollIntoView({ block: 'center' });
          const r = el.getBoundingClientRect();
          const hit = document.elementFromPoint(Math.round(r.x + r.width / 2), Math.round(r.y + r.height / 2));
          out.controls.push({
            id: id, w: Math.round(r.width), h: Math.round(r.height),
            inView: r.top >= 0 && r.bottom <= window.innerHeight + 1,
            hittable: !!hit && (el === hit || el.contains(hit) || hit.contains(el)),
            cover: hit ? (hit.tagName + '.' + (hit.className && hit.className.baseVal !== undefined ? hit.className.baseVal : hit.className)) : 'none',
          });
        }
        return out;
      });
      assert(report.scrollable, 'the panel body is not a scroll box');
      assert(report.scrollBox > 0, 'the panel body has no height');
      assert(report.panelBottom <= report.viewport + 1, 'the panel runs past the viewport: ' + JSON.stringify(report));
      for (const c of report.controls) {
        assert(!c.missing, 'missing control ' + c.id);
        assert(c.w >= 48 && c.h >= 48, `${c.id} is ${c.w}x${c.h}, below the 48px touch target`);
        assert(c.inView, `${c.id} could not be scrolled into view`);
        assert(c.hittable, `${c.id} is covered by ${c.cover}`);
      }
      // The bottom nav must stay usable underneath the panel.
      const nav = await page.evaluate(() => {
        const n = document.querySelector('[data-bottom-nav]');
        if (!n || getComputedStyle(n).display === 'none') return null;
        const r = n.getBoundingClientRect();
        const hit = document.elementFromPoint(Math.round(r.x + r.width / 2), Math.round(r.y + r.height / 2));
        return { covered: !(hit && (n === hit || n.contains(hit))) };
      });
      assert(!nav || !nav.covered, 'the profile panel covers the bottom nav');
      assert(rec.consoleErrors.length === 0, rec.consoleErrors.join(' | '));
    });
    await ctx.close();
  }

  // ── 10. axe, light and dark ───────────────────────────────────────────────
  const AXE_RULES = ['color-contrast', 'image-alt', 'label', 'aria-required-attr', 'aria-valid-attr',
    'aria-valid-attr-value', 'region', 'button-name', 'link-name', 'duplicate-id'];
  for (const theme of ['light', 'dark']) {
    const { ctx, page } = await newCtx({ theme: theme });
    await gotoMap(page, PAIR_LINK);
    await step('axe is clean on the panel in ' + theme, async () => {
      const { AxeBuilder } = require('@axe-core/playwright');
      await page.waitForTimeout(900);
      assert((await panelState(page)).open, 'panel did not open');
      const res = await new AxeBuilder({ page }).include(PANEL).withRules(AXE_RULES).analyze();
      const v = res.violations.map(x => x.id + ' x' + x.nodes.length);
      assert(res.violations.length === 0, 'axe violations: ' + v.join(', '));
    });
    await ctx.close();
  }

  // ── Screenshots for the PR ────────────────────────────────────────────────
  if (SHOT_DIR) {
    const shots = [
      { file: 'clear', scenario: 'clear', viewport: { width: 1280, height: 860 } },
      { file: 'blocked', scenario: 'blocked', viewport: { width: 1280, height: 860 } },
      { file: 'partial', scenario: 'partial', viewport: { width: 1280, height: 860 } },
    ];
    for (const theme of ['light', 'dark']) {
      for (const s of shots) {
        const { ctx, page } = await newCtx({ theme, scenario: s.scenario, viewport: s.viewport });
        await gotoMap(page);
        await page.click(TOGGLE);
        await clickMarkers(page, 2);
        await page.waitForTimeout(1200);
        await page.screenshot({ path: path.join(SHOT_DIR, `681-m4-${s.file}-${theme}.png`) });
        await ctx.close();
      }
    }
    for (const vp of [{ n: '360-portrait', w: 360, h: 740 }, { n: '740x360-landscape', w: 740, h: 360 }]) {
      const { ctx, page } = await newCtx({ theme: 'dark', scenario: 'blocked', viewport: { width: vp.w, height: vp.h }, touch: true });
      await gotoMap(page, PAIR_LINK);
      await page.waitForTimeout(1200);
      await page.screenshot({ path: path.join(SHOT_DIR, `681-m4-${vp.n}.png`) });
      await ctx.close();
    }
    console.log('  screenshots → ' + SHOT_DIR);
  }

  await browser.close();
  console.log(`\n#681 M4 E2E: ${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
