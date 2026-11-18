/* gesture-hints.js — Issue #1065
 * First-visit gesture discoverability hints.
 *
 * - localStorage namespace: meshcore-gesture-hints-<hint>
 *   keys: row-swipe, tab-swipe, edge-drawer, pull-refresh
 *   value: "seen"
 * - Show hint 800ms after page settle; auto-fade 8s; "Got it" dismisses.
 * - aria-live=polite, role=status, no focus stealing, pointer-events:none.
 * - #911: only "Got it" takes taps, and it never lies on the middle of a
 *   page control (placeAll()). A tap on a hint's pill, outside "Got it",
 *   dismisses that hint alone and still reaches the control underneath; a
 *   tap elsewhere, a scroll or a drag dismisses nothing. A hint shows only
 *   where its gesture works: row swipe on the lists (never on a detail page
 *   such as #/nodes/<key>), tab swipe on the bottom-nav tabs' routes, the
 *   edge and pull swipes on any page but Live.
 * - prefers-reduced-motion: animation-name: none (style.css handles via media query).
 * - Singleton + cleanup: module-scoped guard; SPA re-mount must not re-show dismissed.
 * - Pull-to-refresh hint only when .pull-to-reconnect element exists in DOM.
 * - Edge-drawer hint only at viewport <= 768px (mobile layout, where the
 *   edge-swipe drawer is the nav UI; nav-drawer.js NARROW_MAX=768, inclusive).
 * - Row-swipe hint only on table pages: /#/packets, /#/nodes, etc.
 */
(function () {
  'use strict';
  if (window.__gestureHints1065Init) {
    window.__gestureHints1065Init++;
    return;
  }
  window.__gestureHints1065Init = 1;

  var NS = 'meshcore-gesture-hints-';
  // #1244: gesture hints are bottom-anchored pills. On /live they get
  // buried below the absolute-positioned VCR bar (+ safe-area inset),
  // appearing as orphan "Got it" litter visible only after scrolling.
  // Option (a) from #1244 — disable hints on /live entirely. Swipe-nav
  // discoverability doesn't apply on Live anyway (map drag, VCR
  // controls, and feed all own touch).
  function onLiveRoute() {
    var h = location.hash || '';
    return /^#\/live(\/|$|\?)/.test(h);
  }
  // #911: a hint shows only where its gesture works. A row swipe needs the
  // list's rows, so not on a detail page (#/nodes/<key>, #/packets/<hash>).
  function onListRoute() {
    return /^#\/(packets|nodes|channels|observers)\/?(\?|$)/.test(location.hash || '');
  }
  // A tab swipe switches tabs only on a bottom-nav tab's route
  // (touch-gestures.js); #/nodes is reached through More and is not one.
  function canSwipeTabs() {
    var tg = window.TouchGestures;
    return !tg || !tg.canSwipeTabs || tg.canSwipeTabs();
  }
  // #1065 follow-up: hints must only appear on touch-capable viewports.
  // Mouse-only desktops (e.g. analyzer.00id.net opened in Chrome on a
  // workstation) were getting "swipe a row left" tips that make no sense.
  // Three independent probes — any positive answer counts.
  function hasTouchCapability() {
    try {
      if ('ontouchstart' in window) return true;
      if (navigator.maxTouchPoints && navigator.maxTouchPoints > 0) return true;
      if (window.matchMedia && window.matchMedia('(pointer: coarse)').matches) return true;
    } catch (_e) {}
    return false;
  }
  var HINTS = {
    'row-swipe': {
      key: NS + 'row-swipe',
      text: 'Tip: swipe a row left for quick actions.',
      relevant: function () {
        if (!hasTouchCapability()) return false;
        if (onLiveRoute()) return false; // #1244
        return onListRoute();
      },
      position: 'bottom',
    },
    'tab-swipe': {
      key: NS + 'tab-swipe',
      text: 'Tip: swipe left or right to switch tabs.',
      relevant: function () {
        if (!hasTouchCapability()) return false;
        if (onLiveRoute()) return false; // #1244
        if (!canSwipeTabs()) return false; // #911
        return !!document.querySelector('[data-bottom-nav]');
      },
      position: 'bottom',
    },
    'edge-drawer': {
      key: NS + 'edge-drawer',
      text: 'Tip: swipe in from the left edge to open navigation.',
      relevant: function () {
        if (!hasTouchCapability()) return false;
        if (onLiveRoute()) return false; // #1244
        // nav-drawer.js: NARROW_MAX=768; "narrow" is inclusive — narrow when
        // width <= NARROW_MAX (nav-drawer treats width > NARROW_MAX as the
        // non-narrow / sidebar layout). The edge-swipe drawer is the MOBILE
        // (≤768) layout's nav UI per #1064/#1184. Above 768, the persistent
        // sidebar is visible and no edge-swipe is needed.
        return window.innerWidth <= 768 && !!document.querySelector('.nav-drawer, [data-nav-drawer]');
      },
      position: 'top-left',
    },
    'pull-refresh': {
      key: NS + 'pull-refresh',
      text: 'Tip: pull down to refresh the connection.',
      relevant: function () {
        if (!hasTouchCapability()) return false;
        if (onLiveRoute()) return false; // #1244
        return !!document.querySelector('.pull-to-reconnect');
      },
      position: 'top',
    },
  };

  var SHOW_DELAY_MS = 800;
  var AUTO_FADE_MS = 8000;

  var _shown = Object.create(null); // hint id → element (currently rendered)
  var _scheduledTimer = null;
  var _routeChangeBound = false;

  function isSeen(id) {
    try { return localStorage.getItem(HINTS[id].key) === 'seen'; }
    catch (_e) { return false; }
  }
  function markSeen(id) {
    try { localStorage.setItem(HINTS[id].key, 'seen'); } catch (_e) {}
  }
  function clearAll() {
    try {
      Object.keys(HINTS).forEach(function (id) { localStorage.removeItem(HINTS[id].key); });
    } catch (_e) {}
  }

  function buildHintEl(id) {
    var def = HINTS[id];
    var wrap = document.createElement('div');
    wrap.className = 'gesture-hint gesture-hint-' + def.position;
    // Belt-and-suspenders: inline style guarantees pointer-events:none
    // regardless of CSS load order or cascade collisions. The hint must
    // never capture clicks; only the "Got it" button does (#911: the pill
    // body and text are pointer-events:none too, see style.css).
    wrap.style.pointerEvents = 'none';
    wrap.setAttribute('data-gesture-hint', id);
    wrap.setAttribute('role', 'status');
    wrap.setAttribute('aria-live', 'polite');
    wrap.setAttribute('aria-atomic', 'true');

    var inner = document.createElement('div');
    inner.className = 'gesture-hint-inner';

    var msg = document.createElement('span');
    msg.className = 'gesture-hint-text';
    msg.textContent = def.text;
    inner.appendChild(msg);

    var btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'gesture-hint-dismiss';
    btn.setAttribute('data-gesture-hint-dismiss', '');
    // The accessible name starts with the visible text (WCAG 2.5.3).
    btn.setAttribute('aria-label', 'Got it, dismiss hint');
    btn.textContent = 'Got it';
    btn.addEventListener('click', function (e) {
      e.preventDefault();
      e.stopPropagation();
      dismiss(id);
    });
    inner.appendChild(btn);

    wrap.appendChild(inner);
    return wrap;
  }

  // #911: one hint per screen slot. Two pills in the same slot sat on top of
  // each other and the upper one took the tap meant for the other's "Got it".
  function slotTaken(position) {
    return Object.keys(_shown).some(function (k) { return HINTS[k].position === position; });
  }

  function show(id) {
    if (_shown[id]) return;
    if (isSeen(id)) return;
    var def = HINTS[id];
    if (!def || !def.relevant()) return;
    if (slotTaken(def.position)) return;

    var el = buildHintEl(id);
    document.body.appendChild(el);
    _shown[id] = el;
    placeAll();
    // The slide-in moves the pill by a few px: place it again once it ends.
    el.addEventListener('animationend', placeAll);
    startPlacing();

    // Auto-fade after AUTO_FADE_MS — does NOT mark seen; user must explicitly dismiss
    // (per AC: "Got it" button clears the flag).
    var fadeTimer = setTimeout(function () {
      if (_shown[id] === el) {
        el.classList.add('gesture-hint-fading');
        setTimeout(function () {
          if (el.parentNode) el.parentNode.removeChild(el);
          if (_shown[id] === el) delete _shown[id];
        }, 350);
      }
    }, AUTO_FADE_MS);
    el._gestureHintFadeTimer = fadeTimer;
  }

  // #911: "Got it" is the one part of a hint that takes a tap, so it must
  // not lie on the middle of a page control, where a tap meant for the
  // control would only dismiss the hint. Where it lands depends on fonts,
  // data and scroll, so placeAll() measures: each hint stays where it is
  // while its button covers no control's middle, else moves (CSS
  // `translate`, which the slide-in's `transform` leaves alone) to the
  // nearest spot where it covers none, else hides until the page moves.
  // It runs when a hint shows, SCROLL_SETTLE_MS after a scroll or resize,
  // and every PLACE_POLL_MS while a hint is up, as lists render after it
  // shows. One run reads each control's box once for all hints.
  var CONTROLS = 'a[href], button, input, select, textarea, summary, [role="button"], ' +
    '[role="tab"], [role="link"], [role="checkbox"], [role="switch"], [role="menuitem"], tr[data-action]';
  var PLACE_POLL_MS = 250;
  var SCROLL_SETTLE_MS = 80;
  var NUDGE_STEP_PX = 8;
  var NUDGE_MAX_PX = 120;
  var EDGE_GAP_PX = 16;
  var CLEAR_PX = 4; // "Got it" keeps this far from a control's middle
  // Without the `translate` property a hint cannot move, only hide.
  var CAN_MOVE = !!(window.CSS && CSS.supports && CSS.supports('translate', '1px 1px'));

  // The middles of the controls in the viewport.
  function controlMiddles(vw, vh) {
    var out = [];
    var list = document.querySelectorAll(CONTROLS);
    for (var i = 0; i < list.length; i++) {
      var r = list[i].getBoundingClientRect();
      if (!r.width || !r.height) continue;
      var x = r.left + r.width / 2, y = r.top + r.height / 2;
      if (x < 0 || y < 0 || x > vw || y > vh) continue;
      out.push({ el: list[i], x: x, y: y });
    }
    return out;
  }

  function place(el, mids, vw, floor) {
    var btn = el.querySelector('[data-gesture-hint-dismiss]');
    var cur = el._gestureHintNudge || { x: 0, y: 0 };
    var b = btn.getBoundingClientRect(), p = el.getBoundingClientRect();
    if (!b.width || !p.width) return;
    // The hint's own spot: its box without the current nudge.
    var bl = b.left - cur.x, bt = b.top - cur.y;
    var pl = p.left - cur.x, pt = p.top - cur.y, pr = p.right - cur.x, pb = p.bottom - cur.y;
    function fits(n) {
      return pl + n.x >= 0 && pr + n.x <= vw && pt + n.y >= 0 && pb + n.y <= floor;
    }
    function free(n) {
      var l = bl + n.x - CLEAR_PX, t = bt + n.y - CLEAR_PX;
      var r = l + b.width + 2 * CLEAR_PX, btm = t + b.height + 2 * CLEAR_PX;
      for (var i = 0; i < mids.length; i++) {
        var m = mids[i];
        if (m.el !== btn && m.x >= l && m.x <= r && m.y >= t && m.y <= btm) return false;
      }
      return true;
    }
    var spot = null;
    if (fits(cur) && free(cur)) spot = cur;
    else if (CAN_MOVE) {
      var cands = [];
      var dxs = [0, EDGE_GAP_PX - pl, vw - EDGE_GAP_PX - pr];
      for (var dy = -NUDGE_MAX_PX; dy <= NUDGE_MAX_PX; dy += NUDGE_STEP_PX) {
        for (var k = 0; k < dxs.length; k++) cands.push({ x: dxs[k], y: dy });
      }
      cands.sort(function (a, c) { return (Math.abs(a.x) + Math.abs(a.y)) - (Math.abs(c.x) + Math.abs(c.y)); });
      for (var j = 0; j < cands.length && !spot; j++) {
        if (fits(cands[j]) && free(cands[j])) spot = cands[j];
      }
    }
    // Style is written only on a change: a write would make the next
    // measurement lay out the page again.
    var vis = spot ? '' : 'hidden';
    if (el.style.visibility !== vis) el.style.visibility = vis;
    if (!spot || spot === cur) return;
    el._gestureHintNudge = spot;
    el.style.translate = (spot.x || spot.y) ? spot.x + 'px ' + spot.y + 'px' : '';
    // The other hints keep clear of this button where it is now.
    mids.forEach(function (m) {
      if (m.el === btn) { m.x += spot.x - cur.x; m.y += spot.y - cur.y; }
    });
  }

  var _placeTimer = null;
  var _settleTimer = null;
  function placeAll() {
    var ids = Object.keys(_shown);
    if (!ids.length) return;
    var vw = innerWidth, floor = innerHeight;
    var nav = document.querySelector('[data-bottom-nav]');
    var nr = nav && nav.getBoundingClientRect();
    if (nr && nr.height && nr.top < floor) floor = nr.top;
    var mids = controlMiddles(vw, innerHeight);
    ids.forEach(function (id) { place(_shown[id], mids, vw, floor); });
  }
  function startPlacing() {
    if (_placeTimer) return;
    _placeTimer = setInterval(function () {
      if (!Object.keys(_shown).length) { clearInterval(_placeTimer); _placeTimer = null; return; }
      placeAll();
    }, PLACE_POLL_MS);
  }
  function onScrollOrResize() {
    if (!Object.keys(_shown).length) return;
    clearTimeout(_settleTimer);
    _settleTimer = setTimeout(placeAll, SCROLL_SETTLE_MS);
  }

  // #911: which hint's pill (outside "Got it") is on top at (x, y). The pill
  // takes no taps (pointer-events:none), so it is made hit-testable for the
  // one hit test: a sheet or dialog above it wins there, as for a real tap.
  function pillAt(x, y) {
    var ids = Object.keys(_shown);
    for (var i = 0; i < ids.length; i++) {
      var el = _shown[ids[i]];
      if (el.classList.contains('gesture-hint-fading')) continue;
      var inner = el.querySelector('.gesture-hint-inner');
      var r = inner.getBoundingClientRect();
      if (x < r.left || x > r.right || y < r.top || y > r.bottom) continue;
      inner.style.pointerEvents = 'auto';
      var hit = document.elementFromPoint(x, y);
      inner.style.pointerEvents = '';
      if (hit && el.contains(hit) && !hit.closest('[data-gesture-hint-dismiss]')) return ids[i];
    }
    return null;
  }

  // #911: a tap on a hint's pill, outside "Got it", dismisses that hint
  // alone, as the user has seen it and tapped through it; a tap elsewhere
  // dismisses nothing. Only a tap counts: a touch that moves more than
  // TAP_SLOP_PX, or that the browser cancels for a scroll, is not one. The
  // tap still reaches the control underneath: nothing here prevents or stops
  // the event, and the pill is not its target.
  var TAP_SLOP_PX = 10; // touch-gestures.js AXIS_LOCK_DISTANCE
  var _tap = null;
  function onPointerDown(e) {
    _tap = null;
    if (!e.isPrimary || !Object.keys(_shown).length) return;
    var id = pillAt(e.clientX, e.clientY);
    if (id) _tap = { id: id, pointerId: e.pointerId, x: e.clientX, y: e.clientY };
  }
  function onPointerUp(e) {
    var t = _tap;
    _tap = null;
    if (!t || e.pointerId !== t.pointerId) return;
    if (Math.abs(e.clientX - t.x) > TAP_SLOP_PX || Math.abs(e.clientY - t.y) > TAP_SLOP_PX) return;
    if (_shown[t.id]) dismiss(t.id);
  }
  function onPointerCancel() { _tap = null; }

  function dismiss(id) {
    var el = _shown[id];
    markSeen(id);
    if (el) {
      if (el._gestureHintFadeTimer) clearTimeout(el._gestureHintFadeTimer);
      if (el.parentNode) el.parentNode.removeChild(el);
      delete _shown[id];
    }
  }

  function scheduleHints() {
    if (_scheduledTimer) clearTimeout(_scheduledTimer);
    _scheduledTimer = setTimeout(function () {
      _scheduledTimer = null;
      Object.keys(HINTS).forEach(function (id) {
        if (!isSeen(id)) show(id);
      });
    }, SHOW_DELAY_MS);
  }

  function onRouteChange() {
    // Remove hints that are no longer relevant for the new route.
    Object.keys(_shown).slice().forEach(function (id) {
      var def = HINTS[id];
      if (!def || !def.relevant()) {
        var el = _shown[id];
        if (el && el._gestureHintFadeTimer) clearTimeout(el._gestureHintFadeTimer);
        if (el && el.parentNode) el.parentNode.removeChild(el);
        delete _shown[id];
      }
    });
    // Re-evaluate: show any not-yet-seen relevant hints.
    scheduleHints();
  }

  function init() {
    if (!_routeChangeBound) {
      _routeChangeBound = true;
      window.addEventListener('hashchange', onRouteChange);
      document.addEventListener('pointerdown', onPointerDown, { capture: true, passive: true });
      document.addEventListener('pointerup', onPointerUp, { capture: true, passive: true });
      document.addEventListener('pointercancel', onPointerCancel, { capture: true, passive: true });
      document.addEventListener('scroll', onScrollOrResize, { capture: true, passive: true });
      window.addEventListener('resize', onScrollOrResize);
    }
    scheduleHints();
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init, { once: true });
  } else {
    init();
  }

  // #1402 test hook: expose hint definitions for E2E predicate probes.
  // Read-only by convention; tests call .relevant() to verify routing/viewport gates.
  window.__gestureHintsDefs = HINTS;
  // M3: freeze the hint defs to prevent tests / page scripts from mutating
  // production state via the test hook. Shallow-freeze HINTS + each def.
  try {
    Object.keys(HINTS).forEach(function (id) { Object.freeze(HINTS[id]); });
    Object.freeze(HINTS);
  } catch (_e) {}

  window.GestureHints = {
    show: show,
    dismiss: dismiss,
    reset: function () {
      clearAll();
      // Remove any visible.
      Object.keys(_shown).slice().forEach(function (id) {
        var el = _shown[id];
        if (el && el._gestureHintFadeTimer) clearTimeout(el._gestureHintFadeTimer);
        if (el && el.parentNode) el.parentNode.removeChild(el);
        delete _shown[id];
      });
    },
    _keys: function () {
      return Object.keys(HINTS).map(function (id) { return HINTS[id].key; });
    },
  };
})();
