/**
 * E2E (#673) — the map's packet route honours "Hide short (1-byte) path-hash
 * hops" for inferred hops, for one observer and for several.
 *
 * Fixture packet 57224a4a01c9c682, observation 147: a 22-hop flood whose every
 * hop is a 1-byte prefix (the #598 fixture). The #/map deep link infers those
 * hops and, before #673, tagged each route position with the inferred pubkey,
 * so the 1-byte filter never matched: with the toggle on, the relays were
 * still listed and drawn. A packet heard by more than one observer goes
 * through drawPacketRouteMulti, which did not filter at all; the second
 * observer here is synthetic (the fixture has one), added by intercepting the
 * packet's own API response.
 *
 * Every check flips the toggle live (the same MC_setHide1ByteHops the
 * customizer calls) and waits for an exact row count, so a route that has not
 * drawn yet cannot pass as "everything hidden". Packet 73658eb54a439ea7
 * (observation 343, 14 two-byte hops) is the control: the toggle must leave
 * it alone.
 *
 * Usage: BASE_URL=http://localhost:13581 node test-issue-673-map-hide-inferred-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const ONE_BYTE = { packet: '57224a4a01c9c682', obs: '147', relays: 22 };
const TWO_BYTE = { packet: '73658eb54a439ea7', obs: '343' };

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

  // Relay rows: route rows that are not payload endpoints.
  const RELAYS = () => [...document.querySelectorAll('.mc-rt-sidebar .mc-rt-row')]
    .filter(r => !/from payload/.test(r.textContent)).length;

  async function openRoute(target, { hide, multi }) {
    const ctx = await browser.newContext({ viewport: { width: 1280, height: 900 } });
    await ctx.addInitScript((on) => {
      try {
        if (on) localStorage.setItem('meshcore-hide-1byte-hops', 'true');
        else localStorage.removeItem('meshcore-hide-1byte-hops');
      } catch (_e) { /* storage unavailable */ }
    }, !!hide);
    if (multi) {
      await ctx.route(url => url.pathname === '/api/packets/' + target.packet, async route => {
        const res = await route.fetch();
        const body = await res.json();
        const first = body.observations.find(o => String(o.id) === target.obs) || body.observations[0];
        const second = Object.assign({}, first, {
          id: 9990000 + Number(target.obs), observer_id: 'E2E673SECONDOBSERVER', observer_name: 'E2E second observer',
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
    await page.goto(`${BASE}/#/map?packet=${target.packet}&obs=${target.obs}`, { waitUntil: 'load' });
    return { ctx, page, errors };
  }

  const waitRelays = (page, n) => page.waitForFunction(
    ({ n, fn }) => new Function('return (' + fn + ')()')() === n, { n, fn: RELAYS.toString() });
  const relayCount = (page) => page.evaluate((fn) => new Function('return (' + fn + ')()')(), RELAYS.toString());
  const setHide = (page, on) => page.evaluate((v) => window.MC_setHide1ByteHops(v), on);

  // Toggle off → every relay; live toggle on → none, and no stale sidebar; off → back.
  // With every relay hidden the sidebar is either gone (no endpoint survives)
  // or redrawn with only payload endpoints (the CI seeds resolve this
  // packet's recipient) — either way the 22-relay sidebar must not stay up.
  async function flipCycle(multi) {
    const { ctx, page, errors } = await openRoute(ONE_BYTE, { hide: false, multi });
    try {
      await waitRelays(page, ONE_BYTE.relays);
      assert(await page.evaluate(() => window.__mc_lastRouteDraw.kind) === (multi ? 'multi' : 'single'),
        'expected the ' + (multi ? 'multi' : 'single') + '-observer renderer');
      await page.evaluate(() => document.querySelector('.mc-rt-sidebar').setAttribute('data-e2e-old', '1'));
      await setHide(page, true);
      await page.waitForFunction(() => {
        const s = document.querySelector('.mc-rt-sidebar');
        return !s || !s.hasAttribute('data-e2e-old');
      });
      assert(await relayCount(page) === 0, 'relays still listed with 1-byte hops hidden');
      await setHide(page, false);
      await waitRelays(page, ONE_BYTE.relays);
      assert(errors.length === 0, 'page errors: ' + errors.join(' | '));
    } finally { await ctx.close(); }
  }

  console.log(`\n#673 E2E against ${BASE}\n`);

  await step('one observer: all 22 1-byte relays, then none with the toggle on (sidebar cleared), then all again', () => flipCycle(false));

  await step('two observers (drawPacketRouteMulti): the same, live', () => flipCycle(true));

  await step('control: a 2-byte route keeps every relay with the toggle on, on load and after a live flip', async () => {
    const { ctx, page, errors } = await openRoute(TWO_BYTE, { hide: true, multi: false });
    try {
      await page.waitForFunction((fn) => new Function('return (' + fn + ')()')() > 0, RELAYS.toString());
      const n = await relayCount(page);
      // Mark the current sidebar, flip off, wait for a fresh one, then on again.
      for (const on of [false, true]) {
        await page.evaluate(() => document.querySelector('.mc-rt-sidebar').setAttribute('data-e2e-old', '1'));
        await setHide(page, on);
        await page.waitForFunction(() => {
          const s = document.querySelector('.mc-rt-sidebar');
          return s && !s.hasAttribute('data-e2e-old');
        });
        assert(await relayCount(page) === n, `toggle ${on ? 'on' : 'off'}: expected ${n} relays, got ${await relayCount(page)}`);
      }
      assert(errors.length === 0, 'page errors: ' + errors.join(' | '));
    } finally { await ctx.close(); }
  });

  await browser.close();
  console.log(`\n#673 E2E: ${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})().catch(e => { console.error('  ✗ #673 E2E:', e.stack || e); process.exit(1); });
