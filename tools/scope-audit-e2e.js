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
  } finally { await page.unroute(pattern); page.off('request', statsListener); }
};
