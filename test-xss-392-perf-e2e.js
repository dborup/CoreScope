// test-xss-392-perf-e2e.js
//
// Full-page XSS regression test for issue #392 — the perf dashboard
// (public/perf.js) interpolated /api/perf values into innerHTML without
// escaping. The two request-controlled sinks are:
//   - the server-endpoints table key (unmatched /api/ routes collapse to the
//     raw path, so a slow 404 under /api/ can carry markup), and
//   - the slow-query path (raw r.URL.Path).
//
// This drives the REAL #/perf page against a running LOCAL Go server, with
// /api/perf rewritten in the browser (page.route) to a crafted payload whose
// endpoint key and slow-query path are hostile. Nothing hostile is persisted;
// the fixture DB is untouched and no payload leaves the machine. Every
// injected element would run `window.__xss392++` (and <img src=x onerror>),
// so the check is "did any handler fire / is there any injected <img> / did a
// dialog open", not a string match on the source. Reverting the escapeHtml()
// wrappers in perf.js makes the <img> live and flips this red.
//
// Usage: BASE_URL=http://localhost:13900 node test-xss-392-perf-e2e.js
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13900';

// Breaks out of a double-quoted attribute, carries a tag for the text
// context, and a trailing single quote probes single-quoted contexts.
const HOSTILE = '"><img src=x onerror="window.__xss392=(window.__xss392||0)+1">\'';

// Crafted /api/perf response: hostile endpoint key + hostile slow-query path.
// Only the fields perf.js reads unconditionally are included; the rest of the
// dashboard's blocks are `if`-guarded and simply skip.
const PERF_PAYLOAD = {
  uptime: 123,
  totalRequests: 10,
  avgMs: 5,
  endpoints: {
    [HOSTILE]: { count: 3, avgMs: 150, p50Ms: 100, p95Ms: 300, maxMs: 400 },
  },
  slowQueries: [
    { path: HOSTILE, ms: 250, time: '2020-01-01T00:00:00Z', status: 200 },
  ],
};

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

async function hostilePerfContext(browser) {
  const ctx = await browser.newContext({ viewport: { width: 1400, height: 900 } });
  // Rewrite ONLY /api/perf — the page's other perf fetches (io/sqlite/
  // write-sources/health) fall through to the local server untouched.
  await ctx.route((url) => url.pathname === '/api/perf', async (route) => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify(PERF_PAYLOAD),
    });
  });
  const page = await ctx.newPage();
  page.setDefaultTimeout(15000);
  page.on('pageerror', (e) => console.error('    [pageerror]', e.message));
  page._xssDialogs = 0;
  page.on('dialog', (d) => { page._xssDialogs++; d.dismiss().catch(() => {}); });
  return { ctx, page };
}

async function injections(page) {
  const r = await page.evaluate(() => ({
    fired: window.__xss392 || 0,
    imgs: document.querySelectorAll('img[src="x"]').length,
  }));
  r.dialogs = page._xssDialogs;
  return r;
}
function assertInert(r, label) {
  assert(r.fired === 0 && r.imgs === 0 && r.dialogs === 0,
    `${label}: ${r.imgs} injected <img>, ${r.fired} handler call(s), ${r.dialogs} dialog(s)`);
}

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #392 perf dashboard XSS E2E against ${BASE} ===`);

  await step('perf page renders hostile endpoint key + slow-query path as inert text', async () => {
    const { ctx, page } = await hostilePerfContext(browser);
    try {
      await page.goto(BASE + '/#/perf', { waitUntil: 'domcontentloaded' });
      // Wait until both tables have rendered their hostile rows.
      await page.waitForFunction(() => {
        const el = document.getElementById('perfContent');
        return el && /Server Endpoints/.test(el.textContent) && /Recent Slow Queries/.test(el.textContent);
      });
      await page.waitForTimeout(500); // let any onerror microtasks run

      assertInert(await injections(page), 'perf dashboard');

      // The hostile strings must be present as TEXT in <code> cells, proving
      // the row rendered (not merely absent) and was escaped, not dropped.
      const r = await page.evaluate((hostile) => {
        const codes = Array.from(document.querySelectorAll('#perfContent table code'))
          .map((c) => c.textContent);
        return {
          codeCells: codes.length,
          hostileAsText: codes.filter((t) => t === hostile).length,
          // No element actually carries src="x" (the injected <img> marker).
          injectedImgs: document.querySelectorAll('#perfContent img').length,
        };
      }, HOSTILE);
      assert(r.hostileAsText >= 2,
        `expected the hostile key AND slow-query path as escaped text, found ${r.hostileAsText} (of ${r.codeCells} code cells)`);
      assert(r.injectedImgs === 0, `found ${r.injectedImgs} <img> in perf content`);
    } finally {
      await ctx.close();
    }
  });

  await browser.close();
  console.log(`\n#392 perf E2E: ${passed} passed, ${failed} failed`);
  if (failed > 0) process.exit(1);
})().catch((e) => { console.error(e); process.exit(1); });
