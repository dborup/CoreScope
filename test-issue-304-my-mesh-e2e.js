#!/usr/bin/env node
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require('playwright');

const ROOT = path.join(__dirname, 'public');
const ORIGIN = 'http://127.0.0.1:18740';
const KEYS = ['a', 'b', 'c', 'd', 'e', 'f', '1', '2'].map(c => c.repeat(64));
// #351 F2: a name wider than the 14ch clamp. Matches the review's repro name.
const CLIP = 'Jackrabbit Mountain Relay North';
// #351 R1: short (≤14 chars) but WIDE names. The clamp is CSS max-width: 14ch
// (fourteen "0" glyphs), so uppercase/wide glyphs clip well before 14 chars.
// WIDE clips in any sans font; the others are font-dependent and are checked
// by the measured invariant below rather than asserted to clip.
const WIDE = 'WWWWWWWWWWWWWW';
const SHORT_WIDE = ['MOUNT WOLFHAWK', 'MMMMMMMMMMMMM', 'ØSTERBRO MOLE'];
// #351 R2: a quote breaks out of an attribute that is not escapeAttr'd.
const QUOTE = 'x" onmouseover="alert(1)';
const now = Date.now();
const end = new Date(now).toISOString();
const start = new Date(now - 24 * 3600000).toISOString();
const activity = (complete = true) => ({
  windowStart: start, windowEnd: end, coverageStart: complete ? start : new Date(now - 8 * 3600000).toISOString(), complete,
  buckets: Array.from({ length: 24 }, (_, i) => ({
    start: new Date(now - (24 - i) * 3600000).toISOString(),
    end: new Date(now - (23 - i) * 3600000).toISOString(),
    count: i === 3 ? 35 : 0,
  })),
});

async function render(page, responses, width, height) {
  await page.setViewportSize({ width, height });
  await page.route('**/*', route => {
    const u = new URL(route.request().url());
    if (u.origin !== ORIGIN) return route.abort();
    if (u.pathname === '/icons/phosphor-sprite.svg') return route.fulfill({ contentType: 'image/svg+xml', body: fs.readFileSync(path.join(ROOT, 'icons/phosphor-sprite.svg')) });
    return route.fulfill({ contentType: 'text/html', body: '<meta name="viewport" content="width=device-width,initial-scale=1"><main id="fixture"></main>' });
  });
  await page.goto(ORIGIN + '/');
  await page.addStyleTag({ path: path.join(ROOT, 'style.css') });
  await page.addStyleTag({ path: path.join(ROOT, 'home.css') });
  for (const file of ['app.js', 'roles.js']) await page.addScriptTag({ path: path.join(ROOT, file) });
  await page.evaluate((data) => {
    window.removeEventListener('hashchange', navigate);
    window.fixturePages = {};
    registerPage = (name, mod) => { window.fixturePages[name] = mod; };
    api = async (route) => {
      if (route === '/stats') return { totalNodes: 3 };
      if (route in data) return data[route];
      throw Error('Unexpected API: ' + route);
    };
    localStorage.setItem('meshcore-user-level', 'experienced');
    localStorage.setItem('meshcore-my-nodes', JSON.stringify(Object.keys(data).map(route => ({ pubkey: route.split('/')[2], name: 'Node' }))));
  }, responses);
  await page.addScriptTag({ path: path.join(ROOT, 'home.js') });
  await page.evaluate(() => window.fixturePages.home.init(document.getElementById('fixture')));
  await page.waitForFunction((n) => document.querySelectorAll('.my-node-card').length === n, Object.keys(responses).length);
}

(async () => {
  const browser = await chromium.launch({ headless: true, args: ['--no-sandbox'] });
  try {
    for (const [width, height] of [[1280, 900], [375, 812]]) {
      const context = await browser.newContext({ viewport: { width, height }, hasTouch: width < 500, isMobile: width < 500 });
      const page = await context.newPage();
      const errors = [];
      page.on('pageerror', e => errors.push(e.message));
      const names = Array.from({ length: 51 }, (_, i) => i === 50 ? '<img src=x onerror=alert(1)>' : `Observer ${i}`);
      names[0] = QUOTE;
      names[1] = 'Very-long-observer-name-'.repeat(25);
      names[2] = '<svg onload=alert(1)>';
      // Card roles are mixed (#304): a 51-observer repeater, a 1-observer
      // repeater, an observer-less repeater, a ≤3-observer repeater with a
      // clipped name, and a NON-repeater with a clipped name. Repeaters 0–2
      // carry advert-interval estimates with pending, complete and missing
      // route-mask backfill; repeater 3 has no estimate at all (#352).
      const specs = [
        { role: 'repeater', observers: names.map(observer_name => ({ observer_name })), activity: activity(),
          backfill: { status: 'pending', remaining: 15 },
          intervals: {
            zero_hop: { status: 'estimated', interval_s: 7200, samples: 5, last_advert: end, confidence: 'medium' },
            flood: { status: 'too_few', interval_s: null, samples: 2, last_advert: end },
          } },
        { role: 'repeater', observers: [{ observer_name: 'One' }], activity: activity(false),
          backfill: { status: 'complete', remaining: 0 },
          intervals: { zero_hop: { status: 'none_observed', samples: 0 }, flood: { status: 'irregular', samples: 5 } } },
        { role: 'repeater', observers: [], activity: undefined,
          intervals: { zero_hop: { status: 'estimated', interval_s: 3600, samples: 4 }, flood: { status: 'too_few', samples: 1 } } },
        { role: 'repeater', observers: [{ observer_name: CLIP }], activity: activity(false) },
        { role: 'client', observers: [{ observer_name: CLIP }, { observer_name: 'Shorty' }], activity: activity(false) },
        { role: 'repeater', observers: [{ observer_name: WIDE }], activity: activity(false) },
        { role: 'repeater', observers: SHORT_WIDE.map(observer_name => ({ observer_name })), activity: activity(false) },
        { role: 'client', observers: [{ observer_name: 'Shorty' }, ...SHORT_WIDE.map(observer_name => ({ observer_name }))], activity: activity(false) },
      ];
      const data = Object.fromEntries(specs.map((spec, i) => [`/nodes/${KEYS[i]}/health?include=advertIntervals`, {
        node: { name: `Node ${i}`, role: spec.role }, stats: { lastHeard: end, packetsToday: 2 },
        observers: spec.observers, recentPackets: [], activity24h: spec.activity,
        advertRouteBackfill: spec.backfill, advertIntervals: spec.intervals,
      }]));
      await render(page, data, width, height);
      if (process.env.SCREENSHOT_PATH && width === 1280) {
        await page.screenshot({ path: process.env.SCREENSHOT_PATH, fullPage: true });
      }
      const cards = page.locator('.my-node-card');
      const large = cards.nth(0);
      assert.equal(await large.locator('.mnc-observers .mnc-observer-name').count(), 3);
      assert.equal(await large.locator('.mnc-observers .mnc-observer-name').nth(1).evaluate(el => el.scrollWidth > el.clientWidth), true);
      assert.equal(await large.locator('.mnc-observers .mnc-observer-name').nth(1).getAttribute('title'), names[1]);
      assert.equal(await large.locator('.mnc-observers svg').count(), 0);
      assert.equal(await cards.nth(1).locator('.mnc-view-all:visible').count(), 0);
      assert.equal(await cards.nth(2).locator('.mnc-observers').count(), 0);
      assert.equal(await large.locator('.home-spark-bar').count(), 24);
      assert.match(await large.locator('.mnc-spark').innerText(), /Node-associated transmissions.*last 24h/);
      assert.match(await large.locator('.home-spark-bar').nth(3).getAttribute('aria-label'), /35 node-associated transmissions/);
      assert.match(await large.locator('.home-spark-bar').nth(3).getAttribute('title'), /35 node-associated transmissions/);
      assert.match(await large.locator('.home-spark-bar').first().getAttribute('aria-label'), /0 node-associated transmissions/);
      assert.equal(await cards.nth(1).locator('.home-spark-bar').count(), 0);
      assert.match(await cards.nth(1).locator('.mnc-spark').innerText(), /unavailable/i);
      assert.equal(await cards.nth(2).locator('.home-spark-bar').count(), 0);
      assert.match(await cards.nth(2).locator('.mnc-spark').innerText(), /unavailable/i);
      assert.match(await large.locator('.mnc-advert-cadence').innerText(), /Zero-hop[\s\S]*≈ 2 h[\s\S]*Flood[\s\S]*not enough adverts/i);
      assert.match(await large.locator('.mnc-advert-note').getAttribute('title'), /observed estimate.*not the configured timer/i);
      assert.match(await large.locator('.mnc-advert-provisional').innerText(), /route classes provisional/i);
      assert.match(await cards.nth(1).locator('.mnc-advert-cadence').innerText(), /not observed[\s\S]*irregular/i);
      assert.equal(await cards.nth(1).locator('.mnc-advert-provisional').count(), 0);
      assert.match(await cards.nth(2).locator('.mnc-advert-provisional').innerText(), /route classes provisional/i);
      assert.match(await cards.nth(3).locator('.mnc-advert-cadence').innerText(), /unavailable/i);
      assert.equal(await cards.nth(3).locator('.mnc-advert-provisional').count(), 0);
      assert.equal(await cards.nth(4).locator('.mnc-advert-cadence').count(), 0, 'non-repeater card must not show advert intervals');
      const heights = await cards.evaluateAll(els => els.map(el => el.getBoundingClientRect().height));
      assert(heights[0] < 350, `repeater card too tall: ${heights[0]} at ${width}px`);
      const open = large.locator('.mnc-view-all');
      await open.focus();
      await page.keyboard.press('Enter');
      const dialog = page.locator('#homeObserversDialog');
      await dialog.waitFor({ state: 'visible' });
      assert.equal(await dialog.locator('li').count(), 51);
      assert.equal(await dialog.locator('img').count(), 0);
      assert.equal(await page.locator('#homeHealth.visible').count(), 0);
      await page.keyboard.press('Escape');
      assert.equal(await dialog.isVisible(), false);
      assert.equal(await open.evaluate(el => el === document.activeElement), true);
      await open.click();
      await dialog.waitFor({ state: 'visible' });
      const dialogBox = await dialog.boundingBox();
      assert(dialogBox && dialogBox.width <= width - 16, 'dialog must fit viewport');
      await page.mouse.click(2, 2);
      assert.equal(await dialog.isVisible(), false);
      if (width < 500) {
        await open.tap();
        await dialog.waitFor({ state: 'visible' });
        await dialog.locator('.mnc-dialog-close').tap();
      }

      // #351 F2: clipped names stay recoverable on EVERY card — a ≤3-observer
      // repeater (card 3) and a NON-repeater (card 4), not only repeaters
      // with >3 observers. Title tooltip (pointer/AT) + accessible dialog
      // (keyboard/touch), and the cards stay compact.
      for (const idx of [3, 4]) {
        const card = cards.nth(idx);
        const span = card.locator('.mnc-observer-name').first();
        assert.equal(await span.evaluate(el => el.scrollWidth > el.clientWidth), true, `card ${idx} name must clip at ${width}px`);
        assert.equal(await span.getAttribute('title'), CLIP, `card ${idx} span must carry the full name in title`);
        assert.equal(await card.locator('.mnc-view-all:visible').count(), 1, `card ${idx} must offer the observer dialog`);
      }
      assert.equal(await cards.nth(4).locator('.mnc-observer-name').count(), 2);
      const clipBtn = cards.nth(4).locator('.mnc-view-all');
      await clipBtn.scrollIntoViewIfNeeded();
      await clipBtn.click();
      await dialog.waitFor({ state: 'visible' });
      assert((await dialog.locator('li').allInnerTexts()).includes(CLIP), 'dialog must list the full clipped name');
      await page.keyboard.press('Escape');
      assert.equal(await dialog.isVisible(), false);
      const h = await cards.evaluateAll(els => els.map(el => el.getBoundingClientRect().height));
      assert(h[3] < 350 && h[4] < 350, `clipped cards too tall: ${h[3]}, ${h[4]} at ${width}px`);

      // #351 R1: "clipped" is what the browser rendered, not a character
      // count. On EVERY card the dialog button is visible exactly when the
      // preview hides observers or some shown name is visually clipped
      // (scrollWidth > clientWidth: the case where the ellipsis is drawn).
      const disclosure = await cards.evaluateAll(els => els.map(card => {
        const btn = card.querySelector('.mnc-view-all');
        return {
          clipped: [...card.querySelectorAll('.mnc-observer-name')].filter(s => s.scrollWidth > s.clientWidth).map(s => s.textContent),
          visible: !!btn && btn.checkVisibility(),
        };
      }));
      disclosure.forEach((d, idx) => {
        const hides = specs[idx].role === 'repeater' && specs[idx].observers.length > 3;
        assert.equal(d.visible, hides || d.clipped.length > 0,
          `card ${idx} at ${width}px: clipped=${JSON.stringify(d.clipped)} hides=${hides} but button visible=${d.visible}`);
      });
      assert.deepEqual(disclosure[5].clipped, [WIDE], `a 14-char wide name must clip at ${width}px`);
      const wideBtn = cards.nth(5).locator('.mnc-view-all');
      await wideBtn.scrollIntoViewIfNeeded();
      if (width < 500) await wideBtn.tap(); else await wideBtn.click();
      await dialog.waitFor({ state: 'visible' });
      assert.deepEqual(await dialog.locator('li').allInnerTexts(), [WIDE], 'dialog must list the full short-but-wide name');
      await page.keyboard.press('Escape');
      assert.equal(await dialog.isVisible(), false);

      // #351 R2: a name with a double quote must stay inside the title
      // attribute — no injected attributes on any observer span.
      const attrs = await page.locator('.mnc-observer-name').evaluateAll(els => els.map(el => ({ names: el.getAttributeNames().sort(), title: el.title, text: el.textContent })));
      attrs.forEach(a => {
        assert.deepEqual(a.names, ['class', 'title'], `observer span gained attributes: ${a.names}`);
        assert.equal(a.title, a.text, 'title must carry the full, unaltered name');
      });
      assert.equal(attrs.filter(a => a.title === QUOTE).length, 1, 'quoted name must render verbatim');
      assert.equal(await page.locator('[onmouseover]').count(), 0);

      // #352: a card click opens health with the card's own opt-in URL, so
      // the client cache answers it instead of a second request. The stub
      // throws on any other route, which loadHealth shows as a failure.
      await cards.nth(1).locator('.mnc-status-text').click();
      const panel = page.locator('#homeHealth');
      await page.waitForFunction(() => { const el = document.getElementById('homeHealth'); return el && !/Loading/.test(el.textContent); });
      assert.equal(await panel.locator('.health-banner').count(), 1, `card click must render health from the card's request: ${await panel.innerText()}`);
      assert.match(await panel.locator('.health-banner').innerText(), /Node 1/);
      // A node outside My Mesh keeps the plain URL: no advert scan for it.
      const routes = await page.evaluate(async (key) => {
        const seen = [];
        const real = api;
        api = async (route, opts) => { seen.push(route); return real(route, opts); };
        localStorage.setItem('meshcore-my-nodes', JSON.stringify(JSON.parse(localStorage.getItem('meshcore-my-nodes')).filter(n => n.pubkey !== key)));
        document.querySelectorAll('.my-node-card')[1].querySelector('.mnc-status-text').click();
        await new Promise(r => setTimeout(r, 0));
        api = real;
        return seen;
      }, KEYS[1]);
      assert.deepEqual(routes, [`/nodes/${KEYS[1]}/health`]);

      assert.deepEqual(errors, []);
      await context.close();
    }
    console.log('My Mesh #304: desktop/mobile card, sparkline, coverage and observer dialog passed');
  } finally { await browser.close(); }
})().catch(e => { console.error(e); process.exitCode = 1; });
