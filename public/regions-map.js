/* CoreScope Regions: an observed-relay footprint, not an RF coverage map. */
'use strict';

(function (root) {
  const MAX_POINTS = 5000;
  const MAX_SCOPES = 128;
  let leafletMap = null;
  let generation = 0;

  function validPosition(node) {
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

  function collectScopePoints(nodes) {
    if (!Array.isArray(nodes)) return [];
    const seen = new Set();
    const out = [];
    for (const node of nodes) {
      if (out.length >= MAX_POINTS) break;
      if (!node || (node.role !== 'repeater' && node.role !== 'room') ||
          !validPosition(node) || !Array.isArray(node.transported_scopes_recent)) continue;
      const key = String(node.public_key || '');
      if (!key || seen.has(key)) continue;
      const scopes = node.transported_scopes_recent.filter(validScope);
      if (!scopes.length) continue;
      seen.add(key);
      out.push({ publicKey: key, name: String(node.name || key.slice(0, 8)),
        lat: Number(node.lat), lon: Number(node.lon), scopes,
        lastRelayed: typeof node.last_relayed === 'string' ? node.last_relayed : '' });
    }
    return out;
  }

  function scopeCatalog(nodes) {
    const scopes = new Set();
    for (const point of collectScopePoints(nodes)) {
      for (const scope of point.scopes) {
        scopes.add(scope);
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

  function parseSelectedScope(hash) {
    const query = String(hash || '').split('?')[1] || '';
    const scope = new URLSearchParams(query).get('scope') || '';
    return validScope(scope) ? scope : '';
  }

  function scopeHash(scope) {
    return scope ? '#/regions?scope=' + encodeURIComponent(scope) : '#/regions';
  }

  function element(tag, className, text) {
    const el = document.createElement(tag);
    if (className) el.className = className;
    if (text != null) el.textContent = text;
    return el;
  }

  function renderPoints(layer, map, points, scope, status, truncated, scopesTruncated) {
    layer.clearLayers();
    const selected = scope ? points.filter(p => p.scopes.includes(scope)) : points;
    const accent = getComputedStyle(document.documentElement).getPropertyValue('--accent').trim() || 'steelblue';
    for (const point of selected) {
      const marker = L.circleMarker([point.lat, point.lon], { radius: 6, weight: 2, color: accent, fillOpacity: 0.75 });
      const popup = element('div');
      popup.appendChild(element('strong', '', point.name));
      popup.appendChild(element('div', '', point.scopes.join(', ')));
      if (point.lastRelayed) popup.appendChild(element('div', '', 'Last observed relay: ' + point.lastRelayed));
      const link = element('a', '', 'Node page');
      link.href = '#/nodes/' + encodeURIComponent(point.publicKey);
      popup.appendChild(link);
      marker.bindPopup(popup);
      layer.addLayer(marker);
    }
    if (scope && selected.length >= 3) {
      const hull = convexHull(selected.map(p => [p.lon, p.lat]));
      if (hull.length >= 3) layer.addLayer(L.polygon(hull.map(p => [p[1], p[0]]), {
        color: accent, fillOpacity: 0.10, weight: 2, dashArray: '6 5', interactive: false,
      }));
    }
    status.textContent = selected.length ? `${selected.length} positioned repeaters with observed scoped relay traffic in the configured relay-activity window${scope ? ' for ' + scope : ''}. Dashed outline is a convex hull of node positions, not measured RF coverage.`
      : 'No positioned repeaters with recent observed relay traffic for this selection. This does not imply there is no coverage.';
    if (truncated) status.textContent += ' Node fetch hit its 5,000-row safety limit; this view may be incomplete.';
    if (scopesTruncated) status.textContent += ' Scope selector shows only the first 128 observed names; other scopes are omitted from the selector.';
    if (selected.length) {
      const bounds = L.latLngBounds(selected.map(p => [p.lat, p.lon]));
      map.fitBounds(bounds.pad(0.15), { maxZoom: 10 });
    }
  }

  async function init(container) {
    const token = ++generation;
    const shell = element('section', 'regions-page');
    shell.appendChild(element('h1', '', 'Regions'));
    shell.appendChild(element('p', 'regions-note', 'Inferred from positioned repeaters that recently relayed scoped packets. This is observed routing, not a radio coverage measurement or administrative region boundary.'));
    const controls = element('div', 'regions-controls');
    const label = element('label', '', 'Scope ');
    const select = element('select');
    select.setAttribute('aria-label', 'Region scope');
    label.appendChild(select);
    controls.appendChild(label);
    shell.appendChild(controls);
    const status = element('p', 'regions-status', 'Loading observed relay data…');
    status.setAttribute('role', 'status');
    shell.appendChild(status);
    const mapEl = element('div', 'regions-leaflet');
    mapEl.id = 'regionsLeaflet';
    mapEl.setAttribute('aria-label', 'Observed region relay map');
    shell.appendChild(mapEl);
    container.replaceChildren(shell);
    if (typeof L === 'undefined') {
      status.textContent = 'Map library unavailable.';
      return;
    }
    leafletMap = L.map(mapEl).setView([55.7, 10.5], 6);
    const theme = document.documentElement.getAttribute('data-theme') === 'light' ? 'light' : 'dark';
    const tile = typeof root.MC_getTileSpec === 'function' ? root.MC_getTileSpec(theme) : null;
    L.tileLayer(tile && tile.url ? tile.url : 'https://{s}.tile.openstreetmap.org/{z}/{x}/{y}.png', {
      maxZoom: tile && tile.maxZoom ? tile.maxZoom : 19,
      attribution: tile && tile.attribution ? tile.attribution : '© OpenStreetMap contributors',
    }).addTo(leafletMap);
    if (tile && tile.invertFilter) leafletMap.getPane('tilePane').style.filter = tile.invertFilter;
    const layer = L.layerGroup().addTo(leafletMap);
    try {
      const data = await fetchAllNodes('', { ttl: 60000, pageSize: 500, safetyCap: 5000 });
      if (token !== generation || !leafletMap) return;
      const points = collectScopePoints(data.nodes);
      const catalog = scopeCatalog(data.nodes);
      const scopes = catalog.scopes;
      const truncated = data.total >= 5000;
      select.appendChild(new Option('All observed scopes', ''));
      for (const scope of scopes) select.appendChild(new Option(scope, scope));
      let selected = parseSelectedScope(location.hash);
      if (selected && !scopes.includes(selected)) {
        const hasSelectedData = points.some(p => p.scopes.includes(selected));
        select.appendChild(new Option((hasSelectedData ? 'Selected (outside list): ' : 'No recent data: ') + selected, selected));
      }
      select.value = selected;
      renderPoints(layer, leafletMap, points, selected, status, truncated, catalog.truncated);
      select.addEventListener('change', () => {
        selected = select.value;
        history.replaceState(null, '', scopeHash(selected));
        renderPoints(layer, leafletMap, points, selected, status, truncated, catalog.truncated);
      });
      setTimeout(() => { if (token === generation && leafletMap) leafletMap.invalidateSize(); }, 0);
    } catch (err) {
      if (token === generation) status.textContent = 'Could not load observed relay data. Please try again.';
    }
  }

  function destroy() {
    generation++;
    if (leafletMap) leafletMap.remove();
    leafletMap = null;
  }

  const internals = { validScope, collectScopePoints, listScopes, scopeCatalog, convexHull, parseSelectedScope, scopeHash };
  if (typeof module !== 'undefined' && module.exports) module.exports = internals;
  if (typeof root.registerPage === 'function') root.registerPage('regions', { init, destroy });
})(typeof window === 'undefined' ? globalThis : window);
