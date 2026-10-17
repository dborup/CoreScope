/* #479 — rolling 1 h TX airtime share per observer, shared by the RF Health
 * tiles (analytics.js), the observer list (observers.js) and the observer
 * detail page (observer-detail.js). The values and the warning threshold come
 * from /api/observers/metrics/summary (tx_airtime_pct_1h, rx_airtime_pct_1h,
 * duty_cycle_warn_pct).
 * #497 — RX 1h is channel load at the observer, not a regulated limit: it is
 * formatted like TX but never gets the warning class. */
window.ObserverAirtime = (function () {
  'use strict';
  var DEFAULT_WARN_PCT = 10;
  var TX_TITLE = 'Share of the last hour this observer transmitted; flagged at or above the duty-cycle limit.';
  var RX_TITLE = "Share of the last hour this observer's radio was receiving packets \u2014 a measure of channel load at this location, not a limit.";
  var UNKNOWN_TITLE = 'Under 30 minutes of airtime samples in the last hour, or none in the last 15 minutes';

  function warnPct(w) {
    return w > 0 ? w : DEFAULT_WARN_PCT;
  }

  function format(pct) {
    if (pct == null) return '—';
    var n = Number(pct);
    if (n > 0 && n < 0.1) return '<0.1%';
    return n.toFixed(1) + '%';
  }

  // Compared at the one decimal format() shows, so "10.0%" is never unflagged.
  function isOver(pct, w) {
    return pct != null && Math.round(Number(pct) * 10) / 10 >= warnPct(w);
  }

  function unknown() {
    return '<span class="text-muted" title="' + UNKNOWN_TITLE + '">\u2014</span>';
  }

  function badge(pct, w) {
    if (pct == null) return unknown();
    var over = isOver(pct, w);
    var title = TX_TITLE + ' Limit: ' + warnPct(w) + '%.';
    return '<span class="airtime-pct' + (over ? ' airtime-over' : '') + '" title="' + title + '">' +
      format(pct).replace('<', '&lt;') + '</span>';
  }

  // RX 1h: same shape as badge(), never flagged.
  function rxBadge(pct) {
    if (pct == null) return unknown();
    return '<span class="airtime-pct" title="' + RX_TITLE + '">' +
      format(pct).replace('<', '&lt;') + '</span>';
  }

  // One observer's row from a metrics summary; absent values are null.
  function lookup(summary, observerId) {
    var rows = (summary && summary.observers) || [];
    var row = null;
    for (var i = 0; i < rows.length; i++) {
      if (rows[i] && rows[i].observer_id === observerId) { row = rows[i]; break; }
    }
    return {
      tx: row && row.tx_airtime_pct_1h != null ? row.tx_airtime_pct_1h : null,
      rx: row && row.rx_airtime_pct_1h != null ? row.rx_airtime_pct_1h : null,
      warnPct: summary && summary.duty_cycle_warn_pct
    };
  }

  // The two observer-detail stat cards (r = lookup() result).
  function statCards(r) {
    return '<div class="stat-card" data-testid="obs-detail-tx-airtime">' +
        '<div class="stat-label" title="' + TX_TITLE + '">TX 1h</div>' +
        '<div class="stat-value">' + badge(r.tx, r.warnPct) + '</div></div>' +
      '<div class="stat-card" data-testid="obs-detail-rx-airtime">' +
        '<div class="stat-label" title="' + RX_TITLE + '">RX 1h</div>' +
        '<div class="stat-value">' + rxBadge(r.rx) + '</div></div>';
  }

  return {
    format: format, isOver: isOver, badge: badge, rxBadge: rxBadge,
    lookup: lookup, statCards: statCards, TX_TITLE: TX_TITLE, RX_TITLE: RX_TITLE
  };
})();
