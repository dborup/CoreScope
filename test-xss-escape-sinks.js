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
// #393 N10: count EVERY declaration named esc, not just `function esc(`.
// #333 N4 only counted the function form, so a differently-declared second
// escaper (`const esc = …`, `let`/`var`) slipped past the guard and could
// silently retarget the contract test. Count both shapes.
function countEscDefs(src) {
  const fnDefs = (src.match(/function\s+esc\s*\(/g) || []).length;
  const bindDefs = (src.match(/\b(?:const|let|var)\s+esc\s*=/g) || []).length;
  return fnDefs + bindDefs;
}

function loadLocalEsc(file, ctxExtra) {
  const src = fs.readFileSync(file, 'utf8');
  // #333 N4 / #393 N10: the regex below lifts the *first* one-line
  // `function esc(s)`. If a second file-local esc were added above the real
  // sink's escaper — in ANY declaration form — it would silently retarget
  // this contract test. Pin the assumption: there must be exactly one esc
  // definition in the file, else fail loudly.
  const defs = countEscDefs(src);
  if (defs !== 1)
    throw new Error(`expected exactly one esc definition in ${file}, found ${defs}; `
      + 'loadLocalEsc would silently pick the first — add a line anchor to disambiguate');
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

// #217: the observer cells call obsCellName(), where truncation and escaping
// now live. Build the real function from packets.js source.
function realObsCellName(showFullNames, obsNameOnly, truncate) {
  const src = fs.readFileSync('public/packets.js', 'utf8');
  const m = src.match(/function obsCellName\(id, maxLen\) \{[\s\S]*?\n  \}/);
  assert.ok(m, 'packets.js obsCellName not found');
  return new Function('obsNameOnly', 'escapeHtml', 'truncate', 'showFullNames', m[0] + '\nreturn obsCellName;')(
    obsNameOnly, escapeHtml, truncate, showFullNames);
}

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
  for (const full of [false, true]) {
    const trunc = (s, n) => String(s).slice(0, n);
    const nameOf = () => TAG_PAYLOAD + ATTR_PAYLOAD;
    const fn = new Function(
      'isSingle','headerObserverId','obsCellName','escapeHtml','obsNameOnly',
      'obsIataBadge','groupedObserverIataBadgesHtml','p',
      'return ' + tpl + ';'
    );
    const html = fn(
      true, 'obs-1', realObsCellName(full, nameOf, trunc), escapeHtml, nameOf,
      () => '', () => '', {}
    );
    assertNoXss(html, 'packets.js grouped observer cell (Full Names ' + (full ? 'on' : 'off') + ')');
  }
});

// --- 6. public/packets.js child / flat observer cells --------------------
test('packets.js: flat observer cell escapes observer name', () => {
  // Capture the flat observer cell — the OUTER form, with or without
  // escapeHtml: `${truncate(obsNameOnly(p.observer_id), 16)}${obsIataBadge(p)}`
  // or            `${escapeHtml(truncate(obsNameOnly(p.observer_id), 16))}${obsIataBadge(p)}`.
  const src = fs.readFileSync('public/packets.js', 'utf8');
  // Find the FLAT row (the third occurrence with p.observer_id, not c.observer_id).
  const re = /\$\{obsCellName\(p\.observer_id,\s*16\)\}\$\{obsIataBadge\(p\)\}/;
  const m = src.match(re);
  assert.ok(m, 'packets.js flat observer cell template not found');
  const tpl = '`<td>' + m[0] + '</td>`';
  // Use short payloads — truncate(_, 16) would lop off the trailing single
  // quote of a longer string. Both must survive in the first 16 chars.
  for (const full of [false, true]) {
    const fn = new Function('p','obsCellName','obsIataBadge', 'return ' + tpl + ';');
    const html = fn(
      { observer_id: 'obs-1' },
      realObsCellName(full, () => "'<img onerror=", (s, n) => String(s).slice(0, n)),
      () => ''
    );
    const where = 'packets.js flat cell (Full Names ' + (full ? 'on' : 'off') + ')';
    // Custom assertions (assertNoXss expects full payloads).
    assert.ok(!/<img/.test(html), where + ': raw <img survived: ' + html);
    assert.ok(!/^[^&]*'/.test(html.replace('<td>', '').replace('</td>','')), where + ": raw ' survived: " + html);
    assert.ok(html.includes('&lt;img'), where + ': &lt;img marker missing: ' + html);
    assert.ok(html.includes('&#39;'), where + ': &#39; marker missing: ' + html);
  }
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
  // The airtime stat cards come from the real shared helper (#497).
  const airCtx = { window: {} };
  vm.createContext(airCtx);
  vm.runInContext(fs.readFileSync('public/observer-airtime.js', 'utf8'), airCtx);
  const sandbox = {
    window: { ObserverDetailNaiveBanner: { render: () => '' }, ObserverAirtime: airCtx.window.ObserverAirtime },
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
    'statusCls', 'statusLabel', 'neighborsData', 'airtime',
    radioBlock + '\n' + directNeighborsBlock + '\nreturn `' + tpl + '` + renderDirectNeighbors(neighborsData);' // #634: neighbors render into their own region now
  );
  return fn(
    obs, sandbox.obsSkew, escapeHtml, sandbox.window, sandbox.HEALTH_THRESHOLDS,
    sandbox.formatDuration, sandbox.timeAgo, sandbox.formatSkew,
    sandbox.renderSkewBadge, sandbox.observerSkewSeverity,
    statusCls, statusLabel, sandbox.neighborsData, sandbox.window.ObserverAirtime.lookup()
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
    'ObserverAirtime', 'summaryData',
    'return `' + m[1] + '`;');
  // The tile's TX 1h stat comes from the real shared helper (#479).
  const airCtx = { window: {} };
  vm.createContext(airCtx);
  vm.runInContext(fs.readFileSync('public/observer-airtime.js', 'utf8'), airCtx);
  // Double-quote attribute breakout attempt on observer_id.
  const dqPayload = '"><img src=x onerror=alert(1)>';
  const html = fn(
    { observer_id: dqPayload, sample_count: 5 }, false, analyticsEsc,
    '-100', '-105', '-95', '', '', TAG_PAYLOAD + ATTR_PAYLOAD,
    airCtx.window.ObserverAirtime, { duty_cycle_warn_pct: 10 });
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
// /api/config/regions returns; `stored` pre-seeds the persisted selection;
// `groups` (#416) is what /api/config/regions/groups returns.
async function loadRegionFilter(regions, opts) {
  const store = {};
  if (opts && opts.stored) store['meshcore-region-filter'] = JSON.stringify(opts.stored);
  const el = () => ({ hidden: true, onclick: null, setAttribute() {} });
  const container = {
    innerHTML: '', style: {}, onclick: null, querySelector: el,
    classList: { add() {}, remove() {}, contains() { return false; } },
  };
  const ctx = {
    window: {},
    addEventListener() {}, removeEventListener() {}, // #620 watchMenu: ctx.window = ctx
    document: { addEventListener() {}, removeEventListener() {} },
    localStorage: { getItem: k => (k in store ? store[k] : null), setItem: (k, v) => { store[k] = String(v); }, removeItem: k => { delete store[k]; } },
    fetch: async (url) => ({
      json: async () => (String(url).indexOf('/groups') >= 0 ? ((opts && opts.groups) || []) : regions),
    }),
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

asyncTest('#416 region quick picks: hostile group name and code escaped in both slots', async () => {
  const { container } = await loadRegionFilter(
    { SJC: 'San Jose', [REGION_PAYLOAD]: 'x' },
    { groups: [{ name: REGION_PAYLOAD, codes: ['SJC', REGION_PAYLOAD] }] });
  const html = container.innerHTML;
  assert.ok(html.includes('region-quick-pick'), 'expected a quick pick: ' + html);
  assertNoXss(html, '#416 region quick picks');
  // The label is a text slot, the codes a quoted attribute: both decode back
  // to the exact operator/observer values, so escaping is the only defence.
  assert.ok(decodeEntities(html).includes('>' + REGION_PAYLOAD + '</button>'),
    'the group label must show the name as text');
  const title = html.match(/<button type="button" class="region-quick-pick[^"]*" data-region-group="0" aria-pressed="[a-z]+" title="([^"]*)">/);
  assert.ok(title, 'the quick pick must carry its codes in a quoted title: ' + html);
  assert.strictEqual(decodeEntities(title[1]), 'SJC, ' + REGION_PAYLOAD,
    'the title must list the codes as text');
});

asyncTest('#416 region quick picks: a benign group name is shown verbatim (no double escape)', async () => {
  const { container } = await loadRegionFilter(
    { SJC: 'San Jose', SFO: 'San Francisco' },
    { groups: [{ name: BENIGN_REGION, codes: ['SJC'] }] });
  assert.ok(container.innerHTML.includes('>KØB &amp; Æ</button>'), container.innerHTML);
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
// K. Issue #333 round 3 — the two remaining adjacent region sinks the round-2
//    review flagged (N1, N2) plus the analytics repeater-row inline handler
//    (N3). N1 and N2 both render the SAME value — observer IATA from
//    /api/nodes/{pk}/health, via observers.map(o => o.iata) — raw into a
//    summary line one or two lines above an already-escaped sibling badge.
//    Each test lifts the REAL production expression by regex and renders it
//    with a hostile IATA, so dropping the escaper (the mutant) turns it red.
// =========================================================================

// N1/N2 (#76 follow-up): the "Regions:" summaries read o.iata, which the
// health endpoints never emitted, and were removed with the Heard By split.
// The node detail panel, the full page and the Live panel now render Heard By
// through public/heard-by.js; lift the shipped renderers and feed them a
// hostile observer name and id, so dropping an escaper turns this red.
test('#333 N1/N2 Heard By renderers escape observer names and ids (#76)', () => {
  for (const [file, call] of [['public/nodes.js', 'HeardBy.renderList(h)'], ['public/nodes.js', 'HeardBy.renderTables(h)'], ['public/live.js', 'HeardBy.renderList(h, { link: true })']]) {
    assert.ok(fs.readFileSync(file, 'utf8').includes(call), `#333 N1/N2: ${file} must render Heard By via ${call}`);
    assert.ok(!/observers\.map\(o => o\.iata\)/.test(fs.readFileSync(file, 'utf8')), `#333 N1/N2: ${file} still reads the never-emitted o.iata`);
  }
  const ctx = { window: {} };
  vm.createContext(ctx);
  vm.runInContext(fs.readFileSync('public/heard-by.js', 'utf8'), ctx, { filename: 'heard-by.js' });
  const HB = ctx.window.HeardBy;
  const h = {
    directObservers: [{ observer_id: REGION_PAYLOAD, observer_name: REGION_PAYLOAD, packetCount: 2, avgSnr: 1, avgRssi: -90 }],
    relayObservers: [{ observer_id: 'x' + REGION_PAYLOAD, observer_name: '', packetCount: 1 }],
  };
  for (const [label, html] of [['list', HB.renderList(h)], ['linked list', HB.renderList(h, { link: true })], ['tables', HB.renderTables(h)]]) {
    assertNoXss(html, '#333 N1/N2 Heard By ' + label);
    assert.ok(decodeEntities(html).includes(REGION_PAYLOAD), `#333 N1/N2: ${label} must show the name as text: ${html}`);
  }
});

// N3: public/analytics.js renderRepeaterTable. The row carried
// onclick="location.hash='#/nodes/${encodeURIComponent(r.pubkey)}'".
// encodeURIComponent leaves ' ( ) alone, so a pubkey quote breaks out of the
// JS string — the exact pattern F3 removed from observers.js. The fix drops
// the inline handler and navigates via data-action="navigate"/data-value.
test('#333 N3 analytics repeater row: no inline handler carries the node pubkey', () => {
  const src = fs.readFileSync('public/analytics.js', 'utf8');
  const m = src.match(/html \+= `<div class="repeater-row [\s\S]*?`;/);
  assert.ok(m, '#333 N3 analytics repeater-row template not found');
  const render = new Function('r', 'esc', 'pct', 'encodeURIComponent',
    'let html = ""; ' + m[0] + ' return html;');
  const pubkey = "deadbeef'-alert(333)-'";
  const html = render({ pubkey, name: 'Repeater', count: 5, hop: '1' }, s => String(s), 50, encodeURIComponent);
  const handlers = Array.from(html.matchAll(/\son[a-z]+\s*=\s*"([^"]*)"/gi), x => x[1]);
  for (const h of handlers) {
    assert.ok(!h.includes(pubkey) && !h.includes(encodeURIComponent(pubkey)),
      '#333 N3: node pubkey reaches an inline JS handler: ' + html);
  }
  const dv = html.match(/data-value="([^"]*)"/);
  assert.ok(dv && decodeEntities(dv[1]) === '#/nodes/' + encodeURIComponent(pubkey),
    '#333 N3: data-value navigation target lost: ' + html);
});

// =========================================================================
// #392 — public/perf.js interpolates /api/perf values into innerHTML.
// The endpoint key (unmatched /api/ routes collapse to the raw path) and
// the slow-query path (raw r.URL.Path) are request-controlled, so a 404
// under /api/ that is slow can carry markup. These drive the REAL perf.js
// row templates (extracted from source) with a hostile value; dropping the
// escapeHtml() wrapper makes the raw <img survive and flips them red.
// =========================================================================
console.log('\n=== #392 perf.js dashboard sinks ===');

const PERF_PAYLOAD = TAG_PAYLOAD + ATTR_PAYLOAD;

test('#392 perf endpoints table: endpoint key is escaped', () => {
  // First `<tr${cls}>` statement in perf.js is the endpoints row (before the
  // client-calls row). Captured regardless of the escapeHtml() wrapper so the
  // assertion — not the match — flips on revert.
  const html = evalTemplate(
    'public/perf.js',
    /html \+= `(<tr\$\{cls\}><td><code>\$\{[^`]*?<\/tr>)`;/,
    { cls: '', path: PERF_PAYLOAD, s: { count: 1, avgMs: 2, p50Ms: 3, p95Ms: 4, maxMs: 5 }, total: 7 }
  );
  assertNoXss(html, '#392 perf endpoint key');
});

test('#392 perf client-calls table: client path is escaped', () => {
  const html = evalTemplate(
    'public/perf.js',
    /html \+= `(<tr\$\{cls\}><td><code>\$\{[^}]*s\.path[^`]*?<\/tr>)`;/,
    { cls: '', s: { path: PERF_PAYLOAD, count: 1, avgMs: 2, maxMs: 3, totalMs: 4 } }
  );
  assertNoXss(html, '#392 perf client path');
});

test('#392 perf slow-queries table: raw slow-query path is escaped', () => {
  const html = evalTemplate(
    'public/perf.js',
    /html \+= `(<tr class="perf-slow">[^`]*?<\/tr>)`;/,
    { q: { time: '2020-01-01T00:00:00Z', path: PERF_PAYLOAD, ms: 123, status: 200 } }
  );
  assertNoXss(html, '#392 perf slow-query path');
});

test('#392 perf write-sources table: source name is escaped', () => {
  const html = evalTemplate(
    'public/perf.js',
    /html \+= `(<tr><td><code>\$\{[^`]*?<\/tr>)`;/,
    { k: PERF_PAYLOAD, v: 1, rateStr: '', baseStr: '', flag: '' }
  );
  assertNoXss(html, '#392 perf write-source name');
});

test('#392 perf overview: server uptime string is escaped', () => {
  const html = evalTemplate(
    'public/perf.js',
    /(<div class="perf-card"><div class="perf-num">\$\{[^`]*?uptimeHuman[^`]*?Uptime<\/div><\/div>)/,
    { health: { uptimeHuman: PERF_PAYLOAD }, server: { uptime: 60, totalRequests: 0, avgMs: 0 } }
  );
  assertNoXss(html, '#392 perf uptimeHuman');
});

test('#392 perf write-sources footer: sampleAt is escaped', () => {
  const html = evalTemplate(
    'public/perf.js',
    /html \+= `(<div style="font-size:11px;[^`]*?Sampled:[^`]*?)`;/,
    { writeSources: { sampleAt: PERF_PAYLOAD } }
  );
  assertNoXss(html, '#392 perf sampleAt');
});

// =========================================================================
// #394 F2 — perf.js ships its OWN IIFE-local escapeHtml fallback (used when
// evaluated without app.js's global, e.g. the perf-render unit sandboxes).
// The other #392 tests inject app.js's escapeHtml via evalTemplate(), so a
// weakened perf.js-local fallback went unnoticed. Drive the real selector +
// fallback straight from source so an identity mutant on it flips red.
// =========================================================================
console.log('\n=== #394 F2 perf.js IIFE-local escapeHtml fallback ===');

test('#394 F2 perf.js local escapeHtml fallback escapes (identity mutant → red)', () => {
  const src = fs.readFileSync('public/perf.js', 'utf8');
  // Capture the RHS of `const escapeHtml = <ternary>;` — minimal up to the
  // first `};` (the close of the inline fallback function). Captured whether
  // or not the fallback body escapes, so the assertion flips, not the match.
  const m = src.match(/const escapeHtml = ([\s\S]*?\});\s*\n/);
  assert.ok(m, '#394 F2: `const escapeHtml = <ternary>` not found in perf.js');
  const select = new Function('globalThis', 'return (' + m[1] + ');');
  // No global escapeHtml on the passed globalThis → the IIFE-local fallback
  // branch is taken (exactly the standalone / sandbox case).
  const fallback = select({});
  assert.strictEqual(typeof fallback, 'function', '#394 F2: fallback is not a function');
  assert.strictEqual(fallback('<'), '&lt;', '#394 F2: fallback did not escape <');
  assert.strictEqual(fallback("'"), '&#39;', '#394 F2: fallback did not escape single quote');
  assert.strictEqual(fallback('&'), '&amp;');
  assert.strictEqual(fallback('>'), '&gt;');
  assert.strictEqual(fallback('"'), '&quot;');
  assert.strictEqual(fallback(null), '', '#394 F2: fallback null handling changed');
  // A hostile payload is fully neutralised by the fallback alone.
  const html = '<code>' + fallback(TAG_PAYLOAD + ATTR_PAYLOAD) + '</code>';
  assertNoXss(html, '#394 F2 perf.js local fallback');
  // And when a global escapeHtml IS present, the selector must prefer it.
  const sentinel = (s) => 'SENTINEL:' + String(s);
  assert.strictEqual(select({ escapeHtml: sentinel }), sentinel,
    '#394 F2: selector did not prefer globalThis.escapeHtml when present');
});

// =========================================================================
// #394 F1 — defence-in-depth: the numeric perf cards interpolate values
// straight from /api/perf etc. They are number-typed server-side today, so
// not request-controlled, but the issue criterion is "escape EVERY
// interpolated perf value". These drive representative card sinks (one per
// response object) with a hostile value; dropping the escapeHtml() wrapper
// lets the raw <img survive and flips them red.
// =========================================================================
console.log('\n=== #394 F1 perf.js numeric-card sinks (defence in depth) ===');

function perfCardTest(name, regex, bindings, label) {
  test(name, () => {
    const html = evalTemplate('public/perf.js', regex, bindings);
    assertNoXss(html, label);
  });
}

perfCardTest('#394 F1 overview: server.totalRequests card is escaped',
  /(<div class="perf-card"><div class="perf-num">\$\{[^}]*totalRequests[^}]*\}<\/div><div class="perf-label">Total Requests<\/div><\/div>)/,
  { server: { totalRequests: PERF_PAYLOAD } }, '#394 F1 totalRequests');

perfCardTest('#394 F1 cache: c.size card is escaped',
  /(<div class="perf-card"><div class="perf-num">\$\{[^}]*c\.size[^}]*\}<\/div><div class="perf-label">Server Entries<\/div><\/div>)/,
  { c: { size: PERF_PAYLOAD } }, '#394 F1 cache size');

perfCardTest('#394 F1 go runtime: gr.goroutines card is escaped',
  /(<div class="perf-card"><div class="perf-num">\$\{[^}]*gr\.goroutines[^}]*\}<\/div><div class="perf-label">Goroutines<\/div><\/div>)/,
  { gr: { goroutines: PERF_PAYLOAD } }, '#394 F1 goroutines');

perfCardTest('#394 F1 sqlite: sq.walPages.busy card is escaped',
  /(<div class="perf-card"><div class="perf-num">\$\{[^}]*sq\.walPages\.busy[^}]*\}<\/div><div class="perf-label">WAL Busy Pages<\/div><\/div>)/,
  { sq: { walPages: { busy: PERF_PAYLOAD } } }, '#394 F1 walPages.busy');

perfCardTest('#394 F1 packet store: ps.indexes.byObserver card is escaped',
  /(<div class="perf-card"><div class="perf-num">\$\{[^}]*ps\.indexes\.byObserver[^}]*\}<\/div><div class="perf-label">Observers<\/div><\/div>)/,
  { ps: { indexes: { byObserver: PERF_PAYLOAD } } }, '#394 F1 byObserver');

// =========================================================================
// #393 N5 — the #394 F1 cards above only cover the Go-runtime health branch
// (gr.*). The Node-runtime branch (health.memory `m.*` / health.eventLoop
// `el.*`) escapes too but no unit test entered it, so dropping escapeHtml on
// e.g. `m.rss` stayed green. Drive one card per Node-branch response object.
// =========================================================================
console.log('\n=== #393 N5 perf.js Node-branch health cards ===');

perfCardTest('#393 N5 health(node): m.rss RSS card is escaped',
  /(<div class="perf-card"><div class="perf-num">\$\{[^}]*m\.rss[^}]*\}MB<\/div><div class="perf-label">RSS<\/div><\/div>)/,
  { m: { rss: PERF_PAYLOAD } }, '#393 N5 m.rss');

perfCardTest('#393 N5 health(node): el.maxLagMs card is escaped',
  /(<div class="perf-card"><div class="perf-num">\$\{[^}]*el\.maxLagMs[^}]*\}ms<\/div><div class="perf-label">EL Max Lag<\/div><\/div>)/,
  { el: { maxLagMs: PERF_PAYLOAD } }, '#393 N5 el.maxLagMs');

perfCardTest('#393 N5 health(node): el.currentLagMs card is escaped',
  /(<div class="perf-card"><div class="perf-num">\$\{[^}]*el\.currentLagMs[^}]*\}ms<\/div><div class="perf-label">EL Current<\/div><\/div>)/,
  { el: { currentLagMs: PERF_PAYLOAD } }, '#393 N5 el.currentLagMs');

// =========================================================================
// #393 F3 (round-2 review) — the explicit card tests above cover 8 of the
// escapeHtml-wrapped perf-num cards. Drive EVERY card instead: list the
// escaped cards per group, so dropping escapeHtml from any of them fails by
// label, and evaluate each card's real perf-num template with a hostile
// value behind every field it reads.
// =========================================================================
console.log('\n=== #393 F3 perf.js escaped perf-num cards (all) ===');

const PERF_ESCAPED_CARDS = [
  // overview
  'Total Requests', 'Avg Response', 'Uptime',
  // Go runtime
  'Goroutines', 'GC Collections', 'CPUs', 'WS Clients',
  // Node runtime
  'Heap Used / ${escapeHtml(m.heapTotal)}MB', 'RSS', 'Event Loop p95', 'EL Max Lag',
  'EL Current', 'WS Clients',
  // SQLite stats
  'Page Count', 'Page Size',
  // cache (server + client)
  'Server Entries', 'Server Hits', 'Server Misses', 'Server Hit Rate',
  'Stale Hits (SWR)', 'Recomputes', 'Client Hits', 'Client Misses', 'Client Hit Rate',
  // packet store
  'Packets in RAM', 'Tracked Memory', 'Memory Limit', 'Heap (debug)', 'Queries Served',
  'Live Inserts', 'Evicted', 'Unique Hashes', 'Observers', 'Indexed Nodes',
  // SQLite
  'DB Size', 'WAL Size', 'Freelist', 'Transmissions', 'Observations', 'Nodes',
  'Observers', 'WAL Busy Pages',
];

function perfCards() {
  const src = fs.readFileSync('public/perf.js', 'utf8');
  const re = /<div class="perf-card"><div class="perf-num"(?: style="[^"]*")?>(.*?)<\/div><div class="perf-label">(.*?)<\/div><\/div>/g;
  return [...src.matchAll(re)].map(m => ({ num: m[1], label: m[2] }));
}

// Stands in for every API object: any property is another hostile value, and
// it converts to PERF_PAYLOAD as a string, number or via toLocaleString().
function hostileValue() {
  return new Proxy(function () {}, {
    get(_, k) {
      if (k === Symbol.toPrimitive) return () => PERF_PAYLOAD;
      if (k === 'toString' || k === 'valueOf' || k === 'toLocaleString') return () => PERF_PAYLOAD;
      return hostileValue();
    },
  });
}

test('#393 F3 perf.js: every listed card wraps its perf-num value in escapeHtml', () => {
  const escaped = perfCards().filter(c => /\$\{escapeHtml\(/.test(c.num)).map(c => c.label);
  assert.deepStrictEqual(escaped.slice().sort(), PERF_ESCAPED_CARDS.slice().sort(),
    '#393 F3: the set of escapeHtml-wrapped perf-num cards changed');
});

for (const card of perfCards().filter(c => /\$\{escapeHtml\(/.test(c.num))) {
  test(`#393 F3 perf card "${card.label}": hostile value is escaped`, () => {
    const roots = [...new Set([...card.num.matchAll(/\$\{([\s\S]*?)\}/g)]
      .flatMap(x => x[1].match(/(?<![.\w$])[A-Za-z_$][\w$]*/g) || []))]
      .filter(id => id !== 'escapeHtml' && id !== 'Math');
    const fn = new Function(...roots, 'escapeHtml', 'return `' + card.num + '`;');
    assertNoXss(fn(...roots.map(hostileValue), escapeHtml), '#393 F3 ' + card.label);
  });
}

// =========================================================================
// #393 N6 (from the #394 re-review) — the error path at perf.js ~374 puts
// escapeHtml(err.message) into innerHTML. err.message is attacker-influenced:
// a non-JSON /api/perf body embeds in the JSON.parse message. It was the one
// newly escaped sink with no test. Capture the template regardless of the
// wrapper so dropping escapeHtml flips the assertion, not the match.
// =========================================================================
console.log('\n=== #393 N6 perf.js load-error sink ===');

test('#393 N6 perf.js error paragraph: err.message is escaped', () => {
  const html = evalTemplate(
    'public/perf.js',
    /el\.innerHTML = `(<p style="color:var\(--status-red\)">Error: \$\{[^}]*err\.message[^}]*\}<\/p>)`;/,
    { err: { message: PERF_PAYLOAD } }
  );
  assertNoXss(html, '#393 N6 perf.js err.message');
});

// =========================================================================
// #393 follow-ups to #373 — analytics a11y (N6), delegated-nav pin (N7),
// compare.js onclick removal (N9), loadLocalEsc guard robustness (N10).
// =========================================================================
console.log('\n=== #393 follow-ups to #373 ===');

// Minimal element whose closest(sel) honours an optional tag-name prefix, so
// a DIV row matches `[data-action="navigate"]` but NOT `tr[...]` and a TR row
// matches both — exactly the distinction the delegated selectors turn on.
function fakeNavRow(tag, value) {
  const el = { tagName: tag.toUpperCase(), dataset: { action: 'navigate', value } };
  el.closest = (sel) => {
    const mm = sel.match(/^([a-z]*)\[data-action="navigate"\]$/i);
    if (!mm) return null;
    if (mm[1] && mm[1].toUpperCase() !== el.tagName) return null;
    return el.dataset.action === 'navigate' ? el : null;
  };
  return el;
}

// N6: the analytics repeater row is a <div> whose parent .repeater-list is a
// plain <div>, so role="row" has no required rowgroup/table/grid ancestor
// (11 critical axe violations). #393 F4 (round-2 review): rejecting only the
// literal role="row" let role="cell" (equally invalid there) pass. Allow-list
// the focusable row's role instead, and reject every WAI-ARIA 1.2 role with a
// required context role anywhere in the row — none has its parent here.
const ARIA_ROLES_NEEDING_CONTEXT = ['cell', 'columnheader', 'gridcell', 'listitem',
  'menuitem', 'menuitemcheckbox', 'menuitemradio', 'option', 'row', 'rowgroup',
  'rowheader', 'tab', 'treeitem'];
test('#393 N6 analytics repeater row: a standalone interactive role, no role needing a context', () => {
  const src = fs.readFileSync('public/analytics.js', 'utf8');
  const m = src.match(/html \+= `<div class="repeater-row [\s\S]*?`;/);
  assert.ok(m, '#393 N6 analytics repeater-row template not found');
  const render = new Function('r', 'esc', 'pct', 'encodeURIComponent',
    'let html = ""; ' + m[0] + ' return html;');
  const html = render({ pubkey: 'deadbeef12', name: 'Repeater', count: 5, hop: '1' },
    (s) => String(s), 50, encodeURIComponent);
  const rowTag = html.match(/^<div class="repeater-row[^>]*>/);
  assert.ok(rowTag, '#393 N6: rendered repeater row tag not found: ' + html);
  const rowRole = (rowTag[0].match(/\srole="([^"]*)"/) || [])[1];
  assert.ok(rowRole === 'link' || rowRole === 'button',
    '#393 N6: clickable repeater <div> needs role="link" or "button", got ' + rowRole + ': ' + rowTag[0]);
  const bad = [...html.matchAll(/\srole="([^"]*)"/g)].map(x => x[1])
    .filter(r => ARIA_ROLES_NEEDING_CONTEXT.includes(r));
  assert.deepStrictEqual(bad, [],
    '#393 N6: repeater row carries a role with no required ancestor here: ' + html);
  assert.ok(/\btabindex="0"/.test(rowTag[0]),
    '#393 N6: repeater row lost tabindex="0" keyboard access: ' + html);
});

// N7: the delegated handler selector was broadened to [data-action="navigate"]
// so the DIV repeater row is clickable; nothing pinned it. Drive the real
// handler with a DIV row — narrowing the selector back to tr[...] goes red.
test('#393 N7 analytics delegated handler: a DIV row navigates on click', () => {
  const src = fs.readFileSync('public/analytics.js', 'utf8');
  const m = src.match(/const handler = \(e\) => \{[\s\S]*?location\.hash = row\.dataset\.value;\s*\};/);
  assert.ok(m, '#393 N7 analytics delegated nav handler not found');
  const makeHandler = new Function('location', m[0] + '\n return handler;');
  const loc = { hash: '' };
  const handler = makeHandler(loc);
  handler({ type: 'click', target: fakeNavRow('div', '#/nodes/deadbeef12') });
  assert.strictEqual(loc.hash, '#/nodes/deadbeef12',
    '#393 N7: delegated [data-action="navigate"] did not navigate a DIV row on click');
});

// N9a: the compare packet row dropped its inline onclick (the latent
// '-breakout in that double-quoted/JS context). Pin that the row carries no
// inline on* handler — re-adding onclick goes red. #393 F5 (round-2 review):
// scan the whole row expression, <tr> through '</tr>', so an on* attribute
// moved onto a child <td>/<span> goes red too.
test('#393 N9 compare.js packet row: no inline on* handler on the row or its children', () => {
  const src = fs.readFileSync('public/compare.js', 'utf8');
  const m = src.match(/return '<tr style="cursor:pointer"[^\n]*data-action="navigate"[\s\S]*?'<\/tr>';/);
  assert.ok(m, '#393 N9 compare packet-row expression not found');
  assert.ok(/<td/.test(m[0]), '#393 N9: captured row has no <td> children: ' + m[0]);
  const on = m[0].match(/\son[a-z]+\s*=/i);
  assert.ok(!on, '#393 N9: compare packet row still has an inline handler (' + (on && on[0]) + '): ' + m[0]);
});

// Run the REAL compare.js init() in a vm with a fake #app whose innerHTML
// assignment recreates its children, as the browser does. Each call returns
// the shared app element and that visit's #compareContent so a test can see
// which element a listener landed on.
function loadComparePage() {
  const src = fs.readFileSync('public/compare.js', 'utf8');
  function fakeEl() {
    const el = { listeners: {} };
    el.addEventListener = (type, fn) => { (el.listeners[type] = el.listeners[type] || []).push(fn); };
    return el;
  }
  let children = {};
  const app = fakeEl();
  Object.defineProperty(app, 'innerHTML', {
    set() { children = { compareControls: fakeEl(), compareContent: fakeEl(), comparePage: fakeEl() }; },
  });
  // #428: init() binds the row keydown handler to .compare-page (its own
  // container), not #app — see the test for it below.
  app.querySelector = (sel) => (sel === '.compare-page' ? children.comparePage : null);
  const loc = { hash: '#/compare' };
  const ctx = {
    window: {}, location: loc, CLIENT_TTL: {}, escapeHtml, URLSearchParams,
    document: { getElementById: (id) => children[id] || null },
    api: () => new Promise(() => {}), // observers never load: init's wiring only
    registerPage: (name, page) => { ctx.page = page; },
  };
  vm.runInNewContext(src, ctx);
  return {
    app, loc,
    visit() { ctx.page.init(app, ''); return children.compareContent; },
    leave() { ctx.page.destroy(); },
  };
}

// N9b: and the replacement delegated click handler actually navigates. Drive
// the click listener the real init() installs on a <tr> nav row — removing
// or narrowing it goes red.
test('#393 N9 compare.js delegated click handler navigates a nav row', () => {
  const cmp = loadComparePage();
  const content = cmp.visit();
  const clicks = content.listeners.click || [];
  assert.strictEqual(clicks.length, 1, '#393 N9: expected one delegated click handler on #compareContent');
  clicks[0]({ type: 'click', target: fakeNavRow('tr', '#/packets/deadbeef12') });
  assert.strictEqual(cmp.loc.hash, '#/packets/deadbeef12',
    '#393 N9: compare delegated click did not navigate the nav row');
});

// #393 F1 (round-2 review): the N9 click handler was bound to the shared #app
// element, which every page renders into and nothing ever unbinds. After one
// compare visit it fired on every later page's tr[data-action="navigate"],
// overriding the observers slide-over's preventDefault() (#1056 AC#4), and
// each visit stacked one more. It must live on compare's own container,
// which the next page's app.innerHTML discards.
test('#393 F1 compare.js init: no click listener on the shared #app, none stacked per visit', () => {
  const cmp = loadComparePage();
  const first = cmp.visit();
  cmp.leave();
  const second = cmp.visit();
  cmp.leave();
  assert.strictEqual((cmp.app.listeners.click || []).length, 0,
    '#393 F1: compare.js bound a click listener to the shared #app; it outlives the page');
  assert.notStrictEqual(first, second, 'harness: each visit must get a fresh #compareContent');
  assert.strictEqual((second.listeners.click || []).length, 1,
    '#393 F1: the current visit must carry exactly one row click handler');
});

// N10: loadLocalEsc's guard must also catch a differently-declared second
// esc (const/let/var), not only a second `function esc(`. Exercise the lifted
// counter directly — weakening it back to function-only makes case 2 go red.
test('#393 N10 countEscDefs catches a second, differently-declared esc', () => {
  assert.strictEqual(countEscDefs('function esc(s){return s;}'), 1,
    '#393 N10: a single function esc must count as 1');
  assert.strictEqual(countEscDefs('function esc(s){return s;}\nconst esc = (s) => s;'), 2,
    '#393 N10: a second `const esc =` must be counted (the N4 blind spot)');
  assert.strictEqual(countEscDefs('let esc = (s) => s;\nfunction esc(s){}'), 2,
    '#393 N10: let-declared esc must also be counted');
  assert.strictEqual(countEscDefs('function other(s){}'), 0,
    '#393 N10: no esc definition must count as 0');
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
