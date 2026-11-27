/**
 * #880 M3 (AGENTS rule 8) — Live "Follow a packet": the viewer's replay
 * settings in the customizer (Display > Live Map) and the --follow-* theme
 * tokens.
 *
 * The real public/live-follow.js, public/customize-v2.js and
 * public/cb-presets.js run in vm sandboxes; nothing is copied.
 *   1. the rule (LiveFollow.SETTINGS): keys, defaults equal to the M1
 *      constants, bounds, resolve; replayOptions / settleBufferMs; the
 *      schedule at the defaults is the M1 schedule;
 *   2. the `follow` section: persisted in the customizer's overrides by
 *      the rule, import validation, export round trip, the effective config
 *      live.js reads, the Display badge, setFollowSetting;
 *   3. theme: THEME_CSS_MAP, labels, outside preset detection, applyCSS in
 *      light and dark, style.css tokens built on tokens both themes define,
 *      cb-presets followAccents (>= 3:1 on the light and the dark card,
 *      distinct), clearPreset.
 * Usage: node test-880-m3-follow-customizer.js
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

const DEFAULTS = { speed: 1, targetSeconds: 6, settleSeconds: null };

function loadFollow() {
  const ctx = { window: {}, console, Math, Number, String, JSON, Array, Object, isFinite };
  vm.createContext(ctx);
  vm.runInContext(fs.readFileSync(path.join(__dirname, 'public/live-follow.js'), 'utf8'), ctx, { filename: 'live-follow.js' });
  return ctx.window.LiveFollow;
}

function load(o) {
  o = o || {};
  return loadCustomizer({
    theme: o.theme,
    scripts: o.noRule ? [] : ['live-follow.js'],
    setup: (ctx) => { if (o.bufferMs !== undefined) ctx.window.PROPAGATION_BUFFER_MS = o.bufferMs; },
  });
}

// ── 1. The rule ─────────────────────────────────────────────────────────────

test('rule: keys and defaults are the M1 constants (speed 1x, 6 s, the server window)', () => {
  const LF = loadFollow();
  eq(LF.SETTINGS.KEYS, ['speed', 'targetSeconds', 'settleSeconds']);
  eq(LF.SETTINGS.DEFAULTS, DEFAULTS);
  assert.strictEqual(LF.SETTINGS.DEFAULTS.targetSeconds * 1000, LF.TARGET_MS);
  eq(LF.SETTINGS.SPEEDS, [0.5, 1, 2]);
  eq(LF.SETTINGS.LIMITS, { targetSeconds: [3, 15], settleSeconds: [0, 30] });
});

test('rule: valid() takes the speed steps and whole numbers inside the bounds only', () => {
  const v = loadFollow().SETTINGS.valid;
  [0.5, 1, 2].forEach((s) => assert.ok(v('speed', s), 'speed ' + s));
  [0.25, 4, 8, 0, '1', null, NaN].forEach((s) => assert.ok(!v('speed', s), 'speed ' + s));
  [3, 6, 15].forEach((n) => assert.ok(v('targetSeconds', n), 'target ' + n));
  [2, 16, 6.5, '6', null, Infinity].forEach((n) => assert.ok(!v('targetSeconds', n), 'target ' + n));
  [0, 5, 30].forEach((n) => assert.ok(v('settleSeconds', n), 'settle ' + n));
  [-1, 31, 2.5, '5', null].forEach((n) => assert.ok(!v('settleSeconds', n), 'settle ' + n));
  assert.ok(!v('lateSeconds', 60), 'unknown key');
});

test('rule: resolve() is the section over the defaults; a bad value is the default', () => {
  const R = loadFollow().SETTINGS;
  eq(R.resolve(null), DEFAULTS);
  eq(R.resolve('2x'), DEFAULTS);
  eq(R.resolve({ speed: 2, targetSeconds: 10, settleSeconds: 0 }), { speed: 2, targetSeconds: 10, settleSeconds: 0 });
  eq(R.resolve({ speed: 4, targetSeconds: 99, settleSeconds: -3, extra: 1 }), DEFAULTS);
});

test('replayOptions and settleBufferMs: the defaults are 1x, 6000 ms and the server window', () => {
  const LF = loadFollow();
  eq(LF.replayOptions(undefined), { speed: 1, targetMs: 6000 });
  eq(LF.replayOptions({ speed: 0.5, targetSeconds: 12 }), { speed: 0.5, targetMs: 12000 });
  assert.strictEqual(LF.settleBufferMs(undefined, 5000), 5000);
  assert.strictEqual(LF.settleBufferMs({}, 2500), 2500, 'the server value, unrounded');
  assert.strictEqual(LF.settleBufferMs({ settleSeconds: 0 }, 5000), 0);
  assert.strictEqual(LF.settleBufferMs({ settleSeconds: 12 }, 5000), 12000);
  assert.strictEqual(LF.settleMs(Date.now() - 1000, LF.settleBufferMs({ settleSeconds: 12 }, 5000), Date.now()) > 10000, true);
});

test('stepMs follows the settings; an out-of-bounds target is the default', () => {
  const LF = loadFollow();
  assert.strictEqual(LF.stepMsFor(4, LF.replayOptions({})), 1200);
  assert.strictEqual(LF.stepMsFor(4, LF.replayOptions({ speed: 2 })), 600);
  assert.strictEqual(LF.stepMsFor(4, LF.replayOptions({ speed: 0.5 })), 2400);
  assert.strictEqual(LF.stepMsFor(4, LF.replayOptions({ targetSeconds: 3 })), 600);
  assert.strictEqual(LF.stepMsFor(9, LF.replayOptions({ targetSeconds: 15 })), 1500);
  assert.strictEqual(LF.stepMsFor(4, { targetMs: 60000 }), 1200, 'a raw 60 s target is out of bounds');
  assert.strictEqual(LF.stepMsFor(4, { targetMs: 1000 }), 1200, 'a raw 1 s target is out of bounds');
});

test('at the defaults the schedule is exactly the M1 schedule', () => {
  const LF = loadFollow();
  const model = { depth: 3, relays: [{ depth: 1 }, { depth: 2 }], observers: [{ hops: 0 }, { hops: 2 }, { hops: 3, late: true }] };
  eq(LF.buildFollowSchedule(model, LF.replayOptions(undefined)), plain(LF.buildFollowSchedule(model, {})));
  assert.strictEqual(LF.buildFollowSchedule(model, LF.replayOptions({ speed: 2 })).stepMs, 750);
});

// ── 2. The section ──────────────────────────────────────────────────────────

test('follow is a known object section; the keys are the module rule', () => {
  const { api } = load();
  assert.ok(api.VALID_SECTIONS.indexOf('follow') >= 0);
  eq(api.FOLLOW_SETTING_KEYS, ['speed', 'targetSeconds', 'settleSeconds']);
  eq(api.followSettings({}), DEFAULTS);
  assert.strictEqual(api.validateShape({ follow: { speed: 2 } }).valid, true);
  assert.strictEqual(api.validateShape({ follow: '2x' }).valid, false);
});

test('persistence: writeOverrides stores valid values in the overrides key, drops the rest', () => {
  const c = load();
  c.api.writeOverrides({ follow: { speed: 2, targetSeconds: 99, settleSeconds: 0, lateSeconds: 60 } });
  eq(c.api.readOverrides(), { follow: { speed: 2, settleSeconds: 0 } });
  eq(JSON.parse(c.localStorage.getItem('cs-theme-overrides')).follow, { speed: 2, settleSeconds: 0 });
  assert.ok(c.warnings.some((w) => /follow\.targetSeconds/.test(w)), 'a rejected value is logged');
  c.api.writeOverrides({ follow: { speed: 4, targetSeconds: 6.5 } });
  eq(c.api.readOverrides(), {}, 'nothing valid, no section');
});

test('persistence: the stored section is what live.js reads from the effective config', () => {
  const c = load();
  c.api.writeOverrides({ follow: { speed: 0.5, targetSeconds: 12, settleSeconds: 3 } });
  const eff = c.api.computeEffective({}, c.api.readOverrides());
  eq(eff.follow, { speed: 0.5, targetSeconds: 12, settleSeconds: 3 });
  const LF = loadFollow();
  eq(LF.replayOptions(eff.follow), { speed: 0.5, targetMs: 12000 });
  assert.strictEqual(LF.settleBufferMs(eff.follow, 5000), 3000);
  eq(c.api.followSettings(eff), { speed: 0.5, targetSeconds: 12, settleSeconds: 3 });
});

test('without the module on the page no value is accepted and no section is shown', () => {
  const c = load({ noRule: true });
  c.api.writeOverrides({ follow: { speed: 2 } });
  eq(c.api.readOverrides(), {});
  assert.strictEqual(c.api.followSettings({}), null);
  assert.strictEqual(c.api.renderFollowSettings(), '');
  assert.strictEqual(c.api.setFollowSetting('speed', 2), false);
});

test('validateShape names each bad value and what it must be; null settle and unknown keys pass', () => {
  const { api, warnings } = load();
  const res = api.validateShape({ follow: { speed: 3, targetSeconds: 2, settleSeconds: 31 } });
  assert.strictEqual(res.valid, false);
  const txt = res.errors.join('\n');
  assert.ok(/follow\.speed = "3".*0\.5, 1, 2/.test(txt), txt);
  assert.ok(/follow\.targetSeconds = "2".*3-15/.test(txt), txt);
  assert.ok(/follow\.settleSeconds = "31".*0-30/.test(txt), txt);
  assert.strictEqual(api.validateShape({ follow: { settleSeconds: null, lateSeconds: 9 } }).valid, true);
  assert.ok(warnings.some((w) => /Unknown follow key.*lateSeconds/.test(w)));
});

test('export round trip', () => {
  const delta = { follow: { speed: 2, targetSeconds: 10, settleSeconds: 0 } };
  const a = load();
  a.api.writeOverrides(delta);
  const exported = a.api.readOverrides();
  eq(exported, delta);
  const b = load();
  assert.strictEqual(b.api.validateShape(exported).valid, true);
  b.api.writeOverrides(exported);
  eq(b.api.readOverrides(), delta);
  eq(b.api.followSettings(b.api.computeEffective({}, exported)), delta.follow);
});

test('the Display badge counts the section', () => {
  const c = load();
  const before = c.api.countOverrides('follow');
  c.api.writeOverrides({ follow: { speed: 2, targetSeconds: 4 } });
  eq([before, c.api.countOverrides('follow')], [0, 2]);
  assert.ok(/cv2-tab-badge">2</.test(c.api.renderTabs()), 'Display tab badge');
});

test('setFollowSetting: a default (or the server window) is a cleared override, a bad value refused', async () => {
  const c = load({ bufferMs: 4000 });
  c.api.writeOverrides({ follow: { speed: 2, targetSeconds: 9, settleSeconds: 7 } });
  assert.strictEqual(c.api.setFollowSetting('speed', 1), true);
  assert.strictEqual(c.api.setFollowSetting('targetSeconds', 6), true);
  assert.strictEqual(c.api.setFollowSetting('settleSeconds', 4), true, 'the server window');
  eq(c.api.readOverrides(), {});
  assert.strictEqual(c.api.setFollowSetting('speed', 8), false);
  assert.strictEqual(c.api.setFollowSetting('targetSeconds', 20), false);
  assert.strictEqual(c.api.setFollowSetting('settleSeconds', null), true, 'empty: the server window');
  assert.strictEqual(c.api.setFollowSetting('targetSeconds', 12), true);
  await new Promise((r) => setTimeout(r, 700)); // the customizer's debounced write
  eq(c.api.readOverrides(), { follow: { targetSeconds: 12 } });
});

test('the Display tab carries the Live Map placeholder, filled as DOM (no markup string)', () => {
  const { api } = load();
  assert.strictEqual(api.renderFollowSettings(), '<div data-cv2-follow-settings></div>');
  assert.ok(api.renderDisplay().includes('data-cv2-follow-settings'), 'not in the Display tab');
  const src = fs.readFileSync(path.join(__dirname, 'public/customize-v2.js'), 'utf8');
  const fn = src.slice(src.indexOf('function _buildFollowSettings'), src.indexOf('// ── #968 M2 Live kiosk ──'));
  assert.ok(fn.length > 200, 'builder not found');
  assert.ok(!/innerHTML|insertAdjacentHTML|outerHTML/.test(fn), 'the builder writes markup');
});

// ── 3. Theme tokens ─────────────────────────────────────────────────────────

const TOKENS = { followRing: '--follow-ring', followRelay: '--follow-relay', followObserver: '--follow-observer', followFlag: '--follow-flag' };
const css = fs.readFileSync(path.join(__dirname, 'public/style.css'), 'utf8');
const liveCss = fs.readFileSync(path.join(__dirname, 'public/live.css'), 'utf8');

test('THEME_CSS_MAP maps every Follow token, outside preset detection, with a label', () => {
  const { api } = load();
  Object.keys(TOKENS).forEach((k) => assert.strictEqual(api.THEME_CSS_MAP[k], TOKENS[k], k));
  eq(api.FOLLOW_COLOR_KEYS, Object.keys(TOKENS));
  const src = fs.readFileSync(path.join(__dirname, 'public/customize-v2.js'), 'utf8');
  Object.keys(TOKENS).forEach((k) => assert.ok((src.match(new RegExp('\\b' + k + ': \'', 'g')) || []).length >= 3, k + ' lacks a label or description'));
  assert.ok(/class="cust-follow-colors"/.test(src), 'no Theme tab group');
});

test('a theme colour reaches the page in light; a themeDark colour in dark', () => {
  const light = load();
  light.api.init({});
  light.api.writeOverrides({ theme: { followRelay: '#112233' }, themeDark: { followRelay: '#445566' } });
  light.api.applyCSS(light.api.computeEffective({}, light.api.readOverrides()), light.api.readOverrides());
  assert.strictEqual(light.rootStyle._props['--follow-relay'], '#112233');
  const dark = load({ theme: 'dark' });
  dark.api.init({});
  dark.api.writeOverrides({ theme: { followRelay: '#112233' }, themeDark: { followRelay: '#445566', followRing: '#778899' } });
  dark.api.applyCSS(dark.api.computeEffective({}, dark.api.readOverrides()), dark.api.readOverrides());
  assert.strictEqual(dark.rootStyle._props['--follow-relay'], '#445566');
  assert.strictEqual(dark.rootStyle._props['--follow-ring'], '#778899');
});

function block(sel) {
  const i = css.indexOf(sel);
  assert.ok(i >= 0, sel + ' missing');
  let depth = 0, j = css.indexOf('{', i);
  for (let k = j; k < css.length; k++) {
    if (css[k] === '{') depth++;
    else if (css[k] === '}' && --depth === 0) return css.slice(j, k);
  }
  return '';
}

test('style.css: each token once in :root, on tokens the light and both dark blocks define', () => {
  Object.values(TOKENS).forEach((v) => {
    const n = css.split('\n').filter((l) => new RegExp('^\\s*' + v + ':').test(l)).length;
    assert.strictEqual(n, 1, v + ' defined ' + n + ' times');
  });
  assert.ok(/--follow-ring: var\(--accent\);/.test(css));
  assert.ok(/--follow-relay: var\(--mc-follow-relay, var\(--status-yellow\)\);/.test(css));
  assert.ok(/--follow-observer: var\(--mc-follow-observer, var\(--status-green\)\);/.test(css));
  assert.ok(/--follow-flag: var\(--warning\);/.test(css));
  const darkMedia = block('@media (prefers-color-scheme: dark)');
  const darkAttr = block('\n[data-theme="dark"] {');
  ['--accent', '--status-yellow', '--status-green', '--warning'].forEach((t) => {
    assert.ok(new RegExp('^\\s*' + t + ':', 'm').test(css.slice(0, css.indexOf('@media (prefers-color-scheme: dark)'))), t + ' not in light');
    // --accent is one value for both themes (the customizer sets it per theme).
    if (t === '--accent') return;
    assert.ok(new RegExp('^\\s*' + t + ':', 'm').test(darkAttr), t + ' not in [data-theme=dark]');
    assert.ok(new RegExp('^\\s*' + t + ':', 'm').test(darkMedia), t + ' not in the dark media block');
  });
});

test('live.css reads the tokens and hard-codes no colour for the follow UI', () => {
  const start = liveCss.indexOf('/* #880 — "Follow a packet"');
  assert.ok(start >= 0);
  const blk = liveCss.slice(start).replace(/\/\*[\s\S]*?\*\//g, '');
  ['--follow-ring', '--follow-relay', '--follow-observer', '--follow-flag'].forEach((t) => assert.ok(blk.includes('var(' + t + ')'), t));
  const own = blk.split('\n').filter((l) => /lf-|follow/.test(l)).join('\n');
  assert.ok(!/#[0-9a-fA-F]{3,8}\b|rgba?\(/.test(own), 'a literal colour in the follow CSS');
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
const IDS = ['default', 'deut', 'prot', 'trit', 'achromat'];

test('cb-presets: every preset sets the relay and observer colours, clearPreset removes them', () => {
  const { MCP, props } = loadPresets();
  IDS.forEach((id) => {
    assert.ok(MCP.applyPreset(id, { skipPersist: true }), id);
    ['relay', 'observer'].forEach((k) => assert.ok(/^#[0-9a-f]{6}$/i.test(props['--mc-follow-' + k] || ''), id + ' ' + k));
  });
  MCP.clearPreset();
  eq(Object.keys(props).filter((k) => /follow/.test(k)), []);
});

test('preset colours are >= 3:1 on the light and the dark card and distinct within a preset', () => {
  const LIGHT = '#ffffff', DARK = '#232340';
  assert.ok(new RegExp('--surface-2: ' + LIGHT).test(css) && new RegExp('--surface-2: ' + DARK).test(css), 'card backgrounds moved: update this test');
  const { MCP, props } = loadPresets();
  IDS.forEach((id) => {
    MCP.applyPreset(id, { skipPersist: true });
    const v = ['relay', 'observer'].map((k) => props['--mc-follow-' + k]);
    v.forEach((c, i) => {
      assert.ok(contrast(c, LIGHT) >= 3, id + ' ' + i + ' ' + c + ' on light ' + contrast(c, LIGHT).toFixed(2));
      assert.ok(contrast(c, DARK) >= 3, id + ' ' + i + ' ' + c + ' on dark ' + contrast(c, DARK).toFixed(2));
    });
    assert.notStrictEqual(v[0].toLowerCase(), v[1].toLowerCase(), id + ' relay = observer');
  });
});

test('the docs name every viewer setting, every theme token and the server default', () => {
  const custom = fs.readFileSync(path.join(__dirname, 'docs/CUSTOMIZATION.md'), 'utf8');
  ['speed', 'targetSeconds', 'settleSeconds', 'propagationBufferMs'].forEach((k) => assert.ok(custom.includes(k), 'CUSTOMIZATION.md misses ' + k));
  Object.keys(TOKENS).forEach((k) => assert.ok(custom.includes(k), 'CUSTOMIZATION.md misses ' + k));
  const guide = fs.readFileSync(path.join(__dirname, 'docs/user-guide/live.md'), 'utf8');
  assert.ok(/Live Map/.test(guide) && /Replay speed/.test(guide), 'the Live guide does not point at the settings');
});

(async () => {
  for (const [name, fn] of tests) {
    try { await fn(); passed++; console.log('  ✅ ' + name); } catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
  }
  console.log('\n' + passed + ' passed, ' + failed + ' failed');
  process.exit(failed ? 1 : 0);
})();
