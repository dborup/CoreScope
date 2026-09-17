/* WCAG AA contrast for the Live page VCR "LIVE" mode badge (#vcrMode).
 *
 * `.vcr-mode-live` rendered its text in `--status-green` (#22c55e), the
 * swatch hue meant for backgrounds/dots. On the light-theme VCR bar that is
 * 2.27:1 and the axe gate (test-a11y-axe-1668.js, /live desktop light)
 * reported color-contrast. `--status-green-text` is the text variant
 * (#15803d on light, #22c55e on dark).
 *
 * The badge has no background of its own and sits on `.vcr-bar`, which is
 * `--surface-1` mixed 95% over the page (`--surface-0`). Rather than model
 * color-mix, require ≥4.5:1 against both surfaces in light and dark theme.
 */
'use strict';
const fs = require('fs');
const assert = require('assert');

const styleCss = fs.readFileSync('public/style.css', 'utf8');
const liveCss = fs.readFileSync('public/live.css', 'utf8');

function extractBlockTokens(css, blockRegex, label) {
  const tokens = {};
  const re = new RegExp(blockRegex.source, 'g');
  let m, matched = false;
  while ((m = re.exec(css)) !== null) {
    matched = true;
    const decl = /^\s*(--[a-z0-9-]+)\s*:\s*([^;]+);/gim;
    let d;
    while ((d = decl.exec(m[1])) !== null) tokens[d[1]] = d[2].trim();
  }
  if (!matched) throw new Error(`no \`${label} { ... }\` block found in style.css — parser needs updating`);
  return tokens;
}

const lightTokens = extractBlockTokens(styleCss, /:root\s*\{([\s\S]*?)\n\}/, ':root');
const darkTokens = extractBlockTokens(styleCss, /\[data-theme="dark"\]\s*\{([\s\S]*?)\n\}/, '[data-theme="dark"]');

function resolve(value, theme) {
  const map = theme === 'dark' ? { ...lightTokens, ...darkTokens } : lightTokens;
  let v = value.trim();
  for (let i = 0; i < 6; i++) {
    const m = v.match(/^var\(\s*(--[a-z0-9-]+)\s*(?:,\s*(.+))?\)$/i);
    if (!m) return v;
    v = (map[m[1]] || m[2] || '').trim();
    if (!v) return null;
  }
  return v;
}

function parseHex(s) {
  const m = s && s.match(/^#([0-9a-f]{6})$/i);
  return m ? [0, 2, 4].map(i => parseInt(m[1].slice(i, i + 2), 16)) : null;
}

function relLum(rgb) {
  const f = (c) => { c /= 255; return c <= 0.03928 ? c / 12.92 : Math.pow((c + 0.055) / 1.055, 2.4); };
  return 0.2126 * f(rgb[0]) + 0.7152 * f(rgb[1]) + 0.0722 * f(rgb[2]);
}

function contrast(a, b) {
  const [hi, lo] = [relLum(a), relLum(b)].sort((x, y) => y - x);
  return (hi + 0.05) / (lo + 0.05);
}

const rules = [...liveCss.matchAll(/(^|\n)\s*\.vcr-mode-live\s*\{([^}]*)\}/g)];
assert.strictEqual(rules.length, 1, `expected exactly one \`.vcr-mode-live { ... }\` rule in live.css, found ${rules.length}`);
const colorDecl = rules[0][2].match(/(?:^|;)\s*color\s*:\s*([^;]+?)\s*;/);
assert.ok(colorDecl, '`.vcr-mode-live` must declare a text color');

console.log('\nLive VCR LIVE badge contrast');
let failures = 0;
for (const theme of ['light', 'dark']) {
  const fgRaw = resolve(colorDecl[1], theme);
  const fg = parseHex(fgRaw);
  assert.ok(fg, `[${theme}] could not resolve .vcr-mode-live color "${colorDecl[1]}" (got "${fgRaw}")`);
  for (const surface of ['--surface-1', '--surface-0']) {
    const bgRaw = resolve(`var(${surface})`, theme);
    const bg = parseHex(bgRaw);
    assert.ok(bg, `[${theme}] could not resolve ${surface} (got "${bgRaw}")`);
    const ratio = contrast(fg, bg);
    const ok = ratio >= 4.5;
    if (!ok) failures++;
    console.log(`  ${ok ? '✅' : '❌'} ${theme.padEnd(5)} ${fgRaw} on ${surface} ${bgRaw}: ${ratio.toFixed(2)}:1 (need ≥4.5)`);
  }
}

if (failures) {
  console.error(`\nFAIL: .vcr-mode-live text is below WCAG AA 4.5:1 in ${failures} theme/surface combination(s)`);
  process.exit(1);
}
console.log('\nPASS: .vcr-mode-live text ≥4.5:1 on the VCR bar surfaces in light and dark theme');
