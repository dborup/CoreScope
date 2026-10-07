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

test('1-byte renders the warn modifier, the shared warn class, an icon and screen-reader text', () => {
  const html = renderPathHashSize(1, 'Sent with: 1-byte', { block: 'ch-path-hash-badge' });
  assert.match(html, /class="ch-path-hash-badge ch-path-hash-badge--warn path-hash-warn"/);
  assert.match(html, /<svg class="ph-icon" aria-hidden="true"><use href="\/icons\/phosphor-sprite\.svg#ph-warning"\/><\/svg>/);
  assert.match(html, /<span class="sr-only">Warning: <\/span>Sent with: 1-byte<\/span>$/);
});

test('1-byte tooltip explains the collision risk, recommends 2/3-byte and names the firmware settings', () => {
  const html = renderPathHashSize(1, '1 byte', { block: 'detail-hash-size' });
  const title = (html.match(/title="([^"]*)"/) || [])[1] || '';
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

  // The theme token blocks of style.css, sliced at their top-level openers:
  // the palette :root, the light semantic :root, and the two dark blocks
  // (OS-level media query and the manual [data-theme="dark"] toggle).
  function blockAt(opener) {
    const at = css.indexOf(opener);
    assert.ok(at >= 0, 'style.css lost ' + JSON.stringify(opener));
    const indent = opener.match(/^\n( *)/)[1];
    return css.slice(at, css.indexOf('\n' + indent + '}', at + opener.length));
  }
  function decls(block) {
    const out = {};
    const re = /(--[\w-]+)\s*:\s*([^;]+);/g;
    const body = block.replace(/\/\*[\s\S]*?\*\//g, '');
    let m;
    while ((m = re.exec(body))) out[m[1]] = m[2].trim();
    return out;
  }
  const rootBlocks = css.split('\n:root {').slice(1, 3).map((b) => b.slice(0, b.indexOf('\n}')));
  const light = Object.assign({}, decls(rootBlocks[0]), decls(rootBlocks[1]));
  const darkMedia = blockAt('\n  :root:not([data-theme="light"]) {');
  const darkToggle = blockAt('\n[data-theme="dark"] {');
  const themes = {
    light,
    'dark (prefers-color-scheme)': Object.assign({}, light, decls(darkMedia)),
    'dark ([data-theme="dark"])': Object.assign({}, light, decls(darkToggle)),
  };

  // Resolve var(--x[, fallback]) chains to a #rrggbb colour.
  function resolve(vars, value, depth) {
    assert.ok((depth || 0) < 20, 'var() cycle at ' + value);
    const v = value.trim();
    const m = v.match(/^var\(\s*(--[\w-]+)\s*(?:,\s*(.+))?\)$/);
    if (!m) return v;
    if (vars[m[1]] != null) return resolve(vars, vars[m[1]], (depth || 0) + 1);
    assert.ok(m[2] != null, 'undefined ' + m[1] + ' without a fallback');
    return resolve(vars, m[2], (depth || 0) + 1);
  }
  function luminance(hex) {
    const h = hex.replace('#', '');
    assert.ok(/^[0-9a-f]{6}$/i.test(h), 'not a #rrggbb colour: ' + hex);
    const [r, g, b] = [0, 2, 4].map((i) => parseInt(h.substr(i, 2), 16) / 255)
      .map((c) => (c <= 0.03928 ? c / 12.92 : Math.pow((c + 0.055) / 1.055, 2.4)));
    return 0.2126 * r + 0.7152 * g + 0.0722 * b;
  }
  function contrast(a, b) {
    const x = luminance(a), y = luminance(b);
    return (Math.max(x, y) + 0.05) / (Math.min(x, y) + 0.05);
  }
  // The surfaces the warning text sits on: the full packet-detail page
  // (--content-bg), the packet side pane (--detail-bg), the channel and node
  // badges (--surface-2), and cards (--card-bg).
  const SURFACES = ['--content-bg', '--detail-bg', '--surface-2', '--card-bg'];

  test(':root declares --path-hash-warn and both dark blocks redefine it identically', () => {
    assert.ok(light['--path-hash-warn'], 'light :root lacks --path-hash-warn');
    const a = decls(darkMedia)['--path-hash-warn'], b = decls(darkToggle)['--path-hash-warn'];
    assert.ok(a && b, 'a dark block lacks --path-hash-warn (media ' + a + ', toggle ' + b + ')');
    assert.strictEqual(a, b, 'dark blocks out of sync');
  });

  // F1 of the PR #356 review: --danger (#dc2626) on --content-bg (#f4f5f7)
  // is 4.43:1, below WCAG AA 4.5:1 for 13px/600 text (axe color-contrast).
  for (const [theme, vars] of Object.entries(themes)) {
    test(theme + ': --path-hash-warn meets WCAG AA (4.5:1) on every surface it renders on', () => {
      const fg = resolve(vars, 'var(--path-hash-warn)');
      for (const s of SURFACES) {
        const bg = resolve(vars, 'var(' + s + ')');
        const ratio = contrast(fg, bg);
        assert.ok(ratio >= 4.5, fg + ' on ' + s + ' ' + bg + ' is ' + ratio.toFixed(2) + ':1');
      }
    });
  }

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
