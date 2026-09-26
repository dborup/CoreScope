/* === CoreScope — node-adverts.js ===
 *
 * The Recent Adverts panel, shared by the node detail page and the node side
 * panel. The "Recent Adverts" name and its tooltip port upstream
 * `Kpa-clawbot/CoreScope#2071`; the per-route tabs and counts extend upstream
 * `Kpa-clawbot/CoreScope#2073` over the node-detail fields
 * recentAdvertsByRoute and advertCounts (see docs/api-spec.md).
 *
 * render() returns an HTML string built only from escaped values; bind()
 * wires the tab bar through the app's initTabBar (roles, arrow keys) and
 * reports tab changes so the caller can keep ?adverts= in the URL. Nothing
 * here fetches: everything comes from the one node-detail response, which
 * carries the breakdown only when requested through detailPath() - the node
 * page does, other callers of /api/nodes/{pubkey} do not pay for it.
 */
(function (root) {
  'use strict';

  var PARAM = 'adverts';
  // Opt-in query of GET /api/nodes/{pubkey} for recentAdvertsByRoute,
  // advertCounts and route_class (docs/api-spec.md).
  var INCLUDE = 'include=advertRoutes';
  var TITLE_TIP = 'Adverts this node originated. The section is limited to adverts because they are the only packet type attributable to an originating node: transmissions.from_pubkey is populated for ADVERTs only, so a relayed CHAN or TXT packet cannot be traced back to its sender without path resolution.';
  var CLASSES = [
    { key: 'flood', label: 'Flood' },
    { key: 'zero_hop', label: 'Zero-hop' },
    { key: 'mixed', label: 'Mixed' },
    { key: 'unknown', label: 'Unknown' }
  ];
  var TYPE_ICONS = { 4: 'ph-broadcast', 5: 'ph-chat-circle', 2: 'ph-envelope' };
  var TYPE_LABELS = { 4: 'Advert', 5: 'Channel', 2: 'DM' };

  function esc(s) {
    return String(s == null ? '' : s)
      .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
  }

  function truncate(s, len) {
    s = String(s || '');
    return s.length > len ? s.slice(0, len) + '…' : s;
  }

  function icon(name) {
    return '<svg class="ph-icon" aria-hidden="true"><use href="/icons/phosphor-sprite.svg#' + name + '"/></svg>';
  }

  function classLabel(key) {
    for (var i = 0; i < CLASSES.length; i++) {
      if (CLASSES[i].key === key) return CLASSES[i].label;
    }
    return 'Unknown';
  }

  function isTabKey(key) {
    return key === 'all' || CLASSES.some(function (c) { return c.key === key; });
  }

  function splitHash(hash) {
    var s = String(hash || '');
    var i = s.indexOf('?');
    return { path: i < 0 ? s : s.slice(0, i), params: i < 0 || i === s.length - 1 ? [] : s.slice(i + 1).split('&') };
  }

  function paramName(pair) {
    var eq = pair.indexOf('=');
    try { return decodeURIComponent(eq < 0 ? pair : pair.slice(0, eq)); } catch (e) { return ''; }
  }

  /** The tab named by ?adverts= in a location hash; 'all' when absent or invalid. */
  function parseTab(hash) {
    var params = splitHash(hash).params;
    for (var i = 0; i < params.length; i++) {
      if (paramName(params[i]) !== PARAM) continue;
      var eq = params[i].indexOf('=');
      var value = '';
      try { value = decodeURIComponent(eq < 0 ? '' : params[i].slice(eq + 1)); } catch (e) { value = ''; }
      return isTabKey(value) ? value : 'all';
    }
    return 'all';
  }

  /** hash with ?adverts= set to tab (dropped for 'all'); path and other params kept. */
  function hashWithTab(hash, tab) {
    var parts = splitHash(hash);
    var out = [];
    var placed = false;
    for (var i = 0; i < parts.params.length; i++) {
      if (paramName(parts.params[i]) !== PARAM) { out.push(parts.params[i]); continue; }
      if (!placed && tab !== 'all') out.push(PARAM + '=' + encodeURIComponent(tab));
      placed = true;
    }
    if (!placed && tab !== 'all') out.push(PARAM + '=' + encodeURIComponent(tab));
    return parts.path + (out.length ? '?' + out.join('&') : '');
  }

  function num(v) { return Number(v) || 0; }

  function countsRow(label, w) {
    w = w || {};
    var parts = ['Flood ' + num(w.flood), 'Zero-hop ' + num(w.zero_hop), 'Mixed ' + num(w.mixed)];
    if (num(w.unknown) > 0) parts.push('Unknown ' + num(w.unknown));
    return '<span class="node-adverts-count-row"><strong>' + label + '</strong> ' + parts.join(' · ') + '</span>';
  }

  function countsHtml(counts) {
    if (!counts) return '';
    var html = '<div class="node-adverts-counts" title="Distinct adverts by the time they were first heard">' +
      countsRow('24h', counts['24h']) + countsRow('7d', counts['7d']);
    if (counts.truncated) html += '<span class="node-adverts-count-row text-muted">(newest 50,000 adverts only)</span>';
    return html + '</div>';
  }

  function noteHtml(counts) {
    var st = counts && counts.route_mask_backfill && counts.route_mask_backfill.status;
    if (!counts || st === 'complete') return '';
    return '<p class="node-adverts-note text-muted">Route classes are provisional: the route-mask backfill is still running, so older adverts are classified by the route they were first stored with.</p>';
  }

  function routeBadge(p) {
    if (!p.route_class) return '';
    return ' <span class="advert-route-badge" data-route-class="' + esc(p.route_class) + '">' + esc(classLabel(p.route_class)) + '</span>';
  }

  // Hash-size badge for nodes whose adverts disagree on it: path_len's upper
  // two bits of the stored frame (byte 1), as before.
  function hashSizeBadge(p) {
    if (p.payload_type !== 4 || !p.raw_hex) return '';
    var pb = parseInt(String(p.raw_hex).slice(2, 4), 16);
    if (!(pb & 0x3F)) return '';
    var hs = ((pb >> 6) & 0x3) + 1;
    return ' <span class="badge advert-hs-badge hs-' + hs + '">' + hs + 'B</span>';
  }

  function entryHtml(p, opts, withRoute) {
    var decoded = null;
    try { decoded = JSON.parse(p.decoded_json); } catch (e) { decoded = null; }
    var detail = decoded && decoded.text ? ': ' + esc(truncate(decoded.text, 50))
      : decoded && decoded.name ? ' — ' + esc(decoded.name) : '';
    var type = icon(TYPE_ICONS[p.payload_type] || 'ph-package') + ' ' + (TYPE_LABELS[p.payload_type] || 'Packet');
    var count = num(p.observation_count);
    var obsBadge = count > 1 ? ' <span class="badge badge-obs" title="Seen ' + count + ' times">' + icon('ph-eye') + ' ' + count + '</span>' : '';
    var obs = p.observer_name || p.observer_id;
    var signal = (p.snr != null ? ' · SNR ' + esc(p.snr) + 'dB' : '') + (p.rssi != null ? ' · RSSI ' + esc(p.rssi) + 'dBm' : '');
    var body = type + (withRoute ? routeBadge(p) : '') + detail + (opts.hashSizeInconsistent ? hashSizeBadge(p) : '') +
      obsBadge + (obs ? ' via ' + esc(obs) : '') + signal;
    var ts = opts.timestampHtml ? opts.timestampHtml(p.timestamp) : esc(p.timestamp);
    var link = '<a href="#/packets/' + encodeURIComponent(p.hash) + '" class="ch-analyze-link"';
    if (opts.variant === 'pane') {
      return '<div class="advert-entry"><span class="advert-dot" style="background:' + esc(opts.roleColor || 'var(--text-muted)') + '"></span>' +
        '<div class="advert-info"><strong>' + ts + '</strong> ' + body + '<br>' + link + '>Analyze →</a></div></div>';
    }
    return '<div class="node-activity-item"><span class="node-activity-time">' + ts + '</span><span>' + body + '</span>' +
      link + ' style="margin-left:8px;font-size:0.8em">Analyze →</a></div>';
  }

  function byTimestampDesc(a, b) {
    return (Date.parse(b.timestamp) || 0) - (Date.parse(a.timestamp) || 0);
  }

  // Newest first by timestamp, like the node pages always showed the list;
  // the server picks the rows by ingest id (#1345).
  function listHtml(rows, key, opts) {
    var valid = (rows || []).filter(function (p) { return p && p.hash && p.timestamp; }).sort(byTimestampDesc);
    var cls = opts.variant === 'pane' ? 'advert-timeline' : 'node-activity-list';
    if (!valid.length) {
      var empty = key === 'all' ? 'No recent adverts' : 'No ' + classLabel(key).toLowerCase() + ' adverts from this node on record';
      return '<div class="' + cls + '"><div class="text-muted node-adverts-empty">' + empty + '</div></div>';
    }
    return '<div class="' + cls + '">' + valid.map(function (p) { return entryHtml(p, opts, key === 'all'); }).join('') + '</div>';
  }

  /**
   * HTML for the panel. detail is the /api/nodes/{pubkey} response;
   * opts: { variant: 'full'|'pane', tab, idPrefix, timestampHtml(iso),
   * hashSizeInconsistent, roleColor }. Without recentAdvertsByRoute (older
   * server, hidden identity) only the chronological list is shown.
   */
  function render(detail, opts) {
    detail = detail || {};
    opts = opts || {};
    var prefix = opts.idPrefix || 'nodeAdverts';
    var all = (detail.recentAdverts || []).filter(function (p) { return p && p.hash && p.timestamp; });
    var byRoute = detail.recentAdvertsByRoute;
    var html = '<h4 title="' + esc(TITLE_TIP) + '">Recent Adverts (' + all.length + ')</h4>';
    if (!byRoute) return html + listHtml(all, 'all', opts);

    var tabs = [{ key: 'all', label: 'All', rows: all }];
    CLASSES.forEach(function (c) {
      var rows = byRoute[c.key] || [];
      if (c.key !== 'unknown' || rows.length) tabs.push({ key: c.key, label: c.label, rows: rows });
    });
    var tab = tabs.some(function (t) { return t.key === opts.tab; }) ? opts.tab : 'all';

    html += countsHtml(detail.advertCounts) + noteHtml(detail.advertCounts);
    // initTabBar (bind) adds role="tablist"; it skips a bar that has one.
    html += '<div class="node-adverts-tabs" aria-label="Recent adverts by route">' + tabs.map(function (t) {
      var on = t.key === tab;
      return '<button type="button" class="tab-btn' + (on ? ' active' : '') + '" id="' + esc(prefix) + '-tab-' + t.key +
        '" role="tab" aria-selected="' + on + '" tabindex="' + (on ? '0' : '-1') + '" aria-controls="' + esc(prefix) + '-panel-' + t.key +
        '" data-adverts-tab="' + t.key + '">' + t.label + '</button>';
    }).join('') + '</div>';
    html += tabs.map(function (t) {
      return '<div class="node-adverts-panel" id="' + esc(prefix) + '-panel-' + t.key + '" role="tabpanel" aria-labelledby="' + esc(prefix) + '-tab-' + t.key + '"' +
        (t.key === tab ? '' : ' hidden') + '>' + listHtml(t.rows, t.key, opts) + '</div>';
    }).join('');
    return html;
  }

  /** The api() path of a node's detail with the advert route breakdown. */
  function detailPath(pubkey) {
    return '/nodes/' + encodeURIComponent(pubkey) + '?' + INCLUDE;
  }

  /** Wire the tab bar rendered into el; opts.onTabChange(key) on every switch. */
  function bind(el, opts) {
    var bar = el && el.querySelector('.node-adverts-tabs');
    if (!bar || typeof root.initTabBar !== 'function') return;
    root.initTabBar(bar, function (btn) {
      var btns = bar.querySelectorAll('[data-adverts-tab]');
      for (var i = 0; i < btns.length; i++) {
        var on = btns[i] === btn;
        btns[i].classList.toggle('active', on);
        var p = document.getElementById(btns[i].getAttribute('aria-controls'));
        if (p) p.hidden = !on;
      }
      if (opts && opts.onTabChange) opts.onTabChange(btn.getAttribute('data-adverts-tab'));
    });
  }

  root.NodeAdverts = { parseTab: parseTab, hashWithTab: hashWithTab, render: render, bind: bind, detailPath: detailPath };
})(typeof window !== 'undefined' ? window : this);
