/**
 * E2E (#839 finding 1): leaving Live must leave no observer or listener of
 * Live's behind.
 *
 * Live's `data-theme` MutationObserver (_themeObs) was never disconnected
 * in destroy(), and every visit added another one. Since #728 (PR #735)
 * its callback swaps the base map's credit through
 * MC_setLayerAttribution(map, …), so after Live is left (map === null)
 * every theme toggle on any other page threw
 *   TypeError: Cannot read properties of null (reading 'attributionControl')
 * once per Live visit.
 *
 * The same sweep found three more that outlived Live:
 *   - the DragManager's `(pointer: fine) and (min-width: 768px)`
 *     MediaQueryList 'change' listener (onDragMediaChange), one per visit;
 *   - MC_createLayerControl's window 'mc-tile-provider-changed' listener,
 *     one per map, which rebuilt the control of a removed map;
 *   - a feed card's outside-click `document` listener when Live is left
 *     with the card open.
 *
 * Probe: an init script (before the SPA's scripts) counts, from a clean
 * baseline,
 *   - MutationObservers observing <html> for data-theme whose observe()
 *     was called from live.js (net of disconnect());
 *   - MediaQueryList 'change' listeners by the handler's name;
 *   - window 'mc-tile-provider-changed' listeners and document 'click'
 *     listeners by the handler's name.
 * The document never reloads on hash-only navigation, so the counts
 * survive SPA routing.
 *
 * Flow, from a cold start with real clicks at 1440x900: Home → "I know
 * what I'm doing" → Live (settled) → Packets → dark-mode toggle, then a
 * second Live visit and both toggles again.
 *
 * Usage: BASE_URL=http://localhost:13581 node test-issue-839-live-teardown-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:3000';

let passed = 0, failed = 0;
async function test(name, fn) {
  try { await fn(); passed++; console.log('  ✅ ' + name); }
  catch (err) { failed++; console.log('  ❌ ' + name + ': ' + err.message); }
}
function assert(cond, msg) { if (!cond) throw new Error(msg || 'assertion failed'); }

let browser;

async function coldStart() {
  const ctx = await browser.newContext({ viewport: { width: 1440, height: 900 } });
  const page = await ctx.newPage();
  page.__errors = [];
  page.on('pageerror', (e) => page.__errors.push(String(e && e.stack || e)));
  await page.addInitScript(() => {
    const probe = window.__839 = { liveThemeObs: 0, ctlThemeObs: 0, mql: {}, tile: {}, docClick: {} };
    const OrigMO = window.MutationObserver;
    window.MutationObserver = class extends OrigMO {
      observe(target, opts) {
        const filt = opts && opts.attributeFilter;
        const stack = new Error().stack || '';
        // #845: the layer control (map-tile-providers.js, built from
        // live.js) watches the theme too; it is counted on its own.
        const key = /map-tile-providers\.js/.test(stack) ? 'ctlThemeObs' : 'liveThemeObs';
        if (!this.__counted && target === document.documentElement && filt && filt.indexOf('data-theme') >= 0 &&
            /live\.js/.test(stack)) {
          this.__counted = key;
          probe[key]++;
        }
        return super.observe(target, opts);
      }
      disconnect() {
        if (this.__counted) { probe[this.__counted]--; this.__counted = false; }
        return super.disconnect();
      }
    };
    const bump = (bag, fn, d) => { const n = (fn && fn.name) || '(anonymous)'; bag[n] = (bag[n] || 0) + d; };
    const mqlAdd = MediaQueryList.prototype.addEventListener;
    const mqlRemove = MediaQueryList.prototype.removeEventListener;
    MediaQueryList.prototype.addEventListener = function (type, fn, o) {
      if (type === 'change' && /pointer: fine/.test(this.media)) bump(probe.mql, fn, 1);
      return mqlAdd.call(this, type, fn, o);
    };
    MediaQueryList.prototype.removeEventListener = function (type, fn, o) {
      if (type === 'change' && /pointer: fine/.test(this.media)) bump(probe.mql, fn, -1);
      return mqlRemove.call(this, type, fn, o);
    };
    function patch(target, type, bag) {
      const add = target.addEventListener.bind(target);
      const remove = target.removeEventListener.bind(target);
      target.addEventListener = function (t, fn, o) { if (t === type) bump(bag, fn, 1); return add(t, fn, o); };
      target.removeEventListener = function (t, fn, o) { if (t === type) bump(bag, fn, -1); return remove(t, fn, o); };
    }
    patch(window, 'mc-tile-provider-changed', probe.tile);
    patch(document, 'click', probe.docClick);
  });
  await page.goto(`${BASE}/`, { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#chooseExp', { timeout: 15000 });
  await page.click('#chooseExp');
  return page;
}

async function clickLive(page) {
  await page.click('.top-nav .nav-link[data-route="live"]');
  await page.waitForSelector('.live-page', { timeout: 10000 });
  await page.waitForSelector('#liveMap .leaflet-tile-pane', { state: 'attached', timeout: 10000 });
  await page.waitForTimeout(1500); // the async init() runs past its awaits
}
async function clickPackets(page) {
  await page.click('.top-nav .nav-link[data-route="packets"]');
  await page.waitForSelector('.live-page', { state: 'detached', timeout: 10000 });
  await page.waitForTimeout(300);
}
async function toggleTheme(page) {
  const before = await page.evaluate(() => document.documentElement.getAttribute('data-theme'));
  await page.click('#darkModeToggle');
  await page.waitForFunction((b) => document.documentElement.getAttribute('data-theme') !== b, before, { timeout: 5000 });
  await page.waitForTimeout(200);
}
const probe = (page) => page.evaluate(() => JSON.parse(JSON.stringify(window.__839)));
const typeErrors = (page) => page.__errors.filter((e) => /TypeError/.test(e));

(async () => {
  browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #839 Live teardown E2E against ${BASE} ===`);

  await test('a theme toggle after leaving Live throws no TypeError (rc19 regression)', async () => {
    const page = await coldStart();
    await clickLive(page);
    const mounted = await probe(page);
    assert(mounted.liveThemeObs === 1, 'Live theme observer while mounted: ' + mounted.liveThemeObs);
    assert(mounted.ctlThemeObs === 1, 'Live layer-control theme observer while mounted (#845): ' + mounted.ctlThemeObs);
    await clickPackets(page);
    await toggleTheme(page);
    assert(typeErrors(page).length === 0, 'TypeError after leaving Live: ' + typeErrors(page)[0]);
    await page.context().close();
  });

  await test('two Live visits and two toggles: no TypeError, no stacked observer', async () => {
    const page = await coldStart();
    for (let i = 0; i < 2; i++) { await clickLive(page); await clickPackets(page); }
    await toggleTheme(page);
    await toggleTheme(page);
    assert(typeErrors(page).length === 0, typeErrors(page).length + ' TypeError(s), first: ' + typeErrors(page)[0]);
    await page.context().close();
  });

  await test('after leaving Live none of its observers or listeners survive', async () => {
    const page = await coldStart();
    const base = await probe(page);
    for (let i = 0; i < 3; i++) { await clickLive(page); await clickPackets(page); }
    const after = await probe(page);
    const left = [];
    if (after.liveThemeObs !== 0) left.push('Live data-theme observers: ' + after.liveThemeObs);
    if (after.ctlThemeObs !== 0) left.push('Live layer-control data-theme observers (#845): ' + after.ctlThemeObs);
    for (const [name, n] of Object.entries(after.mql)) {
      if (n !== (base.mql[name] || 0)) left.push('(pointer: fine) change listener ' + name + ': ' + n);
    }
    for (const [name, n] of Object.entries(after.tile)) {
      if (n !== (base.tile[name] || 0)) left.push('window mc-tile-provider-changed ' + name + ': ' + n + ' (baseline ' + (base.tile[name] || 0) + ')');
    }
    assert(left.length === 0, 'left behind: ' + left.join('; '));
    assert(page.__errors.length === 0, 'page errors: ' + page.__errors[0]);
    await page.context().close();
  });

  await test('leaving Live with a feed card open leaves no outside-click listener', async () => {
    const page = await coldStart();
    await clickLive(page);
    const base = await probe(page);
    // The fixture server streams nothing, so one packet goes through the
    // same buffer entry point the WS handler uses (as in #1136's E2E).
    const hash = 'probe839' + Date.now().toString(16);
    await page.evaluate((h) => window._liveBufferPacket({
      id: 9999990839, hash: h, raw_hex: '00', path_json: '[]',
      observer_id: 'probe-839', observer_name: 'probe', timestamp: new Date().toISOString(),
      snr: 5, rssi: -90,
      decoded: { header: { payloadTypeName: 'GRP_TXT' }, payload: { text: 'probe-839' }, path: { hops: [] } },
    }), hash);
    const item = await page.waitForSelector(`#liveFeed .live-feed-item[data-hash="${hash}"]`, { timeout: 10000 }).catch(() => null);
    assert(item, 'the probe packet never reached the feed');
    await item.click();
    await page.waitForSelector('.feed-detail-card', { timeout: 5000 });
    const open = await probe(page);
    const name = Object.keys(open.docClick).find((k) => open.docClick[k] > (base.docClick[k] || 0));
    assert(name, 'the open card added no document click listener: ' + JSON.stringify(open.docClick));
    // Leave with the browser's Back button: no click lands on the document,
    // so the card's own outside-click removal never runs.
    await page.goBack();
    await page.waitForSelector('.live-page', { state: 'detached', timeout: 10000 });
    const after = await probe(page);
    assert(after.docClick[name] === (base.docClick[name] || 0), 'document click listener ' + name + ' left: ' + after.docClick[name]);
    await page.context().close();
  });

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed\n`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
