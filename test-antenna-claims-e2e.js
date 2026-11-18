/**
 * E2E (#789 M2b): the owner antenna-height claim flow on the node page and
 * the administrator's queue, against a running server with the CI fixture.
 *
 * The CI server runs without antennaClaims (the feature is off by default),
 * so the antenna-claim routes are answered by Playwright in the shapes of
 * cmd/server/antenna_claims.go; nothing is written anywhere. The page timers
 * run on Playwright's clock so the bounded status polling can be walked
 * through without waiting for it. The poller only schedules its next check
 * once a status answer is handled, and that answer arrives on real time, so
 * the clock moves in 1 s steps and each step waits for every status fetch
 * to settle (advance()). Leaping the clock past an unsettled fetch is what
 * made this suite fail on a loaded machine (#868 review F1).
 *
 * Asserts:
 *   1. feature off (the real server): no section on a repeater's page;
 *   2. feature on: the section shows for a repeater (deep link scrolls to
 *      it) and never for a companion;
 *   3. a height off the 0.1 m grid is refused before any request; a valid
 *      one posts {heightM}, shows the code, the name check (bytes against
 *      the 23-byte budget, a name that cuts the code off) and the exact
 *      `set name` command; the link carries the request id, never the code;
 *   4. the status follows queued → pending → proven → approved, then stops
 *      polling, and offers the name back; a theme refresh, which rebuilds
 *      the node page, keeps the typed name, the code and a single poller
 *      (no two checks closer than the first backoff step, 2 s); a name
 *      longer than the budget with the code inside it says so;
 *   5. a reload of the deep link restores the claim and its code;
 *   6. "Remove my height" posts {clear:true}; 429 says try later;
 *   7. polling gives up after its attempts, with a text that fits the last
 *      status (proven: waiting for review); "Check again" resumes it;
 *   7b. leaving the node page stops the view; coming back to the claim's
 *      link starts a live one instead of a stale copy;
 *   8. admin: the key is asked for, sent as X-API-Key and kept out of the
 *      URL and storage; a name with markup stays text and a malformed key is no link; approve answered with
 *      the 409 "newer approved claim" says to reject instead; reject goes
 *      through and refreshes; revoke needs a confirm; the filter deep link
 *      survives a reload; a 401 drops the key;
 *   9. phone, 375x560, cold start, real taps: the whole owner flow and an
 *      admin decision, every control and admin link at least 44 px, no
 *      sideways scroll;
 *  10. with 11 claims in the queue, on two phones and a desktop, the result
 *      of a decision (the 409 text, "Done.") is on screen where the admin
 *      tapped, not below the whole list nor under the bottom nav when the
 *      button was tapped just above it.
 *
 * Usage:
 *   BASE_URL=http://localhost:13700 node test-antenna-claims-e2e.js
 * Optional: SCREENSHOT_DIR=<dir> writes the PR screenshots.
 */
'use strict';
const { chromium } = require('playwright');
const path = require('path');

const BASE = process.env.BASE_URL || 'http://localhost:13700';
const SHOT_DIR = process.env.SCREENSHOT_DIR || '';
const ADMIN_KEY = 'e2e-antenna-admin-key-0123456789';
const REQ_ID = 'abcdef0123456789';
const CODE = '7KX9QM';
const SECTION = '[data-testid="antenna-claim"]';

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + (e && e.stack || e)); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

function pathOf(url) { return new URL(url).pathname; }

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #789 M2b antenna height claims against ${BASE} ===\n`);

  const nodes = await (await fetch(BASE + '/api/nodes?limit=500')).json();
  const list = nodes.nodes || [];
  const repeater = list.find(n => n.role === 'repeater' && n.lat && n.lon && n.name);
  const companion = list.find(n => n.role === 'companion');
  assert(repeater && companion, 'fixture needs a positioned repeater and a companion');
  const PK = repeater.public_key;

  // newCtx answers the antenna-claim API. o.off leaves the real server's
  // config in place; o.statuses is the request status sequence (the last
  // one repeats); o.submit is the POST answer; o.admin is the admin store.
  async function newCtx(o) {
    o = o || {};
    const ctx = await browser.newContext({
      viewport: o.viewport || { width: 1280, height: 860 },
      hasTouch: !!o.touch, isMobile: !!o.touch,
    });
    const rec = { posts: [], statusCalls: 0, adminCalls: [], decisions: [] };
    if (!o.off) {
      await ctx.route(u => pathOf(u) === '/api/antenna-claims/config', r =>
        r.fulfill({ status: 200, contentType: 'application/json', body: '{"enabled":true}' }));
    }
    await ctx.route(u => /^\/api\/nodes\/[0-9a-f]{64}\/antenna-claims$/.test(pathOf(u)), async (r) => {
      const req = r.request();
      rec.posts.push(JSON.parse(req.postData() || 'null'));
      const s = o.submit || { status: 202, body: {
        requestId: REQ_ID, code: CODE, expiresAt: Date.now() + 86400000, nameBudgetBytes: 23,
        suggestedName: repeater.name.slice(0, 16).trim() + ' ' + CODE } };
      await r.fulfill({ status: s.status, contentType: 'application/json', body: JSON.stringify(s.body) });
    });
    await ctx.route(u => /^\/api\/antenna-claims\/requests\//.test(pathOf(u)), async (r) => {
      const seq = o.statuses || ['pending'];
      const st = seq[Math.min(rec.statusCalls, seq.length - 1)];
      rec.statusCalls++;
      if (st === 404) return r.fulfill({ status: 404, contentType: 'application/json', body: '{"error":"Not found"}' });
      await r.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(typeof st === 'string' ? { status: st } : st) });
    });
    await ctx.route(u => pathOf(u) === '/api/admin/antenna-claims', async (r) => {
      const key = r.request().headers()['x-api-key'];
      rec.adminCalls.push({ key, url: r.request().url() });
      if (key !== ADMIN_KEY) return r.fulfill({ status: 401, contentType: 'application/json', body: '{"error":"unauthorized"}' });
      const status = new URL(r.request().url()).searchParams.get('status');
      const claims = ((o.admin && o.admin.claims) || []).filter(c => c.status === status);
      await r.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ claims, enabled: true, autoApprove: false }) });
    });
    await ctx.route(u => /^\/api\/admin\/antenna-claims\/[0-9a-f]+\/(approve|reject|revoke)$/.test(pathOf(u)), async (r) => {
      const m = /\/([0-9a-f]+)\/(approve|reject|revoke)$/.exec(pathOf(r.request().url()));
      rec.decisions.push({ id: m[1], op: m[2], key: r.request().headers()['x-api-key'] });
      const ans = (o.admin && o.admin.decide && o.admin.decide(m[1], m[2])) || { status: 202, body: { requestId: 'fedcba9876543210' } };
      await r.fulfill({ status: ans.status, contentType: 'application/json', body: JSON.stringify(ans.body) });
    });
    await ctx.addInitScript(() => {
      // Record each status check at the page's (fake) time, and count the
      // checks whose answer the page has fully handled: the counter moves
      // in a MessageChannel task, which runs after the promise chain that
      // handles the answer (MessageChannel is not on Playwright's clock).
      window.__acPolls = [];
      window.__acIssued = 0;
      window.__acSettled = 0;
      const origFetch = window.fetch;
      window.fetch = function (input) {
        const p = origFetch.apply(this, arguments);
        if (!/\/api\/antenna-claims\/requests\//.test(String((input && input.url) || input))) return p;
        window.__acIssued++;
        window.__acPolls.push(Date.now());
        const settled = () => {
          const ch = new MessageChannel();
          ch.port1.onmessage = () => { window.__acSettled++; };
          ch.port2.postMessage(0);
        };
        return p.then((res) => {
          const text = res.text.bind(res);
          res.text = () => { const q = text(); q.then(settled, settled); return q; };
          return res;
        }, (e) => { settled(); throw e; });
      };
      try {
        localStorage.setItem('meshcore-theme', 'light');
        ['row-swipe', 'tab-swipe', 'edge-drawer', 'pull-refresh']
          .forEach(k => localStorage.setItem('meshcore-gesture-hints-' + k, 'seen'));
      } catch (_) {}
    });
    const page = await ctx.newPage();
    const errors = [];
    page.on('pageerror', e => errors.push(String(e)));
    if (o.clock) await page.clock.install();
    return { ctx, page, rec, errors };
  }

  async function openNode(page, pk, extra) {
    await page.goto(BASE + '/#/nodes/' + pk + (extra || ''), { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#nodeFullBody .node-full-card', { timeout: 15000 });
  }
  // advance moves the page clock in 1 s steps (below the 2 s first backoff)
  // and lets every status check settle before the next step.
  async function settle(page) {
    for (let i = 0; i < 400; i++) {
      if (await page.evaluate(() => window.__acIssued === window.__acSettled)) return;
      await new Promise(r => setTimeout(r, 25));
    }
    throw new Error('a status check never settled');
  }
  async function advance(page, ms) {
    for (let t = 0; t < ms; t += 1000) {
      await page.clock.runFor(Math.min(1000, ms - t));
      await settle(page);
    }
  }
  // feedback waits for a status line (role=status) whose text matches re,
  // in the claim row `rowId` if given, and says whether the admin can read
  // it: inside the viewport and the top element at its top, middle and
  // bottom, so no part of it is under the fixed top bar or bottom nav.
  async function feedback(page, re, rowId) {
    for (let i = 0; i < 300; i++) {
      const f = await page.evaluate((a) => {
        const rx = new RegExp(a.src);
        const scope = a.rowId ? document.querySelector('.ac-item[data-claim-id="' + a.rowId + '"]') : document;
        const e = scope && Array.from(scope.querySelectorAll('[role="status"]')).find(x => rx.test(x.textContent));
        if (!e) return null;
        const r = e.getBoundingClientRect();
        const x = r.left + Math.min(r.width / 2, 40);
        const hit = (y) => {
          const t = y >= 0 && y < window.innerHeight ? document.elementFromPoint(x, y) : null;
          return !!t && (t === e || e.contains(t));
        };
        return { text: e.textContent, kind: e.getAttribute('data-kind'), y: Math.round(r.top), bottom: Math.round(r.bottom),
          vh: window.innerHeight, visible: r.height > 0 && hit(r.top + 1) && hit(r.top + r.height / 2) && hit(r.bottom - 1) };
      }, { src: re.source, rowId: rowId || '' });
      if (f) return f;
      await new Promise(r => setTimeout(r, 50));
    }
    throw new Error('no status line matched ' + re);
  }
  async function pollTimes(page) { return page.evaluate(() => window.__acPolls.slice()); }
  // closestGap is the smallest time between two consecutive status checks.
  function closestGap(times) {
    let min = Infinity;
    for (let i = 1; i < times.length; i++) min = Math.min(min, times[i] - times[i - 1]);
    return min;
  }
  async function statusText(page) { return (await page.textContent('[data-testid="ac-status"]')) || ''; }
  async function waitStatus(page, re, maxMs) {
    for (let t = 0; t <= (maxMs || 60000); t += 1000) {
      if (re.test(await statusText(page))) return;
      await advance(page, 1000);
    }
    throw new Error('status never matched ' + re + ': ' + await statusText(page));
  }

  await step('feature off: no owner section on a repeater', async () => {
    const { ctx, page, rec, errors } = await newCtx({ off: true });
    await openNode(page, PK, '?section=node-antenna');
    await page.waitForTimeout(800);
    assert(await page.locator('#node-antenna').count() === 1, 'placeholder missing for a repeater');
    assert(!(await page.locator('#node-antenna').isVisible()), 'section visible with the feature off');
    assert(rec.posts.length === 0 && errors.length === 0, errors.join('; '));
    await ctx.close();
  });

  await step('feature on: the section shows for a repeater and never for a companion', async () => {
    const { ctx, page, errors } = await newCtx();
    await openNode(page, companion.public_key);
    await page.waitForTimeout(500);
    assert(await page.locator('#node-antenna').count() === 0, 'companion got an antenna section');
    await openNode(page, PK, '?section=node-antenna');
    await page.waitForSelector(SECTION + ':visible', { timeout: 15000 });
    await page.waitForTimeout(400);
    const box = await page.locator(SECTION).boundingBox();
    assert(box && box.y >= -2 && box.y < 860, 'deep link did not scroll the section into view: y=' + (box && box.y));
    assert(errors.length === 0, errors.join('; '));
    await ctx.close();
  });

  await step('owner flow: grid check, code, name check, command, status to approved', async () => {
    const { ctx, page, rec, errors } = await newCtx({ clock: true, statuses: ['queued', 'pending', 'pending', 'proven', 'approved'] });
    await openNode(page, PK, '?section=node-antenna');
    await page.waitForSelector(SECTION + ':visible', { timeout: 15000 });
    await page.fill('[data-testid="ac-height"]', '18.55');
    await page.click('[data-testid="ac-submit"]');
    const err = await page.textContent('#acFormStatus');
    assert(/0\.1 m/.test(err), 'no grid message: ' + err);
    assert(rec.posts.length === 0, 'an off-grid height was posted');
    await page.fill('[data-testid="ac-height"]', '18,5');
    await page.click('[data-testid="ac-submit"]');
    await page.waitForSelector('[data-testid="ac-code"]');
    assert(JSON.stringify(rec.posts) === '[{"heightM":18.5}]', 'posted ' + JSON.stringify(rec.posts));
    assert((await page.textContent('[data-testid="ac-code"]')) === CODE, 'code not shown');
    const url = page.url();
    assert(url.includes('section=node-antenna&claim=' + REQ_ID) && !url.includes(CODE), 'link: ' + url);
    const check = await page.textContent('[data-testid="ac-name-check"]');
    assert(/\/ 23 bytes/.test(check) && /fits/.test(check), 'name check: ' + check);
    const suggested = await page.inputValue('[data-testid="ac-name"]');
    assert((await page.textContent('[data-testid="ac-cmd"]')) === 'set name ' + suggested, 'command does not match the name');
    await page.fill('[data-testid="ac-name"]', 'A Very Long Repeater Name ' + CODE);
    const cut = await page.textContent('[data-testid="ac-name-check"]');
    assert(/too long/.test(cut) && (await page.getAttribute('[data-testid="ac-name-check"]', 'data-kind')) === 'error', 'cut name accepted: ' + cut);
    assert((await page.textContent('[data-testid="ac-cmd"]')) === 'set name A Very Long Repeater Name ' + CODE, 'command not updated');
    // Words after the code may run past the budget: the code is still on air.
    await page.fill('[data-testid="ac-name"]', 'Roof ' + CODE + ' and a few more words');
    const over = await page.textContent('[data-testid="ac-name-check"]');
    assert(/^32 bytes; only the first 23 go on air — the code is inside them\.$/.test(over) &&
      (await page.getAttribute('[data-testid="ac-name-check"]', 'data-kind')) === 'success', 'over-budget name with the code inside: ' + over);
    await page.fill('[data-testid="ac-name"]', 'A Very Long Repeater Name ' + CODE);
    await waitStatus(page, /Waiting for an advert/);
    // The node page rebuilds its sections on a theme refresh; the owner's
    // flow must survive it: same name, same code, one poller. Two pollers
    // would put checks closer together than the first backoff step (2 s).
    // A refresh while the owner types must not blur the field: that drops
    // the typing and closes a phone keyboard (#868 N9).
    await page.focus('[data-testid="ac-name"]');
    await page.evaluate(() => window.dispatchEvent(new CustomEvent('theme-refresh')));
    await page.waitForFunction(() => document.querySelectorAll('#node-antenna').length === 1 &&
      document.querySelector('#nodeFullBody #node-antenna [data-testid="ac-code"]'));
    assert((await page.inputValue('[data-testid="ac-name"]')) === 'A Very Long Repeater Name ' + CODE, 'typed name lost on a re-render');
    assert(await page.evaluate(() => document.activeElement && document.activeElement.getAttribute('data-testid') === 'ac-name'),
      'a re-render blurred the name field');
    assert((await page.textContent('[data-testid="ac-code"]')) === CODE, 'code lost on a re-render');
    await waitStatus(page, /Proven/);
    await waitStatus(page, /Approved/);
    const times = await pollTimes(page);
    assert(times.length === rec.statusCalls && closestGap(times) >= 2000,
      're-render started a second poller: checks at ' + JSON.stringify(times.map(t => t - times[0])));
    assert(!(await page.isVisible('[data-testid="ac-check-again"]')), '"Check again" shown while nothing gave up');
    const calls = rec.statusCalls;
    await advance(page, 120000);
    assert(rec.statusCalls === calls, 'kept polling after approval: ' + calls + ' → ' + rec.statusCalls);
    assert(/usual name back/.test(await page.textContent('.ac-restore')), 'no reminder to restore the name');
    if (SHOT_DIR) await page.locator(SECTION).screenshot({ path: path.join(SHOT_DIR, 'owner-flow-desktop.png') });
    assert(errors.length === 0, errors.join('; '));

    // 5. A reload of the deep link keeps the claim and its code.
    await page.reload({ waitUntil: 'domcontentloaded' });
    await page.waitForSelector('[data-testid="ac-code"]', { timeout: 15000 });
    assert((await page.textContent('[data-testid="ac-code"]')) === CODE, 'code lost on reload');
    await waitStatus(page, /Approved/);
    await ctx.close();
  });

  await step('remove my height posts {clear:true}; 429 says try later', async () => {
    const { ctx, page, rec } = await newCtx({ submit: { status: 429, body: { error: 'too many claims right now, please try again later' } } });
    await openNode(page, PK, '?section=node-antenna');
    await page.waitForSelector(SECTION + ':visible', { timeout: 15000 });
    await page.check('[data-testid="ac-mode-clear"]');
    assert(await page.isDisabled('[data-testid="ac-height"]'), 'height input still enabled for a clear');
    await page.click('[data-testid="ac-submit"]');
    await page.waitForFunction(() => /Too many claims/.test(document.getElementById('acFormStatus').textContent));
    assert(JSON.stringify(rec.posts) === '[{"clear":true}]', 'posted ' + JSON.stringify(rec.posts));
    await ctx.close();
  });

  await step('polling is bounded: it gives up with a fitting text, and "Check again" resumes', async () => {
    const { ctx, page, rec } = await newCtx({ clock: true, statuses: ['pending', 'pending', 'proven'] });
    await openNode(page, PK, '?section=node-antenna');
    await page.waitForSelector(SECTION + ':visible', { timeout: 15000 });
    await page.fill('[data-testid="ac-height"]', '12');
    await page.click('[data-testid="ac-submit"]');
    await page.waitForSelector('[data-testid="ac-code"]');
    await advance(page, 10 * 60 * 1000);
    assert(!(await page.isVisible('[data-testid="ac-check-again"]')), '"Check again" shown while still polling');
    await advance(page, 20 * 60 * 1000);
    assert(await page.isVisible('[data-testid="ac-check-again"]'), '"Check again" not shown after the attempts ran out');
    const calls = rec.statusCalls;
    assert(calls === 60, 'polled ' + calls + ' times before giving up, want 60');
    const gaveUp = await statusText(page);
    assert(/^Proven; waiting for review\. Check again later\.$/.test(gaveUp), 'give-up text after proven: ' + gaveUp);
    await advance(page, 10 * 60 * 1000);
    assert(rec.statusCalls === calls, 'polled after giving up');
    await page.click('[data-testid="ac-check-again"]');
    await advance(page, 2000);
    assert(rec.statusCalls === calls + 1, 'Check again did not poll');
    await ctx.close();
  });

  await step('leaving the node page stops the claim view; coming back starts a live one', async () => {
    const { ctx, page, rec } = await newCtx({ clock: true, statuses: ['pending', 'pending', 'pending', 'pending', 'proven'] });
    await openNode(page, PK, '?section=node-antenna');
    await page.waitForSelector(SECTION + ':visible', { timeout: 15000 });
    await page.fill('[data-testid="ac-height"]', '12');
    await page.click('[data-testid="ac-submit"]');
    await page.waitForSelector('[data-testid="ac-code"]');
    await waitStatus(page, /Waiting for an advert/);
    const claimUrl = page.url();
    await page.evaluate(() => { location.hash = '#/packets'; });
    await page.waitForFunction(() => !document.querySelector('#node-antenna'));
    const left = rec.statusCalls;
    await advance(page, 65000);
    assert(rec.statusCalls === left, 'polled ' + (rec.statusCalls - left) + ' times after leaving the node page');
    // Back on the claim's link: a live view polls again and reaches proven,
    // not the stopped view of the earlier visit.
    await page.evaluate((h) => { location.hash = h; }, new URL(claimUrl).hash);
    await page.waitForSelector('[data-testid="ac-code"]', { timeout: 15000 });
    await waitStatus(page, /Proven/, 30000);
    assert(rec.statusCalls > left, 'no check after coming back');
    await ctx.close();
  });

  const XSS_NAME = '<img src=x onerror="window.__xss=1">Roof';
  function adminStore() {
    const store = {
      claims: [
        { id: '1111111111111111', pubkey: PK, name: XSS_NAME, role: 'repeater', heightM: 18.5, status: 'proven',
          needsReview: true, createdAt: Date.now() - 3600000, expiresAt: Date.now() + 3600000, provenAt: Date.now() - 600000,
          proofHash: 'abcdef12', proofLat: 37.6, proofLon: -122.1, movedM: 0 },
        { id: '2222222222222222', pubkey: PK, name: repeater.name, role: 'repeater', heightM: 22, status: 'proven',
          createdAt: Date.now() - 7200000, expiresAt: Date.now() + 3600000, provenAt: Date.now() - 900000, proofHash: 'abcdef13' },
        { id: '4444444444444444', pubkey: 'javascript:window.__xss=2', name: '', role: 'repeater', heightM: 9, status: 'proven',
          createdAt: Date.now() - 7200000, expiresAt: Date.now() + 3600000, provenAt: Date.now() - 900000 },
        { id: '3333333333333333', pubkey: PK, name: repeater.name, role: 'repeater', heightM: null, status: 'approved',
          createdAt: Date.now() - 7200000, expiresAt: Date.now(), provenAt: Date.now() - 800000, reviewedAt: Date.now() - 800000, approvedBy: 'owner' },
      ],
    };
    store.decide = (id, op) => {
      if (op === 'approve' && id === '1111111111111111') return { status: 409, body: { error: 'a newer claim for this node is already approved' } };
      const c = store.claims.find(x => x.id === id);
      if (c) c.status = op === 'approve' ? 'approved' : op === 'reject' ? 'rejected' : 'revoked';
      return { status: 202, body: { requestId: 'fedcba9876543210' } };
    };
    return store;
  }

  await step('admin queue: key in memory, 409 newer-approved, reject, revoke confirm, filters', async () => {
    const store = adminStore();
    const { ctx, page, rec, errors } = await newCtx({ admin: store, statuses: [404] });
    await page.goto(BASE + '/#/tools/antenna-claims', { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('[data-testid="ac-admin-key"]', { timeout: 15000 });
    await page.fill('[data-testid="ac-admin-key"]', 'wrong-key-wrong-key-wrong');
    await page.press('[data-testid="ac-admin-key"]', 'Enter');
    await page.waitForFunction(() => /not accepted/.test(document.getElementById('acAdminStatus').textContent));
    await page.fill('[data-testid="ac-admin-key"]', ADMIN_KEY);
    await page.press('[data-testid="ac-admin-key"]', 'Enter');
    await page.waitForSelector('.ac-item[data-claim-id="1111111111111111"]');
    assert(rec.adminCalls.some(c => c.key === ADMIN_KEY && /status=proven/.test(c.url)), 'list not asked with the key');
    const leaked = await page.evaluate((k) => {
      const all = JSON.stringify(Object.assign({}, localStorage)) + JSON.stringify(Object.assign({}, sessionStorage)) + location.href;
      return all.includes(k);
    }, ADMIN_KEY);
    assert(!leaked, 'the admin key reached storage or the URL');
    const nameText = await page.textContent('.ac-item[data-claim-id="1111111111111111"] .ac-item-name');
    assert(nameText === XSS_NAME, 'name not shown as text: ' + nameText);
    assert(await page.evaluate(() => window.__xss === undefined && !document.querySelector('.ac-list img')), 'markup in a name ran');
    const bad = await page.evaluate(() => {
      const n = document.querySelector('.ac-item[data-claim-id="4444444444444444"] .ac-item-name');
      return n && { tag: n.tagName, href: n.getAttribute('href'), text: n.textContent };
    });
    assert(bad && bad.tag === 'SPAN' && bad.href === null && bad.text === '(unknown node)', 'a malformed key became a link: ' + JSON.stringify(bad));
    assert(/far from the surface/.test(await page.textContent('.ac-item[data-claim-id="1111111111111111"]')), 'review flag not shown');

    await page.click('.ac-item[data-claim-id="1111111111111111"] [data-decide="approve"]');
    const refused = await feedback(page, /Reject this claim instead/);
    assert(refused.kind === 'error', '409 not shown as an error: ' + JSON.stringify(refused));
    assert(store.claims[0].status === 'proven', 'refused approval changed the claim');

    await page.click('.ac-item[data-claim-id="2222222222222222"] [data-decide="reject"]');
    await page.waitForFunction(() => !document.querySelector('.ac-item[data-claim-id="2222222222222222"]'), null, { timeout: 15000 });
    assert((await feedback(page, /^Done/)).kind === 'success', 'reject not confirmed');

    await page.click('[data-filter="approved"]');
    await page.waitForSelector('.ac-item[data-claim-id="3333333333333333"]');
    assert(page.url().endsWith('#/tools/antenna-claims?filter=approved'), 'filter not in the link: ' + page.url());
    assert(/Remove the owner height/.test(await page.textContent('.ac-item[data-claim-id="3333333333333333"]')), 'clear claim not labelled');
    const revoke = '.ac-item[data-claim-id="3333333333333333"] [data-decide="revoke"]';
    await page.click(revoke);
    assert((await page.textContent(revoke)) === 'Confirm revoke' && rec.decisions.filter(d => d.op === 'revoke').length === 0, 'revoke sent without a confirm');
    await page.click(revoke);
    await page.waitForFunction(() => !document.querySelector('.ac-item[data-claim-id="3333333333333333"]'), null, { timeout: 15000 });
    assert(rec.decisions.every(d => d.key === ADMIN_KEY), 'a decision went without the key');
    if (SHOT_DIR) await page.screenshot({ path: path.join(SHOT_DIR, 'admin-desktop.png') });

    await page.reload({ waitUntil: 'domcontentloaded' });
    await page.waitForSelector('[data-testid="ac-admin-key"]', { timeout: 15000 });
    await page.fill('[data-testid="ac-admin-key"]', ADMIN_KEY);
    await page.press('[data-testid="ac-admin-key"]', 'Enter');
    await page.waitForSelector('[data-filter="approved"][aria-pressed="true"]');
    assert(errors.length === 0, errors.join('; '));
    await ctx.close();
  });

  await step('phone 375x560, cold start, real taps: owner flow and an admin decision', async () => {
    const store = adminStore();
    const { ctx, page, rec, errors } = await newCtx({ viewport: { width: 375, height: 560 }, touch: true, clock: true,
      statuses: ['queued', 'pending', 'proven'], admin: store });
    await openNode(page, PK, '?section=node-antenna');
    await page.waitForSelector(SECTION + ':visible', { timeout: 15000 });
    // Let the page's start-up timers run first: the deep link's smooth
    // scroll (300 ms) and the theme refresh would move the target under a
    // tap on a slow machine.
    await advance(page, 1000);
    // tapTarget taps the middle of a control once it has stopped moving and
    // is the element a finger there would hit.
    async function tapTarget(sel) {
      const loc = page.locator(sel).first();
      let prev = null;
      for (let i = 0; i < 50; i++) {
        await loc.scrollIntoViewIfNeeded();
        const b = await loc.boundingBox();
        assert(b && b.height >= 44 && b.x >= 0 && b.x + b.width <= 375 + 0.5, sel + ' box ' + JSON.stringify(b));
        const x = b.x + b.width / 2, y = b.y + b.height / 2;
        const still = prev && prev.x === b.x && prev.y === b.y;
        if (still && await loc.evaluate((e, p) => { const t = document.elementFromPoint(p.x, p.y); return !!t && (t === e || e.contains(t)); }, { x, y })) {
          await page.touchscreen.tap(x, y);
          return;
        }
        prev = b;
        await new Promise(r => setTimeout(r, 100));
      }
      throw new Error(sel + ' never settled under a tap');
    }
    await tapTarget('[data-testid="ac-height"]');
    await page.keyboard.type('18.5');
    assert((await page.inputValue('[data-testid="ac-height"]')) === '18.5', 'the tap did not focus the height field');
    await tapTarget('[data-testid="ac-submit"]');
    await page.waitForSelector('[data-testid="ac-code"]', { timeout: 10000 });
    assert(JSON.stringify(rec.posts) === '[{"heightM":18.5}]', 'posted ' + JSON.stringify(rec.posts));
    await tapTarget('.ac-copy');
    await tapTarget('[data-testid="ac-name"]');
    await waitStatus(page, /Proven/);
    const sideways = await page.evaluate(() => document.scrollingElement.scrollWidth - window.innerWidth);
    assert(sideways <= 0, 'the page scrolls sideways by ' + sideways + ' px');
    if (SHOT_DIR) await page.screenshot({ path: path.join(SHOT_DIR, 'owner-flow-phone.png'), fullPage: false });

    await page.goto(BASE + '/#/tools/antenna-claims', { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('[data-testid="ac-admin-key"]', { timeout: 15000 });
    await tapTarget('[data-testid="ac-admin-key"]');
    await page.keyboard.type(ADMIN_KEY);
    await page.keyboard.press('Enter');
    await page.waitForSelector('.ac-item[data-claim-id="2222222222222222"]');
    // The node and proof links in a row are touch targets too.
    for (const link of ['.ac-item-name', '.ac-item-proof']) {
      const loc = page.locator('.ac-item[data-claim-id="2222222222222222"] ' + link);
      await loc.scrollIntoViewIfNeeded();
      const b = await loc.boundingBox();
      assert(b && b.height >= 44 && b.x >= 0 && b.x + b.width <= 375 + 0.5, link + ' box ' + JSON.stringify(b));
    }
    await tapTarget('.ac-item[data-claim-id="2222222222222222"] [data-decide="approve"]');
    for (let i = 0; i < 20 && !rec.decisions.length; i++) await advance(page, 500);
    assert(rec.decisions.length === 1 && rec.decisions[0].op === 'approve', 'tap did not approve: ' + JSON.stringify(rec.decisions));
    const sideways2 = await page.evaluate(() => document.scrollingElement.scrollWidth - window.innerWidth);
    assert(sideways2 <= 0, 'the admin page scrolls sideways by ' + sideways2 + ' px');
    if (SHOT_DIR) await page.screenshot({ path: path.join(SHOT_DIR, 'admin-phone.png'), fullPage: false });
    await tapTarget('.ac-item[data-claim-id="1111111111111111"] .ac-item-proof');
    await page.waitForFunction(() => location.hash === '#/packets/abcdef12');
    assert(errors.length === 0, errors.join('; '));
    await ctx.close();
  });

  // F2 of the #868 review: with a realistic queue, a line under the list is
  // far below the fold, so a decision's result must show where the admin is.
  function longQueue() {
    const claims = [];
    for (let i = 0; i < 11; i++) {
      claims.push({ id: 'c0000000000000' + String(i).padStart(2, '0'), pubkey: PK, name: repeater.name + ' ' + i, role: 'repeater',
        heightM: 10 + i, status: 'proven', createdAt: Date.now() - 7200000, expiresAt: Date.now() + 3600000,
        provenAt: Date.now() - 900000, proofHash: 'abcdef' + String(i).padStart(2, '0') });
    }
    const store = { claims };
    store.decide = (id, op) => {
      if (op === 'approve' && (id === claims[0].id || id === claims[1].id)) return { status: 409, body: { error: 'a newer claim for this node is already approved' } };
      const c = claims.find(x => x.id === id);
      if (c) c.status = op === 'approve' ? 'approved' : 'rejected';
      return { status: 202, body: { requestId: 'fedcba9876543210' } };
    };
    return store;
  }
  for (const vp of [{ width: 375, height: 560, touch: true, name: 'phone 375x560' }, { width: 360, height: 640, touch: true, name: 'phone 360x640' },
    { width: 1280, height: 800, name: 'desktop 1280x800' }]) {
    await step('admin, 11 claims, ' + vp.name + ': a decision\'s result is on screen', async () => {
      const store = longQueue();
      const { ctx, page, errors } = await newCtx({ viewport: { width: vp.width, height: vp.height }, touch: vp.touch, admin: store, statuses: [404] });
      await page.goto(BASE + '/#/tools/antenna-claims', { waitUntil: 'domcontentloaded' });
      await page.waitForSelector('[data-testid="ac-admin-key"]', { timeout: 15000 });
      await page.fill('[data-testid="ac-admin-key"]', ADMIN_KEY);
      await page.press('[data-testid="ac-admin-key"]', 'Enter');
      await page.waitForFunction(() => document.querySelectorAll('.ac-item').length === 11);
      // On a phone the thumb taps a button just above the bottom nav, where
      // a scrolled-down list leaves it (#868 F3): scroll so the button's
      // bottom is `above` px above the nav, then tap without Playwright
      // moving it. At 30 px the result line lands wholly inside the
      // viewport but under the nav, which scrollIntoView 'nearest' ignores.
      async function decide(id, op, above) {
        const sel = '.ac-item[data-claim-id="' + id + '"] [data-decide="' + op + '"]';
        const loc = page.locator(sel);
        await loc.scrollIntoViewIfNeeded();
        if (!vp.touch) return loc.click();
        await page.evaluate((s) => {
          const e = document.querySelector(s.sel), nav = document.querySelector('.bottom-nav');
          const d = e.getBoundingClientRect().bottom - (nav.getBoundingClientRect().top - s.above);
          let sc = e.parentElement;
          while (sc && !(sc.scrollHeight > sc.clientHeight && /auto|scroll/.test(getComputedStyle(sc).overflowY))) sc = sc.parentElement;
          (sc || document.scrollingElement).scrollTop += d;
        }, { sel, above });
        const p = await loc.evaluate((e) => {
          const r = e.getBoundingClientRect(), nav = document.querySelector('.bottom-nav').getBoundingClientRect();
          const x = r.left + r.width / 2, y = r.top + r.height / 2, t = document.elementFromPoint(x, y);
          return { x, y, gap: Math.round(nav.top - r.bottom), hit: !!t && (t === e || e.contains(t)) };
        });
        assert(p.hit && Math.abs(p.gap - above) <= 1, 'the button is not just above the bottom nav: ' + JSON.stringify(p));
        await page.touchscreen.tap(p.x, p.y);
      }
      for (const [i, above] of [[0, 10], [1, 30]]) {
        await decide(store.claims[i].id, 'approve', above);
        const refused = await feedback(page, /Reject this claim instead/, store.claims[i].id);
        assert(refused.visible && refused.kind === 'error', 'the 409 text is off screen (tapped ' + above + ' px above the nav): ' + JSON.stringify(refused));
      }
      if (SHOT_DIR) await page.screenshot({ path: path.join(SHOT_DIR, 'admin-409-' + vp.width + '.png') });
      await decide(store.claims[5].id, 'reject', 10);
      await page.waitForFunction((id) => !document.querySelector('.ac-item[data-claim-id="' + id + '"]'), store.claims[5].id, { timeout: 15000 });
      const done = await feedback(page, /^Done/);
      assert(done.visible && done.kind === 'success', '"Done." is off screen: ' + JSON.stringify(done));
      if (SHOT_DIR) await page.screenshot({ path: path.join(SHOT_DIR, 'admin-done-' + vp.width + '.png') });
      assert(errors.length === 0, errors.join('; '));
      await ctx.close();
    });
  }

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
