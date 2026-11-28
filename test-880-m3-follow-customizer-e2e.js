/**
 * E2E (#880 M3): Customize → Display → Live Map, the viewer's settings for
 * Live "Follow a packet", and the --follow-* theme colours.
 *
 * The follow data is the M1 suite's stub (tests/live-follow-e2e-stubs.js):
 * an ADVERT over three fixture relays heard by six observers, so the replay
 * has hop depth 3 and four hop steps (0..3) before the summary.
 *
 * A status probe (init script) stamps every change of the follow card's
 * status line ("Hop d / 3") and the moment the summary opens with
 * performance.now(), so the real step timing is measured, not only the
 * scheduled stepMs.
 *
 *   1. Display tab: the Live Map section with the defaults (1x, 6 s, empty
 *      settle with the server's 5 s as placeholder), built as DOM; a bad
 *      value is put back and not stored.
 *   2. Speed: 1x measured ~1500 ms per hop step; 2x chosen in the select is
 *      stored, and the next follow steps ~750 ms (stepMs 750); 0.5x is
 *      stepMs 3000. The reset dot returns to 1x.
 *   3. Length target: 3 s -> stepMs 750 at 1x.
 *   4. Settle delay: default waits the server window for a just-heard
 *      packet; 12 s waits longer; 0 loads at once (one fetch, no wait).
 *   5. Reload: the stored settings come back in the panel and the badge.
 *   6. Theme: the four tokens resolve in light and dark; the Theme tab
 *      group has four colour rows; a relay colour reaches a relay marker; a
 *      colourblind preset changes the relay / observer pair.
 *   7. 375x560 touch phone from a cold start, light and dark, real taps:
 *      More → Customize → Display → Live Map (select and field >= 44 px,
 *      inside the screen) → close → gear → Follow next → a replay at the
 *      chosen step → the sheet above the bottom nav; no sideways scroll.
 *
 * Usage: BASE_URL=http://localhost:13581 node test-880-m3-follow-customizer-e2e.js
 * Screenshots: SHOT_DIR=<dir> writes 880m3-*.png there. ONLY=<regex> runs a subset.
 */
'use strict';
const { chromium } = require('playwright');
const { createFollowStubs, HASH } = require('./tests/live-follow-e2e-stubs');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
if (!['localhost', '127.0.0.1', '[::1]'].includes(new URL(BASE).hostname)) throw new Error('local server only');
const SHOT = (name) => (process.env.SHOT_DIR ? process.env.SHOT_DIR + '/880m3-' + name + '.png' : null);

let passed = 0, failed = 0;
// ONLY=<regex> runs the matching tests only (mutant runs).
const ONLY = process.env.ONLY ? new RegExp(process.env.ONLY) : null;
async function test(name, fn) {
  if (ONLY && !ONLY.test(name)) return;
  try { await fn(); passed++; console.log('  ✅ ' + name); }
  catch (err) { failed++; console.log('  ❌ ' + name + ': ' + (err && err.stack || err)); }
}
function assert(cond, msg) { if (!cond) throw new Error(msg || 'assertion failed'); }

const stubs = createFollowStubs(BASE);
let browser;

async function newPage(opts) {
  const ctx = await browser.newContext(Object.assign({ viewport: { width: 1440, height: 900 } }, opts || {}));
  const page = await ctx.newPage();
  page.__errors = [];
  page.on('pageerror', (e) => page.__errors.push(String(e && e.stack || e)));
  // Live auto-hides the top nav after 4 s; pinned, the palette stays reachable.
  if (!(opts && opts.isMobile)) await page.addInitScript(() => localStorage.setItem('live-nav-pinned', 'true'));
  await page.addInitScript(() => {
    const probe = window.__880m3 = { status: [], summaryAt: null };
    const watch = () => {
      const el = document.querySelector('.live-follow-card .lf-status');
      if (el) {
        const t = el.textContent, last = probe.status[probe.status.length - 1];
        if (!last || last.t !== t) probe.status.push({ t, at: performance.now() });
      }
      if (probe.summaryAt == null && document.querySelector('.live-follow-card .lf-summary:not([hidden]) .lf-rows')) probe.summaryAt = performance.now();
    };
    new MutationObserver(watch).observe(document, { subtree: true, childList: true, characterData: true, attributes: true, attributeFilter: ['hidden'] });
  });
  await stubs.installRoutes(page);
  return page;
}

async function waitLiveReady(page) {
  await page.waitForSelector('.live-page', { timeout: 15000 });
  await page.waitForFunction(() => window._liveNodeData && Object.keys(window._liveNodeData()).length > 0 &&
    window.HopResolver && window.HopResolver.ready() && window._customizerV2 && window._customizerV2.initDone, null, { timeout: 15000 });
  await page.waitForTimeout(500);
}
async function gotoLive(page) {
  await page.goto(BASE + '/?r=' + Date.now() + '#/live', { waitUntil: 'domcontentloaded' });
  await waitLiveReady(page);
}
async function openDisplay(page) {
  if (!(await page.$('.cust-overlay:not(.hidden)'))) {
    await page.click('#customizeToggle');
    await page.waitForSelector('.cust-overlay:not(.hidden)', { timeout: 5000 });
  }
  await page.click('.cust-tab[data-tab="display"]');
  await page.waitForSelector('.cust-panel[data-panel="display"].active [data-cv2-follow="speed"]', { timeout: 4000 });
}
async function closePanel(page) {
  if (await page.$('.cust-overlay:not(.hidden)')) await page.click('.cust-close');
  await page.waitForTimeout(200);
}
const stored = (page) => page.evaluate(() => (JSON.parse(localStorage.getItem('cs-theme-overrides') || '{}').follow) || null);
const fields = (page) => page.evaluate(() => {
  const q = (k) => document.querySelector('.cust-panel[data-panel="display"] [data-cv2-follow="' + k + '"]');
  return { speed: q('speed').value, target: q('targetSeconds').value, settle: q('settleSeconds').value,
    placeholder: q('settleSeconds').placeholder, dots: document.querySelectorAll('[data-cv2-follow-settings] .cv2-override-dot').length };
});
async function setField(page, key, value) {
  const sel = '.cust-panel[data-panel="display"] [data-cv2-follow="' + key + '"]';
  if (key === 'speed') await page.selectOption(sel, String(value));
  else { await page.fill(sel, String(value)); await page.dispatchEvent(sel, 'change'); }
  await page.waitForTimeout(700); // the customizer's debounced write
}

// Follow HASH right away (deep-link path, settle 0) and wait for the summary.
async function followNow(page) {
  await page.evaluate(() => { window.__880m3.status = []; window.__880m3.summaryAt = null; });
  await page.evaluate((h) => window._liveStartFollow(h, { settleMs: 0 }), HASH);
  await page.waitForFunction(() => window.__880m3.summaryAt != null, null, { timeout: 30000 });
  return page.evaluate(() => {
    const st = window._liveFollowState();
    const at = (t) => (window.__880m3.status.find((s) => s.t.indexOf(t) === 0) || {}).at;
    return { stepMs: st.sched.stepMs, totalMs: st.sched.totalMs,
      gap: at('Hop 3 / 3') - at('Hop 1 / 3'), toSummary: window.__880m3.summaryAt - at('Hop 1 / 3') };
  });
}
const near = (v, want, tol) => Math.abs(v - want) <= want * tol;

(async () => {
  browser = await chromium.launch({ headless: true, executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'] });
  console.log('\n=== #880 M3 Follow customizer E2E against ' + BASE + ' ===');
  await stubs.pickNodes();

  await test('Display tab: the Live Map section, defaults, DOM-built; a bad value is put back', async () => {
    const page = await newPage();
    await gotoLive(page);
    await openDisplay(page);
    const title = await page.evaluate(() => {
      const host = document.querySelector('[data-cv2-follow-settings]');
      return { title: host.querySelector('.cust-section-title').textContent, labels: [...host.querySelectorAll('label')].map((l) => l.textContent) };
    });
    assert(title.title === 'Live Map', 'title ' + title.title);
    assert(title.labels.join('|') === 'Replay speed|Replay length target (s)|Settle delay (s)', 'labels ' + title.labels);
    const f = await fields(page);
    assert(f.speed === '1' && f.target === '6' && f.settle === '' && f.placeholder === '5' && f.dots === 0, 'defaults ' + JSON.stringify(f));
    const opts = await page.$$eval('[data-cv2-follow="speed"] option', (o) => o.map((x) => x.value));
    assert(opts.join() === '0.5,1,2', 'speeds ' + opts);
    await setField(page, 'targetSeconds', 99);
    assert((await fields(page)).target === '6', 'out-of-range target put back');
    await setField(page, 'settleSeconds', 2.5);
    assert((await fields(page)).settle === '', 'a fraction put back');
    assert((await stored(page)) === null, 'nothing stored: ' + JSON.stringify(await stored(page)));
    assert(!page.__errors.length, page.__errors[0]);
    await page.context().close();
  });

  await test('speed: 1x steps ~1500 ms, 2x from the select steps ~750 ms, 0.5x is 3000; the dot resets', async () => {
    const page = await newPage();
    await gotoLive(page);
    const one = await followNow(page);
    assert(one.stepMs === 1500 && one.totalMs === 6000, '1x schedule ' + JSON.stringify(one));
    assert(near(one.gap, 3000, 0.25), '1x measured two steps ' + one.gap);
    await page.evaluate(() => window._liveStopFollow());
    await openDisplay(page);
    await setField(page, 'speed', 2);
    const s = await stored(page);
    assert(s && s.speed === 2, 'stored ' + JSON.stringify(s));
    await closePanel(page);
    const two = await followNow(page);
    assert(two.stepMs === 750 && two.totalMs === 3000, '2x schedule ' + JSON.stringify(two));
    assert(near(two.gap, 1500, 0.3), '2x measured two steps ' + two.gap);
    assert(two.gap < one.gap * 0.7 && two.toSummary < one.toSummary * 0.7, 'faster: ' + JSON.stringify([one, two]));
    await page.evaluate(() => window._liveStopFollow());
    await openDisplay(page);
    await setField(page, 'speed', 0.5);
    await closePanel(page);
    await page.evaluate((h) => window._liveStartFollow(h, { settleMs: 0 }), HASH);
    await page.waitForFunction(() => window._liveFollowState() && window._liveFollowState().sched, null, { timeout: 10000 });
    assert(await page.evaluate(() => window._liveFollowState().sched.stepMs) === 3000, '0.5x stepMs');
    await page.evaluate(() => window._liveStopFollow());
    await openDisplay(page);
    assert((await fields(page)).dots === 1, 'one override dot');
    await page.click('[data-cv2-follow-settings] .cv2-override-dot');
    await page.waitForTimeout(700);
    assert((await stored(page)) === null, 'dot cleared the speed: ' + JSON.stringify(await stored(page)));
    assert(!page.__errors.length, page.__errors[0]);
    await page.context().close();
  });

  await test('length target: 3 s gives 750 ms steps at 1x', async () => {
    const page = await newPage();
    await gotoLive(page);
    await openDisplay(page);
    await setField(page, 'targetSeconds', 3);
    assert((await stored(page)).targetSeconds === 3);
    await closePanel(page);
    const r = await followNow(page);
    assert(r.stepMs === 750 && r.totalMs === 3000, 'target 3 ' + JSON.stringify(r));
    await page.context().close();
  });

  // A just-heard packet's popup Follow: the status names the wait.
  async function popupFollow(page, ageMs) {
    await page.evaluate((f) => window._liveBufferPacket(f), stubs.frameFor(HASH, ageMs));
    await page.waitForFunction(() => window._liveClickablePaths.length > 0, null, { timeout: 15000 });
    await page.evaluate(() => {
      const poly = window._liveClickablePaths[window._liveClickablePaths.length - 1].poly;
      const ll = poly.getLatLngs();
      poly.fire('click', { latlng: ll[Math.floor(ll.length / 2)] });
    });
    await (await page.waitForSelector('.path-info-popup .lc-follow-btn', { timeout: 5000 })).click();
    await page.waitForSelector('.live-follow-card', { timeout: 5000 });
    return page.evaluate(() => document.querySelector('.live-follow-card .lf-status').textContent);
  }

  await test('settle delay: the server window by default, 12 s longer, 0 loads at once', async () => {
    const page = await newPage();
    await gotoLive(page);
    let status = await popupFollow(page, 1000);
    assert(/^Waiting [2-5] s for late observations/.test(status), 'default ' + status);
    await page.evaluate(() => window._liveStopFollow());
    await openDisplay(page);
    await setField(page, 'settleSeconds', 12);
    assert((await stored(page)).settleSeconds === 12);
    await closePanel(page);
    status = await popupFollow(page, 1000);
    // 12 s less the packet's age by the time the popup is clicked (~1-3 s).
    assert(/^Waiting (9|1[0-2]) s for late observations/.test(status), '12 s ' + status);
    assert(page.__fetches[HASH] === 0, 'no fetch while settling');
    await page.evaluate(() => window._liveStopFollow());
    await openDisplay(page);
    await setField(page, 'settleSeconds', 0);
    await closePanel(page);
    status = await popupFollow(page, 1000);
    assert(!/Waiting/.test(status), '0 s ' + status);
    await page.waitForFunction(() => window._liveFollowState() && window._liveFollowState().sched, null, { timeout: 5000 });
    assert(page.__fetches[HASH] === 1, 'one fetch at once, got ' + page.__fetches[HASH]);
    // Empty is the server's window again.
    await page.evaluate(() => window._liveStopFollow());
    await openDisplay(page);
    await setField(page, 'settleSeconds', '');
    assert(!('settleSeconds' in ((await stored(page)) || {})), 'empty cleared the override');
    await page.context().close();
  });

  await test('reload: the stored settings come back in the panel and the Display badge', async () => {
    const page = await newPage();
    await gotoLive(page);
    await openDisplay(page);
    await setField(page, 'speed', 2);
    await setField(page, 'targetSeconds', 10);
    await page.reload({ waitUntil: 'domcontentloaded' });
    await waitLiveReady(page);
    await openDisplay(page);
    const f = await fields(page);
    assert(f.speed === '2' && f.target === '10' && f.dots === 2, 'after reload ' + JSON.stringify(f));
    const badge = await page.textContent('.cust-tab[data-tab="display"] .cv2-tab-badge');
    assert(Number(badge) >= 2, 'badge ' + badge);
    await page.context().close();
  });

  await test('theme: tokens in light and dark, four Theme rows, a relay colour reaches the marker, a CB preset', async () => {
    const tokens = (page) => page.evaluate(() => ['--follow-ring', '--follow-relay', '--follow-observer', '--follow-flag']
      .map((v) => getComputedStyle(document.documentElement).getPropertyValue(v).trim()));
    for (const scheme of ['light', 'dark']) {
      const page = await newPage({ colorScheme: scheme });
      await page.addInitScript((s) => localStorage.setItem('meshcore-theme', s), scheme);
      await gotoLive(page);
      const t = await tokens(page);
      assert(t.every((v) => /^(#[0-9a-f]{3,8}|rgb)/i.test(v)), scheme + ' tokens ' + JSON.stringify(t));
      const theme = await page.evaluate(() => document.documentElement.getAttribute('data-theme'));
      assert(theme === scheme, 'theme ' + theme);
      await page.click('#customizeToggle');
      await page.click('.cust-tab[data-tab="theme"]');
      const section = scheme === 'dark' ? 'themeDark' : 'theme';
      const rows = await page.$$eval('.cust-follow-colors [data-cv2-field]', (els) => els.map((e) => e.getAttribute('data-cv2-field')));
      assert(rows.join() === ['followRing', 'followRelay', 'followObserver', 'followFlag'].map((k) => section + '.' + k).join(), 'rows ' + rows);
      const pick = scheme === 'dark' ? '#33ccaa' : '#aa3366';
      await page.evaluate(() => { document.querySelector('.cust-follow-colors').open = true; });
      await page.fill('.cust-follow-colors [data-cv2-field="' + section + '.followRelay"]', pick);
      await page.dispatchEvent('.cust-follow-colors [data-cv2-field="' + section + '.followRelay"]', 'change');
      await page.waitForTimeout(800);
      await closePanel(page);
      await followNow(page);
      const dot = await page.evaluate(() => getComputedStyle(document.querySelector('.lf-relay .lf-dot')).backgroundColor);
      const rgb = 'rgb(' + [1, 3, 5].map((i) => parseInt(pick.slice(i, i + 2), 16)).join(', ') + ')';
      assert(dot === rgb, scheme + ' relay dot ' + dot + ' want ' + rgb);
      await page.screenshot({ path: SHOT('desktop-' + scheme) || undefined }).catch(() => {});
      await page.context().close();
    }
    const page = await newPage();
    await gotoLive(page);
    const before = await tokens(page);
    await page.evaluate(() => window.MeshCorePresets.applyPreset('deut', { skipPersist: true }));
    const after = await tokens(page);
    assert(after[1].toLowerCase() === '#fe6100' && after[2].toLowerCase() === '#648fff', 'deut pair ' + JSON.stringify(after));
    assert(after[0] === before[0] && after[3] === before[3], 'ring and flag stay theme tokens');
    await page.evaluate(() => window.MeshCorePresets.clearPreset());
    assert(JSON.stringify(await tokens(page)) === JSON.stringify(before), 'clearPreset restores');
    await page.context().close();
  });

  for (const scheme of ['light', 'dark']) {
    await test('375x560 phone, cold start, ' + scheme + ', real taps: Live Map settings → Follow next → replay at the chosen step', async () => {
      const page = await newPage({ viewport: { width: 375, height: 560 }, hasTouch: true, isMobile: true, deviceScaleFactor: 2, colorScheme: scheme });
      await page.addInitScript((s) => { if (!sessionStorage.getItem('m3-seeded')) { localStorage.setItem('meshcore-theme', s); sessionStorage.setItem('m3-seeded', '1'); } }, scheme);
      await page.goto(BASE + '/?r=' + Date.now(), { waitUntil: 'domcontentloaded' });
      const tap = async (sel) => {
        const el = await page.waitForSelector(sel, { state: 'visible', timeout: 10000 });
        await el.scrollIntoViewIfNeeded();
        const b = await el.boundingBox();
        assert(b && b.x >= 0 && b.x + b.width <= 375 && b.y >= 0 && b.y + b.height <= 560, sel + ' off screen ' + JSON.stringify(b));
        await page.touchscreen.tap(b.x + b.width / 2, b.y + b.height / 2);
        return b;
      };
      await tap('#chooseExp');
      await tap('.bottom-nav [data-route="live"]');
      await waitLiveReady(page);
      assert(await page.evaluate(() => document.documentElement.getAttribute('data-theme')) === scheme, 'theme');
      await tap('[data-bottom-nav-tab="more"]');
      await tap('[data-mpa-mirror="customizeToggle"]');
      await tap('.cust-tab[data-tab="display"]');
      const sb = await tap('[data-cv2-follow="speed"]');
      assert(sb.height >= 44, 'speed select ' + sb.height + ' px');
      // The native picker the tap opens is outside the page; choose in it.
      await page.selectOption('[data-cv2-follow="speed"]', '2');
      const tb = await tap('[data-cv2-follow="targetSeconds"]');
      assert(tb.height >= 44, 'target field ' + tb.height + ' px');
      await page.keyboard.press('ControlOrMeta+A');
      await page.keyboard.press('Backspace');
      await page.keyboard.type('4');
      await tap('[data-cv2-follow-settings] .cust-section-title');
      await page.waitForTimeout(800);
      const s = await stored(page);
      assert(s && s.speed === 2 && s.targetSeconds === 4, 'stored ' + JSON.stringify(s));
      assert(await page.evaluate(() => document.documentElement.scrollWidth - innerWidth) <= 0, 'sideways scroll in the panel');
      await page.locator('[data-cv2-follow-settings]').scrollIntoViewIfNeeded();
      await page.screenshot({ path: SHOT('phone-panel-' + scheme) || undefined }).catch(() => {});
      await tap('.cust-close');
      await page.waitForTimeout(300);
      await tap('#liveControlsToggle');
      await tap('#liveFollowNextBtn');
      await page.evaluate(() => { window.__880m3.status = []; window.__880m3.summaryAt = null; });
      await page.evaluate((f) => window._liveBufferPacket(f), stubs.frameFor(HASH, 10000));
      await page.waitForSelector('.live-follow-card', { timeout: 5000 });
      await page.waitForFunction(() => window.__880m3.summaryAt != null, null, { timeout: 20000 });
      const r = await page.evaluate(() => {
        const st = window._liveFollowState();
        const at = (t) => (window.__880m3.status.find((x) => x.t.indexOf(t) === 0) || {}).at;
        const c = document.querySelector('.live-follow-card').getBoundingClientRect();
        const nav = document.querySelector('.bottom-nav').getBoundingClientRect();
        return { stepMs: st.sched.stepMs, gap: at('Hop 3 / 3') - at('Hop 1 / 3'), bottom: c.bottom, navTop: nav.top,
          left: c.left, right: c.right, sw: document.scrollingElement.scrollWidth, iw: innerWidth };
      });
      // target 4 s over 4 steps = 1000 ms, at 2x = 500 ms.
      assert(r.stepMs === 500, 'stepMs ' + JSON.stringify(r));
      assert(near(r.gap, 1000, 0.35), 'measured two steps ' + r.gap);
      assert(r.bottom <= r.navTop + 1 && r.left >= 0 && r.right <= r.iw + 0.5 && r.sw <= r.iw, 'sheet ' + JSON.stringify(r));
      await page.screenshot({ path: SHOT('phone-follow-' + scheme) || undefined }).catch(() => {});
      await tap('.live-follow-card .lf-close');
      assert(!(await page.evaluate(() => window._liveFollowState())), 'closed');
      assert(!page.__errors.length, page.__errors[0]);
      await page.context().close();
    });
  }

  await browser.close();
  console.log('\n' + passed + ' passed, ' + failed + ' failed');
  process.exit(failed ? 1 : 0);
})().catch(async (e) => {
  console.error(e);
  if (browser) await browser.close().catch(() => {});
  process.exit(1);
});
