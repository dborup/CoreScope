#!/usr/bin/env node
/* Channel names in HTML attributes and CSS selectors (PR #99 review).
 *
 * Shared channel names are publicly suggestible and may contain < > " ' and
 * backslashes (the firmware imposes no character set). This loads the REAL
 * public/channels.js and public/analytics.js into vm contexts and checks:
 *   - renderChannelRow escapes the name/hash in data-hash, data-channel-hash
 *     and the visible text (mutant M22: escaping removed from data-hash);
 *   - the 1 s relative-time ticker CSS.escape's the hash in its selectors, so
 *     a name with '"' neither throws nor stops the other rows updating;
 *   - the known-channels catalogue row and the analytics channel table escape
 *     their data-supplied fields. */
'use strict';

const assert = require('assert');
const fs = require('fs');
const vm = require('vm');

// CSS.escape as specified by CSSOM (https://drafts.csswg.org/cssom/#serialize-an-identifier).
function cssEscape(value) {
  const s = String(value);
  let out = '';
  for (let i = 0; i < s.length; i++) {
    const c = s.charCodeAt(i);
    if (c === 0) { out += '�'; continue; }
    if ((c >= 0x1 && c <= 0x1f) || c === 0x7f ||
        (i === 0 && c >= 0x30 && c <= 0x39) ||
        (i === 1 && c >= 0x30 && c <= 0x39 && s.charCodeAt(0) === 0x2d)) { out += '\\' + c.toString(16) + ' '; continue; }
    if (i === 0 && s.length === 1 && c === 0x2d) { out += '\\' + s.charAt(i); continue; }
    if (c >= 0x80 || c === 0x2d || c === 0x5f || (c >= 0x30 && c <= 0x39) || (c >= 0x41 && c <= 0x5a) || (c >= 0x61 && c <= 0x7a)) { out += s.charAt(i); continue; }
    out += '\\' + s.charAt(i);
  }
  return out;
}

// Parses the value of a [attr="…"] selector the way a browser would and
// throws a SyntaxError, like querySelector, when an unescaped quote ends it
// early. Returns the unescaped attribute value.
function attrValue(selector, attr) {
  const start = selector.indexOf('[' + attr + '="');
  if (start < 0) throw new Error('no [' + attr + '] in ' + selector);
  let i = start + attr.length + 3, v = '';
  for (; i < selector.length; i++) {
    const ch = selector[i];
    if (ch === '\\') {
      const hex = /^[0-9a-fA-F]{1,6} ?/.exec(selector.slice(i + 1));
      if (hex) { v += String.fromCodePoint(parseInt(hex[0], 16)); i += hex[0].length; }
      else { v += selector[i + 1]; i++; }
      continue;
    }
    if (ch === '"') break;
    v += ch;
  }
  if (selector[i + 1] !== ']') {
    const e = new SyntaxError("'" + selector + "' is not a valid selector");
    throw e;
  }
  return v;
}

function loadChannels(queries) {
  const noop = () => {};
  const fakeEl = { addEventListener: noop, querySelector: () => null, classList: { add: noop, remove: noop, toggle: noop, contains: () => false }, appendChild: noop, removeChild: noop, setAttribute: noop, getAttribute: () => null, textContent: '', innerHTML: '', style: {}, dataset: {} };
  const doc = {
    readyState: 'complete', createElement: () => ({ ...fakeEl }), head: fakeEl, body: fakeEl,
    getElementById: () => null, querySelectorAll: () => [], addEventListener: noop,
    querySelector: (sel) => {
      const attr = sel.includes('data-channel-hash') ? 'data-channel-hash' : 'data-hash';
      const value = attrValue(sel, attr); // throws like a browser on a broken selector
      queries.push({ sel, value });
      const el = { textContent: '' };
      queries.els.push(el);
      return el;
    },
  };
  const ctx = {
    window: { addEventListener: noop }, document: doc, console, Date, Math, JSON, Set, Map, Array, Object, Promise, Error, String,
    setTimeout, clearTimeout, setInterval, clearInterval,
    history: { replaceState: noop, pushState: noop },
    location: { hash: '', href: '', pathname: '/' },
    navigator: { userAgent: 'node' },
    CSS: { escape: cssEscape },
    RegionFilter: { getRegionParam: () => '' },
    api: () => Promise.resolve({}),
    CLIENT_TTL: {},
    truncate: (s) => s,
    formatHashHex: (h) => String(h),
    channelDisplayName: (c) => (c && (c.name || c.hash)) || '',
    getSenderColor: () => 'var(--accent)',
    fetch: () => Promise.resolve({ json: () => Promise.resolve({}) }),
    localStorage: { getItem: () => null, setItem: noop, removeItem: noop },
  };
  vm.createContext(ctx);
  try { vm.runInContext(fs.readFileSync('public/channels.js', 'utf8'), ctx, { filename: 'public/channels.js' }); } catch (e) { /* helpers are exported before any init that needs a DOM */ }
  return ctx.window;
}

const tests = [];
function test(name, fn) { tests.push({ name, fn }); }

const EVIL = '#"><img src=x onerror=__x=1>';
const QUOTE = '#say "hi"\\now';

test('renderChannelRow escapes the name in data-hash, data-channel-hash and text (M22)', () => {
  const queries = []; queries.els = [];
  const w = loadChannels(queries);
  const render = w._channelsRenderChannelRowForTest;
  assert.strictEqual(typeof render, 'function', 'renderChannelRow not exported');
  for (const shared of [false, true]) {
    const html = render({ hash: EVIL, name: EVIL, messageCount: 1, lastActivityMs: 1, shared });
    assert.ok(!/<img\b/i.test(html), 'raw <img survived: ' + html);
    assert.ok(html.includes('data-hash="#&quot;&gt;&lt;img'), 'data-hash not escaped: ' + html);
    assert.ok(html.includes('data-channel-hash="#&quot;&gt;&lt;img'), 'data-channel-hash not escaped: ' + html);
    assert.ok(!/data-hash="#">/.test(html), 'data-hash attribute broken out: ' + html);
  }
});

test('time ticker CSS.escapes channel names in selectors and keeps going after a quote', () => {
  const queries = []; queries.els = [];
  const w = loadChannels(queries);
  const tick = w._channelsTickChannelTimesForTest;
  assert.strictEqual(typeof tick, 'function', 'tickChannelTimes not exported');
  const now = Date.now();
  tick([
    { hash: QUOTE, lastActivityMs: now - 5000 },
    { hash: EVIL, lastActivityMs: now - 7000 },
    { hash: '#plain', lastActivityMs: now - 9000 },
  ], now);
  // Two selectors per channel, and each resolves to exactly the channel's hash.
  assert.deepStrictEqual(queries.map(q => q.value), [QUOTE, QUOTE, EVIL, EVIL, '#plain', '#plain']);
  assert.ok(queries.els.every(el => el.textContent !== ''), 'every row must be updated');
});

test('known-channels catalogue row escapes region, name and description', () => {
  const queries = []; queries.els = [];
  const w = loadChannels(queries);
  const row = w._channelsRenderKnownChannelRowForTest;
  assert.strictEqual(typeof row, 'function', 'renderKnownChannelRow not exported');
  const html = row({ channel: '#<img src=a>', description: '<img src=b>', region: '<img src=c onerror=__x=1>' });
  assert.ok(!/<img\b/i.test(html), 'raw <img survived: ' + html);
  assert.ok(html.includes('&lt;IMG SRC=C'), 'region not escaped: ' + html);
});

test('analytics channel table escapes a string channel hash', () => {
  const noop = () => {};
  const ctx = {
    console, Date, Math, JSON, Set, Map, Array, Object, Promise, Error, String, Number, RegExp, parseInt, isNaN,
    setTimeout, clearTimeout, setInterval, clearInterval, encodeURIComponent,
    document: { documentElement: {}, createElement: () => ({ style: {}, addEventListener: noop }), addEventListener: noop, removeEventListener: noop, querySelector: () => null, querySelectorAll: () => [], getElementById: () => null },
    localStorage: { getItem: () => null, setItem: noop, removeItem: noop },
    getComputedStyle: () => ({ getPropertyValue: () => '' }),
    registerPage: noop, api: async () => ({}), fetch: async () => ({ ok: true, json: async () => ({}) }),
    CLIENT_TTL: {}, RegionFilter: { getRegionParam: () => '' }, timeAgo: () => '', histogram: () => ({ svg: '' }),
  };
  ctx.window = ctx;
  vm.createContext(ctx);
  try { vm.runInContext(fs.readFileSync('public/analytics.js', 'utf8'), ctx, { filename: 'public/analytics.js' }); } catch (e) { /* exported helpers suffice */ }
  const tbody = ctx._analyticsChannelTbodyHtml;
  assert.strictEqual(typeof tbody, 'function', '_analyticsChannelTbodyHtml not exported');
  const html = tbody([{ name: '#x', hash: '"><img src=x onerror=__x=1>', messages: 1, senders: 1, lastActivity: null }], 'messages', 'desc');
  assert.ok(!/<img\b/i.test(html), 'raw <img survived: ' + html);
  assert.ok(!/ch="><img/.test(html), 'data-value broken out: ' + html);
  // A numeric hash byte 0 must still link to its channel.
  assert.ok(tbody([{ name: 'x', hash: 0, messages: 1, senders: 1 }], 'messages', 'desc').includes('#/channels?ch=0"'), 'hash 0 lost');
});

let failed = 0;
for (const t of tests) {
  try { t.fn(); console.log('  ✅ ' + t.name); }
  catch (e) { failed++; console.log('  ❌ ' + t.name + ': ' + e.message); }
}
console.log(`\nchannel name escaping: ${tests.length - failed} passed, ${failed} failed`);
process.exit(failed ? 1 : 0);
