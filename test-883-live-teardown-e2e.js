/**
 * E2E: Live teardown and in-place remount hygiene (#883 M3).
 *
 * Kiosk mode remounts Live in place every 6 h (destroy() + init() in the
 * same document), so every one-shot timer armed by a mount must die with
 * it, and nothing a mount leaves behind may keep the old page alive. Each
 * case below reproduces one leak of the M0 research (§2.7) on the real
 * page with Playwright's clock: arm the timer, tear Live down (to Packets,
 * or in place to Live again), advance the clock past it.
 *
 *   1. propagationBuffer: a buffered packet is not rendered after
 *      teardown, and not into the next mount (its pkts counter stays 0).
 *   2. drawDashedPath(): the TRACE ghost timeouts raise no TypeError after
 *      teardown (pathsLayer is null then).
 *   3. pulseNode(): the ghost-marker timeout raises no TypeError after
 *      teardown, and never deletes the next mount's marker for the key.
 *      A fired liveLater() timeout leaves its set.
 *   4. The replay hand-off: the 1.5 s replay render (real fixture packets
 *      with a path) is not drawn by the next mount, and its 12 s
 *      suppressLive does not mute the next mount's live packets.
 *   5. The F / Escape keydown: after a remount F drives the current
 *      fullscreen button (not the first mount's detached one), and the
 *      window keydown listener count stays the same.
 *   6. feedDedup: a hard cap of 200 entries, the oldest dropped first.
 *   7. In-place remounts add no window/document listener (the region
 *      filter's outside-click and resize listeners) and keep the DOM node
 *      count flat (they held the whole previous page alive); after GC no
 *      detached .live-page is left, the first mount's included (the #1180
 *      once-per-page MediaQueryList handler kept it); leaving Live for a
 *      page without filters leaves no Live (F key) or region/area filter
 *      window/document listener behind, nor do Packets ↔ Nodes round trips.
 *
 * Usage: BASE_URL=http://localhost:13581 node test-883-live-teardown-e2e.js
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

async function open(browser, opts) {
  opts = opts || {};
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 720 }, reducedMotion: 'reduce' });
  await ctx.routeWebSocket(/.*/, () => {});
  // Two areas, so the area filter renders its dropdown (the fixture has none).
  if (opts.areas) await ctx.route(/\/api\/config\/areas(\?|$)/, (r) => r.fulfill({ json: [{ key: 'AAA', label: 'Area A' }, { key: 'BBB', label: 'Area B' }] }));
  if (opts.init) await ctx.addInitScript(opts.init.fn, opts.init.arg);
  const page = await ctx.newPage();
  const h = { ctx, page, errors: [] };
  page.on('pageerror', (e) => h.errors.push(String(e.message)));
  await page.clock.install();
  // Each mount gets a fresh map: wait for the one after the hash change
  // (the hashchange runs after evaluate() returns, so the old map is
  // still there at first). It returns a boolean: waitForFunction keeps a
  // handle to its result, and an element handle would keep the page.
  h.ready = async () => {
    await page.waitForFunction(() => window._liveGetMap && window._liveGetMap() && window._liveGetMap() !== window.__tdOldMap &&
      window._liveWSHandler && window._liveWSHandler() && !!document.querySelector('.live-page'), null, { timeout: 20000 });
  };
  h.go = async (hash) => {
    await page.evaluate((x) => { window.__tdOldMap = window._liveGetMap && window._liveGetMap(); location.hash = x; }, hash);
    if (hash.startsWith('#/live')) {
      await h.ready();
      await page.evaluate(() => { window.__tdOldMap = null; }); // or it keeps the old page
    } else await page.waitForFunction(() => !document.querySelector('.live-page'), null, { timeout: 20000 });
  };
  // Remount Live in place: the router always runs destroy() + init().
  let n = 0;
  h.remount = () => h.go('#/live?remount=' + (++n));
  // A timer that throws inside runFor() rejects it (Playwright rethrows).
  h.run = async (ms) => {
    try { await page.clock.runFor(ms); return null; } catch (e) { return String(e.message || e).split('\n')[0]; }
  };
  h.pkts = () => page.evaluate(() => document.getElementById('livePktCount').textContent);
  h.feedItems = () => page.evaluate(() => document.querySelectorAll('#liveFeed .live-feed-item').length);
  await page.goto(BASE + '/' + (opts.start || '#/live'));
  if (!opts.start) await h.ready();
  return h;
}

// A live packet as live.js's WS handler builds it (dbPacketToLive shape).
function advert(hash) {
  return { id: 1, hash, timestamp: new Date().toISOString(), observer: 'teardown-test', raw: '', raw_hex: '',
    decoded: { header: { payloadTypeName: 'ADVERT' }, payload: { name: 'teardown ' + hash }, path: { hops: [] } } };
}

(async () => {
  const browser = await chromium.launch({ args: ['--no-sandbox'] });
  console.log('\n=== #883 M3 Live teardown and remount E2E ===');
  try {
    // ── 1. propagationBuffer ────────────────────────────────────────────
    await step('1 propagationBuffer: a buffered packet renders neither after teardown nor into the next mount', async () => {
      const h = await open(browser, { init: { fn: () => { try { localStorage.setItem('live-realistic-propagation', 'true'); } catch (_) {} } } });
      const ms = await h.page.evaluate(() => window.PROPAGATION_BUFFER_MS);
      assert(ms > 0, 'PROPAGATION_BUFFER_MS ' + ms);
      // Control: with no teardown the buffer renders it.
      await h.page.evaluate((p) => window._liveBufferPacket(p), advert('pb000001'));
      assert(await h.pkts() === '0', 'buffered, not rendered yet');
      assert(!(await h.run(ms + 500)), 'control run');
      assert(await h.pkts() === '1', 'control: rendered after the buffer window, got ' + await h.pkts());
      // Teardown to Packets.
      await h.page.evaluate((p) => window._liveBufferPacket(p), advert('pb000002'));
      await h.go('#/packets');
      const err = await h.run(ms + 500);
      assert(!err, 'after teardown: ' + err);
      // In place: the old mount's buffer must not render into the new one.
      await h.go('#/live');
      await h.page.evaluate((p) => window._liveBufferPacket(p), advert('pb000003'));
      await h.remount();
      assert(!(await h.run(ms + 500)), 'remount run');
      assert(await h.pkts() === '0', 'the next mount rendered the old mount\'s packet: pkts ' + await h.pkts());
      assert(!h.errors.length, h.errors.join('; '));
      await h.ctx.close();
    });

    // ── 2 + 3. drawDashedPath and pulseNode ─────────────────────────────
    await step('2 drawDashedPath: TRACE ghost timeouts raise no TypeError after teardown', async () => {
      const h = await open(browser);
      const drawn = await h.page.evaluate(() => {
        const d = window._liveNodeData();
        const keys = Object.keys(d).filter((k) => d[k].lat && d[k].lon).slice(0, 3);
        let before = 0; window._liveGetMap().eachLayer(() => before++);
        window._liveBufferPacket({ hash: 'trace0001', timestamp: new Date().toISOString(), observer: 'x', raw: '',
          decoded: { header: { payloadTypeName: 'TRACE' }, path: { hops: keys.map((k) => k.slice(0, 2)), hopsCompleted: 0 }, payload: {} } });
        let after = 0; window._liveGetMap().eachLayer(() => after++);
        return after - before;
      });
      assert(drawn > 0, 'the TRACE drew no dashed path (' + drawn + ')');
      await h.go('#/packets');
      const err = await h.run(11000);
      assert(!err, 'after teardown: ' + err);
      assert(!h.errors.length, h.errors.join('; '));
      await h.ctx.close();
    });

    await step('3 pulseNode: the ghost timeout raises no TypeError after teardown', async () => {
      const h = await open(browser);
      await h.page.evaluate(() => window._liveTestSeams.triggerPulse('ghost-teardown', [37.3, -122.0], 'ADVERT'));
      assert(await h.page.evaluate(() => !!window._liveNodeMarkers()['ghost-teardown']), 'ghost marker added');
      await h.go('#/packets');
      const err = await h.run(31000);
      assert(!err, 'after teardown: ' + err);
      assert(!h.errors.length, h.errors.join('; '));
      await h.ctx.close();
    });

    await step('3 pulseNode: the old mount\'s ghost timeout never deletes the next mount\'s marker', async () => {
      const h = await open(browser);
      await h.page.evaluate(() => window._liveTestSeams.triggerPulse('ghost-remount', [37.3, -122.0], 'ADVERT'));
      await h.remount();
      assert(!(await h.run(10000)), 'run 10 s');
      // The new mount's own ghost for the same key lives 30 s from now.
      const later = () => h.page.evaluate(() => window._liveTestSeams.getLaterCount());
      const idle = await later();
      await h.page.evaluate(() => window._liveTestSeams.triggerPulse('ghost-remount', [37.3, -122.0], 'ADVERT'));
      assert(await later() > idle, 'the ghost timeout is tracked');
      assert(!(await h.run(21000)), 'run past the old ghost\'s 30 s');
      assert(await h.page.evaluate(() => !!window._liveNodeMarkers()['ghost-remount']), 'the new mount\'s marker was deleted by the old mount\'s timeout');
      assert(!(await h.run(10000)), 'run past the new ghost\'s 30 s');
      assert(await h.page.evaluate(() => !window._liveNodeMarkers()['ghost-remount']), 'the new ghost expires on its own');
      // Bounded: a fired timeout leaves the set.
      assert(await later() === idle, 'fired liveLater timeouts stay in the set: ' + idle + ' → ' + await later());
      assert(!h.errors.length, h.errors.join('; '));
      await h.ctx.close();
    });

    // ── 4. Replay hand-off ──────────────────────────────────────────────
    await step('4 replay hand-off: no replay render into the next mount; its suppressLive does not mute it', async () => {
      const h = await open(browser);
      // Real fixture packets with a multi-hop path: a replay render animates.
      const replay = await h.page.evaluate(async () => {
        const r = await fetch('/api/packets?limit=100&expand=observations');
        const d = await r.json();
        return (d.packets || []).map((p) => window._liveDbPacketToLive(Object.assign({}, p, (p.observations || [])[0] || {})))
          .filter((l) => (l.decoded.path.hops || []).length >= 2).slice(0, 3);
      });
      assert(replay.length, 'no fixture packet with a path');
      const drawn = () => h.page.evaluate(() => window._liveTestSeams.getAnimCount() + window._liveTestSeams.getPulses().length);
      const seed = () => h.page.evaluate((p) => sessionStorage.setItem('replay-packet', JSON.stringify(p)), replay);
      // Control: the mount that takes the replay draws it after 1.5 s.
      await seed();
      await h.remount();
      assert(await drawn() === 0, 'nothing drawn before 1.5 s');
      assert(!(await h.run(1600)), 'control run');
      assert(await drawn() > 0, 'control: the replay is drawn by its own mount');
      // The mount that took the replay is remounted within 1.5 s.
      await seed();
      await h.remount();                    // consumes the replay, arms 1.5 s and 12 s
      await h.remount();                    // within 1.5 s: both timers belong to the old mount
      assert(!(await h.run(2000)), 'run past 1.5 s');
      assert(await drawn() === 0, 'the old mount\'s replay was drawn into the next mount (' + await drawn() + ')');
      // Live traffic in the next mount renders at once.
      await h.page.evaluate((p) => window._liveBufferPacket(p), advert('rp000002'));
      assert(await h.pkts() === '1', 'the next mount\'s live packet was muted by the old suppressLive: pkts ' + await h.pkts());
      assert(!(await h.run(12000)), 'run past 12 s');
      assert(!h.errors.length, h.errors.join('; '));
      await h.ctx.close();
    });

    // ── 5. Fullscreen keydown ───────────────────────────────────────────
    await step('5 F after a remount drives the current fullscreen button; one window keydown listener', async () => {
      const h = await open(browser);
      const cdp = await h.ctx.newCDPSession(h.page);
      const keydowns = async () => {
        const { result } = await cdp.send('Runtime.evaluate', { expression: 'window' });
        const { listeners } = await cdp.send('DOMDebugger.getEventListeners', { objectId: result.objectId });
        return listeners.filter((l) => l.type === 'keydown').length;
      };
      const k0 = await keydowns();
      await h.remount();
      await h.remount();
      assert(await keydowns() === k0, 'window keydown listeners ' + k0 + ' → ' + await keydowns());
      await h.page.evaluate(() => document.activeElement && document.activeElement.blur && document.activeElement.blur());
      await h.page.keyboard.press('f');
      const st = await h.page.evaluate(() => ({ body: document.body.classList.contains('live-fullscreen'),
        pressed: document.getElementById('liveFullscreenToggle').getAttribute('aria-pressed') }));
      assert(st.body, 'F did not enter fullscreen');
      assert(st.pressed === 'true', 'the current button\'s aria-pressed is ' + st.pressed + ' (a stale closure updated a detached one)');
      await h.page.keyboard.press('Escape');
      const off = await h.page.evaluate(() => ({ body: document.body.classList.contains('live-fullscreen'),
        pressed: document.getElementById('liveFullscreenToggle').getAttribute('aria-pressed') }));
      assert(!off.body && off.pressed === 'false', 'Escape: ' + JSON.stringify(off));
      await h.page.evaluate(() => { try { localStorage.removeItem('live-fullscreen'); } catch (_) {} });
      assert(!h.errors.length, h.errors.join('; '));
      await h.ctx.close();
    });

    // ── 6. feedDedup cap ────────────────────────────────────────────────
    await step('6 feedDedup: hard cap of 200, the oldest dropped first', async () => {
      const h = await open(browser);
      const r = await h.page.evaluate(() => {
        const add = (hash) => window._liveAddFeedItem('', 'ADVERT', { name: hash }, [], 'currentColor',
          { hash, timestamp: new Date().toISOString(), _ts: Date.now(), decoded: { header: { payloadTypeName: 'ADVERT' }, payload: {} } });
        const badge = (hash) => {
          const el = document.querySelector('#liveFeed .live-feed-item[data-hash="' + hash + '"] .badge-obs');
          return el ? el.textContent.trim() : '';
        };
        for (let i = 0; i < 201; i++) add('dd' + String(i).padStart(4, '0'));
        // The second oldest (dd0001) is still remembered: it is counted.
        add('dd0001');
        const second = badge('dd0001');
        // The oldest (dd0000) is over the cap: a new observation is a new item.
        add('dd0000');
        return { oldest: badge('dd0000'), second,
          items: document.querySelectorAll('#liveFeed .live-feed-item[data-hash="dd0000"]').length };
      });
      assert(r.second === '2', 'dd0001 is within the cap and should dedupe (badge 2), got "' + r.second + '"');
      assert(r.oldest === '' && r.items === 1, 'dd0000 should have been dropped (new item, no badge): ' + JSON.stringify(r));
      assert(!h.errors.length, h.errors.join('; '));
      await h.ctx.close();
    });

    // ── 7. Listeners and DOM across in-place remounts ───────────────────
    await step('7 in-place remounts add no window/document listener and keep the DOM node count flat', async () => {
      const h = await open(browser);
      const cdp = await h.ctx.newCDPSession(h.page);
      await cdp.send('Performance.enable');
      const on = async (expression) => {
        const { result } = await cdp.send('Runtime.evaluate', { expression });
        const { listeners } = await cdp.send('DOMDebugger.getEventListeners', { objectId: result.objectId });
        await cdp.send('Runtime.releaseObject', { objectId: result.objectId });
        return listeners.length;
      };
      const snap = async () => {
        // Let the last destroyed page's short timeouts (Leaflet's, on the
        // installed clock) fire, or they keep it reachable for the snapshot.
        assert(!(await h.run(2000)), 'settle');
        await cdp.send('HeapProfiler.collectGarbage');
        const m = await cdp.send('Performance.getMetrics');
        const g = (n) => m.metrics.find((x) => x.name === n).value;
        return { win: await on('window'), doc: await on('document'), nodes: g('Nodes'), listeners: g('JSEventListeners') };
      };
      await h.remount(); // warm-up: lazy singletons bind once
      const a = await snap();
      for (let i = 0; i < 5; i++) await h.remount();
      const b = await snap();
      console.log('    5 remounts: window ' + a.win + '→' + b.win + ', document ' + a.doc + '→' + b.doc +
        ', Nodes ' + a.nodes + '→' + b.nodes + ', JSEventListeners ' + a.listeners + '→' + b.listeners);
      assert(b.win === a.win && b.doc === a.doc, 'window/document listeners grew: ' + JSON.stringify({ a, b }));
      // Measured flat (0 / 0). One leaked page is ~670 nodes and ~100
      // listeners, far above the slack.
      assert(b.nodes - a.nodes <= 50, 'DOM nodes grew by ' + (b.nodes - a.nodes));
      assert(b.listeners - a.listeners <= 10, 'JSEventListeners grew by ' + (b.listeners - a.listeners));
      assert(!h.errors.length, h.errors.join('; '));
      await h.ctx.close();
    });
    await step('7 no detached Live page survives in-place remounts (first mount included), with traffic', async () => {
      const h = await open(browser);
      const traffic = () => h.page.evaluate(async (tag) => {
        const r = await fetch('/api/packets?limit=40&expand=observations');
        const d = await r.json();
        (d.packets || []).forEach((p, i) => {
          const l = window._liveDbPacketToLive(Object.assign({}, p, (p.observations || [])[0] || {}));
          l.hash = tag + i;
          window._liveBufferPacket(l);
        });
      }, 'tr' + Math.random().toString(16).slice(2, 8));
      await traffic();
      assert(!(await h.run(3000)), 'run');
      for (let i = 0; i < 3; i++) { await h.remount(); await traffic(); assert(!(await h.run(3000)), 'run'); }
      const cdp = await h.ctx.newCDPSession(h.page);
      await cdp.send('HeapProfiler.collectGarbage');
      const { result: proto } = await cdp.send('Runtime.evaluate', { expression: 'HTMLDivElement.prototype' });
      const { objects } = await cdp.send('Runtime.queryObjects', { prototypeObjectId: proto.objectId });
      const { result } = await cdp.send('Runtime.callFunctionOn', { objectId: objects.objectId, returnByValue: true,
        functionDeclaration: 'function () { return this.filter(function (d) { return d.classList.contains("live-page") && !d.isConnected; }).length; }' });
      await cdp.send('Runtime.releaseObject', { objectId: objects.objectId });
      assert(result.value === 0, result.value + ' detached .live-page element(s) still alive after GC');
      assert(await h.page.evaluate(() => window.__liveMQLBindCount) === 1, 'one narrow-viewport handler bound');
      assert(!h.errors.length, h.errors.join('; '));
      await h.ctx.close();
    });
    await step('7 leaving Live for a page without filters, and Packets ↔ Nodes round trips, leave no Live or region/area filter listener', async () => {
      // Starts on Home: its count is the baseline no Live has touched.
      const h = await open(browser, { areas: true, start: '#/home' });
      const cdp = await h.ctx.newCDPSession(h.page);
      // Only Live's and the filters' own window/document listeners: other
      // pages have leaks of their own (Packets' filter-ux click handler).
      const scripts = {};
      cdp.on('Debugger.scriptParsed', (s) => { scripts[s.scriptId] = s.url; });
      await cdp.send('Debugger.enable');
      const on = async (expression) => {
        const { result } = await cdp.send('Runtime.evaluate', { expression });
        const { listeners } = await cdp.send('DOMDebugger.getEventListeners', { objectId: result.objectId });
        await cdp.send('Runtime.releaseObject', { objectId: result.objectId });
        return listeners.filter((l) => /\/((region|area)-filter|live)\.js/.test(scripts[l.scriptId] || '')).length;
      };
      const both = async () => ({ win: await on('window'), doc: await on('document') });
      const goto = async (hash, sel) => {
        await h.page.evaluate((x) => { location.hash = x; }, hash);
        await h.page.waitForFunction((s) => !!document.querySelector(s), sel, { timeout: 20000 });
        await h.page.waitForTimeout(800);   // the filters fetch, then render
      };
      await h.page.waitForFunction(() => !!document.querySelector('.home-hero, .home-chooser'), null, { timeout: 20000 });
      await h.page.waitForTimeout(800);
      const home1 = await both();
      // (live.js keeps one page-lifetime document visibilitychange listener.)
      // Home → Live (both filter dropdowns) → Home: what Live bound must be gone.
      await h.go('#/live');
      await h.page.waitForFunction(() => document.querySelectorAll('#liveAreaFilter .region-dropdown-trigger, #liveRegionFilter .region-dropdown-trigger').length === 2,
        null, { timeout: 20000 });   // both filter dropdowns rendered on Live
      const onLive = await both();
      assert(onLive.win > home1.win && onLive.doc > home1.doc, 'Live bound its listeners: ' + JSON.stringify({ home1, onLive }));
      await goto('#/home', '.home-hero, .home-chooser');
      const home2 = await both();
      assert(home2.win === home1.win && home2.doc === home1.doc, 'Live → Home left Live or filter listeners: ' + JSON.stringify({ home1, home2 }));
      // Packets ↔ Nodes: each page's init() releases the previous container.
      await goto('#/packets', '#packetsRegionFilter');
      await goto('#/nodes', '#nodesRegionFilter');
      const a = await both();
      for (let i = 0; i < 3; i++) { await goto('#/packets', '#packetsRegionFilter'); await goto('#/nodes', '#nodesRegionFilter'); }
      const b = await both();
      assert(b.win === a.win && b.doc === a.doc, 'Packets ↔ Nodes x3 grew listeners: ' + JSON.stringify({ a, b }));
      assert(!h.errors.length, h.errors.join('; '));
      await h.ctx.close();
    });
  } finally {
    await browser.close();
  }
  console.log('\n' + passed + ' passed, ' + failed + ' failed');
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
