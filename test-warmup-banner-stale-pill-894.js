/* Unit tests for the stale-source pill in warmup-banner.js — issue #894
 *
 * Runs the REAL public/warmup-banner.js through vm.createContext with a small
 * fake DOM, fake timers, a fake clock and a fake sessionStorage.
 */
'use strict';
const vm = require('vm');
const fs = require('fs');
const path = require('path');
const assert = require('assert');

let passed = 0, failed = 0;
async function test(name, fn) {
  try { await fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}

const NOW = 1_700_000_000_000;
const SRC = fs.readFileSync(path.join(__dirname, 'public', 'warmup-banner.js'), 'utf8');

function makeNode(tag) {
  const n = {
    tagName: tag, id: '', className: '', attrs: {}, children: [], parentNode: null,
    style: {}, listeners: {}, _text: '', _html: '',
    get textContent() { return this._text + this.children.map(c => c.textContent).join(''); },
    set textContent(v) { this._text = String(v); this.children = []; },
    get innerHTML() { return this._html; },
    set innerHTML(v) { this._html = String(v); },
    classList: {
      _set: new Set(),
      add(c) { this._set.add(c); }, remove(c) { this._set.delete(c); },
      contains(c) { return this._set.has(c); },
    },
    setAttribute(k, v) { this.attrs[k] = String(v); },
    getAttribute(k) { return this.attrs[k]; },
    appendChild(c) { c.parentNode = this; this.children.push(c); return c; },
    insertBefore(c) { c.parentNode = this; this.children.unshift(c); return c; },
    removeChild(c) { const i = this.children.indexOf(c); if (i >= 0) this.children.splice(i, 1); c.parentNode = null; return c; },
    addEventListener(t, fn) { (this.listeners[t] = this.listeners[t] || []).push(fn); },
    click() { (this.listeners.click || []).forEach(fn => fn({ preventDefault() {}, stopPropagation() {} })); },
    getBoundingClientRect() { return { top: 0, bottom: 52, left: 0, right: 375, height: 52, width: 375 }; },
    get firstChild() { return this.children[0] || null; },
  };
  return n;
}
function walk(n, out) { out = out || []; out.push(n); n.children.forEach(c => walk(c, out)); return out; }

function boot(initial, opts) {
  opts = opts || {};
  let healthz = initial.healthz, header = initial.header || 'ready';
  let clock = NOW;
  const store = Object.assign({}, opts.store || {});
  const body = makeNode('body');
  const timers = { active: 0, started: 0, cleared: 0 };
  const doc = {
    readyState: 'complete', body,
    createElement: makeNode,
    addEventListener() {},
    querySelector() { return makeNode('nav'); },
  };
  const sessionStorage = opts.brokenStorage ? {
    getItem() { throw new Error('denied'); }, setItem() { throw new Error('denied'); },
  } : {
    getItem: (k) => (k in store ? store[k] : null),
    setItem: (k, v) => { store[k] = String(v); },
    removeItem: (k) => { delete store[k]; },
  };
  let fetches = 0;
  const ctx = {
    window: { sessionStorage }, document: doc, console, Math, Number, Object, String, JSON, isFinite, Array, Set,
    Date: { now: () => clock },
    sessionStorage,
    setTimeout: () => 0, clearTimeout() {},
    setInterval: () => { timers.active++; timers.started++; return timers.started; },
    clearInterval: () => { timers.active--; timers.cleared++; },
    Promise, module: { exports: {} },
  };
  ctx.fetch = function () {
    fetches++;
    return Promise.resolve({
      ok: true,
      headers: { get: (n) => (String(n).toLowerCase() === 'x-corescope-load-status' ? header : null) },
      json: () => Promise.resolve(healthz),
    });
  };
  ctx.window.fetch = ctx.fetch;
  vm.createContext(ctx);
  vm.runInContext(SRC, ctx);
  const api = ctx.window.__warmupBanner;
  const tick = async () => { for (let i = 0; i < 4; i++) await new Promise(r => setImmediate(r)); };
  return {
    api, body, store, timers, get fetches() { return fetches; },
    set(h, hd) { healthz = h; if (hd !== undefined) header = hd; },
    setClock(ms) { clock = ms; },
    async poll() { api._pollOnce(); await tick(); },
    pills() { return walk(body).filter(n => /warmup-pill\b/.test(n.className) && !/__/.test(n.className)); },
    pillText() { return this.pills().map(p => p.textContent); },
    banner() { return walk(body).find(n => n.id === 'warmup-banner'); },
  };
}

function hz(ageSec, extra) {
  return Object.assign({
    ready: true,
    from_pubkey_backfill: { done: true, processed: 1, total: 1 },
    ingest_liveness: { 'mqtt-eu': { lastReceiptUnix: NOW / 1000 - ageSec } },
  }, extra || {});
}

(async function main() {
  console.log('warmup-banner.js stale pill (#894):');
  const api = boot({ healthz: null }).api;

  // ---- separating warm-up from stale -------------------------------------
  await test('getWarmupOnlyMessages carries loading/backfill but never stale-source text', () => {
    const h = hz(3600, { ready: false, from_pubkey_backfill: { done: false, processed: 5, total: 10 } });
    const w = api.getWarmupOnlyMessages(h, 'loading', NOW);
    assert.strictEqual(w.length, 2, JSON.stringify(w));
    assert.ok(/Loading historical data/.test(w[0]) && /Backfilling pubkey index: 5 \/ 10 \(50%\)/.test(w[1]));
    assert.ok(!w.some(m => /No packets from/.test(m)));
  });
  await test('getStaleSources lists only sources past STALE_INGEST_MS, sorted, with whole minutes', () => {
    const h = hz(0, { ingest_liveness: {
      'b-src': { lastReceiptUnix: NOW / 1000 - 700 },
      'a-src': { lastReceiptUnix: NOW / 1000 - 3600 },
      'fresh': { lastReceiptUnix: NOW / 1000 - 10 },
      'never': {},
    } });
    const s = api.getStaleSources(h, NOW);
    assert.deepStrictEqual(JSON.parse(JSON.stringify(s)), [
      { source: 'a-src', ageMin: 60, text: 'No packets from a-src in 60 min.' },
      { source: 'b-src', ageMin: 11, text: 'No packets from b-src in 11 min.' },
    ]);
    assert.strictEqual(api.getStaleSources(null, NOW).length, 0);
  });
  await test('getWarmupMessages stays backward compatible (warm-up + stale text)', () => {
    const m = api.getWarmupMessages(hz(3600), 'ready', NOW);
    assert.deepStrictEqual(JSON.parse(JSON.stringify(m)), ['No packets from mqtt-eu in 60 min.']);
  });

  // ---- polling decision ----------------------------------------------------
  await test('shouldKeepPolling: stale keeps polling even in steady state', () => {
    assert.strictEqual(api.shouldKeepPolling(hz(3600), 'ready', NOW), true);
  });
  await test('shouldKeepPolling: steady and nothing stale stops', () => {
    assert.strictEqual(api.shouldKeepPolling(hz(10), 'ready', NOW), false);
  });
  await test('shouldKeepPolling: not ready / loading / backfill keep polling', () => {
    assert.strictEqual(api.shouldKeepPolling(hz(10, { ready: false }), 'ready', NOW), true);
    assert.strictEqual(api.shouldKeepPolling(hz(10), 'loading', NOW), true);
    assert.strictEqual(api.shouldKeepPolling(hz(10, { from_pubkey_backfill: { done: false } }), 'ready', NOW), true);
    assert.strictEqual(api.shouldKeepPolling(null, null, NOW), true);
  });

  // ---- dismissal bookkeeping ----------------------------------------------
  await test('pruneDismissed keeps only sources that are still stale', () => {
    const out = api.pruneDismissed({ a: true, b: true }, [{ source: 'b' }]);
    assert.deepStrictEqual(Object.keys(out), ['b']);
    assert.deepStrictEqual(Object.keys(api.pruneDismissed({ a: true }, [])), []);
  });

  // ---- DOM behaviour -------------------------------------------------------
  await test('stale source renders a compact pill, not the full-width banner', async () => {
    const env = boot({ healthz: hz(3600) });
    await env.poll();
    assert.deepStrictEqual(env.pillText().map(t => t.replace('×', '')), ['No packets from mqtt-eu in 60 min.']);
    assert.ok(env.banner().classList.contains('warmup-banner--hidden'), 'banner must stay hidden for stale-only');
    const btn = walk(env.body).find(n => n.tagName === 'button');
    assert.ok(btn && /×/.test(btn.textContent), 'pill has a × button');
    assert.ok(/mqtt-eu/.test(btn.getAttribute('aria-label')), 'accessible label names the source: ' + btn.getAttribute('aria-label'));
    assert.strictEqual(btn.getAttribute('type'), 'button');
  });
  await test('warm-up messages keep today\'s banner (and no pill) while loading', async () => {
    const env = boot({ healthz: hz(10, { ready: false }), header: 'loading' });
    await env.poll();
    assert.ok(!env.banner().classList.contains('warmup-banner--hidden'));
    assert.ok(/Loading historical data/.test(env.banner().children[0].children[0].innerHTML));
    assert.strictEqual(env.pills().length, 0);
  });
  await test('stale + loading: banner holds only warm-up text, pill holds the stale text', async () => {
    const env = boot({ healthz: hz(3600, { ready: false }), header: 'loading' });
    await env.poll();
    assert.ok(!/No packets/.test(env.banner().children[0].children[0].innerHTML));
    assert.strictEqual(env.pills().length, 1);
  });
  await test('keeps polling while stale (steady state) and the minute count moves', async () => {
    const env = boot({ healthz: hz(3600) });
    await env.poll();
    assert.strictEqual(env.timers.active, 1, 'interval must be running');
    assert.strictEqual(env.timers.cleared, 0, 'must not stop polling while stale');
    env.setClock(NOW + 5 * 60000);
    env.set(hz(3600)); // same lastReceipt => 5 more minutes
    await env.poll();
    assert.ok(/in 65 min\./.test(env.pillText()[0]), env.pillText()[0]);
    assert.strictEqual(env.pills().length, 1, 'updated in place, not duplicated');
  });
  await test('source recovers → pill gone by itself and polling stops', async () => {
    const env = boot({ healthz: hz(3600) });
    await env.poll();
    env.set(hz(5));
    await env.poll();
    assert.strictEqual(env.pills().length, 0);
    assert.strictEqual(env.timers.active, 0, 'interval cleared once nothing is stale');
  });
  await test('failed poll while stale keeps the pill and the timer', async () => {
    const env = boot({ healthz: hz(3600) });
    await env.poll();
    env.set(null);
    await env.poll();
    assert.strictEqual(env.pills().length, 1);
    assert.strictEqual(env.timers.active, 1);
  });
  await test('× dismisses that source, stored per session; stays dismissed while stale', async () => {
    const env = boot({ healthz: hz(3600) });
    await env.poll();
    walk(env.body).find(n => n.tagName === 'button').click();
    assert.strictEqual(env.pills().length, 0);
    assert.ok(/mqtt-eu/.test(env.store['corescope-warmup-dismissed'] || ''), JSON.stringify(env.store));
    env.setClock(NOW + 60000);
    await env.poll();
    assert.strictEqual(env.pills().length, 0, 'still dismissed on next poll');
    assert.strictEqual(env.timers.active, 1, 'still polling so recovery is noticed');
  });
  await test('dismissal resets on recovery: stale again → pill returns', async () => {
    const env = boot({ healthz: hz(3600) });
    await env.poll();
    walk(env.body).find(n => n.tagName === 'button').click();
    env.set(hz(5)); await env.poll();
    assert.ok(!/mqtt-eu/.test(env.store['corescope-warmup-dismissed'] || ''), 'stored dismissal cleared');
    env.setClock(NOW + 20 * 60000);
    env.set(hz(0, { ingest_liveness: { 'mqtt-eu': { lastReceiptUnix: NOW / 1000 - 10 * 60 } } }));
    await env.poll();
    assert.strictEqual(env.pills().length, 1, 'pill must return after recovery then new silence');
  });
  await test('dismissal is per source', async () => {
    const h = hz(0, { ingest_liveness: {
      'a-src': { lastReceiptUnix: NOW / 1000 - 3600 }, 'b-src': { lastReceiptUnix: NOW / 1000 - 3600 } } });
    const env = boot({ healthz: h });
    await env.poll();
    assert.strictEqual(env.pills().length, 2);
    walk(env.body).filter(n => n.tagName === 'button')[0].click();
    assert.deepStrictEqual(env.pillText().map(t => t.replace('×', '')), ['No packets from b-src in 60 min.']);
  });
  await test('a stored dismissal survives a fresh page load in the same session', async () => {
    const env = boot({ healthz: hz(3600) });
    await env.poll();
    walk(env.body).find(n => n.tagName === 'button').click();
    const saved = env.store['corescope-warmup-dismissed'];
    const env2 = boot({ healthz: hz(3600) }, { store: { 'corescope-warmup-dismissed': saved } });
    await env2.poll();
    assert.strictEqual(env2.pills().length, 0);
  });
  await test('blocked sessionStorage never throws; dismissal still works in memory', async () => {
    const env = boot({ healthz: hz(3600) }, { brokenStorage: true });
    await env.poll();
    assert.strictEqual(env.pills().length, 1);
    walk(env.body).find(n => n.tagName === 'button').click();
    await env.poll();
    assert.strictEqual(env.pills().length, 0);
  });
  await test('hostile source name is rendered as text, never HTML', async () => {
    const h = hz(0, { ingest_liveness: { '<img src=x onerror=1>': { lastReceiptUnix: NOW / 1000 - 3600 } } });
    const env = boot({ healthz: h });
    await env.poll();
    assert.ok(/<img src=x/.test(env.pillText()[0]));
    assert.ok(walk(env.body).every(n => n.tagName !== 'img'));
    assert.ok(walk(env.body).every(n => !/<img/.test(n.innerHTML)), 'no innerHTML carrying the name');
  });
  await test('pills are capped (rule 0): >3 stale sources show 3 pills + an overflow note', async () => {
    const live = {};
    for (let i = 0; i < 6; i++) live['s' + i] = { lastReceiptUnix: NOW / 1000 - 3600 };
    const env = boot({ healthz: hz(0, { ingest_liveness: live }) });
    await env.poll();
    assert.strictEqual(env.pills().length, 3);
    assert.ok(/\+3 more/.test(walk(env.body).map(n => n.textContent).join('|')));
  });

  console.log('\npassed=' + passed + ' failed=' + failed);
  process.exit(failed > 0 ? 1 : 0);
})();
