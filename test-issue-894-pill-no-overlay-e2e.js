/**
 * E2E #894 (review F1/F4): the stale-source pill must never cover page
 * controls. It takes its own row directly after .top-nav, in normal flow.
 *
 * /api/healthz is stubbed to report one silent MQTT source; everything else
 * is the real server. At 375x560 and 360x640 (touch, cold start) and at
 * 1440x900, with a pill shown on every page, this checks that:
 *   1. the topmost element at the centre of each control (the ones the
 *      review found covered) is that control itself (or its own icon);
 *   2. the pill row is in normal flow (position static, no z-index): the pill
 *      starts at or below the nav's bottom edge and #app starts at or below
 *      the row's bottom edge (on /live the nav is a fixed overlay, so the
 *      row starts at 0 and clears the nav with padding);
 *   3. on the fixed-height pages the extra row is subtracted, so #app ends
 *      at the bottom nav (or the viewport) and the page does not scroll;
 *   4. the pill's × is hit by itself on every page;
 *   5. no console errors;
 *   6. a real-tap walk at 375x560 from a cold start, reaching each page by
 *      tapping the bottom nav (as a visitor would): a real tap at the centre
 *      of each control lands on that control while the pill is shown.
 *
 * Usage:
 *   BASE_URL=http://localhost:13800 node test-issue-894-pill-no-overlay-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13800';
let failures = 0;
function assert(c, m) { if (!c) { failures++; console.log('  ❌ ' + m); } else console.log('  ✅ ' + m); }

const PHONE = ['map-toggle', 'map-zoom', 'live-zoom', 'nodeSearch', 'nodeBackBtn', 'chAddChannelBtn', 'path-symbols'];
const DESKTOP = ['map-toggle', 'pkt-refresh', 'pkt-pause', 'pkt-byop', 'nodes-export'];
const VIEWPORTS = [
  { name: '375x560', vp: { width: 375, height: 560 }, phone: true, required: PHONE },
  { name: '360x640', vp: { width: 360, height: 640 }, phone: true, required: PHONE },
  { name: '1440x900', vp: { width: 1440, height: 900 }, phone: false, required: DESKTOP },
];

// page hash → controls to probe there. Every control is probed at every
// viewport; `required` above lists those the review found covered, and a
// required control that is not on screen fails the run.
const PAGES = [
  { hash: '#/map', fixed: true, controls: [
    ['map-toggle', '#mapControlsToggle'],
    ['map-zoom', '.leaflet-control-zoom-in'],
  ] },
  { hash: '#/live', fixed: true, controls: [['live-zoom', '.leaflet-control-zoom-in']] },
  { hash: '#/packets', fixed: true, controls: [
    ['path-symbols', '.path-symbols-legend > summary'],
    ['pkt-refresh', '[data-action="pkt-refresh"]'],
    ['pkt-pause', '#pktPauseBtn'],
    ['pkt-byop', '[data-action="pkt-byop"]'],
  ] },
  { hash: '#/nodes', fixed: true, controls: [
    ['nodeSearch', '#nodeSearch'],
    ['nodes-export', '#nodesExportBtn'],
  ] },
  { hash: 'NODE_DETAIL', fixed: false, controls: [['nodeBackBtn', '#nodeBackBtn']] },
  { hash: '#/channels', fixed: true, controls: [['chAddChannelBtn', '#chAddChannelBtn']] },
];

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #894 pill never covers page controls, against ${BASE} ===`);

  const nodes = await (await fetch(BASE + '/api/nodes?limit=1')).json().catch(() => null);
  const pk = nodes && (nodes.nodes || nodes)[0] && (nodes.nodes || nodes)[0].public_key;
  assert(!!pk, 'fixture has a node for the detail page');

  for (const V of VIEWPORTS) {
    console.log(`\n── ${V.name}${V.phone ? ' (touch, cold start)' : ''}`);
    const ctx = await browser.newContext(V.phone
      ? { viewport: V.vp, isMobile: true, hasTouch: true, deviceScaleFactor: 2 }
      : { viewport: V.vp });
    const page = await ctx.newPage();
    const errors = [];
    page.on('pageerror', (e) => errors.push(String(e)));
    page.on('console', (m) => { if (m.type() === 'error' && !/Failed to load resource|tile|net::/i.test(m.text())) errors.push(m.text()); });
    await ctx.route('**/api/healthz', (route) => route.fulfill({
      status: 200, contentType: 'application/json', headers: { 'X-Corescope-Load-Status': 'ready' },
      body: JSON.stringify({
        ready: true, from_pubkey_backfill: { done: true, processed: 1, total: 1 },
        ingest_liveness: { 'mqtt-eu': { lastReceiptUnix: Date.now() / 1000 - 3 * 3600 } },
      }),
    }));
    await page.goto(BASE + '/#/home', { waitUntil: 'domcontentloaded' });
    await page.waitForFunction(() => document.querySelectorAll('#warmup-stale-pills .warmup-pill').length === 1, null, { timeout: 10000 }).catch(() => {});

    const covered = {};
    for (const P of PAGES) {
      const hash = P.hash === 'NODE_DETAIL' ? '#/nodes/' + encodeURIComponent(pk || '') : P.hash;
      await page.evaluate((h) => { location.hash = h; }, hash);
      await page.waitForSelector(P.controls[0][1], { state: 'attached', timeout: 15000 }).catch(() => {});
      await page.waitForTimeout(1200); // let the page settle (map tiles, lists)
      for (let i = 0; i < 4; i++) { // dismiss one-time gesture tips that overlay the page
        const b = await page.$('.gesture-hint-dismiss');
        if (!b || !(await b.isVisible())) break;
        await b.click(); await page.waitForTimeout(250);
      }
      const r = await page.evaluate(({ controls, fixed }) => {
        const pills = document.getElementById('warmup-stale-pills');
        const pill = pills && pills.querySelector('.warmup-pill');
        const nav = document.querySelector('.top-nav').getBoundingClientRect();
        const app = document.getElementById('app').getBoundingClientRect();
        const pr = pills ? pills.getBoundingClientRect() : null;
        const out = { shown: !!pill && !pills.classList.contains('warmup-pills--hidden'), controls: {} };
        if (pr) {
          out.position = getComputedStyle(pills).position;
          out.zIndex = getComputedStyle(pills).zIndex;
          out.rowBottom = pr.bottom; out.navBottom = nav.bottom; out.appTop = app.top;
          out.pillTop = pill ? pill.getBoundingClientRect().top : null;
        }
        if (pill) {
          const x = pill.querySelector('.warmup-pill__close').getBoundingClientRect();
          const hit = document.elementFromPoint(x.left + x.width / 2, x.top + x.height / 2);
          out.closeHit = !!hit && hit.classList.contains('warmup-pill__close');
        }
        if (fixed) {
          const bn = document.querySelector('.bottom-nav');
          const bnTop = bn && getComputedStyle(bn).display !== 'none' ? bn.getBoundingClientRect().top : innerHeight;
          out.appBottom = app.bottom; out.limit = bnTop;
          out.scrolls = document.scrollingElement.scrollHeight > innerHeight + 1;
        }
        for (const [key, sel] of controls) {
          const el = Array.from(document.querySelectorAll(sel)).find((e) => {
            const b = e.getBoundingClientRect();
            return b.width > 0 && b.height > 0 && getComputedStyle(e).visibility !== 'hidden';
          });
          if (!el) { out.controls[key] = { present: false }; continue; }
          const b = el.getBoundingClientRect();
          const cx = b.left + b.width / 2, cy = b.top + b.height / 2;
          if (cx < 0 || cy < 0 || cx > innerWidth || cy > innerHeight) { out.controls[key] = { present: false, offscreen: true }; continue; }
          const hit = document.elementFromPoint(cx, cy);
          out.controls[key] = {
            present: true, ok: !!hit && (hit === el || el.contains(hit)),
            hit: hit ? (hit.id ? '#' + hit.id : hit.tagName.toLowerCase() + (hit.className && typeof hit.className === 'string' ? '.' + hit.className.split(' ').join('.') : '')) : null,
          };
        }
        return out;
      }, { controls: P.controls, fixed: P.fixed });

      const label = P.hash === 'NODE_DETAIL' ? '#/nodes/<pk>' : P.hash;
      assert(r.shown, `${label}: a pill is shown`);
      assert(r.position === 'static' && r.zIndex === 'auto', `${label}: pill row in normal flow (position ${r.position}, z-index ${r.zIndex})`);
      assert(r.pillTop >= r.navBottom - 0.5 && r.appTop >= r.rowBottom - 0.5,
        `${label}: pill below the nav, #app below the row (nav ${r.navBottom}, pill ${r.pillTop}, row bottom ${r.rowBottom}, app ${r.appTop})`);
      assert(r.closeHit, `${label}: the pill's × is hit by itself`);
      if (P.fixed) {
        // +3: the bottom nav is 2px taller than --bottom-nav-reserve already
        // on master (pill row or not); the row itself would add ~54px.
        assert(r.appBottom <= r.limit + 3 && !r.scrolls,
          `${label}: fixed page fits (app bottom ${r.appBottom} ≤ ${r.limit}, page scrolls: ${r.scrolls})`);
      }
      for (const [key] of P.controls) {
        const c = r.controls[key];
        if (c.present) {
          covered[key] = true;
          assert(c.ok, `${label}: ${key} is the topmost element at its centre (hit ${c.hit})`);
        } else {
          console.log(`  ·  ${label}: ${key} not on screen at this width${c.offscreen ? ' (off-screen)' : ''}`);
        }
      }
    }
    for (const key of V.required) assert(covered[key], `${V.name}: required control ${key} was checked`);
    assert(errors.length === 0, `${V.name}: no console errors` + (errors.length ? ': ' + errors.join(' | ') : ''));
    await ctx.close();
  }

  // ── real-tap walk, 375x560, cold start ─────────────────────────────────
  console.log('\n── real-tap walk at 375x560 (touch, cold start, bottom-nav taps)');
  {
    const ctx = await browser.newContext({ viewport: { width: 375, height: 560 }, isMobile: true, hasTouch: true, deviceScaleFactor: 2 });
    const page = await ctx.newPage();
    const errors = [];
    page.on('pageerror', (e) => errors.push(String(e)));
    await ctx.route('**/api/healthz', (route) => route.fulfill({
      status: 200, contentType: 'application/json', headers: { 'X-Corescope-Load-Status': 'ready' },
      body: JSON.stringify({
        ready: true, from_pubkey_backfill: { done: true, processed: 1, total: 1 },
        ingest_liveness: { 'mqtt-eu': { lastReceiptUnix: Date.now() / 1000 - 3 * 3600 } },
      }),
    }));
    await page.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
    await page.waitForFunction(() => document.querySelectorAll('#warmup-stale-pills .warmup-pill').length === 1, null, { timeout: 10000 }).catch(() => {});
    await page.waitForTimeout(1500);
    const settle = async () => {
      await page.waitForTimeout(1200);
      // One-time gesture tips are not under test: dismiss them directly.
      await page.evaluate(() => document.querySelectorAll('.gesture-hint-dismiss').forEach((b) => b.click()));
      await page.waitForTimeout(250);
    };
    const tapNav = async (route) => { await settle(); await page.tap(`.bottom-nav [data-route="${route}"]`); await settle(); };
    // Tap the centre of `sel` for real and report which element got the click.
    const tapControl = async (sel) => {
      await page.waitForSelector(sel, { state: 'visible', timeout: 15000 }).catch(() => {});
      const c = await page.evaluate((sel) => {
        const el = document.querySelector(sel);
        if (!el) return null;
        window.__tapHit = null;
        document.addEventListener('click', (e) => {
          window.__tapHit = el.contains(e.target) ? 'control' : (e.target.id || e.target.className || e.target.tagName);
        }, { capture: true, once: true });
        const b = el.getBoundingClientRect();
        return { x: b.left + b.width / 2, y: b.top + b.height / 2,
          pill: document.querySelectorAll('#warmup-stale-pills .warmup-pill').length };
      }, sel);
      if (!c) return { ok: false, why: 'not found' };
      await page.touchscreen.tap(c.x, c.y);
      await page.waitForTimeout(400);
      const hit = await page.evaluate(() => window.__tapHit);
      return { ok: hit === 'control' && c.pill === 1, why: `hit ${hit}, pills ${c.pill}` };
    };
    const steps = [
      ['packets', '.path-symbols-legend > summary', 'Packets: Path symbols'],
      ['live', '.live-page .leaflet-control-zoom-in', 'Live: zoom-in'],
      // zoom-in first: the toggle opens the controls panel over it.
      ['map', '.leaflet-control-zoom-in', 'Map: zoom-in'],
      [null, '#mapControlsToggle', 'Map: controls toggle'],
      ['channels', '#chAddChannelBtn', 'Channels: + Add'],
    ];
    for (const [route, sel, label] of steps) {
      if (route) await tapNav(route);
      const r = await tapControl(sel);
      assert(r.ok, `${label}: a real tap lands on the control with the pill shown (${r.why})`);
      if (route === 'channels') { await page.keyboard.press('Escape'); await page.waitForTimeout(300); }
    }
    await settle();
    await page.tap('.bottom-nav [data-route="more"]'); await page.waitForTimeout(400);
    await page.tap('.bottom-nav-sheet-item[data-route="nodes"]'); await settle();
    let r = await tapControl('#nodeSearch');
    assert(r.ok, `Nodes: search field takes a real tap (${r.why})`);
    if (pk) {
      // Open a node page as a deep link, then go back with a real tap.
      await page.evaluate((h) => { location.hash = h; }, '#/nodes/' + encodeURIComponent(pk)); await settle();
      r = await tapControl('#nodeBackBtn');
      await page.waitForTimeout(500);
      const hash = await page.evaluate(() => location.hash);
      assert(r.ok && /^#\/nodes\/?$/.test(hash), `Node page: Back takes a real tap and returns to the list (${r.why}, ${hash})`);
    }
    assert(errors.length === 0, 'real-tap walk: no page errors' + (errors.length ? ': ' + errors.join(' | ') : ''));
    await ctx.close();
  }

  await browser.close();
  console.log(failures ? `\n${failures} FAILED` : '\nall passed');
  process.exit(failures ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
