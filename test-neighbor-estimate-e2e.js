'use strict';
// Real SPA, DOM, route navigation and node detail rendering against a local
// synthetic API. Leaflet's API is spied (no CDN or tiles); markers are checked
// as calls, not claimed as a geographic/real-radio accuracy validation.
// Run: node test-neighbor-estimate-e2e.js; optional SCREENSHOT_DIR for evidence.
// Optional LEAFLET_ASSET_DIR points to an unpacked Leaflet dist directory
// (leaflet.js, leaflet.css, images/) for real-map validation without CDN access.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const http = require('node:http');
const path = require('node:path');
const { chromium } = require('playwright');

const publicDir = path.join(__dirname, 'public');
const leafletDir = process.env.LEAFLET_ASSET_DIR;
const recent = new Date().toISOString();
const estimate = {
  status: 'estimated', method: 'neighbor_cluster_v1', lat: 55.12345, lon: 12.65432,
  contributor_count: 3, candidate_count: 4, spread_km: 8.4,
  newest_seen: recent, oldest_seen: recent, unknown_freshness_count: 1,
  area: {kind:'polygon',vertices:[{lat:55.1,lon:12.5},{lat:55.2,lon:12.5},{lat:55.1,lon:12.7}]},
};
const cases = [
  { name: 'GPS and approximate area', gps: true, estimate, supported: true },
  { name: 'Approximate area only', gps: false, estimate, supported: true },
  { name: 'One neighbor', gps: false, estimate: { status: 'insufficient', contributor_count: 1, candidate_count: 1 }, text: /Insufficient neighbor evidence/ },
  { name: 'Competing clusters', gps: true, estimate: { status: 'ambiguous', contributor_count: 0, candidate_count: 4 }, text: /Conflicting neighbor groups/ },
  { name: 'Two-neighbor line', gps: false, estimate: {...estimate,area:{kind:'line',vertices:[{lat:55.1,lon:12.5},{lat:55.2,lon:12.6}]}},supported:true },
  { name: 'Dateline GPS and area', gps:true, gpsLat:10.03,gpsLon:-179.97,estimate:{...estimate,area:{kind:'polygon',vertices:[{lat:10,lon:179.95},{lat:10.05,lon:-179.95},{lat:10.1,lon:179.96}]}},supported:true },
  { name: 'Coincident neighbors', gps:false,estimate:{...estimate,area:null},text:/Insufficient geometry/ },
].map((fixture, index) => ({ ...fixture, key: (index + 1).toString(16).padStart(2, '0') + '22'.repeat(31) }));
const nodes = cases.map(fixture => ({
  public_key: fixture.key, name: fixture.name, role: 'repeater',
  lat: fixture.gps ? (fixture.gpsLat || 55.23456) : null, lon: fixture.gps ? (fixture.gpsLon || 12.34567) : null,
  first_seen: recent, last_seen: recent, advert_count: 1,
  neighbor_estimate: fixture.estimate,
  // Deliberately stale legacy coordinates: typed abstention must override them.
  estimated_lat: 54, estimated_lon: 11, estimated_contributor_count: 9,
  estimated_distance_km: 23.4,
}));
function json(res, data) {
  res.writeHead(200, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' });
  res.end(JSON.stringify(data));
}
function recordLeafletCalls() {
  // Wrap the real implementation without replacing maps, markers, or popups.
  for (const kind of ['map', 'marker', 'circleMarker', 'polyline', 'polygon']) {
    const original = window.L[kind];
    window.L[kind] = function (position, options) {
      const call = kind === 'map' ? { kind, id: position } : { kind, position, options };
      window.__mapCalls.push(call);
      const result = original.apply(this, arguments);
      if (kind !== 'map') {
        const bindPopup = result.bindPopup;
        result.bindPopup = function (html) { call.popup = html; return bindPopup.apply(this, arguments); };
      }
      return result;
    };
  }
}
const server = http.createServer((req, res) => {
  const pathname = new URL(req.url, 'http://127.0.0.1').pathname;
  if (leafletDir && pathname.startsWith('/__fixture_leaflet/')) {
    const relative = pathname.slice('/__fixture_leaflet/'.length);
    if (!['leaflet.js', 'leaflet.css', 'images/marker-icon.png', 'images/marker-icon-2x.png', 'images/marker-shadow.png'].includes(relative)) {
      res.writeHead(404); return res.end();
    }
    const file = path.join(leafletDir, relative);
    if (!fs.existsSync(file)) { res.writeHead(404); return res.end(); }
    res.writeHead(200, { 'Content-Type': relative.endsWith('.js') ? 'application/javascript' : relative.endsWith('.css') ? 'text/css' : 'image/png' });
    const content = fs.readFileSync(file);
    return res.end(relative === 'leaflet.js' ? content.toString() + '\n;(' + recordLeafletCalls.toString() + ')();' : content);
  }
  if (pathname === '/api/nodes') return json(res, { nodes, total: nodes.length, counts: { all: nodes.length, repeater: nodes.length } });
  const match = pathname.match(/^\/api\/nodes\/([^/]+)(?:\/(.*))?$/);
  if (match) {
    const node = nodes.find(n => n.public_key === match[1]);
    if (node) {
      if (match[2] === 'health') return json(res, { node, observers: [], recentPackets: [], stats: { lastAdvert: recent, lastHeard: recent } });
      if (match[2] === 'neighbors') return json(res, { neighbors: [] });
      if (match[2] === 'paths') return json(res, { paths: [], totalTransmissions: 0 });
      if (!match[2]) return json(res, { node, recentAdverts: [] });
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
  // All first-party scripts stay real; remove only CDN libraries and the
  // plugin that requires real Leaflet, since the test records that boundary.
  const body = fs.readFileSync(file);
  if (pathname !== '/') return res.end(body);
  let html = body.toString().replace(/<script\b[^>]*src="(?:https?:\/\/|vendor\/leaflet\.markercluster)[^>]*>[\s\S]*?<\/script>/g, '');
  if (leafletDir) html = html.replace('</head>', '<link rel="stylesheet" href="/__fixture_leaflet/leaflet.css"><script src="/__fixture_leaflet/leaflet.js"></script></head>');
  res.end(html);
});

(async () => {
  let browser, checks = 0;
  try {
    await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
    const origin = 'http://127.0.0.1:' + server.address().port;
    browser = await chromium.launch({ headless: true, executablePath: process.env.CHROMIUM_PATH || undefined });
    for (const fixture of cases) {
      for (const viewport of [{width:390,height:844},{width:768,height:1024},{width:1440,height:1000}]) {
        // Mobile opens full detail; narrow-tablet list selection is a separate
        // summary-only slide-over with no map. Test that viewport's full route.
        for (const mode of viewport.width <= 640 ? ['full', 'list-navigation'] : viewport.width < 1024 ? ['full'] : ['full', 'pane']) {
          const page = await browser.newPage({ viewport });
          const pageErrors = [];
          page.on('pageerror', error => pageErrors.push(error.message));
          page.setDefaultTimeout(10000);
          await page.route('**/*', route => new URL(route.request().url()).origin === origin ? route.continue() : route.abort());
          await page.addInitScript(realLeaflet => {
            window.__mapCalls = [];
            if (realLeaflet) return;
            const layer = kind => (position, options) => {
              const call = { kind, position, options };
              window.__mapCalls.push(call);
              return { addTo() { return this; }, bindPopup(html) { call.popup = html; return this; } };
            };
            window.L = {
              map(id) {
                window.__mapCalls.push({ kind: 'map', id });
                return { fitBounds() {}, setView() {}, invalidateSize() {}, remove() {}, getPane() { return document.getElementById(id); } };
              },
              tileLayer: () => ({ addTo() { return this; } }),
              marker: layer('marker'), circleMarker: layer('circleMarker'), polyline: layer('polyline'), polygon: layer('polygon'),
            };
          }, !!leafletDir);
          await page.goto(origin + (mode === 'full' ? '/#/nodes/' + fixture.key : '/#/nodes'), { waitUntil: 'domcontentloaded' });
          if (mode !== 'full') {
            await page.locator('tr[data-key="' + fixture.key + '"]').click();
          }
          const body = page.locator(mode === 'pane' ? '#nodesRight' : '#nodeFullBody');
          const row = body.locator('.neighbor-estimate');
          await row.waitFor();
          const text = await row.innerText();
          const calls = await page.evaluate(() => window.__mapCalls);
          const shapeKind = fixture.estimate.area?.kind === 'line' ? 'polyline' : 'polygon';
          const markers = calls.filter(call => call.kind === shapeKind);
          assert.equal(calls.filter(call => call.kind === 'circleMarker').length, 0, 'no estimated precise point'); checks++;
          assert.equal(markers.length, fixture.supported ? 1 : 0, fixture.name + ': estimated markers'); checks++;
          const gps = calls.filter(call => call.kind === 'marker');
          assert.equal(gps.length, fixture.gps ? 1 : 0, fixture.name + ': GPS markers'); checks++;
          if (leafletDir) {
            assert.equal(await body.locator('.leaflet-container').count(), fixture.gps || fixture.supported ? 1 : 0); checks++;
            assert.equal(await body.locator('.leaflet-marker-icon').count(), fixture.gps ? 1 : 0); checks++;
            assert.equal(await body.locator('path.leaflet-interactive').count(), fixture.supported ? 1 : 0); checks++;
          }
          if (fixture.gps) {
            if (fixture.gpsLon) {
              assert.ok(Math.abs(gps[0].position[1] - markers[0].position[0][1]) < 1, 'GPS+dateline geometry uses local bounds'); checks++;
              assert.match(await body.innerText(), /10\.03000, -179\.97000/); checks++;
            } else {
              assert.deepEqual(gps[0].position, [55.23456, 12.34567]); checks++;
              assert.match(await body.innerText(), /55\.23456, 12\.34567/); checks++;
            }
          }
          if (fixture.supported) {
            assert.match(text, /Neighbor evidence (area|line)/); checks++;
            assert.match(text, /node may be outside/); checks++;
            assert.doesNotMatch(text, /~55\.12|~12\.65/); checks++;
            assert.match(text, /3 of 4 candidate neighbors/); checks++;
            assert.match(text, /not a location error radius/); checks++;
            assert.match(text, /unknown freshness/); checks++;
            assert.match(text, /not triangulation/); checks++;
            assert.equal(markers[0].position.length, fixture.estimate.area.vertices.length); checks++;
            assert.match(markers[0].popup, /Neighbor evidence geometry/); checks++;
            assert.match(markers[0].popup, /not a location error radius/); checks++;
            if (fixture.gps) {
              assert.match(text, /23\.4 km from reported position \(not an error bound\)/); checks++;
            } else {
              assert.doesNotMatch(text, /from reported position/); checks++;
            }
          } else {
            assert.match(text, fixture.text); checks++;
            assert.doesNotMatch(text, /~54|~11|~55/); checks++;
          }
          assert.deepEqual(pageErrors, [], 'no browser JS errors'); checks++;
          if (process.env.SCREENSHOT_DIR) await page.screenshot({ path: path.join(process.env.SCREENSHOT_DIR, 'neighbor-estimate-' + fixture.key.slice(0, 2) + '-' + mode + '-' + viewport.width + '.png') });
          await page.close();
          console.log('PASS ' + fixture.name + ' (' + mode + ', ' + viewport.width + 'px)');
        }
      }
    }
    console.log(checks + ' checks passed');
  } finally {
    if (browser) await browser.close();
    await new Promise(resolve => server.close(resolve));
  }
})().catch(error => { console.error(error); process.exitCode = 1; });
