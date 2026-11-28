/**
 * PR #212 review follow-up — the packet-detail "Hash Size" summary must
 * describe the SAME frame as the raw-byte breakdown below it.
 *
 * Observations of one transmission carry their own wire bytes (routes.go
 * fills observations[].raw_hex per observation, upstream #1999), so a frame
 * whose header encodes a sender width can sit next to one that encodes none.
 * renderDetail reads the byte table from the selected observation; before the
 * fix the summary still read the original transmission's raw_hex, so a
 * selected direct zero-hop observation showed "Hash Size: 2 bytes" above a
 * byte table that said "no encoded hash size".
 *
 * The detail response is stubbed (context.route, so reloads and the
 * observation-row click path both see it) with two observations that differ
 * only in their frame header:
 *   obs 1 — 1540… flood, path byte 0x40 → 2-byte width, zero relay hops
 *   obs 2 — 1600… direct, path byte 0x00 → sendZeroHop marker, no width
 * Nothing outside the local test server is contacted.
 *
 * Usage: BASE_URL=http://localhost:13581 node test-packet-detail-sender-hash-size-obs-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const HASH = 'e2e212aabbccdd';
const PAYLOAD = 'aabbccddeeff00112233445566778899';
const FLOOD_OBS = 912120001;  // header 0x15 (flood), path byte 0x40
const DIRECT_OBS = 912120002; // header 0x16 (direct), path byte 0x00

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

const DECODED = JSON.stringify({
  type: 'CHAN', channel: '#e2e-212', sender: 'HashSizeNode', text: 'sender hash-size regression',
});

function observation(id, rawHex, routeType, observerName, snr) {
  return {
    id: id,
    raw_hex: rawHex,
    timestamp: '2026-05-20T10:00:00Z',
    observer_id: 'e2e-212-' + id,
    observer_name: observerName,
    direction: 'rx',
    snr: snr,
    rssi: -101,
    score: null,
    hash: HASH,
    route_type: routeType,
    payload_type: 5,
    payload_version: 0,
    path_json: '[]',
    decoded_json: DECODED,
    created_at: '2026-05-20T10:00:00Z',
  };
}

async function stubDetail(context) {
  await context.route((url) => url.pathname === '/api/packets/' + HASH, async (route) => {
    const body = {
      packet: {
        id: 912120,
        hash: HASH,
        // The ORIGINAL transmission: a zero-hop flood that does encode a width.
        raw_hex: '1540' + PAYLOAD,
        route_type: 1,
        payload_type: 5,
        payload_version: 0,
        first_seen: '2026-05-20T10:00:00Z',
        timestamp: '2026-05-20T10:00:00Z',
        observer_id: 'e2e-212-' + FLOOD_OBS,
        snr: -5,
        rssi: -101,
        path_json: '[]',
        decoded_json: DECODED,
        scope_name: null,
      },
      path: [],
      observation_count: 2,
      observations: [
        observation(FLOOD_OBS, '1540' + PAYLOAD, 1, 'e2e-212-flood', -5),
        observation(DIRECT_OBS, '1600' + PAYLOAD, 2, 'e2e-212-direct', -7),
      ],
    };
    try {
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(body) });
    } catch (_) {
      try { await route.abort(); } catch (__) { /* context already closed */ }
    }
  });
}

// Summary width (null when the panel emits no Hash Size row), the byte
// table's Path Length description, and which observation is selected.
function readPanel(page) {
  return page.evaluate(() => {
    const dts = Array.from(document.querySelectorAll('dl.detail-meta dt'));
    const dt = dts.find((d) => d.textContent.trim() === 'Hash Size');
    const dd = dt ? dt.nextElementSibling : null;
    const rows = Array.from(document.querySelectorAll('table.field-table tr'));
    const row = rows.find((r) => r.children[1] && r.children[1].textContent.trim() === 'Path Length');
    const current = document.querySelector('.detail-obs-row.observation-current');
    return {
      summary: dd ? dd.textContent.trim() : null,
      pathLen: row ? row.children[3].textContent.trim() : null,
      obs: current ? current.dataset.obsId : null,
    };
  });
}

// The contract: the two readings describe one frame, so they must agree.
function assertAgrees(state, where) {
  assert(state.pathLen, where + ': byte table has no Path Length row');
  const encoded = state.pathLen.match(/hash_size=(\d+) byte/);
  if (encoded) {
    const want = encoded[1] + ' byte' + (encoded[1] === '1' ? '' : 's');
    assert(state.summary === want,
      where + ': byte table says ' + state.pathLen + ' but summary says ' + JSON.stringify(state.summary));
  } else {
    assert(/no encoded hash size/.test(state.pathLen),
      where + ': unexpected Path Length description ' + state.pathLen);
    assert(state.summary === null,
      where + ': byte table encodes no width but summary says ' + JSON.stringify(state.summary));
  }
}

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== packet detail: sender hash size follows the selected observation (${BASE}) ===`);

  const context = await browser.newContext({ viewport: { width: 1400, height: 900 } });
  await stubDetail(context);
  const page = await context.newPage();
  page.setDefaultTimeout(15000);
  page.on('pageerror', (e) => console.error('[pageerror]', e.message));

  async function open(obsId) {
    const suffix = obsId ? '?obs=' + obsId : '';
    await page.goto(`${BASE}/#/packet/${HASH}${suffix}`, { waitUntil: 'load' });
    await page.reload({ waitUntil: 'load' });
    await page.waitForSelector('table.field-table', { timeout: 15000 });
    await page.waitForSelector('.detail-obs-row.observation-current', { timeout: 15000 });
  }

  await step('zero-hop flood observation: summary and byte table both report the 2-byte width', async () => {
    await open(FLOOD_OBS);
    const state = await readPanel(page);
    assert(state.obs === String(FLOOD_OBS), 'wrong observation selected: ' + state.obs);
    assert(state.summary === '2 bytes', 'flood summary width is ' + JSON.stringify(state.summary));
    assertAgrees(state, 'flood observation');
  });

  await step('direct zero-hop observation: neither summary nor byte table claims a width', async () => {
    await open(DIRECT_OBS);
    const state = await readPanel(page);
    assert(state.obs === String(DIRECT_OBS), 'wrong observation selected: ' + state.obs);
    assert(/no encoded hash size/.test(state.pathLen), 'byte table lost the zero-hop marker');
    assert(state.summary === null, 'direct zero-hop still shows Hash Size: ' + state.summary);
    assertAgrees(state, 'direct observation');
  });

  await step('switching observations in place keeps the two readings in agreement', async () => {
    await open(FLOOD_OBS);
    assertAgrees(await readPanel(page), 'before switch');
    await page.click(`.detail-obs-row[data-obs-id="${DIRECT_OBS}"]`);
    await page.waitForFunction((id) => {
      const r = document.querySelector('.detail-obs-row.observation-current');
      return !!r && r.dataset.obsId === String(id);
    }, DIRECT_OBS);
    const afterDirect = await readPanel(page);
    assertAgrees(afterDirect, 'after switching to the direct observation');
    assert(afterDirect.summary === null, 'summary kept the original flood width: ' + afterDirect.summary);
    await page.click(`.detail-obs-row[data-obs-id="${FLOOD_OBS}"]`);
    await page.waitForFunction((id) => {
      const r = document.querySelector('.detail-obs-row.observation-current');
      return !!r && r.dataset.obsId === String(id);
    }, FLOOD_OBS);
    const backToFlood = await readPanel(page);
    assertAgrees(backToFlood, 'after switching back to the flood observation');
    assert(backToFlood.summary === '2 bytes', 'flood width did not come back: ' + backToFlood.summary);
  });

  await context.close();
  await browser.close();
  console.log(`\n=== ${passed} passed, ${failed} failed ===`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
