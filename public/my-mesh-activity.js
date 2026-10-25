/* === CoreScope — my-mesh-activity.js ===
 *
 * Issue #304 (points 3, 4 and 5). A My Mesh card has to answer three
 * questions honestly: has this node been active, is it being used, and has
 * anything changed? The node-health payload supports less than the old card
 * implied, so this helper labels each number with the evidence behind it:
 *
 *   3. STATUS EVIDENCE. stats.lastAdvert is strictly the node's OWN advert.
 *      stats.lastHeard is that advert OR identity-safe relay evidence,
 *      whichever is newer, where "identity-safe" still means a wire hash
 *      prefix that no OTHER KNOWN relay shares (cmd/server/node_activity.go
 *      confirmedRelayKey). Uniqueness among known nodes is not proof: an
 *      unknown relay can share the prefix. So a badge whose freshness rests
 *      only on relay attribution is reported as attributed, never as a
 *      confident green. The thresholds themselves are NOT defined here --
 *      getHealthStatus / getHealthLastHeard (roles.js, #539) stay the single
 *      source of truth and are injectable for tests.
 *
 *   4. SNR. stats.avgSnr is the mean over the receptions that PROVE a direct
 *      reception of this node's own transmission (#76, heard-by.js), measured
 *      by the receiving observer. That describes those links at the observer
 *      end; it is not a device health grade for the node, so this file prints
 *      no Excellent/Good/Marginal/Poor verdict and hands the card no verdict
 *      colour. The shared API field keeps its meaning untouched.
 *
 *   5. TRAFFIC. activity24h is the ROLLING 24-hour window with per-hour
 *      counts, a coverage start and a completeness flag; activity24h.previous
 *      is the 24 hours before it, for a like-for-like baseline only. Every
 *      count states its population and window, an incomplete window renders
 *      as a lower bound, and a window with no proven coverage is unavailable
 *      rather than zero. avgHops is relabelled as what it measures: the mean
 *      observed path length of the node's transmissions, not a distance.
 *
 * Pure functions only: no DOM, no network, no clock of its own (callers pass
 * nowMs). The caller already fetches node health once per card, and nothing
 * here adds a request.
 */
(function (root) {
  'use strict';

  var UNKNOWN_VALUE = '—';

  // ─── point 3: status evidence ───
  var EVIDENCE_SUMMARY = 'Status evidence';
  var ADVERT_ROW_LABEL = 'Last own advert';
  var RELAY_ROW_LABEL = 'Last attributed relay activity';
  var WINDOW_ROW_LABEL = 'Counting window';
  var ADVERT_NONE = 'none observed';
  var RELAY_NONE_NEWER = 'none newer than the last own advert';
  var RELAY_NONE = 'none attributed';
  var ATTRIBUTED_SUFFIX = 'relay-attributed';
  var STATUS_LABELS = { healthy: 'Active', degraded: 'Degraded', silent: 'Silent' };
  var EXPLAIN_ADVERT = 'The badge follows this node’s own advert, the only direct evidence that it transmitted, ' +
    'aged with the shared role thresholds. A recent advert is not proof of continuous uptime or of a device that is online right now.';
  var EXPLAIN_RELAY = 'No own advert is recent enough to carry this badge. The newest evidence is other traffic whose observed path ' +
    'was attributed to this node by a short wire hash prefix that no other known relay shares — an unknown relay can share it, ' +
    'so the match is not proof and this is deliberately not a confident green status.';
  var EXPLAIN_NONE = 'No identity-safe activity was observed in the window, so there is nothing to classify. ' +
    'That is missing evidence, not proof that the node is off.';

  // ─── point 4: SNR ───
  var SNR_LABEL = 'Direct SNR';
  var SNR_TIP = 'Mean SNR over the receptions that prove a direct reception of this node’s own transmission (see Heard By), ' +
    'measured by the receiving observer. n is how many such receptions the mean covers. It describes those observer links, ' +
    'not this node’s own signal quality or device health, and the direct attribution can rest on a wire hash prefix.';
  var SNR_EMPTY_TIP = 'No observation proves a direct reception of this node’s own transmission, so there is no SNR that belongs to it. ' +
    'Any other copy carries the signal of whichever node the observer actually heard.';

  // ─── point 5: traffic ───
  var TRAFFIC_LABEL = 'Transmissions · 24 h';
  var TRAFFIC_TIP = 'Distinct transmissions associated with this node in the rolling 24-hour window below: its own, ' +
    'those addressed to it, and those whose observed path confidently names it. It is not a count of what this node forwarded, ' +
    'and repeated observations of one transmission count once.';
  var TRAFFIC_PARTIAL_TIP = 'The in-memory history does not cover the whole window, so this is a lower bound (≥) over an incomplete window, not a total.';
  var TRAFFIC_UNAVAILABLE_TIP = 'The rolling window is unavailable for this node, so no 24-hour count is shown. An unknown window is not a quiet one.';
  var COMPARE_ROW_LABEL = 'Change vs previous 24 h';
  var COMPARE_NO_HISTORY = 'no comparable 24 h history';
  var COMPARE_TIP = 'Compares the rolling 24-hour count with the previous 24 h, the equally long window right before it. Shown only when both windows are fully covered, ' +
    'so a missing history is never read as zero traffic, and never as a percentage of a zero baseline.';
  var HOPS_LABEL = 'Avg observed path length';
  var HOPS_TIP = 'Mean number of hops on the observed paths of the transmissions associated with this node that carried one. ' +
    'It describes the routes those copies travelled, not this node’s distance from you and not its health.';
  var WINDOW_INCOMPLETE_SUFFIX = ' (coverage incomplete)';

  function num(v) {
    if (typeof v === 'number') return isFinite(v) ? v : null;
    if (typeof v === 'string' && v.trim() !== '') {
      var n = Number(v);
      return isFinite(n) ? n : null;
    }
    return null;
  }

  function ms(value) {
    if (typeof value === 'number') return isFinite(value) ? value : null;
    if (typeof value !== 'string' || value === '') return null;
    var t = Date.parse(value);
    return isFinite(t) ? t : null;
  }

  function arr(v) { return Array.isArray(v) ? v : []; }

  function count(n) { return Number(n).toLocaleString('en-US'); }

  /* ── point 3 ──
   * statusEvidence({ node, stats, nowMs, classify?, lastHeard? })
   *
   * classify defaults to window.getHealthStatus and lastHeard to
   * window.getHealthLastHeard: this file adds no threshold and no evidence
   * rule of its own. source is 'advert' only when the node's OWN advert
   * alone already supports the badge; otherwise the badge rests on relay
   * attribution and is reported as not confident.
   */
  function statusEvidence(opts) {
    opts = opts || {};
    var node = opts.node || {};
    var stats = opts.stats || {};
    var classify = typeof opts.classify === 'function' ? opts.classify
      : (root && typeof root.getHealthStatus === 'function' ? root.getHealthStatus : null);
    var lastHeardOf = typeof opts.lastHeard === 'function' ? opts.lastHeard
      : (root && typeof root.getHealthLastHeard === 'function' ? root.getHealthLastHeard : null);

    var heardAt = lastHeardOf ? lastHeardOf(node, stats) : null;
    if (ms(heardAt) === null) heardAt = null;
    var advertAt = ms(stats.lastAdvert) === null ? null : stats.lastAdvert;
    var classified = classify && heardAt ? classify(node.role, heardAt, opts.nowMs) : null;
    var advertStatus = classify && advertAt ? classify(node.role, advertAt, opts.nowMs) : null;
    // Relay activity is only reported where the evidence supports it: the
    // payload exposes one combined lastHeard, so relay evidence is visible
    // exactly when it is NEWER than the node's own advert.
    var relayAt = heardAt && (!advertAt || ms(heardAt) > ms(advertAt)) ? heardAt : null;

    var source = 'none';
    if (classified) source = advertAt && advertStatus === classified ? 'advert' : (relayAt ? 'relay' : 'advert');
    var status = classified || 'silent';
    var statusCls = status === 'healthy' ? 'status-ok' : status === 'degraded' ? 'status-warn' : 'status-err';
    var attributed = source === 'relay';
    var label = STATUS_LABELS[status] || STATUS_LABELS.silent;
    return {
      status: status,
      classified: classified || null,
      source: source,
      confident: source === 'advert',
      // An attributed match must never read as a confident green dot, so a
      // healthy status drops to the provisional colour and a hollow glyph.
      cls: attributed && status === 'healthy' ? 'status-warn' : statusCls,
      glyph: attributed ? 'ph-circle' : 'ph-circle-fill',
      label: label,
      text: attributed ? label + ' · ' + ATTRIBUTED_SUFFIX : label,
      heardAt: heardAt,
      advertAt: advertAt,
      relayAt: relayAt,
      advertText: advertAt ? null : ADVERT_NONE,
      relayText: relayAt ? null : (advertAt ? RELAY_NONE_NEWER : RELAY_NONE),
      explanation: source === 'advert' ? EXPLAIN_ADVERT : source === 'relay' ? EXPLAIN_RELAY : EXPLAIN_NONE
    };
  }

  /* ── point 4 ── snr(stats, directObservers) */
  function snr(stats, direct) {
    stats = stats || {};
    var rows = arr(direct);
    var value = num(typeof stats.avgSnr === 'string' ? null : stats.avgSnr);
    var samples = 0;
    for (var i = 0; i < rows.length; i++) {
      var n = num(rows[i] && rows[i].packetCount);
      if (n !== null && n >= 0) samples += Math.floor(n);
    }
    // Without a proven direct reception there is no SNR that belongs to this
    // node, whatever the mean says (#76).
    var available = value !== null && rows.length > 0;
    return {
      available: available,
      value: available ? value.toFixed(1) + ' dB' : UNKNOWN_VALUE,
      samples: samples,
      label: available && samples > 0 ? SNR_LABEL + ' · n=' + count(samples) : SNR_LABEL,
      tip: available ? SNR_TIP : SNR_EMPTY_TIP
    };
  }

  /* ── point 5 ──
   * buckets(activity) validates the rolling window's shape and is the one
   * place that decides whether the hourly counts are usable at all (the
   * sparkline shares it).
   */
  function buckets(activity) {
    var b = activity && activity.buckets;
    if (!Array.isArray(b) || b.length !== 24) return null;
    for (var i = 0; i < b.length; i++) {
      var h = b[i];
      if (!h || !Number.isInteger(h.count) || h.count < 0) return null;
      if (!isFinite(Date.parse(h.start)) || !isFinite(Date.parse(h.end))) return null;
    }
    return b;
  }

  function sum(b) {
    var total = 0;
    for (var i = 0; i < b.length; i++) total += b[i].count;
    return total;
  }

  function traffic24h(activity) {
    var b = buckets(activity);
    if (!b) {
      return { available: false, exact: false, count: null, value: UNKNOWN_VALUE, label: TRAFFIC_LABEL, tip: TRAFFIC_UNAVAILABLE_TIP };
    }
    var total = sum(b);
    var exact = activity.complete === true;
    return {
      available: true,
      exact: exact,
      count: total,
      value: (exact ? '' : '≥ ') + count(total),
      label: TRAFFIC_LABEL,
      tip: TRAFFIC_TIP + ' ' + (exact ? windowText(activity) : TRAFFIC_PARTIAL_TIP + ' ' + windowText(activity))
    };
  }

  /* compare24h(activity) -> the like-for-like change, or an explicit
   * "no comparable history". A baseline of zero yields an absolute delta and
   * never a percentage; an incomplete window on either side yields nothing.
   */
  function compare24h(activity) {
    var cur = traffic24h(activity);
    var prev = activity && activity.previous;
    // The baseline is stricter than the display casts elsewhere in this
    // file: it is divided by, so only a genuine non-negative integer count
    // from a window that is itself proven complete can serve as one.
    var baseline = prev && prev.complete === true && typeof prev.count === 'number' &&
      Number.isInteger(prev.count) && prev.count >= 0 ? prev.count : null;
    if (!cur.available || !cur.exact || baseline === null) {
      return { available: false, pct: null, delta: null, short: null, text: COMPARE_NO_HISTORY, tip: COMPARE_TIP };
    }
    var delta = cur.count - baseline;
    if (baseline === 0) {
      // A zero baseline has no percentage to show, so there is no short
      // inline form either: the card would have nothing to divide by.
      return {
        available: true, pct: null, delta: delta, short: null,
        text: (delta > 0 ? '+' : '') + count(delta) + ' vs 0 in the previous 24 h', tip: COMPARE_TIP
      };
    }
    var pct = Math.round((delta / baseline) * 100);
    var sign = pct > 0 ? '+' : '';
    return {
      available: true, pct: pct, delta: delta, short: sign + pct + '%',
      text: sign + pct + '% vs previous 24 h', tip: COMPARE_TIP
    };
  }

  function hops(stats) {
    var v = num(stats && stats.avgHops);
    if (v === null || v < 0) {
      return { available: false, value: UNKNOWN_VALUE, label: HOPS_LABEL, tip: HOPS_TIP };
    }
    return {
      available: true,
      value: count(v) + (v === 1 ? ' hop' : ' hops'),
      label: HOPS_LABEL,
      tip: HOPS_TIP
    };
  }

  /* windowText(activity) -> the window bounds in the reader's own locale,
   * with the coverage caveat when the history does not cover all of it. The
   * card shows this as text, not only as a tooltip: a title attribute is not
   * reachable by touch.
   */
  function windowText(activity) {
    var from = ms(activity && activity.windowStart);
    var to = ms(activity && activity.windowEnd);
    if (from === null || to === null) return UNKNOWN_VALUE;
    return new Date(from).toLocaleString() + ' to ' + new Date(to).toLocaleString() +
      (activity.complete === true ? '' : WINDOW_INCOMPLETE_SUFFIX);
  }

  root.MyMeshActivity = {
    UNKNOWN_VALUE: UNKNOWN_VALUE,
    EVIDENCE_SUMMARY: EVIDENCE_SUMMARY,
    ADVERT_ROW_LABEL: ADVERT_ROW_LABEL,
    RELAY_ROW_LABEL: RELAY_ROW_LABEL,
    WINDOW_ROW_LABEL: WINDOW_ROW_LABEL,
    ADVERT_NONE: ADVERT_NONE,
    RELAY_NONE_NEWER: RELAY_NONE_NEWER,
    RELAY_NONE: RELAY_NONE,
    ATTRIBUTED_SUFFIX: ATTRIBUTED_SUFFIX,
    SNR_LABEL: SNR_LABEL,
    SNR_TIP: SNR_TIP,
    TRAFFIC_LABEL: TRAFFIC_LABEL,
    TRAFFIC_TIP: TRAFFIC_TIP,
    COMPARE_ROW_LABEL: COMPARE_ROW_LABEL,
    COMPARE_NO_HISTORY: COMPARE_NO_HISTORY,
    COMPARE_TIP: COMPARE_TIP,
    HOPS_LABEL: HOPS_LABEL,
    HOPS_TIP: HOPS_TIP,
    statusEvidence: statusEvidence,
    snr: snr,
    buckets: buckets,
    traffic24h: traffic24h,
    compare24h: compare24h,
    hops: hops,
    windowText: windowText
  };
})(typeof window !== 'undefined' ? window : this);
