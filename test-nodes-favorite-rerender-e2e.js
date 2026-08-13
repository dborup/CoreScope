// test-nodes-favorite-rerender-e2e.js
//
// Self-contained real-browser regression test (no BASE_URL / live server
// needed, unlike the other -e2e.js files) for a reviewfix on top of #1889
// (commit 9a9103b2):
//
//   Root cause: bindFavStars(tbody) (public/nodes.js's renderRows()) never
//   passed an onToggle callback, so a favorite click updated localStorage
//   and the star icon but never re-rendered the table. computeDisplayOrder
//   (nodes.js) -- the single order helper both renderRows() and the JSON
//   export call -- reads the favorites list live, so the export would
//   immediately reflect a newly-favorited node's position while the DOM
//   table kept showing the OLD order until some unrelated re-render (a
//   sort click, a filter change) happened to catch up. That's a real
//   WYSIWYG violation: export and screen can visibly disagree.
//
//   Fix: bindFavStars(tbody, function () { renderRows(); }) -- the same
//   click that changes the favorite is now also the trigger that reorders
//   the table.
//
// This test loads the REAL public/app.js (for the real bindFavStars,
// getFavorites, toggleFavorite) and the REAL public/nodes.js (for the real
// renderRows/computeDisplayOrder) into a blank Chromium page, with minimal
// stubs for only the non-DOM globals they need (network/API, other page
// modules) -- document, querySelectorAll, addEventListener, click, focus,
// closest etc. are the browser's own real implementations. It drives an
// actual click on an actual rendered <button class="fav-star">, not a
// direct call to computeDisplayOrder() in isolation and not a source-regex
// check.
'use strict';
const { chromium } = require('playwright');
const http = require('http');
const path = require('path');
const crypto = require('crypto');

let passed = 0, failed = 0;
async function test(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}

const APP_JS = path.join(__dirname, 'public', 'app.js');
const NODES_JS = path.join(__dirname, 'public', 'nodes.js');
const NODES_EXPORT_JS = path.join(__dirname, 'public', 'nodes-export.js');

// nodes.js reads localStorage at top-level (e.g. the last-heard filter
// default), and localStorage throws a SecurityError on the opaque origin
// of about:blank/data: pages -- so this needs a real http(s) origin, not
// page.setContent() on a blank page. A trivial local static server gives
// one without any live CoreScope server or network access.
function startServer() {
  return new Promise((resolve) => {
    const server = http.createServer((req, res) => {
      res.writeHead(200, { 'Content-Type': 'text/html' });
      res.end('<!DOCTYPE html><html><body><table><tbody id="nodesBody"></tbody></table></body></html>');
    });
    server.listen(0, '127.0.0.1', () => resolve(server));
  });
}

async function makePage(browser, baseUrl) {
  // A fresh context per test -- not just a fresh page -- so localStorage
  // (favorites, claimed nodes) never leaks between test cases.
  const context = await browser.newContext();
  const page = await context.newPage();
  const requests = [];
  // Only /api/* requests count as "the click triggered a network call" --
  // the row template also references a static SVG icon sprite
  // (phosphor-sprite.svg) via <use href="...">, which browsers fetch as a
  // native side effect of rendering ANY row with a star/badge icon,
  // regardless of this fix. That's a decorative static asset, not
  // application data traffic, and would fire on the very first render too.
  page.on('request', req => { if (/\/api\//.test(req.url())) requests.push(req.url()); });

  await page.goto(baseUrl);

  // Minimal non-DOM stubs. Everything DOM-related is the browser's own
  // real engine -- nothing about querySelectorAll/addEventListener/click/
  // focus/closest is faked here.
  await page.evaluate(() => {
    window.api = function () { return Promise.resolve({}); };
    window.invalidateApiCache = function () {};
    window.nodePassesGeoFilter = function () { return true; };
    window.MC_GEO_FILTER = null;
    window.ROLE_COLORS = { repeater: '#111', room: '#222', companion: '#333', sensor: '#444' };
    window.ROLE_STYLE = {};
    window.TYPE_COLORS = {};
    window.getNodeStatus = function () { return 'active'; };
    window.getHealthThresholds = function () { return { staleMs: 1, degradedMs: 1, silentMs: 1 }; };
    window.timeAgo = function () { return ''; };
    window.debounce = function (fn) { return fn; };
    window.initTabBar = function () {};
    window.makeColumnsResizable = function () {};
    window.CLIENT_TTL = { nodeList: 0, nodeDetail: 0, nodeHealth: 0 };
    window.RegionFilter = { init(){}, onChange(){ return () => {}; }, offChange(){}, getRegionParam(){ return ''; } };
    window.AreaFilter = { init(){}, onChange(){ return () => {}; }, offChange(){}, getAreaParam(){ return ''; }, getSelected(){ return null; } };
    window.getFleetSkew = function () { return Promise.resolve({}); };
    window.onWS = function () {};
    window.offWS = function () {};
    window.debouncedOnWS = function () { return function () {}; };
    window.getHashParams = function () { return new URLSearchParams(); };
    window.registerPage = function () {};
  });

  // Real app.js (bindFavStars, getFavorites, toggleFavorite, favStar,
  // escapeHtml, truncate, ...), real nodes.js (renderRows,
  // computeDisplayOrder), real nodes-export.js (NodesExport).
  await page.addScriptTag({ path: APP_JS });
  await page.addScriptTag({ path: NODES_JS });
  await page.addScriptTag({ path: NODES_EXPORT_JS });

  return { page, context, requests };
}

// A real, human-readable id like "bravo" is not valid hex (r/v/o aren't
// hex digits) -- nodes-export.js's strict /^[0-9a-fA-F]{64}$/ validation
// (by design, see #1889 review fix 3) correctly rejects it, so padding
// the id directly would silently make every export empty and mask real
// bugs behind a fixture bug. Hash the id into a genuine 64-hex pubkey
// instead; `name` stays human-readable for assertions/debugging.
function node(id, patch) {
  return Object.assign({
    public_key: crypto.createHash('sha256').update(id).digest('hex'),
    name: id,
    role: 'repeater',
    lat: 55.0, lon: 10.0,
    last_seen: '2026-01-01T00:00:00Z',
    advert_count: 1,
  }, patch || {});
}

async function domOrder(page) {
  return page.$$eval('#nodesBody tr[data-key]', rows => rows.map(r => r.getAttribute('data-key')));
}

async function seedAndRender(page, fixture, sortState) {
  await page.evaluate(function (args) {
    window._nodesSetFiltered(args.fixture);
    window._nodesSetSortState(args.sortState);
    window._nodesRenderRows();
  }, { fixture: fixture, sortState: sortState || { column: 'name', direction: 'asc' } });
}

(async () => {
  const server = await startServer();
  const baseUrl = 'http://127.0.0.1:' + server.address().port + '/';
  const browser = await chromium.launch();

  await test('clicking a favorite star re-renders the table immediately, without any sort/filter change', async () => {
    const { page, context, requests } = await makePage(browser, baseUrl);
    const alpha = node('alpha', { name: 'Alpha' });
    const bravo = node('bravo', { name: 'Bravo' });
    await seedAndRender(page, [alpha, bravo]);

    const before = await domOrder(page);
    if (before.join(',') !== [alpha.public_key, bravo.public_key].join(',')) {
      throw new Error('unexpected initial DOM order: ' + before.join(','));
    }

    // Watch for recursive re-render: count actual tbody childList mutation
    // batches triggered by the click (not function-call instrumentation,
    // which the closure-captured onToggle callback would bypass).
    await page.evaluate(() => {
      window.__mutationBatches = 0;
      const mo = new MutationObserver(() => { window.__mutationBatches++; });
      mo.observe(document.getElementById('nodesBody'), { childList: true });
      window.__mo = mo;
    });

    const requestCountBefore = requests.length;

    // Real click on the real rendered star button -- no test-driven
    // renderRows()/sort/filter call follows this.
    await page.click('button[data-fav="' + bravo.public_key + '"]');
    await page.waitForTimeout(50); // let the MutationObserver microtask flush

    const after = await domOrder(page);
    if (after.join(',') !== [bravo.public_key, alpha.public_key].join(',')) {
      throw new Error('DOM did not reorder after the favorite click (stale table), got: ' + after.join(','));
    }

    const mutationBatches = await page.evaluate(() => window.__mutationBatches);
    if (mutationBatches !== 1) {
      throw new Error('expected exactly 1 tbody re-render from the click (no recursive re-render), got ' + mutationBatches);
    }

    if (requests.length !== requestCountBefore) {
      throw new Error('favorite click triggered ' + (requests.length - requestCountBefore) +
        ' unexpected network request(s): ' + requests.slice(requestCountBefore).join(','));
    }

    const computed = await page.evaluate(() => {
      const nodes = window._nodesGetFiltered();
      return window._nodesComputeDisplayOrder(nodes.slice()).map(n => n.public_key);
    });
    if (JSON.stringify(computed) !== JSON.stringify(after)) {
      throw new Error('DOM order does not match computeDisplayOrder(): ' + JSON.stringify({ computed, dom: after }));
    }

    const exportOrder = await page.evaluate(() => {
      const nodes = window._nodesGetFiltered();
      const ordered = window._nodesComputeDisplayOrder(nodes.slice());
      return window.NodesExport.buildContacts(ordered).contacts.map(c => c.public_key);
    });
    if (JSON.stringify(exportOrder) !== JSON.stringify(after)) {
      throw new Error('export order does not match DOM order immediately after the click (WYSIWYG violated): ' +
        JSON.stringify({ exportOrder, dom: after }));
    }

    await context.close();
  });

  await test('un-favoriting moves the node back to its normal sorted place immediately', async () => {
    const { page, context } = await makePage(browser, baseUrl);
    const alpha = node('alpha', { name: 'Alpha' });
    const bravo = node('bravo', { name: 'Bravo' });
    await seedAndRender(page, [alpha, bravo]);

    // Favorite Bravo (-> leads), then un-favorite it (-> back to normal
    // alphabetic position), both via real clicks on the real button.
    await page.click('button[data-fav="' + bravo.public_key + '"]');
    let order = await domOrder(page);
    if (order[0] !== bravo.public_key) throw new Error('precondition failed: favoriting should have moved Bravo first, got ' + order.join(','));

    // The button is a fresh DOM node after the re-render -- re-query it.
    await page.click('button[data-fav="' + bravo.public_key + '"]');
    order = await domOrder(page);
    if (order.join(',') !== [alpha.public_key, bravo.public_key].join(',')) {
      throw new Error('un-favoriting did not restore normal sorted order immediately, got: ' + order.join(','));
    }

    await context.close();
  });

  await test('a claimed node stays ahead of a favorited (but not claimed) node', async () => {
    const { page, context } = await makePage(browser, baseUrl);
    const alpha = node('alpha', { name: 'Alpha' });
    const bravo = node('bravo', { name: 'Bravo' });
    const charlie = node('charlie', { name: 'Charlie' });
    await page.evaluate(pubkey => {
      localStorage.setItem('meshcore-my-nodes', JSON.stringify([{ pubkey }]));
    }, charlie.public_key);
    await seedAndRender(page, [alpha, bravo, charlie]);

    const before = await domOrder(page);
    if (before[0] !== charlie.public_key) {
      throw new Error('precondition failed: claimed Charlie should lead before any favorite, got ' + before.join(','));
    }

    await page.click('button[data-fav="' + bravo.public_key + '"]');
    const after = await domOrder(page);
    if (after.join(',') !== [charlie.public_key, bravo.public_key, alpha.public_key].join(',')) {
      throw new Error('claimed node must stay ahead of the newly-favorited node, got: ' + after.join(','));
    }

    await context.close();
  });

  await test('focus is restored to the favorited row after the immediate re-render (existing #1616 mechanism)', async () => {
    const { page, context } = await makePage(browser, baseUrl);
    const alpha = node('alpha', { name: 'Alpha' });
    const bravo = node('bravo', { name: 'Bravo' });
    await seedAndRender(page, [alpha, bravo]);

    await page.click('button[data-fav="' + bravo.public_key + '"]');

    const focused = await page.evaluate(() => {
      const el = document.activeElement;
      return { tag: el && el.tagName, dataKey: el && el.getAttribute && el.getAttribute('data-key') };
    });
    if (focused.tag !== 'TR' || focused.dataKey !== bravo.public_key) {
      throw new Error('focus was not restored to the favorited row after the immediate re-render: ' + JSON.stringify(focused));
    }

    await context.close();
  });

  await browser.close();
  await new Promise((resolve) => server.close(resolve));

  console.log('\n' + passed + ' passed, ' + failed + ' failed');
  process.exit(failed ? 1 : 0);
})();
