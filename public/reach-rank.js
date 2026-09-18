/* === CoreScope — reach-rank.js ===
   Reach leaderboard (#/reach-rank): every visible node ranked by all-time
   neighbour count — the same Rank shown on each node's Reach page
   (#/nodes/<pubkey>/reach). One page of rows per request from
   /api/reach-rank; search and paging run server-side over one cached
   snapshot, and placements are global (a search never renumbers).
   Reuses the .nq-* report styles from node-reach.css. */
'use strict';
(function () {
  var PAGE_SIZE = 50;           // mirrors the server default (reachRankDefaultLimit)
  var MAX_QUERY = 64;           // mirrors reachRankMaxQueryLen
  var SEARCH_DEBOUNCE_MS = 250;
  var loadGen = 0;              // bumped per fetch + on destroy; drops stale responses
  var state = null;             // { q, page } while the page is mounted
  var searchTimer = null;

  // parseState reads ?q=&page= (1-based) from the hash query string.
  function parseState(params) {
    var q = String(params.get('q') || '').trim().slice(0, MAX_QUERY);
    var page = parseInt(params.get('page'), 10);
    return { q: q, page: isFinite(page) && page > 1 ? page : 1 };
  }

  // Keep search + page in the URL (replaceState: no router re-run, no history
  // entry) so Back from a node's Reach page returns to the same view.
  function syncHash() {
    var parts = [];
    if (state.q) parts.push('q=' + encodeURIComponent(state.q));
    if (state.page > 1) parts.push('page=' + state.page);
    try { history.replaceState(null, '', '#/reach-rank' + (parts.length ? '?' + parts.join('&') : '')); } catch (e) {}
  }

  // rowHtml renders one placement. Every node-controlled value is escaped;
  // a node without a name falls back to its pubkey (as on the Reach page).
  function rowHtml(r) {
    var pk = String(r.pubkey || '');
    var label = r.name ? escapeHtml(r.name) : escapeHtml(pk.slice(0, 12));
    var href = '#/nodes/' + encodeURIComponent(pk) + '/reach';
    return '<tr>' +
      '<td class="nq-n rr-rank">#' + escapeHtml(String(r.rank)) + '</td>' +
      '<td class="rr-node"><a class="nq-link" href="' + escapeHtml(href) + '">' +
      '<span class="sr-only">Reach page for </span>' + label + '</a>' +
      (r.name ? ' <span class="rr-pk" title="' + escapeHtml(pk) + '">' + escapeHtml(pk.slice(0, 8)) + '</span>' : '') +
      '</td>' +
      '<td class="nq-n">' + escapeHtml(String(r.neighbors)) + '</td></tr>';
  }

  function messageRow(text) {
    return '<tr><td colspan="3" class="rr-msg">' + escapeHtml(text) + '</td></tr>';
  }

  // statusText summarises the visible slice, e.g. "Showing 51–100 of 212
  // ranked nodes" or "Showing 1–3 of 3 matches for “ab” (212 ranked nodes)".
  function statusText(d) {
    var n = d.rows.length;
    if (d.q) {
      if (!d.matched) return 'No ranked node matches “' + d.q + '” (' + d.total + ' ranked nodes).';
      return 'Showing ' + (d.offset + 1) + '–' + (d.offset + n) + ' of ' + d.matched +
        ' match' + (d.matched === 1 ? '' : 'es') + ' for “' + d.q + '” (' + d.total + ' ranked nodes).';
    }
    if (!d.total) return 'No ranked nodes yet.';
    if (!n) return 'No rows on this page (' + d.total + ' ranked nodes).';
    return 'Showing ' + (d.offset + 1) + '–' + (d.offset + n) + ' of ' + d.total + ' ranked nodes.';
  }

  // snapshotText states the age of the data behind every placement.
  function snapshotText(iso) {
    var t = new Date(iso);
    if (!iso || !isFinite(t.getTime())) return 'Snapshot time unknown.';
    return 'Snapshot ' + t.toLocaleString() + ' (' + timeAgo(iso) + ').';
  }

  function shellHtml(q) {
    return '<div class="nq-head rr-head">' +
      '<h2 class="nq-title">Reach leaderboard</h2>' +
      '<div class="nq-sub">Nodes ranked by all-time neighbour count — the Rank shown on each node’s Reach page.</div>' +
      '<p class="nq-note rr-note" role="note"><strong>Historical neighbour count — not a measure of radio quality or range.</strong> ' +
      'Counts distinct neighbours in the all-time neighbour graph. Equal counts share a placement (1, 1, 3). Hidden nodes are not ranked.</p>' +
      '<p class="rr-meta" id="rrSnapshot"></p>' +
      '<form class="rr-search" id="rrSearchForm" role="search">' +
      '<label for="rrSearch">Search name or pubkey</label>' +
      '<input type="search" class="nodes-search" id="rrSearch" maxlength="' + MAX_QUERY + '" autocomplete="off" spellcheck="false" value="' + escapeHtml(q) + '">' +
      '</form></div>' +
      '<div class="nq-body">' +
      '<div class="nq-error rr-error" id="rrError" role="alert" hidden></div>' +
      '<table class="nq-table rr-table" id="rrTable">' +
      '<caption class="sr-only">Nodes ranked by all-time neighbour count</caption>' +
      '<thead><tr><th scope="col" class="nq-n">Rank</th><th scope="col">Node</th><th scope="col" class="nq-n">Neighbours</th></tr></thead>' +
      '<tbody id="rrRows"><tr><td colspan="3" class="rr-msg">Loading leaderboard…</td></tr></tbody></table>' +
      '<nav class="rr-pager" aria-label="Leaderboard pages">' +
      '<button type="button" class="btn" id="rrPrev" disabled>Previous</button>' +
      '<span class="nq-count" id="rrStatus" aria-live="polite"></span>' +
      '<button type="button" class="btn" id="rrNext" disabled>Next</button>' +
      '</nav></div>';
  }

  function el(id) { return document.getElementById(id); }

  function setError(msg) {
    var box = el('rrError');
    if (!box) return;
    box.textContent = msg || '';
    box.hidden = !msg;
  }

  // Disabling the focused pager button would drop focus to <body>; hand it
  // to the other button instead so keyboard users keep their place.
  function setPager(hasPrev, hasNext) {
    var prev = el('rrPrev'), next = el('rrNext');
    var active = document.activeElement;
    prev.disabled = !hasPrev;
    next.disabled = !hasNext;
    if (active === prev && prev.disabled && !next.disabled) next.focus();
    else if (active === next && next.disabled && !prev.disabled) prev.focus();
  }

  async function fetchPage() {
    var myGen = ++loadGen;
    var offset = (state.page - 1) * PAGE_SIZE;
    var qs = 'offset=' + offset + '&limit=' + PAGE_SIZE + (state.q ? '&q=' + encodeURIComponent(state.q) : '');
    var table = el('rrTable');
    if (table) table.setAttribute('aria-busy', 'true');
    var d;
    try {
      d = await api('/reach-rank?' + qs, { ttl: 30000 });
    } catch (e) {
      if (myGen !== loadGen) return;
      if (table) table.removeAttribute('aria-busy');
      setError('Failed to load the leaderboard: ' + e.message);
      el('rrRows').innerHTML = messageRow('Leaderboard unavailable.');
      el('rrStatus').textContent = '';
      setPager(state.page > 1, false);
      return;
    }
    if (myGen !== loadGen) return; // a newer search/page (or destroy) won
    table.removeAttribute('aria-busy');
    setError('');
    // A page past the end (e.g. a stale ?page= link) snaps back to the last one.
    var count = d.q ? d.matched : d.total;
    var lastPage = Math.max(1, Math.ceil(count / PAGE_SIZE));
    if (state.page > lastPage) {
      state.page = lastPage;
      syncHash();
      fetchPage();
      return;
    }
    var rows = d.rows || [];
    var rowsHtml = rows.length ? rows.map(rowHtml).join('') : messageRow(statusText(d)); // escaped per field
    el('rrRows').innerHTML = rowsHtml;
    el('rrStatus').textContent = statusText(d);
    el('rrSnapshot').textContent = snapshotText(d.snapshot_at);
    setPager(state.page > 1, offset + rows.length < count);
  }

  function setQuery(q) {
    q = String(q || '').trim().slice(0, MAX_QUERY);
    if (q === state.q) return;
    state.q = q;
    state.page = 1;
    syncHash();
    fetchPage();
  }

  function wire() {
    var input = el('rrSearch');
    input.addEventListener('input', function () {
      clearTimeout(searchTimer);
      searchTimer = setTimeout(function () { setQuery(input.value); }, SEARCH_DEBOUNCE_MS);
    });
    el('rrSearchForm').addEventListener('submit', function (e) {
      e.preventDefault(); // Enter searches immediately
      clearTimeout(searchTimer);
      setQuery(input.value);
    });
    el('rrPrev').addEventListener('click', function () {
      if (state.page > 1) { state.page--; syncHash(); fetchPage(); }
    });
    el('rrNext').addEventListener('click', function () {
      state.page++; syncHash(); fetchPage();
    });
  }

  function init(container) {
    state = parseState(getHashParams());
    container.innerHTML = shellHtml(state.q);
    wire();
    fetchPage();
  }

  function destroy() {
    loadGen++;
    clearTimeout(searchTimer);
    searchTimer = null;
    state = null;
  }

  registerPage('reach-rank', { init: init, destroy: destroy });

  // Pure helpers, exposed for test-reach-rank.js (vm sandbox).
  window.ReachRank = { rowHtml: rowHtml, statusText: statusText, snapshotText: snapshotText, parseState: parseState };
})();
