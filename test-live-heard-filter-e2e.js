/**
 * E2E: Live map "Last heard" filter (#660).
 *   1. #/live?heard=6h restores the select, hides an old repeater, keeps a
 *      fresh repeater and an old companion (role scope), and saves 6h.
 *   2. A live WS flood packet whose resolved_path names the old repeater
 *      brings it back without a reload.
 *   3. VCR: the filter is suspended outside LIVE mode and re-applies on return.
 *   4. Changing the select rewrites the hash (All drops the param) and the
 *      saved value; a reload restores it from the hash, and a bare #/live
 *      restores it from localStorage.
 *   5. A select change re-uses the existing marker objects and makes no
 *      /api/nodes request (no rebuild, no new API call).
 *   6. Live traffic reviving two hidden repeaters re-evaluates the filter
 *      once (coalesced), not once per packet.
 *   7. With the clock advanced, a repeater ages out on the shared 60 s prune
 *      tick, with no reload.
 *
 * /api/nodes is served by request interception so ages are exact and do not
 * depend on fixture freshening. Nodes sit at the hash viewport centre so
 * viewport culling never hides them.
 *
 * Usage: BASE_URL=http://localhost:13581 node test-live-heard-filter-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const KEY = 'live-heard-filter';
let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

const OLD_REP = 'e2e660' + 'a'.repeat(58);
const FRESH_REP = 'e2e660' + 'b'.repeat(58);
const OLD_COMP = 'e2e660' + 'c'.repeat(58);
const OLD_REP2 = 'e2e660' + 'd'.repeat(58);
const AGING_REP = 'e2e660' + 'e'.repeat(58);
const VIEW = 'lat=50.5&lon=10.5&zoom=11';

let nodesRequests = 0;
function nodesBody(nowMs) {
  const ago = (h) => new Date(nowMs - h * 3600000).toISOString();
  const node = (pk, name, role, lat, lon, seen) => ({
    public_key: pk, name, role, lat, lon, last_seen: seen, last_heard: seen,
    first_seen: ago(500), advert_count: 3, foreign: false,
  });
  const nodes = [
    node(OLD_REP, 'E2E-660 old repeater', 'repeater', 50.5, 10.5, ago(10)),
    node(FRESH_REP, 'E2E-660 fresh repeater', 'repeater', 50.505, 10.505, ago(0.2)),
    node(OLD_COMP, 'E2E-660 old companion', 'companion', 50.495, 10.495, ago(10)),
    node(OLD_REP2, 'E2E-660 old repeater 2', 'room', 50.51, 10.49, ago(10)),
    node(AGING_REP, 'E2E-660 aging repeater', 'repeater', 50.49, 10.51, ago(55 / 60)),
  ];
  return JSON.stringify({ nodes, total: nodes.length, counts: {} });
}

// clockNow: when set, the page clock is installed at that instant and the
// node ages are computed against it.
async function newPage(browser, errors, clockNow) {
  const ctx = await browser.newContext({ viewport: { width: 1400, height: 900 } });
  const page = await ctx.newPage();
  page.on('pageerror', (e) => errors.push('pageerror: ' + e.message));
  if (clockNow) await page.clock.install({ time: clockNow });
  await page.route(/\/api\/nodes\?/, (route) => {
    nodesRequests++;
    return route.fulfill({ status: 200, contentType: 'application/json', body: nodesBody(clockNow || Date.now()) });
  });
  return { ctx, page };
}

async function gotoLive(page, query) {
  await page.goto(BASE + '/?r=' + Date.now() + '#/live?' + query, { waitUntil: 'domcontentloaded' });
  await page.waitForFunction((k) => {
    const m = window._liveNodeMarkers && window._liveNodeMarkers();
    return !!(window._liveWSHandler && window._liveWSHandler()) && m && m[k];
  }, OLD_REP, { timeout: 20000 });
}

// On the map = Leaflet attached the marker to a map (marker._map).
function onMap(page) {
  return page.evaluate((keys) => {
    const m = window._liveNodeMarkers();
    const out = {};
    for (const k of keys) out[k] = !!(m[k] && m[k]._map);
    return out;
  }, [OLD_REP, FRESH_REP, OLD_COMP, OLD_REP2, AGING_REP]);
}

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  const errors = [];
  const { ctx, page } = await newPage(browser, errors);
  console.log('\n=== live "Last heard" filter E2E against ' + BASE + ' ===');

  await step('hash heard=6h: select restored, old repeater hidden, fresh repeater and old companion stay', async () => {
    await gotoLive(page, 'heard=6h&' + VIEW);
    assert(await page.$eval('#liveHeardSelect', (s) => s.value) === '6h', 'select not restored from hash');
    const s = await onMap(page);
    assert(s[OLD_REP] === false, 'old repeater still on the map');
    assert(s[FRESH_REP] === true, 'fresh repeater missing');
    assert(s[OLD_COMP] === true, 'old companion hidden (out of scope)');
    const count = await page.$eval('#liveHeardCount', (e) => e.textContent);
    assert(s[OLD_REP2] === false, 'old room still on the map');
    assert(/2 hidden/.test(count), 'count shows "' + count + '"');
    assert(await page.evaluate((k) => localStorage.getItem(k), KEY) === '6h', 'deep link not saved');
  });

  await step('live flood packet relayed by the old repeater brings it back', async () => {
    await page.evaluate((pk) => window._liveBufferPacket({
      id: 660, hash: 'e2e660relay' + Date.now(), raw_hex: '1500', route_type: 1, payload_type: 5,
      path_json: '[]', resolved_path: [pk], observer_id: 'e2e-660', observer_name: 'e2e-660',
      timestamp: new Date().toISOString(), snr: 5, rssi: -90,
      decoded: { header: { payloadTypeName: 'GRP_TXT' }, payload: { text: 'x' }, path: { hops: [] } },
    }), OLD_REP);
    await page.waitForFunction((k) => !!window._liveNodeMarkers()[k]._map, OLD_REP, { timeout: 5000 });
    assert(/1 hidden/.test(await page.$eval('#liveHeardCount', (e) => e.textContent)), 'count not refreshed');
  });

  await step('a new node from a live ADVERT is visible under the filter', async () => {
    const pk = 'e2e660' + 'f'.repeat(58);
    await page.evaluate((k) => window._liveBufferPacket({
      id: 6609, hash: 'e2e660advert' + Date.now(), raw_hex: '1100', route_type: 1, payload_type: 4,
      path_json: '[]', observer_id: 'e2e-660', observer_name: 'e2e-660',
      timestamp: new Date().toISOString(), snr: 5, rssi: -90,
      decoded: { header: { payloadTypeName: 'ADVERT' },
        payload: { pubKey: k, name: 'E2E-660 new repeater', role: 'repeater', lat: 50.502, lon: 10.498 },
        path: { hops: [] } },
    }), pk);
    await page.waitForFunction((k) => { const m = window._liveNodeMarkers()[k]; return !!m; }, pk, { timeout: 5000 });
    assert(await page.evaluate((k) => !!window._liveNodeMarkers()[k]._map, pk), 'new advert node hidden');
  });

  await step('VCR: suspended outside LIVE, re-applied on return', async () => {
    await page.reload({ waitUntil: 'domcontentloaded' });
    await page.waitForFunction((k) => {
      const m = window._liveNodeMarkers && window._liveNodeMarkers();
      return !!(window._liveWSHandler && window._liveWSHandler()) && m && m[k];
    }, OLD_REP, { timeout: 20000 });
    assert((await onMap(page))[OLD_REP] === false, 'precondition: old repeater hidden after reload');
    await page.evaluate(() => window._liveVcrSetMode('PAUSED'));
    assert((await onMap(page))[OLD_REP] === true, 'old repeater not shown while paused');
    await page.evaluate(() => window._liveVcrSetMode('LIVE'));
    assert((await onMap(page))[OLD_REP] === false, 'filter not re-applied in LIVE');
    // Resume-live clears and reloads every marker without moving the map,
    // so viewport culling cannot mask a marker attached while hidden.
    await page.evaluate((k) => { window.__e2e660Before = window._liveNodeMarkers()[k]; }, OLD_REP);
    await page.evaluate(() => window._liveVcrPause());
    await page.evaluate(() => window._liveVcrResumeLive());
    await page.waitForFunction((k) => {
      const m = window._liveNodeMarkers()[k];
      return m && m !== window.__e2e660Before;
    }, OLD_REP, { timeout: 15000 });
    assert((await onMap(page))[OLD_REP] === false, 'old repeater attached after resume-live reload');
  });

  await step('select change rewrites hash + storage; All shows everything and drops the param', async () => {
    // The controls panel starts collapsed; open it the way a user does.
    if (!(await page.isVisible('#liveHeardSelect'))) await page.click('#liveControlsToggle');
    await page.selectOption('#liveHeardSelect', 'all');
    let s = await onMap(page);
    assert(s[OLD_REP] && s[FRESH_REP] && s[OLD_COMP], 'All did not show every node');
    assert(!/[?&]heard=/.test(await page.evaluate(() => location.hash)), 'All left heard= in the hash');
    assert(await page.evaluate((k) => localStorage.getItem(k), KEY) === 'all', 'All not saved');
    await page.selectOption('#liveHeardSelect', '1h');
    const hash = await page.evaluate(() => location.hash);
    assert(/[?&]heard=1h(&|$)/.test(hash), 'hash is ' + hash);
    assert(/lat=50\.5/.test(hash), 'other hash params were dropped: ' + hash);
    s = await onMap(page);
    assert(!s[OLD_REP] && s[FRESH_REP] && s[OLD_COMP], '1h applied wrongly');
  });

  await step('reload restores the filter from the hash', async () => {
    await page.reload({ waitUntil: 'domcontentloaded' });
    await page.waitForFunction((k) => {
      const m = window._liveNodeMarkers && window._liveNodeMarkers();
      return !!(window._liveWSHandler && window._liveWSHandler()) && m && m[k];
    }, OLD_REP, { timeout: 20000 });
    assert(await page.$eval('#liveHeardSelect', (s) => s.value) === '1h', 'select not restored after reload');
    assert((await onMap(page))[OLD_REP] === false, 'old repeater visible after reload');
  });

  await step('bare #/live restores the saved value from localStorage', async () => {
    await gotoLive(page, VIEW);
    assert(await page.$eval('#liveHeardSelect', (s) => s.value) === '1h', 'saved value not restored');
    assert((await onMap(page))[OLD_REP] === false, 'saved filter not applied');
  });

  await step('select change keeps the marker objects and makes no /api/nodes request', async () => {
    await page.evaluate(() => {
      const m = window._liveNodeMarkers();
      window.__e2e660Markers = Object.assign({}, m);
    });
    const before = nodesRequests;
    for (const v of ['all', '24h', '3h', '1h']) await page.selectOption('#liveHeardSelect', v);
    assert(nodesRequests === before, (nodesRequests - before) + ' /api/nodes request(s) on select change');
    const same = await page.evaluate(() => {
      const m = window._liveNodeMarkers(), was = window.__e2e660Markers;
      return Object.keys(was).length === Object.keys(m).length && Object.keys(was).every((k) => m[k] === was[k]);
    });
    assert(same, 'markers were rebuilt');
  });

  await step('live traffic reviving two hidden nodes re-evaluates once (coalesced)', async () => {
    assert((await onMap(page))[OLD_REP] === false && (await onMap(page))[OLD_REP2] === false, 'precondition');
    const updates = await page.evaluate((keys) => new Promise((resolve) => {
      const el = document.getElementById('liveHeardCount');
      let n = 0;
      // Count records, not callbacks: synchronous writes share one callback.
      const mo = new MutationObserver((recs) => { n += recs.length; });
      mo.observe(el, { childList: true, characterData: true, subtree: true });
      keys.forEach((pk, i) => window._liveBufferPacket({
        id: 6600 + i, hash: 'e2e660burst' + i + Date.now(), raw_hex: '1500', route_type: 0, payload_type: 4,
        path_json: '[]', resolved_path: [pk], observer_id: 'e2e-660', observer_name: 'e2e-660',
        timestamp: new Date().toISOString(), snr: 5, rssi: -90,
        decoded: { header: { payloadTypeName: 'GRP_TXT' }, payload: { text: 'x' }, path: { hops: [] } },
      }));
      setTimeout(() => { mo.disconnect(); resolve(n); }, 1000);
    }), [OLD_REP, OLD_REP2]);
    const s = await onMap(page);
    assert(s[OLD_REP] && s[OLD_REP2], 'hidden nodes not revived');
    assert(updates === 1, 'filter re-evaluated ' + updates + ' times for one burst');
  });

  await step('a repeater ages out on the shared 60 s tick, with no reload', async () => {
    const t0 = Date.now();
    const c = await newPage(browser, errors, t0);
    try {
      await gotoLive(c.page, 'heard=1h&' + VIEW);
      assert((await onMap(c.page))[AGING_REP] === true, 'precondition: 55-min-old repeater visible at 1h');
      const reqs = nodesRequests;
      await c.page.clock.runFor(6 * 60000);
      assert((await onMap(c.page))[AGING_REP] === false, 'repeater did not age out after 6 min');
      assert(nodesRequests === reqs, 'aging out triggered an /api/nodes request');
    } finally { await c.ctx.close(); }
  });

  await step('a conflicting hash and stored value: the hash wins', async () => {
    const c = await newPage(browser, errors);
    try {
      // Stored 1h would hide the 10 h old repeater; the deep link says 24h.
      await c.ctx.addInitScript((k) => { try { localStorage.setItem(k, '1h'); } catch (_) {} }, KEY);
      await gotoLive(c.page, 'heard=24h&' + VIEW);
      assert(await c.page.$eval('#liveHeardSelect', (e) => e.value) === '24h', 'stored value beat the hash');
      assert((await onMap(c.page))[OLD_REP] === true, 'old repeater hidden: storage won over the hash');
      assert(await c.page.evaluate((k) => localStorage.getItem(k), KEY) === '24h', 'deep link not saved over the stored value');
    } finally { await c.ctx.close(); }
  });

  await step('the advert-lag caveat is in the aria-describedby text, not only the title', async () => {
    const d = await page.$eval('#liveHeardSelect', (e) => {
      const el = document.getElementById(e.getAttribute('aria-describedby'));
      return el ? el.textContent : null;
    });
    assert(d !== null, 'aria-describedby target missing');
    assert(/advert/i.test(d) && /interval/i.test(d) && /longer/i.test(d), 'describedby text lacks the advert-lag caveat: "' + d + '"');
  });

  await step('page still works when live-heard-filter.js fails to load', async () => {
    const errs = [];
    const c = await newPage(browser, errs);
    try {
      await c.page.route(/\/live-heard-filter\.js/, (route) => route.abort());
      await c.page.goto(BASE + '/?r=' + Date.now() + '#/live?heard=6h&' + VIEW, { waitUntil: 'domcontentloaded' });
      await c.page.waitForFunction((k) => {
        const m = window._liveNodeMarkers && window._liveNodeMarkers();
        return !!(window._liveWSHandler && window._liveWSHandler()) && m && m[k];
      }, OLD_REP, { timeout: 20000 });
      const s = await onMap(c.page);
      assert(s[OLD_REP] && s[FRESH_REP] && s[OLD_COMP], 'markers missing without the module: ' + JSON.stringify(s));
      assert(await c.page.$('.leaflet-container'), 'no Leaflet container');
      assert(!(await c.page.isVisible('#liveHeardLabel')), 'filter control still visible without its module');
      await c.page.evaluate((pk) => window._liveBufferPacket({
        id: 661, hash: 'e2e660nomod' + Date.now(), raw_hex: '1500', route_type: 1, payload_type: 5,
        path_json: '[]', resolved_path: [pk], observer_id: 'e2e-660', timestamp: new Date().toISOString(),
        decoded: { header: { payloadTypeName: 'GRP_TXT' }, payload: { text: 'x' }, path: { hops: [] } },
      }), OLD_REP);
      assert(errs.length === 0, errs.join('\n    '));
    } finally { await c.ctx.close(); }
  });

  await step('no page errors', async () => {
    assert(errors.length === 0, errors.join('\n    '));
  });

  await ctx.close();
  await browser.close();
  console.log('\n--- ' + passed + ' passed, ' + failed + ' failed ---\n');
  process.exit(failed > 0 ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
