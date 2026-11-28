/* version-notice.js — "new version available" notice (#614).
 *
 * Remembers the server commit seen at page load (via the shared
 * window.CoreHealth reader from nav-drawer.js) and shows a non-blocking,
 * dismissible banner when a later reading reports a different commit. It
 * never reloads by itself, and shows once per new commit.
 *
 * Kiosk screens (#883 M3, owner decision 5) are the one exception: while
 * Live's kiosk mode has set setAutoReload(true), a new commit reloads the
 * page after a random 30–120 s delay instead of the banner, so a room of
 * screens does not hit a freshly restarted server at once. A plain restart
 * (same commit) never reloads. Leaving kiosk cancels a pending reload and
 * shows the banner.
 *
 * Checks piggyback on events that already happen: the tab becoming visible,
 * the shared WebSocket (re)opening (app.js dispatches `corescope:ws-open`),
 * and a slow interval for tabs that stay visible. All are throttled.
 * Failed or commit-less health readings (restart window) are ignored.
 *
 * Intervals are hardcoded for now; candidates for the customizer later.
 */
'use strict';

(function () {
  var MIN_GAP_MS = 30 * 1000;        // floor between any two checks
  var INTERVAL_MS = 5 * 60 * 1000;   // periodic check for visible tabs
  var RELOAD_MIN_MS = 30 * 1000;     // kiosk auto-reload window (#883)
  var RELOAD_MAX_MS = 120 * 1000;

  function initialState() { return { baseline: null, notified: null }; }

  // Pure. Returns {notify, commit, state}; never mutates `state`.
  function decide(state, health) {
    var commit = health && typeof health.commit === 'string' ? health.commit.trim() : '';
    if (!commit || commit.toLowerCase() === 'unknown') {
      return { notify: false, commit: '', state: state };
    }
    if (state.baseline === null) {
      return { notify: false, commit: commit, state: { baseline: commit, notified: state.notified } };
    }
    if (commit === state.baseline || commit === state.notified) {
      return { notify: false, commit: commit, state: state };
    }
    return { notify: true, commit: commit, state: { baseline: state.baseline, notified: commit } };
  }

  // random in [0, 1) → a delay in [RELOAD_MIN_MS, RELOAD_MAX_MS).
  function reloadDelayMs(random) {
    var r = Number(random);
    if (!(r >= 0)) r = 0;
    if (r > 1) r = 1;
    return Math.round(RELOAD_MIN_MS + r * (RELOAD_MAX_MS - RELOAD_MIN_MS));
  }

  function shouldCheck(now, last, minGap) {
    return !(last > 0) || now - last >= minGap;
  }

  function buildBanner(onReload, onDismiss) {
    var el = document.createElement('div');
    el.className = 'version-notice';
    el.setAttribute('role', 'status');
    el.setAttribute('data-version-notice', '');
    var msg = document.createElement('span');
    msg.className = 'version-notice-text';
    msg.textContent = 'New version available – reload to update.';
    var reload = document.createElement('button');
    reload.type = 'button';
    reload.className = 'version-notice-btn version-notice-reload';
    reload.setAttribute('data-version-notice-reload', '');
    reload.textContent = 'Reload';
    reload.addEventListener('click', onReload);
    var close = document.createElement('button');
    close.type = 'button';
    close.className = 'version-notice-btn version-notice-dismiss';
    close.setAttribute('data-version-notice-dismiss', '');
    close.setAttribute('aria-label', 'Dismiss');
    close.textContent = 'Dismiss';
    close.addEventListener('click', onDismiss);
    el.appendChild(msg);
    el.appendChild(reload);
    el.appendChild(close);
    return el;
  }

  function start() {
    var reader = window.CoreHealth;
    if (!reader || typeof reader.read !== 'function') return;
    var state = initialState();
    var lastCheck = 0;
    var banner = null;
    var newVersion = false;    // a new commit was seen
    var autoReload = false;    // kiosk mode is on
    var reloadTimer = null;

    function hide() {
      if (banner) { banner.remove(); banner = null; }
    }
    function show() {
      if (banner) return;
      banner = buildBanner(function () { window.location.reload(); },
        hide);
      document.body.appendChild(banner);
    }
    function scheduleReload() {
      hide();
      if (reloadTimer) return;
      reloadTimer = setTimeout(function () { reloadTimer = null; window.location.reload(); },
        reloadDelayMs(Math.random()));
    }
    function onNewVersion() {
      newVersion = true;
      if (autoReload) scheduleReload();
      else show();
    }
    function setAutoReload(on) {
      autoReload = !!on;
      if (autoReload) { if (newVersion) scheduleReload(); return; }
      if (reloadTimer) { clearTimeout(reloadTimer); reloadTimer = null; }
      if (newVersion) show();
    }
    function check(baseline) {
      var now = Date.now();
      if (!baseline && !shouldCheck(now, lastCheck, MIN_GAP_MS)) return;
      lastCheck = now;
      (baseline ? reader.readOnce() : reader.read()).then(function (h) {
        var r = decide(state, h);
        state = r.state;
        if (r.notify) onNewVersion();
      });
    }

    check(true); // baseline
    document.addEventListener('visibilitychange', function () {
      if (!document.hidden) check(false);
    });
    window.addEventListener('corescope:ws-open', function () { check(false); });
    setInterval(function () { if (!document.hidden) check(false); }, INTERVAL_MS);
    return { setAutoReload: setAutoReload };
  }

  var notice = null;
  window.VersionNotice = {
    initialState: initialState, decide: decide,
    shouldCheck: shouldCheck, start: start,
    RELOAD_MIN_MS: RELOAD_MIN_MS, RELOAD_MAX_MS: RELOAD_MAX_MS, reloadDelayMs: reloadDelayMs,
    // Live's kiosk mode turns this on while it is mounted (#883).
    setAutoReload: function (on) { if (notice) notice.setAutoReload(on); },
  };
  if (typeof document !== 'undefined' && document.body !== undefined) notice = start() || null;
})();
