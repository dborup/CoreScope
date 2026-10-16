/* CoreScope Regions: an observed-relay footprint, not an RF coverage map. */
'use strict';

(function (root) {
  const MAX_POINTS = 5000;
  const MAX_SCOPES = 128;
  const WINDOWS = { '1h': 'Last hour', '24h': 'Last 24 hours', '7d': 'Last 7 days' };
  let leafletMap = null;
  let themeObserver = null;
  let generation = 0;
  let stopPlayback = null;
  let visibilityListener = null;

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

  function collectScopeNodes(nodes, historical = false) {
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
      const source = historical ? node.scopes.slice(0, 32) : node.scopes;
      const scopes = source.filter(scope => scope && validScope(scope.name) && !names.has(scope.name) && names.add(scope.name))
        .map(scope => ({ name: scope.name, lastRelayed: Number.isFinite(Date.parse(scope.lastRelayed)) ? scope.lastRelayed : '',
          ...(historical ? { buckets: Array.isArray(scope.buckets) ? scope.buckets : [] } : {}) }));
      if (!scopes.length) continue;
      seen.add(key);
      const positioned = node.positionSource === 'reported' && validPosition(node);
      out.push({ publicKey: key, name: String(node.name || key.slice(0, 8)), role: node.role,
        lat: positioned ? Number(node.lat) : null, lon: positioned ? Number(node.lon) : null,
        positionSource: positioned ? 'reported' : 'none', metadataSource: node.metadataSource === 'archived' ? 'archived' : 'current', scopes });
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

  function mergeScopeCatalog(catalog, additionalScopes) {
    const names = Array.from(new Set(catalog.scopes.concat(additionalScopes.filter(validScope)))).sort((a, b) => a.localeCompare(b));
    return { scopes: names.slice(0, MAX_SCOPES), truncated: catalog.truncated || names.length > MAX_SCOPES };
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
    const selection = { window: Object.prototype.hasOwnProperty.call(WINDOWS, window) ? window : '24h', scope: validScope(scope) ? scope : '' };
    if (['timeline', 'health', 'connections'].includes(params.get('view'))) {
      selection.view = params.get('view');
      selection.at = isoTime(params.get('at'));
      selection.compare = selection.view !== 'connections' && params.get('compare') === '1';
      if (selection.view === 'connections') {
        for (const key of ['page', 'pairsPage']) {
          const value = Number(params.get(key));
          if (Number.isInteger(value) && value > 1 && value <= 200) selection[key] = value;
        }
      }
    }
    return selection;
  }

  function parseSelectedScope(hash) {
    return parseSelection(hash).scope;
  }

  function scopeHash(scope, window = '24h', selection = {}) {
    const params = new URLSearchParams({ window: Object.prototype.hasOwnProperty.call(WINDOWS, window) ? window : '24h' });
    if (validScope(scope)) params.set('scope', scope);
    if (['timeline', 'health', 'connections'].includes(selection.view)) {
      params.set('view', selection.view);
      const at = isoTime(selection.at);
      if (at) params.set('at', at);
      if (selection.view !== 'connections' && selection.compare) params.set('compare', '1');
      if (selection.view === 'connections') for (const key of ['page', 'pairsPage']) {
        if (Number.isInteger(selection[key]) && selection[key] > 1 && selection[key] <= 200) params.set(key, selection[key]);
      }
    }
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

  function createActivityPanel() {
    const panel = element('section', 'regions-health');
    panel.hidden = true;
    panel.setAttribute('aria-label', 'Region health and traffic trends');
    const cards = element('div', 'regions-health-cards');
    const metrics = {};
    for (const [key, label] of [['nodes', 'Observed relay nodes'], ['appearances', 'Observed relay appearances'], ['last-relayed', 'Last relay in selected interval']]) {
      const card = element('div', 'regions-health-card');
      card.dataset.metric = key;
      card.appendChild(element('span', 'regions-health-label', label));
      const value = element('strong', 'regions-health-value', '—');
      const detail = element('span', 'regions-health-detail');
      card.append(value, detail); cards.appendChild(card);
      metrics[key] = { value, detail };
    }
    metrics.appearances.detail.classList.add('regions-health-rate');
    panel.appendChild(cards);
    panel.appendChild(element('h2', '', 'Hourly traffic'));
    const chartStatus = element('p', 'regions-trend-status');
    panel.appendChild(chartStatus);
    function svgElement(tag, className) {
      const node = document.createElementNS('http://www.w3.org/2000/svg', tag);
      if (className) node.setAttribute('class', className);
      return node;
    }
    function attr(node, name, value) {
      const text = String(value);
      if (node.getAttribute(name) !== text) node.setAttribute(name, text);
    }
    const chart = svgElement('svg', 'regions-trend');
    chart.setAttribute('viewBox', '0 0 656 176');
    chart.setAttribute('preserveAspectRatio', 'none');
    chart.setAttribute('role', 'img');
    const baseline = svgElement('line', 'regions-trend-axis');
    baseline.setAttribute('x1', '0'); baseline.setAttribute('x2', '656');
    baseline.setAttribute('y1', '160'); baseline.setAttribute('y2', '160');
    const bars = svgElement('g');
    const scale = element('div', 'regions-trend-scale');
    const maximum = element('span'), zero = element('span', '', '0');
    scale.append(maximum, zero);
    chart.append(baseline, bars);
    const plot = element('div', 'regions-trend-plot');
    plot.append(scale, chart); panel.appendChild(plot);
    const bounds = element('div', 'regions-trend-bounds');
    const startLabel = element('span'), endLabel = element('span');
    bounds.append(startLabel, endLabel); panel.appendChild(bounds);
    panel.appendChild(element('p', 'regions-trend-legend', 'Solid bars: complete hours. Dashed gaps: incomplete or unavailable values. A complete zero hour has no observed relay traffic.'));
    const details = element('details', 'regions-hourly-details');
    details.appendChild(element('summary', '', 'Hourly values'));
    const tableScroll = element('div', 'regions-hourly-scroll');
    tableScroll.setAttribute('tabindex', '0');
    tableScroll.setAttribute('role', 'region');
    tableScroll.setAttribute('aria-label', 'Scrollable hourly activity values');
    const table = element('table', 'regions-hourly-table');
    table.appendChild(element('caption', '', 'Completed UTC hours; retained observations are shown separately when complete values are unavailable.'));
    const head = element('thead'), header = element('tr');
    for (const name of ['UTC hour', 'Relay appearances', 'Relay nodes', 'History', 'Retained observations']) {
      const cell = element('th', '', name); cell.scope = 'col'; header.appendChild(cell);
    }
    head.appendChild(header);
    const body = element('tbody');
    table.append(head, body); tableScroll.appendChild(table); details.appendChild(tableScroll); panel.appendChild(details);
    panel.appendChild(element('p', 'regions-health-note', 'Traffic increase does not establish better health. Spam, observer reception and routing changes can affect observed counts. Relay appearances can count the same transmission via several relays; this is not a global packet total.'));
    const rows = new Map();

    function update(frame, activity, scope) {
      panel.hidden = false;
      metrics.nodes.value.textContent = frame.currentCount.toLocaleString();
      const missing = frame.rows.filter(row => row.count && row.node.positionSource !== 'reported').length;
      metrics.nodes.detail.textContent = missing + ' without usable positions';
      metrics.appearances.value.textContent = frame.currentAppearances.toLocaleString();
      metrics.appearances.detail.textContent = activity.ratePerHour == null ? 'Rate unavailable: incomplete or limited history.' :
        activity.ratePerHour.toFixed(2) + ' relay appearances per hour';
      metrics['last-relayed'].value.replaceChildren();
      if (activity.lastRelayed) metrics['last-relayed'].value.appendChild(timeElement(activity.lastRelayed));
      else metrics['last-relayed'].value.textContent = 'Not observed';
      metrics['last-relayed'].detail.textContent = activity.lastRelayed ? 'Within the selected interval' : 'No relay in retained observations for this interval';
      const bins = activity.bins;
      const available = bins.filter(bin => bin.appearances != null);
      chartStatus.textContent = available.length + ' of ' + bins.length + ' complete hourly values' + (scope ? ' for ' + scope : ' across observed scopes') +
        '. Missing values are gaps, not zero traffic.';
      chart.setAttribute('aria-label', 'Hourly relay appearances' + (scope ? ' for ' + scope : ' across observed scopes') +
        ': ' + available.length + ' of ' + bins.length + ' complete hours, ' + frame.interval.start + ' to ' + frame.interval.end + '. Exact values follow.');
      const upper = Math.max(1, ...available.map(bin => bin.appearances));
      maximum.textContent = new Intl.NumberFormat('en', { notation: 'compact', maximumFractionDigits: 1 }).format(upper);
      startLabel.textContent = frame.interval.start.slice(5, 16).replace('T', ' ') + ' UTC';
      endLabel.textContent = frame.interval.end.slice(5, 16).replace('T', ' ') + ' UTC';
      const wanted = new Set(bins.map(bin => bin.start));
      for (const [key, record] of rows) if (!wanted.has(key)) {
        record.row.remove(); record.rect.remove(); rows.delete(key);
      }
      let cursor = body.firstChild;
      for (let index = 0; index < bins.length; index++) {
        const bin = bins[index];
        let record = rows.get(bin.start);
        if (!record) {
          const row = element('tr'); row.dataset.start = bin.start;
          const cells = {};
          for (const key of ['hour', 'appearances', 'nodes', 'coverage', 'retained']) {
            const cell = element('td'); cell.dataset.column = key; cells[key] = cell; row.appendChild(cell);
          }
          const rect = svgElement('rect');
          const title = svgElement('title'); rect.appendChild(title); bars.appendChild(rect);
          record = { row, cells, rect, title }; rows.set(bin.start, record);
        }
        const complete = bin.appearances != null && bin.nodes != null;
        const state = frame.limited ? 'limited' : bin.coverage;
        record.cells.hour.textContent = bin.start.slice(0, 16).replace('T', ' ') + ' — ' + bin.end.slice(11, 16);
        record.cells.appearances.textContent = complete ? bin.appearances.toLocaleString() : '—';
        record.cells.nodes.textContent = complete ? bin.nodes.toLocaleString() : '—';
        record.cells.coverage.textContent = state;
        record.cells.retained.textContent = bin.observedAppearances.toLocaleString() + ' appearances; ' + bin.observedNodes + ' relay nodes';
        if (record.row !== cursor) body.insertBefore(record.row, cursor);
        cursor = record.row.nextSibling;
        attr(record.rect, 'class', complete ? 'regions-trend-bar' : 'regions-trend-gap');
        attr(record.rect, 'data-hour-start', bin.start);
        if (complete) attr(record.rect, 'data-count', bin.appearances); else record.rect.removeAttribute('data-count');
        const width = 656 / Math.max(1, bins.length);
        const height = complete ? (bin.appearances === 0 ? 0 : Math.max(2, bin.appearances / upper * 156)) : 156;
        attr(record.rect, 'x', index * width + 1);
        attr(record.rect, 'width', Math.max(1, width - 2));
        attr(record.rect, 'y', complete ? 160 - height : 4);
        attr(record.rect, 'height', height);
        record.title.textContent = record.cells.hour.textContent + ' UTC: ' + (complete ? bin.appearances + ' relay appearances, ' + bin.nodes + ' relay nodes.' :
          'Complete value unavailable (' + state + '); retained observations: ' + record.cells.retained.textContent + '.');
      }
      panel.setAttribute('data-ready', 'true');
    }

    function clear() { panel.hidden = true; panel.removeAttribute('data-ready'); }
    return { panel, update, clear };
  }

  async function init(container) {
    destroy();
    const token = ++generation;
    const shell = element('section', 'regions-page');
    shell.appendChild(element('h1', '', 'Regions'));
    shell.appendChild(element('p', 'regions-note', 'Observed scoped relay traffic, not a radio coverage measurement.'));
    const controls = element('div', 'regions-controls');
    const selection = parseSelection(location.hash);
    const viewLabel = element('label', '', 'View ');
    const view = element('select');
    view.setAttribute('aria-label', 'Region view');
    view.append(new Option('Latest', 'latest'), new Option('Timeline', 'timeline'), new Option('Health & trends', 'health'), new Option('Connections', 'connections'));
    view.value = selection.view || 'latest';
    viewLabel.appendChild(view);
    controls.appendChild(viewLabel);
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
    const fitButton = element('button', '', 'Fit map');
    fitButton.type = 'button';
    controls.appendChild(fitButton);
    shell.appendChild(controls);
    const timelineControls = element('div', 'regions-timeline');
    timelineControls.hidden = !['timeline', 'health', 'connections'].includes(selection.view);
    const transport = element('div', 'regions-transport');
    function transportButton(text, label) {
      const button = element('button', '', text);
      button.type = 'button';
      button.setAttribute('aria-label', label);
      transport.appendChild(button);
      return button;
    }
    const previous = transportButton('Previous', 'Previous timeline hour');
    const play = transportButton('Play', 'Play timeline');
    const next = transportButton('Next', 'Next timeline hour');
    play.setAttribute('aria-pressed', 'false');
    timelineControls.appendChild(transport);
    const sliderLabel = element('label', 'regions-slider-label', 'Interval ending (completed UTC hours)');
    const slider = element('input');
    slider.type = 'range';
    slider.min = '0'; slider.max = '168'; slider.step = '1'; slider.value = '168';
    slider.setAttribute('aria-label', 'Timeline interval ending');
    sliderLabel.appendChild(slider);
    timelineControls.appendChild(sliderLabel);
    const compareLabel = element('label', 'regions-compare-label');
    const compare = element('input');
    compare.type = 'checkbox'; compare.checked = selection.compare === true;
    compare.setAttribute('aria-label', 'Compare previous period');
    compareLabel.append(compare, 'Compare previous period');
    timelineControls.appendChild(compareLabel);
    const intervalLabel = element('p', 'regions-interval');
    timelineControls.appendChild(intervalLabel);
    shell.appendChild(timelineControls);
    const comparison = element('section', 'regions-comparison');
    comparison.setAttribute('aria-label', 'Timeline comparison');
    comparison.hidden = true;
    shell.appendChild(comparison);
    const status = element('p', 'regions-status', 'Loading observed relay data…');
    status.setAttribute('role', 'status');
    shell.appendChild(status);
    const snapshotMeta = element('p', 'regions-snapshot');
    shell.appendChild(snapshotMeta);
    const coverage = element('p', 'regions-coverage');
    shell.appendChild(coverage);
    const activityPanel = createActivityPanel();
    shell.appendChild(activityPanel.panel);
    const routesTools = root.RegionsRoutes;
    const routesPanel = routesTools ? routesTools.createPanel((key, value) => {
      selection[key] = value; saveSelection(); draw(false);
    }) : null;
    if (routesPanel) shell.appendChild(routesPanel.panel);
    const details = element('details', 'regions-details');
    details.appendChild(element('summary', '', 'Data & position details'));
    details.appendChild(element('p', '', 'Repeaters and rooms are shown where scoped relay traffic was observed. Positions are self-reported; their accuracy is unverified. Nodes without usable positions are counted but not mapped. Dashed outlines join the outermost node positions and do not measure RF coverage or administrative boundaries.'));
    details.appendChild(element('p', '', 'Retained history is not observer uptime or proof of continuous reception.'));
    details.appendChild(element('p', '', 'Timeline counts are relay appearances: one transmission observed via two relay nodes contributes one appearance for each relay. Hours use UTC and intervals include their start and exclude their end. Positions use current or last-known self-reported coordinates; playback shows activity, not historical movement. Current catalog metadata can revise historical attribution after a refresh.'));
    const exactBounds = element('div', 'regions-exact-bounds');
    details.appendChild(exactBounds);
    shell.appendChild(details);
    const mapEl = element('div', 'regions-leaflet');
    mapEl.id = 'regionsLeaflet';
    mapEl.setAttribute('aria-label', 'Observed region relay map');
    shell.appendChild(mapEl);
    container.replaceChildren(shell);
    // Leaflet 1.9.4 leaves its 250ms zoom-transition timeout alive after
    // remove() deletes the map pane. Synchronous zoom also makes immediate
    // period/hash navigation safe when a user just pressed a zoom control.
    let map = null, layer = null;
    let redrawMap = null;
    const applyTiles = function () {
      if (!map || selection.view === 'health' || token !== generation || leafletMap !== map) return;
      if (tileLayer) map.removeLayer(tileLayer);
      const theme = document.documentElement.getAttribute('data-theme') === 'light' ? 'light' : 'dark';
      const tile = typeof root.MC_getTileSpec === 'function' ? root.MC_getTileSpec(theme) : null;
      tileLayer = L.tileLayer(tile && tile.url ? tile.url : 'https://{s}.tile.openstreetmap.org/{z}/{x}/{y}.png', {
        maxZoom: tile && tile.maxZoom ? tile.maxZoom : 19,
        attribution: tile && tile.attribution ? tile.attribution : '© OpenStreetMap contributors',
      }).addTo(map);
      map.getPane('tilePane').style.filter = tile && tile.invertFilter ? tile.invertFilter : '';
      if (redrawMap) redrawMap();
    };
    let tileLayer = null;
    function ensureMap(reportError = true) {
      if (map) return true;
      if (typeof L === 'undefined') { if (reportError) status.textContent = 'Map library unavailable.'; return false; }
      map = L.map(mapEl, { zoomAnimation: false }).setView([55.7, 10.5], 6);
      leafletMap = map;
      layer = L.layerGroup().addTo(map);
      applyTiles();
      return true;
    }
    themeObserver = new MutationObserver(applyTiles);
    themeObserver.observe(document.documentElement, { attributes: true, attributeFilter: ['data-theme'] });
    let requestGeneration = 0, snapshot = null, nodes = [], catalog = { scopes: [], truncated: false };
    let indexedHistory = null, timer = null, lastFrame = null, scrubFrame = null;
    let activity = null, activityScope = null;
    let routesIndex = null;
    const markers = new Map();
    let hullLayer = null;
    const historyTools = root.RegionsHistory;

    function historicalView() { return ['timeline', 'health', 'connections'].includes(selection.view); }

    function updateView() {
      const health = selection.view === 'health';
      const connections = selection.view === 'connections';
      timelineControls.hidden = !historicalView();
      play.hidden = health || connections;
      compareLabel.hidden = connections;
      fitButton.hidden = health;
      mapEl.hidden = health;
      shell.classList.toggle('regions-page-health', health);
      shell.classList.toggle('regions-page-connections', connections);
      if (!health) activityPanel.clear();
      if (!connections && routesPanel) routesPanel.clear();
      if (health) shell.insertBefore(comparison, details); else shell.insertBefore(comparison, status);
    }

    function pause() {
      if (timer !== null) clearTimeout(timer);
      if (scrubFrame !== null) cancelAnimationFrame(scrubFrame);
      timer = null;
      scrubFrame = null;
      play.textContent = 'Play';
      play.setAttribute('aria-label', 'Play timeline');
      play.setAttribute('aria-pressed', 'false');
    }
    stopPlayback = pause;
    visibilityListener = () => { if (document.hidden) pause(); };
    document.addEventListener('visibilitychange', visibilityListener);

    function clearPoints() {
      if (layer) layer.clearLayers();
      markers.clear(); hullLayer = null; lastFrame = null;
    }

    function timelineEnabled(ready) {
      const enabled = ready && historicalView();
      for (const control of [slider, compare, previous, next, play]) control.disabled = !enabled;
      if (enabled) {
        previous.disabled = Number(slider.value) <= 0;
        next.disabled = Number(slider.value) >= 168;
        play.disabled = Number(slider.value) >= 168;
      }
    }

    function intervalText(start, end) {
      return start.replace('T', ' ').replace(':00.000Z', '') + ' — ' + end.replace('T', ' ').replace(':00.000Z', '') + ' UTC';
    }

    function appendNodeList(parent, labelText, values) {
      const details = element('details', 'regions-change-list');
      details.appendChild(element('summary', '', labelText + ' (' + values.length + ')'));
      const list = element('ul');
      for (const node of values.slice(0, 20)) {
        const item = element('li');
        const link = element('a', '', node.name);
        link.href = '#/nodes/' + encodeURIComponent(node.publicKey);
        item.appendChild(link);
        if (node.positionSource !== 'reported') item.append(' — no usable position');
        list.appendChild(item);
      }
      details.appendChild(list);
      if (values.length > 20) details.appendChild(element('p', '', 'Showing the first 20 of ' + values.length + ' relay nodes.'));
      parent.appendChild(details);
    }

    function renderComparison(frame) {
      comparison.hidden = !selection.compare;
      comparison.replaceChildren();
      if (!selection.compare) return;
      comparison.appendChild(element('p', 'regions-reference', 'Previous interval: ' + intervalText(frame.interval.previousStart, frame.interval.previousEnd)));
      comparison.appendChild(element('p', '', 'Observed relay nodes: ' + frame.currentCount + ' selected / ' + frame.previousCount +
        ' previous. Relay appearances: ' + frame.currentAppearances.toLocaleString() + ' selected / ' + frame.previousAppearances.toLocaleString() + ' previous.'));
      if (!frame.comparable) {
        const reasons = frame.limited ? 'A data safety limit was reached.' : 'Selected history: ' + frame.currentCoverage + '; previous history: ' + frame.previousCoverage + '.';
        comparison.appendChild(element('p', 'regions-comparison-limited', 'Comparison limited by available history. ' + reasons +
          ' Changes and new/not-observed classifications are unavailable; counts show only retained observations.'));
        comparison.setAttribute('data-comparable', 'false');
        return;
      }
      comparison.setAttribute('data-comparable', 'true');
      const legend = element('p', 'regions-comparison-legend');
      legend.append(element('span', 'regions-state-added', frame.added.length + ' newly observed'),
        element('span', 'regions-state-retained', frame.retained.length + ' observed in both'),
        element('span', 'regions-state-not-observed', frame.notObserved.length + ' not observed in selected interval'));
      comparison.appendChild(legend);
      const change = frame.appearanceChange;
      comparison.appendChild(element('p', 'regions-appearance-change', 'Relay appearance change: ' + (change > 0 ? '+' : '') +
        change.toLocaleString() + (frame.percentChange == null ? ' (percentage unavailable: previous count is zero).' :
          ' (' + (frame.percentChange > 0 ? '+' : '') + frame.percentChange.toFixed(1) + '%).')));
      const lists = element('div', 'regions-change-lists');
      appendNodeList(lists, 'Newly observed in selected interval', frame.added);
      appendNodeList(lists, 'Not observed in selected interval', frame.notObserved);
      comparison.appendChild(lists);
    }

    function timelinePopup(row) {
      const point = row.node;
      const popup = element('div');
      popup.appendChild(element('strong', '', point.name));
      popup.appendChild(element('div', '', selection.scope || point.scopes.map(s => s.name).join(', ')));
      popup.appendChild(element('div', '', 'Selected interval: ' + row.count.toLocaleString() + ' relay appearances.'));
      if (selection.compare) popup.appendChild(element('div', '', 'Previous interval: ' + row.previousCount.toLocaleString() + ' relay appearances.'));
      if (row.state === 'added') popup.appendChild(element('div', '', 'Newly observed relative to the previous interval.'));
      if (row.state === 'retained') popup.appendChild(element('div', '', 'Observed in both intervals.'));
      if (row.state === 'not-observed') popup.appendChild(element('div', '', 'Not observed in selected interval; observed in previous interval. This does not establish an outage.'));
      if (row.lastRelayed) popup.appendChild(element('div', '', 'Last observed relay in selected interval: ' + row.lastRelayed));
      popup.appendChild(element('div', '', 'Position: ' + (point.metadataSource === 'archived' ? 'last-known' : 'current') + ' self-reported; accuracy unverified. Not a historical position.'));
      const link = element('a', '', 'Node page');
      link.href = '#/nodes/' + encodeURIComponent(point.publicKey);
      popup.appendChild(link);
      return popup;
    }

    function renderTimelinePoints(frame) {
      const styles = getComputedStyle(document.documentElement);
      const colors = { observed: styles.getPropertyValue('--accent').trim(), retained: styles.getPropertyValue('--accent').trim(),
        added: styles.getPropertyValue('--status-green').trim(), 'not-observed': styles.getPropertyValue('--status-yellow').trim() };
      const visible = new Set();
      for (const row of frame.rows) {
        const point = row.node;
        if (point.positionSource !== 'reported') continue;
        visible.add(point.publicKey);
        const style = { radius: row.state === 'added' ? 7 : 6, weight: 2, color: colors[row.state],
          fillOpacity: row.state === 'not-observed' ? 0.15 : 0.75, dashArray: row.state === 'not-observed' ? '3 3' : null };
        let record = markers.get(point.publicKey);
        if (!record) {
          record = { marker: L.circleMarker([point.lat, point.lon], style), signature: '', style: '' };
          const current = record;
          record.marker.bindPopup(() => timelinePopup(current.row));
          markers.set(point.publicKey, record);
        }
        const styleKey = [style.radius, style.color, style.fillOpacity, style.dashArray].join('|');
        if (record.style !== styleKey) { record.marker.setStyle(style); record.style = styleKey; }
        if (!layer.hasLayer(record.marker)) layer.addLayer(record.marker);
        record.row = row;
        const signature = [row.count, row.previousCount, row.state, row.lastRelayed, row.previousLastRelayed, selection.scope, selection.compare].join('|');
        if (record.signature !== signature) {
          if (record.marker.isPopupOpen()) record.marker.getPopup().setContent(timelinePopup(row));
          record.signature = signature;
        }
      }
      for (const [key, record] of markers) if (!visible.has(key) && layer.hasLayer(record.marker)) layer.removeLayer(record.marker);
      if (hullLayer) { layer.removeLayer(hullLayer); hullLayer = null; }
      const currentPoints = frame.rows.filter(row => row.count > 0 && row.node.positionSource === 'reported').map(row => row.node);
      if (selection.scope && currentPoints.length >= 3) {
        const hull = convexHull(currentPoints.map(point => [point.lon, point.lat]));
        if (hull.length >= 3) {
          hullLayer = L.polygon(hull.map(point => [point[1], point[0]]), { color: colors.observed,
            fillOpacity: 0.10, weight: 2, dashArray: '6 5', interactive: false });
          layer.addLayer(hullLayer);
        }
      }
      status.textContent = currentPoints.length + ' positioned relay nodes; ' + (frame.currentCount - currentPoints.length) +
        ' observed relay nodes without usable positions' + (selection.scope ? ' for ' + selection.scope : '') +
        ' in selected interval. ' + frame.currentAppearances.toLocaleString() + ' relay appearances.';
      if (!frame.currentCount) status.textContent += frame.currentCoverage === 'unavailable' ? ' History is unavailable for this interval.' :
        ' No scoped relay traffic was observed in the available history; this does not imply there is no coverage.';
      if (frame.limited) status.textContent += ' Data safety limits were reached; counts and map may be incomplete.';
    }

    function fitTimeline() {
      const points = lastFrame ? lastFrame.rows.filter(row => row.node.positionSource === 'reported').map(row => row.node) : [];
      if (points.length) map.fitBounds(L.latLngBounds(points.map(point => [point.lat, point.lon])).pad(0.15), { maxZoom: 10 });
    }

    function drawTimeline() {
      const frame = historyTools.selectFrame(indexedHistory, selection);
      lastFrame = frame;
      selection.at = frame.interval.end;
      slider.value = String(historyTools.frameIndex(indexedHistory, selection.at));
      slider.setAttribute('aria-valuetext', intervalText(frame.interval.start, frame.interval.end));
      intervalLabel.textContent = 'Selected interval: ' + intervalText(frame.interval.start, frame.interval.end);
      timelineEnabled(true);
      snapshotMeta.replaceChildren('Snapshot: ', timeElement(indexedHistory.generatedAt));
      const messages = { complete: 'Retained history covers the selected interval.', partial: 'Partial retained history for the selected interval.',
        unavailable: 'No retained history is available in the selected interval.', unknown: 'Retained history bounds are unknown; this view may be incomplete.',
        loading: 'History is still loading; this view may be incomplete.' };
      coverage.textContent = messages[frame.currentCoverage];
      coverage.setAttribute('data-coverage-state', frame.currentCoverage);
      exactBounds.replaceChildren(element('p', '', 'Selected interval (UTC): ' + frame.interval.start + ' — ' + frame.interval.end),
        element('p', '', 'History dataset (UTC): ' + new Date(indexedHistory.start).toISOString() + ' — ' + new Date(indexedHistory.end).toISOString()));
      if (selection.view === 'connections') {
        if (!routesIndex || !routesPanel) throw new Error('Connection evidence unavailable');
        const connections = routesTools.selectRoutes(routesIndex, indexedHistory, frame, selection);
        const pages = routesPanel.update(connections, frame, selection);
        selection.page = pages.page; selection.pairsPage = pages.pairsPage;
        saveSelection(true);
        comparison.hidden = true;
        status.textContent = connections.steps.length + ' directed step/scope rows and ' + connections.pairs.length +
          (connections.pairsTruncated ? '+ retained' : '') + ' shared-scope pairs in retained observations.' +
          (connections.complete ? '' : ' Evidence is incomplete or capped; absence cannot be established.');
        if (catalog.truncated) status.textContent += ' Scope selector is limited to 128 names; a selected scope outside the list is preserved.';
        if (ensureMap(false)) routesPanel.renderMap(map, layer, L);
        else status.textContent += ' Map library unavailable; the evidence tables remain available.';
      } else if (selection.view === 'health') {
        if (!activity || activityScope !== selection.scope) {
          activity = historyTools.indexActivity(indexedHistory, selection.scope);
          activityScope = selection.scope;
        }
        activityPanel.update(frame, historyTools.selectActivity(activity, indexedHistory, frame), selection.scope);
        status.textContent = frame.currentCount + ' observed relay nodes; ' + frame.currentAppearances.toLocaleString() +
          ' observed relay appearances in the selected interval. ' + (frame.currentCoverage === 'complete' && !frame.limited ?
            (frame.currentCount ? 'Observed activity; this does not measure RF coverage or observer uptime.' :
              'No scoped relay activity observed in this complete interval; this does not establish an outage.') :
            'Retained observations only; history is incomplete or limited.');
      } else renderTimelinePoints(frame);
      if (selection.view !== 'connections') renderComparison(frame);
    }

    function populateScopes(loading) {
      select.replaceChildren();
      select.appendChild(new Option('All observed scopes', ''));
      for (const scope of catalog.scopes) select.appendChild(new Option(scope, scope));
      if (selection.scope && !catalog.scopes.includes(selection.scope)) {
        const hasSelectedData = nodes.some(node => node.scopes.some(s => s.name === selection.scope)) ||
          (routesIndex && routesIndex.steps.some(step => step.scope === selection.scope));
        select.appendChild(new Option((loading ? 'Selected scope: ' : hasSelectedData ? 'Selected (outside list): ' : 'No data in interval: ') + selection.scope, selection.scope));
      }
      select.value = selection.scope;
      select.disabled = loading;
    }

    function draw(fit) {
      if (historicalView()) {
        if (!indexedHistory) return;
        if (selection.view === 'timeline' && !ensureMap()) return;
        drawTimeline();
        if (selection.view === 'timeline' && fit) fitTimeline();
        if (selection.view === 'connections' && fit && map) routesPanel.fitMap(map, L);
        return;
      }
      if (!snapshot) return;
      if (!ensureMap()) return;
      renderPoints(layer, map, nodes, selection.scope, status, snapshot.truncated || snapshot.nodes.length > MAX_POINTS,
        snapshot.scopesTruncated || catalog.truncated, fit);
    }
    redrawMap = () => {
      if (!indexedHistory || !lastFrame) return;
      if (selection.view === 'timeline') renderTimelinePoints(lastFrame);
      if (selection.view === 'connections' && routesPanel && map) routesPanel.renderMap(map, layer, L);
    };

    async function load() {
      pause();
      const request = ++requestGeneration, window = selection.window;
      const historical = historicalView();
      updateView();
      snapshot = null;
      indexedHistory = null;
      activity = null; activityScope = null;
      activityPanel.clear();
      routesIndex = null;
      if (routesPanel) routesPanel.clear();
      nodes = [];
      catalog = { scopes: [], truncated: false };
      clearPoints();
      timelineEnabled(false);
      comparison.hidden = true;
      comparison.replaceChildren();
      comparison.removeAttribute('data-comparable');
      intervalLabel.textContent = '';
      populateScopes(true);
      shell.setAttribute('aria-busy', 'true');
      status.textContent = 'Loading observed relay data — ' + WINDOWS[window].toLowerCase() + '…';
      snapshotMeta.textContent = '';
      coverage.textContent = '';
      exactBounds.replaceChildren();
      coverage.removeAttribute('data-coverage-state');
      try {
        const data = await api(selection.view === 'connections' ? '/regions/routes' : historical ? '/regions/history' : '/regions/observed?window=' + encodeURIComponent(window), { ttl: 0, bust: true, retry503: false });
        if (token !== generation || request !== requestGeneration || leafletMap !== map) return;
        if (historical) {
          if (!historyTools || !data || !Array.isArray(data.nodes)) throw new Error('Invalid regions history');
          nodes = collectScopeNodes(data.nodes, true);
          indexedHistory = historyTools.indexHistory({ ...data,
            truncated: data.truncated || data.nodes.length > MAX_POINTS,
            scopesTruncated: data.scopesTruncated || data.nodes.some(node => node && Array.isArray(node.scopes) && node.scopes.length > 32),
          }, nodes);
          catalog = scopeCatalog(nodes);
          if (catalog.truncated) indexedHistory.limited = true;
          if (selection.view === 'connections') {
            if (!routesTools || !routesPanel) throw new Error('Connection module unavailable');
            routesIndex = routesTools.indexRoutes(data, indexedHistory, validScope, validPosition);
            catalog = mergeScopeCatalog(catalog, routesIndex.steps.map(step => step.scope));
            if (catalog.truncated) routesIndex.limited = true;
          }
          populateScopes(false);
          draw(true);
          saveSelection(true);
          return;
        }
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

    function saveSelection(replace = false) {
      const hash = scopeHash(selection.scope, selection.window, selection);
      if (location.hash !== hash) history[replace ? 'replaceState' : 'pushState'](null, '', hash);
    }
    period.addEventListener('change', () => {
      pause();
      selection.window = period.value;
      saveSelection();
      if (historicalView()) draw(false); else load();
    });
    select.addEventListener('change', () => {
      pause();
      selection.scope = select.value;
      saveSelection();
      draw(!historicalView());
    });
    view.addEventListener('change', () => {
      pause();
      const previousHistorical = historicalView();
      const previousView = selection.view;
      selection.view = view.value;
      if (previousView !== selection.view && (previousView === 'connections' || selection.view === 'connections')) clearPoints();
      if (selection.view === 'connections') { selection.compare = false; compare.checked = false; }
      if (!historicalView()) { selection.at = ''; selection.compare = false; compare.checked = false; }
      updateView();
      // Health leaves an existing map hidden and skips tile updates. Refresh
      // both its theme and dimensions before fitting any visible map view.
      if (map && selection.view !== 'health') { applyTiles(); map.invalidateSize(); }
      saveSelection();
      if (previousHistorical && historicalView() && indexedHistory && (selection.view !== 'connections' || routesIndex)) {
        draw(selection.view === 'timeline' || selection.view === 'connections');
      } else load();
    });
    compare.addEventListener('change', () => {
      pause();
      selection.compare = compare.checked;
      saveSelection();
      draw(false);
    });
    slider.addEventListener('input', () => {
      pause();
      if (!indexedHistory) return;
      selection.at = historyTools.frameTime(indexedHistory, slider.value);
      scrubFrame = requestAnimationFrame(() => {
        scrubFrame = null;
        if (token !== generation || leafletMap !== map) return;
        draw(false); saveSelection(true);
      });
    });
    function step(direction) {
      if (!indexedHistory) return;
      selection.at = historyTools.frameTime(indexedHistory, Number(slider.value) + direction);
      draw(false); saveSelection(true);
    }
    previous.addEventListener('click', () => { pause(); step(-1); });
    next.addEventListener('click', () => { pause(); step(1); });
    function tick() {
      if (token !== generation || !indexedHistory || selection.view !== 'timeline' || document.hidden || Number(slider.value) >= 168) { pause(); return; }
      step(1);
      if (Number(slider.value) >= 168) { pause(); return; }
      timer = setTimeout(tick, 1000);
    }
    play.addEventListener('click', () => {
      if (timer !== null) { pause(); return; }
      if (selection.view !== 'timeline' || !indexedHistory || document.hidden || Number(slider.value) >= 168) return;
      play.textContent = 'Pause';
      play.setAttribute('aria-label', 'Pause timeline');
      play.setAttribute('aria-pressed', 'true');
      timer = setTimeout(tick, 1000);
    });
    fitButton.addEventListener('click', () => {
      if (selection.view === 'connections') { if (map && routesPanel) routesPanel.fitMap(map, L); }
      else if (selection.view === 'timeline') { if (indexedHistory) fitTimeline(); }
      else draw(true);
    });
    refresh.addEventListener('click', load);
    load();
    setTimeout(() => { if (map && selection.view !== 'health' && token === generation && leafletMap === map) map.invalidateSize(); }, 0);
  }

  function destroy() {
    generation++;
    if (stopPlayback) stopPlayback();
    stopPlayback = null;
    if (visibilityListener) document.removeEventListener('visibilitychange', visibilityListener);
    visibilityListener = null;
    if (themeObserver) themeObserver.disconnect();
    themeObserver = null;
    if (leafletMap) leafletMap.remove();
    leafletMap = null;
  }

  const internals = { validPosition, validScope, collectScopeNodes, collectScopePoints, selectionSummary,
    lastRelayedForScope, listScopes, scopeCatalog, mergeScopeCatalog, convexHull, parseSelection, parseSelectedScope, scopeHash, coverageInfo };
  if (typeof module !== 'undefined' && module.exports) module.exports = internals;
  if (typeof root.registerPage === 'function') root.registerPage('regions', { init, destroy });
})(typeof window === 'undefined' ? globalThis : window);
