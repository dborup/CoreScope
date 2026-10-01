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
  let resolve;
  const promise = new Promise((r) => { resolve = r; });
  return { promise, resolve };
}

async function flush(n) {
  for (let i = 0; i < (n || 20); i++) await new Promise((r) => setImmediate(r));
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
      querySelector() { return makeFakeEl(); },
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
      matchMedia: () => ({ matches: false, addEventListener() {}, removeEventListener() {} }),
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
  ctx.debouncedOnWS = (fn) => fn;
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

  console.log('\n=== Results: ' + passed + ' passed, ' + failed + ' failed ===');
  process.exit(failed > 0 ? 1 : 0);
})().catch((e) => { console.error('FATAL:', e); process.exit(1); });
