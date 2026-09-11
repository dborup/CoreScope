/**
 * E2E regression for #1206 review must-fix (kent-beck #2):
 *   ResizeObserver leak in initVCRHeightTracker().
 *
 * SPA navigates to /#/live, then bounces /#/nodes ↔ /#/live ≥ 3 times.
 * Each /#/live mount re-runs initVCRHeightTracker(); without the cleanup
 * tear-down (or with a future regression that orphans cleanup) each visit
 * would accumulate another ResizeObserver against #vcrBar.
 *
 * We can't read live ResizeObserver instances directly — wrap the
 * constructor + .disconnect() via addInitScript so we can count
 * outstanding (constructed but not disconnected) observers and assert it
 * does NOT grow with each /live mount.
 *
 * Also exercises node-detail map disposal with a paused browser clock (port
 * of upstream #1970): navigation, pane closure and replacement must cancel
 * the old map's delayed resize, and a delayed detail response may only change
 * the view that requested it -- including A -> B -> A, after Escape, and after
 * leaving and returning to the page.
 *
 * Run: BASE_URL=http://localhost:13581 node test-issue-1206-resize-observer-leak-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

async function gotoHash(page, hash) {
  await page.evaluate((h) => { window.location.hash = h; }, hash);
  if (hash === '#/live') await waitForVCRTracker(page);
  else await page.locator('#nodesLeft[data-loaded="true"]').waitFor();
}

async function waitForVCRTracker(page) {
  // The tracker publishes this only after live's async node loading finishes.
  await page.locator('.live-page[style*="--vcr-bar-height"]').waitFor({ state: 'attached' });
}

// Synthetic nodes keep map lifecycle coverage independent of fixture locations.
// This fork draws no map for (0,0) (a "no GPS lock" sentinel) but does draw one
// for a neighbor-estimated position, so both cases are covered explicitly.
const mapNodes = [
  { public_key: 'a1'.repeat(32), name: 'Map fixture A', lat: 10, lon: 10 },
  { public_key: 'b2'.repeat(32), name: 'Map fixture B', lat: 11, lon: 11 },
  { public_key: 'c3'.repeat(32), name: 'No location fixture', lat: 0, lon: 0 },
  { public_key: 'd4'.repeat(32), name: 'Estimate-only fixture', lat: null, lon: null, estimated_lat: 12, estimated_lon: 12 },
].map((node) => ({ ...node, role: 'repeater', advert_count: 1 }));
const [nodeA, nodeB, nodeNoLoc, nodeEst] = mapNodes;
// A response for node A that arrives too late and must never reach the page.
const staleA = { ...nodeA, name: 'Map fixture A (stale response)', lat: 20, lon: 20 };

function stubNodesApi(path) {
  const node = mapNodes.find((n) => path === '/api/nodes/' + n.public_key);
  return path === '/api/nodes'
    ? { nodes: mapNodes, total: mapNodes.length, counts: { repeater: mapNodes.length } }
    : node ? { node, recentAdverts: [] }
    : path === '/api/nodes/clock-skew' ? []
    : /\/health$/.test(path) ? { stats: {}, observers: [], recentPackets: [] }
    : /\/neighbors$/.test(path) ? { neighbors: [] }
    : /\/paths$/.test(path) ? { paths: [] } : {};
}

function hasMap(node) {
  const hasLoc = node.lat != null && node.lon != null && !(node.lat === 0 && node.lon === 0);
  return hasLoc || (node.estimated_lat != null && node.estimated_lon != null);
}

function rowFor(node) {
  return '#nodesBody tr[data-key="' + node.public_key + '"]';
}

// Starts loading `node` in the side pane (row click) or the full view (hash).
async function requestDetail(page, view, node) {
  if (view === 'side') await page.locator(rowFor(node)).click();
  else await page.evaluate((key) => { location.hash = '#/nodes/' + key; }, node.public_key);
}

async function waitForDetail(page, view, node) {
  const root = view === 'side' ? '#nodesRight' : '#nodeFullBody';
  await page.locator(root + ' .node-detail-name').filter({ hasText: node.name }).waitFor();
  if (hasMap(node)) await page.locator(root + ' .leaflet-map-pane').waitFor({ state: 'attached' });
}

async function withNodeMaps(browser, fn, allowedErrors = []) {
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 800 } });
  const page = await ctx.newPage();
  page.setDefaultTimeout(8000);
  const errors = [];
  page.on('pageerror', (error) => errors.push(error.message));
  try {
    await page.route('**/api/nodes**', (route) => {
      const path = new URL(route.request().url()).pathname;
      return route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(stubNodesApi(path)) });
    });
    await page.clock.install({ time: new Date('2026-01-01T00:00:00Z') });
    await page.goto(BASE + '/#/nodes', { waitUntil: 'domcontentloaded', timeout: 30000 });
    await page.locator(rowFor(nodeA)).waitFor();
    await page.clock.pauseAt(new Date('2026-01-01T00:02:00Z'));
    // Observe real Leaflet instances through public lifecycle APIs. No app hooks.
    await page.evaluate(() => {
      window.__nodeMaps = [];
      window.__nodeMapObjects = [];
      L.Map.addInitHook(function() {
        const container = this.getContainer();
        if (container.id !== 'nodeMap' && container.id !== 'nodeFullMap') return;
        const record = { removed: false, resizes: [] };
        window.__nodeMaps.push(record);
        window.__nodeMapObjects.push(this);
        this.on('unload', () => { record.removed = true; });
        const invalidateSize = this.invalidateSize;
        this.invalidateSize = function(...args) {
          record.resizes.push({ removed: record.removed, connected: container.isConnected });
          return invalidateSize.apply(this, args);
        };
      });
    });
    const open = async (view, node) => {
      await requestDetail(page, view, node);
      await waitForDetail(page, view, node);
    };
    await fn(page, open);
    const unexpectedErrors = errors.filter((error) => !allowedErrors.includes(error));
    assert(unexpectedErrors.length === 0, 'unexpected pageerror: ' + unexpectedErrors.join('; '));
  } finally {
    await ctx.close();
  }
}

async function advanceMapClock(page, ms) {
  // Playwright also reports exceptions from fake-timer callbacks via runFor.
  const error = await page.clock.runFor(ms).then(() => null, (error) => error);
  assert(!error, 'timer callback must not throw: ' + (error && error.message));
}

async function readMaps(page) {
  return page.evaluate(() => window.__nodeMaps.map((record, i) => {
    if (record.removed) return { ...record };
    const center = window.__nodeMapObjects[i].getCenter();
    return { ...record, center: [Math.round(center.lat), Math.round(center.lng)] };
  }));
}

async function viewText(page, view) {
  return page.evaluate((v) => {
    if (v === 'side') return (document.getElementById('nodesRight') || {}).textContent || '';
    const title = document.querySelector('.node-full-title');
    const body = document.getElementById('nodeFullBody');
    return (title ? title.textContent : '') + ' | ' + (body ? body.textContent : '');
  }, view);
}

// Holds the first request for `node` until released and then answers it with
// `firstResponse` (an HTTP status for a failure, otherwise a node payload).
// Later requests for the node are answered at once with `laterResponse`.
async function holdFirstDetail(page, node, firstResponse, laterResponse) {
  let release;
  const gate = new Promise((resolve) => { release = resolve; });
  let requests = 0;
  const path = '/api/nodes/' + node.public_key;
  await page.route('**' + path, async (route) => {
    const first = ++requests === 1;
    if (first) await gate;
    const response = first ? firstResponse : laterResponse;
    if (typeof response === 'number') {
      return route.fulfill({ status: response, contentType: 'application/json', body: '{}' });
    }
    return route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ node: response, recentAdverts: [] }) });
  });
  const requested = page.waitForRequest((request) => new URL(request.url()).pathname === path);
  return { requested, release: () => release() };
}

// Joins the held in-flight request for `node` so the test can wait until the
// page has handled its response. api() shares one in-flight request per path;
// with `detach` the held request is dropped from that table, so the next
// request for the same node is a separate response and the held one can
// arrive last.
async function trackHeldDetail(page, node, detach) {
  await page.evaluate(([key, detachHeld]) => {
    const path = '/nodes/' + key;
    window.__heldDetail = api(path).then(() => null, (error) => error.message);
    if (detachHeld) _inflight.delete(path);
  }, [node.public_key, detach]);
}

async function releaseHeldDetail(page, held) {
  held.release();
  let timeout;
  try {
    return await Promise.race([
      page.evaluate(() => window.__heldDetail),
      new Promise((_, reject) => {
        timeout = setTimeout(() => reject(new Error('held response was not handled')), 8000);
      }),
    ]);
  } finally {
    clearTimeout(timeout);
  }
}

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });

  console.log('\n=== #1206 ResizeObserver leak E2E against ' + BASE + ' ===');

  const ctx = await browser.newContext({ viewport: { width: 1280, height: 800 } });

  // Install ResizeObserver wrapper BEFORE any page script runs.
  await ctx.addInitScript(() => {
    var RealRO = window.ResizeObserver;
    if (typeof RealRO !== 'function') {
      window.__roOutstanding = 0;
      window.__roConstructed = 0;
      return;
    }
    window.__roConstructed = 0;
    window.__roOutstanding = 0;
    function WrappedRO(cb) {
      var inst = new RealRO(cb);
      window.__roConstructed++;
      window.__roOutstanding++;
      var realDisconnect = inst.disconnect.bind(inst);
      var disconnected = false;
      inst.disconnect = function() {
        if (!disconnected) {
          disconnected = true;
          window.__roOutstanding--;
        }
        return realDisconnect();
      };
      return inst;
    }
    WrappedRO.prototype = RealRO.prototype;
    window.ResizeObserver = WrappedRO;
  });

  const page = await ctx.newPage();
  page.setDefaultTimeout(8000);
  page.on('pageerror', (e) => console.error('[pageerror]', e.message));

  await step('initial /#/live mount constructs at most 1 VCR ResizeObserver', async () => {
    await page.goto(BASE + '/#/live', { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#vcrBar', { timeout: 8000 });
    await waitForVCRTracker(page);
    // Baseline snapshot — record outstanding right after first /live mount.
    const snap = await page.evaluate(() => ({
      outstanding: window.__roOutstanding,
      constructed: window.__roConstructed,
    }));
    assert(typeof snap.outstanding === 'number',
      'ResizeObserver wrapper not installed (snap=' + JSON.stringify(snap) + ')');
    // Stash the first-mount baseline on window for the next step.
    await page.evaluate((b) => { window.__roBaseline = b; }, snap);
  });

  await step('3 SPA round-trips /live<->/nodes do NOT grow outstanding observer count', async () => {
    for (let i = 0; i < 3; i++) {
      await gotoHash(page, '#/nodes');
      await gotoHash(page, '#/live');
      await page.waitForSelector('#vcrBar', { timeout: 8000 });
    }
    const after = await page.evaluate(() => ({
      outstanding: window.__roOutstanding,
      constructed: window.__roConstructed,
      baseline: window.__roBaseline,
    }));
    // The VCR tracker MUST clean its observer on destroy(). After N
    // remounts the outstanding count for VCR-tracking observers must not
    // exceed the baseline.  We can't isolate which observers are
    // ours, so we use the delta: 4 mounts * leak-of-1 = 3 extra
    // outstanding observers, which is the failure mode this test gates.
    var delta = after.outstanding - after.baseline.outstanding;
    assert(delta <= 0,
      'ResizeObserver leak: outstanding count grew by ' + delta +
      ' across 3 SPA round-trips (baseline=' + after.baseline.outstanding +
      ', after=' + after.outstanding + ', constructed=' + after.constructed +
      '). Expected delta <= 0.');
  });

  await ctx.close();

  for (const view of ['side', 'full']) {
    await step(view + ' node map: resizes once at its deadline while the view is active', () => withNodeMaps(browser, async (page, open) => {
      await open(view, nodeA);
      await advanceMapClock(page, 99);
      let maps = await readMaps(page);
      assert(maps.length === 1 && maps[0].resizes.length === 0, 'map must wait its full 100ms');
      await advanceMapClock(page, 1);
      maps = await readMaps(page);
      assert(maps[0].resizes.length === 1 && maps[0].resizes[0].connected && !maps[0].resizes[0].removed,
        'active map must resize once while connected');
    }));

    await step(view + ' node map: navigation cancels the pending resize', () => withNodeMaps(browser, async (page, open) => {
      await open(view, nodeA);
      await page.evaluate(() => { location.hash = '#/live'; });
      await page.locator('#vcrBar').waitFor();
      await advanceMapClock(page, 100);
      const maps = await readMaps(page);
      assert(maps.length === 1 && maps[0].removed, 'navigation must remove the node map');
      assert(maps[0].resizes.length === 0, 'removed map must not be resized');
    }));

    await step(view + ' node map: replacement only resizes at its own deadline', () => withNodeMaps(browser, async (page, open) => {
      await open(view, nodeA);
      await advanceMapClock(page, 50);
      await open(view, nodeB);
      await advanceMapClock(page, 50); // First map's deadline; replacement is only 50ms old.
      let maps = await readMaps(page);
      assert(maps.length === 2 && maps[0].removed && !maps[1].removed, 'replacement must own the surviving map');
      assert(maps[0].resizes.length === 0 && maps[1].resizes.length === 0, 'old timer must not resize either map');
      await advanceMapClock(page, 49);
      maps = await readMaps(page);
      assert(maps[1].resizes.length === 0, 'replacement must wait its full 100ms');
      await advanceMapClock(page, 1);
      maps = await readMaps(page);
      assert(maps[1].resizes.length === 1 && !maps[1].resizes[0].removed && maps[1].resizes[0].connected,
        'surviving map must resize once while connected');
    }));

    await step(view + ' node map: estimate-only replacement gets its own map and deadline', () => withNodeMaps(browser, async (page, open) => {
      await open(view, nodeA);
      await advanceMapClock(page, 50);
      await open(view, nodeEst);
      await advanceMapClock(page, 50);
      let maps = await readMaps(page);
      assert(maps.length === 2 && maps[0].removed && !maps[1].removed, 'estimate-only node must replace the map');
      assert(maps[1].center[0] === 12 && maps[1].center[1] === 12, 'estimate-only map must show the estimated position');
      assert(maps[0].resizes.length === 0 && maps[1].resizes.length === 0, 'old timer must not resize either map');
      await advanceMapClock(page, 50);
      maps = await readMaps(page);
      assert(maps[1].resizes.length === 1 && maps[1].resizes[0].connected, 'estimate-only map must resize at its own deadline');
    }));
  }

  for (const close of ['button', 'Escape']) {
    await step('side node map: ' + close + ' disposes the map before resize', () => withNodeMaps(browser, async (page, open) => {
      await open('side', nodeA);
      if (close === 'button') await page.locator('#nodesRight .panel-close-btn').click();
      else await page.keyboard.press('Escape');
      await page.locator('#nodesRight.empty').waitFor();
      await advanceMapClock(page, 100);
      const maps = await readMaps(page);
      assert(maps.length === 1 && maps[0].removed, 'closing the pane must remove its map');
      assert(maps[0].resizes.length === 0, 'closed pane map must not be resized');
    }));
  }

  await step('side node map: (0,0) no-location replacement disposes the pending map', () => withNodeMaps(browser, async (page, open) => {
    await open('side', nodeA);
    await open('side', nodeNoLoc);
    await advanceMapClock(page, 100);
    const maps = await readMaps(page);
    assert(maps.length === 1 && maps[0].removed, 'no-location replacement must remove the previous map');
    assert(maps[0].resizes.length === 0, 'no-location replacement must cancel the old resize');
    assert(await page.locator('#nodeMap').count() === 0, '(0,0) node must not create a map');
  }));

  await step('side node map: repeated navigation does not accumulate resize timers', () => withNodeMaps(browser, async (page, open) => {
    for (let i = 0; i < 3; i++) {
      await open('side', i % 2 ? nodeB : nodeA);
      await page.evaluate(() => { location.hash = '#/tools'; });
      await page.locator('.tools-landing').waitFor();
      await page.evaluate(() => { location.hash = '#/nodes'; });
      await page.locator('#nodesLeft[data-loaded="true"]').waitFor();
    }
    await advanceMapClock(page, 100);
    let maps = await readMaps(page);
    assert(maps.length === 3 && maps.every((m) => m.removed), 'every visit must dispose its map');
    assert(maps.every((m) => m.resizes.length === 0), 'no disposed map may be resized');
    await open('side', nodeA);
    await advanceMapClock(page, 99);
    maps = await readMaps(page);
    assert(maps.length === 4 && maps[3].resizes.length === 0, 'new map must wait its full 100ms');
    await advanceMapClock(page, 1);
    maps = await readMaps(page);
    assert(maps[3].resizes.length === 1 && maps[3].resizes[0].connected, 'new map must resize exactly once');
  }));

  for (const scenario of [
    { view: 'full', fails: true },
    { view: 'full', fails: false },
    { view: 'side', fails: true },
    { view: 'side', fails: false },
  ]) {
    const expectedError = scenario.fails ? 'API 500: /nodes/' + nodeA.public_key : null;
    await step(scenario.view + ' node map: stale ' + (scenario.fails ? 'failure' : 'no-location response') + ' preserves the replacement',
      () => withNodeMaps(browser, async (page, open) => {
        const held = await holdFirstDetail(page, nodeA, scenario.fails ? 500 : { ...nodeA, lat: null, lon: null }, nodeA);
        await requestDetail(page, scenario.view, nodeA);
        await held.requested;
        await trackHeldDetail(page, nodeA, false);
        await open(scenario.view, nodeB);
        await advanceMapClock(page, 50);
        const result = await releaseHeldDetail(page, held);
        assert(result === expectedError, 'held response must complete with the intended result');
        let maps = await readMaps(page);
        assert(maps.length === 1 && !maps[0].removed, 'stale response must not remove the replacement map');
        assert(maps[0].resizes.length === 0, 'replacement must wait for its own resize deadline');
        await advanceMapClock(page, 50);
        maps = await readMaps(page);
        assert(maps[0].resizes.length === 1 && maps[0].resizes[0].connected && !maps[0].resizes[0].removed,
          'replacement must remain connected and receive its own resize');
      }, expectedError ? [expectedError] : []));
  }

  for (const view of ['side', 'full']) {
    await step(view + ' node map: stale located response keeps the replacement view and map', () => withNodeMaps(browser, async (page, open) => {
      const held = await holdFirstDetail(page, nodeA, staleA, nodeA);
      await requestDetail(page, view, nodeA);
      await held.requested;
      await trackHeldDetail(page, nodeA, false);
      await open(view, nodeB);
      await advanceMapClock(page, 50);
      assert(await releaseHeldDetail(page, held) === null, 'held response must succeed');
      const maps = await readMaps(page);
      assert(maps.length === 1 && !maps[0].removed, 'stale response must not replace the map');
      assert(maps[0].center[0] === 11 && maps[0].center[1] === 11, 'map must stay on the replacement node');
      const text = await viewText(page, view);
      assert(text.includes(nodeB.name) && !text.includes(staleA.name), 'stale response must not change the view: ' + text.slice(0, 120));
    }));
  }

  await step('side node map: A -> B -> A sharing the first A request renders A once', () => withNodeMaps(browser, async (page, open) => {
    const held = await holdFirstDetail(page, nodeA, nodeA, nodeA);
    await requestDetail(page, 'side', nodeA);
    await held.requested;
    await trackHeldDetail(page, nodeA, false);
    await open('side', nodeB);
    await requestDetail(page, 'side', nodeA); // api() hands this call the held request
    await advanceMapClock(page, 50);
    assert(await releaseHeldDetail(page, held) === null, 'held response must succeed');
    let maps = await readMaps(page);
    assert(maps.length === 2, 'the shared A response must be rendered once (maps created: ' + maps.length + ')');
    assert(maps[0].removed && !maps[1].removed && maps[1].center[0] === 10, 'the A map must replace the B map');
    assert(maps.every((m) => m.resizes.length === 0), 'no map may be resized before its own deadline');
    await advanceMapClock(page, 99);
    maps = await readMaps(page);
    assert(maps[1].resizes.length === 0, 'the A map must wait its full 100ms');
    await advanceMapClock(page, 1);
    maps = await readMaps(page);
    assert(maps[1].resizes.length === 1 && maps[1].resizes[0].connected, 'the A map must resize exactly once');
  }));

  for (const view of ['side', 'full']) {
    for (const fails of [false, true]) {
      const expectedError = fails ? 'API 500: /nodes/' + nodeA.public_key : null;
      await step(view + ' node map: A -> B -> A ignores the first A ' + (fails ? 'failure' : 'response') + ' arriving last',
        () => withNodeMaps(browser, async (page, open) => {
          const held = await holdFirstDetail(page, nodeA, fails ? 500 : staleA, nodeA);
          await requestDetail(page, view, nodeA);
          await held.requested;
          await trackHeldDetail(page, nodeA, true);
          await open(view, nodeB);
          await open(view, nodeA);
          await advanceMapClock(page, 50);
          assert(await releaseHeldDetail(page, held) === expectedError, 'held response must complete with the intended result');
          let maps = await readMaps(page);
          assert(maps.length === 2 && maps[0].removed && !maps[1].removed,
            'late first A must not remove or replace the current map (maps: ' + JSON.stringify(maps) + ')');
          assert(maps[1].center[0] === 10 && maps[1].center[1] === 10, 'current map must stay on the current A response');
          const text = await viewText(page, view);
          assert(text.includes(nodeA.name) && !text.includes('(stale response)') && !text.includes('Error: API') && !text.includes('Failed to load node'),
            'late first A must not change the view: ' + text.slice(0, 120));
          assert(maps[1].resizes.length === 0, 'current map must wait for its own deadline');
          await advanceMapClock(page, 50);
          maps = await readMaps(page);
          assert(maps[1].resizes.length === 1 && maps[1].resizes[0].connected && !maps[1].resizes[0].removed,
            'current map must receive exactly its own resize');
        }, expectedError ? [expectedError] : []));
    }
  }

  await step('side node map: a response after Escape does not reopen the view', () => withNodeMaps(browser, async (page) => {
    const held = await holdFirstDetail(page, nodeA, nodeA, nodeA);
    await requestDetail(page, 'side', nodeA);
    await held.requested;
    await trackHeldDetail(page, nodeA, false);
    await page.keyboard.press('Escape');
    await page.locator('#nodesRight.empty').waitFor();
    assert(await releaseHeldDetail(page, held) === null, 'held response must succeed');
    await advanceMapClock(page, 100);
    const maps = await readMaps(page);
    assert(maps.length === 0, 'closed pane must not get a map from a late response');
    const text = await viewText(page, 'side');
    assert(!text.includes(nodeA.name), 'closed pane must stay closed: ' + text.slice(0, 120));
  }));

  for (const view of ['side', 'full']) {
    await step(view + ' node map: a response after leaving the page does not touch the returned view', () => withNodeMaps(browser, async (page, open) => {
      const held = await holdFirstDetail(page, nodeA, staleA, nodeA);
      await requestDetail(page, view, nodeA);
      await held.requested;
      await trackHeldDetail(page, nodeA, true);
      await page.evaluate(() => { location.hash = '#/tools'; });
      await page.locator('.tools-landing').waitFor();
      if (view === 'side') {
        await page.evaluate(() => { location.hash = '#/nodes'; });
        await page.locator('#nodesLeft[data-loaded="true"]').waitFor();
      }
      await open(view, nodeA);
      await advanceMapClock(page, 50);
      assert(await releaseHeldDetail(page, held) === null, 'held response must succeed');
      let maps = await readMaps(page);
      assert(maps.length === 1 && !maps[0].removed && maps[0].center[0] === 10 && maps[0].center[1] === 10,
        'late response must not remove or recreate the returned view\'s map (maps: ' + JSON.stringify(maps) + ')');
      const text = await viewText(page, view);
      assert(text.includes(nodeA.name) && !text.includes('(stale response)'), 'late response must not change the view: ' + text.slice(0, 120));
      assert(maps[0].resizes.length === 0, 'returned view\'s map must wait for its own deadline');
      await advanceMapClock(page, 50);
      maps = await readMaps(page);
      assert(maps[0].resizes.length === 1 && maps[0].resizes[0].connected, 'returned view\'s map must resize once');
    }));
  }

  await browser.close();
  console.log('\nSPA observer and node-map lifecycle: ' + passed + ' passed, ' + failed + ' failed');
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
