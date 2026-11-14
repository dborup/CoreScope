/* test-issue-724-masts-hint.js — #724 M4: the interference hint in the
 * terrain profile panel (public/masts-hint.js) and the cell pinning it uses
 * in public/masts-layer.js.
 *
 * vm sandbox + recording Leaflet stub (tests/map-overlay-sandbox.js); the
 * real files run, nothing is copied. /api/masts is a stub function.
 *
 * Covered:
 *   1. distance tiers at the boundaries (99/100/499/500/501 m);
 *   2. only the 700/800/900 MHz bands count; a missing band never triggers,
 *      not even on a site the server flags (GSM-R with no band);
 *   3. the cells around an endpoint: complete for the 500 m circle, at most
 *      four, inside the server extent;
 *   4. the wording: "especially within 100 m", the strong tier, no legal
 *      claims, no claim that interference will happen;
 *   5. both ends: the nearer end leads the chip, both are named;
 *   6. the data path: the same /api/masts?cell= cells through the layer's
 *      own loader, fetched while both overlays are off and below the zoom
 *      gate, never a cell twice, never a request for an unchanged end;
 *   7. the panel: a chip beside the other chips, text only, hidden with no
 *      hint; hostile site fields never reach it;
 *   8. CSS variables only, map.js and index.html wiring.
 *
 * Runs via: node test-issue-724-masts-hint.js
 */
'use strict';
const fs = require('fs');
const path = require('path');
const vm = require('vm');
const assert = require('assert');
const { makeSandbox, buildControl } = require('./tests/map-overlay-sandbox');

let passed = 0, failed = 0;
const pending = [];
function test(name, fn) {
  try {
    const r = fn();
    if (r && typeof r.then === 'function') {
      pending.push(r.then(() => { passed++; console.log('  ✅ ' + name); },
        (e) => { failed++; console.log('  ❌ ' + name + ': ' + e.message); }));
      return;
    }
    passed++; console.log('  ✅ ' + name);
  } catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}

const MASTS = {
  enabled: true, url: '/api/masts', minZoom: 13, cellSizeM: 2000,
  attribution: 'Antenna positions: Mastedatabasen, Digitaliseringsstyrelsen (CC0)',
  sourceUrl: 'https://mastedatabasen.dk/', cellExtent: [219, 3017, 462, 3209],
};
const HINT_SRC = fs.readFileSync(path.join(__dirname, 'public', 'masts-hint.js'), 'utf8');

function sandbox(opts) {
  const ctx = makeSandbox(Object.assign({ masts: MASTS }, opts || {}));
  vm.runInContext(HINT_SRC, ctx);
  return ctx;
}
const ctx0 = sandbox();
const H = ctx0.window.MastsHint;
const tick = () => new Promise((r) => setImmediate(r));

// A point `m` metres due north of (lat, lon), on the hint's own sphere.
const R = 6371000;
const north = (lat, lon, m) => [lat + m / R * 180 / Math.PI, lon];
const A = [56.1629, 10.2039];
const B = [56.2000, 10.4000];
const site = (ll, entries, extra) => Object.assign({ lat: ll[0], lon: ll[1], stations: ['S1'], entries, interference: true }, extra || {});
const LTE800 = [{ technology: 'LTE', bandMHz: 800, interference: true }];

console.log('\n=== #724 M4 interference hint ===');

// ── 1. Distance tiers ───────────────────────────────────────────────────────

test('distance tiers at the boundaries: 99/100 strong, 499/500 near, 501 none', () => {
  assert.strictEqual(H.HINT_DISTANCE_M, 500);
  assert.strictEqual(H.HINT_STRONG_M, 100);
  const want = { 99: 'strong', 100: 'strong', 101: 'near', 499: 'near', 500: 'near', 501: null };
  for (const d of Object.keys(want).map(Number)) {
    const got = H.nearestFor(A[0], A[1], [site(north(A[0], A[1], d), LTE800)]);
    assert.strictEqual(got ? got.tier : null, want[d], d + ' m → ' + JSON.stringify(got));
    if (got) assert.strictEqual(got.distanceM, d, 'distance ' + got.distanceM + ' for ' + d);
  }
});

test('distanceM is the great-circle distance (east-west at 56° N)', () => {
  const d = H.distanceM(56, 10, 56, 10.01);
  assert(Math.abs(d - 621.8) < 1, 'distance ' + d);
  assert.strictEqual(H.distanceM(56, 10, 56, 10), 0);
});

test('the nearest relevant site wins, wherever it is in the list', () => {
  const far = site(north(A[0], A[1], 450), LTE800);
  const near = site(north(A[0], A[1], 220), [{ technology: 'GSM', bandMHz: 900, interference: true }]);
  const got = H.nearestFor(A[0], A[1], [far, near, far]);
  assert.strictEqual(got.distanceM, 220);
  assert.deepStrictEqual(Array.from(got.bands), [900]);
});

// ── 2. Bands ────────────────────────────────────────────────────────────────

test('only 700/800/900 MHz count; other bands and a missing band never trigger', () => {
  const at = north(A[0], A[1], 50);
  const none = [
    [{ technology: 'LTE', bandMHz: 1800 }, { technology: '5G-NR', bandMHz: 3500 }, { technology: 'UMTS', bandMHz: 2100 }],
    [{ technology: 'LTE' }],
    // The server flags GSM-R whatever the band; with no band published the
    // hint has nothing to say about it.
    [{ technology: 'GSM-R', interference: true }],
    [{ technology: 'LTE', bandMHz: 0 }],
    [{ technology: 'LTE', bandMHz: '800' }],
    [],
  ];
  for (const entries of none) {
    assert.strictEqual(H.nearestFor(A[0], A[1], [site(at, entries)]), null, JSON.stringify(entries));
  }
  for (const b of [700, 800, 900]) {
    const got = H.nearestFor(A[0], A[1], [site(at, [{ technology: 'LTE', bandMHz: 1800 }, { technology: 'LTE', bandMHz: b }], { interference: false })]);
    assert(got && got.tier === 'strong', b + ' MHz did not count');
    assert.deepStrictEqual(Array.from(got.bands), [b]);
  }
  const multi = H.relevantBands(site(at, [{ bandMHz: 900 }, { bandMHz: 700 }, { bandMHz: 900 }, { bandMHz: 2600 }]));
  assert.deepStrictEqual(Array.from(multi), [700, 900], 'sorted, distinct');
});

test('a site with a broken position is ignored, not a crash', () => {
  const bad = [{ lat: 'x', lon: 10, entries: LTE800 }, { lat: null, lon: null, entries: LTE800 }, { lat: A[0], lon: A[1] }];
  assert.strictEqual(H.nearestFor(A[0], A[1], bad), null);
});

// ── 3. Cells around an endpoint ─────────────────────────────────────────────

test('cellsNear covers the 500 m circle, at most four cells, inside the extent', () => {
  const ML = ctx0.window.MastsLayer;
  for (let i = 0; i < 400; i++) {
    const lat = 54.8 + (i * 0.0071) % 2.6, lon = 8.3 + (i * 0.0193) % 4.2;
    const keys = H.cellsNear(lat, lon, MASTS);
    assert(keys.length >= 1 && keys.length <= 4, lat + ',' + lon + ' → ' + keys.length + ' cells');
    for (let k = 0; k < 16; k++) {
      const a = k / 16 * 2 * Math.PI;
      const p = ML.toUTM32(lat + 499 * Math.cos(a) / R * 180 / Math.PI, lon + 499 * Math.sin(a) / (R * Math.cos(lat * Math.PI / 180)) * 180 / Math.PI);
      const key = Math.floor(p.e / 2000) + ',' + Math.floor(p.n / 2000);
      assert(keys.includes(key), 'cell ' + key + ' at 499 m from ' + lat + ',' + lon + ' not listed: ' + keys);
    }
  }
  const p = ML.toUTM32(A[0], A[1]);
  assert.strictEqual(H.cellsNear(A[0], A[1], MASTS)[0], Math.floor(p.e / 2000) + ',' + Math.floor(p.n / 2000));
  assert.strictEqual(H.cellsNear(52.52, 13.40, MASTS).length, 0, 'Berlin is outside the extent');
});

// ── 4. Wording ──────────────────────────────────────────────────────────────

const LEGAL = /\b(legal|illegal|unlawful|law|prohibited|forbidden|violat\w*|must not|not allowed|licen[cs]e|permit)\b/i;

test('the hint says "especially within 100 m", names the band and makes no claim', () => {
  const near = H.hintFor({ a: { distanceM: 320, bands: [800], tier: 'near' }, b: null });
  assert.strictEqual(near.tier, 'near');
  assert.strictEqual(near.label, 'Cell transmitter ~320 m');
  assert(/^A: an 800 MHz cellular transmitter about 320 m away\./.test(near.detail), near.detail);
  assert(/Within about 500 m, especially within 100 m, /.test(near.detail), near.detail);
  assert(/may desensitise an unfiltered 868 MHz receiver/.test(near.detail));
  assert(/band-pass filter or another site/.test(near.detail));
  assert(/distance only/.test(near.detail) && /no height or power/.test(near.detail));
  assert(!/most within/.test(near.detail));
  const strong = H.hintFor({ a: null, b: { distanceM: 40, bands: [700, 900], tier: 'strong' } });
  assert.strictEqual(strong.tier, 'strong');
  assert.strictEqual(strong.label, 'Cell transmitter ~40 m');
  assert(/^B: a 700\/900 MHz cellular transmitter about 40 m away, possibly on the same building or mast\./.test(strong.detail), strong.detail);
  for (const h of [near, strong]) {
    assert(!LEGAL.test(h.detail + h.label), 'legal wording: ' + h.detail);
    assert(!/\bwill (interfere|desensiti[sz]e|block)/i.test(h.detail), 'claims interference will happen: ' + h.detail);
  }
  assert.strictEqual(H.hintFor({ a: null, b: null }), null);
});

test('the popup hint (M3) now reads "especially within 100 m" too (#758 F4)', () => {
  const html = ctx0.window.MastsLayer.popupHTML(site(A, LTE800), MASTS);
  assert(/Within about 500 m, especially within 100 m, a cellular transmitter like this may desensitise/.test(html), html);
  assert(!/most within/.test(html));
});

// ── 5. Both ends ────────────────────────────────────────────────────────────

test('both ends: the nearer one leads the chip and both are named, A first', () => {
  const h = H.hintFor({ a: { distanceM: 450, bands: [800], tier: 'near' }, b: { distanceM: 60, bands: [900], tier: 'strong' } });
  assert.strictEqual(h.tier, 'strong');
  assert.strictEqual(h.label, 'Cell transmitter ~60 m');
  assert(/^A: .*450 m away\. B: .*60 m away, possibly on the same building or mast\./.test(h.detail), h.detail);
});

// ── 6. Data path ────────────────────────────────────────────────────────────

function liveHarness(view) {
  const ctx = sandbox({ storage: { 'mc-masts': '0' } }); // both overlays off
  const timers = [];
  ctx.setTimeout = (fn) => { timers.push(fn); return fn; };
  const requests = [];
  const sitesFor = {};
  ctx.fetch = (url) => {
    requests.push(url);
    const key = decodeURIComponent(url.split('cell=')[1]);
    return Promise.resolve({ ok: true, status: 200, headers: { get: () => null },
      json: () => Promise.resolve({ truncated: false, sites: sitesFor[key] || [] }) });
  };
  const { map } = buildControl(ctx);
  map.getZoom = () => view.z;
  map.getBounds = () => ({ getSouth: () => view.b.south, getWest: () => view.b.west, getNorth: () => view.b.north, getEast: () => view.b.east });
  const flush = async () => { for (let i = 0; i < 6; i++) { while (timers.length) timers.shift()(); await tick(); } };
  return { ctx, map, requests, sitesFor, flush };
}

function fakePanel() {
  const made = [];
  const mk = (cls) => {
    const el = { className: cls || '', attrs: {}, hidden: false, title: '', textContent: '', children: [], parentNode: null,
      setAttribute(k, v) { el.attrs[k] = String(v); },
      insertBefore(c, ref) { const i = ref ? el.children.indexOf(ref) : -1; c.parentNode = el; if (i < 0) el.children.push(c); else el.children.splice(i, 0, c); return c; },
      appendChild(c) { c.parentNode = el; el.children.push(c); return c; },
    };
    Object.defineProperty(el, 'nextSibling', { get() { const p = el.parentNode; if (!p) return null; return p.children[p.children.indexOf(el) + 1] || null; } });
    Object.defineProperty(el, 'innerHTML', { get() { return el._html || ''; }, set(v) { el._html = v; el.innerHTMLWrites = (el.innerHTMLWrites || 0) + 1; } });
    return el;
  };
  const panel = mk('tp-panel');
  const head = panel.appendChild(mk('tp-head'));
  head.appendChild(mk('tp-title'));
  head.appendChild(mk('tp-chip'));
  head.appendChild(mk('tp-chip tp-chip-assumed'));
  const copy = head.appendChild(mk('tp-close tp-copy'));
  head.appendChild(mk('tp-close'));
  const body = panel.appendChild(mk('tp-body'));
  const detail = body.appendChild(mk('tp-detail'));
  body.appendChild(mk('tp-surface-note'));
  const find = (root, cls) => {
    if ((' ' + root.className + ' ').indexOf(' ' + cls + ' ') >= 0) return root;
    for (const c of root.children) { const r = find(c, cls); if (r) return r; }
    return null;
  };
  panel.querySelector = (sel) => find(panel, sel.replace(/^\./, ''));
  panel.ownerDocument = { createElement: () => { const e = mk(''); made.push(e); return e; } };
  return { panel, head, copy, detail, made, find };
}

test('the hint fetches the endpoint cells through the layer loader, overlays off and below the gate', async () => {
  const view = { z: 9, b: { south: 55.5, west: 9.5, north: 56.8, east: 11.0 } };
  const h = liveHarness(view);
  const keysA = Array.from(H.cellsNear(A[0], A[1], MASTS));
  h.sitesFor[keysA[0]] = [site(north(A[0], A[1], 320), LTE800)];
  const fp = fakePanel();
  const hint = H.attach({ map: h.map, cfg: MASTS, panel: () => fp.panel });
  hint.update(A, B);
  await h.flush();
  const want = new Set(keysA.concat(Array.from(H.cellsNear(B[0], B[1], MASTS))));
  const asked = h.requests.map((u) => { assert(/^\/api\/masts\?cell=\d+%2C\d+$/.test(u), 'request ' + u); return decodeURIComponent(u.split('cell=')[1]); });
  assert.deepStrictEqual(new Set(asked), want, 'asked ' + asked);
  assert.strictEqual(asked.length, want.size, 'a cell was asked twice');
  const chip = fp.find(fp.head, 'tp-chip-mast');
  assert(chip && !chip.hidden, 'no chip');
  assert.strictEqual(chip.textContent, 'Cell transmitter ~320 m');

  // The same ends again (a redraw): no new request, same chip.
  hint.update(A, B);
  await h.flush();
  assert.strictEqual(h.requests.length, want.size, 'an unchanged end was asked again');

  // Away and back: the cells are kept by the layer's LRU, not asked again.
  hint.update([56.3, 10.6], [56.31, 10.6]);
  await h.flush();
  const n = h.requests.length;
  hint.update(A, B);
  await h.flush();
  assert.strictEqual(h.requests.length, n, 'a kept cell was asked again');
  assert.strictEqual(chip.textContent, 'Cell transmitter ~320 m');
  // Nothing was drawn on the map for the hint's cells: both overlays are off.
  assert.strictEqual(Array.from(h.map.layers).filter((l) => l.kind === 'circleMarker').length, 0);
});

test('far from any relevant site: no chip; clear() hides it and unpins', async () => {
  const view = { z: 14, b: { south: 56.15, west: 10.2, north: 56.16, east: 10.215 } };
  const h = liveHarness(view);
  const keysA = Array.from(H.cellsNear(A[0], A[1], MASTS));
  h.sitesFor[keysA[0]] = [site(north(A[0], A[1], 501), LTE800), site(north(A[0], A[1], 20), [{ technology: 'LTE', bandMHz: 1800 }])];
  const fp = fakePanel();
  const hint = H.attach({ map: h.map, cfg: MASTS, panel: () => fp.panel });
  hint.update(A, B);
  await h.flush();
  const chip = fp.find(fp.head, 'tp-chip-mast');
  assert(!chip || chip.hidden, 'chip shown for a site 501 m away or an 1800 MHz one');
  h.sitesFor[keysA[0]] = [site(north(A[0], A[1], 80), LTE800)];
  hint.clear();
  assert.strictEqual(h.map._mcMasts.stats().wanted, 0, 'clear() left cells pinned');
});

test('a pinned cell the view also needs is fetched once and drawn when the layer is on', async () => {
  const ctx = sandbox();
  const timers = [];
  ctx.setTimeout = (fn) => { timers.push(fn); return fn; };
  const requests = [];
  ctx.fetch = (url) => { requests.push(url); return Promise.resolve({ ok: true, status: 200, headers: { get: () => null },
    json: () => Promise.resolve({ truncated: false, sites: [site(A, LTE800)] }) }); };
  const { map } = buildControl(ctx);
  map.getZoom = () => 15;
  map.getBounds = () => ({ getSouth: () => A[0] - 0.002, getWest: () => A[1] - 0.003, getNorth: () => A[0] + 0.002, getEast: () => A[1] + 0.003 });
  const fp = fakePanel();
  const hint = ctx.window.MastsHint.attach({ map, cfg: MASTS, panel: () => fp.panel });
  hint.update(A, A);
  map.fire('moveend');
  for (let i = 0; i < 6; i++) { while (timers.length) timers.shift()(); await tick(); }
  const asked = requests.map((u) => decodeURIComponent(u.split('cell=')[1]));
  assert.strictEqual(new Set(asked).size, asked.length, 'a cell was asked twice: ' + asked);
  assert(Array.from(map.layers).some((l) => l.kind === 'circleMarker'), 'the visible pinned cell was not drawn');
  assert.strictEqual(fp.find(fp.head, 'tp-chip-mast').textContent, 'Cell transmitter ~0 m');
});

// ── 7. Panel ────────────────────────────────────────────────────────────────

test('the chip sits with the other chips, before the copy button, text only', () => {
  const fp = fakePanel();
  const hostile = '<img src=x onerror="window.__xss=1">';
  H.render(fp.panel, H.hintFor({ a: { distanceM: 120, bands: [800], tier: 'near', site: { street: hostile } }, b: null }));
  const chip = fp.find(fp.head, 'tp-chip-mast');
  assert(chip, 'no chip');
  assert.strictEqual(chip.parentNode, fp.head);
  assert.strictEqual(chip.nextSibling, fp.copy, 'chip not right before the copy button');
  assert(/\btp-chip\b/.test(chip.className) && /\btp-chip-mast-near\b/.test(chip.className), chip.className);
  assert.strictEqual(chip.attrs['data-testid'], 'tp-mast-hint');
  assert.strictEqual(chip.attrs['data-tier'], 'near');
  assert.strictEqual(chip.textContent, 'Cell transmitter ~120 m');
  assert(/especially within 100 m/.test(chip.title));
  const note = fp.find(fp.panel, 'tp-mast-note');
  assert(note && !note.hidden && note.parentNode === fp.detail.parentNode, 'no note under the detail line');
  assert.strictEqual(fp.detail.nextSibling, note);
  assert.strictEqual(note.textContent, chip.title);
  for (const el of fp.made) {
    assert(!el.innerHTMLWrites, 'innerHTML written');
    assert(!/<img|onerror/.test(el.textContent + el.title), 'site text reached the panel');
  }
  H.render(fp.panel, null);
  assert(chip.hidden && note.hidden, 'not hidden without a hint');
  H.render(fp.panel, H.hintFor({ a: { distanceM: 30, bands: [900], tier: 'strong' }, b: null }));
  assert.strictEqual(fp.made.length, 3, 'chip, separator and note are built once');
  assert(/\btp-chip-mast-strong\b/.test(chip.className) && !chip.hidden);
});

test('no panel yet, no masts config, or no layer module: attach is inert', () => {
  const ctx = sandbox();
  ctx.fetch = () => new Promise(() => {});
  const map = buildControl(ctx).map;
  map.getZoom = () => 9;
  const hint = ctx.window.MastsHint.attach({ map, cfg: MASTS, panel: () => null });
  hint.update(A, B); // must not throw
  hint.clear();
  assert.strictEqual(ctx.window.MastsHint.attach({ map, cfg: null, panel: () => null }), null);
  assert.strictEqual(ctx.window.MastsHint.attach({ map: null, cfg: MASTS, panel: () => null }), null);
});

// ── 8. Theming and wiring ───────────────────────────────────────────────────

test('chip colours are CSS variables; no colour literal in masts-hint.js', () => {
  const css = fs.readFileSync(path.join(__dirname, 'public', 'style.css'), 'utf8');
  const rules = css.match(/\.tp-chip-mast[^{]*\{[^}]*\}/g) || [];
  assert(rules.length >= 2, 'no .tp-chip-mast rules');
  for (const r of rules) assert(!/#[0-9a-f]{3,8}\b|rgba?\(/i.test(r), 'colour literal: ' + r);
  assert(rules.some((r) => /var\(--mast-interference\)/.test(r)), 'chip does not use --mast-interference');
  assert(/\.tp-mast-note\s*\{[^}]*var\(--text-muted\)/.test(css));
  assert(!/#[0-9a-f]{3,8}\b['"]|rgba?\(/i.test(HINT_SRC), 'hard-coded colour in masts-hint.js');
});

test('map.js wires the hint to the profile ends; index.html loads masts-hint.js after the layer', () => {
  const map = fs.readFileSync(path.join(__dirname, 'public', 'map.js'), 'utf8');
  assert(/window\.MastsHint\.attach\(\{ map: map, cfg: window\.MC_getMastsConfig\(\), panel: /.test(map), 'not attached');
  assert(/profileMastHint\.update\(a, b\)/.test(map), 'not updated from profileDrawLine');
  assert(/profileMastHint\.clear\(\)/.test(map), 'not cleared with the line');
  const html = fs.readFileSync(path.join(__dirname, 'public', 'index.html'), 'utf8');
  const iL = html.indexOf('masts-layer.js?v=__BUST__'), iH = html.indexOf('masts-hint.js?v=__BUST__');
  assert(iL > 0 && iH > iL, 'masts-hint.js not loaded after masts-layer.js');
  assert(html.indexOf('map.js?v=__BUST__') > iH, 'masts-hint.js not loaded before map.js');
});

test('MC_getMastsConfig is the registry validator: a bad block is null', () => {
  assert.strictEqual(ctx0.window.MC_getMastsConfig(), ctx0.window.MC_MASTS_CFG);
  const bad = sandbox({ masts: Object.assign({}, MASTS, { url: '//evil.example/api' }) });
  assert.strictEqual(bad.window.MC_getMastsConfig(), null);
});

// ── 9. Perf ─────────────────────────────────────────────────────────────────

test('perf: 8 dense cells (4,000 sites) per endpoint pair in well under a frame', () => {
  const sites = [];
  for (let i = 0; i < 4000; i++) sites.push(site([A[0] + (i % 80) * 0.0004, A[1] + Math.floor(i / 80) * 0.0006],
    i % 3 ? [{ bandMHz: 1800 }, { bandMHz: 2100 }] : LTE800));
  const t0 = process.hrtime.bigint();
  let r;
  for (let k = 0; k < 20; k++) r = [H.nearestFor(A[0], A[1], sites), H.nearestFor(B[0], B[1], sites)];
  const ms = Number(process.hrtime.bigint() - t0) / 1e6 / 20;
  console.log(`     two endpoints × 4,000 sites: ${ms.toFixed(2)} ms`);
  assert(r[0] && r[0].tier === 'strong');
  assert(ms < 16, 'took ' + ms + ' ms');
});

Promise.all(pending).then(() => {
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
});
