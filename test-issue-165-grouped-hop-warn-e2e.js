/**
 * E2E (#165, PR #185 review F1): in the default grouped packets view, the
 * list's path summary (.hop-path-warn) may only count hops the server left
 * unresolved for the row's observer. Before the fix the grouped rows carried
 * no resolved_path, the client guessed every hop, and the list flagged hops
 * the server had a definite answer for (23 of 60 rows on this fixture), while
 * the flat view showed none.
 *
 * Checks, on a cold load of #/packets (grouped, 24 h window):
 * - every grouped row carries the header observation's resolved_path;
 * - every hop a row's summary counts (.hop-uncertain, as many as the
 *   summary's number) is one that observation's server resolved_path
 *   leaves null;
 * - control: with resolved_path stripped from the grouped response (the
 *   server knows nothing), the summary still appears, so the check above
 *   is not passing because the summary never renders.
 *
 * Usage: BASE_URL=http://localhost:13581 node test-issue-165-grouped-hop-warn-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const LIST = BASE + '/#/packets?timeWindow=1440';

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

// Rows render with hex prefixes first; hop names arrive from a background
// job. Wait for names, then for the warning count to settle.
async function loadSettled(page) {
  await page.goto(LIST, { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#pktBody tr[data-hash]', { timeout: 20000 });
  await page.waitForFunction(() => document.querySelector('#pktBody .hop-named'), null, { timeout: 20000 });
  let last = -1;
  for (let i = 0; i < 20; i++) {
    const n = await page.evaluate(() => document.querySelectorAll('#pktBody .hop-path-warn').length);
    if (n === last) break;
    last = n;
    await page.waitForTimeout(300);
  }
}

// Per warned row: the summary's number and the path index of every pill it
// counted (.hop-uncertain), in path order.
function warnedRows(page) {
  return page.evaluate(() => [...document.querySelectorAll('#pktBody tr[data-hash]')]
    .filter(tr => tr.querySelector('.path-hops .hop-path-warn'))
    .map(tr => {
      const pills = [...tr.querySelectorAll('.path-hops .hop')];
      return {
        hash: tr.dataset.hash,
        count: parseInt(tr.querySelector('.path-hops .hop-path-warn').textContent, 10),
        flagged: pills.map((p, i) => (p.classList.contains('hop-uncertain') ? i : -1)).filter(i => i >= 0),
      };
    }));
}

(async () => {
  const browser = await chromium.launch({
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox'],
  });

  await step('grouped rows carry the header observation\'s resolved_path', async () => {
    const res = await (await fetch(BASE + '/api/packets?groupByHash=true&limit=200&since=' +
      encodeURIComponent(new Date(Date.now() - 1440 * 60000).toISOString()))).json();
    const rows = res.packets || [];
    assert(rows.length > 20, 'fixture returned only ' + rows.length + ' grouped rows');
    let checked = 0;
    for (const g of rows) {
      const path = JSON.parse(g.path_json || '[]');
      if (!path.length) continue;
      const d = await (await fetch(BASE + '/api/packets/' + g.hash)).json();
      const obs = (d.observations || []).find(o => o.observer_id === g.observer_id && o.path_json === g.path_json);
      if (!obs || !obs.resolved_path) continue;
      checked++;
      assert(JSON.stringify(g.resolved_path) === JSON.stringify(obs.resolved_path),
        g.hash + ': grouped resolved_path ' + JSON.stringify(g.resolved_path) + ' != observation ' + JSON.stringify(obs.resolved_path));
    }
    assert(checked > 10, 'only ' + checked + ' grouped rows had a server resolved_path to compare');
  });

  await step('desktop 1440: no grouped row warns about a hop the server resolved for its observer', async () => {
    const ctx = await browser.newContext({ viewport: { width: 1440, height: 900 } });
    const page = await ctx.newPage();
    await loadSettled(page);
    const named = await page.evaluate(() => document.querySelectorAll('#pktBody .hop-named').length);
    assert(named > 20, 'only ' + named + ' named hops rendered');
    const warned = await warnedRows(page);
    const wrong = [];
    for (const w of warned) {
      if (w.flagged.length !== w.count) { wrong.push(w.hash + ' summary says ' + w.count + ' but marks ' + w.flagged.length + ' hops'); continue; }
      const g = (await (await fetch(BASE + '/api/packets?groupByHash=true&hash=' + w.hash + '&limit=1')).json()).packets[0];
      const d = await (await fetch(BASE + '/api/packets/' + w.hash)).json();
      const obs = (d.observations || []).find(o => o.observer_id === g.observer_id && o.path_json === g.path_json);
      const rp = obs && Array.isArray(obs.resolved_path) ? obs.resolved_path : [];
      const path = JSON.parse(g.path_json || '[]');
      for (const i of w.flagged) {
        if (rp[i]) wrong.push(w.hash + ' flags hop ' + i + ' (' + path[i] + '), which the server resolved to ' + rp[i].slice(0, 8));
      }
    }
    assert(!wrong.length, wrong.length + ' problems in ' + warned.length + ' warned rows: ' + wrong.slice(0, 5).join('; '));
    await ctx.close();
  });

  await step('control: with resolved_path stripped from the grouped rows, the summary still renders', async () => {
    const ctx = await browser.newContext({ viewport: { width: 1440, height: 900 } });
    await ctx.route(/\/api\/packets\?.*groupByHash=true/, async (route) => {
      const resp = await route.fetch();
      const body = await resp.json();
      for (const p of body.packets || []) delete p.resolved_path;
      await route.fulfill({ response: resp, json: body });
    });
    const page = await ctx.newPage();
    await loadSettled(page);
    const warned = await warnedRows(page);
    assert(warned.length > 0, 'no .hop-path-warn rendered even without server answers');
    await ctx.close();
  });

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
