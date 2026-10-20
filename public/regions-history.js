/* Bounded UTC-hour relay history. Counts are relay appearances, not packets. */
'use strict';

(function (root) {
  const HOUR = 3600000;
  const WINDOW_HOURS = { '1h': 1, '24h': 24, '7d': 168 };
  const MAX_NODES = 5000, MAX_SCOPES_PER_NODE = 32, MAX_BUCKETS = 504, MAX_CELLS = 250000;
  const MAX_EVIDENCE_CELLS = 128 * 505;
  const COUNT_KEYS = ['excluded', 'unprovable', 'capped'];

  function millis(value) {
    return typeof value === 'string' && value ? Date.parse(value) : NaN;
  }

  function lowerBound(values, target) {
    let low = 0, high = values.length;
    while (low < high) {
      const mid = (low + high) >>> 1;
      if (values[mid] < target) low = mid + 1; else high = mid;
    }
    return low;
  }

  // Evidence gaps are attributed to the transmission's stored scope and hour.
  // Sparse prefix sums per scope plus a dense all-scopes aggregate answer any
  // interval in O(log n). Present-but-unreadable evidence fails closed.
  function indexEvidence(raw) {
    const all = COUNT_KEYS.map(() => new Float64Array(MAX_BUCKETS + 1));
    const evidence = { truncated: false, scopes: new Map(), all };
    if (raw == null) return evidence;
    const sourceScopes = raw && Array.isArray(raw.scopes) ? raw.scopes : null;
    if (!sourceScopes || raw.truncated !== false && raw.truncated != null) evidence.truncated = true;
    let cells = 0;
    for (const scope of sourceScopes || []) {
      if (!scope || typeof scope.name !== 'string' || !Array.isArray(scope.buckets)) { evidence.truncated = true; continue; }
      const rows = [];
      for (const bucket of scope.buckets) {
        if (++cells > MAX_EVIDENCE_CELLS || !bucket || !Number.isInteger(bucket.index) || bucket.index < 0 || bucket.index >= MAX_BUCKETS ||
            COUNT_KEYS.some(key => bucket[key] != null && (!Number.isSafeInteger(bucket[key]) || bucket[key] < 0))) {
          evidence.truncated = true; continue;
        }
        rows.push({ index: bucket.index, counts: COUNT_KEYS.map(key => bucket[key] || 0) });
      }
      rows.sort((a, b) => a.index - b.index);
      const indices = [], prefix = [COUNT_KEYS.map(() => 0)];
      for (const row of rows) {
        if (indices[indices.length - 1] === row.index) { evidence.truncated = true; continue; }
        indices.push(row.index);
        prefix.push(prefix[prefix.length - 1].map((total, i) => total + row.counts[i]));
        row.counts.forEach((count, i) => { all[i][row.index + 1] += count; });
      }
      if (evidence.scopes.has(scope.name)) evidence.truncated = true;
      else evidence.scopes.set(scope.name, { indices, prefix });
    }
    for (const totals of all) for (let i = 1; i <= MAX_BUCKETS; i++) totals[i] += totals[i - 1];
    return evidence;
  }

  // Counts for [startIndex, endIndex) and the selected scope ('' = all scopes).
  // Only unprovable or capped evidence makes the interval incomplete.
  function evidenceCounts(history, scope, startIndex, endIndex) {
    const evidence = history.evidence;
    let values = [0, 0, 0];
    if (!scope) values = evidence.all.map(totals => totals[endIndex] - totals[startIndex]);
    else if (evidence.scopes.has(scope)) {
      const entry = evidence.scopes.get(scope);
      const low = lowerBound(entry.indices, startIndex), high = lowerBound(entry.indices, endIndex);
      values = entry.prefix[high].map((total, i) => total - entry.prefix[low][i]);
    }
    return { excluded: values[0], unprovable: values[1], capped: values[2],
      incomplete: evidence.truncated || values[1] > 0 || values[2] > 0 };
  }

  // Sparse prefix sums are constructed once. A frame reads two prefix values
  // per scope; it never rescans the bucket payload while scrubbing or playing.
  function indexHistory(data, nodes) {
    const start = millis(data.historyStart), end = millis(data.historyEnd), generated = millis(data.generatedAt);
    if (!Number.isFinite(start) || !Number.isFinite(end) || !Number.isFinite(generated) || generated < end ||
        start % HOUR || end % HOUR || data.bucketSeconds !== 3600 || data.bucketCount !== MAX_BUCKETS ||
        end - start !== MAX_BUCKETS * HOUR || !Array.isArray(nodes)) throw new Error('Invalid regions history');
    // Publication caps are snapshot-wide; evidence gaps are per bucket/scope.
    let limited = !!(data.truncated || data.scopesTruncated || data.bucketsTruncated || nodes.length > MAX_NODES);
    let cells = 0, appearances = 0;
    const indexed = [];
    for (const node of nodes.slice(0, MAX_NODES)) {
      const scopes = [];
      const sourceScopes = Array.isArray(node.scopes) ? node.scopes : [];
      if (sourceScopes.length > MAX_SCOPES_PER_NODE) limited = true;
      for (const scope of sourceScopes.slice(0, MAX_SCOPES_PER_NODE)) {
        const source = Array.isArray(scope.buckets) ? scope.buckets : [];
        if (source.length > MAX_BUCKETS) limited = true;
        const buckets = [];
        for (const bucket of source.slice(0, MAX_BUCKETS)) {
          if (++cells > MAX_CELLS) { limited = true; break; }
          if (!bucket || !Number.isInteger(bucket.index) || bucket.index < 0 || bucket.index >= MAX_BUCKETS ||
              !Number.isSafeInteger(bucket.count) || bucket.count <= 0) { limited = true; continue; }
          const last = millis(bucket.lastRelayed);
          const bucketStart = start + bucket.index * HOUR;
          if (!Number.isFinite(last) || last < bucketStart || last >= bucketStart + HOUR) { limited = true; continue; }
          if (!Number.isSafeInteger(appearances + bucket.count)) { limited = true; continue; }
          appearances += bucket.count;
          buckets.push({ index: bucket.index, count: bucket.count, lastRelayed: new Date(last).toISOString() });
        }
        buckets.sort((a, b) => a.index - b.index);
        const indices = [], prefix = [0], times = [];
        for (const bucket of buckets) {
          if (indices.length && indices[indices.length - 1] === bucket.index) { limited = true; continue; }
          const total = prefix[prefix.length - 1] + bucket.count;
          if (!Number.isSafeInteger(total)) { limited = true; continue; }
          indices.push(bucket.index); prefix.push(total); times.push(bucket.lastRelayed);
        }
        if (indices.length) scopes.push({ name: scope.name, indices, prefix, times });
      }
      indexed.push({ ...node, scopes });
    }
    const coverage = data.coverage || {};
    return { start, end, generatedAt: new Date(generated).toISOString(), nodes: indexed,
      coverageStart: millis(coverage.start), loading: coverage.loading === true,
      evidence: indexEvidence(coverage.evidence), limited };
  }

  function frameIndex(history, at) {
    const requested = millis(at);
    const end = Number.isFinite(requested) ? Math.floor(requested / HOUR) * HOUR : history.end;
    return Math.max(0, Math.min(168, Math.round((end - (history.end - 168 * HOUR)) / HOUR)));
  }

  function frameTime(history, index) {
    const bounded = Math.max(0, Math.min(168, Number.isFinite(Number(index)) ? Math.round(Number(index)) : 168));
    return new Date(history.end - (168 - bounded) * HOUR).toISOString();
  }

  function interval(history, window, at) {
    const end = millis(frameTime(history, frameIndex(history, at)));
    const duration = (WINDOW_HOURS[window] || WINDOW_HOURS['24h']) * HOUR;
    return { start: new Date(end - duration).toISOString(), end: new Date(end).toISOString(),
      previousStart: new Date(end - duration * 2).toISOString(), previousEnd: new Date(end - duration).toISOString() };
  }

  function coverageState(history, start, end) {
    if (history.loading) return 'loading';
    if (!Number.isFinite(history.coverageStart)) return 'unknown';
    if (history.coverageStart >= end) return 'unavailable';
    return history.coverageStart <= start ? 'complete' : 'partial';
  }

  function scopeCount(scope, startIndex, endIndex) {
    const low = lowerBound(scope.indices, startIndex), high = lowerBound(scope.indices, endIndex);
    return { count: scope.prefix[high] - scope.prefix[low], lastRelayed: high > low ? scope.times[high - 1] : '' };
  }

  function selectFrame(history, selection) {
    const bounds = interval(history, selection.window, selection.at);
    const start = millis(bounds.start), end = millis(bounds.end), previousStart = millis(bounds.previousStart);
    const currentCoverage = coverageState(history, start, end);
    const previousCoverage = coverageState(history, previousStart, start);
    const startIndex = (start - history.start) / HOUR, endIndex = (end - history.start) / HOUR;
    const previousIndex = (previousStart - history.start) / HOUR;
    const evidence = evidenceCounts(history, selection.scope || '', startIndex, endIndex);
    const previousEvidence = evidenceCounts(history, selection.scope || '', previousIndex, startIndex);
    const comparable = !history.limited && currentCoverage === 'complete' && previousCoverage === 'complete' &&
      !evidence.incomplete && !previousEvidence.incomplete;
    const current = [], previous = [], rows = [];
    const added = comparable ? [] : null, retained = comparable ? [] : null, notObserved = comparable ? [] : null;
    let currentAppearances = 0, previousAppearances = 0;
    for (const node of history.nodes) {
      let count = 0, before = 0, lastRelayed = '', previousLastRelayed = '';
      for (const scope of node.scopes) {
        if (selection.scope && scope.name !== selection.scope) continue;
        const now = scopeCount(scope, startIndex, endIndex), old = scopeCount(scope, previousIndex, startIndex);
        count += now.count; before += old.count;
        if (now.lastRelayed > lastRelayed) lastRelayed = now.lastRelayed;
        if (old.lastRelayed > previousLastRelayed) previousLastRelayed = old.lastRelayed;
      }
      currentAppearances += count; previousAppearances += before;
      if (count) current.push(node);
      if (before) previous.push(node);
      if (comparable) {
        if (count && before) retained.push(node);
        else if (count) added.push(node);
        else if (before) notObserved.push(node);
      }
      if (count || (selection.compare && comparable && before)) rows.push({ node, count, previousCount: before,
        lastRelayed, previousLastRelayed, state: selection.compare && comparable ?
          (count && before ? 'retained' : count ? 'added' : 'not-observed') : 'observed' });
    }
    return { interval: bounds, rows, currentCount: current.length, previousCount: previous.length,
      currentAppearances, previousAppearances, currentCoverage, previousCoverage, comparable,
      limited: history.limited, evidence, previousEvidence, added, retained, notObserved,
      appearanceChange: comparable ? currentAppearances - previousAppearances : null,
      percentChange: comparable && previousAppearances > 0 ? (currentAppearances - previousAppearances) / previousAppearances * 100 : null };
  }

  // Build one selected-scope activity index per payload/scope change. Dense
  // arrays are bounded to the 504-hour snapshot, never one array per scope.
  // A node can carry several scopes in an hour but remains one observed relay.
  function indexActivity(history, scope) {
    const appearances = new Float64Array(MAX_BUCKETS), nodes = new Uint16Array(MAX_BUCKETS);
    const times = new Array(MAX_BUCKETS).fill(''), seen = new Uint16Array(MAX_BUCKETS);
    let generation = 0;
    for (const node of history.nodes) {
      generation++;
      for (const entry of node.scopes) {
        if (scope && entry.name !== scope) continue;
        for (let i = 0; i < entry.indices.length; i++) {
          const index = entry.indices[i];
          appearances[index] += entry.prefix[i + 1] - entry.prefix[i];
          if (seen[index] !== generation) { nodes[index]++; seen[index] = generation; }
          if (entry.times[i] > times[index]) times[index] = entry.times[i];
        }
      }
    }
    // Withheld hours for this scope, precomputed once per payload/scope change.
    const evidenceIncomplete = new Uint8Array(MAX_BUCKETS);
    for (let index = 0; index < MAX_BUCKETS; index++) evidenceIncomplete[index] = evidenceCounts(history, scope || '', index, index + 1).incomplete ? 1 : 0;
    return { history, scope: scope || '', appearances, nodes, times, evidenceIncomplete };
  }

  // Playback reads only bounded hourly arrays. Partial/unknown history has
  // observed evidence, but no chart value or whole-window activity rate.
  function selectActivity(activity, history, frame) {
    if (!activity || activity.history !== history) throw new Error('Activity index does not match history');
    const start = millis(frame.interval.start), end = millis(frame.interval.end);
    const hours = (end - start) / HOUR;
    if (![1, 24, 168].includes(hours) || start % HOUR || end % HOUR || start < history.start || end > history.end) {
      throw new Error('Invalid activity interval');
    }
    const first = (start - history.start) / HOUR;
    const bins = [];
    let lastRelayed = '', total = 0;
    for (let i = 0; i < hours; i++) {
      const index = first + i, binStart = start + i * HOUR;
      const coverage = coverageState(history, binStart, binStart + HOUR);
      const observedAppearances = activity.appearances[index], observedNodes = activity.nodes[index];
      const evidenceIncomplete = activity.evidenceIncomplete[index] === 1;
      const complete = coverage === 'complete' && !history.limited && !evidenceIncomplete;
      bins.push({ start: new Date(binStart).toISOString(), end: new Date(binStart + HOUR).toISOString(), coverage,
        evidenceIncomplete, observedAppearances, observedNodes, appearances: complete ? observedAppearances : null,
        nodes: complete ? observedNodes : null });
      total += observedAppearances;
      if (activity.times[index] > lastRelayed) lastRelayed = activity.times[index];
    }
    return { bins, lastRelayed,
      ratePerHour: frame.currentCoverage === 'complete' && !history.limited && !frame.evidence.incomplete ? total / hours : null };
  }

  const api = { indexHistory, frameIndex, frameTime, interval, selectFrame, scopeCount, evidenceCounts, indexActivity, selectActivity };
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  root.RegionsHistory = api;
})(typeof window === 'undefined' ? globalThis : window);
