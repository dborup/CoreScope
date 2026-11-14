/**
 * E2E (#681 M5): "Terrain profile to…" on node detail, into the Map page
 * Path profile tool with A filled in and the picker armed for B.
 *
 * Same harness as test-issue-681-profile-tool-e2e.js: the CI server has no
 * map.dhm config and no token, so /api/config/client is given the `dhm`
 * block the Go server sends when the profile is on, and
 * /api/terrain/profile is answered with a synthetic profile in the shape
 * of cmd/server/terrain_profile.go. Nothing reaches a real network.
 *
 * Asserts:
 *   1. the full node page and the side pane both offer the action for a
 *      node with a fix, as a ≥48 px target linking #/map?profile=<key>;
 *   2. following it opens the Map tool with A named and the picker armed
 *      for B, with no profile request until B is picked;
 *   3. picking B measures once, from that node, and shows the height
 *      source badge, the copy-link button (≥48 px, copies the pair) and the
 *      CC BY 4.0 licence link;
 *   4. the same round trip at 360 px portrait on a touch device;
 *   5. no action for a node without a fix;
 *   6. no action, no panel and no request with the profile disabled, even
 *      when the half link is opened directly;
 *   7. #711: the licence link is hit-testable over its whole 48 px box
 *      (elementFromPoint, not just the bounding box), copy and close sit
 *      side by side, the half link stays in the address bar and survives a
 *      reload, A outside the Map page's node list is still named and
 *      panned to, and the side pane gives the action a row of its own.
 *
 * Usage:
 *   BASE_URL=http://localhost:13700 node test-issue-681-m5-node-entry-e2e.js
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
const ACTION = '[data-testid="node-terrain-profile"]';
const PANEL = '[data-testid="tp-panel"]';

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

function profileAnswer(q) {
  const D = 9000;
  const end = (p, h) => {
    const node = /^[0-9a-f]{64}$/.test(p || '');
    const asked = q.get(h);
    return {
      lat: 37.6, lon: -122.1, kind: node ? 'node' : 'point', role: node ? 'repeater' : undefined,
      groundM: 40,
      antennaM: asked !== null ? Number(asked) : (node ? 10 : 2),
      antennaSource: asked !== null ? 'request' : (node ? 'role-default' : 'point-default'),
      assumed: asked === null,
    };
  };
  const samples = [];
  for (let i = 0; i < 90; i++) {
    const d = Math.round(D * i / 89);
    samples.push({ d, dtm: 30 + 15 * Math.sin(d / 800), dsm: 40 + 15 * Math.sin(d / 800) });
  }
  return {
    a: end(q.get('a'), 'ha'), b: end(q.get('b'), 'hb'),
    distanceM: D, stepM: 10, sampleCount: 901, model: q.get('model') === 'dtm' ? 'dtm' : 'both',
    coverage: { state: 'full', pct: 100, firstGapM: null }, samples,
    physics: { freqMHz: 869.618, freqSource: 'firmware', lambdaM: 0.34474, k: 1.3333333333333333, kSource: 'default', f1MaxM: 27.8 },
    verdict: { state: 'clear', model: 'dtm', criterion: 0.6, worstClearanceM: 30.1, worstAtM: 4000, worstF1Fraction: 1.3 },
    attribution: ATTRIBUTION,
  };
}

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #681 M5 node detail → Path profile against ${BASE} ===\n`);

  async function newCtx(o) {
    o = o || {};
    const ctx = await browser.newContext({
      viewport: o.viewport || { width: 1280, height: 860 },
      hasTouch: !!o.touch, isMobile: !!o.touch,
    });
    const rec = { profile: [], errors: [] };
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
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(profileAnswer(u.searchParams)) });
    });
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
      // A recording clipboard: the suite must not depend on the browser
      // granting clipboard permission, and must see what was copied.
      window.__copied = [];
      try {
        Object.defineProperty(navigator, 'clipboard', { configurable: true, value: {
          writeText: (t) => { window.__copied.push(String(t)); return Promise.resolve(); },
        } });
      } catch (_) {}
    });
    const page = await ctx.newPage();
    page.on('pageerror', e => rec.errors.push(String(e)));
    return { ctx, page, rec };
  }

  // Pick fixture nodes from the API, not by hard-coded key: one with a real
  // fix, one without (null or the 0,0 sentinel).
  const nodes = await (async () => {
    const { ctx, page } = await newCtx({ dhm: 'off' });
    await page.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
    const list = await page.evaluate(async () => {
      const r = await fetch('/api/nodes?limit=2000');
      const d = await r.json();
      return (d.nodes || d).map(n => ({ pk: n.public_key, name: n.name, lat: n.lat, lon: n.lon }));
    });
    await ctx.close();
    const fixed = list.filter(n => n.lat != null && n.lon != null && !(n.lat === 0 && n.lon === 0) && /^[0-9a-f]{64}$/i.test(n.pk) && n.name);
    const nofix = list.filter(n => (n.lat == null || n.lon == null || (n.lat === 0 && n.lon === 0)) && /^[0-9a-f]{64}$/i.test(n.pk));
    return { fixed: fixed[0], nofix: nofix[0] };
  })();
  assert(nodes.fixed && nodes.nofix, 'the fixture needs a node with a fix and one without');
  const PK = nodes.fixed.pk.toLowerCase();

  async function gotoNode(page, pk) {
    await page.goto(`${BASE}/#/nodes/${pk}`, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('.node-full-card', { timeout: 30000 });
    await page.waitForFunction(() => window.MeshConfigReady !== undefined, null, { timeout: 15000 });
    await page.evaluate(() => window.MeshConfigReady);
    await page.waitForTimeout(300);
  }

  async function armedState(page) {
    return page.evaluate(() => {
      const q = (s) => document.querySelector(s);
      const panel = q('[data-testid="tp-panel"]');
      const btn = q('[data-testid="map-profile-toggle"]');
      const hint = q('[data-testid="map-profile-hint"]');
      return {
        open: !!(panel && panel.classList.contains('is-open')),
        nameA: (q('[data-testid="tp-name-a"]') || {}).textContent || '',
        nameB: (q('[data-testid="tp-name-b"]') || {}).textContent || '',
        pressed: btn ? btn.getAttribute('aria-pressed') : null,
        picking: !!q('#leaflet-map.tp-picking, .leaflet-container.tp-picking'),
        hint: hint && !hint.hidden ? hint.textContent : '',
      };
    });
  }

  /* Every point of the element's box must hit the element itself: a 48 px
   * box whose padding band is covered by a neighbour is not a 48 px target. */
  async function hitArea(page, sel) {
    return page.$eval(sel, (e) => {
      e.scrollIntoView({ block: 'center' });
      const r = e.getBoundingClientRect();
      const miss = [];
      let probes = 0;
      for (let y = r.top + 1; y <= r.bottom - 1; y += 2) {
        for (let x = r.left + 1; x <= r.right - 1; x += 4) {
          probes++;
          const h = document.elementFromPoint(x, y);
          if (!h || !(h === e || e.contains(h))) {
            miss.push([Math.round(x - r.left), Math.round(y - r.top), h ? (h.getAttribute('class') || h.tagName) : null]);
          }
        }
      }
      return { w: r.width, h: r.height, probes, missCount: miss.length, miss: miss.slice(0, 4) };
    });
  }

  async function box(page, sel) {
    return page.$eval(sel, (e) => { const r = e.getBoundingClientRect(); return { w: r.width, h: r.height, x: r.x, y: r.y }; });
  }

  // Boxes of several elements, read only once they hold still: two
  // consecutive animation frames give identical rects. The side pane slides
  // in, so a box read straight after it appears can be mid-transition.
  async function stableBoxes(page, sels, timeoutMs) {
    return page.evaluate(({ sels, timeoutMs }) => new Promise((resolve, reject) => {
      const read = () => sels.map((s) => {
        const e = document.querySelector(s);
        if (!e) return null;
        const r = e.getBoundingClientRect();
        return { w: r.width, h: r.height, x: r.x, y: r.y };
      });
      const same = (a, b) => JSON.stringify(a) === JSON.stringify(b);
      const t0 = performance.now();
      let prev = read();
      const tick = () => {
        const cur = read();
        if (!cur.includes(null) && same(prev, cur)) return resolve(cur);
        if (performance.now() - t0 > timeoutMs) return reject(new Error('boxes never settled: ' + JSON.stringify(cur)));
        prev = cur;
        requestAnimationFrame(tick);
      };
      requestAnimationFrame(tick);
    }), { sels, timeoutMs });
  }

  // Click/tap an empty spot in the visible upper map, away from markers.
  async function pickPoint(page, touch) {
    const pt = await page.evaluate(() => {
      const m = document.getElementById('leaflet-map').getBoundingClientRect();
      const icons = Array.from(document.querySelectorAll('.leaflet-marker-icon')).map(e => e.getBoundingClientRect());
      for (let fy = 0.15; fy < 0.45; fy += 0.05) {
        for (let fx = 0.2; fx < 0.8; fx += 0.05) {
          const x = m.x + m.width * fx, y = m.y + m.height * fy;
          const hit = document.elementFromPoint(x, y);
          if (!hit || !hit.closest('.leaflet-container') || hit.closest('.leaflet-marker-icon, .leaflet-control, .map-profile-toggle, .tp-panel')) continue;
          if (icons.every(r => x < r.left - 20 || x > r.right + 20 || y < r.top - 20 || y > r.bottom + 20)) return { x: Math.round(x), y: Math.round(y) };
        }
      }
      return null;
    });
    assert(pt, 'no empty map spot to pick B on');
    if (touch) await page.touchscreen.tap(pt.x, pt.y); else await page.mouse.click(pt.x, pt.y);
    await page.waitForTimeout(600);
  }

  async function roundTrip(label, o) {
    const { ctx, page, rec } = await newCtx(o);
    await gotoNode(page, PK);
    await step(`${label}: the full node page offers the action as a ≥48 px link`, async () => {
      const sel = '.node-full-card ' + ACTION;
      await page.waitForSelector(sel, { timeout: 5000 });
      const href = await page.$eval(sel, e => e.getAttribute('href'));
      assert(href === '#/map?profile=' + PK, 'href = ' + href);
      const b = await box(page, sel);
      assert(b.h >= 48 && b.w >= 48, `target ${b.w}x${b.h}`);
      assert(b.x >= 0 && b.x + b.w <= (o.viewport ? o.viewport.width : 1280), 'action off screen: ' + JSON.stringify(b));
    });
    if (SHOT_DIR) await page.screenshot({ path: path.join(SHOT_DIR, `681-m5-${o.shot}-node.png`) });

    if (!(await page.$('.node-full-card ' + ACTION))) {
      failed++;
      console.error(`  ✗ ${label}: round trip not run, node detail offers no action`);
      await ctx.close();
      return;
    }
    if (o.touch) await page.tap('.node-full-card ' + ACTION); else await page.click('.node-full-card ' + ACTION);
    await page.waitForSelector('#leaflet-map[data-loaded="true"]', { timeout: 30000 });
    await page.waitForSelector(PANEL + '.is-open', { timeout: 15000 });
    await page.waitForTimeout(500);
    await step(`${label}: the Map tool opens with A filled in and the picker armed for B`, async () => {
      const s = await armedState(page);
      assert(s.open, 'panel not open');
      assert(s.nameA === nodes.fixed.name, `A = "${s.nameA}", want "${nodes.fixed.name}"`);
      assert(s.nameB === 'not picked', 'B = ' + s.nameB);
      assert(s.pressed === 'true', 'toggle not pressed: ' + s.pressed);
      assert(s.picking, 'the map is not in picking mode');
      assert(/other end/.test(s.hint), 'hint = ' + s.hint);
      assert(rec.profile.length === 0, 'measured before B was picked');
    });
    await step(`${label}: the half link stays in the address bar while B is picked`, async () => {
      const hash = await page.evaluate(() => location.hash);
      assert(hash === '#/map?profile=' + PK, 'hash = ' + hash);
    });

    await pickPoint(page, o.touch);
    await page.waitForSelector('[data-testid="tp-chart"] svg.tp-svg', { timeout: 10000 });
    await step(`${label}: picking B measures once, from that node`, async () => {
      assert(rec.profile.length === 1, 'profile calls = ' + rec.profile.length);
      const q = rec.profile[0].searchParams;
      assert(q.get('a') === PK, 'a = ' + q.get('a'));
      assert(/^-?\d+\.\d{5},-?\d+\.\d{5}$/.test(q.get('b') || ''), 'b = ' + q.get('b'));
      const s = await armedState(page);
      assert(!s.picking && s.pressed === 'false', 'picker still armed after B');
    });
    await step(`${label}: the antenna-height source badges read "default"`, async () => {
      const src = await page.evaluate(() => ['a', 'b'].map(s => {
        const b = document.querySelector('[data-testid="tp-hsrc-' + s + '"]');
        return b && !b.hidden ? b.getAttribute('data-source') + ':' + b.textContent : null;
      }));
      assert(src[0] === 'default:default' && src[1] === 'default:default', JSON.stringify(src));
    });
    await step(`${label}: copy link is a ≥48 px button that copies the pair`, async () => {
      const b = await box(page, '[data-testid="tp-copy-link"]');
      assert(b.h >= 48 && b.w >= 48, `copy target ${b.w}x${b.h}`);
      // Copy and close share the title row; at 360 px the close button
      // used to wrap onto a line of its own.
      const c = await box(page, '[data-testid="tp-close"]');
      const t = await box(page, '.tp-title');
      assert(Math.abs(c.y - b.y) < 2, `copy y=${b.y}, close y=${c.y}`);
      // #711 N6: they share one auto margin, so they sit side by side at the
      // row's end instead of splitting the free space between them.
      const between = c.x - (b.x + b.w);
      assert(between >= 0 && between <= 8.5, `copy→close gap ${between}px`);
      assert(c.y <= t.y + t.h && t.y <= c.y + c.h, `close (y=${c.y}) is not on the title row (y=${t.y})`);
      if (o.touch) await page.tap('[data-testid="tp-copy-link"]'); else await page.click('[data-testid="tp-copy-link"]');
      await page.waitForFunction(() => (document.querySelector('[data-testid="tp-copy-status"]') || {}).textContent === 'Link copied', null, { timeout: 3000 });
      const copied = await page.evaluate(() => window.__copied.slice());
      assert(copied.length === 1, 'copied = ' + JSON.stringify(copied));
      const u = new URL(copied[0]);
      assert(u.origin === new URL(BASE).origin, 'origin = ' + u.origin);
      const qs = new URLSearchParams(u.hash.split('?')[1] || '');
      assert(u.hash.indexOf('#/map?') === 0, 'hash = ' + u.hash);
      assert((qs.get('profile') || '').indexOf(PK + ',') === 0, 'profile = ' + qs.get('profile'));
    });
    await step(`${label}: the credit links CC BY 4.0 to the licence`, async () => {
      const l = await page.$eval('[data-testid="tp-credit-link"]', e => ({ href: e.getAttribute('href'), text: e.textContent, rel: e.getAttribute('rel'), target: e.getAttribute('target'), line: e.parentNode.textContent }));
      assert(l.href === 'https://creativecommons.org/licenses/by/4.0/', 'href = ' + l.href);
      assert(l.text === 'CC BY 4.0' && l.target === '_blank' && /noopener/.test(l.rel), JSON.stringify(l));
      assert(l.line === ATTRIBUTION, 'credit = ' + l.line);
      const b = await box(page, '[data-testid="tp-credit-link"]');
      assert(b.h >= 48 && b.w >= 48, `licence link target ${b.w}x${b.h}`);
      // #711 N1: and all of that box is the link's, not a neighbour's.
      const hit = await hitArea(page, '[data-testid="tp-credit-link"]');
      assert(hit.probes > 50 && hit.missCount === 0,
        `licence link ${hit.w}x${hit.h}: ${hit.missCount}/${hit.probes} probes hit something else, e.g. ${JSON.stringify(hit.miss)}`);
    });
    await step(`${label}: no page errors`, async () => {
      assert(rec.errors.length === 0, rec.errors.join(' | '));
    });
    if (SHOT_DIR) await page.screenshot({ path: path.join(SHOT_DIR, `681-m5-${o.shot}-profile.png`) });
    await ctx.close();
  }

  // ── 1–3. Desktop round trip ───────────────────────────────────────────────
  await roundTrip('desktop', { viewport: { width: 1280, height: 900 }, shot: 'desktop' });

  // ── 4. 360 px portrait, touch ─────────────────────────────────────────────
  await roundTrip('360px touch', { viewport: { width: 360, height: 740 }, touch: true, shot: '360' });

  // ── 1b. The side pane ─────────────────────────────────────────────────────
  {
    const { ctx, page } = await newCtx({});
    await page.goto(`${BASE}/#/nodes`, { waitUntil: 'domcontentloaded' });
    const row = `tr[data-key="${nodes.fixed.pk}"]`;
    await page.waitForSelector(row, { timeout: 30000 });
    await page.click(row + ' td:nth-child(2)');
    await step('side pane: the action has a row of its own under the detail header', async () => {
      const sel = '#nodesRight .node-detail-actions ' + ACTION;
      await page.waitForSelector(sel, { timeout: 10000 });
      const href = await page.$eval(sel, e => e.getAttribute('href'));
      assert(href === '#/map?profile=' + PK, 'href = ' + href);
      const [b, role] = await stableBoxes(page, [sel, '#nodesRight .node-detail-role'], 5000);
      assert(b.h >= 48 && b.w >= 48, `target ${b.w}x${b.h}`);
      // #711 N7: the 48 px action no longer wraps under the 20 px
      // Details/Analytics/Reach buttons and doubles the header row.
      assert(await page.$('#nodesRight .node-detail-role ' + ACTION) === null, 'the action is still inside the button row');
      assert(role.h < 48, `button row grew to ${role.h}px`);
      assert(b.y >= role.y + role.h, `action (y=${b.y}) overlaps the button row (ends ${role.y + role.h})`);
      assert(Math.abs(b.x - role.x) < 1, `action x=${b.x}, row x=${role.x}`);
    });
    await ctx.close();
  }

  // ── 2b. The half link opened directly ────────────────────────────────────
  {
    const { ctx, page, rec } = await newCtx({});
    await page.goto(`${BASE}/#/map?profile=${PK}`, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#leaflet-map[data-loaded="true"]', { timeout: 30000 });
    await page.waitForTimeout(1500);
    await step('#/map?profile=<one key> opens the tool with A set and B armed', async () => {
      const s = await armedState(page);
      assert(s.open && s.nameA === nodes.fixed.name && s.picking && s.pressed === 'true', JSON.stringify(s));
      assert(rec.profile.length === 0, 'measured half a path');
    });
    await ctx.close();
  }

  // ── 2c. #711 N2: a reload keeps A ────────────────────────────────────────
  {
    const { ctx, page, rec } = await newCtx({});
    await gotoNode(page, PK);
    await page.click('.node-full-card ' + ACTION);
    await page.waitForSelector(PANEL + '.is-open', { timeout: 30000 });
    await page.waitForTimeout(500);
    await page.reload({ waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#leaflet-map[data-loaded="true"]', { timeout: 30000 });
    await page.waitForSelector(PANEL + '.is-open', { timeout: 15000 }).catch(() => {});
    await page.waitForTimeout(800);
    await step('a reload while B is being picked keeps A and the armed picker', async () => {
      const s = await armedState(page);
      assert(s.open && s.nameA === nodes.fixed.name && s.picking && s.pressed === 'true', JSON.stringify(s));
      const hash = await page.evaluate(() => location.hash);
      assert(hash === '#/map?profile=' + PK, 'hash = ' + hash);
      assert(rec.profile.length === 0, 'measured half a path');
    });
    await ctx.close();
  }

  // ── 2d. #711 N4: A outside the Map page's own node list ──────────────────
  {
    const { ctx, page, rec } = await newCtx({});
    // The Map page's list request (it carries lastHeard) comes back without
    // A, as it does when a persisted area filter or the last-heard window
    // excludes the node. Node detail's own /api/nodes/<key> is untouched.
    await ctx.route(url => { const u = new URL(url); return u.pathname === '/api/nodes' && u.searchParams.has('lastHeard'); }, async (route) => {
      const response = await route.fetch();
      const body = await response.json();
      const strip = (list) => (list || []).filter(n => String(n.public_key || '').toLowerCase() !== PK);
      if (Array.isArray(body)) await route.fulfill({ response, json: strip(body) });
      else await route.fulfill({ response, json: Object.assign({}, body, { nodes: strip(body.nodes) }) });
    });
    await gotoNode(page, PK);
    await page.evaluate(() => {
      window.__panned = [];
      const orig = L.Map.prototype.panTo;
      L.Map.prototype.panTo = function (ll) { const p = L.latLng(ll); window.__panned.push([p.lat, p.lng]); return orig.apply(this, arguments); };
    });
    await page.click('.node-full-card ' + ACTION);
    await page.waitForSelector(PANEL + '.is-open', { timeout: 30000 });
    await page.waitForTimeout(1200);
    await step('A outside the Map list is named and the map pans to it', async () => {
      const s = await armedState(page);
      assert(s.open && s.picking, JSON.stringify(s));
      assert(s.nameA === nodes.fixed.name, `A = "${s.nameA}", want "${nodes.fixed.name}"`);
      const panned = await page.evaluate(() => window.__panned.slice());
      assert(panned.some(p => Math.abs(p[0] - nodes.fixed.lat) < 1e-6 && Math.abs(p[1] - nodes.fixed.lon) < 1e-6),
        'panTo calls ' + JSON.stringify(panned) + ', want ' + [nodes.fixed.lat, nodes.fixed.lon]);
      assert(rec.profile.length === 0, 'measured half a path');
      assert(rec.errors.length === 0, rec.errors.join(' | '));
    });
    await ctx.close();
  }

  // ── 5. A node without a fix ───────────────────────────────────────────────
  {
    const { ctx, page } = await newCtx({});
    await gotoNode(page, nodes.nofix.pk);
    await page.waitForTimeout(500);
    await step('no action for a node without a position', async () => {
      assert(await page.$(ACTION) === null, 'action offered for a node with no fix');
    });
    await ctx.close();
  }

  // ── 6. Profile disabled ───────────────────────────────────────────────────
  for (const dhm of ['off', 'no-profile']) {
    const { ctx, page, rec } = await newCtx({ dhm });
    await gotoNode(page, PK);
    await page.waitForTimeout(500);
    await step(`profile disabled (${dhm}): no action on node detail`, async () => {
      assert(await page.$('.node-full-card') !== null, 'node page did not render');
      assert(await page.$(ACTION) === null, 'action offered with the profile disabled');
    });
    await page.goto(`${BASE}/#/map?profile=${PK}`, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#leaflet-map[data-loaded="true"]', { timeout: 30000 });
    await page.waitForTimeout(1200);
    await step(`profile disabled (${dhm}): the half link opens no panel and makes no request`, async () => {
      assert(await page.$(PANEL) === null, 'panel built with the profile disabled');
      assert(rec.profile.length === 0, 'profile requests: ' + rec.profile.length);
    });
    await ctx.close();
  }

  await browser.close();
  console.log(`\n#681 M5 E2E: ${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
