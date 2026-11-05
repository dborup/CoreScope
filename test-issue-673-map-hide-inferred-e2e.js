/**
 * E2E (#673) — the map's packet route honours "Hide short (1-byte) path-hash
 * hops" for inferred hops, for one observer and for several.
 *
 * Fixture packet 57224a4a01c9c682, observation 147: a 22-hop flood whose every
 * hop is a 1-byte prefix (the #598 fixture). The #/map deep link infers those
 * hops and, before #673, tagged each route position with the inferred pubkey,
 * so the 1-byte filter never matched: with the toggle on, every one of the 22
 * relays was still listed and drawn. A packet heard by more than one observer
 * goes through drawPacketRouteMulti, which did not filter at all; the second
 * observer here is synthetic (the fixture has one), added by intercepting the
 * packet's own API response.
 *
 * Usage: BASE_URL=http://localhost:13581 node test-issue-673-map-hide-inferred-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const PACKET = '57224a4a01c9c682';
const OBS = '147';
const RELAYS = 22;

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });

  // Opens the route for the fixture observation and returns its relay rows
  // (rows that are not payload endpoints). multi: add a second observer so
  // the map takes drawPacketRouteMulti.
  async function routeRelays({ hide, multi }) {
    const ctx = await browser.newContext({ viewport: { width: 1280, height: 900 } });
    await ctx.addInitScript((on) => {
      try {
        if (on) localStorage.setItem('meshcore-hide-1byte-hops', 'true');
        else localStorage.removeItem('meshcore-hide-1byte-hops');
      } catch (_e) { /* storage unavailable */ }
    }, !!hide);
    if (multi) {
      await ctx.route(url => url.pathname === '/api/packets/' + PACKET, async route => {
        const res = await route.fetch();
        const body = await res.json();
        const first = body.observations[0];
        const second = Object.assign({}, first, {
          id: 9990147, observer_id: 'E2E673SECONDOBSERVER', observer_name: 'E2E second observer',
          path_json: JSON.stringify(JSON.parse(first.path_json).slice(0, 10)), resolved_path: null,
        });
        body.observations = [first, second];
        body.observation_count = 2;
        await route.fulfill({ response: res, json: body });
      });
    }
    const page = await ctx.newPage();
    page.setDefaultTimeout(30000);
    const errors = [];
    page.on('pageerror', e => errors.push(String(e.message)));
    try {
      await page.goto(`${BASE}/#/map?packet=${PACKET}&obs=${OBS}`, { waitUntil: 'load' });
      // With every relay hidden and no payload endpoint, nothing is left to
      // list, so wait for the draw call itself rather than for a row.
      await page.waitForFunction(() => !!window.__mc_lastRouteDraw, null, { timeout: 30000 });
      // The route redraws once the neighbour graph lands; read the settled state.
      await page.waitForTimeout(3000);
      const rows = await page.evaluate(() => [...document.querySelectorAll('.mc-rt-row')].map(r => ({
        name: (r.querySelector('.mc-rt-name') || r).textContent.replace(/\s+/g, ' ').trim(),
        fromPayload: /from payload/.test(r.textContent),
      })));
      const multiObservers = await page.evaluate(() => window.__mc_lastRouteDraw.kind === 'multi');
      return { relays: rows.filter(r => !r.fromPayload), rows, errors, multiObservers };
    } finally {
      await ctx.close();
    }
  }

  console.log(`\n#673 E2E against ${BASE} — packet ${PACKET}, observation ${OBS}\n`);

  await step('one observer, toggle off: all 22 inferred 1-byte relays are listed (baseline)', async () => {
    const r = await routeRelays({ hide: false, multi: false });
    assert(r.relays.length === RELAYS, `expected ${RELAYS} relays, got ${r.relays.length}`);
    assert(r.errors.length === 0, 'page errors: ' + r.errors.join(' | '));
  });

  await step('one observer, toggle on: no 1-byte relay is listed', async () => {
    const r = await routeRelays({ hide: true, multi: false });
    assert(r.relays.length === 0,
      `expected 0 relays with 1-byte hops hidden, got ${r.relays.length}: ` + JSON.stringify(r.relays.map(x => x.name)));
    assert(r.errors.length === 0, 'page errors: ' + r.errors.join(' | '));
  });

  await step('two observers, toggle off: the multi-path route lists all 22 relays (baseline)', async () => {
    const r = await routeRelays({ hide: false, multi: true });
    assert(r.multiObservers, 'the injected second observer did not reach the route view');
    assert(r.relays.length === RELAYS, `expected ${RELAYS} relays, got ${r.relays.length}`);
    assert(r.errors.length === 0, 'page errors: ' + r.errors.join(' | '));
  });

  await step('two observers, toggle on: the multi-path route hides every 1-byte relay too', async () => {
    const r = await routeRelays({ hide: true, multi: true });
    assert(r.multiObservers, 'the injected second observer did not reach the route view');
    assert(r.relays.length === 0,
      `expected 0 relays with 1-byte hops hidden, got ${r.relays.length}: ` + JSON.stringify(r.relays.map(x => x.name)));
    assert(r.errors.length === 0, 'page errors: ' + r.errors.join(' | '));
  });

  await browser.close();
  console.log(`\n#673 E2E: ${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})().catch(e => { console.error('  ✗ #673 E2E:', e.stack || e); process.exit(1); });
