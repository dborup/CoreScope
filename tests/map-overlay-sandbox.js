/* tests/map-overlay-sandbox.js — vm sandbox for the optional map overlays
 * in public/map-tile-providers.js (#680 DHM terrain, #723 protected ancient
 * monuments), shared by test-issue-680-dhm-terrain.js and
 * test-issue-723-heritage-overlay.js.
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
 */
'use strict';
const vm = require('vm');
const fs = require('fs');
const path = require('path');

const SRC = fs.readFileSync(path.join(__dirname, '..', 'public', 'map-tile-providers.js'), 'utf8');
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

function makeLeaflet() {
  function evented(o) {
    const h = {};
    o.on = (types, fn) => { types.split(' ').forEach(t => (h[t] = h[t] || []).push(fn)); return o; };
    o.off = (t, fn) => { if (h[t]) h[t] = h[t].filter(f => f !== fn); return o; };
    o.once = (t, fn) => { const w = (e) => { o.off(t, w); fn(e); }; return o.on(t, w); };
    o.fire = (t, e) => { (h[t] || []).slice().forEach(fn => fn(Object.assign({ type: t }, e || {}))); return o; };
    // Leaflet's Layer.getAttribution.
    o.getAttribution = () => (o.options || {}).attribution;
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
  L.control.attribution = (opts) => {
    const counts = {};
    const c = {
      opts,
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
    // Like Leaflet: a layer listed in the control fires overlayadd/remove,
    // after the map's layer set already reflects the change.
    addLayer(l) {
      if (layers.has(l)) return map;
      layers.add(l);
      if (map._controls.some(c => !c._removed && Object.values(c.overlays || {}).includes(l))) map.fire('overlayadd', { layer: l });
      map.fire('layeradd', { layer: l });
      return map;
    },
    removeLayer(l) {
      if (!layers.has(l)) return map;
      layers.delete(l);
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

// opts: hash, search, storage, theme, dhm (MC_DHM_CFG), heritage (MC_HERITAGE_CFG).
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
  ctx.globalThis = ctx;
  vm.createContext(ctx);
  vm.runInContext(SRC, ctx);
  ctx.replaced = replaced;
  return ctx;
}

// withAttribution: the map has its own attribution control (Map page);
// without it the map is like Live.
function buildControl(ctx, withAttribution) {
  const map = makeMap();
  if (withAttribution) map.attributionControl = { builtIn: true };
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

module.exports = { makeStorage, makeLeaflet, makeMap, makeSandbox, buildControl, liveAttributions, liveCredits };
