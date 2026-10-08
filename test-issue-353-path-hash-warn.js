/**
 * #353 — a 1-byte path hash reads as a warning.
 *
 * Covers the ONE shared decision + markup helper in public/app.js
 * (isPathHashSizeWarn / renderPathHashSize) and its theming contract
 * (--path-hash-warn in style.css). The three call sites are covered next to
 * their existing tests:
 *   - channels "Sent with" badge  -> test-channels-observed-path-hash-size.js
 *   - packet detail Hash Size row -> test-packets.js
 *   - node detail badge           -> test-frontend-helpers.js
 *   - customizer mapping          -> test-customizer-v2.js
 *
 * Run: node test-issue-353-path-hash-warn.js
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

// Extract the REAL functions from public/app.js (no hand-rolled copies).
function loadAppFunctions(names) {
  const source = fs.readFileSync('public/app.js', 'utf8');
  let extracted = '';
  for (const name of names) {
    const start = source.search(new RegExp('^function ' + name + '\\s*\\(', 'm'));
    if (start < 0) throw new Error('public/app.js no longer declares function ' + name);
    const open = source.indexOf('{', start);
    let depth = 0, end = -1;
    for (let i = open; i < source.length; i++) {
      if (source[i] === '{') depth++;
      else if (source[i] === '}' && --depth === 0) { end = i + 1; break; }
    }
    extracted += source.slice(start, end) + '\n';
  }
  const ctx = { Number, String };
  vm.createContext(ctx);
  vm.runInContext(extracted + 'this.__fns = {' + names.join(',') + '};', ctx);
  return ctx.__fns;
}

const { isPathHashSizeWarn, renderPathHashSize } =
  loadAppFunctions(['escapeHtml', 'pathHashSizeValue', 'isPathHashSizeWarn', 'renderPathHashSize']);

console.log('\n=== #353 isPathHashSizeWarn ===');

test('1-byte is a warning, including the string form a JSON field can carry', () => {
  assert.strictEqual(isPathHashSizeWarn(1), true);
  assert.strictEqual(isPathHashSizeWarn('1'), true);
});

test('2- and 3-byte are not a warning', () => {
  assert.strictEqual(isPathHashSizeWarn(2), false);
  assert.strictEqual(isPathHashSizeWarn(3), false);
});

test('missing or invalid sizes are not a warning', () => {
  for (const v of [0, 4, -1, null, undefined, NaN, '', 'x', true]) {
    assert.strictEqual(isPathHashSizeWarn(v), false, 'for ' + String(v));
  }
});

console.log('\n=== #353 renderPathHashSize ===');

test('1-byte renders the warn modifier, the shared warn class, an icon and a recommendation for screen readers', () => {
  const html = renderPathHashSize(1, 'Sent with: 1-byte', { block: 'ch-path-hash-badge' });
  assert.match(html, /class="ch-path-hash-badge ch-path-hash-badge--warn path-hash-warn"/);
  assert.match(html, /<svg class="ph-icon" aria-hidden="true"><use href="\/icons\/phosphor-sprite\.svg#ph-warning"\/><\/svg>/);
  // #353 round 3: an amber recommendation, not a red error. The visible label
  // stays factual; a sr-only clause carries the recommendation for non-sighted
  // users and anyone who cannot perceive the amber colour.
  assert.match(html, /Sent with: 1-byte<span class="sr-only"> — recommended: 2- or 3-byte path hash<\/span><\/span>$/);
  assert.ok(!/Warning: /.test(html), 'must not read as an error ("Warning:")');
});

test('a recommendation-labelled call omits the sr-only clause to avoid repeating itself', () => {
  // The node badge (opts.recInLabel) shows the recommendation as its visible
  // label already, so the duplicate sr-only clause is suppressed.
  const html = renderPathHashSize(1, 'Recommended: 2- or 3-byte path hash',
    { block: 'node-path-hash-badge', cls: 'badge', recInLabel: true });
  assert.match(html, />Recommended: 2- or 3-byte path hash<\/span>$/);
  assert.ok(!html.includes('sr-only'), 'recInLabel must not add a duplicate sr-only clause');
});

test('1-byte tooltip leads with the recommendation, keeps the collision reason and names the firmware settings', () => {
  const html = renderPathHashSize(1, '1 byte', { block: 'detail-hash-size' });
  const title = (html.match(/title="([^"]*)"/) || [])[1] || '';
  assert.match(title, /^Recommended: /);
  assert.match(title, /collide/);
  assert.match(title, /2- or 3-byte/);
  // firmware/docs/faq.md 3.9.3 (companion) and cli_commands.md (repeater)
  assert.match(title, /Settings → Experimental Settings/);
  assert.match(title, /path\.hash\.mode/);
  // faq.md 3.9.3/3.9.6: pre-1.14 repeaters drop 2/3-byte packets
  assert.match(title, /1\.14/);
});

test('2- and 3-byte render neutral: no warn class, no icon, caller title kept', () => {
  for (const size of [2, 3]) {
    const html = renderPathHashSize(size, 'Sent with: ' + size + '-byte',
      { block: 'ch-path-hash-badge', title: 'neutral tip' });
    assert.strictEqual(html,
      '<span class="ch-path-hash-badge" title="neutral tip">Sent with: ' + size + '-byte</span>');
  }
});

test('neutral render without a caller title emits no title attribute', () => {
  assert.strictEqual(renderPathHashSize(2, '2 bytes', { block: 'detail-hash-size' }),
    '<span class="detail-hash-size">2 bytes</span>');
});

test('the warn tooltip replaces the neutral caller title', () => {
  const html = renderPathHashSize(1, 'x', { block: 'b', title: 'neutral tip' });
  assert.ok(!html.includes('neutral tip'));
});

test('extra classes are kept on both variants', () => {
  assert.match(renderPathHashSize(1, 'x', { block: 'node-path-hash-badge', cls: 'badge' }),
    /class="badge node-path-hash-badge node-path-hash-badge--warn path-hash-warn"/);
  assert.match(renderPathHashSize(2, 'x', { block: 'node-path-hash-badge', cls: 'badge' }),
    /class="badge node-path-hash-badge"/);
});

test('missing or invalid sizes render nothing', () => {
  for (const v of [0, 4, null, undefined, NaN, '', '<img src=x onerror=alert(1)>']) {
    assert.strictEqual(renderPathHashSize(v, 'x', { block: 'b' }), '', 'for ' + String(v));
  }
});

test('label and title are escaped', () => {
  const html = renderPathHashSize(2, '<b>', { block: 'b', title: '"x' });
  assert.ok(html.includes('&lt;b&gt;') && html.includes('&quot;x'));
  assert.ok(!html.includes('<b>'));
});

console.log('\n=== #353 theming: --path-hash-warn ===');
{
  const css = fs.readFileSync('public/style.css', 'utf8');
  // The light-theme semantic :root block (the one declaring --danger); the
  // dark blocks redefine --danger, which the variable then follows.
  const dangerAt = css.indexOf('--danger: var(--palette-red-600');
  const rootBlock = css.slice(css.lastIndexOf('\n:root {', dangerAt), css.indexOf('\n}', dangerAt));

  test(':root declares --path-hash-warn defaulting to var(--warning)', () => {
    // #353 round 3: 1-byte is a recommendation, so it follows the amber
    // warning palette, not the red danger palette, in both themes.
    assert.match(rootBlock, /--path-hash-warn:\s*var\(--warning\);/);
    assert.ok(!/--path-hash-warn:\s*var\(--danger\)/.test(rootBlock), 'must not default to --danger');
  });

  test('.path-hash-warn colours its text with the variable, never a hardcoded colour', () => {
    const m = css.match(/\.path-hash-warn\s*\{([^}]*)\}/);
    assert.ok(m, '.path-hash-warn rule missing');
    assert.match(m[1], /color:\s*var\(--path-hash-warn\)/);
    assert.ok(!/#[0-9a-f]{3,8}\b|rgba?\(/i.test(m[1]), 'hardcoded colour in .path-hash-warn');
  });

  test('the channel badge warn modifier uses the variable for its border', () => {
    const m = css.match(/\.ch-path-hash-badge--warn\s*\{([^}]*)\}/);
    assert.ok(m, '.ch-path-hash-badge--warn rule missing');
    assert.match(m[1], /border-color:\s*var\(--path-hash-warn\)/);
  });
}

console.log('\n' + passed + '/' + (passed + failed) + ' tests passed');
process.exit(failed > 0 ? 1 : 0);
