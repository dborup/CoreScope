/* test-issue-724-masts-layer.js — #724 M3: the Mastedatabasen antenna sites
 * map layer (public/masts-layer.js) and its two entries in the overlay
 * registry (public/map-tile-providers.js).
 *
 * vm sandbox + recording Leaflet stub (tests/map-overlay-sandbox.js); the
 * real files run, nothing is copied. Nothing here contacts any server:
 * /api/masts is a stub function.
 *
 * Covered:
 *   1. the WGS84 → EPSG:25832 projection against the server's own
 *      (internal/dhm ToUTM32 values);
 *   2. cell enumeration for a view: complete, inside the extent, centre
 *      first, capped, and the z13 cell count (printed);
 *   3. the loader: pacing (#753 review N4), dropping cells that left the
 *      view, no refetch of a loaded cell, the eviction bound, 503 / 429 /
 *      failure handling;
 *   4. the popup: every field escaped, published fields only, the 500 m /
 *      100 m interference hint and the likely-mast note, no legal claims;
 *   5. the status line (zoom gate, loading, unavailable, truncated);
 *   6. the registry: offered only for a valid masts block, the sites on and
 *      the interference layer off by default, hash parse and serialise
 *      (?masts=0, ?mastsInt=1), stored choice, the credit on Live;
 *   7. the per-cell diff: markers built once, attached and detached as the
 *      cell enters and leaves the view, no request for a cell seen before;
 *   8. CSS variables in light and both dark blocks, customizer mapping,
 *      roles.js and index.html wiring;
 *   9. perf: enumeration + loader planning for a z13 viewport.
 *
 * Runs via: node test-issue-724-masts-layer.js
 */
'use strict';
const fs = require('fs');
const path = require('path');
const assert = require('assert');
const { makeSandbox, buildControl, liveCredits } = require('./tests/map-overlay-sandbox');

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

// What cmd/server/masts_proxy.go sends while map.masts is enabled.
const MASTS = {
  enabled: true, url: '/api/masts', minZoom: 13, cellSizeM: 2000,
  attribution: 'Antenna positions: Mastedatabasen, Digitaliseringsstyrelsen (CC0)',
  sourceUrl: 'https://mastedatabasen.dk/', cellExtent: [219, 3017, 462, 3209],
};
const SITE_LABEL = 'Antenna sites (mast, tower or rooftop)';
const INT_LABEL = 'Nearby transmitters (interference)';

const ML = makeSandbox({ masts: MASTS }).window.MastsLayer;
const tick = () => new Promise((r) => setImmediate(r));

console.log('\n=== #724 M3 antenna sites layer ===');

// ── 1. Projection ───────────────────────────────────────────────────────────

// dhm.ToUTM32 (internal/dhm/utm.go), printed by a throwaway Go test.
const UTM_REF = [
  [55.6761, 12.5683, 724351.9286, 6175804.0221],
  [56.1629, 10.2039, 574766.3925, 6224862.6481],
  [57.7209, 10.5839, 594345.2579, 6398740.4095],
  [55.1009, 14.7066, 863917.8358, 6120903.6924],
  [55.4765, 8.4594, 465829.8183, 6147950.7655],
  [54.5, 8.1, 441715.4009, 6039526.9138],
];
test('toUTM32 matches the server projection to the millimetre', () => {
  for (const [lat, lon, e, n] of UTM_REF) {
    const p = ML.toUTM32(lat, lon);
    assert(Math.abs(p.e - e) < 1e-3 && Math.abs(p.n - n) < 1e-3, `${lat},${lon} → ${p.e},${p.n}, want ${e},${n}`);
  }
});

// ── 2. Cells of a view ──────────────────────────────────────────────────────

// The lat/lon box a w×h px Web Mercator viewport shows at zoom z.
function viewBox(lat, lon, z, w, h) {
  const scale = 256 * Math.pow(2, z);
  const y = (la) => (1 - Math.log(Math.tan(Math.PI / 4 + la * Math.PI / 360)) / Math.PI) / 2 * scale;
  const la = (py) => (2 * Math.atan(Math.exp(Math.PI * (1 - 2 * py / scale))) - Math.PI / 2) * 180 / Math.PI;
  const cy = y(lat), dLon = w / scale * 360;
  return { south: la(cy + h / 2), north: la(cy - h / 2), west: lon - dLon / 2, east: lon + dLon / 2 };
}
function sampledCells(b, steps) {
  const set = new Set();
  for (let i = 0; i <= steps; i++) for (let j = 0; j <= steps; j++) {
    const p = ML.toUTM32(b.south + (b.north - b.south) * i / steps, b.west + (b.east - b.west) * j / steps);
    const e = Math.floor(p.e / 2000), n = Math.floor(p.n / 2000);
    if (e >= 219 && e <= 462 && n >= 3017 && n <= 3209) set.add(e + ',' + n);
  }
  return set;
}

const AARHUS_Z13 = viewBox(56.1629, 10.2039, 13, 1920, 1080);
let z13Count = 0;

test('every cell a z13 viewport touches is listed, and nothing beyond its bounding cells', () => {
  for (const b of [AARHUS_Z13, viewBox(55.6761, 12.5683, 14, 1280, 800), viewBox(55.03, 9.0, 13, 1280, 800)]) {
    const list = ML.cellsForView(b, MASTS);
    const keys = new Set(list.map((c) => c.key));
    assert.strictEqual(keys.size, list.length, 'duplicate cells');
    for (const k of sampledCells(b, 60)) assert(keys.has(k), `cell ${k} in view but not listed`);
    const es = list.map((c) => c.e), ns = list.map((c) => c.n);
    assert.strictEqual(list.length, (Math.max(...es) - Math.min(...es) + 1) * (Math.max(...ns) - Math.min(...ns) + 1));
  }
});

test('a view straddling 9° E lists the cell under the southern edge at the meridian', () => {
  // A parallel's northing is lowest on the central meridian, between the
  // corners: corner sampling alone would miss that row of cells.
  for (let lat = 54.6; lat < 57.6; lat += 0.0137) {
    const b = { south: lat, north: lat + 0.02, west: 7.5, east: 10.5 };
    const list = ML.cellsForView(b, MASTS, 100000);
    const keys = new Set(list.map((c) => c.key));
    const p = ML.toUTM32(lat, 9);
    assert(keys.has(Math.floor(p.e / 2000) + ',' + Math.floor(p.n / 2000)), 'missed the cell under 9° E at ' + lat);
  }
});

test('z13 viewport cell counts (printed) stay under the view cap', () => {
  z13Count = ML.cellsForView(AARHUS_Z13, MASTS).length;
  const big = ML.cellsForView(viewBox(56.1629, 10.2039, 13, 2560, 1440), MASTS, 100000).length;
  const z14 = ML.cellsForView(viewBox(56.1629, 10.2039, 14, 1920, 1080), MASTS).length;
  const big14 = ML.cellsForView(viewBox(56.1629, 10.2039, 14, 3840, 2160), MASTS);
  console.log(`     z13 1920×1080 Aarhus: ${z13Count} cells; z13 2560×1440: ${big}; z14 1920×1080: ${z14}; z14 3840×2160: ${big14 && big14.length}`);
  assert(z13Count >= 60 && z13Count <= 100, 'z13 count ' + z13Count);
  assert(z13Count <= ML.VIEW_CELL_CAP, '1920×1080 z13 over the cap: ' + z13Count);
  // #758 F2: a wider z13 view is over the cap (one more zoom step, F1) ...
  assert(big > ML.VIEW_CELL_CAP && ML.cellsForView(viewBox(56.1629, 10.2039, 13, 2560, 1440), MASTS) === null, '2560×1440 z13: ' + big);
  // ... and one step in, even a 4K view fits.
  assert(big14 && big14.length <= ML.VIEW_CELL_CAP, '3840×2160 z14 over the cap');
  assert(z14 < z13Count / 3);
});

test('a view wider than the cap is null (treated like the zoom gate)', () => {
  assert.strictEqual(ML.cellsForView(viewBox(56.1629, 10.2039, 11, 1920, 1080), MASTS), null);
});

test('the centre cell comes first and distance never decreases', () => {
  const list = ML.cellsForView(AARHUS_Z13, MASTS);
  const c = ML.toUTM32(56.1629, 10.2039);
  assert.strictEqual(list[0].key, Math.floor(c.e / 2000) + ',' + Math.floor(c.n / 2000));
  for (let i = 1; i < list.length; i++) assert(list[i].d >= list[i - 1].d);
});

test('never a cell outside the server extent', () => {
  assert.strictEqual(ML.cellsForView(viewBox(52.52, 13.40, 14, 1280, 800), MASTS).length, 0, 'Berlin');
  const edge = ML.cellsForView(viewBox(54.46, 8.07, 13, 1920, 1080), MASTS);
  assert(edge.length > 0);
  for (const c of edge) assert(c.e >= 219 && c.e <= 462 && c.n >= 3017 && c.n <= 3209, c.key);
});

// ── 3. Loader ───────────────────────────────────────────────────────────────

function harness(opts) {
  const calls = [];
  const timers = [];
  let t = 0;
  const loaded = [], evicted = [];
  const loader = ML.createCellLoader(Object.assign({
    fetchCell: (key) => new Promise((resolve) => calls.push({ key, resolve })),
    onLoad: (key) => loaded.push(key),
    onEvict: (key) => evicted.push(key),
    now: () => t,
    setTimeout: (fn, ms) => { const h = { fn, at: t + ms }; timers.push(h); return h; },
    clearTimeout: (h) => { const i = timers.indexOf(h); if (i >= 0) timers.splice(i, 1); },
  }, opts || {}));
  return {
    loader, calls, loaded, evicted, timers,
    advance(ms) { t += ms; timers.filter((h) => h.at <= t).forEach((h) => { timers.splice(timers.indexOf(h), 1); h.fn(); }); },
    ok: (c, data) => c.resolve({ status: 200, data: data || { sites: [] } }),
  };
}
const keysN = (n, base) => Array.from({ length: n }, (_, i) => (base || 300) + i + ',3100');

test('pacing: at most CONCURRENCY requests at once, in view order (#753 N4)', async () => {
  assert.strictEqual(ML.CONCURRENCY, 4);
  const h = harness();
  const keys = keysN(80);
  h.loader.view(keys);
  assert.deepStrictEqual(h.calls.map((c) => c.key), keys.slice(0, 4));
  h.calls[0].done = true;
  h.ok(h.calls[0]);
  await tick();
  assert.strictEqual(h.calls.length, 5);
  assert.strictEqual(h.calls[4].key, keys[4]);
  let max = 0;
  while (h.calls.some((c) => !c.done)) {
    const open = h.calls.filter((c) => !c.done);
    max = Math.max(max, open.length);
    open.forEach((c) => { c.done = true; h.ok(c); });
    await tick();
  }
  assert.strictEqual(h.calls.length, 80);
  assert(max <= 4, 'max in flight ' + max);
});

test('the client pacing fits the server defaults (maxInFlight, per-IP budget)', () => {
  const go = fs.readFileSync(path.join(__dirname, 'cmd', 'server', 'masts_proxy.go'), 'utf8');
  const maxInFlight = Number(/mastsMaxInFlightDefault\s*=\s*(\d+)/.exec(go)[1]);
  const perMin = Number(/mastsCellRateLimitDefault\s*=\s*(\d+)/.exec(go)[1]);
  assert(ML.CONCURRENCY * 4 <= maxInFlight, `${ML.CONCURRENCY} per viewer vs maxInFlight ${maxInFlight}`);
  assert(z13Count > maxInFlight, 'a cold z13 view would exceed maxInFlight without pacing');
  assert(z13Count <= perMin, `a cold z13 view (${z13Count}) fits the per-IP budget ${perMin}`);
  // #758 F2: the largest view the layer draws at all must fit the budget,
  // or its last cells get 429 and stay empty for up to a minute.
  assert(ML.VIEW_CELL_CAP <= perMin, `view cap ${ML.VIEW_CELL_CAP} over the per-IP budget ${perMin}`);
});

test('cells that leave the view before they start are never requested', async () => {
  const h = harness();
  const a = keysN(40, 300), b = keysN(6, 400);
  h.loader.view(a);
  h.loader.view(b);
  h.calls.slice().forEach((c) => h.ok(c));
  await tick(); await tick();
  h.calls.filter((c) => !c.done).forEach((c) => h.ok(c));
  await tick();
  const asked = h.calls.map((c) => c.key);
  assert.deepStrictEqual(asked.slice(0, 4), a.slice(0, 4));
  assert.deepStrictEqual(asked.slice(4).sort(), b.slice().sort());
});

test('a loaded cell is never requested again while kept', async () => {
  const h = harness();
  h.loader.view(keysN(3));
  h.calls.forEach((c) => h.ok(c));
  await tick();
  h.loader.view(keysN(3, 500));
  h.calls.slice(3).forEach((c) => h.ok(c));
  await tick();
  h.loader.view(keysN(3));
  assert.strictEqual(h.calls.length, 6);
  assert(h.loader.data('300,3100'));
});

test('eviction bound: at most keepCap cells, least recently wanted first, never a visible one', async () => {
  const h = harness({ keepCap: 20 });
  for (let v = 0; v < 6; v++) {
    h.loader.view(keysN(10, 300 + v * 10));
    for (let r = 0; r < 4; r++) { h.calls.filter((c) => !c.done).forEach((c) => { c.done = true; h.ok(c); }); await tick(); }
    assert(h.loader.stats().kept <= 20, 'kept ' + h.loader.stats().kept);
  }
  const visible = keysN(10, 350);
  for (const k of visible) assert(h.loader.data(k), 'visible cell ' + k + ' evicted');
  assert.deepStrictEqual(h.evicted.slice(0, 10), keysN(10, 300));
  assert.strictEqual(h.loader.stats().kept, 20);
  assert.strictEqual(ML.KEEP_CELL_CAP >= 2 * ML.VIEW_CELL_CAP, true);
});

test('a visible cell is never evicted, even when the view alone exceeds keepCap', async () => {
  const h = harness({ keepCap: 3, concurrency: 8 });
  const keys = keysN(5);
  h.loader.view(keys);
  h.calls.forEach((c) => h.ok(c));
  await tick();
  assert.deepStrictEqual(h.evicted, []);
  for (const k of keys) assert(h.loader.data(k), 'visible cell ' + k + ' evicted');
  h.loader.view(keysN(1, 900));
  assert.strictEqual(h.loader.stats().kept, 3, 'not trimmed back to keepCap once the cells left the view');
  assert.deepStrictEqual(h.evicted, keys.slice(0, 3));
});

test('a cell evicted while loading is dropped when it arrives', async () => {
  const h = harness({ keepCap: 4, concurrency: 8 });
  h.loader.view(keysN(4, 300));
  const first = h.calls.slice();
  h.loader.view(keysN(4, 400));
  first.forEach((c) => h.ok(c));
  await tick();
  assert.deepStrictEqual(h.loaded.filter((k) => k.startsWith('30')), []);
  assert(h.loader.stats().kept <= 4);
});

test('503: retried after Retry-After, then failed after the busy retries', async () => {
  const h = harness();
  h.loader.view(['300,3100']);
  for (let i = 0; i < 3; i++) {
    h.calls[h.calls.length - 1].resolve({ status: 503, retryAfterS: '5' });
    await tick();
    assert.strictEqual(h.calls.length, i + 1, 'retried before Retry-After');
    h.advance(4999);
    assert.strictEqual(h.calls.length, i + 1);
    h.advance(1);
    assert.strictEqual(h.calls.length, i + 2, 'not retried after Retry-After');
  }
  h.calls[3].resolve({ status: 503, retryAfterS: '5' });
  await tick();
  h.advance(30000);
  assert.strictEqual(h.calls.length, 4, 'retried past the busy retries');
  assert.strictEqual(h.loader.stats().failed, 1);
});

test('429 pauses the whole queue until Retry-After', async () => {
  const h = harness();
  h.loader.view(keysN(10));
  h.calls[0].resolve({ status: 429, retryAfterS: '7' });
  h.ok(h.calls[1]);
  await tick();
  assert.strictEqual(h.calls.length, 4, 'a request started while paused');
  h.advance(6999);
  assert.strictEqual(h.calls.length, 4);
  h.advance(1);
  assert(h.calls.length > 4, 'not resumed after the pause');
  assert(h.calls.slice(4).some((c) => c.key === '300,3100'), 'the 429 cell was not asked again');
});

test('a 502/504 or network error is not retried for a minute', async () => {
  const h = harness();
  h.loader.view(['300,3100', '301,3100']);
  h.calls[0].resolve({ status: 502 });
  h.calls[1].resolve(Promise.reject(new Error('net')));
  await tick(); await tick();
  h.loader.view(['300,3100', '301,3100']);
  h.advance(59999);
  assert.strictEqual(h.calls.length, 2);
  h.advance(1);
  assert.strictEqual(h.calls.length, 4);
});

// ── 4. Popup ────────────────────────────────────────────────────────────────

const EVIL = '<img src=x onerror="alert(1)">';
const SITE = {
  lat: 56.15, lon: 10.2, street: 'Banegårdspladsen' + EVIL, houseNumber: '1' + EVIL, postcode: '8000', city: 'Aarhus C' + EVIL,
  stations: ['S3152E', 'X' + EVIL],
  entries: [
    { technology: 'LTE' + EVIL, bandMHz: 800, service: 'Mobiltelefoni' + EVIL, commissioned: '2015-03-01' + EVIL, interference: true },
    { technology: '5G-NR', bandMHz: 3500, service: 'Mobiltelefoni', commissioned: '2021-06-01', interference: false },
    { technology: 'FM', service: 'Andre', interference: false },
  ],
  interference: true, likelyMast: true,
};

test('popup escapes every field and the source link', () => {
  const html = ML.popupHTML(SITE, Object.assign({}, MASTS, { sourceUrl: 'https://mastedatabasen.dk/"><script>x</script>' }));
  assert(!/<img|<script|onerror="/i.test(html), 'unescaped markup: ' + html);
  assert.strictEqual((html.match(/&lt;img src=x onerror=&quot;alert\(1\)&quot;&gt;/g) || []).length, 7);
});

test('popup shows the published fields and nothing the source does not publish', () => {
  const html = ML.popupHTML({ lat: 1, lon: 2, street: 'Vej', houseNumber: '2', postcode: '8000', city: 'Aarhus', stations: ['S1'],
    entries: [{ technology: 'GSM', bandMHz: 900, service: 'Mobiltelefoni', commissioned: '2001-01-01', interference: true }],
    interference: true, likelyMast: false, operator: 'TDC', heightM: 42, structure: 'mast' }, MASTS);
  for (const s of ['Vej 2, 8000 Aarhus', 'Station: S1', '>GSM<', '>900<', '>Mobiltelefoni<', '>2001-01-01<', 'href="https://mastedatabasen.dk/"']) {
    assert(html.includes(s), 'missing ' + s);
  }
  assert(!/TDC|42|operator|height:/i.test(html.replace('publishes no height', '')), 'shows an unpublished field: ' + html);
  assert(/mast, a tower or a rooftop/.test(html), 'no structure caveat');
  assert(/CC0/.test(html) && /accuracy not guaranteed/.test(html));
});

test('the interference hint (500 m / 100 m) only on interference sites; no legal claims', () => {
  const on = ML.popupHTML(SITE, MASTS);
  const off = ML.popupHTML(Object.assign({}, SITE, { interference: false, likelyMast: false }), MASTS);
  // #758 F4: the strong tier, not a claim about where most transmitters are.
  assert(/Within about 500 m, especially within 100 m, /.test(on) && /may desensitise/.test(on), on);
  assert(!/most within/.test(on));
  assert(!/desensitise|500 m/.test(off));
  assert(/likely a tall dedicated mast \(a heuristic, not from the source\)/.test(on));
  assert(!/heuristic/.test(off));
  for (const h of [on, off]) {
    assert(!/\b(legal|illegal|unlawful|law|prohibited|forbidden|violat\w*|must not|not allowed)\b/i.test(h), 'legal wording: ' + h);
    assert(!/\bwill (interfere|desensiti[sz]e)/i.test(h), 'claims interference will happen');
  }
  assert.strictEqual(ML.HINT_DISTANCE_M, 500);
  assert.strictEqual(ML.HINT_STRONG_M, 100);
});

// ── 5. Status line ──────────────────────────────────────────────────────────

test('status line: zoom gate, loading, unavailable, truncated, silent when off', () => {
  const s0 = { pending: 0, failed: 0, truncated: 0 };
  assert.strictEqual(ML.statusText(12, MASTS, true, s0, false), 'Zoom in to 13+ to see antenna positions');
  // #758 F1: at the gate zoom a view over the cap must not ask for a zoom
  // the map already has.
  assert.strictEqual(ML.statusText(13, MASTS, true, s0, true), 'This view is too wide for antenna positions; zoom in one more step');
  assert.strictEqual(ML.statusText(15, MASTS, true, s0, true), 'This view is too wide for antenna positions; zoom in one more step');
  assert.strictEqual(ML.statusText(12, MASTS, false, s0, false), '');
  assert.strictEqual(ML.statusText(13, MASTS, true, Object.assign({}, s0, { pending: 2 }), false), 'Loading antenna sites…');
  assert.strictEqual(ML.statusText(13, MASTS, true, Object.assign({}, s0, { failed: 1 }), false), 'Antenna data unavailable for some areas');
  assert(/500-row limit/.test(ML.statusText(13, MASTS, true, Object.assign({}, s0, { truncated: 1 }), false)));
  assert.strictEqual(ML.statusText(13, MASTS, true, s0, false), '');
});

// ── 6. Registry ─────────────────────────────────────────────────────────────

const mastsLabels = (ctl) => Object.keys((ctl && ctl.overlays) || {}).filter((k) => k === SITE_LABEL || k === INT_LABEL);

test('not offered without an enabled, well-formed masts block', () => {
  for (const [name, cfg] of [
    ['absent', undefined], ['null', null], ['enabled:false', Object.assign({}, MASTS, { enabled: false })],
    ['enabled:"true"', Object.assign({}, MASTS, { enabled: 'true' })],
    ['absolute url', Object.assign({}, MASTS, { url: 'https://example.org/api/masts' })],
    ['protocol-relative url', Object.assign({}, MASTS, { url: '//example.org/api/masts' })],
    ['minZoom string', Object.assign({}, MASTS, { minZoom: '13' })],
    ['cellSizeM 0', Object.assign({}, MASTS, { cellSizeM: 0 })],
    ['no extent', Object.assign({}, MASTS, { cellExtent: undefined })],
    ['short extent', Object.assign({}, MASTS, { cellExtent: [1, 2, 3] })],
    ['http source', Object.assign({}, MASTS, { sourceUrl: 'http://mastedatabasen.dk/' })],
  ]) {
    const ctx = makeSandbox(cfg === undefined ? {} : { masts: cfg });
    assert.strictEqual(ctx.window.MC_getMastsConfig(), null, name);
    assert.deepStrictEqual(mastsLabels(buildControl(ctx).ctl), [], name);
  }
  const noModule = makeSandbox({ masts: MASTS, noMastsLayer: true });
  assert.strictEqual(noModule.window.MC_getMastsConfig(), null, 'offered without masts-layer.js');
});

test('offered: two toggles in order; sites on and interference off by default', () => {
  const ctx = makeSandbox({ masts: MASTS });
  const { map, ctl } = buildControl(ctx);
  assert.deepStrictEqual(mastsLabels(ctl), [SITE_LABEL, INT_LABEL]);
  assert.deepStrictEqual(ctl.overlayLabels.map((l) => l.attrs['data-overlay-id']), ['masts-sites', 'masts-interference']);
  assert.strictEqual(map.hasLayer(ctl.overlays[SITE_LABEL]), true, 'sites not on by default');
  assert.strictEqual(map.hasLayer(ctl.overlays[INT_LABEL]), false, 'interference on by default');
  assert.strictEqual(ctl.overlays[SITE_LABEL].options.attribution, MASTS.attribution);
});

test('hash parse: ?masts=0 and ?mastsInt=1 win over defaults and are stored', () => {
  const ctx = makeSandbox({ masts: MASTS, hash: '#/map?masts=0&mastsInt=1' });
  const { map, ctl } = buildControl(ctx);
  assert.strictEqual(map.hasLayer(ctl.overlays[SITE_LABEL]), false);
  assert.strictEqual(map.hasLayer(ctl.overlays[INT_LABEL]), true);
  assert.strictEqual(ctx.localStorage.getItem('mc-masts'), '0');
  assert.strictEqual(ctx.localStorage.getItem('mc-masts-int'), '1');
});

test('stored choice: a stored "off" keeps the sites off without a hash', () => {
  const ctx = makeSandbox({ masts: MASTS, storage: { 'mc-masts': '0', 'mc-masts-int': '1' } });
  const { map, ctl } = buildControl(ctx);
  assert.strictEqual(map.hasLayer(ctl.overlays[SITE_LABEL]), false);
  assert.strictEqual(map.hasLayer(ctl.overlays[INT_LABEL]), true);
});

test('hash serialise: toggles write ?mastsInt=1 / removed, ?masts=0 / =1', () => {
  const ctx = makeSandbox({ masts: MASTS, hash: '#/map?lat=56&lon=10' });
  const { map, ctl } = buildControl(ctx);
  map.addLayer(ctl.overlays[INT_LABEL]);
  assert.strictEqual(ctx.location.hash, '#/map?lat=56&lon=10&mastsInt=1');
  map.removeLayer(ctl.overlays[SITE_LABEL]);
  assert.strictEqual(ctx.location.hash, '#/map?lat=56&lon=10&mastsInt=1&masts=0');
  map.removeLayer(ctl.overlays[INT_LABEL]);
  map.addLayer(ctl.overlays[SITE_LABEL]);
  assert.strictEqual(ctx.location.hash, '#/map?lat=56&lon=10&masts=1');
  assert.strictEqual(ctx.localStorage.getItem('mc-masts-int'), '0');
});

test('resolveOverlayOn: other overlays stay off by default', () => {
  const r = makeSandbox({}).window.MC_resolveOverlayOn;
  assert.strictEqual(r(null, null), false);
  assert.strictEqual(r(null, null, true), true);
  assert.strictEqual(r(null, '0', true), false);
  assert.strictEqual(r('1', '0', false), true);
  assert.strictEqual(r('0', '1', true), false);
});

test('Live: the credit shows once while either layer is on, and goes when both are off', () => {
  const ctx = makeSandbox({ masts: MASTS });
  const { map, ctl } = buildControl(ctx, false);
  assert.deepStrictEqual(liveCredits(map), [MASTS.attribution]);
  map.addLayer(ctl.overlays[INT_LABEL]);
  assert.deepStrictEqual(liveCredits(map), [MASTS.attribution]);
  map.removeLayer(ctl.overlays[SITE_LABEL]);
  map.removeLayer(ctl.overlays[INT_LABEL]);
  assert.deepStrictEqual(liveCredits(map), []);
});

test('the layers survive a control rebuild (one controller per map)', () => {
  const ctx = makeSandbox({ masts: MASTS });
  const { map, ctl } = buildControl(ctx);
  const sites = ctl.overlays[SITE_LABEL];
  ctx.window.dispatchEvent({ type: 'mc-tile-provider-changed', detail: { fromConfig: true } });
  const ctl2 = map._controls[map._controls.length - 1];
  assert.notStrictEqual(ctl2, ctl);
  assert.strictEqual(ctl2.overlays[SITE_LABEL], sites);
});

// ── 7. Per-cell diff through the real controller ───────────────────────────

function liveMap(ctx, view) {
  const { map, ctl } = buildControl(ctx);
  map.getZoom = () => view.z;
  map.getBounds = () => ({ getSouth: () => view.b.south, getWest: () => view.b.west, getNorth: () => view.b.north, getEast: () => view.b.east });
  return { map, ctl };
}

test('cell diffing: markers built once, detached off-view, re-attached without a request', async () => {
  const timers = [];
  const ctx = makeSandbox({ masts: MASTS, cssVars: { '--mast-site': 'rgb(1, 2, 3)', '--mast-interference': 'rgb(4, 5, 6)' } });
  ctx.setTimeout = (fn) => { timers.push(fn); return fn; };
  const requests = [];
  ctx.fetch = (url) => {
    requests.push(url);
    const cell = decodeURIComponent(url.split('cell=')[1]).split(',').map(Number);
    const site = { lat: 56.15, lon: 10.2, stations: ['S' + cell[0]], entries: [{ technology: 'LTE', bandMHz: 800, interference: true }], interference: true };
    return Promise.resolve({ ok: true, status: 200, headers: { get: () => null }, json: () => Promise.resolve({ cell: { e: cell[0], n: cell[1] }, truncated: false, sites: [site] }) });
  };
  const view = { z: 14, b: { south: 56.150, west: 10.200, north: 56.160, east: 10.215 } };
  const { map, ctl } = liveMap(ctx, view);
  const flush = async () => { for (let i = 0; i < 6; i++) { while (timers.length) timers.shift()(); await tick(); } };
  map.addLayer(ctl.overlays[INT_LABEL]);
  map.fire('moveend');
  await flush();
  const first = requests.length;
  assert(first > 0 && first <= 4, 'requests ' + first);
  const markers = Array.from(map.layers).filter((l) => l.kind === 'circleMarker');
  assert.strictEqual(markers.length, first * 2, 'one site and one ring per cell');
  const ring = markers.find((m) => m.options.radius === 10);
  assert.strictEqual(ring.options.color, 'rgb(4, 5, 6)');
  assert(markers.some((m) => m.options.fillColor === 'rgb(1, 2, 3)'));
  assert(/LTE/.test(ring.popup()), 'popup is the site popup');
  // #758 F3: the width every other map popup uses, so it fits a 375 px phone.
  assert(markers.every((m) => m.popupOptions && m.popupOptions.maxWidth === 280), 'popup maxWidth is not 280');
  assert.strictEqual(map._mcMasts.stats().attached, first);

  view.b = { south: 56.40, west: 10.60, north: 56.41, east: 10.615 }; // pan away
  map.fire('moveend');
  await flush();
  assert(markers.every((m) => !map.layers.has(m)), 'off-view markers still drawn');
  const afterPan = requests.length;

  view.b = { south: 56.150, west: 10.200, north: 56.160, east: 10.215 }; // and back
  map.fire('moveend');
  await flush();
  assert.strictEqual(requests.length, afterPan, 'a kept cell was requested again');
  assert(markers.every((m) => map.layers.has(m)), 'markers rebuilt instead of re-attached');

  view.z = 12; // below the gate: nothing drawn, nothing asked
  map.fire('zoomend');
  await flush();
  assert(markers.every((m) => !map.layers.has(m)));
  assert.strictEqual(requests.length, afterPan);
});

test('a theme switch re-reads the CSS variables and restyles every built marker', async () => {
  const vars = { '--mast-site': 'rgb(1, 1, 1)', '--mast-interference': 'rgb(2, 2, 2)' };
  const ctx = makeSandbox({ masts: MASTS, cssVars: vars, storage: { 'mc-masts-int': '1' } });
  const timers = [], observers = [];
  ctx.setTimeout = (fn) => { timers.push(fn); return fn; };
  ctx.MutationObserver = function (cb) { observers.push(cb); this.observe = () => {}; this.disconnect = () => {}; };
  ctx.fetch = () => Promise.resolve({ ok: true, status: 200, headers: { get: () => null },
    json: () => Promise.resolve({ truncated: false, sites: [{ lat: 56.15, lon: 10.2, stations: [], entries: [], interference: true }] }) });
  const { map } = liveMap(ctx, { z: 15, b: { south: 56.150, west: 10.200, north: 56.152, east: 10.203 } });
  map.fire('moveend');
  for (let i = 0; i < 4; i++) { while (timers.length) timers.shift()(); await tick(); }
  const markers = Array.from(map.layers).filter((l) => l.kind === 'circleMarker');
  assert(markers.length >= 2, 'markers ' + markers.length);
  vars['--mast-site'] = 'rgb(9, 9, 9)';
  vars['--mast-interference'] = 'rgb(8, 8, 8)';
  assert.strictEqual(observers.length, 1, 'no data-theme observer');
  observers[0]();
  assert(markers.every((m) => m.options.color === 'rgb(9, 9, 9)' || m.options.color === 'rgb(8, 8, 8)'), 'not restyled');
  assert(markers.some((m) => m.options.radius === 10 && m.options.color === 'rgb(8, 8, 8)'));
});

test('nothing is requested while both layers are off', async () => {
  const ctx = makeSandbox({ masts: MASTS, storage: { 'mc-masts': '0' } });
  const timers = [];
  ctx.setTimeout = (fn) => { timers.push(fn); return fn; };
  let n = 0;
  ctx.fetch = () => { n++; return new Promise(() => {}); };
  const { map } = liveMap(ctx, { z: 15, b: { south: 56.15, west: 10.2, north: 56.16, east: 10.215 } });
  map.fire('moveend');
  while (timers.length) timers.shift()();
  await tick();
  assert.strictEqual(n, 0);
});

test('the status line sits under the interference toggle and shows the zoom gate', async () => {
  const ctx = makeSandbox({ masts: MASTS });
  const timers = [];
  ctx.setTimeout = (fn) => { timers.push(fn); return fn; };
  ctx.fetch = () => new Promise(() => {});
  const made = [];
  ctx.document.createElement = () => {
    const el = { attrs: {}, setAttribute(k, v) { el.attrs[k] = v; }, textContent: '', hidden: false };
    made.push(el);
    return el;
  };
  const view = { z: 11, b: viewBox(56.16, 10.2, 11, 1280, 800) };
  const { map } = buildControl(ctx);
  // The sandbox labels have no parentNode, so give the decorate call one.
  map.getZoom = () => view.z;
  map.getBounds = () => ({ getSouth: () => view.b.south, getWest: () => view.b.west, getNorth: () => view.b.north, getEast: () => view.b.east });
  const label = { setAttribute() {}, parentNode: { insertBefore: (el) => { label.after = el; } } };
  ctx.window.MastsLayer.decorate(map, MASTS, label);
  const st = label.after;
  assert.strictEqual(st.attrs.role, 'status');
  map.fire('moveend');
  while (timers.length) timers.shift()();
  assert.strictEqual(st.textContent, 'Zoom in to 13+ to see antenna positions');
  assert.strictEqual(st.hidden, false);
  view.z = 14; view.b = viewBox(56.16, 10.2, 14, 1280, 800);
  map.fire('moveend');
  while (timers.length) timers.shift()();
  assert.strictEqual(st.textContent, 'Loading antenna sites…');
});

// ── 8. Theming and wiring ───────────────────────────────────────────────────

test('CSS variables in :root and in both dark blocks; no colour literal in the layer', () => {
  const css = fs.readFileSync(path.join(__dirname, 'public', 'style.css'), 'utf8');
  for (const v of ['--mast-site', '--mast-interference']) {
    const defs = css.match(new RegExp('\\n\\s*' + v + ':\\s*#[0-9a-f]{6};', 'gi')) || [];
    assert.strictEqual(defs.length, 3, v + ' defined ' + defs.length + '× (want :root + 2 dark blocks)');
  }
  const js = fs.readFileSync(path.join(__dirname, 'public', 'masts-layer.js'), 'utf8');
  assert(!/#[0-9a-f]{3,8}\b['"]|rgba?\(/i.test(js), 'hard-coded colour in masts-layer.js');
});

test('customizer maps both colours and keeps them out of preset detection', () => {
  const src = fs.readFileSync(path.join(__dirname, 'public', 'customize-v2.js'), 'utf8');
  assert(/mastSite: '--mast-site', mastInterference: '--mast-interference'/.test(src));
  assert(/var MASTS_COLOR_KEYS = \['mastSite', 'mastInterference'\];/.test(src));
  assert(/MASTS_COLOR_KEYS\.indexOf\(k\) === -1/.test(src));
});

test('roles.js stores cfg.masts; index.html loads masts-layer.js after the registry', () => {
  const roles = fs.readFileSync(path.join(__dirname, 'public', 'roles.js'), 'utf8');
  const i = roles.indexOf('window.MC_MASTS_CFG = cfg.masts || null;');
  assert(i > 0 && i < roles.indexOf('window.MC_initTileRegistry(true)', i));
  const html = fs.readFileSync(path.join(__dirname, 'public', 'index.html'), 'utf8');
  const reg = html.indexOf('src="map-tile-providers.js?v=__BUST__"'), ml = html.indexOf('src="masts-layer.js?v=__BUST__"');
  assert(reg > 0 && ml > reg, 'masts-layer.js not loaded after the registry');
});

// ── 9. Perf ─────────────────────────────────────────────────────────────────

test('perf: z13 enumeration + loader planning is well under a frame', () => {
  const h = harness();
  const N = 500;
  const t0 = process.hrtime.bigint();
  for (let i = 0; i < N; i++) {
    const b = viewBox(56.1629 + i * 1e-4, 10.2039, 13, 1920, 1080);
    h.loader.view(ML.cellsForView(b, MASTS).map((c) => c.key));
  }
  const ms = Number(process.hrtime.bigint() - t0) / 1e6 / N;
  console.log(`     z13 1920×1080: cellsForView + loader.view ${ms.toFixed(3)} ms per view change (${z13Count} cells)`);
  assert(ms < 2, ms + ' ms');
});

Promise.all(pending).then(() => {
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
});
