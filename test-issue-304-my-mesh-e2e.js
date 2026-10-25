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
// #384: a fixed advert age so the card's "· last …" text is pinned.
const advert33m = new Date(now - 33 * 60000).toISOString();
// #304 remainder: `previous` is the 24h window before the rolling one, the
// only baseline the card may compare against, and only when both windows are
// fully covered.
const prevStart = new Date(now - 48 * 3600000).toISOString();
const activity = (complete = true, previous = { windowStart: prevStart, windowEnd: start, count: 28, complete: true }) => ({
  windowStart: start, windowEnd: end, coverageStart: complete ? prevStart : new Date(now - 8 * 3600000).toISOString(), complete,
  buckets: Array.from({ length: 24 }, (_, i) => ({
    start: new Date(now - (24 - i) * 3600000).toISOString(),
    end: new Date(now - (23 - i) * 3600000).toISOString(),
    count: i === 3 ? 35 : 0,
  })),
  previous,
});
// An own advert older than the infra degraded threshold (24h): on its own it
// cannot carry an Active badge, so a 24h-fresh lastHeard can only come from
// attributed relay evidence.
const advertStale = new Date(now - 30 * 3600000).toISOString();

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
  for (const file of ['app.js', 'roles.js', 'heard-by.js', 'my-mesh-observers.js', 'my-mesh-activity.js']) await page.addScriptTag({ path: path.join(ROOT, file) });
  await page.evaluate((data) => {
    window.removeEventListener('hashchange', navigate);
    window.fixturePages = {};
    registerPage = (name, mod) => { window.fixturePages[name] = mod; };
    api = async (route) => {
      if (route === '/stats') return { totalNodes: 3 };
      // #262: the grid fetches the bulk observer list once. None of these
      // synthetic nodes is an observer, so the cards keep their #304 shape.
      if (route === '/observers') return { observers: [] };
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
        { role: 'repeater', observers: names.map((observer_name, i) => ({ observer_name, packetCount: i === 0 ? 30 : 0 })), activity: activity(),
          backfill: { status: 'pending', remaining: 15 },
          intervals: {
            zero_hop: { status: 'estimated', interval_s: 7200, samples: 5, last_advert: advert33m, confidence: 'medium' },
            flood: { status: 'too_few', interval_s: null, samples: 2, last_advert: end },
          } },
        { role: 'repeater', observers: [{ observer_name: 'One' }], activity: activity(false), stats: { lastAdvert: advertStale },
          backfill: { status: 'complete', remaining: 0 },
          intervals: { zero_hop: { status: 'none_observed', samples: 0 }, flood: { status: 'irregular', samples: 5 } } },
        { role: 'repeater', observers: [], activity: undefined, stats: { lastAdvert: null, avgSnr: 7, avgHops: null },
          intervals: { zero_hop: { status: 'estimated', interval_s: 3600, samples: 4 }, flood: { status: 'too_few', samples: 1 } } },
        { role: 'repeater', observers: [{ observer_name: CLIP }], activity: activity(false) },
        { role: 'client', observers: [{ observer_name: CLIP }, { observer_name: 'Shorty' }], activity: activity(false) },
        { role: 'repeater', observers: [{ observer_name: WIDE }], activity: activity(false) },
        { role: 'repeater', observers: SHORT_WIDE.map(observer_name => ({ observer_name })), activity: activity(false) },
        { role: 'client', observers: [{ observer_name: 'Shorty' }, ...SHORT_WIDE.map(observer_name => ({ observer_name }))], activity: activity(false) },
      ];
      const data = Object.fromEntries(specs.map((spec, i) => [`/nodes/${KEYS[i]}/health?include=advertIntervals`, {
        node: { name: `Node ${i}`, role: spec.role },
        stats: Object.assign({ lastHeard: end, lastAdvert: end, avgSnr: -3.25, avgHops: 10 }, spec.stats),
        directObservers: spec.observers, recentPackets: [], activity24h: spec.activity,
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
      // #384: each route row pins its value, sample count and latest advert.
      // Card 2's 3600 s is the hours/minutes boundary (≈ 1 h, not ≈ 60 min);
      // a row without last_advert has no "· last" part.
      const advertRows = async card => (await cards.nth(card).locator('.mnc-advert-row').allInnerTexts()).map(t => t.replace(/\s+/g, ' ').trim());
      const [zeroHop0, flood0] = await advertRows(0);
      assert.match(zeroHop0, /^Zero-hop ≈ 2 h · 5 heard · last 3[34]m ago$/);
      assert.match(flood0, /^Flood not enough adverts · 2 heard · last \d+s ago$/);
      assert.deepEqual(await advertRows(1), ['Zero-hop not observed · 0 heard', 'Flood irregular · 5 heard']);
      assert.deepEqual(await advertRows(2), ['Zero-hop ≈ 1 h · 4 heard', 'Flood not enough adverts · 1 heard']);
      assert.equal(await cards.nth(1).locator('.mnc-advert-provisional').count(), 0);
      assert.match(await cards.nth(2).locator('.mnc-advert-provisional').innerText(), /route classes provisional/i);
      assert.match(await cards.nth(3).locator('.mnc-advert-cadence').innerText(), /unavailable/i);
      assert.equal(await cards.nth(3).locator('.mnc-advert-provisional').count(), 0);
      assert.equal(await cards.nth(4).locator('.mnc-advert-cadence').count(), 0, 'non-repeater card must not show advert intervals');
      // ── #304 remainder: traffic, SNR attribution and status evidence ──
      // Card 0: fully covered window, own advert as fresh as the newest
      // evidence. Card 1: 8h coverage only and an own advert 30h old, so
      // the Active badge can only rest on attributed relay evidence.
      // Card 2: no rolling window, no own advert, no direct observer.
      await page.mouse.move(0, 0); // keep :hover off the border-colour probe
      const labels = async i => (await cards.nth(i).locator('.mnc-metrics .mnc-lbl').allInnerTexts()).map(t => t.replace(/\s+/g, ' ').trim());
      const vals = async i => (await cards.nth(i).locator('.mnc-metrics .mnc-val').allInnerTexts()).map(t => t.trim());
      assert.deepEqual(await labels(0), ['Transmissions · 24 h', 'Direct observers', 'Direct SNR · 30 receptions']);
      assert.deepEqual(await vals(0), ['35 +25%', '51', '-3.3 dB']);
      assert.deepEqual(await labels(1), ['Transmissions · 24 h', 'Direct observers', 'Direct SNR']);
      assert.deepEqual(await vals(1), ['≥ 35', '1', '-3.3 dB'], 'an incomplete window is a lower bound, and an unknown sample size prints no n=');
      assert.deepEqual(await vals(2), ['—', '0', '—'], 'no window and no direct attribution render em dashes, not zeroes');
      // The calendar-day counter and the bare hop average are gone from the
      // headline metrics, and no metric carries a quality verdict or colour.
      const metricText = (await page.locator('.mnc-metrics').allInnerTexts()).join(' ');
      assert.ok(!/today/i.test(metricText), 'no "Packets today" left: ' + metricText);
      assert.ok(!/avg hops/i.test(metricText), 'Avg hops must move into the details');
      assert.ok(!/excellent|\bgood\b|marginal|\bpoor\b/i.test(metricText), 'no SNR verdict: ' + metricText);
      assert.equal(await page.locator('.mnc-metrics .mnc-val[style]').count(), 0, 'no verdict colour on a metric value');
      // The previous-period comparison is like-for-like or absent: the inline
      // chip only appears for two complete windows with a non-zero baseline,
      // and its full wording is in the evidence disclosure either way.
      assert.equal(await cards.nth(0).locator('.mnc-metric-sub').innerText(), '+25%');
      assert.match(await cards.nth(0).locator('.mnc-metric-sub').getAttribute('title'), /^\+25% vs previous 24 h — /);
      for (const i of [1, 2]) {
        assert.equal(await cards.nth(i).locator('.mnc-metric-sub').count(), 0, `card ${i} must not show a comparison chip`);
      }

      // The badge: a confident own-advert green, and an attributed match that
      // is deliberately NOT green — dot, wording and card accent.
      assert.equal(await cards.nth(0).locator('.mnc-status .status-ok').count(), 1);
      assert.equal(await cards.nth(0).locator('.mnc-status use').getAttribute('href'), '/icons/phosphor-sprite.svg#ph-circle-fill');
      assert.equal((await cards.nth(0).locator('.mnc-status-text').innerText()).replace(/\s+/g, ' ').trim().split(' · ')[0], 'Active');
      for (const i of [1, 2]) {
        assert.equal(await cards.nth(i).locator('.mnc-status .status-ok').count(), 0, `card ${i} must not show a confident green`);
        assert.equal(await cards.nth(i).locator('.mnc-status .status-warn').count(), 1, `card ${i} must show the provisional dot`);
        assert.equal(await cards.nth(i).locator('.mnc-status use').getAttribute('href'), '/icons/phosphor-sprite.svg#ph-circle');
        assert.match(await cards.nth(i).locator('.mnc-status-text').innerText(), /Active · relay-attributed/);
        assert.equal(await cards.nth(i).evaluate(el => el.classList.contains('evidence-relay')), true);
      }
      const probe = await page.evaluate(() => {
        const root = getComputedStyle(document.documentElement);
        const resolve = name => {
          const el = document.createElement('div');
          el.style.color = root.getPropertyValue(name);
          document.body.appendChild(el);
          const value = getComputedStyle(el).color;
          el.remove();
          return value;
        };
        return { green: resolve('--status-green'), yellow: resolve('--status-yellow') };
      });
      const accents = await cards.evaluateAll(els => els.map(el => getComputedStyle(el).borderLeftColor));
      assert.equal(accents[0], probe.green, 'an own-advert Active card keeps the green accent');
      assert.equal(accents[1], probe.yellow, 'an attributed Active card must not keep the green accent');
      assert.notEqual(probe.green, probe.yellow);

      // The evidence disclosure: compact by default, reachable by keyboard
      // and touch, and it must not navigate to the health panel.
      const ev = cards.nth(1).locator('.mnc-evidence');
      assert.equal(await ev.evaluate(el => el.open), false, 'the disclosure starts closed so the card stays compact');
      assert.equal(await ev.locator('.mnc-evidence-row').first().isVisible(), false);
      await ev.locator('summary').focus();
      await page.keyboard.press('Enter');
      assert.equal(await ev.evaluate(el => el.open), true, 'Enter on the summary must open it');
      assert.equal(await page.locator('#homeHealth.visible').count(), 0, 'opening the evidence must not open the health panel');
      const rows = async i => (await cards.nth(i).locator('.mnc-evidence-row').allInnerTexts()).map(t => t.replace(/\s+/g, ' ').trim());
      const [advert1, relay1, window1, trend1, hops1] = await rows(1);
      assert.match(advert1, /^Last own advert 1d ago$/);
      assert.match(relay1, /^Last attributed relay activity \d+s ago$/);
      assert.match(window1, /^Counting window .+ to .+ \(coverage incomplete\)$/);
      assert.equal(trend1, 'Change vs previous 24 h no comparable 24 h history');
      assert.match(hops1, /^Avg observed path length 10 hops$/);
      assert.match(await cards.nth(1).locator('.mnc-evidence-note').first().innerText(), /prefix/i);
      await cards.nth(0).locator('.mnc-evidence summary').click();
      const [advert0, relay0, window0, trend0] = await rows(0);
      assert.match(advert0, /^Last own advert \d+s ago$/);
      assert.equal(relay0, 'Last attributed relay activity none newer than the last own advert');
      assert.ok(!/coverage incomplete/.test(window0), window0);
      assert.equal(trend0, 'Change vs previous 24 h +25% vs previous 24 h');
      assert.match(await cards.nth(0).locator('.mnc-evidence-note').first().innerText(), /own advert/i);
      await cards.nth(2).locator('.mnc-evidence summary').click();
      const [advert2, , , , hops2] = await rows(2);
      assert.equal(advert2, 'Last own advert none observed');
      assert.equal(hops2, 'Avg observed path length —');
      assert.equal(await page.locator('#homeHealth.visible').count(), 0);
      if (width < 500) {
        await cards.nth(3).locator('.mnc-evidence summary').tap();
        assert.equal(await cards.nth(3).locator('.mnc-evidence').evaluate(el => el.open), true, 'touch must open the disclosure');
        assert.equal(await page.locator('#homeHealth.visible').count(), 0, 'a tap on the summary must not navigate');
        await cards.nth(3).locator('.mnc-evidence summary').tap();
      }
      for (const i of [0, 1, 2]) await cards.nth(i).locator('.mnc-evidence summary').click();

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
