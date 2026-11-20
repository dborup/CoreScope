/**
 * E2E: Live packet feed items must keep their height and the feed must
 * scroll when it is full (#943; same root cause as the #940 moments drawer).
 *
 * .live-feed .panel-content is a column flex container with min-height:0 and
 * overflow-y:auto. Flex items default to flex-shrink:1, so a full feed
 * squeezed every .live-feed-item (overflow:hidden) below its content height
 * instead of overflowing and scrolling.
 *
 * The app's shared WebSocket is replaced with page.routeWebSocket and fed 30
 * packets (the feed keeps 25). At 1920x1080 and 375x560 every item must be at
 * least as tall as its content, nothing clipped, the list must scroll and
 * reach the last item. Also a short feed (3 items) keeps its natural height.
 *
 * Usage: BASE_URL=http://localhost:13581 node test-943-live-feed-items-e2e.js
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

function pkt(i) {
  const hash = ('943' + String(i).padStart(3, '0')).padEnd(16, 'a');
  return { type: 'packet', data: {
    id: 943000 + i, hash, observer_id: 'obs943', observer_name: 'E2E-943', snr: 5, rssi: -90,
    raw_hex: '15000102030405060708090a0b0c0d0e0f',
    timestamp: new Date().toISOString(),
    decoded: { header: { payloadTypeName: 'GRP_TXT' }, payload: { channel: '#e2e943', text: 'E2E-943 message number ' + i + ' with some text' }, path: { hops: [] } },
  } };
}

async function open(browser, viewport, n) {
  const ctx = await browser.newContext({ viewport });
  const sockets = [];
  await ctx.routeWebSocket(/.*/, (ws) => { sockets.push(ws); });
  const page = await ctx.newPage();
  const errors = [];
  page.on('pageerror', (e) => errors.push(String(e.message)));
  await page.goto(BASE + '/?r=' + Date.now() + '#/live?lat=55.3&lon=9.6&zoom=9', { waitUntil: 'domcontentloaded' });
  await page.waitForFunction(() => window._liveWSHandler && window._liveWSHandler(), null, { timeout: 20000 });
  const ws = sockets[sockets.length - 1];
  for (let i = 0; i < n; i++) ws.send(JSON.stringify(pkt(i)));
  await page.waitForFunction((want) => document.querySelectorAll('.live-feed-item').length >= want, Math.min(n, 25), { timeout: 10000 });
  await page.waitForTimeout(600); // enter transition
  return { ctx, page, errors };
}

// measure reports, per item, its height against the height its content needs.
function measure() {
  const pc = document.querySelector('#liveFeed .panel-content');
  const items = [...pc.querySelectorAll('.live-feed-item')];
  const short = [];
  items.forEach((el, i) => {
    const cs = getComputedStyle(el);
    let need = 0;
    for (const c of el.children) need = Math.max(need, c.offsetHeight);
    need += parseFloat(cs.paddingTop) + parseFloat(cs.paddingBottom);
    if (el.offsetHeight + 0.5 < need || el.scrollHeight > el.clientHeight + 1) {
      short.push(i + ': h=' + el.offsetHeight + ' need=' + Math.round(need) + ' scroll=' + el.scrollHeight);
    }
  });
  const last = items[items.length - 1];
  const before = pc.scrollTop;
  pc.scrollTop = pc.scrollHeight;
  const pr = pc.getBoundingClientRect(), lr = last.getBoundingClientRect();
  const reached = lr.bottom <= pr.bottom + 1 && lr.bottom > pr.top;
  pc.scrollTop = before;
  const sum = items.reduce((a, el) => a + el.offsetHeight, 0);
  const oy = getComputedStyle(pc).overflowY;
  return { oy, count: items.length, short, scrollH: pc.scrollHeight, clientH: pc.clientHeight, reached, sum,
    visible: getComputedStyle(document.getElementById('liveFeed')).display !== 'none' && pc.clientHeight > 0 };
}

(async () => {
  const browser = await chromium.launch({ executablePath: process.env.CHROMIUM_PATH || undefined, args: ['--no-sandbox'] });
  for (const vp of [{ width: 1920, height: 1080 }, { width: 375, height: 560 }]) {
    const tag = vp.width + 'x' + vp.height;
    await step('full feed at ' + tag + ': items keep content height, feed scrolls to last item', async () => {
      const h = await open(browser, vp, 30);
      try {
        const m = await h.page.evaluate(measure);
        if (!m.visible) { console.log('    (feed not visible at ' + tag + ', skipped)'); return; }
        assert(m.count === 25, 'feed capped at 25 items, got ' + m.count);
        assert(m.short.length === 0, 'items shrunk/clipped: ' + m.short.slice(0, 5).join('; '));
        assert(m.scrollH > m.clientH, 'feed must scroll: scrollHeight ' + m.scrollH + ' <= clientHeight ' + m.clientH);
        assert(m.oy === 'auto' || m.oy === 'scroll', 'feed body must be user-scrollable, overflow-y is ' + m.oy);
        assert(m.reached, 'scrolling must reach the last item');
        assert(h.errors.length === 0, 'page errors: ' + h.errors.join(' | '));
      } finally { await h.ctx.close(); }
    });
  }
  await step('a short feed (3 items) keeps natural height and does not scroll', async () => {
    const h = await open(browser, { width: 1920, height: 1080 }, 3);
    try {
      const m = await h.page.evaluate(measure);
      assert(m.count === 3 && m.short.length === 0, 'short feed items: ' + JSON.stringify(m.short));
      assert(m.scrollH <= m.clientH + 1, 'short feed must not scroll');
    } finally { await h.ctx.close(); }
  });
  await browser.close();
  console.log(passed + ' passed, ' + failed + ' failed');
  process.exit(failed ? 1 : 0);
})();
