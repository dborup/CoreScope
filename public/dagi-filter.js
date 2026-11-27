/* === CoreScope — dagi-filter.js (Danish areas filter, #740 M3a) === */
'use strict';

/*
 * One single-select filter over the DAGI layers that GET /api/config/dagi
 * lists (kommune, landsdel, region, postnummer). The selection is one
 * {param, code} pair, where `param` is the layer's API filter param
 * (kommune, landsdel, dkregion, postnr).
 *
 * State: a link's param wins over the saved choice (localStorage
 * `meshcore-dagi-filter`). A code is sent to the server only after it has
 * been checked against /api/config/dagi: queryString() stays '' until then,
 * and ready() resolves once the check is done. An unknown code is dropped
 * from the selection, the storage and the URL, silently, as an unknown area
 * is (area-filter.js).
 *
 * The picker is a popover on desktop and a bottom sheet on a phone (the
 * breakpoint is CSS only). Every label is set with textContent: the
 * area-filter.js innerHTML pattern is not copied (#989).
 */
(function () {
  var LS_KEY = 'meshcore-dagi-filter';
  // Most specific first. A link with several DAGI params keeps the first
  // one found in this order and drops the others (#740 owner decision).
  // These are M2's param names; the config's layers[].param is what a code
  // is validated against.
  var PARAMS = ['postnr', 'kommune', 'landsdel', 'dkregion'];
  var LAYER_TITLES = { kommune: 'Kommune', landsdel: 'Landsdel', region: 'Region', postnummer: 'Postnr' };
  var TAB_ORDER = ['region', 'landsdel', 'kommune', 'postnummer'];
  var DEFAULT_TAB = 'kommune';
  var SEARCH_CAP = 30;           // rows shown per layer while searching
  var SEARCH_DEBOUNCE_MS = 100;
  var SHEET_QUERY = '(max-width: 600px)'; // keep in step with style.css
  var BESIDE_GAP = 8;            // px between a `beside` popover and its anchor

  var _sel = null;       // {param, code}, or null for all of Denmark
  var _valid = false;    // _sel has been checked against the config
  var _config = null;    // parseConfig() result, null until loaded or when off
  var _loading = null;   // the config request, shared by init() and ready()
  var _listeners = [];
  var _container = null;
  var _ui = null;        // the rendered control (see render())

  /* ---------- pure helpers ---------- */

  /** Lower case, æ/ø/å spelt out, other accents and punctuation dropped,
   *  so "Ærø", "aeroe" and "AERØ" compare equal, as do "Århus" and
   *  "aarhus". */
  function normalize(s) {
    var t = String(s == null ? '' : s).toLowerCase()
      .replace(/æ/g, 'ae').replace(/ø/g, 'oe').replace(/å/g, 'aa');
    if (t.normalize) t = t.normalize('NFD').replace(/[\u0300-\u036f]/g, '');
    return t.replace(/[^a-z0-9]+/g, ' ').trim();
  }

  var _collator = null;
  /** Danish collation when the browser has it, else the normalised form. */
  function compareNames(a, b) {
    if (_collator === null) {
      try {
        _collator = (typeof Intl !== 'undefined' && Intl.Collator.supportedLocalesOf(['da']).length)
          ? new Intl.Collator('da') : false;
      } catch (e) { _collator = false; }
    }
    if (_collator) return _collator.compare(a.name, b.name);
    return a.norm < b.norm ? -1 : a.norm > b.norm ? 1 : 0;
  }

  function layerTitle(layer) {
    return LAYER_TITLES[layer] || (layer.charAt(0).toUpperCase() + layer.slice(1));
  }

  /** One selectable row from a config area. */
  function makeEntry(layer, param, a) {
    var code = String(a.code);
    var name = String(a.name == null ? '' : a.name);
    var e = { layer: layer, param: param, code: code, name: name, codes: null, meta: '', norm: normalize(name) };
    if (Array.isArray(a.codes) && a.codes.length > 1) {
      // Street postcodes: one row per group (M2's `codes`), never its members.
      e.codes = a.codes.map(String).sort();
      e.label = e.codes[0] + '–' + e.codes[e.codes.length - 1] + ' ' + name;
      e.meta = e.codes.length + ' street postcodes';
    } else if (layer === 'postnummer') {
      e.label = code + ' ' + name;
    } else if (a.outsideKommune) {
      e.label = name + ' (outside the kommune division)';
    } else {
      e.label = name;
    }
    return e;
  }

  /** The config in the shape the control uses, or null when DAGI is off or
   *  the body is not a config. */
  function parseConfig(data) {
    if (!data || data.enabled !== true || !Array.isArray(data.layers)) return null;
    var layers = [];
    data.layers.forEach(function (l) {
      if (!l || typeof l.layer !== 'string' || typeof l.param !== 'string' || !Array.isArray(l.areas)) return;
      var entries = l.areas.filter(function (a) { return a && a.code != null; })
        .map(function (a) { return makeEntry(l.layer, l.param, a); });
      if (l.layer === 'postnummer') entries.sort(function (a, b) { return a.code < b.code ? -1 : a.code > b.code ? 1 : 0; });
      else entries.sort(compareNames);
      layers.push({ layer: l.layer, param: l.param, title: layerTitle(l.layer), entries: entries });
    });
    layers.sort(function (a, b) {
      var ia = TAB_ORDER.indexOf(a.layer), ib = TAB_ORDER.indexOf(b.layer);
      return (ia < 0 ? 99 : ia) - (ib < 0 ? 99 : ib);
    });
    var source = data.source && typeof data.source === 'object' ? data.source : {};
    return { credit: typeof data.credit === 'string' ? data.credit : '', source: source, layers: layers };
  }

  /** The entry a {param, code} selects (a street-postcode member selects
   *  its group), or null. */
  function findEntry(sel) {
    if (!sel || !_config) return null;
    for (var i = 0; i < _config.layers.length; i++) {
      var l = _config.layers[i];
      if (l.param !== sel.param) continue;
      for (var j = 0; j < l.entries.length; j++) {
        var e = l.entries[j];
        if (e.code === sel.code || (e.codes && e.codes.indexOf(sel.code) >= 0)) return e;
      }
    }
    return null;
  }

  /** Whether `e` matches the normalised query `q`: a word start in the name,
   *  or a code prefix (a group matches on any member). Returns null for no
   *  match, else the note to show ('' or "includes 1051"). */
  function matchEntry(e, q) {
    if (!q) return '';
    if ((' ' + e.norm).indexOf(' ' + q) >= 0) return '';
    if (!/^[0-9]+$/.test(q)) return null;
    if (e.code.indexOf(q) === 0) return '';
    if (!e.codes) return null;
    for (var i = 0; i < e.codes.length; i++) {
      if (e.codes[i].indexOf(q) === 0) return e.codes[i] === q ? 'includes ' + q : '';
    }
    return null;
  }

  /** Search every layer: [{layer, title, hits: [{code, label, note}], more}]
   *  for the layers with a hit, at most SEARCH_CAP hits each. */
  function search(query) {
    var q = normalize(query);
    if (!q || !_config) return [];
    var out = [];
    _config.layers.forEach(function (l) {
      var hits = [], total = 0;
      l.entries.forEach(function (e) {
        var note = matchEntry(e, q);
        if (note === null) return;
        total++;
        if (hits.length < SEARCH_CAP) hits.push({ code: e.code, label: e.label, note: note });
      });
      if (total) out.push({ layer: l.layer, title: l.title, hits: hits, more: total - hits.length });
    });
    return out;
  }

  /** The Nodes notice for `n` nodes with no position. The server counts the
   *  whole node list, not only what the page's other filters keep, so the
   *  wording does not claim more (#740 owner decision). '' for none. */
  function hiddenNotice(n) {
    n = Number(n) || 0;
    if (n <= 0) return '';
    return (n === 1 ? '1 node has' : n + ' nodes have') +
      ' no position, so no Danish-area filter can include ' + (n === 1 ? 'it' : 'them') +
      '. The count covers all nodes, before the other filters.';
  }

  /** GET /api/config/dagi/polygons as Leaflet rings: every feature's
   *  rings, each [lon0, lat0, dlon, dlat, …] in quantum degrees with the
   *  closing vertex implicit, summed and swapped to [lat, lon]. The rings
   *  are one even-odd shape (holes and islands alike), so they are drawn as
   *  one polygon. bounds is [[south, west], [north, east]], null for none. */
  function decodeRings(resp) {
    var out = { rings: [], bounds: null, vertices: 0 };
    var q = resp && Number(resp.quantum);
    if (!q || !isFinite(q) || q <= 0 || !Array.isArray(resp.features)) return out;
    var s = Infinity, w = Infinity, n = -Infinity, e = -Infinity;
    resp.features.forEach(function (f) {
      if (!f || !Array.isArray(f.rings)) return;
      f.rings.forEach(function (r) {
        if (!Array.isArray(r) || r.length < 6) return;
        var ring = new Array(r.length >> 1);
        var x = 0, y = 0;
        for (var i = 0, k = 0; i + 1 < r.length; i += 2, k++) {
          x += r[i]; y += r[i + 1];
          var lat = y * q, lon = x * q;
          ring[k] = [lat, lon];
          if (lat < s) s = lat; if (lat > n) n = lat;
          if (lon < w) w = lon; if (lon > e) e = lon;
        }
        out.rings.push(ring);
        out.vertices += ring.length;
      });
    });
    if (out.vertices) out.bounds = [[s, w], [n, e]];
    return out;
  }

  /* ---------- state ---------- */

  function cleanSel(v) {
    if (!v || typeof v.param !== 'string' || PARAMS.indexOf(v.param) < 0) return null;
    if (typeof v.code !== 'string' || !v.code) return null;
    return { param: v.param, code: v.code };
  }

  function loadFromStorage() {
    try { return cleanSel(JSON.parse(localStorage.getItem(LS_KEY))); } catch (e) { return null; }
  }

  function saveToStorage() {
    try {
      if (_sel) localStorage.setItem(LS_KEY, JSON.stringify(_sel));
      else localStorage.removeItem(LS_KEY);
    } catch (e) { /* storage blocked */ }
  }

  _sel = loadFromStorage();

  function sameSel(a, b) {
    if (!a || !b) return !a && !b;
    return a.param === b.param && a.code === b.code;
  }

  function notify() {
    var s = getSelected();
    _listeners.forEach(function (fn) { fn(s); });
  }

  function decodeParam(s) {
    try { return decodeURIComponent(s.replace(/\+/g, ' ')); } catch (e) { return s; }
  }

  /** The DAGI params in a hash: {present, sel}. `present` is true when any
   *  DAGI param is there (even empty); `sel` is the most specific one with
   *  a value (the first of a repeated param), or null. */
  function parseUrl(hash) {
    var h = String(hash || '');
    var q = h.indexOf('?');
    var found = {};
    var present = false;
    if (q >= 0) {
      h.slice(q + 1).split('&').forEach(function (pair) {
        var eq = pair.indexOf('=');
        var k = decodeParam(eq < 0 ? pair : pair.slice(0, eq));
        if (PARAMS.indexOf(k) < 0) return;
        present = true;
        var v = eq < 0 ? '' : decodeParam(pair.slice(eq + 1)).trim();
        if (v && !(k in found)) found[k] = v;
      });
    }
    var sel = null;
    PARAMS.some(function (p) {
      if (!(p in found)) return false;
      sel = { param: p, code: found[p] };
      return true;
    });
    return { present: present, sel: sel };
  }

  /** Put the selection in the current hash with replaceState: its param
   *  set, every other DAGI param removed, other params kept. */
  function writeUrl() {
    if (!window.URLState || !window.location || !window.history) return;
    var hash = String(window.location.hash || '');
    if (hash.indexOf('#/') !== 0) return;
    var upd = {};
    PARAMS.forEach(function (p) { upd[p] = ''; });
    if (_sel) upd[_sel.param] = _sel.code;
    var next = window.URLState.updateHashParams(upd, hash);
    if (next === hash) return;
    try { window.history.replaceState(window.history.state, '', next); } catch (e) { /* Safari throttles replaceState */ }
  }

  /** Check the selection against the loaded config. Unknown: dropped from
   *  the selection, the storage and the URL; nothing was sent with it, so
   *  nobody is told. */
  function validate() {
    if (!_sel || !_config) { _valid = false; return; }
    if (findEntry(_sel)) { _valid = true; return; }
    _sel = null;
    _valid = false;
    saveToStorage();
    writeUrl();
  }

  /** The config, fetched once per page session. A failed request leaves
   *  the saved selection alone (unsent) and is retried on the next call;
   *  `enabled: false` is a loaded config with no layers. */
  function load() {
    if (_loading) return _loading;
    var p = fetch('/api/config/dagi')
      .then(function (r) { return r.ok ? r.json() : Promise.reject(new Error('HTTP ' + r.status)); })
      .then(function (data) {
        _config = parseConfig(data) || { credit: '', source: {}, layers: [] };
        validate();
        return _config;
      }, function () {
        if (_loading === p) _loading = null;
        return null;
      });
    _loading = p;
    return p;
  }

  /** Resolves once the selection may be sent: at once with no selection,
   *  else after the config has loaded and checked it. */
  function ready() {
    if (!_sel || _valid) return Promise.resolve();
    return load().then(function () {});
  }

  /** #531-style: a link's DAGI param is the selection, saved like a pick.
   *  The router calls this before the new page's init(). Several params:
   *  the most specific wins and the rest leave the URL. No DAGI param keeps
   *  the saved choice. Returns whether the selection changed. */
  function syncFromUrl(hash) {
    var u = parseUrl(hash);
    if (!u.present || !u.sel) return false;
    var before = _sel;
    if (!sameSel(_sel, u.sel)) {
      _sel = u.sel;
      _valid = false;
      saveToStorage();
    }
    writeUrl();
    if (_config) validate();
    if (sameSel(before, _sel)) return false;
    if (_ui) updateTrigger();
    notify();
    return true;
  }

  function getSelected() { return _sel ? { param: _sel.param, code: _sel.code } : null; }

  /** "kommune=0751" for a page's own URL builder (even before the check, so
   *  a rewrite keeps a pending link), '' for none. */
  function hashParam() {
    return _sel ? encodeURIComponent(_sel.param) + '=' + encodeURIComponent(_sel.code) : '';
  }

  /** "&kommune=0751" for an API request, only once the code is known. */
  function queryString() {
    return _sel && _valid ? '&' + hashParam() : '';
  }

  /** "layer=kommune&code=0751" for the polygon endpoint, only once the
   *  code is known; '' for none. A street-postcode member asks for its
   *  group, so the group is fetched (and cached) once. */
  function polygonQuery() {
    var e = _valid ? findEntry(_sel) : null;
    return e ? 'layer=' + encodeURIComponent(e.layer) + '&code=' + encodeURIComponent(e.code) : '';
  }

  function triggerLabel() {
    var e = findEntry(_sel);
    if (!e) return 'Denmark: All';
    return layerTitle(e.layer) + ': ' + e.label;
  }

  /** The one path a pick or a clear takes: saved, in the URL, rendered and
   *  announced once. The same selection again changes nothing. */
  function applySelection(next) {
    if (sameSel(_sel, next)) return false;
    _sel = next;
    _valid = !!next;
    saveToStorage();
    writeUrl();
    if (_ui) updateTrigger();
    notify();
    return true;
  }

  function onChange(fn) { _listeners.push(fn); return fn; }
  function offChange(fn) { _listeners = _listeners.filter(function (f) { return f !== fn; }); }

  /* ---------- the control ---------- */

  function el(tag, cls, text) {
    var n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text != null) n.textContent = text;
    return n;
  }

  function setHidden(n, v) { if (n.hidden !== v) n.hidden = v; }

  // The Phosphor "x" from the shared sprite (#1648: icons, not emoji).
  function closeIcon() {
    var ns = 'http://www.w3.org/2000/svg';
    var svg = document.createElementNS(ns, 'svg');
    svg.setAttribute('class', 'ph-icon');
    svg.setAttribute('aria-hidden', 'true');
    var use = document.createElementNS(ns, 'use');
    use.setAttribute('href', '/icons/phosphor-sprite.svg#ph-x');
    svg.appendChild(use);
    return svg;
  }

  function isSheet() {
    try { return !!(window.matchMedia && window.matchMedia(SHEET_QUERY).matches); } catch (e) { return false; }
  }
  function isTouch() {
    try { return !!(window.matchMedia && window.matchMedia('(pointer: coarse)').matches); } catch (e) { return false; }
  }

  function updateTrigger() {
    var ui = _ui;
    ui.label.textContent = triggerLabel();
    var on = !!findEntry(_sel);
    setHidden(ui.clear, !on);
    if (on) ui.wrap.classList.add('dagi-active');
    else ui.wrap.classList.remove('dagi-active');
    if (ui.panel) markSelected();
  }

  function markSelected() {
    var cur = findEntry(_sel);
    builtRows().forEach(function (r) { r.el.setAttribute('aria-selected', String(r.entry === cur)); });
    _ui.allRow.setAttribute('aria-selected', String(!cur));
  }

  /** The trigger (and a clear button when something is selected). The picker itself is
   *  built on first open. opts.beside: see init(). */
  function render(container, opts) {
    if (_ui && _ui.container === container) teardown();
    container.textContent = '';
    if (!_config || !_config.layers.length) {
      container.style.display = 'none';
      return;
    }
    container.style.display = '';
    var wrap = el('div', 'dagi-filter-wrap');
    wrap.setAttribute('role', 'group');
    wrap.setAttribute('aria-label', 'Danish area filter');
    var trigger = el('button', 'region-dropdown-trigger dagi-trigger');
    trigger.type = 'button';
    trigger.setAttribute('aria-haspopup', 'dialog');
    trigger.setAttribute('aria-expanded', 'false');
    var label = el('span', 'dagi-trigger-label');
    trigger.appendChild(label);
    trigger.appendChild(el('span', 'dagi-trigger-caret', ' ▾'));
    var clear = el('button', 'dagi-clear');
    clear.appendChild(closeIcon());
    clear.type = 'button';
    clear.setAttribute('aria-label', 'Clear the Danish area filter');
    clear.setAttribute('title', 'Clear the Danish area filter');
    wrap.appendChild(trigger);
    wrap.appendChild(clear);
    container.appendChild(wrap);

    _ui = { container: container, wrap: wrap, trigger: trigger, label: label, clear: clear, panel: null, rows: null, timer: null, offs: [],
      beside: (opts && opts.beside) || null };
    trigger.addEventListener('click', function (e) {
      if (e && e.stopPropagation) e.stopPropagation();
      if (_ui && _ui.panel && !_ui.panel.hidden) close(false); else open();
    });
    clear.addEventListener('click', function (e) {
      if (e && e.stopPropagation) e.stopPropagation();
      close(false);
      applySelection(null);
      trigger.focus();
    });
    updateTrigger();
  }

  function buildRow(entry) {
    var b = el('button', 'dagi-option');
    b.type = 'button';
    b.setAttribute('role', 'option');
    b.appendChild(el('span', 'dagi-option-label', entry ? entry.label : 'All of Denmark'));
    var meta = el('span', 'dagi-option-meta', entry ? entry.meta : '');
    setHidden(meta, !(entry && entry.meta));
    b.appendChild(meta);
    b._dagiEntry = entry; // null = all of Denmark
    return { el: b, entry: entry, meta: meta };
  }

  function buildCredit() {
    var box = el('div', 'dagi-credit');
    box.appendChild(el('span', 'dagi-credit-text', _config.credit));
    var src = _config.source || {};
    [['Licence', src.licenseUrl], ['Terms', src.termsUrl]].forEach(function (l) {
      if (typeof l[1] !== 'string' || l[1].indexOf('https://') !== 0) return;
      box.appendChild(el('span', 'dagi-credit-sep', ' · '));
      var a = el('a', 'dagi-credit-link', l[0]);
      a.href = l[1];
      a.target = '_blank';
      a.rel = 'noopener noreferrer';
      box.appendChild(a);
    });
    return box;
  }

  /** The picker: header, search, layer tabs, one section per layer, the
   *  credit. Built once per render and then only shown, hidden and
   *  filtered (≤ 720 rows), never rebuilt; a section's rows are made on
   *  first use (fillSection). */
  function buildPanel() {
    var ui = _ui;
    var backdrop = el('div', 'dagi-backdrop');
    backdrop.hidden = true;
    var panel = el('div', ui.beside ? 'dagi-panel dagi-panel-beside' : 'dagi-panel');
    panel.hidden = true;
    panel.setAttribute('role', 'dialog');
    panel.setAttribute('aria-label', 'Filter by Danish area');
    panel.tabIndex = -1;

    var head = el('div', 'dagi-head');
    head.appendChild(el('span', 'dagi-title', 'Filter by Danish area'));
    var closeBtn = el('button', 'dagi-close');
    closeBtn.appendChild(closeIcon());
    closeBtn.type = 'button';
    closeBtn.setAttribute('aria-label', 'Close');
    head.appendChild(closeBtn);
    panel.appendChild(head);

    var input = el('input', 'dagi-search');
    input.type = 'search';
    input.setAttribute('placeholder', 'Search kommune, postnr, region…');
    input.setAttribute('aria-label', 'Search Danish areas by name or code');
    input.setAttribute('autocomplete', 'off');
    panel.appendChild(input);

    var tabs = el('div', 'dagi-tabs');
    tabs.setAttribute('role', 'tablist');
    panel.appendChild(tabs);

    var list = el('div', 'dagi-list');
    list.setAttribute('role', 'listbox');
    list.setAttribute('aria-label', 'Danish areas');
    var allRow = buildRow(null);
    list.appendChild(allRow.el);

    var sections = [], tabBtns = [];
    _config.layers.forEach(function (l) {
      var sec = el('div', 'dagi-section');
      sec.setAttribute('role', 'group');
      sec.setAttribute('aria-label', l.title);
      var secHead = el('div', 'dagi-section-head', l.title);
      sec.appendChild(secHead);
      var more = el('div', 'dagi-more');
      more.hidden = true;
      sec.appendChild(more);
      list.appendChild(sec);
      // Rows are made when the section is first needed (fillSection).
      sections.push({ layer: l.layer, entries: l.entries, el: sec, head: secHead, subs: [], rows: null, more: more });

      var t = el('button', 'dagi-tab');
      t.type = 'button';
      t.setAttribute('role', 'tab');
      t.appendChild(el('span', 'dagi-tab-label', l.title));
      t.appendChild(el('span', 'dagi-tab-count', String(l.entries.length)));
      t._dagiLayer = l.layer;
      tabs.appendChild(t);
      tabBtns.push(t);
    });
    var empty = el('div', 'dagi-empty', 'No Danish area matches.');
    empty.hidden = true;
    list.appendChild(empty);
    panel.appendChild(list);
    panel.appendChild(buildCredit());

    ui.wrap.appendChild(backdrop);
    ui.wrap.appendChild(panel);
    ui.backdrop = backdrop;
    ui.panel = panel;
    ui.input = input;
    ui.tabs = tabs;
    ui.list = list;
    ui.allRow = allRow.el;
    ui.sections = sections;
    ui.tabBtns = tabBtns;
    ui.empty = empty;
    ui.tab = _config.layers.some(function (l) { return l.layer === DEFAULT_TAB; }) ? DEFAULT_TAB : _config.layers[0].layer;

    closeBtn.addEventListener('click', function () { close(true); });
    backdrop.addEventListener('click', function () { close(true); });
    tabs.addEventListener('click', function (e) {
      for (var n = e.target; n && n !== tabs; n = n.parentNode) {
        if (n._dagiLayer) { ui.tab = n._dagiLayer; applyFilter(); return; }
      }
    });
    list.addEventListener('click', function (e) {
      for (var n = e.target; n && n !== list; n = n.parentNode) {
        if (n._dagiEntry !== undefined) { pickRow(n._dagiEntry); return; }
      }
    });
    input.addEventListener('input', function () {
      clearTimeout(ui.timer);
      ui.timer = setTimeout(applyFilter, SEARCH_DEBOUNCE_MS);
    });
    panel.addEventListener('keydown', onPanelKey);
    markSelected();
  }

  /** A section's rows (and Postnr's thousand headers), made on first use:
   *  the open tab on the first open, every section on the first search.
   *  The first open then makes ~100 rows instead of ~720. */
  function fillSection(s) {
    if (s.rows) return;
    var cur = findEntry(_sel);
    var lastBand = null;
    s.rows = [];
    s.entries.forEach(function (e) {
      if (e.layer === 'postnummer' && /^[0-9]{4}$/.test(e.code) && e.code.charAt(0) !== lastBand) {
        lastBand = e.code.charAt(0);
        var sub = el('div', 'dagi-subhead', lastBand + '000–' + lastBand + '999');
        s.subs.push(sub);
        s.el.insertBefore(sub, s.more);
      }
      var r = buildRow(e);
      r.section = s.el;
      r.el.setAttribute('aria-selected', String(e === cur));
      s.rows.push(r);
      s.el.insertBefore(r.el, s.more);
    });
  }

  /** Every row made so far, in list order. */
  function builtRows() {
    var out = [];
    _ui.sections.forEach(function (s) { if (s.rows) out.push.apply(out, s.rows); });
    return out;
  }

  function pickRow(entry) {
    close(true);
    applySelection(entry ? { param: entry.param, code: entry.code } : null);
  }

  /** Show the rows for the current search, or the active tab's list when
   *  the search is empty. Only `hidden` flips; nothing is rebuilt. */
  function applyFilter() {
    var ui = _ui;
    if (!ui || !ui.panel) return;
    clearTimeout(ui.timer);
    ui.timer = null;
    var q = normalize(ui.input.value);
    ui.applied = q;
    var browsing = !q;
    var hits = 0;
    setHidden(ui.tabs, !browsing);
    ui.tabBtns.forEach(function (t) {
      var on = t._dagiLayer === ui.tab;
      t.setAttribute('aria-selected', String(on));
      if (on) t.classList.add('dagi-tab-active'); else t.classList.remove('dagi-tab-active');
    });
    setHidden(ui.allRow, !browsing);
    ui.sections.forEach(function (s) {
      if (browsing) {
        var mine = s.layer === ui.tab;
        if (mine) fillSection(s);
        setHidden(s.el, !mine);
        if (!s.rows) return;
        setHidden(s.head, true);
        s.subs.forEach(function (sub) { setHidden(sub, false); });
        s.rows.forEach(function (r) { setHidden(r.el, false); setMeta(r, r.entry.meta); });
        setHidden(s.more, true);
        return;
      }
      fillSection(s);
      var shown = 0, total = 0;
      s.rows.forEach(function (r) {
        var note = matchEntry(r.entry, q);
        var show = note !== null && shown < SEARCH_CAP;
        if (note !== null) total++;
        if (show) { shown++; setMeta(r, note || r.entry.meta); }
        setHidden(r.el, !show);
      });
      hits += total;
      setHidden(s.el, total === 0);
      setHidden(s.head, false);
      s.subs.forEach(function (sub) { setHidden(sub, true); });
      setHidden(s.more, total <= shown);
      if (total > shown) s.more.textContent = '… ' + (total - shown) + ' more – keep typing';
    });
    setHidden(ui.empty, browsing || hits > 0);
  }

  function setMeta(r, text) {
    if (r.meta.textContent !== text) r.meta.textContent = text;
    setHidden(r.meta, !text);
  }

  function visibleOptions() {
    var ui = _ui;
    var out = [];
    if (!ui.allRow.hidden) out.push(ui.allRow);
    builtRows().forEach(function (r) { if (!r.el.hidden && !r.section.hidden) out.push(r.el); });
    return out;
  }

  function onPanelKey(e) {
    var ui = _ui;
    if (!ui) return;
    if (e.key === 'Escape') {
      e.preventDefault();
      close(true);
      return;
    }
    if (e.key !== 'ArrowDown' && e.key !== 'ArrowUp' && e.key !== 'Enter') return;
    // A key acts on what the box says now: typed within the debounce, or
    // filled in with no input event (autofill, some IMEs).
    if (normalize(ui.input.value) !== ui.applied) applyFilter();
    var opts = visibleOptions();
    if (e.target === ui.input) {
      if (e.key === 'Enter') {
        if (normalize(ui.input.value) && opts.length) { e.preventDefault(); pickRow(opts[0]._dagiEntry); }
        return;
      }
      if (e.key === 'ArrowDown' && opts.length) { e.preventDefault(); opts[0].focus(); }
      return;
    }
    var i = opts.indexOf(e.target);
    if (i < 0 || e.key === 'Enter') return; // Enter on an option is its own click
    e.preventDefault();
    if (e.key === 'ArrowDown' && i < opts.length - 1) opts[i + 1].focus();
    else if (e.key === 'ArrowUp') (i > 0 ? opts[i - 1] : ui.input).focus();
  }

  function place() {
    var ui = _ui;
    if (!ui || !ui.panel || ui.panel.hidden) return;
    ui.panel.style.top = '';
    if (isSheet()) ui.panel.style.left = '';
    else if (ui.beside) placeBeside(ui.panel, ui.beside, ui.trigger);
    else if (!window.RegionFilter || !window.RegionFilter.placeMenu) ui.panel.style.left = '';
    else window.RegionFilter.placeMenu(ui.panel);
  }

  /** A popover inside a scrolling panel (the Map's controls) would be
   *  clipped by it, so it is fixed (style.css) to the panel's left, level
   *  with the trigger and kept inside the viewport. */
  function placeBeside(panel, anchor, trigger) {
    var a = anchor.getBoundingClientRect();
    var m = panel.getBoundingClientRect();
    var vh = window.innerHeight || document.documentElement.clientHeight;
    var top = Math.min(trigger.getBoundingClientRect().top, vh - BESIDE_GAP - m.height);
    panel.style.left = Math.max(BESIDE_GAP, a.left - m.width - BESIDE_GAP) + 'px';
    panel.style.top = Math.max(BESIDE_GAP, top) + 'px';
  }

  function listen(target, type, fn, capture) {
    target.addEventListener(type, fn, capture);
    _ui.offs.push(function () { target.removeEventListener(type, fn, capture); });
  }

  function open() {
    var ui = _ui;
    if (!ui.panel) buildPanel();
    ui.input.value = '';
    // Open on the selection's layer, so its row is in the list.
    var cur = findEntry(_sel);
    if (cur) ui.tab = cur.layer;
    applyFilter();
    setHidden(ui.panel, false);
    setHidden(ui.backdrop, false);
    ui.wrap.classList.add('dagi-open');
    ui.trigger.setAttribute('aria-expanded', 'true');
    place();
    // The selection's row in view (Hjørring is far down the Kommune list).
    builtRows().forEach(function (r) {
      if (r.entry === cur && !r.section.hidden && r.el.scrollIntoView) r.el.scrollIntoView({ block: 'nearest' });
    });
    listen(document, 'click', function (e) {
      if (_ui && !_ui.wrap.contains(e.target)) close(false);
    }, true);
    var timer = null;
    listen(window, 'resize', function () {
      clearTimeout(timer);
      timer = setTimeout(place, 100);
    });
    _ui.offs.push(function () { clearTimeout(timer); });
    // No autofocus in the search box on a touch screen: the keyboard would
    // cover the sheet. The dialog takes focus instead.
    if (isTouch()) ui.panel.focus(); else ui.input.focus();
  }

  function close(returnFocus) {
    var ui = _ui;
    if (!ui) return;
    ui.offs.forEach(function (off) { off(); });
    ui.offs = [];
    if (!ui.panel || ui.panel.hidden) return;
    clearTimeout(ui.timer);
    ui.timer = null;
    setHidden(ui.panel, true);
    setHidden(ui.backdrop, true);
    ui.wrap.classList.remove('dagi-open');
    ui.trigger.setAttribute('aria-expanded', 'false');
    if (returnFocus) ui.trigger.focus();
  }

  function teardown() {
    close(false);
    _ui = null;
  }

  /** Drop a container's document and window listeners and the module's
   *  hold on its DOM. The host page calls it from destroy(). */
  function release(container) {
    if (_ui && _ui.container === container) teardown();
    if (container && container === _container) _container = null;
  }

  /** Renders the control into container once the config has loaded.
   *  opts.beside (optional): an element the control sits in that scrolls
   *  (the Map's controls panel); on desktop the picker then opens beside
   *  it instead of under the trigger, where the panel would clip it. */
  function initFilter(container, opts) {
    if (_container && _container !== container) release(_container);
    _container = container;
    container.style.display = 'none';
    return load().then(function () {
      if (_container === container) render(container, opts);
    });
  }

  window.DagiFilter = {
    init: initFilter,
    release: release,
    ready: ready,
    syncFromUrl: syncFromUrl,
    getSelected: getSelected,
    hashParam: hashParam,
    queryString: queryString,
    polygonQuery: polygonQuery,
    decodeRings: decodeRings,
    triggerLabel: triggerLabel,
    hiddenNotice: hiddenNotice,
    search: search,
    normalize: normalize,
    onChange: onChange,
    offChange: offChange
  };
})();
