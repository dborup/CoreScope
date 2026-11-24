/* test-968-kiosk-preset.js
 * #968 M1 — the operator kiosk preset (config.json "kiosk" →
 * /api/config/client → window.MC_KIOSK_CFG). Unit tests for the real
 * public/live-kiosk.js in a vm sandbox with a fake DOM and a fake clock
 * (tests/fake-dom.js):
 *   SETTINGS   the one rule for the preset: KEYS / DEFAULTS / LIMITS /
 *              valid() / resolve(), the bounds the server uses too
 *   tiles      a tile the preset does not list is not built, and the bulk
 *              reads that only feed it stop (fewer requests)
 * The URL > preset > built-in precedence of parseKioskParams() is in
 * test-883-kiosk-params.js, the remount interval in test-883-m3-kiosk.js.
 */
'use strict';
const assert = require('assert');
const fs = require('fs');
const path = require('path');
const { Target, El, makeDoc, makeClock, loadPublic, flush } = require('./tests/fake-dom');

let passed = 0, failed = 0;
const queue = [];
function test(name, fn) { queue.push({ name, fn }); }
function section(title) { queue.push({ title }); }

const K = loadPublic('live-kiosk.js').LiveKiosk;
const eq = (a, b, m) => assert.deepStrictEqual(JSON.parse(JSON.stringify(a)), b, m);
const AREAS = [{ key: 'AAA', label: 'Area A', latMin: 55, latMax: 56, lonMin: 9, lonMax: 10 },
  { key: 'BBB', label: 'Area B', latMin: 57, latMax: 58, lonMin: 9, lonMax: 10 }];

// One mount the way live.js makes it, with counted bulk reads.
function mount(hash, preset) {
  const clock = makeClock();
  const doc = makeDoc();
  doc.body.className = 'kiosk';
  const win = new Target();
  const root = new El('div');
  const m = { clock, doc, win, root, status: 0, areaStats: 0, reseeds: 0 };
  m.c = K.create({
    doc, win, root,
    params: K.parseKioskParams(new URLSearchParams(hash), preset),
    fetchAreas: () => Promise.resolve(AREAS),
    fly: () => {}, reducedMotion: () => true, getMode: () => 'LIVE',
    requestFullscreen: () => Promise.resolve(), exitFullscreen: () => Promise.resolve(), exit: () => {}, warn: () => {},
    now: clock.now, setTimeout: clock.setTimeout, clearTimeout: clock.clearTimeout,
    fetchStatus: () => { m.status++; return Promise.resolve({ active: 7 }); },
    fetchAreaStats: () => { m.areaStats++; return Promise.resolve({ density: [{ areaKey: 'AAA', label: 'Area A', active: 3 }] }); },
    momentsOn: () => true,
    reseed: () => { m.reseeds++; },
  });
  m.tiles = () => root.all().filter((e) => e.getAttribute('data-tile')).map((e) => e.getAttribute('data-tile'));
  m.text = (cls) => root.find(cls).map((e) => e.textContent);
  return m;
}

section('=== #968 M1 LiveKiosk.SETTINGS ===');

test('KEYS, DEFAULTS and LIMITS are the plan\'s, and the URL clamps read the same bounds', () => {
  const S = K.SETTINGS;
  eq(S.KEYS, ['tour', 'everySeconds', 'theme', 'tiles', 'remountHours']);
  eq(S.DEFAULTS, { tour: [], everySeconds: 60, theme: 'dark', tiles: ['rate', 'active', 'records'], remountHours: 6 });
  eq(S.LIMITS, { everySeconds: [15, 3600], remountHours: [1, 24], tour: [0, 12] });
  eq(S.TILES, ['rate', 'active', 'records']);
  eq(S.THEMES, ['dark', 'light', 'auto']);
  assert.strictEqual(S.LIMITS.everySeconds[0], K.EVERY_MIN);
  assert.strictEqual(S.LIMITS.everySeconds[1], K.EVERY_MAX);
  assert.strictEqual(S.LIMITS.tour[1], K.MAX_STEPS);
  assert.strictEqual(S.DEFAULTS.remountHours * 3600000, K.REMOUNT_MS_DEFAULT);
});

test('valid(): each key\'s type and bounds; unknown keys are never valid', () => {
  const v = K.SETTINGS.valid;
  assert(v('everySeconds', 15) && v('everySeconds', 3600) && !v('everySeconds', 14) && !v('everySeconds', 3601));
  assert(!v('everySeconds', 30.5) && !v('everySeconds', '30') && !v('everySeconds', NaN));
  assert(v('remountHours', 1) && v('remountHours', 24) && v('remountHours', 1.5) && !v('remountHours', 0.99) && !v('remountHours', 24.01));
  assert(v('theme', 'dark') && v('theme', 'light') && v('theme', 'auto') && !v('theme', 'Dark') && !v('theme', ''));
  assert(v('tiles', ['rate']) && v('tiles', ['records', 'active']) && !v('tiles', ['x']) && !v('tiles', 'rate') && !v('tiles', []));
  assert(v('tour', []) && v('tour', ['a']) && !v('tour', 'a') && !v('tour', [1]) && !v('tour', new Array(13).fill('a')));
  for (const k of ['kiosk', 'enabled', 'scale', 'safeInsetVmin', 'toString']) assert(!v(k, 1) && !v(k, true), k);
});

test('resolve(): a bad or missing value is the default, so a hand-edited config never breaks the page', () => {
  const r = K.SETTINGS.resolve;
  const D = JSON.parse(JSON.stringify(K.SETTINGS.DEFAULTS));
  eq(r(null), D);
  eq(r({ everySeconds: 90, theme: 'auto', remountHours: 3 }), { tour: [], everySeconds: 90, theme: 'auto', tiles: ['rate', 'active', 'records'], remountHours: 3 });
  eq(r({ everySeconds: 1, theme: 'x', remountHours: 99, tiles: ['bogus'], tour: 'a' }), D);
  // A resolved preset never carries a key outside KEYS (kiosk stays URL-only).
  eq(Object.keys(r({ kiosk: true, enabled: true, scale: 2 })), JSON.parse(JSON.stringify(K.SETTINGS.KEYS)));
  // The defaults are not shared: a caller cannot change them through a result.
  const a = r(null);
  a.tiles.push('x'); a.tour.push('y');
  eq(K.SETTINGS.DEFAULTS.tiles, ['rate', 'active', 'records']);
  eq(K.SETTINGS.DEFAULTS.tour, []);
});

section('=== #968 M1 tiles ===');

test('all three tiles without a preset (unchanged)', async () => {
  const m = mount('kiosk=1&tour=AAA,BBB');
  await flush();
  eq(m.tiles(), ['rate', 'active', 'records']);
  eq(m.c.stats().tiles, ['rate', 'active', 'records']);
  assert.strictEqual(m.status, 1);
  assert.strictEqual(m.areaStats, 1);
  m.c.destroy();
});

test('preset tiles rate + records: the active tile is not built and its two bulk reads never run', async () => {
  const m = mount('kiosk=1&tour=AAA,BBB', { tiles: ['rate', 'records'] });
  await flush();
  eq(m.tiles(), ['rate', 'records']);
  assert.strictEqual(m.root.find('live-kiosk-tile-area').length, 0, 'no area part');
  assert.strictEqual(m.status + m.areaStats, 0, 'no read at mount');
  m.clock.advance(10 * K.NUMBERS_POLL_MS);
  await flush();
  assert.strictEqual(m.status + m.areaStats, 0, 'no poll');
  assert.strictEqual(m.c.stats().pollTimer, false, 'no poll timer');
  // A socket (re)open still re-seeds Moments but reads no numbers.
  m.win.fire('corescope:ws-open');
  m.doc.hidden = true; m.doc.fire('visibilitychange');
  m.doc.hidden = false; m.doc.fire('visibilitychange');
  await flush();
  assert.strictEqual(m.reseeds, 1);
  assert.strictEqual(m.status + m.areaStats, 0, 'none on ws-open or on show');
  // The rate tile still counts, with no error from the missing slots.
  m.c.setRate(42);
  eq(m.text('live-kiosk-tile-value'), ['42']);
  m.c.onMoment({ kind: 'record_link', at: m.clock.now(), link: { distanceKm: 12.5, a: { name: 'A' }, b: { name: 'B' } } });
  assert(m.text('live-kiosk-record-value').includes('12.5 km'));
  m.c.destroy();
});

test('preset tiles active only: polls as before, and the rate tick writes nothing', async () => {
  const m = mount('kiosk=1&tour=AAA,BBB', { tiles: ['active'] });
  await flush();
  eq(m.tiles(), ['active']);
  assert.strictEqual(m.status, 1);
  assert.strictEqual(m.areaStats, 1);
  m.clock.advance(K.NUMBERS_POLL_MS);
  await flush();
  assert.strictEqual(m.status, 2, 'polled');
  const w = m.c.stats().tileWrites;
  m.c.setRate(5); m.c.setRate(6);
  m.c.onMoment({ kind: 'record_link', at: m.clock.now(), link: { distanceKm: 3, a: {}, b: {} } });
  assert.strictEqual(m.c.stats().tileWrites, w, 'no write for a tile that is not there');
  eq(m.text('live-kiosk-tile-value'), ['7']);
  m.c.destroy();
});

test('the tiles come from the preset only: a link cannot add one back', async () => {
  const m = mount('kiosk=1&tiles=rate,active,records', { tiles: ['records'] });
  await flush();
  eq(m.tiles(), ['records']);
  assert.strictEqual(m.status, 0);
  m.c.destroy();
  assert.strictEqual(K.liveCount(), 0);
});

section('=== #968 M1 live.js and roles.js wiring (source) ===');

// live.js and roles.js are page modules (no vm seam); these pin the wiring
// the E2E (test-968-kiosk-preset-e2e.js) exercises in a browser.
const live = fs.readFileSync(path.join(__dirname, 'public/live.js'), 'utf8');
const roles = fs.readFileSync(path.join(__dirname, 'public/roles.js'), 'utf8');

test('roles.js publishes the preset as window.MC_KIOSK_CFG (null when the server has none)', () => {
  assert(/window\.MC_KIOSK_CFG\s*=\s*cfg\.kiosk\s*\|\|\s*null/.test(roles));
});

test('live.js parses every kiosk link with the preset, and waits for the config before the controller', () => {
  const calls = live.match(/parseKioskParams\(([^)]*\))?[^)]*\)/g) || [];
  assert(calls.length >= 2, 'calls: ' + calls.length);
  calls.forEach((c) => assert(/MC_KIOSK_CFG/.test(c), c));
  const init = live.slice(live.indexOf('function initKiosk('), live.indexOf('function setKioskAutoReload('));
  assert(/await window\.MeshConfigReady/.test(init), 'initKiosk awaits MeshConfigReady');
});

(async () => {
  for (const item of queue) {
    if (item.title) { console.log('\n' + item.title); continue; }
    try { await item.fn(); passed++; console.log('  ✓ ' + item.name); }
    catch (e) { failed++; console.error('  ✗ ' + item.name + '\n    ' + (e && e.stack || e).toString().split('\n').slice(0, 3).join('\n    ')); }
  }
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})();
