/**
 * E2E: Live "Moments" (#881 M2).
 *
 * The server keeps moments off by default, so the moments block of
 * /api/config/client is added by interception, GET /api/moments is stubbed
 * and the app's shared WebSocket is replaced with page.routeWebSocket, which
 * sends the moment frames. Nothing here needs moments on in the server.
 *
 *   1. Without the config block: no UI, no /api/moments request, no toggle.
 *   2. With it: /api/moments is asked once; WS frames become cards, at most
 *      3 at 1440x900; the drawer lists history newest first and Esc closes
 *      it; cards leave after the dwell; pause sends frames to history only.
 *   3. Clicking a card moves the map centre to the payload position and
 *      draws the highlight; the Details href is the node route.
 *   4. Hit test at 1024x768, 1440x900 (legend in all 4 corners) and 375x560:
 *      no Leaflet control, gear, legend toggle/close, feed button or VCR
 *      button is covered by a moment element. Short landscape (740x360,
 *      568x320): frames go to history only and the bar covers nothing.
 *   5. Reduced motion: the click jumps (setView, no animation) and the ring
 *      is static.
 *   6. ?moments=0 starts off and is saved, with no controller at all: no
 *      DOM, no /api/moments request, no WS listener. The toggle turns it on
 *      (one request, cards) and off again (all gone, saved).
 *   7. VCR: leaving LIVE drops the visible cards and sends frames to
 *      history only; back to LIVE shows cards again. A follow (#880)
 *      holds them the same way: a deep-linked follow at 720x900 sends
 *      frames to history only, one started with a card up drops it, and
 *      closing it shows cards again.
 *   8. Teardown: Live → Packets → Live three times, without waiting for
 *      Packets' init, leaves one controller, one moments root, the current
 *      Moments WS handler exactly once (no earlier one) and the same number
 *      of WS listeners in total; leaving Live within 200 ms of a resize
 *      raises no page error; Packets' pause badge does not count a moment
 *      frame.
 *   9. Phone real taps from a cold start (Home, then the Live tab of the
 *      bottom nav) at 375x560 and 360x640 with touch: one card and a +1
 *      chip; tap the card (map moves) and it still leaves after the dwell,
 *      the queued one following; with .live-page scrolled sideways the
 *      bottom sheet still sits inside the viewport; tap the backdrop
 *      (closes); with a card up, Settings and Fullscreen still take their
 *      taps.
 *
 * Usage: BASE_URL=http://localhost:13581 node test-881-live-moments-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
if (!['localhost', '127.0.0.1', '[::1]'].includes(new URL(BASE).hostname)) throw new Error('local server only');

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + (e && e.stack || e)); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

const PK = (c) => c.repeat(64);
const VIEW = 'lat=55.3&lon=9.6&zoom=9';
const at = (agoS) => new Date(Date.now() - agoS * 1000).toISOString();
function nodeMoment(id, lat, lon, name) {
  return { id, kind: 'new_node', at: at(2), node: { publicKey: PK('a'), name: name || 'E2E-881 ' + id, role: 'repeater', lat, lon } };
}
const REST = [
  { id: 'rest-2', kind: 'back_online', at: at(600), silentHours: 37, node: { publicKey: PK('b'), name: 'E2E-881 back', role: 'repeater', lat: 55.1, lon: 9.1 } },
  { id: 'rest-1', kind: 'record_link', at: at(1200), link: { a: { publicKey: PK('c'), name: 'A', role: 'repeater', lat: 55, lon: 9 },
    b: { publicKey: PK('d'), name: 'B', role: 'room', lat: 55.5, lon: 10 }, distanceKm: 61.2 } },
];

// open: a context with the moments block (unless off), the stubbed history
// and a mocked socket. Returns the page and a send() for moment frames.
async function open(browser, opts) {
  opts = opts || {};
  const ctx = await browser.newContext(Object.assign({ viewport: opts.viewport || { width: 1440, height: 900 } },
    opts.touch ? { hasTouch: true, isMobile: true } : {}, opts.reducedMotion ? { reducedMotion: 'reduce' } : {}));
  const h = { ctx, momentsRequests: 0, sockets: [], errors: [] };
  if (!opts.off) {
    await ctx.route(/\/api\/config\/client(\?|$)/, async (route) => {
      const res = await route.fetch();
      const body = await res.json();
      body.moments = { enabled: true, historySize: 50 };
      await route.fulfill({ response: res, json: body });
    });
  }
  await ctx.route(/\/api\/moments(\?|$)/, (route) => {
    h.momentsRequests++;
    return route.fulfill({ json: { moments: REST } });
  });
  await ctx.routeWebSocket(/.*/, (ws) => { h.sockets.push(ws); });
  const page = await ctx.newPage();
  page.on('pageerror', (e) => h.errors.push(String(e.stack || e.message).split('\n').slice(0, 4).join(' | ')));
  h.page = page;
  h.send = async (frames) => {
    const ws = h.sockets[h.sockets.length - 1];
    assert(ws, 'the app socket connected');
    for (const f of [].concat(frames)) ws.send(JSON.stringify(f));
  };
  h.moment = (m) => h.send({ type: 'moment', data: m });
  // ready waits for Live and, with the server flag on, for initMoments:
  // the controller, or only the toggle when the viewer has Moments off.
  h.ready = async (viewerOff) => {
    await page.waitForFunction(() => window._liveWSHandler && window._liveWSHandler() && window._liveGetMap && window._liveGetMap(), null, { timeout: 20000 });
    await page.waitForFunction(() => window.MeshConfigReady !== undefined, null, { timeout: 5000 });
    await page.evaluate(() => window.MeshConfigReady);
    if (opts.off) return;
    if (viewerOff) await page.waitForFunction(() => getComputedStyle(document.getElementById('liveMomentsLabel')).display !== 'none', null, { timeout: 10000 });
    else await page.waitForFunction(() => window._liveMoments && window._liveMoments(), null, { timeout: 10000 });
  };
  h.goto = async (hash, viewerOff) => {
    await page.goto(BASE + '/?r=' + Date.now() + '#/live?' + (hash || VIEW), { waitUntil: 'domcontentloaded' });
    await h.ready(viewerOff);
  };
  return h;
}

async function cardCount(page) {
  return page.evaluate(() => document.querySelectorAll('.live-moment-card:not(.is-leaving)').length);
}

// controlsCovered lists every visible control whose centre hits a moment
// element instead of the control.
async function controlsCovered(page) {
  return page.evaluate(() => {
    const sel = ['.leaflet-control a', '.leaflet-control button', '#liveControlsToggle', '#liveFullscreenToggle',
      '#legendToggleBtn', '#legendCloseBtn', '#feedShowBtn', '#feedHideBtn', '.vcr-bar button',
      '.panel-corner-btn', '#liveHeaderToggle'].join(',');
    const bad = [];
    let checked = 0;
    for (const el of document.querySelectorAll(sel)) {
      const r = el.getBoundingClientRect();
      if (!r.width || !r.height || getComputedStyle(el).visibility === 'hidden') continue;
      const x = r.left + r.width / 2, y = r.top + r.height / 2;
      if (x < 0 || y < 0 || x > innerWidth || y > innerHeight) continue;
      checked++;
      const hit = document.elementFromPoint(x, y);
      if (hit && hit.closest('.live-moments, .live-moments-drawer')) {
        const by = hit.getBoundingClientRect();
        // Both rects, so a CI-only overlap can be read from the log.
        bad.push((el.id || el.className || el.tagName) + ' ' + [r.left, r.top, r.right, r.bottom].map(Math.round).join(',') +
          ' under ' + (hit.className && hit.className.baseVal !== undefined ? hit.tagName : hit.className) + ' ' + [by.left, by.top, by.right, by.bottom].map(Math.round).join(','));
      }
    }
    return { bad, checked };
  });
}

async function fillCards(h, n) {
  const frames = [];
  for (let i = 0; i < n; i++) frames.push({ type: 'moment', data: nodeMoment('fill-' + Date.now() + '-' + i, 55.2 + i * 0.01, 9.4) });
  await h.send(frames);
  await h.page.waitForFunction((n) => document.querySelectorAll('.live-moment-card').length >= n, Math.min(n, 3));
}

(async () => {
  const browser = await chromium.launch({ headless: true, executablePath: process.env.CHROMIUM_PATH || undefined });
  try {
    console.log('\n=== #881 M2 Live moments E2E ===');

    await step('server flag off: no moments UI, no /api/moments request, no toggle', async () => {
      const h = await open(browser, { off: true });
      await h.goto();
      await h.page.waitForTimeout(500);
      const s = await h.page.evaluate(() => ({
        root: document.querySelectorAll('.live-moments').length,
        ctrl: !!(window._liveMoments && window._liveMoments()),
        label: getComputedStyle(document.getElementById('liveMomentsLabel')).display,
      }));
      await h.moment(nodeMoment('off-1', 55.3, 9.6));
      await h.page.waitForTimeout(200);
      assert(s.root === 0 && !s.ctrl, 'no moments UI: ' + JSON.stringify(s));
      assert(s.label === 'none', 'toggle hidden');
      assert(h.momentsRequests === 0, '/api/moments not requested');
      assert(await cardCount(h.page) === 0);
      assert(!h.errors.length, h.errors.join('; '));
      await h.ctx.close();
    });

    const h = await open(browser);
    await h.goto();

    await step('history is fetched once; frames become at most 3 cards with a +N chip', async () => {
      assert(h.momentsRequests === 1, 'one /api/moments request, got ' + h.momentsRequests);
      const frames = [];
      for (let i = 0; i < 5; i++) frames.push({ type: 'moment', data: nodeMoment('ws-' + i, 55.3 + i * 0.01, 9.6) });
      frames.push({ type: 'packet', data: {} });
      frames.push({ type: 'moment', data: nodeMoment('ws-4', 55.3, 9.6) }); // duplicate id
      await h.send(frames);
      await h.page.waitForFunction(() => document.querySelectorAll('.live-moment-card').length === 3);
      const s = await h.page.evaluate(() => ({
        count: document.querySelector('.live-moments-count').textContent,
        more: document.querySelector('.live-moments-more').textContent,
        newest: [...document.querySelectorAll('.live-moment-card')].pop().getAttribute('data-moment-id'),
      }));
      assert(s.count === 'Moments · 7', s.count);
      assert(s.more === '+2', s.more);
      assert(s.newest === 'ws-2', 'newest shown card nearest the anchor: ' + s.newest);
    });

    await step('drawer lists history newest first; Escape closes it', async () => {
      await h.page.click('.live-moments-bar .live-moments-open');
      await h.page.waitForSelector('.live-moments-drawer:not([hidden])');
      const ids = await h.page.$$eval('.live-moment-row', (r) => r.map((e) => e.getAttribute('data-moment-id')));
      assert(ids.join(',') === 'ws-4,ws-3,ws-2,ws-1,ws-0,rest-2,rest-1', ids.join(','));
      await h.page.keyboard.press('Escape');
      await h.page.waitForSelector('.live-moments-drawer', { state: 'hidden' });
    });

    await step('Details link is the node route, never #/live?node=', async () => {
      const hrefs = await h.page.$$eval('.live-moment-card .live-moment-link', (a) => a.map((e) => e.getAttribute('href')));
      assert(hrefs.length && hrefs.every((x) => x === '#/nodes/' + 'a'.repeat(64)), hrefs.join(','));
    });

    await step('click moves the map centre to the moment and draws the ring', async () => {
      await h.page.evaluate(() => window._liveGetMap().setView([54, 8], 8, { animate: false }));
      const id = await h.page.$eval('.live-moment-card', (e) => e.getAttribute('data-moment-id'));
      const target = { 'ws-0': 55.3, 'ws-1': 55.31, 'ws-2': 55.32 }[id];
      await h.page.click('.live-moment-card[data-moment-id="' + id + '"] .live-moment-main');
      await h.page.waitForFunction((lat) => {
        const c = window._liveGetMap().getCenter();
        return Math.abs(c.lat - lat) < 1e-3 && Math.abs(c.lng - 9.6) < 1e-3 && window._liveGetMap().getZoom() >= 12;
      }, target, { timeout: 5000 });
      assert(await h.page.$('.live-moment-hl'), 'highlight ring drawn');
    });

    await step('cards leave after the 8 s dwell, then queued ones follow', async () => {
      await h.page.mouse.move(5, 5);
      await h.page.waitForFunction(() => {
        const ids = [...document.querySelectorAll('.live-moment-card:not(.is-leaving)')].map((e) => e.getAttribute('data-moment-id'));
        return ids.length === 2 && ids.includes('ws-3') && ids.includes('ws-4');
      }, null, { timeout: 12000 });
      await h.page.waitForFunction(() => !document.querySelector('.live-moment-card'), null, { timeout: 12000 });
    });

    await step('pause sends frames to history only', async () => {
      await h.page.click('.live-moments-bar .live-moments-pause');
      await h.moment(nodeMoment('paused-1', 55.3, 9.6));
      await h.page.waitForFunction(() => document.querySelector('.live-moments-count').textContent.startsWith('Moments · 8'));
      assert(await cardCount(h.page) === 0, 'no card while paused');
      await h.page.click('.live-moments-bar .live-moments-pause');
      await h.moment(nodeMoment('resumed-1', 55.3, 9.6));
      await h.page.waitForFunction(() => document.querySelectorAll('.live-moment-card').length === 1);
    });

    await step('VCR away from LIVE drops the cards and sends frames to history only; LIVE shows them again', async () => {
      assert(await cardCount(h.page) === 1, 'a card is up');
      await h.page.click('#vcrPauseBtn');
      await h.page.waitForFunction(() => window._liveVCR().mode === 'PAUSED');
      await h.page.waitForFunction(() => !document.querySelector('.live-moment-card'), null, { timeout: 2000 });
      await h.moment(nodeMoment('vcr-paused', 55.3, 9.6));
      await h.page.waitForFunction(() => document.querySelector('.live-moments-count').textContent === 'Moments · 10');
      assert(await cardCount(h.page) === 0, 'no card while the VCR is paused');
      await h.page.click('#vcrLiveBtn');
      await h.page.waitForFunction(() => window._liveVCR().mode === 'LIVE');
      await h.moment(nodeMoment('vcr-live', 55.3, 9.6));
      await h.page.waitForSelector('.live-moment-card[data-moment-id="vcr-live"]');
    });

    for (const vp of [{ width: 1024, height: 768 }, { width: 1440, height: 900 }]) {
      await step('hit test ' + vp.width + 'x' + vp.height + ', legend in all 4 corners: no control covered', async () => {
        await h.page.setViewportSize(vp);
        for (const corner of ['tl', 'tr', 'br', 'bl']) {
          await h.page.evaluate((c) => localStorage.setItem('panel-corner-liveLegend', c), corner);
          await h.goto();
          await fillCards(h, 3);
          const pos = await h.page.$eval('#liveLegend', (e) => e.getAttribute('data-position'));
          assert(pos === corner, 'legend in ' + corner + ', got ' + pos);
          const r = await controlsCovered(h.page);
          assert(r.checked >= 6, 'checked ' + r.checked + ' controls');
          assert(!r.bad.length, 'covered at legend ' + corner + ': ' + r.bad.join(', '));
        }
      });
    }

    for (const vp of [{ width: 740, height: 360 }, { width: 568, height: 320 }]) {
      await step('short landscape ' + vp.width + 'x' + vp.height + ': history only, the bar covers no control', async () => {
        await h.page.setViewportSize(vp);
        await h.goto();
        await h.moment(nodeMoment('land-' + vp.width, 55.3, 9.6));
        await h.page.waitForFunction((id) => [...document.querySelectorAll('.live-moments-count')].some((e) => e.textContent.startsWith('Moments · ')), 'land');
        await h.page.waitForTimeout(100);
        assert(await cardCount(h.page) === 0, 'no card on a short viewport');
        const r = await controlsCovered(h.page);
        assert(r.checked >= 5 && !r.bad.length, JSON.stringify(r));
        if (vp.width <= 640) {
          // Font-independent: the button is left of the whole control row,
          // not merely below it (the gap below depends on the VCR bar height).
          const g = await h.page.evaluate(() => {
            const bar = document.querySelector('.live-moments-bar').getBoundingClientRect();
            const ctl = [...document.querySelectorAll('.leaflet-top.leaflet-right .leaflet-control')]
              .map((e) => e.getBoundingClientRect()).filter((x) => x.width && x.height);
            return { barRight: bar.right, barW: bar.width, rowLeft: Math.min(...ctl.map((x) => x.left)) };
          });
          assert(g.barW > 0 && g.barRight <= g.rowLeft, 'drawer button left of the control row: ' + JSON.stringify(g));
        }
      });
    }
    await h.page.setViewportSize({ width: 1440, height: 900 });
    await h.goto();

    await step('teardown: Live → Packets → Live x3 keeps one controller and stable WS listeners', async () => {
      // No wait for Packets' init before leaving: that is the hop that
      // leaked Packets' WS handler before #881 F2.
      const base = await h.page.evaluate(() => {
        window.__e2eMomentHandlers = [window._liveMoments().wsHandler];
        return { ws: wsListeners.length, live: LiveMoments.liveCount() };
      });
      base.requests = h.momentsRequests;
      assert(base.live === 1, 'one controller on Live, got ' + base.live);
      for (let i = 0; i < 3; i++) {
        await h.page.evaluate(() => { location.hash = '#/packets'; });
        await h.page.waitForFunction(() => !document.querySelector('.live-page'));
        const off = await h.page.evaluate(() => ({ live: LiveMoments.liveCount(), roots: document.querySelectorAll('.live-moments, .live-moments-drawer').length }));
        assert(off.live === 0 && off.roots === 0, 'nothing left on Packets: ' + JSON.stringify(off));
        await h.page.evaluate((v) => { location.hash = '#/live?' + v; }, VIEW);
        await h.page.waitForFunction(() => window._liveMoments && window._liveMoments(), null, { timeout: 20000 });
        await h.page.evaluate(() => { window.__e2eMomentHandlers.push(window._liveMoments().wsHandler); });
      }
      const after = await h.page.evaluate(() => {
        const all = window.__e2eMomentHandlers, cur = all[all.length - 1];
        return { ws: wsListeners.length, live: LiveMoments.liveCount(), roots: document.querySelectorAll('.live-moments').length,
          current: wsListeners.filter((f) => f === cur).length,
          stale: wsListeners.filter((f) => all.indexOf(f) !== -1 && f !== cur).length,
          distinct: new Set(all).size };
      });
      assert(after.distinct === 4, 'a new controller per entry: ' + JSON.stringify(after));
      assert(after.current === 1 && after.stale === 0, 'the current Moments handler once, no earlier one: ' + JSON.stringify(after));
      assert(after.ws === base.ws, 'WS listeners ' + base.ws + ' → ' + after.ws);
      assert(after.live === 1 && after.roots === 1, JSON.stringify(after));
      assert(h.momentsRequests === base.requests + 3, 'one history fetch per entry: ' + base.requests + ' → ' + h.momentsRequests);
      assert(!h.errors.length, h.errors.join('; '));
    });

    await step('leaving Live within 200 ms of a resize raises no page error', async () => {
      // Leaflet's remove() keeps invalidateSize's debounced moveend timer, so
      // Live's moveend handler ran after destroy() with map === null (found
      // while hit-testing at two viewport sizes).
      const before = h.errors.length;
      await h.page.setViewportSize({ width: 1300, height: 850 });
      // Let Leaflet's resize frame run invalidateSize (which arms the timer).
      await h.page.evaluate(() => new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r))));
      await h.page.evaluate(() => { location.hash = '#/packets'; });
      await h.page.waitForTimeout(400);
      await h.page.setViewportSize({ width: 1440, height: 900 });
      assert(h.errors.length === before, h.errors.slice(before).join('; '));
    });

    await step('Packets pause badge ignores moment frames', async () => {
      await h.page.evaluate(() => { location.hash = '#/packets'; });
      await h.page.waitForSelector('#pktPauseBtn');
      await h.page.click('#pktPauseBtn');
      await h.send([{ type: 'moment', data: nodeMoment('pk-1', 55, 9) }, { type: 'moment', data: nodeMoment('pk-2', 55, 9) },
        { type: 'packet', data: { packet: { id: 1, hash: 'e2e881', payload_type: 4 } } }]);
      await h.page.waitForFunction(() => /\b1$/.test(document.getElementById('pktPauseBtn').textContent.trim()), null, { timeout: 5000 });
    });
    await h.ctx.close();

    await step('reduced motion: the click jumps without animation and the ring is static', async () => {
      const r = await open(browser, { reducedMotion: true });
      await r.goto();
      await r.moment(nodeMoment('rm-1', 55.45, 9.75));
      await r.page.waitForSelector('.live-moment-card');
      await r.page.evaluate(() => window._liveGetMap().setView([54, 8], 8, { animate: false }));
      const c = await r.page.evaluate(() => {
        document.querySelector('.live-moment-card .live-moment-main').click();
        const m = window._liveGetMap(); const c = m.getCenter();
        return { lat: c.lat, lng: c.lng, zoom: m.getZoom(), anim: getComputedStyle(document.querySelector('.live-moment-hl')).animationName,
          staticRing: document.querySelector('.live-moment-hl').classList.contains('is-static') };
      });
      assert(Math.abs(c.lat - 55.45) < 1e-6 && Math.abs(c.lng - 9.75) < 1e-6 && c.zoom === 12, 'synchronous jump: ' + JSON.stringify(c));
      assert(c.staticRing && c.anim === 'none', 'static ring: ' + JSON.stringify(c));
      await r.ctx.close();
    });

    // #880: a follow holds the live stream, so it holds the cards too. At
    // <= 768px the follow card is a bottom sheet over the cards' anchor; a
    // card there would sit unseen under it and still be announced.
    await step('a follow holds the cards (history only) at 720x900 and drops one that is up; closing it shows cards again', async () => {
      const f = await open(browser, { viewport: { width: 720, height: 900 } });
      await f.ctx.route(/\/api\/packets\/881f0110a5ed0001(\?|$)/, (route) => route.fulfill({ status: 404, json: {} }));
      await f.goto(VIEW + '&follow=881f0110a5ed0001');
      await f.page.waitForFunction(() => window._liveFollowState && window._liveFollowState() && document.querySelector('.live-follow-card'));
      await f.page.waitForFunction(() => document.querySelector('.live-moments-count').textContent === 'Moments · 2');
      await f.moment(nodeMoment('follow-held', 55.3, 9.6));
      await f.page.waitForFunction(() => document.querySelector('.live-moments-count').textContent === 'Moments · 3');
      await f.page.waitForTimeout(300);
      const held = await f.page.evaluate(() => Object.assign({ cards: document.querySelectorAll('.live-moment-card').length },
        window._liveMoments().stats()));
      assert(held.cards === 0 && held.visible === 0 && held.queued === 0, 'no card while following: ' + JSON.stringify(held));
      await f.page.click('.live-follow-card .lf-close');
      await f.page.waitForFunction(() => !window._liveFollowState() && !document.querySelector('.live-follow-card'));
      assert(await cardCount(f.page) === 0, 'a held moment does not pop up after the follow');
      await f.moment(nodeMoment('after-follow', 55.3, 9.6));
      await f.page.waitForSelector('.live-moment-card[data-moment-id="after-follow"]');
      // A follow started with a card up drops it.
      await f.page.evaluate(() => window._liveStartFollow('881f0110a5ed0001'));
      await f.page.waitForFunction(() => !document.querySelector('.live-moment-card'), null, { timeout: 2000 });
      await f.page.evaluate(() => window._liveStopFollow());
      assert(!f.errors.length, f.errors.join('\n'));
      await f.ctx.close();
    });

    await step('?moments=0: no controller (no DOM, request or WS listener), saved; the toggle turns it on and off', async () => {
      const o = await open(browser);
      await o.goto(VIEW + '&moments=0', true);
      await o.page.waitForTimeout(300);
      const s = await o.page.evaluate(() => ({ stored: localStorage.getItem('live-moments'),
        dom: document.querySelectorAll('.live-moments, .live-moments-drawer, .live-moments-backdrop').length,
        ctrl: !!window._liveMoments(), live: LiveMoments.liveCount(),
        checked: document.getElementById('liveMomentsToggle').checked }));
      assert(s.stored === 'false' && s.dom === 0 && !s.ctrl && s.live === 0 && !s.checked, JSON.stringify(s));
      assert(o.momentsRequests === 0, '/api/moments not requested while off, got ' + o.momentsRequests);
      const ws0 = await o.page.evaluate(() => wsListeners.length);
      await o.moment(nodeMoment('off-ws', 55, 9));
      await o.page.waitForTimeout(200);
      assert(await cardCount(o.page) === 0);

      await o.page.click('#liveControlsToggle');
      await o.page.check('#liveMomentsToggle');
      const t = await o.page.evaluate(() => ({ stored: localStorage.getItem('live-moments'), hash: location.hash,
        ws: wsListeners.length, live: LiveMoments.liveCount(), roots: document.querySelectorAll('.live-moments').length }));
      assert(t.stored === 'true' && !/moments=/.test(t.hash), JSON.stringify(t));
      assert(t.ws === ws0 + 1 && t.live === 1 && t.roots === 1, 'one controller and one listener once on: ' + JSON.stringify(t));
      await o.page.waitForFunction(() => document.querySelector('.live-moments-count') && /Moments · 2/.test(document.querySelector('.live-moments-count').textContent));
      assert(o.momentsRequests === 1, 'history fetched once on turning on, got ' + o.momentsRequests);
      await o.page.click('#liveControlsToggle');
      await o.moment(nodeMoment('on-ws', 55, 9));
      await o.page.waitForSelector('.live-moment-card');

      await o.page.click('#liveControlsToggle');
      await o.page.uncheck('#liveMomentsToggle');
      const u = await o.page.evaluate(() => ({ stored: localStorage.getItem('live-moments'), hash: location.hash, ws: wsListeners.length,
        live: LiveMoments.liveCount(), dom: document.querySelectorAll('.live-moments, .live-moments-drawer, .live-moments-backdrop').length }));
      assert(u.stored === 'false' && /moments=0/.test(u.hash), JSON.stringify(u));
      assert(u.ws === ws0 && u.live === 0 && u.dom === 0, 'all gone once off: ' + JSON.stringify(u));
      assert(!o.errors.length, o.errors.join('; '));
      await o.ctx.close();
    });

    for (const vp of [{ width: 375, height: 560 }, { width: 360, height: 640 }]) {
      await step('phone ' + vp.width + 'x' + vp.height + ' real taps from a cold start', async () => {
        const p = await open(browser, { viewport: vp, touch: true });
        // Cold start: Home first, then the Live tab of the bottom nav.
        await p.page.goto(BASE + '/?r=' + Date.now() + '#/', { waitUntil: 'domcontentloaded' });
        await p.page.waitForSelector('[data-bottom-nav-tab="live"]', { state: 'visible', timeout: 20000 });
        await p.page.evaluate(() => localStorage.setItem('live-map-view', JSON.stringify({ lat: 55.3, lng: 9.6, zoom: 9 })));
        await p.page.tap('[data-bottom-nav-tab="live"]');
        await p.ready();
        await p.send([{ type: 'moment', data: nodeMoment('ph-1', 55.4, 9.7) }, { type: 'moment', data: nodeMoment('ph-2', 55.41, 9.71) }]);
        await p.page.waitForSelector('.live-moment-card');
        const s = await p.page.evaluate(() => {
          const card = document.querySelector('.live-moment-card').getBoundingClientRect();
          const rail = document.querySelector('.leaflet-top.leaflet-right').getBoundingClientRect();
          const btns = [...document.querySelectorAll('.live-moments-bar button')].filter((b) => b.getClientRects().length)
            .map((b) => { const r = b.getBoundingClientRect(); return [r.width, r.height]; });
          return { cards: document.querySelectorAll('.live-moment-card').length, more: document.querySelector('.live-moments-more').textContent,
            cardRight: card.right, cardH: card.height, railLeft: rail.left, btns, overflow: document.documentElement.scrollWidth - innerWidth };
        });
        assert(s.cards === 1 && s.more === '+1', JSON.stringify(s));
        assert(s.cardRight <= s.railLeft, 'card clear of the control rail: ' + JSON.stringify(s));
        assert(s.cardH <= 56, 'card ≤ 56px tall: ' + s.cardH);
        assert(s.btns.length && s.btns.every(([w, hh]) => w >= 40 && hh >= 40), 'tap targets ≥ 40px: ' + JSON.stringify(s.btns));
        assert(s.overflow <= 0, 'no sideways scroll');
        let r = await controlsCovered(p.page);
        assert(r.checked >= 5 && !r.bad.length, 'phone hit test: ' + JSON.stringify(r));

        // Tap the card: the map flies there, and the card still leaves after
        // the dwell, with the queued moment taking its slot (#881 F3).
        await p.page.evaluate(() => window._liveGetMap().setView([54, 8], 8, { animate: false }));
        const tappedAt = Date.now();
        await p.page.tap('.live-moment-card .live-moment-main');
        await p.page.waitForFunction(() => {
          const c = window._liveGetMap().getCenter();
          return Math.abs(c.lat - 55.4) < 1e-3 && Math.abs(c.lng - 9.7) < 1e-3;
        }, null, { timeout: 5000 });
        await p.page.waitForFunction(() => {
          const ids = [...document.querySelectorAll('.live-moment-card:not(.is-leaving)')].map((e) => e.getAttribute('data-moment-id'));
          return ids.length === 1 && ids[0] === 'ph-2' && document.querySelector('.live-moments-more').textContent === '';
        }, null, { timeout: 12000 });
        assert(Date.now() - tappedAt < 11000, 'the tapped card left within its dwell');

        // The sheet sits inside the viewport even with .live-page scrolled
        // sideways, as CI's tap scroll-into-view did (#881 F4).
        await p.page.evaluate(() => { document.querySelector('.live-page').scrollLeft = 11; });
        await p.page.tap('.live-moments-bar .live-moments-open');
        await p.page.waitForSelector('.live-moments-drawer:not([hidden])');
        const sheet = await p.page.evaluate(() => {
          const d = document.querySelector('.live-moments-drawer').getBoundingClientRect();
          const b = document.querySelector('.live-moments-backdrop').getBoundingClientRect();
          const nav = document.querySelector('[data-bottom-nav]').getBoundingClientRect();
          const rows = [...document.querySelectorAll('.live-moment-row')];
          return { left: d.left, right: d.right, top: d.top, bottom: d.bottom, bdLeft: b.left, bdRight: b.right, bdTop: b.top,
            vw: innerWidth, vh: innerHeight, navTop: nav.top, lastRowBottom: rows.length ? rows[rows.length - 1].getBoundingClientRect().bottom : null,
            scrolled: document.querySelector('.live-page').scrollLeft, rows: rows.length };
        });
        assert(sheet.scrolled > 0, 'the page really was scrolled sideways: ' + JSON.stringify(sheet));
        // Full width, inside the viewport, above the bottom nav: every row
        // ends above the nav (the nav is drawn over the sheet's last pixels
        // of padding, since it is 2px taller than --bottom-nav-reserve).
        assert(Math.abs(sheet.left) < 1 && Math.abs(sheet.right - sheet.vw) < 1 && sheet.top >= 0 && sheet.bottom <= sheet.navTop + 3,
          'bottom sheet inside the viewport, above the nav: ' + JSON.stringify(sheet));
        assert(sheet.lastRowBottom !== null && sheet.lastRowBottom <= sheet.navTop, 'no row under the nav: ' + JSON.stringify(sheet));
        assert(Math.abs(sheet.bdLeft) < 1 && Math.abs(sheet.bdRight - sheet.vw) < 1 && Math.abs(sheet.bdTop) < 1,
          'backdrop covers the page area: ' + JSON.stringify(sheet));
        assert(sheet.rows === 4, 'rows ' + sheet.rows);
        await p.page.touchscreen.tap(180, 40);
        await p.page.waitForSelector('.live-moments-drawer', { state: 'hidden' });
        await p.page.evaluate(() => { document.querySelector('.live-page').scrollLeft = 0; });

        await p.moment(nodeMoment('ph-3', 55.42, 9.72));
        await p.page.waitForSelector('.live-moment-card[data-moment-id="ph-3"]');
        r = await controlsCovered(p.page);
        assert(!r.bad.length, 'covered: ' + r.bad.join(','));
        await p.page.tap('#liveControlsToggle');
        await p.page.waitForFunction(() => document.getElementById('liveControls').classList.contains('is-expanded'));
        await p.page.tap('#liveControlsToggle');
        await p.page.tap('#liveFullscreenToggle');
        await p.page.waitForFunction(() => document.body.classList.contains('live-fullscreen'));
        await p.page.tap('#liveFullscreenToggle');
        assert(!p.errors.length, p.errors.join('; '));
        await p.ctx.close();
      });
    }
  } finally {
    await browser.close();
  }
  console.log('\n' + passed + ' passed, ' + failed + ' failed');
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
