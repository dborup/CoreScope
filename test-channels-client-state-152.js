/**
 * #152 — loadChannels() must keep client-only channel state across a
 * channel-list refresh.
 *
 * loadChannels() replaced `channels` with the server snapshot and carried
 * nothing across. Every refresh (region change, show-encrypted toggle,
 * shared-channel approval) therefore:
 *   - closed an open PSK conversation (a user:* hash is never in the server
 *     snapshot, so reconcileSelectionAfterChannelRefresh() evicted it,
 *     emptied `messages` and rewrote the URL to #/channels);
 *   - dropped the My Channels section and the user's labels;
 *   - reset unread badges and the sidebar preview of user:* rows.
 *
 * These tests load the real channels.js (plus channel-decrypt.js and
 * channel-proposals.js) in a vm sandbox and drive the real loadChannels(),
 * the real WS path and the real init() handlers through the existing test
 * hooks. Nothing here re-implements production logic.
 *
 * Usage: node test-channels-client-state-152.js
 */
'use strict';

const vm = require('vm');
const fs = require('fs');
const path = require('path');
const assert = require('assert');
const nodeCrypto = require('crypto');
const { webcrypto } = nodeCrypto;

// Encrypts a channel message exactly the way internal/channel/channel.go /
// public/channel-decrypt.js expect it (see that file's header comment):
// AES-128-ECB(timestamp(4 LE) + flags(1) + "sender: message" + 0x00, padded
// to a block boundary) plus an HMAC-SHA256(key + 16 zero bytes) MAC
// truncated to 2 bytes. Node's built-in `crypto` implements plain AES-128
// and HMAC-SHA256, so this round-trips through the real (decrypt-only)
// public/vendor/aes-ecb.js the way a real radio-encrypted packet would.
function encryptChannelMessage(keyHex, sender, text) {
  const keyBytes = Buffer.from(keyHex, 'hex');
  const body = Buffer.from(sender + ': ' + text, 'utf8');
  const header = Buffer.alloc(5);
  header.writeUInt32LE(Math.floor(Date.now() / 1000) >>> 0, 0);
  let plaintext = Buffer.concat([header, body, Buffer.from([0])]);
  const pad = (16 - (plaintext.length % 16)) % 16;
  if (pad) plaintext = Buffer.concat([plaintext, Buffer.alloc(pad)]);
  const cipher = nodeCrypto.createCipheriv('aes-128-ecb', keyBytes, null);
  cipher.setAutoPadding(false);
  const ciphertext = Buffer.concat([cipher.update(plaintext), cipher.final()]);
  const secret = Buffer.concat([keyBytes, Buffer.alloc(16)]);
  const mac = nodeCrypto.createHmac('sha256', secret).update(ciphertext).digest().slice(0, 2).toString('hex');
  const channelHash = nodeCrypto.createHash('sha256').update(keyBytes).digest()[0];
  return { encryptedData: ciphertext.toString('hex'), mac, channelHash };
}

function encryptedPacket(hash, firstSeen, enc) {
  return { hash, first_seen: firstSeen, decoded_json: { type: 'GRP_TXT', encryptedData: enc.encryptedData, mac: enc.mac, channelHash: enc.channelHash } };
}

const RealDate = Date;
const HOUR = 60 * 60 * 1000;
const PSK_NAME = 'psk:0badc0de';
const PSK_HASH = 'user:' + PSK_NAME;
const PSK_KEY = '0badc0de0badc0de0badc0de0badc0de';

// A Date whose "now" is shifted by skewMs, to simulate a browser clock that
// runs ahead of (skewMs > 0) or behind (skewMs < 0) the server clock.
function makeSkewedDate(skewMs) {
  function SkewedDate() {
    const args = Array.prototype.slice.call(arguments);
    if (!(this instanceof SkewedDate)) return new RealDate(RealDate.now() + skewMs).toString();
    if (args.length === 0) return new RealDate(RealDate.now() + skewMs);
    return new (Function.prototype.bind.apply(RealDate, [null].concat(args)))();
  }
  SkewedDate.now = () => RealDate.now() + skewMs;
  SkewedDate.parse = RealDate.parse;
  SkewedDate.UTC = RealDate.UTC;
  SkewedDate.prototype = RealDate.prototype;
  return SkewedDate;
}

function deferred() {
  let resolve, reject;
  const promise = new Promise((res, rej) => { resolve = res; reject = rej; });
  return { promise, resolve, reject };
}

async function flush(n) {
  for (let i = 0; i < (n || 20); i++) await new Promise((r) => setImmediate(r));
}

// Flushes until cond() holds or ~2s of real time pass. For steps that wait
// on Web Crypto (computeChannelHash), whose digest runs off the event loop
// and can outlast a fixed number of flushes on a busy machine.
async function settle(cond, ms) {
  const deadline = RealDate.now() + (ms || 2000);
  while (!cond() && RealDate.now() < deadline) await new Promise((r) => setTimeout(r, 5));
  return cond();
}

function makeHarness(opts) {
  opts = opts || {};
  const storage = {};
  const elements = {};
  function makeFakeEl(id) {
    return {
      id: id || '', innerHTML: '', textContent: '', value: '', scrollTop: 0,
      scrollHeight: 0, clientHeight: 0,
      style: {}, dataset: {},
      classList: { add() {}, remove() {}, toggle() {}, contains() { return false; } },
      addEventListener() {}, removeEventListener() {},
      // One child per selector, so a test can read what the page wrote to
      // e.g. #chHeader .ch-header-text.
      querySelector(sel) { this._q = this._q || {}; return this._q[sel] || (this._q[sel] = makeFakeEl()); },
      querySelectorAll() { return []; },
      getAttribute() { return null; }, setAttribute() {}, removeAttribute() {},
      getBoundingClientRect() { return { width: 240, height: 0, top: 0, left: 0, right: 0, bottom: 0 }; },
      appendChild() {}, removeChild() {}, remove() {},
      focus() {}, blur() {},
      checked: false,
    };
  }
  function el(id) {
    if (!elements[id]) elements[id] = makeFakeEl(id);
    return elements[id];
  }

  const historyCalls = [];
  const windowListeners = {};
  const h = {
    elements,
    historyCalls,
    regionChange: null,
    approvedCallback: null,
    // Each /channels request is answered by h.respondChannels(path); tests
    // replace it to return a deferred promise when they need a request in
    // flight.
    respondChannels: () => Promise.resolve({ channels: [] }),
    channelRequests: [],
    // Each /packets request (the client-side decrypt fetch) is answered by
    // h.respondPackets(path); tests replace it to control decrypt timing.
    respondPackets: () => Promise.resolve({ packets: [] }),
    packetsRequests: [],
  };

  const ctx = {
    window: {
      addEventListener(type, fn) { (windowListeners[type] = windowListeners[type] || []).push(fn); },
      removeEventListener() {},
      // opts.mobile: the (max-width: 767px) query matches, so
      // renderChannelList() uses the #1367 mobile rows (#155).
      matchMedia: () => ({ matches: !!opts.mobile, addEventListener() {}, removeEventListener() {} }),
    },
    document: {
      readyState: 'complete',
      documentElement: { getAttribute: () => null, setAttribute() {}, classList: { add() {}, remove() {}, toggle() {}, contains() { return false; } } },
      createElement: () => makeFakeEl(),
      head: { appendChild() {} },
      body: { appendChild() {}, removeChild() {}, contains() { return false; } },
      getElementById: el,
      addEventListener() {}, removeEventListener() {},
      querySelector: () => null,
      querySelectorAll: () => [],
    },
    console,
    Date: makeSkewedDate(opts.skewMs || 0),
    Math, Array, Object, String, Number, JSON, RegExp, Error, TypeError, Set, Map, Promise,
    parseInt, parseFloat, isNaN, isFinite,
    encodeURIComponent, decodeURIComponent,
    setTimeout: (fn) => { Promise.resolve().then(fn); return 0; },
    clearTimeout: () => {},
    setInterval: () => 0,
    clearInterval: () => {},
    fetch: () => Promise.resolve({ ok: true, json: () => Promise.resolve({}) }),
    performance: { now: () => RealDate.now() },
    localStorage: {
      getItem: (k) => Object.prototype.hasOwnProperty.call(storage, k) ? storage[k] : null,
      setItem: (k, v) => { storage[k] = String(v); },
      removeItem: (k) => { delete storage[k]; },
    },
    location: { hash: '' },
    history: { replaceState(_s, _t, url) { historyCalls.push(url); }, pushState() {} },
    crypto: webcrypto,
    TextEncoder, TextDecoder,
    Uint8Array, Uint16Array, Uint32Array, Int8Array, Int16Array, Int32Array, ArrayBuffer,
    URLSearchParams,
    CustomEvent: class CustomEvent {},
    MutationObserver: class MutationObserver { observe() {} disconnect() {} },
    requestAnimationFrame: (cb) => { Promise.resolve().then(cb); return 0; },
    matchMedia: () => ({ matches: false, addEventListener() {}, removeEventListener() {} }),
    addEventListener() {}, dispatchEvent() {},
    getHashParams: () => new URLSearchParams(),
    btoa: (s) => Buffer.from(String(s), 'binary').toString('base64'),
    atob: (s) => Buffer.from(String(s), 'base64').toString('binary'),
  };
  ctx.self = ctx;
  ctx.globalThis = ctx;

  // app.js / roles.js stubs.
  ctx.onWS = () => {};
  ctx.offWS = () => {};
  // h.wsHandler: the batch handler init() registers (the real one, with the
  // live PSK decrypt and the unread bump), called without the debounce.
  ctx.debouncedOnWS = (fn) => { h.wsHandler = fn; return fn; };
  ctx.debounce = (fn) => fn;
  ctx.invalidateApiCache = () => {};
  ctx.api = (p) => {
    if (p.indexOf('/channels') === 0 && p.indexOf('/messages') === -1) {
      h.channelRequests.push(p);
      return h.respondChannels(p);
    }
    if (p.indexOf('/packets') === 0) {
      h.packetsRequests.push(p);
      return h.respondPackets(p);
    }
    if (p.indexOf('/observers') === 0) return Promise.resolve({ observers: [] });
    return Promise.resolve({ messages: [], packets: [], channels: [] });
  };
  ctx.CLIENT_TTL = { channels: 15000, observers: 120000, channelMessages: 10000, nodeDetail: 10000 };
  ctx.escapeHtml = (s) => String(s == null ? '' : s);
  ctx.truncate = (s, n) => { s = String(s || ''); return s.length > n ? s.slice(0, n) : s; };
  ctx.formatHashHex = (x) => String(x);
  ctx.formatSecondsAgo = () => '';
  ctx.timeAgo = () => '';
  ctx.payloadTypeName = () => 'GRP_TXT';
  ctx.ROLE_EMOJI = {};
  ctx.ROLE_LABELS = {};
  ctx.RegionFilter = {
    init() {},
    onChange(fn) { h.regionChange = fn; return fn; },
    offChange() {},
    getRegionParam() { return h.regionParam || ''; },
    getSelected() { return null; },
  };
  ctx.ChannelColors = { get() { return null; }, remove() {} };
  ctx.ChannelColorPicker = { open() {} };
  let pageMod = null;
  ctx.registerPage = (name, mod) => { if (name === 'channels') pageMod = mod; };

  vm.createContext(ctx);
  function load(file) {
    const src = fs.readFileSync(path.join(__dirname, file), 'utf8');
    vm.runInContext(src, ctx, { filename: file });
    for (const k of Object.keys(ctx.window)) ctx[k] = ctx.window[k];
  }
  load('public/vendor/aes-ecb.js');
  load('public/channel-decrypt.js');
  load('public/channel-proposals.js');
  // Capture the onApproved callback init() hands to ChannelProposals.mount
  // without letting mount() touch the DOM or start polling. The pure
  // mergeApprovedChannels() stays the real one.
  ctx.window.ChannelProposals.mount = (mountOpts) => { h.approvedCallback = mountOpts && mountOpts.onApproved; };
  ctx.window.ChannelProposals.unmount = () => {};
  load('public/channels.js');

  h.ctx = ctx;
  h.w = ctx.window;
  h.page = pageMod;
  h.showEncryptedChanged = () => (windowListeners['mc-channels-show-encrypted-changed'] || []).forEach((fn) => fn({}));
  h.state = () => ctx.window._channelsGetStateForTest();
  h.setState = (s) => ctx.window._channelsSetStateForTest(s);
  h.row = (hash) => h.state().channels.find((c) => c.hash === hash);
  h.storeKey = (name, key, label) => ctx.window.ChannelDecrypt.storeKey(name, key, label);
  h.removeKey = (name) => ctx.window.ChannelDecrypt.removeKey(name);
  h.init = async () => {
    await pageMod.init(el('page'), null);
    await flush();
    historyCalls.length = 0;
  };
  // A live CHAN message through the real WS path.
  h.liveMessage = (channel, sender, text, hash) => {
    ctx.window._channelsProcessWSBatchForTest([{
      type: 'message',
      data: { hash: hash || ('pkt-' + channel + '-' + text), decoded: { header: { payloadTypeName: 'GRP_TXT' }, payload: { channel, sender, text: sender + ': ' + text } } },
    }], null);
  };
  return h;
}

// The decrypt cache exactly as channel-decrypt.js persisted it.
const DECRYPT_CACHE_KEY = 'corescope_channel_cache';
function rawDecryptCache(h) {
  return h.ctx.localStorage.getItem(DECRYPT_CACHE_KEY) || '';
}
function decryptCacheKeys(h) {
  return Object.keys(JSON.parse(rawDecryptCache(h) || '{}'));
}
function cacheEntry(text, ts) {
  return {
    messages: [{ sender: 'Alice', text, timestamp: ts || '2026-01-01T00:00:00Z', packetHash: 'p-' + text }],
    lastTimestamp: ts || '2026-01-01T00:00:00Z', count: 1, ts: 1,
  };
}

// channel-decrypt.js alone, over a localStorage that enforces a quota the
// way browsers do: a setItem() that would push the total stored characters
// past `quota` throws QuotaExceededError and leaves the old value in place.
function makeDecryptSandbox(opts) {
  opts = opts || {};
  const sb = { storage: Object.assign({}, opts.storage || {}), quota: opts.quota || Infinity };
  sb.used = (exceptKey) => Object.keys(sb.storage).reduce((n, k) => n + (k === exceptKey ? 0 : k.length + sb.storage[k].length), 0);
  const ctx = {
    localStorage: {
      getItem: (k) => Object.prototype.hasOwnProperty.call(sb.storage, k) ? sb.storage[k] : null,
      setItem: (k, v) => {
        v = String(v);
        if (sb.used(k) + k.length + v.length > sb.quota) {
          const e = new Error('The quota has been exceeded.');
          e.name = 'QuotaExceededError';
          throw e;
        }
        sb.storage[k] = v;
      },
      removeItem: (k) => { delete sb.storage[k]; },
    },
    console, Date, JSON, Math, String, Number, Object, Array, RegExp, Error, Promise, parseInt,
    crypto: webcrypto, TextEncoder, TextDecoder, Uint8Array,
  };
  ctx.window = ctx;
  ctx.self = ctx;
  vm.createContext(ctx);
  // lateStorage: the module loads before localStorage exists.
  const storageApi = ctx.localStorage;
  if (opts.lateStorage) delete ctx.localStorage;
  sb.attachStorage = () => { ctx.localStorage = storageApi; };
  vm.runInContext(fs.readFileSync(path.join(__dirname, 'public/channel-decrypt.js'), 'utf8'), ctx, { filename: 'public/channel-decrypt.js' });
  sb.CD = ctx.ChannelDecrypt;
  sb.raw = () => sb.storage[DECRYPT_CACHE_KEY] || '';
  sb.keys = () => Object.keys(JSON.parse(sb.raw() || '{}'));
  return sb;
}
// One cached message whose text is `chars` long, so a cache entry
// serialises to roughly that many characters.
function bigMessages(chars, tag) {
  return [{ sender: 'S', text: tag + ':' + 'x'.repeat(chars), timestamp: '2026-01-01T00:00:00Z', packetHash: 'h-' + tag }];
}

function serverChannel(hash, extra) {
  return Object.assign({
    hash,
    name: hash,
    messageCount: 10,
    lastActivity: new RealDate(RealDate.now() - 60000).toISOString(),
    lastSender: 'Server',
    lastMessage: 'from server',
  }, extra || {});
}

function pskRow(extra) {
  return Object.assign({
    hash: PSK_HASH,
    name: PSK_NAME,
    userLabel: 'Team',
    messageCount: 3,
    lastActivityMs: RealDate.now() - 5000,
    lastSender: 'Bob',
    lastMessage: 'decrypted preview',
    encrypted: true,
    userAdded: true,
  }, extra || {});
}

// A harness with the page initialised, a stored PSK key and an open PSK
// conversation — the state a user is in when they hit the bug.
async function openPskConversation(opts) {
  const h = makeHarness(opts);
  h.storeKey(PSK_NAME, PSK_KEY, 'Team');
  h.respondChannels = () => Promise.resolve({ channels: [serverChannel('public')] });
  await h.init();
  const messages = [
    { sender: 'Alice', text: 'first', timestamp: '2026-01-01T00:00:00Z', packetHash: 'm1' },
    { sender: 'Bob', text: 'second', timestamp: '2026-01-01T00:01:00Z', packetHash: 'm2' },
  ];
  h.setState({
    channels: [Object.assign({}, h.row('public')), pskRow()],
    messages,
    selectedHash: PSK_HASH,
  });
  return { h, messages };
}

function assertConversationOpen(h, messages, label) {
  const s = h.state();
  assert.strictEqual(s.selectedHash, PSK_HASH, label + ': PSK selection must survive');
  assert.strictEqual(s.messages.length, messages.length, label + ': messages must survive');
  assert.strictEqual(s.messages[0].text, 'first', label + ': messages must be the same conversation');
  assert.ok(!h.historyCalls.includes('#/channels'), label + ': URL must not be rewritten to #/channels (got ' + JSON.stringify(h.historyCalls) + ')');
  const row = h.row(PSK_HASH);
  assert.ok(row && row.userAdded === true, label + ': PSK row must stay in My Channels');
  assert.ok(/ch-section-mychannels/.test(h.elements.chList.innerHTML), label + ': rendered list must contain My Channels');
}

let passed = 0, failed = 0;
async function test(name, fn) {
  try { await fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + (e && e.message)); }
}

(async () => {
  console.log('\n=== #152 loadChannels keeps client-only state ===');

  await test('open PSK conversation survives a silent loadChannels() refresh', async () => {
    const { h, messages } = await openPskConversation();
    await h.w._channelsLoadChannelsForTest(true);
    assertConversationOpen(h, messages, 'loadChannels');
  });

  await test('open PSK conversation survives a region change (RegionFilter.onChange handler)', async () => {
    const { h } = await openPskConversation();
    h.regionParam = 'CPH';
    assert.strictEqual(typeof h.regionChange, 'function', 'init() must register a region handler');
    h.regionChange();
    await flush();
    assert.ok(h.channelRequests[h.channelRequests.length - 1].indexOf('region=CPH') !== -1, 'region change must refetch with the region');
    // F1 (#152 follow-up): an encrypted/PSK selection now re-runs
    // selectChannel() for the new region (see below) instead of just
    // refreshMessages() — which has no REST messages to refetch for it — so
    // unlike a plain server channel its messages are freshly re-decrypted
    // for the new region (empty here, since the harness's default /packets
    // response has none), not carried over verbatim. The conversation
    // itself — selection, URL, My Channels membership — still survives.
    const s = h.state();
    assert.strictEqual(s.selectedHash, PSK_HASH, 'region change: PSK selection must survive');
    assert.ok(!h.historyCalls.includes('#/channels'), 'region change: URL must not be rewritten to #/channels (got ' + JSON.stringify(h.historyCalls) + ')');
    const row = h.row(PSK_HASH);
    assert.ok(row && row.userAdded === true, 'region change: PSK row must stay in My Channels');
    assert.ok(/ch-section-mychannels/.test(h.elements.chList.innerHTML), 'region change: rendered list must contain My Channels');
  });

  // ── F1: a region switch mid-decrypt must restart decryption, not strand
  // the pane on "Decrypting messages…" or leave it showing the previous
  // region's messages. refreshMessages() has nothing to refetch for an
  // encrypted row (no REST messages exist), so it must hand off to
  // selectChannel() instead of silently returning.
  await test('F1: region switch mid-decrypt restarts decryption for the new region', async () => {
    const h = makeHarness();
    const KEY = 'f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1';
    const NAME = 'psk:f1test';
    const HASH = 'user:' + NAME;
    h.storeKey(NAME, KEY, 'F1 Team');
    h.respondChannels = () => Promise.resolve({ channels: [] });
    await h.init();

    const oldMsg = encryptChannelMessage(KEY, 'Alice', 'old region message');
    const newMsg = encryptChannelMessage(KEY, 'Bob', 'new region message');

    const pending = deferred();
    h.respondPackets = () => pending.promise;
    const select = h.w._channelsSelectChannelForTest(HASH);
    // selectChannel() awaits ChannelDecrypt.computeChannelHash() (a real
    // crypto.subtle.digest call) before it even reaches the decrypt-and-set
    // "Decrypting…" step, which takes more ticks than a plain microtask
    // flush — give it a generous margin.
    await flush(300);
    assert.ok(/Decrypting/.test(h.elements.chMessages.innerHTML), 'pane must show "Decrypting…" while the fetch is in flight');

    // Region changes while the first decrypt's /packets fetch is still
    // pending. The region handler's loadChannels() + its own /packets
    // fetch (for the new region) resolve before the stale one does.
    h.regionParam = 'CPH';
    h.respondPackets = () => Promise.resolve({ packets: [encryptedPacket('pkt-new', '2026-01-01T00:01:00Z', newMsg)] });
    h.regionChange();
    await flush(300);

    // The original (stale) fetch finally resolves, for the OLD region.
    pending.resolve({ packets: [encryptedPacket('pkt-old', '2026-01-01T00:00:00Z', oldMsg)] });
    await select;
    await flush(300);

    const msgEl = h.elements.chMessages;
    assert.ok(!/Decrypting/.test(msgEl.innerHTML), 'pane must not be stuck on "Decrypting…" (got ' + msgEl.innerHTML.slice(0, 200) + ')');
    const texts = h.state().messages.map((m) => m.text);
    assert.ok(texts.indexOf('new region message') !== -1, 'new-region message must be shown (got ' + JSON.stringify(texts) + ')');
    assert.ok(texts.indexOf('old region message') === -1, 'stale old-region message must not be shown (got ' + JSON.stringify(texts) + ')');
    assert.strictEqual(h.state().selectedHash, HASH, 'conversation must stay open on the same PSK');
  });

  await test('open PSK conversation survives the show-encrypted toggle', async () => {
    const { h, messages } = await openPskConversation();
    h.ctx.localStorage.setItem('channels-show-encrypted', 'true');
    h.showEncryptedChanged();
    await flush();
    assert.ok(h.channelRequests[h.channelRequests.length - 1].indexOf('includeEncrypted=true') !== -1, 'toggle must refetch with includeEncrypted');
    assertConversationOpen(h, messages, 'show-encrypted');
  });

  await test('shared-channel approval (onApproved) keeps the open PSK conversation, one PSK row', async () => {
    const { h, messages } = await openPskConversation();
    h.respondChannels = () => Promise.resolve({
      channels: [serverChannel('public')],
      approvedChannels: [{ hash: '#shared', name: '#shared' }],
    });
    assert.strictEqual(typeof h.approvedCallback, 'function', 'init() must mount ChannelProposals with onApproved');
    const before = h.channelRequests.length;
    h.approvedCallback();
    await flush();
    assert.strictEqual(h.channelRequests.length, before + 1, 'approval must refetch the channel list exactly once');
    assertConversationOpen(h, messages, 'onApproved');
    assert.ok(h.row('#shared') && h.row('#shared').shared === true, 'approved channel must be listed');
    assert.strictEqual(h.state().channels.filter((c) => c.hash === PSK_HASH).length, 1, 'PSK row must appear exactly once');
  });

  await test('My Channels rows and labels survive a refresh (user:* row and key-matched server row)', async () => {
    const h = makeHarness();
    h.storeKey(PSK_NAME, PSK_KEY, 'Team');
    h.storeKey('#ops', 'aa'.repeat(16), 'Ops room');
    h.respondChannels = () => Promise.resolve({ channels: [serverChannel('public'), serverChannel('#ops', { name: '#ops' })] });
    await h.init();
    await h.w._channelsLoadChannelsForTest(true);
    const psk = h.row(PSK_HASH);
    const ops = h.row('#ops');
    assert.ok(psk && psk.userAdded === true && psk.userLabel === 'Team', 'user:* row keeps userAdded + label');
    assert.ok(ops && ops.userAdded === true && ops.userLabel === 'Ops room', 'server row matched by a stored key keeps userAdded + label');
    assert.ok(!h.row('public').userAdded, 'unrelated server row is not marked userAdded');
    assert.ok(/ch-section-mychannels/.test(h.elements.chList.innerHTML), 'My Channels rendered');
    assert.ok(h.elements.chList.innerHTML.indexOf('Team') !== -1, 'PSK label rendered');
  });

  await test('a label re-read from storage wins over the previous list\'s label', async () => {
    const { h } = await openPskConversation();
    h.setState({ channels: [Object.assign({}, h.row('public')), pskRow({ userLabel: 'Old name' })] });
    await h.w._channelsLoadChannelsForTest(true);
    assert.strictEqual(h.row(PSK_HASH).userLabel, 'Team', 'storage label must win (got ' + h.row(PSK_HASH).userLabel + ')');
  });

  await test('unread and preview on a user:* row survive a refresh (explicit 0 on a server row kept)', async () => {
    const { h } = await openPskConversation();
    h.setState({
      channels: [Object.assign({}, h.row('public'), { unread: 0 }), pskRow({ unread: 4 })],
      selectedHash: null,
      messages: [],
    });
    await h.w._channelsLoadChannelsForTest(true);
    const psk = h.row(PSK_HASH);
    assert.strictEqual(psk.unread, 4, 'user:* unread kept (got ' + psk.unread + ')');
    assert.strictEqual(psk.lastMessage, 'decrypted preview', 'user:* preview kept (got ' + psk.lastMessage + ')');
    assert.strictEqual(psk.lastSender, 'Bob', 'user:* preview sender kept');
    assert.strictEqual(psk.messageCount, 3, 'user:* messageCount kept');
    const pub = h.row('public');
    assert.ok(Object.prototype.hasOwnProperty.call(pub, 'unread') && pub.unread === 0, 'explicit unread: 0 kept on a server row');
    assert.ok(/data-unread-channel="user:psk:0badc0de"/.test(h.elements.chList.innerHTML), 'unread badge rendered for the user:* row');
  });

  await test('unread on a server row survives a refresh', async () => {
    const h = makeHarness();
    h.respondChannels = () => Promise.resolve({ channels: [serverChannel('public')] });
    await h.init();
    h.setState({ channels: [Object.assign({}, h.row('public'), { unread: 7 })] });
    await h.w._channelsLoadChannelsForTest(true);
    assert.strictEqual(h.row('public').unread, 7);
  });

  await test('a server channel the new snapshot omits (region filter) does not come back', async () => {
    const h = makeHarness();
    h.respondChannels = () => Promise.resolve({ channels: [serverChannel('public'), serverChannel('#far')] });
    await h.init();
    h.setState({ channels: h.state().channels.map((c) => Object.assign({}, c, { unread: 2 })) });
    h.respondChannels = () => Promise.resolve({ channels: [serverChannel('public')] });
    await h.w._channelsLoadChannelsForTest(true);
    assert.strictEqual(h.row('#far'), undefined, 'omitted row must not be resurrected');
    assert.deepStrictEqual(h.state().channels.map((c) => c.hash), ['public']);
  });

  await test('a user:* row whose key was removed does not come back', async () => {
    const { h } = await openPskConversation();
    h.setState({ selectedHash: null, messages: [] });
    h.removeKey(PSK_NAME);
    await h.w._channelsLoadChannelsForTest(true);
    assert.strictEqual(h.row(PSK_HASH), undefined, 'removed PSK row must not be resurrected from the previous list');
  });

  for (const skew of [HOUR, -HOUR]) {
    const label = skew > 0 ? 'browser clock 1h ahead' : 'browser clock 1h behind';

    await test('WS update during an in-flight request wins (' + label + ')', async () => {
      const h = makeHarness({ skewMs: skew });
      h.respondChannels = () => Promise.resolve({ channels: [serverChannel('public', { messageCount: 10, lastMessage: 'old snapshot' })] });
      await h.init();
      const pending = deferred();
      h.respondChannels = () => pending.promise;
      const load = h.w._channelsLoadChannelsForTest(true);
      await flush();
      h.liveMessage('public', 'Carol', 'live during flight');
      const liveRow = Object.assign({}, h.row('public'));
      // The snapshot was taken on the server before the live packet arrived.
      pending.resolve({ channels: [serverChannel('public', {
        messageCount: 10,
        lastActivity: new RealDate(RealDate.now() - 1000).toISOString(),
        lastSender: 'Server',
        lastMessage: 'snapshot',
      })] });
      await load;
      const row = h.row('public');
      assert.strictEqual(row.lastMessage, 'live during flight', 'live message kept (got ' + row.lastMessage + ')');
      assert.strictEqual(row.lastSender, 'Carol', 'live sender kept with its message');
      assert.strictEqual(row.lastActivityMs, liveRow.lastActivityMs, 'live activity time kept with its message');
      assert.strictEqual(row.messageCount, 11, 'live messageCount kept with its message');
    });

    await test('a stale client row does not override a newer snapshot (' + label + ')', async () => {
      const h = makeHarness({ skewMs: skew });
      h.respondChannels = () => Promise.resolve({ channels: [serverChannel('public', { messageCount: 10 })] });
      await h.init();
      // Live update BEFORE the refresh starts: the snapshot already has it
      // plus a newer message.
      h.liveMessage('public', 'Carol', 'older live');
      h.respondChannels = () => Promise.resolve({ channels: [serverChannel('public', {
        messageCount: 12,
        lastActivity: new RealDate(RealDate.now() - 1000).toISOString(),
        lastSender: 'Dave',
        lastMessage: 'newer snapshot',
      })] });
      await h.w._channelsLoadChannelsForTest(true);
      const row = h.row('public');
      assert.strictEqual(row.lastMessage, 'newer snapshot', 'snapshot message wins (got ' + row.lastMessage + ')');
      assert.strictEqual(row.lastSender, 'Dave', 'snapshot sender wins');
      assert.strictEqual(row.messageCount, 12, 'snapshot count wins');
    });
  }

  await test('loadChannels() does not mutate the api() payload (shared with the api cache)', async () => {
    const h = makeHarness();
    h.storeKey('#ops', 'aa'.repeat(16), 'Ops room');
    const payload = { channels: [serverChannel('#ops', { name: '#ops' })] };
    const snapshot = JSON.stringify(payload);
    h.respondChannels = () => Promise.resolve(payload);
    await h.init();
    await h.w._channelsLoadChannelsForTest(true);
    assert.strictEqual(JSON.stringify(payload), snapshot, 'api payload must be left untouched');
    assert.notStrictEqual(h.row('#ops'), payload.channels[0], 'channels must not alias the payload rows');
  });

  await test('mergeClientChannelState: pure, carries by hash, never resurrects, never mutates inputs', async () => {
    const h = makeHarness();
    const merge = h.w._channelsMergeClientChannelStateForTest;
    assert.strictEqual(typeof merge, 'function', '_channelsMergeClientChannelStateForTest must be exported');
    const deepFreeze = (o) => { Object.values(o).forEach((v) => { if (v && typeof v === 'object') deepFreeze(v); }); return Object.freeze(o); };
    const fresh = deepFreeze([
      { hash: 'public', name: 'public', lastActivityMs: 100, lastMessage: 'fresh', lastSender: 'S', messageCount: 5 },
      { hash: PSK_HASH, name: PSK_NAME, lastActivityMs: 0, lastMessage: 'Encrypted — click to decrypt', lastSender: '', messageCount: 0, userAdded: true, userLabel: '' },
    ]);
    const prev = deepFreeze([
      { hash: 'public', unread: 0, userAdded: true, userLabel: 'Pub', lastActivityMs: 999999, lastMessage: 'stale', lastSender: 'X', messageCount: 99, _wsSeq: 3 },
      { hash: PSK_HASH, unread: 2, userAdded: true, userLabel: 'Team', lastActivityMs: 50, lastMessage: 'preview', lastSender: 'B', messageCount: 1 },
      { hash: 'gone', unread: 9 },
    ]);
    const freshCopy = JSON.stringify(fresh);
    const prevCopy = JSON.stringify(prev);
    const out = merge(fresh, prev, 3);
    assert.strictEqual(JSON.stringify(fresh), freshCopy, 'fresh input untouched');
    assert.strictEqual(JSON.stringify(prev), prevCopy, 'prev input untouched');
    assert.notStrictEqual(out, fresh);
    out.forEach((o, i) => { assert.notStrictEqual(o, fresh[i], 'rows must be copies'); assert.notStrictEqual(o, prev[i]); });
    assert.deepStrictEqual(out.map((o) => o.hash), ['public', PSK_HASH], 'no resurrected rows, fresh order kept');
    assert.strictEqual(out[0].unread, 0, 'explicit 0 carried');
    // F2 (#152 follow-up): userAdded/userLabel are NOT carried from prev —
    // mergeUserChannels() (run before this, on the same fresh list) is the
    // sole source now, so storage stays authoritative. A stale prev value
    // (here simulating an in-memory row a remove-handler forgot to clear)
    // must never leak onto the merged row.
    assert.strictEqual(out[0].userAdded, undefined, 'userAdded must not be carried from prev');
    assert.strictEqual(out[0].userLabel, undefined, 'userLabel must not be carried from prev');
    assert.strictEqual(out[0].lastMessage, 'fresh', 'stamp not newer than request start → snapshot activity wins');
    assert.strictEqual(out[0].messageCount, 5);
    assert.strictEqual(out[1].unread, 2);
    assert.strictEqual(out[1].userLabel, '', 'fresh already set this (by mergeUserChannels in the real pipeline); merge must not override it from prev');
    assert.strictEqual(out[1].lastMessage, 'preview', 'client-only row keeps its preview');
    const newer = merge(fresh, prev, 2);
    assert.strictEqual(newer[0].lastMessage, 'stale', 'stamp newer than request start → live activity wins');
    assert.strictEqual(newer[0].messageCount, 99, 'moved together with its message');
    assert.strictEqual(newer[0].lastSender, 'X');
    assert.strictEqual(newer[0].lastActivityMs, 999999);
    const none = merge(null, prev, 0);
    assert.ok(Array.isArray(none) && none.length === 0, 'non-array fresh → []');
    const noPrev = merge(fresh, null, 0);
    assert.deepStrictEqual(JSON.parse(JSON.stringify(noPrev)), JSON.parse(freshCopy));
    assert.notStrictEqual(noPrev[0], fresh[0]);
  });

  // ── F2: storage must be authoritative for userAdded/userLabel. A removed
  // key's label must not resurrect across refreshes (regression for the
  // remove-handler leaving ch.userLabel set, and mergeClientChannelState
  // carrying it forward from the in-memory previous list).
  await test('F2: a removed key\'s label and My Channels row do not resurrect across two refreshes', async () => {
    const h = makeHarness();
    h.storeKey('#ops', 'aa'.repeat(16), 'Ops room');
    h.respondChannels = () => Promise.resolve({ channels: [serverChannel('#ops', { name: '#ops' })] });
    await h.init();
    const ops = h.row('#ops');
    assert.ok(ops && ops.userAdded === true && ops.userLabel === 'Ops room', 'precondition: key+label present');

    // Remove the key through the same handler the UI uses (server-known
    // channel branch: unmark userAdded, keep the row). removeUserChannelKey
    // itself calls ChannelDecrypt.removeKey(), so don't double-remove.
    h.w._channelsRemoveKeyHandlerForTest('#ops');

    const afterRemove = h.row('#ops');
    assert.strictEqual(afterRemove.userAdded, false, 'userAdded cleared immediately by the remove handler');
    assert.ok(!afterRemove.userLabel, 'userLabel must be cleared immediately by the remove handler (got ' + JSON.stringify(afterRemove.userLabel) + ')');

    for (let i = 0; i < 2; i++) {
      await h.w._channelsLoadChannelsForTest(true);
      const row = h.row('#ops');
      assert.ok(!row.userLabel, 'refresh ' + (i + 1) + ': label must not resurrect (got ' + JSON.stringify(row.userLabel) + ')');
      assert.notStrictEqual(row.userAdded, true, 'refresh ' + (i + 1) + ': My Channels membership must not resurrect');
    }
    assert.ok(!/ch-section-mychannels/.test(h.elements.chList.innerHTML) || !/Ops room/.test(h.elements.chList.innerHTML), 'My Channels must not show the removed label');
  });

  // ── F3: a PSK whose name collides with a server-known or newly-approved
  // shared channel is matched by name in mergeUserChannels() instead of
  // getting its own user:* row, so the user:* hash the conversation opened
  // under disappears from the fresh list even though the conversation is
  // still live. reconcileSelectionAfterChannelRefresh() must remap to the
  // row mergeUserChannels() annotated instead of closing the conversation.
  await test('F3 (region variant): a PSK is remapped, not closed, when a region switch reveals a same-named server channel', async () => {
    const h = makeHarness();
    const NAME = '#chat';
    const HASH = 'user:' + NAME;
    h.storeKey(NAME, 'aa'.repeat(16), 'Chat');
    h.respondChannels = () => Promise.resolve({ channels: [] });
    await h.init();
    h.setState({ selectedHash: HASH, messages: [{ sender: 'Alice', text: 'hello', timestamp: '2026-01-01T00:00:00Z', packetHash: 'm1' }] });
    assert.ok(h.row(HASH), 'precondition: user:* row exists before the refresh');

    // The new region's snapshot already knows about #chat as a real server
    // channel — mergeUserChannels() will name-match it instead of creating
    // a user:#chat row. Drive loadChannels() directly (what the region
    // handler calls first) rather than the full region-change handler: a
    // same-named match isn't `encrypted`, so the handler's own follow-up
    // would fall through to refreshMessages(), which legitimately clears
    // messages when the new region has none over REST — a separate,
    // unrelated behavior this test isn't about.
    h.respondChannels = () => Promise.resolve({ channels: [serverChannel(NAME, { name: NAME })] });
    await h.w._channelsLoadChannelsForTest(true);

    assert.strictEqual(h.row(HASH), undefined, 'the user:* hash must no longer exist (name-matched instead)');
    const remapped = h.row(NAME);
    assert.ok(remapped && remapped.userAdded === true, 'the name-matched row must carry userAdded');
    assert.strictEqual(h.state().selectedHash, NAME, 'selection must remap to the matched row, not close (got ' + h.state().selectedHash + ')');
    assert.strictEqual(h.state().messages.length, 1, 'messages must not be cleared by the remap');
    assert.ok(!h.historyCalls.includes('#/channels'), 'must not route back to #/channels (got ' + JSON.stringify(h.historyCalls) + ')');
    assert.ok(h.historyCalls[h.historyCalls.length - 1].indexOf(encodeURIComponent(NAME)) !== -1, 'URL must be updated to the matched row (got ' + JSON.stringify(h.historyCalls) + ')');
  });

  await test('F3 (approval variant): a PSK is remapped, not closed, when approving a shared channel with the same name', async () => {
    const h = makeHarness();
    const NAME = '#chat';
    const HASH = 'user:' + NAME;
    h.storeKey(NAME, 'bb'.repeat(16), 'Chat');
    h.respondChannels = () => Promise.resolve({ channels: [] });
    await h.init();
    await h.w._channelsSelectChannelForTest(HASH);
    await flush(300);
    h.setState({ messages: [{ sender: 'Alice', text: 'hello', timestamp: '2026-01-01T00:00:00Z', packetHash: 'm1' }] });

    h.respondChannels = () => Promise.resolve({ channels: [], approvedChannels: [{ hash: NAME, name: NAME }] });
    assert.strictEqual(typeof h.approvedCallback, 'function', 'init() must mount ChannelProposals with onApproved');
    h.approvedCallback();
    await flush(300);

    assert.strictEqual(h.row(HASH), undefined, 'the user:* hash must no longer exist (name-matched instead)');
    const remapped = h.row(NAME);
    assert.ok(remapped && remapped.userAdded === true, 'the name-matched row must carry userAdded');
    assert.strictEqual(h.state().selectedHash, NAME, 'selection must remap on approval too (got ' + h.state().selectedHash + ')');
    assert.strictEqual(h.state().messages.length, 1, 'messages must not be cleared by the remap');
  });

  // init() keeps its own mergeUserChannels()/render after loadChannels():
  // a no-op after a successful load, but the only thing that lists My
  // Channels when /channels fails.
  await test('init(): My Channels still listed when /channels fails', async () => {
    const h = makeHarness();
    h.storeKey(PSK_NAME, PSK_KEY, 'Team');
    h.respondChannels = () => Promise.reject(new Error('offline'));
    await h.init();
    assert.ok(h.row(PSK_HASH) && h.row(PSK_HASH).userAdded === true, 'PSK row listed');
    assert.ok(/ch-section-mychannels/.test(h.elements.chList.innerHTML), 'My Channels rendered');
  });

  await test('init(): a successful load lists each PSK row exactly once', async () => {
    const h = makeHarness();
    h.storeKey(PSK_NAME, PSK_KEY, 'Team');
    h.respondChannels = () => Promise.resolve({ channels: [serverChannel('public')] });
    await h.init();
    assert.strictEqual(h.state().channels.filter((c) => c.hash === PSK_HASH).length, 1);
  });

  // ── Nit: a brand-new WS-pushed channel row must be _wsSeq-stamped like an
  // existing row is, or its freshly-live activity loses to a concurrent,
  // older snapshot once mergeClientChannelState() can no longer tell it's
  // newer than the request.
  await test('a live message for a brand-new channel is _wsSeq-stamped and wins over a concurrent older snapshot', async () => {
    const h = makeHarness();
    h.respondChannels = () => Promise.resolve({ channels: [] });
    await h.init();
    const pending = deferred();
    h.respondChannels = () => pending.promise;
    const load = h.w._channelsLoadChannelsForTest(true);
    await flush();
    assert.strictEqual(h.row('brandnew'), undefined, 'precondition: channel does not exist yet');
    h.liveMessage('brandnew', 'Carol', 'hello');
    assert.ok(h.row('brandnew'), 'WS path must create the row immediately');
    pending.resolve({ channels: [serverChannel('brandnew', {
      messageCount: 1,
      lastActivity: new RealDate(RealDate.now() - 100000).toISOString(),
      lastSender: 'Old',
      lastMessage: 'stale snapshot',
    })] });
    await load;
    const row = h.row('brandnew');
    assert.strictEqual(row.lastMessage, 'hello', 'live message must win over an older concurrent snapshot (got ' + row.lastMessage + ')');
    assert.strictEqual(row.lastSender, 'Carol');
    assert.strictEqual(row.messageCount, 1);
  });

  // ── N1 (#153 review round 3, P3): a remap that happens while a decrypt is
  // still in flight must restart loading for the remapped channel, not
  // leave the pane stuck on "Decrypting messages…". The in-flight request's
  // own staleness check (isStaleMessageRequest — selectedHash changed under
  // it) silently discards its result once reconcile remaps the selection,
  // and nothing else used to restart the fetch.
  await test('N1: a remap mid-decrypt restarts loading for the remapped channel', async () => {
    const h = makeHarness();
    const KEY = 'a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1';
    const NAME = '#n1chat';
    const HASH = 'user:' + NAME;
    h.storeKey(NAME, KEY, 'N1 Chat');
    h.respondChannels = () => Promise.resolve({ channels: [] });
    await h.init();

    const msg = encryptChannelMessage(KEY, 'Alice', 'remapped channel message');

    const pending = deferred();
    h.respondPackets = () => pending.promise;
    const select = h.w._channelsSelectChannelForTest(HASH);
    await flush(300);
    assert.ok(/Decrypting/.test(h.elements.chMessages.innerHTML), 'pane must show "Decrypting…" while the fetch is in flight');

    // #n1chat becomes server-known (e.g. approved) in the next refresh —
    // mergeUserChannels() name-matches it instead of keeping a user:* row,
    // so reconcile remaps the open selection to it mid-decrypt.
    h.respondPackets = () => Promise.resolve({ packets: [encryptedPacket('pkt-remap', '2026-01-01T00:00:00Z', msg)] });
    h.respondChannels = () => Promise.resolve({ channels: [serverChannel(NAME, { name: NAME })] });
    await h.w._channelsLoadChannelsForTest(true);
    await flush(300);

    // The original (now-stale) fetch finally resolves too; it must not
    // clobber the remapped channel's freshly-loaded view.
    pending.resolve({ packets: [] });
    await select;
    await flush(300);

    assert.strictEqual(h.state().selectedHash, NAME, 'selection must have remapped to the server-known hash');
    const msgEl = h.elements.chMessages;
    assert.ok(!/Decrypting/.test(msgEl.innerHTML), 'pane must not be stuck on "Decrypting…" (got ' + msgEl.innerHTML.slice(0, 200) + ')');
    const texts = h.state().messages.map((m) => m.text);
    assert.ok(texts.indexOf('remapped channel message') !== -1, 'the remapped channel must actually load its messages (got ' + JSON.stringify(texts) + ')');
  });

  await test('N1: a quiet remap (no decrypt in flight) still leaves messages untouched', async () => {
    // Regression guard for the F3 (round 2) invariant: only restart loading
    // when a decrypt was genuinely pending at remap time.
    const h = makeHarness();
    const NAME = '#n1quiet';
    const HASH = 'user:' + NAME;
    h.storeKey(NAME, 'cc'.repeat(16), 'Quiet');
    h.respondChannels = () => Promise.resolve({ channels: [] });
    await h.init();
    h.setState({ selectedHash: HASH, messages: [{ sender: 'Alice', text: 'already loaded', timestamp: '2026-01-01T00:00:00Z', packetHash: 'm1' }] });

    h.respondChannels = () => Promise.resolve({ channels: [serverChannel(NAME, { name: NAME })] });
    await h.w._channelsLoadChannelsForTest(true);

    assert.strictEqual(h.state().selectedHash, NAME, 'selection must remap');
    assert.strictEqual(h.state().messages.length, 1, 'messages must not be cleared by a quiet remap (got ' + JSON.stringify(h.state().messages) + ')');
  });

  // ── N2 (#153 review round 3, P3): the client-side decrypt cache must be
  // region-scoped. The cache key was the channel name alone, so a cache
  // entry primed under one region answered a fetch for a different region.
  await test('N2: the decrypt cache is region-scoped — a different region never shows another region\'s cached messages', async () => {
    const h = makeHarness();
    const KEY = 'a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2';
    const NAME = 'psk:n2test';
    const HASH = 'user:' + NAME;
    h.storeKey(NAME, KEY, 'N2 Team');
    h.respondChannels = () => Promise.resolve({ channels: [] });
    await h.init();

    const sjcMsg = encryptChannelMessage(KEY, 'Alice', 'sjc message');
    const sfoMsg = encryptChannelMessage(KEY, 'Bob', 'sfo message');

    // Region "All" (no filter): the decrypt fetch sees both packets.
    h.respondPackets = () => Promise.resolve({ packets: [
      encryptedPacket('pkt-sjc', '2026-01-01T00:00:00Z', sjcMsg),
      encryptedPacket('pkt-sfo', '2026-01-01T00:01:00Z', sfoMsg),
    ] });
    await h.w._channelsSelectChannelForTest(HASH);
    await flush(300);
    let texts = h.state().messages.map((m) => m.text);
    assert.ok(texts.indexOf('sjc message') !== -1 && texts.indexOf('sfo message') !== -1 && texts.length === 2,
      'All region must see both messages (got ' + JSON.stringify(texts) + ')');

    // Switch to OAK: no observer there saw either packet (0 candidates).
    h.regionParam = 'OAK';
    h.respondPackets = () => Promise.resolve({ packets: [] });
    await h.w._channelsSelectChannelForTest(HASH);
    await flush(300);
    texts = h.state().messages.map((m) => m.text);
    assert.strictEqual(texts.length, 0, 'OAK region (0 candidates) must show an empty list, not the All-region cache (got ' + JSON.stringify(texts) + ')');
  });

  await test('N2: a same-candidate-count delta fetch in a different region does not show the old region\'s message', async () => {
    const h = makeHarness();
    const KEY = 'b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2';
    const NAME = 'psk:n2delta';
    const HASH = 'user:' + NAME;
    h.storeKey(NAME, KEY, 'N2 Delta');
    h.respondChannels = () => Promise.resolve({ channels: [] });
    await h.init();

    const sjcMsg = encryptChannelMessage(KEY, 'Alice', 'sjc-only message');
    const mryMsg = encryptChannelMessage(KEY, 'Carol', 'mry-only message');

    h.regionParam = 'SJC';
    h.respondPackets = () => Promise.resolve({ packets: [encryptedPacket('pkt-sjc', '2026-01-01T00:00:00Z', sjcMsg)] });
    await h.w._channelsSelectChannelForTest(HASH);
    await flush(300);
    assert.ok(h.state().messages.some((m) => m.text === 'sjc-only message'), 'precondition: SJC message cached');

    // MRY: exactly one candidate too (same count as the SJC cache), but an
    // older timestamp — if the cache weren't region-scoped, the delta path
    // would see "0 new candidates since lastTs" and return SJC's cached
    // message instead of MRY's own.
    h.regionParam = 'MRY';
    h.respondPackets = () => Promise.resolve({ packets: [encryptedPacket('pkt-mry', '2025-01-01T00:00:00Z', mryMsg)] });
    await h.w._channelsSelectChannelForTest(HASH);
    await flush(300);
    const texts = h.state().messages.map((m) => m.text);
    assert.ok(texts.indexOf('mry-only message') !== -1, 'MRY must show its own message (got ' + JSON.stringify(texts) + ')');
    assert.ok(texts.indexOf('sjc-only message') === -1, 'MRY must not show the SJC-region cached message (got ' + JSON.stringify(texts) + ')');
  });

  await test('N2: a stale (superseded) decrypt does not write its evidence into the cache', async () => {
    const h = makeHarness();
    const KEY = 'c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3';
    const NAME = 'psk:n2stale';
    const HASH = 'user:' + NAME;
    h.storeKey(NAME, KEY, 'N2 Stale');
    h.respondChannels = () => Promise.resolve({ channels: [] });
    await h.init();

    const staleMsg = encryptChannelMessage(KEY, 'Alice', 'stale message');
    const realMsg = encryptChannelMessage(KEY, 'Bob', 'real message');

    const pending = deferred();
    h.respondPackets = () => pending.promise; // first decrypt stays in flight
    const select1 = h.w._channelsSelectChannelForTest(HASH);
    await flush(300);

    // Supersede it before it resolves (e.g. a second region switch). Zero
    // candidates — this leg never touches the cache either way, so it can't
    // mask the thing under test.
    h.respondPackets = () => Promise.resolve({ packets: [] });
    await h.w._channelsSelectChannelForTest(HASH);
    await flush(300);

    // The first (now-stale) fetch resolves with one real decryptable
    // candidate. If the isStale guard were missing, this would write
    // { messages: [stale message], count: 1, lastTimestamp: T } to the
    // cache.
    pending.resolve({ packets: [encryptedPacket('pkt-stale', '2020-01-01T00:00:00Z', staleMsg)] });
    await select1;
    await flush(300);

    // A later select sees exactly ONE candidate too (same count a poisoned
    // cache entry would hold) with the SAME timestamp as the stale one —
    // not a different message rendered via onCacheHit, but specifically the
    // delta path's own "0 candidates newer than lastTs" short-circuit
    // (fetchAndDecryptChannel, the `newCandidates.length === 0` branch),
    // which trusts the cache's count/lastTs and returns its cached messages
    // outright. That only reaches the real candidate instead of the stale
    // one if the cache was never poisoned in the first place (cachedCount
    // stays 0, so this fetch takes the full-decrypt path instead of delta).
    h.respondPackets = () => Promise.resolve({ packets: [encryptedPacket('pkt-real', '2020-01-01T00:00:00Z', realMsg)] });
    await h.w._channelsSelectChannelForTest(HASH);
    await flush(300);
    const texts = h.state().messages.map((m) => m.text);
    assert.ok(texts.indexOf('stale message') === -1, 'a stale decrypt must not have poisoned the cache (got ' + JSON.stringify(texts) + ')');
    assert.ok(texts.indexOf('real message') !== -1, 'the real candidate must be decrypted and shown (got ' + JSON.stringify(texts) + ')');
  });

  // ── R4-1 (#153 review round 4, P2): N2 made the decrypt cache keys
  // region-scoped ("<channel>|<regions>"), but removeKey() still only
  // deleted cache["<channel>"], so removing a key left the channel's
  // decrypted plaintext in localStorage under every region it was viewed in.
  await test('R4-1: removeKey drops every region-scoped cache entry, keeping a prefix-sharing channel', async () => {
    const h = makeHarness();
    const NAME = 'psk:r4rm';
    const OTHER = 'psk:r4rmx'; // "psk:r4rm" is a prefix of this name
    h.storeKey(NAME, 'd4'.repeat(16), 'R4 Remove');
    h.storeKey(OTHER, 'e5'.repeat(16), 'R4 Other');
    // Entries of the current key format; the older formats are dropped by
    // the version migration (see the #163 item 2 tests), not by removeKey.
    h.ctx.localStorage.setItem('corescope_channel_cache_v', '3');
    const blob = {};
    blob[NAME + '|'] = cacheEntry('all-regions secret');
    blob[NAME + '|SJC'] = cacheEntry('sjc secret');
    blob[NAME + '|SFO,SJC'] = cacheEntry('sfo-sjc secret');
    blob[OTHER + '|SJC'] = cacheEntry('other channel text');
    h.ctx.localStorage.setItem(DECRYPT_CACHE_KEY, JSON.stringify(blob));

    h.removeKey(NAME);

    const keys = decryptCacheKeys(h);
    assert.deepStrictEqual(keys.filter((k) => k.indexOf(NAME + '|') === 0), [],
      'no cache entry for the removed channel may survive (got ' + JSON.stringify(keys) + ')');
    assert.ok(!/secret/.test(rawDecryptCache(h)), 'no plaintext of the removed channel may stay in localStorage');
    assert.ok(keys.indexOf(OTHER + '|SJC') !== -1, 'a channel whose name merely shares the prefix must keep its cache (got ' + JSON.stringify(keys) + ')');
    assert.strictEqual(h.w.ChannelDecrypt.getCache(OTHER + '|SJC').messages[0].text, 'other channel text');
  });

  await test('R4-1: removing a PSK through the channel list clears the plaintext cached by real decrypts in several regions', async () => {
    const h = makeHarness();
    const KEY = 'f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6';
    const NAME = 'psk:r4ui';
    const HASH = 'user:' + NAME;
    h.storeKey(NAME, KEY, 'R4 UI');
    h.respondChannels = () => Promise.resolve({ channels: [] });
    await h.init();

    const msg = encryptChannelMessage(KEY, 'Alice', 'plaintext-r4ui');
    h.respondPackets = () => Promise.resolve({ packets: [encryptedPacket('pkt-r4ui', '2026-01-01T00:00:00Z', msg)] });
    for (const region of ['SJC', 'SFO,SJC', '']) {
      h.regionParam = region;
      await h.w._channelsSelectChannelForTest(HASH);
      await flush(300);
    }
    assert.strictEqual(decryptCacheKeys(h).filter((k) => k.indexOf(NAME + '|') === 0).length, 3,
      'precondition: three region-scoped entries cached (got ' + JSON.stringify(decryptCacheKeys(h)) + ')');
    assert.ok(/plaintext-r4ui/.test(rawDecryptCache(h)), 'precondition: plaintext is cached');

    h.w._channelsRemoveKeyHandlerForTest(HASH);

    assert.deepStrictEqual(decryptCacheKeys(h).filter((k) => k.indexOf(NAME) === 0), [],
      'Remove must clear every cached region of the channel (got ' + JSON.stringify(decryptCacheKeys(h)) + ')');
    assert.ok(!/plaintext-r4ui/.test(rawDecryptCache(h)), 'no decrypted plaintext may remain in localStorage after Remove');
  });

  // ── R4-2 (#153 review round 4, P3): the decrypt cache was only capped by
  // entry count. One entry can be ~365 KiB (1000 messages) against a
  // ~5.2M-character localStorage quota, so a full cache made storeKey() and
  // the labels fail silently. It is now kept under a fixed character budget,
  // evicting the least recently used entries.
  await test('R4-2: the decrypt cache stays under its 1.5M-character budget, evicting least recently written entries first', async () => {
    const sb = makeDecryptSandbox();
    for (let i = 1; i <= 5; i++) sb.CD.setCache('budget|' + i, bigMessages(400000, 'b' + i), 'ts', 1);
    assert.ok(sb.raw().length <= 1500000, 'cache blob must stay within 1.5M characters (got ' + sb.raw().length + ')');
    assert.deepStrictEqual(sb.keys().sort(), ['budget|3', 'budget|4', 'budget|5'],
      'the oldest entries must be evicted first, the newest kept (got ' + JSON.stringify(sb.keys()) + ')');
    assert.ok(/b5:/.test(sb.CD.getCache('budget|5').messages[0].text), 'the entry just written must be readable');
  });

  await test('R4-2: a recently read entry outlives an older unread one', async () => {
    const sb = makeDecryptSandbox();
    for (let i = 1; i <= 3; i++) sb.CD.setCache('lru|' + i, bigMessages(400000, 'l' + i), 'ts', 1);
    assert.ok(sb.CD.getCache('lru|1'), 'precondition: lru|1 cached');
    sb.CD.setCache('lru|4', bigMessages(400000, 'l4'), 'ts', 1);
    assert.deepStrictEqual(sb.keys().sort(), ['lru|1', 'lru|3', 'lru|4'],
      'lru|2 (oldest use) must go, not the just-read lru|1 (got ' + JSON.stringify(sb.keys()) + ')');
  });

  await test('R4-2: a QuotaExceededError evicts and retries, and an entry that can never fit leaves a valid blob', async () => {
    const sb = makeDecryptSandbox({ quota: 1000000 });
    for (let i = 1; i <= 3; i++) sb.CD.setCache('quota|' + i, bigMessages(300000, 'q' + i), 'ts', 1);
    assert.deepStrictEqual(sb.keys().sort(), ['quota|1', 'quota|2', 'quota|3'], 'precondition: 900K fits the quota');
    sb.CD.setCache('quota|4', bigMessages(300000, 'q4'), 'ts', 1);
    assert.deepStrictEqual(sb.keys().sort(), ['quota|2', 'quota|3', 'quota|4'],
      'a quota failure must evict the oldest entry and retry (got ' + JSON.stringify(sb.keys()) + ')');

    sb.CD.setCache('quota|huge', bigMessages(1100000, 'qh'), 'ts', 1);
    const raw = sb.storage[DECRYPT_CACHE_KEY];
    if (raw !== undefined) assert.doesNotThrow(() => JSON.parse(raw), 'the cache blob must never be left half-written');
    assert.strictEqual(sb.CD.getCache('quota|huge'), null, 'an entry larger than the quota is not cached');
  });

  await test('R4-2: storeKey and labels still save when the decrypt cache has filled the quota', async () => {
    const sb = makeDecryptSandbox();
    for (let i = 1; i <= 3; i++) sb.CD.setCache('full|' + i, bigMessages(300000, 'f' + i), 'ts', 1);
    sb.quota = sb.used() + 10; // localStorage is now full
    const KEY = '0123456789abcdef0123456789abcdef';
    sb.CD.storeKey('psk:quotakey', KEY, 'Quota Label');
    assert.strictEqual(sb.CD.getKeys()['psk:quotakey'], KEY, 'the key must be saved, evicting decrypt cache to make room');
    assert.strictEqual(sb.CD.getLabel('psk:quotakey'), 'Quota Label', 'the label must be saved too');
    sb.CD.saveLabel('psk:quotakey', 'Renamed ' + 'L'.repeat(200));
    assert.strictEqual(sb.CD.getLabel('psk:quotakey'), 'Renamed ' + 'L'.repeat(200), 'a later label change must also make room');
    assert.ok(sb.keys().length < 3, 'room must come from the decrypt cache (got ' + JSON.stringify(sb.keys()) + ')');
  });

  // #163 item 2: cache keys are "<channel>|<regions>", but a channel name may
  // contain '|' itself, so clearing "#a" also cleared "#a|b" and the two
  // channels' entries could be confused. The channel part now has '|' (and
  // '%', which escapes it) percent-encoded, so the first '|' of a key is
  // always the separator.
  await test('#163 item 2: removing channel "#a" leaves the cache of channel "#a|b" intact', async () => {
    const sb = makeDecryptSandbox();
    const CD = sb.CD;
    const keysOf = { a: [CD.channelCacheKey('#a', ''), CD.channelCacheKey('#a', 'SJC')], ab: [CD.channelCacheKey('#a|b', ''), CD.channelCacheKey('#a|b', 'SJC')] };
    keysOf.a.concat(keysOf.ab).forEach((k) => CD.setCache(k, [{ text: 'plain ' + k }], 'ts', 1));
    assert.strictEqual(new Set(keysOf.a.concat(keysOf.ab)).size, 4, 'every (channel, region) pair needs its own key');

    CD.clearChannelCache('#a');
    keysOf.a.forEach((k) => assert.strictEqual(CD.getCache(k), null, '#a\'s entry ' + k + ' must be cleared'));
    keysOf.ab.forEach((k) => assert.ok(CD.getCache(k), '#a|b\'s entry ' + k + ' must survive (left: ' + JSON.stringify(sb.keys()) + ')'));

    CD.clearChannelCache('#a|b');
    assert.deepStrictEqual(sb.keys(), [], 'clearing #a|b removes its own entries (left: ' + JSON.stringify(sb.keys()) + ')');
  });

  await test('#163 item 2: removeKey("#a") clears only that channel\'s decrypt cache', async () => {
    const sb = makeDecryptSandbox();
    const CD = sb.CD;
    CD.storeKey('#a', '0123456789abcdef0123456789abcdef', 'A');
    CD.storeKey('#a|b', 'fedcba9876543210fedcba9876543210', 'AB');
    CD.setCache(CD.channelCacheKey('#a', 'SJC'), [{ text: 'a' }], 'ts', 1);
    CD.setCache(CD.channelCacheKey('#a|b', ''), [{ text: 'ab' }], 'ts', 1);
    CD.removeKey('#a');
    assert.strictEqual(CD.getCache(CD.channelCacheKey('#a', 'SJC')), null, '#a\'s cache goes with its key');
    assert.ok(CD.getCache(CD.channelCacheKey('#a|b', '')), '#a|b\'s cache must stay');
    assert.ok(CD.getKeys()['#a|b'], '#a|b\'s key must stay');
  });

  await test('#163 item 2: the escaping is injective — "a|b" and "a%7Cb" are different channels', async () => {
    const { CD } = makeDecryptSandbox();
    const names = ['a', 'a|b', 'a%7Cb', 'a%', 'a%25', 'a|', '|', '%7C', 'a|b|c'];
    const keys = names.map((n) => CD.channelCacheKey(n, ''));
    assert.strictEqual(new Set(keys).size, names.length, 'distinct channels must have distinct keys (got ' + JSON.stringify(keys) + ')');
    keys.forEach((k, i) => assert.strictEqual(k.indexOf('|'), k.length - 1, 'the only "|" of ' + JSON.stringify(names[i]) + '\'s key is the separator (got ' + k + ')'));
    assert.strictEqual(CD.channelCacheKey('#a', 'SFO,SJC'), CD.channelCacheKey('#a', 'SJC,SFO'), 'region order still does not matter');
    assert.strictEqual(CD.channelCacheKey('psk:r4sort', 'SJC'), 'psk:r4sort|SJC', 'a name without "|" or "%" keeps its key');
  });

  // #163 item 4: cacheMessages()/getCachedMessages() wrote and read bare
  // "<channel>" keys, outside the "<channel>|<regions>" format. Nothing
  // called them; any caller has to go through channelCacheKey().
  await test('#163 item 4: the unused bare-name cacheMessages/getCachedMessages API is gone', async () => {
    const { CD } = makeDecryptSandbox();
    assert.strictEqual(CD.cacheMessages, undefined, 'cacheMessages must not be exported');
    assert.strictEqual(CD.getCachedMessages, undefined, 'getCachedMessages must not be exported');
    assert.strictEqual(typeof CD.channelCacheKey, 'function', 'channelCacheKey stays the way to build a key');
    assert.strictEqual(typeof CD.setCache, 'function');
    assert.strictEqual(typeof CD.getCache, 'function');
  });

  // Every cache blob written before the key format settled is dropped once:
  // pre-#153 entries are keyed by the bare channel name, #153's by the
  // unescaped "<name>|<regions>", and the two can't be told apart for a name
  // containing '|'. It is only a cache, and none of them may keep plaintext.
  for (const marker of [undefined, '2']) {
    await test('#163 item 2: cache entries of the pre-#153 and #153 key formats are dropped once (version marker ' + marker + ')', async () => {
      const old = {};
      old['#oldchan'] = cacheEntry('PLAIN pre-153 bare name');
      old['#a|b'] = cacheEntry('PLAIN pre-153 name with pipe');
      old['psk:legacy|SJC'] = cacheEntry('PLAIN 153 region key');
      old['#a|b|'] = cacheEntry('PLAIN 153 pipe name');
      old['#a|b|SJC'] = cacheEntry('PLAIN 153 pipe name region');
      const storage = { [DECRYPT_CACHE_KEY]: JSON.stringify(old) };
      if (marker !== undefined) storage.corescope_channel_cache_v = marker;
      const sb = makeDecryptSandbox({ storage });
      assert.strictEqual(sb.CD.getCache('psk:legacy|SJC'), null, 'an old-format entry must not be served');
      assert.deepStrictEqual(sb.keys(), [], 'old-format entries must be dropped (got ' + JSON.stringify(sb.keys()) + ')');
      assert.ok(!/PLAIN/.test(sb.raw()), 'no old plaintext may remain');

      // Only once: an entry written after the migration survives a reload.
      sb.CD.setCache(sb.CD.channelCacheKey('written-later', ''), [{ text: 'kept' }], 'ts', 1);
      const reloaded = makeDecryptSandbox({ storage: sb.storage });
      assert.ok(reloaded.CD.getCache(sb.CD.channelCacheKey('written-later', '')), 'the migration must not run again on the next page load');
    });
  }

  // #163 item 5: the cleanup ran lazily, on the first cache read or write,
  // so old-format plaintext stayed in localStorage until the channels page
  // happened to use the cache. It now runs when ChannelDecrypt initialises.
  const OLD_BLOB = () => JSON.stringify({
    '#oldchan': cacheEntry('PLAIN pre-153'),
    'psk:legacy|SJC': cacheEntry('PLAIN 153'),
  });

  for (const marker of [undefined, '2']) {
    await test('#163 item 5: loading the module drops old-format cache entries without any cache use (version marker ' + marker + ')', async () => {
      const storage = { [DECRYPT_CACHE_KEY]: OLD_BLOB() };
      if (marker !== undefined) storage.corescope_channel_cache_v = marker;
      const sb = makeDecryptSandbox({ storage });
      // No getCache()/setCache()/clearChannelCache() call: only the load.
      assert.ok(!/PLAIN/.test(sb.raw()), 'old plaintext must be gone right after load (got ' + sb.raw().slice(0, 100) + ')');
      assert.strictEqual(sb.storage.corescope_channel_cache_v, '3', 'the version marker must be set');
    });
  }

  await test('#163 item 5: a current-format cache survives the load', async () => {
    const key = makeDecryptSandbox().CD.channelCacheKey('psk:keep', 'SJC');
    const sb = makeDecryptSandbox({ storage: { corescope_channel_cache_v: '3', [DECRYPT_CACHE_KEY]: JSON.stringify({ [key]: cacheEntry('KEEP') }) } });
    assert.deepStrictEqual(sb.keys(), [key], 'a version-3 cache must be left alone by the load');
    assert.strictEqual(sb.CD.getCache(key).messages[0].text, 'KEEP');
  });

  await test('#163 item 5: the lazy call stays as a fallback when localStorage was not available at load', async () => {
    const sb = makeDecryptSandbox({ lateStorage: true, storage: { [DECRYPT_CACHE_KEY]: OLD_BLOB() } });
    assert.ok(/PLAIN/.test(sb.raw()), 'precondition: nothing could be cleaned at load');
    sb.attachStorage();
    assert.strictEqual(sb.CD.getCache('psk:legacy|SJC'), null, 'the first cache use must not serve an old-format entry');
    assert.ok(!/PLAIN/.test(sb.raw()), 'the first cache use must drop the old plaintext (got ' + sb.raw().slice(0, 100) + ')');
  });

  // ── R4-3 (#153 review round 4, P3): the N1 "a load is pending" flag was
  // one shared boolean, set only inside decryptAndRender(). A superseded
  // decrypt cleared it in its finally while a newer one was still running
  // (S1), and a remap during selectChannel()'s first await (S2) saw no
  // flag at all. A superseded selectChannel() also still painted
  // "Decrypting messages…" over a newer request's view (S3).
  await test('R4-3 S1: decrypt A, region switch starts decrypt B, A ends, then a remap — the pane must not hang', async () => {
    const h = makeHarness();
    const KEY = 'a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7';
    const NAME = '#r4s1';
    const HASH = 'user:' + NAME;
    h.storeKey(NAME, KEY, 'R4 S1');
    h.respondChannels = () => Promise.resolve({ channels: [] });
    await h.init();
    const msg = encryptChannelMessage(KEY, 'Alice', 's1 remapped message');

    const decryptA = deferred();
    h.respondPackets = () => decryptA.promise;
    const selectA = h.w._channelsSelectChannelForTest(HASH);
    assert.ok(await settle(() => h.packetsRequests.length === 1), 'precondition: decrypt A is in flight');

    const decryptB = deferred();
    h.respondPackets = () => decryptB.promise;
    h.regionParam = 'SJC';
    h.regionChange(); // re-runs selectChannel for the encrypted selection (F1)
    assert.ok(await settle(() => h.packetsRequests.length === 2), 'precondition: decrypt B is in flight (got ' + h.packetsRequests.length + ' fetches)');

    decryptA.resolve({ packets: [] }); // A is superseded and finishes first
    await selectA;
    await flush(300);

    h.respondPackets = () => Promise.resolve({ packets: [encryptedPacket('pkt-s1', '2026-01-01T00:00:00Z', msg)] });
    h.respondChannels = () => Promise.resolve({ channels: [serverChannel(NAME, { name: NAME })] });
    await h.w._channelsLoadChannelsForTest(true);
    await flush(300);
    decryptB.resolve({ packets: [] });
    await settle(() => h.state().messages.length > 0);
    await flush(300);

    assert.strictEqual(h.state().selectedHash, NAME, 'selection must have remapped');
    assert.ok(!/Decrypting/.test(h.elements.chMessages.innerHTML), 'pane must not hang on "Decrypting…" (got ' + h.elements.chMessages.innerHTML.slice(0, 120) + ')');
    const texts = h.state().messages.map((m) => m.text);
    assert.ok(texts.indexOf('s1 remapped message') !== -1, 'the remapped channel must load (got ' + JSON.stringify(texts) + ')');
  });

  await test('R4-3 S2: a remap before computeChannelHash resolves restarts loading', async () => {
    const h = makeHarness();
    const KEY = 'b8b8b8b8b8b8b8b8b8b8b8b8b8b8b8b8';
    const NAME = '#r4s2';
    const HASH = 'user:' + NAME;
    h.storeKey(NAME, KEY, 'R4 S2');
    h.respondChannels = () => Promise.resolve({ channels: [] });
    await h.init();
    const msg = encryptChannelMessage(KEY, 'Bob', 's2 remapped message');
    h.respondPackets = () => Promise.resolve({ packets: [encryptedPacket('pkt-s2', '2026-01-01T00:00:00Z', msg)] });

    const hashReady = deferred();
    h.w.ChannelDecrypt.computeChannelHash = () => hashReady.promise;
    const select = h.w._channelsSelectChannelForTest(HASH);
    await flush(50);
    assert.strictEqual(h.packetsRequests.length, 0, 'precondition: still waiting for computeChannelHash');

    h.respondChannels = () => Promise.resolve({ channels: [serverChannel(NAME, { name: NAME })] });
    await h.w._channelsLoadChannelsForTest(true);
    hashReady.resolve(msg.channelHash);
    await select;
    await flush(300);

    assert.strictEqual(h.state().selectedHash, NAME, 'selection must have remapped');
    assert.ok(!/Decrypting/.test(h.elements.chMessages.innerHTML), 'pane must not hang on "Decrypting…" (got ' + h.elements.chMessages.innerHTML.slice(0, 120) + ')');
    const texts = h.state().messages.map((m) => m.text);
    assert.ok(texts.indexOf('s2 remapped message') !== -1, 'the remapped channel must load (got ' + JSON.stringify(texts) + ')');
  });

  await test('R4-3 S3: a region switch that reveals a same-named channel mid-decrypt is not overwritten by a late "Decrypting…"', async () => {
    const h = makeHarness();
    const KEY = 'c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9';
    const NAME = '#r4s3';
    const HASH = 'user:' + NAME;
    h.storeKey(NAME, KEY, 'R4 S3');
    h.respondChannels = () => Promise.resolve({ channels: [] });
    await h.init();
    const realApi = h.ctx.api;
    h.ctx.api = (p, o) => (p.indexOf('/channels/' + encodeURIComponent(NAME) + '/messages') === 0
      ? Promise.resolve({ messages: [{ id: 1, sender: 'Srv', text: 's3 server message', timestamp: '2026-01-01T00:00:00Z', packetHash: 'srv1' }] })
      : realApi(p, o));

    const hashReady = deferred();
    h.w.ChannelDecrypt.computeChannelHash = () => hashReady.promise;
    const select = h.w._channelsSelectChannelForTest(HASH);
    await flush(50);

    // SJC lists #r4s3 as a server channel: reconcile remaps the selection,
    // and the region handler refreshes it over REST.
    h.regionParam = 'SJC';
    h.respondChannels = () => Promise.resolve({ channels: [serverChannel(NAME, { name: NAME })] });
    h.regionChange();
    await flush(300);
    assert.strictEqual(h.state().selectedHash, NAME, 'precondition: selection remapped');
    assert.ok(h.state().messages.some((m) => m.text === 's3 server message'), 'precondition: REST refresh rendered the server channel');

    hashReady.resolve(encryptChannelMessage(KEY, 'x', 'y').channelHash);
    await select;
    await flush(300);
    assert.ok(!/Decrypting/.test(h.elements.chMessages.innerHTML),
      'a superseded selectChannel must not paint "Decrypting…" over the current view (got ' + h.elements.chMessages.innerHTML.slice(0, 120) + ')');
    assert.ok(h.state().messages.some((m) => m.text === 's3 server message'), 'the current view must keep its messages');
  });

  // ── #163 item 1: selectChannel() walks every stored key for an encrypted
  // channel, awaiting computeChannelHash once per key, and then paints the
  // #781 "no decryption key" lock text. Nothing checked whether the request
  // was still current, so the lock text landed on top of a newer request's
  // view — the same late-write pattern R4-3 S3 fixed for "Decrypting…".
  const LOCK_TEXT = /no decryption key is configured/;
  const msgBelongsTo = (h, text) => h.state().messages.some((m) => m.text === text);

  async function encryptedChannelHarness(storedKeys) {
    const h = makeHarness();
    for (let i = 0; i < storedKeys; i++) h.storeKey('psk:i163k' + i, (i + 1).toString(16).repeat(32), 'K' + i);
    h.respondChannels = () => Promise.resolve({ channels: [serverChannel('public')] });
    await h.init();
    h.setState({
      channels: [
        Object.assign({}, h.row('public')),
        serverChannel('42', { encrypted: true, name: 'Encrypted 42' }),
      ],
    });
    const realApi = h.ctx.api;
    h.ctx.api = (p, o) => (p.indexOf('/channels/public/messages') === 0
      ? Promise.resolve({ messages: [{ id: 1, sender: 'Srv', text: 'i163 public message', timestamp: '2026-01-01T00:00:00Z', packetHash: 'srv-public' }] })
      : realApi(p, o));
    // computeChannelHash is slow: each stored key's call stays pending
    // until the test releases it, and none of them matches channel 42.
    h.hashCalls = [];
    h.w.ChannelDecrypt.computeChannelHash = () => { const d = deferred(); h.hashCalls.push(d); return d.promise; };
    return h;
  }

  // Releases A's computeChannelHash calls as they appear (none matches),
  // until A's selectChannel() has returned.
  async function releaseKeyLoop(h, selection) {
    let done = false;
    selection.then(() => { done = true; }, () => { done = true; });
    let released = 0;
    for (let i = 0; i < 200 && !done; i++) {
      while (released < h.hashCalls.length) h.hashCalls[released++].resolve(250);
      await flush(5);
    }
    assert.ok(done, 'the superseded selectChannel() must return');
    await flush(100);
  }

  await test('#163 item 1: the lock text of a superseded encrypted selection does not overwrite the newer channel\'s view', async () => {
    const h = await encryptedChannelHarness(3);
    const selectA = h.w._channelsSelectChannelForTest('42');
    assert.ok(await settle(() => h.hashCalls.length === 1), 'precondition: A is awaiting its first computeChannelHash');

    await h.w._channelsSelectChannelForTest('public');
    await flush(50);
    assert.strictEqual(h.state().selectedHash, 'public', 'precondition: B is selected');
    assert.ok(msgBelongsTo(h, 'i163 public message'), 'precondition: B\'s messages are rendered');
    assert.ok(!LOCK_TEXT.test(h.elements.chMessages.innerHTML), 'precondition: no lock text yet');

    await releaseKeyLoop(h, selectA);

    assert.ok(!LOCK_TEXT.test(h.elements.chMessages.innerHTML),
      'the lock text of the superseded request must not be painted over B (got ' + h.elements.chMessages.innerHTML.slice(0, 160) + ')');
    assert.strictEqual(h.state().selectedHash, 'public', 'B must stay selected');
    assert.ok(msgBelongsTo(h, 'i163 public message'), 'B\'s messages must be intact');
  });

  await test('#163 item 1: a superseded encrypted selection stops walking the stored keys', async () => {
    const h = await encryptedChannelHarness(4);
    // Only the first call is slow; any further call is counted and answered.
    let calls = 0;
    const first = deferred();
    h.w.ChannelDecrypt.computeChannelHash = () => (++calls === 1 ? first.promise : Promise.resolve(250));
    const selectA = h.w._channelsSelectChannelForTest('42');
    assert.ok(await settle(() => calls === 1), 'precondition: A awaits the first key');
    await h.w._channelsSelectChannelForTest('public');
    first.resolve(250);
    await selectA;
    await flush(100);
    assert.strictEqual(calls, 1, 'A must not hash the remaining keys once it is superseded (got ' + calls + ' calls)');
  });

  await test('#163 item 1: a region change while the key loop runs keeps the lock text off the pane', async () => {
    const h = await encryptedChannelHarness(2);
    const selectA = h.w._channelsSelectChannelForTest('42');
    assert.ok(await settle(() => h.hashCalls.length === 1), 'precondition: A awaits the first key');
    // The region changes under A, which makes A stale; the pane now shows
    // something else.
    h.regionParam = 'SJC';
    h.elements.chMessages.innerHTML = '<div class="i163-region-view">region view</div>';
    await releaseKeyLoop(h, selectA);
    assert.ok(/i163-region-view/.test(h.elements.chMessages.innerHTML),
      'a request superseded by a region change must not touch the pane (got ' + h.elements.chMessages.innerHTML.slice(0, 160) + ')');
  });

  await test('#163 item 1: a failed encrypted-ness lookup of a superseded deep link does not paint "Loading messages…" over the newer view', async () => {
    const h = await encryptedChannelHarness(0);
    // A deep link to a '#'-named channel that is not in the loaded list
    // starts a /channels?includeEncrypted lookup; it fails after the user
    // has moved on to another channel.
    let failLookup;
    const lookup = new Promise((_resolve, reject) => { failLookup = reject; });
    h.respondChannels = () => lookup;
    const selectA = h.w._channelsSelectChannelForTest('#i163deep');
    assert.ok(await settle(() => h.channelRequests.some((p) => p.indexOf('includeEncrypted=true') !== -1)), 'precondition: the lookup is in flight');

    await h.w._channelsSelectChannelForTest('public');
    await flush(50);
    assert.ok(msgBelongsTo(h, 'i163 public message'), 'precondition: B\'s messages are rendered');

    h.elements.chMessages.innerHTML = '<div class="i163-b-view">B view</div>';
    failLookup(new Error('lookup failed'));
    await selectA;
    await flush(100);
    assert.ok(/i163-b-view/.test(h.elements.chMessages.innerHTML),
      'a superseded request must not paint over B after its lookup fails (got ' + h.elements.chMessages.innerHTML.slice(0, 160) + ')');
  });

  // ── R4-4 (#153 review round 4, P3): when the current region's fetch finds
  // zero candidates, N2 rendered an empty pane but left that region's cache
  // entry in place, so the next visit flashed the outdated history (via
  // onCacheHit) before its own fetch answered.
  await test('R4-4: zero candidates in the same region drop that region\'s cache entry (a stale request leaves it)', async () => {
    const h = makeHarness();
    const KEY = 'dadadadadadadadadadadadadadadada';
    const NAME = 'psk:r4zero';
    const HASH = 'user:' + NAME;
    h.storeKey(NAME, KEY, 'R4 Zero');
    h.respondChannels = () => Promise.resolve({ channels: [] });
    await h.init();
    h.regionParam = 'SJC';
    const msg = encryptChannelMessage(KEY, 'Alice', 'aged-out message');

    const withMessage = () => Promise.resolve({ packets: [encryptedPacket('pkt-zero', '2026-01-01T00:00:00Z', msg)] });
    h.respondPackets = withMessage;
    await h.w._channelsSelectChannelForTest(HASH);
    assert.ok(h.w.ChannelDecrypt.getCache(NAME + '|SJC'), 'precondition: SJC entry cached');

    // A superseded request answering "zero candidates" must not drop the
    // entry the current request owns.
    const staleFetch = deferred();
    h.respondPackets = () => staleFetch.promise;
    const staleSelect = h.w._channelsSelectChannelForTest(HASH);
    assert.ok(await settle(() => h.packetsRequests.length === 2), 'precondition: the soon-stale fetch is in flight');
    h.respondPackets = withMessage;
    await h.w._channelsSelectChannelForTest(HASH);
    staleFetch.resolve({ packets: [] });
    await staleSelect;
    await flush(300);
    assert.ok(h.w.ChannelDecrypt.getCache(NAME + '|SJC'), 'a stale zero-candidate response must leave the cache alone');

    // The current request finds nothing any more (the packets aged out).
    h.respondPackets = () => Promise.resolve({ packets: [] });
    await h.w._channelsSelectChannelForTest(HASH);
    await flush(300);
    assert.strictEqual(h.state().messages.length, 0, 'zero candidates render an empty pane (got ' + JSON.stringify(h.state().messages.map((m) => m.text)) + ')');
    assert.strictEqual(h.w.ChannelDecrypt.getCache(NAME + '|SJC'), null, 'the region\'s cache entry must be dropped');

    // Next visit: nothing outdated may flash while the fetch is in flight.
    const nextFetch = deferred();
    h.respondPackets = () => nextFetch.promise;
    const nextSelect = h.w._channelsSelectChannelForTest(HASH);
    await flush(300);
    assert.ok(!/aged-out message/.test(h.elements.chMessages.innerHTML) && h.state().messages.length === 0,
      'the next visit must not flash the outdated history (got ' + JSON.stringify(h.state().messages.map((m) => m.text)) + ')');
    nextFetch.resolve({ packets: [] });
    await nextSelect;
  });

  // ── R4-5 (#153 review round 4, nits).
  await test('R4-5: region order does not change the decrypt cache key (SJC,SFO and SFO,SJC share one entry)', async () => {
    const h = makeHarness();
    const KEY = 'ebebebebebebebebebebebebebebebeb';
    const NAME = 'psk:r4sort';
    const HASH = 'user:' + NAME;
    h.storeKey(NAME, KEY, 'R4 Sort');
    h.respondChannels = () => Promise.resolve({ channels: [] });
    await h.init();
    const msg = encryptChannelMessage(KEY, 'Alice', 'sorted message');
    h.respondPackets = () => Promise.resolve({ packets: [encryptedPacket('pkt-sort', '2026-01-01T00:00:00Z', msg)] });
    for (const region of ['SJC,SFO', 'SFO,SJC']) {
      h.regionParam = region;
      await h.w._channelsSelectChannelForTest(HASH);
    }
    assert.deepStrictEqual(decryptCacheKeys(h).filter((k) => k.indexOf(NAME + '|') === 0), [NAME + '|SFO,SJC'],
      'both orders must map to the one sorted key (got ' + JSON.stringify(decryptCacheKeys(h)) + ')');
  });

  // The decrypt E2E's process helpers: a child killed by a signal has
  // exitCode null and signalCode set, so checking exitCode alone missed a
  // SIGKILL/SIGSEGV — waitFor() polled out its whole timeout and
  // stopProcess() waited forever for an 'exit' that had already fired.
  const e2eHarness = require('./test-channels-client-state-152-decrypt-e2e.js');
  const os = require('os');
  function spawnSleeper(dir) {
    return e2eHarness.startProcess('sleeper', process.execPath, ['-e', 'setTimeout(() => {}, 60000)'], dir);
  }
  function within(ms, p, what) {
    let t;
    return Promise.race([p, new Promise((_, rej) => { t = setTimeout(() => rej(new Error(what + ' did not settle within ' + ms + 'ms')), ms); })])
      .finally(() => clearTimeout(t));
  }
  async function killedBySignal(dir, sig) {
    const proc = spawnSleeper(dir);
    const exited = new Promise((r) => proc.once('exit', r));
    proc.kill(sig);
    await exited;
    assert.strictEqual(proc.exitCode, null, 'precondition: a signal-killed child has no exit code');
    assert.strictEqual(proc.signalCode, sig, 'precondition: signalCode is set');
    return proc;
  }

  await test('R4-5: the decrypt E2E\'s waitFor() stops at once when its process dies from SIGKILL or SIGSEGV', async () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'r4-waitfor-'));
    try {
      for (const sig of ['SIGKILL', 'SIGSEGV']) {
        const proc = await killedBySignal(dir, sig);
        const started = RealDate.now();
        await assert.rejects(within(3000, e2eHarness.waitFor('never', () => false, 10000, proc), 'waitFor'),
          (e) => /exited early/.test(e.message) && e.message.indexOf(sig) !== -1,
          'waitFor must reject naming ' + sig + ' instead of polling out its timeout');
        assert.ok(RealDate.now() - started < 1000, 'waitFor must notice the dead process at once');
      }
    } finally { fs.rmSync(dir, { recursive: true, force: true }); }
  });

  await test('R4-5: the decrypt E2E\'s stopProcess() returns at once for a process a signal already killed', async () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'r4-stop-'));
    try {
      const proc = await killedBySignal(dir, 'SIGKILL');
      await within(3000, e2eHarness.stopProcess(proc), 'stopProcess');
    } finally { fs.rmSync(dir, { recursive: true, force: true }); }
  });

  // ── #154: overlapping loadChannels() — only the newest request renders ──
  console.log('\n=== #154 overlapping loadChannels(): the newest request wins ===');

  // A /channels responder that parks every request on its own deferred,
  // keyed by request order, so a test decides which response lands first.
  function parkChannelRequests(h) {
    const parked = [];
    h.respondChannels = (p) => { const d = deferred(); parked.push({ path: p, d }); return d.promise; };
    return parked;
  }
  function listedHashes(h) { return h.state().channels.map((c) => c.hash); }

  await test('#154: an older region response that lands last does not replace the newer region\'s list', async () => {
    const h = makeHarness();
    h.respondChannels = () => Promise.resolve({ channels: [serverChannel('public')] });
    await h.init();
    const before = h.channelRequests.length;
    const parked = parkChannelRequests(h);
    h.regionParam = 'SJC';
    h.regionChange();
    h.regionParam = 'SFO';
    h.regionChange();
    await flush();
    assert.strictEqual(h.channelRequests.length - before, 2, 'one /channels request per region change (got ' + (h.channelRequests.length - before) + ')');
    assert.ok(/region=SJC/.test(parked[0].path) && /region=SFO/.test(parked[1].path), 'requests are SJC then SFO');
    parked[1].d.resolve({ channels: [serverChannel('#sfo')] });
    await flush();
    assert.deepStrictEqual(listedHashes(h), ['#sfo'], 'the SFO list renders');
    parked[0].d.resolve({ channels: [serverChannel('#sjc')] });
    await flush();
    assert.deepStrictEqual(listedHashes(h), ['#sfo'], 'the late SJC response must not replace the SFO list (got ' + JSON.stringify(listedHashes(h)) + ')');
    assert.ok(/#sfo/.test(h.elements.chList.innerHTML) && !/#sjc/.test(h.elements.chList.innerHTML), 'the rendered list stays SFO');
    assert.strictEqual(h.channelRequests.length - before, 2, 'no extra /channels request');
  });

  await test('#154: a dropped older response does not reconcile (close) the selection the newer list contains', async () => {
    const h = makeHarness();
    h.respondChannels = () => Promise.resolve({ channels: [serverChannel('public')] });
    await h.init();
    const parked = parkChannelRequests(h);
    const older = h.w._channelsLoadChannelsForTest(true);
    const newer = h.w._channelsLoadChannelsForTest(true);
    parked[1].d.resolve({ channels: [serverChannel('#sfo')] });
    await newer;
    const msgs = [{ sender: 'A', text: 'kept', timestamp: '2026-01-01T00:00:00Z', packetHash: 'k1' }];
    h.setState({ selectedHash: '#sfo', messages: msgs });
    h.historyCalls.length = 0;
    parked[0].d.resolve({ channels: [serverChannel('#sjc')] });
    await older;
    const s = h.state();
    assert.strictEqual(s.selectedHash, '#sfo', 'selection must survive the dropped response (got ' + s.selectedHash + ')');
    assert.strictEqual(s.messages.length, 1, 'messages must survive the dropped response');
    assert.ok(!h.historyCalls.includes('#/channels'), 'URL must not be rewritten (got ' + JSON.stringify(h.historyCalls) + ')');
  });

  await test('#154: a dropped older failure does not paint "Failed to load channels" over the newer list', async () => {
    const h = makeHarness();
    h.respondChannels = () => Promise.resolve({ channels: [serverChannel('public')] });
    await h.init();
    const parked = parkChannelRequests(h);
    const older = h.w._channelsLoadChannelsForTest(false);
    const newer = h.w._channelsLoadChannelsForTest(true);
    parked[1].d.resolve({ channels: [serverChannel('#sfo')] });
    await newer;
    parked[0].d.promise.catch(() => {});
    parked[0].d.reject(new Error('offline'));
    await older;
    assert.ok(!/Failed to load channels/.test(h.elements.chList.innerHTML), 'stale failure must not replace the list');
    assert.ok(/#sfo/.test(h.elements.chList.innerHTML), 'the newer list stays rendered');
  });

  await test('#154: a dropped call resolves only after the newest list rendered (init deep link opens with the right row)', async () => {
    const h = makeHarness();
    const parked = parkChannelRequests(h);
    const page = h.elements.page || (h.elements.page = h.ctx.document.getElementById('page'));
    const initDone = h.page.init(page, 'sfo-room');
    await flush();
    h.regionParam = 'SFO';
    h.regionChange();
    await flush();
    // The first (unfiltered) response lands first, but a newer request is
    // already running, so it is dropped; the deep link must wait for the
    // SFO list, which is the one that has the room.
    parked[0].d.resolve({ channels: [serverChannel('public')] });
    await flush();
    parked[1].d.resolve({ channels: [serverChannel('sfo-room', { messageCount: 7 })] });
    await initDone;
    await flush();
    const header = h.elements.chHeader.querySelector('.ch-header-text').textContent;
    assert.strictEqual(h.state().selectedHash, 'sfo-room', 'deep link selected');
    assert.strictEqual(header, 'sfo-room — 7 messages', 'deep link opened against the SFO list (got ' + JSON.stringify(header) + ')');
    assert.deepStrictEqual(listedHashes(h), ['sfo-room'], 'the SFO list renders');
  });

  await test('#154: the #152 client-state merge still applies to the request that renders (WS update during it wins)', async () => {
    const h = makeHarness();
    h.respondChannels = () => Promise.resolve({ channels: [serverChannel('public', { messageCount: 10, lastMessage: 'old snapshot' })] });
    await h.init();
    h.setState({ channels: [Object.assign({}, h.row('public'), { unread: 2 })] });
    const parked = parkChannelRequests(h);
    const older = h.w._channelsLoadChannelsForTest(true);
    const newer = h.w._channelsLoadChannelsForTest(true);
    await flush();
    h.liveMessage('public', 'Carol', 'live during flight');
    const snapshot = { channels: [serverChannel('public', { messageCount: 10, lastSender: 'Server', lastMessage: 'snapshot' })] };
    parked[1].d.resolve(snapshot);
    await newer;
    parked[0].d.resolve(snapshot);
    await older;
    const row = h.row('public');
    assert.strictEqual(row.lastMessage, 'live during flight', 'live message kept (got ' + row.lastMessage + ')');
    assert.strictEqual(row.messageCount, 11, 'live count kept with its message');
    assert.strictEqual(row.unread, 2, 'unread carried');
  });

  // ── #155: mobile channel rows show the unread badge ──
  console.log('\n=== #155 mobile rows show the unread badge ===');

  // The rendered #1367 mobile row for `hash` (one <button class="ch-row">).
  function mobileRow(h, hash) {
    const html = h.elements.chList.innerHTML;
    const rows = html.split('<button type="button" class="ch-row').slice(1);
    return rows.find((r) => r.indexOf('data-hash="' + hash + '"') !== -1) || null;
  }

  await test('#155: a mobile row shows the unread badge (count, 99+ cap, title, aria-label); none at 0', async () => {
    const h = makeHarness({ mobile: true });
    h.respondChannels = () => Promise.resolve({ channels: [serverChannel('a'), serverChannel('b'), serverChannel('c')] });
    await h.init();
    h.setState({ channels: h.state().channels.map((c) => Object.assign({}, c, { unread: { a: 3, b: 150, c: 0 }[c.hash] })) });
    await h.w._channelsLoadChannelsForTest(true);
    const a = mobileRow(h, 'a'), b = mobileRow(h, 'b'), c = mobileRow(h, 'c');
    assert.ok(a && b && c, 'mobile rows rendered (got ' + h.elements.chList.innerHTML.slice(0, 120) + ')');
    assert.ok(a.indexOf('<span class="ch-unread-badge" data-unread-channel="a" title="3 new" aria-label="3 unread">3</span>') !== -1, 'badge "3" on row a (got ' + a + ')');
    assert.ok(b.indexOf('<span class="ch-unread-badge" data-unread-channel="b" title="150 new" aria-label="150 unread">99+</span>') !== -1, 'badge capped at 99+ on row b');
    assert.ok(c.indexOf('ch-unread-badge') === -1, 'no badge at unread 0');
    assert.ok(/<div class="ch-row-line1">.*ch-row-time.*ch-unread-badge.*<\/div>/.test(a), 'badge sits in line 1 after the time');
  });

  await test('#155: the desktop badge markup is unchanged', async () => {
    const h = makeHarness();
    await h.init();
    const row = (n) => h.w._channelsRenderChannelRowForTest(Object.assign(serverChannel('a'), { unread: n }));
    assert.ok(row(3).indexOf('<span class="ch-item-name">a</span> <span class="ch-unread-badge" data-unread-channel="a" title="3 new" aria-label="3 unread">3</span>') !== -1, 'desktop badge "3"');
    assert.ok(row(150).indexOf(' <span class="ch-unread-badge" data-unread-channel="a" title="150 new" aria-label="150 unread">99+</span>') !== -1, 'desktop badge 99+');
    assert.ok(row(0).indexOf('ch-unread-badge') === -1, 'no desktop badge at 0');
  });

  await test('#155: on mobile a live-decrypted message for a closed channel adds and bumps the badge; opening clears it', async () => {
    const h = makeHarness({ mobile: true });
    h.storeKey(PSK_NAME, PSK_KEY, 'Team');
    h.respondChannels = () => Promise.resolve({ channels: [serverChannel('public')] });
    await h.init();
    assert.strictEqual(typeof h.wsHandler, 'function', 'init() registers the live WS handler');
    h.setState({ selectedHash: 'public' });
    let n = 0;
    const live = (text) => {
      const enc = encryptChannelMessage(PSK_KEY, 'Bob', text);
      h.wsHandler([{ type: 'packet', data: { id: ++n, hash: 'live-' + n, decoded: { header: { payloadTypeName: 'GRP_TXT' }, payload: { type: 'GRP_TXT', channelHash: enc.channelHash, encryptedData: enc.encryptedData, mac: enc.mac } } } }]);
    };
    assert.ok(mobileRow(h, PSK_HASH) && mobileRow(h, PSK_HASH).indexOf('ch-unread-badge') === -1, 'no badge before live traffic');
    live('one');
    assert.ok(await settle(() => (h.row(PSK_HASH).unread || 0) === 1), 'unread bumped to 1');
    assert.ok(mobileRow(h, PSK_HASH).indexOf('aria-label="1 unread">1</span>') !== -1, 'mobile badge shows 1 (got ' + mobileRow(h, PSK_HASH) + ')');
    live('two');
    assert.ok(await settle(() => (h.row(PSK_HASH).unread || 0) === 2), 'unread bumped to 2');
    assert.ok(mobileRow(h, PSK_HASH).indexOf('aria-label="2 unread">2</span>') !== -1, 'mobile badge updates to 2');
    await h.w._channelsSelectChannelForTest(PSK_HASH);
    assert.strictEqual(h.row(PSK_HASH).unread, 0, 'opening clears unread');
    assert.ok(mobileRow(h, PSK_HASH).indexOf('ch-unread-badge') === -1, 'opening clears the mobile badge');
  });

  console.log('\n=== Results: ' + passed + ' passed, ' + failed + ' failed ===');
  process.exit(failed > 0 ? 1 : 0);
})().catch((e) => { console.error('FATAL:', e); process.exit(1); });
