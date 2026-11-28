/**
 * #608 — Follow-up #600: per-observation route for path offsets.
 *
 * One content hash can be heard on several routes (cmd/server/store.go
 * StoreTx.routeMask: "one content hash is heard on multiple routes"). The
 * transmission-level `route_type` returned by the API describes the
 * first-ingested frame, NOT any particular observation. Several places in
 * public/packets.js and public/live.js used to read a path-offset byte out
 * of an OBSERVATION's own raw_hex while taking the offset from the
 * TRANSMISSION's route_type — wrong whenever that observation's frame was
 * actually heard on a different route. They now derive the route from the
 * SAME raw_hex they are slicing (public/app.js rawPathLenByte/
 * routeTypeFromRawHex, the ONE place that reads a frame's own header route).
 *
 * This file covers the two packets.js sites:
 *   1. buildGroupRowHtml's expanded CHILD row (col-hashsize) used to read
 *      the parent group's p.route_type to slice the CHILD's own raw_hex.
 *   2. renderDetail's computeBreakdownRanges() call and raw-hop-count cross
 *      check used pkt.route_type (transmission-level) to slice
 *      effectivePkt.raw_hex (the selected OBSERVATION's own raw_hex) —
 *      ObservationResp carries no route_type of its own, so this was the
 *      only route source ever consulted, and it could be wrong.
 *
 * (hop-filter.js / live.js groupIsMultibyte mixed-route coverage lives in
 * test-live-multibyte-filter.js and test-live.js.)
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
async function testAsync(name, fn) {
  try { await fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}

// Sandbox construction mirrors test-packets.js / test-issue-1849-trace-hashbytes.js.
function makeSandbox() {
  const registeredPages = {};
  const ctx = {
    window: { addEventListener: () => {}, removeEventListener: () => {}, dispatchEvent: () => {}, innerWidth: 1200, PacketFilter: null },
    document: {
      readyState: 'complete',
      createElement: (tag) => ({
        tagName: String(tag).toUpperCase(), id: '', textContent: '', innerHTML: '', className: '', style: {},
        appendChild: () => {}, setAttribute: () => {}, addEventListener: () => {},
        querySelectorAll: () => [], querySelector: () => null,
        classList: { add: () => {}, remove: () => {}, contains: () => false },
      }),
      head: { appendChild: () => {} }, getElementById: () => null,
      addEventListener: () => {}, removeEventListener: () => {},
      querySelectorAll: () => [], querySelector: () => null, body: { appendChild: () => {} },
    },
    console, Date, Infinity, Math, Array, Object, String, Number, JSON, RegExp,
    Error, TypeError, RangeError, parseInt, parseFloat, isNaN, isFinite,
    encodeURIComponent, decodeURIComponent,
    setTimeout: () => {}, clearTimeout: () => {}, setInterval: () => {}, clearInterval: () => {},
    fetch: () => Promise.resolve({ ok: true, json: () => Promise.resolve({}) }),
    performance: { now: () => Date.now() },
    localStorage: (() => { const s = {}; return { getItem: k => s[k] || null, setItem: (k, v) => { s[k] = String(v); }, removeItem: k => { delete s[k]; } }; })(),
    location: { hash: '' }, history: { replaceState: () => {} },
    CustomEvent: class CustomEvent {}, Map, Set, Promise, URLSearchParams,
    addEventListener: () => {}, removeEventListener: () => {}, dispatchEvent: () => {},
    requestAnimationFrame: (cb) => setTimeout(cb, 0),
    _registeredPages: registeredPages,
    registerPage: (name, handler) => { registeredPages[name] = handler; },
    // A single content hash heard over several routes still resolves to one
    // frame per observer: HopResolver is only consulted for name lookups.
    HopResolver: { ready: () => true, resolve: () => ({}), init: () => {}, serverHopEntry: () => null, nameForKey: () => null },
  };
  vm.createContext(ctx);
  return ctx;
}

function loadInCtx(ctx, file) {
  vm.runInContext(fs.readFileSync(file, 'utf8'), ctx, { filename: file });
  for (const k of Object.keys(ctx.window)) ctx[k] = ctx.window[k];
}

function loadPacketsSandbox() {
  const ctx = makeSandbox();
  loadInCtx(ctx, 'public/payload-labels.js');
  loadInCtx(ctx, 'public/roles.js');
  loadInCtx(ctx, 'public/app.js');
  loadInCtx(ctx, 'public/packet-helpers.js');
  // app.js's own `function registerPage` declaration overwrites the context
  // global set in makeSandbox (sloppy-mode top-level function declarations
  // become global-object properties) -- re-apply the capturing override
  // after app.js loads and before packets.js, same as test-packets.js.
  ctx.registerPage = (name, handler) => { ctx._registeredPages[name] = handler; };
  vm.runInContext(`
    window.HopDisplay = {
      renderHop: function(h, entry, opts) { return '<span>' + h + '</span>'; },
      _showFromBtn: function() {},
      conflictBadgeCount: function() { return 0; }
    };
  `, ctx);
  loadInCtx(ctx, 'public/packets.js');
  ctx.HopResolver = ctx.window.HopResolver = {
    ready: () => true, resolve: () => ({}), init: () => {}, serverHopEntry: () => null, nameForKey: () => null,
  };
  return ctx;
}

// Builds a raw frame: header (payloadType<<2 | route), 4 transport-code
// bytes on routes 0/3, path_len byte, path bytes, payload bytes.
// firmware/docs/packet_format.md.
function buildFrame(payloadType, route, pathLenByte, pathHexBytes, payloadHexBytes, codesOverride) {
  const hex2 = (n) => n.toString(16).padStart(2, '0').toUpperCase();
  const codes = (route === 0 || route === 3) ? (codesOverride || 'AABBCCDD') : '';
  return hex2((payloadType << 2) | route) + codes + hex2(pathLenByte) + pathHexBytes.join('') + payloadHexBytes.join('');
}

console.log('\n=== #608 A: buildGroupRowHtml child row reads the CHILD\'s own raw_hex route ===');
{
  const api = loadPacketsSandbox()._packetsTestAPI;

  // Group header: route_type=1 (FLOOD, offset 1) -- the TRANSMISSION-level
  // field, which the bug used for every child row regardless of that
  // child's own frame.
  const parent = {
    hash: 'grp608', count: 2, latest: '', observer_id: null,
    raw_hex: buildFrame(2, 1, 0x00, [], ['FF']), route_type: 1,
    payload_type: 2, decoded_json: '{}', path_json: '[]',
    observation_count: 2, observer_count: 2,
    _children: [
      // Child's own frame: route 3 (TRANSPORT_DIRECT, offset 5), path-len
      // byte 0x81 -> hashSize (0x81>>6)+1 = 3. The first transport-code
      // byte is deliberately 0x11, not the default 0xAA: 0xAA's top two
      // bits ("10") happen to match 0x81's, so a misread at the parent's
      // offset 1 would coincidentally also yield hashSize 3 and the
      // assertion below could not tell the two offsets apart. 0x11's top
      // two bits ("00") give hashSize 1 instead, so reading the wrong
      // offset is distinguishable from reading the right one.
      { id: 1, observer_id: null, payload_type: 2,
        raw_hex: buildFrame(2, 3, 0x81, ['AABBCC'], ['42'], '11BBCCDD'),
        path_json: '["AABBCC"]', hash: 'grp608' },
    ],
  };

  test('child col-hashsize reads 3 (its own TRANSPORT_DIRECT frame, offset 5), not what the parent\'s FLOOD route_type (offset 1) would give', () => {
    const html = api.buildGroupRowHtml(Object.assign({}, parent), 0);
    // Force expansion path: buildGroupRowHtml only emits child rows when
    // groupIsExpandedInView(p.hash) is true. Use the public toggle API.
    api._setExpanded('grp608', true);
    const html2 = api.buildGroupRowHtml(Object.assign({}, parent), 0);
    const rows = html2.match(/<tr class="group-child"[\s\S]*?<\/tr>/g) || [];
    assert.ok(rows.length === 1, 'expected exactly one child row, got ' + rows.length + '. Full html: ' + html2.slice(0, 400));
    const m = rows[0].match(/<td class="col-hashsize[^"]*"[^>]*>([^<]*)<\/td>/);
    assert.ok(m, 'child col-hashsize cell must be present');
    assert.strictEqual(m[1].trim(), '3',
      'child hashsize must be 3 (from its own raw_hex at offset 5), got ' + JSON.stringify(m[1]) +
      ' — a byte1 (offset 1) misread of the parent\'s route would give a different value');
  });
}

async function runDetailTests() {
console.log('\n=== #608 B: renderDetail reads the SELECTED OBSERVATION\'s own raw_hex route ===');
{
  // header payloadType=2 (TXT_MSG), route=3 (TRANSPORT_DIRECT): transport
  // codes AABBCCDD, path-len byte 0x41 (hashSize 2, 1 hop), 1 hop (AABB, 2
  // bytes), payload 11223344. 12 bytes total.
  const obsRawHex = buildFrame(2, 3, 0x41, ['AABB'], ['11', '22', '33', '44']);

  function makeData(pktRouteType) {
    return {
      packet: {
        id: 1, hash: 'detail608', payload_type: 2, route_type: pktRouteType,
        timestamp: '2026-01-01T00:00:00Z', decoded_json: '{}', path_json: '[]',
      },
      observations: [
        { id: 10, raw_hex: obsRawHex, path_json: '["AABB"]', timestamp: '2026-01-01T00:00:00Z' },
      ],
    };
  }

  async function renderWithRouteType(pktRouteType, warnings) {
    const ctx = loadPacketsSandbox();
    const elements = [];
    const makeEl = ctx.document.createElement;
    ctx.document.createElement = (tag) => { const el = makeEl(tag); elements.push(el); return el; };
    ctx.console = { warn: (...a) => warnings.push(a.join(' ')), log: () => {}, error: (...a) => console.error(...a) };
    const data = makeData(pktRouteType);
    ctx.api = async (path) => {
      if (path === '/observers') return [];
      if (path === '/packets/detail608') return data;
      return {};
    };
    const app = ctx.document.createElement('div');
    await ctx._registeredPages['packet-detail'].init(app, 'detail608');
    return elements;
  }

  // pkt.route_type=1 (FLOOD/offset 1) disagrees with the observation's own
  // TRANSPORT_DIRECT (offset 5) frame. Before #608, computeBreakdownRanges
  // and the hop-count cross-check both used pkt.route_type.
  await testAsync('hex legend includes "Transport Codes" (derived from the observation\'s own route, not pkt.route_type=FLOOD)', async () => {
    const warnings = [];
    const elements = await renderWithRouteType(1, warnings);
    const legend = elements.find(el => typeof el.innerHTML === 'string' && el.innerHTML.includes('class="swatch"'));
    assert.ok(legend, 'hex legend element not found among ' + elements.length + ' created elements');
    assert.ok(legend.innerHTML.includes('Transport Codes'),
      'legend must include "Transport Codes" (route 3, offset 5 correctly read) — got: ' + legend.innerHTML);
    assert.ok(legend.innerHTML.includes('Payload'),
      'legend must include "Payload" (full range computed past the path bytes) — got: ' + legend.innerHTML);
  });

  await testAsync('no spurious "Hop count inconsistency" warning when the observation\'s own route is used (1 hop both ways)', async () => {
    const warnings = [];
    await renderWithRouteType(1, warnings);
    const hopWarnings = warnings.filter(w => w.includes('Hop count inconsistency'));
    assert.strictEqual(hopWarnings.length, 0,
      'must not warn: path_json has 1 hop and the observation\'s own raw_hex path-len byte (offset 5) also encodes 1 hop. Got: ' + JSON.stringify(hopWarnings));
  });

  // Sanity: when pkt.route_type actually agrees with the observation's own
  // route, behavior is unchanged (no false negative from the fix itself).
  await testAsync('agreeing pkt.route_type (3) still renders the same legend', async () => {
    const warnings = [];
    const elements = await renderWithRouteType(3, warnings);
    const legend = elements.find(el => typeof el.innerHTML === 'string' && el.innerHTML.includes('class="swatch"'));
    assert.ok(legend && legend.innerHTML.includes('Transport Codes'));
    assert.strictEqual(warnings.filter(w => w.includes('Hop count inconsistency')).length, 0);
  });
}
}

runDetailTests().then(() => {
  console.log('\n--- ' + passed + ' passed, ' + failed + ' failed ---\n');
  process.exit(failed > 0 ? 1 : 0);
}).catch((e) => {
  console.error('Failed waiting for async tests:', e);
  process.exit(1);
});
