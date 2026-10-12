/**
 * test-xss-escape-sinks.js
 *
 * Sink-level XSS regression tests for #1536 (CVE-class adv_name escaping).
 *
 * Strategy: for each render sink that interpolates untrusted node / observer /
 * channel data into HTML, locate the exact template literal in the production
 * source via regex, evaluate it with malicious bindings, and assert the
 * rendered HTML contains the *escaped* form (not the raw payload).
 *
 * This is "DOM-grep" output-string testing — equivalent to driving the render
 * function and inspecting container.innerHTML, but without a real DOM. The
 * captured template is the literal one from source, so reverting the
 * escapeHtml() wrapper in production code makes the regex still match (now
 * the unsafe template), the eval produces unsafe HTML, and the assertions
 * flip red. This is what gates the per-sink fixes, not just the helper.
 *
 * Each test below MUST fail on revert of the corresponding sink fix. We
 * verify this manually by neutering escapeHtml to identity and re-running:
 *   every sink test flips red.
 */
'use strict';
const fs = require('fs');
const assert = require('assert');
const vm = require('vm');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log(`  ✅ ${name}`); }
  catch (e) { failed++; console.log(`  ❌ ${name}: ${e.message}`); }
}

// --- Malicious payloads exercised at every sink --------------------------
// TAG_PAYLOAD = inline <script>-style injection (must escape angle brackets)
// ATTR_PAYLOAD = single-quote attribute breakout (must escape ')
const TAG_PAYLOAD = '<img src=x onerror=alert(1)>';
const ATTR_PAYLOAD = "' onfocus=alert(1) '";

function assertNoXss(html, label) {
  // Raw <img with payload chars surviving → angle-bracket escape failed.
  if (/<img\b/i.test(html))
    throw new Error(label + ': raw <img tag survived → angle-bracket escape broken: ' + html);
  // Raw single-quote-followed-by-event-handler (attr breakout) survived.
  if (/'\s*onfocus\s*=/i.test(html))
    throw new Error(label + ": raw ' onfocus= survived → single-quote attr breakout: " + html);
  // Positive markers: escaped forms must appear.
  if (!html.includes('&lt;img'))
    throw new Error(label + ': missing &lt;img marker (tag was not escaped): ' + html);
  if (!html.includes('&#39;'))
    throw new Error(label + ": missing &#39; marker (single quote was not escaped): " + html);
}

// --- Load global escapeHtml from public/app.js into a real vm context ----
// We don't use the giant test-frontend-helpers sandbox — just extract the
// escapeHtml function definition and evaluate it standalone.
function loadEscapeHtml() {
  const src = fs.readFileSync('public/app.js', 'utf8');
  const m = src.match(/\/\* Global escapeHtml [\s\S]*?\nfunction escapeHtml\(s\) \{[\s\S]*?\n\}/);
  if (!m) throw new Error('could not locate global escapeHtml in public/app.js');
  const ctx = { escapeHtml: null };
  vm.createContext(ctx);
  vm.runInContext(m[0] + '\nthis.escapeHtml = escapeHtml;', ctx);
  return ctx.escapeHtml;
}
const escapeHtml = loadEscapeHtml();

// #333 F5: sinks that call a file-local escaper must be tested with THAT
// escaper, not with app.js's escapeHtml, or a weakened local escaper goes
// unnoticed. Lift the one-line `function esc(s) { … }` from the file.
function loadLocalEsc(file, ctxExtra) {
  const src = fs.readFileSync(file, 'utf8');
  const m = src.match(/\n\s*(function esc\(s\) \{[^\n]*\})/);
  if (!m) throw new Error('could not locate function esc(s) in ' + file);
  const ctx = Object.assign({ esc: null }, ctxExtra || {});
  vm.createContext(ctx);
  vm.runInContext(m[1] + '\nthis.esc = esc;', ctx);
  return ctx.esc;
}
const analyticsEsc = loadLocalEsc('public/analytics.js');
// customize-v2.js's esc() serializes a text node through a <div>
// (textContent → innerHTML). Node has no DOM, so model the serializer as the
// HTML spec defines it for text nodes: & < > and U+00A0 are escaped, quotes
// are not. The function under test is still the production esc().
const customizeEsc = loadLocalEsc('public/customize-v2.js', {
  document: {
    createElement: () => {
      let text = '';
      return {
        set textContent(v) { text = String(v); },
        get innerHTML() {
          return text.replace(/&/g, '&amp;').replace(/</g, '&lt;')
            .replace(/>/g, '&gt;').replace(/\u00a0/g, '&nbsp;');
        },
      };
    },
  },
});

// Text-context check for escapers that legitimately leave quotes alone
// (customize-v2.js esc): no live tag, and the tag is shown escaped.
function assertNoTagInText(html, label) {
  if (/<img\b/i.test(html))
    throw new Error(label + ': raw <img tag survived → angle-bracket escape broken: ' + html);
  if (!html.includes('&lt;img'))
    throw new Error(label + ': missing &lt;img marker (tag was not escaped): ' + html);
}

// --- Template extraction + eval ------------------------------------------
// Captures a template literal from a source file, given a regex with ONE
// group that contains the inner of the backtick-delimited template.
// Evaluates it with the supplied bindings + escapeHtml in scope.
function evalTemplate(srcFile, regex, bindings) {
  const src = fs.readFileSync(srcFile, 'utf8');
  const m = src.match(regex);
  if (!m) throw new Error(`template not found in ${srcFile} via ${regex}`);
  const tpl = m[1];
  const argNames = Object.keys(bindings);
  const args = Object.values(bindings);
  // truncate / encodeURIComponent / escapeHtml / safeEsc may appear inside
  // the captured template — provide harmless implementations.
  const truncate = (s, n) => (s == null ? '' : String(s).slice(0, n));
  const safeEsc = escapeHtml; // map.js fallback path → real escaper
  const fn = new Function(
    ...argNames,
    'escapeHtml', 'truncate', 'encodeURIComponent', 'safeEsc',
    'return `' + tpl + '`;'
  );
  return fn(...args, escapeHtml, truncate, encodeURIComponent, safeEsc);
}

// =========================================================================
// A. Helper contract — escapeHtml MUST escape all 5 OWASP chars
// =========================================================================
console.log('\n=== A. escapeHtml 5-char contract (gates app.js:795) ===');

test("escapeHtml(\"'\") === '&#39;' (single-quote attribute-breakout fix)", () => {
  assert.strictEqual(escapeHtml("'"), '&#39;');
});

test('escapeHtml covers the full 5-char OWASP set', () => {
  assert.strictEqual(escapeHtml('&'),  '&amp;');
  assert.strictEqual(escapeHtml('<'),  '&lt;');
  assert.strictEqual(escapeHtml('>'),  '&gt;');
  assert.strictEqual(escapeHtml('"'),  '&quot;');
  assert.strictEqual(escapeHtml("'"),  '&#39;');
});

test('escapeHtml escapes & first (no double-decoding)', () => {
  assert.strictEqual(escapeHtml('a & b'), 'a &amp; b');
});

test('escapeHtml(null/undefined) → empty string', () => {
  assert.strictEqual(escapeHtml(null), '');
  assert.strictEqual(escapeHtml(undefined), '');
});

// =========================================================================
// B. safeEsc defensive pin — must NOT be identity-passthrough (regression
//    pin for #1536: the original bug was `safeEsc = function(s){return s;}`)
// =========================================================================
console.log('\n=== B. safeEsc identity-passthrough pin (gates public/map.js:30) ===');

test('public/map.js safeEsc fallback is NOT identity (returns s)', () => {
  const src = fs.readFileSync('public/map.js', 'utf8');
  // Look up the safeEsc declaration block and the inline fallback function body.
  const m = src.match(/const safeEsc[\s\S]{0,400}?function\s*\(s\)\s*\{([\s\S]*?)\}/);
  assert.ok(m, 'safeEsc fallback function not found in map.js');
  const body = m[1];
  // The original bug: `return s;` with no escaping. Pin that this string
  // does NOT appear as the fallback body.
  assert.ok(
    !/^[\s]*return\s+s\s*;?\s*$/m.test(body.trim()),
    'safeEsc fallback is identity passthrough — XSS regression: ' + body
  );
  // And positively: the body must reference an escaper or the angle brackets.
  assert.ok(
    /&lt;|&amp;|escapeHtml/.test(body),
    'safeEsc fallback does not escape angle brackets / amp / call escapeHtml: ' + body
  );
});

// =========================================================================
// C. Sink-level DOM-grep tests
// =========================================================================
console.log('\n=== C. per-sink XSS containment ===');

// --- 1. public/app.js search dropdown — Node result row -------------------
test('app.js search dropdown: Node row escapes n.name', () => {
  const html = evalTemplate(
    'public/app.js',
    /(<div class="search-result-item"[^`]*?<span class="search-result-type">Node<\/span>[^`]*?)`/,
    { n: { name: TAG_PAYLOAD + ATTR_PAYLOAD, public_key: 'abcdef0123456789' } }
  );
  assertNoXss(html, 'app.js search Node row');
});

// --- 2. public/app.js search dropdown — Channel row -----------------------
test('app.js search dropdown: Channel row escapes c.name', () => {
  const html = evalTemplate(
    'public/app.js',
    /(<div class="search-result-item"[^`]*?<span class="search-result-type">Channel<\/span>[^`]*?)`/,
    { c: { name: TAG_PAYLOAD + ATTR_PAYLOAD, channel_hash: 'deadbeef' } }
  );
  assertNoXss(html, 'app.js search Channel row');
});

// --- 3. public/nodes.js nodes-table row — name <strong> cell --------------
test('nodes.js renderRow: <strong>name</strong> cell escapes node name', () => {
  // Match the <strong>${...n.name...}</strong> token specifically.
  const html = evalTemplate(
    'public/nodes.js',
    /(<strong>\$\{[^}]*n\.name[^}]*\}<\/strong>)/,
    { n: { name: TAG_PAYLOAD + ATTR_PAYLOAD, public_key: 'abc' } }
  );
  assertNoXss(html, 'nodes.js renderRow strong cell');
});

// --- 4. public/observers.js observers-table row — name cell ---------------
test('observers.js renderRow: observer name cell escapes o.name', () => {
  // Capture just the <td class="mono">${ ... o.name || o.id ... }${chip}</td>.
  const html = evalTemplate(
    'public/observers.js',
    // Allow optional <td> attributes before class="mono" (e.g. data-testid /
    // data-value added by #1639 for sortable-column tests) AND optional trailing
    // content (e.g. listener/repeater badge added by #1290) between the naive
    // chip and the closing </td>. Capturing any data-value="${...}" attr makes
    // this assertion STRICTER, not weaker: the eval now also renders those attr
    // interpolations, so dropping escapeHtml() on either the attr OR the cell
    // text trips assertNoXss (raw <img / attr-breakout markers).
    /(<td\b[^>]*\bclass="mono"[^>]*>\$\{[^}]*o\.name[^}]*\}\$\{window\.ObserversNaiveChip\.render\(o\)\}[^`]*?<\/td>)/,
    { o: { name: TAG_PAYLOAD + ATTR_PAYLOAD, id: 'obs-1' },
      window: { ObserversNaiveChip: { render: () => '' } } }
  );
  assertNoXss(html, 'observers.js obs name cell');
});

// --- 5. public/packets.js observer cell (grouped header — isSingle path) --
test('packets.js: grouped observer cell escapes observer name', () => {
  // Capture the isSingle ternary on the grouped header row.
  // Master form: `${isSingle ? truncate(obsNameOnly(headerObserverId), 16) + obsIataBadge(p) : ...}`
  // Fixed form:  `${isSingle ? escapeHtml(truncate(obsNameOnly(headerObserverId), 16)) + obsIataBadge(p) : ...}`
  const src = fs.readFileSync('public/packets.js', 'utf8');
  const m = src.match(
    /(<td class="col-observer"[^`]*?headerObserverId[^`]*?groupedObserverIataBadgesHtml\(p\)\}<\/td>)/
  );
  assert.ok(m, 'packets.js grouped observer cell template not found');
  const tpl = '`' + m[1] + '`';
  const fn = new Function(
    'isSingle','headerObserverId','escapeHtml','truncate','obsNameOnly',
    'obsIataBadge','groupedObserverIataBadgesHtml','p',
    'return ' + tpl + ';'
  );
  const html = fn(
    true, 'obs-1', escapeHtml,
    (s, n) => String(s).slice(0, n),
    () => TAG_PAYLOAD + ATTR_PAYLOAD,
    () => '', () => '', {}
  );
  assertNoXss(html, 'packets.js grouped observer cell');
});

// --- 6. public/packets.js child / flat observer cells --------------------
test('packets.js: flat observer cell escapes observer name', () => {
  // Capture the flat observer cell — the OUTER form, with or without
  // escapeHtml: `${truncate(obsNameOnly(p.observer_id), 16)}${obsIataBadge(p)}`
  // or            `${escapeHtml(truncate(obsNameOnly(p.observer_id), 16))}${obsIataBadge(p)}`.
  const src = fs.readFileSync('public/packets.js', 'utf8');
  // Find the FLAT row (the third occurrence with p.observer_id, not c.observer_id).
  const re = /\$\{(?:escapeHtml\()?truncate\(obsNameOnly\(p\.observer_id\),\s*16\)\)?\}\$\{obsIataBadge\(p\)\}/;
  const m = src.match(re);
  assert.ok(m, 'packets.js flat observer cell template not found');
  const tpl = '`<td>' + m[0] + '</td>`';
  const fn = new Function(
    'p','escapeHtml','truncate','obsNameOnly','obsIataBadge',
    'return ' + tpl + ';'
  );
  // Use short payloads — truncate(_, 16) would lop off the trailing single
  // quote of a longer string. Both must survive in the first 16 chars.
  const html = fn(
    { observer_id: 'obs-1' }, escapeHtml,
    (s, n) => String(s).slice(0, n),
    () => "'<img onerror=", // 14 chars — both ' and <img present
    () => ''
  );
  // Custom assertions (assertNoXss expects full payloads).
  assert.ok(!/<img/.test(html),
    'packets.js flat cell: raw <img survived: ' + html);
  assert.ok(!/^[^&]*'/.test(html.replace('<td>', '').replace('</td>','')),
    "packets.js flat cell: raw ' survived: " + html);
  assert.ok(html.includes('&lt;img'),
    'packets.js flat cell: &lt;img marker missing: ' + html);
  assert.ok(html.includes('&#39;'),
    'packets.js flat cell: &#39; marker missing: ' + html);
});

// --- 7. public/map.js Leaflet popup — observer popup ---------------------
test('map.js buildObserverPopup: observer name escaped (via real safeEsc)', () => {
  // Drive only the relevant template fragment. The full pipeline:
  //   name = safeEsc(obs.name || obs.id || 'Unknown')
  //   then `<h3>${name}</h3>`.
  // On master safeEsc is identity → name is raw → XSS.
  // On the fixed branch safeEsc falls back to a real escaper → name is safe.
  const src = fs.readFileSync('public/map.js', 'utf8');
  // Extract safeEsc fallback body.
  const m = src.match(/const safeEsc\s*=[\s\S]*?function\s*\(s\)\s*\{([\s\S]*?)\};?/);
  assert.ok(m, 'safeEsc declaration not found in map.js');
  const body = m[1];
  // Build a callable from the fallback body alone.
  const fallback = new Function('s', body);
  const name = fallback(TAG_PAYLOAD + ATTR_PAYLOAD);
  const html = '<h3>' + name + '</h3>';
  assertNoXss(html, 'map.js observer popup (safeEsc fallback)');
});

// --- 8. public/live.js hop popup (buildClickablePathPopupHtml) ----------
test('live.js buildClickablePathPopupHtml: hopNames join escapes each hop', () => {
  // Extract the function body and evaluate with a malicious hopNames array.
  const src = fs.readFileSync('public/live.js', 'utf8');
  const m = src.match(
    /function buildClickablePathPopupHtml\(typeName, color, hopNames, tsMs, hash\)\s*\{([\s\S]*?)\n\s*\}/
  );
  assert.ok(m, 'buildClickablePathPopupHtml not found');
  const body = m[1];
  const html = new Function(
    'typeName','color','hopNames','tsMs','hash','escapeHtml',
    body + '\n;' // body returns
  )(
    'CHAN', '#fff',
    [TAG_PAYLOAD, ATTR_PAYLOAD],
    Date.now(), 'abc123',
    escapeHtml
  );
  assertNoXss(html, 'live.js hop popup');
});

// --- 9. public/area-map.html node popup ----------------------------------
test('area-map.html node popup: node name escaped (local helper handles 5 chars)', () => {
  const src = fs.readFileSync('public/area-map.html', 'utf8');
  // Local escapeHtml must exist AND escape single quotes.
  const localM = src.match(/function escapeHtml\(s\)\s*\{[\s\S]*?\n\}/);
  assert.ok(localM, 'area-map.html local escapeHtml not found');
  assert.ok(/&#39;/.test(localM[0]),
    'area-map.html local escapeHtml missing &#39; (single-quote) escape: ' + localM[0]);
  // Eval the local helper.
  const localEsc = new Function('s',
    localM[0] + '\nreturn escapeHtml(s);');
  assert.strictEqual(localEsc("'"), '&#39;');
  // Drive the buildNodeLayer popup template (which uses ${escapeHtml(n.name||...)}
  // on the fixed branch; on master it's raw ${n.name || ...}).
  const popM = src.match(
    /(<div class="node-popup"><strong>\$\{(?:escapeHtml\()?n\.name[^`]*?<\/div>)/
  );
  assert.ok(popM, 'area-map node popup template not found');
  const tpl = popM[1];
  const html = new Function('n','areaKey','escapeHtml',
    'return `' + tpl + '`;'
  )(
    { name: TAG_PAYLOAD + ATTR_PAYLOAD,
      public_key: TAG_PAYLOAD + ATTR_PAYLOAD,
      lat: 0, lon: 0 },
    TAG_PAYLOAD + ATTR_PAYLOAD,
    localEsc
  );
  assertNoXss(html, 'area-map.html node popup');
});

// --- 10. public/hop-display.js — data-conflict single-quoted attribute --
test('hop-display.js data-conflict attribute: single-quote payload escaped', () => {
  // The sink: data-conflict='${escapeHtml(JSON.stringify({...}))}'.
  // Without ' in escapeHtml, a node name containing ' breaks out of the attr.
  // The fix is in escapeHtml itself; this asserts the contract holds end-to-end.
  const payload = JSON.stringify({ name: ATTR_PAYLOAD });
  const escaped = escapeHtml(payload);
  assert.ok(!/'/.test(escaped),
    "escapeHtml(JSON containing ') still contains raw ': " + escaped);
  assert.ok(/&#39;/.test(escaped),
    'escapeHtml output missing &#39; marker: ' + escaped);
  // And the source file still uses this attribute pattern.
  const src = fs.readFileSync('public/hop-display.js', 'utf8');
  assert.ok(/data-conflict='\$\{(escapeHtml\(JSON\.stringify|conflictData)/.test(src),
    'hop-display.js data-conflict attribute pattern changed unexpectedly');
});

// =========================================================================
// D. XSS sweep R2 — TRACE-1 / OBS-1 / ANL-1 (post-#1537 audit findings)
// =========================================================================
console.log('\n=== D. XSS R2: TRACE-1 / OBS-1 / ANL-1 ===');

// ----- TRACE-1: traces.js <input value="${urlHash}"> -----
// urlHash comes from URL fragment, interpolated raw → URL-fragment XSS.
test('TRACE-1: traces.js urlHash interpolation escapes URL-fragment payload', () => {
  const src = fs.readFileSync('public/traces.js', 'utf8');
  const m = src.match(/(<input type="text" id="traceHashInput"[^`]*?value="\$\{[^}]+\}"[^`]*?>)/);
  assert.ok(m, 'TRACE-1: <input value="${urlHash}"> template not found in traces.js');
  const tpl = m[1];
  const fn = new Function('urlHash', 'escapeHtml',
    'return `' + tpl + '`;');
  const html = fn(TAG_PAYLOAD + ATTR_PAYLOAD, escapeHtml);
  assertNoXss(html, 'TRACE-1 traces.js urlHash input');
});

// ----- OBS-1: observer-detail.js obs.* fields → renderDetail innerHTML -----
function extractObsStatValue(field) {
  const src = fs.readFileSync('public/observer-detail.js', 'utf8');
  const labelMap = { model: 'Model', firmware: 'Firmware', client_version: 'Client', iata: 'Region' };
  const label = labelMap[field];
  const re = new RegExp(
    '<div class="stat-label">' + label + '<\\/div>[\\s\\S]{0,250}?' +
    '<div class="stat-value"[^>]*>(\\$\\{[^}]*obs\\.' + field + '[^}]*\\})<\\/div>'
  );
  const m = src.match(re);
  if (!m) throw new Error('OBS-1: stat-value for ' + field + ' not found');
  return m[1];
}

test('OBS-1: observer-detail.js obs.model is escaped at render sink', () => {
  const expr = extractObsStatValue('model');
  const tpl = '<div>' + expr + '</div>';
  const fn = new Function('obs', 'escapeHtml', 'return `' + tpl + '`;');
  const html = fn({ model: TAG_PAYLOAD + ATTR_PAYLOAD }, escapeHtml);
  assertNoXss(html, 'OBS-1 obs.model');
});

test('OBS-1: observer-detail.js obs.firmware is escaped at render sink', () => {
  const expr = extractObsStatValue('firmware');
  const tpl = '<div>' + expr + '</div>';
  const fn = new Function('obs', 'escapeHtml', 'return `' + tpl + '`;');
  const html = fn({ firmware: TAG_PAYLOAD + ATTR_PAYLOAD }, escapeHtml);
  assertNoXss(html, 'OBS-1 obs.firmware');
});

test('OBS-1: observer-detail.js obs.client_version is escaped at render sink', () => {
  const expr = extractObsStatValue('client_version');
  const tpl = '<div>' + expr + '</div>';
  const fn = new Function('obs', 'escapeHtml', 'return `' + tpl + '`;');
  const html = fn({ client_version: TAG_PAYLOAD + ATTR_PAYLOAD }, escapeHtml);
  assertNoXss(html, 'OBS-1 obs.client_version');
});

// ----- ANL-1: analytics.js mutation-XSS via data-tip dataset round-trip -----
// `:1599` writes escapeHtml-escaped tipHtml into data-tip (attr storage
// entity-decodes), and `:1524` does `tip.innerHTML = td.dataset.tip` which
// reanimates the raw payload. Fix A: tip.textContent = td.dataset.tip.
test('ANL-1: analytics.js does NOT assign td.dataset.tip to tip.innerHTML', () => {
  const src = fs.readFileSync('public/analytics.js', 'utf8');
  const broken = /\btip\.innerHTML\s*=\s*td\.dataset\.tip\b/;
  assert.ok(!broken.test(src),
    'ANL-1: `tip.innerHTML = td.dataset.tip` still present (mutation-XSS). Use textContent.');
});

test('ANL-1: hashCellTd now emits per-field data-tip-* attrs (no HTML round-trip)', () => {
  // Post-#1539-round-2 contract (see test-anl1-tooltip-render.js for the
  // behavioral assertions). The XSS gate here: hashCellTd must NOT carry a
  // pre-rendered HTML string in a single data-tip attribute. Per-field
  // attrs (data-tip-hex / data-tip-status / data-tip-lines) are plain text
  // and are rebuilt into DOM via createElement + textContent.
  const src = fs.readFileSync('public/analytics.js', 'utf8');
  const m = src.match(/function hashCellTd\([^)]*\)\s*\{([\s\S]*?)\n\s{2}\}/);
  assert.ok(m, 'ANL-1: hashCellTd function not found');
  const body = m[1];
  assert.ok(!/data-tip\s*=\s*"\$\{tipHtml/.test(body),
    'ANL-1: hashCellTd still writes pre-rendered HTML to data-tip="${tipHtml...}"');
  assert.ok(/data-tip-hex/.test(body),
    'ANL-1: hashCellTd missing data-tip-hex attribute (spec-driven contract)');
  // Mouseover handler must use the new per-field reader, not innerHTML.
  assert.ok(/buildMatrixTipChildren\s*\(\s*tip\s*,\s*td\s*\)/.test(src),
    'ANL-1: mouseover handler no longer calls buildMatrixTipChildren(tip, td)');
  assert.ok(!/\btip\.innerHTML\s*=\s*td\.dataset\.tip\b/.test(src),
    'ANL-1: tip.innerHTML = td.dataset.tip is the mutation-XSS regression');
});

// =========================================================================
// E. OBS-1 expanded surface — iata, id, radio split parts (PR #1539 follow-up)
//     and OBS-2 Number() coercion at battery/uptime/noise_floor sinks.
// =========================================================================
console.log('\n=== E. OBS-1 expanded + OBS-2 Number() coercion ===');

// Capture the entire renderDetail template-literal block once. The block
// runs `el.innerHTML = \`<long template>\`` — we extract the template body
// between the first ${window.ObserverDetailNaiveBanner.render(obs)} marker
// and the closing `;`.
function loadRenderDetailTemplate() {
  const src = fs.readFileSync('public/observer-detail.js', 'utf8');
  const m = src.match(/el\.innerHTML\s*=\s*`([\s\S]*?)`;/);
  if (!m) throw new Error('renderDetail template literal not found');
  return m[1];
}

// Extract the PRODUCTION directNeighborRoleIcon + renderDirectNeighbors pair
// via named-marker slicing (indexOf, not a regex) -- the pair has nested
// braces (a .map(function(n) {...}) callback) that a bracket-counting regex
// would be fragile against. Same technique as this repo's other "extract a
// real block and eval it" tests (see public/analytics.js's REPEATER METRICS
// BLOCK markers). renderDirectNeighbors is the sink this gate is for: it
// escapes n.name/n.scopes/n.pubkey at render time, and reverting any of
// those escapeHtml() calls in production must flip this test red -- so we
// run the real function, never a reimplementation or a payload-blind stub.
function loadDirectNeighborsBlock() {
  const src = fs.readFileSync('public/observer-detail.js', 'utf8');
  const start = src.indexOf('function directNeighborRoleIcon');
  const end = src.indexOf('window.renderDirectNeighbors = renderDirectNeighbors;');
  if (start < 0 || end < 0) throw new Error('renderObsDetail: directNeighborRoleIcon/renderDirectNeighbors block not found');
  return src.slice(start, end);
}

function renderObsDetail(obs, neighborsData) {
  const src = fs.readFileSync('public/observer-detail.js', 'utf8');
  const tpl = loadRenderDetailTemplate();
  // Extract the PRODUCTION radio-parsing block from source so reverting the
  // escapeHtml() calls inside that block flips red here. Don't duplicate it
  // in the test driver — that decouples the assertion from the fix.
  const radioBlockM = src.match(/let radioHtml[\s\S]*?if \(obs\.radio\) \{[\s\S]*?\n\s*\}/);
  if (!radioBlockM) throw new Error('renderObsDetail: radio-parsing block not found');
  const radioBlock = radioBlockM[0];
  const directNeighborsBlock = loadDirectNeighborsBlock();
  // Stub out window globals and helpers used inside the template.
  const sandbox = {
    window: { ObserverDetailNaiveBanner: { render: () => '' } },
    escapeHtml,
    HEALTH_THRESHOLDS: { nodeDegradedMs: 1800000 },
    obsSkew: null,
    formatDuration: (s) => (s ? Math.floor(s / 60) + 'm' : '—'),
    timeAgo: () => '—',
    formatSkew: () => '',
    renderSkewBadge: () => '',
    observerSkewSeverity: () => 'ok',
    // Default: no direct-neighbor rows, so callers that don't care about
    // this sink (OBS-1/OBS-2 tests) get the static "no data yet" branch,
    // which carries no payload markers and can't disturb their assertions.
    neighborsData: neighborsData || { neighbors: [] },
  };
  const ago = obs.last_seen ? Date.now() - new Date(obs.last_seen).getTime() : Infinity;
  const statusCls = ago < 600000 ? 'health-green' : ago < sandbox.HEALTH_THRESHOLDS.nodeDegradedMs ? 'health-yellow' : 'health-red';
  const statusLabel = ago < 600000 ? 'Online' : ago < sandbox.HEALTH_THRESHOLDS.nodeDegradedMs ? 'Stale' : 'Offline';
  // Eval the radio block + the real directNeighborRoleIcon/renderDirectNeighbors
  // pair in a fn body, then return the template render.
  const fn = new Function(
    'obs', 'obsSkew', 'escapeHtml', 'window', 'HEALTH_THRESHOLDS',
    'formatDuration', 'timeAgo', 'formatSkew', 'renderSkewBadge', 'observerSkewSeverity',
    'statusCls', 'statusLabel', 'neighborsData',
    radioBlock + '\n' + directNeighborsBlock + '\nreturn `' + tpl + '`;'
  );
  return fn(
    obs, sandbox.obsSkew, escapeHtml, sandbox.window, sandbox.HEALTH_THRESHOLDS,
    sandbox.formatDuration, sandbox.timeAgo, sandbox.formatSkew,
    sandbox.renderSkewBadge, sandbox.observerSkewSeverity,
    statusCls, statusLabel, sandbox.neighborsData
  );
}

test('OBS-1 expanded: obs.iata payload is escaped', () => {
  const html = renderObsDetail({ id: 'obs-1', iata: TAG_PAYLOAD + ATTR_PAYLOAD });
  assertNoXss(html, 'OBS-1 obs.iata');
});

test('OBS-1 expanded: obs.id payload is escaped', () => {
  const html = renderObsDetail({ id: TAG_PAYLOAD + ATTR_PAYLOAD });
  assertNoXss(html, 'OBS-1 obs.id');
});

test('OBS-1 expanded: obs.radio[0] (frequency) payload is escaped', () => {
  const html = renderObsDetail({ id: 'obs-1', radio: TAG_PAYLOAD + ATTR_PAYLOAD + ',125,7,5' });
  assertNoXss(html, 'OBS-1 obs.radio[0]');
});

test('OBS-1 expanded: obs.radio[1] (BW) payload is escaped', () => {
  const html = renderObsDetail({ id: 'obs-1', radio: '868.5,' + TAG_PAYLOAD + ATTR_PAYLOAD + ',7,5' });
  assertNoXss(html, 'OBS-1 obs.radio[1]');
});

test('OBS-1 expanded: obs.radio[2] (SF) payload is escaped', () => {
  const html = renderObsDetail({ id: 'obs-1', radio: '868.5,125,' + TAG_PAYLOAD + ATTR_PAYLOAD + ',5' });
  assertNoXss(html, 'OBS-1 obs.radio[2]');
});

test('OBS-1 expanded: obs.radio[3] (CR) payload is escaped', () => {
  const html = renderObsDetail({ id: 'obs-1', radio: '868.5,125,7,' + TAG_PAYLOAD + ATTR_PAYLOAD });
  assertNoXss(html, 'OBS-1 obs.radio[3]');
});

// ----- OBS-2: Number() coercion strips string XSS payloads at sinks -----
test('OBS-2: contaminated string obs.battery_mv is Number()-coerced (no payload in HTML)', () => {
  const html = renderObsDetail({ id: 'obs-1', battery_mv: TAG_PAYLOAD });
  // Number(TAG_PAYLOAD) is NaN → renders as '—'. Raw payload must NOT survive.
  if (/<img\b/i.test(html))
    throw new Error('OBS-2 battery_mv: Number() coercion missing — raw <img survived: ' + html);
  if (html.includes(TAG_PAYLOAD))
    throw new Error('OBS-2 battery_mv: raw payload survived: ' + html);
});

test('OBS-2: contaminated string obs.noise_floor is Number()-coerced (no payload in HTML)', () => {
  const html = renderObsDetail({ id: 'obs-1', noise_floor: TAG_PAYLOAD });
  if (/<img\b/i.test(html))
    throw new Error('OBS-2 noise_floor: Number() coercion missing — raw <img survived: ' + html);
  if (html.includes(TAG_PAYLOAD))
    throw new Error('OBS-2 noise_floor: raw payload survived: ' + html);
});

// =========================================================================
// H. Direct Neighbors panel (#1865 follow-up, PR #1867) — the sink this
// gate exists for. renderDirectNeighbors is now run for real (see
// loadDirectNeighborsBlock above), so these assert against the production
// escaping, not a reimplementation. n.role and n.status are deliberately
// NOT payload-tested here: neither is ever written into the HTML string —
// role only selects an SVG icon via a switch (unmatched values fall to a
// safe default), and status only feeds a strict `=== 'timeout'` compare —
// so there is no sink to exercise for either field.
// =========================================================================
console.log('\n=== H. Direct Neighbors panel: neighbor-row payload containment ===');

test('Direct Neighbors: neighbor.name (linked row) payload is escaped', () => {
  const html = renderObsDetail({ id: 'obs-1' }, {
    neighbors: [{ pubkey: 'AABBCCDDEEFF0011', name: TAG_PAYLOAD + ATTR_PAYLOAD, role: 'repeater', seenViaPackets: true }],
  });
  assertNoXss(html, 'Direct Neighbors neighbor.name');
});

test('Direct Neighbors: neighbor.scopes (Configured Scope column) payload is escaped', () => {
  const html = renderObsDetail({ id: 'obs-1' }, {
    neighbors: [{ pubkey: 'AABBCCDDEEFF0011', name: 'Real Node', role: 'room', scopes: TAG_PAYLOAD + ATTR_PAYLOAD, seenViaPackets: false }],
  });
  assertNoXss(html, 'Direct Neighbors neighbor.scopes');
});

test('Direct Neighbors: neighbor.pubkey (unnamed row + sparkline id attribute) payload is escaped', () => {
  // No n.name -> falls to the shortPubkey branch (escapeHtml on a slice of
  // pubkey) AND the sparkline placeholder's id="nb-spark-${escapeHtml(pubkey)}"
  // attribute -- two separate sinks fed by the same field in one assertion.
  const html = renderObsDetail({ id: 'obs-1' }, {
    neighbors: [{ pubkey: TAG_PAYLOAD + ATTR_PAYLOAD, role: 'sensor', status: 'timeout', seenViaPackets: false }],
  });
  assertNoXss(html, 'Direct Neighbors neighbor.pubkey');
});

// =========================================================================
// F. PR #1539 follow-up — renderRecentPackets must not emit inline onclick=
// =========================================================================
console.log('\n=== F. renderRecentPackets: no inline onclick (CSP / XSS-amplifier) ===');

test('renderRecentPackets row template does NOT use inline onclick=', () => {
  const src = fs.readFileSync('public/observer-detail.js', 'utf8');
  // Locate the renderRecentPackets function body and grep for onclick=.
  const m = src.match(/function renderRecentPackets\([^)]*\)\s*\{([\s\S]*?)\n\s{2}\}/);
  assert.ok(m, 'renderRecentPackets function not found');
  const body = m[1];
  // Strip JS line comments before grepping — explanatory comments are allowed
  // to mention the word `onclick=`. The contract is on the rendered HTML.
  const code = body.replace(/\/\/[^\n]*/g, '').replace(/\/\*[\s\S]*?\*\//g, '');
  assert.ok(!/\bonclick\s*=\s*['"]/i.test(code),
    'renderRecentPackets still contains inline onclick= attribute');
  // And positive contract: a delegated click listener on `el` exists.
  assert.ok(/el\.addEventListener\(\s*['"]click['"]/.test(body),
    'renderRecentPackets missing delegated click listener (data-action=navigate dispatch)');
});

// =========================================================================
// G. PR #1539 follow-up — loadDetail error path uses textContent
// =========================================================================
console.log('\n=== G. loadDetail error path: textContent over innerHTML ===');

test('loadDetail catch block does NOT interpolate e.message into innerHTML', () => {
  const src = fs.readFileSync('public/observer-detail.js', 'utf8');
  const m = src.match(/catch\s*\(\s*e\s*\)\s*\{([\s\S]*?)\n\s{4}\}/);
  assert.ok(m, 'loadDetail catch block not found');
  const body = m[1];
  // The old form interpolated e.message directly into a string assigned to
  // innerHTML. New form uses textContent.
  assert.ok(!/innerHTML\s*=\s*[^;]*e\.message/.test(body),
    'loadDetail still interpolates e.message into innerHTML: ' + body);
  assert.ok(/textContent\s*=\s*[^;]*e\.message/.test(body),
    'loadDetail missing textContent assignment for e.message: ' + body);
});

// =========================================================================
// I. Issue #333 — node/observer/route values in favorites, geo-filter prune
//    preview, observer regions, packet region badges, clock-skew observer
//    names, RF-health attributes and the unknown-route heading.
//
// Each test extracts the PRODUCTION sink (dual-form regex matching both the
// escaped and the unescaped shape) and evals it with a hostile binding, so
// removing the escape wrapper in production flips the test red (the mutant).
// =========================================================================
console.log('\n=== I. Issue #333 sinks ===');

// 1. Favorites dropdown (app.js) — node name in the fav-dd-name span.
test('#333 favorites dropdown: fav-dd-name span escapes node name', () => {
  const src = fs.readFileSync('public/app.js', 'utf8');
  const m = src.match(/<span class="fav-dd-name">'\s*\+\s*([\s\S]*?)\s*\+\s*'<\/span>'/);
  assert.ok(m, '#333 favorites: fav-dd-name concat segment not found');
  const fn = new Function('h', 'pk', 'escapeHtml', 'truncate',
    'return `<span>` + (' + m[1] + ') + `</span>`;');
  const html = fn(
    { node: { name: TAG_PAYLOAD + ATTR_PAYLOAD } }, 'abcdef0123456789',
    escapeHtml, (s, n) => (s == null ? '' : String(s).slice(0, n)));
  assertNoXss(html, '#333 favorites dropdown name');
});

// 2. Geo-filter prune preview (customize-v2.js) — node name in list item.
test('#333 geo-filter prune preview escapes node name', () => {
  const src = fs.readFileSync('public/customize-v2.js', 'utf8');
  const m = src.match(/return '<div>'\s*\+\s*([\s\S]*?)\s*\+\s*coords\s*\+\s*'<\/div>';/);
  assert.ok(m, '#333 geo-prune: prune list item concat not found');
  // #333 F5: run customize-v2.js's own esc() (text context, quotes are
  // legitimately left alone there), not app.js's escapeHtml.
  const fn = new Function('n', 'esc', 'coords',
    'return `<div>` + (' + m[1] + ') + coords + `</div>`;');
  const html = fn({ name: TAG_PAYLOAD + ATTR_PAYLOAD, pubkey: 'abcdef012345' }, customizeEsc, '');
  assertNoTagInText(html, '#333 geo-filter prune preview name');
});

// 3a. Observer regions — observers table badge (template-literal form).
test('#333 observers table region badge escapes o.iata', () => {
  const src = fs.readFileSync('public/observers.js', 'utf8');
  const m = src.match(/<span class="badge-region">\$\{[^}]*o\.iata[^}]*\}<\/span>/);
  assert.ok(m, '#333 observers table badge-region sink not found');
  const fn = new Function('o', 'escapeHtml', 'return `' + m[0] + '`;');
  const html = fn({ iata: TAG_PAYLOAD + ATTR_PAYLOAD }, escapeHtml);
  assertNoXss(html, '#333 observers table region badge');
});

// 3b. Observer regions — slide-over region badge (concat form).
test('#333 observers slide-over region badge escapes o.iata', () => {
  const src = fs.readFileSync('public/observers.js', 'utf8');
  const m = src.match(/'<span class="badge-region">'\s*\+\s*([\s\S]*?)\s*\+\s*'<\/span>'/);
  assert.ok(m, '#333 observers slide-over badge-region concat not found');
  const fn = new Function('o', 'escapeHtml', 'return `<span>` + (' + m[1] + ') + `</span>`;');
  const html = fn({ iata: TAG_PAYLOAD + ATTR_PAYLOAD }, escapeHtml);
  assertNoXss(html, '#333 observers slide-over region badge');
});

// 4. Packet region badge (packets.js) — groupRegion in badge-region span.
test('#333 packets region badge escapes groupRegion', () => {
  const src = fs.readFileSync('public/packets.js', 'utf8');
  const m = src.match(/<span class="badge-region">\$\{[^}]*groupRegion[^}]*\}<\/span>/);
  assert.ok(m, '#333 packets badge-region sink not found');
  const fn = new Function('groupRegion', 'escapeHtml', 'return `' + m[0] + '`;');
  const html = fn(TAG_PAYLOAD + ATTR_PAYLOAD, escapeHtml);
  assertNoXss(html, '#333 packets region badge');
});

// 5. Clock-skew observer/node names — clock-health fleet table (regression
//    pin: already escaped in our fork; the mutant is removing esc()).
test('#333 clock-health table escapes node name (regression pin)', () => {
  const src = fs.readFileSync('public/analytics.js', 'utf8');
  // Escaped or not (`esc(n.nodeName …)` / `(n.nodeName …)`), so a mutant fails
  // on behaviour, not on "cell not found" (#333 F6).
  const m = src.match(/<td><strong>'\s*\+\s*([^\n]*?n\.nodeName[^\n]*?)\s*\+\s*'<\/strong><\/td>/);
  assert.ok(m, '#333 clock-health name cell not found');
  const fn = new Function('n', 'esc',
    'return `<td><strong>` + (' + m[1] + ') + `</strong></td>`;');
  const html = fn({ nodeName: TAG_PAYLOAD + ATTR_PAYLOAD, pubkey: 'abcdef012345' }, analyticsEsc);
  assertNoXss(html, '#333 clock-health node name');
});

// 6. RF-health attributes (analytics.js) — observer_id in data-observer and
//    id attributes (double-quoted); observer name in aria-label.
test('#333 RF-health cell escapes observer_id in attributes', () => {
  const src = fs.readFileSync('public/analytics.js', 'utf8');
  const m = src.match(/grid\.innerHTML = filteredObservers\.map\(obs => \{[\s\S]*?return `([\s\S]*?)`;[\s\S]*?\}\)\.join/);
  assert.ok(m, '#333 RF-health cell template not found');
  const fn = new Function(
    'obs', 'isSelected', 'esc', 'nf', 'avgNf', 'maxNf', 'batt', 'nfClass', 'name',
    'return `' + m[1] + '`;');
  // Double-quote attribute breakout attempt on observer_id.
  const dqPayload = '"><img src=x onerror=alert(1)>';
  const html = fn(
    { observer_id: dqPayload, sample_count: 5 }, false, analyticsEsc,
    '-100', '-105', '-95', '', '', TAG_PAYLOAD + ATTR_PAYLOAD);
  assert.ok(!/<img\b/i.test(html),
    '#333 RF-health: raw <img survived (observer_id attr breakout): ' + html);
  assert.ok(html.includes('&quot;') || html.includes('&lt;'),
    '#333 RF-health: observer_id not escaped in attribute: ' + html);
  // The aria-label name must also be escaped.
  assert.ok(!/'\s*onfocus\s*=/i.test(html),
    '#333 RF-health: aria-label attr-breakout survived: ' + html);
});

// 6b. #333 F5: the RF-health attribute fix relies on analytics.js's own
//     esc() encoding both quote characters. Pin that contract directly.
test("#333 analytics.js esc() encodes the 5-char OWASP set (attribute-safe)", () => {
  assert.strictEqual(analyticsEsc('&<>"\''), '&amp;&lt;&gt;&quot;&#39;');
  assert.strictEqual(analyticsEsc(null), '');
});

// 7a. Unknown-route heading (app.js) — URL-hash route in <h2>.
test('#333 unknown-route heading escapes route', () => {
  const src = fs.readFileSync('public/app.js', 'utf8');
  const m = src.match(/<h2>\$\{[^}]*route[^}]*\}<\/h2>/);
  assert.ok(m, '#333 unknown-route <h2> sink not found');
  const fn = new Function('route', 'escapeHtml', 'return `' + m[0] + '`;');
  const html = fn('#/' + TAG_PAYLOAD + ATTR_PAYLOAD, escapeHtml);
  assertNoXss(html, '#333 unknown-route heading');
});

// 7b. Unknown-route template must not hardcode a hex colour (CSS var).
test('#333 unknown-route template uses a CSS var, not a hardcoded hex colour', () => {
  const src = fs.readFileSync('public/app.js', 'utf8');
  const m = src.match(/`<div style="[^`]*?Page not yet implemented[^`]*?<\/div>`/);
  assert.ok(m, '#333 unknown-route div template not found');
  assert.ok(!/#[0-9a-fA-F]{3,8}\b/.test(m[0]),
    '#333 unknown-route div still has a hardcoded hex colour: ' + m[0]);
  assert.ok(/var\(--/.test(m[0]),
    '#333 unknown-route div does not use a CSS variable for colour: ' + m[0]);
});

// =========================================================================
// J. Issue #333 round 2 — adjacent sinks found in review: packet region
//    badges in group child rows and the flat view (F1), the shared region
//    filter (F2) and the observers-row navigation (F3).
//
// These drive the REAL render functions (packets.js buildGroupRowHtml /
// buildFlatRowHtml, region-filter.js render) in a vm sandbox with app.js's
// real escapeHtml as the page global, so a missing wrapper at any call site
// turns the test red — no template is lifted by regex.
// =========================================================================
const asyncTests = [];
function asyncTest(name, fn) { asyncTests.push({ name, fn }); }

// Hostile region code: breaks out of a double-quoted attribute AND carries a
// tag for the text context; the single quote probes single-quoted contexts.
const REGION_PAYLOAD = '"><img src=x onerror=alert(1)>' + ATTR_PAYLOAD;
const BENIGN_REGION = 'KØB & Æ';

function decodeEntities(s) {
  return s.replace(/&lt;/g, '<').replace(/&gt;/g, '>').replace(/&quot;/g, '"')
    .replace(/&#39;/g, "'").replace(/&amp;/g, '&');
}

// Real public/packets.js in a sandbox (same shape as
// test-issue-165-hop-resolution-per-observer.js). `observers` is what
// /api/observers returns; resolveHops() seeds the page's observerMap from it.
async function loadPacketsWithObservers(observers) {
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
    localStorage: { getItem: k => (k in store ? store[k] : null), setItem: (k, v) => { store[k] = String(v); }, removeItem: k => { delete store[k]; } },
    console, Math, Object, Array, Number, Date, Map, Set, JSON, String, Boolean, RegExp, Error, TypeError, Promise,
    parseInt, parseFloat, isNaN, isFinite, encodeURIComponent, decodeURIComponent, URLSearchParams,
    setTimeout, clearTimeout, setInterval: () => 0, clearInterval() {},
    requestAnimationFrame: () => 0, cancelAnimationFrame() {},
    performance: { now: () => Date.now() },
    location: { hash: '' },
    fetch: () => Promise.resolve({ ok: true, json: () => Promise.resolve({}) }),
    registerPage() {}, onWS() {}, offWS() {}, debouncedOnWS: fn => fn,
    CLIENT_TTL: {},
    fetchAllNodes: () => Promise.resolve({ nodes: [] }),
    api: (p) => Promise.resolve(p.startsWith('/observers') ? { observers } : p.startsWith('/iata-coords') ? { coords: {} } : {}),
    invalidateApiCache() {},
  };
  ctx.window.localStorage = ctx.localStorage;
  vm.createContext(ctx);
  const load = (f) => {
    vm.runInContext(fs.readFileSync('public/' + f, 'utf8'), ctx, { filename: f });
    for (const k of Object.keys(ctx.window)) ctx[k] = ctx.window[k];
  };
  // roles.js + app.js supply the page globals the row builders call
  // (payloadTypeName, …); app.js's own escapeHtml becomes the global.
  for (const f of ['payload-labels.js', 'roles.js', 'app.js']) load(f);
  ctx.api = (p) => Promise.resolve(p.startsWith('/observers') ? { observers } : p.startsWith('/iata-coords') ? { coords: {} } : {});
  ctx.fetchAllNodes = () => Promise.resolve({ nodes: [] });
  for (const f of ['packet-helpers.js', 'hop-resolver.js', 'hop-display.js', 'packets.js']) load(f);
  const T = ctx.window._packetsTestAPI;
  await T.resolveHops(['ef'], observers[0].id); // seeds observerMap via ensureHopResolver
  return T;
}

// Every .badge-region in a rendered row, decoded the way the browser would.
function regionBadges(html) {
  return Array.from(html.matchAll(/<span class="badge-region">([^<]*)<\/span>/g), m => decodeEntities(m[1]));
}

const RAW_HEX = '15000102030405060708090a0b0c0d0e0f';
function pkt(extra) {
  return Object.assign({ hash: 'fae0c9e6d357a814', raw_hex: RAW_HEX, payload_type: 5, route_type: 1,
    decoded_json: '{}', path_json: '[]', timestamp: '', latest: '' }, extra);
}

asyncTest('#333 F1 packets flat view: region badge escapes the observer IATA', async () => {
  const T = await loadPacketsWithObservers([{ id: 'OBS-H', name: 'h', iata: REGION_PAYLOAD }]);
  const html = T.buildFlatRowHtml(pkt({ id: 7, observer_id: 'OBS-H' }));
  assertNoXss(html, '#333 F1 flat view region');
  assert.deepStrictEqual(regionBadges(html), [REGION_PAYLOAD], 'flat view badge must show the code as text');
});

asyncTest('#333 F1 packets group child rows: region badge escapes the observer IATA', async () => {
  const T = await loadPacketsWithObservers([
    { id: 'OBS-B', name: 'b', iata: 'SJC' },
    { id: 'OBS-H', name: 'h', iata: REGION_PAYLOAD },
  ]);
  const p = pkt({ observer_id: 'OBS-B', count: 2, observation_count: 2, observer_count: 2,
    _children: [pkt({ id: 1, observer_id: 'OBS-B' }), pkt({ id: 2, observer_id: 'OBS-H' })] });
  T._setExpanded(p.hash, true);
  const html = T.buildGroupRowHtml(p);
  assert.ok(html.includes('class="group-child"'), 'expanded group must render child rows');
  assertNoXss(html, '#333 F1 group child region');
  assert.deepStrictEqual(regionBadges(html), ['SJC', 'SJC', REGION_PAYLOAD],
    'header + both child badges must show their codes as text');
});

asyncTest('#333 F1 packets group header: region badge escapes the observer IATA', async () => {
  const T = await loadPacketsWithObservers([{ id: 'OBS-H', name: 'h', iata: REGION_PAYLOAD }]);
  const html = T.buildGroupRowHtml(pkt({ observer_id: 'OBS-H', count: 1, observation_count: 1 }));
  assertNoXss(html, '#333 F1 group header region');
  assert.deepStrictEqual(regionBadges(html), [REGION_PAYLOAD]);
});

asyncTest('#333 F1 packets region badges show a benign code verbatim (no double escape)', async () => {
  const T = await loadPacketsWithObservers([{ id: 'OBS-K', name: 'k', iata: BENIGN_REGION }]);
  const flat = T.buildFlatRowHtml(pkt({ id: 3, observer_id: 'OBS-K' }));
  assert.ok(flat.includes('<span class="badge-region">KØB &amp; Æ</span>'), flat);
  assert.deepStrictEqual(regionBadges(flat), [BENIGN_REGION]);
});

// Real public/region-filter.js in a sandbox. `regions` is what
// /api/config/regions returns; `stored` pre-seeds the persisted selection.
async function loadRegionFilter(regions, opts) {
  const store = {};
  if (opts && opts.stored) store['meshcore-region-filter'] = JSON.stringify(opts.stored);
  const el = () => ({ hidden: true, onclick: null, setAttribute() {} });
  const container = { innerHTML: '', style: {}, onclick: null, querySelector: el };
  const ctx = {
    window: {},
    document: { addEventListener() {}, removeEventListener() {} },
    localStorage: { getItem: k => (k in store ? store[k] : null), setItem: (k, v) => { store[k] = String(v); }, removeItem: k => { delete store[k]; } },
    fetch: async () => ({ json: async () => regions }),
    console, setTimeout, clearTimeout,
    escapeHtml, // app.js's real global escaper
  };
  ctx.window = ctx;
  vm.createContext(ctx);
  vm.runInContext(fs.readFileSync('public/region-filter.js', 'utf8'), ctx, { filename: 'region-filter.js' });
  await ctx.RegionFilter.init(container, opts && opts.dropdown ? { dropdown: true } : undefined);
  return { RF: ctx.RegionFilter, container, store };
}

// The value the browser would hand to toggleRegion via el.dataset.region.
function dataRegions(html) {
  return Array.from(html.matchAll(/data-region="([^"]*)"/g), m => decodeEntities(m[1]));
}

asyncTest('#333 F2 region filter pills: hostile code escaped in data-region and label', async () => {
  const { container } = await loadRegionFilter({ SJC: 'San Jose', [REGION_PAYLOAD]: REGION_PAYLOAD });
  const html = container.innerHTML;
  assert.ok(html.includes('region-pill'), 'expected pill mode for 2 regions: ' + html);
  assertNoXss(html, '#333 F2 region pills');
  assert.deepStrictEqual(dataRegions(html), ['__all__', 'SJC', REGION_PAYLOAD],
    'each pill data-region must round-trip to its code');
  assert.ok(decodeEntities(html).includes('>' + REGION_PAYLOAD + '</button>'), 'pill label must show the code as text');
});

asyncTest('#333 F2 region filter dropdown: hostile code + label escaped, trigger label escaped', async () => {
  const { container } = await loadRegionFilter(
    { SJC: 'San Jose', [REGION_PAYLOAD]: REGION_PAYLOAD + ' city' },
    { dropdown: true, stored: [REGION_PAYLOAD] });
  const html = container.innerHTML;
  assert.ok(html.includes('region-dropdown-trigger'), 'expected dropdown mode: ' + html);
  assertNoXss(html, '#333 F2 region dropdown');
  assert.deepStrictEqual(dataRegions(html), ['__all__', 'SJC', REGION_PAYLOAD]);
  const trigger = html.match(/<button class="region-dropdown-trigger"[^>]*>([^<]*)<\/button>/);
  assert.ok(trigger, 'trigger button must contain only text: ' + html);
  assert.strictEqual(decodeEntities(trigger[1]), REGION_PAYLOAD + ' ▾', 'dropdownLabel must show the selected code as text');
});

asyncTest('#333 F2 region filter: selecting a hostile code still filters by that exact code', async () => {
  const { RF, container, store } = await loadRegionFilter({ SJC: 'San Jose', [REGION_PAYLOAD]: 'x' });
  const code = dataRegions(container.innerHTML)[2];
  container.onclick({ target: { closest: () => ({ dataset: { region: code } }) } });
  assert.deepStrictEqual(Array.from(RF.getSelected()), [REGION_PAYLOAD]); // re-realm the vm array
  assert.strictEqual(RF.regionQueryString(), '&region=' + encodeURIComponent(REGION_PAYLOAD));
  assert.deepStrictEqual(JSON.parse(store['meshcore-region-filter']), [REGION_PAYLOAD]);
  // Re-rendered after the toggle: still inert, hostile pill now the only active one.
  assertNoXss(container.innerHTML, '#333 F2 region pills after toggle');
});

asyncTest('#333 F2 region filter shows a benign code verbatim (no double escape)', async () => {
  const { container } = await loadRegionFilter({ SJC: 'San Jose', [BENIGN_REGION]: BENIGN_REGION });
  assert.ok(container.innerHTML.includes('data-region="KØB &amp; Æ"'), container.innerHTML);
  assert.ok(container.innerHTML.includes('>KØB &amp; Æ</button>'), container.innerHTML);
  assert.deepStrictEqual(dataRegions(container.innerHTML), ['__all__', 'SJC', BENIGN_REGION]);
});

// F3: observers row — an observer id containing ' must not reach JS code.
test('#333 F3 observers row: no inline handler interpolates the observer id', () => {
  const src = fs.readFileSync('public/observers.js', 'utf8');
  const m = src.match(/return `(<tr style="cursor:pointer"[^>]*data-action="navigate"[^>]*>)/);
  assert.ok(m, '#333 F3 observers navigate row not found');
  const fn = new Function('o', 'escapeHtml', 'return `' + m[1] + '`;');
  const id = "Q'-console.error(1)-'";
  const tag = fn({ id }, escapeHtml);
  const handlers = Array.from(tag.matchAll(/\son[a-z]+="([^"]*)"/gi), x => x[1]);
  for (const h of handlers) {
    assert.ok(!h.includes(id) && !h.includes(encodeURIComponent(id)),
      '#333 F3: observer id reaches an inline JS handler: ' + tag);
  }
  // Navigation target stays on the row for the delegated handler.
  const dv = tag.match(/data-value="([^"]*)"/);
  assert.ok(dv && decodeEntities(dv[1]) === '#/observers/' + encodeURIComponent(id),
    '#333 F3: data-value navigation target lost: ' + tag);
});

// =========================================================================
// SUMMARY
// =========================================================================
(async () => {
  for (const t of asyncTests) {
    try { await t.fn(); passed++; console.log(`  ✅ ${t.name}`); }
    catch (e) { failed++; console.log(`  ❌ ${t.name}: ${e.message}`); }
  }
  console.log('\n' + '═'.repeat(48));
  console.log(`  XSS escape sinks: ${passed} passed, ${failed} failed`);
  console.log('═'.repeat(48));
  if (failed > 0) process.exit(1);
})();
