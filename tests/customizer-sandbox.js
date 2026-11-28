/* tests/customizer-sandbox.js — public/customize-v2.js in a browser-shaped
 * vm sandbox, shared by the customizer tests (test-issue-681-m6-*,
 * test-issue-724-m5-*, test-issue-727-m4-*). The real files run; nothing
 * of the customizer is copied.
 *
 * loadCustomizer(opts):
 *   theme    'light' (default) or 'dark': <html data-theme>
 *   dhm      window.MC_DHM_CFG, when given
 *   scripts  public/ files to run between antenna-height.js and
 *            customize-v2.js (index.html order), e.g. ['masts-layer.js']
 *   setup    fn(ctx) run before any script, for globals the page would set
 *   afterHeight  fn(ctx) run right after antenna-height.js, to alter the shared
 *            rule before the customizer reads it
 *   server   passed to api.init() once loaded
 * Returns { api, ctx, localStorage, rootStyle, warnings, events }: warnings
 * are console.warn lines, events the types of dispatched events.
 */
'use strict';

const vm = require('vm');
const fs = require('fs');
const path = require('path');
const assert = require('assert');

const PUBLIC = path.join(__dirname, '..', 'public');

function makeStyle() {
  const props = {};
  return {
    _props: props,
    setProperty(name, value) { props[name] = String(value); },
    removeProperty(name) { delete props[name]; },
    getPropertyValue(name) { return props[name] || ''; },
  };
}

function run(ctx, file) {
  vm.runInContext(fs.readFileSync(path.join(PUBLIC, file), 'utf8'), ctx, { filename: file });
}

function loadCustomizer(opts) {
  opts = opts || {};
  const store = {};
  const localStorage = {
    getItem(k) { return Object.prototype.hasOwnProperty.call(store, k) ? store[k] : null; },
    setItem(k, v) { store[k] = String(v); },
    removeItem(k) { delete store[k]; },
    _data: store,
  };
  const rootStyle = makeStyle();
  const bodyStyle = makeStyle();
  const warnings = [];
  const events = [];
  const body = {
    style: bodyStyle, _attrs: {},
    setAttribute(n, v) { this._attrs[n] = v; },
    getAttribute(n) { return this._attrs[n] || null; },
    removeAttribute(n) { delete this._attrs[n]; },
    hasAttribute(n) { return n in this._attrs; },
  };
  const docEl = {
    style: rootStyle,
    getAttribute: (n) => (n === 'data-theme' ? (opts.theme || 'light') : null),
    setAttribute() {}, removeAttribute() {},
  };
  const ctx = {
    window: {
      addEventListener() {}, dispatchEvent: (ev) => { events.push(ev && ev.type); return true; },
      SITE_CONFIG: {}, setInstanceTitle() {},
    },
    document: {
      readyState: 'complete',
      createElement: () => ({
        id: '', textContent: '', innerHTML: '', className: '',
        setAttribute() {}, appendChild() {}, style: makeStyle(),
        addEventListener() {}, querySelectorAll: () => [], querySelector: () => null,
      }),
      head: { appendChild() {} },
      body: body,
      documentElement: docEl,
      getElementById: () => null,
      addEventListener() {},
      querySelector: () => null,
      querySelectorAll: () => [],
    },
    console: Object.assign({}, console, { warn: (...a) => { warnings.push(a.join(' ')); } }),
    localStorage,
    setTimeout: (fn) => fn(),
    clearTimeout() {},
    Date, Math, Array, Object, JSON, String, Number, Boolean, Set, Map,
    parseInt, parseFloat, isNaN, isFinite, Infinity, NaN, undefined,
    MutationObserver: class { observe() {} },
    HashChangeEvent: class {},
    CustomEvent: class { constructor(t, o) { this.type = t; this.detail = o && o.detail; } },
    getComputedStyle: () => ({ getPropertyValue: () => '' }),
  };
  ctx.window.localStorage = localStorage;
  ctx.window.CustomEvent = ctx.CustomEvent;
  ctx.self = ctx.window;
  if (opts.dhm !== undefined) ctx.window.MC_DHM_CFG = opts.dhm;
  if (opts.setup) opts.setup(ctx);
  vm.createContext(ctx);
  // index.html order: the shared height rule first, then the customizer.
  run(ctx, 'antenna-height.js');
  if (opts.afterHeight) opts.afterHeight(ctx);
  (opts.scripts || []).forEach((f) => run(ctx, f));
  run(ctx, 'customize-v2.js');
  const api = ctx.window._customizerV2;
  assert.ok(api, 'customize-v2.js must define window._customizerV2');
  if (opts.server) api.init(opts.server);
  return { api, ctx, localStorage, rootStyle, warnings, events };
}

module.exports = { loadCustomizer, makeStyle };
