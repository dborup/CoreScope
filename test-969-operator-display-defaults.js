/**
 * #969 — the operator's display defaults for timestamps and Live Moments
 * reach the browser.
 *
 * config.json `timestamps` and `moments.display` are served on
 * /api/config/client (cmd/server). roles.js keeps them as
 * window.MC_OPERATOR_DISPLAY; app.js's bootSiteConfig waits for both
 * config fetches and hands the customizer the theme with the operator's
 * sections, so the customizer's own merge gives the precedence
 *   URL (Moments on/off only) > viewer > operator > built-in.
 *
 * The real public/roles.js, app.js, customize-v2.js and live-moments.js
 * run in vm sandboxes; nothing is copied.
 * Usage: node test-969-operator-display-defaults.js
 */
'use strict';
const fs = require('fs');
const vm = require('vm');
const assert = require('assert');
const { loadCustomizer } = require('./tests/customizer-sandbox');

let passed = 0, failed = 0;
const tests = [];
function test(name, fn) { tests.push([name, fn]); }
const plain = (v) => JSON.parse(JSON.stringify(v === undefined ? null : v));
const eq = (a, b, m) => assert.deepStrictEqual(plain(a), b, m);

const BUILTIN_TS = { defaultMode: 'ago', timezone: 'local', formatPreset: 'iso', customFormat: '', allowCustomFormat: false };
const OP_TS = { defaultMode: 'absolute', timezone: 'utc', formatPreset: 'iso-seconds', customFormat: '', allowCustomFormat: false };
const CLIENT = {
  timestamps: OP_TS,
  moments: { enabled: true, historySize: 50, display: { enabled: false, dwellSeconds: 20, visibleCards: 1 } },
};

// roles.js + app.js in a browser-shaped sandbox. fetchJson(url) answers a
// fetch with that JSON, or rejects when it returns undefined.
function loadPage(fetchJson) {
  const store = {};
  const ctx = {
    window: { addEventListener() {}, dispatchEvent() {} },
    document: {
      readyState: 'loading',
      createElement: () => ({ id: '', textContent: '', innerHTML: '', setAttribute() {}, appendChild() {}, style: {} }),
      head: { appendChild() {} },
      getElementById: () => null,
      addEventListener() {},
      querySelectorAll: () => [],
      querySelector: () => null,
    },
    console, Date, Infinity, Math, Array, Object, String, Number, JSON, RegExp, Error, TypeError,
    parseInt, parseFloat, isNaN, isFinite, encodeURIComponent, decodeURIComponent, Map, Set, Promise, URLSearchParams,
    setTimeout: () => 0, clearTimeout() {}, setInterval: () => 0, clearInterval() {},
    performance: { now: () => Date.now() },
    localStorage: { getItem: (k) => (k in store ? store[k] : null), setItem: (k, v) => { store[k] = String(v); }, removeItem: (k) => { delete store[k]; } },
    location: { hash: '' },
    CustomEvent: class {},
    addEventListener() {}, dispatchEvent() {},
    requestAnimationFrame: () => 0,
    fetch: (url) => {
      const body = fetchJson(String(url));
      if (body && typeof body.then === 'function') return body.then((b) => ({ json: () => Promise.resolve(b) }));
      return body === undefined ? Promise.reject(new Error('offline')) : Promise.resolve({ json: () => Promise.resolve(body) });
    },
  };
  vm.createContext(ctx);
  for (const f of ['payload-labels.js', 'roles.js', 'app.js']) {
    vm.runInContext(fs.readFileSync('public/' + f, 'utf8'), ctx, { filename: f });
    for (const k of Object.keys(ctx.window)) ctx[k] = ctx.window[k];
  }
  return { ctx, w: ctx.window, store };
}
const clientOnly = (client) => (url) => (url.indexOf('/api/config/client') === 0 ? client : {});

// ── 1. roles.js keeps the operator's values from /api/config/client ─────────

test('roles.js: MC_OPERATOR_DISPLAY holds the served timestamps and moments.display', async () => {
  const { w } = loadPage(clientOnly(CLIENT));
  await w.MeshConfigReady;
  eq(w.MC_OPERATOR_DISPLAY, { timestamps: OP_TS, moments: { enabled: false, dwellSeconds: 20, visibleCards: 1 } });
});

test('roles.js: no moments.display while the server has Moments off', async () => {
  const { w } = loadPage(clientOnly({ timestamps: OP_TS, moments: { enabled: false, display: { dwellSeconds: 20 } } }));
  await w.MeshConfigReady;
  eq(w.MC_OPERATOR_DISPLAY.moments, null);
});

test('roles.js: served timestamps are cast at the boundary; a bad value is the built-in', async () => {
  const { w } = loadPage(clientOnly({ timestamps: { defaultMode: 'later', timezone: 'utc', formatPreset: 7, customFormat: 3, allowCustomFormat: 'yes' } }));
  await w.MeshConfigReady;
  eq(w.MC_OPERATOR_DISPLAY.timestamps, { defaultMode: 'ago', timezone: 'utc', formatPreset: 'iso', customFormat: '', allowCustomFormat: false });
});

// ── 2. One source per value ─────────────────────────────────────────────────

test('withOperatorDisplayDefaults: the operator values replace whatever the theme carries', () => {
  const { w } = loadPage(() => undefined);
  const theme = { branding: { siteName: 'X' }, timestamps: { defaultMode: 'ago' }, moments: { dwellSeconds: 5 } };
  const cfg = w.withOperatorDisplayDefaults(theme, { timestamps: OP_TS, moments: { dwellSeconds: 20 } });
  eq(cfg, { branding: { siteName: 'X' }, timestamps: OP_TS, moments: { dwellSeconds: 20 } });
  eq(theme.timestamps, { defaultMode: 'ago' }, 'the theme object is not changed');
  eq(w.withOperatorDisplayDefaults(theme, null), { branding: { siteName: 'X' }, timestamps: BUILTIN_TS },
    'no operator values: the built-ins, never the theme endpoint');
});

// ── 3. app.js waits for both fetches and hands the customizer one config ────

function bootWith(fetchJson) {
  const page = loadPage(fetchJson);
  const inits = [];
  page.w._customizerV2 = { init: (cfg) => inits.push(plain(cfg)) };
  page.ctx._customizerV2 = page.w._customizerV2;
  return { page, inits, run: () => page.w.bootSiteConfig(page.ctx.fetch('/api/config/theme'), page.w.MeshConfigReady) };
}

test('bootSiteConfig: the customizer starts once, with the theme and the operator sections', async () => {
  const b = bootWith((url) => (url.indexOf('/api/config/theme') === 0 ? { branding: { siteName: 'X' } } : CLIENT));
  await b.run();
  eq(b.inits, [{ branding: { siteName: 'X' }, timestamps: OP_TS, moments: { enabled: false, dwellSeconds: 20, visibleCards: 1 } }]);
});

test('bootSiteConfig: a client config that lands after the theme is still waited for', async () => {
  let release;
  const late = new Promise((r) => { release = r; });
  const b = bootWith((url) => (url.indexOf('/api/config/theme') === 0 ? { branding: { siteName: 'X' } } : late.then(() => CLIENT)));
  const done = b.run();
  for (let i = 0; i < 20; i++) await Promise.resolve();
  eq(b.inits.length, 0, 'the customizer started before the client config');
  release();
  await done;
  eq(b.inits.length, 1);
  eq(b.inits[0].timestamps, OP_TS);
});

test('bootSiteConfig: a failed theme fetch still gets the operator values', async () => {
  const b = bootWith((url) => (url.indexOf('/api/config/client') === 0 ? CLIENT : undefined));
  await b.run();
  eq(b.inits.length, 1);
  eq(b.inits[0].timestamps, OP_TS);
});

test('bootSiteConfig: a failed client config fetch falls back to the built-ins', async () => {
  const b = bootWith((url) => (url.indexOf('/api/config/theme') === 0 ? {} : undefined));
  await b.run();
  eq(b.inits, [{ timestamps: BUILTIN_TS }]);
});

// ── 4. Precedence: viewer > operator > built-in (customizer merge) ──────────

function customizer(server, overrides) {
  return loadCustomizer({
    scripts: ['payload-labels.js', 'live-moments.js'],
    setup: (ctx) => {
      ctx.window.MC_MOMENTS_CFG = { enabled: true, historySize: 50 };
      if (overrides) ctx.localStorage.setItem('cs-theme-overrides', JSON.stringify(overrides));
    },
    server,
  });
}
const OPERATOR = { timestamps: OP_TS, moments: { enabled: false, dwellSeconds: 20, visibleCards: 1 } };

test('operator over built-in: SITE_CONFIG and the resolved Moments settings', () => {
  const page = loadPage(() => undefined);
  const c = customizer(page.w.withOperatorDisplayDefaults({}, OPERATOR));
  eq(c.ctx.window.SITE_CONFIG.timestamps, OP_TS);
  eq(c.api.momentsSettings(c.ctx.window.SITE_CONFIG),
    { enabled: false, visibleCards: 1, dwellSeconds: 20, showNewNode: true, showBackOnline: true, showRecordLink: true, showBusiest: true });
});

test('viewer over operator: a customizer value wins per key', () => {
  const page = loadPage(() => undefined);
  const c = customizer(page.w.withOperatorDisplayDefaults({}, OPERATOR),
    { timestamps: { defaultMode: 'ago' }, moments: { dwellSeconds: 12, enabled: true } });
  const ts = c.ctx.window.SITE_CONFIG.timestamps;
  eq([ts.defaultMode, ts.timezone, ts.formatPreset], ['ago', 'utc', 'iso-seconds']);
  const m = c.api.momentsSettings(c.ctx.window.SITE_CONFIG);
  eq([m.enabled, m.dwellSeconds, m.visibleCards], [true, 12, 1]);
  eq(c.api.readOverrides(), { timestamps: { defaultMode: 'ago' }, moments: { dwellSeconds: 12, enabled: true } },
    'a viewer value unlike the operator default is kept');
});

test('a viewer value equal to the operator default is not kept as an override', () => {
  const page = loadPage(() => undefined);
  const c = customizer(page.w.withOperatorDisplayDefaults({}, OPERATOR), { timestamps: { timezone: 'utc' }, moments: { dwellSeconds: 20 } });
  eq(c.api.readOverrides(), {});
});

test('no operator values: the built-ins', () => {
  const page = loadPage(() => undefined);
  const c = customizer(page.w.withOperatorDisplayDefaults({}, null));
  eq(c.ctx.window.SITE_CONFIG.timestamps, BUILTIN_TS);
  eq(c.api.momentsSettings(c.ctx.window.SITE_CONFIG).dwellSeconds, 8);
});

// ── 5. Where the values are used ────────────────────────────────────────────

test('timestamps: the operator format is what the page prints; a saved viewer choice still wins', () => {
  const page = loadPage(() => undefined);
  page.w.SITE_CONFIG = page.w.withOperatorDisplayDefaults({}, OPERATOR);
  page.ctx.SITE_CONFIG = page.w.SITE_CONFIG;
  const iso = '2026-01-02T03:04:05.678Z';
  eq(vm.runInContext('getTimestampMode()', page.ctx), 'absolute');
  eq(vm.runInContext('formatAbsoluteTimestamp(' + JSON.stringify(iso) + ')', page.ctx), '2026-01-02 03:04:05.678');
  page.store['meshcore-timestamp-mode'] = 'ago';
  eq(vm.runInContext('getTimestampMode()', page.ctx), 'ago');
});

test('Moments on/off: a ?moments= link > the saved choice > the operator default', () => {
  const ctx = { window: {}, console, Object, Array, Math, JSON, Date, Number, String, Boolean, isFinite };
  vm.createContext(ctx);
  vm.runInContext(fs.readFileSync('public/live-moments.js', 'utf8'), ctx);
  const M = ctx.window.LiveMoments;
  const op = M.SETTINGS.resolve(OPERATOR.moments).enabled;
  eq(op, false);
  eq(M.resolveInitialEnabled(null, null, op), false, 'the operator default');
  eq(M.resolveInitialEnabled(null, 'true', op), true, 'the saved choice');
  eq(M.resolveInitialEnabled('1', 'false', op), true, 'the link');
});

(async () => {
  for (const [name, fn] of tests) {
    try { await fn(); passed++; console.log('  ✓ ' + name); }
    catch (e) { failed++; console.log('  ✗ ' + name + '\n    ' + (e && e.message)); }
  }
  console.log('\n#969 operator display defaults: ' + passed + ' passed, ' + failed + ' failed');
  process.exit(failed ? 1 : 0);
})();
