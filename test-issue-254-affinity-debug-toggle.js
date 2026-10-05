/**
 * #254: the node page's Affinity Debug card (shown only with debugAffinity, e.g.
 * localStorage meshcore-affinity-debug=true) folds its body in and out.
 *
 * Its heading used to carry an inline onclick whose value held the Phosphor
 * sprite markup with unescaped double quotes, so the browser cut the attribute
 * short and the handler was a syntax error: the body could never open. Its
 * carets were also reversed against the #189 convention (collapsed caret-right,
 * expanded caret-down).
 *
 * Tests the real nodes.js in a vm sandbox:
 * - the card renders a disclosure <button> (aria-expanded="false",
 *   aria-controls on the hidden body, caret-right) and no inline handler;
 * - toggling opens (true, body shown, caret-down) and closes again;
 * - the delegated click handler toggles from a click inside the button and
 *   ignores other clicks.
 *
 * Usage: node test-issue-254-affinity-debug-toggle.js
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

function loadNodes() {
  const noop = () => {};
  const ctx = {
    window: { addEventListener: noop, removeEventListener: noop, dispatchEvent: noop },
    document: {
      readyState: 'complete',
      createElement: () => ({ id: '', textContent: '', innerHTML: '', style: {} }),
      head: { appendChild: noop },
      getElementById: () => null,
      addEventListener: noop,
      removeEventListener: noop,
      querySelectorAll: () => [],
      querySelector: () => null,
    },
    console, Date, Math, Array, Object, String, Number, JSON, RegExp, Error, TypeError,
    Map, Set, Promise, URLSearchParams, parseInt, parseFloat, isNaN, isFinite,
    encodeURIComponent, decodeURIComponent,
    setTimeout: noop, clearTimeout: noop, setInterval: noop, clearInterval: noop,
    fetch: () => Promise.resolve({ ok: true, json: () => Promise.resolve({}) }),
    performance: { now: () => Date.now() },
    localStorage: { getItem: () => null, setItem: noop, removeItem: noop },
    location: { hash: '' },
    CustomEvent: class CustomEvent {},
    requestAnimationFrame: noop,
    // Globals nodes.js reads at load time.
    ROLE_COLORS: {}, ROLE_STYLE: {}, TYPE_COLORS: {},
    getNodeStatus: () => 'active',
    getHealthThresholds: () => ({ staleMs: 1, degradedMs: 2, silentMs: 3 }),
    timeAgo: () => '', truncate: (s) => s, escapeHtml: (s) => String(s == null ? '' : s),
    payloadTypeName: () => '', payloadTypeColor: () => '',
    registerPage: noop,
    RegionFilter: { init: noop, onChange: () => noop, getRegionParam: () => '' },
    debouncedOnWS: () => null, onWS: noop, offWS: noop, debounce: (fn) => fn,
    api: () => Promise.resolve({}), invalidateApiCache: noop,
    CLIENT_TTL: { nodeList: 1, nodeDetail: 1, nodeHealth: 1 },
    initTabBar: noop, getFavorites: () => [], favStar: () => '', bindFavStars: noop,
    makeColumnsResizable: noop,
  };
  vm.createContext(ctx);
  vm.runInContext(fs.readFileSync('public/nodes.js', 'utf8'), ctx, { filename: 'public/nodes.js' });
  return ctx.window;
}

// Attributes of the first start tag in html that matches re.
function startTag(html, re) {
  const m = re.exec(html);
  assert(m, 'no start tag matching ' + re);
  return m[0];
}
function attr(tag, name) {
  const m = new RegExp('\\s' + name + '(?:="([^"]*)")?(?=[\\s>/])').exec(tag);
  return m ? (m[1] === undefined ? '' : m[1]) : null;
}
function caretsIn(html) {
  return (html.match(/#ph-caret-[a-z]+/g) || []).map((s) => s.slice('#ph-'.length));
}

// A minimal DOM for the rendered card: the toggle button, its .toggle-icon and
// the body it controls, seeded from the rendered markup.
function cardDom(html) {
  const btnTag = startTag(html, /<button\b[^>]*class="[^"]*affinity-debug-toggle[^"]*"[^>]*>/);
  const bodyId = attr(btnTag, 'aria-controls');
  const bodyTag = startTag(html, new RegExp('<div\\b[^>]*\\sid="' + bodyId + '"[^>]*>'));
  const iconHtml = /<span class="toggle-icon">([\s\S]*?)<\/span>/.exec(html);
  assert(iconHtml, 'the button has a .toggle-icon');
  const body = { id: bodyId, hidden: attr(bodyTag, 'hidden') !== null };
  const icon = { innerHTML: iconHtml[1] };
  const doc = { getElementById: (id) => (id === bodyId ? body : null) };
  const attrs = { 'aria-expanded': attr(btnTag, 'aria-expanded'), 'aria-controls': bodyId };
  const btn = {
    ownerDocument: doc,
    getAttribute: (k) => (k in attrs ? attrs[k] : null),
    setAttribute: (k, v) => { attrs[k] = String(v); },
    querySelector: (sel) => (sel === '.toggle-icon' ? icon : null),
    closest: (sel) => (sel === '.affinity-debug-toggle' ? btn : null),
  };
  return { btn, body, icon, attrs };
}

const w = loadNodes();
const render = w._nodesRenderAffinityDebugCard;
const toggle = w._nodesToggleAffinityDebug;
const onClick = w._nodesOnFullBodyClick;

console.log('\n=== #254 Affinity Debug card: markup ===');

test('nodes.js exposes the card renderer, the toggle and the delegated click handler', () => {
  assert.strictEqual(typeof render, 'function', '_nodesRenderAffinityDebugCard');
  assert.strictEqual(typeof toggle, 'function', '_nodesToggleAffinityDebug');
  assert.strictEqual(typeof onClick, 'function', '_nodesOnFullBodyClick');
});

const html = typeof render === 'function' ? render() : '';

test('the card is hidden until debugAffinity shows it', () => {
  const card = startTag(html, /<div\b[^>]*\sid="node-affinity-debug"[^>]*>/);
  assert(/display:\s*none/.test(attr(card, 'style') || ''), card);
});

test('no inline event handler anywhere in the card', () => {
  const handlers = html.match(/\son[a-z]+\s*=/gi);
  assert(!handlers, 'inline handler(s): ' + JSON.stringify(handlers));
});

test('the heading holds a disclosure button, collapsed, controlling the hidden body', () => {
  const h4 = /<h4\b[^>]*>([\s\S]*?)<\/h4>/.exec(html);
  assert(h4, 'the card has an <h4> heading');
  const btnTag = startTag(h4[1], /<button\b[^>]*>/);
  assert.strictEqual(attr(btnTag, 'type'), 'button', btnTag);
  assert(/\baffinity-debug-toggle\b/.test(attr(btnTag, 'class') || ''), btnTag);
  assert.strictEqual(attr(btnTag, 'aria-expanded'), 'false', btnTag);
  const bodyId = attr(btnTag, 'aria-controls');
  assert(bodyId, 'aria-controls names the body');
  const bodyTag = startTag(html, new RegExp('<div\\b[^>]*\\sid="' + bodyId + '"[^>]*>'));
  assert(/\baffinity-debug-body\b/.test(attr(bodyTag, 'class') || ''), bodyTag);
  assert(attr(bodyTag, 'hidden') !== null, 'the body starts hidden: ' + bodyTag);
  assert(/Affinity Debug/.test(h4[1]), 'the button is labelled Affinity Debug');
  assert(/id="affinityDebugContent"/.test(html), 'the body keeps #affinityDebugContent for the loader');
});

test('collapsed shows exactly one caret, caret-right', () => {
  const icon = /<span class="toggle-icon">([\s\S]*?)<\/span>/.exec(html);
  assert(icon, 'the button has a .toggle-icon');
  assert.deepStrictEqual(caretsIn(icon[1]), ['caret-right']);
  assert.deepStrictEqual(caretsIn(html), ['caret-right'], 'no other caret in the card');
});

console.log('\n=== #254 Affinity Debug card: toggle ===');

test('the toggle opens the body: aria-expanded true, caret-down', () => {
  const d = cardDom(html);
  assert.strictEqual(toggle(d.btn), true, 'returns the new state');
  assert.strictEqual(d.attrs['aria-expanded'], 'true');
  assert.strictEqual(d.body.hidden, false, 'body shown');
  assert.deepStrictEqual(caretsIn(d.icon.innerHTML), ['caret-down']);
});

test('a second toggle closes it again: aria-expanded false, caret-right', () => {
  const d = cardDom(html);
  toggle(d.btn);
  assert.strictEqual(toggle(d.btn), false, 'returns the new state');
  assert.strictEqual(d.attrs['aria-expanded'], 'false');
  assert.strictEqual(d.body.hidden, true, 'body hidden');
  assert.deepStrictEqual(caretsIn(d.icon.innerHTML), ['caret-right']);
});

test('the delegated click handler toggles from a click inside the button', () => {
  const d = cardDom(html);
  onClick({ target: { closest: (sel) => d.btn.closest(sel) } });
  assert.strictEqual(d.attrs['aria-expanded'], 'true');
  assert.strictEqual(d.body.hidden, false);
  onClick({ target: { closest: (sel) => d.btn.closest(sel) } });
  assert.strictEqual(d.attrs['aria-expanded'], 'false');
  assert.strictEqual(d.body.hidden, true);
});

test('the delegated click handler ignores clicks elsewhere on the node page', () => {
  const d = cardDom(html);
  onClick({ target: { closest: () => null } });
  onClick({ target: {} });
  assert.strictEqual(d.attrs['aria-expanded'], 'false');
  assert.strictEqual(d.body.hidden, true);
});

console.log(`\n${passed} passed, ${failed} failed`);
process.exit(failed === 0 ? 0 : 1);
