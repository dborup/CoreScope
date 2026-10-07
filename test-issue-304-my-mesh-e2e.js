#!/usr/bin/env node
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require('playwright');

const ROOT = path.join(__dirname, 'public');
const ORIGIN = 'http://127.0.0.1:18740';
const KEYS = ['a'.repeat(64), 'b'.repeat(64), 'c'.repeat(64), 'd'.repeat(64)];
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

async function render(page, responses, width) {
  await page.setViewportSize({ width, height: 800 });
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
    for (const width of [1280, 390]) {
      const context = await browser.newContext({ viewport: { width, height: 800 }, hasTouch: width < 500, isMobile: width < 500 });
      const page = await context.newPage();
      const errors = [];
      page.on('pageerror', e => errors.push(e.message));
      const names = Array.from({ length: 51 }, (_, i) => i === 50 ? '<img src=x onerror=alert(1)>' : `Observer ${i}`);
      names[1] = 'Very-long-observer-name-'.repeat(25);
      names[2] = '<svg onload=alert(1)>';
      const data = Object.fromEntries(KEYS.map((key, i) => [`/nodes/${key}/health?include=advertIntervals`, {
        node: { name: `Repeater ${i}`, role: 'repeater' }, stats: { lastHeard: end, packetsToday: 2 },
        observers: i === 0 ? names.map(observer_name => ({ observer_name })) : i === 1 ? [{ observer_name: 'One' }] : [],
        recentPackets: [], activity24h: i === 0 ? activity() : i === 1 ? activity(false) : undefined,
        advertRouteBackfill: i === 0 ? { status: 'pending', remaining: 15 } : i === 1 ? { status: 'complete', remaining: 0 } : undefined,
        advertIntervals: i === 0 ? {
          zero_hop: { status: 'estimated', interval_s: 7200, samples: 5, last_advert: end, confidence: 'medium' },
          flood: { status: 'too_few', interval_s: null, samples: 2, last_advert: end },
        } : i === 1 ? { zero_hop: { status: 'none_observed', samples: 0 }, flood: { status: 'irregular', samples: 5 } }
          : i === 2 ? { zero_hop: { status: 'estimated', interval_s: 3600, samples: 4 }, flood: { status: 'too_few', samples: 1 } } : undefined,
      }]));
      await render(page, data, width);
      if (process.env.SCREENSHOT_PATH && width === 1280) {
        await page.screenshot({ path: process.env.SCREENSHOT_PATH, fullPage: true });
      }
      const cards = page.locator('.my-node-card');
      const large = cards.nth(0);
      assert.equal(await large.locator('.mnc-observers .mnc-observer-name').count(), 3);
      assert.equal(await large.locator('.mnc-observers .mnc-observer-name').nth(1).evaluate(el => el.scrollWidth > el.clientWidth), true);
      assert.equal(await large.locator('.mnc-observers svg').count(), 0);
      assert.equal(await cards.nth(1).locator('.mnc-view-all').count(), 0);
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
      assert.deepEqual(errors, []);
      await context.close();
    }
    console.log('My Mesh #304: desktop/mobile card, sparkline, coverage and observer dialog passed');
  } finally { await browser.close(); }
})().catch(e => { console.error(e); process.exitCode = 1; });
