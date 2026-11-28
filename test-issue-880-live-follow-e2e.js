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
 *   - feed card Follow: a fresh packet waits out the settle window, an
 *     older one fetches at once;
 *   - "Follow next packet" (the feed-header icon on desktop): it moves no
 *     existing control, arm, cancel, the next visible packet; it skips
 *     packets the favourites, node or region filter hides; leaving Live
 *     disarms it;
 *   - VCR replay: "Follow next" is not armed outside LIVE (and leaving LIVE
 *     disarms it); a follow stops the replay from animating;
 *   - phone 375x560 with touch, cold start, real taps only: gear → Follow
 *     next → the panel folds away → packet → sheet above the bottom nav,
 *     markers visible → 44 px popup Follow and "View packet" → close; no
 *     sideways scroll.
 *
 * Usage: BASE_URL=http://localhost:13581 node test-issue-880-live-follow-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:3000';
const { createFollowStubs, HASH, HASH2 } = require('./tests/live-follow-e2e-stubs');

let passed = 0, failed = 0;
async function test(name, fn) {
  try { await fn(); passed++; console.log('  ✅ ' + name); }
  catch (err) { failed++; console.log('  ❌ ' + name + ': ' + (err && err.stack || err)); }
}
function assert(cond, msg) { if (!cond) throw new Error(msg || 'assertion failed'); }

let browser;

const stubs = createFollowStubs(BASE);
const { pickNodes, frameFor } = stubs;

async function newPage(opts) {
  const ctx = await browser.newContext(Object.assign({ viewport: { width: 1440, height: 900 } }, opts || {}));
  const page = await ctx.newPage();
  page.__errors = [];
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
  await stubs.installRoutes(page);
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
  await pickNodes();

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

  await test('"Follow next packet": a feed-header icon that moves nothing; arm, cancel, then the next visible packet', async () => {
    const page = await newPage();
    await coldStartLive(page);
    const btn = '#liveFeedFollowNextBtn';
    // Review #1/#2: a row in the controls panel pushed the quick-picks and
    // the node filter. On desktop the panel row is not rendered at all; the
    // icon sits in the feed header between the corner and hide buttons.
    await page.click('#liveControlsToggle');
    await page.waitForFunction(() => document.getElementById('liveControls').classList.contains('is-expanded'));
    const lay = await page.evaluate(() => {
      const box = (el) => { const r = el.getBoundingClientRect(); return { l: r.left, t: r.top, r: r.right, b: r.bottom, w: r.width, h: r.height }; };
      const icon = document.getElementById('liveFeedFollowNextBtn');
      const b = box(icon);
      const top = document.elementFromPoint(b.l + b.w / 2, b.t + b.h / 2);
      return {
        menuRects: document.getElementById('liveFollowNextBtn').getClientRects().length,
        icon: b, hit: !!top && icon.contains(top),
        hide: box(document.getElementById('feedHideBtn')),
        corner: box(document.querySelector('#liveFeed .panel-corner-btn')),
        header: box(document.querySelector('#liveFeed .panel-header')),
      };
    });
    const apart = (a, b) => a.r <= b.l || b.r <= a.l || a.b <= b.t || b.b <= a.t;
    assert(lay.menuRects === 0, 'the controls-panel row is not rendered on desktop');
    assert(lay.hit, 'feed-header icon hit-testable ' + JSON.stringify(lay.icon));
    assert(apart(lay.icon, lay.hide) && apart(lay.icon, lay.corner), 'icon clear of the hide and corner buttons ' + JSON.stringify(lay));
    assert(lay.header.h <= 36.5, 'feed header not taller than before: ' + lay.header.h);
    await page.click('#liveControlsToggle');
    await page.click(btn);
    assert(await page.getAttribute(btn, 'aria-pressed') === 'true', 'armed');
    assert(await page.getAttribute('#liveFollowNextBtn', 'aria-pressed') === 'true', 'both buttons share the state');
    await page.waitForSelector('.live-follow-toast', { timeout: 2000 });
    await page.click(btn);
    assert(await page.getAttribute(btn, 'aria-pressed') === 'false', 'cancelled');
    await page.evaluate((f) => window._liveBufferPacket(f), frameFor(HASH2, 10000));
    await page.waitForTimeout(400);
    assert(!(await state(page)).active, 'a cancelled arm follows nothing');
    await page.click(btn);
    await page.evaluate((f) => window._liveBufferPacket(f), frameFor(HASH, 10000));
    await page.waitForSelector('.live-follow-card', { timeout: 5000 });
    const s = await state(page);
    assert(s.active && s.hash === HASH && !s.armed, 'follows the next packet ' + JSON.stringify(s));
    assert(await page.getAttribute(btn, 'aria-pressed') === 'false', 'disarmed after the pick');
    await waitSummary(page);
    assert(page.__fetches[HASH] === 1 && page.__fetches[HASH2] === 0, 'fetches ' + JSON.stringify(page.__fetches));
    await page.context().close();
  });

  // Review #6b: "always the full buffer" kept every E2E green.
  await test('feed card: Follow on an older packet fetches at once', async () => {
    const page = await newPage({ viewport: { width: 700, height: 900 } });
    await page.goto(`${BASE}/#/live`, { waitUntil: 'domcontentloaded' });
    await waitLiveReady(page);
    await page.evaluate(() => { window.PROPAGATION_BUFFER_MS = 2500; });
    await page.evaluate((f) => window._liveBufferPacket(f), frameFor(HASH, 60000));
    await page.click('#liveFeed .live-feed-item');
    await page.click('.feed-detail-card .fdc-follow');
    await page.waitForSelector('.live-follow-card', { timeout: 5000 });
    assert(!/^Waiting/.test(await page.textContent('.live-follow-card .lf-status')), 'no settle countdown for an older packet');
    await page.waitForFunction(() => document.querySelector('.live-follow-card .lf-status').textContent !== 'Loading…' || false, null, { timeout: 1500 });
    assert(page.__fetches[HASH] === 1, 'fetched at once, got ' + page.__fetches[HASH]);
    await page.context().close();
  });

  // Review #6a: moving the pick above the favourites/node/region filters
  // kept every E2E green. Each filter hides one packet; the arm survives it.
  await test('"Follow next packet" skips packets the favourites, node or region filter hides', async () => {
    const page = await newPage();
    await coldStartLive(page);
    await page.waitForFunction(() => window.RegionFilter && typeof RegionFilter.setSelected === 'function');
    await page.click('#liveFeedFollowNextBtn');
    let id = 8800;
    const hidden = async (label, on, off) => {
      await page.evaluate(on);
      const f = Object.assign(frameFor('880f0110a5ed00' + (id - 8790).toString(16).padStart(2, '0'), 10000), { id: ++id });
      await page.evaluate((x) => window._liveBufferPacket(x), f);
      await page.waitForTimeout(400);
      const s = await state(page);
      assert(!s.active && !s.card && s.armed, label + ': a hidden packet is not followed and the arm stays ' + JSON.stringify(s));
      await page.evaluate(off);
    };
    // No favourites: "Favorites" hides every packet.
    await hidden('favourites', () => { const t = document.getElementById('liveFavoritesToggle'); if (!t.checked) t.click(); },
      () => { const t = document.getElementById('liveFavoritesToggle'); if (t.checked) t.click(); });
    // A node no frame touches.
    await hidden('node filter', () => window._liveSetNodeFilter(['ffffffffffff']), () => window._liveSetNodeFilter([]));
    // The frame's observer F880B is in CPH (stubbed /api/observers).
    await hidden('region filter', () => RegionFilter.setSelected(['AAR']), () => RegionFilter.setSelected([]));
    await page.evaluate((f) => window._liveBufferPacket(f), Object.assign(frameFor(HASH, 10000), { id: 8890 }));
    await page.waitForSelector('.live-follow-card', { timeout: 5000 });
    const s = await state(page);
    assert(s.active && s.hash === HASH && !s.armed, 'with the filters cleared, the next packet is followed ' + JSON.stringify(s));
    assert(page.__errors.length === 0, 'page errors: ' + page.__errors[0]);
    await page.context().close();
  });

  // Review #6c: dropping `followArmed = false` from destroy() kept every
  // E2E green.
  await test('leaving Live while "Follow next" is armed disarms it', async () => {
    const page = await newPage();
    await coldStartLive(page);
    await page.click('#liveFeedFollowNextBtn');
    assert((await state(page)).armed, 'armed');
    await page.click('.top-nav .nav-link[data-route="packets"]');
    await page.waitForSelector('.live-page', { state: 'detached', timeout: 10000 });
    assert(await page.evaluate(() => window._liveFollowArmed()) === false, 'disarmed on leaving Live');
    await page.click('.top-nav .nav-link[data-route="live"]');
    await waitLiveReady(page);
    await page.evaluate((f) => window._liveBufferPacket(f), frameFor(HASH, 10000));
    await page.waitForTimeout(500);
    const s = await state(page);
    assert(!s.active && !s.card && !s.armed, 'the next visit follows nothing by itself ' + JSON.stringify(s));
    assert(await page.getAttribute('#liveFeedFollowNextBtn', 'aria-pressed') === 'false', 'icon not pressed');
    await page.context().close();
  });

  // Review #5: in VCR REPLAY the replay kept animating under a follow, and
  // an armed "Follow next" picked a replayed historical packet.
  await test('VCR replay: "Follow next" only arms in LIVE; a follow stops the replay animating', async () => {
    const page = await newPage();
    await coldStartLive(page);
    const icon = '#liveFeedFollowNextBtn';
    // Armed in LIVE, then Rewind replays the last hour of the fixture DB
    // (freshened to now): the arm is dropped.
    await page.click(icon);
    assert((await state(page)).armed, 'armed in LIVE');
    await page.click('#vcrRewindBtn');
    await page.waitForFunction(() => window._liveVCR().mode === 'REPLAY', null, { timeout: 10000 });
    let s = await state(page);
    assert(!s.armed && await page.getAttribute(icon, 'aria-pressed') === 'false', 'leaving LIVE disarms ' + JSON.stringify(s));
    // In REPLAY a click does not arm; a toast says why.
    await page.click(icon);
    s = await state(page);
    assert(!s.armed && !s.active, 'not armed in REPLAY ' + JSON.stringify(s));
    assert(/LIVE mode/.test(await page.textContent('.live-follow-toast')), 'toast explains');
    // A follow during the replay: the replay clock runs, nothing animates.
    await page.evaluate((h) => window._liveStartFollow(h, { settleMs: 0 }), HASH);
    await page.waitForSelector('.live-follow-card', { timeout: 5000 });
    const before = await page.evaluate(() => ({
      playhead: window._liveVCR().playhead,
      pkts: document.getElementById('livePktCount').textContent,
      feedTop: (document.querySelector('#liveFeed .live-feed-item') || {}).outerHTML,
    }));
    await page.waitForTimeout(4500); // >= 2 replay ticks (each <= 2 s at 1x)
    const after = await page.evaluate(() => ({
      mode: window._liveVCR().mode,
      playhead: window._liveVCR().playhead,
      pkts: document.getElementById('livePktCount').textContent,
      feedTop: (document.querySelector('#liveFeed .live-feed-item') || {}).outerHTML,
    }));
    assert(after.mode === 'REPLAY' && after.playhead > before.playhead, 'the replay kept going ' + JSON.stringify([before.playhead, after.playhead]));
    assert(after.pkts === before.pkts, 'no replayed packet counted ' + before.pkts + ' → ' + after.pkts);
    assert(after.feedTop === before.feedTop, 'no replayed packet listed in the feed');
    assert(page.__errors.length === 0, 'page errors: ' + page.__errors[0]);
    await page.context().close();
  });

  // Review #3 and nit #8: a follow that starts with the panel pinned open
  // (here a shared link) folds it, and at 360x640 the 40vh sheet covers no
  // marker once the summary has grown it.
  await test('phone 360x640: a follow folds a pinned-open panel; no marker under the full-height sheet', async () => {
    const page = await newPage({ viewport: { width: 360, height: 640 }, hasTouch: true, isMobile: true });
    await page.addInitScript(() => { try { localStorage.setItem('live-controls-expanded', 'true'); } catch (_) {} });
    await page.goto(`${BASE}/#/live`, { waitUntil: 'domcontentloaded' });
    await waitLiveReady(page);
    assert(await page.evaluate(() => document.getElementById('liveControls').classList.contains('is-expanded')), 'panel pinned open');
    await page.evaluate((h) => window._liveStartFollow(h, { settleMs: 0 }), HASH);
    await page.waitForSelector('.live-follow-card', { timeout: 5000 });
    assert(await page.evaluate(() => document.getElementById('liveControls').classList.contains('is-collapsed')), 'panel folded when the follow started');
    await waitSummary(page);
    await page.waitForTimeout(400);
    const r = await page.evaluate(() => {
      const sheet = document.querySelector('.live-follow-card').getBoundingClientRect();
      const dots = [...document.querySelectorAll('.lf-marker .lf-dot')].map((d) => d.getBoundingClientRect());
      return { sheetTop: sheet.top, sheetH: sheet.height, ih: innerHeight, n: dots.length,
        under: dots.filter((b) => b.top + b.height / 2 >= sheet.top).length };
    });
    assert(r.sheetH >= r.ih * 0.4 - 2, 'the summary grew the sheet to its cap ' + JSON.stringify(r));
    assert(r.n === 10 && r.under === 0, 'markers under the sheet ' + JSON.stringify(r));
    assert(page.__errors.length === 0, 'page errors: ' + page.__errors[0]);
    await page.context().close();
  });

  // The feed (and with it the feed card) is hidden at <= 640px (live.css
  // "Responsive"), so on a phone the entry is the gear menu's "Follow next
  // packet"; the feed card's Follow button serves 641px and up.
  await test('phone 375x560, cold start, real taps: gear → Follow next → panel folds → sheet above the nav → close', async () => {
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
    // Review #3: the panel folds away by itself; the user does not close it.
    const folded = await page.evaluate(() => ({
      collapsed: document.getElementById('liveControls').classList.contains('is-collapsed'),
      bodyHidden: document.getElementById('liveControlsBody').hidden,
      gear: document.getElementById('liveControlsToggle').getAttribute('aria-expanded'),
    }));
    assert(folded.collapsed && folded.bodyHidden && folded.gear === 'false', 'panel folded on arm ' + JSON.stringify(folded));
    await page.evaluate((f) => window._liveBufferPacket(f), frameFor(HASH, 10000));
    await page.waitForSelector('.live-follow-card', { timeout: 5000 });
    await waitSummary(page);
    // Every positioned follow marker is in view and not under the sheet,
    // the header or the controls (review #3, nit #8).
    const marks = await page.evaluate(() => {
      const sheet = document.querySelector('.live-follow-card').getBoundingClientRect();
      const out = { total: 0, covered: [] };
      for (const dot of document.querySelectorAll('.lf-marker .lf-dot')) {
        const r = dot.getBoundingClientRect();
        const x = r.left + r.width / 2, y = r.top + r.height / 2;
        out.total++;
        const top = document.elementFromPoint(x, y);
        const onMap = !!top && !!top.closest('#liveMap');
        if (y >= sheet.top || !onMap) out.covered.push([Math.round(x), Math.round(y), top ? (top.id || top.className && String(top.className.baseVal != null ? top.className.baseVal : top.className)) : 'none']);
      }
      return out;
    });
    assert(marks.total === 10, 'markers drawn ' + marks.total);
    assert(!marks.covered.length, 'markers covered or under the sheet: ' + JSON.stringify(marks.covered));
    // Review #4: "View packet →" is a 44 px target.
    const link = await page.evaluate(() => { const r = document.querySelector('.live-follow-card .lf-link').getBoundingClientRect(); return { w: r.width, h: r.height }; });
    assert(link.w >= 44 && link.h >= 44, 'View packet touch target ' + JSON.stringify(link));
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
    // Review #4: the line popup's "Follow ▶" is a 44 px target, and a tap on
    // it starts a follow.
    await page.evaluate(() => {
      const poly = window._liveClickablePaths[window._liveClickablePaths.length - 1].poly;
      const ll = poly.getLatLngs();
      poly.fire('click', { latlng: ll[Math.floor(ll.length / 2)] });
    });
    const pb = await tapSel('.path-info-popup .lc-follow-btn');
    assert(pb.width >= 44 && pb.height >= 44, 'popup Follow touch target ' + JSON.stringify(pb));
    await page.waitForSelector('.live-follow-card', { timeout: 5000 });
    assert((await state(page)).hash === HASH, 'popup Follow started a follow');
    await tapSel('.live-follow-card .lf-close');
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
