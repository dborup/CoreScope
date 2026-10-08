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

  // Deterministic adversarial API responses supplement the real fixture
  // above. Same observer/prefix, distinct canonical answers and explicit
  // holes: these used to overwrite (or borrow) prefix-cache certainty.
  const ctx = await browser.newContext({ viewport: { width: 1440, height: 900 } });
  const far = {public_key: 'efbf' + '01'.repeat(30), name: 'CANON-FAR', role: 'repeater', lat: 50.87, lon: 5.52};
  const near = {public_key: 'ef00' + '02'.repeat(30), name: 'CANON-NEAR', role: 'repeater', lat: 51.08, lon: 3.78};
  const hash = '7a'.repeat(32), now = new Date().toISOString();
  const packet = {id: 36301, hash, observer_id: 'CANON-OBS', observer_name: 'CANON-OBS', payload_type: 4,
    route_type: 1, raw_hex: '1101ef', path_json: '["ef"]', decoded_json: '{}', timestamp: now, first_seen: now,
    resolved_path: [far.public_key]};
  const observations = [
    {id: 36301, observer_id: packet.observer_id, path_json: '["ef"]', raw_hex: packet.raw_hex, timestamp: now, resolved_path: [far.public_key]},
    {id: 36302, observer_id: packet.observer_id, path_json: '["ef"]', raw_hex: packet.raw_hex, timestamp: now, resolved_path: JSON.stringify([near.public_key])},
    {id: 36303, observer_id: packet.observer_id, path_json: '["ef"]', raw_hex: packet.raw_hex, timestamp: now, resolved_path: [null]},
    {id: 36304, observer_id: packet.observer_id, path_json: '["ef"]', raw_hex: packet.raw_hex, timestamp: now},
  ];
  await ctx.route(/\/api\/nodes(?:\?|$)/, route => route.fulfill({json: {nodes: [far, near], total: 2}}));
  await ctx.route(/\/api\/observers(?:\?|$)/, route => route.fulfill({json: {observers: [{id: packet.observer_id, name: packet.observer_name, iata: 'XYZ', lat: 51.21, lon: 3.44}]}}));
  await ctx.route(/\/api\/iata-coords(?:\?|$)/, route => route.fulfill({json: {coords: {}}}));
  await ctx.route(/\/api\/packets(?:[/?]|$)/, route => {
    const url = new URL(route.request().url());
    if (url.pathname !== '/api/packets') return route.fulfill({json: {packet, observations}});
    const grouped = url.searchParams.get('groupByHash') === 'true';
    const rows = grouped ? [{...packet, latest: now, count: observations.length, observer_count: 1}]
      : [{...packet, observations}];
    return route.fulfill({json: {packets: rows, total: rows.length}});
  });
  const page = await ctx.newPage(), errors = [];
  page.on('pageerror', error => errors.push(error.message));
  try {
    await step('observation-local UI: grouped header and expanded children keep different answers and holes', async () => {
      await page.goto(LIST, {waitUntil: 'domcontentloaded'});
      const header = page.locator('#pktBody tr[data-hash="' + hash + '"]').first();
      await page.waitForFunction(() => document.querySelector('#pktBody .hop-named')?.textContent === 'CANON-FAR');
      await header.click();
      await page.waitForSelector('#pktBody tr.group-child[data-id="36304"]');
      for (const [id, name, warns] of [[36301, 'CANON-FAR', 0], [36302, 'CANON-NEAR', 0], [36303, 'CANON-NEAR', 1], [36304, 'CANON-NEAR', 1]]) {
        const path = page.locator('#pktBody tr.group-child[data-id="' + id + '"] .path-hops');
        assert((await path.textContent()).includes(name), id + ': wrong child name');
        assert(await path.locator('.hop-path-warn').count() === warns, id + ': wrong child certainty');
      }
      assert((await header.locator('.path-hops').textContent()).includes('CANON-FAR'), 'children overwrote group header');
    });
    await step('observation-local UI: changing observation sort keeps header name/certainty aligned', async () => {
      observations[1].timestamp = new Date(Date.parse(now) - 60000).toISOString();
      await page.reload({waitUntil: 'domcontentloaded'});
      const header = page.locator('#pktBody tr.group-header[data-hash="' + hash + '"]');
      await page.waitForFunction(() => document.querySelector('#pktBody .hop-named')?.textContent === 'CANON-FAR');
      await header.click();
      await page.waitForSelector('#pktBody tr.group-child[data-id="36302"]');
      await page.waitForFunction(() => document.querySelector('#pktBody tr.group-header .hop-named')?.textContent === 'CANON-NEAR');
      assert(await header.locator('.hop-path-warn').count() === 0, 'sorted canonical header became uncertain');
      await page.locator('#fObsSort').selectOption('chrono-desc');
      await page.waitForFunction(() => document.querySelector('#pktBody tr.group-header .hop-named')?.textContent === 'CANON-FAR');
      await page.locator('#fObsSort').selectOption('chrono-asc');
      await page.waitForFunction(() => document.querySelector('#pktBody tr.group-header .hop-named')?.textContent === 'CANON-NEAR');
    });
    await step('observation-local UI: raw list preserves each observation rather than inheriting parent certainty', async () => {
      await page.locator('#fGroup').click();
      await page.waitForSelector('#pktBody tr[data-id="36304"]:not(.group-child)');
      for (const [id, name, warns] of [[36301, 'CANON-FAR', 0], [36302, 'CANON-NEAR', 0], [36303, 'CANON-NEAR', 1], [36304, 'CANON-NEAR', 1]]) {
        const path = page.locator('#pktBody tr[data-id="' + id + '"] .path-hops');
        assert((await path.textContent()).includes(name), id + ': wrong raw-list name');
        assert(await path.locator('.hop-path-warn').count() === warns, id + ': wrong raw-list certainty');
      }
    });
    await step('observation-local UI: selected detail and byte breakdown both use that observation', async () => {
      await page.goto(BASE + '/#/packet/' + hash + '?obs=36302', {waitUntil: 'domcontentloaded'});
      await page.waitForSelector('.detail-obs-row.observation-current[data-obs-id="36302"]');
      const path = () => page.locator('dt').filter({hasText: /^Path$/}).locator('xpath=following-sibling::dd[1]');
      assert((await path().textContent()).includes('CANON-NEAR') && await path().locator('.hop-conflict-btn').count() === 0, 'selected canonical detail differs from child');
      const byteRow = page.locator('tr').filter({hasText: /Hop 0 —/});
      assert((await byteRow.textContent()).includes('CANON-NEAR'), 'byte table reads another row canonical answer');
      await page.locator('.detail-obs-row[data-obs-id="36303"]').click();
      await page.waitForSelector('.detail-obs-row.observation-current[data-obs-id="36303"]');
      assert(await path().locator('.hop-conflict-btn').count() === 1, 'null canonical selected detail must remain ambiguous');
      assert(await byteRow.locator('.hop-conflict-btn').count() === 1, 'byte table must preserve null ambiguity too');
      await page.locator('.detail-obs-row[data-obs-id="36304"]').click();
      await page.waitForSelector('.detail-obs-row.observation-current[data-obs-id="36304"]');
      assert(await path().locator('.hop-conflict-btn').count() === 1, 'absent canonical cannot inherit parent answer');
    });
    await step('observation-local UI: WS new-group packets keep their canonical path without promoting certainty', async () => {
      await page.goto(LIST, {waitUntil: 'domcontentloaded'});
      if (!(await page.locator('#fGroup').getAttribute('class')).includes('active')) await page.locator('#fGroup').click();
      await page.waitForSelector('#pktBody tr[data-hash="' + hash + '"]');
      const live = {...packet, id: 36305, hash: '7b'.repeat(32), timestamp: new Date().toISOString()};
      // Deliver the real WS message shape through app.js's real registered
      // listener/batching path, without a live broker or copied handler code.
      await page.evaluate(p => wsListeners.slice().forEach(fn => fn({type: 'packet', data: {packet: p}})), live);
      await page.waitForFunction(h => document.querySelector('#pktBody tr[data-hash="' + h + '"] .hop-named')?.textContent === 'CANON-FAR', live.hash);
      const hole = {...live, id: 36306, hash: '7c'.repeat(32), resolved_path: [null]};
      await page.evaluate(p => wsListeners.slice().forEach(fn => fn({type: 'packet', data: {packet: p}})), hole);
      await page.waitForSelector('#pktBody tr[data-hash="' + hole.hash + '"] .hop-path-warn');
      assert((await page.locator('#pktBody tr[data-hash="' + live.hash + '"] .path-hops').textContent()).includes('CANON-FAR'), 'later WS hole overwrote definite earlier row');
    });
    await step('observation-local UI: remount and mobile selected-detail deep link keep canonical/null distinctions', async () => {
      await page.goto(BASE + '/#/packet/' + hash + '?obs=36301', {waitUntil: 'domcontentloaded'});
      await page.waitForSelector('.detail-obs-row.observation-current[data-obs-id="36301"]');
      await page.setViewportSize({width: 390, height: 844});
      await page.goto(BASE + '/#/packet/' + hash + '?obs=36304', {waitUntil: 'domcontentloaded'});
      await page.waitForSelector('.detail-obs-row.observation-current[data-obs-id="36304"]');
      const path = page.locator('dt').filter({hasText: /^Path$/}).locator('xpath=following-sibling::dd[1]');
      assert(await path.locator('.hop-conflict-btn').count() === 1, 'remounted mobile null/missing observation borrowed certainty');
      assert(!errors.length, 'browser exceptions: ' + errors.join('; '));
    });
  } finally { await ctx.close(); }

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
