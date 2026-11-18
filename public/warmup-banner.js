/*
 * warmup-banner.js — global top-banner / status pill surfacing server warm-up state.
 *
 * Issue: #1660. FE-only sub-deliverables (1)+(3); per-card pill (2) is deferred
 * pending the recomputer.first_pass_done server flag (#1659).
 *
 * Consumes:
 *   - X-Corescope-Load-Status response header ("loading" | "ready"), set by
 *     cmd/server/chunked_load.go on every response.
 *   - GET /api/healthz, polled every 30s while not in steady-state.
 *
 * Renders a role="status" live region so screen readers announce transitions.
 * Auto-dismisses (fades out) once ready=true AND from_pubkey_backfill.done=true.
 *
 * #894: a silent MQTT source is operator information, not a warm-up state. It
 * is shown as a compact dismissible pill in its own row right after the top
 * nav (#warmup-stale-pills, normal flow: it never overlays page content);
 * polling continues while one is shown so it updates and clears by itself.
 */
(function () {
  'use strict';

  var STALE_INGEST_MS = 5 * 60 * 1000; // 5 min — matches acceptance criteria
  var POLL_INTERVAL_MS = 30 * 1000;    // 30s while warming up

  // -------- Pure helpers (testable in isolation) ----------------------------

  function fmtNum(n) {
    try { return Number(n).toLocaleString('en-US'); }
    catch (e) { return String(n); }
  }

  /**
   * Warm-up messages only (loading + pubkey backfill). These keep the
   * full-width banner exactly as before (#1660). Stale-source text is NOT
   * included — see getStaleSources() (#894).
   *
   * @param {object|null} healthz   - parsed /api/healthz body, or null when unknown
   * @param {string|null} loadStatus - last seen X-Corescope-Load-Status header value
   * @param {number} nowMs           - unused; kept so callers share one signature
   * @returns {string[]} ordered list of messages; empty when steady-state.
   */
  function getWarmupOnlyMessages(healthz, loadStatus, nowMs) {
    var msgs = [];
    var h = healthz || {};
    var backfill = h.from_pubkey_backfill || null;

    var loading = loadStatus === 'loading' || h.ready === false;
    if (loading) {
      msgs.push('\u23F3 Loading historical data \u2014 counts may be incomplete.');
    }

    if (backfill && backfill.done === false) {
      var processed = Number(backfill.processed) || 0;
      var total = Number(backfill.total) || 0;
      // Clamp pct: total=0 → 0%; processed>total (race) → 100%.
      // Never NaN, never >100%.
      var rawPct = total > 0 ? Math.floor((processed / total) * 100) : 0;
      var pct = Math.max(0, Math.min(100, rawPct));
      msgs.push('Backfilling pubkey index: ' + fmtNum(processed) +
        ' / ' + fmtNum(total) + ' (' + pct + '%)');
    }
    return msgs;
  }

  /**
   * MQTT sources silent for longer than STALE_INGEST_MS (#894: shown as a
   * compact dismissible pill, not in the banner). Sorted by source name.
   *
   * @returns {{source:string, ageMin:number, text:string}[]}
   */
  function getStaleSources(healthz, nowMs) {
    var out = [];
    var liveness = (healthz && healthz.ingest_liveness) || {};
    var srcs = Object.keys(liveness).sort();
    for (var i = 0; i < srcs.length; i++) {
      var src = srcs[i];
      var info = liveness[src] || {};
      var lastUnix = Number(info.lastReceiptUnix);
      if (!lastUnix || !isFinite(lastUnix)) continue;
      var ageMs = nowMs - lastUnix * 1000;
      if (ageMs > STALE_INGEST_MS) {
        var ageMin = Math.floor(ageMs / 60000);
        out.push({ source: src, ageMin: ageMin,
          text: 'No packets from ' + src + ' in ' + ageMin + ' min.' });
      }
    }
    return out;
  }

  /** Warm-up messages followed by stale-source messages (pre-#894 shape). */
  function getWarmupMessages(healthz, loadStatus, nowMs) {
    var msgs = getWarmupOnlyMessages(healthz, loadStatus, nowMs);
    var stale = getStaleSources(healthz, nowMs);
    for (var i = 0; i < stale.length; i++) msgs.push(stale[i].text);
    return msgs;
  }

  /** True when the full-width banner is shown: warm-up only (#894 F5). */
  function shouldShowBanner(healthz, loadStatus, nowMs) {
    return getWarmupOnlyMessages(healthz, loadStatus, nowMs).length > 0;
  }

  /**
   * Steady-state predicate: ready=true AND from_pubkey_backfill.done=true.
   * Once true, banner is dismissed and polling is torn down.
   */
  function isSteadyState(healthz) {
    if (!healthz) return false;
    if (healthz.ready !== true) return false;
    var bf = healthz.from_pubkey_backfill;
    if (bf && bf.done === false) return false;
    return true;
  }

  /**
   * Polling decision: keep going until the server is steady AND no source is
   * stale, so a stale pill updates and clears by itself (#894).
   */
  function shouldKeepPolling(healthz, loadStatus, nowMs) {
    if (!isSteadyState(healthz) || loadStatus === 'loading') return true;
    return getStaleSources(healthz, nowMs).length > 0;
  }

  /** Drop dismissals for sources that are no longer stale (recovery reset). */
  function pruneDismissed(dismissed, staleList) {
    var out = {};
    for (var i = 0; i < staleList.length; i++) {
      var s = staleList[i].source;
      if (dismissed && dismissed[s]) out[s] = true;
    }
    return out;
  }

  // -------- Browser integration (mount + poll + fetch intercept) ------------

  var state = {
    healthz: null,
    loadStatus: null,
    el: null,
    listEl: null,
    timer: null,
    inflight: false,
    mounted: false,
    pillsEl: null,   // #894 container for stale-source pills
    announceEl: null, // #894 live region: announces a pill being added only
    reservePx: 0,     // last --warmup-pills-h written
    pills: {},       // source -> { el, textEl }
    moreEl: null,    // "+N more" overflow note
    dismissed: null, // source -> true; lazily loaded from sessionStorage
  };

  var DISMISS_KEY = 'corescope-warmup-dismissed';
  var MAX_PILLS = 3; // rule 0: bounded DOM however many sources go silent

  function loadDismissed() {
    if (state.dismissed) return state.dismissed;
    var d = {};
    try {
      var raw = window.sessionStorage.getItem(DISMISS_KEY);
      var parsed = raw ? JSON.parse(raw) : null;
      if (parsed && typeof parsed === 'object') {
        var keys = Object.keys(parsed);
        for (var i = 0; i < keys.length; i++) if (parsed[keys[i]]) d[keys[i]] = true;
      }
    } catch (e) { /* storage blocked/corrupt — in-memory only */ }
    state.dismissed = d;
    return d;
  }

  function saveDismissed() {
    try { window.sessionStorage.setItem(DISMISS_KEY, JSON.stringify(state.dismissed || {})); }
    catch (e) { /* blocked — in-memory only */ }
  }

  function ensureMounted() {
    if (state.mounted || typeof document === 'undefined') return;
    var body = document.body;
    if (!body) return;
    var el = document.createElement('div');
    el.id = 'warmup-banner';
    el.setAttribute('role', 'status');
    el.setAttribute('aria-live', 'polite');
    el.setAttribute('aria-atomic', 'true');
    el.className = 'warmup-banner warmup-banner--hidden';
    var inner = document.createElement('div');
    inner.className = 'warmup-banner__inner';
    var list = document.createElement('ul');
    list.className = 'warmup-banner__list';
    inner.appendChild(list);
    el.appendChild(inner);
    body.insertBefore(el, body.firstChild);
    state.el = el;
    state.listEl = list;
    // #894: stale sources get compact dismissible pills, not the banner.
    // The row sits in normal flow right after the top nav, so it takes its
    // own row and never overlays page content.
    var pills = document.createElement('div');
    pills.id = 'warmup-stale-pills';
    pills.className = 'warmup-pills warmup-pills--hidden';
    var nav = null;
    try { nav = document.querySelector('.top-nav'); } catch (e) { /* minimal DOM */ }
    if (nav && nav.parentNode) nav.parentNode.insertBefore(pills, nav.nextSibling);
    else body.appendChild(pills);
    state.pillsEl = pills;
    // The pills are not a live region: their minute count changes on every
    // poll. Only a newly added pill is announced, through this element.
    var announce = document.createElement('div');
    announce.id = 'warmup-stale-announce';
    announce.className = 'sr-only';
    announce.setAttribute('role', 'status');
    announce.setAttribute('aria-live', 'polite');
    body.appendChild(announce);
    state.announceEl = announce;
    if (typeof ResizeObserver === 'function') {
      // border-box: the /live nav clearance is padding (live.css).
      try { new ResizeObserver(syncPillsReserve).observe(pills, { box: 'border-box' }); } catch (e) { /* render-time sync only */ }
    }
    state.mounted = true;
  }

  // Fixed-height pages (#app.app-fixed, /live) subtract the row's height via
  // --warmup-pills-h so the row never pushes them under the bottom nav.
  // live.js sizes its page in JS, so a change is also announced as an event.
  function syncPillsReserve() {
    try {
      var h = state.pillsEl ? Math.ceil(state.pillsEl.offsetHeight || 0) : 0;
      if (h === state.reservePx) return;
      state.reservePx = h;
      document.documentElement.style.setProperty('--warmup-pills-h', h + 'px');
      window.dispatchEvent(new CustomEvent('warmup-pills-resize'));
    } catch (e) { /* no layout (tests) */ }
  }

  function dismissSource(source) {
    loadDismissed()[source] = true;
    saveDismissed();
    renderPills();
  }

  function makePill(source) {
    var el = document.createElement('div');
    el.className = 'warmup-pill';
    var textEl = document.createElement('span');
    textEl.className = 'warmup-pill__text';
    var btn = document.createElement('button');
    btn.setAttribute('type', 'button');
    btn.className = 'warmup-pill__close';
    btn.setAttribute('aria-label', 'Dismiss: no packets from ' + source);
    btn.textContent = '\u00d7';
    btn.addEventListener('click', function () { dismissSource(source); });
    el.appendChild(textEl);
    el.appendChild(btn);
    return { el: el, textEl: textEl };
  }

  function renderPills() {
    if (!state.pillsEl) return;
    var stale = getStaleSources(state.healthz, Date.now());
    // Recovery forgets the dismissal, so a later silence shows the pill again.
    // Only once healthz is known: "no data yet" is not "recovered".
    var before = Object.keys(loadDismissed()).length;
    if (state.healthz) {
      state.dismissed = pruneDismissed(state.dismissed, stale);
      if (Object.keys(state.dismissed).length !== before) saveDismissed();
    }

    var visible = [];
    for (var i = 0; i < stale.length; i++) {
      if (!state.dismissed[stale[i].source]) visible.push(stale[i]);
    }
    var shown = visible.slice(0, MAX_PILLS);
    var keep = {};
    for (var j = 0; j < shown.length; j++) keep[shown[j].source] = true;

    Object.keys(state.pills).forEach(function (src) {
      if (keep[src]) return;
      var p = state.pills[src];
      if (p.el.parentNode) p.el.parentNode.removeChild(p.el);
      delete state.pills[src];
    });
    var added = [];
    for (var k = 0; k < shown.length; k++) {
      var s = shown[k];
      var pill = state.pills[s.source];
      if (!pill) {
        pill = state.pills[s.source] = makePill(s.source);
        state.pillsEl.appendChild(pill.el);
        added.push(s.text);
      }
      if (pill.text !== s.text) { pill.textEl.textContent = s.text; pill.text = s.text; }
    }

    var extra = visible.length - shown.length;
    if (extra > 0) {
      if (!state.moreEl) {
        state.moreEl = document.createElement('div');
        state.moreEl.className = 'warmup-pill__more';
      }
      state.moreEl.textContent = '+' + extra + ' more';
      if (!state.moreEl.parentNode) state.pillsEl.appendChild(state.moreEl);
    } else if (state.moreEl && state.moreEl.parentNode) {
      state.moreEl.parentNode.removeChild(state.moreEl);
    }

    if (shown.length === 0) {
      state.pillsEl.classList.add('warmup-pills--hidden');
    } else {
      state.pillsEl.classList.remove('warmup-pills--hidden');
    }
    if (added.length && state.announceEl) state.announceEl.textContent = added.join(' ');
    syncPillsReserve();
  }

  function render() {
    if (!state.mounted) ensureMounted();
    if (!state.el || !state.listEl) return;
    var now = Date.now();
    var msgs = getWarmupOnlyMessages(state.healthz, state.loadStatus, now);
    renderPills();
    if (msgs.length === 0) {
      // Fade out.
      state.el.classList.add('warmup-banner--hidden');
      state.listEl.innerHTML = '';
      return;
    }
    // Diff-render: rebuild list (always small — <=3 items).
    var html = '';
    for (var i = 0; i < msgs.length; i++) {
      var safe = String(msgs[i])
        .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
      html += '<li class="warmup-banner__item">' + safe + '</li>';
    }
    state.listEl.innerHTML = html;
    state.el.classList.remove('warmup-banner--hidden');
  }

  function noteLoadStatus(value) {
    if (!value) return;
    if (value !== 'loading' && value !== 'ready') return;
    if (state.loadStatus === value) return;
    state.loadStatus = value;
    render();
    // First time we observe "loading" — make sure polling is on.
    if (value === 'loading') startPolling();
  }

  function pollOnce() {
    if (state.inflight || typeof fetch !== 'function') return;
    state.inflight = true;
    fetch('/api/healthz', { credentials: 'same-origin', cache: 'no-store' })
      .then(function (resp) {
        if (resp && resp.headers && resp.headers.get) {
          noteLoadStatus(resp.headers.get('X-Corescope-Load-Status'));
        }
        if (!resp || !resp.ok) return null;
        return resp.json();
      })
      .then(function (body) {
        if (body) state.healthz = body;
        render();
        // #894: a shown stale-source pill keeps polling so it updates and
        // clears by itself; stop only when steady and nothing is stale.
        if (shouldKeepPolling(state.healthz, state.loadStatus, Date.now())) {
          ensureTimer();
        } else {
          stopPolling();
        }
      })
      .catch(function () { /* swallow — banner is best-effort */ })
      .then(function () { state.inflight = false; });
  }

  function ensureTimer() {
    if (!state.timer) state.timer = setInterval(pollOnce, POLL_INTERVAL_MS);
  }

  function startPolling() {
    if (state.timer) return;
    pollOnce();
    ensureTimer();
  }

  function stopPolling() {
    if (state.timer) {
      clearInterval(state.timer);
      state.timer = null;
    }
  }

  function installFetchInterceptor() {
    if (typeof window === 'undefined' || typeof window.fetch !== 'function') return;
    // Double-install guard: check both the module flag AND a marker stamped
    // onto the wrapper itself. Prevents nested wrap if window.fetch was
    // reassigned externally between installs.
    if (window.__warmupBannerFetchPatched && window.fetch.__warmupWrapped) return;
    if (window.fetch.__warmupWrapped) { window.__warmupBannerFetchPatched = true; return; }
    var orig = window.fetch.bind(window);
    var wrapped = function () {
      var p = orig.apply(null, arguments);
      try {
        p.then(function (resp) {
          if (resp && resp.headers && resp.headers.get) {
            noteLoadStatus(resp.headers.get('X-Corescope-Load-Status'));
          }
        }, function () {});
      } catch (e) { /* ignore */ }
      return p;
    };
    wrapped.__warmupWrapped = true;
    window.fetch = wrapped;
    window.__warmupBannerFetchPatched = true;
  }

  function init() {
    if (typeof document === 'undefined') return;
    ensureMounted();
    installFetchInterceptor();
    // Kick off an immediate health check so the banner appears within ~2s
    // of first paint (acceptance criterion).
    startPolling();
  }

  if (typeof document !== 'undefined') {
    if (document.readyState === 'loading') {
      document.addEventListener('DOMContentLoaded', init);
    } else {
      init();
    }
  }

  // -------- Exports ---------------------------------------------------------

  var api = {
    getWarmupMessages: getWarmupMessages,
    getWarmupOnlyMessages: getWarmupOnlyMessages,
    getStaleSources: getStaleSources,
    shouldKeepPolling: shouldKeepPolling,
    pruneDismissed: pruneDismissed,
    shouldShowBanner: shouldShowBanner,
    isSteadyState: isSteadyState,
    STALE_INGEST_MS: STALE_INGEST_MS,
    POLL_INTERVAL_MS: POLL_INTERVAL_MS,
    // Test hooks
    _state: state,
    _pollOnce: pollOnce,
    _installFetchInterceptor: installFetchInterceptor,
  };

  if (typeof window !== 'undefined') {
    window.__warmupBanner = api;
  }
  if (typeof module !== 'undefined' && module.exports) {
    module.exports = api;
  }
})();
