/**
 * Browser regression for observed path-hash-size evidence on channel messages.
 *
 * Uses channels.js' real state/render/WS hooks against the local test server;
 * no production or external host is contacted.
 */
'use strict';

const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const TOOLTIP = 'Path hash size encoded in the sender’s packet header';

let passed = 0;
let failed = 0;
async function step(name, fn) {
  try {
    await fn();
    passed++;
    console.log('  ✓ ' + name);
  } catch (e) {
    failed++;
    console.error('  ✗ ' + name + ': ' + e.message);
  }
}
function assert(condition, message) {
  if (!condition) throw new Error(message || 'assertion failed');
}

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  const context = await browser.newContext({ viewport: { width: 1280, height: 800 } });
  const page = await context.newPage();
  page.setDefaultTimeout(8000);

  console.log('\n=== channel observed path-hash-size browser regression ===');

  await page.goto(BASE + '/#/channels', { waitUntil: 'domcontentloaded' });
  await page.waitForFunction(() =>
    typeof window._channelsRenderMessagesForTest === 'function' &&
    typeof window._channelsProcessWSBatchForTest === 'function');

  async function render(messages) {
    await page.evaluate((items) => {
      window._channelsSetStateForTest({
        channels: [{ hash: '#hash-evidence', name: '#hash-evidence', messageCount: items.length }],
        messages: items,
        selectedHash: '#hash-evidence',
      });
      document.querySelector('.ch-layout')?.classList.add('ch-detail-open');
      window._channelsRenderMessagesForTest();
    }, messages);
  }

  const baseMessage = {
    sender: 'EvidenceNode',
    text: 'hello',
    timestamp: '2026-09-28T08:00:00Z',
    packetHash: 'evidence-hash',
    observers: [],
    repeats: 1,
  };

  await step('sender size replaces the observed-path label', async () => {
    await render([{ ...baseMessage, observedPathHashSizes: [1], senderPathHashSize: 2 }]);
    const badge = page.locator('.ch-path-hash-badge');
    assert(await badge.count() === 1, 'expected one badge');
    assert(await badge.textContent() === 'Sent with: 2-byte', 'wrong sender-size label');
    assert(await badge.getAttribute('title') === TOOLTIP, 'tooltip wording drifted');
  });

  await step('zero-hop flood has a sender label while direct zero-hop does not', async () => {
    const sizes = await page.evaluate(() => [
      senderPathHashSize('1540DEADBEEF'), senderPathHashSize('1600DEADBEEF'),
    ]);
    assert(JSON.stringify(sizes) === '[2,null]', 'header rule disagrees with zero-hop contract');
    await render([
      { ...baseMessage, packetHash: 'flood-zero', senderPathHashSize: sizes[0] },
      { ...baseMessage, packetHash: 'direct-zero', senderPathHashSize: sizes[1] },
    ]);
    assert(await page.locator('.ch-path-hash-badge').count() === 1, 'direct marker rendered a badge');
    assert(await page.locator('.ch-path-hash-badge').textContent() === 'Sent with: 2-byte', 'flood width hidden');
  });

  await step('mixed observed evidence stays internal; unknown sender size has no badge', async () => {
    await render([
      { ...baseMessage, packetHash: 'mixed', observed_path_hash_sizes: [3, 1, 2], senderPathHashSize: 3 },
      { ...baseMessage, packetHash: 'unknown', observedPathHashSizes: [] },
    ]);
    const badges = page.locator('.ch-path-hash-badge');
    assert(await badges.count() === 1, 'unknown message must not render a badge');
    assert(await badges.first().textContent() === 'Sent with: 3-byte', 'wrong sender label');
  });

  await step('untrusted evidence cannot create markup or attributes', async () => {
    await render([{
      ...baseMessage,
      observedPathHashSizes: ['<img src=x onerror="window.__hashSizePwned=1">'],
      senderPathHashSize: '<img src=x onerror="window.__hashSizePwned=1">',
    }]);
    assert(await page.locator('.ch-path-hash-badge').count() === 0, 'malformed evidence rendered a badge');
    assert(await page.locator('#chMessages img').count() === 0, 'evidence injected an image');
    assert(await page.evaluate(() => !window.__hashSizePwned), 'injected handler executed');
  });

  await step('locally decrypted WS shape preserves packet evidence', async () => {
    await render([]);
    await page.evaluate(() => {
      window._channelsProcessWSBatchForTest([{
        type: 'packet',
        data: {
          hash: 'client-decrypted-hash',
          raw_hex: '1580DEADBEEF',
          packet: { observed_path_hash_sizes: [3], raw_hex: '1580DEADBEEF' },
          decoded: {
            header: { payloadTypeName: 'GRP_TXT' },
            payload: {
              channel: '#hash-evidence',
              sender: 'LocalDecrypt',
              text: 'decrypted in browser',
              decryptedLocally: true,
            },
          },
        },
      }], []);
    });
    const state = await page.evaluate(() => window._channelsGetStateForTest());
    const message = state.messages.find((item) => item.packetHash === 'client-decrypted-hash');
    assert(message, 'client-decrypted WS message was not appended');
    assert(JSON.stringify(message.observedPathHashSizes) === '[3]', 'WS evidence was not normalized');
    assert(message.senderPathHashSize === 3, 'WS sender size missing');
    assert(await page.locator('.ch-path-hash-badge').textContent() === 'Sent with: 3-byte', 'WS badge missing');
  });

  await step('duplicate WS observations union their known sizes', async () => {
    await render([]);
    await page.evaluate(() => {
      const make = (size, observer) => ({
        type: 'message',
        data: {
          hash: 'ws-union-hash',
          observer: observer,
          observed_path_hash_sizes: [size],
          raw_hex: '1540DEADBEEF',
          decoded: { payload: { channel: '#hash-evidence', sender: 'UnionNode', text: 'same' } },
        },
      });
      window._channelsProcessWSBatchForTest([make(1, 'one')], []);
      window._channelsProcessWSBatchForTest([make(2, 'two')], []);
    });
    const sizes = await page.evaluate(() => {
      const item = window._channelsGetStateForTest().messages.find((m) => m.packetHash === 'ws-union-hash');
      return item && item.observedPathHashSizes;
    });
    assert(JSON.stringify(sizes) === '[1,2]', 'duplicate observations did not union: ' + JSON.stringify(sizes));
    assert(await page.locator('.ch-path-hash-badge').textContent() === 'Sent with: 2-byte', 'WS sender badge missing');
  });

  await step('delayed REST refresh cannot overwrite richer WS evidence', async () => {
    await render([{
      ...baseMessage,
      packetHash: 'refresh-union-hash',
      observedPathHashSizes: [2, 3],
      senderPathHashSize: 2,
      _fromWS: true,
      _wsAt: Date.now(),
    }]);
    const result = await page.evaluate(async () => {
      const originalApi = window.api;
      window.api = async function (path) {
        if (path.indexOf('/channels/') === 0) {
          return { messages: [{
            sender: 'EvidenceNode',
            text: 'hello',
            timestamp: '2026-09-28T08:00:00Z',
            packetHash: 'refresh-union-hash',
            observers: [],
            repeats: 1,
            observedPathHashSizes: [1],
          }] };
        }
        return originalApi.apply(this, arguments);
      };
      try {
        await window._channelsRefreshMessagesForTest({ forceNoCache: true });
        const item = window._channelsGetStateForTest().messages.find((m) => m.packetHash === 'refresh-union-hash');
        return item && item.observedPathHashSizes;
      } finally {
        window.api = originalApi;
      }
    });
    assert(JSON.stringify(result) === '[1,2,3]', 'REST refresh lost WS evidence: ' + JSON.stringify(result));
    assert(await page.locator('.ch-path-hash-badge').textContent() === 'Sent with: 2-byte', 'refresh badge missing');
  });

  await step('badge remains visible without horizontal overflow at 375px', async () => {
    await page.setViewportSize({ width: 375, height: 740 });
    await render([{ ...baseMessage, observedPathHashSizes: [1, 2, 3], senderPathHashSize: 2 }]);
    const metrics = await page.locator('.ch-path-hash-badge').evaluate((el) => {
      const rect = el.getBoundingClientRect();
      const style = getComputedStyle(el);
      return {
        left: rect.left,
        right: rect.right,
        visible: rect.width > 0 && rect.height > 0,
        viewport: document.documentElement.clientWidth,
        docWidth: document.documentElement.scrollWidth,
        display: style.display,
        maxWidth: style.maxWidth,
      };
    });
    assert(metrics.visible, 'badge is not visible on mobile');
    assert(metrics.display === 'inline-flex' && metrics.maxWidth === '100%',
      'compact/mobile badge CSS is not applied: ' + JSON.stringify(metrics));
    assert(metrics.left >= 0 && metrics.right <= metrics.viewport + 0.5,
      'badge clips outside viewport: ' + JSON.stringify(metrics));
    assert(metrics.docWidth <= metrics.viewport + 0.5,
      'badge introduces horizontal page overflow: ' + JSON.stringify(metrics));
  });

  await browser.close();
  console.log('\n=== observed path-hash-size browser: ' + passed + ' passed, ' + failed + ' failed ===\n');
  process.exit(failed === 0 ? 0 : 1);
})().catch((e) => {
  console.error(e && e.stack ? e.stack : e);
  process.exit(1);
});
