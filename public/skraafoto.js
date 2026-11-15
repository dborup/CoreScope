/* skraafoto.js — #737 M2: the Skråfoto viewer, oblique aerial photos of a
 * map point from Klimadatastyrelsen, through this server's proxy (#737 M1,
 * cmd/server/skraafoto_proxy.go).
 *
 * Offered only while /api/config/client carries an enabled `skraafoto`
 * block (window.MC_SKRAAFOTO_CFG, set by roles.js): the server sends it
 * only when map.skraafoto.enabled is on and a token is configured, and it
 * never carries the token. Without it this module shows nothing at all.
 *
 * Entry points: a node with a fix inside the block's bounds, Denmark
 * (node page and side pane, nodeAction), and the Path profile's ends and
 * clicked chart point (terrain-profile.js, actionButton). Each opens one
 * dialog:
 *   - one search per point and year (GET searchUrl?lat=&lon=[&year=]),
 *     the newest year first, with a selector for the older ones;
 *   - a direction switcher for the five photos (N, E, S, W, nadir);
 *   - only the photo shown is fetched: no prefetch of the other four, and
 *     a photo seen before is not fetched again while the dialog is open;
 *   - the photo date, the server's privacy note, its #733 credit with the
 *     dataset and licence links, and "Full resolution" in the official
 *     viewer;
 *   - loading, error (429, 503, other) and empty states;
 *   - Escape and Close return focus to the button that opened it; Tab
 *     stays inside; Escape is taken before the page's own Escape handlers
 *     (node page "back", Map "close the profile").
 *
 * Deep link (AGENTS.md "Deep Linking"): the point, direction and year
 * ride the hash as skraafoto=<lat>,<lon>,<dir>[,<year>] next to whatever
 * the page keeps there, written with replaceState, removed on close; a
 * cold load or a hash change that carries it opens the viewer there.
 *
 * Everything shown is built with createElement + textContent; nothing
 * from the server or a node passes through an HTML string.
 *
 * Exposes window.Skraafoto. */
(function (global) {
  'use strict';

  // The five photos, in the order the server returns them (#737 Q5).
  var DIRECTIONS = ['north', 'east', 'south', 'west', 'nadir'];
  var DIR_SHORT = { north: 'N', east: 'E', south: 'S', west: 'W', nadir: 'Nadir' };
  var DIR_TEXT = {
    north: 'Looking north', east: 'Looking east', south: 'Looking south', west: 'Looking west',
    nadir: 'Straight down (nadir)'
  };
  // internal/skraafoto MinYear: 2017 is left out (#737 Q4).
  var MIN_YEAR = 2019;
  var MAX_YEAR = 2100;
  var HASH_PARAM = 'skraafoto';
  // The official viewer, the "Full resolution" link (#737 Q9).
  var VIEWER_PREFIX = 'https://skraafoto.dataforsyningen.dk/?';
  // Same-origin API paths only: a block cannot point the viewer elsewhere.
  var SEARCH_PATH_RE = /^\/api\/[A-Za-z0-9_\/-]+$/;
  // The server's own preview path of one photo (internal/skraafoto ids).
  var PREVIEW_RE = /^\/api\/skraafoto\/preview\/skraafotos20[0-9]{2}\/[0-9]{4}_[0-9A-Za-z_]{1,64}$/;
  var COORD_RE = /^-?[0-9]{1,3}(\.[0-9]{1,10})?$/;
  // Photos kept as object URLs while a dialog is open: five a year.
  var MAX_KEPT = 10;

  // ── Pure helpers ───────────────────────────────────────────────────────────

  function offered(cfg) {
    return !!(cfg && cfg.enabled === true && typeof cfg.searchUrl === 'string' && SEARCH_PATH_RE.test(cfg.searchUrl));
  }

  function round5(v) { return Math.round(v * 1e5) / 1e5; }

  /* point is a coordinate pair the search accepts, rounded to ~1 m. */
  function point(lat, lon) {
    if (lat === null || lon === null || lat === undefined || lon === undefined || lat === '' || lon === '') return null;
    var la = Number(lat), lo = Number(lon);
    if (!isFinite(la) || !isFinite(lo) || la < -90 || la > 90 || lo < -180 || lo > 180) return null;
    return { lat: round5(la), lon: round5(lo) };
  }

  /* covered: the point is inside the block's bounds, Denmark (#818), by
   * the heritage overlay's bounds test (HeritageInfo.inBounds, loaded on
   * every page). A block without bounds, an older server's, sets no
   * limit. */
  function covered(cfg, p) {
    var hi = global.HeritageInfo;
    return !hi || typeof hi.inBounds !== 'function' || hi.inBounds(cfg && cfg.bounds, p.lat, p.lon);
  }

  /* nodePoint is where a node's photos are: its own position, by the
   * node-detail "real fix" rule (TerrainProfile.hasFix), else null. */
  function nodePoint(node) {
    var tp = global.TerrainProfile;
    if (!tp || typeof tp.hasFix !== 'function' || !tp.hasFix(node)) return null;
    return point(node.lat, node.lon);
  }

  function validYear(y) {
    return typeof y === 'number' && y % 1 === 0 && y >= MIN_YEAR && y <= MAX_YEAR;
  }

  function searchURL(cfg, lat, lon, year) {
    var p = point(lat, lon);
    if (!p || !offered(cfg)) return null;
    var u = cfg.searchUrl + '?lat=' + p.lat.toFixed(5) + '&lon=' + p.lon.toFixed(5);
    return validYear(year) ? u + '&year=' + year : u;
  }

  function previewURL(photo) {
    var u = photo && photo.previewUrl;
    return typeof u === 'string' && PREVIEW_RE.test(u) ? u : null;
  }

  function viewerLink(photo) {
    var u = photo && photo.viewerUrl;
    return typeof u === 'string' && u.indexOf(VIEWER_PREFIX) === 0 && !/[\s"'<>\\]/.test(u) ? u : null;
  }

  /* orderPhotos is the answer's photos in DIRECTIONS order, one per
   * direction (the first wins), unknown directions dropped. */
  function orderPhotos(photos) {
    var by = {};
    (Array.isArray(photos) ? photos : []).forEach(function (p) {
      if (p && DIRECTIONS.indexOf(p.direction) >= 0 && !by[p.direction]) by[p.direction] = p;
    });
    return DIRECTIONS.filter(function (d) { return by[d]; }).map(function (d) { return by[d]; });
  }

  function pickDirection(ordered, wanted) {
    if (!ordered || !ordered.length) return null;
    for (var i = 0; i < ordered.length; i++) if (ordered[i].direction === wanted) return wanted;
    return ordered[0].direction;
  }

  function yearChoices(years) {
    var out = [];
    (Array.isArray(years) ? years : []).forEach(function (y) {
      if (validYear(y) && out.indexOf(y) < 0) out.push(y);
    });
    return out.sort(function (a, b) { return b - a; });
  }

  function selectedYear(resp) {
    var ys = yearChoices(resp && resp.years);
    if (!ys.length) return 0;
    return ys.indexOf(resp.year) >= 0 ? resp.year : ys[0];
  }

  function photoDate(dt) {
    var m = /^([0-9]{4}-[0-9]{2}-[0-9]{2})T/.exec(typeof dt === 'string' ? dt : '');
    return m ? m[1] : '';
  }

  /* errorText words a failed search or preview. retrySec is Retry-After. */
  function errorText(status, retrySec) {
    var wait = retrySec > 0 ? ' Try again in about ' + Math.ceil(retrySec) + ' s.' : ' Try again in a minute.';
    if (status === 429) return 'Too many photo requests from you just now.' + wait;
    if (status === 503) return 'Oblique photos are busy, or this server\'s daily allowance is used up.' + (retrySec > 0 ? wait : ' Try again later.');
    if (status === 404) return 'This photo was not found.';
    return 'Oblique photos are unavailable right now.';
  }

  function formatLink(s) {
    var p = s && point(s.lat, s.lon);
    if (!p || DIRECTIONS.indexOf(s.dir) < 0) return null;
    return p.lat.toFixed(5) + ',' + p.lon.toFixed(5) + ',' + s.dir + (validYear(s.year) ? ',' + s.year : '');
  }

  function parseLink(v) {
    if (typeof v !== 'string' || !v) return null;
    var f = v.split(',');
    if (f.length < 2 || f.length > 4 || !COORD_RE.test(f[0]) || !COORD_RE.test(f[1])) return null;
    var p = point(f[0], f[1]);
    if (!p) return null;
    var dir = null, year = 0;
    if (f.length > 2) {
      if (DIRECTIONS.indexOf(f[2]) < 0) return null;
      dir = f[2];
    }
    if (f.length > 3) {
      year = /^[0-9]{4}$/.test(f[3]) ? Number(f[3]) : NaN;
      if (!validYear(year)) return null;
    }
    return { lat: p.lat, lon: p.lon, dir: dir, year: year };
  }

  /* withParam writes (or, with value null, removes) the viewer's key in
   * a hash and keeps every other key, as withProfileParams does for the
   * profile's. */
  function withParam(hash, value) {
    hash = String(hash || '');
    var qi = hash.indexOf('?');
    var base = qi >= 0 ? hash.slice(0, qi) : hash;
    var params = new URLSearchParams(qi >= 0 ? hash.slice(qi + 1) : '');
    params.delete(HASH_PARAM);
    if (value) params.set(HASH_PARAM, value);
    var qs = params.toString();
    return qs ? (base || '#') + '?' + qs : base;
  }

  function linkFromHash(hash) {
    var h = String(hash || '');
    var qi = h.indexOf('?');
    if (qi < 0) return null;
    return parseLink(new URLSearchParams(h.slice(qi + 1)).get(HASH_PARAM));
  }

  /* creditLinks: the #733 marks, in text order (the #765 contract). */
  function creditLinks(c) {
    if (!c) return [];
    return [
      { key: 'dataset', mark: c.datasetName, href: c.datasetUrl, rel: 'noopener noreferrer' },
      { key: 'license', mark: c.licenseName, href: c.licenseUrl, rel: 'noopener noreferrer license' }
    ];
  }

  // ── DOM ────────────────────────────────────────────────────────────────────

  function el(tag, cls, text) {
    var e = global.document.createElement(tag);
    if (cls) e.className = cls;
    if (text !== undefined && text !== null) e.textContent = text;
    return e;
  }

  // Static markup, no interpolation: a Phosphor sprite icon, the camera
  // or the close cross.
  function icon(camera) {
    var s = el('span', 'sf-icon');
    s.setAttribute('aria-hidden', 'true');
    s.innerHTML = camera
      ? '<svg class="ph-icon" aria-hidden="true"><use href="/icons/phosphor-sprite.svg#ph-camera"/></svg>'
      : '<svg class="ph-icon" aria-hidden="true"><use href="/icons/phosphor-sprite.svg#ph-x"/></svg>';
    return s;
  }

  var current = null; // the open viewer, if any

  /* open shows the viewer at a point. o: { lat, lon, label, opener, cfg,
   * year, dir, fetch }. cfg defaults to the server's block; fetch is a
   * seam for tests. Returns the viewer, or null when not offered. */
  function open(o) {
    o = o || {};
    var cfg = o.cfg || global.MC_SKRAAFOTO_CFG;
    var at = point(o.lat, o.lon);
    if (!offered(cfg) || !at) return null;
    if (current) current.close(false);
    var doc = global.document;
    var fetcher = o.fetch || function (u, init) { return global.fetch(u, init); };
    var opener = o.opener || null;
    // A page may re-render while the viewer is open (the node page draws
    // its card twice), replacing the opener: then its successor, the
    // element with the same test id, gets the focus back.
    var openerId = opener && opener.getAttribute ? opener.getAttribute('data-testid') : null;
    var state = { year: validYear(o.year) ? o.year : 0, dir: DIRECTIONS.indexOf(o.dir) >= 0 ? o.dir : 'north', resp: null, photos: [] };
    var seq = 0, pseq = 0, previewAbort = null, searchAbort = null;
    var kept = []; // [{ url, objectURL }], newest last
    var shown = null; // the object URL that is the image's source
    var ui = {};
    var gone = false;

    var overlay = el('div', 'sf-overlay');
    overlay.setAttribute('data-testid', 'sf-overlay');
    var dialog = el('div', 'sf-dialog');
    dialog.setAttribute('role', 'dialog');
    dialog.setAttribute('aria-modal', 'true');
    dialog.setAttribute('aria-labelledby', 'sfTitle');
    dialog.setAttribute('data-testid', 'sf-dialog');
    dialog.setAttribute('tabindex', '-1');
    overlay.appendChild(dialog);

    var head = el('header', 'sf-head');
    var titles = el('div', 'sf-titles');
    var title = el('h2', 'sf-title', 'Oblique photo');
    title.id = 'sfTitle';
    titles.appendChild(title);
    ui.where = el('p', 'sf-where', o.label ? String(o.label) : at.lat.toFixed(5) + ', ' + at.lon.toFixed(5));
    ui.where.setAttribute('data-testid', 'sf-where');
    titles.appendChild(ui.where);
    head.appendChild(titles);
    var closeBtn = el('button', 'sf-close');
    closeBtn.type = 'button';
    closeBtn.setAttribute('aria-label', 'Close oblique photo');
    closeBtn.setAttribute('data-testid', 'sf-close');
    closeBtn.appendChild(icon(false));
    closeBtn.addEventListener('click', function () { api.close(true); });
    head.appendChild(closeBtn);
    dialog.appendChild(head);

    var controls = el('div', 'sf-controls');
    var dirs = el('div', 'sf-dirs');
    dirs.setAttribute('role', 'group');
    dirs.setAttribute('aria-label', 'Direction');
    ui.dirs = {};
    DIRECTIONS.forEach(function (d) {
      var b = el('button', 'sf-dir', DIR_SHORT[d]);
      b.type = 'button';
      b.title = DIR_TEXT[d];
      b.setAttribute('aria-label', DIR_TEXT[d]);
      b.setAttribute('aria-pressed', 'false');
      b.setAttribute('data-dir', d);
      b.setAttribute('data-testid', 'sf-dir-' + d);
      b.disabled = true;
      b.addEventListener('click', function () { choose(d); });
      ui.dirs[d] = b;
      dirs.appendChild(b);
    });
    controls.appendChild(dirs);
    var yearWrap = el('label', 'sf-year-wrap');
    yearWrap.appendChild(el('span', 'sf-year-label', 'Year'));
    ui.year = el('select', 'sf-year');
    ui.year.setAttribute('data-testid', 'sf-year');
    ui.year.disabled = true;
    ui.year.addEventListener('change', function () {
      var y = Number(ui.year.value);
      if (validYear(y) && y !== state.year) { state.year = y; search(); }
    });
    yearWrap.appendChild(ui.year);
    controls.appendChild(yearWrap);
    dialog.appendChild(controls);

    var stage = el('div', 'sf-stage');
    stage.setAttribute('data-testid', 'sf-stage');
    ui.img = el('img', 'sf-img');
    ui.img.setAttribute('data-testid', 'sf-img');
    ui.img.alt = '';
    ui.img.hidden = true;
    stage.appendChild(ui.img);
    ui.status = el('p', 'sf-status');
    ui.status.setAttribute('role', 'status');
    ui.status.setAttribute('data-testid', 'sf-status');
    stage.appendChild(ui.status);
    dialog.appendChild(stage);

    var meta = el('p', 'sf-meta');
    ui.date = el('span', 'sf-date');
    ui.date.setAttribute('data-testid', 'sf-date');
    meta.appendChild(ui.date);
    ui.full = el('a', 'sf-full', 'Full resolution');
    ui.full.setAttribute('data-testid', 'sf-full');
    ui.full.setAttribute('target', '_blank');
    ui.full.setAttribute('rel', 'noopener noreferrer');
    ui.full.hidden = true;
    meta.appendChild(ui.full);
    dialog.appendChild(meta);

    // The privacy note (#737 Q10) and the credit are the server's words,
    // from its block: no copy of either lives here (#733 guard).
    var note = el('p', 'sf-note', typeof cfg.note === 'string' ? cfg.note : '');
    note.setAttribute('data-testid', 'sf-note');
    note.hidden = !note.textContent;
    dialog.appendChild(note);
    ui.credit = el('p', 'sf-credit');
    ui.credit.setAttribute('data-testid', 'sf-credit');
    dialog.appendChild(ui.credit);
    setCredit(cfg.credit);

    overlay.addEventListener('click', function (e) { if (e.target === overlay) api.close(true); });

    function setCredit(c) {
      ui.credit.textContent = '';
      var text = c && typeof c.attribution === 'string' ? c.attribution : '';
      if (typeof global.MC_appendCredit === 'function') {
        global.MC_appendCredit(ui.credit, text, creditLinks(c), function (a, l) { a.setAttribute('data-credit', l.key); });
      } else {
        ui.credit.textContent = text;
      }
    }

    function setState(s, text) {
      dialog.setAttribute('data-state', s);
      stage.setAttribute('aria-busy', s === 'loading' ? 'true' : 'false');
      ui.status.textContent = text || '';
      ui.status.hidden = !text;
    }

    function writeHash() {
      try {
        var v = formatLink({ lat: at.lat, lon: at.lon, dir: state.dir, year: state.year });
        var loc = global.location;
        global.history.replaceState(global.history.state, '', loc.pathname + loc.search + withParam(loc.hash, gone ? null : v));
      } catch (_) { /* a hash we cannot write is not worth breaking the viewer for */ }
    }

    function syncControls() {
      var have = {};
      state.photos.forEach(function (p) { have[p.direction] = true; });
      DIRECTIONS.forEach(function (d) {
        var b = ui.dirs[d];
        b.disabled = !have[d];
        b.setAttribute('aria-pressed', d === state.dir && have[d] ? 'true' : 'false');
        b.classList.toggle('active', d === state.dir && !!have[d]);
      });
      var years = yearChoices(state.resp && state.resp.years);
      ui.year.textContent = '';
      years.forEach(function (y) {
        var opt = el('option', null, String(y));
        opt.value = String(y);
        ui.year.appendChild(opt);
      });
      ui.year.value = String(state.year);
      ui.year.disabled = years.length < 2;
    }

    function retryAfter(r) {
      var v = r && r.headers && r.headers.get ? Number(r.headers.get('Retry-After')) : 0;
      return isFinite(v) && v > 0 ? v : 0;
    }

    function search() {
      var mine = ++seq;
      pseq++;
      if (previewAbort) previewAbort.abort();
      if (searchAbort) searchAbort.abort();
      searchAbort = typeof AbortController === 'function' ? new AbortController() : null;
      ui.img.hidden = true;
      ui.full.hidden = true;
      ui.date.textContent = '';
      setState('loading', 'Looking up photos…');
      var url = searchURL(cfg, at.lat, at.lon, state.year);
      fetcher(url, searchAbort ? { signal: searchAbort.signal } : undefined).then(function (r) {
        if (!r.ok) {
          var err = new Error('status ' + r.status);
          err.status = r.status;
          err.retry = retryAfter(r);
          throw err;
        }
        return r.json();
      }).then(function (resp) {
        if (mine !== seq || gone) return;
        state.resp = resp || {};
        state.photos = orderPhotos(state.resp.photos);
        state.year = selectedYear(state.resp);
        if (state.resp.credit) setCredit(state.resp.credit);
        if (!state.photos.length) {
          syncControls();
          setState('empty', 'No oblique photos at this point.');
          writeHash();
          return;
        }
        state.dir = pickDirection(state.photos, state.dir);
        syncControls();
        writeHash();
        show();
      }).catch(function (err) {
        if (mine !== seq || gone || (err && err.name === 'AbortError')) return;
        state.photos = [];
        syncControls();
        setState('error', errorText(err && err.status ? err.status : 0, err && err.retry));
      });
    }

    function photoOf(d) {
      for (var i = 0; i < state.photos.length; i++) if (state.photos[i].direction === d) return state.photos[i];
      return null;
    }

    function keptURL(u) {
      for (var i = 0; i < kept.length; i++) if (kept[i].url === u) return kept[i].objectURL;
      return null;
    }

    function isKept(objectURL) {
      for (var i = 0; i < kept.length; i++) if (kept[i].objectURL === objectURL) return true;
      return false;
    }

    /* keep holds a fetched photo for the dialog. A copy pushed out is
     * revoked, unless it is the image's source: display() revokes that
     * one when the next photo replaces it, close() when the dialog goes. */
    function keep(u, objectURL) {
      kept.push({ url: u, objectURL: objectURL });
      while (kept.length > MAX_KEPT) {
        var old = kept.shift();
        if (old.objectURL !== shown) revoke(old.objectURL);
      }
    }

    function revoke(u) {
      try { global.URL.revokeObjectURL(u); } catch (_) {}
    }

    function display(objectURL, p) {
      ui.img.alt = 'Oblique aerial photo, ' + DIR_TEXT[p.direction].toLowerCase() + (photoDate(p.datetime) ? ', ' + photoDate(p.datetime) : '');
      ui.img.onload = function () { if (!gone) { ui.img.hidden = false; setState('ready', ''); } };
      ui.img.onerror = function () { if (!gone) { ui.img.hidden = true; setState('error', errorText(0)); } };
      var was = shown;
      shown = objectURL;
      ui.img.src = objectURL;
      if (was && was !== objectURL && !isKept(was)) revoke(was);
    }

    /* show puts the chosen direction's photo up: the one preview it
     * needs, fetched now, or the copy this dialog already holds. */
    function show() {
      var p = photoOf(state.dir);
      if (!p) return;
      var mine = ++pseq;
      if (previewAbort) previewAbort.abort();
      previewAbort = null;
      var date = photoDate(p.datetime);
      ui.date.textContent = DIR_TEXT[p.direction] + (date ? ' · photographed ' + date : '');
      var link = viewerLink(p);
      ui.full.hidden = !link;
      if (link) ui.full.setAttribute('href', link); else ui.full.removeAttribute('href');
      ui.img.hidden = true;
      var u = previewURL(p);
      if (!u) { setState('error', 'No preview of this photo here; open it in full resolution.'); return; }
      var have = keptURL(u);
      if (have) { setState('loading', 'Loading photo…'); display(have, p); return; }
      setState('loading', 'Loading photo…');
      previewAbort = typeof AbortController === 'function' ? new AbortController() : null;
      fetcher(u, previewAbort ? { signal: previewAbort.signal } : undefined).then(function (r) {
        if (!r.ok) {
          var err = new Error('status ' + r.status);
          err.status = r.status;
          err.retry = retryAfter(r);
          throw err;
        }
        return r.blob();
      }).then(function (blob) {
        if (gone) return;
        var objectURL = global.URL.createObjectURL(blob);
        keep(u, objectURL);
        if (mine !== pseq) return;
        display(objectURL, p);
      }).catch(function (err) {
        if (mine !== pseq || gone || (err && err.name === 'AbortError')) return;
        setState('error', errorText(err && err.status ? err.status : 0, err && err.retry));
      });
    }

    function choose(d) {
      if (d === state.dir && dialog.getAttribute('data-state') !== 'error') return;
      if (!photoOf(d)) return;
      state.dir = d;
      syncControls();
      writeHash();
      show();
    }

    function focusables() {
      return Array.prototype.filter.call(dialog.querySelectorAll('button, select, a[href], [tabindex]:not([tabindex="-1"])'),
        function (e) { return !e.disabled && !e.hidden && e.offsetParent !== null; });
    }

    /* Capture phase, so the page's own Escape (node page back, Map closes
     * the profile) never sees the one that closes the viewer. */
    function onKey(e) {
      if (gone) return;
      if (e.key === 'Escape' || e.key === 'Esc') {
        e.preventDefault();
        e.stopImmediatePropagation();
        api.close(true);
        return;
      }
      if (e.key !== 'Tab') return;
      var f = focusables();
      if (!f.length) { e.preventDefault(); dialog.focus(); return; }
      var first = f[0], last = f[f.length - 1], a = doc.activeElement;
      if (!dialog.contains(a)) { e.preventDefault(); first.focus(); }
      else if (e.shiftKey && (a === first || a === dialog)) { e.preventDefault(); last.focus(); }
      else if (!e.shiftKey && a === last) { e.preventDefault(); first.focus(); }
    }

    var api = {
      el: function () { return dialog; },
      state: function () { return { lat: at.lat, lon: at.lon, dir: state.dir, year: state.year }; },
      /* close(true) is the viewer's own Close or Escape: focus goes back
       * to the opener and the hash drops the viewer's key. close(false)
       * is a page change or another viewer taking over. */
      close: function (byUser) {
        if (gone) return;
        gone = true;
        seq++;
        pseq++;
        if (previewAbort) previewAbort.abort();
        if (searchAbort) searchAbort.abort();
        doc.removeEventListener('keydown', onKey, true);
        if (overlay.parentNode) overlay.parentNode.removeChild(overlay);
        ui.img.removeAttribute('src');
        if (shown && !isKept(shown)) revoke(shown);
        shown = null;
        kept.forEach(function (k) { revoke(k.objectURL); });
        kept = [];
        if (current === api) current = null;
        if (byUser) {
          writeHash();
          var back = opener && opener.isConnected ? opener
            : (openerId ? doc.querySelector('[data-testid="' + openerId + '"]') : null);
          if (back && typeof back.focus === 'function') back.focus();
        }
      }
    };

    current = api;
    doc.addEventListener('keydown', onKey, true);
    doc.body.appendChild(overlay);
    closeBtn.focus();
    search();
    return api;
  }

  /* actionButton is an "Oblique photo" button; getPoint() is asked at
   * click time for { lat, lon, label } (null: nothing to show). */
  function actionButton(o) {
    var b = el('button', o.cls || 'btn-primary');
    b.type = 'button';
    if (o.testid) b.setAttribute('data-testid', o.testid);
    if (o.title) b.title = o.title;
    if (o.icon !== false) b.appendChild(icon(true));
    b.appendChild(el('span', 'sf-action-text', o.text || 'Oblique photo'));
    b.addEventListener('click', function () {
      var p = o.getPoint ? o.getPoint() : null;
      if (!p) return;
      open({ lat: p.lat, lon: p.lon, label: p.label, opener: b, cfg: o.cfg });
    });
    return b;
  }

  /* nodeAction is the node-detail button, or null when the photos are not
   * offered, the node has no fix or its fix is outside the block's bounds
   * (there the search always answers empty). Asks nothing until it is
   * clicked. */
  function nodeAction(node, cfg) {
    var at = nodePoint(node);
    if (!offered(cfg) || !at || !covered(cfg, at)) return null;
    var name = node.name || node.advert_name || '';
    return actionButton({
      cls: 'btn-primary node-terrain-action sf-node-action', testid: 'node-skraafoto', cfg: cfg,
      title: 'Oblique aerial photos of this node\'s position, from four sides and straight down',
      getPoint: function () { return { lat: at.lat, lon: at.lon, label: name ? String(name) : null }; }
    });
  }

  /* fromHash opens, follows or closes the viewer for the hash's key. */
  function fromHash() {
    var cfg = global.MC_SKRAAFOTO_CFG;
    var link = null;
    try { link = linkFromHash(global.location && global.location.hash); } catch (_) {}
    if (!link || !offered(cfg)) { if (current) current.close(false); return; }
    if (current) {
      var s = current.state();
      if (s.lat === link.lat && s.lon === link.lon && (!link.dir || s.dir === link.dir) && (!link.year || s.year === link.year)) return;
    }
    open({ lat: link.lat, lon: link.lon, dir: link.dir, year: link.year, cfg: cfg });
  }

  if (global.addEventListener) global.addEventListener('hashchange', fromHash);
  if (global.Promise) global.Promise.resolve(global.MeshConfigReady).catch(function () {}).then(fromHash);

  global.Skraafoto = {
    DIRECTIONS: DIRECTIONS,
    MIN_YEAR: MIN_YEAR,
    HASH_PARAM: HASH_PARAM,
    offered: offered,
    nodePoint: nodePoint,
    searchURL: searchURL,
    previewURL: previewURL,
    viewerLink: viewerLink,
    orderPhotos: orderPhotos,
    pickDirection: pickDirection,
    yearChoices: yearChoices,
    selectedYear: selectedYear,
    photoDate: photoDate,
    errorText: errorText,
    formatLink: formatLink,
    parseLink: parseLink,
    withParam: withParam,
    linkFromHash: linkFromHash,
    creditLinks: creditLinks,
    open: open,
    actionButton: actionButton,
    nodeAction: nodeAction,
    current: function () { return current; }
  };
})(typeof window !== 'undefined' ? window : this);
