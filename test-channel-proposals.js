#!/usr/bin/env node
/* Unit tests for public/channel-proposals.js — the real production file,
 * loaded into a vm context (no copies of the code under test). */
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');
const vm = require('vm');

const SRC_PATH = path.join(__dirname, 'public', 'channel-proposals.js');
const SRC = fs.readFileSync(SRC_PATH, 'utf8');

function load() {
  const ctx = { console, Promise, Map, Set, Math, JSON, Object, Array, String, encodeURIComponent };
  ctx.window = ctx;
  ctx.setTimeout = setTimeout;
  ctx.clearTimeout = clearTimeout;
  vm.createContext(ctx);
  vm.runInContext(SRC, ctx, { filename: SRC_PATH });
  return ctx.ChannelProposals;
}

const CP = load();
let passed = 0;
const tests = [];
function test(name, fn) { tests.push({ name, fn }); }

// ── normalizeName mirrors internal/channelregistry.NormalizeName ─────────
test('normalizeName accepts and preserves case', () => {
  const ok = {
    '#MeshCore': '#MeshCore',
    'MeshCore': '#MeshCore',
    '  #wardriving  ': '#wardriving',
    '#Test': '#Test',
    '#test': '#test',
    '#København': '#København',
    '#my channel': '#my channel',
    '##double': '##double',
    '#\u{1F468}\u200D\u{1F469}\u200D\u{1F467}': '#\u{1F468}\u200D\u{1F469}\u200D\u{1F467}',
  };
  ok['#' + 'a'.repeat(30)] = '#' + 'a'.repeat(30); // exactly 31 bytes
  for (const [input, want] of Object.entries(ok)) {
    const r = CP.normalizeName(input);
    assert.strictEqual(r.error, undefined, `${JSON.stringify(input)}: ${r.error}`);
    assert.strictEqual(r.name, want, JSON.stringify(input));
  }
});

test('normalizeName rejects what the firmware cannot hold or would mislead', () => {
  const bad = [
    '', '   ', '#',
    '#' + 'a'.repeat(31),            // 32 bytes
    '#' + 'æ'.repeat(16),       // 16 characters but 33 bytes
    '#0123456789abcdef0123456789abcdef', // a pasted PSK
    '#bad\u0000name', '#tab\tname', '#rtl\u202Eevil', '#iso\u2066late',
    '# leading', '#\u00A0leading-nbsp',
    '#lone\uD800surrogate',
  ];
  for (const input of bad) {
    const r = CP.normalizeName(input);
    assert.ok(r.error && !r.name, `${JSON.stringify(input)} should be rejected, got ${JSON.stringify(r)}`);
  }
});

// ── mergeApprovedChannels ────────────────────────────────────────────────
function deepFreeze(o) {
  Object.freeze(o);
  for (const v of Object.values(o)) if (v && typeof v === 'object' && !Object.isFrozen(v)) deepFreeze(v);
  return o;
}

test('merge marks existing channels shared, appends quiet ones, never mutates input', () => {
  const channels = deepFreeze([
    { hash: '#busy', name: '#busy', messageCount: 5, lastActivityMs: 10 },
    { hash: '#other', name: '#other', messageCount: 1, lastActivityMs: 5 },
  ]);
  const approved = deepFreeze([
    { name: '#busy', hash: '#busy' },
    { name: '#Quiet', hash: '#Quiet' },
    { name: '#Quiet', hash: '#Quiet' }, // duplicate from the server is ignored
  ]);
  const before = JSON.stringify({ channels, approved });
  const out = CP.mergeApprovedChannels(channels, approved);
  assert.strictEqual(JSON.stringify({ channels, approved }), before, 'inputs changed');
  assert.notStrictEqual(out, channels, 'must return a new array');
  assert.strictEqual(out.length, 3);
  assert.strictEqual(out[0].shared, true);
  assert.strictEqual(out[0].messageCount, 5, 'existing data kept');
  assert.notStrictEqual(out[0], channels[0], 'shared entry is a copy');
  assert.strictEqual(out[1], channels[1], 'untouched entries are reused');
  assert.strictEqual(out[1].shared, undefined);
  assert.deepStrictEqual(JSON.parse(JSON.stringify(out[2])), {
    hash: '#Quiet', name: '#Quiet', messageCount: 0, lastActivity: null,
    lastActivityMs: 0, lastMessage: null, lastSender: null, shared: true,
  });
});

test('merge is byte-exact: #Test and #test are different channels', () => {
  const out = CP.mergeApprovedChannels([{ hash: '#test', name: '#test' }], [{ name: '#Test', hash: '#Test' }]);
  assert.strictEqual(out.length, 2);
  assert.strictEqual(out[0].shared, undefined);
  assert.strictEqual(out[1].hash, '#Test');
});

test('merge handles missing or empty approvedChannels', () => {
  const channels = [{ hash: '#a', name: '#a' }];
  for (const approved of [undefined, null, [], {}, [null, { name: 'x' }]]) {
    const out = CP.mergeApprovedChannels(channels, approved);
    assert.strictEqual(out.length, 1);
    assert.strictEqual(out[0], channels[0]);
  }
  const empty = CP.mergeApprovedChannels(undefined, undefined);
  assert.ok(Array.isArray(empty) && empty.length === 0, 'no input yields an empty list');
});

test('merge is O(n+m): 30K channels x 128 approved in well under 50ms', () => {
  const channels = [];
  for (let i = 0; i < 30000; i++) channels.push({ hash: '#c' + i, name: '#c' + i, messageCount: i });
  const approved = [];
  for (let i = 0; i < 128; i++) approved.push({ name: '#c' + (i * 200), hash: '#c' + (i * 200) });
  for (let i = 0; i < 128; i++) approved.push({ name: '#new' + i, hash: '#new' + i });
  CP.mergeApprovedChannels(channels, approved); // warm up
  const t0 = process.hrtime.bigint();
  const out = CP.mergeApprovedChannels(channels, approved);
  const ms = Number(process.hrtime.bigint() - t0) / 1e6;
  assert.strictEqual(out.length, 30128);
  assert.strictEqual(out.filter((c) => c.shared).length, 256);
  assert.ok(ms < 50, `merge took ${ms.toFixed(1)}ms`);
  console.log(`    merge 30000 + 256 approved: ${ms.toFixed(2)}ms`);
});

// ── polling ──────────────────────────────────────────────────────────────
test('pollDelay backs off and is capped at 10s', () => {
  const seq = [0, 1, 2, 3, 4, 5, 6, 7, 20].map(CP.pollDelay);
  assert.deepStrictEqual(seq, [1000, 1500, 2250, 3375, 5063, 7594, 10000, 10000, 10000]);
});

function fakeTimers() {
  const pending = new Map();
  let next = 1;
  return {
    setTimeout(fn, ms) { const id = next++; pending.set(id, { fn, ms }); return id; },
    clearTimeout(id) { pending.delete(id); },
    delays: [],
    async fireNext() {
      const [id, t] = pending.entries().next().value || [];
      if (!id) return false;
      pending.delete(id);
      this.delays.push(t.ms);
      t.fn();
      await new Promise((r) => setImmediate(r));
      return true;
    },
    size() { return pending.size; },
  };
}

test('poller polls only while queued, then stops', async () => {
  const timers = fakeTimers();
  const answers = [{ status: 'queued' }, { status: 'queued' }, { status: 'pending', proposal: { name: '#x' } }];
  const updates = [];
  const p = CP.createPoller({
    fetchStatus: (id) => { assert.strictEqual(id, 'req1'); return Promise.resolve(answers.shift()); },
    onUpdate: (st) => updates.push(st.status),
    setTimeout: timers.setTimeout, clearTimeout: timers.clearTimeout,
  });
  p.start('req1');
  while (await timers.fireNext()) { /* drain */ }
  assert.deepStrictEqual(updates, ['queued', 'queued', 'pending']);
  assert.deepStrictEqual(timers.delays, [1000, 1500, 2250]);
  assert.strictEqual(p.isActive(), false);
});

test('starting a new request cancels the old one; cancel stops polling', async () => {
  const timers = fakeTimers();
  const seen = [];
  const p = CP.createPoller({
    fetchStatus: (id) => Promise.resolve({ status: 'queued', id }),
    onUpdate: (st) => seen.push(st.id),
    setTimeout: timers.setTimeout, clearTimeout: timers.clearTimeout,
  });
  p.start('old');
  p.start('new');
  assert.strictEqual(timers.size(), 1, 'only the active request is scheduled');
  await timers.fireNext();
  assert.deepStrictEqual(seen, ['new']);
  p.cancel();
  assert.strictEqual(timers.size(), 0);
  assert.strictEqual(p.isActive(), false);
});

test('a result arriving after cancel is ignored', async () => {
  const timers = fakeTimers();
  let resolveFetch;
  const seen = [];
  const p = CP.createPoller({
    fetchStatus: () => new Promise((r) => { resolveFetch = r; }),
    onUpdate: (st) => seen.push(st.status),
    setTimeout: timers.setTimeout, clearTimeout: timers.clearTimeout,
  });
  p.start('r');
  await timers.fireNext();
  p.cancel(); // e.g. the user navigated away
  resolveFetch({ status: 'pending' });
  await new Promise((r) => setImmediate(r));
  assert.deepStrictEqual(seen, []);
});

test('404 ends polling with an error; transient errors back off; attempts are bounded', async () => {
  let timers = fakeTimers();
  let seen = [];
  const notFound = Object.assign(new Error('gone'), { status: 404 });
  let p = CP.createPoller({
    fetchStatus: () => Promise.reject(notFound),
    onUpdate: (st) => seen.push(st.status),
    setTimeout: timers.setTimeout, clearTimeout: timers.clearTimeout,
  });
  p.start('r');
  while (await timers.fireNext()) { /* drain */ }
  assert.deepStrictEqual(seen, ['error']);

  timers = fakeTimers();
  seen = [];
  let gaveUp = false;
  p = CP.createPoller({
    fetchStatus: () => Promise.reject(new Error('offline')),
    onUpdate: (st) => seen.push(st.status),
    onGiveUp: () => { gaveUp = true; },
    maxAttempts: 4,
    setTimeout: timers.setTimeout, clearTimeout: timers.clearTimeout,
  });
  p.start('r');
  while (await timers.fireNext()) { /* drain */ }
  assert.deepStrictEqual(seen, []);
  assert.deepStrictEqual(timers.delays, [1000, 1500, 2250, 3375]);
  assert.strictEqual(gaveUp, true);
});

// ── security properties of the source ────────────────────────────────────
test('the admin key is never persisted or put in a URL', () => {
  const code = SRC.replace(/\/\*[\s\S]*?\*\//g, '').replace(/\/\/.*$/gm, '');
  assert.ok(!/localStorage|sessionStorage|indexedDB|document\.cookie/.test(code), 'storage API used');
  assert.ok(/'X-API-Key'\] = adminKey/.test(code), 'key must travel in the X-API-Key header');
  assert.ok(!/[?&](key|apiKey|api_key)=/.test(code), 'key-like query parameter found');
});

// ── revoke: FILTERS, renderAdminRow, XSS ──────────────────────────────────
test('FILTERS includes a revoked tab alongside the existing three', () => {
  // Array.from copies out of the vm realm first: comparing a vm-realm array
  // straight to a host array literal with deepStrictEqual fails on
  // prototype identity even when every element is equal.
  assert.deepStrictEqual(Array.from(CP.FILTERS, (f) => f[0]), ['pending', 'approved', 'rejected', 'revoked']);
  // Every filter key must be a status the server actually accepts
  // (channelregistry.ValidStatus); a typo here would 400 the admin list.
  for (const [key] of CP.FILTERS) assert.match(key, /^(pending|approved|rejected|revoked)$/);
});

test('renderAdminRow: pending gets Approve/Reject, approved gets a Remove (revoke) action, others a plain pill', () => {
  const pending = CP.renderAdminRow({ id: 'aaaaaaaaaaaaaaaa', name: '#Pending', status: 'pending', createdAt: 1700000000000 });
  assert.match(pending, /data-proposals-decide="approve"/);
  assert.match(pending, /data-proposals-decide="reject"/);
  assert.ok(!/data-proposals-decide="revoke"/.test(pending));

  const approved = CP.renderAdminRow({ id: 'bbbbbbbbbbbbbbbb', name: '#Approved', status: 'approved', createdAt: 1700000000000, reviewedAt: 1700000001000 });
  assert.match(approved, /data-proposals-decide="revoke"/);
  assert.match(approved, /data-proposal-id="bbbbbbbbbbbbbbbb"/);
  assert.match(approved, /data-proposal-name="#Approved"/);
  assert.match(approved, /aria-label="Remove #Approved"/);
  assert.match(approved, /data-state="approved"/, 'the approved pill must stay alongside Remove');

  for (const status of ['rejected', 'revoked']) {
    const html = CP.renderAdminRow({ id: 'cccccccccccccccc', name: '#X', status, createdAt: 1700000000000, reviewedAt: 1700000001000 });
    assert.ok(!/data-proposals-decide/.test(html), `${status} row must have no action buttons`);
    assert.match(html, new RegExp('data-state="' + status + '"'));
  }
});

test('renderAdminRow escapes the channel name everywhere it is interpolated (XSS)', () => {
  // NormalizeName allows < > " ' (the firmware has no character set), so a
  // publicly suggested name can carry markup: every sink must escape it.
  const evil = '#<img src=x onerror=alert(1)>';
  const html = CP.renderAdminRow({ id: 'dddddddddddddddd', name: evil, status: 'approved', createdAt: 1700000000000, reviewedAt: 1700000001000 });
  assert.ok(!html.includes('<img'), 'raw <img> tag leaked into the rendered row — a live element, not inert text');
  assert.match(html, /&lt;img src=x onerror=alert\(1\)&gt;/, 'the name must appear HTML-escaped (onerror= survives only as inert text inside it)');
  // Also proves the aria-label and data-proposal-name attributes specifically:
  assert.ok(!/aria-label="Remove #<img/.test(html), 'aria-label not escaped');
  assert.ok(!/data-proposal-name="#<img/.test(html), 'data-proposal-name not escaped');
});

// ── revoke: mergeApprovedChannels degrades correctly once a channel is
// revoked and drops out of the server's approvedChannels list ────────────
test('merge: a channel that drops out of approvedChannels (revoked) loses shared:true and is not phantom-duplicated', () => {
  // Before revoke: server still reports it as approved.
  const channels = [{ hash: '#WasShared', name: '#WasShared', messageCount: 3, lastActivityMs: 10 }];
  const beforeRevoke = CP.mergeApprovedChannels(channels, [{ name: '#WasShared', hash: '#WasShared' }]);
  assert.strictEqual(beforeRevoke[0].shared, true);

  // After revoke: approvedChannels no longer includes it (GET /api/channels
  // stops listing it — cmd/server/channel_proposals.go's approvedChannels()
  // filters by status='approved' via channelregistry.ListApprovedNames).
  // With no local key/messages for it, it simply disappears from the list,
  // same as any channel with zero traffic and no local data.
  const afterRevokeNoLocalData = CP.mergeApprovedChannels([], []);
  assert.deepStrictEqual(afterRevokeNoLocalData, []);

  // With the viewer's OWN local key/messages for that hash (they had it
  // monitored independently), it reverts to an ordinary, non-shared local
  // channel row: present, but shared is no longer set, and it is not
  // duplicated into a second phantom row.
  const localOnly = [{ hash: '#WasShared', name: '#WasShared', messageCount: 3, lastActivityMs: 10 }];
  const afterRevokeWithLocalData = CP.mergeApprovedChannels(localOnly, []);
  assert.strictEqual(afterRevokeWithLocalData.length, 1, 'must not leave a phantom duplicate row');
  assert.strictEqual(afterRevokeWithLocalData[0].shared, undefined, 'shared:true must not survive the channel dropping out of approvedChannels');
  assert.strictEqual(afterRevokeWithLocalData[0], localOnly[0], 'untouched local row is reused, not copied');
});

// ── revoke: admin dialog + confirm flow (hand-rolled minimal DOM) ─────────
//
// public/channel-proposals.js is a browser-only module with no DOM
// abstraction layer to inject (test-frontend-helpers.js establishes the
// pattern of hand-rolling just enough of `document` inside a vm context
// for exactly this reason — no jsdom dependency in this repo). This block
// builds only what openAdmin/renderAdminRow/decide/onAdminClick/
// onAdminKeydown actually touch: element creation, attributes, class,
// simple selectors (#id/.class/[attr]/[attr="v"]/tag, comma lists, and the
// one :not([tabindex="-1"]) case focusables() uses), a small innerHTML
// parser (our own markup is simple, well-formed and always escaped), event
// listeners with bubbling, and focus/activeElement tracking.
function buildMiniDom() {
  const ENTITIES = { amp: '&', lt: '<', gt: '>', quot: '"', '#39': "'" };
  function decodeEntities(s) {
    return String(s).replace(/&(amp|lt|gt|quot|#39);/g, (_, n) => ENTITIES[n]);
  }

  function splitSelectorList(sel) { return sel.split(',').map((s) => s.trim()).filter(Boolean); }
  function parseAttrSel(s) {
    const m = /^\[([\w-]+)(?:=("|')(.*?)\2)?\]$/.exec(s);
    if (!m) return null;
    return { name: m[1], value: m[3] };
  }
  function matchesPart(el, part) {
    const notMatch = /^(.*?):not\((.*)\)$/.exec(part);
    let notSel = null;
    if (notMatch) { part = notMatch[1]; notSel = notMatch[2]; }
    let base;
    if (part === '') base = true;
    else if (part[0] === '#') base = el.id === part.slice(1);
    else if (part[0] === '.') base = (el.className || '').split(/\s+/).indexOf(part.slice(1)) !== -1;
    else if (part[0] === '[') {
      const a = parseAttrSel(part);
      base = !a ? false : a.value === undefined ? el.hasAttribute(a.name) : el.getAttribute(a.name) === a.value;
    } else {
      base = el.tagName === part.toLowerCase();
    }
    if (base && notSel) base = !matchesPart(el, notSel);
    return base;
  }
  function elMatches(el, selector) {
    return el.nodeType === 1 && splitSelectorList(selector).some((p) => matchesPart(el, p));
  }

  class TextNode {
    constructor(text) { this.nodeType = 3; this.textContent = text; }
  }

  class Elem {
    constructor(tag) {
      this.nodeType = 1;
      this.tagName = tag;
      this.attrs = {};
      this.children = [];
      this.parentNode = null;
      this._listeners = {};
      this.disabled = false;
    }
    get id() { return this.attrs.id || ''; }
    set id(v) { this.attrs.id = v; }
    get className() { return this.attrs.class || ''; }
    set className(v) { this.attrs.class = v; }
    getAttribute(name) { return Object.prototype.hasOwnProperty.call(this.attrs, name) ? this.attrs[name] : null; }
    setAttribute(name, value) { this.attrs[name] = String(value); }
    hasAttribute(name) { return Object.prototype.hasOwnProperty.call(this.attrs, name); }
    removeAttribute(name) { delete this.attrs[name]; }
    get textContent() {
      let out = '';
      (function walk(n) {
        if (n.nodeType === 3) { out += n.textContent; return; }
        (n.children || []).forEach(walk);
      })(this);
      return out;
    }
    set textContent(v) { this.children = [new TextNode(String(v))]; }
    set innerHTML(html) { this.children = []; parseInto(this, html); }
    appendChild(child) { child.parentNode = this; this.children.push(child); return child; }
    remove() { if (this.parentNode) this.parentNode.children = this.parentNode.children.filter((c) => c !== this); this.parentNode = null; }
    contains(node) { for (let n = node; n; n = n.parentNode) if (n === this) return true; return false; }
    closest(selector) { for (let n = this; n; n = n.parentNode) if (n.nodeType === 1 && elMatches(n, selector)) return n; return null; }
    querySelectorAll(selector) {
      const out = [];
      (function walk(n) {
        (n.children || []).forEach((c) => {
          if (c.nodeType === 1) { if (elMatches(c, selector)) out.push(c); walk(c); }
        });
      })(this);
      return out;
    }
    querySelector(selector) { return this.querySelectorAll(selector)[0] || null; }
    addEventListener(type, fn) { (this._listeners[type] = this._listeners[type] || []).push(fn); }
    removeEventListener(type, fn) { this._listeners[type] = (this._listeners[type] || []).filter((f) => f !== fn); }
    focus() { doc.activeElement = this; }
    get offsetParent() { return this.hasAttribute('hidden') ? null : {}; }
  }

  function parseInto(parent, html) {
    const stack = [parent];
    const re = /<\/([a-zA-Z0-9-]+)\s*>|<([a-zA-Z0-9-]+)((?:\s+[a-zA-Z_:][\w:-]*(?:\s*=\s*(?:"[^"]*"|'[^']*'))?)*)\s*(\/?)>|([^<]+)/g;
    const VOID = { input: 1, br: 1, hr: 1, img: 1, use: 1 };
    let m;
    while ((m = re.exec(html))) {
      if (m[1]) {
        for (let i = stack.length - 1; i > 0; i--) {
          if (stack[i].tagName === m[1].toLowerCase()) { stack.length = i; break; }
        }
      } else if (m[2]) {
        const tag = m[2].toLowerCase();
        const el = new Elem(tag);
        const attrRe = /([a-zA-Z_:][\w:-]*)(?:\s*=\s*("([^"]*)"|'([^']*)'))?/g;
        let am;
        while ((am = attrRe.exec(m[3] || ''))) {
          if (!am[1]) continue;
          const val = am[3] !== undefined ? am[3] : am[4] !== undefined ? am[4] : '';
          el.setAttribute(am[1], decodeEntities(val));
        }
        stack[stack.length - 1].appendChild(el);
        if (!(m[4] === '/' || VOID[tag])) stack.push(el);
      } else if (m[5]) {
        stack[stack.length - 1].appendChild(new TextNode(decodeEntities(m[5])));
      }
    }
  }

  const docListeners = {};
  const body = new Elem('body');
  const doc = {
    activeElement: body,
    body,
    createElement: (tag) => new Elem(tag),
    getElementById(id) { return body.querySelectorAll('*').concat([body]).find((e) => e.id === id) || null; },
    querySelector: (sel) => body.querySelector(sel),
    querySelectorAll: (sel) => body.querySelectorAll(sel),
    addEventListener(type, fn) { (docListeners[type] = docListeners[type] || []).push(fn); },
    removeEventListener(type, fn) { docListeners[type] = (docListeners[type] || []).filter((f) => f !== fn); },
    contains: (node) => body.contains(node) || node === body,
    __events: docListeners, // test-only escape hatch: see fireDocKeydown below
  };
  // '*' pseudo-selector for getElementById's full-tree scan.
  const origMatches = elMatches;
  function elMatchesStar(el, selector) { return selector === '*' || origMatches(el, selector); }
  body.querySelectorAll = function (selector) {
    const out = [];
    (function walk(n) { (n.children || []).forEach((c) => { if (c.nodeType === 1) { if (elMatchesStar(c, selector)) out.push(c); walk(c); } }); })(body);
    return out;
  };

  return { doc };
}

function flush() {
  let p = Promise.resolve();
  for (let i = 0; i < 12; i++) p = p.then(() => new Promise((r) => setImmediate(r)));
  return p;
}

function loadWithDom(fetchImpl, opts) {
  opts = opts || {};
  const { doc } = buildMiniDom();
  const fetchCalls = [];
  const fakeFetch = (url, opts) => {
    fetchCalls.push({ url, method: (opts && opts.method) || 'GET', body: opts && opts.body });
    const res = fetchImpl(url, opts);
    return Promise.resolve({
      ok: res.status >= 200 && res.status < 300,
      status: res.status,
      text: () => Promise.resolve(JSON.stringify(res.body === undefined ? {} : res.body)),
    });
  };
  const location = { hash: '#/channels?view=proposals' };
  const history = { replaceState: (_s, _t, url) => { location.hash = url; } };
  const ctx = { console, Promise, Map, Set, Math, JSON, Object, Array, String, encodeURIComponent, setImmediate };
  ctx.window = ctx;
  ctx.document = doc;
  ctx.location = location;
  ctx.history = history;
  ctx.fetch = fakeFetch;
  ctx.setTimeout = opts.setTimeout || setTimeout;
  ctx.clearTimeout = opts.clearTimeout || clearTimeout;
  vm.createContext(ctx);
  vm.runInContext(SRC, ctx, { filename: SRC_PATH });
  return { CP: ctx.ChannelProposals, document: doc, fetchCalls };
}

test('admin dialog: Remove opens a confirm dialog; Escape cancels it and returns focus to Remove', async () => {
  const env = loadWithDom(() => ({ status: 200, body: { proposals: [{ id: 'bbbbbbbbbbbbbbbb', name: '#Approved', status: 'approved', createdAt: 1, reviewedAt: 2 }], enabled: true } }));
  await openApprovedAdminWithConfirm(env);
  const removeBtn = env.document.getElementById('chProposalsList').querySelector('[data-proposals-decide="revoke"]');
  assert.ok(removeBtn, 'Remove button must be rendered for the approved row');

  const confirmDlg = env.document.getElementById('chProposalsConfirm');
  assert.ok(confirmDlg, 'confirm dialog must open');
  assert.match(confirmDlg.textContent, /Remove #Approved\?/);
  assert.match(confirmDlg.textContent, /stop being shared with everyone/);

  // Escape closes only the confirm layer, not the whole admin dialog.
  fireDocKeydown(env, { key: 'Escape', preventDefault() {}, stopPropagation() {} });
  assert.strictEqual(env.document.getElementById('chProposalsConfirm'), null, 'confirm must close on Escape');
  assert.ok(env.document.getElementById('chProposalsAdmin'), 'admin dialog must stay open');
  assert.strictEqual(env.document.activeElement, removeBtn, 'focus must return to the Remove button');
});

test('admin dialog: Tab traps inside the confirm dialog while it is open', async () => {
  const env = loadWithDom(() => ({ status: 200, body: { proposals: [{ id: 'bbbbbbbbbbbbbbbb', name: '#Approved', status: 'approved', createdAt: 1, reviewedAt: 2 }], enabled: true } }));
  await openApprovedAdminWithConfirm(env);
  const confirmDlg = env.document.getElementById('chProposalsConfirm');
  const buttons = confirmDlg.querySelectorAll('button');
  assert.strictEqual(buttons.length, 2, 'Cancel and Remove');
  const [cancelBtn, confirmBtn] = buttons;
  assert.strictEqual(env.document.activeElement, confirmBtn, 'the Remove/confirm button starts focused');

  // Tab forward from the last focusable wraps to the first.
  const tabEvt = { key: 'Tab', shiftKey: false, preventDefault() {} };
  fireDocKeydown(env, tabEvt);
  assert.strictEqual(env.document.activeElement, cancelBtn, 'Tab from the last item wraps to the first');

  // Shift+Tab from the first wraps to the last.
  const shiftTabEvt = { key: 'Tab', shiftKey: true, preventDefault() {} };
  fireDocKeydown(env, shiftTabEvt);
  assert.strictEqual(env.document.activeElement, confirmBtn, 'Shift+Tab from the first item wraps to the last');
});

test('admin dialog: Confirm posts to the revoke endpoint and handles success', async () => {
  const env = loadWithDom((url) => {
    if (/\/revoke$/.test(url)) return { status: 202, body: { requestId: '0123456789abcdef' } };
    if (/\/requests\//.test(url)) return { status: 200, body: { status: 'revoked', proposal: { id: 'bbbbbbbbbbbbbbbb', name: '#Approved', status: 'revoked' } } };
    return { status: 200, body: { proposals: [{ id: 'bbbbbbbbbbbbbbbb', name: '#Approved', status: 'approved', createdAt: 1, reviewedAt: 2 }], enabled: true } };
  });
  const overlay = await openApprovedAdminWithConfirm(env);
  const confirmBtn = env.document.getElementById('chProposalsConfirm').querySelectorAll('button')[1];
  overlay._listeners.click[0]({ target: confirmBtn, preventDefault() {} });
  await flush();

  assert.strictEqual(env.document.getElementById('chProposalsConfirm'), null, 'confirm dialog must close on Confirm');
  const revokeCalls = env.fetchCalls.filter((c) => /\/revoke$/.test(c.url));
  assert.strictEqual(revokeCalls.length, 1, 'exactly one POST to the revoke endpoint');
  assert.strictEqual(revokeCalls[0].method, 'POST');
});

test('admin dialog: a synchronous 409 (not approved anymore) is surfaced without ever starting a poller', async () => {
  const env = loadWithDom((url) => {
    if (/\/revoke$/.test(url)) return { status: 409, body: { error: 'suggestion is not currently approved' } };
    return { status: 200, body: { proposals: [{ id: 'bbbbbbbbbbbbbbbb', name: '#Approved', status: 'approved', createdAt: 1, reviewedAt: 2 }], enabled: true } };
  });
  const overlay = await openApprovedAdminWithConfirm(env);
  const confirmBtn = env.document.getElementById('chProposalsConfirm').querySelectorAll('button')[1];
  overlay._listeners.click[0]({ target: confirmBtn, preventDefault() {} });
  await flush();

  const revokeCalls = env.fetchCalls.filter((c) => /\/revoke$/.test(c.url));
  assert.strictEqual(revokeCalls.length, 1);
  const requestPolls = env.fetchCalls.filter((c) => /\/requests\//.test(c.url));
  assert.strictEqual(requestPolls.length, 0, 'a 409 must never start polling a request status');
  const status = env.document.getElementById('chProposalsStatus');
  assert.match(status.textContent, /not currently approved/);
  assert.strictEqual(status.getAttribute('data-kind'), 'error');
});

// Shared setup: unlock the admin dialog on a fixture with one approved
// proposal, click its Remove button, and return the overlay (with the
// confirm dialog open) for the caller to act on.
async function openApprovedAdminWithConfirm(env) {
  env.CP.mount({ root: env.document.body });
  env.CP.openAdmin();
  const keyInput = env.document.getElementById('chProposalsKey');
  keyInput.value = 'strong-enough-admin-key-012345';
  const overlay = env.document.getElementById('chProposalsAdmin');
  overlay._listeners.submit[0]({ target: env.document.getElementById('chProposalsKeyForm'), preventDefault() {} });
  await flush();
  const removeBtn = env.document.getElementById('chProposalsList').querySelector('[data-proposals-decide="revoke"]');
  env.document.activeElement = removeBtn;
  overlay._listeners.click[0]({ target: removeBtn, preventDefault() {} });
  return overlay;
}

function fireDocKeydown(env, evt) {
  // onAdminKeydown is registered via document.addEventListener('keydown', ...)
  // in openAdmin(); replaying it the same way a real keydown would reach it,
  // through the document-level listener registry buildMiniDom exposes.
  (env.document.__events.keydown || []).slice().forEach((fn) => fn(evt));
}

// ── #232: an approval reported by the suggest poller refreshes the list ──
// Polls fire on the next macrotask instead of after 1s/1.5s/….
const fastTimers = { setTimeout: (fn) => setImmediate(fn), clearTimeout: (id) => clearImmediate(id) };

// Mount with a suggest section and an onApproved spy; statusFor(requestId,
// pollIndex) answers each status request. timers defaults to fastTimers.
async function mountSuggest(statusFor, extraRoute, timers) {
  const polls = {};
  const approved = [];
  const env = loadWithDom((url, o) => {
    const method = (o && o.method) || 'GET';
    if (/\/channel-proposals\/config$/.test(url)) return { status: 200, body: { enabled: true } };
    if (method === 'POST' && /\/api\/channel-proposals$/.test(url)) {
      const name = JSON.parse(o.body).name;
      return { status: 202, body: { requestId: 'req-' + name.slice(1) } };
    }
    const m = /\/requests\/([^/?]+)$/.exec(url);
    if (m) {
      const id = decodeURIComponent(m[1]);
      polls[id] = (polls[id] || 0) + 1;
      return { status: 200, body: statusFor(id, polls[id]) };
    }
    if (extraRoute) { const r = extraRoute(url, method); if (r) return r; }
    return { status: 404, body: { error: 'unexpected ' + method + ' ' + url } };
  }, timers || fastTimers);
  const section = env.document.createElement('section');
  section.setAttribute('hidden', '');
  env.document.body.appendChild(section);
  env.CP.mount({ root: env.document.body, suggestSection: section, onApproved: (name) => approved.push(name) });
  await flush();
  assert.ok(section.querySelector('#chSuggestForm'), 'the suggest form must render when suggestions are enabled');
  env.approved = approved;
  env.suggest = async (name) => {
    section.querySelector('#chSuggestName').value = name;
    section.querySelector('#chSuggestForm')._listeners.submit[0]({ preventDefault() {} });
    await flush();
  };
  env.suggestStatus = () => section.querySelector('#chSuggestStatus');
  return env;
}

function proposal(id, name, status, reviewedAt) {
  const p = { id, name, status, createdAt: 1 };
  if (reviewedAt) p.reviewedAt = reviewedAt;
  return p;
}

test('suggest poller: an auto-approved suggestion calls onApproved exactly once (#232)', async () => {
  const env = await mountSuggest((id, n) => n === 1
    ? { status: 'queued' }
    : { status: 'approved', proposal: proposal('a1', '#AutoShared', 'approved', 50) });
  await env.suggest('AutoShared');
  assert.match(env.suggestStatus().textContent, /#AutoShared is already shared with everyone/);
  assert.deepStrictEqual(env.approved, ['#AutoShared'], 'onApproved must run once, with the channel name');
  // The poller stopped at the final status: nothing else is polled or refreshed.
  await flush();
  assert.strictEqual(env.fetchCalls.filter((c) => /\/requests\//.test(c.url)).length, 2);
  assert.deepStrictEqual(env.approved, ['#AutoShared']);
});

test('suggest poller: pending, rejected and failed suggestions do not call onApproved (#232)', async () => {
  const final = {
    'req-Waiting': { status: 'pending', proposal: proposal('p1', '#Waiting', 'pending') },
    'req-Refused': { status: 'rejected', proposal: proposal('p2', '#Refused', 'rejected', 7) },
    'req-Broken': { status: 'error', error: 'capacity reached' },
  };
  const env = await mountSuggest((id) => final[id]);
  for (const name of ['Waiting', 'Refused', 'Broken']) await env.suggest(name);
  assert.match(env.suggestStatus().textContent, /capacity reached/);
  assert.deepStrictEqual(env.approved, []);
});

test('admin decision and suggest poller reporting the same approval refresh only once (#232)', async () => {
  const P = proposal('dddddddddddddddd', '#Both', 'approved', 99);
  const env = await mountSuggest(
    (id) => ({ status: 'approved', proposal: id === 'req-Other' ? proposal('eeeeeeeeeeeeeeee', '#Other', 'approved', 99) : P }),
    (url, method) => {
      if (method === 'POST' && /\/approve$/.test(url)) return { status: 202, body: { requestId: 'req-admin' } };
      if (/\/admin\/channel-proposals\?/.test(url)) return { status: 200, body: { proposals: [proposal(P.id, P.name, 'pending')], enabled: true } };
      return null;
    });
  // Admin approves #Both in this tab …
  env.CP.openAdmin();
  env.document.getElementById('chProposalsKey').value = 'strong-enough-admin-key-012345';
  const overlay = env.document.getElementById('chProposalsAdmin');
  overlay._listeners.submit[0]({ target: env.document.getElementById('chProposalsKeyForm'), preventDefault() {} });
  await flush();
  const approveBtn = env.document.getElementById('chProposalsList').querySelector('[data-proposals-decide="approve"]');
  overlay._listeners.click[0]({ target: approveBtn, preventDefault() {} });
  await flush();
  assert.match(env.document.getElementById('chProposalsStatus').textContent, /#Both is now shared/);
  assert.deepStrictEqual(env.approved, ['#Both'], 'the admin path refreshes once');
  // … and the suggest poller reports the very same approval.
  await env.suggest('Both');
  assert.match(env.suggestStatus().textContent, /#Both is already shared/);
  assert.deepStrictEqual(env.approved, ['#Both'], 'the same approval must not refresh the list a second time');
  // A different approval still refreshes.
  await env.suggest('Other');
  assert.deepStrictEqual(env.approved, ['#Both', '#Other']);
});

test('a proposal approved again after a revoke refreshes again (#232)', async () => {
  let reviewedAt = 10;
  const env = await mountSuggest(() => ({ status: 'approved', proposal: proposal('ffffffffffffffff', '#Again', 'approved', reviewedAt) }));
  await env.suggest('Again');
  await env.suggest('Again');
  assert.deepStrictEqual(env.approved, ['#Again'], 're-reading the same approval is not a new one');
  reviewedAt = 20; // revoked, re-proposed and approved again: same id, new decision
  await env.suggest('Again');
  assert.deepStrictEqual(env.approved, ['#Again', '#Again']);
});

// #243: leaving the Channels page must stop the suggest poller, or it keeps
// polling a queued suggestion's status after the page is gone.
test('unmount() cancels the suggest poller: no poll stays scheduled, no status request afterwards (#243)', async () => {
  const timers = fakeTimers();
  const env = await mountSuggest(() => ({ status: 'queued' }), null, timers);
  await env.suggest('StillQueued');
  assert.strictEqual(timers.size(), 1, 'a queued suggestion schedules a status poll');
  const polls = () => env.fetchCalls.filter((c) => /\/requests\//.test(c.url)).length;
  const before = polls();
  env.CP.unmount();
  assert.strictEqual(timers.size(), 0, 'unmount() must clear the scheduled poll');
  while (await timers.fireNext()) { /* run whatever is still scheduled */ }
  await flush();
  assert.strictEqual(polls(), before, 'no status request after unmount()');
});

// ── Invisible formatting characters (PR #99 review, finding 3) ───────────
test('normalizeName rejects invisible format characters (Cf) and line separators', () => {
  const chars = ['\u00AD', '\u0600', '\u180E', '\u200B', '\u200C', '\u2060', '\u2062', '\uFEFF', '\uFFF9',
    '\u{E0001}', '\u{E0020}', '\u{E0061}', '\u{E007F}', '\u2028', '\u2029', '\u202E', '\u200E'];
  for (const c of chars) {
    for (const input of ['#a' + c + 'b', '#' + c + 'ab']) {
      const r = CP.normalizeName(input);
      assert.ok(r.error && !r.name, `${JSON.stringify(input)} should be rejected, got ${JSON.stringify(r)}`);
    }
  }
  assert.match(CP.normalizeName('#a\u200Bb').error, /invisible/);
});

test('normalizeName keeps emoji with ZWJ and variation selectors', () => {
  for (const input of ['#\u{1F3F3}\uFE0F\u200D\u{1F308}', '#\u2764\uFE0F', '#\u2764\uFE0E', '#\u{1F469}\u200D\u{1F4BB}',
    '#a\uFE00b', '#\u845B\u{E0100}', '#mesh\u200Dcore']) {
    const r = CP.normalizeName(input);
    assert.strictEqual(r.error, undefined, `${JSON.stringify(input)}: ${r.error}`);
    assert.strictEqual(r.name, input);
  }
});

// ── Trim parity with internal/channelregistry.NormalizeName (#99 follow-up) ──
// JS's native String.prototype.trim() and Go's strings.TrimSpace do not agree
// on which runes are whitespace: trim() also strips U+FEFF (BOM), and does
// not strip U+0085 (NEL) — TrimSpace is the other way around. normalizeName
// must trim exactly what the server trims, so the two sides reject/accept
// the same inputs. The pinned list below matches the one independently
// enumerated in internal/channelregistry/name_test.go
// (TestGoSpaceCodepointsParity) via unicode.IsSpace over the full rune
// range. The Go test catches a change in Go's own Unicode White_Space
// table; this JS constant only catches a change to this list itself
// (assert.deepStrictEqual below just checks it still matches the runtime
// GO_SPACE_CODEPOINTS shipped in channel-proposals.js). The two lists are
// kept in sync by hand — update both together.
const GO_SPACE_CODEPOINTS = [
  0x0009, 0x000A, 0x000B, 0x000C, 0x000D, 0x0020, 0x0085, 0x00A0,
  0x1680, 0x2000, 0x2001, 0x2002, 0x2003, 0x2004, 0x2005, 0x2006,
  0x2007, 0x2008, 0x2009, 0x200A, 0x2028, 0x2029, 0x202F, 0x205F, 0x3000
];

test('GO_SPACE_CODEPOINTS matches the pinned Go unicode.IsSpace enumeration', () => {
  // CP.GO_SPACE_CODEPOINTS is an Array from the vm context's own realm, so
  // compare values (JSON round-trip), not object/constructor identity.
  assert.deepStrictEqual(JSON.parse(JSON.stringify(CP.GO_SPACE_CODEPOINTS)), GO_SPACE_CODEPOINTS);
});

test('normalizeName rejects a leading/trailing U+FEFF as invisible instead of silently trimming it', () => {
  // Go's TrimSpace does NOT strip U+FEFF, so the server sees it, decides it
  // is Cf, and rejects with ErrNameInvisible. The frontend must match: not
  // trim it away and submit a name the user never typed.
  for (const input of ['\uFEFFtest', 'test\uFEFF', '\uFEFF\uFEFFtest\uFEFF']) {
    const r = CP.normalizeName(input);
    assert.ok(r.error && !r.name, `${JSON.stringify(input)} should be rejected, got ${JSON.stringify(r)}`);
    assert.match(r.error, /invisible/, JSON.stringify(input));
  }
});

test('normalizeName trims a leading/trailing U+0085 (NEL) and accepts the result', () => {
  // Go's TrimSpace DOES strip U+0085 (unicode.IsSpace), so the server sees
  // "#test" and accepts it. JS's native trim() does not touch U+0085, which
  // used to leave it in CONTROL_RE range and get the frontend to reject a
  // name the server would have accepted.
  for (const input of ['test\u0085', '\u0085test', '\u0085test\u0085']) {
    const r = CP.normalizeName(input);
    assert.strictEqual(r.error, undefined, `${JSON.stringify(input)}: ${r.error}`);
    assert.strictEqual(r.name, '#test', JSON.stringify(input));
  }
});

test('normalizeName trims every codepoint Go treats as whitespace, and only those', () => {
  for (const cp of GO_SPACE_CODEPOINTS) {
    const c = String.fromCodePoint(cp);
    const input = c + 'test' + c;
    const r = CP.normalizeName(input);
    assert.strictEqual(r.error, undefined, `U+${cp.toString(16).toUpperCase()}: ${r.error}`);
    assert.strictEqual(r.name, '#test', `U+${cp.toString(16).toUpperCase()}`);
  }
  // trimGoSpace itself: no more, no less than the pinned list.
  assert.strictEqual(CP.trimGoSpace('  \t\u3000hello\u3000\t  '), 'hello');
  assert.strictEqual(CP.trimGoSpace('\uFEFFhello\uFEFF'), '\uFEFFhello\uFEFF', 'U+FEFF must not be trimmed');
  assert.strictEqual(CP.trimGoSpace('\u0085hello\u0085'), 'hello', 'U+0085 must be trimmed');
});

test('normalizeName still rejects U+FEFF / U+200B in the middle of a name (unchanged behavior)', () => {
  for (const input of ['#mid\uFEFFdle', '#mid\u200Bdle']) {
    const r = CP.normalizeName(input);
    assert.ok(r.error && !r.name, `${JSON.stringify(input)} should be rejected, got ${JSON.stringify(r)}`);
    assert.match(r.error, /invisible/, JSON.stringify(input));
  }
});

// ── Remove confirmation escaping (mutant M21) ────────────────────────────
// The dialog is built with the DOM API (no HTML strings with data), so the
// name must arrive as text in the title and as a plain attribute value on
// the confirm button: no element or handler may come out of it.
test('admin dialog: the Remove confirmation renders an XSS name as inert text', async () => {
  const evil = '#"><img src=x onerror=__x=1>';
  const env = loadWithDom(() => ({ status: 200, body: { proposals: [{ id: 'dddddddddddddddd', name: evil, status: 'approved', createdAt: 1, reviewedAt: 2 }], enabled: true } }));
  await openApprovedAdminWithConfirm(env);
  const dlg = env.document.getElementById('chProposalsConfirm');
  assert.ok(dlg, 'confirm dialog must open');
  assert.strictEqual(dlg.querySelectorAll('img').length, 0, 'no <img> element may be created from the name');
  assert.ok(!dlg.querySelectorAll('*').some((el) => Object.keys(el.attrs).some((a) => /^on/i.test(a))), 'no event-handler attribute may appear');
  const title = env.document.getElementById('chProposalsConfirmTitle');
  assert.strictEqual(title.tagName, 'h4');
  assert.strictEqual(title.textContent, 'Remove ' + evil + '?');
  const confirmBtn = dlg.querySelector('[data-proposals-confirm-action="confirm"]');
  assert.strictEqual(confirmBtn.getAttribute('data-proposal-name'), evil);
  assert.strictEqual(confirmBtn.getAttribute('data-proposal-id'), 'dddddddddddddddd');
  assert.match(dlg.textContent, /stop being shared with everyone/);
  assert.strictEqual(dlg.getAttribute('role'), 'alertdialog');
  assert.strictEqual(dlg.getAttribute('aria-modal'), 'true');
  assert.strictEqual(dlg.getAttribute('aria-labelledby'), 'chProposalsConfirmTitle');
  assert.strictEqual(env.document.activeElement, confirmBtn, 'focus starts on the confirm button');
});

test('the confirmation dialog is not built from an HTML string', () => {
  // Guards the preflight XSS gate's reason for this design: data must never
  // reach .innerHTML in the confirm layer again.
  const body = SRC.slice(SRC.indexOf('function openConfirm('), SRC.indexOf('function closeConfirm('));
  assert.ok(body.length > 0, 'openConfirm not found');
  assert.ok(!/innerHTML|insertAdjacentHTML|outerHTML/.test(body), 'openConfirm must use createElement/textContent/setAttribute');
});

// ── Built-in names (PR #99 review, finding 4) ────────────────────────────
test('renderAdminRow marks names the server already decrypts through its built-in list', () => {
  const html = CP.renderAdminRow({ id: 'eeeeeeeeeeeeeeee', name: '#test', status: 'approved', createdAt: 1, reviewedAt: 2, builtIn: true });
  assert.match(html, /class="ch-proposals-builtin"/);
  assert.match(html, /Built in: already decrypted/);
  assert.match(html, /data-builtin="true"/);
  assert.match(html, /data-proposal-builtin="true"/, 'the Remove button must carry the built-in flag to its confirmation');
  const plain = CP.renderAdminRow({ id: 'ffffffffffffffff', name: '#Mine', status: 'pending', createdAt: 1 });
  assert.ok(!/ch-proposals-builtin|data-builtin/.test(plain), 'a normal name must not be marked');
});

test('suggestMessage warns when a suggestion is already decrypted, and explains the rejection block', () => {
  const warn = CP.suggestMessage({ status: 'pending', builtIn: true, proposal: { name: '#chat' } });
  assert.strictEqual(warn.kind, 'warn');
  assert.match(warn.text, /#chat is waiting/);
  assert.match(warn.text, /already decrypts it through its built-in channel list/);
  const normal = CP.suggestMessage({ status: 'pending', proposal: { name: '#Mine' } });
  assert.strictEqual(normal.kind, 'success');
  assert.ok(!/built-in/.test(normal.text));
  assert.match(CP.suggestMessage({ status: 'rejected', proposal: { name: '#Spam' } }).text, /suggested again once the earlier decision expires/);
});

test('admin dialog: the Remove confirmation shows a markup name as text and warns for built-in names', async () => {
  const evil = '#<b>bold</b>';
  const env = loadWithDom(() => ({ status: 200, body: { proposals: [{ id: 'bbbbbbbbbbbbbbbb', name: evil, status: 'approved', createdAt: 1, reviewedAt: 2, builtIn: true }], enabled: true } }));
  await openApprovedAdminWithConfirm(env);
  const confirmDlg = env.document.getElementById('chProposalsConfirm');
  assert.ok(confirmDlg, 'confirm dialog must open');
  const title = env.document.getElementById('chProposalsConfirmTitle');
  assert.strictEqual(title.textContent, 'Remove ' + evil + '?', 'the name must render as text, not markup');
  assert.strictEqual(title.querySelectorAll('b').length, 0, 'no element may be created from the name');
  assert.match(confirmDlg.textContent, /keeps decrypting it through its built-in channel list/);
});

// ── #251: a revoked channel leaves the channel list ──────────────────────
test('renderAdminRow shows case near-duplicates as escaped text and omits the note otherwise (#251)', () => {
  const html = CP.renderAdminRow({ id: 'aaaaaaaaaaaaaaaa', name: '#HelloWorld', status: 'pending', createdAt: 1,
    nearDuplicateOf: ['#helloworld', '#<b>x</b>'] });
  assert.match(html, /class="ch-proposals-neardup"/);
  assert.match(html, /Same name in different case: #helloworld, #&lt;b&gt;x&lt;\/b&gt;/);
  assert.ok(!/<b>x/.test(html), 'a near-duplicate name must be escaped');
  for (const none of [undefined, [], null, [42, '']]) {
    const plain = CP.renderAdminRow({ id: 'bbbbbbbbbbbbbbbb', name: '#Mine', status: 'pending', createdAt: 1, nearDuplicateOf: none });
    assert.ok(!/ch-proposals-neardup/.test(plain), 'no note without a near-duplicate: ' + JSON.stringify(none));
  }
});

// Revoking from the admin dialog refreshes the channel list (the server now
// leaves a revoked channel out of it); approve and reject do not.
async function decideAndCollect(op, resultStatus) {
  const revoked = [];
  const env = loadWithDom((url) => {
    if (/\/(approve|reject|revoke)$/.test(url)) return { status: 202, body: { requestId: '0123456789abcdef' } };
    if (/\/requests\//.test(url)) return { status: 200, body: { status: resultStatus, proposal: { id: 'bbbbbbbbbbbbbbbb', name: '#Shared', status: resultStatus, reviewedAt: 9 } } };
    return { status: 200, body: { proposals: [{ id: 'bbbbbbbbbbbbbbbb', name: '#Shared', status: op === 'revoke' ? 'approved' : 'pending', createdAt: 1, reviewedAt: 2 }], enabled: true } };
  }, fastTimers);
  env.CP.mount({ root: env.document.body, onRevoked: (name) => revoked.push(name) });
  env.CP.openAdmin();
  env.document.getElementById('chProposalsKey').value = 'strong-enough-admin-key-012345';
  const overlay = env.document.getElementById('chProposalsAdmin');
  overlay._listeners.submit[0]({ target: env.document.getElementById('chProposalsKeyForm'), preventDefault() {} });
  await flush();
  const btn = env.document.getElementById('chProposalsList').querySelector('[data-proposals-decide="' + op + '"]');
  env.document.activeElement = btn;
  overlay._listeners.click[0]({ target: btn, preventDefault() {} });
  if (op === 'revoke') {
    const confirmBtn = env.document.getElementById('chProposalsConfirm').querySelectorAll('button')[1];
    overlay._listeners.click[0]({ target: confirmBtn, preventDefault() {} });
  }
  await flush();
  await flush();
  return revoked;
}

test('admin revoke calls onRevoked once with the channel name (#251)', async () => {
  assert.deepStrictEqual(await decideAndCollect('revoke', 'revoked'), ['#Shared']);
});

test('admin approve and reject do not call onRevoked (#251)', async () => {
  assert.deepStrictEqual(await decideAndCollect('approve', 'approved'), []);
  assert.deepStrictEqual(await decideAndCollect('reject', 'rejected'), []);
});

(async () => {
  for (const t of tests) {
    try {
      await t.fn();
      passed++;
      console.log('  ✓ ' + t.name);
    } catch (e) {
      console.error('  ✗ ' + t.name + '\n    ' + (e && e.stack || e));
      process.exitCode = 1;
    }
  }
  console.log(`test-channel-proposals.js: ${passed} passed, ${tests.length - passed} failed`);
})();
