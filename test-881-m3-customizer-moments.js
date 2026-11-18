/**
 * #881 M3 (AGENTS rule 8) — the Live Moments viewer settings in the
 * customizer, and the Moments theme tokens.
 *
 * The real public/customize-v2.js, public/live-moments.js and
 * public/cb-presets.js run in vm sandboxes; nothing is copied.
 *   1. the `moments` section: a known object section, stored values by the
 *      module's rule (SETTINGS), import validation, export round trip;
 *   2. the rendered controls: shown only where the server has Moments on,
 *      one input per key, the 48 px touch rows, override dots;
 *   3. moments-defaults-changed fires when the effective section moves;
 *   4. Reset All forgets the viewer's saved Live choice;
 *   5. theme: THEME_CSS_MAP, labels, preset detection, style.css tokens,
 *      cb-presets momentAccents (>= 3:1 on the light and the dark card).
 * Usage: node test-881-m3-customizer-moments.js
 */
'use strict';
const fs = require('fs');
const path = require('path');
const vm = require('vm');
const assert = require('assert');
const { loadCustomizer } = require('./tests/customizer-sandbox');

let passed = 0, failed = 0;
const tests = [];
function test(name, fn) { tests.push([name, fn]); }
const plain = (v) => JSON.parse(JSON.stringify(v === undefined ? null : v));
const eq = (a, b, m) => assert.deepStrictEqual(plain(a), b, m);

const DEFAULTS = { enabled: true, visibleCards: 3, dwellSeconds: 8, showNewNode: true, showBackOnline: true, showRecordLink: true, showBusiest: true };

function load(o) {
  o = o || {};
  return loadCustomizer({
    scripts: o.noRule ? [] : ['live-moments.js'],
    setup: (ctx) => { ctx.window.MC_MOMENTS_CFG = o.cfg === undefined ? { enabled: true, historySize: 50 } : o.cfg; },
  });
}

// ── 1. The section ──────────────────────────────────────────────────────────

test('moments is a known object section; the keys are the module rule', () => {
  const { api } = load();
  assert.ok(api.VALID_SECTIONS.indexOf('moments') >= 0);
  eq(api.momentsSettings({}), DEFAULTS);
  assert.strictEqual(api.validateShape({ moments: { dwellSeconds: 12 } }).valid, true);
  assert.strictEqual(api.validateShape({ moments: 'on' }).valid, false);
});

test('writeOverrides keeps valid values, drops the rest, by the module rule', () => {
  const c = load();
  c.api.writeOverrides({ moments: { visibleCards: 5, dwellSeconds: 99, enabled: 'no', showBusiest: false, maxPerMinute: 60, historySize: 5 } });
  eq(c.api.readOverrides(), { moments: { visibleCards: 5, showBusiest: false } });
  assert.ok(c.warnings.some((w) => /moments\.dwellSeconds/.test(w)), 'a rejected value is logged');
  assert.ok(c.warnings.some((w) => /moments\.enabled/.test(w)));
});

test('without the module on the page no value is accepted', () => {
  const c = load({ noRule: true });
  c.api.writeOverrides({ moments: { visibleCards: 2 } });
  eq(c.api.readOverrides(), {});
  assert.strictEqual(c.api.momentsSettings({}), null);
  assert.strictEqual(c.api.renderMomentsSettings(), '');
});

test('validateShape names each bad value and its allowed range; unknown keys only warn', () => {
  const { api, warnings } = load();
  const res = api.validateShape({ moments: { visibleCards: 6, dwellSeconds: 2.5, showNewNode: 'yes', historySize: 5 } });
  assert.strictEqual(res.valid, false);
  const txt = res.errors.join('\n');
  assert.ok(/moments\.visibleCards = "6".*1-5/.test(txt), txt);
  assert.ok(/moments\.dwellSeconds = "2.5".*3-30/.test(txt), txt);
  assert.ok(/moments\.showNewNode = "yes".*true or false/.test(txt), txt);
  assert.ok(!/historySize/.test(txt), 'an unknown key is not an error');
  assert.ok(warnings.some((w) => /Unknown moments key.*historySize/.test(w)));
});

test('export round trip', () => {
  const delta = { moments: { enabled: false, visibleCards: 5, dwellSeconds: 20, showNewNode: false } };
  const a = load();
  a.api.writeOverrides(delta);
  const exported = a.api.readOverrides();
  eq(exported, delta);
  const b = load();
  assert.strictEqual(b.api.validateShape(exported).valid, true);
  b.api.writeOverrides(exported);
  eq(b.api.readOverrides(), delta);
  eq(b.api.momentsSettings(b.api.computeEffective({}, exported)),
    Object.assign({}, DEFAULTS, delta.moments));
});

test('the Display badge counts the section', () => {
  const c = load();
  const before = c.api.countOverrides('moments');
  c.api.writeOverrides({ moments: { visibleCards: 2, showBusiest: false } });
  eq([before, c.api.countOverrides('moments')], [0, 2]);
});

// ── 2. The controls ─────────────────────────────────────────────────────────

const input = (html, key) => (html.match(new RegExp('<input[^>]*data-cv2-moments="' + key + '"[^>]*>')) || [''])[0];

test('controls: one per key, shown only with the server\'s Moments on', () => {
  const c = load();
  const html = c.api.renderMomentsSettings();
  ['enabled', 'visibleCards', 'dwellSeconds', 'showNewNode', 'showBackOnline', 'showRecordLink', 'showBusiest']
    .forEach((k) => assert.ok(input(html, k), 'no input for ' + k));
  assert.ok(/type="checkbox"/.test(input(html, 'enabled')) && /checked/.test(input(html, 'enabled')), 'on by default');
  assert.ok(/type="number"[^>]*min="1"[^>]*max="5"|min="1"[^>]*max="5"/.test(input(html, 'visibleCards')));
  assert.ok(/min="3"[^>]*max="30"/.test(input(html, 'dwellSeconds')));
  assert.ok(/value="3"/.test(input(html, 'visibleCards')) && /value="8"/.test(input(html, 'dwellSeconds')));
  assert.strictEqual(load({ cfg: null }).api.renderMomentsSettings(), '', 'shown with Moments off');
  assert.strictEqual(load({ cfg: undefined === 1 }).api.renderMomentsSettings(), '', 'shown with a falsy cfg');
});

test('controls: touch rows are 48 px, labels carry the override dot', () => {
  const c = load();
  c.api.writeOverrides({ moments: { dwellSeconds: 12, showBusiest: false } });
  c.ctx.window.SITE_CONFIG = c.api.computeEffective({}, c.api.readOverrides());
  const html = c.api.renderMomentsSettings();
  assert.ok((html.match(/min-height:48px/g) || []).length >= 5, 'checkbox rows keep the 48 px hit area');
  assert.ok(/value="12"/.test(input(html, 'dwellSeconds')));
  assert.ok(!/checked/.test(input(html, 'showBusiest')));
  assert.ok((html.match(/data-reset-s="moments"/g) || []).length >= 2, 'a reset dot per overridden key');
});

test('controls: the operator\'s policy keys are not offered', () => {
  const html = load().api.renderMomentsSettings();
  ['maxPerMinute', 'historySize', 'backOnlineHours', 'recordMinKm', 'busyMinRelays'].forEach((k) => assert.ok(!html.includes(k), k));
});

// ── 3. Event, 4. Reset ──────────────────────────────────────────────────────

test('moments-defaults-changed fires when the effective section moves, not otherwise', () => {
  const c = load();
  c.api.init({});
  const n = () => c.events.filter((t) => t === 'moments-defaults-changed').length;
  const run = () => c.api.applyCSS(c.api.computeEffective({}, c.api.readOverrides()), c.api.readOverrides());
  const n0 = n();
  c.api.writeOverrides({ moments: { visibleCards: 5 } });
  run();
  const n1 = n();
  assert.ok(n1 > n0, 'no event for a changed value');
  run();
  assert.strictEqual(n(), n1, 'an unchanged section fired again');
});

test('Reset All clears the section and the viewer\'s saved Live choice', () => {
  const c = load();
  c.api.init({});
  c.api.writeOverrides({ moments: { enabled: false } });
  c.localStorage.setItem('live-moments', 'false');
  c.api.resetAll();
  assert.strictEqual(c.localStorage.getItem('live-moments'), null);
  eq(c.api.readOverrides(), {});
});

// ── 5. Theme tokens ─────────────────────────────────────────────────────────

const TOKENS = {
  momentCardBg: '--moment-card-bg', momentNewNode: '--moment-new-node', momentBackOnline: '--moment-back-online',
  momentRecordLink: '--moment-record-link', momentBusiest: '--moment-busiest',
};
const css = fs.readFileSync(path.join(__dirname, 'public/style.css'), 'utf8');
const liveCss = fs.readFileSync(path.join(__dirname, 'public/live.css'), 'utf8');

test('THEME_CSS_MAP maps every Moments token, outside preset detection', () => {
  const { api } = load();
  Object.keys(TOKENS).forEach((k) => {
    assert.strictEqual(api.THEME_CSS_MAP[k], TOKENS[k], k);
    assert.ok(api.MOMENTS_COLOR_KEYS.indexOf(k) >= 0, k + ' not in MOMENTS_COLOR_KEYS');
  });
  eq(api.MOMENTS_COLOR_KEYS, Object.keys(TOKENS));
});

test('a theme colour reaches the page; the theme tab lists the group', () => {
  const c = load();
  c.api.init({});
  c.api.writeOverrides({ theme: { momentNewNode: '#112233' } });
  c.api.applyCSS(c.api.computeEffective({}, c.api.readOverrides()), c.api.readOverrides());
  assert.strictEqual(c.rootStyle._props['--moment-new-node'], '#112233');
});

test('style.css defines each token once in :root, with a preset hook for the accents', () => {
  Object.values(TOKENS).forEach((v) => {
    const n = css.split('\n').filter((l) => new RegExp('^\\s*' + v + ':').test(l)).length;
    assert.strictEqual(n, 1, v + ' defined ' + n + ' times');
  });
  ['new-node', 'back-online', 'record-link', 'busiest'].forEach((k) =>
    assert.ok(new RegExp('--moment-' + k + ': var\\(--mc-moment-' + k + ', #[0-9a-f]{6}\\)').test(css), k));
  assert.ok(/--moment-card-bg: var\(--card-bg\)/.test(css));
});

test('live.css reads the tokens and hard-codes no colour for the cards', () => {
  const start = liveCss.lastIndexOf('/*', liveCss.indexOf('#881 M2 — Moments'));
  const block = liveCss.slice(start);
  assert.ok(/background: var\(--moment-card-bg\)/.test(block));
  ['new_node', 'back_online', 'record_link', 'busiest_repeater'].forEach((k) => assert.ok(block.includes('[data-kind="' + k + '"]'), k));
  assert.ok(/\.live-moment-main \.ph-icon \{[^}]*var\(--moment-accent/.test(block));
  assert.ok(!/#[0-9a-fA-F]{3,8}\b/.test(block.replace(/\/\*[\s\S]*?\*\//g, '')), 'a hex colour in the Moments CSS');
});

function relLum(hex) {
  const c = [1, 3, 5].map((i) => parseInt(hex.slice(i, i + 2), 16) / 255).map((v) => (v <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4)));
  return 0.2126 * c[0] + 0.7152 * c[1] + 0.0722 * c[2];
}
const contrast = (a, b) => { const x = relLum(a), y = relLum(b); return (Math.max(x, y) + 0.05) / (Math.min(x, y) + 0.05); };

function loadPresets() {
  const props = {};
  const style = { setProperty: (k, v) => { props[k] = v; }, removeProperty: (k) => { delete props[k]; }, getPropertyValue: (k) => props[k] || '' };
  const ctx = { window: { dispatchEvent() {}, CustomEvent: class {}, addEventListener() {} }, document: { body: { setAttribute() {}, removeAttribute() {} }, documentElement: { style } },
    localStorage: { getItem: () => null, setItem() {}, removeItem() {} }, console };
  vm.createContext(ctx);
  vm.runInContext(fs.readFileSync(path.join(__dirname, 'public/cb-presets.js'), 'utf8'), ctx, { filename: 'cb-presets.js' });
  return { MCP: ctx.window.MeshCorePresets, props };
}

test('cb-presets: every preset sets the four kind accents, clearPreset removes them', () => {
  const { MCP, props } = loadPresets();
  const ids = ['default', 'deut', 'prot', 'trit', 'achromat'];
  ids.forEach((id) => {
    assert.ok(MCP.applyPreset(id, { skipPersist: true }), id);
    ['new-node', 'back-online', 'record-link', 'busiest'].forEach((k) => assert.ok(/^#[0-9a-f]{6}$/i.test(props['--mc-moment-' + k] || ''), id + ' ' + k));
  });
  MCP.clearPreset();
  assert.deepStrictEqual(Object.keys(props).filter((k) => /moment/.test(k)), []);
});

test('accents are >= 3:1 on the light and the dark card (non-text, WCAG 1.4.11), in the preset and the default', () => {
  const LIGHT = '#ffffff', DARK = '#232340'; // --card-bg in style.css: surface-2
  assert.ok(new RegExp('--surface-2: ' + LIGHT).test(css) && new RegExp('--surface-2: ' + DARK).test(css), 'card backgrounds moved: update this test');
  const { MCP, props } = loadPresets();
  const check = (label, c) => {
    assert.ok(contrast(c, LIGHT) >= 3, label + ' ' + c + ' on light ' + contrast(c, LIGHT).toFixed(2));
    assert.ok(contrast(c, DARK) >= 3, label + ' ' + c + ' on dark ' + contrast(c, DARK).toFixed(2));
  };
  ['default', 'deut', 'prot', 'trit', 'achromat'].forEach((id) => {
    MCP.applyPreset(id, { skipPersist: true });
    ['new-node', 'back-online', 'record-link', 'busiest'].forEach((k) => check(id + ' ' + k, props['--mc-moment-' + k]));
  });
  ['new-node', 'back-online', 'record-link', 'busiest'].forEach((k) => {
    const m = css.match(new RegExp('--moment-' + k + ': var\\(--mc-moment-' + k + ', (#[0-9a-f]{6})\\)'));
    check('style.css fallback ' + k, m[1]);
  });
});

test('within a preset the four accents are distinct (the icon carries the rest)', () => {
  const { MCP, props } = loadPresets();
  ['default', 'deut', 'trit'].forEach((id) => {
    MCP.applyPreset(id, { skipPersist: true });
    const v = ['new-node', 'back-online', 'record-link', 'busiest'].map((k) => props['--mc-moment-' + k]);
    assert.strictEqual(new Set(v).size, 4, id);
  });
});

test('the docs name every viewer setting, every theme token and the default', () => {
  const custom = fs.readFileSync(path.join(__dirname, 'docs/CUSTOMIZATION.md'), 'utf8');
  const guide = fs.readFileSync(path.join(__dirname, 'docs/moments.md'), 'utf8');
  const { api } = load();
  api.momentsSettings({}) && Object.keys(DEFAULTS).forEach((k) => assert.ok(custom.includes(k), 'CUSTOMIZATION.md misses ' + k));
  Object.keys(TOKENS).forEach((k) => assert.ok(custom.includes(k), 'CUSTOMIZATION.md misses ' + k));
  assert.ok(custom.includes('moments.md') && /moments\.md/.test(fs.readFileSync(path.join(__dirname, 'docs/user-guide/live.md'), 'utf8')), 'guide not linked');
  assert.ok(/retained history/.test(guide) && /first ever/i.test(guide), 'the "first ever" caveat is gone from the guide');
  ['New node', 'Back online', 'Record link', 'Busiest repeater'].forEach((k) => assert.ok(guide.includes('**' + k + '**'), k));
});

for (const [name, fn] of tests) {
  try { fn(); passed++; console.log('  ✅ ' + name); } catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}
console.log('\n' + passed + ' passed, ' + failed + ' failed');
process.exit(failed ? 1 : 0);
