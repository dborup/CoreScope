// Observer Neighbors tool — network-wide list of every observer's
// firmware-reported direct (zero-hop) neighbors, flattened across all
// observers into one searchable/sortable table (Tools > Observer
// Neighbors). Requested by dborup as a single place to see this instead
// of clicking into each observer's Direct Neighbors panel individually
// (public/observer-detail.js's renderDirectNeighbors, whose row shape and
// col-scope-list wrap-fix this reuses).
(function () {
  'use strict';

  var container = null;
  var allRows = [];
  var unknownScopes = [];
  var filterText = '';
  var sortState = { col: 'observer', dir: 'asc' };
  var scopeAdminKey = '';
  var scopeDecisions = [];
  var scopeClickHandler = null;

  function escapeHtml(s) {
    return s == null ? '' : String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;').replace(/'/g, '&#39;');
  }

  function init(app) {
    container = app;
    allRows = [];
    unknownScopes = [];
    scopeAdminKey = '';
    scopeDecisions = [];
    filterText = '';
    sortState = { col: 'observer', dir: 'asc' };

    container.innerHTML =
      '<div class="tools-landing" style="max-width:1100px">' +
        '<h2>Observer Neighbors</h2>' +
        '<p class="help-text">Every observer\'s firmware-reported direct (zero-hop) neighbors, network-wide. Ground truth from each observer\'s own /neighbors report -- distinct from the packet-path-inferred neighbor graph. Click a column header to sort.</p>' +
        '<div id="obs-nb-unknown-scopes-wrap"></div>' +
        '<details id="obs-nb-scope-admin" class="analytics-card" style="margin:12px 0"><summary>Admin: review region scopes</summary>' +
        '<p class="text-muted">Approval enables future scope matching; it does not rewrite historical packets. config.json and hashRegionsPath remain the baseline.</p>' +
        '<label>Admin API key <input id="obs-nb-admin-key" type="password" autocomplete="off" class="input"></label> ' +
        '<button id="obs-nb-admin-load" type="button" class="btn btn-secondary">Load decisions</button>' +
        '<p id="obs-nb-admin-status" role="status" class="text-muted"></p><div id="obs-nb-admin-list"></div></details>' +
        '<div style="margin:12px 0"><input type="text" id="obs-nb-filter" class="input" placeholder="Filter by observer or neighbor…" style="width:100%;max-width:420px"></div>' +
        '<div id="obs-nb-status" class="text-muted" style="font-size:12px;margin-bottom:8px"></div>' +
        '<div id="obs-nb-table-wrap" class="table-fluid-wrap"></div>' +
      '</div>';

    var filterInput = document.getElementById('obs-nb-filter');
    if (filterInput) {
      filterInput.addEventListener('input', function () {
        filterText = filterInput.value.toLowerCase();
        renderTable();
      });
    }

    var adminLoad = document.getElementById('obs-nb-admin-load');
    if (adminLoad) adminLoad.addEventListener('click', function () {
      scopeAdminKey = document.getElementById('obs-nb-admin-key').value;
      loadScopeDecisions();
    });
    scopeClickHandler = function (e) {
      var button = e.target.closest('[data-scope-action]');
      if (!button || !scopeAdminKey) return;
      decideScope(button.getAttribute('data-scope-action'), button.getAttribute('data-scope-name'));
    };
    container.addEventListener('click', scopeClickHandler);

    load();
  }

  function destroy() {
    if (container && scopeClickHandler) container.removeEventListener('click', scopeClickHandler);
    scopeClickHandler = null;
    container = null;
    allRows = [];
    unknownScopes = [];
    scopeAdminKey = '';
    scopeDecisions = [];
  }

  function load() {
    var statusEl = document.getElementById('obs-nb-status');
    var wrap = document.getElementById('obs-nb-table-wrap');
    if (wrap) wrap.innerHTML = '<p class="text-muted">Loading…</p>';
    fetch('/api/observers/neighbors')
      .then(function (r) {
        if (!r.ok) return r.json().then(function (d) { throw new Error(d.error || 'Request failed'); });
        return r.json();
      })
      .then(function (data) {
        allRows = (data && Array.isArray(data.neighbors)) ? data.neighbors : [];
        unknownScopes = (data && Array.isArray(data.unknownScopes)) ? data.unknownScopes : [];
        renderUnknownScopes();
        renderTable();
      })
      .catch(function (e) {
        if (wrap) wrap.innerHTML = '';
        if (statusEl) statusEl.textContent = 'Failed to load: ' + e.message;
      });
  }

  // "Scopes CoreScope doesn't know about yet" -- region-scope names seen
  // in reported neighbor scope lists that aren't part of this
  // deployment's configured hashRegions (dborup: "kan vi have en panel
  // med scopes vi ikke kender på corescope som observer neighbors har
  // fundet"). Computed server-side (computeUnknownScopes, db.go) from the
  // same rows this page already fetches -- no second request.
  function renderUnknownScopes() {
    var wrap = document.getElementById('obs-nb-unknown-scopes-wrap');
    if (!wrap) return;
    if (unknownScopes.length === 0) {
      wrap.innerHTML = '';
      return;
    }
    var rows = unknownScopes.map(function (u) {
      var adminButtons = scopeAdminKey ? '<td>' + scopeButton('approve', u.scope) + ' ' + scopeButton('reject', u.scope) + '</td>' : '';
      return '<tr>' +
        '<td><code>' + escapeHtml(u.scope) + '</code></td>' +
        '<td style="text-align:right">' + u.count.toLocaleString() + '</td>' +
        '<td class="text-muted" style="font-size:0.85em">' + (u.examples || []).map(escapeHtml).join(', ') + '</td>' + adminButtons +
        '</tr>';
    }).join('');
    wrap.innerHTML =
      '<div class="analytics-card" style="margin:12px 0">' +
        '<h3 style="margin:0 0 4px">Scopes CoreScope Doesn\'t Know About Yet (' + unknownScopes.length.toLocaleString() + ')</h3>' +
        '<p class="text-muted" style="margin:0 0 8px;font-size:0.85em">Region-scope names reported in the wild by neighbors\' OTA scope query, but not part of this deployment\'s configured regions. Might be worth adding to config -- or just neighboring mesh communities using their own naming.</p>' +
        '<table class="data-table"><thead><tr><th>Scope</th><th style="text-align:right">Seen By</th><th>Example Neighbors</th>' + (scopeAdminKey ? '<th>Admin</th>' : '') + '</tr></thead><tbody>' + rows + '</tbody></table>' +
      '</div>';
  }

  function scopeButton(action, name) {
    return '<button type="button" class="btn btn-secondary" data-scope-action="' + escapeHtml(action) + '" data-scope-name="' + escapeHtml(name) + '">' + escapeHtml(action) + '</button>';
  }

  function adminStatus(message) {
    var el = document.getElementById('obs-nb-admin-status');
    if (el) el.textContent = message;
  }

  function adminFetch(path, opts) {
    opts = opts || {};
    opts.headers = Object.assign({ 'X-API-Key': scopeAdminKey }, opts.headers || {});
    return fetch(path, opts).then(function (r) {
      return r.json().then(function (body) {
        if (!r.ok) throw new Error(body.error || 'Request failed (' + r.status + ')');
        return body;
      });
    });
  }

  function loadScopeDecisions() {
    if (!scopeAdminKey) { adminStatus('Enter the admin API key.'); return; }
    adminStatus('Loading decisions…');
    adminFetch('/api/admin/region-scopes').then(function (data) {
      scopeDecisions = Array.isArray(data.decisions) ? data.decisions : [];
      renderScopeDecisions();
      renderUnknownScopes();
      adminStatus('Decisions loaded. The key stays in this tab only.');
    }).catch(function (e) { scopeAdminKey = ''; renderUnknownScopes(); adminStatus(e.message); });
  }

  function renderScopeDecisions() {
    var wrap = document.getElementById('obs-nb-admin-list');
    if (!wrap) return;
    var rows = scopeDecisions.map(function (d) {
      return '<tr><td><code>' + escapeHtml(d.name) + '</code></td><td>' + escapeHtml(d.status) + '</td><td>' +
        (d.status === 'approved' ? scopeButton('revoke', d.name) : '') + '</td></tr>';
    }).join('');
    wrap.innerHTML = rows ? '<table class="data-table"><thead><tr><th>Scope</th><th>Status</th><th>Action</th></tr></thead><tbody>' + rows + '</tbody></table>' :
      '<p class="text-muted">No scope decisions yet.</p>';
  }

  function decideScope(action, name) {
    if (!scopeAdminKey || !name || ['approve', 'reject', 'revoke'].indexOf(action) < 0) return;
    if (typeof window.confirm === 'function' && !window.confirm('Confirm ' + action + ' for ' + name + '? This changes future scope matching.')) return;
    adminStatus('Submitting ' + action + ' for ' + name + '…');
    adminFetch('/api/admin/region-scopes/' + action, {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ name: name })
    }).then(function (accepted) {
      var attempts = 0;
      function poll() {
        return adminFetch('/api/admin/region-scopes/requests/' + encodeURIComponent(accepted.requestId)).then(function (state) {
          if (state.status === 'queued' && attempts++ < 15) return new Promise(function (resolve) { setTimeout(resolve, 1000); }).then(poll);
          if (state.status === 'error') throw new Error(state.error || 'Decision failed');
          if (state.status === 'queued') throw new Error('Decision still processing; reload decisions shortly.');
          adminStatus(name + ': ' + state.status);
          loadScopeDecisions();
          load();
        });
      }
      return poll();
    }).catch(function (e) { adminStatus(e.message); });
  }

  function sortValue(row, col) {
    switch (col) {
      case 'observer': return (row.observerName || row.observerId || '').toLowerCase();
      case 'neighbor': return (row.neighborName || row.neighborPubkey || '').toLowerCase();
      case 'evidence': return row.seenViaPackets ? 1 : 0;
      case 'status': return (row.status || '').toLowerCase();
      case 'reportedAt': return row.reportedAt || '';
      default: return '';
    }
  }

  function sortArrow(col) {
    if (col !== sortState.col) return '<span class="sort-arrow">⇅</span>';
    return '<span class="sort-arrow">' + (sortState.dir === 'asc' ? '↑' : '↓') + '</span>';
  }

  function sortTh(col, label) {
    var cls = 'sortable' + (col === sortState.col ? ' sort-active' : '');
    return '<th class="' + cls + '" data-sort-col="' + col + '">' + label + sortArrow(col) + '</th>';
  }

  function matchesFilter(row) {
    if (!filterText) return true;
    var observer = (row.observerName || row.observerId || '').toLowerCase();
    var neighbor = (row.neighborName || row.neighborPubkey || '').toLowerCase();
    return observer.indexOf(filterText) !== -1 || neighbor.indexOf(filterText) !== -1;
  }

  function renderTable() {
    var statusEl = document.getElementById('obs-nb-status');
    var wrap = document.getElementById('obs-nb-table-wrap');
    if (!wrap) return;

    if (allRows.length === 0) {
      if (statusEl) statusEl.textContent = '';
      wrap.innerHTML = '<p class="text-muted">No observer has reported any direct neighbors yet.</p>';
      return;
    }

    var filtered = allRows.filter(matchesFilter);
    var mult = sortState.dir === 'asc' ? 1 : -1;
    var sorted = filtered.slice().sort(function (a, b) {
      var av = sortValue(a, sortState.col), bv = sortValue(b, sortState.col);
      if (av < bv) return -1 * mult;
      if (av > bv) return 1 * mult;
      return 0;
    });

    if (statusEl) {
      statusEl.textContent = filtered.length.toLocaleString() + ' of ' + allRows.length.toLocaleString() + ' neighbor pairs' +
        (filterText ? ' (filtered)' : '');
    }

    if (sorted.length === 0) {
      wrap.innerHTML = '<p class="text-muted">No rows match "' + escapeHtml(filterText) + '".</p>';
      return;
    }

    var rows = sorted.map(function (row) {
      var observerLabel = row.observerName ? escapeHtml(row.observerName) : escapeHtml(row.observerId);
      var observerCell = '<a href="#/observers/' + encodeURIComponent(row.observerId) + '">' + observerLabel + '</a>';

      var neighborLabel = row.neighborName ? escapeHtml(row.neighborName) : escapeHtml(String(row.neighborPubkey).slice(0, 12)) + '…';
      var neighborCell = row.neighborName
        ? '<a href="#/nodes/' + encodeURIComponent(row.neighborPubkey) + '">' + neighborLabel + '</a>'
        : '<span class="mono">' + neighborLabel + '</span>';

      var scopeCell = row.scopes
        ? '<span class="badge-region">' + escapeHtml(row.scopes) + '</span>'
        : (row.status === 'timeout'
          ? '<span class="text-muted" title="Scope query timed out">no reply</span>'
          : '<span class="text-muted">—</span>');

      var evidenceCell = row.seenViaPackets
        ? '<span class="text-muted" title="A packet path connecting this station and the observer has been resolved">confirmed</span>'
        : '<span style="color:var(--text-muted)" title="Firmware reports this as a direct RF neighbor, but no packet path between the two has been resolved yet.">not seen yet</span>';

      var reportedCell = row.reportedAt
        ? '<span title="' + escapeHtml(row.reportedAt) + '">' + (typeof timeAgo === 'function' ? timeAgo(row.reportedAt) : escapeHtml(row.reportedAt)) + '</span>'
        : '<span class="text-muted">—</span>';

      return '<tr><td>' + observerCell + '</td><td>' + neighborCell + '</td><td class="col-scope-list">' + scopeCell + '</td>' +
        '<td>' + evidenceCell + '</td><td>' + escapeHtml(row.status || '') + '</td><td>' + reportedCell + '</td></tr>';
    }).join('');

    wrap.innerHTML =
      '<table class="data-table" id="obs-nb-table"><thead><tr>' +
        sortTh('observer', 'Observer') +
        sortTh('neighbor', 'Neighbor') +
        '<th>Configured Scope</th>' +
        sortTh('evidence', 'Packet Evidence') +
        sortTh('status', 'Status') +
        sortTh('reportedAt', 'Reported') +
      '</tr></thead><tbody>' + rows + '</tbody></table>';

    var table = document.getElementById('obs-nb-table');
    if (table) {
      table.querySelectorAll('th[data-sort-col]').forEach(function (th) {
        th.addEventListener('click', function () {
          var col = th.dataset.sortCol;
          if (sortState.col === col) {
            sortState.dir = sortState.dir === 'asc' ? 'desc' : 'asc';
          } else {
            sortState.col = col;
            sortState.dir = (col === 'observer' || col === 'neighbor' || col === 'status') ? 'asc' : 'desc';
          }
          renderTable();
        });
      });
    }
  }

  window.ObserverNeighborsTool = { init: init, destroy: destroy, sortValue: sortValue };
  if (typeof registerPage === 'function') registerPage('observer-neighbors-tool', { init: init, destroy: destroy });
})();
