#!/usr/bin/env node
/* #883 M1 perf proof: Live's per-packet render cost with kiosk on vs off.
 *
 * Feeds the fixture's packets (GET /api/packets, through Live's own
 * dbPacketToLive) into Live's WebSocket handler at 20 packets/s and times
 * each synchronous handler call (bufferPacket → renderPacketTree) with
 * performance.now(). The app socket is stubbed, so these are the only
 * packets. Three modes, all moving the camera the same way:
 *   kiosk  #/live?kiosk=1 with its tour (three stubbed areas, every=15)
 *   off    plain #/live; the same areas on the same schedule with the same
 *          flyToBounds call
 *   css    plain #/live with body.kiosk added by hand: kiosk's hidden chrome
 *          (feed, pills, VCR bar) without the controller and its tour
 * kiosk vs css is the cost of the kiosk JavaScript per packet (the gate);
 * kiosk vs off is what a viewer sees, chrome included. Runs alternate the
 * modes to spread drift.
 *
 * Usage: BASE_URL=http://localhost:13581 node scripts/bench-883-kiosk-render.js
 *        [SECONDS=30] [RUNS=2] [RATE=20]
 * Prints one JSON line per run and a summary; exits 1 when the kiosk mean
 * is more than 5 % above the css mean. One-sided: kiosk coming out faster
 * is noise in the harmless direction, not a failure.
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
if (!['localhost', '127.0.0.1', '[::1]'].includes(new URL(BASE).hostname)) throw new Error('local server only');
const SECONDS = Number(process.env.SECONDS || 30);
const RUNS = Number(process.env.RUNS || 2);
const RATE = Number(process.env.RATE || 20);
const EVERY = 15;
const AREAS = [
  { key: 'AAA', label: 'Bench A', latMin: 37.2, latMax: 37.45, lonMin: -122.05, lonMax: -121.75 },
  { key: 'BBB', label: 'Bench B', latMin: 37.70, latMax: 37.83, lonMin: -122.52, lonMax: -122.35 },
  { key: 'CCC', label: 'Bench C', latMin: 36.9, latMax: 37.1, lonMin: -122.15, lonMax: -121.9 },
];

async function run(browser, mode) {
  const kiosk = mode === 'kiosk';
  const ctx = await browser.newContext({ viewport: { width: 1920, height: 1080 } });
  await ctx.route(/\/api\/config\/areas\/polygons(\?|$)/, (r) => r.fulfill({ json: AREAS }));
  await ctx.routeWebSocket(/.*/, () => {});
  const page = await ctx.newPage();
  const hash = kiosk ? '#/live?kiosk=1&tour=AAA,BBB,CCC&every=' + EVERY : '#/live';
  await page.goto(BASE + '/?r=' + Date.now() + hash, { waitUntil: 'domcontentloaded' });
  await page.waitForFunction(() => window._liveWSHandler && window._liveWSHandler() && window._liveGetMap && window._liveGetMap(),
    null, { timeout: 30000 });
  if (kiosk) await page.waitForFunction(() => window._liveKiosk() && window._liveKiosk().stats().steps === 3, null, { timeout: 10000 });
  if (mode === 'css') {
    await page.evaluate(() => { document.body.classList.add('kiosk'); window.dispatchEvent(new Event('resize')); });
    await page.waitForTimeout(200);
  }
  const out = await page.evaluate(async ({ kiosk, seconds, rate, every, areas }) => {
    const res = await fetch('/api/packets?limit=200&expand=observations');
    const groups = (await res.json()).packets || [];
    const pkts = [];
    groups.forEach((g) => (g.observations && g.observations.length ? g.observations : [g]).forEach((o) => {
      pkts.push(window._liveDbPacketToLive(Object.assign({}, g, o, { hash: g.hash, raw_hex: g.raw_hex, decoded_json: g.decoded_json })));
    }));
    const map = window._liveGetMap();
    // "off": the same camera path as the kiosk tour, without the module.
    let cam = null, step = 0;
    if (!kiosk) {
      const fly = () => { const a = areas[step++ % areas.length]; map.flyToBounds([[a.latMin, a.lonMin], [a.latMax, a.lonMax]], { padding: [32, 32], maxZoom: 12, duration: 2.5 }); };
      fly();
      cam = setInterval(fly, every * 1000);
    }
    const handler = window._liveWSHandler();
    const times = [];
    const total = seconds * rate;
    let i = 0;
    await new Promise((done) => {
      const tick = setInterval(() => {
        const p = Object.assign({}, pkts[i % pkts.length]);
        p.hash = 'b883' + i.toString(16).padStart(12, '0');
        p.timestamp = new Date().toISOString();
        p._ts = Date.now();
        const t0 = performance.now();
        handler({ type: 'packet', data: p });
        times.push(performance.now() - t0);
        if (++i >= total) { clearInterval(tick); done(); }
      }, 1000 / rate);
    });
    if (cam) clearInterval(cam);
    times.sort((a, b) => a - b);
    const sum = times.reduce((a, b) => a + b, 0);
    const q = (f) => times[Math.min(times.length - 1, Math.floor(f * times.length))];
    const k = window._liveKiosk && window._liveKiosk();
    return { n: times.length, mean: sum / times.length, p50: q(0.5), p95: q(0.95), max: times[times.length - 1],
      kioskSteps: k ? k.stats().step : null, kioskTimers: k ? k.stats().timers : null };
  }, { kiosk, seconds: SECONDS, rate: RATE, every: EVERY, areas: AREAS });
  await ctx.close();
  return Object.assign({ mode }, out);
}

(async () => {
  const browser = await chromium.launch({ headless: true, executablePath: process.env.CHROMIUM_PATH || undefined });
  const all = { off: [], css: [], kiosk: [] };
  try {
    for (let r = 0; r < RUNS; r++) {
      for (const mode of ['off', 'css', 'kiosk']) {
        const res = await run(browser, mode);
        all[res.mode].push(res);
        console.log(JSON.stringify(Object.fromEntries(Object.entries(res).map(([k, v]) => [k, typeof v === 'number' ? +v.toFixed(4) : v]))));
      }
    }
  } finally {
    await browser.close();
  }
  const agg = (rs) => {
    const n = rs.reduce((a, r) => a + r.n, 0);
    return { n, mean: rs.reduce((a, r) => a + r.mean * r.n, 0) / n, p50: rs.reduce((a, r) => a + r.p50, 0) / rs.length,
      p95: rs.reduce((a, r) => a + r.p95, 0) / rs.length };
  };
  const off = agg(all.off), css = agg(all.css), on = agg(all.kiosk);
  const pct = (a, b) => +((a.mean - b.mean) / b.mean * 100).toFixed(2);
  const fmt = (a) => ({ n: a.n, meanMs: +a.mean.toFixed(4), p50Ms: +a.p50.toFixed(4), p95Ms: +a.p95.toFixed(4) });
  const gate = pct(on, css);
  console.log('SUMMARY ' + JSON.stringify({ rate: RATE, seconds: SECONDS, runs: RUNS, off: fmt(off), css: fmt(css), kiosk: fmt(on),
    kioskVsCssPct: gate, kioskVsOffPct: pct(on, off) }));
  process.exit(gate > 5 ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
