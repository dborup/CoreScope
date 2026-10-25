'use strict';
// #562 — battery badge: real SVG icon at the shared icon size, no SVG source text,
// hostile voltage inert, chart sizing unaffected. Network responses are intercepted.
const assert = require('node:assert/strict');
const { chromium } = require('playwright');
const BASE = process.env.BASE_URL || 'http://localhost:13581';
assert.ok(['localhost', '127.0.0.1', '[::1]'].includes(new URL(BASE).hostname), 'local server required');
const PK = 'ab'.repeat(32);
const json = body => ({ status: 200, contentType: 'application/json', body: JSON.stringify(body) });
const analytics = { node: { name: 'Badge node', public_key: PK, role: 'repeater' },
  computedStats: { totalPackets: 1, availabilityPct: 100, signalGrade: 'B', avgPacketsPerDay: 1, uniqueObservers: 1, relayPct: 0, longestSilenceMs: 0 },
  activityTimeline: [], snrTrend: [], packetTypeBreakdown: [], observerCoverage: [], hopDistribution: [], peerInteractions: [], uptimeHeatmap: [] };
const samples = [0, 1, 2].map(i => ({ timestamp: new Date(Date.now() - i * 3600e3).toISOString(), battery_mv: 3900 - i * 100 }));
const CASES = [
  { status: 'ok', mv: 3900, cls: 'status-ok', cssVar: '--status-green' },
  { status: 'low', mv: 3200, cls: 'status-warn', cssVar: '--status-yellow' },
  { status: 'critical', mv: 2900, cls: 'status-err', cssVar: '--status-red' },
  { status: 'unknown', mv: 0, cls: 'status-muted', cssVar: null },
];
(async () => {
  const browser = await chromium.launch({ headless: true, ...(process.env.CHROMIUM_PATH ? { executablePath: process.env.CHROMIUM_PATH } : {}) });
  let passed = 0;
  try {
    for (const theme of ['light', 'dark']) for (const width of [1440, 375]) for (const c of [...CASES, { status: 'ok', mv: '<img src=x onerror="window.__pwn=1">', hostile: true, cls: 'status-ok', cssVar: '--status-green' }]) {
      const context = await browser.newContext({ viewport: { width, height: 900 } });
      await context.addInitScript(t => { try { localStorage.setItem('meshcore-theme', t); } catch (e) {} }, theme);
      const page = await context.newPage(), errors = [];
      page.on('pageerror', e => errors.push(e.message));
      await context.route(u => u.pathname === `/api/nodes/${PK}/analytics`, r => r.fulfill(json(analytics)));
      await context.route(u => u.pathname === `/api/nodes/${PK}/hop_analytics`, r => r.fulfill(json({ packets: [] })));
      await context.route(u => u.pathname === `/api/nodes/${PK}/battery`, r => r.fulfill(json(
        { status: c.status, latest_mv: c.mv, samples: c.status === 'unknown' ? [] : samples, thresholds: { low_mv: 3300, critical_mv: 3000 } })));
      await page.goto(`${BASE}/#/nodes/${PK}/analytics`, { waitUntil: 'domcontentloaded' });
      await page.evaluate(t => document.documentElement.setAttribute('data-theme', t), theme);
      const badge = page.locator('#batteryStatusBadge');
      await badge.locator('.battery-status-label').waitFor();
      const text = await badge.textContent();
      assert.ok(!/<svg|<use|&lt;svg/.test(text), 'no SVG source shown: ' + text);
      assert.match(text, c.status === 'unknown' ? /No data/ : new RegExp({ ok: 'OK', low: 'Low', critical: 'Critical' }[c.status]));
      const icons = badge.locator('svg.ph-icon');
      if (c.status === 'unknown') assert.equal(await icons.count(), 0);
      else {
        assert.equal(await icons.count(), 1);
        assert.equal(await icons.getAttribute('aria-hidden'), 'true');
        const box = await icons.boundingBox();
        assert.ok(Math.abs(box.width - 16) < 0.5 && Math.abs(box.height - 16) < 0.5, `icon is 16x16, got ${box.width}x${box.height}`);
        const drawn = await icons.evaluate(s => s.getBoundingClientRect().width > 0 && getComputedStyle(s).display === 'inline-block');
        assert.ok(drawn, 'icon rendered inline');
      }
      assert.ok(await badge.evaluate((b, cls) => b.classList.contains(cls), c.cls));
      if (c.cssVar) {
        const same = await page.evaluate(([v]) => {
          const probe = document.createElement('i'); probe.style.color = `var(${v})`; document.body.appendChild(probe);
          const want = getComputedStyle(probe).color; probe.remove();
          return getComputedStyle(document.getElementById('batteryStatusBadge')).color === want;
        }, [c.cssVar]);
        assert.ok(same, 'badge colour comes from ' + c.cssVar);
      }
      if (c.hostile) {
        assert.equal(await badge.locator('img').count(), 0, 'no injected element');
        assert.equal(await page.evaluate(() => window.__pwn), undefined, 'no script execution');
        assert.ok(text.includes('onerror'), 'payload shown as text');
      }
      if (c.status !== 'unknown' && !c.hostile) {
        const canvas = page.locator('#batteryChart'); await canvas.waitFor();
        const [cb, card] = await Promise.all([canvas.boundingBox(), page.locator('.analytics-chart-card.full:has(#batteryChart)').boundingBox()]);
        assert.ok(cb.width > 100 && cb.width <= card.width + 1, 'chart stays responsive inside its card');
      }
      if (!c.hostile && c.status !== 'unknown') {
        // Screenshot check: the captured icon is 16x16 CSS px (PNG IHDR at DPR 1) and not blank.
        const png = await icons.screenshot();
        for (const off of [16, 20]) assert.ok([16, 17].includes(png.readUInt32BE(off)), "icon screenshot is ~16px (rounded to pixel bounds)");
        assert.ok(png.length > 100, 'icon screenshot is not a blank image');
      }
      assert.deepEqual(errors, [], 'no page errors');
      console.log(`  ✓ ${c.hostile ? 'hostile' : c.status} ${theme} ${width}px`); passed++;
      await context.close();
    }
  } finally { await browser.close(); }
  console.log(`\n${passed} passed`);
})().catch(e => { console.error(e); process.exit(1); });
