/**
 * Unit tests (#723 M3): the protected ancient monument warning in the Path
 * profile — public/terrain-profile.js.
 *
 * The real module runs in a vm context with a browser-shaped sandbox, as
 * test-issue-721-move-endpoints.js does; nothing here copies production
 * logic, and no request leaves the process: GET /api/terrain/profile and
 * GET /api/terrain/heritage are both answered by a recording stub.
 *
 * Covered:
 *   1. the note builder, heritageNote: inside a zone, within the radius,
 *      both ends, nothing near, a failed lookup, the feature disabled;
 *      the neutral wording (the statutory 100 m line could not be
 *      confirmed from public sources), never "you may build here"; dataset
 *      text escaped through the file's single esc();
 *   2. the panel: after a profile resolves, one lookup per end from the
 *      server's own endpoint coordinates, only with the heritage block; the
 *      chip and the note under the detail line, never in the title row
 *      (#784 review: it wrapped Copy/Close there); the verdict chip
 *      exactly as without the note; silence on failure, with no all-clear
 *      text anywhere; the note gone while re-measuring and on Close;
 *   3. the cost: height edits and model switches re-measure without a
 *      single new lookup, a moved end asks for that end only, a stale
 *      answer is never shown, and the remembered answers are bounded and
 *      least recently used out;
 *   4. the host: map.js passes the heritage block, style.css uses theme
 *      variables only for the new rules.
 *
 * Usage: node test-issue-723-profile-warning.js
 */
'use strict';
const fs = require('fs');
const assert = require('assert');

let passed = 0, failed = 0;
const tests = [];
function test(name, fn) { tests.push([name, fn]); }
const sleep = (ms) => new Promise(r => setTimeout(r, ms));
// The request after an edit is debounced (350 ms in terrain-profile.js).
const SETTLE_MS = 450;

// The sandbox, the stubbed endpoints and the answers live in
// tests/heritage-profile-sandbox.js, shared with the #723 M4 suite.
const {
  SRC, PK_A, PK_B, CFG, INSIDE, NEAR, NONE, WARNING, load, openTool, A1, B1, setPoints, ui, byEnd,
} = require('./tests/heritage-profile-sandbox');

// ── 1. The note builder ────────────────────────────────────────────────────

test('builder: inside a zone — the neutral warning, the type and "inside"', () => {
  const { TP } = load();
  const n = TP.heritageNote({ a: INSIDE, b: null }, CFG);
  assert(n, 'no note for an end inside a zone');
  assert.strictEqual(TP.HERITAGE_WARNING, WARNING);
  assert(n.text.indexOf(WARNING) === 0, 'text does not lead with the warning: ' + n.text);
  assert(/A: Rundhøj, inside its protection zone\./.test(n.text), 'A line: ' + n.text);
  assert(!/B:/.test(n.text), 'a B line without an answer for B');
  assert.strictEqual(n.label, 'In monument zone');
  assert.strictEqual(n.lead, 'a');
  assert(n.html.indexOf('<strong class="tp-heritage-warning">' + WARNING + '</strong>') === 0, 'html: ' + n.html);
  assert(/© Slots- og Kulturstyrelsen/.test(n.text) && /tp-heritage-credit/.test(n.html), 'credit missing');
});

test('builder: within the radius — the type and the distance to the zone', () => {
  const { TP } = load();
  const n = TP.heritageNote({ a: null, b: NEAR }, CFG);
  assert(n, 'no note for an end 42 m from a zone');
  assert(/B: Vandfyldt grav, about 42 m from its protection zone\./.test(n.text), 'B line: ' + n.text);
  assert.strictEqual(n.label, 'Monument zone ~42 m');
  assert.strictEqual(n.lead, 'b');
});

test('builder: both ends — one line each, A first; the end inside leads the chip', () => {
  const { TP } = load();
  const n = TP.heritageNote({ a: NEAR, b: INSIDE }, CFG);
  const ia = n.text.indexOf('A: Vandfyldt grav'), ib = n.text.indexOf('B: Rundhøj');
  assert(ia > 0 && ib > ia, 'order: ' + n.text);
  assert.strictEqual(n.lead, 'b');
  assert.strictEqual(n.label, 'In monument zone');
  const near2 = JSON.parse(JSON.stringify(NEAR)); near2.nearest.distanceM = 17;
  const m = TP.heritageNote({ a: NEAR, b: near2 }, CFG);
  assert.strictEqual(m.lead, 'b');
  assert.strictEqual(m.label, 'Monument zone ~17 m');
});

test('builder: outside a zone by less than 0.5 m — "at the edge", never "about 0 m"', () => {
  const { TP } = load();
  // The server rounds distanceM to whole metres, so 0 outside a zone is an
  // end on its edge.
  for (const d of [0, 0.4]) {
    const edge = JSON.parse(JSON.stringify(NEAR)); edge.nearest.distanceM = d;
    const n = TP.heritageNote({ a: null, b: edge }, CFG);
    assert(/B: Vandfyldt grav, at the edge of its protection zone\./.test(n.text), d + ' m: ' + n.text);
    assert(!/\b0 m\b/.test(n.text + n.label), 'reads "0 m": ' + n.text + ' / ' + n.label);
    assert.strictEqual(n.label, 'At monument zone edge');
  }
  // From half a metre on it is a distance again.
  const half = JSON.parse(JSON.stringify(NEAR)); half.nearest.distanceM = 0.5;
  assert(/about 1 m from its protection zone/.test(TP.heritageNote({ a: half }, CFG).text), 'half a metre');
  // Inside still wins over the edge, and the edge leads a farther end.
  const edge = JSON.parse(JSON.stringify(NEAR)); edge.nearest.distanceM = 0;
  assert.strictEqual(TP.heritageNote({ a: edge, b: INSIDE }, CFG).label, 'In monument zone');
  assert.strictEqual(TP.heritageNote({ a: NEAR, b: edge }, CFG).lead, 'b');
});

test('builder: nothing within the radius — no note, and nothing that reads as an all-clear', () => {
  const { TP } = load();
  assert.strictEqual(TP.heritageNote({ a: NONE, b: NONE }, CFG), null);
  // Truncated with nothing found is still nothing to say, not "none here".
  assert.strictEqual(TP.heritageNote({ a: Object.assign({}, NONE, { truncated: true }), b: NONE }, CFG), null);
});

test('builder: a failed lookup is silence; the other end still speaks', () => {
  const { TP } = load();
  assert.strictEqual(TP.heritageNote({ a: null, b: null }, CFG), null);
  assert.strictEqual(TP.heritageNote({}, CFG), null);
  assert.strictEqual(TP.heritageNote(null, CFG), null);
  const n = TP.heritageNote({ a: null, b: INSIDE }, CFG);
  assert(n && /B: Rundhøj/.test(n.text) && !/A:/.test(n.text), 'one end: ' + (n && n.text));
});

test('builder: disabled — no heritage block, or one not enabled, gives no note', () => {
  const { TP } = load();
  assert.strictEqual(TP.heritageNote({ a: INSIDE, b: NEAR }, null), null);
  assert.strictEqual(TP.heritageNote({ a: INSIDE, b: NEAR }, undefined), null);
  assert.strictEqual(TP.heritageNote({ a: INSIDE, b: NEAR }, Object.assign({}, CFG, { enabled: false })), null);
  assert.strictEqual(TP.heritageNote({ a: INSIDE, b: NEAR }, Object.assign({}, CFG, { enabled: 'true' })), null);
});

test('builder: wording is neutral — no legal claim, never "you may build"', () => {
  const { TP } = load();
  const texts = [TP.heritageNote({ a: INSIDE, b: NEAR }, CFG).text, TP.HERITAGE_WARNING];
  texts.forEach((t) => {
    assert(!/may build|can build|allowed|permitted|safe to build|no monument|all clear/i.test(t), 'reads as permission: ' + t);
    assert(!/dispensation|100 m protection line|§/i.test(t), 'states the unconfirmed legal line: ' + t);
  });
  assert(!/you may build/i.test(SRC), 'terrain-profile.js says "you may build"');
});

test('builder: dataset text is escaped through the file\'s single esc()', () => {
  const { TP } = load();
  const evil = '<img src=x onerror="window.__x=1">&\'';
  const bad = JSON.parse(JSON.stringify(INSIDE));
  bad.nearest.type = evil;
  bad.attribution = '<b>credit</b>';
  const n = TP.heritageNote({ a: bad, b: null }, CFG);
  assert(n.html.indexOf('<img') < 0 && n.html.indexOf('<b>') < 0, 'raw markup in the html: ' + n.html);
  assert(n.html.indexOf('&lt;img src=x onerror=&quot;window.__x=1&quot;&gt;&amp;&#39;') > 0, 'not esc()-escaped: ' + n.html);
  assert(n.text.indexOf(evil) > 0, 'the plain text lost the dataset value');
  assert.strictEqual((SRC.match(/function esc\(/g) || []).length, 1, 'a second esc() definition');
  // Every ${…}/+ value in the builder's html goes through esc: no other sink.
  const builder = SRC.slice(SRC.indexOf('function heritageNote('), SRC.indexOf('\n  }\n', SRC.indexOf('function heritageNote(')));
  assert(builder.length > 100 && /esc\(/.test(builder), 'builder not found or does not use esc');
});

test('builder: junk fields — a missing type, a non-numeric distance, a non-boolean inside flag', () => {
  const { TP } = load();
  const junk = { radiusM: 100, nearest: { type: '', distanceM: 'far', insideZone: 'yes' } };
  const n = TP.heritageNote({ a: junk, b: null }, CFG);
  assert(/A: Protected monument, near its protection zone\./.test(n.text), 'junk line: ' + n.text);
  assert.strictEqual(n.label, 'Monument zone');
  assert.strictEqual(TP.heritageNote({ a: { nearest: 'x' }, b: 5 }, CFG), null);
  const neg = { nearest: { type: 'Høj', distanceM: -3, insideZone: false } };
  assert(/near its protection zone/.test(TP.heritageNote({ a: neg }, CFG).text), 'a negative distance was shown');
});

// ── 2. The panel ───────────────────────────────────────────────────────────

test('panel: after the profile, one lookup per end from the server\'s endpoint coordinates', async () => {
  const env = load();
  const { tool, fetchJSON } = openTool(env, { heritage: byEnd(INSIDE, NEAR) });
  tool.setState({ a: { kind: 'node', pubkey: PK_A }, b: { kind: 'node', pubkey: PK_B } });
  await sleep(20);
  const h = fetchJSON.heritage();
  assert.deepStrictEqual(h, ['/api/terrain/heritage?lat=56.43001&lon=9.40002', '/api/terrain/heritage?lat=56.44003&lon=9.43004'], 'lookups: ' + h);
  // The lookups come after the profile, never before or instead of it.
  assert(fetchJSON.calls[0].indexOf('/api/terrain/profile?') === 0, 'first call ' + fetchJSON.calls[0]);
  const u = ui(env);
  assert(u.shown && u.noteShown, 'chip ' + u.shown + ' note ' + u.noteShown);
  assert.strictEqual(u.chip.textContent, 'In monument zone');
  assert(/tp-chip-warn/.test(u.chip.className), 'chip not in the warn tone: ' + u.chip.className);
  assert(/A: Rundhøj, inside/.test(u.chip.title) && /B: Vandfyldt grav, about 42 m/.test(u.chip.title), 'title ' + u.chip.title);
  assert(u.note.innerHTML.indexOf(WARNING) > 0 && /A: Rundhøj/.test(u.note.innerHTML) && /B: Vandfyldt grav/.test(u.note.innerHTML), 'note ' + u.note.innerHTML);
});

test('panel: the chip and then the note sit under the detail, not in the title row', () => {
  const env = load();
  const { tool } = openTool(env);
  tool.open();
  // #796 F5: the chips have their own row inside the title row.
  const chips = env.dom.find('tp-verdict').parentNode;
  assert(/\btp-chips\b/.test(chips.className), 'the verdict is not in the chip row: ' + chips.className);
  const head = chips.parentNode;
  const ids = head.children.map(c => c.attrs['data-testid'] || c.className);
  const cids = chips.children.map(c => c.attrs['data-testid'] || c.className);
  assert(ids.indexOf('tp-heritage-chip') < 0 && cids.indexOf('tp-heritage-chip') < 0,
    'the chip is in the title row: ' + ids.concat(cids).join(' | '));
  assert(ids.indexOf('tp-copy-link') >= 0 && ids.indexOf('tp-close') > ids.indexOf('tp-copy-link'), 'title row: ' + ids.join(' | '));
  const body = env.dom.find('tp-body');
  const bids = body.children.map(c => c.attrs['data-testid'] || c.className);
  assert(bids.indexOf('tp-chart') === 0, 'the chart is no longer first in the body: ' + bids.join(' | '));
  const id = bids.indexOf('tp-detail'), ih = bids.indexOf('tp-heritage-chip'), inote = bids.indexOf('tp-heritage-note');
  assert(id >= 0 && ih > id && inote === ih + 1, 'body order: ' + bids.join(' | '));
});

test('panel: the verdict chip and detail are exactly as without the heritage block', async () => {
  const snaps = [];
  for (const cfg of [null, CFG]) {
    const env = load();
    const { tool } = openTool(env, { cfg, heritage: () => INSIDE });
    setPoints(tool, A1, B1);
    await sleep(20);
    const u = ui(env);
    assert.strictEqual(u.shown, cfg !== null, 'chip shown ' + u.shown + ' with cfg ' + !!cfg);
    snaps.push({ verdict: u.verdictSnap, detail: u.detail });
  }
  assert.deepStrictEqual(snaps[1], snaps[0], 'the note changed the verdict');
  assert.strictEqual(snaps[0].verdict.state, 'fresnel60');
});

test('panel: no heritage block — no lookup at all', async () => {
  const env = load();
  const { tool, fetchJSON } = openTool(env, { cfg: null, heritage: () => INSIDE });
  setPoints(tool, A1, B1);
  await sleep(20);
  assert.strictEqual(fetchJSON.heritage().length, 0, 'lookups without the block');
  assert(!ui(env).shown && !ui(env).noteShown);
});

test('panel: nothing near and failed lookups stay silent — no chip, no note, no all-clear text', async () => {
  for (const answer of [NONE, new Error('Request failed (429)'), new Error('Request failed (502)')]) {
    const env = load();
    const { tool, fetchJSON } = openTool(env, { heritage: () => answer });
    setPoints(tool, A1, B1);
    await sleep(20);
    assert.strictEqual(fetchJSON.heritage().length, 2, 'lookups');
    const u = ui(env);
    assert(!u.shown && !u.noteShown, 'shown for ' + (answer.message || 'none'));
    assert.strictEqual(u.chip.textContent + u.note.innerHTML + u.chip.title, '', 'leftover text');
    assert(!/monument|heritage|protect/i.test(u.detail), 'detail mentions monuments: ' + u.detail);
    assert.strictEqual(u.verdictSnap.state, 'fresnel60', 'a lookup failure touched the verdict');
  }
});

test('panel: one end failing, the other inside — the note for that end only', async () => {
  const env = load();
  const { tool } = openTool(env, { heritage: byEnd(new Error('Request failed (504)'), INSIDE) });
  setPoints(tool, A1, B1);
  await sleep(20);
  const u = ui(env);
  assert(u.shown && /B: Rundhøj/.test(u.chip.title) && !/A:/.test(u.chip.title), 'title ' + u.chip.title);
});

test('panel: a failed profile asks for no monuments', async () => {
  const env = load();
  const { tool, fetchJSON } = openTool(env, { heritage: () => INSIDE });
  fetchJSON.failProfile = true;
  setPoints(tool, A1, B1);
  await sleep(20);
  assert.strictEqual(fetchJSON.heritage().length, 0);
  assert(!ui(env).shown);
});

test('panel: the note goes while re-measuring and on Close', async () => {
  const env = load();
  const { tool } = openTool(env, { heritage: () => INSIDE });
  setPoints(tool, A1, B1);
  await sleep(20);
  assert(ui(env).shown, 'not shown');
  tool.moveEndpoint('b', { kind: 'point', lat: 56.45, lon: 9.44 });
  assert(!ui(env).shown && !ui(env).noteShown, 'stale note kept while the moved end is measured');
  await sleep(SETTLE_MS);
  assert(ui(env).shown, 'not back after the move');
  tool.close();
  assert(!ui(env).shown && !ui(env).noteShown, 'note kept after Close');
  assert.strictEqual(ui(env).note.innerHTML, '', 'note html kept after Close');
});

// ── 3. Cost ────────────────────────────────────────────────────────────────

test('cost: height edits and a model switch re-measure with no new lookup', async () => {
  const env = load();
  const { tool, fetchJSON } = openTool(env, { heritage: () => INSIDE });
  setPoints(tool, A1, B1);
  await sleep(20);
  assert.strictEqual(fetchJSON.heritage().length, 2);
  const plus = env.dom.find('tp-h-a-plus');
  for (let i = 0; i < 5; i++) plus.fire('click');
  await sleep(SETTLE_MS);
  env.dom.find('tp-model-dtm').fire('click');
  await sleep(20);
  assert.strictEqual(fetchJSON.profiles().length, 3, 'profiles ' + fetchJSON.profiles().length);
  assert.strictEqual(fetchJSON.heritage().length, 2, 'lookups after edits: ' + fetchJSON.heritage().length);
  assert(ui(env).shown, 'the note did not come back from the remembered answers');
});

test('cost: moving one end asks for that end only; a swap asks for nothing', async () => {
  const env = load();
  const { tool, fetchJSON } = openTool(env, { heritage: () => NEAR });
  setPoints(tool, A1, B1);
  await sleep(20);
  tool.moveEndpoint('b', { kind: 'point', lat: 56.45, lon: 9.44 });
  await sleep(SETTLE_MS);
  assert.deepStrictEqual(fetchJSON.heritage().slice(2), ['/api/terrain/heritage?lat=56.45000&lon=9.44000']);
  tool.swap();
  await sleep(SETTLE_MS);
  assert.strictEqual(fetchJSON.heritage().length, 3, 'a swap looked up again');
});

test('cost: a failed lookup is not remembered — the next profile asks for that end again', async () => {
  const env = load();
  let down = true;
  const { tool, fetchJSON } = openTool(env, { heritage: (lat) => (Math.abs(lat - 56.43) < 0.005 ? (down ? new Error('Request failed (502)') : INSIDE) : NONE) });
  setPoints(tool, A1, B1);
  await sleep(20);
  assert(!ui(env).shown, 'shown after a failure');
  down = false;
  env.dom.find('tp-h-b-plus').fire('click');
  await sleep(SETTLE_MS);
  assert.deepStrictEqual(fetchJSON.heritage().slice(2), ['/api/terrain/heritage?lat=56.43000&lon=9.40000'], 'retry: ' + fetchJSON.heritage());
  assert(ui(env).shown && /A: Rundhøj/.test(ui(env).chip.title), 'not shown after the retry');
});

test('cost: an answer that lands after the ends moved is not shown', async () => {
  const env = load();
  let release;
  const held = new Promise(r => { release = r; });
  const { tool } = openTool(env, { heritage: (lat) => (Math.abs(lat - 56.43) < 1e-6 ? () => held : NONE) });
  setPoints(tool, A1, B1);
  await sleep(20);
  tool.moveEndpoint('a', { kind: 'point', lat: 56.46, lon: 9.45 });
  release(INSIDE);
  await sleep(20);
  assert(!ui(env).shown, 'a stale answer was shown during the debounce');
  await sleep(SETTLE_MS);
  assert(!ui(env).shown, 'the old end\'s answer was shown for the new end');
});

test('cost: the remembered answers are bounded — an old end is asked again', async () => {
  const env = load();
  const { tool, fetchJSON } = openTool(env, { heritage: () => NONE });
  setPoints(tool, A1, B1);
  await sleep(20);
  // Ten new ends, five each: more than the eight answers kept.
  for (let i = 1; i <= 5; i++) {
    tool.moveEndpoint('a', { kind: 'point', lat: 56.5 + i / 100, lon: 9.5 });
    await sleep(SETTLE_MS);
    tool.moveEndpoint('b', { kind: 'point', lat: 56.6 + i / 100, lon: 9.6 });
    await sleep(SETTLE_MS);
  }
  const before = fetchJSON.heritage().length;
  assert.strictEqual(before, 12, 'lookups ' + before);
  // A1 has not been an end for ten moves: moving A back asks again, B nothing.
  tool.moveEndpoint('a', { kind: 'point', lat: 56.43, lon: 9.4 });
  await sleep(SETTLE_MS);
  assert.deepStrictEqual(fetchJSON.heritage().slice(before), ['/api/terrain/heritage?lat=56.43000&lon=9.40000']);
});

test('cost: the remembered answers are least recently used out — an end in use is never asked again', async () => {
  const env = load();
  const { tool, fetchJSON } = openTool(env, { heritage: () => NONE });
  setPoints(tool, A1, B1);
  await sleep(20);
  // A stays put while B moves to eight new places: each profile uses A's
  // remembered answer again. Kept first-in-first-out, A's answer would be
  // the oldest when the ninth answer arrives and A would be asked again.
  for (let i = 1; i <= 8; i++) {
    tool.moveEndpoint('b', { kind: 'point', lat: 56.6 + i / 100, lon: 9.6 });
    await sleep(SETTLE_MS);
  }
  const h = fetchJSON.heritage();
  assert.strictEqual(h.length, 10, 'lookups: ' + h.join(' ; '));
  assert.strictEqual(h.filter(u => u === '/api/terrain/heritage?lat=56.43000&lon=9.40000').length, 1, 'A asked again: ' + h.join(' ; '));
  // B1 has not been used since the first profile, so it is the one dropped.
  tool.moveEndpoint('b', { kind: 'point', lat: 56.44, lon: 9.43 });
  await sleep(SETTLE_MS);
  assert.deepStrictEqual(fetchJSON.heritage().slice(10), ['/api/terrain/heritage?lat=56.44000&lon=9.43000']);
});

// ── 4. Host and CSS ────────────────────────────────────────────────────────

test('map.js hands the profile tool the heritage block', () => {
  const map = fs.readFileSync('public/map.js', 'utf8');
  const create = map.slice(map.indexOf('window.TerrainProfile.create({'), map.indexOf('});', map.indexOf('window.TerrainProfile.create({')));
  assert(/heritage:\s*window\.MC_getHeritageConfig\s*\?\s*window\.MC_getHeritageConfig\(\)\s*:\s*null/.test(create), 'create() is not given the heritage block');
});

test('style.css: the chip and note use theme variables only', () => {
  const css = fs.readFileSync('public/style.css', 'utf8').replace(/\/\*[\s\S]*?\*\//g, '');
  const rules = css.split('}').filter(r => /\.tp-(chip-heritage|heritage-)/.test(r));
  assert(rules.length >= 3, 'rules: ' + rules.length);
  rules.forEach(r => assert(!/#[0-9a-f]{3,8}\b|rgba?\(/i.test(r), 'hard-coded colour: ' + r));
  assert(rules.some(r => /\.tp-heritage-note\s*\{[^]*var\(--warning\)/.test(r)), 'note not in the warn colour');
});

// ── Run ────────────────────────────────────────────────────────────────────

(async () => {
  console.log('\n=== #723 M3: protected ancient monument warning in the Path profile ===');
  for (const [name, fn] of tests) {
    try { await fn(); passed++; console.log('  ✅ ' + name); }
    catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
  }
  console.log(`\n#723 M3 unit: ${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})();
