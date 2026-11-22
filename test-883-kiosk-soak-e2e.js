/**
 * Soak: Live kiosk mode with bounded memory (#883 M3).
 *
 * Runs a touring kiosk for SOAK_MINUTES (default 5, the CI run; locally
 * SOAK_MINUTES=240 for the 4 h perf proof) in real time under a load far
 * above a real mesh, and samples the page every 30 s after a forced GC:
 *
 *   load      the app socket is page.routeWebSocket: packet frames at
 *             20/s (fixture packets in the server's broadcast shape, fresh
 *             hashes), a moment frame every 30 s, and a forced socket close
 *             every 2 min (reconnect, re-seed); every=15 over three stubbed
 *             areas; the in-place remount shortened to 2 min through
 *             LiveKiosk._setRemountOverrideMs (test-only)
 *   sampling  CDP Performance.getMetrics (JSHeapUsedSize, Nodes,
 *             JSEventListeners), the Leaflet layer count, LiveMoments and
 *             LiveKiosk controller counts, the kiosk timers, and the app
 *             socket's listener count (app.js wsListeners)
 *
 * The heap, DOM and listener counts swing with the traffic (paths live
 * 30 s, the feed and the cards churn), so the leak criteria compare two
 * quiet samples: the load paused for QUIET_MS (past the 30 s path TTL),
 * once after the 60 s warm-up (the baseline) and once at the end, both on
 * the same tour step and SETTLE_MS after it (each area shows a different
 * number of map tiles, and every tile <img> carries four listeners).
 *
 * Pass criteria, quiet end against quiet baseline:
 *   heap ≤ base × 1.15 + 2 MB; DOM Nodes ≤ base × 1.10; JSEventListeners
 *   ≤ base + 10; layers ≤ base + 80; and over the run: the heap slope of
 *   the second half below SOAK_SLOPE_MB_PER_10MIN (least squares over the
 *   lowest sample of each remount cycle, so the 2 min sawtooth of a mount
 *   filling its bounded buffers is not read as a trend; runs with fewer than
 *   4 such points report it only); kiosk timers ≤ 4 and
 *   the socket listener count unchanged at every sample; one kiosk and one
 *   Moments controller at every sample; at least one remount, reconnect and
 *   re-seed; the step carries over each remount; fullscreen survives a
 *   remount; no page error.
 *
 * Writes a JSON report (SOAK_REPORT, default in the OS temp dir) and prints
 * the metrics table.
 *
 * Usage: BASE_URL=http://localhost:13581 [SOAK_MINUTES=240] node test-883-kiosk-soak-e2e.js
 */
'use strict';
const fs = require('fs');
const os = require('os');
const path = require('path');
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
if (!['localhost', '127.0.0.1', '[::1]'].includes(new URL(BASE).hostname)) throw new Error('local server only');
const MINUTES = Number(process.env.SOAK_MINUTES) > 0 ? Number(process.env.SOAK_MINUTES) : 5;
const REPORT = process.env.SOAK_REPORT || path.join(os.tmpdir(), 'corescope-883-soak-' + Date.now() + '.json');
const SAMPLE_MS = 30000;
const BASELINE_MS = 60000;
const PKT_PER_S = 20;
const MOMENT_EVERY_MS = 30000;
const CLOSE_EVERY_MS = 120000;
const REMOUNT_MS = 120000;
const QUIET_MS = 35000;
const SETTLE_MS = 6000;
// Least-squares heap slope over the second half. The plan's first proposal
// was 1 MB / 10 min; calibrated on the measured runs (see the PR).
const SLOPE_LIMIT = Number(process.env.SOAK_SLOPE_MB_PER_10MIN) > 0 ? Number(process.env.SOAK_SLOPE_MB_PER_10MIN)
  : (MINUTES >= 60 ? 1 / 6 : 1);   // a long run: < 1 MB / h

const AREAS = [
  { key: 'CCC', label: 'Soak Area C', latMin: 36.9, latMax: 37.1, lonMin: -122.15, lonMax: -121.9 },
  { key: 'AAA', label: 'Soak Area A', latMin: 37.2, latMax: 37.45, lonMin: -122.05, lonMax: -121.75 },
  { key: 'BBB', label: 'Soak Area B', polygon: [[37.70, -122.52], [37.83, -122.52], [37.83, -122.35], [37.70, -122.35]] },
];
const KIOSK = '#/live?kiosk=1&tour=aaa,bbb,ccc&every=15&moments=1';

function nodeMoment(id, at, pk, lat, lon) {
  return { id, kind: 'new_node', at, node: { publicKey: pk, name: 'Soak ' + id, role: 'repeater', lat, lon } };
}

// y on x (minutes) → MB per 10 min.
function slopePer10Min(points) {
  const n = points.length;
  if (n < 2) return 0;
  const mx = points.reduce((s, p) => s + p.x, 0) / n;
  const my = points.reduce((s, p) => s + p.y, 0) / n;
  let num = 0, den = 0;
  points.forEach((p) => { num += (p.x - mx) * (p.y - my); den += (p.x - mx) * (p.x - mx); });
  return den ? (num / den) * 10 : 0;
}
const MB = 1024 * 1024;
const fmtMB = (b) => (b / MB).toFixed(2);

(async () => {
  console.log('\n=== #883 M3 kiosk soak: ' + MINUTES + ' min ===');
  const browser = await chromium.launch({ args: ['--no-sandbox'] });
  const ctx = await browser.newContext({ viewport: { width: 1920, height: 1080 } });
  const errors = [];
  const counters = { sockets: 0, closes: 0, packets: 0, moments: 0, momentRequests: 0 };
  const history = [];
  let socket = null;

  await ctx.route(/\/api\/config\/areas\/polygons(\?|$)/, (route) => route.fulfill({ json: AREAS }));
  await ctx.route(/\/api\/config\/client(\?|$)/, async (route) => {
    const res = await route.fetch();
    const body = await res.json();
    body.moments = { enabled: true, historySize: 50 };
    await route.fulfill({ response: res, json: body });
  });
  await ctx.route(/\/api\/moments(\?|$)/, (route) => { counters.momentRequests++; return route.fulfill({ json: { moments: history.slice(0, 20) } }); });
  await ctx.routeWebSocket(/.*/, (ws) => { counters.sockets++; socket = ws; ws.onClose(() => { if (socket === ws) socket = null; }); });

  const page = await ctx.newPage();
  page.on('pageerror', (e) => errors.push(String(e.message)));
  await page.goto(BASE + '/' + KIOSK);
  await page.waitForFunction(() => window._liveKiosk && window._liveKiosk() && window._liveKiosk().stats().steps === 3, null, { timeout: 30000 });
  await page.evaluate((ms) => window.LiveKiosk._setRemountOverrideMs(ms), REMOUNT_MS);

  // Fullscreen from the start overlay (a real click: the API needs a gesture).
  let fullscreen = null;
  if (await page.locator('.live-kiosk-start').count()) {
    await page.click('.live-kiosk-start');
    fullscreen = await page.waitForFunction(() => !!document.fullscreenElement, null, { timeout: 5000 }).then(() => true, () => false);
  }

  // The frames: fixture packets in the broadcast shape (flat DB fields plus
  // decoded), fresh hashes per send.
  const base = await page.evaluate(async () => {
    const r = await fetch('/api/packets?limit=200&expand=observations');
    const d = await r.json();
    const out = [];
    (d.packets || []).forEach((p) => {
      const obs = (p.observations && p.observations.length) ? p.observations : [p];
      obs.slice(0, 3).forEach((o) => {
        const flat = Object.assign({}, p, o, { hash: p.hash, raw_hex: p.raw_hex, decoded_json: p.decoded_json });
        delete flat.observations;
        const live = window._liveDbPacketToLive(flat);
        out.push(Object.assign(flat, { decoded: live.decoded }));
      });
    });
    return out;
  });
  const nodes = await page.evaluate(() => {
    const d = window._liveNodeData();
    return Object.keys(d).filter((k) => d[k].lat && d[k].lon).slice(0, 50).map((k) => [k, d[k].lat, d[k].lon]);
  });
  if (!base.length || !nodes.length) throw new Error('no fixture packets or nodes');
  console.log('    frames cycled from ' + base.length + ' fixture observations; ' + (fullscreen === null ? 'no start overlay' : 'fullscreen ' + fullscreen));

  let seq = 0;
  let quiet = false;   // the load paused for a quiet sample
  const send = (frame) => { if (socket) { try { socket.send(JSON.stringify(frame)); return true; } catch (_) {} } return false; };
  const pktTimer = setInterval(() => {
    if (quiet) return;
    for (let i = 0; i < PKT_PER_S / 2; i++) {
      const b = base[seq % base.length];
      seq++;
      const hash = 'soak' + seq.toString(16).padStart(12, '0');
      const data = Object.assign({}, b, { hash, timestamp: new Date().toISOString(), first_seen: new Date().toISOString() });
      data.packet = Object.assign({}, data);
      delete data.packet.decoded;
      if (send({ type: 'packet', data })) counters.packets++;
    }
  }, 500);
  const momentTimer = setInterval(() => {
    if (quiet) return;
    const n = nodes[counters.moments % nodes.length];
    const m = nodeMoment('soak-' + counters.moments, new Date().toISOString(), n[0], n[1], n[2]);
    history.unshift(m);
    if (history.length > 50) history.length = 50;
    if (send({ type: 'moment', data: m })) counters.moments++;
  }, MOMENT_EVERY_MS);
  const closeTimer = setInterval(() => { if (socket) { counters.closes++; socket.close({ code: 1012, reason: 'soak restart' }).catch(() => {}); } }, CLOSE_EVERY_MS);

  // The step must carry over each remount: poll the step once a second.
  const carry = { checked: 0, wrong: [], last: null, remounts: 0 };
  const tour = { step: null, since: Date.now(), mountedAt: Date.now(), remountIn: Infinity };   // the step on screen and since when; the mount
  const carryTimer = setInterval(async () => {
    try {
      const s = await page.evaluate(() => ({ remounts: window.LiveKiosk.remounts(), k: window._liveKiosk() && window._liveKiosk().stats() }));
      if (!s.k || s.k.steps !== 3 || s.k.step < 0) { tour.step = null; tour.since = Date.now(); return; }   // mid-remount, or the tour not loaded yet
      if (s.k.step !== tour.step) { tour.step = s.k.step; tour.since = Date.now(); }
      if (s.remounts !== carry.remounts) tour.mountedAt = Date.now();
      tour.remountIn = s.k.remountInMs;
      if (s.remounts !== carry.remounts && carry.last) {
        // The mount that remounted was at step `last`; it handed over the
        // next one, and the new mount may have stepped once since.
        const want = (carry.last.step + 1) % 3, next = (want + 1) % 3;
        if (s.k.step !== want && s.k.step !== next) carry.wrong.push({ before: carry.last.step, after: s.k.step });
        carry.checked++;
      }
      carry.remounts = s.remounts;
      carry.last = s.k;
    } catch (_) { /* mid-remount */ }
  }, 1000);

  const cdp = await ctx.newCDPSession(page);
  await cdp.send('Performance.enable');
  let metricNames = null;
  async function sample(tMs) {
    await cdp.send('HeapProfiler.collectGarbage');
    const m = await cdp.send('Performance.getMetrics');
    if (!metricNames) metricNames = m.metrics.map((x) => x.name);
    const g = (n) => { const x = m.metrics.find((y) => y.name === n); return x ? x.value : null; };
    const p = await page.evaluate(() => {
      const map = window._liveGetMap && window._liveGetMap();
      let layers = null;
      if (map) { layers = 0; map.eachLayer(() => { layers++; }); }
      const k = window._liveKiosk && window._liveKiosk();
      const ks = k ? k.stats() : null;
      return { layers, kiosks: window.LiveKiosk.liveCount(), moments: window.LiveMoments.liveCount(),
        kioskTimers: ks ? ks.timers : null, step: ks ? ks.step : null, remounts: window.LiveKiosk.remounts(),
        wsListeners: (typeof wsListeners !== 'undefined') ? wsListeners.length : null,
        fullscreen: !!document.fullscreenElement, kiosk: document.body.classList.contains('kiosk') };
    });
    return Object.assign({ t: Math.round(tMs / 1000), heap: g('JSHeapUsedSize'), nodes: g('Nodes'), listeners: g('JSEventListeners') }, p);
  }

  const start = Date.now();
  const end = start + MINUTES * 60000;
  const samples = [];
  let next = start + SAMPLE_MS;
  let quietBase = null, quietEnd = null;
  const log = (s) => { if (process.env.SOAK_VERBOSE || MINUTES >= 30) console.log('    ' + JSON.stringify(s)); };
  // Like with like: a mount that has had traffic, then QUIET_MS with the
  // load paused, on the given step (any when null) for SETTLE_MS, and no
  // remount in between (a fresh mount has an empty feed and fewer markers):
  // the remount is held off while the sample is taken, then the 2 min
  // interval applies again.
  async function quietSample(step) {
    const MOUNT_WARM_MS = 30000;
    await page.evaluate(() => window.LiveKiosk._setRemountOverrideMs(3600000));
    try {
      return await quietOnce(step, MOUNT_WARM_MS);
    } finally {
      await page.evaluate((ms) => window.LiveKiosk._setRemountOverrideMs(ms), REMOUNT_MS);
    }
  }
  async function quietOnce(step, MOUNT_WARM_MS) {
    for (let tries = 0; tries < 6; tries++) {
      while (Date.now() - tour.mountedAt < MOUNT_WARM_MS) await page.waitForTimeout(500);
      const mount = tour.mountedAt;
      quiet = true;
      const from = Date.now();
      while (tour.mountedAt === mount && (Date.now() - from < QUIET_MS || tour.step == null || (step != null && tour.step !== step) ||
        Date.now() - tour.since < SETTLE_MS)) await page.waitForTimeout(500);
      if (tour.mountedAt === mount) {
        const s = Object.assign(await sample(Date.now() - start), { quiet: true });
        quiet = false;
        if (tour.mountedAt === mount) { log(s); return s; }
      }
      quiet = false;
    }
    throw new Error('no quiet sample without a remount in 6 tries');
  }
  try {
    for (;;) {
      const now = Date.now();
      if (!quietBase && now - start >= BASELINE_MS) { quietBase = await quietSample(null); next = Date.now() + SAMPLE_MS; continue; }
      if (now >= end) { quietEnd = await quietSample(quietBase.step); break; }
      if (now >= next) {
        const s = await sample(now - start);
        samples.push(s);
        log(s);
        next += SAMPLE_MS;
        continue;
      }
      const wake = Math.min(next, quietBase ? end : start + BASELINE_MS);
      await page.waitForTimeout(Math.max(50, Math.min(wake - Date.now(), 5000)));
    }
  } finally {
    [pktTimer, momentTimer, closeTimer, carryTimer].forEach(clearInterval);
  }

  // ── Criteria ──────────────────────────────────────────────────────────
  const baseS = quietBase;
  const last = quietEnd;
  // One point per remount cycle: its lowest heap (a fresh mount).
  const cycles = new Map();
  samples.forEach((s) => { const c = cycles.get(s.remounts); if (!c || s.heap < c.heap) cycles.set(s.remounts, s); });
  const lows = [...cycles.values()].sort((a, b) => a.t - b.t);
  const half = lows.filter((s) => s.t >= (samples[0].t + samples[samples.length - 1].t) / 2);
  const slope = slopePer10Min(half.map((s) => ({ x: s.t / 60, y: s.heap / MB })));
  const slopeMeasured = half.length >= 4;
  const wsBase = baseS.wsListeners;
  const all = samples.concat([baseS, last]);
  const results = [];
  const check = (name, ok, detail) => { results.push({ name, ok: !!ok, detail }); };
  check('quiet heap at end ≤ base × 1.15 + 2 MB', last.heap <= baseS.heap * 1.15 + 2 * MB, fmtMB(baseS.heap) + ' → ' + fmtMB(last.heap) + ' MB');
  check('heap slope (second half, per-cycle lows) < ' + SLOPE_LIMIT.toFixed(3) + ' MB / 10 min', !slopeMeasured || slope < SLOPE_LIMIT,
    slopeMeasured ? slope.toFixed(3) + ' MB / 10 min over ' + half.length + ' cycles' : 'not measured (' + half.length + ' cycles in the second half; a long run measures it)');
  check('quiet DOM Nodes ≤ base × 1.10', last.nodes <= baseS.nodes * 1.10, baseS.nodes + ' → ' + last.nodes + ' (max under load ' + Math.max(...samples.map((s) => s.nodes)) + ')');
  check('quiet JSEventListeners ≤ base + 10', last.listeners <= baseS.listeners + 10, baseS.listeners + ' → ' + last.listeners + ' (max under load ' + Math.max(...samples.map((s) => s.listeners)) + ')');
  check('quiet Leaflet layers ≤ base + 80', last.layers <= baseS.layers + 80, baseS.layers + ' → ' + last.layers + ' (max under load ' + Math.max(...samples.map((s) => s.layers || 0)) + ')');
  check('kiosk timers ≤ 4 at every sample', all.every((s) => s.kioskTimers != null && s.kioskTimers <= 4), 'max ' + Math.max(...all.map((s) => s.kioskTimers || 0)));
  check('socket listeners constant', all.every((s) => s.wsListeners === wsBase), 'wsListeners ' + [...new Set(all.map((s) => s.wsListeners))].join(','));
  check('one kiosk and one Moments controller at every sample', all.every((s) => s.kiosks === 1 && s.moments === 1),
    'kiosks ' + [...new Set(all.map((s) => s.kiosks))].join(',') + ', moments ' + [...new Set(all.map((s) => s.moments))].join(','));
  check('at least one in-place remount (every 2 min)', last.remounts >= Math.max(1, Math.floor(MINUTES / 2) - 1), last.remounts + ' remounts');
  check('the step carries over each remount', carry.checked >= 1 && !carry.wrong.length, carry.checked + ' checked, wrong ' + JSON.stringify(carry.wrong.slice(0, 5)));
  check('reconnects re-seed Moments', counters.sockets >= 2 && counters.momentRequests >= counters.sockets,
    counters.closes + ' closes, ' + counters.sockets + ' sockets, ' + counters.momentRequests + ' /api/moments');
  check('kiosk and fullscreen survive every remount', all.every((s) => s.kiosk) && (fullscreen !== true || all.every((s) => s.fullscreen)),
    'fullscreen at start ' + fullscreen + ', at end ' + last.fullscreen);
  check('no page error', !errors.length, errors.slice(0, 5).join('; '));
  check('CDP metric names (JSHeapUsedSize, Nodes, JSEventListeners)', ['JSHeapUsedSize', 'Nodes', 'JSEventListeners'].every((n) => metricNames.includes(n)), 'present');

  const report = {
    issue: 883, milestone: 'M3', startedAt: new Date(start).toISOString(), minutes: MINUTES,
    load: { packetsPerSecond: PKT_PER_S, momentEveryS: MOMENT_EVERY_MS / 1000, socketCloseEveryS: CLOSE_EVERY_MS / 1000, every: 15, remountS: REMOUNT_MS / 1000 },
    counters, carry: { checked: carry.checked, wrong: carry.wrong }, metricNames,
    baseline: baseS, end: last, heapSlopeMBPer10Min: slopeMeasured ? slope : null, slopeLimit: SLOPE_LIMIT, cycleLows: lows.map((s) => ({ t: s.t, heap: s.heap })),
    results, samples, errors,
  };
  fs.mkdirSync(path.dirname(REPORT), { recursive: true });
  fs.writeFileSync(REPORT, JSON.stringify(report, null, 1));

  console.log('\n    | Metric | Quiet baseline (' + baseS.t + ' s) | Quiet end (' + last.t + ' s) | Max under load |');
  console.log('    |---|---|---|---|');
  [['JS heap (MB)', 'heap', fmtMB], ['DOM Nodes', 'nodes', String], ['JSEventListeners', 'listeners', String], ['Leaflet layers', 'layers', String],
    ['kiosk timers', 'kioskTimers', String], ['wsListeners', 'wsListeners', String]].forEach(([label, k, f]) => {
    console.log('    | ' + label + ' | ' + f(baseS[k]) + ' | ' + f(last[k]) + ' | ' + f(Math.max(...all.map((s) => s[k] || 0))) + ' |');
  });
  console.log('    packets sent ' + counters.packets + ', moments ' + counters.moments + ', socket closes ' + counters.closes +
    ', remounts ' + last.remounts + '; report ' + REPORT + '\n');
  let failed = 0;
  results.forEach((r) => { if (r.ok) console.log('  ✓ ' + r.name + ' — ' + r.detail); else { failed++; console.error('  ✗ ' + r.name + ' — ' + r.detail); } });
  await browser.close();
  console.log('\n' + (results.length - failed) + ' passed, ' + failed + ' failed');
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
