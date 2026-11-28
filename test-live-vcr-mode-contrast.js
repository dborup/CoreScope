/* WCAG AA contrast for the Live page VCR mode badge (#vcrMode) in all three
 * modes: LIVE, PAUSED and REPLAY.
 *
 * The badge text used swatch hues (--status-green, --status-yellow,
 * --accent) that fail AA on the light-theme VCR bar (2.27:1, 1.81:1,
 * 2.52:1); the axe gate (test-a11y-axe-1668.js) reported LIVE on
 * /live desktop light. Text now uses the semantic text tokens
 * (--status-green-text, --warning, --link-color).
 *
 * Background model: the badge sits on `.vcr-bar`, whose background is
 * `color-mix(in srgb, var(--surface-1) N%, transparent)`, i.e. surface-1 at
 * N% alpha over the page. PAUSED/REPLAY add their own translucent tint.
 * The bar pixel is composed over both --surface-0 and --surface-1 as the
 * page underneath, the mode tint over that, and the text must reach 4.5:1
 * in light theme and in BOTH dark token blocks ([data-theme="dark"] and the
 * prefers-color-scheme: dark :root block).
 */
'use strict';
const fs = require('fs');
const assert = require('assert');

const styleCss = fs.readFileSync('public/style.css', 'utf8');
const liveCss = fs.readFileSync('public/live.css', 'utf8');

function extractBlockTokens(blockRegex, label) {
  const tokens = {};
  const re = new RegExp(blockRegex.source, 'g');
  let m, matched = false;
  while ((m = re.exec(styleCss)) !== null) {
    matched = true;
    const decl = /^\s*(--[a-z0-9-]+)\s*:\s*([^;]+);/gim;
    let d;
    while ((d = decl.exec(m[1])) !== null) tokens[d[1]] = d[2].trim();
  }
  if (!matched) throw new Error(`no \`${label} { ... }\` block found in style.css — parser needs updating`);
  return tokens;
}

const lightTokens = extractBlockTokens(/:root\s*\{([\s\S]*?)\n\}/, ':root');
const THEMES = {
  light: lightTokens,
  'dark [data-theme]': { ...lightTokens, ...extractBlockTokens(/\[data-theme="dark"\]\s*\{([\s\S]*?)\n\}/, '[data-theme="dark"]') },
  'dark prefers-color-scheme': { ...lightTokens, ...extractBlockTokens(
    /@media \(prefers-color-scheme: dark\)\s*\{\s*:root:not\(\[data-theme="light"\]\)\s*\{([\s\S]*?)\n\s*\}/,
    '@media (prefers-color-scheme: dark) :root:not([data-theme="light"])') },
};

function resolve(value, tokens) {
  let v = value.trim();
  for (let i = 0; i < 6; i++) {
    const m = v.match(/^var\(\s*(--[a-z0-9-]+)\s*(?:,\s*(.+))?\)$/i);
    if (!m) return v;
    v = (tokens[m[1]] || m[2] || '').trim();
    if (!v) return null;
  }
  return v;
}

function parseColor(s) {
  if (!s) return null;
  let m = s.match(/^#([0-9a-f]{6})$/i);
  if (m) return [0, 2, 4].map(i => parseInt(m[1].slice(i, i + 2), 16)).concat(1);
  m = s.match(/^rgba?\(\s*(\d+)\s*,\s*(\d+)\s*,\s*(\d+)\s*(?:,\s*([0-9.]+))?\s*\)$/i);
  return m ? [+m[1], +m[2], +m[3], m[4] != null ? parseFloat(m[4]) : 1] : null;
}

const over = (fg, bg) => fg.slice(0, 3).map((v, i) => v * fg[3] + bg[i] * (1 - fg[3])).concat(1);

function relLum(rgb) {
  const f = (c) => { c /= 255; return c <= 0.03928 ? c / 12.92 : Math.pow((c + 0.055) / 1.055, 2.4); };
  return 0.2126 * f(rgb[0]) + 0.7152 * f(rgb[1]) + 0.0722 * f(rgb[2]);
}

function contrast(a, b) {
  const [hi, lo] = [relLum(a), relLum(b)].sort((x, y) => y - x);
  return (hi + 0.05) / (lo + 0.05);
}

const hex = (c) => '#' + c.slice(0, 3).map(v => Math.round(v).toString(16).padStart(2, '0')).join('');

function ruleBodies(selector) {
  const esc = selector.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
  return [...liveCss.matchAll(new RegExp(`(?:^|\\n)\\s*${esc}\\s*\\{([^}]*)\\}`, 'g'))].map(r => r[1]);
}

function ruleBody(selector) {
  const bodies = ruleBodies(selector);
  assert.strictEqual(bodies.length, 1, `expected exactly one \`${selector} { ... }\` rule in live.css, found ${bodies.length}`);
  return bodies[0];
}

function decl(body, prop) {
  const m = body.match(new RegExp(`(?:^|[;\\s])${prop}\\s*:\\s*([^;]+?)\\s*;`));
  return m ? m[1] : null;
}

// .vcr-bar has several rules (padding/layout overrides); exactly one may set
// its background, otherwise this static model cannot tell which one wins.
const barBgs = ruleBodies('.vcr-bar').map(b => decl(b, 'background')).filter(Boolean);
assert.strictEqual(barBgs.length, 1, `expected exactly one \`.vcr-bar\` background declaration in live.css, found ${barBgs.length}`);
const barBg = barBgs[0];
const barMix = barBg && barBg.match(/^color-mix\(in srgb,\s*(var\(--[a-z0-9-]+\))\s+(\d+(?:\.\d+)?)%,\s*transparent\)$/);
assert.ok(barMix, `.vcr-bar background must be \`color-mix(in srgb, var(--token) N%, transparent)\` for this model (got "${barBg}")`);

const MODES = ['.vcr-mode-live', '.vcr-mode-paused', '.vcr-mode-replay'];

console.log('\nLive VCR mode badge contrast');
let failures = 0;
for (const selector of MODES) {
  const body = ruleBody(selector);
  const color = decl(body, 'color');
  assert.ok(color, `\`${selector}\` must declare a text color`);
  const tint = decl(body, 'background');
  for (const [theme, tokens] of Object.entries(THEMES)) {
    const fg = parseColor(resolve(color, tokens));
    assert.ok(fg && fg[3] === 1, `[${theme}] ${selector}: could not resolve opaque color "${color}"`);
    const barColor = parseColor(resolve(barMix[1], tokens));
    assert.ok(barColor, `[${theme}] could not resolve .vcr-bar ${barMix[1]}`);
    barColor[3] = parseFloat(barMix[2]) / 100;
    const tintColor = tint ? parseColor(resolve(tint, tokens)) : null;
    assert.ok(!tint || tintColor, `[${theme}] ${selector}: could not parse background "${tint}"`);
    for (const page of ['--surface-0', '--surface-1']) {
      const pageColor = parseColor(resolve(`var(${page})`, tokens));
      assert.ok(pageColor, `[${theme}] could not resolve ${page}`);
      let bg = over(barColor, pageColor);
      if (tintColor) bg = over(tintColor, bg);
      const ratio = contrast(fg, bg);
      const ok = ratio >= 4.5;
      if (!ok) failures++;
      console.log(`  ${ok ? '✅' : '❌'} ${selector.padEnd(17)} ${theme.padEnd(26)} ${hex(fg)} on ${hex(bg)} (bar over ${page}): ${ratio.toFixed(2)}:1`);
    }
  }
}

if (failures) {
  console.error(`\nFAIL: ${failures} VCR mode badge theme/background combination(s) below WCAG AA 4.5:1`);
  process.exit(1);
}
console.log('\nPASS: LIVE, PAUSED and REPLAY badge text ≥4.5:1 in light and both dark theme blocks');
