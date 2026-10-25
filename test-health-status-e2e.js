// Exercise the real SPA with synthetic API data, including authoritative
// identity-safe activity. This local-only server never reads a mesh database.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const http = require('node:http');
const path = require('node:path');
const { chromium } = require('playwright');

const publicDir = path.join(__dirname, 'public');
const NOW = Date.UTC(2026, 9, 24, 12);
const HOUR = 3600000;
const iso = hours => new Date(NOW - hours * HOUR).toISOString();
const fixtures = [
  { name: 'Repeater five', role: 'repeater', hours: 5, want: 'healthy', custom: 'degraded' },
  { name: 'Repeater thirty', role: 'repeater', hours: 30, want: 'degraded', custom: 'silent' },
  { name: 'Room seventy two', role: 'room', hours: 72, want: 'silent', custom: 'silent' },
  { name: 'Companion one', role: 'companion', hours: 1, want: 'degraded', custom: 'degraded' },
  { name: 'Sensor twenty four', role: 'sensor', hours: 24, want: 'silent', custom: 'silent' },
  { name: 'Legacy directory', role: 'repeater', hours: 30, legacy: true, want: 'degraded', custom: 'silent' },
  { name: 'Confirmed relay', role: 'repeater', hours: 0.1, relay: true, want: 'healthy', custom: 'healthy' },
  { name: 'Unsafe directory', role: 'repeater', hours: 0.1, unknown: true, want: 'silent', custom: 'silent' },
  { name: 'Invalid timestamp', role: 'repeater', hours: 0.1, invalid: true, want: 'silent', custom: 'silent' },
].map((fixture, i) => ({ ...fixture, key: (i + 1).toString(16).padStart(2, '0') + '12'.repeat(31) }));
const byKey = new Map(fixtures.map(f => [f.key, f]));
function node(f) {
  return {
    public_key: f.key, name: f.name, role: f.role,
    last_seen: iso(f.hours), last_heard: f.invalid ? 'invalid' : iso(f.hours),
    // A fresh ambiguous prefix is not enough to override health's explicit null.
    last_relayed: iso(0.01), relay_active: true,
    lat: 56 + Number.parseInt(f.key.slice(0, 2), 16) / 100, lon: 10,
  };
}
function health(f) {
  return {
    node: node(f), directObservers: [], relayObservers: [], recentPackets: [],
    stats: {
      ...(f.legacy ? {} : { lastHeard: f.unknown ? null : f.invalid ? 'invalid' : iso(f.hours), lastAdvert: f.relay || f.unknown ? null : iso(f.hours) }),
      packetsToday: 0, avgSnr: null, avgHops: null,
    },
  };
}
function json(res, data) {
  res.writeHead(200, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' });
  res.end(JSON.stringify(data));
}
const server = http.createServer((req, res) => {
  const url = new URL(req.url, 'http://127.0.0.1');
  const pathname = url.pathname;
  if (pathname === '/api/nodes') return json(res, { nodes: fixtures.map(node), total: fixtures.length, counts: {} });
  if (pathname === '/api/nodes/bulk-health') return json(res, []);
  if (pathname === '/api/nodes/clock-skew') return json(res, []);
  const match = pathname.match(/^\/api\/nodes\/([0-9a-f]{64})(?:\/(.*))?$/);
  if (match && byKey.has(match[1])) {
    const f = byKey.get(match[1]);
    if (match[2] === 'health') return json(res, health(f));
    if (match[2] === 'neighbors') return json(res, { neighbors: [] });
    return json(res, { node: node(f), recentAdverts: [] });
  }
  if (pathname === '/api/observers') return json(res, { observers: [] });
  if (pathname === '/api/channels') return json(res, { channels: [] });
  if (pathname === '/api/packets') return json(res, { packets: [], total: 0 });
  if (pathname === '/api/analytics/neighbor-graph') return json(res, { nodes: [], edges: [] });
  if (pathname.startsWith('/api/')) return json(res, {});
  const file = pathname === '/' ? path.join(publicDir, 'index.html') : path.resolve(publicDir, '.' + pathname);
  if (!file.startsWith(publicDir + path.sep) || !fs.existsSync(file) || !fs.statSync(file).isFile()) {
    res.writeHead(404); return res.end();
  }
  const types = { '.js': 'application/javascript', '.css': 'text/css', '.html': 'text/html', '.svg': 'image/svg+xml' };
  res.writeHead(200, { 'Content-Type': types[path.extname(file)] || 'application/octet-stream' });
  res.end(fs.readFileSync(file));
});

(async () => {
  let browser, checks = 0;
  try {
    await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
    const origin = 'http://127.0.0.1:' + server.address().port;
    browser = await chromium.launch({ headless: true, executablePath: process.env.CHROMIUM_PATH || undefined });
    for (const mode of [{ width: 1440, theme: 'dark', custom: false }, { width: 360, theme: 'light', custom: true }, { width: 1440, theme: 'light', custom: true }]) {
      const ctx = await browser.newContext({ viewport: { width: mode.width, height: 900 } });
      await ctx.addInitScript(({ keys, theme }) => {
        localStorage.setItem('meshcore-user-level', 'experienced');
        localStorage.setItem('meshcore-theme', theme);
        localStorage.setItem('meshcore-my-nodes', JSON.stringify(keys.map(pubkey => ({ pubkey, name: 'Synthetic' }))));
        localStorage.setItem('meshcore-favorites', JSON.stringify(keys));
      }, { keys: fixtures.map(f => f.key), theme: mode.theme });
      const page = await ctx.newPage();
      page.setDefaultTimeout(15000);
      await page.clock.setFixedTime(new Date(NOW));
      const errors = [];
      page.on('pageerror', error => errors.push(error.message));
      await page.route('**/*', route => new URL(route.request().url()).origin === origin ? route.continue() : route.abort());
      if (mode.custom) await page.route('**/api/config/client', route => route.fulfill({ json: {
        healthThresholds: { infraDegradedMs: 2 * HOUR, infraSilentMs: 6 * HOUR, nodeDegradedMs: HOUR, nodeSilentMs: 24 * HOUR },
      } }));
      const expected = f => mode.custom ? f.custom : f.want;
      // #304: the badge now names the evidence behind it. A fixture with no
      // own advert — the confirmed-relay case and the legacy directory
      // fallback — can only be attributed, so its label carries the suffix;
      // a fixture with no identity-safe activity at all has nothing to
      // attribute and keeps the plain label.
      const STATUS_LABEL = { healthy: 'Active', degraded: 'Degraded', silent: 'Silent' };
      const expectedLabel = f => STATUS_LABEL[expected(f)] +
        (!f.unknown && !f.invalid && (f.relay || f.legacy) ? ' · relay-attributed' : '');
      await page.goto(origin + '/#/home', { waitUntil: 'domcontentloaded' });
      await page.waitForFunction(n => document.querySelectorAll('#myNodesGrid .my-node-card').length === n, fixtures.length);
      assert.equal(await page.locator('html').getAttribute('data-theme'), mode.theme, 'requested theme is applied'); checks++;
      for (const f of fixtures) {
        const card = page.locator('#myNodesGrid .my-node-card[data-key="' + f.key + '"]');
        assert.ok((await card.getAttribute('class')).split(/\s+/).includes(expected(f)), `${mode.width} ${f.name}: My Mesh status`); checks++;
        const visibleTime = await card.locator('.mnc-status-text').innerText();
        assert.equal(visibleTime.includes('·'), !f.unknown && !f.invalid, f.name + ': time follows safe evidence'); checks++;
        await card.locator('.mnc-name').click();
        const banner = page.locator('#homeHealth .health-banner');
        await page.waitForFunction(name => document.querySelector('#homeHealth .health-banner')?.textContent.includes(name), f.name);
        assert.equal(await banner.locator('[aria-label]').first().getAttribute('aria-label'), expectedLabel(f), f.name + ': health detail'); checks++;
        assert.ok((await card.locator('.mnc-status .ph-icon use').getAttribute('href')).endsWith(
          !f.unknown && !f.invalid && (f.relay || f.legacy) ? '#ph-circle' : '#ph-circle-fill'), f.name + ': evidence glyph'); checks++;
        if (f.unknown || f.invalid) {
          assert.match(await banner.innerText(), /No confirmed activity in the current window/); checks++;
        }
      }
      if (mode.width > 768) {
        await page.locator('#favToggle').click();
        await page.locator('#favDropdown .fav-dd-item[data-key="' + fixtures[0].key + '"]').waitFor();
        for (const f of fixtures) {
          const dot = page.locator('#favDropdown .fav-dd-item[data-key="' + f.key + '"] .fav-dd-status');
          assert.equal(await dot.getAttribute('title'), f.unknown || f.invalid ? 'unknown' : expected(f), f.name + ': favorite'); checks++;
        }
      } else {
        // Existing bottom-nav layout intentionally suppresses the desktop
        // header controls. Do not manufacture a mobile Favorites flow.
        assert.equal(await page.locator('#favToggle').isVisible(), false); checks++;
      }
      if (process.env.SCREENSHOT_DIR) await page.screenshot({ path: path.join(process.env.SCREENSHOT_DIR, `role-health-${mode.width}-${mode.theme}.png`), fullPage: true });
      await page.goto(origin + '/#/analytics?tab=nodes', { waitUntil: 'domcontentloaded' });
      const network = page.locator('.analytics-section', { hasText: 'Network Status' }).first().locator('.analytics-stat-card');
      await network.first().waitFor();
      // Network status keeps its directory input, not a per-node health fetch.
      const wantCounts = [0, 0, 0];
      const listStates = await page.evaluate(list => list.map(n => getHealthStatus(n.role, getHealthLastHeard(n))), fixtures.map(node));
      for (const status of listStates) wantCounts[status === 'healthy' ? 0 : status === 'degraded' ? 1 : 2]++;
      const counts = await network.locator('div:first-child').allTextContents();
      assert.deepEqual(counts.slice(0, 3).map(Number), wantCounts, 'network three-state counts'); checks++;
      if (!mode.custom) {
        const timings = await page.evaluate(({ now }) => {
          const nodes = Array.from({ length: 30000 }, (_, i) => ({ role: i % 2 ? 'repeater' : 'companion', last_heard: new Date(now - (i % 80) * 3600000).toISOString() }));
          const old = [], shared = [];
          for (let run = 0; run < 23; run++) {
            let start = performance.now(), count = 0;
            for (const n of nodes) {
              const th = getHealthThresholds(n.role), age = now - new Date(n.last_heard).getTime();
              count += age < th.degradedMs ? 1 : age < th.silentMs ? 2 : 3;
            }
            const before = performance.now() - start;
            start = performance.now();
            let next = 0;
            for (const n of nodes) {
              const s = getHealthStatus(n.role, getHealthLastHeard(n), now);
              next += s === 'healthy' ? 1 : s === 'degraded' ? 2 : 3;
            }
            const after = performance.now() - start;
            if (count !== next) throw new Error('benchmark classifications differ');
            if (run > 1) { old.push(before); shared.push(after); }
          }
          return { beforeMedianMs: old.sort((a, b) => a - b)[10], afterMedianMs: shared.sort((a, b) => a - b)[10] };
        }, { now: NOW });
        console.log('PERF 30000-node classification (21 warm runs): ' + JSON.stringify(timings));
      }
      await page.goto(origin + '/#/analytics?tab=my-repeaters', { waitUntil: 'domcontentloaded' });
      await page.getByText('Repeater thirty', { exact: true }).first().waitFor();
      const quietRow = page.locator('tr', { hasText: 'Repeater thirty' });
      assert.match(await quietRow.innerText(), mode.custom ? /Silent/ : /Degraded/); checks++;
      await page.goto(origin + '/#/live?lat=56&lon=10&zoom=10', { waitUntil: 'domcontentloaded' });
      await page.waitForFunction(() => window.MeshConfigReady && document.querySelector('#liveMap .live-node-marker'));
      await page.locator('#liveMap .live-node-marker').first().click();
      await page.locator('#nodeDetailContent .health-green, #nodeDetailContent .health-yellow, #nodeDetailContent .health-red').first().waitFor();
      assert.match(await page.locator('#nodeDetailContent').innerText(), mode.custom ? /Degraded/ : /Online/); checks++;
      assert.deepEqual(errors, [], 'no uncaught browser errors'); checks++;
      await ctx.close();
      console.log(`PASS ${mode.width}px ${mode.theme} ${mode.custom ? 'custom' : 'default'} thresholds: ${mode.width > 768 ? 'five views' : 'four views; Favorites intentionally hidden'} and My Repeaters`);
    }
    console.log(checks + ' checks passed');
  } finally {
    if (browser) await browser.close();
    await new Promise(resolve => server.close(resolve));
  }
})().catch(error => { console.error(error); process.exitCode = 1; });
