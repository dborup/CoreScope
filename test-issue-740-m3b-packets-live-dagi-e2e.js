/* #740 M3b: the Danish areas (DAGI) filter on the Packets page.
 *
 * The REST list carries the checked DAGI param, and live WebSocket packets
 * follow it by the server's rule (cmd/server dagi_api.go): the ADVERT sender
 * (public_key, else pubKey) is a member of the selection; a packet with no
 * sender is dropped; a known code nobody is in matches nothing
 * (dagiMembershipSnapshot.matching). The member set is fetched once per
 * selection change, never per packet, and an unknown or not-yet-checked code
 * is never sent (M3a's validation gate), so the empty set only ever means a
 * real empty area.
 *
 * The real packets page, dagi-filter.js picker (driven by clicks),
 * scope-member-filter.js, app.js API helper, WebSocket and debounce run.
 * Every request is fulfilled here, including the WebSocket; no server,
 * external service or real mesh data is used.
 *
 * Usage: node test-issue-740-m3b-packets-live-dagi-e2e.js
 */
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require('playwright');

const ORIGIN = 'http://127.0.0.1:18740';
const PUB = path.join(__dirname, 'public');
const DAGI = ['kommune', 'landsdel', 'dkregion', 'postnr'];
const hash = id => id.toString(16).padStart(16, '0');
const A = 'a1'.repeat(32), B = 'b2'.repeat(32), C = 'c3'.repeat(32);
// Members per code, as /api/nodes?kommune=<code> answers them. 0101 is a
// known kommune nobody is in: /api/nodes answers nodes: null.
const MEMBERS = { '0751': [A, B], '0860': [C], '0101': [] };
const CONFIG = {
  enabled: true,
  credit: 'Indeholder data fra Klimadatastyrelsen, DAGI, november 2026, CC BY 4.0 — data bearbejdet',
  source: { licenseUrl: 'https://creativecommons.org/licenses/by/4.0/' },
  layers: [{ layer: 'kommune', param: 'kommune', areas: [
    { code: '0101', name: 'København' }, { code: '0751', name: 'Aarhus' }, { code: '0860', name: 'Hjørring' }] }],
};

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

// The server's rule for the initial list, written out for these packets.
const initial = [advert(1, A), advert(2, C), groupText(3)];
const initialFor = code => code === '' ? initial
  : initial.filter(p => (MEMBERS[code] || []).includes(JSON.parse(p.decoded_json).public_key));
const dagiOf = q => DAGI.filter(k => q.has(k)).map(k => k + '=' + q.get(k)).join('&');

async function main() {
  const browser = await chromium.launch({ headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined });
  let checks = 0;
  try {
    const page = await browser.newPage({ viewport: { width: 1024, height: 900 } });
    page.setDefaultTimeout(10000);
    const errors = [], packetRequests = [], memberRequests = [], allDagi = [];
    page.on('pageerror', error => errors.push(error.message));
    let socket, configHold = null, configStatus = 200;
    await page.routeWebSocket('**', ws => { socket = ws; });
    await page.route('**/*', async route => {
      const url = new URL(route.request().url());
      if (url.origin !== ORIGIN) return route.abort();
      const q = url.searchParams;
      if (dagiOf(q)) allDagi.push(url.pathname + '?' + dagiOf(q));
      if (url.pathname === '/api/config/dagi') {
        if (configHold) await configHold;
        if (configStatus !== 200) return route.fulfill({ status: configStatus, json: { error: 'down' } });
        return route.fulfill({ json: CONFIG });
      }
      if (url.pathname === '/api/packets') {
        packetRequests.push(q);
        const packets = initialFor(q.get('kommune') || '');
        return route.fulfill({ json: { packets, total: packets.length,
          ...(q.has('kommune') ? { dagi: { unpositionedHidden: 4 } } : {}) } });
      }
      if (url.pathname === '/api/observers') return route.fulfill({ json: { observers: [
        { id: 'fixture-observer', name: 'Fixture observer', iata: 'AAA' }] } });
      if (url.pathname === '/api/config/areas') return route.fulfill({ json: [] });
      if (url.pathname === '/api/nodes' && dagiOf(q)) {
        memberRequests.push(q);
        const list = MEMBERS[q.get('kommune')] || [];
        const offset = Number(q.get('offset') || 0);
        if (!list.length) return route.fulfill({ json: { nodes: null, total: 0, dagi: { unpositionedHidden: 4 } } });
        const nodes = offset ? [] : list.map(pk => ({ public_key: pk, name: 'Node ' + pk.slice(0, 4), role: 'repeater' }));
        return route.fulfill({ json: { nodes, total: nodes.length, dagi: { unpositionedHidden: 4 } } });
      }
      if (url.pathname.startsWith('/api/')) return route.fulfill({ json: {} });
      if (url.pathname === '/icons/phosphor-sprite.svg') return route.fulfill({
        contentType: 'image/svg+xml', body: fs.readFileSync(path.join(PUB, 'icons/phosphor-sprite.svg')) });
      return route.fulfill({ contentType: 'text/html', body:
        '<!doctype html><html><body><main id="fixture"></main></body></html>' });
    });

    // A cold deep link, with the config answer held: nothing is sent until
    // the code is checked.
    // A fresh page session on `hash`: the scripts, then the router's
    // DagiFilter.syncFromUrl, then the page's init().
    async function boot(hash) {
      await page.goto('about:blank');
      await page.goto(ORIGIN + '/' + hash);
      await page.evaluate(() => { try { localStorage.clear(); } catch (e) {} });
      await page.addStyleTag({ path: path.join(PUB, 'style.css') });
      for (const file of ['payload-labels.js', 'roles.js', 'app.js', 'url-state.js', 'packet-helpers.js', 'hop-resolver.js',
        'hop-display.js', 'scope-member-filter.js', 'area-filter.js', 'dagi-filter.js']) {
        await page.addScriptTag({ path: path.join(PUB, file) });
      }
      await page.evaluate(() => {
        // Only the unrelated region control is stubbed. The DAGI control is
        // the real one and is driven by clicks.
        window.RegionFilter = { init(){}, onChange(){}, offChange(){}, setSelected(){}, getRegionParam(){ return ''; },
          placeMenu(){}, watchMenu(){ return function () {}; } };
        window.fixturePages = {};
        registerPage = (name, mod) => { window.fixturePages[name] = mod; };
      });
      await page.addScriptTag({ path: path.join(PUB, 'packets.js') });
      await page.evaluate(() => {
        DagiFilter.syncFromUrl(location.hash); // the router does this before init()
        window.fixtureInit = window.fixturePages.packets.init(document.getElementById('fixture'));
        connectWS();
      });
    }

    let releaseConfig;
    configHold = new Promise(r => { releaseConfig = r; });
    await boot('#/packets?kommune=0751');
    await page.waitForTimeout(400);
    assert.equal(packetRequests.length, 0, 'a packets request went out before the code was checked'); checks++;
    assert.equal(memberRequests.length, 0, 'a member request went out before the code was checked'); checks++;
    releaseConfig();
    configHold = null;
    await page.evaluate(() => window.fixtureInit);
    await page.waitForSelector('#pktLeft[data-loaded="true"]');

    const shown = async () => (await page.locator('#pktBody tr[data-hash]')
      .evaluateAll(rows => rows.map(r => r.dataset.hash))).sort();
    const send = p => socket.send(JSON.stringify({ type: 'packet', data: {
      packet: p, decoded: { header: { payloadTypeName: 'ADVERT' }, payload: {} } } }));
    async function flush(p) {
      send(p);
      await page.waitForSelector('tr[data-hash="' + p.hash + '"]');
      return p.hash;
    }
    const membersReady = () => page.waitForFunction(() => window._packetsTestAPI._dagiMembersReady());
    const loadedWith = code => page.waitForResponse(r => {
      const url = new URL(r.url());
      return url.pathname === '/api/packets' && (url.searchParams.get('kommune') || '') === code;
    });
    async function pick(name, code) {
      const loaded = loadedWith(code);
      await page.locator('#packetsDagiFilter .dagi-trigger').click();
      await page.locator('#packetsDagiFilter .dagi-search').fill(name.slice(0, 3));
      await page.locator('#packetsDagiFilter .dagi-option', { hasText: name }).first().click();
      await loaded;
      await page.waitForSelector('#pktLeft[data-loaded="true"]');
      await membersReady();
    }
    const notice = () => page.evaluate(() => {
      const el = document.getElementById('packetsDagiNotice');
      return el.hidden ? '' : el.textContent;
    });

    // --- The REST param, the member set, and the note.
    assert.equal(packetRequests.length, 1); checks++;
    assert.equal(packetRequests[0].get('kommune'), '0751', 'the REST list lacks the DAGI param'); checks++;
    await membersReady();
    assert.equal(memberRequests.length, 1, 'one member set per selection'); checks++;
    assert.equal(memberRequests[0].get('kommune'), '0751'); checks++;
    assert.equal(memberRequests[0].get('geoFilter'), '0', 'the packets rule ignores geo_filter'); checks++;
    assert.deepEqual(await shown(), [hash(1)]); checks++;
    assert.match(await notice(), /only adverts are shown/); checks++;
    assert.match(await notice(), /4 nodes have no position/); checks++;
    assert.match(await page.evaluate(() => location.hash), /[?&]kommune=0751(&|$)/); checks++;
    console.log('PASS: a cold link sends nothing until checked, then the REST param, one member set and the note');

    // --- Live: only members' ADVERTs are added; no request per packet.
    const before = packetRequests.length;
    send(advert(20, C));                    // outside
    send(groupText(21));                    // no sender
    send(packet(22, '{bad json', 4));       // no sender
    send(advertPubKey(23, B));              // a member, by pubKey
    await flush(advert(25, A));             // a member
    assert.deepEqual(await shown(), [hash(1), hash(23), hash(25)]); checks++;
    assert.equal(memberRequests.length, 1, 'live packets fetch no member set'); checks++;
    assert.equal(packetRequests.length, before, 'live packets request no packets'); checks++;
    console.log('PASS: live packets in (members) and out (others, no sender)');

    // --- A selection change fetches its member set once; the URL follows.
    await pick('Hjørring', '0860');
    assert.equal(memberRequests.length, 2, 'a selection change refreshes the set once'); checks++;
    assert.equal(memberRequests[1].get('kommune'), '0860'); checks++;
    assert.match(await page.evaluate(() => location.hash), /[?&]kommune=0860(&|$)/); checks++;
    assert.deepEqual(await shown(), [hash(2)]); checks++;
    send(advert(30, A));
    await flush(advert(31, C));
    assert.deepEqual(await shown(), [hash(2), hash(31)]); checks++;
    // A reload with the same selection fetches no member set.
    const reloaded = loadedWith('0860');
    await page.locator('[data-action="pkt-refresh"]').click();
    await reloaded;
    await page.waitForSelector('#pktLeft[data-loaded="true"]');
    assert.equal(memberRequests.length, 2, 'a reload with the same selection refetched the member set'); checks++;
    console.log('PASS: a selection change refreshes the member set once, and a reload does not');

    // --- A known area nobody is in matches nothing, live as in REST.
    await pick('København', '0101');
    assert.equal(memberRequests.length, 3); checks++;
    assert.deepEqual(await shown(), []); checks++;
    send(advert(40, A));
    send(advert(41, C));
    send(groupText(42));
    await page.waitForTimeout(1000); // four 250 ms WS debounces: no sentinel can pass
    assert.deepEqual(await page.evaluate(() => window._packetsTestAPI._packetHashes()), [], 'an empty area let a packet in'); checks++;
    console.log('PASS: an empty DAGI area matches nothing, as /api/packets does');

    // --- Clear: unfiltered, no member request, no note, param gone.
    const cleared = loadedWith('');
    await page.locator('#packetsDagiFilter .dagi-clear').click();
    await cleared;
    await page.waitForSelector('#pktLeft[data-loaded="true"]');
    send(advert(50, C));
    await flush(groupText(51));
    assert((await shown()).includes(hash(50))); checks++;
    assert.equal(memberRequests.length, 3, 'a clear fetched a member set'); checks++;
    assert.equal(await notice(), ''); checks++;
    assert.doesNotMatch(await page.evaluate(() => location.hash), /kommune=/); checks++;
    console.log('PASS: clearing the filter adds every live packet again');

    // --- Leave and re-enter with the choice saved: one member set, once.
    await pick('Aarhus', '0751');
    assert.equal(memberRequests.length, 4); checks++;
    await page.evaluate(async () => {
      window.fixturePages.packets.destroy();
      await window.fixturePages.packets.init(document.getElementById('fixture'));
    });
    await page.waitForSelector('#pktLeft[data-loaded="true"]');
    await membersReady();
    assert.equal(memberRequests.length, 5, 'a new visit loads the member set once'); checks++;
    send(advert(60, C));
    await flush(advert(61, A));
    assert(!(await shown()).includes(hash(60))); checks++;
    assert.equal(await page.locator('.dagi-filter-wrap').count(), 1, 'a DAGI control leaked across visits'); checks++;
    assert.equal(await page.locator('.dagi-panel').count(), 0, 'a picker panel leaked across visits'); checks++;
    console.log('PASS: a new visit loads the saved selection\'s member set once');

    // --- An unknown code from a link is never sent, and filters nothing.
    await page.evaluate(() => window.fixturePages.packets.destroy());
    const sentBefore = allDagi.length;
    await page.evaluate(async () => {
      history.replaceState(null, '', '#/packets?kommune=9999');
      DagiFilter.syncFromUrl(location.hash);
      await window.fixturePages.packets.init(document.getElementById('fixture'));
    });
    await page.waitForSelector('#pktLeft[data-loaded="true"]');
    assert.deepEqual(allDagi.slice(sentBefore), [], 'an unknown code was sent'); checks++;
    assert.equal(packetRequests.at(-1).get('kommune'), null); checks++;
    assert.doesNotMatch(await page.evaluate(() => location.hash), /kommune=/, 'the unknown code stayed in the URL'); checks++;
    send(advert(70, C));
    await flush(groupText(71));
    assert((await shown()).includes(hash(70)), 'an unknown code filtered live packets'); checks++;
    assert.equal(await notice(), ''); checks++;
    console.log('PASS: an unknown code is dropped, never sent, and filters nothing');

    // --- The config cannot be loaded: the link's code is kept but never
    // sent (M3a), so the list is unfiltered, and live packets must be too.
    await page.evaluate(() => window.fixturePages.packets.destroy());
    configStatus = 500;
    const membersBefore = memberRequests.length, dagiBefore = allDagi.length;
    await boot('#/packets?kommune=0751');
    await page.evaluate(() => window.fixtureInit);
    await page.waitForSelector('#pktLeft[data-loaded="true"]');
    assert.equal(packetRequests.at(-1).get('kommune'), null, 'an unchecked code was sent'); checks++;
    assert.equal(memberRequests.length, membersBefore, 'a member set was fetched for an unchecked code'); checks++;
    assert.deepEqual(allDagi.slice(dagiBefore), [], 'an unchecked code was sent'); checks++;
    assert.deepEqual(await shown(), [hash(1), hash(2), hash(3)], 'the list is not the unfiltered one'); checks++;
    send(advert(80, C));
    await flush(groupText(81));
    assert((await shown()).includes(hash(80)), 'live packets are filtered by a code the list was not filtered by'); checks++;
    assert.equal(await notice(), '', 'a note for an unsent filter'); checks++;
    console.log('PASS: with the config unavailable, the kept code is never sent and filters nothing, live as in REST');

    assert.deepEqual(errors, []); checks++;
    await page.evaluate(() => window.fixturePages.packets.destroy());
    console.log(checks + ' checks passed');
  } finally { await browser.close(); }
}
main().catch(error => { console.error(error); process.exitCode = 1; });
