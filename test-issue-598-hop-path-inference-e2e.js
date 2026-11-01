/**
 * E2E (#598, port of upstream `Kpa-clawbot/CoreScope#2245`) — whole-path hop
 * inference in the packet detail pane and the map route view.
 *
 * Fixture packet 57224a4a01c9c682, observation 147: a 22-hop flood whose every
 * hop is a 1-byte prefix, with "10" at three positions and "D6" at two.
 *
 * On master the same node ("1000💥Repeater G2") is named at all three "10"
 * positions — a node forwards a given packet once (firmware wasSeen/markSeen),
 * so at least two of those are wrong — and the detail pane and the map route
 * view disagree at five of the 22 hops, because each resolves prefixes on its
 * own. Both are checked here.
 *
 * Usage: BASE_URL=http://localhost:13581 node test-issue-598-hop-path-inference-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const PACKET = '57224a4a01c9c682';
const OBS = '147';
const HOPS = ['37', 'BB', '29', '87', '10', '9F', 'F5', '78', '70', '10', '2D', 'D6',
  '47', 'C0', '6B', 'E2', 'D6', 'EE', '20', '10', '04', '5B'];

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

// The Path row's hops, in order, with what each one claims about itself.
// Falls back to the Path row itself when the inferred-hops span is absent, so
// the behavioural assertions below still run (and fail) on an unported build
// rather than stopping at a missing selector.
function readPathRow(page) {
  return page.evaluate(() => {
    const span = document.querySelector('.detail-path-hops') ||
      [...document.querySelectorAll('.detail-meta dt')]
        .filter(d => d.textContent.trim() === 'Path')
        .map(d => d.nextElementSibling)[0];
    if (!span) return null;
    return [...span.querySelectorAll('.hop')].map(el => ({
      text: el.textContent.trim(),
      href: el.getAttribute('href') || null,
      title: el.getAttribute('title') || '',
      unlisted: el.classList.contains('hop-unlisted'),
      guess: el.classList.contains('hop-ambiguous'),
    }));
  });
}
const HOP_SELECTOR = '.detail-path-hops .hop, .detail-meta dd .hop';

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });

  async function openDetail(viewport, isMobile) {
    const ctx = await browser.newContext(Object.assign({ viewport: viewport },
      isMobile ? { isMobile: true, hasTouch: true, deviceScaleFactor: 2 } : {}));
    const page = await ctx.newPage();
    page.setDefaultTimeout(30000);
    const errors = [];
    page.on('pageerror', e => errors.push(String(e.message)));
    await page.goto(`${BASE}/#/packets/${PACKET}?obs=${OBS}&timeWindow=0`, { waitUntil: 'load' });
    await page.waitForSelector(HOP_SELECTOR, { timeout: 30000 });
    // The pane renders before the neighbour graph lands and redraws the hop
    // rows when it does; read the settled state.
    await page.waitForTimeout(2500);
    return { ctx, page, errors };
  }

  console.log(`\n#598 E2E against ${BASE} — packet ${PACKET}, observation ${OBS}\n`);

  // ── Desktop ──
  const desktop = await openDetail({ width: 1280, height: 900 }, false);
  const page = desktop.page;
  let hops = null;

  await step('the Path row renders all 22 hops from their own redrawable span', async () => {
    hops = await readPathRow(page);
    assert(hops, 'the Path row is readable');
    assert(hops.length === HOPS.length,
      `expected ${HOPS.length} hops, got ${hops.length}: ` + JSON.stringify(hops.map(h => h.text)));
    assert(await page.$('.detail-path-hops'),
      'and its hops sit in .detail-path-hops, the in-place redraw target');
  });

  await step('every hop was inferred — each says how likely it is, or why it is unlisted', async () => {
    const bad = hops.filter(h => !/% likely|no listed node with this prefix|no known node has this prefix|no GPS to check/.test(h.title));
    assert(bad.length === 0,
      'hops with no inferred tooltip: ' + JSON.stringify(bad.map(h => [h.text, h.title])));
  });

  await step('no node is named at two positions (a node forwards a flood once)', async () => {
    const named = hops.filter(h => h.href).map(h => h.href);
    const dupes = named.filter((h, i) => named.indexOf(h) !== i);
    assert(dupes.length === 0,
      'the same node appears at several hops: ' + JSON.stringify(dupes) +
      ' — full path ' + JSON.stringify(hops.map(h => h.text)));
  });

  await step('the prefix "10", at three positions, is not one node three times', async () => {
    const idx = HOPS.reduce((a, h, i) => (h === '10' ? a.concat(i) : a), []);
    assert(idx.length === 3, 'fixture: "10" sits at three positions, got ' + idx.length);
    // At most one of them can be a given node. Master names the same node at
    // all three; here the others are reported unlisted rather than guessed.
    const named = idx.map(i => hops[i].href).filter(Boolean);
    assert(new Set(named).size === named.length,
      'no node is named at two of the three "10" positions, got ' +
      JSON.stringify(idx.map(i => hops[i].text)));
    assert(new Set(idx.map(i => hops[i].text)).size > 1,
      'the three "10" hops do not all show the same thing, got ' +
      JSON.stringify(idx.map(i => hops[i].text)));
  });

  await step('an unlisted hop shows its hex prefix, not a node that shares it', async () => {
    const unlisted = hops.filter(h => h.unlisted);
    assert(unlisted.length > 0, 'this path has unlisted hops on the fixture catalogue');
    for (const h of unlisted) {
      if (/no GPS/.test(h.title)) continue; // a named node without a position
      assert(/^[0-9A-Fa-f]{2}$/.test(h.text),
        'an unlisted hop is labelled with its prefix, got ' + JSON.stringify(h.text));
    }
  });

  await step('a candidate badge opens a popover listing each candidate\'s likelihood', async () => {
    const btn = await page.$('.detail-path-hops .hop-conflict-btn, .detail-meta dd .hop-conflict-btn');
    assert(btn, 'at least one hop offers its candidates');
    await btn.click();
    await page.waitForSelector('.hop-conflict-popover', { timeout: 10000 });
    const pop = await page.evaluate(() => {
      const el = document.querySelector('.hop-conflict-popover');
      return { header: el.querySelector('.hop-conflict-header').textContent.trim(),
        items: [...el.querySelectorAll('.hop-conflict-item')].map(i => i.textContent.replace(/\s+/g, ' ').trim()) };
    });
    assert(/likely .+ \(\d+%\)|unlisted node \(\d+%\)/.test(pop.header),
      'the popover header states the inferred answer: ' + pop.header);
    assert(pop.items.length > 0, 'and lists the candidates');
    assert(pop.items.some(t => /\d+%|no GPS|hop \d+/.test(t)),
      'each candidate carries its likelihood (or where the path uses it instead): ' + JSON.stringify(pop.items));
    // Dismiss through the delegated document listener. Escape would reach
    // _pktEsc and close the whole detail panel (#282).
    await page.mouse.click(5, 5);
    await page.waitForSelector('.hop-conflict-popover', { state: 'detached', timeout: 10000 });
  });

  await step('the byte breakdown names the same node as the Path row at every hop', async () => {
    const bytes = await page.evaluate(() => [...document.querySelectorAll('.detail-hop-name')].map(el => {
      const a = el.querySelector('.hop');
      return { idx: el.dataset.hopIdx, href: a ? a.getAttribute('href') : null };
    }));
    assert(bytes.length === HOPS.length, 'the byte table lists all 22 hops, got ' + bytes.length);
    bytes.forEach((b, i) => assert(b.idx === String(i), 'byte row ' + i + ' carries index ' + b.idx));
    const mismatch = bytes.map((b, i) => [i, b.href, hops[i].href]).filter(r => r[1] !== r[2]);
    assert(mismatch.length === 0, 'byte table vs Path row: ' + JSON.stringify(mismatch));
  });

  await step('no uncaught page error on the detail pane', async () => {
    assert(desktop.errors.length === 0, desktop.errors.join(' | '));
  });

  // ── The map route view, for the same observation ──
  await step('the map route list matches the detail pane hop for hop', async () => {
    const errors = [];
    page.on('pageerror', e => errors.push(String(e.message)));
    await page.goto(`${BASE}/#/map?packet=${PACKET}&obs=${OBS}`, { waitUntil: 'load' });
    await page.waitForSelector('.mc-rt-row', { timeout: 30000 });
    await page.waitForTimeout(2000);
    const rows = await page.evaluate(() => [...document.querySelectorAll('.mc-rt-row')].map(r => ({
      name: (r.querySelector('.mc-rt-name') || r).textContent.replace(/\s+/g, ' ').trim(),
      fromPayload: /from payload/.test(r.textContent),
      unlisted: /unlisted/.test(r.textContent),
    })));
    const relays = rows.filter(r => !r.fromPayload);
    assert(relays.length === HOPS.length,
      `the route lists the 22 recorded relays (plus payload endpoints), got ${relays.length}: ` +
      JSON.stringify(rows.map(r => r.name)));
    // Each row's label is the node name, or the hex prefix for an unlisted hop
    // — the same thing the Path row shows.
    const mismatch = relays.map((r, i) => [i, r.name, hops[i].text])
      .filter(([, a, b]) => !a.includes(b) && !b.includes(a.replace(/ unlisted$/, '')));
    assert(mismatch.length === 0,
      'route row vs detail hop: ' + JSON.stringify(mismatch));
    const unlistedRows = relays.filter(r => r.unlisted).length;
    const unlistedHops = hops.filter(h => h.unlisted && !/no GPS/.test(h.title)).length;
    assert(unlistedRows === unlistedHops,
      `the route marks the same hops unlisted as the detail pane (${unlistedRows} vs ${unlistedHops})`);
    assert(errors.length === 0, 'no uncaught page error on the map: ' + errors.join(' | '));
  });

  await desktop.ctx.close();

  // ── Mobile ──
  await step('the same path renders on a 390px viewport, badges included', async () => {
    const m = await openDetail({ width: 390, height: 844 }, true);
    try {
      const mHops = await readPathRow(m.page);
      assert(mHops && mHops.length === HOPS.length,
        `expected ${HOPS.length} hops at 390px, got ${mHops ? mHops.length : 'no Path row'}`);
      const named = mHops.filter(h => h.href).map(h => h.href);
      assert(new Set(named).size === named.length, 'no node named twice at 390px');
      const btn = await m.page.$('.detail-path-hops .hop-conflict-btn, .detail-meta dd .hop-conflict-btn');
      assert(btn, 'a candidate badge is present at 390px');
      const box = await btn.boundingBox();
      assert(box && box.width > 0 && box.height > 0, 'and it is laid out, not collapsed');
      await btn.click();
      await m.page.waitForSelector('.hop-conflict-popover', { timeout: 10000 });
      const inView = await m.page.evaluate(() => {
        const r = document.querySelector('.hop-conflict-popover').getBoundingClientRect();
        return r.left >= 0 && r.right <= window.innerWidth + 1;
      });
      assert(inView, 'the candidate popover stays inside the 390px viewport');
      assert(m.errors.length === 0, 'no uncaught page error at 390px: ' + m.errors.join(' | '));
    } finally {
      await m.ctx.close();
    }
  });

  await browser.close();
  console.log(`\n#598 E2E: ${passed} passed, ${failed} failed\n`);
  process.exit(failed ? 1 : 0);
})().catch(e => { console.error(e); process.exit(1); });
