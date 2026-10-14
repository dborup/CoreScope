/* #406: real packets DOM + app.js WebSocket batching with deterministic API
 * fixtures. Browser requests are fulfilled locally, including the WebSocket;
 * no running server, external service or real mesh data is used. */
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require('playwright');

const ORIGIN = 'http://127.0.0.1:18406';
const PUB = path.join(__dirname, 'public');
const hash = id => id.toString(16).padStart(16, '0');
function packet(id, decoded, type = 5) {
  return { id, hash: hash(id), payload_type: type, route_type: 1,
    timestamp: new Date().toISOString(), latest: new Date().toISOString(),
    path_json: '[]', decoded_json: typeof decoded === 'string' ? decoded : JSON.stringify({ type: 'CHAN', ...decoded }),
    observer_id: 'fixture-observer', observer_name: 'Fixture observer',
    count: 1, observation_count: 1, raw_hex: '1500', payload_length: 1 };
}
// Deliberately independently specified fixture lookup: it is NOT the production
// predicate under test. It models the backend's exact-name OR enc_<HEX> contract.
const initial = [packet(1, { channel: '#one', channelHashHex: 'AB' }),
  packet(2, { channel: '#two', channelHashHex: 'CD' }),
  packet(3, { channelHashHex: 'AB' }),
  // SQL fallback can select a row using t.channel_hash even when decoded_json
  // is sparse/null. REST does not expose that indexed column. Its selection
  // stays authoritative: the frontend must not re-filter this HTTP result.
  { ...packet(4, {}), decoded_json: null }];
const matches = { '#one': [1, 4], '#two': [2], enc_AB: [1, 3], enc_CD: [2] };

async function main() {
  const browser = await chromium.launch({ headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined });
  let checks = 0;
  try {
    // 1024 selects the real mobile cap of 1000 packets, without modifying JS.
    const page = await browser.newPage({ viewport: { width: 1024, height: 900 } });
    page.setDefaultTimeout(10000);
    const errors = [], requests = [];
    page.on('pageerror', error => errors.push(error.message));
    let socket, releaseNodes, nodesHeld;
    await page.routeWebSocket('**', ws => { socket = ws; });
    await page.route('**/*', async route => {
      const url = new URL(route.request().url());
      if (url.origin !== ORIGIN) return route.abort();
      if (url.pathname === '/api/packets') {
        requests.push(url.searchParams);
        let ids = matches[url.searchParams.get('channel')] || [1, 2, 3, 4];
        if (url.searchParams.has('hash')) ids = [Number.parseInt(url.searchParams.get('hash'), 16)];
        const packets = initial.filter(p => ids.includes(p.id));
        return route.fulfill({ json: { packets, total: packets.length } });
      }
      if (url.pathname === '/api/channels') return route.fulfill({ json: { channels:
        ['#one', '#two', 'enc_AB', 'enc_CD'].map(h => ({ hash: h, name: h })) } });
      if (url.pathname === '/api/observers') return route.fulfill({ json: { observers: [
        { id: 'fixture-observer', name: 'Fixture observer', iata: 'AAA' }] } });
      if (url.pathname === '/api/nodes') {
        if (nodesHeld) {
          nodesHeld();
          await new Promise(resolve => { releaseNodes = resolve; });
        }
        return route.fulfill({ json: { nodes: [{ public_key: 'aa'.repeat(32), name: 'Fixture relay', role: 'repeater' }], total: 1 } });
      }
      if (url.pathname.startsWith('/api/')) return route.fulfill({ json: {} });
      if (url.pathname === '/icons/phosphor-sprite.svg') return route.fulfill({
        contentType: 'image/svg+xml', body: fs.readFileSync(path.join(PUB, 'icons/phosphor-sprite.svg')) });
      return route.fulfill({ contentType: 'text/html', body:
        '<!doctype html><html><body><main id="fixture"></main></body></html>' });
    });
    await page.goto(ORIGIN + '/#/packets?channel=%23one');
    await page.addStyleTag({ path: path.join(PUB, 'style.css') });
    for (const file of ['payload-labels.js', 'roles.js', 'app.js', 'packet-helpers.js', 'hop-resolver.js', 'hop-display.js']) {
      await page.addScriptTag({ path: path.join(PUB, file) });
    }
    await page.evaluate(() => {
      // Only unrelated region/area controls are stubbed. The actual packets
      // page, filter select, API helper, WebSocket handler and debounce run.
      window.RegionFilter = { init(){}, onChange(){}, offChange(){}, setSelected(){}, getRegionParam(){ return ''; } };
      window.AreaFilter = { init(){}, onChange(){}, offChange(){}, getAreaParam(){ return ''; } };
      window.fixturePages = {};
      registerPage = (name, mod) => { window.fixturePages[name] = mod; };
    });
    await page.addScriptTag({ path: path.join(PUB, 'packets.js') });
    await page.evaluate(async () => {
      await window.fixturePages.packets.init(document.getElementById('fixture'));
      connectWS();
    });
    await page.waitForSelector('#pktLeft[data-loaded="true"]');
    const shown = () => page.locator('#pktBody tr[data-hash]').evaluateAll(rows => rows.map(r => r.dataset.hash));
    const send = p => socket.send(JSON.stringify({ type: 'packet', data: {
      // This is the Go poller's shape: packets.js reads data.packet, not
      // live.js's top-level decoded.payload object.
      packet: p, decoded: { header: { payloadTypeName: 'GRP_TXT' }, payload: { channel: '#decoy' } }
    } }));
    async function select(channel) {
      if (!(await page.locator('#fChannel').isVisible())) {
        await page.locator('#filterToggleBtn, .filter-toggle-btn-mirror').filter({ visible: true }).first().click();
      }
      const response = page.waitForResponse(r => {
        const url = new URL(r.url());
        return url.pathname === '/api/packets' && (url.searchParams.get('channel') || '') === channel;
      });
      await page.locator('#fChannel').selectOption(channel);
      await response;
      await page.waitForSelector('#pktLeft[data-loaded="true"]');
    }
    async function flush(accepted) {
      send(accepted);
      await page.waitForSelector('tr[data-hash="' + accepted.hash + '"]');
    }
    assert.deepEqual((await shown()).sort(), [hash(1), hash(4)]); checks++;
    assert.equal(requests.at(-1).get('channel'), '#one'); checks++;
    console.log('PASS: authoritative selected-channel HTTP row remains visible with null decoded metadata');
    // The accepted sentinel lets us observe completion of the same real WS
    // batch without a sleep or a false passing negative assertion.
    const before = requests.length;
    send(packet(10, { channel: '#two' }));
    send(packet(11, {}));
    send(packet(12, '{bad json'));
    send(packet(13, { channel: '#one' }, 4));
    await flush(packet(14, { channel: '#one', text: 'matching live packet' }));
    assert.deepEqual((await shown()).sort(), [hash(1), hash(4), hash(14)]); checks++;
    assert.equal(requests.length, before, 'live channel matching must not request packets'); checks++;
    console.log('PASS: named filter rejects other/missing/malformed/non-channel packets before live insertion');

    // More off-channel arrivals than the actual page cap must not evict
    // matching history, even though a render-only filter could hide them.
    for (let i = 1000; i < 2001; i++) send(packet(i, { channel: '#two' }));
    await flush(packet(2001, { channel: '#one' }));
    assert((await shown()).includes(hash(1)), 'off-channel traffic evicted matching history'); checks++;
    assert((await shown()).includes(hash(4)), 'unrelated live traffic removed authoritative sparse HTTP row'); checks++;
    assert.equal(requests.length, before, 'channel admission added a per-packet refetch'); checks++;
    console.log('PASS: over-cap off-channel live traffic cannot evict matching history');

    // Both observations of a matching transmission still update its group.
    send(packet(1, { channel: '#one', text: 'updated matching group' }));
    await flush(packet(15, { channel: '#one' }));
    await page.waitForFunction(() => /updated matching group/.test(document.querySelector('tr[data-hash="0000000000000001"]').textContent));
    assert.equal((await shown()).filter(h => h === hash(1)).length, 1); checks++;
    console.log('PASS: a matching group updates in place');

    await select('enc_AB');
    assert.deepEqual((await shown()).sort(), [hash(1), hash(3)]); checks++;
    send(packet(20, { channel: '#two', channelHashHex: 'CD' }));
    send(packet(21, { channelHash: 171 })); // Do not invent channelHashHex evidence.
    send(packet(22, { channelHashHex: 'ab' })); // Backend exact spelling.
    send(packet(23, { channel: '#renamed', channelHashHex: 'AB' }));
    await flush(packet(24, { channelHashHex: 'AB' }));
    assert.deepEqual((await shown()).sort(), [hash(1), hash(3), hash(23), hash(24)]); checks++;
    console.log('PASS: encrypted-hash filter also matches readable payloads, preserving exact backend semantics');

    // Pause uses the real bounded pauseBuffer, then admission consults the
    // current select when the queued packets resume rather than the old one.
    await page.locator('#pktPauseBtn').click();
    send(packet(30, { channelHashHex: 'AB' }));
    send(packet(31, { channel: '#two', channelHashHex: 'CD' }));
    await page.waitForFunction(() => document.querySelector('#pktPauseBtn').textContent.includes('2'));
    await select('#two');
    await page.locator('#pktPauseBtn').click();
    await page.waitForSelector('tr[data-hash="' + hash(31) + '"]');
    assert.deepEqual((await shown()).sort(), [hash(2), hash(31)]); checks++;
    assert.equal(requests.at(-1).get('channel'), '#two'); checks++;
    console.log('PASS: changing channel while paused applies the new filter to resumed traffic');

    // Initializing the real hop resolver awaits a bulk nodes request. Change
    // the selected channel during that await; the old update must not enter
    // the cache when resolution resumes (not merely be hidden at rendering).
    const held = new Promise(resolve => { nodesHeld = resolve; });
    await page.evaluate(() => {
      window.fixtureHopResolutions = 0;
      const resolve = HopResolver.resolve;
      HopResolver.resolve = function(...args) {
        window.fixtureHopResolutions++;
        return resolve.apply(this, args);
      };
    });
    const delayed = packet(35, { channel: '#two' });
    delayed.path_json = '["AA"]';
    send(delayed);
    let heldTimeout;
    try {
      await Promise.race([held, new Promise((_, reject) => {
        heldTimeout = setTimeout(() => reject(new Error('bulk hop request was not held')), 10000);
      })]);
    } finally { clearTimeout(heldTimeout); }
    await select('#one');
    releaseNodes();
    nodesHeld = null;
    await page.waitForFunction(() => window.fixtureHopResolutions > 0);
    await flush(packet(36, { channel: '#one' }));
    // The production test hook only snapshots internal cache identity; no
    // test packet enters the state through a hook.
    const cache = await page.evaluate(() => window._packetsTestAPI._packetHashes());
    assert(!cache.includes(hash(35)), 'old-channel packet entered cache after asynchronous hop resolution'); checks++;
    assert.deepEqual((await shown()).sort(), [hash(1), hash(4), hash(36)]); checks++;
    console.log('PASS: channel change during hop initialization rejects stale update before caching');

    await select('#two');

    await page.locator('#fGroup').click();
    await page.waitForSelector('#pktLeft[data-loaded="true"]');
    assert.equal(requests.at(-1).get('expand'), 'observations'); checks++;
    send(packet(40, { channel: '#one' }));
    await flush(packet(41, { channel: '#two' }));
    assert.deepEqual((await shown()).sort(), [hash(2), hash(41)]); checks++;
    console.log('PASS: flat observation mode applies the same live channel filter');

    // Hash pin deliberately bypasses saved channel, as it does in REST.
    await page.locator('#fHash').fill(hash(1));
    await page.waitForFunction(() => document.querySelector('#pktBody tr[data-hash]')?.dataset.hash === '0000000000000001');
    assert.equal(requests.at(-1).get('channel'), null); checks++;
    send(packet(1, { channel: '#one', text: 'pinned hash live' }));
    await page.waitForFunction(() => /pinned hash live/.test(document.querySelector('#pktBody').textContent));
    assert((await shown()).every(h => h === hash(1))); checks++;
    console.log('PASS: a pinned hash bypasses channel matching');
    await page.locator('#fHash').fill('');
    await page.waitForFunction(() => !location.hash.includes('hash='));
    await select('');
    send(packet(60, {}, 4));
    await flush(packet(61, {}));
    assert((await shown()).includes(hash(60)), 'empty channel selection must pass non-GRP traffic'); checks++;
    assert((await shown()).includes(hash(61)), 'empty channel selection must pass missing channel fields'); checks++;
    console.log('PASS: All Channels keeps normal unfiltered live traffic');
    assert.deepEqual(errors, []); checks++;
    await page.evaluate(() => window.fixturePages.packets.destroy());
    console.log(checks + ' checks passed');
  } finally { await browser.close(); }
}
main().catch(error => { console.error(error); process.exitCode = 1; });
