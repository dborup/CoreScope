#!/usr/bin/env node
/* E2E for issue #902 — My Mesh credits a node that is a reporting observer,
 * and companions/sensors read "Quiet" instead of "Degraded".
 *
 * Renders the real home.js My Mesh grid (with the real roles.js,
 * my-mesh-observers.js and my-mesh-activity.js) against stubbed health and
 * /observers responses at 1280x900 and 375x560 (touch). Synthetic keys only.
 */
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require('playwright');

const ROOT = path.join(__dirname, 'public');
const ORIGIN = 'http://127.0.0.1:13802';
const MIN = 60000;
const HOUR = 60 * MIN;
const now = Date.now();
const iso = ago => new Date(now - ago).toISOString();

// One card per case in the issue's test list.
const CASES = [
  { key: '1'.repeat(64), name: 'Quiet Companion', role: 'companion', advertAgo: 10 * HOUR, observerAgo: null },
  { key: '2'.repeat(64), name: 'Observer Companion', role: 'companion', advertAgo: 10 * HOUR, observerAgo: MIN },
  { key: '3'.repeat(64), name: 'Stale Observer', role: 'companion', advertAgo: 10 * HOUR, observerAgo: 2 * HOUR },
  { key: '4'.repeat(64), name: 'Old Repeater', role: 'repeater', advertAgo: 30 * HOUR, observerAgo: null },
  { key: '5'.repeat(64), name: 'Old Room', role: 'room', advertAgo: 30 * HOUR, observerAgo: null },
  { key: '6'.repeat(64), name: 'Quiet Sensor', role: 'sensor', advertAgo: 10 * HOUR, observerAgo: null },
];

const health = Object.fromEntries(CASES.map(c => [`/nodes/${c.key}/health?include=advertIntervals`, {
  node: { name: c.name, role: c.role },
  stats: { lastHeard: iso(c.advertAgo), lastAdvert: iso(c.advertAgo), avgSnr: 1, avgHops: 1 },
  directObservers: [], recentPackets: [],
}]));
const observers = {
  observers: CASES.filter(c => c.observerAgo != null)
    // Observer ids are stored uppercase (#1836): the match must survive it.
    .map(c => ({ id: c.key.toUpperCase(), name: c.name + ' uplink', last_seen: iso(c.observerAgo), packetsLastHour: 3 })),
};

async function render(page) {
  await page.route('**/*', route => {
    const u = new URL(route.request().url());
    if (u.origin !== ORIGIN) return route.abort();
    if (u.pathname === '/icons/phosphor-sprite.svg') return route.fulfill({ contentType: 'image/svg+xml', body: fs.readFileSync(path.join(ROOT, 'icons/phosphor-sprite.svg')) });
    return route.fulfill({ contentType: 'text/html', body: '<meta name="viewport" content="width=device-width,initial-scale=1"><main id="fixture"></main>' });
  });
  await page.goto(ORIGIN + '/');
  await page.addStyleTag({ path: path.join(ROOT, 'style.css') });
  await page.addStyleTag({ path: path.join(ROOT, 'home.css') });
  for (const file of ['app.js', 'roles.js', 'heard-by.js', 'observers.js', 'my-mesh-observers.js', 'my-mesh-activity.js']) await page.addScriptTag({ path: path.join(ROOT, file) });
  await page.evaluate(({ health, observers }) => {
    window.removeEventListener('hashchange', navigate);
    window.fixturePages = {};
    window.apiCalls = [];
    registerPage = (name, mod) => { window.fixturePages[name] = mod; };
    api = async (route) => {
      window.apiCalls.push(route);
      if (route === '/stats') return { totalNodes: 6 };
      if (route === '/observers') return observers;
      if (route in health) return health[route];
      throw Error('Unexpected API: ' + route);
    };
    localStorage.setItem('meshcore-user-level', 'experienced');
    localStorage.setItem('meshcore-my-nodes', JSON.stringify(Object.keys(health).map(route => ({ pubkey: route.split('/')[2], name: 'Node' }))));
  }, { health, observers });
  await page.addScriptTag({ path: path.join(ROOT, 'home.js') });
  await page.evaluate(() => window.fixturePages.home.init(document.getElementById('fixture')));
  await page.waitForFunction(n => document.querySelectorAll('.my-node-card').length === n, CASES.length);
}

const card = (page, i) => page.locator(`.my-node-card[data-key="${CASES[i].key}"]`);
const color = (loc, prop) => loc.evaluate((el, p) => getComputedStyle(el)[p], prop);
const varColor = (page, name) => page.evaluate(n => {
  const probe = document.createElement('span');
  probe.style.color = `var(${n})`;
  document.body.appendChild(probe);
  const c = getComputedStyle(probe).color;
  probe.remove();
  return c;
}, name);

(async () => {
  const browser = await chromium.launch({ headless: true, args: ['--no-sandbox'] });
  let checks = 0;
  try {
    for (const [width, height] of [[1280, 900], [375, 560]]) {
      const touch = width < 500;
      const context = await browser.newContext({ viewport: { width, height }, hasTouch: touch, isMobile: touch });
      const page = await context.newPage();
      const errors = [];
      page.on('pageerror', e => errors.push(e.message));
      await render(page);
      const muted = await varColor(page, '--text-muted');
      const yellow = await varColor(page, '--status-yellow');
      const green = await varColor(page, '--status-green');

      // 1. companion, advert 10 h old, not an observer: Quiet, neutral colour.
      const quiet = card(page, 0);
      assert.match(await quiet.locator('.mnc-status-text').innerText(), /^Quiet · last heard /);
      assert.doesNotMatch(await quiet.innerText(), /Degraded/);
      assert.equal(await quiet.locator('.mnc-status span').getAttribute('class'), 'status-muted');
      assert.equal(await color(quiet.locator('.mnc-status span'), 'color'), muted);
      assert.equal(await color(quiet, 'borderLeftColor'), muted);
      assert.notEqual(await color(quiet, 'borderLeftColor'), yellow);
      checks++;

      // 2. the same companion as an observer that reported 1 min ago: Active.
      const obs = card(page, 1);
      assert.match(await obs.locator('.mnc-status-text').innerText(), /^Active · observer reporting · /);
      assert.equal(await obs.locator('.mnc-status span').getAttribute('class'), 'status-ok');
      assert.equal(await color(obs, 'borderLeftColor'), green);
      // The evidence row says "observer reporting"; tap the disclosure open.
      const summary = obs.locator('.mnc-evidence summary');
      if (touch) await summary.tap(); else await summary.click();
      await assert.doesNotReject(obs.locator('.mnc-evidence[open]').waitFor());
      const rows = await obs.locator('.mnc-evidence').innerText();
      assert.match(rows, /Observer reporting\s+\S/);
      assert.doesNotMatch(rows, /not counted/);
      assert.doesNotMatch(await obs.locator('.mnc-evidence-note').innerText(), /follows this node.s own advert/);
      assert.match(await obs.locator('.mnc-evidence-note').innerText(), /not a radio transmission/);
      checks++;

      // 3. an observer that reported 2 h ago is not credited.
      const stale = card(page, 2);
      assert.match(await stale.locator('.mnc-status-text').innerText(), /^Quiet · last heard /);
      assert.match(await stale.locator('.mnc-evidence').textContent(), /Observer reporting[\s\S]*not counted/);
      checks++;

      // 4/5. repeater and room are unchanged: Degraded in the warning colour.
      for (const i of [3, 4]) {
        const c = card(page, i);
        assert.match(await c.locator('.mnc-status-text').innerText(), /^Degraded · /, CASES[i].role);
        assert.equal(await c.locator('.mnc-status span').getAttribute('class'), 'status-warn');
        assert.equal(await color(c, 'borderLeftColor'), yellow);
      }
      checks++;

      // Sensor reads Quiet too.
      assert.match(await card(page, 5).locator('.mnc-status-text').innerText(), /^Quiet · /);
      checks++;

      // No per-node observer request: one bulk /observers call for the grid.
      assert.equal(await page.evaluate(() => window.apiCalls.filter(r => r === '/observers').length), 1);
      checks++;

      // Real tap on the observer card opens the health panel; its banner
      // credits the observer too (the same evidence model).
      const name = obs.locator('.mnc-name');
      if (touch) await name.tap(); else await name.click();
      const banner = page.locator('#homeHealth .health-banner');
      await banner.waitFor({ timeout: 5000 });
      assert.match(await banner.innerText(), /observer reporting/);
      assert.match(await banner.getAttribute('class'), /\bgreen\b/);
      checks++;

      assert.deepEqual(errors, []);
      await context.close();
    }
  } finally {
    await browser.close();
  }
  console.log(`PASS #902 My Mesh observer credit and Quiet companions (${checks} checks)`);
})().catch(e => { console.error(e); process.exit(1); });
