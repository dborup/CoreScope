#!/usr/bin/env node
/*
 * scripts/bench-hop-resolution.js — #165 perf proof for the packets page.
 *
 * Times the initial-load hop resolution of the REAL public/packets.js on a
 * synthetic ~30K packet load, in a vm sandbox (network stubbed):
 *   - 2000 repeaters in three regions, 42 observers (27 with coordinates),
 *   - 0-8 hops per packet (85% 1-byte, 15% 2-byte), 30% with resolved_path
 *     (RP_SHARE=0.95 for the share the grouped rows carry since PR #185 F1,
 *     after #190; 80% of a resolved_path's hops are non-null either way).
 *
 * Reports the median of 7 runs of:
 *   - total time from cacheResolvedPaths() to the last resolved hop,
 *   - the longest main-thread block (max gap of a setTimeout(0) chain),
 *   - the cache size left behind.
 *
 * Works on any checkout. Point it at a public/ dir from before #165
 * (e.g. `git worktree add /tmp/m origin/master`) for the baseline: when
 * packets.js has no resolveHopsForPackets() it runs the old sequence
 * (one resolveHops() over all hops) instead.
 *
 * Usage: [RP_SHARE=0.3] node scripts/bench-hop-resolution.js [publicDir]   (default: public)
 */
'use strict';
const fs = require('fs');
const path = require('path');
const vm = require('vm');

const PUBLIC = path.resolve(process.argv[2] || path.join(__dirname, '..', 'public'));
const RUNS = 7;
const RP_SHARE = Number(process.env.RP_SHARE || 0.3);

// Deterministic data (mulberry32).
function rng(a) {
  return () => {
    a |= 0; a = a + 0x6D2B79F5 | 0;
    let t = Math.imul(a ^ a >>> 15, 1 | a);
    t = t + Math.imul(t ^ t >>> 7, 61 | t) ^ t;
    return ((t ^ t >>> 14) >>> 0) / 4294967296;
  };
}
const r = rng(42);
const hex = n => Array.from({ length: n }, () => Math.floor(r() * 256).toString(16).padStart(2, '0')).join('');
const centers = [[51.1, 3.7], [50.85, 4.35], [51.2, 4.4]];
const nodes = Array.from({ length: 2000 }, (_, i) => {
  const c = centers[i % 3];
  return { public_key: hex(32), name: 'N' + i, role: 'repeater', lat: c[0] + (r() - 0.5), lon: c[1] + (r() - 0.5) };
});
const observers = Array.from({ length: 42 }, (_, i) => {
  const c = centers[i % 3];
  return i < 27
    ? { id: 'OBS' + i, iata: 'X' + (i % 3), lat: c[0] + (r() - 0.5) * 0.5, lon: c[1] + (r() - 0.5) * 0.5 }
    : { id: 'OBS' + i, iata: 'X' + (i % 3), lat: null, lon: null };
});
const iataCoords = { X0: { lat: 51.1, lon: 3.7 }, X1: { lat: 50.85, lon: 4.35 }, X2: { lat: 51.2, lon: 4.4 } };
const packets = Array.from({ length: 30000 }, (_, i) => {
  const len = Math.floor(r() * 9), bytes = r() < 0.85 ? 1 : 2;
  const hops = Array.from({ length: len }, () => nodes[Math.floor(r() * nodes.length)].public_key.slice(0, bytes * 2).toUpperCase());
  const p = { id: i, hash: 'h' + i, observer_id: observers[Math.floor(r() * observers.length)].id, path_json: JSON.stringify(hops) };
  if (r() < RP_SHARE) {
    p.resolved_path = JSON.stringify(hops.map(h => (r() < 0.8
      ? ((nodes.find(n => n.public_key.toUpperCase().startsWith(h)) || {}).public_key || null) : null)));
  }
  return p;
});

function load() {
  const store = {};
  const ctx = {
    window: { addEventListener() {}, dispatchEvent() {}, innerWidth: 1280 },
    document: {
      readyState: 'complete',
      createElement: () => ({ style: {}, dataset: {}, classList: { add() {}, remove() {} }, appendChild() {}, setAttribute() {} }),
      head: { appendChild() {} }, body: { appendChild() {}, removeChild() {} },
      getElementById: () => null, querySelector: () => null, querySelectorAll: () => [],
      addEventListener() {}, removeEventListener() {},
    },
    localStorage: { getItem: k => (k in store ? store[k] : null), setItem: (k, v) => { store[k] = String(v); }, removeItem() {} },
    console: { log() {}, warn() {}, error: console.error },
    Math, Object, Array, Number, Date, Map, Set, JSON, String, Boolean, RegExp, Error, TypeError, Promise,
    parseInt, parseFloat, isNaN, isFinite, encodeURIComponent, decodeURIComponent, URLSearchParams,
    setTimeout, clearTimeout, setInterval: () => 0, clearInterval() {},
    requestAnimationFrame: () => 0, cancelAnimationFrame() {}, performance, location: { hash: '' },
    escapeHtml: s => String(s),
    registerPage() {}, onWS() {}, offWS() {}, debouncedOnWS: f => f, CLIENT_TTL: {},
    fetchAllNodes: () => Promise.resolve({ nodes }),
    api: p => Promise.resolve(p.startsWith('/observers') ? { observers } : p.startsWith('/iata-coords') ? { coords: iataCoords } : {}),
  };
  ctx.window.localStorage = ctx.localStorage;
  vm.createContext(ctx);
  let legacy = false;
  for (const f of ['payload-labels.js', 'packet-helpers.js', 'hop-resolver.js', 'hop-display.js', 'packets.js']) {
    let src = fs.readFileSync(path.join(PUBLIC, f), 'utf8');
    if (f === 'packets.js' && !/async function resolveHopsForPackets\(/.test(src)) {
      // Before #165: expose the two functions the old initial load used.
      legacy = true;
      src = src.replace('window._packetsTestAPI = {',
        'window._packetsTestAPI = { resolveHops, cacheResolvedPaths, _hopCacheSize: () => Object.keys(hopNameCache).length,');
    }
    vm.runInContext(src, ctx, { filename: f });
    for (const k of Object.keys(ctx.window)) ctx[k] = ctx.window[k];
  }
  return { T: ctx.window._packetsTestAPI, ctx, legacy };
}

(async () => {
  const times = [], blocks = [];
  let size = 0, legacyMode = false;
  for (let run = 0; run < RUNS; run++) {
    const { T, ctx, legacy } = load();
    legacyMode = legacy;
    const pk = packets.map(p => ({ ...p }));
    await T.cacheResolvedPaths(pk.slice(0, 1)); // HopResolver.init outside the timing
    let maxGap = 0, last = performance.now(), on = true;
    (function tick() { const n = performance.now(); maxGap = Math.max(maxGap, n - last); last = n; if (on) setTimeout(tick, 0); })();
    const t0 = performance.now();
    await T.cacheResolvedPaths(pk);
    if (legacy) {
      const all = new Set();
      for (const p of pk) { try { ctx.getParsedPath(p).forEach(h => all.add(h)); } catch (e) { /* skip */ } }
      if (all.size) await T.resolveHops([...all]);
    } else {
      await T.resolveHopsForPackets(pk);
    }
    const t1 = performance.now();
    await new Promise(res => setTimeout(res, 0)); // let the last gap be recorded
    on = false;
    times.push(t1 - t0); blocks.push(maxGap); size = T._hopCacheSize();
  }
  const median = a => a.slice().sort((x, y) => x - y)[Math.floor(a.length / 2)];
  console.log(JSON.stringify({
    publicDir: PUBLIC, mode: legacyMode ? 'single resolve (pre-#165)' : 'per observer (#165)',
    packets: packets.length, observers: observers.length, nodes: nodes.length, runs: RUNS, rp_share: RP_SHARE,
    total_ms_median: +median(times).toFixed(1), longest_block_ms_median: +median(blocks).toFixed(1),
    cache_entries: size,
  }));
})();
