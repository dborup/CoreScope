/* Connections retain two separate facts: shared relay-history attribution and directed
 * adjacent steps from scope-matched flood observations. Neither is a handoff. */
'use strict';
(function (root) {
  const historyTools = typeof module !== 'undefined' && module.exports ? require('./regions-history.js') : root.RegionsHistory;
  const MAX_STEPS = 5000, MAX_CELLS = 100000, MAX_PAIRS = 1000, PAGE_SIZE = 25, MAP_LIMIT = 200;
  const keyPattern = /^[0-9a-f]{64}$/i;

  function indexRoutes(data, history, validScope, validPosition) {
    const nodes = new Map(history.nodes.map(node => [node.publicKey.toLowerCase(), node]));
    let limited = !!(data.stepsTruncated || data.stepBucketsTruncated || data.stepEvidence?.sourceTruncated || data.stepEvidence?.incomplete);
    for (const key of ['skippedObservations', 'unresolvedHops', 'missingEndpointSteps']) {
      const count = data.stepEvidence?.[key];
      if (count != null && (!Number.isSafeInteger(count) || count < 0 || count > 0)) limited = true;
    }
    if (Array.isArray(data.stepNodes)) {
      if (data.stepNodes.length > MAX_STEPS * 2) limited = true;
      for (const node of data.stepNodes.slice(0, MAX_STEPS * 2)) {
        if (!node || !keyPattern.test(node.publicKey) || !['repeater', 'room'].includes(node.role)) { limited = true; continue; }
        const positioned = node.positionSource === 'reported' && typeof validPosition === 'function' && validPosition(node);
        nodes.set(node.publicKey.toLowerCase(), { publicKey: node.publicKey.toLowerCase(), name: String(node.name || node.publicKey.slice(0, 8)), role: node.role,
          positionSource: positioned ? 'reported' : 'none', lat: positioned ? Number(node.lat) : null, lon: positioned ? Number(node.lon) : null,
          metadataSource: node.metadataSource === 'archived' ? 'archived' : 'current' });
      }
    }
    const input = Array.isArray(data.steps) ? data.steps : [];
    if (!Array.isArray(data.steps) || input.length > MAX_STEPS) limited = true;
    const steps = [], seen = new Set();
    let cells = 0, total = 0;
    for (const source of input.slice(0, MAX_STEPS)) {
      if (!source || !keyPattern.test(source.from) || !keyPattern.test(source.to) ||
          !validScope(source.scope) || source.from.toLowerCase() === source.to.toLowerCase()) { limited = true; continue; }
      const from = source.from.toLowerCase(), to = source.to.toLowerCase();
      const key = JSON.stringify([from, to, source.scope]);
      if (!nodes.has(from) || !nodes.has(to) || seen.has(key) || !Array.isArray(source.buckets)) { limited = true; continue; }
      seen.add(key);
      const values = [];
      if (source.buckets.length > 504) limited = true;
      for (const bucket of source.buckets.slice(0, 504)) {
        if (++cells > MAX_CELLS) { limited = true; break; }
        const last = typeof bucket?.lastRelayed === 'string' ? Date.parse(bucket.lastRelayed) : NaN;
        if (!Number.isInteger(bucket?.index) || bucket.index < 0 || bucket.index >= 504 ||
            !Number.isSafeInteger(bucket.count) || bucket.count < 1 || !Number.isSafeInteger(total + bucket.count) ||
            !Number.isFinite(last) || last < history.start + bucket.index * 3600000 || last >= history.start + (bucket.index + 1) * 3600000) {
          limited = true; continue;
        }
        total += bucket.count;
        values.push({ index: bucket.index, count: bucket.count, time: new Date(last).toISOString() });
      }
      values.sort((a, b) => a.index - b.index);
      const indices = [], prefix = [0], times = [];
      for (const bucket of values) {
        if (indices[indices.length - 1] === bucket.index) { limited = true; continue; }
        indices.push(bucket.index); prefix.push(prefix[prefix.length - 1] + bucket.count); times.push(bucket.time);
      }
      if (indices.length) steps.push({ key, from, to, scope: source.scope, fromNode: nodes.get(from), toNode: nodes.get(to), indices, prefix, times });
    }
    const evidence = data.stepEvidence || {};
    const count = value => Number.isSafeInteger(value) && value >= 0 ? value : 0;
    return { history, steps, limited, evidence: { skippedObservations: count(evidence.skippedObservations), unresolvedHops: count(evidence.unresolvedHops), missingEndpointSteps: count(evidence.missingEndpointSteps),
      sourceTruncated: evidence.sourceTruncated === true, incomplete: evidence.incomplete === true }, cached: null };
  }

  function sharedPairs(history, first, last, scope) {
    const memberships = new Map(), groups = new Map();
    let sharedRelays = 0;
    history.nodes.forEach((node, index) => {
      const active = node.scopes.filter(item => historyTools.scopeCount(item, first, last).count > 0).map(item => item.name).sort();
      if (active.length < 2 || (scope && !active.includes(scope))) return;
      sharedRelays++;
      for (const name of active) {
        if (!memberships.has(name)) memberships.set(name, []);
        memberships.get(name).push(index);
      }
      const key = JSON.stringify(active);
      if (!groups.has(key)) groups.set(key, active);
    });
    const candidates = new Map();
    const possible = scope ? Math.max(0, memberships.size - 1) : memberships.size * (memberships.size - 1) / 2;
    let truncated = false;
    outer: for (const active of groups.values()) {
      for (let a = 0; a < active.length; a++) for (let b = a + 1; b < active.length; b++) {
        if (scope && active[a] !== scope && active[b] !== scope) continue;
        const key = JSON.stringify([active[a], active[b]]);
        if (!candidates.has(key)) {
          if (candidates.size === MAX_PAIRS) { truncated = true; break outer; }
          candidates.set(key, { key, from: active[a], to: active[b] });
        }
      }
      if (candidates.size === possible) break;
    }
    const pairs = [];
    for (const pair of candidates.values()) {
      const left = memberships.get(pair.from), right = memberships.get(pair.to);
      let a = 0, b = 0, count = 0;
      const examples = [];
      while (a < left.length && b < right.length) {
        if (left[a] < right[b]) a++;
        else if (right[b] < left[a]) b++;
        else {
          count++;
          if (examples.length < 3) examples.push(history.nodes[left[a]]);
          a++; b++;
        }
      }
      pairs.push({ ...pair, count, examples });
    }
    pairs.sort((a, b) => b.count - a.count || a.key.localeCompare(b.key));
    return { pairs, sharedRelays, pairsTruncated: truncated };
  }

  function positioned(node) { return node.positionSource === 'reported' && Number.isFinite(node.lat) && Number.isFinite(node.lon); }

  function selectRoutes(index, history, frame, selection) {
    if (index.history !== history) throw new Error('Routes index belongs to another history snapshot');
    const key = JSON.stringify([frame.interval.start, frame.interval.end, selection.scope || '']);
    if (index.cached?.key === key) return index.cached.value;
    const first = (Date.parse(frame.interval.start) - history.start) / 3600000;
    const last = (Date.parse(frame.interval.end) - history.start) / 3600000;
    const steps = [];
    let stepAppearances = 0, unmappedSteps = 0;
    for (const step of index.steps) {
      if (selection.scope && step.scope !== selection.scope) continue;
      const value = historyTools.scopeCount(step, first, last);
      if (!value.count) continue;
      stepAppearances += value.count;
      if (!positioned(step.fromNode) || !positioned(step.toNode)) unmappedSteps++;
      steps.push({ ...step, count: value.count, lastRelayed: value.lastRelayed });
    }
    steps.sort((a, b) => b.count - a.count || a.key.localeCompare(b.key));
    const pairs = sharedPairs(history, first, last, selection.scope || '');
    const value = { ...pairs, steps, stepAppearances, unmappedSteps,
      complete: frame.currentCoverage === 'complete' && !history.limited && !index.limited,
      sharedComplete: frame.currentCoverage === 'complete' && !history.limited,
      evidence: index.evidence, limited: index.limited || history.limited,
      mapSteps: steps.filter(step => positioned(step.fromNode) && positioned(step.toNode)).slice(0, MAP_LIMIT) };
    index.cached = { key, value };
    return value;
  }

  let panelSequence = 0;
  function createPanel(onPage) {
    const el = (tag, className, text) => {
      const node = document.createElement(tag);
      if (className) node.className = className;
      if (text != null) node.textContent = text;
      return node;
    };
    const panel = el('section', 'regions-connections'); panel.hidden = true;
    panel.setAttribute('aria-label', 'Scope connections and observed relay steps');
    const summary = el('p', 'regions-connections-summary'); panel.appendChild(summary);
    panel.appendChild(el('h2', '', 'Shared relay-history memberships'));
    panel.appendChild(el('p', 'regions-connections-provenance', 'Distinct visible identities attributed to both scopes by the transmission-level relay history used by Timeline/Health. Observation route/scope variants are not independently verified here; mixed flood/direct transmissions can include planned-path candidates. Only directed steps below use each observation’s own frame evidence.'));
    panel.appendChild(el('p', 'regions-connections-note', 'Shared history membership does not establish cross-scope traffic, a scope transition or a direct radio link.'));
    const pairSummary = el('p', 'regions-pairs-summary'); panel.appendChild(pairSummary);

    function tableSection(headers, className, pageKey, label) {
      const section = el('div', 'regions-relations-section');
      const scroll = el('div', 'regions-relations-scroll'); scroll.tabIndex = 0;
      scroll.setAttribute('role', 'region'); scroll.setAttribute('aria-label', label);
      const table = el('table', className), head = el('thead'), tr = el('tr');
      for (const text of headers) { const th = el('th', '', text); th.scope = 'col'; tr.appendChild(th); }
      head.appendChild(tr); const body = el('tbody'); table.append(head, body); scroll.appendChild(table); section.appendChild(scroll);
      const pager = el('div', 'regions-relations-pagination');
      const previous = el('button', '', 'Previous'), next = el('button', '', 'Next'), text = el('span');
      previous.type = next.type = 'button'; previous.setAttribute('aria-label', 'Previous ' + label + ' page'); next.setAttribute('aria-label', 'Next ' + label + ' page');
      let page = 1;
      previous.addEventListener('click', () => onPage(pageKey, page - 1)); next.addEventListener('click', () => onPage(pageKey, page + 1));
      pager.append(previous, text, next); section.appendChild(pager); panel.appendChild(section);
      const records = new Map();
      return { update(values, requestedPage, makeCells, signature) {
        const pages = Math.max(1, Math.ceil(values.length / PAGE_SIZE));
        page = Math.max(1, Math.min(pages, Number.isInteger(requestedPage) ? requestedPage : 1));
        const selected = values.slice((page - 1) * PAGE_SIZE, page * PAGE_SIZE), keys = new Set(selected.map(value => value.key));
        for (const [key, record] of records) if (!keys.has(key)) { record.row.remove(); records.delete(key); }
        let cursor = body.firstChild;
        for (const value of selected) {
          let record = records.get(value.key);
          if (!record) { record = { row: el('tr'), signature: null }; record.row.dataset.key = value.key; records.set(value.key, record); }
          const current = signature(value);
          if (current !== record.signature) { record.row.replaceChildren(...makeCells(value)); record.signature = current; }
          if (record.row !== cursor) body.insertBefore(record.row, cursor);
          cursor = record.row.nextSibling;
        }
        text.textContent = 'Page ' + page + ' of ' + pages + ' · ' + values.length + ' returned rows';
        previous.disabled = page <= 1; next.disabled = page >= pages;
        section.dataset.page = String(page);
        return page;
      } };
    }

    const pairTable = tableSection(['Scope pair', 'History identities', 'Example identities'], 'regions-pairs-table', 'pairsPage', 'shared scope pairs');
    panel.appendChild(el('h2', '', 'Observed directed relay steps'));
    panel.appendChild(el('p', 'regions-connections-note', 'Consecutive identity-resolved hops from scope-matched flood observations. Opposite directions are separate. Steps from different observations are never joined into a claimed full route. Counts are distinct transmissions per directed step and scope, not a global packet total.'));
    const stepSummary = el('p', 'regions-steps-summary'); panel.appendChild(stepSummary);
    const stepTable = tableSection(['Direction', 'Packet scope', 'Distinct transmissions', 'Last observed (UTC)'], 'regions-steps-table', 'page', 'directed relay steps');
    const diagnostics = el('p', 'regions-step-diagnostics'); panel.appendChild(diagnostics);
    const mapSummary = el('p', 'regions-connections-map-note'); panel.appendChild(mapSummary);
    const link = node => { const a = el('a', '', node.name); a.href = '#/nodes/' + encodeURIComponent(node.publicKey); return a; };
    let latest = null, attachedLayer = null, parentLayer = null, markerDefinition = null;
    const mapRecords = new Map(), mapNodes = new Map();
    const arrowId = 'regions-step-arrow-' + (++panelSequence);

    function update(value, frame, selection) {
      latest = value; panel.hidden = false;
      const limited = value.complete ? '' : ' Retained evidence is incomplete or limited; missing results do not establish absence.';
      summary.textContent = 'History memberships and observed directed steps in ' + frame.interval.start + ' — ' + frame.interval.end + '.' + limited;
      pairSummary.textContent = value.pairs.length + (value.pairsTruncated ? '+ retained' : '') + ' scope pairs; ' + value.sharedRelays +
        ' distinct identities with shared history membership.' + (value.sharedComplete ? '' : ' Shared membership history is incomplete or limited.') +
        (value.pairsTruncated ? ' The 1,000-pair safety limit was reached; this is a deterministic retained subset, not a ranking of all pairs. Select a scope to narrow it.' : '');
      const pairsPage = pairTable.update(value.pairs, selection.pairsPage, pair => {
        const names = el('td', '', pair.from + ' — ' + pair.to), count = el('td', '', pair.count.toLocaleString()), examples = el('td');
        pair.examples.forEach((node, index) => { if (index) examples.append(', '); examples.appendChild(link(node)); });
        if (pair.count > pair.examples.length) examples.append(' (examples only)');
        return [names, count, examples];
      }, pair => JSON.stringify([pair.count, pair.examples.map(node => [node.publicKey, node.name])]));
      stepSummary.textContent = value.steps.length + ' directed step/scope rows; ' + value.stepAppearances.toLocaleString() +
        ' step appearances in retained observations. ' + value.unmappedSteps + ' rows have endpoints without usable positions.' +
        (!value.steps.length && value.complete ? ' No qualifying directed steps were observed; this does not establish an outage or lack of connectivity.' : '');
      const page = stepTable.update(value.steps, selection.page, step => {
        const direction = el('td'); direction.append(link(step.fromNode), ' → ', link(step.toNode));
        return [direction, el('td', '', step.scope), el('td', '', step.count.toLocaleString()), el('td', '', step.lastRelayed)];
      }, step => JSON.stringify([step.count, step.lastRelayed, step.fromNode.name, step.toNode.name]));
      const evidence = value.evidence;
      diagnostics.textContent = 'Snapshot-wide evidence diagnostics (not selected-interval totals): ' + evidence.skippedObservations +
        ' excluded or unusable observations; ' + evidence.unresolvedHops + ' unresolved hops; ' + evidence.missingEndpointSteps +
        ' steps without publishable endpoint metadata.' + (evidence.incomplete || evidence.sourceTruncated ? ' The source snapshot is incomplete or capped.' : '') +
        ' Direct planned paths, TRACE/adverts and mismatched or unknown scopes are excluded. Unresolved or hidden hops break adjacency.';
      mapSummary.textContent = value.mapSteps.length + ' directed step rows mapped of ' + (value.steps.length - value.unmappedSteps) +
        ' with usable endpoint positions. The map is limited to ' + MAP_LIMIT + ' rows. Positions are current or last-known self-reported coordinates, not historical movement or measured radio paths.';
      panel.setAttribute('data-ready', 'true'); panel.setAttribute('data-complete', String(value.complete));
      return { page, pairsPage };
    }

    function clearMap() {
      if (attachedLayer) attachedLayer.clearLayers();
      mapRecords.clear(); mapNodes.clear();
      if (markerDefinition) markerDefinition.remove(); markerDefinition = null;
    }

    function renderMap(map, layer, leaflet) {
      if (!latest) return;
      if (!attachedLayer || parentLayer !== layer) { clearMap(); attachedLayer = leaflet.layerGroup(); parentLayer = layer; }
      if (!layer.hasLayer(attachedLayer)) layer.addLayer(attachedLayer);
      const accent = getComputedStyle(document.documentElement).getPropertyValue('--accent').trim();
      const wanted = new Set(), nodes = new Map();
      for (const step of latest.mapSteps) {
        wanted.add(step.key); nodes.set(step.from, step.fromNode); nodes.set(step.to, step.toNode);
        let record = mapRecords.get(step.key);
        if (!record) {
          record = { line: leaflet.polyline([[step.fromNode.lat, step.fromNode.lon], [step.toNode.lat, step.toNode.lon]], {
            color: accent, weight: 3, opacity: 0.8, className: 'regions-directed-step',
          }).addTo(attachedLayer), signature: '' };
          mapRecords.set(step.key, record);
        }
        record.line.setStyle({ color: accent });
        const signature = step.count + '|' + step.lastRelayed;
        if (record.signature !== signature) {
          const popup = el('div'); popup.append(link(step.fromNode), ' → ', link(step.toNode),
            el('div', '', step.count + ' distinct transmissions · ' + step.scope), el('div', '', 'Last observed: ' + step.lastRelayed));
          record.line.bindPopup(popup); record.signature = signature;
        }
        const path = record.line.getElement();
        if (path) { path.setAttribute('marker-end', 'url(#' + arrowId + ')'); path.setAttribute('role', 'img'); path.setAttribute('aria-label', step.fromNode.name + ' to ' + step.toNode.name + ', ' + step.count + ' distinct transmissions'); }
      }
      for (const [key, record] of mapRecords) if (!wanted.has(key)) { attachedLayer.removeLayer(record.line); mapRecords.delete(key); }
      for (const [key, marker] of mapNodes) if (!nodes.has(key)) { attachedLayer.removeLayer(marker); mapNodes.delete(key); }
      for (const [key, node] of nodes) if (!mapNodes.has(key)) {
        mapNodes.set(key, leaflet.circleMarker([node.lat, node.lon], { radius: 5, color: accent, fillOpacity: 0.8 }).bindPopup(link(node)).addTo(attachedLayer));
      } else mapNodes.get(key).setStyle({ color: accent });
      if (!markerDefinition && mapRecords.size) {
        const svg = attachedLayer.getLayers()[0]?.getElement()?.ownerSVGElement;
        if (svg) {
          markerDefinition = document.createElementNS('http://www.w3.org/2000/svg', 'defs');
          const marker = document.createElementNS('http://www.w3.org/2000/svg', 'marker');
          marker.id = arrowId; marker.setAttribute('viewBox', '0 0 10 10'); marker.setAttribute('refX', '9'); marker.setAttribute('refY', '5');
          marker.setAttribute('markerWidth', '5'); marker.setAttribute('markerHeight', '5'); marker.setAttribute('orient', 'auto');
          const arrow = document.createElementNS('http://www.w3.org/2000/svg', 'path'); arrow.setAttribute('d', 'M 0 0 L 10 5 L 0 10 z'); arrow.setAttribute('class', 'regions-step-arrow');
          marker.appendChild(arrow); markerDefinition.appendChild(marker); svg.prepend(markerDefinition);
        }
      }
    }

    function fitMap(map, leaflet) {
      if (!latest?.mapSteps.length) return;
      const points = latest.mapSteps.flatMap(step => [[step.fromNode.lat, step.fromNode.lon], [step.toNode.lat, step.toNode.lon]]);
      map.fitBounds(leaflet.latLngBounds(points).pad(0.15), { maxZoom: 10 });
    }
    function clear() { panel.hidden = true; panel.removeAttribute('data-ready'); clearMap(); }
    return { panel, update, renderMap, fitMap, clear, clearMap };
  }

  const api = { indexRoutes, selectRoutes, createPanel, PAGE_SIZE, MAP_LIMIT };
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  root.RegionsRoutes = api;
})(typeof window === 'undefined' ? globalThis : window);
