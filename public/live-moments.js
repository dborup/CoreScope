/* === CoreScope — live-moments.js === */
/* #881 M2 — Live "Moments": short-lived cards for the server's
 * {"type":"moment"} WebSocket frames (cmd/server/moments.go), a drawer with
 * the latest ones, pause and fly-to on click. live.js owns the per-viewer
 * off switch (resolveInitialEnabled).
 *
 * live.js owns the map and the page; this module owns the cards, the queue
 * and the drawer. Every dependency is injected (create(deps)), so the logic
 * runs in a vm sandbox with a fake DOM (test-881-live-moments.js).
 *
 * Hot path: the WS listener does one type check per frame and nothing else
 * for a packet. A moment is a handful of frames per hour; its DOM work is
 * coalesced into one render per tick.
 *
 * XSS: every server string reaches the page through textContent or a
 * property set from an encodeURIComponent'd value. No innerHTML.
 *
 * Viewer settings (#881 M3, AGENTS.md rule 8) are the SETTINGS rule below:
 * the customizer stores exactly these keys (its `moments` section) and this
 * module reads them back through deps.getSettings(). Server policy
 * (thresholds, rate) stays in the operator's config.json `moments` block.
 * The drawer length, queue size and the phone's single card slot stay
 * fixed; they are layout, not taste.
 */
'use strict';

(function () {
  var KINDS = {
    new_node: { icon: 'ph-star', title: 'New node' },
    back_online: { icon: 'ph-arrow-clockwise', title: 'Back online' },
    record_link: { icon: 'ph-ruler', title: 'Record link' },
    busiest_repeater: { icon: 'ph-trend-up', title: 'Busiest repeater' }
  };
  var VISIBLE_PHONE = 1;     // one slot: the node sheet and the controls own the rest
  var LEAVE_MS = 250;        // the CSS fade; 0 under reduced motion
  var DRAWER_LEN = 20;       // history kept and listed
  var QUEUE_MAX = 10;        // cards waiting for a slot; oldest dropped
  var SEEN_MAX = 200;        // ids remembered for dedup
  var NAME_MAX = 64;
  var FLY_MIN_ZOOM = 12;
  var STORAGE_KEY = 'live-moments';
  var HASH_PARAM = 'moments';
  var PHONE_MQ = '(max-width: 640px)';
  // Short landscape (any width, e.g. 740x360 or 844x390): the Leaflet
  // controls become a row (live.css), so cards go to history only.
  var SHORT_MQ = '(max-height: 419px)';
  var REDUCE_MQ = '(prefers-reduced-motion: reduce)';
  // The viewer settings: one rule for the customizer, the controller and the
  // tests. visibleCards is the desktop stack (a phone always has one slot);
  // dwellSeconds is how long a card stays before it fades out.
  var KIND_SETTING = {
    new_node: 'showNewNode',
    back_online: 'showBackOnline',
    record_link: 'showRecordLink',
    busiest_repeater: 'showBusiest'
  };
  var SETTINGS = {
    KEYS: ['enabled', 'visibleCards', 'dwellSeconds', 'showNewNode', 'showBackOnline', 'showRecordLink', 'showBusiest'],
    DEFAULTS: { enabled: true, visibleCards: 3, dwellSeconds: 8, showNewNode: true, showBackOnline: true, showRecordLink: true, showBusiest: true },
    LIMITS: { visibleCards: [1, 5], dwellSeconds: [3, 30] },
    valid: function (key, val) {
      if (SETTINGS.KEYS.indexOf(key) === -1) return false;
      var lim = SETTINGS.LIMITS[key];
      if (!lim) return typeof val === 'boolean';
      return typeof val === 'number' && isFinite(val) && Math.floor(val) === val && val >= lim[0] && val <= lim[1];
    },
    // resolve is a section over the defaults; a bad or missing value is the
    // default, so a hand-edited override never breaks the page.
    resolve: function (sec) {
      var out = {};
      var src = sec && typeof sec === 'object' ? sec : {};
      for (var i = 0; i < SETTINGS.KEYS.length; i++) {
        var k = SETTINGS.KEYS[i];
        out[k] = Object.prototype.hasOwnProperty.call(src, k) && SETTINGS.valid(k, src[k]) ? src[k] : SETTINGS.DEFAULTS[k];
      }
      return out;
    }
  };
  var PUBKEY_RE = /^[0-9a-f]{64}$/;
  var SVG_NS = 'http://www.w3.org/2000/svg';
  var SPRITE = '/icons/phosphor-sprite.svg#';

  // ── Pure helpers ────────────────────────────────────────────────────────

  function str(v, max) {
    if (typeof v !== 'string') return '';
    return v.length > max ? v.slice(0, max) : v;
  }
  function finite(v) {
    var n = typeof v === 'number' ? v : (typeof v === 'string' && v !== '' ? Number(v) : NaN);
    return isFinite(n) ? n : null;
  }

  // Cast one MomentNode at the boundary. Lat/lon only as a pair, never (0,0).
  function normNode(raw) {
    if (!raw || typeof raw !== 'object') return null;
    var pk = str(raw.publicKey, 128);
    if (!pk) return null;
    var lat = finite(raw.lat), lon = finite(raw.lon);
    var hasPos = lat !== null && lon !== null && !(lat === 0 && lon === 0) &&
      Math.abs(lat) <= 90 && Math.abs(lon) <= 180;
    return {
      publicKey: pk,
      name: str(raw.name, NAME_MAX),
      role: str(raw.role, 32),
      lat: hasPos ? lat : null,
      lon: hasPos ? lon : null
    };
  }

  // normalize casts a Moment from the WS or REST to the shape this module
  // uses, or null when it is not one it can show.
  function normalize(raw) {
    if (!raw || typeof raw !== 'object') return null;
    var id = str(raw.id, 64), kind = str(raw.kind, 32);
    if (!id || !KINDS[kind]) return null;
    var at = Date.parse(raw.at);
    var m = {
      id: id, kind: kind, at: isFinite(at) ? at : null,
      node: null, link: null,
      silentHours: finite(raw.silentHours),
      relays: finite(raw.relays),
      windowMinutes: finite(raw.windowMinutes),
      previousKm: finite(raw.previousKm),
      more: Math.max(0, Math.floor(finite(raw.more) || 0))
    };
    if (kind === 'record_link') {
      var l = raw.link;
      var a = l && normNode(l.a), b = l && normNode(l.b);
      var km = l ? finite(l.distanceKm) : null;
      if (!a || !b || km === null) return null;
      m.link = { a: a, b: b, distanceKm: km };
    } else {
      m.node = normNode(raw.node);
      if (!m.node) return null;
    }
    return m;
  }

  function label(n) {
    return n.name || n.publicKey.slice(0, 8);
  }

  function fmtNum(v, digits) {
    return v === null ? '?' : Number(v).toFixed(digits);
  }

  // describe is the card text: { icon, title, detail }. Plain strings; the
  // caller sets them with textContent.
  function describe(m) {
    var k = KINDS[m.kind];
    var detail;
    if (m.kind === 'record_link') {
      detail = fmtNum(m.link.distanceKm, 1) + ' km · ' + label(m.link.a) + ' ↔ ' + label(m.link.b);
    } else if (m.kind === 'back_online') {
      detail = label(m.node) + (m.silentHours !== null ? ' · silent ' + Math.round(m.silentHours) + ' h' : '');
    } else if (m.kind === 'busiest_repeater') {
      detail = label(m.node) + (m.relays !== null ? ' · ' + m.relays + ' relays' : '') +
        (m.windowMinutes !== null ? ' / ' + m.windowMinutes + ' min' : '');
    } else {
      detail = label(m.node) + (m.node.role ? ' · ' + m.node.role : '');
    }
    if (m.more > 0) detail += ' +' + m.more + ' more';
    return { icon: k.icon, title: k.title, detail: detail };
  }

  function relTime(at, now) {
    if (at === null) return '';
    var s = Math.max(0, Math.round((now - at) / 1000));
    if (s < 5) return 'now';
    if (s < 60) return s + ' s';
    if (s < 3600) return Math.round(s / 60) + ' min';
    if (s < 86400) return Math.round(s / 3600) + ' h';
    return Math.round(s / 86400) + ' d';
  }

  // detailsHref builds the card's secondary link from existing routes only.
  // Never #/live?node=: that is the #1110 Live node filter, which is saved,
  // so a card link would silently change the viewer's filter.
  function detailsHref(m, profileOffered) {
    if (m.kind === 'record_link') {
      var a = m.link.a.publicKey.toLowerCase(), b = m.link.b.publicKey.toLowerCase();
      if (profileOffered && PUBKEY_RE.test(a) && PUBKEY_RE.test(b)) {
        return '#/map?profile=' + a + ',' + b;
      }
      return '#/nodes/' + encodeURIComponent(m.link.a.publicKey) + '?section=node-neighbors';
    }
    return '#/nodes/' + encodeURIComponent(m.node.publicKey);
  }

  // flyTarget is where a card click takes the map, or null without GPS.
  function flyTarget(m) {
    if (m.kind === 'record_link') {
      var a = m.link.a, b = m.link.b;
      if (a.lat === null || b.lat === null) return null;
      return { type: 'link', a: { key: a.publicKey, lat: a.lat, lon: a.lon }, b: { key: b.publicKey, lat: b.lat, lon: b.lon } };
    }
    if (m.node.lat === null) return null;
    return { type: 'node', key: m.node.publicKey, lat: m.node.lat, lon: m.node.lon };
  }

  // flyPlan turns a target into one Leaflet call, so the reduced-motion
  // choice is testable: { method, args }. Leaflet does not honour reduced
  // motion itself, hence animate:false.
  function flyPlan(t, reduce, zoom) {
    if (!t) return null;
    if (t.type === 'link') {
      return { method: 'fitBounds', reduce: !!reduce, args: [[[t.a.lat, t.a.lon], [t.b.lat, t.b.lon]], { padding: [60, 60], maxZoom: 13, animate: !reduce }] };
    }
    var z = Math.max(typeof zoom === 'number' ? zoom : 0, FLY_MIN_ZOOM);
    if (reduce) return { method: 'setView', reduce: true, args: [[t.lat, t.lon], z, { animate: false }] };
    return { method: 'flyTo', reduce: false, args: [[t.lat, t.lon], z, { duration: 0.8 }] };
  }

  // resolveInitialEnabled: a valid ?moments=0|1 wins over the saved value
  // (the #660 ?heard= contract), then the viewer's saved choice, then the
  // customizer default (on unless the viewer set it off).
  function resolveInitialEnabled(fromHash, stored, dflt) {
    if (fromHash === '1') return true;
    if (fromHash === '0') return false;
    if (stored === 'true') return true;
    if (stored === 'false') return false;
    return dflt !== false;
  }

  // cap (#883 M2 kiosk: 2) only ever lowers the desktop stack.
  function visibleCap(isPhone, settings, cap) {
    if (isPhone) return VISIBLE_PHONE;
    var n = (settings || SETTINGS.DEFAULTS).visibleCards;
    return cap > 0 ? Math.min(n, cap) : n;
  }

  // ── Controller ──────────────────────────────────────────────────────────
  /* deps:
   *   doc            document
   *   root           element the UI is appended to (.live-page)
   *   onWS, offWS    app.js shared channel
   *   fetchJSON(url, signal) → Promise
   *   now()          ms
   *   matchMedia(q)  → { matches }
   *   setTimeout, clearTimeout
   *   getMode()      'LIVE' | 'PAUSED' | 'REPLAY' | 'FOLLOW' (cards only in LIVE)
   *   isSuppressed() true while a phone sheet/panel owns the bottom
   *   profileOffered() bool
   *   onFly(plan, target, moment)
   *   getSettings()  → the viewer's `moments` section (SETTINGS.resolve'd
   *                    on every read, so a customizer change applies live)
   *   cardCap        optional: at most this many cards (#883 kiosk: 2)
   *   timeRefreshMs  optional: refresh the shown times this often while a
   *                  card or the drawer shows (#883 kiosk: 30 s)
   *   onMoment(m)    optional: every valid moment, normalized, from the
   *                  socket and the seed (the #883 kiosk record tiles)
   *   panelId        optional: makes the window a Live drag panel (#958):
   *                  this id, .live-overlay, the bar as its .panel-header
   *                  and a corner button. live.js registers it; not in kiosk.
   *
   * The per-viewer off switch lives in live.js: a viewer who turned Moments
   * off gets no controller at all (no fetch, no WS listener, no DOM).
   */
  var liveCount = 0; // controllers not yet destroyed (leak check)

  function create(deps) {
    var doc = deps.doc;
    var paused = false;
    var destroyed = false;
    var history = [];          // newest first, ≤ DRAWER_LEN
    var seen = new Set(), seenOrder = [];
    var queue = [];            // moments waiting for a card slot
    var cards = [];            // { m, el, timer, deadline, left, held }
    var timers = new Set();
    var renderTimer = null;
    var timeTimer = null;      // timeRefreshMs only
    var rows = [];             // { m, time } of the open drawer
    var abort = (typeof AbortController === 'function') ? new AbortController() : null;
    var drawerOpen = false;
    var keyHandler = null;
    liveCount++;

    function settings() {
      return SETTINGS.resolve(deps.getSettings ? deps.getSettings() : null);
    }
    function kindShown(kind, s) {
      return s[KIND_SETTING[kind]] !== false;
    }
    function mq(q) {
      try { return !!(deps.matchMedia && deps.matchMedia(q).matches); } catch (_) { return false; }
    }
    function later(fn, ms) {
      var id = deps.setTimeout(function () { timers.delete(id); if (!destroyed) fn(); }, ms);
      timers.add(id);
      return id;
    }
    function cancel(id) {
      if (id == null) return;
      deps.clearTimeout(id);
      timers.delete(id);
    }
    function el(tag, cls, text) {
      var e = doc.createElement(tag);
      if (cls) e.className = cls;
      if (text != null) e.textContent = text;
      return e;
    }
    // iconId is always one of this file's own sprite ids (KINDS, ph-list,
    // ph-x, ph-pause, ph-play), never server data.
    function icon(iconId) {
      var svg = doc.createElementNS(SVG_NS, 'svg');
      svg.setAttribute('class', 'ph-icon');
      svg.setAttribute('aria-hidden', 'true');
      var use = doc.createElementNS(SVG_NS, 'use');
      use.setAttribute('href', SPRITE + iconId);
      svg.appendChild(use);
      return svg;
    }

    // ── DOM skeleton ──
    var wrap = el('div', 'live-moments');
    wrap.setAttribute('data-moments', '');
    var stack = el('div', 'live-moments-stack');
    var bar = el('div', 'live-moments-bar');
    var pill = el('span', 'live-moments-count');
    // Two pause buttons, one state: the bar's (desktop) and the drawer's
    // (phone, where the bar is only the drawer button).
    var pauseBtns = [el('button', 'live-moments-btn live-moments-pause'), el('button', 'live-moments-btn live-moments-pause')];
    var pauseBtn = pauseBtns[0];
    pauseBtns.forEach(function (b) { b.type = 'button'; });
    var drawerBtn = el('button', 'live-moments-btn live-moments-open');
    drawerBtn.type = 'button';
    drawerBtn.setAttribute('aria-label', 'Show recent moments');
    drawerBtn.setAttribute('aria-haspopup', 'dialog');
    drawerBtn.appendChild(icon('ph-list'));
    var moreChip = el('span', 'live-moments-more');
    drawerBtn.appendChild(moreChip);
    bar.appendChild(pill);
    bar.appendChild(pauseBtn);
    bar.appendChild(drawerBtn);
    if (deps.panelId) {
      wrap.id = deps.panelId;
      wrap.classList.add('live-overlay');
      bar.classList.add('panel-header');
      var cornerBtn = el('button', 'panel-corner-btn'); // live.js sets its arrow
      cornerBtn.type = 'button';
      cornerBtn.setAttribute('data-panel', deps.panelId);
      cornerBtn.title = 'Move panel to next corner';
      cornerBtn.setAttribute('aria-label', 'Move panel to next corner');
      bar.appendChild(cornerBtn);
    }
    wrap.appendChild(stack);
    wrap.appendChild(bar);
    var announce = el('div', 'sr-only');
    announce.setAttribute('role', 'status');
    announce.setAttribute('aria-live', 'polite');
    wrap.appendChild(announce);

    var backdrop = el('div', 'live-moments-backdrop');
    backdrop.hidden = true;
    var drawer = el('div', 'live-moments-drawer');
    drawer.hidden = true;
    drawer.setAttribute('role', 'dialog');
    drawer.setAttribute('aria-label', 'Recent moments');
    var dHead = el('div', 'live-moments-drawer-head');
    dHead.appendChild(el('span', 'live-moments-drawer-title', 'Moments'));
    dHead.appendChild(pauseBtns[1]);
    var closeBtn = el('button', 'live-moments-btn live-moments-close');
    closeBtn.type = 'button';
    closeBtn.setAttribute('aria-label', 'Close moments');
    closeBtn.appendChild(icon('ph-x'));
    dHead.appendChild(closeBtn);
    var list = el('ul', 'live-moments-list');
    drawer.appendChild(dHead);
    drawer.appendChild(list);

    deps.root.appendChild(wrap);
    deps.root.appendChild(backdrop);
    deps.root.appendChild(drawer);

    function setPauseLabel() {
      pauseBtns.forEach(function (b) {
        b.textContent = '';
        b.appendChild(icon(paused ? 'ph-play' : 'ph-pause'));
        b.setAttribute('aria-label', paused ? 'Resume moments' : 'Pause moments');
        b.setAttribute('aria-pressed', paused ? 'true' : 'false');
      });
    }
    setPauseLabel();

    // ── Rendering ──
    function scheduleRender() {
      if (renderTimer !== null || destroyed) return;
      renderTimer = later(function () { renderTimer = null; render(); }, 0);
    }

    function render() {
      bar.hidden = history.length === 0;
      pill.textContent = 'Moments · ' + history.length;
      moreChip.textContent = queue.length ? '+' + queue.length : '';
      wrap.classList.toggle('is-paused', paused);
      if (drawerOpen) fillDrawer();
      pump();
    }

    function canShow() {
      if (paused || destroyed) return false;
      if (deps.getMode && deps.getMode() !== 'LIVE') return false;
      if (mq(SHORT_MQ)) return false;
      if (mq(PHONE_MQ) && deps.isSuppressed && deps.isSuppressed()) return false;
      return true;
    }

    // pump moves queued moments into free card slots.
    function pump() {
      var cap = visibleCap(mq(PHONE_MQ), settings(), deps.cardCap);
      while (cards.length > cap) leave(cards[0], true);
      while (queue.length && cards.length < cap) show(queue.shift());
      moreChip.textContent = queue.length ? '+' + queue.length : '';
    }

    function buildEntry(m, tag, cls, onActivate, refs) {
      var d = describe(m);
      var row = el(tag, cls);
      row.setAttribute('data-kind', m.kind);
      row.setAttribute('data-moment-id', m.id);
      var main = el('button', 'live-moment-main');
      main.type = 'button';
      main.appendChild(icon(d.icon));
      var text = el('span', 'live-moment-text');
      text.appendChild(el('span', 'live-moment-title', d.title));
      text.appendChild(el('span', 'live-moment-detail', d.detail));
      main.appendChild(text);
      var time = main.appendChild(el('span', 'live-moment-time', relTime(m.at, deps.now())));
      if (refs) refs.time = time;
      main.setAttribute('aria-label', d.title + ': ' + d.detail);
      main.addEventListener('click', function () { onActivate(m); });
      var link = el('a', 'live-moment-link', 'Details');
      link.href = detailsHref(m, !!(deps.profileOffered && deps.profileOffered()));
      link.setAttribute('aria-label', 'Details: ' + d.detail);
      row.appendChild(main);
      row.appendChild(link);
      return row;
    }

    function show(m) {
      var card = { m: m, el: null, time: null, timer: null, deadline: 0, left: settings().dwellSeconds * 1000, held: false, hover: false, focus: false };
      card.el = buildEntry(m, 'div', 'live-moment-card', activate, card);
      // Hold for a hovering mouse and for keyboard focus only. A tap on a
      // phone also fires a compat mouseenter and focuses the button, and
      // neither is undone until the next tap elsewhere, so a tapped card
      // would stay up and every new moment would queue behind it.
      card.el.addEventListener('pointerenter', function (e) { if (e && e.pointerType === 'mouse') holdFor(card, 'hover', true); });
      card.el.addEventListener('pointerleave', function (e) { if (e && e.pointerType === 'mouse') holdFor(card, 'hover', false); });
      card.el.addEventListener('focusin', function (e) { holdFor(card, 'focus', keyboardFocus(e)); });
      card.el.addEventListener('focusout', function () { holdFor(card, 'focus', false); });
      // Newest last, so nearest the bottom anchor.
      stack.appendChild(card.el);
      cards.push(card);
      var d = describe(m);
      announce.textContent = d.title + ': ' + d.detail;
      arm(card);
      armTimes();
    }

    // ── Times ──
    // refreshTimes rewrites a shown time only when its text changed, and
    // says how many it rewrote.
    function refreshTimes() {
      if (destroyed) return 0;
      var now = deps.now(), n = 0;
      cards.concat(rows).forEach(function (c) {
        var t = relTime(c.m.at, now);
        if (c.time && c.time.textContent !== t) { c.time.textContent = t; n++; }
      });
      return n;
    }
    function armTimes() {
      if (!(deps.timeRefreshMs > 0) || timeTimer !== null || destroyed) return;
      timeTimer = later(function () {
        timeTimer = null;
        refreshTimes();
        if (cards.length || drawerOpen) armTimes();
      }, deps.timeRefreshMs);
    }

    function arm(card) {
      card.deadline = deps.now() + card.left;
      card.timer = later(function () { card.timer = null; leave(card, false); }, card.left);
    }
    // keyboardFocus: focus a viewer moved there with the keyboard, not the
    // focus a click or tap leaves on the button. Without :focus-visible
    // support, any focus counts.
    function keyboardFocus(e) {
      var t = e && e.target;
      try { return !(t && typeof t.matches === 'function') || t.matches(':focus-visible'); } catch (_) { return true; }
    }
    // holdFor: a card is held while the mouse is over it or it has keyboard
    // focus, and resumes its dwell when neither is true.
    function holdFor(card, why, on) {
      card[why] = !!on;
      if (card.hover || card.focus) hold(card); else release(card);
    }
    function hold(card) {
      if (card.held || card.timer === null) return;
      card.held = true;
      cancel(card.timer); card.timer = null;
      card.left = Math.max(0, card.deadline - deps.now());
    }
    function release(card) {
      if (!card.held) return;
      card.held = false;
      if (cards.indexOf(card) !== -1) arm(card);
    }

    function leave(card, now) {
      var i = cards.indexOf(card);
      if (i === -1) return;
      cards.splice(i, 1);
      cancel(card.timer); card.timer = null;
      var e = card.el;
      var ms = (now || mq(REDUCE_MQ)) ? 0 : LEAVE_MS;
      if (ms === 0) { if (e.parentNode) e.parentNode.removeChild(e); }
      else {
        e.classList.add('is-leaving');
        later(function () { if (e.parentNode) e.parentNode.removeChild(e); }, ms);
      }
      if (!now) pump();
    }

    function activate(m) {
      var t = flyTarget(m);
      if (!t) { if (deps.navigate) deps.navigate(detailsHref(m, !!(deps.profileOffered && deps.profileOffered()))); return; }
      var plan = flyPlan(t, mq(REDUCE_MQ), deps.getZoom ? deps.getZoom() : null);
      if (deps.onFly) deps.onFly(plan, t, m);
      if (drawerOpen && mq(PHONE_MQ)) closeDrawer();
    }

    // ── Drawer ──
    function fillDrawer() {
      while (list.firstChild) list.removeChild(list.firstChild);
      rows = [];
      var s = settings(), n = 0;
      for (var i = 0; i < history.length; i++) {
        if (!kindShown(history[i].kind, s)) continue;
        var r = { m: history[i], time: null };
        list.appendChild(buildEntry(history[i], 'li', 'live-moment-row', activate, r));
        rows.push(r);
        n++;
      }
      if (!n) list.appendChild(el('li', 'live-moments-empty', 'No moments yet'));
    }
    function openDrawer() {
      if (drawerOpen || destroyed) return;
      drawerOpen = true;
      fillDrawer();
      drawer.hidden = false;
      backdrop.hidden = false;
      drawerBtn.setAttribute('aria-expanded', 'true');
      keyHandler = function (e) { if (e.key === 'Escape') closeDrawer(); };
      doc.addEventListener('keydown', keyHandler);
      armTimes();
      // preventScroll: on phones the hidden node-detail sheet sits off to
      // the right, so .live-page is scrollable and a plain focus() can
      // shift the whole page sideways.
      try { closeBtn.focus({ preventScroll: true }); } catch (_) {}
    }
    function closeDrawer() {
      if (!drawerOpen) return;
      drawerOpen = false;
      drawer.hidden = true;
      backdrop.hidden = true;
      drawerBtn.setAttribute('aria-expanded', 'false');
      if (keyHandler) { doc.removeEventListener('keydown', keyHandler); keyHandler = null; }
      while (list.firstChild) list.removeChild(list.firstChild);
      rows = [];
    }
    drawerBtn.setAttribute('aria-expanded', 'false');
    drawerBtn.addEventListener('click', function () { if (drawerOpen) closeDrawer(); else openDrawer(); });
    closeBtn.addEventListener('click', closeDrawer);
    backdrop.addEventListener('click', closeDrawer);
    pauseBtns.forEach(function (b) { b.addEventListener('click', function () { setPaused(!paused); }); });

    // ── Ingest ──
    function remember(id) {
      if (seen.has(id)) return false;
      seen.add(id); seenOrder.push(id);
      if (seenOrder.length > SEEN_MAX) seen.delete(seenOrder.shift());
      return true;
    }
    // tell hands a valid moment to the kiosk tiles; their bug is not ours.
    function tell(m) {
      if (!deps.onMoment) return;
      try { deps.onMoment(m); } catch (_) { /* never break the cards */ }
    }
    function addHistory(m) {
      history.unshift(m);
      if (history.length > DRAWER_LEN) history.length = DRAWER_LEN;
    }

    // ingest takes one raw moment from the WS: history always, a card only
    // when the stream is showing.
    function ingest(raw) {
      if (destroyed) return false;
      var m = normalize(raw);
      if (m) tell(m);
      if (!m || !kindShown(m.kind, settings()) || !remember(m.id)) return false;
      addHistory(m);
      if (canShow()) {
        queue.push(m);
        if (queue.length > QUEUE_MAX) queue.shift();
      }
      scheduleRender();
      return true;
    }

    // seed merges the REST history (newest first) into the drawer only.
    function seed(list) {
      if (destroyed || !Array.isArray(list)) return;
      var add = [], s = settings();
      for (var i = 0; i < list.length && add.length < DRAWER_LEN; i++) {
        var m = normalize(list[i]);
        if (m) tell(m);
        if (m && kindShown(m.kind, s) && remember(m.id)) add.push(m);
      }
      // WS frames that raced the fetch are newer; keep them first.
      history = history.concat(add).slice(0, DRAWER_LEN);
      scheduleRender();
    }

    var wsHandler = function (msg) {
      if (!msg || msg.type !== 'moment') return;
      try { ingest(msg.data); } catch (_) { /* never break app.js's fan-out */ }
    };
    deps.onWS(wsHandler);

    // The history: once at create, and again on reseed() (#883 M3: kiosk
    // calls it on every socket open; seed() drops what is already shown).
    function loadHistory() {
      if (!deps.fetchJSON || destroyed) return;
      Promise.resolve()
        .then(function () { return destroyed ? null : deps.fetchJSON('/api/moments?limit=' + DRAWER_LEN, abort ? abort.signal : undefined); })
        .then(function (body) { if (body && !destroyed) seed(body.moments); })
        .catch(function () { /* drawer starts empty */ });
    }
    loadHistory();

    function setPaused(v) {
      paused = !!v;
      setPauseLabel();
      if (paused) dropVisible();
      scheduleRender();
    }
    function dropVisible() {
      queue.length = 0;
      while (cards.length) leave(cards[0], true);
    }
    // onModeChange: leaving LIVE clears the cards; moments keep going to
    // history (VCR PAUSED / REPLAY).
    function onModeChange() {
      if (deps.getMode && deps.getMode() !== 'LIVE') dropVisible();
      scheduleRender();
    }

    // onSettingsChanged: the customizer changed a value. Cards and queued
    // moments of a kind now hidden go; the stack re-fits the new cap, and an
    // open drawer re-lists. Dwell applies from the next card.
    function onSettingsChanged() {
      if (destroyed) return;
      var s = settings();
      queue = queue.filter(function (m) { return kindShown(m.kind, s); });
      cards.slice().forEach(function (c) { if (!kindShown(c.m.kind, s)) leave(c, true); });
      pump();
      if (drawerOpen) fillDrawer();
      scheduleRender();
    }

    function destroy() {
      if (destroyed) return;
      destroyed = true;
      liveCount--;
      deps.offWS(wsHandler);
      if (abort) { try { abort.abort(); } catch (_) {} }
      closeDrawer();
      timers.forEach(function (id) { deps.clearTimeout(id); });
      timers.clear();
      renderTimer = timeTimer = null;
      cards.length = 0; queue.length = 0; history = []; rows = [];
      [wrap, backdrop, drawer].forEach(function (e) { if (e.parentNode) e.parentNode.removeChild(e); });
    }

    render();

    return {
      ingest: ingest,
      seed: seed,
      reseed: loadHistory,
      setPaused: setPaused,
      onModeChange: onModeChange,
      onSettingsChanged: onSettingsChanged,
      openDrawer: openDrawer,
      closeDrawer: closeDrawer,
      refreshTimes: refreshTimes,
      destroy: destroy,
      wsHandler: wsHandler,
      // Test seam: counts, never the elements.
      stats: function () {
        return { visible: cards.length, queued: queue.length, history: history.length, timers: timers.size,
          paused: paused, drawerOpen: drawerOpen, keyListener: !!keyHandler, destroyed: destroyed };
      }
    };
  }

  window.LiveMoments = {
    STORAGE_KEY: STORAGE_KEY,
    HASH_PARAM: HASH_PARAM,
    SETTINGS: SETTINGS,
    DWELL_MS: SETTINGS.DEFAULTS.dwellSeconds * 1000, // the default dwell
    KIND_SETTING: KIND_SETTING,
    DRAWER_LEN: DRAWER_LEN,
    normalize: normalize,
    describe: describe,
    detailsHref: detailsHref,
    flyTarget: flyTarget,
    flyPlan: flyPlan,
    relTime: relTime,
    resolveInitialEnabled: resolveInitialEnabled,
    visibleCap: visibleCap,
    create: create,
    liveCount: function () { return liveCount; }
  };
})();
