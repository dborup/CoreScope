/* === CoreScope — region-filter.js (shared region filter component) === */
'use strict';

(function () {
  var LS_KEY = 'meshcore-region-filter';
  var _regions = {};       // { code: label }
  var _groups = [];        // #416: [{ name, codes[] }] quick picks, [] = none
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

  /** Fetch regions from server.
   *  #416: the optional quick-pick groups come from their own endpoint,
   *  asked for in parallel so the groups cost no extra round trip, and with
   *  its own failure path — a deployment whose server predates the endpoint,
   *  or an operator who configured no groups, must get exactly the control
   *  this component rendered before. */
  async function fetchRegions() {
    if (_loaded) return _regions;
    var groupsReq = fetch('/api/config/regions/groups')
      .then(function (r) { return r.json(); })
      .catch(function () { return []; });
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
    // No try/catch here on purpose: a failed request, a non-JSON body and a
    // body that is not a list of groups are all turned into [] by the
    // request's own catch and by normalizeGroups, so a swallow-everything
    // wrapper would only hide a real defect in one of them.
    _groups = normalizeGroups(await groupsReq);
    return _regions;
  }

  /** Region codes keyed by their upper-case form: codes match
   *  case-insensitively, as the server does. */
  function regionsByUpper() {
    var byUpper = {};
    Object.keys(_regions).forEach(function (c) { byUpper[String(c).toUpperCase()] = c; });
    return byUpper;
  }

  /** The configured codes among `codes`, in their configured spelling (codes
   *  match case-insensitively, as the server does). null when none is. */
  function knownCodes(codes) {
    var byUpper = regionsByUpper();
    var kept = new Set();
    codes.forEach(function (c) {
      var known = byUpper[String(c).toUpperCase()];
      if (known) kept.add(known);
    });
    return kept.size > 0 ? kept : null;
  }

  /**
   * #416: the groups the control will offer, from the server's answer.
   *
   * The server already drops malformed entries and codes no observer
   * reports, so this is a guard rather than a second implementation of the
   * rules: a response that is not a list of {name, codes[]} is ignored, and
   * every code is matched against the region list this component just
   * loaded. That last step is what makes the two parallel responses safe to
   * combine — a code can disappear between them (the region list is partly
   * the set of observer IATAs in the database), and a quick pick for a code
   * no pill offers would be a filter the user cannot undo by clicking.
   *
   * Codes take the spelling the region list uses, so a group's selection
   * compares equal to one assembled by clicking pills. A group left with no
   * code is dropped, not rendered as a button that does nothing.
   */
  function normalizeGroups(list) {
    if (!Array.isArray(list)) return [];
    var byUpper = regionsByUpper();
    var out = [];
    list.forEach(function (g) {
      if (!g || typeof g.name !== 'string' || !Array.isArray(g.codes)) return;
      var seen = {};
      var codes = [];
      g.codes.forEach(function (c) {
        var known = byUpper[String(c).trim().toUpperCase()];
        if (!known || seen[known]) return;
        seen[known] = true;
        codes.push(known);
      });
      if (codes.length > 0) out.push({ name: g.name, codes: codes });
    });
    return out;
  }

  /** #416: the groups on offer, as a copy callers cannot mutate. */
  function getGroups() {
    return _groups.map(function (g) { return { name: g.name, codes: g.codes.slice() }; });
  }

  /** Two selections (Set or null for all regions) hold the same regions. */
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

  /**
   * #416: the one place a new selection is saved, rendered, put in the URL
   * and announced, so a quick pick and a click on a pill travel the same
   * path and a page refetches once either way. #531's syncUrlRegion runs
   * here, before listeners hear of the change, which is what gives the
   * quick picks URL state with no code of their own.
   *
   * An unchanged selection re-renders (the dropdown's own checkboxes have
   * already toggled themselves, so the markup has to be put back) but saves
   * nothing, touches no URL and tells nobody: picking the group already
   * shown, or clearing a filter that is already clear, must not make every
   * listening page fetch again. Returns whether the selection changed.
   */
  function applySelection(next, container) {
    // #556: every user choice counts, changed or not, so an older entry's
    // link cannot override it on Back.
    noteChoice();
    if (sameSelection(_selected, next)) {
      render(container);
      return false;
    }
    _selected = next;
    saveToStorage();
    render(container);
    syncUrlRegion();
    notify();
    return true;
  }

  /** #416: what picking `group` selects — its codes, or all regions (null)
   *  when they are every code there is, the same normalization a pill click
   *  applies so the two cannot disagree about what "everything" means. */
  function groupSelection(group) {
    if (group.codes.length >= Object.keys(_regions).length) return null;
    return new Set(group.codes);
  }

  /** #416: select the codes of the group at `index`. An index that names no
   *  group changes nothing. */
  function pickGroup(index, container) {
    var group = _groups[index];
    if (!group) return;
    applySelection(groupSelection(group), container);
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
    var next;
    if (region === '__all__') {
      next = null;
    } else if (!_selected) {
      next = new Set([region]);
    } else {
      next = new Set(_selected);
      if (next.has(region)) {
        next.delete(region);
        if (next.size === 0) next = null;
      } else {
        next.add(region);
      }
      if (next && next.size === codes.length) next = null;
    }
    applySelection(next, container);
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

  /** Pill bar mode markup (≤4 regions) */
  function pillsHtml(codes) {
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
    return html;
  }

  /** Dropdown mode markup (>4 regions) */
  function dropdownHtml(codes) {
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
    return html;
  }

  /** #620: keep an open dropdown menu inside the viewport. The menu is
   *  anchored to its trigger's left edge in CSS; when that would push it past
   *  the right edge (trigger in the right half, or a narrow phone) shift it
   *  left by exactly the overflow, never past the left gutter. Shared by the
   *  region and area filters so every host page behaves the same. */
  function placeMenu(menu) {
    if (!menu || !menu.parentNode || !menu.getBoundingClientRect) return;
    var wrap = menu.parentNode;
    menu.style.left = '';
    var vw = document.documentElement.clientWidth;
    var gutter = 8;
    var w = wrap.getBoundingClientRect();
    var m = menu.getBoundingClientRect();
    var left = Math.min(w.left, vw - gutter - m.width);
    left = Math.max(left, gutter);
    menu.style.left = (left - w.left) + 'px';
  }

  /** Wire the dropdown control after its markup is in the container. */
  function wireDropdown(container, codes) {
    var trigger = container.querySelector('.region-dropdown-trigger');
    var menu = container.querySelector('.region-dropdown-menu');

    trigger.onclick = function () {
      var open = !menu.hidden;
      menu.hidden = open;
      trigger.setAttribute('aria-expanded', String(!open));
      if (!open) placeMenu(menu);
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

  /** #416: quick-pick row markup, empty when no group is on offer. The codes
   *  go in the title so the label can stay short without hiding what it
   *  selects; both slots are escaped (#333) because a group name is operator
   *  text and the codes are observer IATA values from MQTT. */
  function quickPicksHtml() {
    if (_groups.length === 0) return '';
    var html = '<div class="region-quick-picks" role="group" aria-label="Region quick picks">';
    _groups.forEach(function (g, i) {
      var active = sameSelection(_selected, groupSelection(g));
      html += '<button type="button" class="region-quick-pick' + (active ? ' region-quick-pick-active' : '') +
        '" data-region-group="' + i + '" aria-pressed="' + active +
        '" title="' + escapeHtml(g.codes.join(', ')) + '">' + escapeHtml(g.name) + '</button>';
    });
    return html + '</div>';
  }

  /** #416: one delegated handler for the whole container, so the quick picks
   *  work in both modes without a second listener. In dropdown mode
   *  [data-region] belongs to the menu's own change handler — handling the
   *  bubbled click here too would toggle the region twice. */
  function wireClicks(container, codes, pillMode) {
    container.onclick = function (e) {
      var pick = e.target.closest('[data-region-group]');
      var index = pick && pick.dataset ? pick.dataset.regionGroup : null;
      if (index != null && /^[0-9]+$/.test(String(index))) {
        pickGroup(Number(index), container);
        return;
      }
      if (!pillMode) return;
      var btn = e.target.closest('[data-region]');
      if (!btn) return;
      toggleRegion(btn.dataset.region, codes, container);
    };
  }

  /** #416: the container only wraps onto a second line while it has a
   *  quick-pick row to wrap (public/style.css). */
  function setQuickPickClass(container, on) {
    if (!container.classList) return;
    if (on) container.classList.add('has-region-quick-picks');
    else container.classList.remove('has-region-quick-picks');
  }

  /** Render the filter bar into a container element */
  function render(container) {
    // Clean up previous outside-click listener if any
    if (container._regionCleanup) { container._regionCleanup(); container._regionCleanup = null; }

    var codes = Object.keys(_regions);
    if (codes.length < 2) {
      container.innerHTML = '';
      container.style.display = 'none';
      setQuickPickClass(container, false);
      return;
    }
    container.style.display = '';

    var dropdown = codes.length > 4 || container._forceDropdown;
    container.innerHTML = (dropdown ? dropdownHtml(codes) : pillsHtml(codes)) + quickPicksHtml();
    if (dropdown) wireDropdown(container, codes);
    wireClicks(container, codes, !dropdown);
    setQuickPickClass(container, _groups.length > 0);
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
    syncFromUrl: syncFromUrl,
    getGroups: getGroups,
    placeMenu: placeMenu
  };
  window.RegionShowAll = {
    get: showAllGet,
    set: showAllSet,
    onChange: showAllOnChange,
    offChange: showAllOffChange,
    STORAGE_KEY: SHOW_ALL_KEY
  };
})();
