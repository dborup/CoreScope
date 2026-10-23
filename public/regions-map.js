/* CoreScope Regions: an observed-relay footprint, not an RF coverage map. */
'use strict';

(function (root) {
  const MAX_POINTS = 5000;
  const MAX_SCOPES = 128;
  const WINDOWS = { '1h': 'Last hour', '24h': 'Last 24 hours', '7d': 'Last 7 days' };
  let leafletMap = null;
  let themeObserver = null;
  let generation = 0;

  function validPosition(node) {
    if (!node || node.lat == null || node.lon == null || node.lat === '' || node.lon === '') return false;
    const lat = Number(node.lat), lon = Number(node.lon);
    return Number.isFinite(lat) && Number.isFinite(lon) &&
      lat >= -90 && lat <= 90 && lon >= -180 && lon <= 180 &&
      (lat !== 0 || lon !== 0);
  }

  // MeshCore RegionMap::is_name_char byte rules, matching the admin scope
  // approval validator. Keep the exact OTA spelling and case.
  function validScope(name) {
    if (typeof name !== 'string' || !name.startsWith('#')) return false;
    const bytes = new TextEncoder().encode(name);
    if (bytes.length < 2 || bytes.length > 30 || new TextDecoder().decode(bytes) !== name) return false;
    for (let i = 1; i < bytes.length; i++) {
      const c = bytes[i];
      if (!(c === 45 || c === 36 || c === 35 || (c >= 48 && c <= 57) || (c >= 65 && c !== 127))) return false;
    }
    return true;
  }

  function collectScopeNodes(nodes) {
    if (!Array.isArray(nodes)) return [];
    const seen = new Set();
    const out = [];
    for (const node of nodes) {
      if (out.length >= MAX_POINTS) break;
      if (!node || (node.role !== 'repeater' && node.role !== 'room') ||
          !Array.isArray(node.scopes)) continue;
      const key = String(node.publicKey || '');
      if (!key || seen.has(key)) continue;
      const names = new Set();
      const scopes = node.scopes.filter(scope => scope && validScope(scope.name) && !names.has(scope.name) && names.add(scope.name))
        .map(scope => ({ name: scope.name, lastRelayed: Number.isFinite(Date.parse(scope.lastRelayed)) ? scope.lastRelayed : '' }));
      if (!scopes.length) continue;
      seen.add(key);
      const positioned = node.positionSource === 'reported' && validPosition(node);
      out.push({ publicKey: key, name: String(node.name || key.slice(0, 8)), role: node.role,
        lat: positioned ? Number(node.lat) : null, lon: positioned ? Number(node.lon) : null,
        positionSource: positioned ? 'reported' : 'none', scopes });
    }
    return out;
  }

  function collectScopePoints(nodes) {
    return collectScopeNodes(nodes).filter(node => node.positionSource === 'reported');
  }

  function selectionSummary(nodes, scope) {
    const selected = scope ? nodes.filter(node => node.scopes.some(s => s.name === scope)) : nodes;
    const points = selected.filter(node => node.positionSource === 'reported');
    return { selected, points, missingPositionCount: selected.length - points.length };
  }

  function lastRelayedForScope(node, scope) {
    let latest = '';
    for (const relay of node.scopes) {
      if ((!scope || relay.name === scope) && relay.lastRelayed && (!latest || Date.parse(relay.lastRelayed) > Date.parse(latest))) latest = relay.lastRelayed;
    }
    return latest;
  }

  function scopeCatalog(nodes) {
    const scopes = new Set();
    for (const point of collectScopeNodes(nodes)) {
      for (const scope of point.scopes) {
        scopes.add(scope.name);
        if (scopes.size > MAX_SCOPES) return { scopes: Array.from(scopes).slice(0, MAX_SCOPES).sort((a, b) => a.localeCompare(b)), truncated: true };
      }
    }
    return { scopes: Array.from(scopes).sort((a, b) => a.localeCompare(b)), truncated: false };
  }

  function listScopes(nodes) {
    return scopeCatalog(nodes).scopes;
  }

  function convexHull(points) {
    if (!Array.isArray(points)) return [];
    const sorted = points.filter(p => Array.isArray(p) && p.length === 2 && Number.isFinite(p[0]) && Number.isFinite(p[1]))
      .map(p => [p[0], p[1]]).sort((a, b) => a[0] - b[0] || a[1] - b[1]);
    const unique = sorted.filter((p, i) => i === 0 || p[0] !== sorted[i - 1][0] || p[1] !== sorted[i - 1][1]);
    if (unique.length < 3) return [];
    const cross = (a, b, c) => (b[0] - a[0]) * (c[1] - a[1]) - (b[1] - a[1]) * (c[0] - a[0]);
    const lower = [], upper = [];
    for (const p of unique) {
      while (lower.length >= 2 && cross(lower[lower.length - 2], lower[lower.length - 1], p) <= 0) lower.pop();
      lower.push(p);
    }
    for (let i = unique.length - 1; i >= 0; i--) {
      const p = unique[i];
      while (upper.length >= 2 && cross(upper[upper.length - 2], upper[upper.length - 1], p) <= 0) upper.pop();
      upper.push(p);
    }
    lower.pop(); upper.pop();
    const hull = lower.concat(upper);
    return hull.length >= 3 ? hull : [];
  }

  function parseSelection(hash) {
    const query = String(hash || '').split('?')[1] || '';
    const params = new URLSearchParams(query);
    const scope = params.get('scope') || '';
    const window = params.get('window');
    return { window: Object.prototype.hasOwnProperty.call(WINDOWS, window) ? window : '24h', scope: validScope(scope) ? scope : '' };
  }

  function parseSelectedScope(hash) {
    return parseSelection(hash).scope;
  }

  function scopeHash(scope, window = '24h') {
    const params = new URLSearchParams({ window: Object.prototype.hasOwnProperty.call(WINDOWS, window) ? window : '24h' });
    if (validScope(scope)) params.set('scope', scope);
    return '#/regions?' + params.toString();
  }

  function isoTime(value) {
    const ms = typeof value === 'string' ? Date.parse(value) : NaN;
    return Number.isFinite(ms) ? new Date(ms).toISOString() : '';
  }

  function coverageInfo(data) {
    const coverage = data.coverage || {};
    const requestedStart = isoTime(data.windowStart), requestedEnd = isoTime(data.windowEnd);
    const retainedStart = isoTime(coverage.start);
    const availableStart = retainedStart && requestedStart && retainedStart < requestedEnd ?
      (retainedStart > requestedStart ? retainedStart : requestedStart) : null;
    let kind = 'unknown', message = 'Retained history bounds are unknown; this view may be incomplete.';
    if (coverage.loading) {
      kind = 'loading';
      message = 'History is still loading; this view may be incomplete.';
    } else if (retainedStart) {
      const complete = coverage.complete === true && retainedStart <= requestedStart;
      kind = complete ? 'complete' : 'partial';
      message = complete ? 'Retained history covers the requested interval.' :
        (availableStart ? 'Partial retained history; earlier activity in the requested interval is unavailable.' : 'No retained history is available in the requested interval.');
    }
    return { kind, message, requestedStart, requestedEnd, availableStart, generatedAt: isoTime(data.generatedAt) };
  }

  function element(tag, className, text) {
    const el = document.createElement(tag);
    if (className) el.className = className;
    if (text != null) el.textContent = text;
    return el;
  }

  function timeElement(iso, exact) {
    const label = exact ? iso : new Intl.DateTimeFormat('en-GB', { day: 'numeric', month: 'short', year: 'numeric',
      hour: '2-digit', minute: '2-digit', hourCycle: 'h23', timeZone: 'UTC' }).format(new Date(iso)) + ' UTC';
    const time = element('time', '', label);
    time.dateTime = iso;
    time.title = iso;
    return time;
  }

  function renderPoints(layer, map, nodes, scope, status, truncated, scopesTruncated, fit) {
    layer.clearLayers();
    const { selected, points, missingPositionCount } = selectionSummary(nodes, scope);
    const accent = getComputedStyle(document.documentElement).getPropertyValue('--accent').trim() || 'steelblue';
    for (const point of points) {
      const marker = L.circleMarker([point.lat, point.lon], { radius: 6, weight: 2, color: accent, fillOpacity: 0.75 });
      const popup = element('div');
      popup.appendChild(element('strong', '', point.name));
      popup.appendChild(element('div', '', point.scopes.map(s => s.name).join(', ')));
      popup.appendChild(element('div', '', 'Position: self-reported; accuracy unverified.'));
      const lastRelayed = lastRelayedForScope(point, scope);
      if (lastRelayed) popup.appendChild(element('div', '', 'Last observed relay' + (scope ? ' for ' + scope : ' across these scopes') + ': ' + lastRelayed));
      const link = element('a', '', 'Node page');
      link.href = '#/nodes/' + encodeURIComponent(point.publicKey);
      popup.appendChild(link);
      marker.bindPopup(popup);
      layer.addLayer(marker);
    }
    if (scope && points.length >= 3) {
      const hull = convexHull(points.map(p => [p.lon, p.lat]));
      if (hull.length >= 3) layer.addLayer(L.polygon(hull.map(p => [p[1], p[0]]), {
        color: accent, fillOpacity: 0.10, weight: 2, dashArray: '6 5', interactive: false,
      }));
    }
    status.textContent = `${points.length} positioned relay nodes; ${missingPositionCount} observed relay nodes without usable positions${scope ? ' for ' + scope : ''} in this interval.`;
    if (!selected.length) status.textContent += ' No observed scoped relay traffic for this selection; this does not imply there is no coverage.';
    if (truncated) status.textContent += ' The 5,000-node safety limit was reached; counts and map may be incomplete.';
    if (scopesTruncated) status.textContent += ' The scope-name safety limit was reached; the selector may be incomplete.';
    if (fit && points.length) {
      const bounds = L.latLngBounds(points.map(p => [p.lat, p.lon]));
      map.fitBounds(bounds.pad(0.15), { maxZoom: 10 });
    }
  }

  async function init(container) {
    destroy();
    const token = ++generation;
    const shell = element('section', 'regions-page');
    shell.appendChild(element('h1', '', 'Regions'));
    shell.appendChild(element('p', 'regions-note', 'Observed scoped relay traffic, not a radio coverage measurement.'));
    const controls = element('div', 'regions-controls');
    const selection = parseSelection(location.hash);
    const periodLabel = element('label', '', 'Period ');
    const period = element('select');
    period.setAttribute('aria-label', 'Region period');
    for (const [value, labelText] of Object.entries(WINDOWS)) period.appendChild(new Option(labelText, value));
    period.value = selection.window;
    periodLabel.appendChild(period);
    controls.appendChild(periodLabel);
    const label = element('label', '', 'Scope ');
    const select = element('select');
    select.setAttribute('aria-label', 'Region scope');
    label.appendChild(select);
    controls.appendChild(label);
    const refresh = element('button', '', 'Refresh');
    refresh.type = 'button';
    controls.appendChild(refresh);
    shell.appendChild(controls);
    const status = element('p', 'regions-status', 'Loading observed relay data…');
    status.setAttribute('role', 'status');
    shell.appendChild(status);
    const snapshotMeta = element('p', 'regions-snapshot');
    shell.appendChild(snapshotMeta);
    const coverage = element('p', 'regions-coverage');
    shell.appendChild(coverage);
    const details = element('details', 'regions-details');
    details.appendChild(element('summary', '', 'Data & position details'));
    details.appendChild(element('p', '', 'Repeaters and rooms are shown where scoped relay traffic was observed. Positions are self-reported; their accuracy is unverified. Nodes without usable positions are counted but not mapped. Dashed outlines join the outermost node positions and do not measure RF coverage or administrative boundaries.'));
    details.appendChild(element('p', '', 'Retained history is not observer uptime or proof of continuous reception.'));
    const exactBounds = element('div', 'regions-exact-bounds');
    details.appendChild(exactBounds);
    shell.appendChild(details);
    const mapEl = element('div', 'regions-leaflet');
    mapEl.id = 'regionsLeaflet';
    mapEl.setAttribute('aria-label', 'Observed region relay map');
    shell.appendChild(mapEl);
    container.replaceChildren(shell);
    if (typeof L === 'undefined') {
      status.textContent = 'Map library unavailable.';
      return;
    }
    // Leaflet 1.9.4 leaves its 250ms zoom-transition timeout alive after
    // remove() deletes the map pane. Synchronous zoom also makes immediate
    // period/hash navigation safe when a user just pressed a zoom control.
    leafletMap = L.map(mapEl, { zoomAnimation: false }).setView([55.7, 10.5], 6);
    const map = leafletMap;
    const applyTiles = function () {
      if (token !== generation || leafletMap !== map) return;
      if (tileLayer) map.removeLayer(tileLayer);
      const theme = document.documentElement.getAttribute('data-theme') === 'light' ? 'light' : 'dark';
      const tile = typeof root.MC_getTileSpec === 'function' ? root.MC_getTileSpec(theme) : null;
      tileLayer = L.tileLayer(tile && tile.url ? tile.url : 'https://{s}.tile.openstreetmap.org/{z}/{x}/{y}.png', {
        maxZoom: tile && tile.maxZoom ? tile.maxZoom : 19,
        attribution: tile && tile.attribution ? tile.attribution : '© OpenStreetMap contributors',
      }).addTo(map);
      map.getPane('tilePane').style.filter = tile && tile.invertFilter ? tile.invertFilter : '';
    };
    let tileLayer = null;
    applyTiles();
    themeObserver = new MutationObserver(applyTiles);
    themeObserver.observe(document.documentElement, { attributes: true, attributeFilter: ['data-theme'] });
    const layer = L.layerGroup().addTo(map);
    let requestGeneration = 0, snapshot = null, nodes = [], catalog = { scopes: [], truncated: false };

    function populateScopes(loading) {
      select.replaceChildren();
      select.appendChild(new Option('All observed scopes', ''));
      for (const scope of catalog.scopes) select.appendChild(new Option(scope, scope));
      if (selection.scope && !catalog.scopes.includes(selection.scope)) {
        const hasSelectedData = nodes.some(node => node.scopes.some(s => s.name === selection.scope));
        select.appendChild(new Option((loading ? 'Selected scope: ' : hasSelectedData ? 'Selected (outside list): ' : 'No data in interval: ') + selection.scope, selection.scope));
      }
      select.value = selection.scope;
      select.disabled = loading;
    }

    function draw(fit) {
      if (!snapshot) return;
      renderPoints(layer, map, nodes, selection.scope, status, snapshot.truncated || snapshot.nodes.length > MAX_POINTS,
        snapshot.scopesTruncated || catalog.truncated, fit);
    }

    async function load() {
      const request = ++requestGeneration, window = selection.window;
      snapshot = null;
      nodes = [];
      catalog = { scopes: [], truncated: false };
      layer.clearLayers();
      populateScopes(true);
      shell.setAttribute('aria-busy', 'true');
      status.textContent = 'Loading observed relay data — ' + WINDOWS[window].toLowerCase() + '…';
      snapshotMeta.textContent = '';
      coverage.textContent = '';
      exactBounds.replaceChildren();
      coverage.removeAttribute('data-coverage-state');
      try {
        const data = await api('/regions/observed?window=' + encodeURIComponent(window), { ttl: 0, bust: true, retry503: false });
        if (token !== generation || request !== requestGeneration || leafletMap !== map) return;
        if (!data || data.window !== window || !Array.isArray(data.nodes) ||
            !isoTime(data.windowStart) || !isoTime(data.windowEnd) || !isoTime(data.generatedAt)) throw new Error('Invalid regions snapshot');
        snapshot = data;
        nodes = collectScopeNodes(data.nodes);
        catalog = scopeCatalog(nodes);
        populateScopes(false);
        const info = coverageInfo(data);
        snapshotMeta.replaceChildren('Snapshot: ', timeElement(info.generatedAt));
        coverage.replaceChildren(info.message);
        if (info.availableStart) coverage.append(' Available from ', timeElement(info.availableStart), ' to the snapshot.');
        const requested = element('p');
        requested.append('Requested interval (UTC): ', timeElement(info.requestedStart, true), ' — ', timeElement(info.requestedEnd, true));
        exactBounds.appendChild(requested);
        if (info.availableStart) {
          const available = element('p');
          available.append('Retained portion of this interval (UTC): ', timeElement(info.availableStart, true), ' — ', timeElement(info.requestedEnd, true));
          exactBounds.appendChild(available);
        }
        coverage.setAttribute('data-coverage-state', info.kind);
        draw(true);
      } catch (err) {
        if (token !== generation || request !== requestGeneration) return;
        status.textContent = 'Could not load observed relay data for ' + WINDOWS[window].toLowerCase() + '. Use Refresh to try again.';
      } finally {
        if (token === generation && request === requestGeneration) shell.setAttribute('aria-busy', 'false');
      }
    }

    function saveSelection() {
      const hash = scopeHash(selection.scope, selection.window);
      if (location.hash !== hash) history.pushState(null, '', hash);
    }
    period.addEventListener('change', () => {
      selection.window = period.value;
      saveSelection();
      load();
    });
    select.addEventListener('change', () => {
      selection.scope = select.value;
      saveSelection();
      draw(true);
    });
    refresh.addEventListener('click', load);
    load();
    setTimeout(() => { if (token === generation && leafletMap === map) map.invalidateSize(); }, 0);
  }

  function destroy() {
    generation++;
    if (themeObserver) themeObserver.disconnect();
    themeObserver = null;
    if (leafletMap) leafletMap.remove();
    leafletMap = null;
  }

  const internals = { validPosition, validScope, collectScopeNodes, collectScopePoints, selectionSummary,
    lastRelayedForScope, listScopes, scopeCatalog, convexHull, parseSelection, parseSelectedScope, scopeHash, coverageInfo };
  if (typeof module !== 'undefined' && module.exports) module.exports = internals;
  if (typeof root.registerPage === 'function') root.registerPage('regions', { init, destroy });
})(typeof window === 'undefined' ? globalThis : window);
