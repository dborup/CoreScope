/* version-notice.js — "new version available" notice (#614).
 *
 * Remembers the server commit seen at page load (via the shared
 * window.CoreHealth reader from nav-drawer.js) and shows a non-blocking,
 * dismissible banner when a later reading reports a different commit. It
 * never reloads by itself, and shows once per new commit.
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

    function hide() {
      if (banner) { banner.remove(); banner = null; }
    }
    function show() {
      if (banner) return;
      banner = buildBanner(function () { window.location.reload(); },
        hide);
      document.body.appendChild(banner);
    }
    function check(force) {
      var now = Date.now();
      if (!force && !shouldCheck(now, lastCheck, MIN_GAP_MS)) return;
      lastCheck = now;
      reader.read().then(function (h) {
        var r = decide(state, h);
        state = r.state;
        if (r.notify) show();
      });
    }

    check(true); // baseline
    document.addEventListener('visibilitychange', function () {
      if (!document.hidden) check(false);
    });
    window.addEventListener('corescope:ws-open', function () { check(false); });
    setInterval(function () { if (!document.hidden) check(false); }, INTERVAL_MS);
  }

  window.VersionNotice = {
    initialState: initialState, decide: decide,
    shouldCheck: shouldCheck, start: start,
  };
  if (typeof document !== 'undefined' && document.body !== undefined) start();
})();
