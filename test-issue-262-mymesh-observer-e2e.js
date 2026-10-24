#!/usr/bin/env node
/* E2E for issue #262 — My Mesh shows observer identity, status and traffic
 * separately from the mesh role.
 *
 * Renders the real home.js My Mesh grid against stubbed /api responses on
 * desktop (1280) and mobile (390), with synthetic fixture identities only.
 * Covers: a node that is ALSO an observer, a node without an observer
 * record, two confusably-named devices that must stay separate, an observer
 * whose traffic metric is missing (em dash, never a zero), and a failed
 * observer-list request (explicit notice, no false non-observer cards).
 */
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require('playwright');

const ROOT = path.join(__dirname, 'public');
const ORIGIN = 'http://127.0.0.1:13801';

// Synthetic keys. KEY_OBS is registered as an observer with an UPPERCASE id
// (observers rows are uppercase, nodes rows lowercase — #1836), which is the
// case-normalisation the match has to survive.
const KEY_OBS = 'b'.repeat(64);
const KEY_PLAIN = 'a'.repeat(64);
const KEY_ROOM = 'c'.repeat(64);
const KEY_DOKK = 'd'.repeat(64);
const KEYS = [KEY_OBS, KEY_PLAIN, KEY_ROOM, KEY_DOKK];
const HOSTILE = '"><img src=x onerror=alert(1)>';

const now = Date.now();
const iso = msAgo => new Date(now - msAgo).toISOString();
const activity = () => ({
  windowStart: iso(24 * 3600000), windowEnd: iso(0), coverageStart: iso(24 * 3600000), complete: true,
  buckets: Array.from({ length: 24 }, (_, i) => ({ start: iso((24 - i) * 3600000), end: iso((23 - i) * 3600000), count: i })),
});

// Node health, one entry per card. Card 3 is "Dokk1 Lab": NOT an observer,
// even though the observer list holds a device called "Dokk1 Lab Meshview".
const health = {
  [KEY_OBS]: { node: { name: 'Hub North', role: 'repeater' }, stats: { lastHeard: iso(60000), packetsToday: 77, avgSnr: 6, avgHops: 1.5 } },
  [KEY_PLAIN]: { node: { name: 'Plain Relay', role: 'repeater' }, stats: { lastHeard: iso(60000), packetsToday: 12, avgSnr: 2, avgHops: 2 } },
  [KEY_ROOM]: { node: { name: 'Chat Room', role: 'room' }, stats: { lastHeard: iso(60000), packetsToday: 4, avgSnr: 1, avgHops: 3 } },
  [KEY_DOKK]: { node: { name: 'Dokk1 Lab', role: 'repeater' }, stats: { lastHeard: iso(60000), packetsToday: 9, avgSnr: 0, avgHops: 2 } },
};

const observersPayload = {
  server_time: new Date(now).toISOString(),
  observers: [
    // Card 0: same device as KEY_OBS, id cased differently. Reporting now.
    { id: KEY_OBS.toUpperCase(), name: 'Hub North Uplink' + HOSTILE, last_seen: iso(5 * 60000), last_packet_at: iso(2 * 60000), packetsLastHour: 1234, packet_count: 98765 },
    // Card 2: a room server that is also an observer, offline for 10 days,
    // with NO packetsLastHour field at all.
    { id: KEY_ROOM.toUpperCase(), name: 'Chat Room Uplink', last_seen: iso(10 * 86400000), last_packet_at: null },
    // A confusable NAME, a different key: must never land on the Dokk1 card.
    { id: 'e'.repeat(64), name: 'Dokk1 Lab Meshview', last_seen: iso(60000), last_packet_at: iso(60000), packetsLastHour: 50 },
    // A 12-hex PREFIX of KEY_PLAIN: must never match the plain card.
    { id: KEY_PLAIN.slice(0, 12).toUpperCase(), name: 'Prefix Trap', last_seen: iso(60000), packetsLastHour: 999 },
  ],
};

async function render(page, { observersFail = false } = {}) {
  await page.route('**/*', route => {
    const u = new URL(route.request().url());
    if (u.origin !== ORIGIN) return route.abort();
    if (u.pathname === '/icons/phosphor-sprite.svg') return route.fulfill({ contentType: 'image/svg+xml', body: fs.readFileSync(path.join(ROOT, 'icons/phosphor-sprite.svg')) });
    return route.fulfill({ contentType: 'text/html', body: '<meta name="viewport" content="width=device-width,initial-scale=1"><main id="fixture"></main>' });
  });
  await page.goto(ORIGIN + '/');
  await page.addStyleTag({ path: path.join(ROOT, 'style.css') });
  await page.addStyleTag({ path: path.join(ROOT, 'home.css') });
  // observers.js is loaded for real so the card's status uses the observers
  // page's own classifier (window.observerHealthStatus, #1552/#1563) rather
  // than a threshold invented for the home page.
  for (const file of ['app.js', 'roles.js', 'heard-by.js', 'my-mesh-observers.js', 'observers.js']) {
    await page.addScriptTag({ path: path.join(ROOT, file) });
  }
  await page.evaluate((data) => {
    window.removeEventListener('hashchange', navigate);
    window.fixturePages = {};
    registerPage = (name, mod) => { window.fixturePages[name] = mod; };
    window.apiCalls = [];
    api = async (route) => {
      window.apiCalls.push(route);
      if (route === '/stats') return { totalNodes: 4 };
      if (route === '/observers') {
        if (data.observersFail) throw Error('500 observers unavailable');
        return data.observers;
      }
      const m = route.match(/^\/nodes\/([0-9a-f]{64})\/health/);
      if (m && data.health[m[1]]) {
        const h = data.health[m[1]];
        return { node: h.node, stats: h.stats, directObservers: [{ observer_name: 'Rooftop' }], relayObservers: [], recentPackets: [], activity24h: data.activity };
      }
      throw Error('Unexpected API: ' + route);
    };
    localStorage.setItem('meshcore-user-level', 'experienced');
    localStorage.setItem('meshcore-my-nodes', JSON.stringify(data.keys.map(pubkey => ({ pubkey, name: 'Node' }))));
  }, { observers: observersPayload, health, keys: KEYS, activity: activity(), observersFail });
  await page.addScriptTag({ path: path.join(ROOT, 'home.js') });
  await page.evaluate(() => window.fixturePages.home.init(document.getElementById('fixture')));
  await page.waitForFunction(n => document.querySelectorAll('.my-node-card').length === n, KEYS.length);
}

(async () => {
  const browser = await chromium.launch({ headless: true, args: ['--no-sandbox'] });
  try {
    for (const [width, height] of [[1280, 900], [390, 844]]) {
      const context = await browser.newContext({ viewport: { width, height }, hasTouch: width < 500, isMobile: width < 500 });
      const page = await context.newPage();
      const errors = [];
      page.on('pageerror', e => errors.push(e.message));
      await render(page);
      if (process.env.SCREENSHOT_PATH && width === 1280) await page.screenshot({ path: process.env.SCREENSHOT_PATH, fullPage: true });

      const cards = page.locator('.my-node-card');
      const [obsCard, plainCard, roomCard, dokkCard] = [0, 1, 2, 3].map(i => cards.nth(i));

      // ── one bulk observer fetch for the whole grid, never per card ──
      const calls = await page.evaluate(() => window.apiCalls);
      assert.equal(calls.filter(c => c === '/observers').length, 1, `exactly one /observers call, got ${JSON.stringify(calls)}`);
      assert.equal(calls.filter(c => /\/observers\//.test(c)).length, 0, 'no per-observer requests');

      // ── OBSERVER is prominent; the mesh role is NOT overwritten ──
      // Rendered uppercase by CSS text-transform; the DOM text stays "Observer".
      assert.equal(await obsCard.locator('.mnc-badge-observer').innerText(), 'OBSERVER');
      assert.equal(await obsCard.locator('.mnc-badge-observer').textContent(), 'Observer');
      assert.equal(await obsCard.locator('.mnc-role').innerText(), 'REPEATER');
      assert.equal(await obsCard.locator('.mnc-badge-observer').getAttribute('title'), 'Observer · Repeater');
      assert.equal(await roomCard.locator('.mnc-badge-observer').getAttribute('title'), 'Observer · Room');
      assert.equal(await roomCard.locator('.mnc-role').innerText(), 'ROOM');
      // The observer badge must be visually ahead of the mesh-role chip.
      const order = await obsCard.evaluate(el => [...el.querySelector('.mnc-header').children].map(c => c.className.split(' ')[0]));
      assert.ok(order.indexOf('mnc-badge-observer') < order.indexOf('mnc-role'), `badge order: ${order}`);
      const badgeBox = await obsCard.locator('.mnc-badge-observer').boundingBox();
      assert.ok(badgeBox && badgeBox.width > 0 && badgeBox.height > 0, `badge must be visible at ${width}px`);
      // The badge is filled with the themed observer color, not the muted
      // surface the secondary role chip uses.
      const [badgeBg, roleBg] = await obsCard.evaluate(el => [
        getComputedStyle(el.querySelector('.mnc-badge-observer')).backgroundColor,
        getComputedStyle(el.querySelector('.mnc-role')).backgroundColor,
      ]);
      assert.notEqual(badgeBg, roleBg, 'observer badge must not look like the secondary role chip');
      assert.equal(badgeBg, await page.evaluate(() => {
        const p = document.createElement('div');
        p.style.backgroundColor = getComputedStyle(document.documentElement).getPropertyValue('--role-observer').trim();
        document.body.appendChild(p);
        const v = getComputedStyle(p).backgroundColor; p.remove(); return v;
      }), 'badge must come from --role-observer');

      // ── a node without an observer record keeps its old card ──
      for (const card of [plainCard, dokkCard]) {
        assert.equal(await card.locator('.mnc-badge-observer').count(), 0);
        assert.equal(await card.locator('.mnc-observer-panel').count(), 0);
        assert.equal(await card.locator('[data-action="observer"]').count(), 0);
        assert.equal(await card.locator('.mnc-role').innerText(), 'REPEATER');
      }
      // Confusable names stay separate devices, and a 12-hex prefix of the
      // plain node's key never promotes it to an observer.
      assert.equal(await dokkCard.locator('.mnc-name').innerText(), 'Dokk1 Lab');
      assert.equal(await page.locator('.my-node-card.is-observer').count(), 2);

      // ── observer status comes from the observer's own report ──
      const statusRow = obsCard.locator('.mnc-observer-row').first();
      assert.equal(await obsCard.locator('.mnc-observer-state').innerText(), 'Online');
      assert.equal(await statusRow.locator('.status-ok').count(), 1);
      assert.match((await statusRow.innerText()).replace(/\s+/g, ' '), /^Status Online · reported 5m ago$/);
      assert.match((await obsCard.locator('.mnc-observer-row').nth(1).innerText()).replace(/\s+/g, ' '), /^Last packet reported 2m ago$/);
      // The node's own status line is still there, still about the node.
      assert.match(await obsCard.locator('.mnc-status-text').innerText(), /^Active · 1m ago$/);

      // A 10-day-old observer report is Offline, and that verdict is the
      // observer's — the node itself was heard a minute ago.
      assert.equal(await roomCard.locator('.mnc-observer-state').innerText(), 'Offline');
      assert.equal(await roomCard.locator('.mnc-observer-row').first().locator('.status-err').count(), 1);
      assert.match(await roomCard.locator('.mnc-status-text').innerText(), /^Active · 1m ago$/);
      // No observer packet ever reported → em dash, not "never heard".
      assert.equal(await roomCard.locator('.mnc-observer-last').innerText(), '—');

      // ── observer traffic states its population AND its window ──
      assert.equal(await obsCard.locator('.mnc-observer-metric .mnc-val').innerText(), '1,234');
      assert.equal(await obsCard.locator('.mnc-observer-metric .mnc-lbl').innerText(), 'Packets reported · last hour');
      // A missing metric is never shown as a measured zero.
      assert.equal(await roomCard.locator('.mnc-observer-metric .mnc-val').innerText(), '—');

      // ── node activity stays separate and labelled as node activity ──
      assert.deepEqual(await obsCard.locator('.mnc-section-lbl').evaluateAll(els => els.map(e => e.textContent)),
        ['Observer reporting', 'Node activity']);
      assert.deepEqual(await obsCard.locator('.mnc-section-lbl').allInnerTexts(), ['OBSERVER REPORTING', 'NODE ACTIVITY']);
      assert.equal(await obsCard.locator('.mnc-metrics .mnc-lbl').first().innerText(), 'Packets today');
      assert.equal(await obsCard.locator('.mnc-metrics .mnc-val').first().innerText(), '77');
      assert.match(await obsCard.locator('.mnc-spark').innerText(), /Node-associated transmissions.*last 24h/);
      assert.equal(await plainCard.locator('.mnc-section-lbl').count(), 0, 'a non-observer card gains no section labels');
      // The observer metric must not sit inside the node metric grid.
      assert.equal(await obsCard.locator('.mnc-metrics .mnc-observer-metric').count(), 0);

      // ── no injected markup from a hostile observer name ──
      assert.equal(await page.locator('img, [onerror]').count(), 0);

      // ── mobile: the panel stacks inside the card, nothing overflows ──
      const geom = await obsCard.evaluate(el => ({
        card: el.getBoundingClientRect(), panel: el.querySelector('.mnc-observer-panel').getBoundingClientRect(),
        docScroll: document.documentElement.scrollWidth, docClient: document.documentElement.clientWidth,
      }));
      assert.ok(geom.panel.right <= geom.card.right + 1, `observer panel overflows the card at ${width}px`);
      assert.ok(geom.docScroll <= geom.docClient + 1, `page scrolls horizontally at ${width}px`);
      // #304 keeps these cards compact. Card 0 and card 1 are identical
      // repeaters apart from the observer chrome, so the difference IS the
      // cost of #262 — a far tighter bound than an absolute pixel cap.
      const [obsH, plainH] = await page.locator('.my-node-card').evaluateAll(els => [els[0].getBoundingClientRect().height, els[1].getBoundingClientRect().height]);
      console.log(`  ${width}px: observer card ${obsH}px vs plain card ${plainH}px (+${(obsH - plainH).toFixed(1)})`);
      // Measured composition of that delta at 390px: the panel (~101px + 10px
      // margin), the "Node activity" heading (~14px) and one extra button in
      // the actions row (~28px). The cap leaves room for rendering variance
      // but not for a third status row or a second panel.
      assert.ok(obsH - plainH <= 170, `the observer chrome adds ${obsH - plainH}px at ${width}px`);
      assert.equal(await obsCard.locator('.mnc-observer-row').count(), 2, 'the panel stays at two status rows');
      const panelH = await obsCard.locator('.mnc-observer-panel').evaluate(el => el.getBoundingClientRect().height);
      assert.ok(panelH <= 115, `observer panel too tall at ${width}px: ${panelH}`);
      // Desktop puts the status rows and the traffic metric side by side.
      assert.equal(await obsCard.locator('.mnc-observer-panel').evaluate(el => getComputedStyle(el).gridTemplateColumns.split(' ').length), width < 500 ? 1 : 2);

      // ── the Observer page action selects THIS observer, exact id case ──
      const obsBtn = obsCard.locator('[data-action="observer"]');
      assert.equal(await obsBtn.innerText(), 'Observer page →');
      assert.equal(await obsCard.locator('[data-action="node"]').count(), 1, 'node navigation must stay available');
      await obsBtn.scrollIntoViewIfNeeded();
      if (width < 500) await obsBtn.tap(); else await obsBtn.click();
      assert.equal(await page.evaluate(() => location.hash), '#/observers/' + KEY_OBS.toUpperCase());
      await page.evaluate(() => { location.hash = ''; });
      // The room observer's button opens the room observer, not the first one.
      const roomBtn = roomCard.locator('[data-action="observer"]');
      await roomBtn.scrollIntoViewIfNeeded();
      if (width < 500) await roomBtn.tap(); else await roomBtn.click();
      assert.equal(await page.evaluate(() => location.hash), '#/observers/' + KEY_ROOM.toUpperCase());
      await page.evaluate(() => { location.hash = ''; });

      // ── a healthy grid carries no unavailable notice ──
      assert.equal(await page.locator('.my-nodes-observer-notice').count(), 0);

      // ── a failed observer list is stated, not silently dropped ──
      await render(page, { observersFail: true });
      const notice = page.locator('.my-nodes-observer-notice');
      assert.equal(await notice.count(), 1);
      assert.match(await notice.innerText(), /observer list could not be loaded/i);
      assert.equal(await notice.getAttribute('role'), 'status');
      assert.ok((await notice.boundingBox()).width > 0, 'the notice must be visible');
      assert.equal(await page.locator('.mnc-badge-observer').count(), 0, 'no badge may be claimed without the list');
      assert.equal(await page.locator('.mnc-observer-panel').count(), 0);
      assert.equal(await page.locator('[data-action="observer"]').count(), 0);
      // Every card still renders, with its node data intact.
      assert.equal(await page.locator('.my-node-card').count(), KEYS.length);
      assert.equal(await cards.nth(0).locator('.mnc-metrics .mnc-val').first().innerText(), '77');
      assert.equal(await page.locator('.my-node-card.is-observer').count(), 0);

      assert.deepEqual(errors, []);
      await context.close();
    }
    console.log('My Mesh #262: observer identity, status, traffic, navigation and unavailable state passed on desktop + mobile');
  } finally { await browser.close(); }
})().catch(e => { console.error(e); process.exitCode = 1; });
