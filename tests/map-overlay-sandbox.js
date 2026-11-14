/* tests/map-overlay-sandbox.js — vm sandbox for the optional map overlays
 * in public/map-tile-providers.js (#680 DHM terrain, #723 protected ancient
 * monuments, #724 antenna sites), shared by test-issue-680-dhm-terrain.js,
 * test-issue-723-heritage-overlay.js and test-issue-724-masts-layer.js.
 *
 * Runs the real file in a vm context with a recording Leaflet stub:
 *   - layers, panes and the layer control's overlays are recorded;
 *   - addLayer/removeLayer fire overlayadd/overlayremove for layers the
 *     control lists, the way L.Control.Layers does;
 *   - after overlayadd the map fires layeradd; on removal overlayremove
 *     fires before the layer's own 'remove' listeners (L.Control.Layers
 *     subscribes to the layer before any attribution hook does);
 *   - L.control.attribution() keeps credits the way Leaflet 1.9.4's
 *     Control.Attribution does: addTo() claims map.attributionControl and
 *     counts the credits of the layers already on the map (with no removal
 *     hook), then listens to layeradd, counting each later layer's credit
 *     and dropping it again on that layer's 'remove'. remove() only stops
 *     listening to layeradd and does not give map.attributionControl back.
 *     getContainer() returns a stub element whose classList records the
 *     classes added to it (#752 adds the compact class there).
 */
'use strict';
const vm = require('vm');
const fs = require('fs');
const path = require('path');

const SRC = fs.readFileSync(path.join(__dirname, '..', 'public', 'map-tile-providers.js'), 'utf8');
const MASTS_SRC = fs.readFileSync(path.join(__dirname, '..', 'public', 'masts-layer.js'), 'utf8');
// The shared escapeHtml from public/app.js, run as is (not a copy).
const APP_SRC = fs.readFileSync(path.join(__dirname, '..', 'public', 'app.js'), 'utf8');
const ESCAPE_SRC = (function () {
  const i = APP_SRC.indexOf('function escapeHtml(s) {');
  return APP_SRC.slice(i, APP_SRC.indexOf('\n}\n', i) + 3);
})();
const OVERLAY_LABELS = '.leaflet-control-layers-overlays label';

function makeStorage(init) {
  const store = Object.assign({}, init || {});
  return {
    getItem(k) { return Object.prototype.hasOwnProperty.call(store, k) ? store[k] : null; },
    setItem(k, v) { store[k] = String(v); },
    removeItem(k) { delete store[k]; },
    _raw: store,
  };
}

// A light stand-in for L.Control.Attribution, for the suites that drive a
// single call site with a tile-layer spy rather than the whole registry
// (#332, #390, #1470). It records what the control was given; the fuller
// model inside makeLeaflet() counts credits the way Leaflet does.
function makeAttributionStub() {
  const credits = [];
  const classes = [];
  const c = {
    credits, classes, opts: null,
    addTo(map) { map.attributionControl = c; (map._attributionControls = map._attributionControls || []).push(c); return c; },
    addAttribution(t) { if (t) credits.push(t); return c; },
    removeAttribution(t) { const i = credits.indexOf(t); if (i >= 0) credits.splice(i, 1); return c; },
    getContainer() { return { classList: { add: (cl) => { if (!classes.includes(cl)) classes.push(cl); }, contains: (cl) => classes.includes(cl) } }; },
  };
  return (opts) => { c.opts = opts || null; return c; };
}

function makeLeaflet() {
  function evented(o) {
    const h = {};
    o.on = (types, fn) => { types.split(' ').forEach(t => (h[t] = h[t] || []).push(fn)); return o; };
    o.off = (t, fn) => { if (h[t]) h[t] = h[t].filter(f => f !== fn); return o; };
    o.once = (t, fn) => { const w = (e) => { o.off(t, w); fn(e); }; return o.on(t, w); };
    o.fire = (t, e) => { (h[t] || []).slice().forEach(fn => fn(Object.assign({ type: t }, e || {}))); return o; };
    // Leaflet's Layer.getAttribution and Layer.addTo.
    o.getAttribution = () => (o.options || {}).attribution;
    o.addTo = (map) => { map.addLayer(o); return o; };
    return o;
  }
  const L = {
    CRS: { EPSG3857: { code: 'EPSG:3857' } },
    latLngBounds: (b) => ({ _bounds: b }),
    tileLayer: (url, opts) => evented({ _url: url, options: opts || {}, kind: 'tile' }),
    control: {
      layers: (base, overlays, opts) => {
        const overlayLabels = Object.keys(overlays || {}).map(name => {
          const attrs = {};
          return { name, setAttribute: (k, v) => { attrs[k] = v; }, attrs };
        });
        const ctl = {
          base, overlays, opts, overlayLabels,
          addTo(map) { ctl._map = map; map._controls.push(ctl); return ctl; },
          remove() { ctl._removed = true; },
          getContainer() {
            return {
              querySelectorAll: (sel) => sel === OVERLAY_LABELS ? overlayLabels : [],
              querySelector: (sel) => sel === OVERLAY_LABELS ? (overlayLabels[0] || null) : null,
            };
          },
        };
        return ctl;
      },
    },
  };
  L.tileLayer.wms = (url, opts) => evented({ _url: url, options: opts || {}, kind: 'wms' });
  // #724 — what public/masts-layer.js builds: layer groups of canvas
  // circle markers. A group on a map puts its members on that map.
  L.canvas = (opts) => ({ kind: 'canvas', options: opts || {} });
  L.circleMarker = (ll, opts) => {
    const m = evented({ kind: 'circleMarker', latlng: ll, options: Object.assign({}, opts || {}) });
    m.setStyle = (st) => { Object.assign(m.options, st); return m; };
    m.bindPopup = (content, popupOpts) => { m.popup = content; m.popupOptions = popupOpts; return m; };
    return m;
  };
  L.layerGroup = (layers, opts) => {
    const members = new Set(layers || []);
    const g = evented({ kind: 'layerGroup', options: opts || {}, members });
    g.addLayer = (l) => { members.add(l); if (g._map) g._map.addLayer(l); return g; };
    g.removeLayer = (l) => { members.delete(l); if (g._map) g._map.removeLayer(l); return g; };
    g.eachLayer = (fn) => { Array.from(members).forEach(fn); return g; };
    return g;
  };
  L.control.attribution = (opts) => {
    const counts = {};
    // Leaflet's Control.getContainer(): the registry adds the compact class
    // to it for the inset maps (#752).
    const classes = [];
    const container = {
      classList: {
        add: (cl) => { if (!classes.includes(cl)) classes.push(cl); },
        contains: (cl) => classes.includes(cl),
      },
      classes,
    };
    const c = {
      opts,
      getContainer() { return container; },
      classes,
      addAttribution(t) { if (t) counts[t] = (counts[t] || 0) + 1; return c; },
      removeAttribution(t) { if (t && counts[t]) counts[t]--; return c; },
      // Leaflet 1.9.4 Control.Attribution._addAttribution.
      _addAttribution(e) {
        if (!e.layer.getAttribution) return;
        c.addAttribution(e.layer.getAttribution());
        e.layer.once('remove', () => c.removeAttribution(e.layer.getAttribution()));
      },
      // Leaflet 1.9.4 Control.Attribution.onAdd: map.attributionControl = this,
      // count the credits already on the map, then follow layeradd.
      addTo(map) {
        c._map = map; map.attributionControl = c; map._attributionControls.push(c);
        map.layers.forEach(l => { if (l.getAttribution) c.addAttribution(l.getAttribution()); });
        map.on('layeradd', c._addAttribution);
        return c;
      },
      // ...and onRemove only stops following layeradd; map.attributionControl
      // keeps pointing at it.
      remove() { c._removed = true; c._map.off('layeradd', c._addAttribution); return c; },
      // The credits the control shows (Leaflet's _update lists every one
      // whose count is above zero).
      credits() { return Object.keys(counts).filter(t => counts[t] > 0); },
    };
    return c;
  };
  return L;
}

function makeMap() {
  const panes = { tilePane: { zIndex: 200, attrs: {}, setAttribute() {}, removeAttribute() {}, style: {} } };
  const layers = new Set();
  const map = {
    _controls: [], _attributionControls: [], panes, layers,
    getPane: (n) => panes[n],
    createPane: (n) => {
      const classes = [];
      panes[n] = { style: {}, classList: { add: (c) => classes.push(c) }, classes };
      return panes[n];
    },
    hasLayer: (l) => layers.has(l),
    eachLayer(fn) { Array.from(layers).forEach(l => fn(l)); return map; },
    // Like Leaflet: a layer listed in the control fires overlayadd/remove,
    // after the map's layer set already reflects the change.
    addLayer(l) {
      if (layers.has(l)) return map;
      layers.add(l);
      if (l.kind === 'layerGroup') { l._map = map; l.members.forEach(m => map.addLayer(m)); }
      if (map._controls.some(c => !c._removed && Object.values(c.overlays || {}).includes(l))) map.fire('overlayadd', { layer: l });
      map.fire('layeradd', { layer: l });
      return map;
    },
    removeLayer(l) {
      if (!layers.has(l)) return map;
      layers.delete(l);
      if (l.kind === 'layerGroup') { l.members.forEach(m => map.removeLayer(m)); l._map = null; }
      map.fire('layerremove', { layer: l });
      if (map._controls.some(c => !c._removed && Object.values(c.overlays || {}).includes(l))) map.fire('overlayremove', { layer: l });
      if (l.fire) l.fire('remove');
      return map;
    },
  };
  const h = {};
  map.on = (types, fn) => { types.split(' ').forEach(t => (h[t] = h[t] || []).push(fn)); return map; };
  map.off = (t, fn) => { if (h[t]) h[t] = h[t].filter(f => f !== fn); return map; };
  map.fire = (t, e) => { (h[t] || []).slice().forEach(fn => fn(Object.assign({ type: t }, e || {}))); return map; };
  return map;
}

// opts: hash, search, storage, theme, dhm (MC_DHM_CFG), heritage (MC_HERITAGE_CFG),
// masts (MC_MASTS_CFG; also loads public/masts-layer.js unless noMastsLayer),
// mapCfg (MC_MAP_CFG, read when the registry loads).
function makeSandbox(opts) {
  opts = opts || {};
  const listeners = {};
  const replaced = [];
  const loc = { hash: 'hash' in opts ? opts.hash : '#/map', pathname: '/', search: opts.search || '' };
  const ctx = {
    console, URLSearchParams, JSON, Math, Object, Array, String, Number, Boolean,
    location: loc,
    history: {
      state: null,
      replaceState(_s, _t, url) { replaced.push(url); const i = url.indexOf('#'); loc.hash = i >= 0 ? url.slice(i) : ''; },
    },
    localStorage: makeStorage(opts.storage),
    document: {
      documentElement: { getAttribute: () => opts.theme || 'light' },
      querySelector: () => null,
      createElement: () => ({}),
    },
    CustomEvent: function (type, init) { this.type = type; this.detail = (init && init.detail) || null; },
    L: makeLeaflet(),
  };
  ctx.window = {
    addEventListener: (type, fn) => { (listeners[type] = listeners[type] || []).push(fn); },
    dispatchEvent: (ev) => { (listeners[ev.type] || []).slice().forEach(fn => fn(ev)); return true; },
    matchMedia: () => ({ matches: false }),
    localStorage: ctx.localStorage,
  };
  if (opts.dhm !== undefined) ctx.window.MC_DHM_CFG = opts.dhm;
  if (opts.heritage !== undefined) ctx.window.MC_HERITAGE_CFG = opts.heritage;
  if (opts.mapCfg !== undefined) ctx.window.MC_MAP_CFG = opts.mapCfg;
  if (opts.masts !== undefined) ctx.window.MC_MASTS_CFG = opts.masts;
  ctx.globalThis = ctx;
  vm.createContext(ctx);
  if (opts.masts !== undefined && !opts.noMastsLayer) {
    ctx.Map = Map; ctx.Set = Set; ctx.Promise = Promise;
    ctx.window.removeEventListener = () => {};
    ctx.setTimeout = (fn, ms) => ({ fn, ms });
    ctx.clearTimeout = () => {};
    ctx.getComputedStyle = () => ({ getPropertyValue: (n) => (opts.cssVars || {})[n] || '' });
    ctx.MutationObserver = function () { this.observe = () => {}; this.disconnect = () => {}; };
    vm.runInContext(ESCAPE_SRC, ctx);
    vm.runInContext(MASTS_SRC, ctx);
    ctx.window.MastsLayer = ctx.MastsLayer = ctx.window.MastsLayer || ctx.MastsLayer;
  }
  vm.runInContext(SRC, ctx);
  ctx.replaced = replaced;
  return ctx;
}

// withAttribution: the map has its own attribution control (Map page);
// without it the map is like Live. base: a layer put on the map before the
// layer control is built, as Live does with its Auto tile layer.
function buildControl(ctx, withAttribution, base) {
  const map = makeMap();
  if (withAttribution) map.attributionControl = { builtIn: true };
  if (base) map.addLayer(base);
  const auto = { _auto: true };
  ctx.window.MC_createLayerControl(map, auto, 'topleft');
  const ctl = map._controls[map._controls.length - 1];
  return { map, ctl };
}

function liveAttributions(map) {
  return map._attributionControls.filter(c => !c._removed);
}

// The credits shown on a map without its own attribution control (Live).
function liveCredits(map) {
  return liveAttributions(map).flatMap(c => c.credits()).sort();
}

module.exports = { makeStorage, makeLeaflet, makeAttributionStub, makeMap, makeSandbox, buildControl, liveAttributions, liveCredits };
