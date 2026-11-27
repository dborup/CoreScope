/* #990: live WebSocket packets on the Packets page must follow the Area
 * filter, by the server's own ?area= rule: the sender (the decoded ADVERT
 * public_key, or pubKey) is a member of the area; a packet with no sender is
 * dropped; an area with no positioned member filters nothing.
 *
 * The real packets page, area-filter.js dropdown, scope-member-filter.js,
 * app.js API helper, WebSocket and debounce run. Every request is fulfilled
 * here, including the WebSocket; no server, external service or real mesh
 * data is used.
 *
 * Usage: node test-issue-990-packets-live-area-e2e.js
 */
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require('playwright');

const ORIGIN = 'http://127.0.0.1:18990';
const PUB = path.join(__dirname, 'public');
const hash = id => id.toString(16).padStart(16, '0');
const A = 'a1'.repeat(32), B = 'b2'.repeat(32), C = 'c3'.repeat(32);
// The fixture's areas. EMPTY has no positioned node: the server's
// /api/nodes?area=EMPTY answers nodes: null, and /api/packets?area=EMPTY
// filters nothing (areaMemberSet returns nil).
const MEMBERS = { IN: [A, B], OTHER: [C], EMPTY: [] };

function packet(id, decoded, type) {
  return { id, hash: hash(id), payload_type: type, route_type: 1,
    timestamp: new Date().toISOString(), latest: new Date().toISOString(),
    path_json: '[]', decoded_json: typeof decoded === 'string' ? decoded : JSON.stringify(decoded),
    observer_id: 'fixture-observer', observer_name: 'Fixture observer',
    count: 1, observation_count: 1, raw_hex: '1100', payload_length: 1 };
}
const advert = (id, pk) => packet(id, { type: 'ADVERT', public_key: pk, name: 'Node ' + pk.slice(0, 4) }, 4);
const advertPubKey = (id, pk) => packet(id, { type: 'ADVERT', pubKey: pk }, 4);
const groupText = id => packet(id, { type: 'CHAN', channel: '#test', text: 'no sender' }, 5);

// Deliberately independent of the code under test: the server's rule for the
// initial list, written out for these fixture packets.
const initial = [advert(1, A), advert(2, C), groupText(3)];
const initialFor = area => area === '' || area === 'EMPTY' ? initial
  : initial.filter(p => MEMBERS[area].includes(JSON.parse(p.decoded_json).public_key));

async function main() {
  const browser = await chromium.launch({ headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined });
  let checks = 0;
  try {
    const page = await browser.newPage({ viewport: { width: 1024, height: 900 } });
    page.setDefaultTimeout(10000);
    const errors = [], packetRequests = [], memberRequests = [], hopRequests = [];
    page.on('pageerror', error => errors.push(error.message));
    let socket, memberHold = null, hopHold = null;
    await page.routeWebSocket('**', ws => { socket = ws; });
    await page.route('**/*', async route => {
      const url = new URL(route.request().url());
      if (url.origin !== ORIGIN) return route.abort();
      const q = url.searchParams;
      if (url.pathname === '/api/packets') {
        packetRequests.push(q);
        const packets = initialFor(q.get('area') || '');
        return route.fulfill({ json: { packets, total: packets.length } });
      }
      if (url.pathname === '/api/config/areas') return route.fulfill({ json: [
        { key: 'IN', label: 'Inside' }, { key: 'OTHER', label: 'Other' }, { key: 'EMPTY', label: 'Empty' }] });
      if (url.pathname === '/api/observers') return route.fulfill({ json: { observers: [
        { id: 'fixture-observer', name: 'Fixture observer', iata: 'AAA' }] } });
      if (url.pathname === '/api/nodes' && q.has('area')) {
        memberRequests.push(q);
        if (memberHold) { const h = memberHold; memberHold = null; await h(); }
        const list = MEMBERS[q.get('area')] || [];
        const offset = Number(q.get('offset') || 0);
        if (!list.length) return route.fulfill({ json: { nodes: null, total: 0 } });
        const nodes = offset ? [] : list.map(pk => ({ public_key: pk, name: 'Node ' + pk.slice(0, 4), role: 'repeater' }));
        return route.fulfill({ json: { nodes, total: nodes.length } });
      }
      if (url.pathname === '/api/nodes') {
        hopRequests.push(q);
        if (hopHold) { const h = hopHold; hopHold = null; await h(); }
        return route.fulfill({ json: { nodes: [{ public_key: 'aa'.repeat(32), name: 'Fixture relay', role: 'repeater' }], total: 1 } });
      }
      if (url.pathname.startsWith('/api/')) return route.fulfill({ json: {} });
      if (url.pathname === '/icons/phosphor-sprite.svg') return route.fulfill({
        contentType: 'image/svg+xml', body: fs.readFileSync(path.join(PUB, 'icons/phosphor-sprite.svg')) });
      return route.fulfill({ contentType: 'text/html', body:
        '<!doctype html><html><body><main id="fixture"></main></body></html>' });
    });
    await page.goto(ORIGIN + '/#/packets');
    await page.evaluate(() => { try { localStorage.clear(); } catch (e) {} });
    await page.addStyleTag({ path: path.join(PUB, 'style.css') });
    for (const file of ['payload-labels.js', 'roles.js', 'app.js', 'packet-helpers.js', 'hop-resolver.js',
      'hop-display.js', 'scope-member-filter.js', 'area-filter.js']) {
      await page.addScriptTag({ path: path.join(PUB, file) });
    }
    await page.evaluate(() => {
      // Only the unrelated region control is stubbed. The Area dropdown is
      // the real one and is driven by clicks.
      window.RegionFilter = { init(){}, onChange(){}, offChange(){}, setSelected(){}, getRegionParam(){ return ''; },
        placeMenu(){}, watchMenu(){ return function () {}; } };
      window.fixturePages = {};
      registerPage = (name, mod) => { window.fixturePages[name] = mod; };
    });
    await page.addScriptTag({ path: path.join(PUB, 'packets.js') });
    await page.evaluate(async () => {
      await window.fixturePages.packets.init(document.getElementById('fixture'));
      connectWS();
    });
    await page.waitForSelector('#pktLeft[data-loaded="true"]');

    const shown = async () => (await page.locator('#pktBody tr[data-hash]')
      .evaluateAll(rows => rows.map(r => r.dataset.hash))).sort();
    const cached = () => page.evaluate(() => window._packetsTestAPI._packetHashes());
    const send = p => socket.send(JSON.stringify({ type: 'packet', data: {
      // The Go poller's shape: packets.js reads data.packet.
      packet: p, decoded: { header: { payloadTypeName: 'ADVERT' }, payload: {} } } }));
    // Sends a packet that must be accepted and waits for its row, which also
    // proves every packet sent before it was processed (the WS batches keep
    // their order).
    async function flush(p) {
      send(p);
      await page.waitForSelector('tr[data-hash="' + p.hash + '"]');
      return p.hash;
    }
    const membersReady = () => page.waitForFunction(() => window._packetsTestAPI._areaMembersReady());
    async function pickArea(key, { held = false } = {}) {
      if (!(await page.locator('#packetsAreaFilter .region-dropdown-trigger').isVisible())) {
        await page.locator('#filterToggleBtn, .filter-toggle-btn-mirror').filter({ visible: true }).first().click();
      }
      const loaded = page.waitForResponse(r => {
        const url = new URL(r.url());
        return url.pathname === '/api/packets' && (url.searchParams.get('area') || '') === (key === '__all__' ? '' : key);
      });
      await page.locator('#packetsAreaFilter .region-dropdown-trigger').click();
      await page.locator('#packetsAreaFilter [data-area="' + key + '"]').click();
      await loaded;
      await page.waitForSelector('#pktLeft[data-loaded="true"]');
      if (!held) await membersReady();
    }

    // --- No area: live traffic is not filtered, and nothing is fetched.
    assert.deepEqual(await shown(), [hash(1), hash(2), hash(3)]); checks++;
    send(advert(10, C));
    await flush(groupText(11));
    assert.deepEqual(await shown(), [hash(1), hash(2), hash(3), hash(10), hash(11)]); checks++;
    assert.equal(memberRequests.length, 0, 'no area fetches no member set'); checks++;
    console.log('PASS: with no area, every live packet is added and no member set is fetched');

    // --- Area IN: only packets from its members are added.
    await pickArea('IN');
    assert.deepEqual(await shown(), [hash(1)]); checks++;
    assert.equal(memberRequests.length, 1, 'one member set per area change'); checks++;
    assert.equal(memberRequests[0].get('area'), 'IN'); checks++;
    assert.equal(memberRequests[0].get('geoFilter'), '0', 'the packets rule ignores geo_filter'); checks++;
    const before = packetRequests.length;
    send(advert(20, C));                    // a sender outside the area
    send(groupText(21));                    // no resolvable sender
    send(packet(22, '{bad json', 4));       // no resolvable sender
    send(advertPubKey(23, B));              // a member, by the pubKey field
    send(advert(24, 'd4'.repeat(32)));      // a node the server does not know
    const outsideWithHops = advert(26, C);  // dropped before any hop work
    outsideWithHops.path_json = '["AA"]';
    send(outsideWithHops);
    await flush(advert(25, A));
    assert.deepEqual(await shown(), [hash(1), hash(23), hash(25)]); checks++;
    assert.equal(memberRequests.length, 1, 'live packets fetch no member set'); checks++;
    assert.equal(packetRequests.length, before, 'live packets request no packets'); checks++;
    assert.equal(hopRequests.length, 0, 'a packet outside the area started hop resolution'); checks++;
    console.log('PASS: Area IN adds only its members\' live packets, with no request per packet');

    // --- A change of area fetches the new member set once. While it loads,
    // nothing is added, and the old set no longer answers.
    let releaseMembers;
    const memberHeld = new Promise(resolve => {
      memberHold = () => { resolve(); return new Promise(r => { releaseMembers = r; }); };
    });
    await pickArea('OTHER', { held: true });
    await memberHeld;
    send(advert(30, C));                    // a member of OTHER, but its set is loading
    send(advert(31, A));                    // a member of IN only
    // No packet can pass while the set loads, so no sentinel row can prove
    // the batch ran: wait out the 250 ms WS debounce (app.js) four times.
    await page.waitForTimeout(1000);
    assert(!(await cached()).includes(hash(30)), 'a packet was added while its member set loaded'); checks++;
    assert(!(await cached()).includes(hash(31)), 'the old member set answered for the new area'); checks++;
    releaseMembers();
    await membersReady();
    send(advert(32, A));
    await flush(advert(33, C));
    assert.deepEqual(await shown(), [hash(2), hash(33)]); checks++;
    assert.equal(memberRequests.length, 2); checks++;
    assert.equal(memberRequests[1].get('area'), 'OTHER'); checks++;
    console.log('PASS: changing the area fetches its member set once, and nothing slips in while it loads');

    // --- An area change while a packet awaits hop resolution: the packet is
    // checked again against the new area before it enters the list.
    let releaseHops;
    const hopHeld = new Promise(resolve => {
      hopHold = () => { resolve(); return new Promise(r => { releaseHops = r; }); };
    });
    await page.evaluate(() => {
      window.fixtureHopResolutions = 0;
      const resolve = HopResolver.resolve;
      HopResolver.resolve = function (...args) {
        window.fixtureHopResolutions++;
        return resolve.apply(this, args);
      };
    });
    const late = advert(40, C);
    late.path_json = '["AA"]';
    send(late);
    await hopHeld;
    await pickArea('IN');
    releaseHops();
    await page.waitForFunction(() => window.fixtureHopResolutions > 0);
    await flush(advert(41, A));
    assert(!(await cached()).includes(hash(40)), 'an old-area packet entered the list after hop resolution'); checks++;
    assert.deepEqual(await shown(), [hash(1), hash(41)]); checks++;
    assert.equal(memberRequests.length, 3); checks++;
    console.log('PASS: a packet from the old area is not added after its hops resolve');

    // --- An area with no positioned node filters nothing, as the server.
    await pickArea('EMPTY');
    assert.deepEqual(await shown(), [hash(1), hash(2), hash(3)]); checks++;
    send(groupText(50));
    send(advert(51, C));
    await flush(advert(52, 'e5'.repeat(32)));
    assert.deepEqual(await shown(), [hash(1), hash(2), hash(3), hash(50), hash(51), hash(52)]); checks++;
    assert.equal(memberRequests.length, 4); checks++;
    console.log('PASS: an area with no member filters nothing, as /api/packets?area= does');

    // --- Flat (ungrouped) mode and pause/resume apply the same rule, and a
    // reload with the same area fetches no member set.
    await pickArea('IN');
    assert.equal(memberRequests.length, 5); checks++;
    await page.locator('#fGroup').click();
    await page.waitForSelector('#pktLeft[data-loaded="true"]');
    assert.equal(packetRequests.at(-1).get('expand'), 'observations'); checks++;
    send(advert(60, C));
    await flush(advert(61, A));
    assert.deepEqual(await shown(), [hash(1), hash(61)]); checks++;
    await page.locator('#pktPauseBtn').click();
    send(advert(62, C));
    send(advert(63, B));
    await page.waitForFunction(() => document.querySelector('#pktPauseBtn').textContent.includes('2'));
    await page.locator('#pktPauseBtn').click();
    await page.waitForSelector('tr[data-hash="' + hash(63) + '"]');
    assert.deepEqual(await shown(), [hash(1), hash(61), hash(63)]); checks++;
    assert.equal(memberRequests.length, 5, 'grouping and pause fetch no member set'); checks++;
    console.log('PASS: flat mode and resumed traffic follow the area, with no new member set');

    // --- Back to All: unfiltered again, no fetch.
    await pickArea('__all__');
    send(advert(70, C));
    await flush(groupText(71));
    assert((await shown()).includes(hash(70))); checks++;
    assert.equal(memberRequests.length, 5, 'All fetches no member set'); checks++;
    console.log('PASS: All adds every live packet again');

    // --- A new visit with an area saved fetches its member set again, once.
    await pickArea('IN');
    assert.equal(memberRequests.length, 6); checks++;
    await page.evaluate(async () => {
      window.fixturePages.packets.destroy();
      await window.fixturePages.packets.init(document.getElementById('fixture'));
    });
    await page.waitForSelector('#pktLeft[data-loaded="true"]');
    await membersReady();
    assert.equal(memberRequests.length, 7, 'a new visit loads the member set once'); checks++;
    assert.equal(memberRequests[6].get('area'), 'IN'); checks++;
    send(advert(80, C));
    await flush(advert(81, A));
    assert(!(await shown()).includes(hash(80))); checks++;
    console.log('PASS: a new visit loads the saved area\'s member set once');

    assert.deepEqual(errors, []); checks++;
    await page.evaluate(() => window.fixturePages.packets.destroy());
    console.log(checks + ' checks passed');
  } finally { await browser.close(); }
}
main().catch(error => { console.error(error); process.exitCode = 1; });
