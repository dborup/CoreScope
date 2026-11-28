const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require('playwright');
let browser;
(async () => {
  browser = await chromium.launch({ headless: true });
  const page = await browser.newPage();
  const errors = [];
  page.on('pageerror', e => errors.push(e.message));
  await page.route('**/*', route => {
    const url = new URL(route.request().url());
    if (url.pathname === '/velkommen') return route.fulfill({ contentType: 'text/html', body: fs.readFileSync(path.join(__dirname, 'index.html'), 'utf8') });
    if (url.pathname.startsWith('/api/')) return route.fulfill({ contentType: 'application/json', body: url.pathname === '/api/stats' ? '{}' : '[]' });
    return route.abort(); // No real network, including payload URLs.
  });
  await page.goto('http://127.0.0.1:19876/velkommen');
  await page.waitForFunction(() => document.querySelector('#pkt-feed').textContent.includes('Ingen pakker'));
  const results = await page.evaluate(async () => {
    window.__xss = 0;
    const payload = '<img src="/xss-probe" onerror="window.__xss++">';
    const attr = '\" onmouseover=\"window.__xss++\" data-injected=\"yes';
    const now = new Date().toISOString();
    const findings = [];
    function check(label, fn) { try { fn(); } catch (e) { findings.push(label + ': ' + e.message); } }
    function inspect(id, literal) {
      const el = document.getElementById(id);
      if (el.querySelector('img,script,svg,iframe,[onerror],[onmouseover],[data-injected]')) throw new Error('HTML/attribute injection');
      if (literal && !el.textContent.includes(literal)) throw new Error('literal text lost');
    }
    renderNodes([{ name: payload, role: attr, relay_count_24h: payload, last_seen: now }]);
    check('nodes', () => inspect('node-tbody', payload));
    renderObservers([{ name: payload, packetsLastHour: payload, packet_count: payload, noise_floor: payload, uptime_secs: payload }]);
    check('observers', () => inspect('obs-list', payload));
    try { renderPackets([{ observer_name: payload, snr: payload, timestamp: now }]); }
    catch (e) { findings.push('packets throws on untrusted numeric field: ' + e.message); }
    check('packets', () => inspect('pkt-feed', payload));
    await new Promise(resolve => setTimeout(resolve, 100));
    if (window.__xss) findings.push('payload executed ' + window.__xss + ' times');
    const cases = [
      ['node role', 'node-tbody', () => renderNodes([{ name: 'safe', role: attr, last_seen: now }])],
      ['node count', 'node-tbody', () => renderNodes([{ name: 'safe', relay_count_24h: payload, last_seen: now }])],
      ['node fallback', 'node-tbody', () => renderNodes([{ public_key: payload, last_seen: now }])],
      ['observer iata', 'obs-list', () => renderObservers([{ iata: payload }])],
      ['observer count', 'obs-list', () => renderObservers([{ name: 'safe', packet_count: payload }])],
      ['observer noise', 'obs-list', () => renderObservers([{ name: 'safe', noise_floor: payload }])],
      ['observer rate', 'obs-list', () => renderObservers([{ name: 'safe', packetsLastHour: payload }])],
      ['packet name', 'pkt-feed', () => renderPackets([{ observer_name: payload, snr: 1 }])],
      ['packet iata', 'pkt-feed', () => renderPackets([{ observer_iata: payload, snr: 1 }])]
    ];
    for (const [label, id, render] of cases) check(label, () => { render(); inspect(id); });
    // Names containing quotes, ampersands and Danish characters must round-trip.
    const label = 'AAR & BLL <b>Æøå</b> "quoted" \'single\'';
    renderNodes([{ name: label, role: 'repeater', relay_count_24h: 3, last_seen: now }]);
    check('normal node', () => {
      inspect('node-tbody', label);
      if (!document.querySelector('.role-badge.repeater')) throw new Error('role styling lost');
    });
    renderObservers([{ name: label, packetsLastHour: 3, packet_count: 12, noise_floor: -110, uptime_secs: 86400 }]);
    check('normal observer', () => inspect('obs-list', label));
    try { renderPackets([{ observer_name: label, snr: '8.5', timestamp: now, decoded_json: '{"type":"ADVERT"}' }]); }
    catch (e) { findings.push('numeric string SNR throws: ' + e.message); }
    check('normal packet', () => {
      inspect('pkt-feed', label);
      if (!document.querySelector('.pkt-snr').textContent.includes('8.5dB')) throw new Error('numeric string SNR not formatted');
    });
    return findings;
  });
  assert.deepEqual(results, []);
  assert.deepEqual(errors, []);
  console.log('PASS: all three renderers resist HTML/attribute injection; text and valid formatting preserved');
})().catch(e => { console.error(e); process.exitCode = 1; }).finally(async () => { if (browser) await browser.close(); });
