/**
 * #912 M1 — the Live map follows the 1-byte hop setting (Show / Mark / Hide).
 *
 * The setting lives in public/hop-filter.js (#674) and already applies to the
 * route view, the Live feed and the node path panel. These tests pin that the
 * Live map animation follows it too, against the real public/live.js,
 * public/hop-filter.js and public/hop-resolver.js in a vm sandbox:
 *
 *   1. the mode is read per packet: the visitor's stored choice, else the
 *      operator default (#912 M2, customizer.oneByteHopsDefault), else Show
 *      when no default has arrived or hop-filter.js is missing (owner
 *      decision on #912, 2026-11-27); a change (MC_set1ByteHopsMode fires
 *      mc-hide-1byte-hops-changed) reaches the next packet at once;
 *   2. Show: every hop reaches the animation, no segment is marked;
 *   3. Hide: 1-byte hops are left out, the server's resolved_path stays
 *      paired with the hop it belongs to, and when hops were dropped every
 *      remaining segment is marked (dashed + lighter, as route-render.js does);
 *      Hide on a path without 1-byte hops changes nothing;
 *   4. Mark: 1-byte hops stay and the segments that start or end at one are
 *      marked; the sender anchor and multi-byte hops are not;
 *   5. the input arrays are never mutated;
 *   6. perf (AGENTS.md rule 0): the filter plus resolveHopPositions on 10k
 *      synthetic paths stays within budget.
 *
 * Run: node test-issue-912-live-1byte-hops.js
 */
'use strict';
const fs = require('fs');
const path = require('path');
const vm = require('vm');
const assert = require('assert');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}

function makeStorage(seed) {
  const store = Object.assign({}, seed);
  return {
    getItem: k => (k in store ? store[k] : null),
    setItem: (k, v) => { store[k] = String(v); },
    removeItem: k => { delete store[k]; },
  };
}

// A browser-like sandbox with just enough DOM for live.js to load. Leaflet,
// the app shell and the WS channel are stubs; hop filtering and resolution
// are the real files.
// operatorDefault is handed over as roles.js does once /api/config/client
// arrives (MC_set1ByteHopsDefault); omitted, no client config has arrived.
function loadLive({ withHopFilter = true, storage = {}, operatorDefault } = {}) {
  const el = () => ({
    style: {}, dataset: {}, textContent: '', innerHTML: '',
    classList: { add() {}, remove() {}, contains() { return false; } },
    setAttribute() {}, getAttribute() { return null; }, addEventListener() {},
    appendChild() {}, getContext: () => ({}),
  });
  const listeners = {};
  const ctx = {
    window: {
      devicePixelRatio: 1,
      addEventListener(t, fn) { (listeners[t] = listeners[t] || []).push(fn); },
      removeEventListener() {},
      dispatchEvent(e) { (listeners[e.type] || []).forEach(fn => fn(e)); },
    },
    document: {
      readyState: 'complete', hidden: false,
      createElement: el, createElementNS: el,
      head: { appendChild() {} }, body: { appendChild() {}, removeChild() {}, contains: () => false },
      documentElement: { getAttribute: () => null, setAttribute() {} },
      getElementById: () => null, querySelector: () => null, querySelectorAll: () => [],
      addEventListener() {}, removeEventListener() {},
    },
    // hop-resolver.js logs every heuristic pick; keep the run readable.
    console: { log() {}, debug() {}, info() {}, warn: console.warn, error: console.error },
    Date, Infinity, Math, Array, Object, String, Number, JSON, RegExp,
    Error, TypeError, Map, Set, Promise, URLSearchParams,
    parseInt, parseFloat, isNaN, isFinite, encodeURIComponent, decodeURIComponent,
    setTimeout: () => 0, clearTimeout() {}, setInterval: () => 0, clearInterval() {},
    requestAnimationFrame: () => 0, cancelAnimationFrame() {},
    performance: { now: () => Date.now() },
    localStorage: makeStorage(storage),
    location: { hash: '', protocol: 'https:', host: 'localhost' },
    getComputedStyle: () => ({ getPropertyValue: () => '' }),
    matchMedia: () => ({ matches: false, addEventListener() {} }),
    navigator: {}, visualViewport: null,
    MutationObserver: function () { this.observe = () => {}; this.disconnect = () => {}; },
    WebSocket: function () { this.close = () => {}; },
    IATA_COORDS_GEO: {},
    AreaFilter: { init() {}, onChange() {}, areaQueryString: () => '' },
    RegionFilter: { init() {}, getSelected: () => null, onRegionChange() {}, nodesRegionQueryString: () => '', packetsRegionQueryString: () => '' },
    L: { polyline: () => { const p = { addTo() { return p; }, setStyle() {}, remove() {} }; return p; } },
    registerPage() {}, onWS() {}, offWS() {}, connectWS() {},
    api: () => Promise.resolve([]), invalidateApiCache() {},
    favStar: () => '', bindFavStars() {}, getFavorites: () => [], isFavorite: () => false,
    MeshAudio: null,
    fetch: () => new Promise(() => {}),
  };
  ctx.window.localStorage = ctx.localStorage;
  ctx.window.CustomEvent = function (type, init) { this.type = type; this.detail = init && init.detail; };
  ctx.CustomEvent = ctx.window.CustomEvent;
  vm.createContext(ctx);
  const files = ['payload-labels.js', 'roles.js', 'live-heard-filter.js', 'packet-helpers.js', 'hop-resolver.js', 'live.js'];
  if (withHopFilter) files.unshift('hop-filter.js');
  for (const f of files) {
    vm.runInContext(fs.readFileSync(path.join(__dirname, 'public', f), 'utf8'), ctx, { filename: f });
    for (const k of Object.keys(ctx.window)) ctx[k] = ctx.window[k];
  }
  if (operatorDefault !== undefined) ctx.window.MC_set1ByteHopsDefault(operatorDefault);
  return ctx;
}

// Sender A (GPS from its advert), a 1-byte hop to B, a 2-byte hop to C and a
// 3-byte hop to D. Every hop has a distinct position, so a dropped hop is
// visible as a missing vertex.
const A = { public_key: 'a1' + '0'.repeat(62), name: 'SENDER-A', role: 'companion', lat: 55.40, lon: 10.38 };
const B = { public_key: '18' + '1'.repeat(62), name: 'REPEATER-B', role: 'repeater', lat: 55.70, lon: 9.53 };
const C = { public_key: 'c2c2' + '2'.repeat(60), name: 'REPEATER-C', role: 'repeater', lat: 56.15, lon: 10.20 };
const D = { public_key: 'd3d3d3' + '3'.repeat(58), name: 'REPEATER-D', role: 'repeater', lat: 57.05, lon: 9.92 };
const PAYLOAD = { pubKey: A.public_key, name: A.name, lat: A.lat, lon: A.lon };

function setup(opts) {
  const ctx = loadLive(opts);
  const nodes = [A, B, C, D];
  ctx.HopResolver.init(nodes);
  ctx.window.HopResolver = ctx.HopResolver;
  const nodeData = ctx.window._liveNodeData();
  for (const n of nodes) nodeData[n.public_key] = Object.assign({}, n);
  return ctx;
}

const keys = out => out.map(p => p.key);
const marked = out => out.map(p => !!p.uncertain);

console.log('\n=== #912 M1: the mode Live reads ===');
{
  const ctx = setup();
  const mode = ctx.window._liveOneByteHopsMode;

  test('no stored mode and no operator default yet: the Live map uses Show; other pages keep hop-filter.js\'s default', () => {
    // Owner decision on #912 (2026-11-27): without an operator default, Live
    // draws as before #912.
    assert.strictEqual(typeof mode, 'function', '_liveOneByteHopsMode must be exposed');
    assert.strictEqual(ctx.localStorage.getItem('meshcore-1byte-hops-mode'), null, 'precondition: no choice');
    assert.strictEqual(mode(), 'show');
    assert.strictEqual(ctx.window.MC_get1ByteHopsMode(), 'mark', 'hop-filter.js DEFAULT_MODE is unchanged');
    assert.strictEqual(ctx.window.MC_getStored1ByteHopsMode(), null);
  });

  // #912 M2: once M2 lands the operator default decides the starting mode
  // (owner decision, 2026-11-27). Red with M1's `stored || 'show'` fallback.
  test('oneByteHopsDefault "hide" and a fresh profile: Hide on Live', () => {
    const fresh = loadLive({ operatorDefault: 'hide' });
    assert.strictEqual(fresh.localStorage.getItem('meshcore-1byte-hops-mode'), null, 'precondition: no choice');
    assert.strictEqual(fresh.window._liveOneByteHopsMode(), 'hide');
  });

  for (const dflt of ['show', 'mark']) {
    test('no stored mode: the operator default "' + dflt + '" applies on Live', () => {
      assert.strictEqual(loadLive({ operatorDefault: dflt }).window._liveOneByteHopsMode(), dflt);
    });
  }

  test('a stored choice beats the operator default on Live, Mark too', () => {
    const fresh = loadLive({ storage: { 'meshcore-1byte-hops-mode': 'mark' }, operatorDefault: 'hide' });
    assert.strictEqual(fresh.window._liveOneByteHopsMode(), 'mark');
  });

  test('a default that arrives late reaches the next packet; Use server default returns to it', () => {
    const fresh = loadLive();
    assert.strictEqual(fresh.window._liveOneByteHopsMode(), 'show', 'before the client config');
    fresh.window.MC_set1ByteHopsDefault('hide');
    assert.strictEqual(fresh.window._liveOneByteHopsMode(), 'hide', 'after the client config');
    fresh.window.MC_set1ByteHopsMode('show');
    assert.strictEqual(fresh.window._liveOneByteHopsMode(), 'show', 'a pick');
    fresh.window.MC_clear1ByteHopsMode();
    assert.strictEqual(fresh.window._liveOneByteHopsMode(), 'hide', 'back to the operator default');
  });

  test('an invalid operator default counts as none: Show on Live', () => {
    assert.strictEqual(loadLive({ operatorDefault: 'constructor' }).window._liveOneByteHopsMode(), 'show');
  });

  test('no stored mode: a 1-byte route is drawn unmarked on Live', () => {
    const out = ctx.window._liveResolveHopPositions(['18', 'C2C2'], PAYLOAD,
      [B.public_key, C.public_key], mode());
    assert.deepStrictEqual(keys(out), [A.public_key, B.public_key, C.public_key]);
    assert.deepStrictEqual(marked(out), [false, false, false]);
  });

  for (const stored of ['show', 'mark', 'hide']) {
    test('a stored "' + stored + '" from an earlier visit applies on Live', () => {
      const fresh = loadLive({ storage: { 'meshcore-1byte-hops-mode': stored } });
      assert.strictEqual(fresh.window._liveOneByteHopsMode(), stored);
    });
  }

  test('the #1633 "hide" key migrates and applies on Live', () => {
    const fresh = loadLive({ storage: { 'meshcore-hide-1byte-hops': 'true' } });
    assert.strictEqual(fresh.window._liveOneByteHopsMode(), 'hide');
  });

  test('a junk stored mode counts as no choice: the operator default on Live, else Show', () => {
    const withDefault = loadLive({ storage: { 'meshcore-1byte-hops-mode': 'constructor' }, operatorDefault: 'hide' });
    assert.strictEqual(withDefault.window._liveOneByteHopsMode(), 'hide');
    const without = loadLive({ storage: { 'meshcore-1byte-hops-mode': 'constructor' } });
    assert.strictEqual(without.window._liveOneByteHopsMode(), 'show');
  });

  test('a mode change reaches the next packet at once (MC_set1ByteHopsMode)', () => {
    let fired = null;
    ctx.window.addEventListener('mc-hide-1byte-hops-changed', e => { fired = e.detail.mode; });
    ctx.window.MC_set1ByteHopsMode('hide');
    assert.strictEqual(fired, 'hide', 'the setter fires mc-hide-1byte-hops-changed');
    assert.strictEqual(mode(), 'hide');
    ctx.window.MC_set1ByteHopsMode('show');
    assert.strictEqual(mode(), 'show');
    ctx.window.MC_set1ByteHopsMode('mark');
    assert.strictEqual(mode(), 'mark');
  });

  test('without hop-filter.js the Live map behaves as Show', () => {
    const bare = loadLive({ withHopFilter: false });
    assert.strictEqual(bare.window._liveOneByteHopsMode(), 'show');
  });
}

console.log('\n=== #912 M1: which hops reach the animation, and which are marked ===');
{
  const ctx = setup();
  const resolve = ctx.window._liveResolveHopPositions;
  const hops = ['18', 'C2C2', 'D3D3D3'];
  const rp = [B.public_key, C.public_key, D.public_key];

  test('Show: every hop reaches the animation, nothing is marked', () => {
    const out = resolve(hops, PAYLOAD, rp, 'show');
    assert.deepStrictEqual(keys(out), [A.public_key, B.public_key, C.public_key, D.public_key]);
    assert.deepStrictEqual(marked(out), [false, false, false, false]);
  });

  test('no mode argument behaves as Show (existing callers unchanged)', () => {
    const out = resolve(hops, PAYLOAD, rp);
    assert.deepStrictEqual(keys(out), [A.public_key, B.public_key, C.public_key, D.public_key]);
    assert.deepStrictEqual(marked(out), [false, false, false, false]);
  });

  test('Hide: the 1-byte hop is left out of the animated path', () => {
    const out = resolve(hops, PAYLOAD, rp, 'hide');
    assert.deepStrictEqual(keys(out), [A.public_key, C.public_key, D.public_key]);
    assert.ok(!out.some(p => p.pos[0] === B.lat && p.pos[1] === B.lon), 'no vertex may sit on B');
  });

  test('Hide: when hops were dropped every remaining segment is marked', () => {
    const out = resolve(hops, PAYLOAD, rp, 'hide');
    assert.deepStrictEqual(marked(out), [true, true, true]);
    for (let i = 0; i < out.length - 1; i++) {
      assert.strictEqual(ctx.window._liveIsUncertainSegment(out[i], out[i + 1]), true, 'segment ' + i);
    }
  });

  test('Hide: resolved_path stays paired with its own hop after the drop', () => {
    // A 1-byte hop between two multi-byte ones: the server answer for the
    // last hop must not shift onto the middle one.
    const out = resolve(['C2C2', '18', 'D3D3D3'], PAYLOAD, [C.public_key, B.public_key, D.public_key], 'hide');
    assert.deepStrictEqual(keys(out), [A.public_key, C.public_key, D.public_key]);
  });

  test('Hide: the server answer wins over the heuristic for a shared multi-byte prefix', () => {
    // C and CX share "c2c2". The heuristic picks C (next to the sender); the
    // server said CX. After the 1-byte hop is dropped CX must still win, so
    // resolved_path has to be filtered in step with the hops.
    const CX = { public_key: 'c2c2' + '9'.repeat(60), name: 'REPEATER-CX', role: 'repeater', lat: 57.70, lon: 11.95 };
    ctx.HopResolver.init([A, B, C, D, CX]);
    ctx.window._liveNodeData()[CX.public_key] = Object.assign({}, CX);
    const heuristic = resolve(['C2C2'], PAYLOAD, null, 'show');
    const out = resolve(['18', 'C2C2'], PAYLOAD, [B.public_key, CX.public_key], 'hide');
    ctx.HopResolver.init([A, B, C, D]);
    delete ctx.window._liveNodeData()[CX.public_key];
    assert.strictEqual(heuristic[1].key, C.public_key, 'precondition: without the server the heuristic picks C');
    assert.deepStrictEqual(keys(out), [A.public_key, CX.public_key]);
  });

  test('Hide: a resolved_path of the wrong length is not realigned by the drop', () => {
    // resolved_path [C] does not match the 2 hops, so it is ignored. After
    // the drop only one hop is left; [C] must not become its answer.
    const E = { public_key: 'e5e5' + '5'.repeat(60), name: 'REPEATER-E', role: 'repeater', lat: 54.90, lon: 9.80 };
    ctx.HopResolver.init([A, B, C, D, E]);
    ctx.window._liveNodeData()[E.public_key] = Object.assign({}, E);
    const out = resolve(['18', 'E5E5'], PAYLOAD, [C.public_key], 'hide');
    ctx.HopResolver.init([A, B, C, D]);
    assert.deepStrictEqual(keys(out), [A.public_key, E.public_key], JSON.stringify(keys(out)));
  });

  test('Hide with no 1-byte hops changes nothing', () => {
    const mb = ['C2C2', 'D3D3D3'], mbrp = [C.public_key, D.public_key];
    const show = resolve(mb, PAYLOAD, mbrp, 'show');
    const hide = resolve(mb, PAYLOAD, mbrp, 'hide');
    assert.deepStrictEqual(hide, show);
    assert.deepStrictEqual(marked(hide), [false, false, false]);
  });

  test('Mark: the 1-byte hop stays, and only its own position is marked', () => {
    const out = resolve(hops, PAYLOAD, rp, 'mark');
    assert.deepStrictEqual(keys(out), [A.public_key, B.public_key, C.public_key, D.public_key]);
    assert.deepStrictEqual(marked(out), [false, true, false, false]);
  });

  test('Mark: the segments that start or end at a 1-byte hop are marked, the rest not', () => {
    const out = resolve(hops, PAYLOAD, rp, 'mark');
    const seg = i => ctx.window._liveIsUncertainSegment(out[i], out[i + 1]);
    assert.deepStrictEqual([seg(0), seg(1), seg(2)], [true, true, false],
      'A→B and B→C touch the 1-byte hop; C→D does not');
  });

  test('Mark with no 1-byte hops marks nothing', () => {
    const out = resolve(['C2C2', 'D3D3D3'], PAYLOAD, [C.public_key, D.public_key], 'mark');
    assert.deepStrictEqual(marked(out), [false, false, false]);
  });

  test('an unknown mode behaves as Show', () => {
    const out = resolve(hops, PAYLOAD, rp, 'bogus');
    assert.deepStrictEqual(keys(out), [A.public_key, B.public_key, C.public_key, D.public_key]);
    assert.deepStrictEqual(marked(out), [false, false, false, false]);
  });

  test('the input hops and resolved_path are never mutated', () => {
    const h = hops.slice(), r = rp.slice();
    resolve(h, PAYLOAD, r, 'hide');
    resolve(h, PAYLOAD, r, 'mark');
    assert.deepStrictEqual(h, hops);
    assert.deepStrictEqual(r, rp);
  });

  test('the follow (#880) still sees every hop position in Hide', () => {
    // resolveHopEntries is the per-position view a follow uses; the setting
    // only shapes what is animated.
    ctx.window.MC_set1ByteHopsMode('hide');
    const out = ctx.window._liveResolveHopEntries(hops, PAYLOAD, rp);
    ctx.window.MC_set1ByteHopsMode('mark');
    assert.deepStrictEqual(keys(out), [B.public_key, C.public_key, D.public_key]);
  });
}

console.log('\n=== #912 M1: perf — filter + resolveHopPositions on 10k paths ===');
{
  // AGENTS.md rule 0: the WS hot path. 2,000 repeaters (the production
  // scale), 10,000 paths of 1–8 hops mixing 1-, 2- and 3-byte hashes; 80 %
  // carry a server resolved_path, the rest go through the heuristic.
  const ctx = loadLive();
  const nodes = [];
  let seed = 912;
  const rnd = () => (seed = (seed * 1103515245 + 12345) & 0x7fffffff) / 0x7fffffff;
  for (let i = 0; i < 2000; i++) {
    const pk = (i * 2654435761 >>> 0).toString(16).padStart(8, '0') + i.toString(16).padStart(56, '0');
    nodes.push({ public_key: pk, name: 'N' + i, role: 'repeater', lat: 54 + rnd() * 3, lon: 8 + rnd() * 4 });
  }
  ctx.HopResolver.init(nodes);
  ctx.window.HopResolver = ctx.HopResolver;
  const nodeData = ctx.window._liveNodeData();
  for (const n of nodes) nodeData[n.public_key] = n;
  const paths = [];
  for (let i = 0; i < 10000; i++) {
    const len = 1 + Math.floor(rnd() * 8);
    const size = 1 + Math.floor(rnd() * 3);
    const hops = [], rp = [];
    for (let j = 0; j < len; j++) {
      const n = nodes[Math.floor(rnd() * nodes.length)];
      hops.push(n.public_key.slice(0, size * 2).toUpperCase());
      rp.push(n.public_key);
    }
    const s = nodes[Math.floor(rnd() * nodes.length)];
    paths.push({ hops, rp: rnd() < 0.8 ? rp : null, payload: { pubKey: s.public_key, lat: s.lat, lon: s.lon } });
  }
  const resolve = ctx.window._liveResolveHopPositions;
  const filter = ctx.window._liveApplyOneByteHopsMode;
  function time(fn) {
    const t0 = process.hrtime.bigint();
    fn();
    return Number(process.hrtime.bigint() - t0) / 1e6;
  }
  const full = mode => time(() => { for (const p of paths) resolve(p.hops, p.payload, p.rp, mode); });
  const filterOnly = mode => time(() => { for (const p of paths) filter(p.hops, p.rp, mode); });
  // Interleaved, best of 5 after a warm-up, so machine noise hits every mode.
  const best = { show: Infinity, hide: Infinity, mark: Infinity };
  const bestFilter = { hide: Infinity, mark: Infinity };
  for (let r = 0; r < 6; r++) {
    for (const m of ['show', 'hide', 'mark']) {
      const ms = full(m);
      if (r > 0) best[m] = Math.min(best[m], ms);
    }
    for (const m of ['hide', 'mark']) {
      const ms = filterOnly(m);
      if (r > 0) bestFilter[m] = Math.min(bestFilter[m], ms);
    }
  }
  console.log(`  filter+resolve: show ${best.show.toFixed(1)} ms · hide ${best.hide.toFixed(1)} ms · mark ${best.mark.toFixed(1)} ms`);
  console.log(`  filter alone:   hide ${bestFilter.hide.toFixed(1)} ms · mark ${bestFilter.mark.toFixed(1)} ms (10k paths, 2k nodes)`);

  test('the filter alone handles 10k paths within 250 ms and 15 % of the resolve', () => {
    assert.strictEqual(typeof filter, 'function', '_liveApplyOneByteHopsMode must be exposed');
    for (const m of ['hide', 'mark']) {
      assert.ok(bestFilter[m] < 250, `${m}: ${bestFilter[m].toFixed(1)} ms over the 250 ms budget`);
      assert.ok(bestFilter[m] <= best.show * 0.15,
        `${m}: ${bestFilter[m].toFixed(1)} ms is over 15 % of the ${best.show.toFixed(1)} ms resolve`);
    }
  });

  // About 60–125 ms per mode locally and in CI, so the 6 s cap (and the
  // 250 ms filter cap above) only catch a gross regression such as a hang.
  // The relative checks are what catch a regression in the filter.
  test('filter + resolve: 10k paths within 6 s per mode', () => {
    for (const m of ['show', 'hide', 'mark']) {
      assert.ok(best[m] < 6000, `${m}: ${best[m].toFixed(1)} ms over the 6000 ms budget`);
    }
  });

  test('filter + resolve in Hide / Mark costs at most 50 % + 20 ms over Show', () => {
    for (const m of ['hide', 'mark']) {
      assert.ok(best[m] <= best.show * 1.5 + 20, `${m}: ${best[m].toFixed(1)} ms vs show ${best.show.toFixed(1)} ms`);
    }
  });
}

console.log('\n--- ' + passed + ' passed, ' + failed + ' failed ---\n');
process.exit(failed > 0 ? 1 : 0);
