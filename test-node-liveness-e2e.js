// Actual SPA/status rendering with synthetic API evidence; never contacts prod.
// Run: node test-node-liveness-e2e.js (Playwright Chromium required).
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const http = require('node:http');
const path = require('node:path');
const { chromium } = require('playwright');

const publicDir = path.join(__dirname, 'public');
const stale = new Date(Date.now() - 4 * 86400000).toISOString();
const recent = new Date(Date.now() - 20 * 60000).toISOString();
const cases = [
  { name: 'Offline collision', active: false, advert: stale, heard: stale },
  { name: 'Own advert', active: true, advert: recent, heard: recent },
  { name: 'Relay without advert', active: true, advert: null, heard: recent, relay: true },
  { name: 'Legacy unsafe health', active: false, advert: stale, heard: recent, legacy: true },
  { name: 'Unknown safe activity', active: false, advert: null, heard: null },
].map((fixture, index) => ({ ...fixture, key: (index + 1).toString(16).padStart(2, '0') + '11'.repeat(31) }));
const nodes = cases.map(fixture => ({
  public_key: fixture.key, name: fixture.name, role: 'repeater',
  lat: null, lon: null, last_seen: recent, last_heard: recent,
  first_seen: stale, advert_count: fixture.advert ? 1 : 0,
  last_relayed: fixture.relay ? recent : null, relay_active: !!fixture.relay,
  relay_count_1h: fixture.relay ? 1 : 0, relay_count_24h: fixture.relay ? 1 : 0,
}));

function json(res, data) {
  res.writeHead(200, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' });
  res.end(JSON.stringify(data));
}
const server = http.createServer((req, res) => {
  const pathname = new URL(req.url, 'http://127.0.0.1').pathname;
  if (pathname === '/api/nodes') return json(res, { nodes, total: nodes.length, counts: { all: nodes.length, repeater: nodes.length } });
  const match = pathname.match(/^\/api\/nodes\/([^/]+)(?:\/(.*))?$/);
  if (match) {
    const index = cases.findIndex(fixture => fixture.key === match[1]);
    if (index >= 0) {
      const fixture = cases[index], node = nodes[index];
      if (match[2] === 'health') return json(res, {
        node, observers: [], recentPackets: [], stats: {
          lastHeard: fixture.heard, ...(fixture.legacy ? {} : { lastAdvert: fixture.advert }),
          totalPackets: 2, totalTransmissions: 2, totalObservations: 2, packetsToday: 1,
        },
      });
      if (match[2] === 'neighbors') return json(res, { neighbors: [] });
      if (match[2] === 'paths') return json(res, { paths: [], totalTransmissions: 0 });
      if (!match[2]) return json(res, {
        node, recentAdverts: fixture.advert ? [{
          id: 1, hash: 'synthetic-advert', payload_type: 4, route_type: 1,
          first_seen: fixture.advert, timestamp: fixture.advert,
          decoded_json: JSON.stringify({ type: 'ADVERT', pubKey: fixture.key, signatureValid: true }), observations: [],
        }] : [],
      });
    }
  }
  if (pathname === '/api/observers') return json(res, { observers: [] });
  if (pathname === '/api/channels') return json(res, { channels: [] });
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
  let browser;
  let checks = 0;
  try {
    await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
    const origin = 'http://127.0.0.1:' + server.address().port;
    browser = await chromium.launch({ headless: true, executablePath: process.env.CHROMIUM_PATH || undefined });
    const page = await browser.newPage({ viewport: { width: 1440, height: 1000 } });
    // Exclude CDN/fonts/map tiles and WebSocket reconnects from this local test.
    await page.route('**/*', route => new URL(route.request().url()).origin === origin ? route.continue() : route.abort());
    for (const fixture of cases) {
      await page.goto(origin + '/#/nodes/' + fixture.key, { waitUntil: 'domcontentloaded' });
      const body = page.locator('#nodeFullBody');
      await body.locator('tr').filter({ hasText: 'Last Heard (advert)' }).waitFor();
      const text = await body.innerText();
      assert.match(text, fixture.active ? /Active/ : /Stale/, fixture.name + ': full detail status'); checks++;
      assert.ok(!text.includes('alive (idle)'), fixture.name + ': no unsupported alive claim'); checks++;
      const advertRow = body.locator('tr').filter({ hasText: 'Last Heard (advert)' });
      assert.match(await advertRow.innerText(), fixture.advert ? (fixture.active ? /\d+m ago/ : /4d ago/) : /—/); checks++;
      await page.goto(origin + '/#/nodes', { waitUntil: 'domcontentloaded' });
      await page.reload({ waitUntil: 'domcontentloaded' });
      await page.locator('tr[data-key="' + fixture.key + '"]').click();
      const pane = page.locator('#nodesRight');
      await pane.locator('dt').filter({ hasText: 'Last Heard (advert)' }).waitFor();
      assert.match(await pane.innerText(), fixture.active ? /Active/ : /Stale/, fixture.name + ': sidepane status'); checks++;
      const advertValue = pane.locator('dt').filter({ hasText: 'Last Heard (advert)' }).locator('xpath=following-sibling::dd[1]');
      assert.match(await advertValue.innerText(), fixture.advert ? (fixture.active ? /\d+m ago/ : /4d ago/) : /—/); checks++;
      if (process.env.SCREENSHOT_DIR) await page.screenshot({ path: path.join(process.env.SCREENSHOT_DIR, 'node-liveness-' + fixture.key.slice(0, 2) + '.png') });
      console.log('PASS ' + fixture.name);
    }
    console.log(checks + ' checks passed');
  } finally {
    if (browser) await browser.close();
    await new Promise(resolve => server.close(resolve));
  }
})().catch(error => { console.error(error); process.exitCode = 1; });
