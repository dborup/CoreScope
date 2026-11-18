/**
 * E2E (#881 M3): the Live Moments viewer settings and theme tokens in the
 * customizer, and what they do to the Live page.
 *
 * As in the M2 suite the server has Moments off, so the moments block of
 * /api/config/client is added by interception, GET /api/moments is stubbed
 * and the app socket is replaced with page.routeWebSocket.
 *
 *   1. Display tab: the Live Moments section with the defaults, 48 px rows;
 *      none without the server's block.
 *   2. Cards at once: 5 frames, setting 2 -> two cards and a +3 chip; a
 *      change while cards are up re-fits the stack at once.
 *   3. Fade time: 3 s -> a card is gone after ~3.5 s, still up at 2 s.
 *   4. Per-kind switch: record link off -> no card and no drawer row;
 *      back on -> a card again.
 *   5. On/off default: off -> the Live page starts without a controller and
 *      an unchecked box; ?moments=1 wins; the Live checkbox wins until the
 *      default is changed again; Reset All restores every default.
 *   6. Theme: a New Node colour reaches the card's stripe and icon; a
 *      colourblind preset changes the four accents.
 *   7. Reload and export round trip; no operator-policy key in the export.
 *   8. 375x560 touch phone: More -> Customize -> Display, every Moments
 *      control changed by a real tap inside the screen; the Live page then
 *      shows one card whatever the stack setting.
 *
 * Usage: BASE_URL=http://localhost:13581 node test-881-m3-moments-customizer-e2e.js
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
const nodeMoment = (id, i) => ({ id, kind: 'new_node', at: at(2), node: { publicKey: PK('a'), name: 'E2E-881M3 ' + id, role: 'repeater', lat: 55.2 + (i || 0) * 0.01, lon: 9.4 } });
const linkMoment = (id) => ({ id, kind: 'record_link', at: at(2), previousKm: 40,
  link: { a: { publicKey: PK('c'), name: 'A', role: 'repeater', lat: 55, lon: 9 }, b: { publicKey: PK('d'), name: 'B', role: 'room', lat: 55.5, lon: 10 }, distanceKm: 61.2 } });

async function open(browser, opts) {
  opts = opts || {};
  const ctx = await browser.newContext(Object.assign({ viewport: opts.viewport || { width: 1440, height: 900 } }, opts.touch ? { hasTouch: true, isMobile: true } : {}));
  const h = { ctx, sockets: [], errors: [], momentsRequests: 0 };
  if (!opts.noBlock) {
    await ctx.route(/\/api\/config\/client(\?|$)/, async (route) => {
      const res = await route.fetch();
      const body = await res.json();
      body.moments = { enabled: true, historySize: 50 };
      await route.fulfill({ response: res, json: body });
    });
  }
  await ctx.route(/\/api\/moments(\?|$)/, (route) => { h.momentsRequests++; return route.fulfill({ json: { moments: [] } }); });
  await ctx.routeWebSocket(/.*/, (ws) => { h.sockets.push(ws); });
  const page = await ctx.newPage();
  page.on('pageerror', (e) => h.errors.push(String(e.stack || e.message).split('\n').slice(0, 4).join(' | ')));
  h.page = page;
  h.send = async (frames) => {
    const ws = h.sockets[h.sockets.length - 1];
    assert(ws, 'the app socket connected');
    for (const f of [].concat(frames)) ws.send(JSON.stringify({ type: 'moment', data: f }));
  };
  h.ready = async (viewerOff) => {
    await page.waitForFunction(() => window._liveWSHandler && window._liveWSHandler() && window._liveGetMap && window._liveGetMap(), null, { timeout: 20000 });
    await page.evaluate(() => window.MeshConfigReady);
    await page.waitForFunction(() => window._customizerV2 && window._customizerV2.initDone, null, { timeout: 15000 });
    if (opts.noBlock) return;
    if (viewerOff) await page.waitForFunction(() => getComputedStyle(document.getElementById('liveMomentsLabel')).display !== 'none', null, { timeout: 10000 });
    else await page.waitForFunction(() => window._liveMoments && window._liveMoments(), null, { timeout: 10000 });
  };
  h.goto = async (query, viewerOff) => {
    await page.goto(BASE + '/?r=' + Date.now() + '#/live?' + VIEW + (query || ''), { waitUntil: 'domcontentloaded' });
    await h.ready(viewerOff);
  };
  // The palette button, or on a phone the More sheet, then Customize, then a tab.
  h.openTab = async (tab, phone) => {
    if (!(await page.$('.cust-overlay:not(.hidden)'))) {
      if (phone) {
        await page.tap('[data-bottom-nav-tab="more"]');
        await page.waitForSelector('[data-mpa-mirror="customizeToggle"]', { state: 'visible', timeout: 5000 });
        await page.tap('[data-mpa-mirror="customizeToggle"]');
      } else await page.click('#customizeToggle');
      await page.waitForSelector('.cust-overlay:not(.hidden)', { timeout: 5000 });
    }
    if (phone) await page.tap('.cust-tab[data-tab="' + tab + '"]'); else await page.click('.cust-tab[data-tab="' + tab + '"]');
    await page.waitForSelector('.cust-panel[data-panel="' + tab + '"].active', { timeout: 4000 });
  };
  h.setNum = async (key, v) => {
    await page.fill('[data-cv2-moments="' + key + '"]', String(v));
    await page.dispatchEvent('[data-cv2-moments="' + key + '"]', 'change');
    await page.waitForTimeout(600); // the customizer's debounced write
  };
  h.toggle = async (key) => { await page.click('label[for="cv2-moments-' + key + '"]'); await page.waitForTimeout(600); };
  h.cards = () => page.evaluate(() => document.querySelectorAll('.live-moment-card:not(.is-leaving)').length);
  return h;
}

(async () => {
  const browser = await chromium.launch({ headless: true, executablePath: process.env.CHROMIUM_PATH || undefined });
  try {
    console.log('\n=== #881 M3 Moments customizer E2E against ' + BASE + ' ===');

    await step('Display tab: the Live Moments section, defaults, 48 px rows; none without the server block', async () => {
      const h = await open(browser);
      await h.goto();
      await h.openTab('display');
      const s = await h.page.evaluate(() => {
        const get = (k) => document.querySelector('[data-cv2-moments="' + k + '"]');
        const rows = [...document.querySelectorAll('label.cust-check[for^="cv2-moments-"]')].map((l) => l.getBoundingClientRect().height);
        return { title: [...document.querySelectorAll('.cust-section-title')].some((p) => p.textContent === 'Live Moments'),
          enabled: get('enabled').checked, cards: get('visibleCards').value, dwell: get('dwellSeconds').value,
          kinds: ['showNewNode', 'showBackOnline', 'showRecordLink', 'showBusiest'].map((k) => get(k).checked), rows,
          policy: !!document.querySelector('[data-cv2-moments="maxPerMinute"],[data-cv2-moments="historySize"]') };
      });
      assert(s.title && s.enabled && s.cards === '3' && s.dwell === '8' && s.kinds.every(Boolean), JSON.stringify(s));
      assert(s.rows.length === 5 && s.rows.every((r) => r >= 48), 'rows 48px: ' + JSON.stringify(s.rows));
      assert(!s.policy, 'operator policy offered');
      assert(!h.errors.length, h.errors.join('; '));
      await h.ctx.close();
      const n = await open(browser, { noBlock: true });
      await n.goto();
      await n.openTab('display');
      assert(await n.page.evaluate(() => !document.querySelector('[data-cv2-moments]')), 'a section without the server block');
      await n.ctx.close();
    });

    await step('cards at once: the stack follows the setting, live while cards are up', async () => {
      const h = await open(browser);
      await h.goto();
      await h.openTab('display');
      await h.setNum('visibleCards', 2);
      await h.send([0, 1, 2, 3, 4].map((i) => nodeMoment('v' + i, i)));
      await h.page.waitForFunction(() => document.querySelectorAll('.live-moment-card').length >= 2);
      await h.page.waitForTimeout(300);
      const a = await h.page.evaluate(() => ({ cards: document.querySelectorAll('.live-moment-card:not(.is-leaving)').length, more: document.querySelector('.live-moments-more').textContent }));
      assert(a.cards === 2 && a.more === '+3', JSON.stringify(a));
      await h.setNum('visibleCards', 1);
      await h.page.waitForTimeout(400);
      assert(await h.cards() === 1, 'the stack did not shrink at once');
      await h.setNum('visibleCards', 99); // out of range: put back
      assert(await h.page.inputValue('[data-cv2-moments="visibleCards"]') === '1', 'a bad value stayed in the field');
      assert(!h.errors.length, h.errors.join('; '));
      await h.ctx.close();
    });

    await step('fade time: a card stays for the setting', async () => {
      const h = await open(browser);
      await h.goto();
      await h.openTab('display');
      await h.setNum('dwellSeconds', 3);
      await h.send([nodeMoment('f1')]);
      await h.page.waitForSelector('.live-moment-card');
      const t0 = Date.now();
      await h.page.waitForTimeout(1800);
      assert(await h.cards() === 1, 'gone before 3 s');
      await h.page.waitForFunction(() => !document.querySelector('.live-moment-card:not(.is-leaving)'), null, { timeout: 4000 });
      const dt = Date.now() - t0;
      assert(dt >= 2800 && dt < 4500, 'faded after ' + dt + ' ms');
      await h.ctx.close();
    });

    await step('per-kind switch: a hidden kind gets no card and no drawer row', async () => {
      const h = await open(browser);
      await h.goto();
      await h.openTab('display');
      await h.toggle('showRecordLink');
      await h.send([linkMoment('k1'), nodeMoment('k2')]);
      await h.page.waitForSelector('.live-moment-card');
      await h.page.waitForTimeout(300);
      const kinds = await h.page.evaluate(() => [...document.querySelectorAll('.live-moment-card')].map((c) => c.getAttribute('data-kind')));
      assert(kinds.length === 1 && kinds[0] === 'new_node', JSON.stringify(kinds));
      await h.page.evaluate(() => window._liveMoments().openDrawer());
      const rows = await h.page.evaluate(() => [...document.querySelectorAll('.live-moment-row')].map((c) => c.getAttribute('data-kind')));
      assert(rows.length === 1 && rows[0] === 'new_node', 'drawer: ' + JSON.stringify(rows));
      await h.toggle('showRecordLink');
      await h.send([linkMoment('k3')]);
      await h.page.waitForFunction(() => [...document.querySelectorAll('.live-moment-card')].some((c) => c.getAttribute('data-kind') === 'record_link'), null, { timeout: 5000 });
      await h.ctx.close();
    });

    await step('on/off default: off starts the Live page without Moments; link and checkbox win; Reset All restores', async () => {
      const h = await open(browser);
      await h.goto();
      await h.page.evaluate(() => localStorage.setItem('live-moments', 'true')); // the viewer once chose on
      await h.openTab('display');
      await h.toggle('enabled'); // default off: it forgets that choice, and the controller goes at once
      assert(await h.page.evaluate(() => localStorage.getItem('live-moments')) === null, 'the saved choice outlived a new default');
      await h.page.waitForFunction(() => !window._liveMoments(), null, { timeout: 4000 });
      assert(await h.page.evaluate(() => !document.getElementById('liveMomentsToggle').checked), 'the checkbox did not follow');
      await h.goto('', true);
      const off = await h.page.evaluate(() => ({ ctrl: !!window._liveMoments(), checked: document.getElementById('liveMomentsToggle').checked, dom: document.querySelectorAll('.live-moments').length }));
      assert(!off.ctrl && !off.checked && off.dom === 0, 'default off ignored: ' + JSON.stringify(off));
      const reqs = h.momentsRequests;
      await h.page.goto(BASE + '/?r=' + Date.now() + '#/live?' + VIEW + '&moments=1', { waitUntil: 'domcontentloaded' });
      await h.ready();
      assert(await h.page.evaluate(() => !!window._liveMoments()), '?moments=1 lost to the default');
      assert(h.momentsRequests > reqs, 'no history request with the link');
      // The Live checkbox is the viewer's own choice: off here, stored.
      await h.page.click('#liveControlsToggle');
      await h.page.uncheck('#liveMomentsToggle');
      assert(await h.page.evaluate(() => localStorage.getItem('live-moments')) === 'false');
      // Reset All: the section and the saved choice are gone, Moments are on.
      await h.page.click('#liveControlsToggle');
      await h.openTab('export');
      h.page.once('dialog', (d) => d.accept());
      await h.page.click('#cv2ResetAll');
      await h.page.waitForTimeout(500);
      const r = await h.page.evaluate(() => ({ stored: localStorage.getItem('live-moments'), ov: localStorage.getItem('cs-theme-overrides') }));
      assert(r.stored === null && !(r.ov && /moments/.test(r.ov)), JSON.stringify(r));
      await h.goto();
      assert(await h.page.evaluate(() => !!window._liveMoments()), 'Moments not back on after Reset All');
      assert(!h.errors.length, h.errors.join('; '));
      await h.ctx.close();
    });

    await step('theme: a New Node colour and a colourblind preset reach the card', async () => {
      const h = await open(browser);
      await h.goto();
      await h.send([nodeMoment('t1')]);
      await h.page.waitForSelector('.live-moment-card');
      const colour = () => h.page.evaluate(() => {
        const c = document.querySelector('.live-moment-card');
        return { stripe: getComputedStyle(c).borderLeftColor, icon: getComputedStyle(c.querySelector('.ph-icon')).color, bg: getComputedStyle(c).backgroundColor };
      });
      const before = await colour();
      assert(before.stripe === before.icon, 'stripe and icon share the accent: ' + JSON.stringify(before));
      await h.page.evaluate(() => window.MeshCorePresets.applyPreset('trit', { skipPersist: true }));
      const trit = await colour();
      assert(trit.stripe === 'rgb(42, 143, 76)' && trit.icon === trit.stripe, 'preset accent: ' + JSON.stringify(trit));
      await h.page.evaluate(() => {
        const api = window._customizerV2;
        api.writeOverrides({ theme: { momentNewNode: '#112233', momentCardBg: '#fefefe' } });
        api.applyCSS(api.computeEffective({}, api.readOverrides()), api.readOverrides());
      });
      const after = await colour();
      assert(after.stripe === 'rgb(17, 34, 51)' && after.icon === 'rgb(17, 34, 51)', 'theme colour: ' + JSON.stringify(after));
      assert(after.bg === 'rgb(254, 254, 254)', 'card fill: ' + JSON.stringify(after));
      // The Theme tab lists the group.
      await h.openTab('theme');
      assert(await h.page.evaluate(() => !!document.querySelector('.cust-moments-colors')), 'no Live moments cards group');
      await h.ctx.close();
    });

    await step('reload and export round trip; no operator-policy key', async () => {
      const h = await open(browser);
      await h.goto();
      await h.openTab('display');
      await h.setNum('dwellSeconds', 12);
      await h.toggle('showBusiest');
      await h.goto();
      await h.openTab('display');
      const v = await h.page.evaluate(() => ({ dwell: document.querySelector('[data-cv2-moments="dwellSeconds"]').value, busiest: document.querySelector('[data-cv2-moments="showBusiest"]').checked }));
      assert(v.dwell === '12' && v.busiest === false, 'did not survive a reload: ' + JSON.stringify(v));
      await h.openTab('export');
      const json = JSON.parse(await h.page.inputValue('#cv2ExportJson'));
      assert(JSON.stringify(json.moments) === JSON.stringify({ dwellSeconds: 12, showBusiest: false }), JSON.stringify(json));
      assert(!/maxPerMinute|historySize|backOnlineHours/.test(JSON.stringify(json)));
      await h.ctx.close();
    });

    await step('375x560 phone: every Moments control changed by a real tap, inside the screen', async () => {
      const h = await open(browser, { viewport: { width: 375, height: 560 }, touch: true });
      await h.page.goto(BASE + '/?r=' + Date.now() + '#/', { waitUntil: 'domcontentloaded' });
      await h.page.waitForSelector('[data-bottom-nav-tab="live"]', { state: 'visible', timeout: 20000 });
      await h.page.evaluate(() => localStorage.setItem('live-map-view', JSON.stringify({ lat: 55.3, lng: 9.6, zoom: 9 })));
      await h.page.tap('[data-bottom-nav-tab="live"]');
      await h.ready();
      await h.openTab('display', true);
      const inScreen = async (selector) => {
        const loc = h.page.locator(selector).first();
        await loc.scrollIntoViewIfNeeded();
        const b = await loc.boundingBox();
        assert(b && b.x >= 0 && b.x + b.width <= 375 && b.y >= 0 && b.y + b.height <= 560, selector + ' off screen ' + JSON.stringify(b));
        return b;
      };
      const tapCheck = async (key, want) => {
        const label = 'label[for="cv2-moments-' + key + '"]';
        const b = await inScreen(label);
        assert(b.height >= 48, key + ' row ' + b.height + ' px');
        await h.page.tap(label);
        await h.page.waitForTimeout(700);
        assert((await h.page.isChecked('[data-cv2-moments="' + key + '"]')) === want, key + ' did not follow the tap');
      };
      const tapType = async (key, value) => {
        const sel = '[data-cv2-moments="' + key + '"]';
        const b = await inScreen(sel);
        assert(b.height >= 44, key + ' input ' + b.height + ' px');
        await h.page.tap(sel);
        await h.page.keyboard.press('ControlOrMeta+A');
        await h.page.keyboard.press('Backspace');
        await h.page.keyboard.type(String(value));
        const t = h.page.locator('.cust-section-title', { hasText: 'Live Moments' });
        await t.scrollIntoViewIfNeeded();
        await t.tap();
        await h.page.waitForTimeout(700);
        assert(await h.page.inputValue(sel) === String(value), key + ' did not take ' + value);
      };
      await tapType('visibleCards', 5);
      await tapType('dwellSeconds', 20);
      await tapCheck('showNewNode', false);
      await tapCheck('showNewNode', true);
      await tapCheck('showBackOnline', false);
      await tapCheck('showRecordLink', false);
      await tapCheck('showBusiest', false);
      const stored = await h.page.evaluate(() => JSON.parse(localStorage.getItem('cs-theme-overrides')).moments);
      assert(stored.visibleCards === 5 && stored.dwellSeconds === 20 && stored.showBackOnline === false && stored.showRecordLink === false && stored.showBusiest === false, JSON.stringify(stored));
      assert(await h.page.evaluate(() => document.documentElement.scrollWidth - innerWidth) <= 0, 'sideways scroll');
      // Close the panel: the Live page keeps one card on a phone, only new nodes.
      await h.page.tap('.cust-close');
      await h.page.waitForTimeout(300);
      await h.send([linkMoment('p0'), nodeMoment('p1', 1), nodeMoment('p2', 2)]);
      await h.page.waitForSelector('.live-moment-card');
      await h.page.waitForTimeout(400);
      const p = await h.page.evaluate(() => [...document.querySelectorAll('.live-moment-card')].map((c) => c.getAttribute('data-kind')));
      assert(p.length === 1 && p[0] === 'new_node', 'phone cards: ' + JSON.stringify(p));
      assert(!h.errors.length, h.errors.join('; '));
      await h.ctx.close();
    });
  } finally {
    await browser.close();
  }
  console.log('\n' + passed + ' passed, ' + failed + ' failed');
  process.exit(failed ? 1 : 0);
})();
