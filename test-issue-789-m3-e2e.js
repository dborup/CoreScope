/**
 * E2E (#789 M3): owner antenna heights, polish — against a running server
 * with the CI fixture.
 *
 * The CI server runs without antennaClaims and without the terrain
 * profile, so the claim routes, GET /api/nodes/{pk}/antenna and the
 * profile are answered by Playwright in the shapes of
 * cmd/server/antenna_claims.go and cmd/server/terrain_profile.go; nothing
 * is written anywhere. The real server's own answers are used where the
 * point is what it says (the config route's defaults, the 503 for the
 * node's height without the terrain profile).
 *
 * Asserts:
 *   1. the real config route carries the operator settings with the M2
 *      constants as defaults;
 *   2. node page: the current height and its source (owner, operator,
 *      estimate, default with a pending estimate) above the claim, with the
 *      profile's badge; the owner badge is drawn in --owner-badge, which is
 *      --success in light and dark until the customizer sets ownerBadge;
 *      without an answer (the real server's 503) there is no line;
 *   3. operator settings reach the page: the status polling follows
 *      pollInitialMs/pollBackoff/pollMaxAttempts and stops with "Check
 *      again"; adminDefaultFilter is the admin queue's first view and is
 *      left out of the link;
 *   4. Prefer owner heights: the Display tab switch is on by default; off,
 *      an open Path profile re-asks with the viewer's role height for an
 *      owner end, and the choice survives a reload; on again, the owner
 *      height is back;
 *   5. a11y: axe is clean on the node section (form and claim) and the
 *      admin queue; the section is a labelled region; the focus goes on
 *      from the claim after Get code, to the height field after Start over,
 *      to the active filter after Unlock and a filter change, and to the
 *      key field after Lock;
 *   6. phone 375x560, cold start on the node list, real taps only: a row
 *      opens the node, the section's controls are 44 px targets that a tap
 *      reaches, the current height line fits, no sideways scroll; the
 *      customizer switch is a 48 px row and a tap stores it.
 *
 * Usage:
 *   BASE_URL=http://localhost:13700 node test-issue-789-m3-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13700';
const ADMIN_KEY = 'e2e-antenna-admin-key-0123456789';
const REQ_ID = 'abcdef0123456789';
const CODE = '7KX9QM';
const SECTION = '[data-testid="antenna-claim"]';
const PROFILE_PATH = '/api/terrain/profile';
const DHM_BLOCK = { enabled: true, profile: true, maxPathKm: 50,
  attribution: 'Indeholder data fra Klimadatastyrelsen, Danmarks Højdemodel, CC BY 4.0' };
const PNG_1X1 = Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII=', 'base64');
const PK_A = 'a'.repeat(64);
const PK_B = 'b'.repeat(64);
const OWNER_M = 18.5;

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + (e && e.stack || e)); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }
function pathOf(url) { return new URL(url).pathname; }
const json = (body) => ({ status: 200, contentType: 'application/json', body: JSON.stringify(body) });

// A profile answer in the server's shape: an end with ?ha/?hb is the
// request's height, else the owner's.
function profileFor(q) {
  const end = (h) => (h !== null
    ? { lat: 56.1629, lon: 10.2039, kind: 'node', role: 'repeater', groundM: 40, antennaM: Number(h), antennaSource: 'request', assumed: false }
    : { lat: 56.1629, lon: 10.2039, kind: 'node', role: 'repeater', groundM: 40, antennaM: OWNER_M, antennaSource: 'owner', assumed: false });
  const D = 12000;
  const samples = [];
  for (let i = 0; i < 120; i++) { const d = Math.round(D * i / 119); samples.push({ d, dtm: 30 + 10 * Math.sin(d / 900), dsm: 42 + 10 * Math.sin(d / 900) }); }
  return {
    a: end(q.get('ha')), b: end(q.get('hb')),
    distanceM: D, stepM: 11.7, sampleCount: 1025, model: 'both',
    coverage: { state: 'full', pct: 100, firstGapM: null }, samples,
    physics: { freqMHz: 869.618, freqSource: 'firmware', lambdaM: 0.34474, k: 4 / 3, kSource: 'default', f1MaxM: 32.14 },
    verdict: { state: 'clear', model: 'dtm', criterion: 0.6, worstClearanceM: 12, worstAtM: 6200, worstF1Fraction: 0.8 },
    attribution: DHM_BLOCK.attribution,
  };
}

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #789 M3 antenna claims polish against ${BASE} ===\n`);

  const nodes = await (await fetch(BASE + '/api/nodes?limit=500')).json();
  const list = nodes.nodes || [];
  const repeater = list.find(n => n.role === 'repeater' && n.lat && n.lon && n.name);
  assert(repeater, 'fixture needs a positioned repeater');
  const PK = repeater.public_key;
  const positioned = new Set(list.filter(n => (n.role === 'repeater' || n.role === 'room') && n.lat && n.lon).map(n => n.public_key));

  // newCtx answers the claim API. o.config is the config answer (null: the
  // real server's); o.antenna the node height answer (null: the real
  // server's); o.statuses the status sequence; o.dhm adds the profile.
  async function newCtx(o) {
    o = o || {};
    const ctx = await browser.newContext({
      viewport: o.viewport || { width: 1280, height: 860 },
      hasTouch: !!o.touch, isMobile: !!o.touch,
    });
    const rec = { status: [], admin: [], profile: [] };
    if (o.config !== null) {
      await ctx.route(u => pathOf(u) === '/api/antenna-claims/config', r => r.fulfill(json(o.config || { enabled: true })));
    }
    if (o.antenna !== null) {
      await ctx.route(u => /^\/api\/nodes\/[0-9a-f]{64}\/antenna$/.test(pathOf(u)), r =>
        r.fulfill(json(o.antenna || { antennaM: OWNER_M, antennaSource: 'owner', assumed: false })));
    }
    await ctx.route(u => /^\/api\/nodes\/[0-9a-f]{64}\/antenna-claims$/.test(pathOf(u)), r => r.fulfill({ status: 202, contentType: 'application/json',
      body: JSON.stringify({ requestId: REQ_ID, code: CODE, expiresAt: Date.now() + 86400000, nameBudgetBytes: 23, suggestedName: 'Roof ' + CODE }) }));
    await ctx.route(u => /^\/api\/antenna-claims\/requests\//.test(pathOf(u)), r => {
      rec.status.push(Date.now());
      const seq = o.statuses || ['pending'];
      return r.fulfill(json({ status: seq[Math.min(rec.status.length - 1, seq.length - 1)] }));
    });
    await ctx.route(u => pathOf(u) === '/api/admin/antenna-claims', r => {
      const key = r.request().headers()['x-api-key'];
      rec.admin.push(new URL(r.request().url()).searchParams.get('status'));
      if (key !== ADMIN_KEY) return r.fulfill({ status: 401, contentType: 'application/json', body: '{"error":"unauthorized"}' });
      return r.fulfill(json({ claims: [], enabled: true, autoApprove: false }));
    });
    if (o.dhm) {
      await ctx.route(BASE + '/api/config/client', async (route) => {
        const response = await route.fetch();
        const config = await response.json();
        config.dhm = DHM_BLOCK;
        await route.fulfill({ response, json: config });
      });
      await ctx.route(u => pathOf(u) === PROFILE_PATH, r => {
        const u = new URL(r.request().url());
        rec.profile.push(u.searchParams);
        return r.fulfill(json(profileFor(u.searchParams)));
      });
      await ctx.route(/^https?:\/\/(?!localhost|127\.0\.0\.1)/i, r => r.fulfill({ status: 200, contentType: 'image/png', body: PNG_1X1 }));
      await ctx.route(u => pathOf(u) === '/api/map/dhm/wms', r => r.fulfill({ status: 200, contentType: 'image/png', body: PNG_1X1 }));
    }
    await ctx.addInitScript(({ theme, overrides }) => {
      try {
        localStorage.setItem('meshcore-theme', theme);
        localStorage.setItem('meshcore-map-clustering', 'false');
        ['row-swipe', 'tab-swipe', 'edge-drawer', 'pull-refresh']
          .forEach(k => localStorage.setItem('meshcore-gesture-hints-' + k, 'seen'));
        if (overrides && !sessionStorage.getItem('e2e-seeded')) {
          localStorage.setItem('cs-theme-overrides', overrides);
          sessionStorage.setItem('e2e-seeded', '1');
        }
      } catch (_) {}
      const apply = () => document.documentElement && document.documentElement.setAttribute('data-theme', theme);
      apply();
      document.addEventListener('DOMContentLoaded', apply);
    }, { theme: o.theme || 'light', overrides: o.overrides ? JSON.stringify(o.overrides) : null });
    const page = await ctx.newPage();
    const errors = [];
    page.on('pageerror', e => errors.push(String(e)));
    return { ctx, page, rec, errors };
  }

  async function openNode(page, pk, extra) {
    await page.goto(BASE + '/#/nodes/' + pk + (extra || ''), { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#nodeFullBody .node-full-card', { timeout: 15000 });
    await page.waitForSelector(SECTION + ':visible', { timeout: 15000 });
  }
  async function currentLine(page) {
    await page.waitForSelector('[data-testid="ac-current"]:visible', { timeout: 10000 });
    return page.evaluate(() => {
      const l = document.querySelector('[data-testid="ac-current"]');
      const b = l.querySelector('[data-testid="ac-current-badge"]');
      const cs = getComputedStyle(document.documentElement);
      return { text: l.textContent, source: l.getAttribute('data-source'), badge: b && b.textContent, badgeClass: b && b.className,
        badgeColor: b && getComputedStyle(b).borderTopColor, ownerVar: cs.getPropertyValue('--owner-badge').trim(),
        successVar: cs.getPropertyValue('--success').trim() };
    });
  }
  // Resolve a CSS colour to rgb() the way the badge's computed style has it.
  async function rgbOf(page, colour) {
    return page.evaluate((c) => { const d = document.createElement('div'); d.style.color = c; document.body.appendChild(d); const v = getComputedStyle(d).color; d.remove(); return v; }, colour);
  }

  // ── 1. The real config route ────────────────────────────────────────────
  await step('the config route carries the operator settings, the M2 constants by default', async () => {
    const c = await (await fetch(BASE + '/api/antenna-claims/config')).json();
    const want = { pollInitialMs: 2000, pollBackoff: 1.5, pollMaxMs: 30000, pollMaxAttempts: 60, adminDefaultFilter: 'proven' };
    for (const k of Object.keys(want)) assert(c[k] === want[k], k + ' is ' + c[k]);
    assert(c.enabled === false, 'claims are on in the CI server');
    const r = await fetch(BASE + '/api/nodes/' + PK + '/antenna');
    assert(r.status === 503, 'the node height without the terrain profile answered ' + r.status);
  });

  // ── 2. The current height on the node page ──────────────────────────────
  for (const theme of ['light', 'dark']) {
    await step(theme + ': the owner height shows with the owner badge in --owner-badge (= --success)', async () => {
      const { ctx, page, errors } = await newCtx({ theme });
      await openNode(page, PK);
      const c = await currentLine(page);
      assert(/Current height: 18\.5 m/.test(c.text) && /set by the node owner/.test(c.text), c.text);
      assert(c.source === 'owner' && c.badge === 'owner' && /tp-hbadge-owner/.test(c.badgeClass), JSON.stringify(c));
      assert(c.ownerVar && c.badgeColor === await rgbOf(page, c.successVar), 'badge ' + c.badgeColor + ' vs --success ' + c.successVar);
      assert(errors.length === 0, errors.join('; '));
      await ctx.close();
    });
  }

  await step('every source gets its badge and wording; a pending estimate says so', async () => {
    for (const [ans, badge, re] of [
      [{ antennaM: 24, antennaSource: 'operator-node', assumed: false }, 'operator', /24 m.*set by the operator for this node/],
      [{ antennaM: 22, antennaSource: 'dsm-estimate', assumed: true }, 'estimate', /22 m.*estimated from surface model/],
      [{ antennaM: 10, antennaSource: 'role-default', assumed: true, estimatePending: true }, 'default', /10 m.*role default for this node type\. A path profile may still raise it/],
    ]) {
      const { ctx, page } = await newCtx({ antenna: ans });
      await openNode(page, PK);
      const c = await currentLine(page);
      assert(c.badge === badge && re.test(c.text), ans.antennaSource + ': ' + JSON.stringify(c));
      await ctx.close();
    }
  });

  await step('without an answer (the real 503) there is no line, and the claim form still shows', async () => {
    const { ctx, page } = await newCtx({ antenna: null });
    await openNode(page, PK);
    await page.waitForSelector('[data-testid="ac-height"]');
    await page.waitForTimeout(500);
    assert(await page.locator('[data-testid="ac-current"]').isHidden(), 'a line without an answer');
    await ctx.close();
  });

  await step('a customizer ownerBadge colour reaches the badge', async () => {
    const { ctx, page } = await newCtx({ overrides: { theme: { ownerBadge: '#123456' } } });
    await openNode(page, PK);
    const c = await currentLine(page);
    assert(c.badgeColor === 'rgb(18, 52, 86)', 'badge colour ' + c.badgeColor);
    await ctx.close();
  });

  // ── 3. Operator settings reach the page ─────────────────────────────────
  await step('status polling follows pollInitialMs, pollBackoff and pollMaxAttempts', async () => {
    const { ctx, page, rec } = await newCtx({ config: { enabled: true, pollInitialMs: 600, pollBackoff: 1, pollMaxMs: 600, pollMaxAttempts: 3 } });
    await openNode(page, PK);
    await page.fill('[data-testid="ac-height"]', '18.5');
    const t0 = Date.now();
    await page.click('[data-testid="ac-submit"]');
    await page.waitForSelector('[data-testid="ac-check-again"]:visible', { timeout: 10000 });
    const took = Date.now() - t0;
    assert(rec.status.length === 3, rec.status.length + ' status checks, want 3');
    assert(took >= 1700 && took < 6000, 'gave up after ' + took + ' ms, want about 3 × 600 ms');
    const gaps = rec.status.slice(1).map((t, i) => t - rec.status[i]);
    assert(gaps.every(g => g >= 450 && g < 2000), 'gaps ' + gaps.join(','));
    assert(/Stopped checking automatically/.test(await page.textContent('[data-testid="ac-status"]')), 'no give-up text');
    await ctx.close();
  });

  await step('adminDefaultFilter is the first view and is left out of the link', async () => {
    const { ctx, page, rec } = await newCtx({ config: { enabled: true, adminDefaultFilter: 'pending' } });
    await page.goto(BASE + '/#/tools/antenna-claims', { waitUntil: 'domcontentloaded' });
    await page.fill('[data-testid="ac-admin-key"]', ADMIN_KEY);
    await page.keyboard.press('Enter');
    await page.waitForSelector('[data-testid="ac-admin-list"][aria-busy="false"]');
    assert(rec.admin[rec.admin.length - 1] === 'pending', 'first view asked for ' + rec.admin.join(','));
    assert(await page.getAttribute('.ac-filter[data-filter="pending"]', 'aria-pressed') === 'true', 'pending not pressed');
    await page.click('.ac-filter[data-filter="proven"]');
    await page.waitForFunction(() => /filter=proven/.test(location.hash));
    await page.click('.ac-filter[data-filter="pending"]');
    await page.waitForFunction(() => location.hash === '#/tools/antenna-claims');
    await ctx.close();
  });

  // ── 4. Prefer owner heights ─────────────────────────────────────────────
  await step('Prefer owner heights: on by default; off re-asks with the role height and survives a reload', async () => {
    const { ctx, page, rec, errors } = await newCtx({ dhm: true, config: null, antenna: null, overrides: { terrain: { heightRepeater: 30 } } });
    const link = `${BASE}/#/map?lat=56.16&lon=10.2&zoom=10&profile=${PK_A},${PK_B}`;
    await page.goto(link, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('[data-testid="tp-chart"] svg.tp-svg', { timeout: 20000 });
    await page.waitForFunction(() => window._customizerV2 && window._customizerV2.initDone, null, { timeout: 15000 });
    const badge = () => page.textContent('[data-testid="tp-hsrc-a"]');
    assert(await badge() === 'owner' && await page.inputValue('[data-testid="tp-h-a"]') === '18.5', 'owner not shown by default');
    assert(rec.profile.length === 1 && rec.profile[0].get('ha') === null, 'the owner height was re-asked: ' + rec.profile.map(p => p.toString()).join(' | '));
    await page.click('#customizeToggle');
    await page.click('.cust-tab[data-tab="display"]');
    // The open profile panel overlaps the customizer at this size, so the
    // switch is clicked in place (the phone case below taps it for real).
    const box = page.locator('#cv2-terrain-owner');
    assert(await box.isChecked(), 'the switch is off by default');
    await box.evaluate(e => e.click());
    await page.waitForFunction(() => document.querySelector('[data-testid="tp-hsrc-a"]').textContent === 'default', null, { timeout: 8000 });
    const last = rec.profile[rec.profile.length - 1];
    assert(last.get('ha') === '30' && last.get('hb') === '30', 'asked ' + last.toString());
    assert(JSON.parse(await page.evaluate(() => localStorage.getItem('cs-theme-overrides'))).terrain.preferOwnerHeights === false, 'not stored');
    await page.reload({ waitUntil: 'domcontentloaded' });
    await page.waitForSelector('[data-testid="tp-chart"] svg.tp-svg', { timeout: 20000 });
    await page.waitForFunction(() => document.querySelector('[data-testid="tp-hsrc-a"]').textContent === 'default', null, { timeout: 8000 });
    await page.click('#customizeToggle');
    await page.click('.cust-tab[data-tab="display"]');
    assert(!(await page.locator('#cv2-terrain-owner').isChecked()), 'the switch did not survive the reload');
    await page.locator('#cv2-terrain-owner').evaluate(e => e.click());
    await page.waitForFunction(() => document.querySelector('[data-testid="tp-hsrc-a"]').textContent === 'owner', null, { timeout: 8000 });
    assert(errors.length === 0, errors.join('; '));
    await ctx.close();
  });

  // ── 5. a11y ─────────────────────────────────────────────────────────────
  const { AxeBuilder } = require('@axe-core/playwright');
  async function axe(page, sel) {
    // The page's enter animation fades it in: measure the settled colours.
    await page.waitForFunction(() => document.getAnimations().every(a => a.playState !== 'running'));
    const res = await new AxeBuilder({ page }).include(sel).withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa']).analyze();
    return res.violations.map(x => x.id + ' x' + x.nodes.length + ' ' + x.nodes.map(n => n.target.join(' ')).join(' | '));
  }
  async function focused(page) {
    return page.evaluate(() => { const a = document.activeElement; return a ? (a.id || a.getAttribute('data-filter') || a.className || a.tagName) : null; });
  }

  for (const theme of ['light', 'dark']) {
    await step(theme + ': the section is a labelled region, axe is clean, and the focus goes on after Get code and Start over', async () => {
      const { ctx, page } = await newCtx({ theme });
      await openNode(page, PK);
      await currentLine(page);
      const region = await page.evaluate((s) => { const e = document.querySelector(s); const l = document.getElementById(e.getAttribute('aria-labelledby')); return [e.getAttribute('role'), l && l.textContent]; }, SECTION);
      assert(region[0] === 'region' && region[1] === 'Antenna height (owner)', JSON.stringify(region));
      let v = await axe(page, SECTION);
      assert(v.length === 0, 'form: ' + v.join(', '));
      await page.fill('[data-testid="ac-height"]', '18.5');
      await page.press('[data-testid="ac-height"]', 'Enter');
      await page.waitForSelector('[data-testid="ac-code"]');
      assert(/ac-what/.test(await focused(page)), 'focus after Get code: ' + await focused(page));
      v = await axe(page, SECTION);
      assert(v.length === 0, 'claim: ' + v.join(', '));
      await page.keyboard.press('Tab');
      assert(await focused(page) === 'acName', 'Tab from the claim went to ' + await focused(page));
      await page.click('[data-testid="ac-new"]');
      assert(await focused(page) === 'acHeight', 'focus after Start over: ' + await focused(page));
      await ctx.close();
    });

    await step(theme + ': admin queue: axe is clean, the focus follows Unlock, a filter and Lock', async () => {
      const { ctx, page } = await newCtx({ theme });
      await page.goto(BASE + '/#/tools/antenna-claims', { waitUntil: 'domcontentloaded' });
      await page.waitForSelector('[data-testid="ac-admin-key"]');
      let v = await axe(page, '.ac-admin');
      assert(v.length === 0, 'locked: ' + v.join(', '));
      await page.fill('[data-testid="ac-admin-key"]', ADMIN_KEY);
      await page.keyboard.press('Enter');
      await page.waitForSelector('[data-testid="ac-admin-list"][aria-busy="false"]');
      assert(await focused(page) === 'proven', 'focus after Unlock: ' + await focused(page));
      v = await axe(page, '.ac-admin');
      assert(v.length === 0, 'unlocked: ' + v.join(', '));
      await page.keyboard.press('Tab');
      await page.keyboard.press('Enter');
      assert(await focused(page) === 'pending', 'focus after a filter: ' + await focused(page));
      await page.click('.ac-toolbar button:has-text("Lock")');
      assert(await focused(page) === 'acAdminKey', 'focus after Lock: ' + await focused(page));
      await ctx.close();
    });
  }

  // ── 6. Phone, cold start, real taps ─────────────────────────────────────
  await step('phone 375x560, cold start on the node list, real taps: node, section, customizer switch', async () => {
    const { ctx, page, errors } = await newCtx({ viewport: { width: 375, height: 560 }, touch: true, dhm: true });
    async function tapTarget(sel, minH) {
      const loc = page.locator(sel).first();
      let prev = null;
      for (let i = 0; i < 50; i++) {
        // Centre it: at the viewport's edge the fixed top bar or bottom nav
        // would take the tap (a finger scrolls a control into the open).
        await loc.evaluate(e => e.scrollIntoView({ block: 'center', inline: 'nearest' }));
        const b = await loc.boundingBox();
        assert(b && b.height >= (minH || 44) && b.x >= 0 && b.x + b.width <= 375 + 0.5, sel + ' box ' + JSON.stringify(b));
        const x = b.x + b.width / 2, y = b.y + b.height / 2;
        const still = prev && prev.x === b.x && prev.y === b.y;
        if (still && await loc.evaluate((e, p) => { const t = document.elementFromPoint(p.x, p.y); return !!t && (t === e || e.contains(t)); }, { x, y })) {
          await page.touchscreen.tap(x, y);
          return;
        }
        prev = b;
        await new Promise(r => setTimeout(r, 100));
      }
      throw new Error(sel + ' never settled under a tap');
    }
    await page.goto(BASE + '/#/nodes', { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('tr[data-key]', { timeout: 15000 });
    const keys = await page.$$eval('tr[data-key]', rs => rs.map(r => r.getAttribute('data-key')));
    const pk = keys.find(k => positioned.has(k));
    assert(pk, 'no positioned repeater or room in the list');
    await tapTarget('tr[data-key="' + pk + '"]', 30);
    await page.waitForFunction((p) => location.hash.indexOf('#/nodes/' + p) === 0, pk, { timeout: 10000 });
    await page.waitForSelector(SECTION + ':visible', { timeout: 15000 });
    const c = await currentLine(page);
    assert(c.badge === 'owner', 'line ' + JSON.stringify(c));
    const line = await page.locator('[data-testid="ac-current"]').boundingBox();
    assert(line.x >= 0 && line.x + line.width <= 375 + 0.5, 'the current height line overflows: ' + JSON.stringify(line));
    for (const sel of ['[data-testid="ac-mode-set"]', '[data-testid="ac-mode-clear"]']) {
      const b = await page.locator(sel).locator('xpath=..').boundingBox();
      assert(b && b.height >= 44, sel + ' row ' + JSON.stringify(b));
    }
    await tapTarget('[data-testid="ac-height"]');
    await page.keyboard.type('18.5');
    await tapTarget('[data-testid="ac-submit"]');
    await page.waitForSelector('[data-testid="ac-code"]', { timeout: 10000 });
    await tapTarget('.ac-copy');
    await tapTarget('[data-testid="ac-new"]');
    await page.waitForSelector('[data-testid="ac-height"]');
    const sideways = await page.evaluate(() => document.scrollingElement.scrollWidth - window.innerWidth);
    assert(sideways <= 0, 'the node page scrolls sideways by ' + sideways + ' px');
    // On a phone Customize is in the bottom nav's More sheet (#1467).
    await tapTarget('[data-bottom-nav-tab="more"]', 44);
    await tapTarget('[data-bottom-nav-sheet] [data-mpa-mirror="customizeToggle"]', 44);
    await tapTarget('.cust-tab[data-tab="display"]', 30);
    await tapTarget('label[for="cv2-terrain-owner"]', 48);
    await page.waitForFunction(() => {
      const o = JSON.parse(localStorage.getItem('cs-theme-overrides') || '{}');
      return o.terrain && o.terrain.preferOwnerHeights === false;
    }, null, { timeout: 5000 });
    assert(errors.length === 0, errors.join('; '));
    await ctx.close();
  });

  await browser.close();
  console.log(`\n#789 M3 E2E: ${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
