/* test-issue-873-base-deep-link.js — #873: the base map deep link,
 * #/map?base=<style id> (and #/live, which shares the layer control).
 *
 *   R  resolveBase: a registry id offered for the theme resolves to itself;
 *      a wrong-theme id to its counterpart; a retired CARTO id to its
 *      migrated map; unknown, gated (not in the registry) and junk to null.
 *   L  Link on open: the linked map is the one shown, over a stored Aerial
 *      pick; an invalid link falls back to Auto and leaves the hash clean.
 *   H  Hash: a pick writes base=<id> with replaceState (no history entry),
 *      Auto removes it, other parameters are kept.
 *   S  Share: the Copy link of the profile panel carries base=.
 *
 * Runs via: node test-issue-873-base-deep-link.js
 */
'use strict';
const assert = require('assert');
const { makeSandbox, makeMap } = require('./tests/map-overlay-sandbox');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}

const DF = { enabled: true, token: 'x-873' };
const CFG = { tiles: { providers: { osm: { enabled: true }, esri: { enabled: true }, opentopomap: { enabled: true } } } };
const CFG_DF = { tiles: { providers: { osm: { enabled: true }, esri: { enabled: true }, dataforsyningen: DF } } };
const ORTHO = { enabled: true, url: '/api/map/dhm', ortho: { url: '/api/map/dhm/ortho/{z}/{x}/{y}', minZoom: 10, maxZoom: 19, bounds: [[54, 8], [58, 15.5]], attribution: 'SDFI' } };

function open(o) {
  o = o || {};
  const ctx = makeSandbox({ mapCfg: o.mapCfg || CFG, storage: o.storage, theme: o.theme || 'light', hash: o.hash });
  ctx.document.documentElement = { getAttribute: () => o.theme || 'light' };
  if (o.ortho) ctx.window.MC_DHM_CFG = ORTHO;
  const map = makeMap();
  const auto = { _auto: true };
  ctx.window.MC_createLayerControl(map, auto, 'topleft');
  const last = () => map._controls.filter(c => !c._removed).slice(-1)[0];
  const on = () => Object.keys(last().base).filter(id => map.hasLayer(last().base[id]));
  return { ctx, map, auto, last, on, pick: (id) => map.fire('baselayerchange', { name: id, layer: last().base[id] }) };
}

console.log('\n=== #873 R: resolveBase ===');
{
  const { ctx } = open({ mapCfg: CFG_DF });
  const LG = ctx.window.MC_LayerGroups;
  test('R1 a listed id resolves to itself', () => assert.strictEqual(LG.resolveBase('esri-imagery', false), 'esri-imagery'));
  test('R2 a wrong-theme id resolves to its counterpart', () => {
    assert.strictEqual(LG.resolveBase('osm-dark', false), 'osm-standard');
    assert.strictEqual(LG.resolveBase('osm-standard', true), 'osm-dark');
  });
  test('R3 unknown, empty and prototype-key values resolve to null', () => {
    ['nope', '', '   ', '__proto__', 'constructor', null, undefined].forEach(v => assert.strictEqual(LG.resolveBase(v, false), null, String(v)));
  });
  test('R4 a gated style (provider not enabled) resolves to null', () => assert.strictEqual(LG.resolveBase('opentopomap', false), null));
  test('R5 a retired CARTO id migrates to its keyless map', () => assert.strictEqual(LG.resolveBase('carto-light', false), 'osm-standard'));
}

console.log('\n=== #873 L: link on open ===');
test('L1 a linked id is shown instead of Auto', () => {
  const m = open({ hash: '#/map?base=esri-imagery' });
  assert.deepStrictEqual(m.on(), ['esri-imagery']);
  assert.ok(!m.map.hasLayer(m.auto));
});
test('L2 a wrong-theme link shows its counterpart (dark)', () => {
  const m = open({ hash: '#/map?base=osm-standard', theme: 'dark' });
  assert.deepStrictEqual(m.on(), ['osm-dark']);
});
test('L3 an unknown link falls back to Auto and leaves no base= in the hash', () => {
  const m = open({ hash: '#/map?base=nope&zoom=5' });
  assert.deepStrictEqual(m.on(), ['__auto__']);
  assert.strictEqual(m.ctx.location.hash, '#/map?zoom=5');
});
test('L4 a gated link (Dataforsyningen without a token) falls back to Auto', () => {
  const m = open({ hash: '#/map?base=dataforsyningen-skaermkort' });
  assert.ok(m.map.hasLayer(m.auto));
  assert.ok(!/base=/.test(m.ctx.location.hash), m.ctx.location.hash);
});
test('L5 the link wins over a stored Aerial pick, and the Aerial pick is forgotten', () => {
  const m = open({ hash: '#/map?base=esri-imagery', ortho: true, storage: { 'mc-ortho-base': '1' } });
  assert.deepStrictEqual(m.on(), ['esri-imagery']);
  assert.strictEqual(m.ctx.localStorage.getItem('mc-ortho-base'), '0');
});
test('L6 base=dhm-ortho opens the Aerial map when it is offered', () => {
  const m = open({ hash: '#/map?base=dhm-ortho', ortho: true });
  assert.deepStrictEqual(m.on(), ['dhm-ortho']);
});
test('L7 a valid link is kept in the hash', () => {
  const m = open({ hash: '#/map?base=esri-imagery' });
  assert.strictEqual(m.ctx.location.hash, '#/map?base=esri-imagery');
});
test('L8 the link is the session pick: a rebuilt control starts from it', () => {
  const m = open({ hash: '#/map?base=esri-imagery' });
  const map2 = makeMap();
  m.ctx.location.hash = '#/map';
  m.ctx.window.MC_createLayerControl(map2, { _auto: true }, 'topleft');
  const c = map2._controls.filter(x => !x._removed).slice(-1)[0];
  assert.ok(map2.hasLayer(c.base['esri-imagery']));
  assert.strictEqual(m.ctx.location.hash, '#/map?base=esri-imagery', 'the address bar follows the restored pick');
});

console.log('\n=== #873 H: hash updates ===');
test('H1 a pick writes base=<id> with replaceState, keeping other params', () => {
  const m = open({ hash: '#/map?zoom=5' });
  m.pick('esri-imagery');
  assert.strictEqual(m.ctx.location.hash, '#/map?zoom=5&base=esri-imagery');
});
test('H2 Auto removes base=', () => {
  const m = open({ hash: '#/map?base=esri-imagery&zoom=5' });
  m.pick('__auto__');
  assert.strictEqual(m.ctx.location.hash, '#/map?zoom=5');
});
test('H3 picking the Aerial map replaces base= (ortho keeps its own param)', () => {
  const m = open({ hash: '#/map?base=esri-imagery', ortho: true });
  m.pick('dhm-ortho');
  assert.ok(!/base=/.test(m.ctx.location.hash), m.ctx.location.hash);
  assert.ok(/ortho=1/.test(m.ctx.location.hash), m.ctx.location.hash);
});
test('H4 picks use replaceState only (no history entries)', () => {
  const m = open({ hash: '#/map' });
  let pushed = 0;
  m.ctx.history.pushState = () => { pushed++; };
  m.pick('esri-imagery'); m.pick('osm-standard'); m.pick('__auto__');
  assert.strictEqual(pushed, 0);
});
test('H5 a theme switch rewrites base= to the counterpart', () => {
  let theme = 'light', obs = null;
  const ctx = makeSandbox({ mapCfg: CFG, theme, hash: '#/map?base=osm-standard' });
  ctx.document.documentElement = { getAttribute: () => theme };
  ctx.MutationObserver = function (fn) { obs = fn; this.observe = () => {}; this.disconnect = () => {}; };
  const map = makeMap();
  ctx.window.MC_createLayerControl(map, { _auto: true }, 'topleft');
  theme = 'dark'; obs([]);
  assert.strictEqual(ctx.location.hash, '#/map?base=osm-dark');
});

console.log('\n=== #873 S: share link ===');
{
  const fs = require('fs'), vm = require('vm');
  const sb = { window: {}, console, URLSearchParams, document: {} };
  sb.window = sb; vm.createContext(sb);
  vm.runInContext(fs.readFileSync('public/terrain-profile.js', 'utf8'), sb);
  const TP = sb.TerrainProfile;
  const st = TP.parseState('profile=' + 'a'.repeat(64) + ',' + 'b'.repeat(64));
  const mk = (hash) => ({ origin: 'https://x.test', pathname: '/', search: '', hash });
  test('S1 the #/map copy link carries base=', () => {
    const u = TP.shareURL(st, mk('#/map?base=esri-imagery&zoom=3'));
    assert.ok(/[?&]base=esri-imagery(&|$)/.test(u), u);
    assert.ok(u.indexOf('#/map?') > 0, u);
  });
  test('S2 with no base= the link is as before', () => assert.ok(!/base=/.test(TP.shareURL(st, mk('#/map?zoom=3')))));
  test('S3 a junk base= (markup) is not copied', () => assert.ok(!/base=/.test(TP.shareURL(st, mk('#/map?base=%3Cscript%3E')))));
}

console.log('\n' + passed + ' passed, ' + failed + ' failed');
process.exit(failed ? 1 : 0);
