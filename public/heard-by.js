/* === CoreScope — heard-by.js ===
 *
 * Heard By (#76), shared by the node detail page, the node side panel, the
 * Live node panel, the home My Mesh cards and the Analytics node rankings.
 * The node health endpoints return two lists (see docs/api-spec.md):
 *   directObservers - observers that received the node's own transmission,
 *                     with signal values averaged over those receptions;
 *   relayObservers  - every other observer of the node's traffic, without
 *                     signal values (they would describe another node).
 * Both describe observed routing evidence, not radio range.
 *
 * Everything returned as HTML is built from escaped values only.
 */
(function (root) {
  'use strict';

  var DIRECT_TIP = 'Observers that received this node\'s own transmission on air. Only adverts can show that, read per observation: ' +
    'this node\'s own advert with an empty flood path or a zero-hop (empty) direct path, or any flood advert whose last hop is this node by full key or by a hash prefix no other relay shares. ' +
    'Packets, SNR and RSSI cover those receptions only, so the signal values are this node\'s. This is observed routing evidence, not a radio range.';
  var RELAY_TIP = 'Observers that saw traffic from, to or through this node, but no observation shows they received this node\'s own transmission: ' +
    'relayed copies, non-advert packets, direct routes and ambiguous hash prefixes do not identify who transmitted. ' +
    'Their SNR and RSSI belong to another transmitter, so none are shown. Packets counts the transmissions each observer saw.';
  var EMPTY_DIRECT = 'No observation proves a direct reception here, which is not the same as being out of range.';
  var EMPTY_DIRECT_TIP = 'Only adverts identify who was heard, and only while their route and path show it. ' +
    'Direct routes with a path, non-advert traffic and hash prefixes shared with another relay are left unattributed rather than guessed, ' +
    'so an empty list is missing evidence, not proof of missing coverage.';
  var SNR_TIP = 'Mean SNR over this node\'s direct receptions only (see Heard By); empty when no observation proves one.';

  var helpSeq = 0;

  function esc(s) {
    return String(s == null ? '' : s)
      .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
  }

  function arr(v) { return Array.isArray(v) ? v : []; }

  /** The two lists of a node health (or bulk-health) entry. */
  function lists(h) {
    return { direct: arr(h && h.directObservers), relay: arr(h && h.relayObservers) };
  }

  function counts(h) {
    var l = lists(h);
    return { direct: l.direct.length, relay: l.relay.length, total: l.direct.length + l.relay.length };
  }

  function plural(n, one, many) { return n + ' ' + (n === 1 ? one : many); }

  /** Short text, e.g. "2 direct · 5 via relay". */
  function summary(h) {
    var c = counts(h);
    return c.direct + ' direct · ' + c.relay + ' via relay';
  }

  function observerName(o) {
    return (o && (o.observer_name || o.observer_id)) || 'Unknown';
  }

  function num(v) { return v == null || v === '' ? null : Number(v); }

  function snrText(o, sep) {
    var v = num(o.avgSnr);
    return v == null || isNaN(v) ? null : v.toFixed(1) + sep + 'dB';
  }

  function rssiText(o, sep) {
    var v = num(o.avgRssi);
    return v == null || isNaN(v) ? null : v.toFixed(0) + sep + 'dBm';
  }

  // #1290 tri-state: false = listener, true = repeater, absent = unknown.
  function relayBadge(o) {
    if (o.can_relay === false) return ' <span class="badge-listener" title="Firmware reported repeat:off — excluded from path-hop disambiguator (#1290)">listener</span>';
    if (o.can_relay === true) return ' <span class="badge-repeater" title="Firmware reported repeat:on — eligible as a path hop">repeater</span>';
    return '';
  }

  /** A focusable help button carrying tip as its tooltip. */
  function help(label, tip, cls) {
    var id = 'heardByHelp' + (++helpSeq);
    return '<button type="button" class="sort-help heard-by-help' + (cls ? ' ' + cls : '') + '" aria-label="' + esc(label) + ' help" aria-describedby="' + id + '">' +
      esc(label) + ' <span aria-hidden="true">ⓘ</span><span class="sort-help-tip" id="' + id + '" role="tooltip">' + esc(tip) + '</span></button>';
  }

  function nameCell(o, opts) {
    var name = esc(observerName(o));
    if (opts && opts.link && o.observer_id) {
      name = '<a href="#/observers/' + encodeURIComponent(o.observer_id) + '" class="heard-by-link">' + name + '</a>';
    }
    return name + relayBadge(o);
  }

  function heading(h, tag) {
    var c = counts(h);
    return '<' + tag + ' class="heard-by-title">Heard By (' + plural(c.total, 'observer', 'observers') + ')</' + tag + '>';
  }

  function emptyDirect() {
    return '<p class="text-muted heard-by-empty" title="' + esc(EMPTY_DIRECT_TIP) + '">' + esc(EMPTY_DIRECT) + '</p>';
  }

  /** Full detail page: one sortable table per list. */
  function renderTables(h) {
    var l = lists(h);
    var html = heading(h, 'h4');
    html += '<h5 class="heard-by-subtitle" data-heard="direct">' + help('Heard directly', DIRECT_TIP) + ' <span class="heard-by-count">(' + l.direct.length + ')</span></h5>';
    if (l.direct.length) {
      html += '<table class="data-table observer-sort-table heard-by-table" data-heard="direct">' +
        '<thead><tr>' +
        '<th scope="col" data-sort-key="observer">Observer</th>' +
        '<th scope="col" data-sort-key="packets" data-type="numeric" data-sort-default="desc">Packets</th>' +
        '<th scope="col" data-sort-key="snr" data-type="numeric" data-sort-default="desc">Avg SNR</th>' +
        '<th scope="col" data-sort-key="rssi" data-type="numeric" data-sort-default="desc">Avg RSSI</th>' +
        '</tr></thead><tbody>' +
        l.direct.map(function (o) {
          var snr = num(o.avgSnr), rssi = num(o.avgRssi);
          return '<tr>' +
            '<td data-value="' + esc(observerName(o).toLowerCase()) + '" class="heard-by-name">' + nameCell(o) + '</td>' +
            '<td data-value="' + (Number(o.packetCount) || 0) + '">' + (Number(o.packetCount) || 0) + '</td>' +
            '<td data-value="' + (snr == null ? '' : snr) + '">' + (snrText(o, ' ') || '—') + '</td>' +
            '<td data-value="' + (rssi == null ? '' : rssi) + '">' + (rssiText(o, ' ') || '—') + '</td>' +
            '</tr>';
        }).join('') +
        '</tbody></table>';
    } else {
      html += emptyDirect();
    }
    html += '<h5 class="heard-by-subtitle" data-heard="relay">' + help('Heard via relay', RELAY_TIP) + ' <span class="heard-by-count">(' + l.relay.length + ')</span></h5>';
    if (l.relay.length) {
      html += '<table class="data-table observer-sort-table heard-by-table" data-heard="relay">' +
        '<thead><tr>' +
        '<th scope="col" data-sort-key="observer">Observer</th>' +
        '<th scope="col" data-sort-key="packets" data-type="numeric" data-sort-default="desc">Packets</th>' +
        '</tr></thead><tbody>' +
        l.relay.map(function (o) {
          return '<tr>' +
            '<td data-value="' + esc(observerName(o).toLowerCase()) + '" class="heard-by-name">' + nameCell(o) + '</td>' +
            '<td data-value="' + (Number(o.packetCount) || 0) + '">' + (Number(o.packetCount) || 0) + '</td>' +
            '</tr>';
        }).join('') +
        '</tbody></table>';
    }
    return html;
  }

  function row(o, opts, stats) {
    return '<div class="observer-row heard-by-row" data-heard="' + (opts.kind || '') + '">' +
      '<span class="heard-by-name">' + nameCell(o, opts) + '</span>' +
      '<span class="heard-by-stats">' + esc(stats.join(' · ')) + '</span></div>';
  }

  /** Compact list (side panel, Live panel). opts.link links observer names. */
  function renderList(h, opts) {
    opts = opts || {};
    var l = lists(h);
    var html = heading(h, opts.headingTag || 'h4');
    html += '<div class="heard-by-subtitle" data-heard="direct">' + help('Heard directly', DIRECT_TIP) + ' <span class="heard-by-count">(' + l.direct.length + ')</span></div>';
    if (l.direct.length) {
      html += '<div class="observer-list" data-heard="direct">' + l.direct.map(function (o) {
        var stats = [(Number(o.packetCount) || 0) + ' pkts'];
        var snr = snrText(o, ''), rssi = rssiText(o, ' ');
        if (snr) stats.push('SNR ' + snr);
        if (rssi) stats.push('RSSI ' + rssi);
        return row(o, { link: opts.link, kind: 'direct' }, stats);
      }).join('') + '</div>';
    } else {
      html += emptyDirect();
    }
    html += '<div class="heard-by-subtitle" data-heard="relay">' + help('Heard via relay', RELAY_TIP) + ' <span class="heard-by-count">(' + l.relay.length + ')</span></div>';
    if (l.relay.length) {
      html += '<div class="observer-list" data-heard="relay">' + l.relay.map(function (o) {
        return row(o, { link: opts.link, kind: 'relay' }, [(Number(o.packetCount) || 0) + ' pkts']);
      }).join('') + '</div>';
    }
    return html;
  }

  root.HeardBy = {
    DIRECT_TIP: DIRECT_TIP,
    RELAY_TIP: RELAY_TIP,
    EMPTY_DIRECT: EMPTY_DIRECT,
    EMPTY_DIRECT_TIP: EMPTY_DIRECT_TIP,
    SNR_TIP: SNR_TIP,
    lists: lists,
    counts: counts,
    summary: summary,
    observerName: observerName,
    help: help,
    renderTables: renderTables,
    renderList: renderList
  };
})(typeof window !== 'undefined' ? window : this);
