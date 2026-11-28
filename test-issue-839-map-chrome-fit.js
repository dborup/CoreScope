/**
 * #839 (CS-Macmini rc19 findings 1, 2, 8, 9) — unit tests.
 *
 * The real files run in vm sandboxes; nothing is copied.
 *   1. public/map-chrome-fit.js listRoom: the layer list stops above the
 *      chrome under it (the credit, the bottom nav), only chrome it
 *      overlaps counts, never below one row;
 *   2. popupRoom: top and bottom chrome the popup would overlap pad it,
 *      chrome beside it does not; a column taller than half the map is a
 *      wall that narrows it instead (the desktop Map Controls panel); a
 *      narrow button it would overlap (the gear, the ruler) is a wall when
 *      the popup fits beside it (640x360, #841 F1) and not when it does not
 *      (375x560); the geometry of the 375x560, 640x360 and 1440x900 maps;
 *      fitLayerControl schedules nothing while the control is closed, and
 *      a refit keeps the open list's scroll position (#841 F8);
 *   3. MC_forgetMapLayerPrefs (map-tile-providers.js): every overlay and
 *      the Aerial base forgotten, their link parameters dropped, the rest
 *      of the link kept, no history write when there is nothing to drop;
 *   4. the customizer's colour inputs get a #rrggbb value or none.
 *
 * Runs via: node test-issue-839-map-chrome-fit.js
 */
'use strict';
const vm = require('vm');
const fs = require('fs');
const path = require('path');
const assert = require('assert');
const { makeSandbox } = require('./tests/map-overlay-sandbox');
const { loadCustomizer } = require('./tests/customizer-sandbox');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}

const F = (() => {
  const ctx = { window: {}, Math, Array };
  vm.createContext(ctx);
  vm.runInContext(fs.readFileSync(path.join(__dirname, 'public', 'map-chrome-fit.js'), 'utf8'), ctx);
  return ctx.window.MapChromeFit;
})();
const R = (left, top, right, bottom) => ({ left, top, right, bottom });

console.log('\n📋 #839 — map chrome fit, Reset All of the layer choices, colour inputs\n');

// The 375x560 map of finding 1: 375x452, the open control's list from
// y=126 at x 10..280, the credit at 408..452 across the width.
const PHONE = { x: 375, y: 452 };
const PHONE_CREDIT = R(0, 408, 375, 452);

test('listRoom: the list ends GAP above the credit, less the control\'s own bottom padding', () => {
  const room = F.listRoom(PHONE, [PHONE_CREDIT], { left: 10, right: 280, top: 126 }, 6);
  assert.strictEqual(room, 408 - F.GAP - 126 - 6);
});

test('listRoom: the highest chrome below wins; chrome above the list or beside it does not count', () => {
  const chrome = [PHONE_CREDIT, R(0, 400, 375, 404), R(0, 10, 52, 110), R(300, 300, 375, 340)];
  assert.strictEqual(F.listRoom(PHONE, chrome, { left: 10, right: 280, top: 126 }, 0), 400 - F.GAP - 126);
});

test('listRoom: without chrome the map edge is the limit; never under MIN_LIST', () => {
  assert.strictEqual(F.listRoom(PHONE, [], { left: 10, right: 280, top: 126 }, 0), 452 - F.GAP - 126);
  assert.strictEqual(F.listRoom({ x: 640, y: 252 }, [R(0, 130, 640, 252)], { left: 10, right: 280, top: 126 }, 6), F.MIN_LIST);
});

test('popupRoom 375x560: pads below the zoom/layers column and the gear, above the credit', () => {
  // zoom 10..62 x 10..110, layers 10..58 x 120..168, gear 329..365 x 10..46.
  const chrome = [R(10, 10, 62, 110), R(10, 120, 58, 168), R(329, 10, 365, 46), PHONE_CREDIT];
  const r = F.popupRoom(PHONE, chrome, 187, 327);
  assert.deepStrictEqual(Array.from(r.autoPanPaddingTopLeft), [F.GAP, 168 + F.GAP]);
  assert.deepStrictEqual(Array.from(r.autoPanPaddingBottomRight), [F.GAP, 452 - 408 + F.GAP]);
  assert.strictEqual(r.maxHeight, 452 - (168 + F.GAP) - (44 + F.GAP) - F.POPUP_EXTRA_H);
});

test('popupRoom 1440x900: the Map Controls panel is a wall on the right, the left column is clear', () => {
  const size = { x: 1440, y: 848 };
  const panel = R(1156, 12, 1396, 836);
  const chrome = [R(10, 10, 62, 110), R(10, 120, 58, 168), panel, R(490, 831, 1408, 848)];
  const r = F.popupRoom(size, chrome, 720, 327);
  assert.deepStrictEqual(Array.from(r.autoPanPaddingTopLeft), [F.GAP, F.GAP], 'the left column far away padded the top');
  assert.deepStrictEqual(Array.from(r.autoPanPaddingBottomRight), [1440 - 1156 + F.GAP, 848 - 831 + F.GAP]);
  assert(r.maxHeight > 700, 'a desktop popup was squeezed: ' + r.maxHeight);
  // Near the panel the popup is kept left of it, so the panel never pads the top.
  const near = F.popupRoom(size, chrome, 1300, 327);
  assert.strictEqual(near.autoPanPaddingTopLeft[1], F.GAP);
});

// The 640x360 map: 640x252, the zoom/layers column on the left, the gear
// (594..630 x 10..46) and the path-profile ruler under it (582..630 x
// 54..102) on the right, the credit along the bottom.
const LAND = { x: 640, y: 252 };
const LAND_GEAR = R(594, 10, 630, 46);
const LAND_RULER = R(582, 54, 630, 102);
const LAND_CHROME = [R(10, 10, 62, 110), R(10, 120, 58, 168), LAND_GEAR, LAND_RULER, R(0, 222, 640, 252)];

test('popupRoom 640x360: the tall left column is a wall; the right buttons cost a popup on the left nothing', () => {
  const left = F.popupRoom(LAND, LAND_CHROME, 200, 327);
  assert.strictEqual(left.autoPanPaddingTopLeft[0], 58 + F.GAP);
  assert.strictEqual(left.autoPanPaddingTopLeft[1], F.GAP, 'the gear far right padded a popup on the left');
  assert.strictEqual(left.autoPanPaddingBottomRight[0], F.GAP, 'the buttons far right narrowed a popup on the left');
});

test('popupRoom 640x360, site on the right (#841 F1): the popup sits left of the gear and the ruler, not under them', () => {
  for (const x of [450, 545, 576]) { // x = 0.70, 0.85, 0.90 of the map
    const r = F.popupRoom(LAND, LAND_CHROME, x, 327);
    assert.strictEqual(r.autoPanPaddingBottomRight[0], 640 - 582 + F.GAP, 'x=' + x + ': the ruler is not a wall');
    assert.strictEqual(r.autoPanPaddingTopLeft[1], F.GAP, 'x=' + x + ': the popup was pushed under the buttons');
    assert.strictEqual(r.maxHeight, 252 - F.GAP - (30 + F.GAP) - F.POPUP_EXTRA_H);
    // The box Leaflet pans to: within the padding, so clear of both buttons.
    const x1 = Math.min(Math.max(x - 327 / 2, r.autoPanPaddingTopLeft[0]), 640 - r.autoPanPaddingBottomRight[0] - 327) + 327;
    assert(x1 + F.GAP <= LAND_RULER.left, 'x=' + x + ': box ends at ' + x1);
  }
});

test('popupRoom 640x360: the ruler alone (no gear) is a wall as well', () => {
  const r = F.popupRoom(LAND, [LAND_RULER], 560, 327);
  assert.strictEqual(r.autoPanPaddingBottomRight[0], 640 - 582 + F.GAP);
  assert.strictEqual(r.autoPanPaddingTopLeft[1], F.GAP);
});

test('popupRoom 375x560, site on the right: no room beside the buttons, so the popup goes under them', () => {
  const chrome = [R(10, 10, 62, 110), R(10, 120, 58, 168), R(329, 10, 365, 46), R(317, 54, 365, 102), PHONE_CREDIT];
  const r = F.popupRoom(PHONE, chrome, 320, 327);
  assert.deepStrictEqual(Array.from(r.autoPanPaddingTopLeft), [F.GAP, 168 + F.GAP]);
  assert.strictEqual(r.autoPanPaddingBottomRight[0], F.GAP);
});

test('popupRoom: a wide piece of chrome (the bottom nav) never becomes a wall', () => {
  const r = F.popupRoom(LAND, [R(0, 200, 640, 252)], 560, 327);
  assert.deepStrictEqual(Array.from(r.autoPanPaddingBottomRight), [F.GAP, 52 + F.GAP]);
});

test('popupRoom: never under MIN_POPUP, and a box wider than the room starts at the left padding', () => {
  const r = F.popupRoom({ x: 300, y: 120 }, [R(0, 0, 300, 50), R(0, 70, 300, 120)], 150, 400);
  assert.strictEqual(r.maxHeight, F.MIN_POPUP);
});

test('fitLayerControl schedules no refit while the control is closed (#841 F5)', () => {
  const timers = [];
  const ctx = { window: {}, Math, Array, setTimeout: (fn) => { timers.push(fn); return timers.length; } };
  vm.createContext(ctx);
  vm.runInContext(fs.readFileSync(path.join(__dirname, 'public', 'map-chrome-fit.js'), 'utf8'), ctx);
  const handlers = [];
  const map = { on: (ev, fn) => handlers.push(fn), off() {} };
  const container = { className: 'leaflet-control-layers' };
  const control = { _map: map, getContainer: () => container, expand() {} };
  ctx.window.MapChromeFit.fitLayerControl(map, control);
  handlers.forEach((h) => h()); handlers.forEach((h) => h());
  assert.strictEqual(timers.length, 0, 'a closed control scheduled ' + timers.length + ' refit(s)');
  container.className += ' leaflet-control-layers-expanded';
  handlers.forEach((h) => h()); handlers.forEach((h) => h());
  assert.strictEqual(timers.length, 1, 'an open control did not coalesce a burst into one refit');
});

test('a refit of the open list keeps its scroll position (#841 F8)', () => {
  const timers = [];
  const ctx = { window: {}, Math, Array, setTimeout: (fn) => { timers.push(fn); return timers.length; } };
  vm.createContext(ctx);
  vm.runInContext(fs.readFileSync(path.join(__dirname, 'public', 'map-chrome-fit.js'), 'utf8'), ctx);
  const rect = (l, t, r, b) => () => ({ left: l, top: t, right: r, bottom: b, width: r - l, height: b - t });
  // The list as a browser lays it out: 600 px of rows, and a height of ''
  // (the full rows) leaves nothing to scroll, so scrollTop clamps to 0.
  let h = '', top = 0;
  const list = {
    scrollHeight: 600,
    classList: { add() {}, remove() {} },
    getBoundingClientRect() { return rect(10, 126, 280, h ? 126 + parseInt(h, 10) : 726)(); },
    get scrollTop() { return top; },
    set scrollTop(v) { top = Math.max(0, Math.min(v, h ? 600 - parseInt(h, 10) : 0)); },
  };
  list.style = {
    get height() { return h; },
    set height(v) { h = v; if (!v) top = 0; },
  };
  const container = {
    className: 'leaflet-control-layers leaflet-control-layers-expanded',
    querySelector: () => list,
    getBoundingClientRect() { const b = list.getBoundingClientRect(); return rect(10, 120, 280, b.bottom + 6)(); },
  };
  const handlers = [];
  const map = {
    on: (ev, fn) => handlers.push(fn), off() {},
    getContainer: () => ({ getBoundingClientRect: rect(0, 0, 375, 452), ownerDocument: { querySelectorAll: () => [] }, querySelectorAll: () => [] }),
  };
  const control = { _map: map, getContainer: () => container, expand() {} };
  ctx.window.MapChromeFit.fitLayerControl(map, control);
  control.expand();
  assert.ok(h, 'the list was not given a height, so this case proves nothing');
  list.scrollTop = 250;
  assert.strictEqual(list.scrollTop, 250);
  handlers.forEach((hd) => hd());
  timers.splice(0).forEach((fn) => fn());
  assert.strictEqual(list.scrollTop, 250, 'the refit threw the list back to scrollTop ' + list.scrollTop);
});

// ── MC_forgetMapLayerPrefs ─────────────────────────────────────────────────
const PREF_KEYS = ['mc-terrain-dhm', 'mc-contours-dhm', 'mc-heritage', 'mc-masts', 'mc-masts-int', 'mc-ortho-base'];

test('MC_forgetMapLayerPrefs forgets every layer-control choice and drops only their link parameters', () => {
  const storage = {};
  PREF_KEYS.forEach((k) => { storage[k] = '1'; });
  storage['mc-light-tile-provider'] = 'osm';
  const ctx = makeSandbox({ storage, hash: '#/map?lat=55.7&lon=9.4&zoom=15&terrain=surface&contours=1&heritage=1&masts=0&mastsInt=1&ortho=1' });
  ctx.window.MC_forgetMapLayerPrefs();
  PREF_KEYS.forEach((k) => assert.strictEqual(ctx.localStorage.getItem(k), null, k + ' kept'));
  assert.strictEqual(ctx.localStorage.getItem('mc-light-tile-provider'), 'osm', 'a key that is not the control\'s was dropped');
  assert.strictEqual(ctx.location.hash, '#/map?lat=55.7&lon=9.4&zoom=15');
  assert.strictEqual(ctx.replaced.length, 1);
});

test('MC_forgetMapLayerPrefs leaves a link without those parameters alone', () => {
  const ctx = makeSandbox({ hash: '#/nodes?tab=all' });
  ctx.window.MC_forgetMapLayerPrefs();
  assert.strictEqual(ctx.replaced.length, 0);
  assert.strictEqual(ctx.location.hash, '#/nodes?tab=all');
});

// ── colour inputs ──────────────────────────────────────────────────────────
test('colorValueAttr: #rrggbb for every colour it can read, nothing for one it cannot', () => {
  const { api } = loadCustomizer();
  assert.strictEqual(api.colorValueAttr('#4a9eff'), ' value="#4a9eff"');
  assert.strictEqual(api.colorValueAttr('#abc'), ' value="#aabbcc"');
  assert.strictEqual(api.colorValueAttr('rgba(255,255,255,0.85)'), ' value="#ffffff"');
  assert.strictEqual(api.colorValueAttr('rgba(74,158,255,0.15)'), ' value="#4a9eff"');
  assert.strictEqual(api.colorValueAttr(''), '');
  assert.strictEqual(api.colorValueAttr(undefined), '');
  assert.strictEqual(api.colorValueAttr('var(--warning)'), '');
  assert.strictEqual(api.colorValueAttr('"><img src=x>'), '');
});

console.log(`\n${passed} passed, ${failed} failed\n`);
process.exit(failed ? 1 : 0);
