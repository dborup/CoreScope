/**
 * E2E: #883 M2 — accessibility of the Live kiosk (TV) route.
 *
 * Reuses the #1668 axe harness (test-a11y-axe-1668.js): its rule sets, its
 * allowlist (tests/a11y-allowlist.yaml, matched on the route
 * '/live?kiosk=1') and its zero-net-violation policy, on the kiosk route
 * with everything kiosk draws on screen: the start overlay, the step label,
 * the big-number tiles (stubbed network-status, areas analytics and
 * Moments history), two large Moments cards, the exit button and the map
 * credit. Cells: 1920x1080 (TV) and 1200x900 with the desktop rules,
 * 375x812 with the mobile rules, each in &theme=dark and &theme=light.
 *
 * axe cannot judge text over the map (the tiles are 92 % opaque over map
 * tiles: "incomplete"), so the kiosk's own text is also checked by hand:
 * its colour against its tile colour blended over pure black and pure
 * white, the worst a map can do, must reach 4.5:1 (3:1 for the 24px+
 * numbers, WCAG 1.4.3 large text).
 *
 * Usage: BASE_URL=http://localhost:13581 node test-883-kiosk-a11y-e2e.js
 */
'use strict';
const { chromium } = require('playwright');
const { AxeBuilder } = require('@axe-core/playwright');
const harness = require('./test-a11y-axe-1668.js');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
if (!['localhost', '127.0.0.1', '[::1]'].includes(new URL(BASE).hostname)) throw new Error('local server only');
const ROUTE = '/live?kiosk=1';

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + (e && e.stack || e)); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

const PK = (c) => c.repeat(64);
const ago = (ms) => new Date(Date.now() - ms).toISOString();
const AREAS = [
  { key: 'AAA', label: 'Kiosk Area A', latMin: 37.2, latMax: 37.45, lonMin: -122.05, lonMax: -121.75 },
  { key: 'BBB', label: 'Kiosk Area B', polygon: [[37.70, -122.52], [37.83, -122.52], [37.83, -122.35], [37.70, -122.35]] },
];
const HISTORY = [
  { id: 'a11y-busy', kind: 'busiest_repeater', at: ago(300000), relays: 212, windowMinutes: 10,
    node: { publicKey: PK('f'), name: 'Rpt-3', role: 'repeater', lat: 37.4, lon: -122 } },
  { id: 'a11y-link', kind: 'record_link', at: ago(3600000), link: { a: { publicKey: PK('c'), name: 'Ridge-Rpt', lat: 37.3, lon: -121.9 },
    b: { publicKey: PK('d'), name: 'Hill-Rpt', lat: 37.34, lon: -121.64 }, distanceKm: 38.24 } },
];
const CELLS = [
  { name: 'tv', w: 1920, h: 1080, rules: harness.RULES_DESKTOP },
  { name: 'desktop', w: 1200, h: 900, rules: harness.RULES_DESKTOP },
  { name: 'mobile', w: 375, h: 812, rules: harness.RULES_MOBILE },
];
// The kiosk's own text: [selector, minimum ratio].
const KIOSK_TEXT = [
  ['.live-kiosk-tile-value', 3], ['.live-kiosk-tile-area', 4.5], ['.live-kiosk-tile-label', 4.5],
  ['.live-kiosk-record-label', 4.5], ['.live-kiosk-record-value', 4.5], ['.live-kiosk-record-detail', 4.5],
  ['.live-kiosk-step-label', 4.5], ['.live-kiosk-step-count', 4.5],
];

// Contrast of text on its tile, the tile's alpha blended over black and over
// white (whatever the map shows lies between). Colours as rgb()/rgba() or
// color(srgb r g b / a) from getComputedStyle.
function parseColour(s) {
  let m = s.match(/^rgba?\(([^)]+)\)$/);
  if (m) { const p = m[1].split(/[ ,/]+/).filter(Boolean).map(Number); return { r: p[0] / 255, g: p[1] / 255, b: p[2] / 255, a: p.length > 3 ? p[3] : 1 }; }
  m = s.match(/^color\(srgb ([^)]+)\)$/);
  if (m) { const p = m[1].split(/[ /]+/).filter(Boolean).map(Number); return { r: p[0], g: p[1], b: p[2], a: p.length > 3 ? p[3] : 1 }; }
  throw new Error('colour: ' + s);
}
const lin = (v) => (v <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4));
const lum = (c) => 0.2126 * lin(c.r) + 0.7152 * lin(c.g) + 0.0722 * lin(c.b);
const over = (c, bg) => ({ r: c.r * c.a + bg * (1 - c.a), g: c.g * c.a + bg * (1 - c.a), b: c.b * c.a + bg * (1 - c.a) });
const ratio = (a, b) => { const x = lum(a), y = lum(b); return (Math.max(x, y) + 0.05) / (Math.min(x, y) + 0.05); };
function worstContrast(fg, bg) {
  const f = parseColour(fg), b = parseColour(bg);
  return Math.min(...[0, 1].map((base) => ratio(over(f, base), over(b, base))));
}

(async () => {
  const browser = await chromium.launch({ headless: true, executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'] });
  const allowlist = harness.loadAllowlist();
  try {
    console.log('\n=== #883 M2 kiosk a11y (axe + kiosk text contrast) ===');
    for (const cell of CELLS) {
      for (const theme of harness.THEMES) {
        await step(cell.name + ' ' + cell.w + 'x' + cell.h + ' ' + theme + ': axe 0 net violations; kiosk text contrast', async () => {
          const ctx = await browser.newContext({ viewport: { width: cell.w, height: cell.h } });
          await ctx.addInitScript((t) => { try { localStorage.setItem('meshcore-theme', t); } catch (_) {} }, theme === 'dark' ? 'light' : 'dark');
          await ctx.route(/\/api\/config\/areas\/polygons(\?|$)/, (r) => r.fulfill({ json: AREAS }));
          await ctx.route(/\/api\/analytics\/areas(\?|$)/, (r) => r.fulfill({ json: { density: [{ areaKey: 'AAA', label: 'Kiosk Area A', total: 60, active: 41 }] } }));
          await ctx.route(/\/api\/nodes\/network-status(\?|$)/, (r) => r.fulfill({ json: { total: 400, active: 318, degraded: 2, silent: 80, roleCounts: {} } }));
          await ctx.route(/\/api\/config\/client(\?|$)/, async (route) => {
            const res = await route.fetch(); const body = await res.json();
            body.moments = { enabled: true, historySize: 50 };
            await route.fulfill({ response: res, json: body });
          });
          await ctx.route(/\/api\/moments(\?|$)/, (r) => r.fulfill({ json: { moments: HISTORY } }));
          const sockets = [];
          await ctx.routeWebSocket(/.*/, (ws) => { sockets.push(ws); });
          const page = await ctx.newPage();
          const errors = [];
          page.on('pageerror', (e) => errors.push(e.message));
          // The kiosk theme wins over the viewer's (seeded as the opposite).
          await page.goto(BASE + '/#' + ROUTE + '&tour=AAA,BBB&theme=' + theme, { waitUntil: 'domcontentloaded' });
          await page.waitForFunction(() => window._liveKiosk && window._liveKiosk() && window._liveKiosk().stats().steps > 0 &&
            window._liveMoments && window._liveMoments(), null, { timeout: 20000 });
          await page.waitForFunction(() => { const v = document.querySelectorAll('.live-kiosk-record-value');
            return v.length === 2 && v[1].textContent === 'Rpt-3' && document.querySelectorAll('.live-kiosk-tile-value')[1].textContent === '318'; },
          null, { timeout: 10000 });
          assert(await page.evaluate(() => document.documentElement.getAttribute('data-theme')) === theme, 'kiosk theme ' + theme);
          const now = await page.evaluate(() => Date.now());
          for (const i of [1, 2]) {
            sockets[sockets.length - 1].send(JSON.stringify({ type: 'moment', data: { id: 'a11y-n' + i, kind: 'new_node', at: new Date(now).toISOString(),
              node: { publicKey: PK('e'), name: 'Kiosk node ' + i, role: 'repeater', lat: 37.3, lon: -121.9 } } }));
          }
          await page.waitForSelector('.live-moment-card');
          const scan = async (include) => {
            let b = new AxeBuilder({ page }).withRules(cell.rules);
            if (include) b = b.include(include);
            const r = await b.analyze();
            const net = [];
            for (const v of r.violations) for (const node of v.nodes) {
              const m = harness.matchViolation(ROUTE, v.id, node, allowlist);
              if (!(m.allowed && !m.overflow)) net.push(v.id + ' ' + JSON.stringify(node.target) + ' ' + (node.failureSummary || '').split('\n').slice(0, 2).join(' '));
            }
            return { net, r };
          };
          // The start overlay first (its button), then the page without it.
          const ov = await page.$('.live-kiosk-overlay');
          if (ov) {
            const o = await scan('.live-kiosk-overlay');
            assert(!o.net.length, 'overlay: ' + o.net.join(' | '));
            await page.click('.live-kiosk-start');
            await page.waitForFunction(() => !document.querySelector('.live-kiosk-overlay'));
          }
          await page.mouse.move(cell.w / 2, cell.h / 2); // the exit button shows
          await page.waitForTimeout(300);
          const { net, r } = await scan();
          assert(!net.length, net.length + ' net violation(s): ' + net.join(' | '));
          const kioskNodes = (list) => list.reduce((n, v) => n + v.nodes.filter((x) => /live-kiosk|live-moment/.test(JSON.stringify(x.target))).length, 0);
          const seen = kioskNodes(r.passes) + kioskNodes(r.incomplete);
          assert(seen > 0, 'axe saw no kiosk node (false-clean)');
          // Kiosk text against its tile, worst case over the map.
          const pairs = await page.evaluate((sels) => sels.map(([sel]) => {
            const e = document.querySelector(sel);
            if (!e || !e.textContent || !e.getClientRects().length) return { sel, skip: true };
            let bgEl = e; while (bgEl && getComputedStyle(bgEl).backgroundColor === 'rgba(0, 0, 0, 0)') bgEl = bgEl.parentElement;
            return { sel, fg: getComputedStyle(e).color, bg: bgEl ? getComputedStyle(bgEl).backgroundColor : 'rgb(255, 255, 255)' };
          }), KIOSK_TEXT);
          const low = [];
          let checked = 0;
          pairs.forEach((p, i) => {
            if (p.skip) return;
            checked++;
            const c = worstContrast(p.fg, p.bg);
            if (c < KIOSK_TEXT[i][1]) low.push(p.sel + ' ' + c.toFixed(2) + ' < ' + KIOSK_TEXT[i][1] + ' (' + p.fg + ' on ' + p.bg + ')');
          });
          assert(checked >= 6, 'kiosk text checked: ' + checked);
          assert(!low.length, 'contrast: ' + low.join(' | '));
          console.log('    axe kiosk nodes passed/incomplete: ' + kioskNodes(r.passes) + '/' + kioskNodes(r.incomplete) + ', own contrast checks: ' + checked);
          assert(!errors.length, errors.join('; '));
          await ctx.close();
        });
      }
    }
  } finally {
    await browser.close();
  }
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
