/**
 * Unit tests (#790): while an end of the Path profile is still to be picked
 * — A, B, or a pending Move A / Move B — the panel is a compact bar: the
 * title, what to do next, Cancel and Close. Once both ends exist it is the
 * full panel again. public/terrain-profile.js, loaded for real in a
 * browser-shaped vm sandbox as test-issue-721-move-endpoints.js does.
 *
 * Covered:
 *   1. compactPhase follows state.a && state.b and the pending move;
 *   2. the panel after A only: compact, body hidden, the status says to
 *      pick the second point, Cancel shown, the copy button and the chips
 *      out of the bar;
 *   3. picking B expands it; a half link (setState with A only) is compact;
 *   4. Move A / Move B: compact while pending, expanded after the move or
 *      a cancel; Cancel during a move is cancelMove, not Close;
 *   5. Cancel while picking resets the profile (the panel closes, the hash
 *      drops the profile, the host's onClose runs);
 *   6. no empty verdict chip: the chip is hidden until it has a label.
 *
 * Usage: node test-issue-790-profile-compact.js
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
const asyncTests = [];
function atest(name, fn) { asyncTests.push([name, fn]); }
const sleep = (ms) => new Promise(r => setTimeout(r, ms));
const SETTLE_MS = 450;

const PK_A = 'a'.repeat(64);
const PK_B = 'b'.repeat(64);

function makeStorage() {
  const store = {};
  return {
    getItem: k => (Object.prototype.hasOwnProperty.call(store, k) ? store[k] : null),
    setItem: (k, v) => { store[k] = String(v); },
    removeItem: k => { delete store[k]; },
  };
}

function fakeDom() {
  const made = [];
  // #790 F1/F2: focus is tracked so the tests can follow activeElement.
  const holder = { doc: null };
  function mkEl(tag) {
    const on = {};
    const classes = new Set();
    const e = {
      tagName: String(tag).toUpperCase(), children: [], attrs: {}, style: {},
      className: '', textContent: '', innerHTML: '', value: '', hidden: false, disabled: false,
      parentNode: null,
      classList: {
        add: (c) => classes.add(c),
        remove: (c) => classes.delete(c),
        contains: (c) => classes.has(c),
        toggle: (c, want) => { const v = want === undefined ? !classes.has(c) : !!want; if (v) classes.add(c); else classes.delete(c); },
      },
      setAttribute: (k, v) => { e.attrs[k] = String(v); },
      getAttribute: (k) => (Object.prototype.hasOwnProperty.call(e.attrs, k) ? e.attrs[k] : null),
      appendChild: (c) => { e.children.push(c); c.parentNode = e; return c; },
      removeChild: (c) => { e.children = e.children.filter(x => x !== c); c.parentNode = null; return c; },
      addEventListener: (t, fn) => { (on[t] || (on[t] = [])).push(fn); },
      removeEventListener: (t, fn) => { on[t] = (on[t] || []).filter(f => f !== fn); },
      fire: (t) => { (on[t] || []).slice().forEach(fn => fn({ target: e, preventDefault() {} })); },
      focus: () => { holder.doc.activeElement = e; },
      contains: (x) => x === e || e.children.some(c => c.contains && c.contains(x)),
    };
    made.push(e);
    return e;
  }
  const body = mkEl('body');
  holder.doc = { createElement: mkEl, body: body, activeElement: body, addEventListener() {}, querySelector: () => null, querySelectorAll: () => [] };
  return {
    doc: holder.doc,
    body: body,
    find: (id) => made.filter(e => e.attrs['data-testid'] === id).pop() || null,
  };
}

function load() {
  const dom = fakeDom();
  const loc = { origin: 'http://x', pathname: '/', search: '', hash: '#/map?lat=56&lon=10&zoom=9' };
  const win = {
    addEventListener() {}, removeEventListener() {}, dispatchEvent() {},
    localStorage: makeStorage(), document: dom.doc, location: loc,
    history: {
      state: null,
      replaceState: (s, t, url) => { const hi = url.indexOf('#'); loc.hash = hi >= 0 ? url.slice(hi) : ''; },
    },
    setTimeout: (fn, ms) => setTimeout(fn, ms),
    clearTimeout: (t) => clearTimeout(t),
  };
  const ctx = {
    window: win, console, Math, Date, JSON, Array, Object, String, Number,
    RegExp, Error, TypeError, Boolean, isNaN, isFinite, parseInt, parseFloat,
    URLSearchParams, Map, Set, Promise, encodeURIComponent, decodeURIComponent,
    setTimeout, clearTimeout, document: dom.doc,
  };
  vm.createContext(ctx);
  vm.runInContext(fs.readFileSync('public/antenna-height.js', 'utf8'), ctx);
  vm.runInContext(fs.readFileSync('public/terrain-profile.js', 'utf8'), ctx);
  return { TP: ctx.window.TerrainProfile, dom, win, loc };
}

function recordingFetch() {
  const calls = [];
  const fn = (url) => {
    calls.push(url);
    const end = { kind: 'node', groundM: 0, antennaM: 10, antennaSource: 'role-default', assumed: true };
    return Promise.resolve({
      a: end, b: end, distanceM: 1000, model: 'both',
      coverage: { state: 'full', pct: 100 },
      samples: [{ d: 0, dtm: 0 }, { d: 500, dtm: 0 }, { d: 1000, dtm: 0 }],
      physics: { freqMHz: 869.618, freqSource: 'firmware', lambdaM: 0.34474, k: 1, f1MaxM: 9 },
      verdict: { state: 'clear', criterion: 0.6 },
    });
  };
  fn.calls = calls;
  return fn;
}

function openTool(env) {
  const fetchJSON = recordingFetch();
  const rec = { starts: [], cancels: [], closes: 0 };
  const tool = env.TP.create({
    mount: env.dom.body, fetchJSON,
    resolveNode: (pk) => ({ name: pk.slice(0, 4), lat: 56, lon: 10, role: 'repeater' }),
    onMoveStart: (side) => rec.starts.push(side),
    onMoveCancel: () => rec.cancels.push(true),
    onClose: () => { rec.closes++; },
  });
  return { tool, fetchJSON, rec };
}

/* The bar as a viewer sees it: what the compact rules and [hidden] leave. */
function bar(env) {
  const panel = env.dom.find('tp-panel');
  const status = env.dom.find('tp-pick-status');
  const cancel = env.dom.find('tp-cancel');
  return {
    compact: panel.classList.contains('tp-compact'),
    phase: panel.getAttribute('data-phase'),
    bodyHidden: env.dom.find('tp-body').hidden,
    status: status && !status.hidden ? status.textContent : null,
    cancel: !!(cancel && !cancel.hidden),
    copyHidden: env.dom.find('tp-copy-link').hidden,
  };
}

const NODE_A = { kind: 'node', pubkey: PK_A };
const NODE_B = { kind: 'node', pubkey: PK_B };

console.log('\n=== #790: compactPhase follows the ends and the pending move ===');

test('compactPhase: no end, A only, B only, both, and both with a move', () => {
  const { TP } = load();
  assert.strictEqual(typeof TP.compactPhase, 'function', 'compactPhase is not exported');
  assert.strictEqual(TP.compactPhase({ a: null, b: null }, null), 'pick-a');
  assert.strictEqual(TP.compactPhase(null, null), 'pick-a');
  assert.strictEqual(TP.compactPhase({ a: NODE_A, b: null }, null), 'pick-b');
  assert.strictEqual(TP.compactPhase({ a: null, b: NODE_B }, null), 'pick-a');
  assert.strictEqual(TP.compactPhase({ a: NODE_A, b: NODE_B }, null), null);
  assert.strictEqual(TP.compactPhase({ a: NODE_A, b: NODE_B }, 'a'), 'move-a');
  assert.strictEqual(TP.compactPhase({ a: NODE_A, b: NODE_B }, 'b'), 'move-b');
  assert.strictEqual(TP.compactPhase({ a: NODE_A, b: NODE_B }, 'x'), null, 'an unknown side is not a move');
});

console.log('\n=== #790: the panel while picking ===');

test('A only: compact bar with the next step, Cancel, no body, no copy button', () => {
  const env = load();
  const { tool } = openTool(env);
  tool.setEndpoint('a', NODE_A);
  const b = bar(env);
  assert.strictEqual(b.compact, true, 'not compact with A only');
  assert.strictEqual(b.phase, 'pick-b');
  assert.strictEqual(b.bodyHidden, true, 'the body (credit, A/B rows, steppers) is still shown');
  assert.strictEqual(b.status, 'Pick the second point on the map.');
  assert.strictEqual(b.cancel, true, 'no Cancel in the bar');
  assert.strictEqual(b.copyHidden, true, 'the copy button is in the bar');
  assert.strictEqual(env.dom.find('tp-pick-status').getAttribute('role'), 'status', 'the next step is not announced');
});

atest('B expands it: the body is back, the bar status and Cancel go', async () => {
  const env = load();
  const { tool, fetchJSON } = openTool(env);
  tool.setEndpoint('a', NODE_A);
  tool.setEndpoint('b', NODE_B);
  await sleep(0);
  const b = bar(env);
  assert.strictEqual(b.compact, false, 'still compact with both ends');
  assert.strictEqual(b.phase, 'ready', 'phase = ' + b.phase);
  assert.strictEqual(b.bodyHidden, false, 'the chart body is hidden with both ends');
  assert.strictEqual(b.status, null, 'the picking status stayed');
  assert.strictEqual(b.cancel, false, 'Cancel stayed');
  assert.strictEqual(b.copyHidden, false, 'the copy button did not come back');
  assert.strictEqual(fetchJSON.calls.length, 1, 'not measured once');
});

test('a half link (profile=<one key>, the node-detail entry) opens compact', () => {
  const env = load();
  const { tool } = openTool(env);
  // What map.js initProfileTool does with a half link: parseStart, then A.
  const start = env.TP.parseStart('profile=' + PK_A);
  assert.ok(start, 'precondition: parseStart reads the half link');
  tool.setEndpoint('a', start);
  const b = bar(env);
  assert.strictEqual(b.compact, true);
  assert.strictEqual(b.status, 'Pick the second point on the map.');
});

test('no end yet (opened empty) says to pick the first point', () => {
  const env = load();
  const { tool } = openTool(env);
  tool.open();
  const b = bar(env);
  assert.strictEqual(b.phase, 'pick-a');
  assert.strictEqual(b.status, 'Pick the first point on the map.');
});

console.log('\n=== #790: Move A / Move B ===');

atest('Move A is compact while pending and expands after the move', async () => {
  const env = load();
  const { tool, rec } = openTool(env);
  tool.setEndpoint('a', NODE_A);
  tool.setEndpoint('b', NODE_B);
  await sleep(0);
  env.dom.find('tp-move-a').fire('click');
  assert.deepStrictEqual(rec.starts, ['a']);
  let b = bar(env);
  assert.strictEqual(b.compact, true, 'not compact while Move A is pending');
  assert.strictEqual(b.phase, 'move-a');
  assert.strictEqual(b.status, 'Pick the new position for A on the map.');
  assert.strictEqual(b.cancel, true);
  assert.strictEqual(tool.moveEndpoint('a', { kind: 'point', lat: 56.1, lon: 10.2 }), true);
  b = bar(env);
  assert.strictEqual(b.compact, false, 'still compact after the move');
  assert.strictEqual(b.bodyHidden, false);
});

atest('Cancel during Move B cancels the move only: no request, panel open, expanded', async () => {
  const env = load();
  const { tool, fetchJSON, rec } = openTool(env);
  tool.setEndpoint('a', NODE_A);
  tool.setEndpoint('b', NODE_B);
  await sleep(0);
  tool.beginMove('b');
  assert.strictEqual(bar(env).status, 'Pick the new position for B on the map.');
  const before = fetchJSON.calls.length;
  env.dom.find('tp-cancel').fire('click');
  await sleep(SETTLE_MS);
  assert.strictEqual(tool.movingSide(), null, 'the move is still pending');
  assert.strictEqual(rec.cancels.length, 1, 'the host was not told to disarm');
  assert.strictEqual(rec.closes, 0, 'Cancel during a move closed the profile');
  assert.strictEqual(tool.isOpen(), true);
  assert.strictEqual(fetchJSON.calls.length, before, 'Cancel issued a request');
  assert.strictEqual(bar(env).compact, false, 'still compact after Cancel');
  const st = tool.getState();
  assert.strictEqual(st.b.pubkey, PK_B, 'B moved');
});

console.log('\n=== #790: Cancel while picking ===');

test('Cancel with A only resets the profile and keeps the page', () => {
  const env = load();
  const { tool, rec } = openTool(env);
  tool.setEndpoint('a', NODE_A);
  assert.ok(/profile=/.test(env.loc.hash), 'precondition: the half link is in the hash: ' + env.loc.hash);
  env.dom.find('tp-cancel').fire('click');
  assert.strictEqual(tool.isOpen(), false, 'the profile is still open');
  assert.strictEqual(rec.closes, 1, 'the host did not get onClose (picker disarm, line cleanup)');
  assert.strictEqual(tool.getState().a, null, 'A survived Cancel');
  assert.ok(!/profile=/.test(env.loc.hash), 'the hash still holds the profile: ' + env.loc.hash);
  assert.ok(/^#\/map\?/.test(env.loc.hash), 'the page left the Map: ' + env.loc.hash);
  // A new pick starts clean, compact.
  tool.setEndpoint('a', NODE_A);
  assert.strictEqual(bar(env).phase, 'pick-b');
});

console.log('\n=== #790: no empty chip pill ===');

atest('the verdict chip is hidden until it has a label', async () => {
  const env = load();
  const { tool } = openTool(env);
  tool.setEndpoint('a', NODE_A);
  const chip = env.dom.find('tp-verdict');
  assert.strictEqual(chip.textContent, '', 'precondition: no label yet');
  assert.strictEqual(chip.hidden, true, 'an empty chip pill is rendered');
  tool.setEndpoint('b', NODE_B);
  await sleep(0);
  assert.strictEqual(chip.hidden, false, 'the verdict chip stayed hidden with a verdict');
  assert.ok(chip.textContent.length > 0);
});

console.log('\n=== #790 F1/F2: focus through a keyboard Move ===');

async function readyWithFocus(side) {
  const env = load();
  const opened = openTool(env);
  opened.tool.setEndpoint('a', NODE_A);
  opened.tool.setEndpoint('b', NODE_B);
  await sleep(0);
  const move = env.dom.find('tp-move-' + side);
  // A keyboard user: focus on Move, then Enter (a click on the button).
  move.focus();
  move.fire('click');
  return Object.assign({ env, move }, opened);
}

for (const side of ['a', 'b']) {
  atest('Move ' + side.toUpperCase() + ' from the keyboard: focus goes to Cancel, Escape brings it back to Move', async () => {
    const { env, tool, move } = await readyWithFocus(side);
    assert.strictEqual(bar(env).phase, 'move-' + side);
    assert.strictEqual(env.dom.doc.activeElement, env.dom.find('tp-cancel'), 'focus was left in the hidden body, not on Cancel');
    tool.cancelMove(); // what map.js does on Escape
    assert.strictEqual(env.dom.doc.activeElement, move, 'focus after Escape: ' + (env.dom.doc.activeElement.attrs['data-testid'] || env.dom.doc.activeElement.tagName));
  });
  atest('Move ' + side.toUpperCase() + ' from the keyboard: Cancel brings focus back to Move', async () => {
    const { env, tool, move } = await readyWithFocus(side);
    env.dom.find('tp-cancel').fire('click');
    assert.strictEqual(tool.movingSide(), null);
    assert.strictEqual(env.dom.doc.activeElement, move, 'focus after Cancel: ' + (env.dom.doc.activeElement.attrs['data-testid'] || env.dom.doc.activeElement.tagName));
  });
}

atest('focus outside the panel is left alone when a move ends', async () => {
  const env = load();
  const { tool } = openTool(env);
  tool.setEndpoint('a', NODE_A);
  tool.setEndpoint('b', NODE_B);
  await sleep(0);
  tool.beginMove('a'); // a mouse/touch start: focus stays on <body>
  assert.strictEqual(env.dom.doc.activeElement, env.dom.body);
  tool.cancelMove();
  assert.strictEqual(env.dom.doc.activeElement, env.dom.body, 'focus was moved without the viewer being in the panel');
});

console.log('\n=== #790 F4: the fold toggle ===');

function foldView(env) {
  const fold = env.dom.find('tp-fold');
  const status = env.dom.find('tp-pick-status');
  return {
    shown: !!(fold && !fold.hidden),
    expanded: fold && fold.getAttribute('aria-expanded'),
    glyph: fold && fold.children[0] && fold.children[0].textContent,
    name: fold && fold.textContent,
    folded: env.dom.find('tp-panel').classList.contains('tp-folded'),
    bodyHidden: env.dom.find('tp-body').hidden,
    chipHidden: env.dom.find('tp-verdict').hidden,
    chip: env.dom.find('tp-verdict').textContent,
    status: status && !status.hidden ? status.textContent : null,
    copyHidden: env.dom.find('tp-copy-link').hidden,
    phase: env.dom.find('tp-panel').getAttribute('data-phase'),
  };
}

test('the toggle is not in the compact bar', () => {
  const env = load();
  const { tool } = openTool(env);
  tool.setEndpoint('a', NODE_A);
  assert.strictEqual(foldView(env).shown, false, 'the fold toggle is shown while picking');
  const plain = env.dom.find('tp-fold').parentNode.children[0];
  assert.strictEqual(plain.hidden, false, 'the compact bar has no title');
  assert.strictEqual(plain.textContent, 'Path profile');
});

atest('fold: the head keeps title, verdict chip and the verdict as status; unfold restores the panel', async () => {
  const env = load();
  const { tool, fetchJSON } = openTool(env);
  tool.setEndpoint('a', NODE_A);
  tool.setEndpoint('b', NODE_B);
  await sleep(0);
  let v = foldView(env);
  assert.strictEqual(v.shown, true, 'no fold toggle on a finished profile');
  assert.strictEqual(v.expanded, 'true');
  assert.strictEqual(v.glyph, '\u25BE');
  const fold = env.dom.find('tp-fold');
  assert.strictEqual(fold.tagName, 'BUTTON', 'the toggle is not a button');
  // The title is the toggle: its text is its name, no second title shown.
  assert.strictEqual(v.name, 'Path profile', 'the toggle is not the title: ' + v.name);
  assert.strictEqual(fold.getAttribute('aria-label'), null, 'an aria-label would hide the title text');
  assert.strictEqual(fold.parentNode.children[0].hidden, true, 'the plain title shows beside the toggle: the title reads twice');
  const calls = fetchJSON.calls.length;
  fold.fire('click');
  v = foldView(env);
  assert.strictEqual(v.expanded, 'false', 'aria-expanded did not follow');
  assert.strictEqual(v.glyph, '\u25B4');
  assert.strictEqual(v.folded, true);
  assert.strictEqual(v.bodyHidden, true, 'the body is still shown when folded');
  assert.strictEqual(v.chipHidden, false, 'the verdict chip went with the body');
  assert.ok(v.chip.length > 0);
  assert.strictEqual(v.status, 'The whole first Fresnel zone is clear of terrain.', 'folded status: ' + v.status);
  assert.strictEqual(v.copyHidden, false, 'the copy button left the folded head');
  assert.strictEqual(v.phase, 'ready');
  fold.fire('click');
  v = foldView(env);
  assert.strictEqual(v.expanded, 'true');
  assert.strictEqual(v.folded, false);
  assert.strictEqual(v.bodyHidden, false, 'unfold did not bring the body back');
  assert.strictEqual(v.status, null, 'the folded status stayed');
  assert.strictEqual(fetchJSON.calls.length, calls, 'folding issued a request');
});

atest('a profile closed folded opens unfolded next time', async () => {
  const env = load();
  const { tool } = openTool(env);
  tool.setEndpoint('a', NODE_A);
  tool.setEndpoint('b', NODE_B);
  await sleep(0);
  env.dom.find('tp-fold').fire('click');
  tool.close();
  tool.setEndpoint('a', NODE_A);
  tool.setEndpoint('b', NODE_B);
  await sleep(0);
  const v = foldView(env);
  assert.strictEqual(v.folded, false, 'still folded');
  assert.strictEqual(v.expanded, 'true');
  assert.strictEqual(v.bodyHidden, false);
});

(async () => {
  for (const [name, fn] of asyncTests) {
    try { await fn(); passed++; console.log('  ✅ ' + name); }
    catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
  }
  console.log(`\n#790 unit: ${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})();
