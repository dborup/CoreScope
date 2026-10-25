/* === CoreScope — advert-interval-analytics.js ===
 *
 * Analytics → Advert Intervals (#245 M2): the flood and zero-hop advert
 * intervals the nodes of the mesh run with, by role and by region, how many
 * show no adverts of a class, and the nodes advertising more often than the
 * firmware's timers allow. Rendered from GET /api/analytics/advert-intervals
 * (docs/api-spec.md), which estimates each node exactly as node detail's
 * advertIntervals does (M1); intervals are written in the units of the
 * firmware settings through NodeAdverts.formatInterval (node-adverts.js).
 *
 * render() returns an HTML string built only from escaped values. Nothing
 * here fetches; analytics.js passes the response in.
 */
(function (root) {
  'use strict';

  var CLASSES = [
    { key: 'flood', label: 'Flood', setting: 'flood.advert.interval' },
    { key: 'zero_hop', label: 'Zero-hop', setting: 'advert.interval' }
  ];
  var NOT_OBSERVED_ZH = 'off, or no observer in direct range';
  var REASONS = {
    flood_faster_than_settable: 'flood more often than every 3 h, the shortest flood.advert.interval (manual or scripted adverts, a reboot loop, other firmware)',
    zero_hop_faster_than_settable: 'zero-hop more often than every 60 min, the shortest advert.interval (manual adverts, a reboot loop, other firmware)',
    zero_hop_new_install_default: 'zero-hop every 2 min: the new-install default (advert.interval never saved)'
  };
  var TOP_VALUES = 3;

  function esc(s) {
    return String(s == null ? '' : s)
      .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
  }

  function num(v) {
    var n = Number(v);
    return isFinite(n) ? n : 0;
  }

  function fmt(seconds, key) {
    var na = root.NodeAdverts;
    return na && na.formatInterval ? na.formatInterval(seconds, key) : String(num(seconds)) + ' s';
  }

  /** The n buckets with the most nodes; ties go to the shorter interval. */
  function topBuckets(buckets, n) {
    return (Array.isArray(buckets) ? buckets.slice() : [])
      .sort(function (a, b) { return num(b.nodes) - num(a.nodes) || num(a.interval_s) - num(b.interval_s); })
      .slice(0, n);
  }

  function statCard(id, value, label, detail) {
    return '<div class="stat-card" data-ai-stat="' + id + '"><div class="stat-value">' + num(value) + '</div>' +
      '<div class="stat-label">' + esc(label) + '</div>' +
      (detail ? '<div class="stat-detail">' + esc(detail) + '</div>' : '') + '</div>';
  }

  function summaryHtml(d) {
    var flood = d.flood || {}, zh = d.zero_hop || {};
    return '<div class="stats-grid" data-ai-section="summary">' +
      statCard('nodes', d.nodes, 'Nodes with adverts') +
      statCard('flood-estimated', flood.estimated, 'Flood interval estimated') +
      statCard('zero_hop-estimated', zh.estimated, 'Zero-hop interval estimated') +
      statCard('zero_hop-not-observed', zh.not_observed, 'Zero-hop not observed', NOT_OBSERVED_ZH) +
      statCard('outliers', d.outliers_total, 'Outliers', 'very frequent adverts') +
      '</div>';
  }

  function classCounts(s, c) {
    s = s || {};
    var notObserved = c.key === 'zero_hop' ? 'not observed (' + NOT_OBSERVED_ZH + ')' : 'not observed';
    return '<div class="ai-class-counts text-muted">Estimated ' + num(s.estimated) +
      ' · low confidence ' + num(s.low_confidence) + ' · irregular ' + num(s.irregular) +
      ' · too few adverts ' + num(s.too_few) + ' · ' + notObserved + ' ' + num(s.not_observed) + '</div>';
  }

  function distributionHtml(s, c) {
    s = s || {};
    var buckets = Array.isArray(s.buckets) ? s.buckets : [];
    var max = buckets.reduce(function (m, b) { return Math.max(m, num(b.nodes)); }, 0) || 1;
    var rows = buckets.map(function (b) {
      var pct = Math.round(num(b.nodes) / max * 100);
      return '<div class="ai-dist-row" data-interval="' + num(b.interval_s) + '">' +
        '<span class="ai-dist-label">' + esc(fmt(b.interval_s, c.key)) +
        (b.snapped ? '' : ' <span class="text-muted">(outside the settable range)</span>') + '</span>' +
        '<span class="ai-dist-bar"><span class="ai-dist-fill" style="width:' + pct + '%"></span></span>' +
        '<span class="ai-dist-count">' + num(b.nodes) + '</span></div>';
    }).join('');
    if (!rows) rows = '<div class="text-muted">No medium or high confidence estimates.</div>';
    return '<div class="analytics-card" data-ai-section="dist-' + c.key + '">' +
      '<h3>' + c.label + ' advert interval</h3>' +
      '<div class="ai-dist" title="Nodes per estimated ' + c.setting + ' value">' + rows + '</div>' +
      classCounts(s, c) + '</div>';
  }

  function topValues(s, key) {
    var top = topBuckets(s && s.buckets, TOP_VALUES);
    if (!top.length) return '—';
    var more = (s.buckets.length > top.length) ? ' · +' + (s.buckets.length - top.length) + ' more' : '';
    return top.map(function (b) { return esc(fmt(b.interval_s, key)) + ' × ' + num(b.nodes); }).join(' · ') + more;
  }

  function groupsHtml(section, title, keyLabel, groups, keyText, footer) {
    var rows = (Array.isArray(groups) ? groups : []).map(function (g) {
      return '<tr data-ai-group="' + esc(g.key) + '">' +
        '<td>' + esc(keyText(g.key)) + '</td>' +
        '<td style="text-align:right">' + num(g.nodes) + '</td>' +
        '<td>' + topValues(g.flood, 'flood') + '</td>' +
        '<td>' + topValues(g.zero_hop, 'zero_hop') + '</td>' +
        '<td style="text-align:right">' + num(g.zero_hop && g.zero_hop.not_observed) + '</td></tr>';
    }).join('');
    return '<div class="analytics-card" id="ai-' + section + '" data-ai-section="' + section + '">' +
      '<h3>' + title + '</h3>' +
      '<table class="data-table analytics-table"><thead><tr>' +
        '<th>' + keyLabel + '</th><th style="text-align:right">Nodes</th>' +
        '<th>Flood intervals</th><th>Zero-hop intervals</th>' +
        '<th style="text-align:right" title="No zero-hop adverts: ' + NOT_OBSERVED_ZH + '">Zero-hop not observed</th>' +
      '</tr></thead><tbody>' + rows + '</tbody></table>' + (footer || '') + '</div>';
  }

  function reArmHtml(r) {
    r = r || {};
    return '<div class="analytics-card" data-ai-section="re-arm">' +
      '<h3>Flood timer and zero-hop</h3>' +
      '<p class="text-muted ai-note">A flood advert re-arms the zero-hop timer in the firmware, which shapes what the zero-hop adverts show.</p>' +
      '<div class="ai-rearm-row"><strong>Zero-hop paced by flood</strong> ' + num(r.zero_hop_paced_by_flood) +
        ' <span class="text-muted">— the zero-hop estimate equals the flood interval, so the zero-hop setting lies between half the flood interval and the flood interval.</span></div>' +
      '<div class="ai-rearm-row"><strong>Zero-hop maybe suppressed by flood</strong> ' + num(r.zero_hop_maybe_suppressed) +
        ' <span class="text-muted">— no zero-hop adverts and a flood interval of at most 4 h: with advert.interval at or above the flood interval the zero-hop timer never fires.</span></div>' +
      '</div>';
  }

  function outliersHtml(list, total) {
    list = Array.isArray(list) ? list : [];
    var head = '<div class="analytics-card" id="ai-outliers" data-ai-section="outliers"><h3>Outliers: very frequent adverts</h3>';
    if (!list.length) return head + '<div class="text-muted">No outliers.</div></div>';
    var rows = list.map(function (o) {
      var cls = o.class === 'zero_hop' ? 'zero_hop' : 'flood';
      var name = o.name ? esc(o.name) : '<span class="mono">' + esc(String(o.public_key || '').slice(0, 16)) + '</span>';
      return '<tr data-ai-outlier="' + esc(o.public_key) + '">' +
        '<td><a href="#/nodes/' + encodeURIComponent(o.public_key || '') + '">' + name + '</a></td>' +
        '<td>' + esc(o.role || 'unknown') + '</td>' +
        '<td>' + (cls === 'zero_hop' ? 'Zero-hop' : 'Flood') + '</td>' +
        '<td style="text-align:right">' + esc(fmt(o.median_gap_s, cls)) + '</td>' +
        '<td>' + esc(REASONS[o.reason] || o.reason) + '</td>' +
        '<td>' + esc((o.regions || []).join(', ') || '—') + '</td></tr>';
    }).join('');
    var shown = num(total) > list.length ? '<p class="text-muted ai-note">showing ' + list.length + ' of ' + num(total) + '</p>' : '';
    return head + shown +
      '<table class="data-table analytics-table"><thead><tr><th>Node</th><th>Role</th><th>Class</th>' +
      '<th style="text-align:right" title="Median gap between the class\'s newest adverts">Median gap</th><th>Why</th><th>Regions</th>' +
      '</tr></thead><tbody>' + rows + '</tbody></table></div>';
  }

  function methodHtml(d) {
    var scan = d.scan || {};
    return '<p class="text-muted ai-note" data-ai-section="method">Estimated per node from its newest ' + num(d.window) +
      ' flood and ' + num(d.window) + ' zero-hop adverts, exactly as on the node page; the distributions count medium and high confidence estimates only. ' +
      'Values snap to what the firmware can be set to: flood.advert.interval 3–168 h, advert.interval 60–240 min in 2-minute steps, or 2 min on an untouched new install. ' +
      'Region: the regions of the observers that heard the node; a node heard in two regions counts in both. ' +
      'Computed in the background once per analytics cycle (last ' + esc(scan.generated_at || '—') + ').' +
      (scan.truncated ? ' Read the newest ' + num(scan.rows) + ' adverts only.' : '') + '</p>';
  }

  /** The tab's HTML for one /api/analytics/advert-intervals response. */
  function render(d) {
    if (!d || !num(d.nodes)) {
      return '<div class="text-center text-muted ai-empty" style="padding:40px">No nodes with adverts in this selection.</div>';
    }
    return '<div class="ai-analytics" data-testid="advert-intervals">' +
      '<p class="text-muted ai-note">How often the nodes send flood and zero-hop adverts, as estimated from the adverts CoreScope heard.</p>' +
      summaryHtml(d) +
      '<div class="analytics-row">' + CLASSES.map(function (c) { return distributionHtml(d[c.key], c); }).join('') + '</div>' +
      reArmHtml(d.flood_re_arm) +
      groupsHtml('by-role', 'By role', 'Role', d.by_role, function (k) { return k; }) +
      groupsHtml('by-region', 'By region', 'Region', d.by_region, function (k) { return k === 'unknown' ? 'No observer region' : k; },
        d.regions_truncated ? '<p class="text-muted ai-note">Only the 50 largest regions are listed.</p>' : '') +
      outliersHtml(d.outliers, d.outliers_total) +
      methodHtml(d) +
      '</div>';
  }

  root.AdvertIntervalAnalytics = { render: render, topBuckets: topBuckets };
})(typeof window !== 'undefined' ? window : this);
