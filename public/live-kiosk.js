/* === CoreScope — live-kiosk.js === */
/* #883 M1 — Live TV/kiosk mode: #/live?kiosk=1&tour=<areaKey>,…&every=60.
 *
 * live.js owns the page, the map and body.kiosk (the CSS that hides the
 * chrome); this module owns the tour, the idle cursor, the start overlay
 * and the exit. Every dependency is injected (create(deps)), so the logic
 * runs in a vm sandbox with a fake DOM and a fake clock
 * (test-883-kiosk-params.js, test-883-kiosk-tour.js).
 *
 * A tour step is an `areas` entry (/api/config/areas/polygons), the only
 * config object with geometry. A step moves the camera only: the packet
 * stream, the nodes and the counters stay whole-mesh. Steps follow the URL
 * (the server's response order is random), at most MAX_STEPS.
 *
 * Hot path: none. Nothing here runs per packet. The tour is one chained
 * setTimeout per step (no setInterval), the areas are fetched once per
 * mount, and a step makes no request. Pointer moves only store a time.
 *
 * Nothing is persisted: kiosk is on by URL only, the tour never writes the
 * hash or localStorage, and exiting strips the kiosk params from the hash.
 *
 * #883 M3, running for days: every 6 h (owner decision 6) the controller
 * asks live.js to remount Live in place (destroy() + init() in the same
 * document, so browser fullscreen stays), at a step boundary with no Moments
 * card up; the step it was due to show carries over to the new mount through
 * module state (a hard reload starts at step 1). The deadline is wall clock,
 * checked by one heartbeat; a heartbeat that runs late means the machine
 * slept, and the tour re-anchors from now. While visible it holds a Screen
 * Wake Lock (re-taken when the browser drops it), re-seeds Moments on every
 * socket (re)open, and turns on version-notice's auto-reload.
 *
 * XSS: area keys and labels come from the operator's config and reach the
 * page through textContent only. No innerHTML.
 */
'use strict';

(function () {
  var EVERY_MIN = 15;          // s; time for a fly plus reading
  var EVERY_MAX = 3600;
  var EVERY_DEFAULT = 60;
  var MAX_STEPS = 12;          // the regionGroups cap
  var IDLE_MS = 3000;          // cursor and exit button hide after this
  var IDLE_MIN_REARM_MS = 250; // the idle timeout re-arms at most this often
  var OVERLAY_MS = 15000;      // the start overlay leaves on its own
  var NOTE_MS = 10000;         // "Skipped: …" stays this long
  var RESUME_MIN_MS = 60000;
  var RESUME_MAX_MS = 300000;
  var REMOUNT_MS_DEFAULT = 6 * 3600000;   // #883 M3, owner decision 6
  var REMOUNT_MS_MIN = 3600000;
  var REMOUNT_MS_MAX = 24 * 3600000;
  var BEAT_MS = 60000;         // the heartbeat: remount deadline, sleep/wake
  var WAKE_GAP_MS = 2 * BEAT_MS; // a heartbeat later than this: the machine slept
  var CARRY_MS = 60000;        // a remount's step carry-over is used within this
  // Kiosk config values for the customizer later (#883 M4, AGENTS.md
  // rule 8): every, the idle seconds, the resume delay and the remount
  // interval.

  // Module state, so it survives an in-place remount (not a reload).
  var carry = null;            // { hash, step, at } from the mount that remounted
  var remountOverrideMs = null; // test-only: the soak test's short interval
  var remounts = 0;

  // ── URL ─────────────────────────────────────────────────────────────────
  // params: URLSearchParams (or anything with get()). Only kiosk=1 turns
  // kiosk on; tour/every are parsed regardless and the caller decides.
  function parseKioskParams(params) {
    var get = function (k) { return params && typeof params.get === 'function' ? params.get(k) : null; };
    var tour = String(get('tour') || '').split(',')
      .map(function (s) { return s.trim(); })
      .filter(Boolean)
      .slice(0, MAX_STEPS);
    var every = parseInt(get('every'), 10);
    if (isNaN(every)) every = EVERY_DEFAULT;
    every = Math.min(EVERY_MAX, Math.max(EVERY_MIN, every));
    return { kiosk: get('kiosk') === '1', tour: tour, every: every };
  }

  // The hash to leave kiosk by: the same route without kiosk/tour/every.
  function stripKioskParams(hash) {
    var h = String(hash || '');
    var q = h.indexOf('?');
    var route = (q === -1 ? h : h.slice(0, q)) || '#/live';
    if (q === -1) return route;
    var params = new URLSearchParams(h.slice(q + 1));
    ['kiosk', 'tour', 'every'].forEach(function (k) { params.delete(k); });
    var rest = params.toString();
    return rest ? route + '?' + rest : route;
  }

  // The remount interval: 6 h unless given, clamped to 1–24 h.
  function remountDelayMs(ms) {
    var v = Number(ms);
    if (!(v > 0)) return REMOUNT_MS_DEFAULT;
    return Math.min(REMOUNT_MS_MAX, Math.max(REMOUNT_MS_MIN, v));
  }

  function resumeAfterMs(everyS) {
    return Math.min(RESUME_MAX_MS, Math.max(RESUME_MIN_MS, 2 * everyS * 1000));
  }

  // ── Geometry ────────────────────────────────────────────────────────────
  function num(v, lim) { return typeof v === 'number' && isFinite(v) && Math.abs(v) <= lim; }

  // [[south, west], [north, east]]. As on the server, a polygon of ≥3
  // points wins, otherwise the full box; null when that gives no
  // non-empty box (a bad point, zero area, a partial or inverted box).
  function areaBounds(a) {
    if (!a || typeof a !== 'object') return null;
    var s, w, n, e;
    if (Array.isArray(a.polygon) && a.polygon.length >= 3) {
      s = 90; w = 180; n = -90; e = -180;
      for (var i = 0; i < a.polygon.length; i++) {
        var p = a.polygon[i];
        if (!Array.isArray(p) || !num(p[0], 90) || !num(p[1], 180)) return null;
        s = Math.min(s, p[0]); n = Math.max(n, p[0]); w = Math.min(w, p[1]); e = Math.max(e, p[1]);
      }
    } else if (num(a.latMin, 90) && num(a.latMax, 90) && num(a.lonMin, 180) && num(a.lonMax, 180)) {
      s = a.latMin; n = a.latMax; w = a.lonMin; e = a.lonMax;
    } else {
      return null;
    }
    return n > s && e > w ? [[s, w], [n, e]] : null;
  }

  // keys (URL order) → steps. Exact key, then case-insensitive key (the
  // smallest key on a tie, so the random response order cannot matter),
  // never the label. Unknown keys and unusable geometry are skipped.
  function resolveSteps(keys, areas) {
    var out = { steps: [], skipped: [] };
    if (!Array.isArray(areas)) return out;
    var byKey = {}, byLower = {};
    areas.forEach(function (a) {
      if (!a || typeof a.key !== 'string' || !a.key) return;
      byKey[a.key] = a;
      var low = a.key.toLowerCase();
      if (!byLower[low] || a.key < byLower[low].key) byLower[low] = a;
    });
    (keys || []).forEach(function (k) {
      var a = Object.prototype.hasOwnProperty.call(byKey, k) ? byKey[k] : byLower[String(k).toLowerCase()];
      if (!a) { out.skipped.push({ key: k, reason: 'unknown' }); return; }
      var bounds = areaBounds(a);
      if (!bounds) { out.skipped.push({ key: k, reason: 'geometry' }); return; }
      out.steps.push({ key: a.key, label: (typeof a.label === 'string' && a.label) || a.key, bounds: bounds });
    });
    return out;
  }

  // ── Tour scheduler ──────────────────────────────────────────────────────
  /* o: { count, everyMs, resumeAfterMs, show(i), setTimeout, clearTimeout, now }
   * One chained setTimeout per step, its delay taken from now(). A fire that
   * missed the next boundary (sleep, a throttled tab; the plan's "late by
   * more than 2 × every" is one such case) re-anchors to now instead of
   * catching up, so no step is skipped or shown for 0 s. Paused while any
   * reason holds: interaction, hidden, mode (not LIVE). The end of a pause
   * resumes at the next step with a full step ahead. 0 or 1 steps arm no
   * timer at all.
   */
  function createTour(o) {
    var n = o.count, every = o.everyMs, resumeAfter = o.resumeAfterMs;
    var index = -1, stepTimer = null, resumeTimer = null, nextAt = 0, lastInteraction = 0;
    var reasons = { interaction: false, hidden: false, mode: false };
    var started = false, stopped = false, reanchors = 0;

    function paused() { return reasons.interaction || reasons.hidden || reasons.mode; }
    function clearStep() { if (stepTimer != null) { o.clearTimeout(stepTimer); stepTimer = null; } }
    function clearResume() { if (resumeTimer != null) { o.clearTimeout(resumeTimer); resumeTimer = null; } }
    function go(i) { index = i; o.show(i); }
    function arm(at) {
      clearStep();
      if (n < 2 || stopped || paused()) return;
      nextAt = at;
      stepTimer = o.setTimeout(fire, Math.max(0, at - o.now()));
    }
    function fire() {
      stepTimer = null;
      if (stopped || paused()) return;
      var now = o.now();
      var next = nextAt + every;
      if (next <= now) { next = now + every; reanchors++; }
      go((index + 1) % n);
      arm(next);
    }
    function resume() {
      if (!started || stopped || n < 2 || paused()) return;
      go((index + 1) % n);
      arm(o.now() + every);
    }
    function armResume(ms) {
      clearResume();
      resumeTimer = o.setTimeout(function () {
        resumeTimer = null;
        var quiet = o.now() - lastInteraction;
        if (quiet < resumeAfter) { armResume(resumeAfter - quiet); return; }
        reasons.interaction = false;
        resume();
      }, ms);
    }

    return {
      // first: the step to start at (an in-place remount carries it over).
      start: function (first) {
        if (started || stopped) return;
        started = true;
        if (n === 0) return;
        go(first > 0 && first < n ? first : 0);
        arm(o.now() + every);
      },
      // After a sleep: a step overdue by the wall clock moves on now and the
      // next one is a full step later. A step that already re-anchored itself
      // (it fired late) is left alone, so a wake is never two moves.
      reanchor: function () {
        if (!started || stopped || n < 2 || paused() || stepTimer == null || nextAt > o.now()) return;
        reanchors++;
        go((index + 1) % n);
        arm(o.now() + every);
      },
      // An interaction pauses; quiet for resumeAfter resumes. One timer,
      // whatever the event rate: a later event only moves the time.
      interact: function () {
        if (!started || stopped || n < 2 || reasons.hidden) return;
        lastInteraction = o.now();
        if (reasons.interaction) return;
        reasons.interaction = true;
        clearStep();
        armResume(resumeAfter);
      },
      setHidden: function (hidden) {
        if (stopped || !!hidden === reasons.hidden) return;
        reasons.hidden = !!hidden;
        if (hidden) { clearStep(); clearResume(); reasons.interaction = false; }
        else resume();
      },
      setBlocked: function (blocked) {
        if (stopped || !!blocked === reasons.mode) return;
        reasons.mode = !!blocked;
        if (blocked) clearStep();
        else resume();
      },
      destroy: function () { stopped = true; clearStep(); clearResume(); },
      stats: function () {
        return { step: index, steps: n, paused: paused(), reasons: Object.keys(reasons).filter(function (k) { return reasons[k]; }),
          reanchors: reanchors, timers: (stepTimer != null ? 1 : 0) + (resumeTimer != null ? 1 : 0) };
      }
    };
  }

  // ── Controller ──────────────────────────────────────────────────────────
  /* deps:
   *   doc, win            document / window (window only for ws-open)
   *   root                element the UI is appended to (.live-page)
   *   map                 { on(type, fn), off(type, fn) } (Leaflet), optional
   *   params              parseKioskParams() result
   *   fetchAreas()        → Promise<[{key,label,polygon?,latMin?,…}]>
   *   fly(bounds, {animate})
   *   reducedMotion()     bool
   *   getMode()           'LIVE' | 'PAUSED' | 'REPLAY' | 'FOLLOW'
   *   requestFullscreen() → Promise (must run inside the gesture)
   *   exitFullscreen()    → Promise
   *   exit()              leave kiosk (live.js navigates)
   *   warn(msg)
   *   now(), setTimeout, clearTimeout
   * #883 M3, each optional (an M1 host passes none):
   *   hash()              the current location.hash (the step carry-over)
   *   remount()           remount Live in place (live.js: the router)
   *   momentsVisible()    bool: a Moments card is on screen
   *   reseed()            re-seed Moments (on every socket open)
   *   wakeLock            navigator.wakeLock, or null (plain http)
   *   setAutoReload(on)   version-notice's kiosk auto-reload
   *   remountMs           the interval (M4 config), clamped to 1–24 h
   */
  var liveCount = 0; // controllers not yet destroyed (leak check)

  function create(deps) {
    var doc = deps.doc;
    var params = deps.params || parseKioskParams(null);
    var destroyed = false;
    var timers = new Set();
    var listeners = [];        // [target, type, fn, opts]
    var ui = [];               // nodes this controller appended
    var tour = null;
    var steps = [];
    var skipped = [];
    var fetches = 0;
    var ownMove = false;       // true while the tour itself moves the map
    var idleTimer = null, lastMove = 0, idle = false;
    var overlay = null, overlayTimer = null;
    var stepEl = null, labelEl = null, countEl = null, progressEl = null;
    var exitBtn = null;
    var retryOff = null;
    // #883 M3
    var mountedAt = deps.now();
    var mountHash = deps.hash ? deps.hash() : null;
    var remountMs = remountDelayMs(deps.remountMs);
    var remounting = false;
    var beating = !!(deps.remount || deps.wakeLock);
    var beatTimer = null, lastBeat = mountedAt, wakes = 0;
    var lock = null, lockOff = null, lockPending = false;
    var reseeds = 0;
    var startStep = 0;
    // An in-place remount of this same kiosk link: carry the step over, and
    // do not offer the start overlay or the skipped note again (they were
    // shown at the start; an unattended screen would get them every 6 h).
    var fromRemount = !!(carry && mountHash != null && carry.hash === mountHash && mountedAt - carry.at < CARRY_MS);
    if (fromRemount) startStep = carry.step;
    carry = null;
    liveCount++;

    function later(fn, ms) {
      var id = deps.setTimeout(function () { timers.delete(id); if (!destroyed) fn(); }, ms);
      timers.add(id);
      return id;
    }
    function cancel(id) {
      if (id == null) return null;
      deps.clearTimeout(id);
      timers.delete(id);
      return null;
    }
    function listen(target, type, fn, opts) {
      if (!target) return function () {};
      var rec = [target, type, fn, opts];
      if (target.addEventListener) target.addEventListener(type, fn, opts);
      else target.on(type, fn);
      listeners.push(rec);
      return function () {
        var i = listeners.indexOf(rec);
        if (i === -1) return;
        listeners.splice(i, 1);
        unbind(rec);
      };
    }
    function unbind(rec) {
      if (rec[0].removeEventListener) rec[0].removeEventListener(rec[1], rec[2], rec[3]);
      else rec[0].off(rec[1], rec[2]);
    }
    function el(tag, cls, text) {
      var e = doc.createElement(tag);
      if (cls) e.className = cls;
      if (text != null) e.textContent = text;
      return e;
    }
    function add(node) { deps.root.appendChild(node); ui.push(node); return node; }
    function drop(node) {
      if (!node) return null;
      if (node.parentNode) node.parentNode.removeChild(node);
      var i = ui.indexOf(node);
      if (i !== -1) ui.splice(i, 1);
      return null;
    }
    function inUI(t) {
      if (!t) return false;
      for (var i = 0; i < ui.length; i++) if (ui[i] === t || (ui[i].contains && ui[i].contains(t))) return true;
      return false;
    }
    function isTyping(t) {
      var tag = t && t.tagName ? String(t.tagName).toUpperCase() : '';
      return tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT' || !!(t && t.isContentEditable);
    }
    function settle(p) { if (p && typeof p.then === 'function') p.then(null, function () {}); }

    // ── Idle cursor and exit button ───────────────────────────────────────
    function setIdle(on) {
      idle = !!on;
      if (doc.body) doc.body.classList.toggle('kiosk-idle', idle);
    }
    function idleCheck() {
      idleTimer = null;
      var quiet = deps.now() - lastMove;
      if (quiet >= IDLE_MS) { setIdle(true); return; }
      idleTimer = later(idleCheck, Math.max(IDLE_MIN_REARM_MS, IDLE_MS - quiet));
    }
    // A move (or a key) shows the cursor and the exit button for IDLE_MS.
    function wake() {
      lastMove = deps.now();
      if (idle) setIdle(false);
      if (idleTimer == null && !doc.hidden) idleTimer = later(idleCheck, IDLE_MS);
    }

    // ── Start overlay (the Fullscreen API needs a user gesture) ───────────
    function goFullscreen() {
      closeOverlay();
      try { settle(deps.requestFullscreen()); } catch (_) { /* kiosk works without it */ }
    }
    function closeOverlay() {
      overlayTimer = cancel(overlayTimer);
      overlay = drop(overlay);
    }
    function openOverlay() {
      if (doc.fullscreenElement || !doc.fullscreenEnabled) return;
      overlay = add(el('div', 'live-kiosk-overlay'));
      var btn = el('button', 'live-kiosk-start');
      btn.type = 'button';
      btn.appendChild(el('span', 'live-kiosk-start-title', 'Start fullscreen'));
      btn.appendChild(el('span', 'live-kiosk-start-hint', 'Click or press Enter · Esc twice to exit kiosk'));
      listen(btn, 'click', goFullscreen);
      overlay.appendChild(btn);
      overlayTimer = later(closeOverlay, OVERLAY_MS);
      try { btn.focus(); } catch (_) {}
    }

    // ── Step label ────────────────────────────────────────────────────────
    function buildStepUI() {
      stepEl = add(el('div', 'live-kiosk-step'));
      stepEl.setAttribute('aria-live', 'polite');
      labelEl = stepEl.appendChild(el('div', 'live-kiosk-step-label'));
      countEl = stepEl.appendChild(el('div', 'live-kiosk-step-count'));
    }
    function showStep(i) {
      var s = steps[i];
      if (!s) return;
      // #883 M3: a due remount takes this boundary; the new mount shows i.
      if (remountNow(i)) return;
      labelEl.textContent = s.label;
      countEl.textContent = steps.length > 1 ? (i + 1) + ' / ' + steps.length : '';
      var reduced = !!(deps.reducedMotion && deps.reducedMotion());
      // The bar restarts its CSS animation by being replaced: once a step.
      if (progressEl && progressEl.parentNode) progressEl.parentNode.removeChild(progressEl);
      progressEl = null;
      if (!reduced && steps.length > 1) {
        progressEl = stepEl.appendChild(el('div', 'live-kiosk-progress'));
        progressEl.style.animationDuration = params.every + 's';
      }
      ownMove = true;
      try { deps.fly(s.bounds, { animate: !reduced }); } catch (_) { /* a map mid-teardown */ }
      finally { ownMove = false; }
    }
    function syncPaused() {
      if (stepEl && tour) stepEl.classList.toggle('is-paused', tour.stats().paused);
    }

    function startTour(areas) {
      var r = resolveSteps(params.tour, areas);
      steps = r.steps;
      var seen = {};
      skipped = [];
      r.skipped.forEach(function (s) {
        if (seen[s.key]) return;
        seen[s.key] = true;
        skipped.push(s.key);
        if (deps.warn) deps.warn('[kiosk] tour step skipped (' + s.reason + '): ' + s.key);
      });
      if (skipped.length && !fromRemount) {
        var note = add(el('div', 'live-kiosk-note', 'Skipped: ' + skipped.join(', ')));
        note.setAttribute('role', 'status');
        later(function () { drop(note); }, NOTE_MS);
      }
      if (!steps.length) return;
      buildStepUI();
      tour = createTour({
        count: steps.length, everyMs: params.every * 1000, resumeAfterMs: resumeAfterMs(params.every),
        show: showStep, now: deps.now, setTimeout: later,
        clearTimeout: function (id) { cancel(id); }
      });
      tour.start(startStep);
      tour.setHidden(!!doc.hidden);
      onModeChange();
    }

    function loadAreas(retry) {
      fetches++;
      var p;
      try { p = deps.fetchAreas(); } catch (e) { p = Promise.reject(e); }
      Promise.resolve(p).then(function (areas) {
        if (destroyed) return;
        if (!Array.isArray(areas)) throw new Error('areas: not a list');
        startTour(areas);
      }).then(null, function () {
        // No steps: a static view. One retry, on the next socket open.
        if (destroyed || retry) return;
        retryOff = listen(deps.win, 'corescope:ws-open', function () {
          if (retryOff) { retryOff(); retryOff = null; }
          loadAreas(true);
        });
      });
    }

    // ── In-place remount, heartbeat, wake lock (#883 M3) ──────────────────
    function remountEvery() { return remountOverrideMs > 0 ? remountOverrideMs : remountMs; }
    function remountDue() {
      return !!deps.remount && deps.now() - mountedAt >= remountEvery();
    }
    // Remount now if due and no Moments card is up (a card is somebody's
    // moment; the next boundary will do). step: what the new mount shows.
    function remountNow(step) {
      if (remounting) return true;
      if (!remountDue() || (deps.momentsVisible && deps.momentsVisible())) return false;
      remounting = true;
      remounts++;
      carry = { hash: mountHash, step: step, at: deps.now() };
      // Out of the tour's own call stack: the router destroys this controller.
      later(function () { deps.remount(); }, 0);
      return true;
    }
    // A tour that is stepping remounts at its next boundary (showStep). A
    // static view, or a single step, remounts here. A paused tour (someone
    // is using the screen, the VCR is out of LIVE) waits for its resume.
    function remountIdle() {
      if (tour && tour.stats().steps >= 2) return;
      remountNow(tour ? Math.max(0, tour.stats().step) : 0);
    }
    function beat() {
      beatTimer = null;
      var t = deps.now();
      if (t - lastBeat > WAKE_GAP_MS) onWake();
      lastBeat = t;
      remountIdle();
      armBeat();
    }
    function armBeat() {
      if (!beating || destroyed || doc.hidden || beatTimer != null) return;
      lastBeat = deps.now();
      beatTimer = later(beat, BEAT_MS);
    }
    // The machine slept (its timers did not run): move the tour on from now
    // and take the lock again (an OS sleep drops it, maybe with no
    // visibilitychange). #883 M2's numbers refresh joins here.
    function onWake() {
      wakes++;
      if (tour) { tour.reanchor(); syncPaused(); }
      acquireLock();
    }
    function acquireLock() {
      var wl = deps.wakeLock;
      if (destroyed || lock || lockPending || doc.hidden || !wl || typeof wl.request !== 'function') return;
      lockPending = true;
      var p;
      try { p = wl.request('screen'); } catch (e) { p = Promise.reject(e); }
      Promise.resolve(p).then(function (s) {
        lockPending = false;
        if (!s) return;
        if (destroyed) { try { settle(s.release()); } catch (_) {} return; }
        lock = s;
        // The browser drops it when the page is hidden; re-taken on visible.
        if (s.addEventListener) {
          lockOff = listen(s, 'release', function () {
            if (lockOff) { lockOff(); lockOff = null; }
            if (lock === s) lock = null;
          });
        }
      }, function () { lockPending = false; /* denied, or no secure context: next visible */ });
    }
    function releaseLock() {
      var s = lock;
      lock = null;
      if (s) { try { settle(s.release()); } catch (_) {} }
    }
    function onWsOpen() {
      reseeds++;
      // #883 M2's numbers refresh joins here.
      deps.reseed();
    }

    // ── Page events ───────────────────────────────────────────────────────
    function interact() {
      if (!tour) return;
      tour.interact();
      syncPaused();
    }
    function onPointerDown(e) {
      wake();
      if (!inUI(e && e.target)) interact();
    }
    function onKey(e) {
      if (!e || typeof e.key !== 'string') return;
      if (e.key === 'Escape') {
        // Two steps: browser fullscreen first (its own Esc is native and
        // never reaches the page), then kiosk. preventDefault keeps the
        // #1532 fullscreen toggle from also handling (and persisting) it.
        e.preventDefault();
        if (doc.fullscreenElement) settle(deps.exitFullscreen());
        else deps.exit();
        return;
      }
      if (isTyping(e.target)) return;
      wake();
      if ((e.key === 'f' || e.key === 'F') && !e.altKey && !e.ctrlKey && !e.metaKey) {
        e.preventDefault();
        if (!doc.fullscreenElement) goFullscreen();
        return;
      }
      if (!inUI(e.target)) interact();
    }
    function onVisibility() {
      if (doc.hidden) { idleTimer = cancel(idleTimer); beatTimer = cancel(beatTimer); }
      else wake();
      if (tour) { tour.setHidden(!!doc.hidden); syncPaused(); }
      if (!doc.hidden) { acquireLock(); remountIdle(); armBeat(); }
    }
    function onMapMove() { if (!ownMove) interact(); }
    function onModeChange() {
      if (!tour || destroyed) return;
      tour.setBlocked(deps.getMode() !== 'LIVE');
      syncPaused();
    }

    function destroy() {
      if (destroyed) return;
      destroyed = true;
      liveCount--;
      if (tour) tour.destroy();
      releaseLock();
      if (deps.setAutoReload) deps.setAutoReload(false);
      listeners.splice(0).forEach(unbind);
      timers.forEach(function (id) { deps.clearTimeout(id); });
      timers.clear();
      idleTimer = overlayTimer = beatTimer = null;
      ui.splice(0).forEach(function (n) { if (n.parentNode) n.parentNode.removeChild(n); });
      overlay = stepEl = labelEl = countEl = progressEl = exitBtn = null;
      setIdle(false);
    }

    // ── Mount ─────────────────────────────────────────────────────────────
    exitBtn = add(el('button', 'live-kiosk-exit', 'Exit kiosk'));
    exitBtn.type = 'button';
    listen(exitBtn, 'click', function () { deps.exit(); });
    listen(doc, 'pointerdown', onPointerDown, { capture: true, passive: true });
    listen(doc, 'wheel', interact, { capture: true, passive: true });
    listen(doc, 'pointermove', wake, { passive: true });
    listen(doc, 'keydown', onKey);
    listen(doc, 'visibilitychange', onVisibility);
    if (deps.map) {
      listen(deps.map, 'dragstart', onMapMove);
      listen(deps.map, 'zoomstart', onMapMove);
    }
    if (deps.reseed) listen(deps.win, 'corescope:ws-open', onWsOpen);
    if (deps.setAutoReload) deps.setAutoReload(true);
    if (!fromRemount) openOverlay();
    wake();
    acquireLock();
    armBeat();
    if (params.tour.length) loadAreas(false);

    return {
      destroy: destroy,
      onModeChange: onModeChange,
      // Test seam: counts and state, never the elements.
      stats: function () {
        var t = tour ? tour.stats() : null;
        return { timers: timers.size, listeners: listeners.length, step: t ? t.step : -1, steps: steps.length,
          paused: !!(t && t.paused), reasons: t ? t.reasons : [], reanchors: t ? t.reanchors : 0,
          skipped: skipped.slice(), idle: idle, overlay: !!overlay, fetches: fetches, every: params.every,
          destroyed: destroyed,
          // #883 M3
          beat: beatTimer != null, wakes: wakes, wakeLock: !!lock, reseeds: reseeds,
          remountDue: remountDue(), remounting: remounting, remountInMs: Math.max(0, mountedAt + remountEvery() - deps.now()) };
      }
    };
  }

  window.LiveKiosk = {
    HASH_PARAM: 'kiosk',
    EVERY_MIN: EVERY_MIN,
    EVERY_MAX: EVERY_MAX,
    EVERY_DEFAULT: EVERY_DEFAULT,
    MAX_STEPS: MAX_STEPS,
    IDLE_MS: IDLE_MS,
    OVERLAY_MS: OVERLAY_MS,
    REMOUNT_MS_DEFAULT: REMOUNT_MS_DEFAULT,
    BEAT_MS: BEAT_MS,
    remountDelayMs: remountDelayMs,
    parseKioskParams: parseKioskParams,
    stripKioskParams: stripKioskParams,
    resumeAfterMs: resumeAfterMs,
    areaBounds: areaBounds,
    resolveSteps: resolveSteps,
    createTour: createTour,
    create: create,
    liveCount: function () { return liveCount; },
    // In-place remounts asked for since the page loaded (soak test).
    remounts: function () { return remounts; },
    // Test-only (test-883-kiosk-soak-e2e.js): a short remount interval,
    // unclamped, read on every check; null restores the normal one.
    _setRemountOverrideMs: function (ms) { remountOverrideMs = ms > 0 ? ms : null; }
  };
})();
