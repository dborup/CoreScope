/**
 * E2E (#880 M1): Live "Follow a packet".
 *
 * The CI fixture has no packet heard by more than one observer, so the
 * follow data is stubbed: /api/packets/<HASH> (an ADVERT over three relays
 * heard by six observers, one late, one with a naive clock, one with a
 * skewed clock), /api/observers (their positions and regions) and
 * /api/observers/clock-skew. Relays and the origin are real fixture nodes
 * with a position, so hop resolution runs through Live's own HopResolver.
 *
 * Probes (init script, before the SPA's scripts): document 'keydown'
 * listeners named onFollowKey (net of removals), requestAnimationFrame calls
 * whose callback is followTick (with timestamps), and every request to
 * /api/packets/<HASH> and /api/observers/clock-skew.
 *
 * Covered:
 *   - desktop 1440x900 cold start: line popup → Follow, one fetch, hop
 *     progress, live packets suppressed while following, summary values,
 *     caption, close tears everything down and live animation resumes;
 *   - deep link #/live?follow=<hash> starts on load; an invalid one does
 *     nothing but a toast; Esc stops;
 *   - prefers-reduced-motion: final state at once, no follow frames;
 *   - leaving Live mid-replay leaves no card, state, listener or frame;
 *   - "Follow next packet": arm, cancel, and the next visible packet;
 *   - phone 375x560 with touch, cold start, real taps only: feed item →
 *     Follow → sheet above the bottom nav → close; no sideways scroll.
 *
 * Usage: BASE_URL=http://localhost:13581 node test-issue-880-live-follow-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:3000';
const HASH = '880f0110a5ed0001';
const HASH2 = '880f0110a5ed0002';

let passed = 0, failed = 0;
async function test(name, fn) {
  try { await fn(); passed++; console.log('  ✅ ' + name); }
  catch (err) { failed++; console.log('  ❌ ' + name + ': ' + (err && err.stack || err)); }
}
function assert(cond, msg) { if (!cond) throw new Error(msg || 'assertion failed'); }

let browser;
let FIX = null; // { nodes: [origin, r1, r2, r3] } picked from the fixture

async function pickNodes() {
  const res = await fetch(BASE + '/api/nodes?limit=2000');
  const data = await res.json();
  const list = (data.nodes || data).filter((n) => n.lat != null && n.lon != null && !(n.lat === 0 && n.lon === 0) && n.public_key);
  // Distinct 1-byte prefixes so the heuristic cannot mix them up.
  const seen = new Set();
  const pick = [];
  for (const n of list) {
    const p = n.public_key.slice(0, 2).toUpperCase();
    if (seen.has(p)) continue;
    seen.add(p);
    pick.push(n);
    if (pick.length === 4) break;
  }
  assert(pick.length === 4, 'fixture needs 4 positioned nodes, got ' + pick.length);
  return pick;
}

function observers() {
  const [o, r1, r2, r3] = FIX;
  const near = (n, d) => [n.lat + d, n.lon + d];
  return [
    { id: 'F880A', name: 'Obs Alpha', iata: 'CPH', pos: near(o, 0.01), clock_naive: false },
    { id: 'F880B', name: 'Obs Bravo', iata: 'CPH', pos: near(r1, 0.01), clock_naive: false },
    { id: 'F880C', name: 'Obs Charlie', iata: 'AAR', pos: near(r1, -0.01), clock_naive: true },
    { id: 'F880D', name: 'Obs Delta', iata: 'AAR', pos: near(r2, 0.01), clock_naive: false },
    { id: 'F880E', name: 'Obs Echo <b>x</b>', iata: 'ODE', pos: near(r3, 0.01), clock_naive: false },
    { id: 'F880F', name: 'Obs Foxtrot', iata: 'ODE', pos: near(r1, 0.02), clock_naive: false },
  ];
}

function detailFor(hash, t0Sec) {
  const [o, r1, r2, r3] = FIX;
  const pre = (n) => n.public_key.slice(0, 2).toUpperCase();
  const iso = (s) => new Date(s * 1000).toISOString();
  const ob = observers();
  const mk = (id, idx, dt, relays) => ({
    id, observer_id: ob[idx].id, observer_name: ob[idx].name, observer_iata: ob[idx].iata,
    snr: 5, rssi: -90, path_json: JSON.stringify(relays.map(pre)),
    resolved_path: relays.map((n) => n.public_key), timestamp: iso(t0Sec + dt),
  });
  return {
    packet: {
      hash, route_type: 1, payload_type: 4, first_seen: iso(t0Sec),
      decoded_json: JSON.stringify({ type: 'ADVERT', pubKey: o.public_key, name: o.name, lat: o.lat, lon: o.lon }),
    },
    path: [],
    observation_count: 7,
    observations: [
      mk(1, 0, 0, []),
      mk(2, 1, 1, [r1]),
      mk(3, 2, 0, [r1]),
      mk(4, 3, 2, [r1, r2]),
      mk(5, 4, 3, [r1, r2, r3]),
      mk(6, 4, 5, [r1, r2, r3, r1]), // later and deeper: not Echo's first report
      mk(7, 5, 95, [r1]),           // late upload
    ],
  };
}

// A WS-shaped frame for bufferPacket(): the transmission as Live sees it.
function frameFor(hash, ageMs) {
  const [o, r1] = FIX;
  return {
    id: 880, hash, timestamp: new Date(Date.now() - (ageMs || 10000)).toISOString(),
    route_type: 1, payload_type: 4, observer_id: 'F880B', observer_name: 'Obs Bravo',
    path_json: JSON.stringify([r1.public_key.slice(0, 2).toUpperCase()]),
    resolved_path: [r1.public_key],
    decoded: { header: { payloadTypeName: 'ADVERT' },
      payload: { type: 'ADVERT', pubKey: o.public_key, name: o.name, lat: o.lat, lon: o.lon } },
  };
}

async function newPage(opts) {
  const ctx = await browser.newContext(Object.assign({ viewport: { width: 1440, height: 900 } }, opts || {}));
  const page = await ctx.newPage();
  page.__errors = [];
  page.__fetches = { [HASH]: 0, [HASH2]: 0, skew: 0, other: [] };
  page.on('pageerror', (e) => page.__errors.push(String(e && e.stack || e)));
  await page.addInitScript(() => {
    const probe = window.__880 = { keydown: 0, frames: [] };
    const add = document.addEventListener.bind(document);
    const remove = document.removeEventListener.bind(document);
    document.addEventListener = function (t, fn, o) { if (t === 'keydown' && fn && fn.name === 'onFollowKey') probe.keydown++; return add(t, fn, o); };
    document.removeEventListener = function (t, fn, o) { if (t === 'keydown' && fn && fn.name === 'onFollowKey') probe.keydown--; return remove(t, fn, o); };
    const raf = window.requestAnimationFrame.bind(window);
    window.requestAnimationFrame = function (fn) { if (fn && fn.name === 'followTick') probe.frames.push(performance.now()); return raf(fn); };
  });
  const t0 = Math.floor(Date.now() / 1000) - 120;
  await page.route(/\/api\/packets\/880f0110a5ed000[12](\?.*)?$/, (route) => {
    const h = route.request().url().match(/880f0110a5ed000[12]/)[0];
    page.__fetches[h]++;
    route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(detailFor(h, t0)) });
  });
  await page.route(/\/api\/packets\/[0-9a-z]+(\?.*)?$/, (route) => {
    const u = route.request().url();
    if (!/880f0110a5ed000[12]/.test(u) && /\/api\/packets\/[0-9a-f]{16}/.test(u)) page.__fetches.other.push(u);
    route.fallback();
  });
  await page.route(/\/api\/observers\/clock-skew(\?.*)?$/, (route) => {
    page.__fetches.skew++;
    route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify([{ observerID: 'F880D', offsetSec: 900, samples: 20 }]) });
  });
  await page.route(/\/api\/observers(\?.*)?$/, (route) => {
    const list = observers().map((o) => ({ id: o.id, name: o.name, iata: o.iata, lat: o.pos[0], lon: o.pos[1], clock_naive: o.clock_naive }));
    route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ observers: list, server_time: new Date().toISOString() }) });
  });
  return page;
}

async function coldStartLive(page) {
  await page.goto(`${BASE}/`, { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#chooseExp', { timeout: 15000 });
  await page.click('#chooseExp');
  await page.click('.top-nav .nav-link[data-route="live"]');
  await waitLiveReady(page);
}
async function waitLiveReady(page) {
  await page.waitForSelector('.live-page', { timeout: 15000 });
  await page.waitForFunction(() => window._liveNodeData && Object.keys(window._liveNodeData()).length > 0 &&
    window.HopResolver && window.HopResolver.ready(), null, { timeout: 15000 });
  await page.waitForTimeout(500);
}
const state = (page) => page.evaluate(() => {
  const st = window._liveFollowState();
  return {
    active: !!st, hash: st && st.hash,
    url: location.hash,
    card: !!document.querySelector('.live-follow-card'),
    markers: document.querySelectorAll('.lf-marker').length,
    keydown: window.__880.keydown,
    armed: window._liveFollowArmed(),
  };
});
async function waitSummary(page, timeout) {
  await page.waitForSelector('.live-follow-card .lf-summary:not([hidden]) .lf-rows', { timeout: timeout || 20000 });
}
const summary = (page) => page.evaluate(() => {
  const out = {};
  const dts = document.querySelectorAll('.live-follow-card .lf-rows dt');
  for (const dt of dts) out[dt.textContent] = dt.nextElementSibling.textContent;
  out._caption = (document.querySelector('.live-follow-card .lf-caption') || {}).textContent;
  out._list = Array.from(document.querySelectorAll('.live-follow-card .lf-observers li .lf-obs-name')).map((e) => e.textContent);
  out._flags = document.querySelectorAll('.live-follow-card .lf-observers .lf-clock-flag').length;
  out._link = (document.querySelector('.live-follow-card .lf-link') || {}).getAttribute && document.querySelector('.live-follow-card .lf-link').getAttribute('href');
  return out;
});
const feedCount = (page) => page.evaluate(() => document.querySelectorAll('#liveFeed .live-feed-item').length);

(async () => {
  browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #880 Live follow a packet E2E against ${BASE} ===`);
  FIX = await pickNodes();

  await test('desktop: line popup → Follow, one fetch, hop progress, suppression, summary, close', async () => {
    const page = await newPage();
    await coldStartLive(page);
    await page.evaluate((f) => window._liveBufferPacket(f), frameFor(HASH));
    await page.waitForFunction(() => window._liveClickablePaths.length > 0, null, { timeout: 15000 });
    await page.evaluate(() => {
      const poly = window._liveClickablePaths[window._liveClickablePaths.length - 1].poly;
      const ll = poly.getLatLngs();
      poly.fire('click', { latlng: ll[Math.floor(ll.length / 2)] });
    });
    const btn = await page.waitForSelector('.path-info-popup .lc-follow-btn', { timeout: 5000 });
    await btn.click();
    await page.waitForSelector('.live-follow-card', { timeout: 5000 });
    let s = await state(page);
    assert(s.active && s.hash === HASH, 'following ' + JSON.stringify(s));
    assert(/[?&]follow=880f0110a5ed0001\b/.test(s.url), 'deep link written: ' + s.url);
    assert(s.keydown === 1, 'one Esc listener: ' + s.keydown);
    await page.waitForSelector('.path-info-popup', { state: 'detached', timeout: 2000 });
    await page.waitForFunction(() => /^Hop \d \/ 3$/.test(document.querySelector('.live-follow-card .lf-status').textContent), null, { timeout: 10000 });
    // A live packet while following: buffered, not animated or listed.
    const before = await feedCount(page);
    const vcrBefore = await page.evaluate(() => window._liveVCR().buffer.length);
    await page.evaluate((f) => window._liveBufferPacket(f), frameFor(HASH2, 1000));
    await page.waitForTimeout(300);
    assert(await feedCount(page) === before, 'live packet suppressed while following');
    assert(await page.evaluate(() => window._liveVCR().buffer.length) === vcrBefore + 1, 'still buffered for VCR');
    await waitSummary(page);
    const sum = await summary(page);
    assert(sum['Hops (max)'] === '3', 'hops ' + sum['Hops (max)']);
    assert(sum.Observers === '6 (2 with unreliable clock)', 'observers ' + sum.Observers);
    assert(sum['Regions reached'] === 'AAR, CPH, ODE', 'regions ' + sum['Regions reached']);
    assert(/\(\+95 s\)$/.test(sum['First / last heard']), 'spread ' + sum['First / last heard']);
    assert(/km \(.+ → .+\)/.test(sum['Longest single hop']), 'longest ' + sum['Longest single hop']);
    assert(/as reported by observers/i.test(sum._caption), 'caption ' + sum._caption);
    assert(sum._list[0] === 'Obs Alpha' && sum._list[1] === 'Obs Charlie' && sum._list[5] === 'Obs Foxtrot', 'heard order ' + sum._list);
    assert(sum._list.includes('Obs Echo <b>x</b>'), 'node-controlled name rendered as text');
    assert(!(await page.$('.live-follow-card b')), 'no markup injected from a name');
    assert(sum._flags === 2, 'clock flags ' + sum._flags);
    assert(sum._link === '#/packets/' + HASH, 'packet link ' + sum._link);
    const marks = await page.evaluate(() => ({
      origin: document.querySelectorAll('.lf-origin').length, relay: document.querySelectorAll('.lf-relay').length,
      observer: document.querySelectorAll('.lf-observer').length, late: document.querySelectorAll('.lf-observer.lf-late').length,
    }));
    assert(marks.origin === 1 && marks.relay === 3 && marks.observer === 6 && marks.late === 1, 'markers ' + JSON.stringify(marks));
    assert(page.__fetches[HASH] === 1, 'one /api/packets/{hash} fetch, got ' + page.__fetches[HASH]);
    assert(page.__fetches.skew === 1, 'one clock-skew fetch, got ' + page.__fetches.skew);
    assert(page.__fetches.other.length === 0, 'no other packet fetches: ' + page.__fetches.other);
    // Close.
    await page.click('.live-follow-card .lf-close');
    s = await state(page);
    assert(!s.active && !s.card && s.markers === 0, 'torn down ' + JSON.stringify(s));
    assert(!/follow=/.test(s.url), 'deep link removed: ' + s.url);
    assert(s.keydown === 0, 'Esc listener removed: ' + s.keydown);
    const n0 = await page.evaluate(() => window.__880.frames.length);
    await page.waitForTimeout(500);
    assert(await page.evaluate(() => window.__880.frames.length) === n0, 'no follow frame after close');
    const after = await feedCount(page);
    await page.evaluate((f) => window._liveBufferPacket(f), Object.assign(frameFor(HASH2, 1000), { id: 881 }));
    await page.waitForFunction((n) => document.querySelectorAll('#liveFeed .live-feed-item').length > n || window._liveVCR().mode !== 'LIVE', after, { timeout: 8000 });
    assert(page.__errors.length === 0, 'page errors: ' + page.__errors[0]);
    await page.context().close();
  });

  await test('deep link #/live?follow=<hash> starts on load; Esc stops it', async () => {
    const page = await newPage();
    await page.goto(`${BASE}/#/live?follow=${HASH.toUpperCase()}`, { waitUntil: 'domcontentloaded' });
    await waitLiveReady(page);
    await page.waitForSelector('.live-follow-card', { timeout: 10000 });
    await waitSummary(page);
    assert(page.__fetches[HASH] === 1, 'one fetch, got ' + page.__fetches[HASH]);
    assert(/follow=880f0110a5ed0001/.test((await state(page)).url), 'canonical lower-case hash in URL');
    await page.keyboard.press('Escape');
    const s = await state(page);
    assert(!s.active && !s.card && s.keydown === 0 && !/follow=/.test(s.url), 'Esc tears down ' + JSON.stringify(s));
    assert(page.__errors.length === 0, 'page errors: ' + page.__errors[0]);
    await page.context().close();
  });

  await test('an invalid deep link changes nothing but a toast', async () => {
    const page = await newPage();
    await page.goto(`${BASE}/#/live?follow=nothex<script>`, { waitUntil: 'domcontentloaded' });
    await waitLiveReady(page);
    await page.waitForSelector('.live-follow-toast', { timeout: 10000 });
    const s = await state(page);
    assert(!s.active && !s.card, 'no follow ' + JSON.stringify(s));
    assert(page.__fetches[HASH] === 0 && page.__fetches.skew === 0, 'no fetch');
    await page.context().close();
  });

  await test('reduced motion: final state at once, no follow frames, no pause/restart', async () => {
    const page = await newPage({ reducedMotion: 'reduce' });
    await page.goto(`${BASE}/#/live?follow=${HASH}`, { waitUntil: 'domcontentloaded' });
    await waitLiveReady(page);
    await waitSummary(page, 10000);
    const r = await page.evaluate(() => ({
      frames: window.__880.frames.length,
      observers: document.querySelectorAll('.lf-observer').length,
      relays: document.querySelectorAll('.lf-relay').length,
      controls: document.querySelector('.live-follow-card .lf-controls').hidden,
      replay: !!document.querySelector('.live-follow-card .lf-replay'),
      status: document.querySelector('.live-follow-card .lf-status').textContent,
    }));
    assert(r.frames === 0, 'follow frames under reduced motion: ' + r.frames);
    assert(r.observers === 6 && r.relays === 3, 'final state drawn ' + JSON.stringify(r));
    assert(r.controls === true && r.replay === false, 'no motion controls ' + JSON.stringify(r));
    assert(/^Done · 3 hops$/.test(r.status), 'status ' + r.status);
    await page.context().close();
  });

  await test('leaving Live mid-replay leaves no card, state, listener or frame', async () => {
    const page = await newPage();
    await coldStartLive(page);
    await page.evaluate((h) => window._liveStartFollow(h, { settleMs: 0 }), HASH);
    await page.waitForFunction(() => /^Hop [1-2] \/ 3$/.test((document.querySelector('.live-follow-card .lf-status') || {}).textContent || ''), null, { timeout: 10000 });
    assert((await page.evaluate(() => window.__880.frames.length)) > 0, 'the replay was running');
    await page.click('.top-nav .nav-link[data-route="packets"]');
    await page.waitForSelector('.live-page', { state: 'detached', timeout: 10000 });
    const n0 = await page.evaluate(() => window.__880.frames.length);
    await page.waitForTimeout(500);
    const r = await page.evaluate(() => ({
      frames: window.__880.frames.length, state: window._liveFollowState(), keydown: window.__880.keydown,
      card: !!document.querySelector('.live-follow-card'), url: location.hash,
    }));
    assert(r.frames === n0, 'follow frames after leaving: ' + (r.frames - n0));
    assert(r.state === null && r.keydown === 0 && !r.card, 'torn down ' + JSON.stringify(r));
    assert(/^#\/packets/.test(r.url) && !/follow=/.test(r.url), 'route untouched: ' + r.url);
    // A second visit starts clean and can follow again.
    await page.click('.top-nav .nav-link[data-route="live"]');
    await waitLiveReady(page);
    await page.evaluate((h) => window._liveStartFollow(h, { settleMs: 0 }), HASH);
    await waitSummary(page);
    assert((await page.evaluate(() => window.__880.keydown)) === 1, 'one Esc listener on the second visit');
    assert(page.__errors.length === 0, 'page errors: ' + page.__errors[0]);
    await page.context().close();
  });

  await test('feed card (tablet 700x900): Follow a fresh packet waits out the settle window, then one fetch', async () => {
    const page = await newPage({ viewport: { width: 700, height: 900 } });
    await page.goto(`${BASE}/#/live`, { waitUntil: 'domcontentloaded' });
    await waitLiveReady(page);
    await page.evaluate(() => { window.PROPAGATION_BUFFER_MS = 2500; });
    await page.evaluate((f) => window._liveBufferPacket(f), frameFor(HASH, 1));
    await page.click('#liveFeed .live-feed-item');
    const clicked = Date.now();
    await page.click('.feed-detail-card .fdc-follow');
    await page.waitForSelector('.live-follow-card', { timeout: 5000 });
    assert(!(await page.$('.feed-detail-card')), 'feed card makes room for the follow card');
    assert((await state(page)).hash === HASH, 'following the tapped packet');
    assert(/^Waiting \d s for late observations/.test(await page.textContent('.live-follow-card .lf-status')), 'settle countdown shown');
    await page.waitForTimeout(1000);
    assert(page.__fetches[HASH] === 0, 'no fetch inside the settle window, got ' + page.__fetches[HASH]);
    await waitSummary(page);
    assert(Date.now() - clicked >= 1500, 'fetched after the settle window');
    assert(page.__fetches[HASH] === 1, 'one fetch');
    await page.context().close();
  });

  await test('"Follow next packet": arm, cancel, then the next visible packet', async () => {
    const page = await newPage();
    await coldStartLive(page);
    const btn = '#liveFollowNextBtn';
    await page.evaluate((sel) => document.querySelector(sel).click(), btn);
    assert(await page.getAttribute(btn, 'aria-pressed') === 'true', 'armed');
    await page.evaluate((sel) => document.querySelector(sel).click(), btn);
    assert(await page.getAttribute(btn, 'aria-pressed') === 'false', 'cancelled');
    await page.evaluate((f) => window._liveBufferPacket(f), frameFor(HASH2, 10000));
    await page.waitForTimeout(400);
    assert(!(await state(page)).active, 'a cancelled arm follows nothing');
    await page.evaluate((sel) => document.querySelector(sel).click(), btn);
    await page.evaluate((f) => window._liveBufferPacket(f), frameFor(HASH, 10000));
    await page.waitForSelector('.live-follow-card', { timeout: 5000 });
    const s = await state(page);
    assert(s.active && s.hash === HASH && !s.armed, 'follows the next packet ' + JSON.stringify(s));
    assert(await page.getAttribute(btn, 'aria-pressed') === 'false', 'disarmed after the pick');
    await waitSummary(page);
    assert(page.__fetches[HASH] === 1 && page.__fetches[HASH2] === 0, 'fetches ' + JSON.stringify(page.__fetches));
    await page.context().close();
  });

  // The feed (and with it the feed card) is hidden at <= 640px (live.css
  // "Responsive"), so on a phone the entry is the gear menu's "Follow next
  // packet"; the feed card's Follow button serves 641px and up.
  await test('phone 375x560, cold start, real taps: gear → Follow next → sheet above the nav → close', async () => {
    const page = await newPage({ viewport: { width: 375, height: 560 }, hasTouch: true, isMobile: true, deviceScaleFactor: 2 });
    await page.goto(`${BASE}/`, { waitUntil: 'domcontentloaded' });
    const tapSel = async (sel) => {
      const el = await page.waitForSelector(sel, { state: 'visible', timeout: 10000 });
      await el.scrollIntoViewIfNeeded();
      const b = await el.boundingBox();
      assert(b && b.width > 0, 'no box for ' + sel);
      await page.touchscreen.tap(b.x + b.width / 2, b.y + b.height / 2);
      return b;
    };
    await tapSel('#chooseExp');
    await tapSel('.bottom-nav [data-route="live"]');
    await waitLiveReady(page);
    await tapSel('#liveControlsToggle');
    const fb = await tapSel('#liveFollowNextBtn');
    assert(fb.height >= 44, 'Follow next touch target ' + JSON.stringify(fb));
    assert(await page.getAttribute('#liveFollowNextBtn', 'aria-pressed') === 'true', 'armed by a tap');
    await tapSel('#liveControlsToggle');
    await page.evaluate((f) => window._liveBufferPacket(f), frameFor(HASH, 10000));
    await page.waitForSelector('.live-follow-card', { timeout: 5000 });
    await waitSummary(page);
    const geo = await page.evaluate(() => {
      const c = document.querySelector('.live-follow-card').getBoundingClientRect();
      const nav = document.querySelector('.bottom-nav');
      const n = nav ? nav.getBoundingClientRect() : null;
      const x = document.querySelector('.live-follow-card .lf-close').getBoundingClientRect();
      return { top: c.top, bottom: c.bottom, left: c.left, right: c.right, navTop: n && n.top,
        closeW: x.width, closeH: x.height, sw: document.scrollingElement.scrollWidth, iw: window.innerWidth, ih: window.innerHeight };
    });
    assert(geo.left >= 0 && geo.right <= geo.iw + 0.5 && geo.top >= 0, 'sheet inside the viewport ' + JSON.stringify(geo));
    assert(geo.navTop == null || geo.bottom <= geo.navTop + 1, 'sheet above the bottom nav ' + JSON.stringify(geo));
    assert(geo.bottom - geo.top <= geo.ih * 0.4 + 1, 'sheet ≤ 40vh ' + JSON.stringify(geo));
    assert(geo.closeW >= 44 && geo.closeH >= 44, 'close touch target ' + JSON.stringify(geo));
    assert(geo.sw <= geo.iw, 'no horizontal scroll ' + JSON.stringify(geo));
    await page.screenshot({ path: process.env.SHOT_DIR ? process.env.SHOT_DIR + '/880-phone.png' : '/dev/null' }).catch(() => {});
    await tapSel('.live-follow-card .lf-close');
    const s = await state(page);
    assert(!s.active && !s.card && s.keydown === 0 && !/follow=/.test(s.url), 'closed ' + JSON.stringify(s));
    assert(page.__errors.length === 0, 'page errors: ' + page.__errors[0]);
    await page.context().close();
  });

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})().catch(async (e) => {
  console.error(e);
  try { if (browser) await browser.close(); } catch (_) {}
  process.exit(1);
});
