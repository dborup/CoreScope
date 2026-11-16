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
   * desktop Map Controls panel, the zoom/layers column in landscape) is a
   * wall instead and narrows the popup's horizontal room, so it never
   * squeezes the popup to nothing. A narrow button the popup would overlap
   * (the gear and the ruler on the right) becomes a wall too whenever the
   * popup still fits beside it: in landscape the popup then sits beside the
   * buttons instead of under them, where the close button of a popup for a
   * site on the right used to land on the ruler (#841 F1). Only the chrome the
   * popup will overlap after Leaflet's horizontal auto-pan counts, so a
   * button on the far side of a wide map costs nothing. */
  function popupRoom(size, chrome, anchorX, boxW) {
    var mid = size.y / 2, walls = [], flat = [], i, r;
    for (i = 0; i < chrome.length; i++) {
      r = chrome[i];
      if (r.top < mid && r.bottom > mid) walls.push(r); else flat.push(r);
    }
    function place(ws) {
      var padL = GAP, padR = GAP;
      for (var k = 0; k < ws.length; k++) {
        var w = ws[k];
        if ((w.left + w.right) / 2 < size.x / 2) padL = Math.max(padL, w.right + GAP);
        else padR = Math.max(padR, size.x - w.left + GAP);
      }
      var x0 = Math.min(Math.max(anchorX - boxW / 2, padL), size.x - padR - boxW);
      if (x0 < padL) x0 = padL;
      return { padL: padL, padR: padR, x0: x0, x1: x0 + boxW };
    }
    var p = place(walls);
    for (i = 0; i < flat.length; i++) {
      r = flat[i];
      if (r.right - r.left > size.x / 4 || !overlapsX(r, p.x0, p.x1)) continue;
      var q = place(walls.concat([r]));
      if (size.x - q.padL - q.padR < boxW) continue;
      walls.push(r);
      flat.splice(i, 1);
      p = q;
      i = -1; // the popup moved: look at what it overlaps now
    }
    var padT = GAP, padB = GAP;
    for (i = 0; i < flat.length; i++) {
      r = flat[i];
      if (!overlapsX(r, p.x0, p.x1)) continue;
      if (r.bottom <= mid) padT = Math.max(padT, r.bottom + GAP);
      else padB = Math.max(padB, size.y - r.top + GAP);
    }
    return {
      autoPanPaddingTopLeft: [p.padL, padT],
      autoPanPaddingBottomRight: [p.padR, padB],
      maxHeight: Math.max(MIN_POPUP, Math.floor(size.y - padT - padB - POPUP_EXTRA_H))
    };
  }

  // ── Leaflet glue (browser only) ─────────────────────────────────────────

  // The page's own buttons and panels that float over the map (the gear,
  // its panel, the path-profile ruler under the gear, the bottom nav);
  // Leaflet's controls are found by class.
  var PAGE_CHROME = '#mapControlsToggle, .map-controls, .map-profile-toggle, .bottom-nav';

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
    // Measuring at full height clamps scrollTop to 0; a refit (a row
    // tapped, markers re-added, a packet animation) must not throw the
    // viewer back to the top of the list (#841 F8).
    var keep = s.scrollTop;
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
    s.scrollTop = keep;
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
      // A closed control has nothing to fit; expand() fits it on opening.
      var c = control.getContainer && control.getContainer();
      if (t || !c || c.className.indexOf('leaflet-control-layers-expanded') < 0) return;
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
