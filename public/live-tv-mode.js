/* === CoreScope — live-tv-mode.js === */
/* #998 — Live's "TV mode" button: a small dialog that builds the kiosk link
 * (#883, #/live?kiosk=1&tour=…&every=…&theme=…&moments=0) and starts it
 * here, copies it, or shows it as a QR code for a TV or a tablet.
 *
 * It only writes the kiosk's existing URL parameters, and normalises them
 * with the kiosk's own rules: LiveKiosk.parseKioskParams for every, theme
 * and the step cap, LiveKiosk.SETTINGS.resolve for the operator preset
 * (#968, window.MC_KIOSK_CFG) that fills the dialog. Kiosk itself is not
 * touched. The link spells out every choice (tour= even when empty), so a
 * screen shows what the dialog showed whatever the preset says later.
 *
 * The areas are the Area filter's (/api/config/areas, cached by
 * area-filter.js): the dialog makes no request. Live (live.js openTvMode)
 * hands everything in through open(deps), so the logic runs in a vm
 * sandbox with a fake DOM (test-998-live-tv-mode.js).
 *
 * XSS: area keys and labels come from the operator's config; they reach
 * the page through textContent and value only. No innerHTML.
 */
'use strict';

(function () {
  var SVG_NS = 'http://www.w3.org/2000/svg';
  var QR_QUIET = 4;            // modules of white around the code (the QR spec)
  var THEME_LABELS = { dark: 'Dark', light: 'Light', auto: 'The screen’s own' };
  var seq = 0;

  // The areas the dialog offers: a key the link can carry (no comma, which
  // separates the steps, and no outer space, which the kiosk trims), once,
  // sorted by label for a stable list (the server's order is random).
  function areaList(areas) {
    if (!Array.isArray(areas)) return [];
    var seen = {};
    var out = [];
    areas.forEach(function (a) {
      if (!a || typeof a.key !== 'string' || !a.key || a.key.indexOf(',') !== -1 || a.key.trim() !== a.key) return;
      if (Object.prototype.hasOwnProperty.call(seen, a.key)) return;
      seen[a.key] = true;
      out.push({ key: a.key, label: typeof a.label === 'string' && a.label ? a.label : a.key });
    });
    return out.sort(function (x, y) { return x.label.localeCompare(y.label) || (x.key < y.key ? -1 : 1); });
  }

  // The dialog's first values: the preset as the kiosk resolves it. Its
  // tour keys are matched as the kiosk matches them (exact, then without
  // case, live-kiosk.js resolveSteps), so the boxes show the canonical key.
  function defaults(preset, areas, K) {
    var p = K.SETTINGS.resolve(preset);
    var list = areaList(areas);
    var byKey = {}, byLower = {};
    list.forEach(function (a) {
      byKey[a.key] = a.key;
      var low = a.key.toLowerCase();
      if (!byLower[low] || a.key < byLower[low]) byLower[low] = a.key;
    });
    var tour = [];
    p.tour.forEach(function (k) {
      var key = Object.prototype.hasOwnProperty.call(byKey, k) ? byKey[k] : byLower[k.toLowerCase()];
      if (key && tour.indexOf(key) === -1) tour.push(key);
    });
    return { tour: tour.slice(0, K.MAX_STEPS), every: p.everySeconds, theme: p.theme, moments: true };
  }

  // The parameters the kiosk will use for a choice: the known keys in the
  // given order, then the kiosk's own parser (bounds, theme, step cap).
  function resolveChoice(choice, areas, K, preset) {
    var c = choice || {};
    var known = {};
    areaList(areas).forEach(function (a) { known[a.key] = true; });
    var keys = [];
    (Array.isArray(c.tour) ? c.tour : []).forEach(function (k) {
      if (typeof k === 'string' && Object.prototype.hasOwnProperty.call(known, k) && keys.indexOf(k) === -1) keys.push(k);
    });
    var raw = { kiosk: '1', tour: keys.join(','), every: c.every == null ? null : String(c.every), theme: c.theme == null ? null : String(c.theme) };
    var p = K.parseKioskParams({ get: function (k) { return Object.prototype.hasOwnProperty.call(raw, k) ? raw[k] : null; } }, preset);
    return { tour: p.tour, every: p.every, theme: p.theme, moments: c.moments !== false };
  }

  function buildHash(choice, areas, K, preset) {
    var r = resolveChoice(choice, areas, K, preset);
    return '#/live?kiosk=1&tour=' + r.tour.map(encodeURIComponent).join(',') + '&every=' + r.every +
      '&theme=' + r.theme + (r.moments ? '' : '&moments=0');
  }

  // The link for another browser: this page's origin and path (a sub-path
  // install keeps its base) and the hash; the query (?r= busters) is not.
  function linkUrl(loc, hash) {
    return String(loc.origin) + String(loc.pathname || '/') + hash;
  }

  // The dark modules of a QR code as one SVG path, with the quiet zone.
  function qrPath(qr) {
    var n = qr.getModuleCount();
    var d = '';
    for (var r = 0; r < n; r++) {
      for (var c = 0; c < n; c++) if (qr.isDark(r, c)) d += 'M' + (c + QR_QUIET) + ' ' + (r + QR_QUIET) + 'h1v1h-1z';
    }
    return { size: n + 2 * QR_QUIET, d: d };
  }

  /* deps:
   *   doc, root        document, and the element the dialog is appended to
   *   K                window.LiveKiosk (the rules)
   *   areas            [{key,label}] (AreaFilter.list(), the cached config)
   *   preset           window.MC_KIOSK_CFG or null
   *   momentsAvailable the server has Moments (window.MC_MOMENTS_CFG)
   *   location         { origin, pathname } for the copied link
   *   qrcode           the vendored qrcode(type, level) factory, or null
   *   copy(text, ok, fail)  window.copyToClipboard
   *   navigate(hash)   start the link in this tab
   *   returnFocus      element focused again on close (the button)
   *   onClose()
   * Returns { el, close(), state(), hash() }. */
  function open(deps) {
    var doc = deps.doc, K = deps.K;
    var areas = areaList(deps.areas);
    var first = defaults(deps.preset, areas, K);
    var order = first.tour.slice();
    var id = 'liveTv' + (++seq);
    var closed = false;
    var qrShown = false;
    var focusables = [];

    function el(tag, cls, text) {
      var n = doc.createElement(tag);
      if (cls) n.className = cls;
      if (text != null) n.textContent = text;
      return n;
    }
    function button(cls, text) {
      var b = el('button', cls, text);
      b.type = 'button';
      focusables.push(b);
      return b;
    }

    var overlay = el('div', 'live-tv-overlay');
    overlay.setAttribute('role', 'dialog');
    overlay.setAttribute('aria-modal', 'true');
    overlay.setAttribute('data-live-tv-dialog', '');
    var panel = el('div', 'live-tv-panel');
    overlay.appendChild(panel);

    var head = el('div', 'live-tv-head');
    var title = el('h2', 'live-tv-title', 'TV mode');
    title.id = id + 'Title';
    overlay.setAttribute('aria-labelledby', title.id);
    var closeBtn = button('live-tv-close', '×');
    closeBtn.setAttribute('aria-label', 'Close');
    head.appendChild(title);
    head.appendChild(closeBtn);
    panel.appendChild(head);
    panel.appendChild(el('p', 'live-tv-intro',
      'A link for a TV or wall screen: Live in full screen with big numbers, touring the areas you pick.'));

    // The tour: one box per area; the tick order is the tour order.
    var tourSet = el('fieldset', 'live-tv-tour');
    tourSet.appendChild(el('legend', 'live-tv-legend', 'Tour'));
    var rows = [];
    if (areas.length) {
      tourSet.appendChild(el('p', 'live-tv-hint', 'Tick the areas in the order to show them. None: one fixed view.'));
      var listEl = el('ul', 'live-tv-areas');
      areas.forEach(function (a) {
        var li = el('li', 'live-tv-area');
        var lab = el('label', 'live-tv-area-row');
        var box = el('input', 'live-tv-area-input');
        box.type = 'checkbox';
        box.setAttribute('data-area', a.key);
        box.checked = order.indexOf(a.key) !== -1;
        focusables.push(box);
        var name = el('span', 'live-tv-area-label', a.label);
        var num = el('span', 'live-tv-order');
        num.setAttribute('aria-hidden', 'true');
        lab.appendChild(box);
        lab.appendChild(name);
        lab.appendChild(num);
        li.appendChild(lab);
        listEl.appendChild(li);
        box.addEventListener('change', function () {
          var at = order.indexOf(a.key);
          if (box.checked && at === -1 && order.length < K.MAX_STEPS) order.push(a.key);
          else if (!box.checked && at !== -1) order.splice(at, 1);
          box.checked = order.indexOf(a.key) !== -1;
          update();
        });
        rows.push({ key: a.key, box: box, num: num });
      });
      tourSet.appendChild(listEl);
    } else {
      tourSet.appendChild(el('p', 'live-tv-hint', 'No areas are configured on this server, so the screen shows one fixed view.'));
    }
    panel.appendChild(tourSet);

    var everyField = el('div', 'live-tv-field');
    var everyLabel = el('label', null, 'Seconds per area');
    everyLabel.setAttribute('for', id + 'Every');
    var every = el('input', 'live-tv-every');
    every.type = 'number';
    every.id = id + 'Every';
    every.setAttribute('min', String(K.EVERY_MIN));
    every.setAttribute('max', String(K.EVERY_MAX));
    every.setAttribute('step', '1');
    every.setAttribute('inputmode', 'numeric');
    every.value = String(first.every);
    focusables.push(every);
    everyField.appendChild(everyLabel);
    everyField.appendChild(every);
    everyField.appendChild(el('div', 'live-tv-hint', K.EVERY_MIN + '–' + K.EVERY_MAX + ' s, used when the tour has two or more areas.'));
    panel.appendChild(everyField);

    var themeField = el('div', 'live-tv-field');
    var themeLabel = el('label', null, 'Theme');
    themeLabel.setAttribute('for', id + 'Theme');
    var theme = el('select', 'live-tv-theme');
    theme.id = id + 'Theme';
    K.THEMES.forEach(function (t) {
      var o = el('option', null, THEME_LABELS[t] || t);
      o.value = t;
      theme.appendChild(o);
    });
    theme.value = first.theme;
    focusables.push(theme);
    themeField.appendChild(themeLabel);
    themeField.appendChild(theme);
    panel.appendChild(themeField);

    var moments = null;
    if (deps.momentsAvailable) {
      var mLab = el('label', 'live-tv-check');
      moments = el('input', 'live-tv-moments');
      moments.type = 'checkbox';
      moments.checked = first.moments;
      focusables.push(moments);
      mLab.appendChild(moments);
      mLab.appendChild(el('span', null, 'Show Moments (new nodes, records)'));
      panel.appendChild(mLab);
    }

    var linkField = el('div', 'live-tv-field');
    linkField.appendChild(el('div', 'live-tv-link-label', 'Link'));
    var url = el('output', 'live-tv-url');
    linkField.appendChild(url);
    panel.appendChild(linkField);

    var actions = el('div', 'live-tv-actions');
    var start = button('live-tv-action live-tv-start', 'Start here');
    var copy = button('live-tv-action live-tv-copy', 'Copy link');
    actions.appendChild(start);
    actions.appendChild(copy);
    var qrBtn = null, qrBox = null;
    if (typeof deps.qrcode === 'function') {
      qrBtn = button('live-tv-action live-tv-qr-btn', 'QR code');
      qrBtn.setAttribute('aria-expanded', 'false');
      actions.appendChild(qrBtn);
    }
    panel.appendChild(actions);
    if (qrBtn) {
      qrBox = el('div', 'live-tv-qr');
      qrBox.id = id + 'Qr';
      qrBox.hidden = true;
      qrBtn.setAttribute('aria-controls', qrBox.id);
      panel.appendChild(qrBox);
    }
    var status = el('p', 'live-tv-status');
    status.setAttribute('role', 'status');
    status.setAttribute('aria-live', 'polite');
    panel.appendChild(status);

    function choice() {
      return { tour: order.slice(), every: every.value, theme: theme.value, moments: moments ? !!moments.checked : true };
    }
    function hash() { return buildHash(choice(), areas, K, deps.preset); }
    function fullUrl() { return linkUrl(deps.location, hash()); }

    function drawQr(text) {
      while (qrBox.firstChild) qrBox.removeChild(qrBox.firstChild);
      try {
        var qr = deps.qrcode(0, 'M');
        qr.addData(text);
        qr.make();
        var p = qrPath(qr);
        var svg = doc.createElementNS(SVG_NS, 'svg');
        svg.setAttribute('viewBox', '0 0 ' + p.size + ' ' + p.size);
        svg.setAttribute('role', 'img');
        svg.setAttribute('aria-label', 'QR code of the TV mode link');
        svg.setAttribute('class', 'live-tv-qr-svg');
        var path = doc.createElementNS(SVG_NS, 'path');
        path.setAttribute('d', p.d);
        svg.appendChild(path);
        qrBox.appendChild(svg);
      } catch (e) {
        qrBox.textContent = 'The link is too long for a QR code.';
      }
    }

    function update() {
      var r = resolveChoice(choice(), areas, K, deps.preset);
      rows.forEach(function (row) {
        var at = r.tour.indexOf(row.key);
        row.num.textContent = at === -1 ? '' : String(at + 1);
        row.box.disabled = at === -1 && order.length >= K.MAX_STEPS;
      });
      var text = fullUrl();
      url.textContent = text;
      if (qrShown) drawQr(text);
      status.textContent = '';
    }

    function close() {
      if (closed) return;
      closed = true;
      doc.removeEventListener('keydown', onKey);
      if (overlay.parentNode) overlay.parentNode.removeChild(overlay);
      if (deps.returnFocus && typeof deps.returnFocus.focus === 'function') {
        try { deps.returnFocus.focus(); } catch (_) { /* detached */ }
      }
      if (deps.onClose) deps.onClose();
    }

    // Escape closes; Tab stays inside the dialog.
    function onKey(e) {
      if (e.key === 'Escape') { if (e.preventDefault) e.preventDefault(); close(); return; }
      if (e.key !== 'Tab') return;
      var live = focusables.filter(function (f) { return !f.disabled && f.parentNode && !(qrBox && f === qrBox); });
      if (!live.length) return;
      var a = doc.activeElement;
      if (e.shiftKey && (a === live[0] || !overlay.contains(a))) { e.preventDefault(); live[live.length - 1].focus(); }
      else if (!e.shiftKey && (a === live[live.length - 1] || !overlay.contains(a))) { e.preventDefault(); live[0].focus(); }
    }

    every.addEventListener('input', update);
    every.addEventListener('change', function () {
      every.value = String(resolveChoice(choice(), areas, K, deps.preset).every);
      update();
    });
    theme.addEventListener('change', update);
    if (moments) moments.addEventListener('change', update);
    closeBtn.addEventListener('click', close);
    overlay.addEventListener('click', function (e) { if (e.target === overlay) close(); });
    start.addEventListener('click', function () {
      var h = hash();
      close();
      deps.navigate(h);
    });
    copy.addEventListener('click', function () {
      deps.copy(fullUrl(), function () {
        if (!closed) status.textContent = 'Link copied. Open it in the screen’s browser.';
      }, function () {
        if (!closed) status.textContent = 'Could not copy. Select the link above and copy it.';
      });
    });
    if (qrBtn) {
      qrBtn.addEventListener('click', function () {
        qrShown = !qrShown;
        qrBox.hidden = !qrShown;
        qrBtn.setAttribute('aria-expanded', String(qrShown));
        if (qrShown) drawQr(fullUrl());
        else while (qrBox.firstChild) qrBox.removeChild(qrBox.firstChild);
      });
    }
    doc.addEventListener('keydown', onKey);

    update();
    deps.root.appendChild(overlay);
    try { (rows.length ? rows[0].box : every).focus(); } catch (_) { /* not focusable */ }

    return {
      el: overlay,
      close: close,
      hash: hash,
      state: function () { return resolveChoice(choice(), areas, K, deps.preset); }
    };
  }

  window.LiveTvMode = {
    areaList: areaList,
    defaults: defaults,
    resolveChoice: resolveChoice,
    buildHash: buildHash,
    linkUrl: linkUrl,
    open: open
  };
})();
