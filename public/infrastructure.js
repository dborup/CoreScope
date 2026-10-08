(function () {
  'use strict';

  var active = 0;
  var map = null;
  var adminKey = '';

  function number(v) { var n = Number(v); return Number.isFinite(n) ? n : 0; }

  // Suggestions are only a ranked shortlist, never automatically selected.
  // A recorded relay is required so idle/geographically speculative nodes do
  // not appear solely because a structural score happened to be high.
  function rankCandidates(nodes, selected) {
    return (nodes || []).filter(function (n) {
      return n && n.role === 'repeater' && n.public_key && !selected.has(n.public_key) && number(n.relay_count_24h) > 0;
    }).sort(function (a, b) {
      return number(b.relay_count_24h) - number(a.relay_count_24h) ||
        number(b.bridge_score) - number(a.bridge_score) ||
        String(a.public_key).localeCompare(String(b.public_key));
    }).slice(0, 20);
  }

  function el(tag, className, content) {
    var node = document.createElement(tag);
    if (className) node.className = className;
    if (content != null) node.textContent = String(content);
    return node;
  }

  function appendNodeRow(parent, node, selected, onDecision) {
    var row = el('div', 'infrastructure-row');
    var detail = el('div', 'infrastructure-row-detail');
    var link = el('a', '', node.name || node.public_key);
    link.href = '#/nodes/' + encodeURIComponent(node.public_key);
    detail.appendChild(link);
    detail.appendChild(el('small', '', 'Relayed 24h: ' + number(node.relay_count_24h) + ' · Bridge: ' + number(node.bridge_score).toFixed(2)));
    row.appendChild(detail);
    var btn = el('button', 'btn btn-secondary', selected ? 'Remove' : 'Select');
    btn.type = 'button';
    btn.hidden = !adminKey;
    btn.addEventListener('click', function () { onDecision(node.public_key, selected ? 'remove' : 'select'); });
    row.appendChild(btn);
    parent.appendChild(row);
  }

  function renderMap(container, nodes) {
    if (map) { map.remove(); map = null; }
    if (!window.L || !nodes.length) { container.hidden = true; return; }
    var points = nodes.filter(function (n) { return n.lat != null && n.lon != null && Number.isFinite(Number(n.lat)) && Number.isFinite(Number(n.lon)) && Math.abs(Number(n.lat)) <= 90 && Math.abs(Number(n.lon)) <= 180 && (Number(n.lat) !== 0 || Number(n.lon) !== 0); });
    if (!points.length) { container.hidden = true; return; }
    container.hidden = false;
    map = L.map(container, {scrollWheelZoom:false}).setView([Number(points[0].lat), Number(points[0].lon)], 8);
    var dark = document.documentElement.getAttribute('data-theme') === 'dark';
    var spec = window.MC_getTileSpec && window.MC_getTileSpec(dark ? 'dark' : 'light');
    if (!spec || !spec.url) spec = {url:'https://tile.openstreetmap.org/{z}/{x}/{y}.png', attribution:'© OpenStreetMap contributors'};
    L.tileLayer(spec.url, {attribution:spec.attribution, maxZoom:19}).addTo(map);
    var color = getComputedStyle(document.documentElement).getPropertyValue('--accent').trim() || 'currentColor';
    var bounds = [];
    points.forEach(function (n) {
      var coord = [Number(n.lat), Number(n.lon)];
      bounds.push(coord);
      var marker = L.circleMarker(coord, {radius:7, color:color, fillColor:color, fillOpacity:0.8}).addTo(map);
      var popup = el('a', '', n.name || n.public_key);
      popup.href = '#/nodes/' + encodeURIComponent(n.public_key);
      marker.bindPopup(popup);
    });
    if (bounds.length > 1) map.fitBounds(bounds, {padding:[20,20], maxZoom:12});
    setTimeout(function () { if (map) map.invalidateSize(); }, 0);
  }

  async function init(container) {
    var run = ++active;
    adminKey = '';
    container.replaceChildren();
    var root = el('section', 'infrastructure-page');
    root.appendChild(el('h1', '', 'Infrastructure'));
    root.appendChild(el('p', 'text-muted', 'A curated view of important repeaters. Activity-based suggestions are not added automatically.'));
    var status = el('p', 'text-muted', 'Loading…');
    var mapBox = el('div', 'infrastructure-map');
    var selectedTitle = el('h2', '', 'Selected repeaters');
    var selectedList = el('div', 'infrastructure-list');
    var candidateTitle = el('h2', '', 'Suggested candidates');
    var candidateList = el('div', 'infrastructure-list');
    var admin = el('details', 'infrastructure-admin');
    admin.appendChild(el('summary', '', 'Admin controls'));
    var keyInput = el('input');
    keyInput.type = 'password'; keyInput.placeholder = 'API key (this tab only)'; keyInput.autocomplete = 'off';
    var unlock = el('button', 'btn btn-secondary', 'Enable controls'); unlock.type = 'button';
    unlock.addEventListener('click', async function () {
      var candidateKey = keyInput.value;
      keyInput.value = '';
      adminKey = '';
      if (!candidateKey) { status.textContent = 'Enter an API key first.'; render(); return; }
      try {
        var check = await fetch('/api/admin/infrastructure/auth', {headers:{'X-API-Key':candidateKey}});
        if (run !== active) return;
        if (!check.ok) { status.textContent = 'API key was not accepted (' + check.status + ').'; render(); return; }
        adminKey = candidateKey;
        await load();
      } catch (_) { if (run === active) { status.textContent = 'Could not verify API key.'; render(); } }
    });
    admin.appendChild(keyInput); admin.appendChild(unlock);
    root.appendChild(admin); root.appendChild(status); root.appendChild(mapBox);
    root.appendChild(selectedTitle); root.appendChild(selectedList);
    root.appendChild(candidateTitle); root.appendChild(el('p', 'text-muted', 'Top 20 repeaters with observed relays in the last 24 hours, ranked by relay count then bridge score. Review before selecting.'));
    root.appendChild(candidateList); container.appendChild(root);

    var nodes = [];
    var curated = [];
    async function decide(key, op) {
      if (!adminKey || !window.confirm((op === 'select' ? 'Select' : 'Remove') + ' this repeater?')) return;
      try {
        var response = await fetch('/api/admin/infrastructure/' + encodeURIComponent(key) + '/' + op, {method:'POST', headers:{'X-API-Key':adminKey}});
        if (!response.ok) { status.textContent = 'Could not queue change (' + response.status + ').'; return; }
        var result = await response.json();
        status.textContent = 'Change queued…';
        for (var i = 0; i < 8 && run === active; i++) {
          await new Promise(function (resolve) { setTimeout(resolve, 2000); });
          if (run !== active) return;
          var check = await fetch('/api/admin/infrastructure/requests/' + encodeURIComponent(result.requestId), {headers:{'X-API-Key':adminKey}});
          if (!check.ok) { status.textContent = 'Could not check change (' + check.status + ').'; return; }
          var outcome = await check.json();
          if (outcome.status !== 'queued') {
            status.textContent = outcome.status === 'error' ? (outcome.error || 'Change failed') : 'Selection updated.';
            if (outcome.status !== 'error') await load();
            return;
          }
        }
        status.textContent = 'Still queued; reload to check later.';
      } catch (_) {
        if (run === active) status.textContent = 'Could not reach the server. No selection was confirmed.';
      }
    }
    function render() {
      selectedList.replaceChildren(); candidateList.replaceChildren();
      var byKey = new Map(nodes.map(function (n) { return [n.public_key, n]; }));
      var selected = new Set(curated.map(function (e) { return e.publicKey; }));
      var selectedNodes = curated.map(function (e) { return byKey.get(e.publicKey) || {public_key:e.publicKey, name:'Node not in current list'}; });
      selectedNodes.forEach(function (n) { appendNodeRow(selectedList, n, true, decide); });
      if (!selectedNodes.length) selectedList.appendChild(el('p', 'text-muted', 'No repeaters selected yet.'));
      rankCandidates(nodes, selected).forEach(function (n) { appendNodeRow(candidateList, n, false, decide); });
      if (!candidateList.children.length) candidateList.appendChild(el('p', 'text-muted', 'No active candidates in the current node snapshot.'));
      renderMap(mapBox, selectedNodes);
    }
    async function load() {
      try {
        var data = await Promise.all([api('/infrastructure', {ttl:0}), fetchAllNodes('', {ttl:60000, safetyCap:10000})]);
        if (run !== active) return;
        curated = Array.isArray(data[0].selected) ? data[0].selected : [];
        nodes = data[1].nodes || [];
        status.textContent = curated.length + ' selected · ' + nodes.length + ' nodes considered' +
          (nodes.length >= 10000 ? ' · Node-list safety cap reached; suggestions may be incomplete.' : '');
        render();
      } catch (_) { if (run === active) status.textContent = 'Infrastructure data is unavailable.'; }
    }
    load();
  }

  function destroy() { ++active; adminKey = ''; if (map) { map.remove(); map = null; } }

  window.InfrastructurePage = {rankCandidates:rankCandidates, init:init, destroy:destroy};
  if (typeof registerPage === 'function') registerPage('infrastructure', window.InfrastructurePage);
})();
