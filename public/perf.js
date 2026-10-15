/* === CoreScope — perf.js === */
'use strict';

var SOURCE_REPO_URL = 'https://github.com/dborup/CoreScope';

// detectPerfAnomalies — pure, testable.
// Computes per-component write rates over a rolling time window and flags any
// component whose current per-second rate exceeds `factor` × its rolling
// baseline rate. Issue #1120 acceptance: 5-minute window, 10× threshold.
//
// Inputs:
//   history: ordered array of snapshots [{ sampleAt: ISO, sources: { name: cum } }]
//   current: the freshest snapshot, same shape
//   opts:
//     windowMs       (default 5*60*1000) — rolling baseline window
//     factor         (default 10)        — rate-multiplier threshold
//     minHistorySec  (default 30)        — refuse to flag until baseline is stable
//
// Returns: { rates, baselineRates, flags } — all keyed by source name.
function detectPerfAnomalies(history, current, opts) {
  opts = opts || {};
  const windowMs = opts.windowMs || (5 * 60 * 1000);
  const factor = opts.factor || 10;
  const minHistorySec = opts.minHistorySec != null ? opts.minHistorySec : 30;
  const out = { rates: {}, baselineRates: {}, flags: {} };
  if (!current || !current.sources || !history || history.length === 0) return out;
  const curT = Date.parse(current.sampleAt);
  if (!isFinite(curT)) return out;

  // Find the most recent prior sample (for the *current* per-second rate)
  // and the oldest sample within the window (for the baseline).
  const prior = history[history.length - 1];
  const priorT = Date.parse(prior.sampleAt);
  const curDt = (curT - priorT) / 1000;
  if (!(curDt > 0)) return out;

  // Baseline: oldest sample within window vs. prior (the snapshot just before
  // `current`). Anything older than windowMs is excluded.
  const cutoff = curT - windowMs;
  let baseIdx = 0;
  for (let i = history.length - 1; i >= 0; i--) {
    if (Date.parse(history[i].sampleAt) < cutoff) { baseIdx = i + 1; break; }
  }
  if (baseIdx >= history.length) baseIdx = history.length - 1;
  const baseSnap = history[baseIdx];
  const baseT = Date.parse(baseSnap.sampleAt);
  const baseDt = (priorT - baseT) / 1000;

  // Compute rates for every source seen in current.
  for (const k of Object.keys(current.sources)) {
    const cur = current.sources[k] || 0;
    const prev = (prior.sources && prior.sources[k]) || 0;
    const rate = (cur - prev) / curDt;
    out.rates[k] = rate;
    if (baseDt <= 0 || baseDt < minHistorySec) {
      out.baselineRates[k] = null;
      continue;
    }
    const baseStart = (baseSnap.sources && baseSnap.sources[k]) || 0;
    const baseEnd = prev; // baseline window = [baseSnap .. prior]
    const baseRate = (baseEnd - baseStart) / baseDt;
    out.baselineRates[k] = baseRate;
    // Guard floor to avoid 0-baseline → infinite ratio false positives.
    const floor = 0.05; // 1 event per 20s minimum baseline
    if (rate > factor * Math.max(baseRate, floor) && rate > factor * floor) {
      out.flags[k] = true;
    }
  }
  return out;
}
if (typeof window !== 'undefined') {
  window.detectPerfAnomalies = detectPerfAnomalies;
}

// buildWSClientChart — pure, testable. Maps WS-client history samples
// ([{t, clients, peak}], oldest first) onto a w×h box. X follows time, Y is
// scaled to the highest peak. Everything is coerced to Number at the
// boundary: a null/non-object entry, a non-numeric field, Infinity (e.g. a
// huge JSON literal) and negative counts all fall back to 0 rather than
// throwing or producing NaN/Infinity points (#433 F4).
function buildWSClientChart(samples, w, h, intervalSec) {
  const toCount = function (v) {
    const n = Number(v);
    return (isFinite(n) && n >= 0) ? n : 0;
  };
  const rows = (samples || []).map(function (s) {
    s = s || {};
    return { t: Date.parse(s.t), clients: toCount(s.clients), peak: toCount(s.peak), gapBefore: s.gapBefore === true };
  }).filter(function (s) { return isFinite(s.t); });
  let peakOverall = 0;
  rows.forEach(function (s) {
    peakOverall = Math.max(peakOverall, s.peak, s.clients);
  });
  const maxY = Math.max(1, peakOverall);
  const t0 = rows.length ? rows[0].t : 0;
  const span = rows.length ? rows[rows.length - 1].t - t0 : 0;
  const pad = 4; // keep lines at 0 / max from being clipped by the svg edge
  const pt = function (t, v) {
    return {
      x: span > 0 ? (t - t0) / span * w : w / 2,
      y: pad + (h - 2 * pad) * (1 - Math.min(v, maxY) / maxY)
    };
  };
  // Keep missing measurements unknown. In particular, joining the last point
  // before a restart to the first point after it invents measurements during
  // downtime. The backend preserves these gaps even inside aggregate buckets.
  const maxGap = Math.max(60, Number(intervalSec) || 60) * 2000;
  const clientSegments = [], peakSegments = [];
  rows.forEach(function (s, i) {
    if (!i || s.gapBefore || s.t - rows[i - 1].t > maxGap) {
      clientSegments.push([]);
      peakSegments.push([]);
    }
    clientSegments[clientSegments.length - 1].push(pt(s.t, s.clients));
    peakSegments[peakSegments.length - 1].push(pt(s.t, Math.max(s.peak, s.clients)));
  });
  return {
    maxY: maxY,
    latest: rows.length ? rows[rows.length - 1].clients : 0,
    peakOverall: peakOverall,
    fromT: t0,
    toT: t0 + span,
    clientPoints: rows.map(function (s) { return pt(s.t, s.clients); }),
    peakPoints: rows.map(function (s) { return pt(s.t, Math.max(s.peak, s.clients)); }),
    clientSegments: clientSegments,
    peakSegments: peakSegments
  };
}

function normalizeWSHistoryRange(range) {
  return range === '7d' || range === '30d' ? range : '24h';
}

function wsHistoryStorageNote(hist) {
  const storage = hist.persistence;
  let text = 'Persistent storage status is unknown.';
  if (storage && storage.enabled === false) {
    text = 'Persistent storage is disabled; history is kept only in memory.';
  } else if (storage && storage.enabled === true && storage.healthy === false) {
    text = 'Persistent storage is unavailable; new samples may be lost on restart.';
  } else if (storage && storage.enabled === true && storage.healthy === true) {
    text = 'History is saved for up to 30 days and survives restarts when the history file is retained.';
  }
  return '<p class="text-muted ws-history-storage-note" style="font-size:12px;">' + text + '</p>';
}

// renderWSClientHistory — HTML for the "open connections over time" card.
// Only numbers and locale time strings are interpolated.
//
// currentClients (#433 F1): the chart's own "latest" value is the last
// *minute* sample, so it can lag the true count by up to 60s. The page
// refresh already fetches /api/health every 5s, so pass its
// health.websocket.clients through here for the "open now" headline and
// reserve the sampled value as a chart-only fallback (e.g. health failed).
function renderWSClientHistory(hist, currentClients, range, includeHeading) {
  if (!hist) return '';
  range = normalizeWSHistoryRange(range || hist.range);
  const periodLabel = range === '7d' ? '7 days' : range === '30d' ? '30 days' : '24 h';
  const heading = includeHeading === false ? '' : '<h3><svg class="ph-icon" aria-hidden="true"><use href="/icons/phosphor-sprite.svg#ph-lightning"/></svg> WebSocket clients (' + periodLabel + ')</h3>';
  const samples = Array.isArray(hist.samples) ? hist.samples : [];
  if (samples.length < 2) {
    return heading + '<p class="text-muted">Collecting data… showing available history within ' + periodLabel + '.</p>' + wsHistoryStorageNote(hist);
  }
  const W = 600, H = 120;
  const c = buildWSClientChart(samples, W, H, hist.intervalSec);
  const openNow = (function () {
    const n = Number(currentClients);
    return (currentClients != null && isFinite(n) && n >= 0) ? n : c.latest;
  })();
  const poly = function (pts) {
    return pts.map(function (p) { return p.x.toFixed(1) + ',' + p.y.toFixed(1); }).join(' ');
  };
  const timeLabel = function (ms) {
    const d = new Date(ms);
    return range === '24h'
      ? d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
      : d.toLocaleString([], { year: 'numeric', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' });
  };
  const series = function (segments, color, width, dash) {
    return segments.map(function (pts) {
      const attrs = ' stroke="' + color + '" stroke-width="' + width + '"' + (dash ? ' stroke-dasharray="3 3"' : '') + ' vector-effect="non-scaling-stroke"';
      // A one-point segment has no line length; draw its measured value too.
      const dot = pts.length === 1 ? '<circle cx="' + pts[0].x.toFixed(1) + '" cy="' + pts[0].y.toFixed(1) + '" r="2" fill="' + color + '"/>' : '';
      return '<polyline fill="none"' + attrs + ' points="' + poly(pts) + '"/>' + dot;
    }).join('');
  };
  const intervalMinutes = Math.max(1, Math.round((Number(hist.intervalSec) || 60) / 60));
  return heading + '<div class="perf-card" style="text-align:left;">' +
    '<div style="display:flex;gap:24px;flex-wrap:wrap;margin-bottom:8px;">' +
    '<span><b>' + openNow + '</b> open now</span>' +
    '<span><b>' + c.peakOverall + '</b> highest in period</span>' +
    '<span class="text-muted">' + timeLabel(c.fromT) + ' – ' + timeLabel(c.toT) + '</span></div>' +
    '<svg class="ws-history" viewBox="0 0 ' + W + ' ' + H + '" preserveAspectRatio="none" role="img" ' +
    'aria-label="Open WebSocket connections over time" style="width:100%;height:' + H + 'px;display:block;">' +
    series(c.peakSegments, 'var(--text-muted)', 1, true) +
    series(c.clientSegments, 'var(--link-color)', 2, false) +
    '</svg>' +
    '<div class="text-muted" style="font-size:12px;">' + periodLabel + ' selected; ' + intervalMinutes + '-minute samples. Solid: last measured count in each interval. Dashed: highest count in each interval. Gaps are unmeasured, not zero. Y-axis 0–' + c.maxY + '.</div>' +
    wsHistoryStorageNote(hist) + '</div>';
}
if (typeof window !== 'undefined') {
  window.buildWSClientChart = buildWSClientChart;
  window.renderWSClientHistory = renderWSClientHistory;
}

// Version and commit render as plain text, never as links: a git-describe
// build (v0.0.1-36-gdc1297db) or a local integration commit may not exist on
// GitHub, and the string format can't tell us. The source link is separate.
function renderVersionCard(health) {
  if (!health || (!health.version && !health.commit)) return '';
  var ver = health.version && health.version !== 'unknown' ? String(health.version) : null;
  var sha = health.commit && health.commit !== 'unknown' ? String(health.commit) : null;
  if (!ver && !sha) return '';
  function esc(s) {
    return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;').replace(/'/g, '&#39;');
  }
  var parts = [];
  if (ver) parts.push(esc(ver));
  if (sha) parts.push('<span title="' + esc(sha) + '">' + esc(sha.slice(0, 8)) + '</span>');
  return '<div class="perf-card"><div class="perf-num perf-num--small">' + parts.join(' · ') + '</div>' +
    '<div class="perf-label">Version</div>' +
    '<div class="perf-num--small"><a href="' + SOURCE_REPO_URL + '" target="_blank" rel="noopener">Source Code</a></div></div>';
}

(function () {
  let interval = null;

  // At most three range entries. History changes once a minute, whereas
  // /api/health's open-now count still refreshes every five seconds. Cache
  // by range, coalesce in-flight requests and retry failed fetches next tick.
  let wsHistoryCache = new Map();
  let wsHistoryRange = '24h';
  let pageGeneration = 0;
  let refreshGeneration = 0;
  let pageActive = false;
  const WS_HISTORY_CACHE_MS = 60000;

  function fetchWSHistory(range) {
    const entry = wsHistoryCache.get(range);
    if (entry && entry.promise) return entry.promise;
    if (entry && Date.now() - entry.fetchedAt < WS_HISTORY_CACHE_MS) return Promise.resolve(entry.data);
    const generation = pageGeneration;
    const pending = {};
    const url = '/api/perf/ws-clients' + (range === '24h' ? '' : '?range=' + range);
    pending.promise = fetch(url)
      // #433 F2: /api/* 404s and 500s still return a JSON body, so r.json()
      // would resolve and renderWSClientHistory would see a shapeless object
      // and show "Collecting data…" forever. Check r.ok first so a bad
      // response hides the card (renderWSClientHistory(null) === '') instead.
      .then(function (r) { return r.ok ? r.json() : null; })
      .catch(function () { return null; })
      .then(function (data) {
        if (generation === pageGeneration && wsHistoryCache.get(range) === pending) {
          if (data) wsHistoryCache.set(range, { data: data, fetchedAt: Date.now() });
          else wsHistoryCache.delete(range);
        }
        return data;
      });
    wsHistoryCache.set(range, pending);
    return pending.promise;
  }

  function rangeFromURL() {
    const hash = window.location ? String(window.location.hash || '') : '';
    const query = hash.indexOf('?');
    return normalizeWSHistoryRange(query < 0 ? null : new URLSearchParams(hash.slice(query + 1)).get('wsRange'));
  }

  function storeRangeInURL(range) {
    if (!window.location || !window.history || !window.history.replaceState) return;
    const hash = String(window.location.hash || '#/perf');
    const query = hash.indexOf('?');
    const params = new URLSearchParams(query < 0 ? '' : hash.slice(query + 1));
    params.set('wsRange', range);
    // replaceState avoids remounting the page or adding a history entry for
    // each selection, but leaves the state shareable and reloadable.
    window.history.replaceState(null, '', (query < 0 ? hash : hash.slice(0, query)) + '?' + params.toString());
  }

  // #392: perf values (endpoint keys, raw slow-query paths) are interpolated
  // into innerHTML below, so they must be HTML-escaped. Prefer app.js's
  // canonical global escapeHtml (loaded before perf.js in index.html); fall
  // back to an identical 5-char OWASP escaper so perf.js is also safe when
  // evaluated standalone (e.g. the perf-render unit-test sandboxes). The RHS
  // reads globalThis.escapeHtml, never the bare name, to avoid a TDZ
  // self-reference on this very binding.
  const escapeHtml = (typeof globalThis !== 'undefined' && typeof globalThis.escapeHtml === 'function')
    ? globalThis.escapeHtml
    : function (s) {
        if (s == null) return '';
        return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;').replace(/'/g, '&#39;');
      };

  async function render(app) {
    // The selector stays outside perfContent's existing five-second DOM
    // rebuild, preserving keyboard focus and an open native select menu.
    app.innerHTML = '<div id="perfWrapper" style="padding:16px 24px;"><h2><svg class="ph-icon" aria-hidden="true"><use href="/icons/phosphor-sprite.svg#ph-lightning"/></svg> Performance Dashboard</h2>' +
      '<section id="wsHistorySection" aria-labelledby="wsHistoryHeading"><div style="display:flex;align-items:center;justify-content:space-between;gap:12px;flex-wrap:wrap;">' +
      '<h3 id="wsHistoryHeading">WebSocket clients</h3><label for="wsHistoryRange">Period <select id="wsHistoryRange" style="min-height:44px;padding:6px;">' +
      '<option value="24h">24 hours</option><option value="7d">7 days</option><option value="30d">30 days</option></select></label></div>' +
      '<div id="wsHistoryContent">Loading history…</div></section><div id="perfContent">Loading...</div></div>';
    const selector = document.getElementById('wsHistoryRange');
    if (selector) {
      selector.value = wsHistoryRange;
      selector.addEventListener('change', function () {
        wsHistoryRange = normalizeWSHistoryRange(selector.value);
        selector.value = wsHistoryRange;
        storeRangeInURL(wsHistoryRange);
        const historyEl = document.getElementById('wsHistoryContent');
        if (historyEl) historyEl.textContent = 'Loading history…';
        return refresh();
      });
    }
    await refresh();
  }

  async function refresh() {
    const el = document.getElementById('perfContent');
    if (!el) return;
    const generation = pageGeneration;
    const request = ++refreshGeneration;
    const range = wsHistoryRange;
    try {
      // #1258: /api/health was awaited AFTER Promise.all, adding a full RTT
      // (~50-200ms) on every 5s refresh. Issue it in parallel with the rest.
      const [server, client, ioStats, sqliteStats, writeSources, health, wsHistory] = await Promise.all([
        fetch('/api/perf').then(r => r.json()),
        Promise.resolve(window.apiPerf ? window.apiPerf() : null),
        fetch('/api/perf/io').then(r => r.json()).catch(() => null),
        fetch('/api/perf/sqlite').then(r => r.json()).catch(() => null),
        fetch('/api/perf/write-sources').then(r => r.json()).catch(() => null),
        fetch('/api/health').then(r => r.json()).catch(() => null),
        fetchWSHistory(range)
      ]);
      // A slower old range / old page visit must never overwrite the current
      // chart, even when its network request could not be cancelled.
      if (!pageActive || generation !== pageGeneration || request !== refreshGeneration || range !== wsHistoryRange) return;

      const historyEl = document.getElementById('wsHistoryContent');
      if (historyEl) historyEl.innerHTML = renderWSClientHistory(wsHistory, health && health.websocket ? health.websocket.clients : null, range, false) ||
        '<p class="text-muted">History is temporarily unavailable. Retrying on the next refresh.</p>';

      let html = '';

      // Server overview
      html += `<div style="display:flex;gap:16px;flex-wrap:wrap;margin:16px 0;">
        <div class="perf-card"><div class="perf-num">${escapeHtml(server.totalRequests)}</div><div class="perf-label">Total Requests</div></div>
        <div class="perf-card"><div class="perf-num">${escapeHtml(server.avgMs)}ms</div><div class="perf-label">Avg Response</div></div>
        <div class="perf-card"><div class="perf-num">${escapeHtml(health ? health.uptimeHuman : Math.round(server.uptime / 60) + 'm')}</div><div class="perf-label">Uptime</div></div>
        <div class="perf-card"><div class="perf-num">${server.slowQueries.length}</div><div class="perf-label">Slow (&gt;100ms)</div></div>
        ${renderVersionCard(health)}
      </div>`;

      // System health (memory, event loop / go runtime, WS)
      if (health) {
        const isGo = health.engine === 'go';
        if (isGo && server.goRuntime) {
          const gr = server.goRuntime;
          const gcColor = gr.lastPauseMs > 5 ? 'var(--status-red)' : gr.lastPauseMs > 1 ? 'var(--status-yellow)' : 'var(--status-green)';
          html += `<h3><svg class="ph-icon" aria-hidden="true"><use href="/icons/phosphor-sprite.svg#ph-wrench"/></svg> Go Runtime</h3><div style="display:flex;gap:16px;flex-wrap:wrap;margin:8px 0;">
            <div class="perf-card"><div class="perf-num">${escapeHtml(gr.goroutines)}</div><div class="perf-label">Goroutines</div></div>
            <div class="perf-card"><div class="perf-num">${escapeHtml(gr.numGC)}</div><div class="perf-label">GC Collections</div></div>
            <div class="perf-card"><div class="perf-num" style="color:${gcColor}">${(+gr.pauseTotalMs).toFixed(1)}ms</div><div class="perf-label">GC Pause Total</div></div>
            <div class="perf-card"><div class="perf-num">${(+gr.lastPauseMs).toFixed(1)}ms</div><div class="perf-label">Last GC Pause</div></div>
            <div class="perf-card"><div class="perf-num">${(+gr.heapAllocMB).toFixed(1)}MB</div><div class="perf-label">Heap Alloc</div></div>
            <div class="perf-card"><div class="perf-num">${(+gr.heapSysMB).toFixed(1)}MB</div><div class="perf-label">Heap Sys</div></div>
            <div class="perf-card"><div class="perf-num">${(+gr.heapInuseMB).toFixed(1)}MB</div><div class="perf-label">Heap Inuse</div></div>
            <div class="perf-card"><div class="perf-num">${(+gr.heapIdleMB).toFixed(1)}MB</div><div class="perf-label">Heap Idle</div></div>
            <div class="perf-card"><div class="perf-num">${escapeHtml(gr.numCPU)}</div><div class="perf-label">CPUs</div></div>
            <div class="perf-card"><div class="perf-num">${escapeHtml(health.websocket.clients)}</div><div class="perf-label">WS Clients</div></div>
          </div>`;
        } else {
          const m = health.memory, el = health.eventLoop;
          const elColor = el.p95Ms > 500 ? 'var(--status-red)' : el.p95Ms > 100 ? 'var(--status-yellow)' : 'var(--status-green)';
          const memColor = m.heapUsed > m.heapTotal * 0.85 ? 'var(--status-red)' : m.heapUsed > m.heapTotal * 0.7 ? 'var(--status-yellow)' : 'var(--status-green)';
          html += `<h3>System Health</h3><div style="display:flex;gap:16px;flex-wrap:wrap;margin:8px 0;">
            <div class="perf-card"><div class="perf-num" style="color:${memColor}">${escapeHtml(m.heapUsed)}MB</div><div class="perf-label">Heap Used / ${escapeHtml(m.heapTotal)}MB</div></div>
            <div class="perf-card"><div class="perf-num">${escapeHtml(m.rss)}MB</div><div class="perf-label">RSS</div></div>
            <div class="perf-card"><div class="perf-num" style="color:${elColor}">${escapeHtml(el.p95Ms)}ms</div><div class="perf-label">Event Loop p95</div></div>
            <div class="perf-card"><div class="perf-num">${escapeHtml(el.maxLagMs)}ms</div><div class="perf-label">EL Max Lag</div></div>
            <div class="perf-card"><div class="perf-num">${escapeHtml(el.currentLagMs)}ms</div><div class="perf-label">EL Current</div></div>
            <div class="perf-card"><div class="perf-num">${escapeHtml(health.websocket.clients)}</div><div class="perf-label">WS Clients</div></div>
          </div>`;
        }
      }

      // Disk I/O (#1120)
      if (ioStats) {
        const fmtRate = (bps) => {
          if (bps >= 1048576) return (bps / 1048576).toFixed(1) + ' MB/s';
          if (bps >= 1024) return (bps / 1024).toFixed(1) + ' KB/s';
          return Math.round(bps) + ' B/s';
        };
        const writeWarn = ioStats.writeBytesPerSec > 10 * 1048576 ? ' <svg class="ph-icon" aria-hidden="true"><use href="/icons/phosphor-sprite.svg#ph-warning"/></svg>' : '';
        const cancelled = ioStats.cancelledWriteBytesPerSec || 0;
        // Cancelled writes warn at >1 MB/s — sustained cancellation usually
        // means truncate/unlink racing with active writers (#1119-shaped bug).
        const cancelledWarn = cancelled > 1048576 ? ' <svg class="ph-icon" aria-hidden="true"><use href="/icons/phosphor-sprite.svg#ph-warning"/></svg>' : '';
        html += `<h3>Disk I/O (server process)</h3><div style="display:flex;gap:16px;flex-wrap:wrap;margin:8px 0;">
          <div class="perf-card"><div class="perf-num">${fmtRate(ioStats.readBytesPerSec || 0)}</div><div class="perf-label">Read</div></div>
          <div class="perf-card"><div class="perf-num">${fmtRate(ioStats.writeBytesPerSec || 0)}${writeWarn}</div><div class="perf-label">Write</div></div>
          <div class="perf-card"><div class="perf-num">${fmtRate(cancelled)}${cancelledWarn}</div><div class="perf-label">Cancelled Write</div></div>
          <div class="perf-card"><div class="perf-num">${Math.round(ioStats.syscallsRead || 0)}/s</div><div class="perf-label">Syscalls Read</div></div>
          <div class="perf-card"><div class="perf-num">${Math.round(ioStats.syscallsWrite || 0)}/s</div><div class="perf-label">Syscalls Write</div></div>
        </div>`;

        // Ingestor row — sourced from ingestor's own /proc/self/io snapshot
        // surfaced via the stats file (#1120: "Both ingestor and server").
        if (ioStats.ingestor) {
          const ing = ioStats.ingestor;
          const ingWriteWarn = (ing.writeBytesPerSec || 0) > 10 * 1048576 ? ' <svg class="ph-icon" aria-hidden="true"><use href="/icons/phosphor-sprite.svg#ph-warning"/></svg>' : '';
          const ingCancelled = ing.cancelledWriteBytesPerSec || 0;
          const ingCancelledWarn = ingCancelled > 1048576 ? ' <svg class="ph-icon" aria-hidden="true"><use href="/icons/phosphor-sprite.svg#ph-warning"/></svg>' : '';
          html += `<h3>Disk I/O (Ingestor process)</h3><div style="display:flex;gap:16px;flex-wrap:wrap;margin:8px 0;">
            <div class="perf-card"><div class="perf-num">${fmtRate(ing.readBytesPerSec || 0)}</div><div class="perf-label">Read</div></div>
            <div class="perf-card"><div class="perf-num">${fmtRate(ing.writeBytesPerSec || 0)}${ingWriteWarn}</div><div class="perf-label">Write</div></div>
            <div class="perf-card"><div class="perf-num">${fmtRate(ingCancelled)}${ingCancelledWarn}</div><div class="perf-label">Cancelled Write</div></div>
            <div class="perf-card"><div class="perf-num">${Math.round(ing.syscallsRead || 0)}/s</div><div class="perf-label">Syscalls Read</div></div>
            <div class="perf-card"><div class="perf-num">${Math.round(ing.syscallsWrite || 0)}/s</div><div class="perf-label">Syscalls Write</div></div>
          </div>`;
        }
      }

      // Write Sources (#1120) — per-component counters from ingestor
      if (writeSources && writeSources.sources) {
        const src = writeSources.sources;
        const keys = Object.keys(src).sort((a, b) => (src[b] || 0) - (src[a] || 0));
        html += '<h3>Write Sources</h3>';
        if (keys.length === 0) {
          html += '<p style="color:var(--text-muted)">No ingestor stats yet (waiting for /tmp/corescope-ingestor-stats.json)</p>';
        } else {
          // Anomaly detection (#1120 acceptance): flag any component whose
          // per-second write rate exceeds 10× its 5-minute rolling baseline.
          // History is stashed on window so the detector has multi-sample
          // context across the 5s refresh tick.
          if (!window._perfWriteSourcesHistory) window._perfWriteSourcesHistory = [];
          const history = window._perfWriteSourcesHistory;
          const current = { sampleAt: writeSources.sampleAt || new Date().toISOString(), sources: { ...src } };
          const anom = detectPerfAnomalies(history, current, { windowMs: 5 * 60 * 1000, factor: 10 });
          // Append current and prune anything older than 6 minutes (keeps a
          // little headroom past the 5-min window, bounded memory).
          history.push(current);
          const cutoff = Date.parse(current.sampleAt) - (6 * 60 * 1000);
          while (history.length > 1 && Date.parse(history[0].sampleAt) < cutoff) history.shift();

          html += '<div style="overflow-x:auto"><table class="perf-table"><thead><tr><th scope="col">Source</th><th scope="col">Total</th><th scope="col">Rate/s</th><th scope="col">Baseline/s</th><th scope="col">Anomaly</th></tr></thead><tbody>';
          for (const k of keys) {
            const v = src[k] || 0;
            const rate = anom.rates[k];
            const base = anom.baselineRates[k];
            const flag = anom.flags[k] ? ' <svg class="ph-icon" aria-hidden="true"><use href="/icons/phosphor-sprite.svg#ph-warning"/></svg>' : '';
            const rateStr = (rate != null && isFinite(rate)) ? rate.toFixed(2) : '—';
            const baseStr = (base != null && isFinite(base)) ? base.toFixed(2) : '—';
            html += `<tr><td><code>${escapeHtml(k)}</code></td><td>${escapeHtml(v.toLocaleString())}</td><td>${rateStr}</td><td>${baseStr}</td><td>${flag}</td></tr>`;
          }
          html += '</tbody></table></div>';
          if (writeSources.sampleAt) {
            html += `<div style="font-size:11px;color:var(--text-muted);margin-top:4px">Sampled: ${escapeHtml(writeSources.sampleAt)} · baseline window: 5 min · threshold: 10×</div>`;
          }
        }
      }

      // SQLite perf (separate from existing SQLite block — focused on WAL + cache hit) (#1120)
      if (sqliteStats) {
        const walMB = sqliteStats.walSizeMB || 0;
        const walFlag = walMB > 100 ? ' <svg class="ph-icon" aria-hidden="true"><use href="/icons/phosphor-sprite.svg#ph-warning"/></svg>' : '';
        const hitRate = (sqliteStats.cacheHitRate || 0) * 100;
        const hitFlag = hitRate > 0 && hitRate < 90 ? ' <svg class="ph-icon" aria-hidden="true"><use href="/icons/phosphor-sprite.svg#ph-warning"/></svg>' : '';
        html += `<h3>SQLite (WAL + Cache Hit)</h3><div style="display:flex;gap:16px;flex-wrap:wrap;margin:8px 0;">
          <div class="perf-card"><div class="perf-num">${walMB.toFixed(1)}MB${walFlag}</div><div class="perf-label">WAL Size</div></div>
          <div class="perf-card"><div class="perf-num">${escapeHtml((sqliteStats.pageCount || 0).toLocaleString())}</div><div class="perf-label">Page Count</div></div>
          <div class="perf-card"><div class="perf-num">${escapeHtml(sqliteStats.pageSize || 0)}</div><div class="perf-label">Page Size</div></div>
          <div class="perf-card"><div class="perf-num">${hitRate.toFixed(1)}%${hitFlag}</div><div class="perf-label">Cache Hit Rate</div></div>
        </div>`;
      }

      // Cache stats
      if (server.cache) {
        const c = server.cache;
        const clientCache = _apiCache ? _apiCache.size : 0;
        html += `<h3>Cache</h3><div style="display:flex;gap:16px;flex-wrap:wrap;margin:8px 0;">
          <div class="perf-card"><div class="perf-num">${escapeHtml(c.size)}</div><div class="perf-label">Server Entries</div></div>
          <div class="perf-card"><div class="perf-num">${escapeHtml(c.hits)}</div><div class="perf-label">Server Hits</div></div>
          <div class="perf-card"><div class="perf-num">${escapeHtml(c.misses)}</div><div class="perf-label">Server Misses</div></div>
          <div class="perf-card"><div class="perf-num" style="color:${c.hitRate > 50 ? 'var(--status-green)' : c.hitRate > 20 ? 'var(--status-yellow)' : 'var(--status-red)'}">${escapeHtml(c.hitRate)}%</div><div class="perf-label">Server Hit Rate</div></div>
          <div class="perf-card"><div class="perf-num">${escapeHtml(c.staleHits || 0)}</div><div class="perf-label">Stale Hits (SWR)</div></div>
          <div class="perf-card"><div class="perf-num">${escapeHtml(c.recomputes || 0)}</div><div class="perf-label">Recomputes</div></div>
          <div class="perf-card"><div class="perf-num">${clientCache}</div><div class="perf-label">Client Entries</div></div>
        </div>`;
        if (client) {
          html += `<div style="display:flex;gap:16px;flex-wrap:wrap;margin:8px 0;">
            <div class="perf-card"><div class="perf-num">${escapeHtml(client.cacheHits || 0)}</div><div class="perf-label">Client Hits</div></div>
            <div class="perf-card"><div class="perf-num">${escapeHtml(client.cacheMisses || 0)}</div><div class="perf-label">Client Misses</div></div>
            <div class="perf-card"><div class="perf-num" style="color:${(client.cacheHitRate||0) > 50 ? 'var(--status-green)' : 'var(--status-yellow)'}">${escapeHtml(client.cacheHitRate || 0)}%</div><div class="perf-label">Client Hit Rate</div></div>
          </div>`;
        }
      }

      // Packet Store stats
      if (server.packetStore) {
        const ps = server.packetStore;
        html += `<h3>In-Memory Packet Store</h3><div style="display:flex;gap:16px;flex-wrap:wrap;margin:8px 0;">
          <div class="perf-card"><div class="perf-num">${escapeHtml(ps.inMemory.toLocaleString())}</div><div class="perf-label">Packets in RAM</div></div>
          <div class="perf-card"><div class="perf-num">${escapeHtml(ps.trackedMB)}MB</div><div class="perf-label">Tracked Memory</div></div>
          <div class="perf-card"><div class="perf-num">${escapeHtml(ps.maxMB)}MB</div><div class="perf-label">Memory Limit</div></div>
          <div class="perf-card"><div class="perf-num">${escapeHtml(ps.estimatedMB)}MB</div><div class="perf-label">Heap (debug)</div></div>
          <div class="perf-card"><div class="perf-num">${escapeHtml(ps.queries.toLocaleString())}</div><div class="perf-label">Queries Served</div></div>
          <div class="perf-card"><div class="perf-num">${escapeHtml(ps.inserts.toLocaleString())}</div><div class="perf-label">Live Inserts</div></div>
          <div class="perf-card"><div class="perf-num">${escapeHtml(ps.evicted.toLocaleString())}</div><div class="perf-label">Evicted</div></div>
          <div class="perf-card"><div class="perf-num">${escapeHtml(ps.indexes.byHash.toLocaleString())}</div><div class="perf-label">Unique Hashes</div></div>
          <div class="perf-card"><div class="perf-num">${escapeHtml(ps.indexes.byObserver)}</div><div class="perf-label">Observers</div></div>
          <div class="perf-card"><div class="perf-num">${escapeHtml(ps.indexes.byNode.toLocaleString())}</div><div class="perf-label">Indexed Nodes</div></div>
        </div>`;
      }

      // SQLite stats
      if (server.sqlite && !server.sqlite.error) {
        const sq = server.sqlite;
        const walColor = sq.walSizeMB > 50 ? 'var(--status-red)' : sq.walSizeMB > 10 ? 'var(--status-yellow)' : 'var(--status-green)';
        const freelistColor = sq.freelistMB > 10 ? 'var(--status-yellow)' : 'var(--status-green)';
        html += `<h3>SQLite</h3><div style="display:flex;gap:16px;flex-wrap:wrap;margin:8px 0;">
          <div class="perf-card"><div class="perf-num">${escapeHtml(sq.dbSizeMB)}MB</div><div class="perf-label">DB Size</div></div>
          <div class="perf-card"><div class="perf-num" style="color:${walColor}">${escapeHtml(sq.walSizeMB)}MB</div><div class="perf-label">WAL Size</div></div>
          <div class="perf-card"><div class="perf-num" style="color:${freelistColor}">${escapeHtml(sq.freelistMB)}MB</div><div class="perf-label">Freelist</div></div>
          <div class="perf-card"><div class="perf-num">${escapeHtml((sq.rows.transmissions || 0).toLocaleString())}</div><div class="perf-label">Transmissions</div></div>
          <div class="perf-card"><div class="perf-num">${escapeHtml((sq.rows.observations || 0).toLocaleString())}</div><div class="perf-label">Observations</div></div>
          <div class="perf-card"><div class="perf-num">${escapeHtml(sq.rows.nodes || 0)}</div><div class="perf-label">Nodes</div></div>
          <div class="perf-card"><div class="perf-num">${escapeHtml(sq.rows.observers || 0)}</div><div class="perf-label">Observers</div></div>`;
        if (sq.walPages) {
          html += `<div class="perf-card"><div class="perf-num">${escapeHtml(sq.walPages.busy)}</div><div class="perf-label">WAL Busy Pages</div></div>`;
        }
        html += `</div>`;
      }

      // Server endpoints table — sort by total time (count * avg) DESC.
      // #1258: header claimed "sorted by total time" but JSON map order is
      // undefined and the frontend was not sorting. Slow endpoints could
      // appear anywhere in the table, defeating the section's whole purpose.
      const eps = Object.entries(server.endpoints).sort((a, b) => {
        const ta = (a[1].count || 0) * (a[1].avgMs || 0);
        const tb = (b[1].count || 0) * (b[1].avgMs || 0);
        return tb - ta;
      });
      if (eps.length) {
        html += '<h3>Server Endpoints (sorted by total time)</h3>';
        html += '<div style="overflow-x:auto"><table class="perf-table"><thead><tr><th scope="col">Endpoint</th><th scope="col">Count</th><th scope="col">Avg</th><th scope="col">P50</th><th scope="col">P95</th><th scope="col">Max</th><th scope="col">Total</th></tr></thead><tbody>';
        for (const [path, s] of eps) {
          const total = Math.round(s.count * s.avgMs);
          const cls = s.p95Ms > 200 ? ' class="perf-slow"' : s.p95Ms > 50 ? ' class="perf-warn"' : '';
          html += `<tr${cls}><td><code>${escapeHtml(path)}</code></td><td>${escapeHtml(s.count)}</td><td>${escapeHtml(s.avgMs)}ms</td><td>${escapeHtml(s.p50Ms)}ms</td><td>${escapeHtml(s.p95Ms)}ms</td><td>${escapeHtml(s.maxMs)}ms</td><td>${total}ms</td></tr>`;
        }
        html += '</tbody></table></div>';
      }

      // Client API calls
      if (client && client.endpoints.length) {
        html += '<h3>Client API Calls (this session)</h3>';
        html += '<div style="overflow-x:auto"><table class="perf-table"><thead><tr><th scope="col">Endpoint</th><th scope="col">Count</th><th scope="col">Avg</th><th scope="col">Max</th><th scope="col">Total</th></tr></thead><tbody>';
        for (const s of client.endpoints) {
          const cls = s.maxMs > 500 ? ' class="perf-slow"' : s.avgMs > 200 ? ' class="perf-warn"' : '';
          html += `<tr${cls}><td><code>${escapeHtml(s.path)}</code></td><td>${escapeHtml(s.count)}</td><td>${escapeHtml(s.avgMs)}ms</td><td>${escapeHtml(s.maxMs)}ms</td><td>${escapeHtml(s.totalMs)}ms</td></tr>`;
        }
        html += '</tbody></table></div>';
      }

      // Slow queries
      if (server.slowQueries.length) {
        html += '<h3>Recent Slow Queries (&gt;100ms)</h3>';
        html += '<div style="overflow-x:auto"><table class="perf-table"><thead><tr><th scope="col">Time</th><th scope="col">Path</th><th scope="col">Duration</th><th scope="col">Status</th></tr></thead><tbody>';
        for (const q of server.slowQueries.slice().reverse()) {
          html += `<tr class="perf-slow"><td>${escapeHtml(new Date(q.time).toLocaleTimeString())}</td><td><code>${escapeHtml(q.path)}</code></td><td>${escapeHtml(q.ms)}ms</td><td>${escapeHtml(q.status)}</td></tr>`;
        }
        html += '</tbody></table></div>';
      }

      html += `<div style="margin-top:16px"><button id="perfReset" style="padding:8px 16px;cursor:pointer">Reset Stats</button> <button id="perfRefresh" style="padding:8px 16px;cursor:pointer">Refresh</button></div>`;
      el.innerHTML = html;

      document.getElementById('perfReset')?.addEventListener('click', async () => {
        await fetch('/api/perf/reset', { method: 'POST' });
        if (window._apiPerf) { window._apiPerf = { calls: 0, totalMs: 0, log: [] }; }
        refresh();
      });
      document.getElementById('perfRefresh')?.addEventListener('click', refresh);
    } catch (err) {
      if (pageActive && generation === pageGeneration && request === refreshGeneration) {
        el.innerHTML = `<p style="color:red">Error: ${escapeHtml(err.message)}</p>`;
      }
    }
  }

  registerPage('perf', {
    init(app) {
      // Force a fresh ws-clients fetch on (re)entry rather than reusing a
      // cache that may be stale from a previous visit (#433 F6).
      wsHistoryCache = new Map();
      wsHistoryRange = rangeFromURL();
      pageGeneration++;
      pageActive = true;
      render(app);
      // #1258: don't burn CPU/network rebuilding the page (and its many cards
      // + 3 large tables) every 5s while the tab is hidden. Pause polling on
      // visibilitychange and resume on focus. Reduces background fetch traffic
      // to zero and prevents a returning user from seeing a 100+ms thrash as
      // a backlog of refreshes flush.
      const tick = () => {
        if (document.hidden) return;
        refresh();
      };
      interval = setInterval(tick, 5000);
      const onVis = () => {
        if (!document.hidden) refresh();
      };
      document.addEventListener('visibilitychange', onVis);
      this._onVis = onVis;
    },
    destroy() {
      pageActive = false;
      pageGeneration++;
      if (interval) { clearInterval(interval); interval = null; }
      if (this._onVis) {
        document.removeEventListener('visibilitychange', this._onVis);
        this._onVis = null;
      }
    }
  });
})();
