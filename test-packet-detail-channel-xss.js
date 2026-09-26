#!/usr/bin/env node
/* XSS regression for channel data in the packet detail (PR #99 review).
 *
 * Loads the REAL public/packets.js (plus the real app.js escapeHtml) into a
 * vm context and renders the detail message block, the list preview and the
 * byte-breakdown row with a malicious channel name / channel hash. Since
 * shared channel names can be suggested by anyone and NormalizeName allows
 * < > " ', the channel label must be escaped wherever it becomes HTML.
 * Removing any of the escapeHtml calls in those sinks turns a test red. */
'use strict';

const assert = require('assert');
const fs = require('fs');
const vm = require('vm');

const PAYLOAD = '#<img src=x onerror=__x=1>'; // the review's 26-byte name
const ATTR = "#\"'><svg onload=__x=1>";

function loadPackets() {
  const noop = () => {};
  const el = () => ({
    id: '', textContent: '', innerHTML: '', className: '', style: {},
    appendChild: noop, setAttribute: noop, addEventListener: noop,
    querySelectorAll: () => [], querySelector: () => null,
    classList: { add: noop, remove: noop, contains: () => false },
  });
  const store = {};
  const ctx = {
    console, Date, Math, JSON, Object, Array, String, Number, RegExp, Error, TypeError, RangeError,
    Map, Set, Promise, URLSearchParams, Infinity,
    parseInt, parseFloat, isNaN, isFinite, encodeURIComponent, decodeURIComponent,
    setTimeout: noop, clearTimeout: noop, setInterval: noop, clearInterval: noop,
    requestAnimationFrame: noop,
    fetch: () => Promise.resolve({ ok: true, json: () => Promise.resolve({}) }),
    performance: { now: () => Date.now() },
    localStorage: { getItem: k => store[k] || null, setItem: (k, v) => { store[k] = String(v); }, removeItem: k => { delete store[k]; } },
    location: { hash: '' },
    history: { replaceState: noop },
    CustomEvent: class CustomEvent {},
    addEventListener: noop, removeEventListener: noop, dispatchEvent: noop,
    registerPage: noop,
    document: {
      readyState: 'complete', createElement: el, head: { appendChild: noop }, body: { appendChild: noop },
      getElementById: () => null, addEventListener: noop, removeEventListener: noop,
      querySelectorAll: () => [], querySelector: () => null,
    },
  };
  ctx.window = { addEventListener: noop, removeEventListener: noop, dispatchEvent: noop, innerWidth: 1200 };
  vm.createContext(ctx);
  for (const file of ['public/payload-labels.js', 'public/roles.js', 'public/app.js', 'public/packet-helpers.js']) {
    vm.runInContext(fs.readFileSync(file, 'utf8'), ctx, { filename: file });
    Object.assign(ctx, ctx.window);
  }
  vm.runInContext('window.HopDisplay = { renderHop: function (h) { return h; }, _showFromBtn: function () {} };', ctx);
  vm.runInContext(fs.readFileSync('public/packets.js', 'utf8'), ctx, { filename: 'public/packets.js' });
  Object.assign(ctx, ctx.window);
  assert.ok(ctx._packetsTestAPI, '_packetsTestAPI not exposed');
  return ctx._packetsTestAPI;
}

function assertEscaped(html, label) {
  assert.ok(!/<img\b/i.test(html), `${label}: raw <img survived: ${html}`);
  assert.ok(!/<svg onload/i.test(html), `${label}: raw <svg onload survived: ${html}`);
  assert.ok(!/"'>/.test(html), `${label}: attribute breakout survived: ${html}`);
}

const api = loadPackets();
const tests = [];
function test(name, fn) { tests.push({ name, fn }); }

test('detail message: channel name is escaped and shown as text', () => {
  const html = api.buildDetailMessageHtml({ text: 'hi', channel: PAYLOAD, path_len: 2 }, 5);
  assertEscaped(html, 'channel');
  assert.ok(html.includes('#&lt;img src=x onerror=__x=1&gt;'), html);
  assert.ok(html.includes('2 hops') && html.includes('SNR 5 dB'), html);
});

test('detail message: attribute-breaking channel name is escaped', () => {
  assertEscaped(api.buildDetailMessageHtml({ text: 'hi', channel: ATTR }, null), 'attr');
});

test('detail message: channel_idx, path_len and snr from data are escaped', () => {
  const html = api.buildDetailMessageHtml({ text: 'hi', channel_idx: '<img src=x>', path_len: '<img src=y>' }, '<img src=z>');
  assertEscaped(html, 'meta parts');
});

test('detail message: message text stays escaped', () => {
  assertEscaped(api.buildDetailMessageHtml({ text: PAYLOAD, channel: '#ok' }, null), 'text');
});

test('detail message: legitimate channel renders unchanged', () => {
  const html = api.buildDetailMessageHtml({ text: 'hello', channel: '#test', path_len: 1 }, -3.5);
  assert.ok(html.includes('#test · 1 hops · SNR -3.5 dB'), html);
});

test('detail message: undecrypted GRP_TXT escapes a data-supplied channel hash', () => {
  const html = api.buildDetailMessageHtml({ type: 'GRP_TXT', channelHash: 1, channelHashHex: '<img src=x>', decryptionStatus: 'no_key' }, null);
  assertEscaped(html, 'GRP_TXT hash');
  assert.ok(html.includes('0x&lt;img'), html);
});

test('list preview: data-supplied channel hash is escaped (GRP_TXT and GRP_DATA)', () => {
  assertEscaped(api.getDetailPreview({ type: 'GRP_TXT', channelHash: 1, channelHashHex: '<img src=x>', decryptionStatus: 'no_key' }), 'GRP_TXT preview');
  assertEscaped(api.getDetailPreview({ type: 'GRP_DATA', channelHash: 1, channelHashHex: '<img src=x>' }), 'GRP_DATA preview');
});

test('byte breakdown: data-supplied GRP_TXT channel hash is escaped', () => {
  const pkt = { raw_hex: '1500' + '11' + 'aabb' + '00'.repeat(16), route_type: 1, payload_type: 5 };
  const html = api.buildFieldTable(pkt, { type: 'GRP_TXT', channelHashHex: '<img src=x>', decryptionStatus: 'no_key' }, [], {});
  assertEscaped(html, 'field table');
  assert.ok(html.includes('0x&lt;img'), html);
});

let failed = 0;
for (const t of tests) {
  try { t.fn(); console.log('  ✅ ' + t.name); }
  catch (e) { failed++; console.log('  ❌ ' + t.name + ': ' + e.message); }
}
console.log(`\npacket detail channel XSS: ${tests.length - failed} passed, ${failed} failed`);
process.exit(failed ? 1 : 0);
