/**
 * E2E (#898 M2): owner station details on the node page and in the admin
 * queue — against a running server with the CI fixture.
 *
 * The CI server runs with antenna claims off, so the claim routes and
 * GET /api/nodes/{pk}/station are answered by Playwright in the shapes of
 * cmd/server/antenna_claims.go (#898 M1); nothing is written anywhere. The
 * real server is used where the point is what it says (the config route
 * and /station with the feature off).
 *
 * Asserts:
 *   1. feature off: the real config says detailsEnabled false and /station
 *      answers {details:null}; with claims on but details off the page has
 *      no Station block, no details fieldset and never asks /station (U5);
 *   2. the read-only Station block for everyone: values, the public
 *      contact as text (markup stays inert), asked once per node-page view
 *      across a theme re-render and Start over (U6); stale or no details
 *      show nothing;
 *   3. contactAdmin is never shown publicly: a /station answer that leaked
 *      one (a broken server) shows it nowhere, does not pre-fill the admin
 *      field and is not sent (U3), next to a public contact and with the
 *      public contact empty (no fallback, F2); the public fields are
 *      pre-filled;
 *   4. the owner form: Keep + Keep cannot be sent (U7); a combined claim
 *      posts exactly {heightM, details{…, contactAdmin}}, the claim line
 *      names both parts and no contact or detail reaches the URL,
 *      sessionStorage or localStorage (U4); Remove details posts
 *      {clearDetails:true}; a bad contact or gain is refused before any
 *      request;
 *   5. the admin queue: per-part lines, the replaced marker, contactAdmin
 *      for proven and approved only, markup in both contacts as text, a
 *      details-only claim never reads as a height removal (U1), the 409
 *      names the overlapping part;
 *   6. a11y: axe clean on the section (form with the details open, Station
 *      block) and on the admin queue, light and dark;
 *   7. phone 375x560 and 360x640 from a cold start on the node list, real
 *      taps only: the node, the details radios, power checkboxes and
 *      selects are 44 px targets, Get code gives the code, no sideways
 *      scroll; and an admin row at 375x560.
 *
 * Usage:
 *   BASE_URL=http://localhost:13581 node test-issue-898-m2-e2e.js
 *   E2E_SHOTS=/some/dir … also writes screenshots for browser validation.
 */
'use strict';
const { chromium } = require('playwright');
const { tapWhenSettled } = require('./tests/e2e-tap');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const SHOTS = process.env.E2E_SHOTS || '';
const ADMIN_KEY = 'e2e-antenna-admin-key-0123456789';
const REQ_ID = 'abcdef0123456789';
const CODE = '7KX9QM';
const SECTION = '[data-testid="antenna-claim"]';
const STATION = '[data-testid="ac-station"]';
const ADMIN_LEAK = 'ZZADMIN9';
const XSS = '<img src=x onerror="window.__xss=1">';
const DETAILS = { antennaType: 'yagi', gainDbi: 6.5, power: ['solar', 'battery'], installation: 'mast', contactPublic: 'OZ1ABC' };

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + (e && e.stack || e)); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }
function pathOf(url) { return new URL(url).pathname; }
const json = (body, status) => ({ status: status || 200, contentType: 'application/json', body: JSON.stringify(body) });

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #898 M2 station details UI against ${BASE} ===\n`);

  const nodes = await (await fetch(BASE + '/api/nodes?limit=500')).json();
  const list = nodes.nodes || [];
  const repeater = list.find(n => n.role === 'repeater' && n.lat && n.lon && n.name);
  assert(repeater, 'fixture needs a positioned repeater');
  const PK = repeater.public_key;
  const positioned = new Set(list.filter(n => (n.role === 'repeater' || n.role === 'room') && n.lat && n.lon).map(n => n.public_key));

  // newCtx answers the claim API. o.config: the config answer (null: the
  // real server's); o.station: the /station answer (null: the real
  // server's); o.statuses: the status sequence; o.admin: the admin store.
  async function newCtx(o) {
    o = o || {};
    const ctx = await browser.newContext({
      viewport: o.viewport || { width: 1280, height: 860 },
      hasTouch: !!o.touch, isMobile: !!o.touch,
    });
    const rec = { station: 0, posts: [], status: 0, decisions: [] };
    if (o.config !== null) {
      await ctx.route(u => pathOf(u) === '/api/antenna-claims/config', r => r.fulfill(json(o.config || { enabled: true, detailsEnabled: true })));
    }
    await ctx.route(u => /^\/api\/nodes\/[0-9a-f]{64}\/antenna$/.test(pathOf(u)), r =>
      r.fulfill(json({ antennaM: 18.5, antennaSource: 'owner', assumed: false })));
    if (o.station !== null) {
      await ctx.route(u => /^\/api\/nodes\/[0-9a-f]{64}\/station$/.test(pathOf(u)), r => {
        rec.station++;
        return r.fulfill(json(o.station || { details: DETAILS, approvedAt: Date.UTC(2026, 10, 1) }));
      });
    } else {
      await ctx.route(u => /^\/api\/nodes\/[0-9a-f]{64}\/station$/.test(pathOf(u)), r => { rec.station++; return r.continue(); });
    }
    await ctx.route(u => /^\/api\/nodes\/[0-9a-f]{64}\/antenna-claims$/.test(pathOf(u)), r => {
      rec.posts.push(JSON.parse(r.request().postData() || 'null'));
      return r.fulfill(json({ requestId: REQ_ID, code: CODE, expiresAt: Date.now() + 86400000, nameBudgetBytes: 23, suggestedName: 'Roof ' + CODE }, 202));
    });
    await ctx.route(u => /^\/api\/antenna-claims\/requests\//.test(pathOf(u)), r => {
      rec.status++;
      const seq = o.statuses || ['pending'];
      return r.fulfill(json({ status: seq[Math.min(rec.status - 1, seq.length - 1)] }));
    });
    if (o.admin) {
      await ctx.route(u => pathOf(u) === '/api/admin/antenna-claims', r => {
        if (r.request().headers()['x-api-key'] !== ADMIN_KEY) return r.fulfill(json({ error: 'unauthorized' }, 401));
        const st = new URL(r.request().url()).searchParams.get('status');
        return r.fulfill(json({ claims: o.admin.filter(c => c.status === st), enabled: true, autoApprove: false }));
      });
      await ctx.route(u => /^\/api\/admin\/antenna-claims\/[^/]+\/(approve|reject|revoke)$/.test(pathOf(u)), r => {
        const m = pathOf(r.request().url()).split('/');
        rec.decisions.push([m[4], m[5]]);
        if (m[5] === 'approve' && m[4] === '6666666666666666') return r.fulfill(json({ error: 'a newer claim for this node is already approved' }, 409));
        return r.fulfill(json({ requestId: 'fedcba9876543210' }, 202));
      });
    }
    await ctx.addInitScript((theme) => {
      try {
        localStorage.setItem('meshcore-theme', theme);
        localStorage.setItem('meshcore-map-clustering', 'false');
        ['row-swipe', 'tab-swipe', 'edge-drawer', 'pull-refresh']
          .forEach(k => localStorage.setItem('meshcore-gesture-hints-' + k, 'seen'));
      } catch (_) {}
      const apply = () => document.documentElement && document.documentElement.setAttribute('data-theme', theme);
      apply();
      document.addEventListener('DOMContentLoaded', apply);
    }, o.theme || 'light');
    const page = await ctx.newPage();
    const errors = [];
    page.on('pageerror', e => errors.push(String(e)));
    return { ctx, page, rec, errors };
  }

  async function openNode(page, pk, extra) {
    await page.goto(BASE + '/#/nodes/' + pk + (extra || ''), { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#nodeFullBody .node-full-card', { timeout: 15000 });
    await page.waitForSelector(SECTION + ':visible', { timeout: 15000 });
  }
  async function shot(page, name) {
    if (!SHOTS) return;
    await page.waitForFunction(() => document.getAnimations().every(a => a.playState !== 'running'));
    await page.locator(SECTION + ':visible, .ac-admin').first().screenshot({ path: require('path').join(SHOTS, name + '.png') });
  }
  // Everything the browser keeps or shows outside the form's own inputs.
  async function leakScan(page) {
    return page.evaluate(() => {
      const store = (s) => { const o = []; for (let i = 0; i < s.length; i++) o.push(s.key(i) + '=' + s.getItem(s.key(i))); return o.join('\n'); };
      return { url: location.href, session: store(sessionStorage), local: store(localStorage), text: document.body.innerText };
    });
  }

  // ── 1. Feature off ──────────────────────────────────────────────────────
  await step('real server: detailsEnabled is false and /station answers {details:null}', async () => {
    const c = await (await fetch(BASE + '/api/antenna-claims/config')).json();
    assert(c.detailsEnabled === false, 'detailsEnabled ' + c.detailsEnabled);
    const r = await fetch(BASE + '/api/nodes/' + PK + '/station');
    assert(r.status === 200, '/station ' + r.status);
    const body = await r.json();
    assert(JSON.stringify(body) === '{"details":null}', JSON.stringify(body));
  });

  await step('claims on, details off: no Station block, no details fieldset, /station never asked (U5)', async () => {
    const { ctx, page, rec, errors } = await newCtx({ config: { enabled: true, detailsEnabled: false } });
    await openNode(page, PK);
    await page.waitForSelector('[data-testid="ac-height"]');
    await page.waitForTimeout(400);
    assert(await page.locator(STATION).count() === 0, 'a Station block with details off');
    assert(await page.locator('[data-testid^="ac-details-"]').count() === 0, 'a details control with details off');
    assert(rec.station === 0, '/station asked ' + rec.station + ' times');
    assert(await page.locator('[data-testid="ac-mode-keep"]').count() === 0, 'Keep offered without a second part');
    assert(await page.isEnabled('[data-testid="ac-submit"]'), 'Get code disabled for the height-only form');
    assert(errors.length === 0, errors.join('; '));
    await ctx.close();
  });

  await step('real config (claims off): no section, no /station request', async () => {
    const { ctx, page, rec } = await newCtx({ config: null, station: null });
    await page.goto(BASE + '/#/nodes/' + PK, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#nodeFullBody .node-full-card', { timeout: 15000 });
    await page.waitForTimeout(800);
    assert(await page.locator(SECTION + ':visible').count() === 0, 'a section with claims off');
    assert(rec.station === 0, '/station asked ' + rec.station + ' times');
    await ctx.close();
  });

  // ── 2. The public Station block ─────────────────────────────────────────
  await step('Station block: values and the public contact for everyone, asked once per view (U6)', async () => {
    const { ctx, page, rec, errors } = await newCtx();
    await openNode(page, PK);
    await page.waitForSelector(STATION + ':visible');
    const rows = await page.$$eval(STATION + ' .ac-station-row', rs => rs.map(r => [r.querySelector('dt').textContent, r.querySelector('dd').textContent]));
    const want = [['Antenna', 'Yagi'], ['Gain', '6.5 dBi'], ['Power', 'Solar + Battery'], ['Installation', 'Mast'], ['Owner contact', 'OZ1ABC']];
    assert(JSON.stringify(rows) === JSON.stringify(want), JSON.stringify(rows));
    assert(/Given by the node owner, confirmed /.test(await page.textContent(STATION)), await page.textContent(STATION));
    const title = await page.evaluate((s) => document.getElementById(document.querySelector(s).getAttribute('aria-labelledby')).textContent, SECTION);
    assert(title === 'Antenna and station (owner)', title);
    await shot(page, 'station-block-desktop-light');
    // A theme toggle rebuilds the node page; Start over re-renders the form.
    await page.click('#darkModeToggle');
    await page.waitForSelector(STATION + ':visible');
    await page.check('[data-testid="ac-details-clear"]');
    await page.click('[data-testid="ac-submit"]');
    await page.waitForSelector('[data-testid="ac-code"]');
    await page.click('[data-testid="ac-new"]');
    await page.waitForSelector('[data-testid="ac-details-keep"]');
    await page.waitForTimeout(300);
    assert(rec.station === 1, '/station asked ' + rec.station + ' times in one view');
    assert(errors.length === 0, errors.join('; '));
    await ctx.close();
  });

  await step('Station block: markup in the public contact is text and never runs', async () => {
    const { ctx, page } = await newCtx({ station: { details: { antennaType: 'omni', contactPublic: XSS + 'P' }, approvedAt: 1 } });
    await openNode(page, PK);
    await page.waitForSelector('[data-testid="ac-station-contact"]');
    assert(await page.textContent('[data-testid="ac-station-contact"]') === XSS + 'P', 'contact not shown as text');
    assert(await page.locator(SECTION + ' img').count() === 0, 'an <img> was built from the contact');
    assert(await page.evaluate(() => window.__xss) === undefined, 'the onerror ran');
    await ctx.close();
  });

  for (const [label, station] of [['stale', { details: null, stale: true }], ['none', { details: null }], ['empty', { details: {} }]]) {
    await step('Station block: ' + label + ' details show nothing', async () => {
      const { ctx, page, rec } = await newCtx({ station });
      await openNode(page, PK);
      await page.waitForSelector('[data-testid="ac-details-keep"]');
      await page.waitForFunction(() => true);
      await page.waitForTimeout(300);
      assert(rec.station === 1, '/station asked ' + rec.station);
      assert(await page.locator(STATION + ':visible').count() === 0, 'a Station block for ' + label);
      await ctx.close();
    });
  }

  // ── 3. contactAdmin is never public ─────────────────────────────────────
  // Twice: next to a public contact, and with the public contact empty, so
  // that a `contactPublic || contactAdmin` fallback (in the Station block or
  // in the pre-fill, which would then publish the admin contact) is caught
  // too (review PR#908 F2).
  for (const pub of ['OZ1ABC', '']) {
    await step('a /station answer that leaked contactAdmin shows it nowhere and never pre-fills it (U3), public contact ' + (pub ? 'set' : 'empty'), async () => {
      const { ctx, page, rec } = await newCtx({ station: { details: Object.assign({}, DETAILS, { contactPublic: pub, contactAdmin: ADMIN_LEAK }), approvedAt: 1 } });
      await openNode(page, PK);
      await page.waitForSelector(STATION + ':visible');
      await page.check('[data-testid="ac-details-set"]');
      await page.waitForFunction(() => document.querySelector('[data-testid="ac-antenna-type"]').value === 'yagi');
      const form = await page.evaluate(() => ({
        type: document.querySelector('[data-testid="ac-antenna-type"]').value,
        gain: document.querySelector('[data-testid="ac-gain"]').value,
        solar: document.querySelector('[data-testid="ac-power-solar"]').checked,
        battery: document.querySelector('[data-testid="ac-power-battery"]').checked,
        mains: document.querySelector('[data-testid="ac-power-mains"]').checked,
        inst: document.querySelector('[data-testid="ac-installation"]').value,
        pub: document.querySelector('[data-testid="ac-contact-public"]').value,
        adm: document.querySelector('[data-testid="ac-contact-admin"]').value,
        values: Array.from(document.querySelectorAll('input, select, textarea')).map(e => e.value).join('|'),
      }));
      assert(form.type === 'yagi' && form.gain === '6.5' && form.solar && form.battery && !form.mains && form.inst === 'mast' && form.pub === pub,
        'public values not pre-filled: ' + JSON.stringify(form));
      const block = await page.textContent(STATION);
      assert(block.indexOf(ADMIN_LEAK) < 0, ADMIN_LEAK + ' in the Station block: ' + block);
      assert(/Owner contact/.test(block) === !!pub, 'Owner contact row with public contact ' + JSON.stringify(pub) + ': ' + block);
      assert(form.adm === '', 'the admin contact was pre-filled: ' + form.adm);
      const scan = await leakScan(page);
      for (const where of ['url', 'session', 'local', 'text']) assert(scan[where].indexOf(ADMIN_LEAK) < 0, ADMIN_LEAK + ' in ' + where);
      assert(form.values.indexOf(ADMIN_LEAK) < 0, ADMIN_LEAK + ' in a form value');
      const labels = await page.evaluate(() => [
        document.querySelector('label[for="acContactPublic"]').textContent,
        document.querySelector('label[for="acContactAdmin"]').textContent,
        document.getElementById('acContactAdminHint').textContent,
      ]);
      assert(/Shown publicly/.test(labels[0]) && /Admin only/.test(labels[1]) && /never shown publicly/.test(labels[2]), JSON.stringify(labels));
      // Sending the pre-filled details as they stand never carries the leak,
      // in either contact.
      await page.click('[data-testid="ac-submit"]');
      await page.waitForSelector('[data-testid="ac-code"]', { timeout: 10000 });
      assert(rec.posts.length === 1 && JSON.stringify(rec.posts[0]).indexOf(ADMIN_LEAK) < 0, 'posted ' + JSON.stringify(rec.posts));
      assert((rec.posts[0].details.contactPublic || '') === pub, 'posted public contact ' + JSON.stringify(rec.posts[0].details));
      await ctx.close();
    });
  }

  // ── 4. The owner form ───────────────────────────────────────────────────
  await step('Keep + Keep cannot be sent (U7)', async () => {
    const { ctx, page, rec } = await newCtx();
    await openNode(page, PK);
    await page.waitForSelector('[data-testid="ac-details-keep"]');
    assert(await page.isChecked('[data-testid="ac-mode-keep"]') && await page.isChecked('[data-testid="ac-details-keep"]'), 'not Keep + Keep at the start');
    assert(await page.isDisabled('[data-testid="ac-submit"]'), 'Get code enabled on Keep + Keep');
    const dim = await page.$eval('[data-testid="ac-submit"]', b => getComputedStyle(b).opacity);
    assert(dim === '0.5', 'a disabled Get code is not dimmed: ' + dim);
    assert(/Choose what to change/.test(await page.textContent('[data-testid="ac-form-status"]')), 'no hint on Keep + Keep');
    await page.evaluate(() => document.querySelector('.ac-form').requestSubmit());
    await page.waitForTimeout(300);
    assert(rec.posts.length === 0, 'Keep + Keep posted ' + JSON.stringify(rec.posts));
    await page.check('[data-testid="ac-mode-set"]');
    assert(await page.isEnabled('[data-testid="ac-submit"]'), 'Get code still disabled with a height part');
    await ctx.close();
  });

  await step('combined claim: exact body, both parts named, no contact or detail kept in the browser (U4)', async () => {
    const { ctx, page, rec, errors } = await newCtx({ station: { details: null }, statuses: ['pending', 'proven', 'approved'] });
    await openNode(page, PK);
    await page.check('[data-testid="ac-mode-set"]');
    await page.fill('[data-testid="ac-height"]', '12,5');
    await page.check('[data-testid="ac-details-set"]');
    await page.selectOption('[data-testid="ac-antenna-type"]', 'omni');
    await page.fill('[data-testid="ac-gain"]', '3,7');
    await page.check('[data-testid="ac-power-battery"]');
    await page.check('[data-testid="ac-power-mains"]');
    await page.selectOption('[data-testid="ac-installation"]', 'roof');
    await page.fill('[data-testid="ac-contact-public"]', 'Ærø Radio');
    await page.fill('[data-testid="ac-contact-admin"]', 'ZZADMIN7 jens@example.dk');
    await shot(page, 'owner-form-desktop-light');
    await page.click('[data-testid="ac-submit"]');
    await page.waitForSelector('[data-testid="ac-code"]');
    const want = { heightM: 12.5, details: { antennaType: 'omni', gainDbi: 3.7, power: ['mains', 'battery'], installation: 'roof', contactPublic: 'Ærø Radio', contactAdmin: 'ZZADMIN7 jens@example.dk' } };
    assert(rec.posts.length === 1 && JSON.stringify(rec.posts[0]) === JSON.stringify(want), JSON.stringify(rec.posts));
    assert(await page.textContent('.ac-what') === 'Setting the antenna height to 12.5 m and replacing the station details of this node.', await page.textContent('.ac-what'));
    await page.waitForFunction(() => /station details are shown/.test(document.querySelector('[data-testid="ac-status"]').textContent), null, { timeout: 15000 });
    const scan = await leakScan(page);
    for (const v of ['ZZADMIN7', 'jens@example.dk', 'Ærø', 'omni', '3.7', 'roof', 'battery']) {
      assert(scan.url.indexOf(encodeURIComponent(v)) < 0 && scan.url.indexOf(v) < 0, v + ' in the URL ' + scan.url);
      assert(scan.session.indexOf(v) < 0, v + ' in sessionStorage: ' + scan.session);
      assert(scan.local.indexOf(v) < 0, v + ' in localStorage');
    }
    assert(/claim=abcdef0123456789/.test(scan.url), 'no claim deep link: ' + scan.url);
    // A reload restores the claim from the stored parts alone.
    await page.reload({ waitUntil: 'domcontentloaded' });
    await page.waitForSelector('[data-testid="ac-code"]', { timeout: 15000 });
    assert(/12\.5 m and replacing the station details/.test(await page.textContent('.ac-what')), 'after a reload: ' + await page.textContent('.ac-what'));
    assert(errors.length === 0, errors.join('; '));
    await ctx.close();
  });

  await step('Remove my details posts {clearDetails:true}; the height stays Keep', async () => {
    const { ctx, page, rec } = await newCtx();
    await openNode(page, PK);
    await page.check('[data-testid="ac-details-clear"]');
    assert(await page.locator('[data-testid="ac-details-fields"]').isHidden(), 'the fields show for Remove');
    await page.click('[data-testid="ac-submit"]');
    await page.waitForSelector('[data-testid="ac-code"]');
    assert(JSON.stringify(rec.posts) === '[{"clearDetails":true}]', JSON.stringify(rec.posts));
    assert(await page.textContent('.ac-what') === 'Removing the station details of this node.', await page.textContent('.ac-what'));
    await ctx.close();
  });

  await step('a bad contact, a bad gain or empty details are refused before any request', async () => {
    const { ctx, page, rec } = await newCtx({ station: { details: null } });
    await openNode(page, PK);
    await page.check('[data-testid="ac-details-set"]');
    await page.click('[data-testid="ac-submit"]');
    assert(/at least one station detail/.test(await page.textContent('[data-testid="ac-form-status"]')), 'empty details: ' + await page.textContent('[data-testid="ac-form-status"]'));
    await page.fill('[data-testid="ac-contact-admin"]', 'https://evil.example');
    await page.click('[data-testid="ac-submit"]');
    assert(/Administrator contact/.test(await page.textContent('[data-testid="ac-form-status"]')), 'bad admin contact');
    assert(await page.evaluate(() => document.activeElement.id) === 'acContactAdmin', 'focus not on the admin contact');
    await page.fill('[data-testid="ac-contact-admin"]', '');
    await page.fill('[data-testid="ac-contact-public"]', 'a‮b');
    await page.click('[data-testid="ac-submit"]');
    assert(/Public contact/.test(await page.textContent('[data-testid="ac-form-status"]')), 'bad public contact');
    await page.fill('[data-testid="ac-contact-public"]', '');
    await page.fill('[data-testid="ac-gain"]', '20.1');
    await page.click('[data-testid="ac-submit"]');
    assert(/-5 and 20 dBi/.test(await page.textContent('[data-testid="ac-form-status"]')), 'bad gain');
    assert(rec.posts.length === 0, 'posted ' + JSON.stringify(rec.posts));
    await ctx.close();
  });

  // ── 5. The admin queue ──────────────────────────────────────────────────
  const now = Date.now();
  const adminClaims = [
    { id: '5555555555555555', pubkey: PK, name: repeater.name, role: 'repeater', status: 'proven', createdAt: now - 7200000, expiresAt: now + 3600000,
      provenAt: now - 600000, proofHash: 'abcdef14', heightOp: 'set', heightM: 12.5, detailsOp: 'set',
      details: { antennaType: 'omni', gainDbi: 17, power: ['solar', 'battery'], installation: 'mast', contactPublic: XSS + 'P' },
      contactAdmin: XSS + 'A', needsReview: true },
    { id: '6666666666666666', pubkey: PK, name: 'Details only', role: 'repeater', status: 'proven', createdAt: now - 7200000, expiresAt: now + 3600000,
      provenAt: now - 900000, heightM: null, detailsOp: 'set', details: { installation: 'roof' } },
    { id: '7777777777777777', pubkey: PK, name: 'Not proven yet', role: 'repeater', status: 'pending', createdAt: now - 60000, expiresAt: now + 3600000,
      heightM: null, detailsOp: 'set', details: { antennaType: 'panel' }, contactAdmin: 'ZZPENDING1' },
    { id: '8888888888888888', pubkey: PK, name: 'Partly replaced', role: 'repeater', status: 'approved', createdAt: now - 9e6, expiresAt: now - 8e6,
      provenAt: now - 8.5e6, reviewedAt: now - 8e6, approvedBy: 'admin', heightOp: 'set', heightM: 10, heightInForce: true,
      detailsOp: 'set', detailsInForce: false, details: { antennaType: 'yagi' }, contactAdmin: 'ZZAPPROVED1' },
    { id: '9999999999999999', pubkey: PK, name: 'Clear only', role: 'repeater', status: 'approved', createdAt: now - 5e6, expiresAt: now - 4e6,
      provenAt: now - 4.5e6, reviewedAt: now - 4.5e6, approvedBy: 'owner', heightM: null, heightOp: 'clear', heightInForce: true, detailsOp: 'clear', detailsInForce: true },
  ];
  async function unlock(page, filter) {
    await page.goto(BASE + '/#/tools/antenna-claims' + (filter ? '?filter=' + filter : ''), { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('[data-testid="ac-admin-key"]', { timeout: 15000 });
    await page.fill('[data-testid="ac-admin-key"]', ADMIN_KEY);
    await page.press('[data-testid="ac-admin-key"]', 'Enter');
    await page.waitForSelector('#acAdminList[aria-busy="false"]', { timeout: 10000 });
  }
  const row = (id) => '.ac-item[data-claim-id="' + id + '"]';

  await step('admin: per-part lines, contactAdmin for proven only, markup as text, gain flag', async () => {
    const { ctx, page, errors } = await newCtx({ admin: adminClaims });
    await unlock(page);
    const combined = await page.textContent(row('5555555555555555'));
    assert(/Height: 12\.5 m/.test(combined), combined);
    assert(combined.indexOf('Station details: Omni · 17.0 dBi · Solar + Battery · Mast · public contact ' + XSS + 'P') >= 0, combined);
    assert(combined.indexOf('Contact (admin only): ' + XSS + 'A') >= 0, combined);
    assert(/Check: gain outside the usual 0–15 dBi/.test(combined), combined);
    // The disabled style is the owner form's only (F7): a disabled Approve
    // keeps its look.
    const approveDim = await page.$eval(row('5555555555555555') + ' [data-decide="approve"]', b => { b.disabled = true; const o = getComputedStyle(b).opacity; b.disabled = false; return o; });
    assert(approveDim === '1', 'a disabled Approve is dimmed: ' + approveDim);
    // One flag, two possible reasons: both are listed (F3).
    assert(/Check: the height may also be far from the surface estimate/.test(combined), combined);
    assert(await page.locator('.ac-admin img').count() === 0 && await page.evaluate(() => window.__xss) === undefined, 'markup ran in the admin queue');
    const detailsOnly = await page.textContent(row('6666666666666666'));
    assert(/Station details: Roof/.test(detailsOnly) && !/Height/.test(detailsOnly), 'details-only row: ' + detailsOnly);
    assert(await page.locator(row('6666666666666666') + ' [data-testid="ac-admin-contact"]').count() === 0, 'a contact line without a contact');
    await shot(page, 'admin-proven-desktop-light');
    // 409 on the details-only claim names the details.
    await page.click(row('6666666666666666') + ' [data-decide="approve"]');
    await page.waitForFunction((s) => /already sets this node’s station details/.test(document.querySelector(s).textContent), row('6666666666666666'), { timeout: 8000 });
    assert(errors.length === 0, errors.join('; '));
    await ctx.close();
  });

  await step('admin: a pending claim never lists contactAdmin; approved rows show replaced parts and clear-only lines', async () => {
    const { ctx, page } = await newCtx({ admin: adminClaims });
    await unlock(page, 'pending');
    const pending = await page.textContent(row('7777777777777777'));
    assert(/Station details: Panel/.test(pending) && pending.indexOf('ZZPENDING1') < 0 && !/Contact \(admin only\)/.test(pending), 'pending: ' + pending);
    await page.click('.ac-filter[data-filter="approved"]');
    await page.waitForSelector(row('8888888888888888'));
    const partly = await page.textContent(row('8888888888888888'));
    assert(/Height: 10 m/.test(partly) && /Station details: Yagi replaced/.test(partly) && /Contact \(admin only\): ZZAPPROVED1/.test(partly), 'partly: ' + partly);
    const marks = await page.$$eval(row('8888888888888888') + ' .ac-item-part', ps => ps.map(p => [p.getAttribute('data-part'), !!p.querySelector('.ac-replaced')]));
    assert(JSON.stringify(marks) === '[["height",false],["details",true]]', JSON.stringify(marks));
    const clear = await page.textContent(row('9999999999999999'));
    assert(/Height: remove/.test(clear) && /Station details: remove/.test(clear) && !/replaced/.test(clear), 'clear-only: ' + clear);
    await shot(page, 'admin-approved-desktop-light');
    await ctx.close();
  });

  // ── 6. a11y ─────────────────────────────────────────────────────────────
  const { AxeBuilder } = require('@axe-core/playwright');
  async function axe(page, sel) {
    await page.waitForFunction(() => document.getAnimations().every(a => a.playState !== 'running'));
    const res = await new AxeBuilder({ page }).include(sel).withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa']).analyze();
    return res.violations.map(x => x.id + ' x' + x.nodes.length + ' ' + x.nodes.map(n => n.target.join(' ')).join(' | '));
  }
  for (const theme of ['light', 'dark']) {
    await step(theme + ': axe is clean on the Station block, the open details form and the admin rows', async () => {
      const { ctx, page } = await newCtx({ theme, admin: adminClaims });
      await openNode(page, PK);
      await page.waitForSelector(STATION + ':visible');
      await page.check('[data-testid="ac-mode-set"]');
      await page.check('[data-testid="ac-details-set"]');
      await page.waitForFunction(() => document.querySelector('[data-testid="ac-antenna-type"]').value === 'yagi');
      let v = await axe(page, SECTION);
      assert(v.length === 0, 'node section: ' + v.join(', '));
      if (theme === 'dark') await shot(page, 'owner-form-desktop-dark');
      await unlock(page, 'approved');
      v = await axe(page, '.ac-admin');
      assert(v.length === 0, 'admin: ' + v.join(', '));
      if (theme === 'dark') await shot(page, 'admin-approved-desktop-dark');
      await ctx.close();
    });
  }

  // ── 7. Phones, cold start, real taps ────────────────────────────────────
  for (const vp of [{ width: 375, height: 560 }, { width: 360, height: 640 }]) {
    for (const theme of vp.width === 375 ? ['light', 'dark'] : ['light']) {
      const tag = vp.width + 'x' + vp.height + ' ' + theme;
      await step('phone ' + tag + ', cold start on the node list, real taps: a combined claim to the code', async () => {
        const { ctx, page, rec, errors } = await newCtx({ viewport: vp, touch: true, theme, station: { details: { antennaType: 'yagi', contactPublic: 'OZ1ABC' }, approvedAt: 1 } });
        const tap = (sel, minH) => tapWhenSettled(page, sel, { minHeight: minH });
        await page.goto(BASE + '/#/nodes', { waitUntil: 'domcontentloaded' });
        await page.waitForSelector('tr[data-key]', { timeout: 15000 });
        const keys = await page.$$eval('tr[data-key]', rs => rs.map(r => r.getAttribute('data-key')));
        const pk = keys.find(k => positioned.has(k));
        assert(pk, 'no positioned repeater or room in the list');
        await tap('tr[data-key="' + pk + '"]', 30);
        await page.waitForFunction((p) => location.hash.indexOf('#/nodes/' + p) === 0, pk, { timeout: 10000 });
        await page.waitForSelector(STATION + ':visible', { timeout: 15000 });
        const sb = await page.locator(STATION).boundingBox();
        assert(sb.x >= 0 && sb.x + sb.width <= vp.width + 0.5, 'the Station block overflows: ' + JSON.stringify(sb));
        for (const sel of ['ac-mode-keep', 'ac-mode-set', 'ac-mode-clear', 'ac-details-keep', 'ac-details-set', 'ac-details-clear']) {
          const b = await page.locator('[data-testid="' + sel + '"]').locator('xpath=..').boundingBox();
          assert(b && b.height >= 44, sel + ' row ' + JSON.stringify(b));
        }
        await tap('label:has([data-testid="ac-details-set"])');
        await page.waitForSelector('[data-testid="ac-details-fields"]:visible');
        await page.waitForFunction(() => document.querySelector('[data-testid="ac-contact-public"]').value === 'OZ1ABC');
        for (const p of ['mains', 'solar', 'battery']) {
          const b = await page.locator('[data-testid="ac-power-' + p + '"]').locator('xpath=..').boundingBox();
          assert(b && b.height >= 44, 'power ' + p + ' row ' + JSON.stringify(b));
        }
        for (const sel of ['ac-antenna-type', 'ac-installation', 'ac-gain', 'ac-contact-public', 'ac-contact-admin']) {
          const b = await page.locator('[data-testid="' + sel + '"]').boundingBox();
          assert(b && b.height >= 44 && b.x >= 0 && b.x + b.width <= vp.width + 0.5, sel + ' ' + JSON.stringify(b));
        }
        await tap('label:has([data-testid="ac-mode-set"])');
        await tap('[data-testid="ac-height"]');
        await page.keyboard.type('9.5');
        await tap('[data-testid="ac-gain"]');
        await page.keyboard.type('3');
        await tap('label:has([data-testid="ac-power-solar"])');
        await tap('[data-testid="ac-contact-admin"]');
        await page.keyboard.type('OZ1ABC admin');
        if (vp.width === 375) await shot(page, 'owner-form-375-' + theme);
        const sideways = await page.evaluate(() => document.scrollingElement.scrollWidth - window.innerWidth);
        assert(sideways <= 0, 'the node page scrolls sideways by ' + sideways + ' px');
        await tap('[data-testid="ac-submit"]');
        await page.waitForSelector('[data-testid="ac-code"]', { timeout: 10000 });
        const want = { heightM: 9.5, details: { antennaType: 'yagi', gainDbi: 3, power: ['solar'], contactPublic: 'OZ1ABC', contactAdmin: 'OZ1ABC admin' } };
        assert(rec.posts.length === 1 && JSON.stringify(rec.posts[0]) === JSON.stringify(want), JSON.stringify(rec.posts));
        await tap('[data-testid="ac-new"]');
        await page.waitForSelector('[data-testid="ac-details-keep"]');
        assert(rec.station === 1, '/station asked ' + rec.station + ' times');
        assert(errors.length === 0, errors.join('; '));
        await ctx.close();
      });
    }
  }

  await step('phone 375x560, cold start, real taps: an admin row with both parts and its decision', async () => {
    const { ctx, page, rec } = await newCtx({ viewport: { width: 375, height: 560 }, touch: true, admin: adminClaims });
    const tap = (sel, minH) => tapWhenSettled(page, sel, { minHeight: minH });
    await page.goto(BASE + '/#/tools/antenna-claims', { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('[data-testid="ac-admin-key"]', { timeout: 15000 });
    await tap('[data-testid="ac-admin-key"]');
    await page.keyboard.type(ADMIN_KEY);
    await page.keyboard.press('Enter');
    await page.waitForSelector(row('5555555555555555'));
    const r = await page.locator(row('5555555555555555')).boundingBox();
    assert(r.x >= 0 && r.x + r.width <= 375 + 0.5, 'the admin row overflows: ' + JSON.stringify(r));
    await shot(page, 'admin-375-light');
    await tap(row('5555555555555555') + ' [data-decide="approve"]');
    await page.waitForFunction(() => true);
    for (let i = 0; i < 50 && !rec.decisions.length; i++) await page.waitForTimeout(100);
    assert(JSON.stringify(rec.decisions[0]) === '["5555555555555555","approve"]', JSON.stringify(rec.decisions));
    const sideways = await page.evaluate(() => document.scrollingElement.scrollWidth - window.innerWidth);
    assert(sideways <= 0, 'the admin page scrolls sideways by ' + sideways + ' px');
    await ctx.close();
  });

  await browser.close();
  console.log(`\n#898 M2 E2E: ${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
