// #901: the Live node popup reads its health from the shared last-heard
// evidence (stats.lastHeard), not from last_seen. Synthetic local API; no
// mesh database. 375x560 touch phone, cold start, real taps.
'use strict';
const { AxeBuilder } = require('@axe-core/playwright');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const http = require('node:http');
const path = require('node:path');
const { chromium } = require('playwright');

const publicDir = path.join(__dirname, 'public');
const NOW = Date.UTC(2026, 9, 24, 12);
const HOUR = 3600000;
const iso = hours => new Date(NOW - hours * HOUR).toISOString();
// last_seen and the health API disagree in both directions.
const fixtures = [
  { name: 'Stale seen fresh heard', seen: 100, heard: 2, want: 'Online', cls: 'health-green' },
  { name: 'Fresh seen never heard', seen: 1, heard: null, want: 'Unknown', cls: 'health-red' },
  { name: 'Fresh seen old heard', seen: 1, heard: 100, want: 'Offline', cls: 'health-red' },
].map((f, i) => ({ ...f, key: (i + 1).toString(16).padStart(2, '0') + '34'.repeat(31) }));
const byKey = new Map(fixtures.map(f => [f.key, f]));
const node = f => ({
  public_key: f.key, name: f.name, role: 'repeater', last_seen: iso(f.seen), last_heard: iso(f.seen),
  lat: 56 + Number.parseInt(f.key.slice(0, 2), 16) / 1000, lon: 10, advert_count: 1,
});
const health = f => ({
  node: node(f), directObservers: [], relayObservers: [], recentPackets: [],
  stats: { lastHeard: f.heard == null ? null : iso(f.heard), packetsToday: 0, avgSnr: null, avgHops: null },
});
function json(res, data) {
  res.writeHead(200, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' });
  res.end(JSON.stringify(data));
}
const server = http.createServer((req, res) => {
  const pathname = new URL(req.url, 'http://127.0.0.1').pathname;
  if (pathname === '/api/nodes') return json(res, { nodes: fixtures.map(node), total: fixtures.length, counts: {} });
  const match = pathname.match(/^\/api\/nodes\/([0-9a-f]{64})(?:\/(.*))?$/);
  if (match && byKey.has(match[1])) {
    const f = byKey.get(match[1]);
    if (match[2] === 'health') return json(res, health(f));
    if (match[2] === 'paths') return json(res, { paths: [], totalPaths: 0 });
    return json(res, { node: node(f), recentAdverts: [] });
  }
  if (pathname === '/api/observers') return json(res, { observers: [] });
  if (pathname === '/api/packets') return json(res, { packets: [], total: 0 });
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
    for (const f of fixtures) {
      // Cold start per fixture: fresh context, no storage, touch phone.
      const ctx = await browser.newContext({ viewport: { width: 375, height: 560 }, hasTouch: true, isMobile: true });
      await ctx.addInitScript(() => localStorage.setItem('meshcore-user-level', 'experienced'));
      const page = await ctx.newPage();
      page.setDefaultTimeout(15000);
      await page.clock.setFixedTime(new Date(NOW));
      const errors = [];
      page.on('pageerror', e => errors.push(e.message));
      await page.route('**/*', r => new URL(r.request().url()).origin === origin ? r.continue() : r.abort());
      const lat = 56 + Number.parseInt(f.key.slice(0, 2), 16) / 1000;
      await page.goto(origin + '/#/live?lat=' + lat + '&lon=10&zoom=13', { waitUntil: 'domcontentloaded' });
      await page.waitForFunction(() => window.MeshConfigReady && document.querySelectorAll('#liveMap .live-node-marker').length >= 1);
      // Real tap on the marker that belongs to this fixture.
      const idx = await page.evaluate(k => {
        const m = window._liveNodeMarkers()[k];
        return m && m.getElement ? Array.from(document.querySelectorAll('#liveMap .live-node-marker')).indexOf(m.getElement()) : -1;
      }, f.key);
      assert.ok(idx >= 0, f.name + ': marker present');
      const box = await page.locator('#liveMap .live-node-marker').nth(idx).boundingBox();
      await page.touchscreen.tap(box.x + box.width / 2, box.y + box.height / 2);
      const panel = page.locator('#nodeDetailContent');
      await panel.getByText(f.name, { exact: true }).waitFor();
      const dot = panel.locator('span[aria-hidden="true"]').first();
      assert.ok((await dot.getAttribute('class')).split(/\s+/).includes(f.cls), f.name + ': dot class ' + f.cls); checks++;
      // The accessible name is what the browser exposes, not an attribute
      // string: "Status: X" in the accessibility tree, "X" on screen.
      const aria = await panel.ariaSnapshot();
      assert.ok(aria.includes('Status: ' + f.want), f.name + ': accessible status text in ' + aria); checks++;
      const status = panel.getByText('Status: ' + f.want, { exact: true });
      const prefix = status.locator('.sr-only');
      assert.equal(await prefix.textContent(), 'Status: ');
      assert.ok((await prefix.boundingBox()).width <= 1, f.name + ': "Status:" prefix is visually hidden');
      assert.equal(await status.evaluate(el => el.lastChild.textContent), f.want, f.name + ': visible status text'); checks++;
      const axe = await new AxeBuilder({ page }).include('#nodeDetailContent').withRules(['aria-prohibited-attr']).analyze();
      assert.deepEqual(axe.violations.concat(axe.incomplete).map(v => v.id), [], f.name + ': axe aria-prohibited-attr'); checks++;
      const row = panel.locator('tr', { hasText: 'Last Heard' });
      const rowText = await row.innerText();
      if (f.heard == null) {
        assert.ok(!/ago|\d{4}/.test(rowText.replace('Last Heard', '')), f.name + ': no time without evidence: ' + rowText);
      } else {
        // The shown time is the stats.lastHeard time, never the last_seen one.
        const fmt = ts => page.evaluate(v => formatTimestampWithTooltip(v, getTimestampMode()).text, ts);
        const shown = await row.locator('.timestamp-text').innerText();
        assert.equal(shown, await fmt(iso(f.heard)), f.name + ': time follows lastHeard');
        assert.notEqual(shown, await fmt(iso(f.seen)), f.name + ': fixture disagrees with last_seen');
      }
      checks++;
      assert.deepEqual(errors, [], 'no uncaught browser errors'); checks++;
      console.log('PASS ' + f.name + ' -> ' + f.want);
      await ctx.close();
    }
    console.log(checks + ' checks passed');
  } finally {
    if (browser) await browser.close();
    await new Promise(resolve => server.close(resolve));
  }
})().catch(error => { console.error(error); process.exitCode = 1; });
