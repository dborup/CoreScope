/* #839 — keep map UI clear of the chrome drawn over the map.
 *
 * On a phone the map shares its few hundred pixels with the controls
 * Leaflet floats over it (zoom, layers, the attribution) and with the
 * page's own buttons. Leaflet sizes two things without looking at them:
 *
 *   - the layer control's list gets `map height - (offsetTop + 50)`; at
 *     375x560 the attribution alone is 44 px and wraps to more with a few
 *     credits, so the last rows end up under it and a tap lands on the
 *     credit instead (#839 finding 1);
 *   - a popup is as tall as its content and auto-pans with 5 px of padding,
 *     so the antenna-site popup outgrew the map and its close button sat
 *     under the zoom/settings buttons (#839 finding 2).
 *
 * The geometry is pure (listRoom, popupRoom) so it is unit-tested in Node;
 * the two small Leaflet glue functions measure the page and apply it.
 * Every rectangle is relative to the map container: {left, top, right,
 * bottom} in CSS px.
 */
(function (global) {
  'use strict';

  var GAP = 8;          // clearance kept from any chrome, px
  var MIN_LIST = 48;    // the layer list never shrinks below one row and a bit
  var MIN_POPUP = 96;   // nor a popup's scroll area below a few lines
  // Leaflet 1.9.4's popup box around .leaflet-popup-content (leaflet.css):
  // content margins 13+13 / 20+24, wrapper padding 1+1, the 20 px bottom
  // margin that holds the tip, and the +1 _updateLayout adds to the width.
  var POPUP_EXTRA_H = 48;
  var POPUP_EXTRA_W = 47;

  function overlapsX(r, x0, x1) { return r.left < x1 && x0 < r.right; }

  /* The height a list that starts at `box.top` and spans box.left..right
   * may take before it reaches chrome below it or the map's bottom edge.
   * `padBottom` is what the list's own container adds under the list. */
  function listRoom(size, chrome, box, padBottom) {
    var limit = size.y;
    for (var i = 0; i < chrome.length; i++) {
      var r = chrome[i];
      if (r.top >= box.top && overlapsX(r, box.left, box.right) && r.top < limit) limit = r.top;
    }
    return Math.max(MIN_LIST, Math.floor(limit - GAP - box.top - (padBottom || 0)));
  }

  /* Auto-pan padding and a maximum content height for a popup of at most
   * `boxW` px wide (the whole popup box) whose tip is at x `anchorX`.
   *
   * Chrome in the top half of the map pushes the popup down, chrome in the
   * bottom half pushes it up; a column taller than half the map (the
   * desktop Map Controls panel) is a wall instead and narrows the popup's
   * horizontal room, so it never squeezes the popup to nothing. Only the
   * chrome the popup will overlap after Leaflet's horizontal auto-pan
   * counts, so a button on the far side of a wide map costs nothing. */
  function popupRoom(size, chrome, anchorX, boxW) {
    var mid = size.y / 2, padL = GAP, padR = GAP, i, r;
    var flat = [];
    for (i = 0; i < chrome.length; i++) {
      r = chrome[i];
      if (r.top < mid && r.bottom > mid) {
        if ((r.left + r.right) / 2 < size.x / 2) padL = Math.max(padL, r.right + GAP);
        else padR = Math.max(padR, size.x - r.left + GAP);
      } else {
        flat.push(r);
      }
    }
    var x0 = Math.min(Math.max(anchorX - boxW / 2, padL), size.x - padR - boxW);
    if (x0 < padL) x0 = padL;
    var x1 = x0 + boxW;
    var padT = GAP, padB = GAP;
    for (i = 0; i < flat.length; i++) {
      r = flat[i];
      if (!overlapsX(r, x0, x1)) continue;
      if (r.bottom <= mid) padT = Math.max(padT, r.bottom + GAP);
      else padB = Math.max(padB, size.y - r.top + GAP);
    }
    return {
      autoPanPaddingTopLeft: [padL, padT],
      autoPanPaddingBottomRight: [padR, padB],
      maxHeight: Math.max(MIN_POPUP, Math.floor(size.y - padT - padB - POPUP_EXTRA_H))
    };
  }

  // ── Leaflet glue (browser only) ─────────────────────────────────────────

  // The page's own buttons and panels that float over the map; Leaflet's
  // controls are found by class.
  var PAGE_CHROME = '#mapControlsToggle, .map-controls, .bottom-nav';

  /* Map-relative rectangles of every visible piece of chrome over `map`,
   * except `skip` (the element being fitted) and anything inside it. */
  function chromeRects(map, skip) {
    var c = map.getContainer(), doc = c.ownerDocument;
    var mr = c.getBoundingClientRect();
    var els = Array.prototype.slice.call(c.querySelectorAll('.leaflet-control'))
      .concat(Array.prototype.slice.call(doc.querySelectorAll(PAGE_CHROME)));
    var out = [];
    for (var i = 0; i < els.length; i++) {
      var el = els[i];
      if (skip && (el === skip || skip.contains(el) || el.contains(skip))) continue;
      var b = el.getBoundingClientRect();
      if (!b.width || !b.height) continue;
      var r = {
        left: Math.max(0, b.left - mr.left), top: Math.max(0, b.top - mr.top),
        right: Math.min(mr.width, b.right - mr.left), bottom: Math.min(mr.height, b.bottom - mr.top)
      };
      if (r.right > r.left && r.bottom > r.top) out.push(r);
    }
    return out;
  }

  function fitList(map, control) {
    var c = control.getContainer && control.getContainer();
    if (!c || !control._map || c.className.indexOf('leaflet-control-layers-expanded') < 0) return;
    var s = c.querySelector('.leaflet-control-layers-list');
    if (!s) return;
    s.style.height = '';
    var mr = map.getContainer().getBoundingClientRect();
    var sr = s.getBoundingClientRect(), cr = c.getBoundingClientRect();
    var size = { x: mr.width, y: mr.height };
    var box = { left: cr.left - mr.left, right: cr.right - mr.left, top: sr.top - mr.top };
    var room = listRoom(size, chromeRects(map, c), box, cr.bottom - sr.bottom);
    if (s.scrollHeight > room) {
      s.classList.add('leaflet-control-layers-scrollbar');
      s.style.height = room + 'px';
    } else {
      s.classList.remove('leaflet-control-layers-scrollbar');
    }
  }

  /* Makes a layer control's list end above the chrome under it, every time
   * it opens and whenever the credits (an overlay switched on), the view
   * (the masts status line) or the map's size change while it is open. */
  function fitLayerControl(map, control) {
    if (!control || control._mcFitted) return control;
    control._mcFitted = true;
    var expand = control.expand;
    control.expand = function () {
      var r = expand.apply(this, arguments);
      fitList(map, control);
      return r;
    };
    var t = null;
    function refit() {
      if (!control._map) { map.off('layeradd layerremove moveend', refit); return; }
      if (t) return;
      // After the attribution control has redrawn its credits.
      t = setTimeout(function () { t = null; fitList(map, control); }, 0);
    }
    map.on('layeradd layerremove moveend', refit);
    return control;
  }

  /* Sets `popup`'s auto-pan padding and maxHeight from the chrome over
   * `map`, for a popup whose tip is at `latlng`. Call it before the popup
   * lays itself out (e.g. from its content function). */
  function fitPopup(map, popup, latlng) {
    if (!map || !popup || !latlng) return;
    var c = map.getContainer(), mr = c.getBoundingClientRect();
    var size = { x: mr.width, y: mr.height };
    var x = map.latLngToContainerPoint(latlng).x;
    var boxW = Math.min(size.x, (popup.options.maxWidth || 300) + POPUP_EXTRA_W);
    var room = popupRoom(size, chromeRects(map, null), x, boxW);
    popup.options.autoPanPaddingTopLeft = room.autoPanPaddingTopLeft;
    popup.options.autoPanPaddingBottomRight = room.autoPanPaddingBottomRight;
    popup.options.maxHeight = room.maxHeight;
  }

  global.MapChromeFit = {
    GAP: GAP,
    MIN_LIST: MIN_LIST,
    MIN_POPUP: MIN_POPUP,
    POPUP_EXTRA_H: POPUP_EXTRA_H,
    POPUP_EXTRA_W: POPUP_EXTRA_W,
    listRoom: listRoom,
    popupRoom: popupRoom,
    fitLayerControl: fitLayerControl,
    fitPopup: fitPopup
  };
})(typeof window !== 'undefined' ? window : this);
