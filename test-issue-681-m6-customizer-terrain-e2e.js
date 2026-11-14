/**
 * E2E (#681 M6): the customizer's terrain-profile settings, and their effect
 * on the Path profile tool on the Map page.
 *
 * The CI server has no map.dhm config and no Dataforsyningen token, so the
 * tool is switched on the way an operator's config would: /api/config/client
 * is intercepted and given the `dhm` block the Go server sends when the
 * proxy and the profile are both active, and /api/terrain/profile is
 * answered with a synthetic profile that honours the ha/hb/model parameters
 * exactly as cmd/server/terrain_profile.go does.
 *
 * Asserts:
 *   1. the Display tab carries the five antenna-height defaults and the
 *      elevation-model select, and the Theme tab the six chart colours in
 *      both light and dark;
 *   2. a changed antenna default is actually sent and changes the rendered
 *      profile, while an operator-set height is left alone;
 *   3. a changed chart colour is what the chart draws with;
 *   4. the default model reaches a fresh profile;
 *   5. every new control is at least 48px on touch at 360px wide, and
 *      reachable in 740x360 landscape;
 *   6. the export carries the terrain section and no token, and nothing
 *      under map.dhm is anywhere in the client;
 *   7. axe and measured text contrast are clean in light and dark with the
 *      default colours;
 *   8. review PR#714: a hex height planted in storage is no default, a
 *      pick over the translucent clutter colour keeps its alpha, and the
 *      section is gone when the server has the profile switched off.
 *
 * Usage:
 *   BASE_URL=http://localhost:13800 node test-issue-681-m6-customizer-terrain-e2e.js
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
const PNG_1X1 = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGM4ceIEAAS0AlkWLoFAAAAAAElFTkSuQmCC',
  'base64');

const PK_A = 'a'.repeat(64);
const PK_B = 'b'.repeat(64);
const PAIR_LINK = 'profile=' + PK_A + ',' + PK_B;

const HEIGHT_FIELDS = ['heightRepeater', 'heightRoom', 'heightCompanion', 'heightSensor', 'heightPoint'];
const CHART_VARS = ['--terrain-fill', '--terrain-clutter', '--los-line',
  '--fresnel-100', '--fresnel-60', '--profile-blocked'];
const CHART_KEYS = ['terrainFill', 'terrainClutter', 'losLine', 'fresnel100', 'fresnel60', 'profileBlocked'];

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

// ── Synthetic profile ───────────────────────────────────────────────────────
// Same field-for-field shape as GET /api/terrain/profile, and it resolves the
// antenna height the way the server does: the request, then an operator value
// for this node, then the built-in role default.

function samples(n, D, fn) {
  const out = [];
  for (let i = 0; i < n; i++) {
    const d = Math.round((D * i / (n - 1)) * 100) / 100;
    out.push(Object.assign({ d: d }, fn(d, i)));
  }
  return out;
}

const ROLE_DEFAULT_M = 10;       // terrainRoleAntennaM["repeater"]
const OPERATOR_NODE_M = 18;      // a height "the operator set" for PK_B

function profileFor(q, opts) {
  const D = 12000;
  const both = q.get('model') !== 'dtm';
  const end = (param, hParam, operator) => {
    const asked = q.get(hParam);
    if (asked !== null) {
      return { lat: 56.1629, lon: 10.2039, kind: 'node', role: 'repeater', groundM: 40,
        antennaM: Number(asked), antennaSource: 'request', assumed: false };
    }
    if (operator) {
      return { lat: 56.1629, lon: 10.2039, kind: 'node', role: 'repeater', groundM: 40,
        antennaM: OPERATOR_NODE_M, antennaSource: 'operator-node', assumed: false };
    }
    return { lat: 56.1629, lon: 10.2039, kind: 'node', role: 'repeater', groundM: 40,
      antennaM: ROLE_DEFAULT_M, antennaSource: 'role-default', assumed: true };
  };
  const op = (opts && opts.operator) || false;
  return {
    a: end(q.get('a'), 'ha', false),
    b: end(q.get('b'), 'hb', op),
    distanceM: D, stepM: 11.7, sampleCount: 1025, model: both ? 'both' : 'dtm',
    coverage: { state: 'full', pct: 100, firstGapM: null },
    samples: samples(120, D, (d) => {
      const dtm = 30 + 18 * Math.sin(d / 900);
      // A ridge at mid-path, so terrain reaches into the zone and the
      // obstruction colour is actually drawn.
      const ridge = Math.abs(d - 6200) < 700 ? 60 * (1 - Math.abs(d - 6200) / 700) : 0;
      const s = { dtm: dtm + ridge };
      if (both) s.dsm = dtm + ridge + 12;
      return s;
    }),
    physics: { freqMHz: 869.618, freqSource: 'firmware', lambdaM: 0.34474,
      k: 1.3333333333333333, kSource: 'default', f1MaxM: 32.14 },
    verdict: { state: 'blocked', model: 'dtm', criterion: 0.6,
      worstClearanceM: -12.4, worstAtM: 6200, worstF1Fraction: -0.4 },
    attribution: ATTRIBUTION,
  };
}

// ── Harness ─────────────────────────────────────────────────────────────────

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #681 M6 customizer terrain settings against ${BASE} ===\n`);

  async function newCtx(o) {
    o = o || {};
    const ctx = await browser.newContext({
      viewport: o.viewport || { width: 1280, height: 900 },
      hasTouch: !!o.touch, isMobile: !!o.touch,
    });
    const rec = { profile: [], consoleErrors: [], all: [] };
    await ctx.route(BASE + '/api/config/client', async (route) => {
      const response = await route.fetch();
      const config = await response.json();
      delete config.dhm;
      config.dhm = o.dhm !== undefined ? o.dhm : DHM_BLOCK;
      await route.fulfill({ response, json: config });
    });
    await ctx.route(url => new URL(url).pathname === PROFILE_PATH, async (route) => {
      const u = new URL(route.request().url());
      rec.profile.push(u);
      await route.fulfill({ status: 200, contentType: 'application/json',
        body: JSON.stringify(profileFor(u.searchParams, { operator: !!o.operator })) });
    });
    await ctx.route(/^https?:\/\/(?!localhost|127\.0\.0\.1)/i, async (route) => {
      await route.fulfill({ status: 200, contentType: 'image/png', body: PNG_1X1 });
    });
    await ctx.route(url => new URL(url).pathname === '/api/map/dhm/wms', async (route) => {
      await route.fulfill({ status: 200, contentType: 'image/png', body: PNG_1X1 });
    });
    await ctx.addInitScript(({ theme, overrides }) => {
      try {
        localStorage.setItem('meshcore-theme', theme);
        localStorage.setItem('meshcore-map-clustering', 'false');
        ['row-swipe', 'tab-swipe', 'edge-drawer', 'pull-refresh']
          .forEach(k => localStorage.setItem('meshcore-gesture-hints-' + k, 'seen'));
        if (overrides) localStorage.setItem('cs-theme-overrides', overrides);
      } catch (_) {}
      const apply = () => document.documentElement && document.documentElement.setAttribute('data-theme', theme);
      apply();
      document.addEventListener('DOMContentLoaded', apply);
    }, { theme: o.theme || 'light', overrides: o.overrides ? JSON.stringify(o.overrides) : null });
    const page = await ctx.newPage();
    page.on('console', m => { if (m.type() === 'error') rec.consoleErrors.push(m.text()); });
    page.on('pageerror', e => rec.consoleErrors.push(String(e)));
    page.on('request', r => rec.all.push(r.url()));
    return { ctx, page, rec };
  }

  const VIEW = { lat: 37.62, lon: -122.1, zoom: 10 };
  async function gotoMap(page, extra) {
    const qs = `lat=${VIEW.lat}&lon=${VIEW.lon}&zoom=${VIEW.zoom}` + (extra ? '&' + extra : '');
    await page.goto(`${BASE}/#/map?${qs}`, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#leaflet-map[data-loaded="true"]', { timeout: 30000 });
    await page.waitForFunction(() => window._customizerV2 && window._customizerV2.initDone, null, { timeout: 15000 });
    await page.waitForTimeout(900);
  }

  async function openCustomizer(page, tab) {
    await page.click('#customizeToggle');
    await page.waitForSelector('.cust-overlay:not(.hidden)', { timeout: 5000 });
    if (tab) {
      await page.click(`.cust-tab[data-tab="${tab}"]`);
      await page.waitForSelector(`.cust-panel[data-panel="${tab}"].active`, { timeout: 4000 });
    }
  }

  async function setHeight(page, key, value) {
    const sel = `[data-cv2-terrain-height="${key}"]`;
    await page.fill(sel, String(value));
    await page.dispatchEvent(sel, 'change');
    await page.waitForTimeout(600); // the customizer's debounced write
  }

  // ── 1. The controls exist ─────────────────────────────────────────────────

  {
    const { ctx, page, rec } = await newCtx({});
    await gotoMap(page);
    await step('the Display tab carries all five antenna-height defaults', async () => {
      await openCustomizer(page, 'display');
      const seen = await page.evaluate((keys) => keys.map((k) => {
        const el = document.querySelector('[data-cv2-terrain-height="' + k + '"]');
        if (!el) return null;
        const label = document.querySelector('label[for="' + el.id + '"]');
        return { value: el.value, min: el.min, max: el.max, type: el.type,
          labelled: !!(label && label.textContent.trim()) };
      }), HEIGHT_FIELDS);
      HEIGHT_FIELDS.forEach((k, i) => {
        assert(seen[i], 'missing the ' + k + ' field');
        assert(seen[i].type === 'number', k + ' is not a number input');
        assert(seen[i].min === '0' && seen[i].max === '100', k + ' is not bounded 0-100');
        assert(seen[i].labelled, k + ' has no label');
      });
      // The built-in role defaults, so an untouched panel promises nothing new.
      assert(seen.map(s => s.value).join(',') === '10,5,2,2,2',
        'defaults are ' + seen.map(s => s.value).join(','));
    });

    await step('the Display tab carries the elevation-model default', async () => {
      const sel = await page.evaluate(() => {
        const el = document.querySelector('[data-cv2-terrain-model]');
        if (!el) return null;
        return { value: el.value, options: Array.from(el.options).map(o => o.value) };
      });
      assert(sel, 'no elevation-model select');
      assert(sel.value === 'both', 'the default model is ' + sel.value);
      assert(JSON.stringify(sel.options.slice().sort()) === '["both","dtm"]',
        'model options are ' + sel.options.join(','));
    });

    await step('nothing server-side is offered anywhere in the panel', async () => {
      const found = await page.evaluate(() => {
        const panel = document.querySelector('.cust-overlay');
        const hooks = Array.from(panel.querySelectorAll('[data-cv2-field],[data-cv2-select],[data-cv2-terrain-height],[data-cv2-terrain-model],[data-cv2-slider]'))
          .map(el => el.getAttribute('data-cv2-field') || el.getAttribute('data-cv2-select')
            || el.getAttribute('data-cv2-terrain-height') || (el.hasAttribute('data-cv2-terrain-model') ? 'terrain.model' : '')
            || el.getAttribute('data-cv2-slider') || '');
        return { hooks: hooks, text: panel.textContent };
      });
      found.hooks.forEach((h) => {
        assert(!/token|dhm|kfactor|ratelimit|cachettl|maxpath|chunk|defaultfreq/i.test(h),
          'a control is bound to a server-side value: ' + h);
      });
      assert(!/token/i.test(found.text), 'the panel mentions a token');
      assert(!/dataforsyningen/i.test(found.text), 'the panel mentions the upstream service');
      assert(rec.consoleErrors.length === 0, rec.consoleErrors.join(' | '));
    });
    await ctx.close();
  }

  for (const theme of ['light', 'dark']) {
    const { ctx, page } = await newCtx({ theme });
    await gotoMap(page);
    await step('the Theme tab carries the six chart colours in ' + theme, async () => {
      await openCustomizer(page, 'theme');
      const section = theme === 'dark' ? 'themeDark' : 'theme';
      const rows = await page.evaluate((s) => Array.from(
        document.querySelectorAll('.cust-terrain-colors [data-cv2-field]'))
        .map(el => el.getAttribute('data-cv2-field')).filter(f => f.indexOf(s + '.') === 0), section);
      CHART_KEYS.forEach((k) => {
        assert(rows.indexOf(section + '.' + k) >= 0, 'no ' + section + '.' + k + ' row');
      });
      assert(rows.length === 6, 'the group has ' + rows.length + ' rows');
    });
    await ctx.close();
  }

  // ── 2. A changed antenna default changes the profile ──────────────────────

  {
    const { ctx, page, rec } = await newCtx({});
    await gotoMap(page, PAIR_LINK);
    let before = null;
    await step('a profile with the built-in defaults is the baseline', async () => {
      await page.waitForSelector('[data-testid="tp-chart"] svg.tp-svg', { timeout: 10000 });
      before = await page.evaluate(() => ({
        los: document.querySelector('[data-testid="tp-chart"] svg.tp-svg').outerHTML,
        ha: document.querySelector('[data-testid="tp-h-a"]').value,
        badge: document.querySelector('[data-testid="tp-hsrc-a"]').textContent,
      }));
      assert(before.ha === String(ROLE_DEFAULT_M), 'baseline height is ' + before.ha);
      assert(before.badge === 'default', 'baseline badge is ' + before.badge);
      assert(rec.profile.length === 1, 'baseline took ' + rec.profile.length + ' requests');
      assert(rec.profile[0].searchParams.get('ha') === null, 'the baseline sent a height');
    });

    await step('changing the repeater default re-measures and redraws', async () => {
      await openCustomizer(page, 'display');
      await setHeight(page, 'heightRepeater', 45);
      await page.waitForFunction(() => {
        const el = document.querySelector('[data-testid="tp-h-a"]');
        return el && el.value === '45';
      }, null, { timeout: 8000 });
      const asked = rec.profile[rec.profile.length - 1].searchParams;
      assert(asked.get('ha') === '45', 'the new default was not sent: ha=' + asked.get('ha'));
      assert(asked.get('hb') === '45', 'the new default was not sent: hb=' + asked.get('hb'));
      const after = await page.evaluate(() => ({
        los: document.querySelector('[data-testid="tp-chart"] svg.tp-svg').outerHTML,
        badge: document.querySelector('[data-testid="tp-hsrc-a"]').textContent,
        assumed: !document.querySelector('[data-testid="tp-assumed-chip"]').hidden,
      }));
      assert(after.los !== before.los, 'the rendered profile did not change');
      assert(after.badge === 'default', 'the badge now reads ' + after.badge);
      assert(after.assumed, 'a class-wide default is still an assumption');
      assert(rec.consoleErrors.length === 0, rec.consoleErrors.join(' | '));
    });

    await step('the customizer default is not written into the link', async () => {
      const hash = await page.evaluate(() => location.hash);
      assert(hash.indexOf('ha=') === -1, 'the hash carries an ambient default: ' + hash);
      assert(hash.indexOf('hb=') === -1, 'the hash carries an ambient default: ' + hash);
    });
    await ctx.close();
  }

  {
    // An operator who named a real height for a node outranks the customizer.
    const { ctx, page, rec } = await newCtx({
      operator: true,
      overrides: { terrain: { heightRepeater: 45 } },
    });
    await gotoMap(page, PAIR_LINK);
    await step('an operator-set height is never overruled by the customizer', async () => {
      await page.waitForSelector('[data-testid="tp-chart"] svg.tp-svg', { timeout: 10000 });
      await page.waitForFunction(() => {
        const b = document.querySelector('[data-testid="tp-hsrc-b"]');
        return b && b.textContent;
      }, null, { timeout: 8000 });
      const got = await page.evaluate(() => ({
        ha: document.querySelector('[data-testid="tp-h-a"]').value,
        hb: document.querySelector('[data-testid="tp-h-b"]').value,
        badgeA: document.querySelector('[data-testid="tp-hsrc-a"]').textContent,
        badgeB: document.querySelector('[data-testid="tp-hsrc-b"]').textContent,
      }));
      assert(got.ha === '45', 'A should take the customizer default, got ' + got.ha);
      assert(got.badgeA === 'default', 'A badge is ' + got.badgeA);
      assert(got.hb === String(OPERATOR_NODE_M), 'B lost the operator height, got ' + got.hb);
      assert(got.badgeB === 'operator', 'B badge is ' + got.badgeB);
      const last = rec.profile[rec.profile.length - 1].searchParams;
      assert(last.get('hb') === null, 'the customizer overwrote the operator height');
      assert(rec.consoleErrors.length === 0, rec.consoleErrors.join(' | '));
    });
    await ctx.close();
  }

  {
    // Review PR#714: Number("0x1F4") is 500, and the profile used to clamp
    // it to 100 m while the customizer showed 10. Planted straight into
    // storage, past the write-side validation, as an old override would be.
    const { ctx, page, rec } = await newCtx({ overrides: { terrain: { heightRepeater: '0x1F4' } } });
    await gotoMap(page, PAIR_LINK);
    await step('a hex height in storage is no default, in the profile or the customizer', async () => {
      await page.waitForSelector('[data-testid="tp-chart"] svg.tp-svg', { timeout: 10000 });
      await page.waitForTimeout(600);
      assert(rec.profile.length === 1, 'the hex height cost a second request: ' + rec.profile.map(u => u.search).join(' '));
      assert(rec.profile[0].searchParams.get('ha') === null, 'ha=' + rec.profile[0].searchParams.get('ha'));
      const ha = await page.evaluate(() => document.querySelector('[data-testid="tp-h-a"]').value);
      assert(ha === String(ROLE_DEFAULT_M), 'the profile drew ' + ha + ' m');
      await openCustomizer(page, 'display');
      const shown = await page.evaluate(() => document.querySelector('[data-cv2-terrain-height="heightRepeater"]').value);
      assert(shown === ha, 'the customizer shows ' + shown + ' m, the profile draws ' + ha + ' m');
    });
    await ctx.close();
  }

  {
    // Review PR#714: an instance with the profile switched off offers no
    // defaults for it.
    for (const dhm of [null, Object.assign({}, DHM_BLOCK, { profile: false })]) {
      const { ctx, page, rec } = await newCtx({ dhm });
      await gotoMap(page);
      await step('the Terrain Profile section is hidden with dhm ' + (dhm ? 'profile:false' : 'absent'), async () => {
        await openCustomizer(page, 'display');
        const got = await page.evaluate(() => ({
          fields: document.querySelectorAll('[data-cv2-terrain-height], [data-cv2-terrain-model]').length,
          title: Array.from(document.querySelectorAll('.cust-panel[data-panel="display"] .cust-section-title'))
            .some(el => /Terrain Profile/.test(el.textContent)),
        }));
        assert(got.fields === 0, got.fields + ' terrain fields still shown');
        assert(!got.title, 'the Terrain Profile title is still shown');
        assert(rec.consoleErrors.length === 0, rec.consoleErrors.join(' | '));
      });
      await ctx.close();
    }
  }

  // ── 3. A changed chart colour is what the chart draws with ────────────────

  {
    const { ctx, page, rec } = await newCtx({});
    await gotoMap(page, PAIR_LINK);
    await step('a chart colour picked in the customizer is what the chart uses', async () => {
      await page.waitForSelector('[data-testid="tp-chart"] svg.tp-svg', { timeout: 10000 });
      await openCustomizer(page, 'theme');
      await page.evaluate(() => {
        const d = document.querySelector('.cust-terrain-colors');
        if (d) d.open = true;
      });
      const sel = '[data-cv2-field="theme.terrainFill"]';
      await page.waitForSelector(sel, { timeout: 4000 });
      await page.fill(sel, '#ff00ff');
      await page.dispatchEvent(sel, 'input');
      await page.dispatchEvent(sel, 'change');
      await page.waitForTimeout(700);
      const got = await page.evaluate(() => {
        const cs = getComputedStyle(document.documentElement);
        const poly = document.querySelector('[data-testid="tp-chart"] svg.tp-svg polygon');
        return {
          cssVar: cs.getPropertyValue('--terrain-fill').trim(),
          fill: poly ? getComputedStyle(poly).fill : null,
          stored: localStorage.getItem('cs-theme-overrides'),
        };
      });
      assert(got.cssVar === '#ff00ff', '--terrain-fill is ' + got.cssVar);
      assert(got.fill === 'rgb(255, 0, 255)', 'the chart draws with ' + got.fill);
      assert(JSON.parse(got.stored).theme.terrainFill === '#ff00ff', 'the pick was not stored');
      assert(rec.consoleErrors.length === 0, rec.consoleErrors.join(' | '));
    });

    await step('the obstruction colour follows the customizer too', async () => {
      const sel = '[data-cv2-field="theme.profileBlocked"]';
      await page.fill(sel, '#00ff00');
      await page.dispatchEvent(sel, 'input');
      await page.dispatchEvent(sel, 'change');
      await page.waitForTimeout(700);
      const got = await page.evaluate(() => {
        const band = document.querySelector('[data-testid="tp-chart"] rect.tp-intrusion');
        return { var: getComputedStyle(document.documentElement).getPropertyValue('--profile-blocked').trim(),
          fill: band ? getComputedStyle(band).fill : null };
      });
      assert(got.var === '#00ff00', '--profile-blocked is ' + got.var);
      assert(got.fill === 'rgb(0, 255, 0)', 'the intrusion band draws with ' + got.fill);
    });

    await step('a pick over the translucent clutter colour keeps its alpha', async () => {
      // Review PR#714: the native picker has no alpha channel, so the first
      // pick used to make the canopy opaque.
      const sel = '[data-cv2-field="theme.terrainClutter"]';
      await page.fill(sel, '#ff0000');
      await page.dispatchEvent(sel, 'input');
      await page.dispatchEvent(sel, 'change');
      await page.waitForTimeout(700);
      const got = await page.evaluate(() => ({
        cssVar: getComputedStyle(document.documentElement).getPropertyValue('--terrain-clutter').trim(),
        stored: JSON.parse(localStorage.getItem('cs-theme-overrides')).theme.terrainClutter,
      }));
      assert(got.stored === 'rgba(255, 0, 0, 0.55)', 'stored ' + got.stored);
      assert(/^rgba\(255, 0, 0, 0\.55\)$/.test(got.cssVar), '--terrain-clutter is ' + got.cssVar);
      assert(rec.consoleErrors.length === 0, rec.consoleErrors.join(' | '));
    });

    await step('a reset puts every chart colour back', async () => {
      await page.evaluate(() => window._customizerV2.resetAll());
      await page.waitForTimeout(400);
      const got = await page.evaluate((vars) => {
        const root = document.documentElement;
        const inline = vars.filter(v => root.style.getPropertyValue(v) !== '');
        const cs = getComputedStyle(root);
        return { inline: inline, resolved: vars.map(v => cs.getPropertyValue(v).trim()) };
      }, CHART_VARS);
      assert(got.inline.length === 0, 'reset left ' + got.inline.join(', ') + ' on :root');
      got.resolved.forEach((v, i) => {
        assert(v, CHART_VARS[i] + ' has no value after reset');
      });
    });
    await ctx.close();
  }

  // ── 4. The default model reaches a fresh profile ──────────────────────────

  {
    const { ctx, page, rec } = await newCtx({ overrides: { terrain: { model: 'dtm' } } });
    await gotoMap(page, PAIR_LINK);
    await step('a terrain-only default opens a terrain-only profile', async () => {
      await page.waitForSelector('[data-testid="tp-chart"] svg.tp-svg', { timeout: 10000 });
      assert(rec.profile[0].searchParams.get('model') === 'dtm',
        'the request asked for ' + rec.profile[0].searchParams.get('model'));
      const active = await page.evaluate(() => ['both', 'dtm'].filter(m =>
        document.querySelector('[data-testid="tp-model-' + m + '"]').classList.contains('active')));
      assert(active.join(',') === 'dtm', 'the active model button is ' + active.join(','));
    });
    await ctx.close();
  }

  {
    const { ctx, page, rec } = await newCtx({ overrides: { terrain: { model: 'dtm' } } });
    await gotoMap(page, PAIR_LINK + '&model=both');
    await step('a link that spells the model out still wins', async () => {
      await page.waitForSelector('[data-testid="tp-chart"] svg.tp-svg', { timeout: 15000 });
      assert(rec.profile[0].searchParams.get('model') === 'both',
        'the request asked for ' + rec.profile[0].searchParams.get('model'));
      const active = await page.evaluate(() => ['both', 'dtm'].filter(m =>
        document.querySelector('[data-testid="tp-model-' + m + '"]').classList.contains('active')));
      assert(active.join(',') === 'both', 'the link was ignored, model is ' + active.join(','));
      assert(rec.consoleErrors.length === 0, rec.consoleErrors.join(' | '));
    });
    await ctx.close();
  }

  // ── 5. Touch targets and landscape reachability ───────────────────────────

  for (const vp of [{ n: '360x740 portrait', w: 360, h: 740 }, { n: '740x360 landscape', w: 740, h: 360 }]) {
    const { ctx, page, rec } = await newCtx({ viewport: { width: vp.w, height: vp.h }, touch: true });
    await gotoMap(page);
    await step('every new control is at least 48px and reachable at ' + vp.n, async () => {
      // On a narrow viewport the palette button moves into the page-actions
      // sheet, so open the panel through its own API rather than hunting it.
      await page.evaluate(() => window._customizerV2 && document.getElementById('customizeToggle').click());
      await page.waitForSelector('.cust-overlay:not(.hidden)', { timeout: 5000 });
      await page.click('.cust-tab[data-tab="display"]');
      await page.waitForSelector('.cust-panel[data-panel="display"].active', { timeout: 4000 });
      const controls = await page.evaluate((keys) => {
        const out = [];
        const add = (el, id) => {
          if (!el) { out.push({ id: id, missing: true }); return; }
          el.scrollIntoView({ block: 'center' });
          const r = el.getBoundingClientRect();
          const cx = Math.round(r.x + r.width / 2), cy = Math.round(r.y + r.height / 2);
          const hit = document.elementFromPoint(cx, cy);
          out.push({
            id: id, w: Math.round(r.width), h: Math.round(r.height),
            inView: r.y >= 0 && r.bottom <= window.innerHeight + 1,
            hittable: !!(hit && (hit === el || el.contains(hit) || hit.contains(el))),
            cover: hit ? (hit.getAttribute('data-testid') || hit.className || hit.tagName) : 'nothing',
          });
        };
        keys.forEach(k => add(document.querySelector('[data-cv2-terrain-height="' + k + '"]'), k));
        add(document.querySelector('[data-cv2-terrain-model]'), 'terrain.model');
        return out;
      }, HEIGHT_FIELDS);
      for (const c of controls) {
        assert(!c.missing, c.id + ' is missing');
        assert(c.h >= 48, `${c.id} is ${c.w}x${c.h}, below the 48px touch target`);
        assert(c.inView, c.id + ' could not be scrolled into view');
        assert(c.hittable, c.id + ' is covered by ' + c.cover);
      }
      assert(rec.consoleErrors.length === 0, rec.consoleErrors.join(' | '));
    });

    await step('the chart colour swatches are 48px on touch at ' + vp.n, async () => {
      await page.click('.cust-tab[data-tab="theme"]');
      await page.waitForSelector('.cust-panel[data-panel="theme"].active', { timeout: 4000 });
      const sizes = await page.evaluate(() => {
        const d = document.querySelector('.cust-terrain-colors');
        if (d) d.open = true;
        return Array.from(document.querySelectorAll('.cust-terrain-colors input[type="color"]')).map((el) => {
          el.scrollIntoView({ block: 'center' });
          const r = el.getBoundingClientRect();
          return { w: Math.round(r.width), h: Math.round(r.height) };
        });
      });
      assert(sizes.length === 6, 'found ' + sizes.length + ' swatches');
      sizes.forEach((s, i) => {
        assert(s.w >= 48 && s.h >= 48, 'swatch ' + i + ' is ' + s.w + 'x' + s.h);
      });
    });
    await ctx.close();
  }

  // ── 6. Export, and nothing server-side in the client ─────────────────────

  {
    const { ctx, page, rec } = await newCtx({
      overrides: { terrain: { heightRepeater: 45, model: 'dtm' }, theme: { terrainFill: '#9a8a73' } },
    });
    await gotoMap(page, PAIR_LINK);
    await step('the export carries the terrain section and no token', async () => {
      await openCustomizer(page, 'export');
      const json = await page.inputValue('#cv2ExportJson');
      const parsed = JSON.parse(json);
      assert(parsed.terrain && parsed.terrain.heightRepeater === 45, 'the export lost the heights: ' + json);
      assert(parsed.terrain.model === 'dtm', 'the export lost the model');
      assert(parsed.theme.terrainFill === '#9a8a73', 'the export lost the chart colour');
      assert(!/token/i.test(json), 'the export mentions a token');
      assert(parsed.map === undefined, 'the export carries a map section');
      assert(parsed.radio === undefined, 'the export carries a radio section');
    });

    await step('nothing under map.dhm reaches the client at all', async () => {
      const cfg = await page.evaluate(() => {
        const out = { dhm: null, site: null };
        try { out.dhm = JSON.stringify(window.MC_MAP_CFG && window.MC_MAP_CFG.dhm || null); } catch (_) {}
        try { out.site = JSON.stringify(window.SITE_CONFIG || null); } catch (_) {}
        return out;
      });
      assert(!/token/i.test(cfg.dhm || ''), 'the dhm client block carries a token: ' + cfg.dhm);
      assert(!/rateLimit|cacheTTL|maxPathKm|chunkKm|upstreamTimeout/i.test(cfg.dhm || ''),
        'the dhm client block carries server limits: ' + cfg.dhm);
      assert(!/token/i.test(cfg.site || ''), 'SITE_CONFIG carries a token');
      assert(!/antennaHeightsM|kFactor|defaultFreqMHz/i.test(cfg.site || ''),
        'SITE_CONFIG carries server-side radio config');
      const upstream = rec.all.filter(u => /dataforsyningen|datafordeler|kds\.dk/i.test(u));
      assert(upstream.length === 0, 'the browser reached upstream: ' + upstream.join(', '));
      const leaky = rec.all.filter(u => /token=/i.test(u));
      assert(leaky.length === 0, 'a request carried a token: ' + leaky.join(', '));
    });
    await ctx.close();
  }

  // ── 7. axe and measured contrast, light and dark, default colours ────────

  const AXE_RULES = ['color-contrast', 'image-alt', 'label', 'aria-required-attr', 'aria-valid-attr',
    'aria-valid-attr-value', 'region', 'button-name', 'link-name', 'duplicate-id'];
  for (const theme of ['light', 'dark']) {
    const { ctx, page } = await newCtx({ theme });
    await gotoMap(page, PAIR_LINK);
    await step('axe is clean on the customizer in ' + theme, async () => {
      await openCustomizer(page, 'display');
      await page.waitForTimeout(400);
      const { AxeBuilder } = require('@axe-core/playwright');
      const res = await new AxeBuilder({ page }).include('.cust-overlay').withRules(AXE_RULES).analyze();
      const v = res.violations.map(x => x.id + ' x' + x.nodes.length + ' ' + x.nodes.map(n => n.target).join('/'));
      assert(res.violations.length === 0, 'axe violations: ' + v.join(', '));
    });

    await step('axe is clean on the chart with the default colours in ' + theme, async () => {
      await page.click('#customizeToggle');
      await page.waitForTimeout(300);
      await page.waitForSelector('[data-testid="tp-chart"] svg.tp-svg', { timeout: 10000 });
      const { AxeBuilder } = require('@axe-core/playwright');
      const res = await new AxeBuilder({ page }).include('[data-testid="tp-panel"]').withRules(AXE_RULES).analyze();
      const v = res.violations.map(x => x.id + ' x' + x.nodes.length);
      assert(res.violations.length === 0, 'axe violations: ' + v.join(', '));
    });

    await step('the terrain labels and hints clear 4.5:1 in ' + theme, async () => {
      await page.click('#customizeToggle');
      await page.waitForSelector('.cust-overlay:not(.hidden)', { timeout: 5000 });
      await page.click('.cust-tab[data-tab="display"]');
      await page.waitForSelector('.cust-panel[data-panel="display"].active', { timeout: 4000 });
      const worst = await page.evaluate(() => {
        const lum = (c) => {
          const [r, g, b] = c.map(v => {
            const s = v / 255;
            return s <= 0.03928 ? s / 12.92 : Math.pow((s + 0.055) / 1.055, 2.4);
          });
          return 0.2126 * r + 0.7152 * g + 0.0722 * b;
        };
        const parse = (s) => {
          const m = String(s).match(/rgba?\(([^)]+)\)/);
          if (!m) return null;
          const p = m[1].split(',').map(Number);
          return { rgb: p.slice(0, 3), a: p.length > 3 ? p[3] : 1 };
        };
        const bgOf = (el) => {
          let n = el;
          while (n && n !== document.documentElement) {
            const c = parse(getComputedStyle(n).backgroundColor);
            if (c && c.a > 0.95) return c.rgb;
            n = n.parentElement;
          }
          return [255, 255, 255];
        };
        const ratio = (fg, bg) => {
          const a = lum(fg) + 0.05, b = lum(bg) + 0.05;
          return (Math.max(a, b) / Math.min(a, b));
        };
        let out = { ratio: 99, text: '' };
        const rows = Array.from(document.querySelectorAll('.cust-terrain-row label, .cust-terrain-row .cust-hint'));
        rows.forEach((el) => {
          const fg = parse(getComputedStyle(el).color);
          if (!fg) return;
          const r = ratio(fg.rgb, bgOf(el));
          if (r < out.ratio) out = { ratio: Math.round(r * 100) / 100, text: el.textContent.slice(0, 40) };
        });
        return out;
      });
      assert(worst.ratio >= 4.5, `worst terrain label contrast ${worst.ratio}:1 on "${worst.text}"`);
    });
    await ctx.close();
  }

  // ── Screenshots for the PR ────────────────────────────────────────────────

  if (SHOT_DIR) {
    for (const theme of ['light', 'dark']) {
      // The customizer section itself.
      let s = await newCtx({ theme });
      await gotoMap(s.page);
      await openCustomizer(s.page, 'display');
      await s.page.evaluate(() => {
        const el = document.querySelector('[data-cv2-terrain-height="heightRepeater"]');
        if (el) el.scrollIntoView({ block: 'center' });
      });
      await s.page.waitForTimeout(400);
      await s.page.screenshot({ path: path.join(SHOT_DIR, `681-m6-customizer-${theme}.png`) });
      await s.ctx.close();

      // The profile before and after a change to the repeater default.
      s = await newCtx({ theme });
      await gotoMap(s.page, PAIR_LINK);
      await s.page.waitForSelector('[data-testid="tp-chart"] svg.tp-svg', { timeout: 10000 });
      await s.page.waitForTimeout(600);
      await s.page.screenshot({ path: path.join(SHOT_DIR, `681-m6-profile-before-${theme}.png`) });
      await openCustomizer(s.page, 'display');
      await setHeight(s.page, 'heightRepeater', 45);
      await s.page.waitForFunction(() => {
        const el = document.querySelector('[data-testid="tp-h-a"]');
        return el && el.value === '45';
      }, null, { timeout: 8000 });
      await s.page.click('#customizeToggle');
      await s.page.waitForTimeout(600);
      await s.page.screenshot({ path: path.join(SHOT_DIR, `681-m6-profile-after-${theme}.png`) });
      await s.ctx.close();

      // The six colour rows.
      s = await newCtx({ theme });
      await gotoMap(s.page);
      await openCustomizer(s.page, 'theme');
      await s.page.evaluate(() => {
        const d = document.querySelector('.cust-terrain-colors');
        if (d) { d.open = true; d.scrollIntoView({ block: 'center' }); }
      });
      await s.page.waitForTimeout(400);
      await s.page.screenshot({ path: path.join(SHOT_DIR, `681-m6-chart-colours-${theme}.png`) });
      await s.ctx.close();
    }
    console.log('  screenshots → ' + SHOT_DIR);
  }

  await browser.close();
  console.log(`\n#681 M6 E2E: ${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
