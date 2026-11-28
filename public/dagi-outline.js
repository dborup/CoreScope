/* === CoreScope — dagi-outline.js (the selected Danish area on a map, #740 M3b) === */
'use strict';

/*
 * Draws the DAGI filter's selected area as one outline on a Leaflet map.
 *
 * create({ L, map, fetchJson, decode, color, escape }):
 *   fetchJson(query)  a Promise of GET /api/config/dagi/polygons?<query>
 *                     (the browser caches it: max-age plus an ETag).
 *   decode(resp)      DagiFilter.decodeRings.
 *   color()           the outline colour from a CSS variable; '' leaves
 *                     Leaflet's default.
 *   escape(s)         HTML-escapes the credit, which Leaflet's attribution
 *                     control renders as HTML.
 *
 * The instance:
 *   show(query, {fit})  draws query's area, replacing any other outline at
 *                       once. fit: zoom to it (a user's pick only, never a
 *                       cold link, #740 owner decision 7). Only the latest
 *                       call draws or fits.
 *   clear()             removes the outline; an answer still in flight is
 *                       dropped.
 *   query()             the area drawn or loading, '' for none.
 *   setVisible(v)       the Map's "Selected area outline" checkbox: a hidden
 *                       outline is still loaded and fitted, but off the map.
 *
 * chooseBoundary({ area, dagi }) — the Map draws one selected boundary at a
 * time (#740 plan §4.6): 'area' when an Area with an outline is selected
 * (area: true), otherwise 'dagi' when DAGI has a polygon query, else 'none'.
 *
 * One request per area: decoded areas are kept (at most CACHE_MAX), and
 * nothing is fetched on a pan or a zoom.
 */
(function () {
  var CACHE_MAX = 4;

  function create(opts) {
    var L = opts.L, map = opts.map;
    var cache = new Map(); // query → decoded area, oldest first
    var layer = null;
    var current = '';
    var generation = 0;
    var visible = true;

    function load(query) {
      if (cache.has(query)) return Promise.resolve(cache.get(query));
      return Promise.resolve(opts.fetchJson(query)).then(function (resp) {
        var area = opts.decode(resp);
        area.credit = resp && typeof resp.credit === 'string' ? resp.credit : '';
        cache.set(query, area);
        if (cache.size > CACHE_MAX) cache.delete(cache.keys().next().value);
        return area;
      });
    }

    function remove() {
      if (layer) map.removeLayer(layer);
      layer = null;
    }

    function draw(area) {
      var style = { weight: 2, opacity: 0.9, fillOpacity: 0.06, interactive: false, className: 'dagi-outline' };
      var c = opts.color();
      if (c) { style.color = c; style.fillColor = c; }
      if (area.credit) style.attribution = opts.escape(area.credit);
      layer = L.polygon(area.rings, style);
      if (visible) layer.addTo(map);
    }

    function setVisible(v) {
      visible = !!v;
      if (!layer) return;
      if (visible && !map.hasLayer(layer)) layer.addTo(map);
      else if (!visible && map.hasLayer(layer)) map.removeLayer(layer);
    }

    function show(query, o) {
      var fit = !!(o && o.fit);
      var gen = ++generation;
      if (query === current && layer) {
        if (fit) fitTo(cache.get(query));
        return Promise.resolve();
      }
      remove();
      current = query;
      return load(query).then(function (area) {
        if (gen !== generation || !area.rings.length) return;
        draw(area);
        if (fit) fitTo(area);
      }, function () {
        if (gen === generation) current = ''; // not cached: the next show() asks again
      });
    }

    function fitTo(area) {
      if (area && area.bounds) map.fitBounds(area.bounds, { padding: [24, 24] });
    }

    function clear() {
      generation++;
      current = '';
      remove();
    }

    return {
      show: show,
      clear: clear,
      setVisible: setVisible,
      query: function () { return current; },
      cacheSize: function () { return cache.size; },
    };
  }

  function chooseBoundary(sel) {
    if (sel && sel.area) return 'area';
    if (sel && sel.dagi) return 'dagi';
    return 'none';
  }

  window.DagiOutline = { create: create, chooseBoundary: chooseBoundary };
})();
