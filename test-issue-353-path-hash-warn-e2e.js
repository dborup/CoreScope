/**
 * #353 — browser regression: a 1-byte path hash reads as a warning in the
 * channel "Sent with" badge, the packet-detail Hash Size row and the node
 * detail badges; 2/3-byte stay neutral. Checked in the light and dark theme
 * and at a 375×812 phone viewport (no horizontal page overflow).
 *
 * Channels use channels.js' own test hooks, packet detail a stubbed
 * /api/packets/<hash>, node detail real fixture nodes picked by hash_size.
 * Only the local test server is contacted.
 *
 * Usage: BASE_URL=http://localhost:13581 node test-issue-353-path-hash-warn-e2e.js
 * Optional: SCREENSHOT_DIR=<dir> saves one screenshot per page/theme/viewport.
 */
'use strict';
const path = require('path');
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const SHOTS = process.env.SCREENSHOT_DIR || '';
const HASH_1 = 'e2e353aa11223344';
const HASH_2 = 'e2e353bb11223344';
const PAYLOAD = 'aabbccddeeff00112233445566778899';
const DECODED = JSON.stringify({ type: 'CHAN', channel: '#e2e-353', sender: 'WarnNode', text: 'path hash warn' });

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

function packetBody(hash, rawHex) {
  const obs = {
    id: 935300001, raw_hex: rawHex, timestamp: '2026-10-07T10:00:00Z',
    observer_id: 'e2e-353-obs', observer_name: 'e2e-353-obs', direction: 'rx',
    snr: -5, rssi: -101, score: null, hash: hash, route_type: 1, payload_type: 5,
    payload_version: 0, path_json: '[]', decoded_json: DECODED, created_at: '2026-10-07T10:00:00Z',
  };
  return {
    packet: {
      id: 935300, hash: hash, raw_hex: rawHex, route_type: 1, payload_type: 5, payload_version: 0,
      first_seen: obs.timestamp, timestamp: obs.timestamp, observer_id: obs.observer_id,
      snr: -5, rssi: -101, path_json: '[]', decoded_json: DECODED, scope_name: null,
    },
    path: [], observation_count: 1, observations: [obs],
  };
}

async function stubPackets(context) {
  // Flood (header 0x15) with path byte 0x00 -> 1-byte; 0x40 -> 2-byte.
  const bodies = { [HASH_1]: packetBody(HASH_1, '1500' + PAYLOAD), [HASH_2]: packetBody(HASH_2, '1540' + PAYLOAD) };
  await context.route((url) => /^\/api\/packets\/e2e353/.test(url.pathname), async (route) => {
    const hash = new URL(route.request().url()).pathname.split('/').pop();
    try {
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(bodies[hash]) });
    } catch (_) { try { await route.abort(); } catch (__) { /* closed */ } }
  });
}

// Shared assertions for a rendered warning element: modifier class, icon
// hidden from AT, sr-only text, firmware-named tooltip, colour = --path-hash-warn.
async function readWarn(locator) {
  return locator.evaluate((el) => {
    const cs = getComputedStyle(el);
    const probe = document.createElement('span');
    probe.style.color = 'var(--path-hash-warn)';
    document.body.appendChild(probe);
    const want = getComputedStyle(probe).color;
    probe.style.color = 'var(--danger)';
    const danger = getComputedStyle(probe).color;
    probe.remove();
    const icon = el.querySelector('svg.ph-icon use[href$="#ph-warning"]');
    const r = el.getBoundingClientRect();
    return {
      cls: el.className, title: el.getAttribute('title') || '', color: cs.color, want, danger,
      iconHidden: !!icon && icon.closest('svg').getAttribute('aria-hidden') === 'true',
      iconVisible: !!icon && icon.closest('svg').getBoundingClientRect().width > 0,
      sr: (el.querySelector('.sr-only') || {}).textContent || '',
      right: r.right, vw: document.documentElement.clientWidth,
      overflow: document.documentElement.scrollWidth - document.documentElement.clientWidth,
    };
  });
}
function assertWarn(w, block, where) {
  assert(w.cls.split(/\s+/).includes(block + '--warn'), where + ': no ' + block + '--warn in ' + w.cls);
  assert(w.cls.split(/\s+/).includes('path-hash-warn'), where + ': no path-hash-warn');
  assert(w.iconHidden && w.iconVisible, where + ': warning icon missing, visible=' + w.iconVisible);
  assert(w.sr === 'Warning: ', where + ': sr-only text ' + JSON.stringify(w.sr));
  assert(/2- or 3-byte/.test(w.title) && /Experimental Settings/.test(w.title) && /path\.hash\.mode/.test(w.title),
    where + ': tooltip ' + JSON.stringify(w.title));
  assert(w.color === w.want && w.want === w.danger, where + ': colour ' + w.color + ' vs --path-hash-warn ' + w.want + ' / --danger ' + w.danger);
  assert(w.right <= w.vw + 0.5, where + ': warning runs off-screen (' + w.right + ' > ' + w.vw + ')');
  assert(w.overflow <= 0, where + ': page scrolls horizontally by ' + w.overflow + 'px');
}

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #353 1-byte path hash warning (${BASE}) ===`);

  // Real fixture nodes: one advertising 1-byte, one 2-byte.
  const probeCtx = await browser.newContext();
  const nodesResp = await probeCtx.request.get(BASE + '/api/nodes?limit=500');
  const nodes = ((await nodesResp.json()).nodes) || [];
  await probeCtx.close();
  const node1 = nodes.find((n) => Number(n.hash_size) === 1);
  const node2 = nodes.find((n) => Number(n.hash_size) === 2);

  const variants = [
    { name: 'light desktop', theme: 'light', viewport: { width: 1400, height: 900 } },
    { name: 'dark desktop', theme: 'dark', viewport: { width: 1400, height: 900 } },
    { name: 'light 375x812', theme: 'light', viewport: { width: 375, height: 812 } },
    { name: 'dark 375x812', theme: 'dark', viewport: { width: 375, height: 812 } },
  ];

  for (const v of variants) {
    const context = await browser.newContext({ viewport: v.viewport, colorScheme: v.theme });
    await context.addInitScript((theme) => { try { localStorage.setItem('meshcore-theme', theme); } catch (_) {} }, v.theme);
    await stubPackets(context);
    const page = await context.newPage();
    page.setDefaultTimeout(15000);
    page.on('pageerror', (e) => console.error('[pageerror]', e.message));
    const shot = async (label) => {
      if (SHOTS) await page.screenshot({ path: path.join(SHOTS, `353-${label}-${v.name.replace(/\W+/g, '-')}.png`) });
    };

    await step(`${v.name}: theme applied`, async () => {
      await page.goto(BASE + '/#/channels', { waitUntil: 'domcontentloaded' });
      const t = await page.evaluate(() => document.documentElement.getAttribute('data-theme'));
      assert(t === v.theme, 'data-theme is ' + t);
    });

    await step(`${v.name}: channel 1-byte badge warns, 2/3-byte stay neutral`, async () => {
      await page.waitForFunction(() => typeof window._channelsRenderMessagesForTest === 'function');
      await page.evaluate(() => {
        const base = { text: 'hello', timestamp: '2026-10-07T08:00:00Z', observers: [], repeats: 1 };
        window._channelsSetStateForTest({
          channels: [{ hash: '#e2e-353', name: '#e2e-353', messageCount: 3 }],
          messages: [
            Object.assign({}, base, { sender: 'OneByte', packetHash: 'h1', senderPathHashSize: 1 }),
            Object.assign({}, base, { sender: 'TwoByte', packetHash: 'h2', senderPathHashSize: 2 }),
            Object.assign({}, base, { sender: 'ThreeByte', packetHash: 'h3', senderPathHashSize: 3 }),
          ],
          selectedHash: '#e2e-353',
        });
        document.querySelector('.ch-layout')?.classList.add('ch-detail-open');
        window._channelsRenderMessagesForTest();
      });
      const badges = page.locator('.ch-path-hash-badge');
      assert(await badges.count() === 3, 'badge count ' + await badges.count());
      const warn = page.locator('.ch-path-hash-badge--warn');
      assert(await warn.count() === 1, 'warn count ' + await warn.count());
      await warn.scrollIntoViewIfNeeded();
      assertWarn(await readWarn(warn), 'ch-path-hash-badge', 'channel');
      assert(await warn.textContent() === 'Warning: Sent with: 1-byte', 'warn text ' + await warn.textContent());
      const neutral = await page.locator('.ch-path-hash-badge:not(.ch-path-hash-badge--warn)').evaluateAll((els) =>
        els.map((el) => ({ text: el.textContent, icon: !!el.querySelector('svg'), title: el.title })));
      assert(JSON.stringify(neutral.map((n) => n.text)) === '["Sent with: 2-byte","Sent with: 3-byte"]', JSON.stringify(neutral));
      assert(neutral.every((n) => !n.icon && n.title === 'Path hash size encoded in the sender’s packet header'), JSON.stringify(neutral));
      await shot('channels');
    });

    await step(`${v.name}: packet detail Hash Size warns on 1-byte`, async () => {
      await page.goto(`${BASE}/#/packet/${HASH_1}`, { waitUntil: 'load' });
      await page.reload({ waitUntil: 'load' });
      const warn = page.locator('dl.detail-meta .detail-hash-size--warn');
      await warn.waitFor();
      await warn.scrollIntoViewIfNeeded();
      assertWarn(await readWarn(warn), 'detail-hash-size', 'packet detail');
      assert(await warn.textContent() === 'Warning: 1 byte', 'text ' + await warn.textContent());
      await shot('packet');
    });

    await step(`${v.name}: packet detail Hash Size stays neutral on 2-byte`, async () => {
      await page.goto(`${BASE}/#/packet/${HASH_2}`, { waitUntil: 'load' });
      await page.reload({ waitUntil: 'load' });
      const size = page.locator('dl.detail-meta .detail-hash-size');
      await size.waitFor();
      assert((await size.textContent()) === '2 bytes', 'text ' + await size.textContent());
      assert(await page.locator('dl.detail-meta .path-hash-warn').count() === 0, '2-byte packet warns');
    });

    await step(`${v.name}: node detail shows the 1-byte badge, not on a 2-byte node`, async () => {
      assert(node1 && node2, 'fixture lacks a 1-byte or 2-byte node');
      await page.goto(`${BASE}/#/nodes/${encodeURIComponent(node1.public_key)}`, { waitUntil: 'load' });
      const warn = page.locator('.node-full-card .node-path-hash-badge--warn');
      await warn.waitFor();
      assertWarn(await readWarn(warn), 'node-path-hash-badge', 'node detail');
      assert(await warn.textContent() === 'Warning: 1-byte path hash', 'text ' + await warn.textContent());
      assert(await page.locator('.node-full-card .multibyte-badge').count() === 0, '1-byte node claims Multibyte');
      await shot('node');
      await page.goto(`${BASE}/#/nodes/${encodeURIComponent(node2.public_key)}`, { waitUntil: 'load' });
      await page.locator('.node-full-card .multibyte-badge').waitFor();
      assert(await page.locator('.node-full-card .node-path-hash-badge').count() === 0, '2-byte node warns');
    });

    await context.close();
  }

  await browser.close();
  console.log(`\n=== ${passed} passed, ${failed} failed ===`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
