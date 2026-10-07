/**
 * E2E: live "Multibyte only" toggle.
 *   1. Toggle exists in live controls and defaults OFF.
 *   2. With it ON, a single-byte synthetic packet does NOT create a feed item,
 *      while a multibyte one does.
 *   3. Turning it OFF and rebuilding shows the previously-hidden single-byte pkt.
 *   4. The saved setting (true, false or unset) is shown from the moment the
 *      toggle is rendered, survives a real reload and SPA navigation, and a
 *      click made while Live is still initializing is saved and kept. In each
 *      of these cases the feed itself is probed, both while init is held and
 *      after it, so a box that shows ON over a feed that filters as OFF fails.
 *
 * Why step 4 holds init: live.js awaits /api/config/map and loadNodes() after
 * rendering the controls. The toggle's saved state and change listener used to
 * be applied only after both awaits, so during init the box showed OFF even
 * when saved ON, and a click made then was never saved and was reverted. The
 * test holds /api/config/map with request interception, which freezes init at
 * its first await, and checks the toggle while it is held. It then releases
 * init and waits until Live has subscribed to packets (window._liveWSHandler()
 * returns a handler). connectWS() sets that handler after loadNodes() in the same
 * synchronous run of init as the old toggle wiring, and destroy() clears it,
 * so it marks a finished init on every load and SPA mount. The test also
 * asserts there is no handler while init is held, so the checks after init
 * cannot run too early. No step sleeps or retries.
 *
 * Usage: BASE_URL=http://localhost:13581 node test-live-multibyte-only-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const ORIGIN = new URL(BASE).origin;
const KEY = 'live-multibyte-only';
let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

// raw_hex builders. Header 0x15 = 0b00010101: route type 01 (FLOOD, so no
// transport codes and the path-length byte sits at offset 1) and payload type
// 5 (GRP_TXT), matching makePkt's route_type and payloadTypeName. The top
// two bits of the path-length byte encode hash size - 1 (firmware
// docs/packet_format.md).
const SINGLE_HEX = '1500'; // (0x00>>6)+1 = 1-byte path hash, 0 hops
const MULTI_HEX  = '1540'; // (0x40>>6)+1 = 2-byte path hash, 0 hops

function makePkt(hash, rawHex) {
  return {
    id: Math.floor(Math.random() * 1e9),
    hash: hash,
    raw_hex: rawHex,
    route_type: 1,
    path_json: '[]',
    observer_id: 'mb-e2e-obs',
    observer_name: 'mb-e2e',
    timestamp: new Date().toISOString(),
    snr: 5, rssi: -90,
    decoded: {
      header: { payloadTypeName: 'GRP_TXT' },
      payload: { text: 'mb-probe' },
      path: { hops: [] },
    },
  };
}

// Buffers one packet with a 1-byte and one with a 2-byte path hash and reports
// which of them the feed rendered. In LIVE mode with Realistic off,
// bufferPacket -> renderPacketTree -> addFeedItem runs synchronously, so the
// feed DOM is final when this evaluate returns; nothing needs to wait.
let probeSeq = 0;
async function probeFeed(page, tag) {
  const id = tag.replace(/\W+/g, '-') + '-' + (++probeSeq) + '-' + Date.now().toString(16);
  return page.evaluate((a) => {
    window._liveBufferPacket(a.single);
    window._liveBufferPacket(a.multi);
    const shown = (h) => !!document.querySelector('.live-feed-item[data-hash="' + h + '"]');
    return {
      single: shown(a.single.hash),
      multi: shown(a.multi.hash),
      sizes: [a.single, a.multi].map((p) => window.MC_packetHashSize(p.raw_hex, p.route_type)),
    };
  }, { single: makePkt('mb-probe-1b-' + id, SINGLE_HEX), multi: makePkt('mb-probe-2b-' + id, MULTI_HEX) });
}

// The feed must filter the way the setting says: ON hides the 1-byte packet,
// OFF shows it. The 2-byte packet must render either way, which proves the
// feed was live when probed.
async function assertFeedFilter(page, on, tag) {
  const f = await probeFeed(page, tag);
  assert(f.sizes[0] === 1 && f.sizes[1] === 2, 'probe packets must carry 1-byte and 2-byte path hashes, got ' + f.sizes + ' (' + tag + ')');
  assert(f.multi, 'the multibyte probe packet did not render, so the feed was not live (' + tag + ')');
  assert(f.single === !on, 'setting is ' + (on ? 'ON' : 'OFF') + ' but the feed filter behaves as ' + (f.single ? 'OFF' : 'ON') +
    ': the single-byte packet was ' + (f.single ? 'shown' : 'hidden') + ' (' + tag + ')');
}

// Records page errors, console errors and unhandled rejections, and counts
// writes of the setting (a doubled change listener would write twice per
// change). storedValue: 'true' / 'false' seeds the setting before the first
// load, null removes it.
async function newLivePage(browser, errors, storedValue) {
  const ctx = await browser.newContext({ viewport: { width: 1400, height: 900 } });
  const page = await ctx.newPage();
  page.setDefaultTimeout(15000);
  page.on('pageerror', (e) => errors.push('pageerror: ' + e.message));
  page.on('console', (m) => {
    if (m.type() !== 'error') return;
    // Map tiles come from an external CDN whose failures have nothing to do
    // with Live; only errors from this app's own origin count.
    const url = (m.location() && m.location().url) || '';
    if (url && !url.startsWith(ORIGIN)) return;
    errors.push('console.error: ' + m.text());
  });
  await page.addInitScript(([key, value]) => {
    // Seed once per tab, not on every reload.
    if (!sessionStorage.getItem('mb-e2e-seeded')) {
      sessionStorage.setItem('mb-e2e-seeded', '1');
      if (value === null) localStorage.removeItem(key); else localStorage.setItem(key, value);
    }
    window.__mbWrites = 0;
    const setItem = Storage.prototype.setItem;
    Storage.prototype.setItem = function (k, v) {
      if (this === window.localStorage && k === key) window.__mbWrites++;
      return setItem.call(this, k, v);
    };
    window.addEventListener('unhandledrejection', (e) => {
      console.error('unhandledrejection: ' + (e.reason && e.reason.message || e.reason));
    });
  }, [KEY, storedValue]);
  return { ctx, page };
}

// Holds the next /api/config/map request, the first await in Live init, so
// init stays frozen right after rendering the controls.
async function holdLiveInit(page) {
  let release, hit;
  const released = new Promise((r) => { release = r; });
  const intercepted = new Promise((r) => { hit = r; });
  await page.route('**/api/config/map', async (route) => {
    hit();
    await released;
    await route.continue().catch((e) => { if (!page.isClosed()) throw e; });
  }, { times: 1 });
  return {
    release,
    async reached() {
      let timer;
      const timeout = new Promise((_, rej) => { timer = setTimeout(() => rej(new Error(
        'Live init never requested /api/config/map within 15s; the init hold point this test relies on has moved')), 15000); });
      try { await Promise.race([intercepted, timeout]); } finally { clearTimeout(timer); }
    },
  };
}

async function readToggle(page) {
  return page.evaluate((key) => {
    const cb = document.getElementById('liveMultibyteToggle');
    return {
      exists: !!cb,
      checked: cb ? cb.checked : null,
      stored: localStorage.getItem(key),
      liveReady: !!window._liveWSHandler(),
      writes: window.__mbWrites,
    };
  }, KEY);
}

// Runs load() with Live init held, then inspect(page, state, toggle) while it
// is still held, then releases init and waits for init to finish. It waits
// even when a check failed, so the next step never starts on a half-built page.
async function withInitHeld(page, load, inspect) {
  const hold = await holdLiveInit(page);
  let failure = null;
  try {
    await load();
    await hold.reached();
    const cb = page.locator('#liveMultibyteToggle');
    await cb.waitFor({ state: 'visible' });
    const st = await readToggle(page);
    assert(!st.liveReady, 'Live is already subscribed to packets while init is held; the readiness signal moved and the checks after init would run too early');
    assert(await cb.isEnabled(), 'toggle should be enabled while init is held (the user can already click it)');
    await inspect(page, st, cb);
  } catch (e) {
    failure = e;
  } finally {
    hold.release();
  }
  const ready = await page.waitForFunction(() => !!window._liveWSHandler(), null, { timeout: 15000 }).then(() => true, () => false);
  if (failure) throw failure;
  if (!ready) throw new Error('Live init did not finish: it never subscribed to packets (window._liveWSHandler() stayed empty) after init was released');
}

// Clicks the toggle the way a user does after init: init collapses the
// controls panel by default, so open it first when it is collapsed.
async function clickToggleAsUser(page) {
  if (await page.locator('#liveControlsBody').isHidden()) await page.locator('#liveControlsToggle').click();
  await page.locator('#liveMultibyteToggle').click();
}

// Sets the saved value directly, so a step does not depend on the previous one.
async function saveSetting(page, value) {
  await page.evaluate(([key, v]) => { localStorage.setItem(key, v); }, [KEY, value]);
}

const loadLive = (page) => () => page.goto(BASE + '/#/live', { waitUntil: 'domcontentloaded' });
const reload = (page) => () => page.reload({ waitUntil: 'domcontentloaded' });

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  const errors = [];
  const { ctx, page } = await newLivePage(browser, errors, null);

  console.log('\n=== live multibyte-only E2E against ' + BASE + ' ===');

  await step('navigate to /#/live, toggle exists and defaults OFF', async () => {
    await withInitHeld(page, loadLive(page), async (p, st) => {
      assert(st.stored === null, 'precondition: no saved setting');
      assert(st.checked === false, 'multibyte toggle must default OFF when nothing is saved');
    });
    const st = await readToggle(page);
    assert(st.checked === false, 'multibyte toggle must still be OFF after init');
    assert(st.writes === 0, 'init must not write the setting on its own (writes=' + st.writes + ')');
  });

  const singleHash = 'mb-single-' + Date.now().toString(16);
  const multiHash  = 'mb-multi-'  + Date.now().toString(16);

  await step('turn toggle ON: single-byte packet hidden, multibyte shown', async () => {
    await page.evaluate(() => {
      const cb = document.getElementById('liveMultibyteToggle');
      cb.checked = true;
      cb.dispatchEvent(new Event('change', { bubbles: true }));
    });
    await page.evaluate((args) => {
      window._liveBufferPacket(args.single);
      window._liveBufferPacket(args.multi);
    }, { single: makePkt(singleHash, SINGLE_HEX), multi: makePkt(multiHash, MULTI_HEX) });

    await page.waitForFunction((h) => !!document.querySelector('.live-feed-item[data-hash="' + h + '"]'),
      multiHash, { timeout: 5000 }).catch(() => { throw new Error('multibyte packet never rendered a feed item within 5s with the toggle ON'); });

    const multiShown = await page.evaluate((h) => !!document.querySelector('.live-feed-item[data-hash="' + h + '"]'), multiHash);
    const singleShown = await page.evaluate((h) => !!document.querySelector('.live-feed-item[data-hash="' + h + '"]'), singleHash);
    assert(multiShown, 'multibyte packet should render a feed item when toggle ON');
    assert(!singleShown, 'single-byte packet must NOT render a feed item when toggle ON');
  });

  await step('turn toggle OFF: previously-hidden single-byte packet appears', async () => {
    await page.evaluate(() => {
      const cb = document.getElementById('liveMultibyteToggle');
      cb.checked = false;
      cb.dispatchEvent(new Event('change', { bubbles: true }));
    });
    await page.waitForFunction((h) => !!document.querySelector('.live-feed-item[data-hash="' + h + '"]'),
      singleHash, { timeout: 5000 }).catch(() => { throw new Error('single-byte packet never reappeared within 5s after toggle OFF + feed rebuild'); });
    const singleShown = await page.evaluate((h) => !!document.querySelector('.live-feed-item[data-hash="' + h + '"]'), singleHash);
    assert(singleShown, 'single-byte packet should reappear after toggle OFF + feed rebuild');
  });

  await step('a click made while Live is still initializing is saved and kept', async () => {
    await saveSetting(page, 'false');
    // "Kept" means kept once init has finished. A revert deferred by an
    // arbitrary timer after init is out of reach for a deterministic test.
    await withInitHeld(page, reload(page), async (p, st, cb) => {
      assert(st.checked === false && st.stored === 'false', 'setup: saved OFF should show OFF (checked=' + st.checked + ', stored=' + st.stored + ')');
      await cb.click();
      const after = await readToggle(p);
      assert(after.checked === true, 'the click should turn the toggle ON');
      assert(after.stored === 'true', 'a click during init must save the setting at once (stored=' + after.stored + ')');
      assert(after.writes === 1, 'one click must write the setting exactly once (writes=' + after.writes + ')');
      await assertFeedFilter(p, true, 'click during init, init still held');
    });
    const st = await readToggle(page);
    assert(st.checked === true, 'init must not revert the user\'s click (toggle is OFF after init)');
    assert(st.stored === 'true', 'setting must stay saved ON after init (stored=' + st.stored + ')');
    await assertFeedFilter(page, true, 'click during init, after init');
  });

  await step('saved ON is shown from the start of a real reload', async () => {
    await saveSetting(page, 'true');
    await withInitHeld(page, reload(page), async (p, st) => {
      assert(st.stored === 'true', 'setup: expected saved ON, got ' + st.stored);
      assert(st.checked === true, 'toggle must show the saved ON state while init is still running (it showed OFF)');
      await assertFeedFilter(p, true, 'reload with saved ON, init still held');
    });
    assert((await readToggle(page)).checked === true, 'toggle must stay ON after init');
    await assertFeedFilter(page, true, 'reload with saved ON, after init');
  });

  await step('SPA navigation away and back keeps the setting and one listener', async () => {
    // Setup only: the reload itself is covered by the steps above.
    await saveSetting(page, 'true');
    await page.reload({ waitUntil: 'domcontentloaded' });
    await page.waitForFunction(() => !!window._liveWSHandler(), null, { timeout: 15000 })
      .catch(() => { throw new Error('setup: Live init did not finish after reload'); });
    for (let i = 0; i < 3; i++) {
      await page.evaluate(() => { location.hash = '#/packets'; });
      await page.waitForSelector('#pktTable', { state: 'attached' });
      assert(!(await readToggle(page)).exists, 'live controls should be gone on the packets page');
      await withInitHeld(page, () => page.evaluate(() => { location.hash = '#/live'; }), async (p, st) => {
        assert(st.checked === true, 'toggle must show the saved ON state after navigating back (round trip ' + (i + 1) + ')');
        await assertFeedFilter(p, true, 'SPA round trip ' + (i + 1) + ', init still held');
      });
    }
    await assertFeedFilter(page, true, 'after 3 SPA round trips, after init');
    const before = (await readToggle(page)).writes;
    await clickToggleAsUser(page);
    const st = await readToggle(page);
    assert(st.checked === false && st.stored === 'false', 'click after navigation should turn the setting OFF (checked=' + st.checked + ', stored=' + st.stored + ')');
    assert(st.writes - before === 1, 'one click after 3 round trips must write once, not ' + (st.writes - before) + ' times (listeners piled up)');
    await assertFeedFilter(page, false, 'click OFF after SPA round trips');
    await withInitHeld(page, reload(page), async (p, s) => {
      assert(s.checked === false, 'toggle must show the saved OFF state after reload');
    });
  });

  await ctx.close();

  for (const saved of ['true', 'false', null]) {
    await step('fresh load with the setting ' + (saved === null ? 'unset' : 'saved as ' + saved) + ' shows it before init finishes', async () => {
      const fresh = await newLivePage(browser, errors, saved);
      const want = saved === 'true';
      try {
        await withInitHeld(fresh.page, loadLive(fresh.page), async (p, st) => {
          assert(st.stored === saved, 'setup: stored=' + saved + ', got ' + st.stored);
          assert(st.checked === want, 'toggle must show ' + (want ? 'ON' : 'OFF') + ' while init is still running, got ' + (st.checked ? 'ON' : 'OFF'));
          await assertFeedFilter(p, want, 'fresh load, saved ' + saved + ', init still held');
        });
        const st = await readToggle(fresh.page);
        assert(st.checked === want, 'toggle must still show ' + (want ? 'ON' : 'OFF') + ' after init');
        assert(st.writes === 0, 'loading the page must not write the setting (writes=' + st.writes + ')');
        await assertFeedFilter(fresh.page, want, 'fresh load, saved ' + saved + ', after init');
      } finally {
        await fresh.ctx.close();
      }
    });
  }

  await step('no page errors, console errors or unhandled rejections', async () => {
    assert(errors.length === 0, errors.length + ' error(s):\n    ' + errors.join('\n    '));
  });

  await browser.close();
  console.log('\n--- ' + passed + ' passed, ' + failed + ' failed ---\n');
  process.exit(failed > 0 ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
