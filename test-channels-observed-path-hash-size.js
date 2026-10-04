/**
 * Observed path-hash-size frontend contract.
 *
 * Exercises the real helpers exported by public/channels.js.  The server may
 * expose snake_case evidence on packet/WebSocket shapes and camelCase evidence
 * on channel-message REST rows.  The browser keeps one sorted, unique union per
 * packet hash so a delayed REST response cannot erase richer live evidence.
 */
'use strict';

const vm = require('vm');
const fs = require('fs');
const assert = require('assert');

const noop = () => {};
const fakeEl = {
  addEventListener: noop,
  querySelector: () => fakeEl,
  querySelectorAll: () => [],
  classList: { add: noop, remove: noop, toggle: noop, contains: () => false },
  appendChild: noop,
  removeChild: noop,
  setAttribute: noop,
  getAttribute: () => null,
  textContent: '',
  innerHTML: '',
  style: {},
  dataset: {},
};
const doc = {
  readyState: 'complete',
  createElement: () => ({ ...fakeEl }),
  head: fakeEl,
  body: fakeEl,
  getElementById: () => null,
  querySelector: () => null,
  querySelectorAll: () => [],
  addEventListener: noop,
};
const win = { addEventListener: noop };
const ctx = {
  window: win,
  document: doc,
  console,
  Date,
  Math,
  JSON,
  Set,
  Map,
  Array,
  Object,
  Promise,
  Response: function () {},
  Error,
  setTimeout,
  clearTimeout,
  setInterval,
  clearInterval,
  history: { replaceState: noop, pushState: noop },
  location: { hash: '', href: '', pathname: '/' },
  navigator: { userAgent: 'node' },
  RegionFilter: { getRegionParam: () => '' },
  api: () => Promise.resolve({ messages: [] }),
  CLIENT_TTL: {},
  ChannelDecrypt: undefined,
  truncate: (s) => s,
  formatHashHex: (h) => String(h),
  channelDisplayName: (c) => c && c.name,
  escapeHtml: (s) => String(s)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;'),
  getSenderColor: () => 'var(--accent)',
  fetch: () => Promise.resolve({ json: () => Promise.resolve({}) }),
  btoa: (s) => Buffer.from(s, 'binary').toString('base64'),
  registerPage: noop,
  senderPathHashSize: (rawHex) => rawHex === '59C0' ? 3 : null,
};
vm.createContext(ctx);
// Expose the cache fetch helper only inside this VM so the regression can
// exercise the real delta/early-return path without adding a production API.
const channelsSource = fs.readFileSync('public/channels.js', 'utf8').replace(
  'window._channelsRenderMessagesForTest = renderMessages;',
  'window._channelsFetchAndDecryptChannelForTest = fetchAndDecryptChannel;\n' +
  '  window._channelsRenderMessagesForTest = renderMessages;');
vm.runInContext(channelsSource, ctx);

const normalize = ctx.window._channelsNormalizeObservedPathHashSizesForTest;
const union = ctx.window._channelsUnionObservedPathHashSizesForTest;
const badge = ctx.window._channelsRenderObservedPathHashBadgeForTest;
const senderBadge = ctx.window._channelsRenderSenderPathHashBadgeForTest;
const merge = ctx.window._channelsMergeWsAppendedIntoRestForTest;
const dedup = ctx.window._channelsDeduplicateAndMergeForTest;
const fetchAndDecrypt = ctx.window._channelsFetchAndDecryptChannelForTest;

for (const [name, fn] of Object.entries({ normalize, union, badge, senderBadge, merge, dedup, fetchAndDecrypt })) {
  if (typeof fn !== 'function') {
    console.error('FATAL: missing channels.js test export: ' + name);
    process.exit(2);
  }
}

let passed = 0;
let failed = 0;
const tests = [];
function test(name, fn) {
  tests.push({ name, fn });
}

function plain(value) {
  return JSON.parse(JSON.stringify(value));
}

console.log('\n=== channels observed path-hash-size evidence ===');

test('normalizes both API spellings into sorted unique 1/2/3 evidence', () => {
  assert.deepStrictEqual(
    plain(normalize({
      observedPathHashSizes: [3, 1, 2, 2],
      observed_path_hash_sizes: [1, 3],
    })),
    [1, 2, 3]);
});

test('rejects unknown, empty and injection-shaped values', () => {
  assert.deepStrictEqual(plain(normalize(null)), []);
  assert.deepStrictEqual(plain(normalize({ observedPathHashSizes: [] })), []);
  assert.deepStrictEqual(
    plain(normalize({ observed_path_hash_sizes: [0, 4, null, true, '2<script>', {}, 2] })),
    [2]);
});

test('unions evidence without mutating either input', () => {
  const a = { observedPathHashSizes: [3, 1] };
  const b = { observed_path_hash_sizes: [2, 1] };
  assert.deepStrictEqual(plain(union(a, b)), [1, 2, 3]);
  assert.deepStrictEqual(a.observedPathHashSizes, [3, 1]);
  assert.deepStrictEqual(b.observed_path_hash_sizes, [2, 1]);
});

test('single evidence badge has exact label and conservative tooltip', () => {
  const html = badge({ observedPathHashSizes: [2] });
  assert.match(html, />Observed path hash: 2-byte</);
  assert.match(html, /title="Path hash size observed in one or more relayed wire paths for this message\. Direct zero-hop copies do not provide hash-size evidence\. This does not prove the sender’s permanent configuration\."/);
});

test('mixed evidence badge is sorted and compact', () => {
  const html = badge({ observed_path_hash_sizes: [3, 1, 2] });
  assert.match(html, />Mixed path hashes: 1\/2\/3-byte</);
  assert.ok(!html.includes('undefined'));
});

test('unknown evidence renders no badge and cannot inject markup', () => {
  assert.strictEqual(badge({}), '');
  assert.strictEqual(badge({ observedPathHashSizes: ['<img src=x onerror=alert(1)>'] }), '');
});

test('sender badge uses only a valid encoded width', () => {
  assert.match(senderBadge({ senderPathHashSize: 2 }), />Sent with: 2-byte</);
  assert.strictEqual(senderBadge({ senderPathHashSize: 0 }), '');
  assert.strictEqual(senderBadge({ senderPathHashSize: '<img src=x onerror=alert(1)>' }), '');
});

test('REST refresh retains a sender width from the matching live message', () => {
  const current = [{ packetHash: 'sender', senderPathHashSize: 3, _fromWS: true, _wsAt: Date.now() }];
  const rest = [{ packetHash: 'sender', text: 'REST' }];
  const out = merge(current, rest);
  assert.strictEqual(out[0].senderPathHashSize, 3);
  assert.strictEqual(rest[0].senderPathHashSize, undefined);
});

test('REST refresh unions a matching WS message instead of erasing its evidence', () => {
  const current = [{
    packetHash: 'same',
    _fromWS: true,
    _wsAt: Date.now(),
    observedPathHashSizes: [2, 3],
  }];
  const rest = [{ packetHash: 'same', text: 'REST', observedPathHashSizes: [1] }];
  const out = merge(current, rest);
  assert.strictEqual(out.length, 1);
  assert.strictEqual(out[0].text, 'REST', 'REST remains the preferred row');
  assert.deepStrictEqual(plain(out[0].observedPathHashSizes), [1, 2, 3]);
  assert.deepStrictEqual(rest[0].observedPathHashSizes, [1], 'REST input is not mutated');
});

test('REST then WS and WS then REST produce the same evidence union', () => {
  const ws = [{ packetHash: 'same', _fromWS: true, _wsAt: Date.now(), observed_path_hash_sizes: [3] }];
  const rest = [{ packetHash: 'same', observedPathHashSizes: [1, 2] }];
  const a = merge(ws, rest)[0];
  const b = merge(rest, ws)[0];
  assert.deepStrictEqual(plain(a.observedPathHashSizes), [1, 2, 3]);
  assert.deepStrictEqual(plain(b.observedPathHashSizes), [1, 2, 3]);
});

test('cache/decrypt dedup unions evidence for duplicate packet hashes', () => {
  const cached = [{ packetHash: 'dup', observedPathHashSizes: [1], text: 'cached' }];
  const fresh = [{ packetHash: 'dup', observed_path_hash_sizes: [2, 3], text: 'fresh' }];
  const out = dedup(cached, fresh);
  assert.strictEqual(out.length, 1);
  assert.strictEqual(out[0].text, 'cached', 'existing cache row stays canonical');
  assert.deepStrictEqual(plain(out[0].observedPathHashSizes), [1, 2, 3]);
});

test('delta cache absorbs later evidence for the same packet without re-decrypting', async () => {
  const timestamp = '2026-09-28T08:00:00Z';
  const legacyMessage = {
    packetHash: 'same-packet',
    timestamp,
    sender: 'Cached sender',
    text: 'Keep this decrypted text',
  };
  let stored = null;
  let decryptCalls = 0;

  ctx.ChannelDecrypt = {
    hexToBytes: () => new Uint8Array(16),
    getCache: () => ({ messages: [legacyMessage], lastTimestamp: timestamp, count: 1 }),
    setCache: (key, messages, lastTimestamp, count) => {
      stored = { key, messages: plain(messages), lastTimestamp, count };
    },
    decryptPacket: async () => { decryptCalls++; return null; },
  };
  ctx.api = async () => ({
    packets: [{
      hash: 'same-packet',
      first_seen: timestamp,
      raw_hex: '59C0',
      observed_path_hash_sizes: [2],
      decoded_json: { type: 'CHAN', channel: '#test', sender: 'API sender', text: 'API text' },
    }],
  });

  const result = await fetchAndDecrypt('00'.repeat(16), 0, '#test');
  assert.strictEqual(result.fromCache, true);
  assert.strictEqual(result.messages[0].text, 'Keep this decrypted text', 'cached plaintext is preserved');
  assert.deepStrictEqual(plain(result.messages[0].observedPathHashSizes), [2]);
  assert.strictEqual(result.messages[0].senderPathHashSize, 3);
  assert.strictEqual(decryptCalls, 0, 'same timestamp is not decrypted again');
  assert.ok(stored, 'enriched legacy cache is persisted');
  assert.deepStrictEqual(stored.messages[0].observedPathHashSizes, [2]);
  assert.strictEqual(stored.messages[0].senderPathHashSize, 3);
  assert.strictEqual(stored.count, 1);
});

test('delta cache unions richer evidence without replacing cached plaintext', async () => {
  const timestamp = '2026-09-28T08:00:00Z';
  const cachedMessage = {
    packetHash: 'same-packet',
    timestamp,
    sender: 'Cached sender',
    text: 'Keep this decrypted text',
    observedPathHashSizes: [1],
  };
  let stored = null;

  ctx.ChannelDecrypt = {
    hexToBytes: () => new Uint8Array(16),
    getCache: () => ({ messages: [cachedMessage], lastTimestamp: timestamp, count: 1 }),
    setCache: (key, messages, lastTimestamp, count) => {
      stored = { key, messages: plain(messages), lastTimestamp, count };
    },
    decryptPacket: async () => { throw new Error('must not decrypt unchanged candidate'); },
  };
  ctx.api = async () => ({
    packets: [{
      hash: 'same-packet',
      first_seen: timestamp,
      observed_path_hash_sizes: [2, 3],
      decoded_json: { type: 'CHAN', channel: '#test', sender: 'API sender', text: 'API text' },
    }],
  });

  const result = await fetchAndDecrypt('00'.repeat(16), 0, '#test');
  assert.strictEqual(result.messages[0].text, 'Keep this decrypted text');
  assert.deepStrictEqual(plain(result.messages[0].observedPathHashSizes), [1, 2, 3]);
  assert.deepStrictEqual(stored.messages[0].observedPathHashSizes, [1, 2, 3]);
});

async function main() {
  for (const entry of tests) {
    try {
      await entry.fn();
      passed++;
      console.log('  ✅ ' + entry.name);
    } catch (e) {
      failed++;
      console.log('  ❌ ' + entry.name + ': ' + e.message);
    }
  }
  console.log('\n=== ' + passed + ' passed, ' + failed + ' failed ===\n');
  process.exit(failed === 0 ? 0 : 1);
}

main();
