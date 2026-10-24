/* === CoreScope — region-filter.js (shared region filter component) === */
'use strict';

(function () {
  var LS_KEY = 'meshcore-region-filter';
  var _regions = {};       // { code: label }
  var _selected = null;    // Set of selected region codes, null = all
  var _listeners = [];
  var _container = null;
  var _loaded = false;
  var _urlBeforeLoad = false; // #531: the selection came from a link before regions loaded

  function loadFromStorage() {
    try {
      var stored = JSON.parse(localStorage.getItem(LS_KEY));
      if (Array.isArray(stored) && stored.length > 0) return new Set(stored);
    } catch (e) { /* ignore */ }
    return null; // null = all selected
  }

  function saveToStorage() {
    if (!_selected) {
      localStorage.removeItem(LS_KEY);
    } else {
      localStorage.setItem(LS_KEY, JSON.stringify(Array.from(_selected)));
    }
  }

  _selected = loadFromStorage();

  // #556: choices made in the control (or by Clear) in this tab, counted in
  // sessionStorage so the count outlives a reload, as history entries do.
  // Each history entry whose link ?region= was applied records the count at
  // that moment in its history.state (as Analytics records its view, #208).
  var CHOICES_KEY = 'meshcore-region-choices';
  var LINK_STATE_KEY = 'regionLinkChoices';
  var _choices = 0;
  try { _choices = Number(sessionStorage.getItem(CHOICES_KEY)) || 0; } catch (e) { /* storage blocked */ }

  function noteChoice() {
    _choices++;
    try { sessionStorage.setItem(CHOICES_KEY, String(_choices)); } catch (e) { /* storage blocked */ }
  }

  function historyState() {
    var st = window.history && window.history.state;
    return st && typeof st === 'object' ? st : null;
  }

  /** The current entry's history.state with the choice count recorded;
   *  other keys are kept and the URL is left as it is. */
  function markLinkApplied() {
    var st = historyState();
    if (st && st[LINK_STATE_KEY] === _choices) return;
    var next = {};
    if (st) Object.keys(st).forEach(function (k) { next[k] = st[k]; });
    next[LINK_STATE_KEY] = _choices;
    try { window.history.replaceState(next, ''); } catch (e) { /* Safari throttles replaceState */ }
  }

  /** Fetch regions from server */
  async function fetchRegions() {
    if (_loaded) return _regions;
    try {
      var data = await fetch('/api/config/regions').then(function (r) { return r.json(); });
      _regions = data || {};
      _loaded = true;
      // If stored selection has codes no longer valid, clean up
      if (_selected) {
        _selected = knownCodes(Array.from(_selected));
        saveToStorage();
        // #531: a link's codes are checked here on a fresh load, after pages
        // may already have fetched with them. A dropped code matches no
        // observer (the region list holds every observer's IATA), so only a
        // fall back to all regions changes what they show: tell them once.
        if (_urlBeforeLoad && !_selected) notify();
      }
      _urlBeforeLoad = false;
    } catch (e) {
      _regions = {};
    }
    return _regions;
  }

  /** The configured codes among `codes`, in their configured spelling (codes
   *  match case-insensitively, as the server does). null when none is. */
  function knownCodes(codes) {
    var byUpper = {};
    Object.keys(_regions).forEach(function (c) { byUpper[c.toUpperCase()] = c; });
    var kept = new Set();
    codes.forEach(function (c) {
      var known = byUpper[String(c).toUpperCase()];
      if (known) kept.add(known);
    });
    return kept.size > 0 ? kept : null;
  }

  function sameSelection(a, b) {
    if (!a || !b) return !a && !b;
    if (a.size !== b.size) return false;
    var same = true;
    a.forEach(function (c) { if (!b.has(c)) same = false; });
    return same;
  }

  function notify() {
    var sel = getSelected();
    _listeners.forEach(function (fn) { fn(sel); });
  }

  function decodeParam(s) {
    try { return decodeURIComponent(s.replace(/\+/g, ' ')); } catch (e) { return s; }
  }

  /** #531: the codes in the hash-query `region` parameter (the first one if
   *  repeated), trimmed, without empty entries or repeats. null when there is
   *  no parameter or it holds no code. "All" in any case is the server's
   *  catch-all (#770): it is dropped, and a parameter holding only that
   *  gives [] (all regions). */
  function parseUrlRegions(hash) {
    var h = String(hash || '');
    var q = h.indexOf('?');
    if (q < 0) return null;
    var raw = null;
    h.slice(q + 1).split('&').some(function (pair) {
      var eq = pair.indexOf('=');
      if (decodeParam(eq < 0 ? pair : pair.slice(0, eq)) !== 'region') return false;
      raw = eq < 0 ? '' : decodeParam(pair.slice(eq + 1));
      return true;
    });
    if (raw == null) return null;
    var seen = new Set();
    var codes = [];
    var all = false;
    raw.split(',').forEach(function (c) {
      c = c.trim();
      if (c.toUpperCase() === 'ALL') all = true;
      else if (c && !seen.has(c.toUpperCase())) { seen.add(c.toUpperCase()); codes.push(c); }
    });
    return codes.length > 0 || all ? codes : null;
  }

  /** #531: after a change in the control, a `region` parameter already in the
   *  hash follows the selection (removed for all regions), with replaceState
   *  and the other parameters kept, so a stale link value cannot override the
   *  choice on Back or reload. A hash without one is left alone. */
  function syncUrlRegion() {
    if (!window.URLState || !window.location || !window.history) return;
    var hash = window.location.hash;
    if (!Object.prototype.hasOwnProperty.call(URLState.parseHash(hash).params, 'region')) return;
    var next = URLState.updateHashParams({ region: getRegionParam() }, hash);
    if (next === hash) return;
    try { window.history.replaceState(window.history.state, '', next); } catch (e) { /* Safari throttles replaceState */ }
  }

  /**
   * #531: make a link's ?region= the selection, saved like a click. The
   * router calls this after the old page's destroy() and before the new
   * page's init(), so the new page's first region-dependent fetch (Live's
   * early node fetch included) already carries it. No parameter, or one with
   * no code, leaves the saved selection; "All" selects all regions. Once
   * regions are loaded unknown codes are dropped, and none left means all
   * regions; before that, fetchRegions() drops them. Listeners hear a genuine
   * change once. Returns whether the selection changed.
   * #556: an entry whose link was applied before a later choice in the
   * control keeps that choice on Back/Forward or reload. A new entry (a
   * link, a first load) has no record, so its link still wins.
   */
  function syncFromUrl(hash) {
    var codes = parseUrlRegions(hash);
    if (!codes) return false;
    var st = historyState();
    if (st && typeof st[LINK_STATE_KEY] === 'number' && st[LINK_STATE_KEY] < _choices) return false;
    markLinkApplied();
    var next = codes.length === 0 ? null : _loaded ? knownCodes(codes) : new Set(codes);
    _urlBeforeLoad = !_loaded;
    if (sameSelection(_selected, next)) return false;
    _selected = next;
    saveToStorage();
    notify();
    return true;
  }

  /** Get selected regions as array, or null if all */
  function getSelected() {
    if (!_selected || _selected.size === 0) return null;
    return Array.from(_selected);
  }

  /** Get region query param string for API calls: "SJC,SFO" or empty */
  function getRegionParam() {
    var sel = getSelected();
    return sel ? sel.join(',') : '';
  }

  /** Build query string fragment: "&region=SJC,SFO" or "" */
  function regionQueryString() {
    var p = getRegionParam();
    return p ? '&region=' + encodeURIComponent(p) : '';
  }

  /** Handle a region toggle (shared logic for both pill and dropdown modes) */
  function toggleRegion(region, codes, container) {
    if (region === '__all__') {
      _selected = null;
    } else {
      if (!_selected) {
        _selected = new Set([region]);
      } else if (_selected.has(region)) {
        _selected.delete(region);
        if (_selected.size === 0) _selected = null;
      } else {
        _selected.add(region);
      }
      if (_selected && _selected.size === codes.length) _selected = null;
    }
    noteChoice();
    saveToStorage();
    render(container);
    syncUrlRegion();
    notify();
  }

  // #333: region codes include observer IATA values from MQTT, so every code
  // and label is escaped for its attribute or text slot. dataset.region hands
  // toggleRegion() the decoded code, so selection still matches _regions.

  /** Build summary label for dropdown trigger (HTML-escaped) */
  function dropdownLabel(codes) {
    if (!_selected) return 'All Regions';
    var sel = Array.from(_selected);
    if (sel.length === 0) return 'All Regions';
    if (sel.length <= 2) return escapeHtml(sel.join(', '));
    return sel.length + ' Regions';
  }

  /** Render pill bar mode (≤4 regions) */
  function renderPills(container, codes) {
    var allSelected = !_selected;
    var html = '<div class="region-filter-bar" role="group" aria-label="Region filter">';
    html += '<span class="region-filter-label" id="region-filter-label">Region:</span>';
    html += '<button class="region-pill' + (allSelected ? ' region-pill-active' : '') +
      '" data-region="__all__" role="checkbox" aria-checked="' + allSelected + '">All</button>';
    codes.forEach(function (code) {
      var label = _regions[code] || code;
      var active = allSelected || (_selected && _selected.has(code));
      html += '<button class="region-pill' + (active ? ' region-pill-active' : '') +
        '" data-region="' + escapeHtml(code) + '" role="checkbox" aria-checked="' + !!active + '">' + escapeHtml(label) + '</button>';
    });
    html += '</div>';
    container.innerHTML = html;

    container.onclick = function (e) {
      var btn = e.target.closest('[data-region]');
      if (!btn) return;
      toggleRegion(btn.dataset.region, codes, container);
    };
  }

  /** Render dropdown mode (>4 regions) */
  function renderDropdown(container, codes) {
    var allSelected = !_selected;
    var html = '<div class="region-dropdown-wrap" role="group" aria-label="Region filter">';
    html += '<button class="region-dropdown-trigger" aria-haspopup="listbox" aria-expanded="false">' +
      dropdownLabel(codes) + ' ▾</button>';
    html += '<div class="region-dropdown-menu" role="listbox" aria-label="Select regions" hidden>';
    html += '<label class="region-dropdown-item"><input type="checkbox" data-region="__all__"' +
      (allSelected ? ' checked' : '') + '> <strong>All</strong></label>';
    codes.forEach(function (code) {
      var configLabel = _regions[code];
      var cityName = configLabel || (window.IATA_CITIES && window.IATA_CITIES[code]);
      var label = cityName ? (code + ' - ' + cityName) : code;
      var active = allSelected || (_selected && _selected.has(code));
      html += '<label class="region-dropdown-item"><input type="checkbox" data-region="' + escapeHtml(code) + '"' +
        (active ? ' checked' : '') + '> ' + escapeHtml(label) + '</label>';
    });
    html += '</div></div>';
    container.innerHTML = html;

    var trigger = container.querySelector('.region-dropdown-trigger');
    var menu = container.querySelector('.region-dropdown-menu');

    trigger.onclick = function () {
      var open = !menu.hidden;
      menu.hidden = open;
      trigger.setAttribute('aria-expanded', String(!open));
    };

    menu.onchange = function (e) {
      var input = e.target;
      if (!input.dataset.region) return;
      toggleRegion(input.dataset.region, codes, container);
    };

    // Close on outside click
    function onDocClick(e) {
      if (!container.contains(e.target)) {
        menu.hidden = true;
        trigger.setAttribute('aria-expanded', 'false');
      }
    }
    document.addEventListener('click', onDocClick, true);
    container._regionCleanup = function () {
      document.removeEventListener('click', onDocClick, true);
    };
  }

  /** Render the filter bar into a container element */
  function render(container) {
    // Clean up previous outside-click listener if any
    if (container._regionCleanup) { container._regionCleanup(); container._regionCleanup = null; }

    var codes = Object.keys(_regions);
    if (codes.length < 2) {
      container.innerHTML = '';
      container.style.display = 'none';
      return;
    }
    container.style.display = '';

    if (codes.length > 4 || container._forceDropdown) {
      renderDropdown(container, codes);
    } else {
      renderPills(container, codes);
    }
  }

  /** Subscribe to selection changes. Callback receives selected array or null */
  function onChange(fn) {
    _listeners.push(fn);
    return fn;
  }

  /** Unsubscribe */
  function offChange(fn) {
    _listeners = _listeners.filter(function (f) { return f !== fn; });
  }

  /** Initialize filter in a container, fetch regions, render, return promise.
   *  Options: { dropdown: true } to force dropdown mode regardless of region count */
  async function initFilter(container, opts) {
    _container = container;
    if (opts && opts.dropdown) container._forceDropdown = true;
    await fetchRegions();
    render(container);
  }

  /** Override selected regions (e.g. from URL param). Persists to localStorage and re-renders. */
  function setSelected(codesArray) {
    _selected = (codesArray && codesArray.length > 0) ? new Set(codesArray) : null;
    noteChoice();
    saveToStorage();
    if (_container) render(_container);
  }

  /**
   * #1108 — "Show all nodes (faded)" toggle.
   *
   * When a region is selected, the default behavior (showAll = false) is to
   * HIDE non-region nodes on the map: the operator is looking at a region for
   * a reason, and far-away nodes are visual noise. When the toggle is ON
   * (showAll = true), legacy behavior is restored — all nodes load, region
   * scoping only applies to packet feeds / metrics.
   *
   * State persists across reloads in localStorage. Default: false (hide).
   */
  var SHOW_ALL_KEY = 'mc-region-show-all-nodes';
  var _showAllListeners = [];
  function showAllGet() {
    try { return localStorage.getItem(SHOW_ALL_KEY) === 'true'; }
    catch (e) { return false; }
  }
  function showAllSet(v) {
    var bool = !!v;
    try {
      if (bool) localStorage.setItem(SHOW_ALL_KEY, 'true');
      else localStorage.removeItem(SHOW_ALL_KEY);
    } catch (e) { /* ignore */ }
    _showAllListeners.forEach(function (fn) { fn(bool); });
  }
  function showAllOnChange(fn) { _showAllListeners.push(fn); return fn; }
  function showAllOffChange(fn) {
    _showAllListeners = _showAllListeners.filter(function (f) { return f !== fn; });
  }

  /**
   * Build a node-list query fragment that respects the "show all nodes" toggle.
   * Returns "&region=SJC,SFO" only when a region is selected AND showAll is
   * OFF; otherwise empty string. Use this for /api/nodes? requests on map
   * surfaces where the operator expects the visible markers to follow the
   * region selector. Other surfaces (packets, metrics) should keep using
   * regionQueryString() which is unconditional.
   */
  function nodesRegionQueryString() {
    if (showAllGet()) return '';
    return regionQueryString();
  }

  // Expose globally
  window.RegionFilter = {
    init: initFilter,
    render: render,
    getSelected: getSelected,
    getRegionParam: getRegionParam,
    regionQueryString: regionQueryString,
    nodesRegionQueryString: nodesRegionQueryString,
    onChange: onChange,
    offChange: offChange,
    fetchRegions: fetchRegions,
    setSelected: setSelected,
    syncFromUrl: syncFromUrl
  };
  window.RegionShowAll = {
    get: showAllGet,
    set: showAllSet,
    onChange: showAllOnChange,
    offChange: showAllOffChange,
    STORAGE_KEY: SHOW_ALL_KEY
  };
})();
