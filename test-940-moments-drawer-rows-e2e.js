/**
 * E2E: Live Moments drawer rows keep their height when the list is longer
 * than the drawer (#940).
 *
 * `.live-moment-row` is a flex item with `overflow: hidden`, so its
 * automatic minimum height is 0 and it shrank inside the scrolling flex
 * column `.live-moments-list` instead of making the list scroll. The
 * moments block of /api/config/client is added by interception, GET
 * /api/moments is stubbed with 34 moments (the drawer lists the newest 20)
 * and the app socket is replaced with page.routeWebSocket, as in
 * test-881-live-moments-e2e.js.
 *
 * At 1920x1080 and 375x560, with the drawer open:
 *   - every row is at least as tall as its content (title and detail are
 *     not clipped and do not overlap the next row);
 *   - the list is taller inside than outside (scrollHeight > clientHeight)
 *     and scrolls to its last row.
 *
 * Usage: BASE_URL=http://localhost:13581 node test-940-moments-drawer-rows-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
if (!['localhost', '127.0.0.1', '[::1]'].includes(new URL(BASE).hostname)) throw new Error('local server only');

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + (e && e.stack || e)); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

const PK = (c) => c.repeat(64);
const at = (agoS) => new Date(Date.now() - agoS * 1000).toISOString();
const N = 34;
const KINDS = ['new_node', 'back_online', 'record_link', 'busiest_repeater'];
function moment(i) {
  const node = { publicKey: PK('abcdef'[i % 6]), name: 'E2E-940 node ' + i, role: 'repeater', lat: 55 + i * 0.01, lon: 9 + i * 0.01 };
  const kind = KINDS[i % KINDS.length];
  const m = { id: 'e940-' + i, kind, at: at(60 + i * 30), node };
  if (kind === 'back_online') m.silentHours = 37;
  if (kind === 'record_link') {
    m.link = { a: Object.assign({}, node, { name: 'E2E-940 A' + i }), b: Object.assign({}, node, { name: 'E2E-940 B' + i, lat: 55.5, lon: 10 }), distanceKm: 61.2 };
    delete m.node;
  }
  if (kind === 'busiest_repeater') m.packets = 1200 + i;
  return m;
}
const HISTORY = Array.from({ length: N }, (_, i) => moment(i));

async function run(browser, viewport) {
  const ctx = await browser.newContext({ viewport });
  await ctx.route(/\/api\/config\/client(\?|$)/, async (route) => {
    const res = await route.fetch();
    const body = await res.json();
    body.moments = { enabled: true, historySize: 50 };
    await route.fulfill({ response: res, json: body });
  });
  await ctx.route(/\/api\/moments(\?|$)/, (route) => route.fulfill({ json: { moments: HISTORY } }));
  await ctx.routeWebSocket(/.*/, () => {});
  const page = await ctx.newPage();
  const errors = [];
  page.on('pageerror', (e) => errors.push(String(e.message)));
  await page.goto(BASE + '/?r=' + Date.now() + '#/live?lat=55.3&lon=9.6&zoom=9', { waitUntil: 'domcontentloaded' });
  await page.waitForFunction(() => window._liveMoments && window._liveMoments(), null, { timeout: 20000 });
  await page.click('.live-moments-bar .live-moments-open');
  await page.waitForSelector('.live-moments-drawer:not([hidden])');
  await page.waitForFunction(() => document.querySelectorAll('.live-moment-row').length >= 20);
  const geo = await page.evaluate(() => {
    const list = document.querySelector('.live-moments-list');
    const rows = [...list.querySelectorAll('.live-moment-row')];
    return {
      rows: rows.length,
      scrollH: list.scrollHeight, clientH: list.clientHeight,
      // The row's content height: its padding-free text column and its
      // button, whichever is taller, plus the row border.
      short: rows.map((r, i) => {
        const h = r.getBoundingClientRect().height;
        let need = 0;
        for (const c of r.children) need = Math.max(need, c.scrollHeight);
        need += r.offsetHeight - r.clientHeight;
        const kids = [...r.querySelectorAll('.live-moment-title, .live-moment-detail')];
        const textBottom = Math.max.apply(null, kids.map((k) => k.getBoundingClientRect().bottom));
        return { i, h: Math.round(h), need: Math.round(need), clipped: textBottom > r.getBoundingClientRect().bottom + 0.5 };
      }).filter((x) => x.h + 0.5 < x.need || x.clipped),
    };
  });
  const scrolled = await page.evaluate(() => {
    const list = document.querySelector('.live-moments-list');
    list.scrollTop = list.scrollHeight;
    const last = list.querySelector('.live-moment-row:last-child').getBoundingClientRect();
    const box = list.getBoundingClientRect();
    return { top: list.scrollTop, lastBottom: Math.round(last.bottom), boxBottom: Math.round(box.bottom) };
  });
  await ctx.close();
  return { geo, scrolled, errors };
}

(async () => {
  const browser = await chromium.launch({ headless: true, executablePath: process.env.CHROMIUM_PATH || undefined });
  try {
    console.log('\n=== #940 Moments drawer rows E2E ===');
    for (const vp of [{ width: 1920, height: 1080 }, { width: 375, height: 560 }]) {
      await step(vp.width + 'x' + vp.height + ': ' + N + ' moments keep full-height rows and the list scrolls', async () => {
        const r = await run(browser, vp);
        assert(r.geo.rows === 20, 'drawer lists the newest 20, got ' + r.geo.rows);
        assert(!r.geo.short.length, 'rows squashed or clipped: ' + JSON.stringify(r.geo.short.slice(0, 5)));
        assert(r.geo.scrollH > r.geo.clientH + 1, 'list overflows: ' + r.geo.scrollH + ' <= ' + r.geo.clientH);
        assert(r.scrolled.top > 0, 'list scrolls');
        assert(r.scrolled.lastBottom <= r.scrolled.boxBottom + 1, 'last row reachable: ' + JSON.stringify(r.scrolled));
        assert(!r.errors.length, r.errors.join('; '));
      });
    }
  } finally {
    await browser.close();
  }
  console.log('\n' + passed + ' passed, ' + failed + ' failed');
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
