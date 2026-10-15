/**
 * #480 — channel messages sent without a region scope get a compact amber
 * "no scope" marker (like the #353 1-byte path-hash recommendation).
 *
 * Covers the ONE shared decision + markup helper in public/app.js
 * (isUnscopedFlood / renderNoScope), its use on the Channels message meta
 * line, and the theming contract. The packet-detail Scope row is covered in
 * test-packets.js, the browser behaviour in test-issue-480-no-scope-e2e.js.
 *
 * Run: node test-issue-480-no-scope.js
 */
'use strict';
const vm = require('vm');
const fs = require('fs');
const assert = require('assert');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}

// Extract the REAL functions from public/app.js (no hand-rolled copies).
function loadAppFunctions(names) {
  const source = fs.readFileSync('public/app.js', 'utf8');
  let extracted = '';
  for (const name of names) {
    const start = source.search(new RegExp('^function ' + name + '\\s*\\(', 'm'));
    if (start < 0) throw new Error('public/app.js no longer declares function ' + name);
    const open = source.indexOf('{', start);
    let depth = 0, end = -1;
    for (let i = open; i < source.length; i++) {
      if (source[i] === '{') depth++;
      else if (source[i] === '}' && --depth === 0) { end = i + 1; break; }
    }
    extracted += source.slice(start, end) + '\n';
  }
  const ctx = { Number, String };
  vm.createContext(ctx);
  vm.runInContext(extracted + 'this.__fns = {' + names.join(',') + '};', ctx);
  return ctx.__fns;
}
const helpers = loadAppFunctions(['escapeHtml', 'isUnscopedFlood', 'renderNoScope']);
const { isUnscopedFlood, renderNoScope } = helpers;

console.log('\n=== #480 isUnscopedFlood ===');

test('FLOOD (1) is unscoped, including the string form a JSON field can carry', () => {
  assert.strictEqual(isUnscopedFlood(1), true);
  assert.strictEqual(isUnscopedFlood('1'), true);
});

test('TRANSPORT_FLOOD (0) is scoped, whether or not the name is known', () => {
  // The scope NAME is deliberately not an input: a scoped transmission with an
  // unknown name has scope_name = '' and is NOT "no scope".
  assert.strictEqual(isUnscopedFlood(0), false);
  assert.strictEqual(isUnscopedFlood('0'), false);
  assert.strictEqual(isUnscopedFlood.length, 1);
});

test('DIRECT (2) and TRANSPORT_DIRECT (3) are not flood scope: no marker', () => {
  assert.strictEqual(isUnscopedFlood(2), false);
  assert.strictEqual(isUnscopedFlood(3), false);
});

test('missing or invalid route types are not unscoped', () => {
  for (const v of [null, undefined, '', 'abc', 4, -1, 1.5, true, {}, [1]]) {
    assert.strictEqual(isUnscopedFlood(v), false, 'for ' + JSON.stringify(v));
  }
});

console.log('\n=== #480 renderNoScope ===');

test('unscoped flood renders the amber marker: icon, "no scope", explanation', () => {
  const html = renderNoScope(1);
  assert.match(html, /^<span class="no-scope path-hash-warn" title="/);
  assert.match(html, /#ph-warning/);
  assert.match(html, /aria-hidden="true"/);
  assert.match(html, /no scope/);
  assert.match(html, /title="Sent without a scope\. The message floods outside your region, and repeaters that limit unscoped traffic \(flood\.max\.unscoped \/ region denyf\) may stop it\. Recommended: set a default scope \(region\) in the companion app\."/);
  assert.ok(!/error|danger/i.test(html), 'a recommendation, not an error');
});

test('the accessible text carries the explanation, not just the tooltip', () => {
  assert.match(renderNoScope(1), /<span class="sr-only"> — sent without a scope\. Recommended: set a default scope[^<]*<\/span>/);
});

test('does not claim the missing scope explains a low heard-count', () => {
  assert.ok(!/heard|repeats|lower|fewer/i.test(renderNoScope(1)));
});

test('scoped, direct and invalid route types render nothing', () => {
  for (const v of [0, 2, 3, null, undefined, 'x', 4]) {
    assert.strictEqual(renderNoScope(v), '', 'for ' + String(v));
  }
});

console.log('\n=== #480 channels meta line ===');

const channelsSrc = fs.readFileSync('public/channels.js', 'utf8');
function makeChannelsSandbox(withHelper) {
  const noop = () => {};
  const fakeEl = () => ({
    addEventListener: noop, querySelector: () => null, querySelectorAll: () => [],
    classList: { add: noop, remove: noop, toggle: noop, contains: () => false },
    appendChild: noop, removeChild: noop, setAttribute: noop, getAttribute: () => null,
    textContent: '', innerHTML: '', style: {}, dataset: {}, scrollTop: 0, scrollHeight: 0,
  });
  const chMessagesEl = fakeEl();
  const doc = {
    readyState: 'complete', createElement: fakeEl, head: fakeEl(), body: fakeEl(),
    documentElement: fakeEl(),
    getElementById: (id) => (id === 'chMessages' ? chMessagesEl : null),
    querySelector: () => null, querySelectorAll: () => [], addEventListener: noop,
  };
  const ctx = {
    window: { addEventListener: noop, matchMedia: () => ({ matches: false, addListener: noop, addEventListener: noop }) },
    document: doc, console, Date, Math, JSON, Set, Map, Array, Object, Promise, Response: function () {}, Error,
    setTimeout, clearTimeout, setInterval, clearInterval,
    history: { replaceState: noop, pushState: noop },
    location: { hash: '', href: '', pathname: '/' },
    navigator: { userAgent: 'node' },
    RegionFilter: { getRegionParam: () => '' },
    api: () => Promise.resolve({ messages: [] }),
    CLIENT_TTL: {}, ChannelDecrypt: undefined,
    truncate: (s) => s, formatHashHex: (h) => String(h),
    channelDisplayName: (c) => c && c.name,
    escapeHtml: helpers.escapeHtml,
    getSenderColor: () => '#123456',
    fetch: () => Promise.resolve({ json: () => Promise.resolve({}) }),
    btoa: (s) => Buffer.from(String(s), 'binary').toString('base64'),
    isTransportRoute: (rt) => rt === 0 || rt === 3,
    registerPage: noop,
  };
  if (withHelper) ctx.renderNoScope = renderNoScope;
  vm.createContext(ctx);
  vm.runInContext(channelsSrc, ctx);
  return { ctx, chMessagesEl };
}
function render(msg, withHelper = true) {
  const { ctx, chMessagesEl } = makeChannelsSandbox(withHelper);
  ctx.window._channelsSetStateForTest({ messages: [Object.assign({
    sender: 'Alice', text: 'hello', timestamp: '2026-01-15T10:00:00Z' }, msg)] });
  ctx.window._channelsRenderMessagesForTest();
  return chMessagesEl.innerHTML;
}

test('an unscoped FLOOD message shows the marker where "scope: #dk" would be', () => {
  const html = render({ routeType: 1, scope: null });
  assert.match(html, /class="no-scope path-hash-warn"/);
  assert.ok(!html.includes('scope: '), 'no "scope: …" text on an unscoped message');
});

test('a scoped message with a name is unchanged: "scope: #dk", no marker', () => {
  const html = render({ routeType: 0, scope: '#dk' });
  assert.ok(html.includes('scope: #dk'));
  assert.ok(!html.includes('no-scope'));
});

test('a scoped message with an unknown name stays "scope: unknown", not "no scope"', () => {
  const html = render({ routeType: 0, scope: '' });
  assert.ok(html.includes('scope: unknown'));
  assert.ok(!html.includes('no-scope'));
});

test('DIRECT and TRANSPORT_DIRECT messages get no marker', () => {
  assert.ok(!render({ routeType: 2, scope: null }).includes('no-scope'));
  assert.ok(!render({ routeType: 3, scope: '#dk' }).includes('no-scope'));
});

test('a message without a route type gets no marker', () => {
  assert.ok(!render({ scope: null }).includes('no-scope'));
});

test('the marker is guarded when app.js has not provided the helper', () => {
  assert.ok(!render({ routeType: 1 }, false).includes('no-scope'));
});

test('the meta line uses the shared helper, not a private copy', () => {
  assert.ok(channelsSrc.includes('renderNoScope('), 'channels.js must call renderNoScope');
  assert.ok(!/no scope/.test(channelsSrc.replace(/\/\/.*$/gm, '')), 'no inline "no scope" markup in channels.js');
});

console.log('\n=== #480 theming ===');

const css = fs.readFileSync('public/style.css', 'utf8');
test('the marker reuses --path-hash-warn and hardcodes no colour', () => {
  const rule = css.match(/\.no-scope\s*\{[^}]*\}/);
  assert.ok(rule, '.no-scope rule exists');
  assert.ok(!/#[0-9a-fA-F]{3,8}\b|rgb\(|hsl\(/.test(rule[0]), 'no hardcoded colour: ' + rule[0]);
  assert.ok(/\.path-hash-warn\s*\{[^}]*color:\s*var\(--path-hash-warn\)/.test(css));
});

console.log('\n' + passed + ' passed, ' + failed + ' failed');
process.exit(failed ? 1 : 0);
