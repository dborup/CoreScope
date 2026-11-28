/* === CoreScope — area-filter.js (single-select area filter component) === */
'use strict';

(function () {
  var LS_KEY = 'meshcore-area-filter';
  var _areas = [];       // [{key, label}, ...]
  var _selected = null;  // selected area key string, or null = all
  var _listeners = [];
  var _container = null;
  var _loaded = false;

  function loadFromStorage() {
    try {
      var v = localStorage.getItem(LS_KEY);
      if (v) return v;
    } catch (e) {}
    return null;
  }

  function saveToStorage() {
    if (!_selected) {
      localStorage.removeItem(LS_KEY);
    } else {
      localStorage.setItem(LS_KEY, _selected);
    }
  }

  _selected = loadFromStorage();

  async function fetchAreas() {
    if (_loaded) return _areas;
    try {
      var data = await fetch('/api/config/areas').then(function (r) { return r.json(); });
      _areas = Array.isArray(data) ? data : [];
      _loaded = true;
      if (_selected && !_areas.some(function (a) { return a.key === _selected; })) {
        _selected = null;
        saveToStorage();
      }
    } catch (e) {
      _areas = [];
    }
    return _areas;
  }

  function getSelected() { return _selected; }
  /** The selected area's label (its key until the areas load), or '' for all. */
  function getSelectedLabel() {
    if (!_selected) return '';
    var area = _areas.find(function (a) { return a.key === _selected; });
    return area ? area.label : _selected;
  }
  function getAreaParam() { return _selected || ''; }
  function areaQueryString() { return _selected ? '&area=' + encodeURIComponent(_selected) : ''; }

  function triggerLabel() {
    if (!_selected) return 'Area: All ▾';
    var area = _areas.find(function (a) { return a.key === _selected; });
    return 'Area: ' + (area ? area.label : _selected) + ' ▾';
  }

  function render(container) {
    if (_areas.length === 0) {
      container.innerHTML = '';
      container.style.display = 'none';
      return;
    }
    container.style.display = '';

    if (container._areaCleanup) { container._areaCleanup(); container._areaCleanup = null; }

    // #989: labels and keys come from config — build with DOM APIs, never HTML strings.
    function el(tag, className, attrs) {
      var node = document.createElement(tag);
      node.className = className;
      Object.keys(attrs || {}).forEach(function (k) { node.setAttribute(k, attrs[k]); });
      return node;
    }
    function item(key, text, active) {
      var btn = el('button', 'region-dropdown-item area-dropdown-item' + (active ? ' area-item-active' : ''),
        { 'data-area': key });
      btn.textContent = text;
      return btn;
    }

    var wrap = el('div', 'region-dropdown-wrap', { role: 'group', 'aria-label': 'Area filter' });
    var trigger = el('button', 'region-dropdown-trigger', { 'aria-haspopup': 'listbox', 'aria-expanded': 'false' });
    trigger.textContent = triggerLabel();
    var menu = el('div', 'region-dropdown-menu area-dropdown-menu', { role: 'listbox', 'aria-label': 'Select area' });
    menu.hidden = true;
    menu.appendChild(item('__all__', 'All', !_selected));
    _areas.forEach(function (a) { menu.appendChild(item(a.key, a.label, _selected === a.key)); });
    wrap.appendChild(trigger);
    wrap.appendChild(menu);
    container.innerHTML = '';
    container.appendChild(wrap);

    trigger.onclick = function (e) {
      e.stopPropagation();
      var open = !menu.hidden;
      menu.hidden = open;
      trigger.setAttribute('aria-expanded', String(!open));
      if (!open && window.RegionFilter) RegionFilter.placeMenu(menu);
    };

    menu.onclick = function (e) {
      var btn = e.target.closest('[data-area]');
      if (!btn) return;
      _selected = (btn.dataset.area === '__all__') ? null : btn.dataset.area;
      saveToStorage();
      render(container);
      _listeners.forEach(function (fn) { fn(_selected); });
    };

    function onDocClick(e) {
      if (!container.contains(e.target)) {
        menu.hidden = true;
        trigger.setAttribute('aria-expanded', 'false');
      }
    }
    document.addEventListener('click', onDocClick, true);
    var unwatch = window.RegionFilter ? RegionFilter.watchMenu(menu) : function () {};
    container._areaCleanup = function () {
      document.removeEventListener('click', onDocClick, true);
      unwatch();
    };
  }

  function onChange(fn) { _listeners.push(fn); return fn; }
  function offChange(fn) { _listeners = _listeners.filter(function (f) { return f !== fn; }); }

  // #883 M3: as RegionFilter.release(): the host's destroy() drops the
  // document/window listeners; init() on a new container drops the old.
  function release(container) {
    if (container && container._areaCleanup) { container._areaCleanup(); container._areaCleanup = null; }
    if (container && container === _container) _container = null;
  }

  async function initFilter(container) {
    if (_container && _container !== container) release(_container);
    _container = container;
    await fetchAreas();
    render(container);
  }

  window.AreaFilter = {
    init: initFilter,
    render: render,
    release: release,
    getSelected: getSelected,
    getSelectedLabel: getSelectedLabel,
    getAreaParam: getAreaParam,
    areaQueryString: areaQueryString,
    onChange: onChange,
    offChange: offChange,
  };
})();
