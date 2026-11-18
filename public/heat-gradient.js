/* heat-gradient.js — #914: the node heat map's palette, one helper for Map
 * and Live.
 *
 * The gradient is a single hue family (violet -> magenta) that the "Colour by
 * elevation" ramp does not use, so a hot heat area is never mistaken for high
 * terrain. Its five stops are theme tokens, read when the layer is built:
 *   --heat-1..5          style.css (:root and both dark blocks), the
 *                        customizer (THEME_CSS_MAP heat1..5)
 *   --mc-heat-light-1..5 set by a colour-blind preset (cb-presets.js
 *   --mc-heat-dark-1..5  heatRamp), one ramp per base-map mode
 *   --heat-stop-1..5     what this helper reads: style.css resolves it to the
 *                        preset's ramp for the current mode, else --heat-N
 * A missing or unusable token falls back to DEFAULTS, so the layer is never
 * left without a gradient.
 *
 * watch(cb) calls back after a theme, customizer, dark-mode or preset change
 * that actually changed the gradient, so a page rebuilds its layer without a
 * reload and does nothing for an unrelated change.
 */
(function () {
  'use strict';

  var STOPS = [0.2, 0.4, 0.6, 0.8, 1];
  // The last-resort values: the :root --heat-N in style.css (a unit test
  // keeps them equal).
  var DEFAULTS = ['#5b21b6', '#7e22ce', '#a21caf', '#c026d3', '#e879f9'];
  var COLOR_RE = /^(#[0-9a-fA-F]{3,8}|rgba?\([0-9\s,.%\/]+\)|hsla?\([0-9\s,.%\/deg]+\))$/;

  function token(cs, name) {
    var v = cs && typeof cs.getPropertyValue === 'function' ? String(cs.getPropertyValue(name) || '').trim() : '';
    return COLOR_RE.test(v) ? v : '';
  }

  function colors() {
    var cs = null;
    try { cs = getComputedStyle(document.documentElement); } catch (e) {}
    var out = [];
    for (var i = 1; i <= 5; i++) {
      out.push(token(cs, '--heat-stop-' + i) || DEFAULTS[i - 1]);
    }
    return out;
  }

  /** {0.2: c1, 0.4: c2, ... 1: c5} — leaflet.heat's gradient option. */
  function gradient() {
    var c = colors(), g = {};
    for (var i = 0; i < STOPS.length; i++) g[STOPS[i]] = c[i];
    return g;
  }

  /** The options both pages hand to L.heatLayer. */
  function options() {
    return { radius: 25, blur: 15, maxZoom: 14, minOpacity: 0.05, gradient: gradient() };
  }

  function signature() { return colors().join('|'); }

  // One module-level data-theme observer feeds every watcher: the dark-mode
  // toggle flips the attribute without a theme-changed event. It lives for the
  // page's life (a subscriber-less call is a no-op), so a page that mounts and
  // unmounts leaves no observer of its own behind.
  var subscribers = [];
  function pokeAll() { subscribers.slice().forEach(function (f) { f(); }); }
  if (typeof MutationObserver === 'function' && typeof document !== 'undefined' && document.documentElement && document.documentElement.nodeType) {
    new MutationObserver(pokeAll).observe(document.documentElement, { attributes: true, attributeFilter: ['data-theme'] });
  }

  /** cb() after a change that moved the gradient. Returns the unsubscribe. */
  function watch(cb) {
    var last = signature(), timer = null;
    function check() {
      timer = null;
      var now = signature();
      if (now === last) return;
      last = now;
      cb();
    }
    function schedule() {
      if (timer != null) return;
      timer = -1; // set first: a synchronous timer (a test's) clears it in check()
      var t = setTimeout(check, 30);
      if (timer === -1) timer = t;
    }
    window.addEventListener('theme-changed', schedule);
    window.addEventListener('cb-preset-changed', schedule);
    subscribers.push(schedule);
    return function unwatch() {
      window.removeEventListener('theme-changed', schedule);
      window.removeEventListener('cb-preset-changed', schedule);
      var k = subscribers.indexOf(schedule);
      if (k >= 0) subscribers.splice(k, 1);
      if (timer != null) { clearTimeout(timer); timer = null; }
    };
  }

  window.HeatGradient = { DEFAULTS: DEFAULTS, STOPS: STOPS, gradient: gradient, options: options, watch: watch };
})();
