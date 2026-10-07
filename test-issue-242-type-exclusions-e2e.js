/* #242: real browser + deterministic capped API fixture. Go tests separately
 * verify the same exclusion-before-limit contract in memory and SQLite. */
'use strict';
const assert = require('assert');
const { chromium } = require('playwright');
const BASE = process.env.BASE_URL || 'http://localhost:13581';
if (!['localhost', '127.0.0.1', '[::1]'].includes(new URL(BASE).hostname)) throw new Error('Local test server required');
const hash = i => i.toString(16).padStart(16, '0');
const now = Date.now();
const packet = (id, type) => ({ id, hash: hash(id), payload_type: type, timestamp: new Date(now - id).toISOString(), latest: new Date(now - id).toISOString(), path_json: '[]', decoded_json: '{}', observer_id: 'fixture', count: 1, observation_count: 1, payload_length: 10 });
const fixture = [...Array.from({length: 1000}, (_, i) => packet(i + 1, 11)), packet(1001, 5), packet(1002, 4), packet(1003, 5)];

(async () => {
  const browser = await chromium.launch({ headless: true, executablePath: process.env.CHROMIUM_PATH || undefined });
  try {
    // 1024px selects the real 1000-row mobile cap, without replacing production JS.
    const context = await browser.newContext({ viewport: {width: 1024, height: 900} });
    const page = await context.newPage();
    const errors = [];
    page.on('pageerror', error => errors.push(error.message));
    let requests = [], holdNext = false, release, held, socket;
    await page.routeWebSocket('**', ws => { socket = ws; });
    async function waitForRelease() {
      const deadline = Date.now() + 15000;
      while (!release) {
        if (Date.now() > deadline) throw new Error('Expected held packet request');
        await new Promise(r => setTimeout(r, 10));
      }
    }
    await page.route('**/api/packets?**', async route => {
      const q = new URL(route.request().url()).searchParams;
      requests.push(q);
      let selected = fixture;
      if (q.has('hash')) selected = selected.filter(p => p.hash === q.get('hash'));
      const excluded = new Set((q.get('excludeTypes') || '').split(',').filter(Boolean).map(Number));
      selected = selected.filter(p => !excluded.has(p.payload_type)).slice(0, Number(q.get('limit')));
      if (holdNext) {
        holdNext = false;
        held = new Promise(resolve => { release = resolve; });
        await held;
      }
      await route.fulfill({json: {packets: selected, total: selected.length}});
    });
    const ready = () => page.waitForSelector('#pktLeft[data-loaded="true"]');
    const shown = () => page.locator('#pktBody tr[data-hash]').evaluateAll(rows => rows.map(r => r.dataset.hash));
    async function checkHide(value) {
      if (!(await page.locator('#fHideControl').isVisible())) await page.locator('#filterToggleBtn, .filter-toggle-btn-mirror').filter({visible: true}).first().click();
      await page.locator('#fHideControl').setChecked(value);
    }
    async function type(value) {
      await page.locator('#typeFilterWrap .multi-select-trigger').click();
      await page.locator('[data-type-id="' + value + '"]').check();
      await page.locator('#typeFilterWrap .multi-select-trigger').click();
    }
    await page.goto(BASE + '/#/packets');
    await ready();
    assert.strictEqual(requests.at(-1).get('limit'), '1000');
    assert((await shown()).every(h => Number.parseInt(h, 16) <= 1000));
    holdNext = true;
    await checkHide(true);
    await waitForRelease();
    // #211's hint survives only until the refetch: the loaded page was all CONTROL.
    // textContent: column hiding may hide the message cell at this width.
    assert.match(await page.locator('#pktBody').textContent(), /CONTROL packets are hidden/);
    const firstRelease = release;
    release = null;
    firstRelease();
    await ready();
    assert.strictEqual(requests.at(-1).get('excludeTypes'), '11');
    assert.deepStrictEqual(await shown(), [hash(1001), hash(1002), hash(1003)]);
    console.log('PASS: exclusion fills capped grouped page with older non-CONTROL packets');
    assert(socket, 'WebSocket connected');
    await page.locator('#pktPauseBtn').click();
    // More hidden updates than the page cap must not evict matching history.
    const hiddenUpdates = Array.from({length:1001}, (_, i) => packet(i + 2001, 11));
    fixture.unshift(...hiddenUpdates); // model server persistence for subsequent uncheck
    for (const p of hiddenUpdates) socket.send(JSON.stringify({type:'packet', data:{packet:p}}));
    socket.send(JSON.stringify({type:'packet', data:{packet:packet(3002, 5)}}));
    await page.waitForFunction(() => document.querySelector('#pktPauseBtn').textContent.includes('1002'));
    await page.locator('#pktPauseBtn').click();
    await page.waitForSelector('tr[data-hash="' + hash(3002) + '"]');
    assert(!(await shown()).includes(hash(2001)), 'live CONTROL must remain hidden');
    assert((await shown()).includes(hash(1001)), 'hidden live traffic must not evict matching history');
    console.log('PASS: resumed live packets apply Hide CONTROL without evicting matching history');

    await page.locator('#fGroup').click();
    await ready();
    assert.strictEqual(requests.at(-1).get('expand'), 'observations');
    assert.deepStrictEqual(await shown(), [hash(1001), hash(1002), hash(1003)]);
    await checkHide(false);
    await ready();
    assert.strictEqual(requests.at(-1).get('excludeTypes'), null);
    assert((await shown()).every(h => Number.parseInt(h, 16) >= 2001));
    assert((await shown()).includes(hash(2001)), 'unchecking refetches CONTROL received while hidden');
    console.log('PASS: raw mode and unchecking refill the matching page');

    await type('5');
    await ready();
    assert.deepStrictEqual(await shown(), [hash(1001), hash(1003)]);
    await page.locator('#clearFiltersBtn').click();
    await ready();
    assert.strictEqual(requests.at(-1).get('excludeTypes'), null);
    assert((await shown()).every(h => Number.parseInt(h, 16) >= 2001));
    console.log('PASS: type selection and Clear refetch, without altering URL type contract');

    holdNext = true;
    await checkHide(true);
    await page.waitForFunction(() => document.querySelector('#pktLeft').dataset.loaded === 'false');
    await waitForRelease();
    // The server ages 1003 out after answering the held request, so the late
    // response still carries it and stays visibly stale after client filters.
    const agedOut = fixture.splice(fixture.findIndex(p => p.id === 1003), 1);
    await type('5');
    await ready();
    assert.deepStrictEqual(await shown(), [hash(1001)]);
    const oldRelease = release;
    release = null;
    oldRelease();
    await page.waitForTimeout(200);
    assert.deepStrictEqual(await shown(), [hash(1001)], 'a late earlier response replaced the newest results');
    fixture.push(...agedOut);
    console.log('PASS: late earlier filter response cannot replace newest results');
    socket.send(JSON.stringify({type:'packet', data:{packet:packet(2003, 4)}}));
    socket.send(JSON.stringify({type:'packet', data:{packet:packet(2004, 5)}}));
    await page.waitForSelector('tr[data-hash="' + hash(2004) + '"]');
    assert(!(await shown()).includes(hash(2003)), 'live packets must respect selected type');
    console.log('PASS: live packets keep applying the selected type');

    // Hash lookup deliberately ignores both active exclusions.
    await page.locator('#fHash').fill(hash(1));
    await page.waitForFunction(() => document.querySelector('#pktBody tr[data-hash]')?.dataset.hash === '0000000000000001');
    await ready();
    assert.strictEqual(requests.at(-1).get('excludeTypes'), null);
    assert.deepStrictEqual(await shown(), [hash(1)]);
    console.log('PASS: pinned CONTROL hash bypasses active type and Hide CONTROL filters');

    holdNext = true;
    await page.locator('#fHash').fill(hash(2));
    await waitForRelease();
    await page.evaluate(() => { location.hash = '#/home'; });
    await page.waitForSelector('#pktLeft', {state:'detached'});
    await page.evaluate(() => { location.hash = '#/packets?hash=0000000000000003'; });
    await ready();
    assert.deepStrictEqual(await shown(), [hash(3)]);
    release();
    await page.waitForTimeout(200);
    assert.deepStrictEqual(await shown(), [hash(3)]);
    assert.deepStrictEqual(errors, []);
    console.log('PASS: departed view request cannot overwrite remounted packet view');

    // A shared URL may name a time window the dropdown carries no option for
    // (?timeWindow=240), which leaves the select blank. The refetches added
    // here must keep that window; reading the blank value as Number('')===0
    // would silently widen every refetch to All time.
    const customCtx = await browser.newContext({ viewport: {width: 1024, height: 900} });
    const customPage = await customCtx.newPage();
    const customErrors = [];
    customPage.on('pageerror', error => customErrors.push(error.message));
    const windows = [];
    await customPage.routeWebSocket('**', () => {});
    await customPage.route('**/api/packets?**', async route => {
      const q = new URL(route.request().url()).searchParams;
      const since = q.get('since');
      windows.push(since === null ? null : Math.round((Date.now() - Date.parse(since)) / 60000));
      const excluded = new Set((q.get('excludeTypes') || '').split(',').filter(Boolean).map(Number));
      const selected = fixture.filter(p => !excluded.has(p.payload_type)).slice(0, Number(q.get('limit')));
      await route.fulfill({json: {packets: selected, total: selected.length}});
    });
    await customPage.goto(BASE + '/#/packets?timeWindow=240');
    await customPage.waitForSelector('#pktLeft[data-loaded="true"]');
    assert.strictEqual(await customPage.locator('#fTimeWindow').inputValue(), '',
      'a 240-minute window is expected to have no dropdown option');
    assert(windows.at(-1) >= 235 && windows.at(-1) <= 245,
      'cold load must honour the URL window, asked for ' + windows.at(-1) + ' minutes');
    const beforeRefetch = windows.length;
    if (!(await customPage.locator('#fHideControl').isVisible())) {
      await customPage.locator('#filterToggleBtn, .filter-toggle-btn-mirror').filter({visible: true}).first().click();
    }
    await customPage.locator('#fHideControl').setChecked(true);
    const refetchDeadline = Date.now() + 15000;
    while (windows.length === beforeRefetch) {
      if (Date.now() > refetchDeadline) throw new Error('Hide CONTROL did not refetch');
      await new Promise(r => setTimeout(r, 10));
    }
    const refetched = windows.at(-1);
    assert(refetched !== null, 'the refetch dropped the time window and asked for all time');
    assert(refetched >= 235 && refetched <= 245,
      'the refetch changed the window to ' + refetched + ' minutes');
    assert.deepStrictEqual(customErrors, []);
    console.log('PASS: a URL time window absent from the dropdown survives an exclusion refetch');
    if (process.env.SCREENSHOT_PATH) await page.screenshot({path:process.env.SCREENSHOT_PATH});
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; });
