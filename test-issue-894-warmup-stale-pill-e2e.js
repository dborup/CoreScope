/**
 * E2E #894: a silent MQTT source is shown as a compact, dismissible pill that
 * updates and clears by itself; warm-up messages keep the banner.
 *
 * /api/healthz is stubbed (everything else is the real server); the page
 * clock is installed so the 30 s poll interval can be fast-forwarded.
 *
 * Asserts, on a phone at 375x560 from a cold start (no storage), real taps:
 *   1. stale source → a pill in its own row right under the top nav (never
 *      over it), inside the screen, no sideways overflow, × at least 44 px
 *      and hit by itself, an accessible label naming the source; the new
 *      pill is announced once, the pill row itself is not a live region; the
 *      full-width banner stays hidden;
 *   2. polling continues while stale: the minute count moves after one 30 s
 *      interval, with no reload;
 *   3. the source recovers → the pill is gone by itself, no reload;
 *   4. stale again → tap × → pill gone and stays gone across polls; the
 *      dismissal is per session (survives a reload); recovery forgets it, so a
 *      later silence shows the pill again;
 *   5. warm-up (ready=false) → today's banner with its text; a pill shown at
 *      the same time moves down with the nav and its × stays hit-testable;
 *   6. the banner hides again → the pill row is right under the nav (no gap)
 *      and a real tap on × still dismisses it;
 *   7. no console errors.
 *
 * Usage:
 *   BASE_URL=http://localhost:13800 [SHOT=/path/shot.png] node test-issue-894-warmup-stale-pill-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13800';
const VP = { width: 375, height: 560 };
let failures = 0;
function assert(c, m) { if (!c) { failures++; console.log('  ❌ ' + m); } else console.log('  ✅ ' + m); }

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #894 stale-source pill against ${BASE} ===\n`);
  const ctx = await browser.newContext({ viewport: VP, isMobile: true, hasTouch: true, deviceScaleFactor: 2 });
  const page = await ctx.newPage();
  const errors = [];
  page.on('pageerror', (e) => errors.push(String(e)));
  page.on('console', (m) => { if (m.type() === 'error' && !/Failed to load resource|tile|net::/i.test(m.text())) errors.push(m.text()); });

  // ── stubbed /api/healthz ────────────────────────────────────────────────
  let mode = 'stale';   // stale | fresh | loading
  let ageSec = 12 * 60;
  let skewMs = 0;       // page clock fast-forwarded this far ahead of real time
  let healthzCalls = 0;
  await ctx.route('**/api/healthz', async (route) => {
    healthzCalls++;
    const nowSec = (Date.now() + skewMs) / 1000;
    const body = {
      ready: mode !== 'loading',
      from_pubkey_backfill: { done: true, processed: 1, total: 1 },
      ingest_liveness: { 'mqtt-eu': { lastReceiptUnix: nowSec - (mode === 'fresh' ? 5 : ageSec) } },
    };
    await route.fulfill({
      status: 200, contentType: 'application/json',
      headers: { 'X-Corescope-Load-Status': mode === 'loading' ? 'loading' : 'ready' },
      body: JSON.stringify(body),
    });
  });
  await page.clock.install({ time: Date.now() });

  const tick = async (ms) => { skewMs += ms; await page.clock.fastForward(ms); await page.waitForTimeout(400); };
  const pills = () => page.evaluate(() => Array.from(document.querySelectorAll('#warmup-stale-pills .warmup-pill')).map(p => p.querySelector('.warmup-pill__text').textContent));
  const bannerShown = () => page.evaluate(() => { const b = document.getElementById('warmup-banner'); return !!b && !b.classList.contains('warmup-banner--hidden'); });
  async function dismissTips() {
    for (let i = 0; i < 4; i++) {
      const b = await page.waitForSelector('.gesture-hint-dismiss', { state: 'visible', timeout: 2500 }).catch(() => null);
      if (!b) break;
      await b.tap(); await page.waitForTimeout(300);
    }
  }
  async function waitPill(n) {
    await page.waitForFunction((n) => document.querySelectorAll('#warmup-stale-pills .warmup-pill').length === n, n, { timeout: 8000 }).catch(() => {});
  }
  const geom = () => page.evaluate(() => {
    const pill = document.querySelector('#warmup-stale-pills .warmup-pill');
    const nav = document.querySelector('.top-nav');
    if (!pill) return null;
    const r = pill.getBoundingClientRect(), n = nav.getBoundingClientRect();
    const btn = pill.querySelector('button'), br = btn.getBoundingClientRect();
    const hit = document.elementFromPoint(br.x + br.width / 2, br.y + br.height / 2);
    const row = document.getElementById('warmup-stale-pills');
    const ann = document.getElementById('warmup-stale-announce');
    return {
      top: r.top, bottom: r.bottom, left: r.left, right: r.right, h: r.height, navBottom: n.bottom,
      rowTop: row.getBoundingClientRect().top, rowPos: getComputedStyle(row).position,
      bw: br.width, bh: br.height, bx: br.x + br.width / 2, by: br.y + br.height / 2,
      hitBtn: hit === btn, label: btn.getAttribute('aria-label'),
      overflowX: document.documentElement.scrollWidth > window.innerWidth,
      rowLive: row.getAttribute('aria-live'), rowRole: row.getAttribute('role'),
      annRole: ann && ann.getAttribute('role'), annLive: ann && ann.getAttribute('aria-live'),
      annText: ann ? ann.textContent : null, annInRow: !!ann && row.contains(ann),
    };
  });

  // 1 ── cold start, stale ───────────────────────────────────────────────
  console.log('1. stale source → pill');
  await page.goto(BASE + '/#/home', { waitUntil: 'domcontentloaded' });
  await waitPill(1);
  await dismissTips();
  await page.evaluate(() => { window.__noReloadMarker = 1; });
  let t = await pills();
  assert(t.length === 1 && /^No packets from mqtt-eu in 1[123] min\.$/.test(t[0]), 'pill text: ' + JSON.stringify(t));
  assert(!(await bannerShown()), 'full-width banner stays hidden for a stale source');
  let g = await geom();
  assert(g && g.top >= g.navBottom - 0.5, `pill (top ${g && g.top}) sits under the nav (bottom ${g && g.navBottom})`);
  assert(g && g.rowPos === 'static' && Math.abs(g.rowTop - g.navBottom) <= 0.5, `pill row in normal flow right after the nav (${g && g.rowPos}, row top ${g && g.rowTop})`);
  assert(g && g.left >= 0 && g.right <= VP.width && !g.overflowX, 'pill inside the 375px screen, no sideways overflow');
  assert(g && g.h < 100, 'pill is compact (' + (g && g.h) + 'px tall)');
  assert(g && g.bw >= 44 && g.bh >= 44 && g.hitBtn, `× is ${g && g.bw}x${g && g.bh}, hit by itself`);
  assert(g && /mqtt-eu/.test(g.label), 'accessible label names the source: ' + (g && g.label));
  assert(g && !g.rowLive && !g.rowRole, 'the pill row is not a live region (minute updates stay silent)');
  assert(g && g.annRole === 'status' && g.annLive === 'polite' && !g.annInRow && /^No packets from mqtt-eu in 1[123] min\.$/.test(g.annText),
    'the added pill is announced once via a separate status region: ' + JSON.stringify(g && g.annText));
  if (process.env.SHOT) await page.screenshot({ path: process.env.SHOT });

  // 2 ── still polling, minute count moves ────────────────────────────────
  console.log('2. keeps polling while stale');
  ageSec = 20 * 60;
  const before = healthzCalls;
  await tick(30000);
  t = await pills();
  assert(healthzCalls > before, 'another /api/healthz request after one interval');
  assert(t.length === 1 && /in (19|20|21) min\./.test(t[0]), 'minute count moved: ' + JSON.stringify(t));
  g = await geom();
  assert(g && /in 1[123] min\./.test(g.annText), 'the minute update is not announced again: ' + JSON.stringify(g && g.annText));

  // 3 ── recovery clears it ───────────────────────────────────────────────
  console.log('3. recovery clears the pill');
  mode = 'fresh';
  await tick(30000);
  assert((await pills()).length === 0, 'pill gone after the source delivers again');
  assert(await page.evaluate(() => window.__noReloadMarker === 1), 'no reload happened');

  // 4 ── dismissal ────────────────────────────────────────────────────────
  console.log('4. × dismisses, per session, reset on recovery');
  const callsAfterRecovery = healthzCalls;
  await tick(30000);
  assert(healthzCalls === callsAfterRecovery, 'polling stopped once nothing is stale');
  mode = 'stale'; ageSec = 15 * 60;
  await page.evaluate(() => window.__warmupBanner._pollOnce()); // a navigation-triggered check
  await waitPill(1);
  g = await geom();
  assert(g && g.hitBtn, 'pill is back and × is tappable');
  await page.touchscreen.tap(g.bx, g.by);
  await page.waitForTimeout(300);
  assert((await pills()).length === 0, 'tapping × removes the pill');
  await tick(30000);
  assert((await pills()).length === 0, 'stays dismissed across a poll while still stale');
  await page.reload({ waitUntil: 'domcontentloaded' });
  await page.waitForTimeout(1500);
  assert((await pills()).length === 0, 'dismissal survives a reload in the same session');
  mode = 'fresh';
  await page.evaluate(() => window.__warmupBanner._pollOnce());
  await page.waitForTimeout(500);
  mode = 'stale'; ageSec = 8 * 60;
  await page.evaluate(() => window.__warmupBanner._pollOnce());
  await waitPill(1);
  assert((await pills()).length === 1, 'recover then silent again → the pill returns');

  // 5 ── warm-up keeps the banner ─────────────────────────────────────────
  console.log('5. warm-up keeps today\'s banner');
  mode = 'loading';
  await page.evaluate(() => window.__warmupBanner._pollOnce());
  await page.waitForFunction(() => { const b = document.getElementById('warmup-banner'); return b && !b.classList.contains('warmup-banner--hidden'); }, null, { timeout: 8000 }).catch(() => {});
  assert(await bannerShown(), 'banner visible while loading');
  const btxt = await page.evaluate(() => document.querySelector('#warmup-banner').textContent);
  assert(/Loading historical data/.test(btxt) && !/No packets from/.test(btxt), 'banner has the loading text only: ' + btxt.trim());
  // review F2: the banner appearing while a pill is shown must not leave the
  // × under the nav.
  await page.waitForTimeout(600); // banner max-height transition
  g = await geom();
  assert((await pills()).length === 1, 'the stale pill stays shown next to the banner');
  assert(g && g.top >= g.navBottom - 0.5 && g.hitBtn, `banner shown: pill (top ${g && g.top}) under the moved nav (bottom ${g && g.navBottom}), × hit by itself`);

  // 6 ── banner hides again ───────────────────────────────────────────────
  console.log('6. banner hides → pill row right under the nav');
  mode = 'stale';
  await page.evaluate(() => window.__warmupBanner._pollOnce());
  await page.waitForFunction(() => document.getElementById('warmup-banner').classList.contains('warmup-banner--hidden'), null, { timeout: 8000 }).catch(() => {});
  await page.waitForTimeout(600);
  assert(!(await bannerShown()), 'banner hidden once ready');
  g = await geom();
  assert(g && Math.abs(g.rowTop - g.navBottom) <= 0.5 && g.hitBtn, `no gap: row top ${g && g.rowTop} = nav bottom ${g && g.navBottom}, × hit by itself`);
  await page.touchscreen.tap(g.bx, g.by);
  await page.waitForTimeout(300);
  assert((await pills()).length === 0, 'a real tap on × dismisses it');

  assert(errors.length === 0, 'no console errors' + (errors.length ? ': ' + errors.join(' | ') : ''));
  await browser.close();
  console.log(failures ? `\n${failures} FAILED` : '\nall passed');
  process.exit(failures ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
