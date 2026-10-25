/* Invoked by the existing Playwright suite against its local server. */
'use strict';
module.exports = async function scopeAuditE2E({page, BASE, assert, test}) {
  const pattern = '**/api/scope-audit*';
  const row = {publicKey: 'a'.repeat(64), name: '<img src=x onerror="window.auditXss=1">', role: 'repeater', configuredScope: '#alpha', configuredScopeAt: '2026-01-01T00:00:00Z', declaredScopes: ['alpha', 'quiet'], allowsUnscoped: false, declarationAgeSeconds: '7200', staleDeclaration: false, observedScopes: [{name: 'alpha', count: '3'}], unscopedObserved: '1', unknownScopeObserved: '2', forwarded: '6', notObserved: ['quiet'], undeclaredObserved: [], wildcardContradiction: true, incompleteEvidence: true, status: 'incomplete'};
  let mode = 'rows', calls = [], statsWindows = [];
  const statsListener = request => {
    const url = new URL(request.url());
    if (url.pathname === '/api/scope-stats') statsWindows.push(url.searchParams.get('window'));
  };
  page.on('request', statsListener);
  await page.route(pattern, async route => {
    calls.push(new URL(route.request().url()).searchParams.get('window'));
    if (mode === 'error') return route.fulfill({status: 503, contentType: 'application/json', body: JSON.stringify({error: 'Audit unavailable'})});
    return route.fulfill({status: 200, contentType: 'application/json', body: JSON.stringify({window: calls.at(-1), rows: mode === 'empty' ? [] : [row], summary: {total: mode === 'empty' ? 0 : 1, incomplete: 1}, ambiguousHops: 2})});
  });
  try {
    await test('Scope Audit: lazy tab, window, local search, stable refresh and deep link', async () => {
      await page.goto(`${BASE}/#/analytics?tab=scopes&sub=overview&swin=24h`, {waitUntil: 'domcontentloaded'});
      await page.reload({waitUntil: 'domcontentloaded'});
      await page.waitForSelector('[data-subtab="audit"]');
      assert(calls.length === 0, 'Audit should not fetch while Overview is selected');
      await page.click('[data-subtab="audit"]');
      await page.waitForSelector('#scope-audit-rows tr');
      assert(calls.join(',') === '24h', 'Selecting Audit should issue one bulk request');
      assert(await page.locator('#scope-audit-rows').innerText().then(t => t.includes('Findings · incomplete evidence')), 'Unknown evidence must not be consistent');
      assert(await page.locator('#scope-audit-rows img').count() === 0, 'Node names must remain text');
      assert(await page.evaluate(() => !window.auditXss), 'XSS payload must not execute');
      const layout = await page.evaluate(() => {
        const table = document.querySelector('#scope-audit-table');
        const cells = Array.from(table.querySelectorAll('tbody td'));
        return {wrapped: cells.every(cell => getComputedStyle(cell).whiteSpace === 'normal'),
          contained: cells.every(cell => cell.scrollWidth <= cell.clientWidth + 1),
          resized: table.querySelectorAll('.col-resize-handle').length};
      });
      assert(layout.wrapped && layout.contained, 'Audit long labels must wrap within their columns');
      assert(layout.resized === 0, 'Audit table must avoid empty-table generic resizing');
      await page.evaluate(() => { window.auditStableRow = document.querySelector('#scope-audit-rows tr'); });
      await page.locator('#scopes-panel-audit').getByRole('button', {name: 'Refresh', exact: true}).click();
      await page.waitForFunction(() => !document.querySelector('#scope-audit-message').textContent.includes('Loading'));
      assert(await page.evaluate(() => window.auditStableRow === document.querySelector('#scope-audit-rows tr')), 'Refresh must retain unchanged DOM rows');
      await page.locator('#scopes-panel-audit [data-win="7d"]').click();
      await page.waitForFunction(() => document.querySelector('#scope-audit-message').textContent.includes('7d'));
      assert(calls.at(-1) === '7d', 'Window should reach endpoint');
      assert(page.url().includes('swin=7d'), 'Window must be deep linked');
      await page.click('[data-subtab="overview"]');
      await page.waitForFunction(() => !document.querySelector('#scopes-loading') || document.querySelector('#scopes-loading').style.display === 'none');
      assert(statsWindows.includes('7d'), 'Returning to Overview must load the Audit-selected window');
      await page.click('[data-subtab="audit"]');
      await page.waitForSelector('#scope-audit-rows tr');
      if (process.env.SCOPE_AUDIT_SCREENSHOT) await page.screenshot({path: process.env.SCOPE_AUDIT_SCREENSHOT, fullPage: true});
      const count = calls.length;
      await page.fill('#scope-audit-search', 'quiet');
      await page.waitForFunction(() => location.hash.includes('saq=quiet'));
      assert(calls.length === count, 'Search must be local');
      await page.reload({waitUntil: 'domcontentloaded'});
      await page.waitForSelector('#scope-audit-rows tr');
      assert(await page.inputValue('#scope-audit-search') === 'quiet', 'Search deep link must restore');
      await page.evaluate(async () => {
        history.replaceState(history.state, '', '#/analytics?tab=scopes&sub=audit&swin=1h&saq=alpha');
        await window._analyticsRenderScopesTab(document.querySelector('#analyticsContent'));
      });
      await page.waitForFunction(() => document.querySelector('#scope-audit-message').textContent.includes('1h'));
      assert(await page.inputValue('#scope-audit-search') === 'alpha', 'Retained DOM must restore the current hash search');
      assert(calls.at(-1) === '1h', 'Retained DOM must restore the current hash window');
      await page.fill('#scope-audit-search', 'does-not-match');
      await page.waitForFunction(() => document.querySelector('#scope-audit-message').textContent.includes('No repeaters match'));
    });
    await test('Scope Audit: empty and failed responses remain actionable', async () => {
      mode = 'empty';
      await page.goto(`${BASE}/#/analytics?tab=scopes&sub=audit&swin=1h`, {waitUntil: 'domcontentloaded'});
      await page.reload({waitUntil: 'domcontentloaded'});
      await page.waitForFunction(() => document.querySelector('#scope-audit-message')?.textContent.includes('No confirmed observer declarations'));
      mode = 'error';
      await page.locator('#scopes-panel-audit').getByRole('button', {name: 'Refresh', exact: true}).click();
      await page.waitForFunction(() => document.querySelector('#scope-audit-message').textContent.includes('Failed to load scope audit'));
      mode = 'rows';
      await page.locator('#scopes-panel-audit').getByRole('button', {name: 'Refresh', exact: true}).click();
      await page.waitForSelector('#scope-audit-rows tr');
    });
    // #414 — transport-carrier view. The API smoke runs against the real fixture server.
    await test('Scope Audit transport: API answers with the region-filtered contract', async () => {
      const res = await page.request.get(`${BASE}/api/scope-audit/transport?window=7d&region=sjc`);
      assert(res.status() === 200, 'transport endpoint status ' + res.status());
      const body = await res.json();
      assert(Array.isArray(body.rows) && Array.isArray(body.scopes), 'rows and scopes are arrays');
      assert(JSON.stringify(body.region) === '["SJC"]' && typeof body.population.regionObservers === 'number', 'region is normalised and echoed');
      assert(body.summary.total === body.rows.length, 'summary counts the rows served');
      const bad = await page.request.get(`${BASE}/api/scope-audit/transport?window=2d`);
      assert(bad.status() === 400, 'invalid window rejected');
    });
    await test('Scope Audit transport: deep link, scope, region, limitations and 390px layout', async () => {
      const transportPattern = '**/api/scope-audit/transport*';
      const transportCalls = [];
      const never = {publicKey: 'b'.repeat(64), name: 'Never asked relay with a long name that must wrap', role: 'repeater', declaration: null, forwarded: 9, carrier: 0, nonCarrier: 9, unscoped: 9, unknownScope: 0, observedScopes: [], ambiguousHops: 0, ambiguousScopeHops: 0, legacyObservations: 0, evidence: 'not-seen-carrying'};
      const carrier = {publicKey: 'c'.repeat(64), name: '<img src=x onerror="window.transportXss=1">', role: 'repeater', declaration: {configuredScope: '#dk', declaredScopes: ['dk'], declaresScope: true}, forwarded: 6, carrier: 6, nonCarrier: 0, unscoped: 0, unknownScope: 0, observedScopes: [{name: 'dk', count: 6}], ambiguousHops: 2, evidence: 'carrier'};
      await page.route(transportPattern, route => {
        const url = new URL(route.request().url());
        transportCalls.push(url);
        const scope = url.searchParams.get('scope') || '';
        return route.fulfill({status: 200, contentType: 'application/json', body: JSON.stringify({window: url.searchParams.get('window'), scope, region: url.searchParams.get('region') ? [url.searchParams.get('region')] : [], minForwarded: 5, rows: [never, carrier],
          summary: {total: 2, carrier: 1, notSeenCarrying: 1, neverQueried: 1, declared: 1}, population: {transmissions: 15, observations: 20, scopeTransmissions: 6, ambiguousHops: 2, regionObservers: 1},
          coverage: {oldestObservation: '2026-01-01T00:00:00Z', complete: true}, scopes: [{name: 'dk', count: 6}, {name: 'eu', count: 1}]})});
      });
      const viewport = page.viewportSize();
      try {
        await page.setViewportSize({width: 390, height: 844});
        await page.goto(`${BASE}/#/analytics?tab=scopes&sub=audit&sav=transport&swin=24h&scope=dk&region=SJC`, {waitUntil: 'domcontentloaded'});
        await page.reload({waitUntil: 'domcontentloaded'});
        await page.waitForSelector('#scope-audit-rows tr');
        const first = transportCalls.at(-1);
        assert(first.searchParams.get('scope') === 'dk' && first.searchParams.get('region') === 'SJC', 'deep link restores scope and region: ' + first);
        const text = await page.locator('#scopes-panel-audit').innerText();
        assert(text.includes('Never asked') && text.includes('not the same as an empty declaration'), 'never asked is worded apart from empty');
        assert(text.includes('never a configuration fault') && text.includes('Not seen carrying dk'), 'limitations and evidence wording shown');
        assert(text.includes('1 observer in SJC'), 'region population shown');
        assert(await page.evaluate(() => !window.transportXss) && await page.locator('#scope-audit-rows img').count() === 0, 'names stay text');
        const layout = await page.evaluate(() => ({page: document.documentElement.scrollWidth <= window.innerWidth + 1,
          cards: getComputedStyle(document.querySelector('#scope-audit-rows td')).display === 'block',
          // Stacked cells use the card's width, not a squeezed table column.
          wide: document.querySelector('#scope-audit-rows td').getBoundingClientRect().width >= window.innerWidth * 0.75}));
        assert(layout.page && layout.cards && layout.wide, '390px layout must stack rows without page overflow: ' + JSON.stringify(layout));
        await page.fill('#scope-audit-scope', '#eu');
        await page.press('#scope-audit-scope', 'Enter');
        await page.waitForFunction(() => location.hash.includes('scope=eu'));
        assert(transportCalls.at(-1).searchParams.get('scope') === 'eu', 'scope change reaches the endpoint');
        assert(page.url().includes('sav=transport') && page.url().includes('region=SJC'), 'transport link is shareable');
        await page.locator('#scopes-panel-audit [data-audit-view="declared"]').click();
        await page.waitForFunction(() => !location.hash.includes('sav=') && !location.hash.includes('scope='));
        await page.waitForSelector('#scope-audit-rows tr');
        assert(await page.locator('#scope-audit-scope-label').isHidden(), 'scope box hidden in the declared view');
      } finally {
        if (viewport) await page.setViewportSize(viewport);
        await page.unroute(transportPattern);
      }
    });
  } finally { await page.unroute(pattern); page.off('request', statsListener); }
};
