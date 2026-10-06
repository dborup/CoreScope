/**
 * Tests for public/packet-path-map.js — the on-demand "View path" modal
 * that draws a packet's resolved relay path on a Leaflet map (backed by
 * GET /api/packets/{hash}/path, cmd/server/db.go GetPacketPath).
 *
 * Two layers, matching this repo's established pattern for modal/DOM
 * code (see test-channel-modal-ux.js): string-contract checks over the
 * raw source for structural/safety properties, plus a functional smoke
 * test using a minimal-but-real DOM mock (createElement/appendChild/
 * getElementById/remove all actually work, unlike the channels.js test
 * sandbox's inert stubs) to exercise open()/close() end-to-end on the
 * two code paths that don't need Leaflet: a failed fetch, and a
 * fetch that resolves with nothing plottable.
 */
'use strict';

const vm = require('vm');
const fs = require('fs');
const assert = require('assert');

const src = fs.readFileSync('public/packet-path-map.js', 'utf8');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}

console.log('\n=== packet-path-map.js: string-contract checks ===');

test('exports window.PacketPathMap.{open,close,restore}', () => {
  assert.ok(/window\.PacketPathMap\s*=\s*\{\s*open:\s*open,\s*close:\s*close,\s*restore:\s*restore\s*\}/.test(src));
});

test('fetches via the shared api() helper, not a raw fetch (picks up auth/base-URL handling)', () => {
  assert.ok(/api\(\s*'\/packets\/'\s*\+\s*encodeURIComponent\(hash\)\s*\+\s*'\/path'\s*\)/.test(src));
});

test('escapes node/observer names before interpolating into tooltip HTML (operator-controlled data)', () => {
  assert.ok(/escapeHtml\(pt\.name\)/.test(src), 'point tooltips must escape the name');
});

test('tooltips use a wrapping CSS class -- Leaflet\'s default nowrap tooltip becomes unreadable once role/approx/distance/timing info are all combined', () => {
  const bindCalls = (src.match(/\.bindTooltip\(/g) || []).length;
  const classNameUses = (src.match(/className:\s*'packet-path-tooltip'/g) || []).length;
  assert.ok(bindCalls > 0, 'expected at least one bindTooltip call');
  assert.strictEqual(classNameUses, bindCalls, `expected every bindTooltip call (${bindCalls}) to pass the wrapping class, found ${classNameUses}`);
});

test('draws every branch, not just the deepest one', () => {
  assert.ok(/branches\.map/.test(src), 'should iterate all branches from the response');
});

test('draws highlighted branch(es) on top of the others', () => {
  assert.ok(/a\.highlightRole \? 1 : 0/.test(src), 'should reorder so highlighted (farthest/deepest) branches paint last');
});

test('handles Escape key and click-outside to close, matching other CoreScope modals', () => {
  assert.ok(/e\.key [!=]== 'Escape'/.test(src));
  assert.ok(/e\.target === overlay/.test(src));
});

test('degrades gracefully when the Leaflet global is unavailable, rather than throwing', () => {
  assert.ok(/typeof L === 'undefined'/.test(src));
});

test('close() tears down the Leaflet map instance, not just the DOM overlay (avoids a leaked map on repeat opens)', () => {
  assert.ok(/activeMap\.remove\(\)/.test(src));
});

test('the "Copy link" button builds a #/packets/<hash>?viewPath=1 URL and copies via window.copyToClipboard', () => {
  assert.ok(/id="packetPathCopyLink"/.test(src), 'expected a packetPathCopyLink button in the modal');
  assert.ok(/'\/#\/packets\/'\s*\+\s*encodeURIComponent\(hash\)\s*\+\s*'\?viewPath=1'/.test(src), 'expected the link to encode the hash and carry ?viewPath=1');
  assert.ok(/window\.copyToClipboard/.test(src), 'expected the shared copyToClipboard helper, not a bespoke clipboard implementation');
});

console.log('\n=== packet-path-map.js: functional smoke test (no-Leaflet code paths) ===');

function makeSandbox(apiImpl) {
  // A minimal but REAL DOM: elements track their own children/attributes
  // so createElement -> appendChild -> getElementById -> remove() all
  // actually work, unlike the inert stubs used for pure string-render
  // testing elsewhere. Deliberately small: only what open()/close() touch.
  function makeElement(tag) {
    const el = {
      tagName: tag, children: [], attributes: {}, style: {}, dataset: {},
      _listeners: {},
      get id() { return this.attributes.id || ''; },
      set id(v) { this.attributes.id = v; },
      // Real innerHTML would parse into a live child tree; this mock only
      // needs id-addressable children with a settable textContent (all
      // open()/close() read back), so it scans for id="..." occurrences
      // and registers one lightweight child per id found.
      set innerHTML(html) {
        this._innerHTML = html;
        this.children = [];
        const re = /id="([^"]+)"/g;
        let m;
        while ((m = re.exec(html))) {
          const child = makeElement('div');
          child.id = m[1];
          this.appendChild(child);
        }
      },
      get innerHTML() { return this._innerHTML || ''; },
      set textContent(t) { this._text = t; },
      get textContent() { return this._text || ''; },
      appendChild(child) { this.children.push(child); child._parent = this; return child; },
      remove() { if (this._parent) this._parent.children = this._parent.children.filter(c => c !== this); },
      contains(node) { for (let n = node; n; n = n._parent || n.parentElement) if (n === this) return true; return false; },
      addEventListener(type, fn) { (this._listeners[type] = this._listeners[type] || []).push(fn); },
      removeEventListener(type, fn) { if (this._listeners[type]) this._listeners[type] = this._listeners[type].filter(f => f !== fn); },
      querySelector() { return null; },
    };
    return el;
  }

  const body = makeElement('body');
  const docListeners = {};
  const doc = {
    createElement: makeElement,
    body,
    documentElement: { style: {} },
    getElementById(id) {
      const search = (el) => {
        if (el.id === id) return el;
        for (const c of el.children) { const found = search(c); if (found) return found; }
        return null;
      };
      return search(body);
    },
    // #180: the topmost element at a point; a test sets __topAt.
    elementFromPoint() { return ctx.__topAt || null; },
    // docLog: the live document listeners with their capture flag (#167 r2:
    // the modal's Escape handler must run in the capture phase).
    addEventListener(type, fn, opts) {
      (docListeners[type] = docListeners[type] || []).push(fn);
      docLog.push({ type, fn, capture: opts === true || !!(opts && opts.capture) });
    },
    removeEventListener(type, fn, opts) {
      if (docListeners[type]) docListeners[type] = docListeners[type].filter(f => f !== fn);
      const capture = opts === true || !!(opts && opts.capture);
      const i = docLog.findIndex(r => r.type === type && r.fn === fn && r.capture === capture);
      if (i !== -1) docLog.splice(i, 1);
    },
  };
  const docLog = [];

  // #180: the modal listens for hashchange on window while it is open.
  const winListeners = {};
  const win = {
    addEventListener(type, fn) { (winListeners[type] = winListeners[type] || []).push(fn); },
    removeEventListener(type, fn) { if (winListeners[type]) winListeners[type] = winListeners[type].filter(f => f !== fn); },
  };
  const ctx = {
    window: win, document: doc, console, Math, String, JSON, Promise, Error, Date, URLSearchParams,
    setTimeout, clearTimeout,
    // Returns the variable name itself (not a real color) so tests can
    // assert two markers use DIFFERENT css vars without caring what the
    // actual theme color is.
    // position: an element's own _pos (#180 layer test), else static.
    getComputedStyle: (el) => ({ getPropertyValue: (name) => name, position: (el && el._pos) || 'static' }),
    escapeHtml: (s) => String(s == null ? '' : s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;'),
    api: apiImpl,
    L: undefined, // Leaflet deliberately absent -- these tests only cover the no-plot-data / no-Leaflet paths.
    location: { origin: 'https://stg.meshview.dk', hash: '' },
    // Records replaceState() so the #147 tests can see the address bar. A
    // call without a URL changes only the entry's state (#180), not the
    // address bar, so it is not in __replaced.
    history: {
      state: null,
      replaceState(state, _t, url) {
        ctx.history.state = state;
        if (url === undefined) return;
        ctx.__replaced.push(url); ctx.location.hash = url;
      },
    },
    __replaced: [],
    __docLog: docLog,
    __winListeners: winListeners,
    // #180: a navigation (Back/Forward, a nav link): the new entry's hash and
    // state, then the window's hashchange listeners.
    __navigate(hash, state) {
      ctx.location.hash = hash;
      ctx.history.state = state === undefined ? null : state;
      (winListeners.hashchange || []).slice().forEach(fn => fn({}));
    },
  };
  ctx.window.copyToClipboard = (text, onDone) => { ctx.__copiedText = text; if (onDone) onDone(); };
  vm.createContext(ctx);
  vm.runInContext(src, ctx);
  return ctx;
}

(async () => {
  async function historyCase(name, fn) {
    try { await fn(); passed++; console.log('  ✅ ' + name); }
    catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
  }
  function installHistoryLeaflet(ctx) {
    const counts = { maps: 0, removed: 0, invalidated: 0 };
    const map = { setView() { return this; }, fitBounds(_, options) { counts.fitOptions = options; }, invalidateSize() { counts.invalidated++; }, remove() { counts.removed++; } };
    const layer = () => ({ addTo() { return this; }, bindTooltip() { return this; }, on() { return this; } });
    ctx.L = { map() { counts.maps++; return map; }, tileLayer: layer, circleMarker: layer, polyline: layer };
    return counts;
  }
  const archivedPath = { hash: 'historic', branches: [{ hops: 0, points: [], observer: { name: 'Saved station', lat: 56, lon: 10 } }] };
  const pingOptions = (loadPath) => ({ loadPath, routePrefix: '#/ping-scores/', routeQueryKey: 'record', shareURL: 'https://stg.meshview.dk/#/ping-scores/historic?viewPath=1&record=allTime.farthestPing' });

  await historyCase('ping archive loader renders a saved-time disclaimer and copies a ping-specific URL', async () => {
    const ctx = makeSandbox(() => { throw new Error('ordinary packet API must not be used'); });
    const counts = installHistoryLeaflet(ctx);
    ctx.location.hash = '#/ping-scores/historic?viewPath=1&record=allTime.farthestPing';
    let requested;
    await ctx.window.PacketPathMap.open('historic', pingOptions(async h => {
      requested = h;
      return { status: 'archived', capturedAt: '2026-10-06T08:00:00Z', path: archivedPath };
    }));
    assert.strictEqual(requested, 'historic');
    assert.strictEqual(counts.maps, 1);
    assert.strictEqual(counts.fitOptions.animate, false, 'historical initial bounds must not animate after navigation');
    const note = ctx.document.getElementById('packetPathArchiveNote');
    assert.ok(note.textContent.includes('2026-10-06 08:00:00 UTC'));
    assert.ok(note.textContent.includes('not necessarily when the packet was sent'));
    ctx.document.getElementById('packetPathCopyLink')._listeners.click[0]();
    assert.strictEqual(ctx.__copiedText, pingOptions().shareURL);
    ctx.window.PacketPathMap.close();
    assert.strictEqual(ctx.location.hash, '#/ping-scores/historic?record=allTime.farthestPing');
    assert.strictEqual(counts.removed, 1);
  });

  await historyCase('expired, unpositioned, private, and oversized records explain unavailable maps without an empty map box', async () => {
    const messages = { raw_data_expired_before_capture: 'expired before', no_coordinates: 'known positions', privacy_filtered: 'privacy settings', archive_too_large: 'size limit', record_evidence_unavailable: 'no longer reproduce this record' };
    for (const [reason, expected] of Object.entries(messages)) {
      const ctx = makeSandbox(() => { throw new Error('wrong API'); });
      const counts = installHistoryLeaflet(ctx);
      await ctx.window.PacketPathMap.open('historic', pingOptions(async () => ({ status: 'unavailable', reason })));
      assert.ok(ctx.document.getElementById('packetPathStatus').textContent.includes(expected), reason);
      assert.strictEqual(ctx.document.getElementById('packetPathMapContainer').style.display, 'none', reason);
      assert.strictEqual(ctx.document.getElementById('packetPathLegend').style.display, 'none', reason);
      assert.strictEqual(counts.maps, 0, reason);
      ctx.window.PacketPathMap.close();
    }
  });

  await historyCase('initializing history is not confused with expired history or a network failure', async () => {
    const ctx = makeSandbox(() => { throw new Error('wrong API'); });
    await ctx.window.PacketPathMap.open('historic', pingOptions(async () => ({ status: 'initializing' })));
    assert.ok(ctx.document.getElementById('packetPathStatus').textContent.includes('initializing'));
    assert.strictEqual(ctx.document.getElementById('packetPathMapContainer').style.display, 'none');
    await ctx.window.PacketPathMap.open('historic', pingOptions(async () => { throw new Error('network offline'); }));
    assert.ok(ctx.document.getElementById('packetPathStatus').textContent.includes('Failed to load path: network offline'));
    assert.strictEqual(ctx.document.getElementById('packetPathMapContainer').style.display, 'none');
    ctx.window.PacketPathMap.close();
  });

  await historyCase('a live ping path renders normally without pretending to be an archive', async () => {
    const ctx = makeSandbox(() => { throw new Error('wrong API'); });
    const counts = installHistoryLeaflet(ctx);
    await ctx.window.PacketPathMap.open('historic', pingOptions(async () => ({ status: 'live', path: archivedPath })));
    assert.strictEqual(counts.maps, 1);
    assert.strictEqual(ctx.document.getElementById('packetPathArchiveNote').textContent, '');
    assert.ok(ctx.document.getElementById('packetPathModal').innerHTML.includes('id="packetPathArchiveNote" class="text-muted" style="display:none;'));
    ctx.window.PacketPathMap.close();
  });

  await historyCase('ping Back/Forward entries use the same closed-entry lifecycle as ordinary packet links', async () => {
    const ctx = makeSandbox(() => { throw new Error('wrong API'); });
    const url = '#/ping-scores/historic?viewPath=1&record=allTime.farthestPing';
    const opts = pingOptions(async () => ({ status: 'unavailable', reason: 'no_coordinates' }));
    ctx.location.hash = url;
    await ctx.window.PacketPathMap.open('historic', opts);
    const state = ctx.history.state;
    assert.ok(state.packetPathModal, 'ping history entry must be tagged');
    ctx.__navigate('#/ping-scores');
    assert.ok(!ctx.document.getElementById('packetPathModal'));
    ctx.__navigate(url, state);
    assert.strictEqual(ctx.window.PacketPathMap.restore('historic', opts), false);
    ctx.__navigate(url, null);
    assert.strictEqual(ctx.window.PacketPathMap.restore('historic', opts), true);
    await Promise.resolve();
    const escape = ctx.__docLog.find(e => e.type === 'keydown').fn;
    escape({ key: 'Escape', stopPropagation() {} });
    assert.ok(!ctx.document.getElementById('packetPathModal'));
    assert.strictEqual(ctx.location.hash, '#/ping-scores/historic?record=allTime.farthestPing');
  });

  await historyCase('late path responses cannot create a map in a replacement or closed modal', async () => {
    let resolve;
    const ctx = makeSandbox(() => Promise.resolve({ hash: 'new', branches: [] }));
    const counts = installHistoryLeaflet(ctx);
    const old = ctx.window.PacketPathMap.open('historic', pingOptions(() => new Promise(r => { resolve = r; })));
    await ctx.window.PacketPathMap.open('new');
    resolve({ status: 'archived', capturedAt: '2026-10-06T08:00:00Z', path: archivedPath });
    await old;
    assert.strictEqual(counts.maps, 0);
    assert.ok(ctx.document.getElementById('packetPathStatus').textContent.includes('no observations'));
    const closed = ctx.window.PacketPathMap.open('historic', pingOptions(() => new Promise(r => { resolve = r; })));
    ctx.window.PacketPathMap.close();
    resolve({ status: 'live', path: archivedPath });
    await closed;
    assert.strictEqual(counts.maps, 0);
  });

  await historyCase('Back between two slots for the same hash preserves the incoming record URL and restores the correct map', async () => {
    const ctx = makeSandbox(() => { throw new Error('wrong API'); });
    const urlA = '#/ping-scores/historic?viewPath=1&record=allTime.farthestPing';
    const urlB = '#/ping-scores/historic?viewPath=1&record=thisWeek.farthestPing';
    const options = pingOptions(async () => ({ status: 'unavailable', reason: 'no_coordinates' }));
    ctx.location.hash = urlA;
    await ctx.window.PacketPathMap.open('historic', options);
    const stateA = ctx.history.state;
    // A card click uses pushState then open(), not a hashchange: replacing
    // A with B must not tag the incoming A entry as closed on a later Back.
    ctx.location.hash = urlB;
    ctx.history.state = null;
    await ctx.window.PacketPathMap.open('historic', options);
    const stateB = ctx.history.state;
    ctx.location.hash = urlA;
    ctx.history.state = stateA;
    ctx.window.PacketPathMap.close(); // app.navigate() destroys before init().
    assert.strictEqual(ctx.location.hash, urlA, 'closing departing B changed incoming A URL');
    assert.strictEqual(ctx.window.PacketPathMap.restore('historic', options), true, 'incoming A was wrongly marked closed');
    await Promise.resolve();
    ctx.__navigate(urlB, stateB);
    assert.strictEqual(ctx.window.PacketPathMap.restore('historic', options), false, 'the departing B entry retains the established closed-entry semantics');
  });

  await historyCase('a scheduled resize does not use a map after the modal was destroyed', async () => {
    const ctx = makeSandbox(() => Promise.resolve(archivedPath));
    const counts = installHistoryLeaflet(ctx);
    const timers = [];
    ctx.setTimeout = fn => { timers.push(fn); };
    await ctx.window.PacketPathMap.open('historic');
    assert.strictEqual(counts.maps, 1);
    assert.strictEqual(Object.prototype.hasOwnProperty.call(counts.fitOptions, 'animate'), false, 'ordinary packet animation defaults must not change');
    ctx.window.PacketPathMap.close();
    timers.forEach(fn => fn());
    assert.strictEqual(counts.invalidated, 0);
  });

  await historyCase('initializing history can be retried without refreshing the board or using the ordinary packet endpoint', async () => {
    const ctx = makeSandbox(() => { throw new Error('wrong API'); });
    const counts = installHistoryLeaflet(ctx);
    let calls = 0;
    const options = pingOptions(async () => ++calls === 1 ? { status: 'initializing' } : { status: 'live', path: archivedPath });
    await ctx.window.PacketPathMap.open('historic', options);
    const retry = ctx.document.getElementById('packetPathRetry');
    assert.strictEqual(retry.style.display, 'inline-block');
    await retry._listeners.click[0]();
    assert.strictEqual(calls, 2);
    assert.strictEqual(counts.maps, 1);
    ctx.window.PacketPathMap.close();
  });

  await historyCase('invalid history envelopes and unknown reasons never render a misleading map or raw server text', async () => {
    const ctx = makeSandbox(() => { throw new Error('wrong API'); });
    const counts = installHistoryLeaflet(ctx);
    await ctx.window.PacketPathMap.open('historic', pingOptions(async () => ({ status: 'live' })));
    assert.strictEqual(ctx.document.getElementById('packetPathStatus').textContent, 'Failed to load path: invalid history response.');
    assert.strictEqual(ctx.document.getElementById('packetPathMapContainer').style.display, 'none');
    await ctx.window.PacketPathMap.open('historic', pingOptions(async () => ({ status: 'unavailable', reason: '__proto__' })));
    assert.strictEqual(ctx.document.getElementById('packetPathStatus').textContent, 'A historical map is not available for this record.');
    assert.strictEqual(counts.maps, 0);
    ctx.window.PacketPathMap.close();
  });

  await (async () => {
    try {
      const ctx = makeSandbox(() => Promise.reject(new Error('network down')));
      await ctx.window.PacketPathMap.open('deadbeef');
      const overlay = ctx.document.getElementById('packetPathModal');
      const status = ctx.document.getElementById('packetPathStatus');
      assert.ok(overlay, 'modal overlay should be created');
      assert.ok(status.textContent.includes('Failed to load path'), 'should surface the fetch error, got: ' + status.textContent);
      passed++;
      console.log('  ✅ a failed fetch shows an error status without throwing');
    } catch (e) { failed++; console.log('  ❌ a failed fetch shows an error status without throwing: ' + e.message); }
  })();

  await (async () => {
    try {
      const ctx = makeSandbox(() => Promise.resolve({ hash: 'deadbeef', branches: [] }));
      await ctx.window.PacketPathMap.open('deadbeef');
      const status = ctx.document.getElementById('packetPathStatus');
      assert.ok(status.textContent.includes('no observations'), 'should explain there is nothing to show yet, got: ' + status.textContent);
      passed++;
      console.log('  ✅ no branches at all shows a clear "nothing to show" status');
    } catch (e) { failed++; console.log('  ❌ no branches at all shows a clear "nothing to show" status: ' + e.message); }
  })();

  await (async () => {
    try {
      // A compact legend explaining marker/line meaning (solid vs
      // approximate, primary vs other route, first-heard ring) -- the
      // old approach explained all of this in a long paragraph instead.
      const ctx = makeSandbox(() => Promise.resolve({ hash: 'deadbeef', branches: [] }));
      await ctx.window.PacketPathMap.open('deadbeef');
      const overlay = ctx.document.getElementById('packetPathModal');
      const html = overlay.innerHTML;
      assert.ok(html.includes('farthest-traveled route'), 'legend should explain the primary route color, got: ' + html);
      assert.ok(html.includes('other station'), 'legend should explain the secondary route color, got: ' + html);
      assert.ok(html.includes('approximate position'), 'legend should explain dashed markers, got: ' + html);
      assert.ok(html.includes('first to hear it'), 'legend should explain the green ring, got: ' + html);
      passed++;
      console.log('  ✅ renders a compact legend explaining route/marker colors and symbols');
    } catch (e) { failed++; console.log('  ❌ renders a compact legend explaining route/marker colors and symbols: ' + e.message); }
  })();

  await (async () => {
    try {
      const ctx = makeSandbox(() => Promise.resolve({
        hash: 'deadbeef',
        branches: [{ hops: 3, points: [{ publicKey: 'pk1', name: 'RepeaterA', lat: null, lon: null }], observer: null }],
      }));
      await ctx.window.PacketPathMap.open('deadbeef');
      const status = ctx.document.getElementById('packetPathStatus');
      assert.ok(status.textContent.includes('3 hop'), 'should mention the hop count even when no branch has a known position, got: ' + status.textContent);
      passed++;
      console.log('  ✅ hops with no known position at all still report the hop count, not a silent blank');
    } catch (e) { failed++; console.log('  ❌ hops with no known position at all still report the hop count, not a silent blank: ' + e.message); }
  })();

  await (async () => {
    try {
      const ctx = makeSandbox(() => Promise.reject(new Error('boom')));
      await ctx.window.PacketPathMap.open('deadbeef');
      assert.ok(ctx.document.getElementById('packetPathModal'), 'modal should be open');
      ctx.window.PacketPathMap.close();
      assert.ok(!ctx.document.getElementById('packetPathModal'), 'modal should be removed after close()');
      passed++;
      console.log('  ✅ close() removes the modal overlay from the DOM');
    } catch (e) { failed++; console.log('  ❌ close() removes the modal overlay from the DOM: ' + e.message); }
  })();

  await (async () => {
    try {
      const ctx = makeSandbox(() => Promise.reject(new Error('boom')));
      await ctx.window.PacketPathMap.open('deadbeef');
      const copyBtn = ctx.document.getElementById('packetPathCopyLink');
      assert.ok(copyBtn, 'expected a packetPathCopyLink button in the modal');
      copyBtn._listeners.click.forEach((fn) => fn());
      assert.strictEqual(ctx.__copiedText, 'https://stg.meshview.dk/#/packets/deadbeef?viewPath=1',
        'expected the copied URL to encode the packet hash and carry ?viewPath=1, got: ' + ctx.__copiedText);
      passed++;
      console.log('  ✅ clicking "Copy link" copies a #/packets/<hash>?viewPath=1 URL via window.copyToClipboard');
    } catch (e) { failed++; console.log('  ❌ clicking "Copy link" copies a #/packets/<hash>?viewPath=1 URL via window.copyToClipboard: ' + e.message); }
  })();

  await (async () => {
    try {
      // Two branches: a 2-hop chain (deepest, drawn primary) and a
      // 0-hop direct observer with no resolvable relay names at all.
      // Both should still get plotted -- this is the whole point of the
      // "show every branch" rework (a station that heard the packet
      // directly is real reach data even without a relay chain).
      const ctx = makeSandbox(() => Promise.resolve({
        hash: 'deadbeef',
        branches: [
          {
            hops: 2,
            points: [
              { publicKey: 'pk1', name: 'RepeaterA', lat: 56.0, lon: 10.0 },
              { publicKey: 'pk2', name: 'RepeaterB', lat: 56.1, lon: 10.1 },
            ],
            observer: { name: 'FarObserver', lat: 56.2, lon: 10.2 },
          },
          { hops: 0, points: [], observer: { name: 'NearObserver', lat: 55.9, lon: 9.9 } },
        ],
      }));

      let markerCount = 0, polylineCount = 0;
      ctx.L = {
        map: () => ({
          setView() { return this; },
          fitBounds() {},
          invalidateSize() {},
          remove() {},
        }),
        tileLayer: () => ({ addTo() { return this; } }),
        circleMarker: () => { markerCount++; return { addTo() { return this; }, bindTooltip() { return this; }, on() { return this; } }; },
        polyline: () => { polylineCount++; return { addTo() { return this; } }; },
      };

      await ctx.window.PacketPathMap.open('deadbeef');
      const status = ctx.document.getElementById('packetPathStatus');
      assert.strictEqual(markerCount, 4, 'expected 4 markers: 2 hops + observer for branch 1, 1 observer-only point for branch 2, got ' + markerCount);
      assert.strictEqual(polylineCount, 1, 'expected exactly 1 polyline (only the 3-point branch has >1 point to connect), got ' + polylineCount);
      assert.ok(status.textContent.includes('2 of 2 stations shown'), 'status should report both branches plotted, got: ' + status.textContent);
      assert.ok(status.textContent.includes('deepest reached 2 hop'), 'status should report the deepest branch hop count, got: ' + status.textContent);
      passed++;
      console.log('  ✅ multiple branches (including a 0-hop direct observer) are all plotted, not just the deepest');
    } catch (e) { failed++; console.log('  ❌ multiple branches (including a 0-hop direct observer) are all plotted, not just the deepest: ' + e.message); }
  })();

  await (async () => {
    try {
      // The "show only farthest route" toggle should only appear when
      // there's more than one branch to hide, and checking/unchecking it
      // should remove/re-add exactly the non-primary layers -- the
      // primary branch's own markers/polyline must never be touched.
      const ctx = makeSandbox(() => Promise.resolve({
        hash: 'deadbeef',
        branches: [
          {
            hops: 2,
            points: [
              { publicKey: 'pk1', name: 'RepeaterA', lat: 56.0, lon: 10.0 },
              { publicKey: 'pk2', name: 'RepeaterB', lat: 56.1, lon: 10.1 },
            ],
            observer: { name: 'FarObserver', lat: 56.2, lon: 10.2 },
          },
          { hops: 0, points: [], observer: { name: 'NearObserver', lat: 55.9, lon: 9.9 } },
        ],
      }));

      const removedLayers = [];
      const mapObj = {
        setView() { return this; }, fitBounds() {}, invalidateSize() {}, remove() {},
        removeLayer(layer) { removedLayers.push(layer); },
      };
      ctx.L = {
        map: () => mapObj,
        tileLayer: () => ({ addTo() { return this; } }),
        circleMarker: () => ({ addTo() { return this; }, bindTooltip() { return this; }, on() { return this; } }),
        polyline: () => ({ addTo() { return this; } }),
      };

      await ctx.window.PacketPathMap.open('deadbeef');
      const checkbox = ctx.document.getElementById('packetPathPrimaryOnly');
      assert.ok(checkbox, 'expected the declutter checkbox to exist when there is more than one branch');
      const controlsEl = ctx.document.getElementById('packetPathControls');
      assert.ok(controlsEl.innerHTML.includes('1 other station'), 'label should count the non-primary branch, got: ' + controlsEl.innerHTML);

      checkbox.checked = true;
      (checkbox._listeners.change || []).forEach((fn) => fn());
      assert.strictEqual(removedLayers.length, 1, 'expected exactly 1 removeLayer call: the 0-hop branch\'s single observer marker (no polyline, single point) -- the primary branch\'s 3 markers + 1 polyline must stay untouched, got ' + removedLayers.length);

      checkbox.checked = false;
      (checkbox._listeners.change || []).forEach((fn) => fn());
      passed++;
      console.log('  ✅ "show only farthest route" toggle hides/reveals exactly the non-primary layers');
    } catch (e) { failed++; console.log('  ❌ "show only farthest route" toggle hides/reveals exactly the non-primary layers: ' + e.message); }
  })();

  await (async () => {
    try {
      // A single-branch packet has nothing to declutter -- the toggle
      // must not appear at all.
      const ctx = makeSandbox(() => Promise.resolve({
        hash: 'deadbeef',
        branches: [{ hops: 1, points: [], observer: { name: 'OnlyObserver', lat: 56.0, lon: 10.0 } }],
      }));
      ctx.L = {
        map: () => ({ setView() { return this; }, fitBounds() {}, invalidateSize() {}, remove() {} }),
        tileLayer: () => ({ addTo() { return this; } }),
        circleMarker: () => ({ addTo() { return this; }, bindTooltip() { return this; }, on() { return this; } }),
        polyline: () => ({ addTo() { return this; } }),
      };

      await ctx.window.PacketPathMap.open('deadbeef');
      const checkbox = ctx.document.getElementById('packetPathPrimaryOnly');
      assert.ok(!checkbox, 'expected no declutter checkbox for a single-branch packet, found one');
      passed++;
      console.log('  ✅ declutter toggle is absent for a single-branch packet');
    } catch (e) { failed++; console.log('  ❌ declutter toggle is absent for a single-branch packet: ' + e.message); }
  })();

  await (async () => {
    try {
      // "Show only approximate positions" should only appear when at
      // least one marker is approx, and checking it should hide exactly
      // the non-approx markers while leaving the polyline (route context)
      // and the approx marker itself untouched.
      const ctx = makeSandbox(() => Promise.resolve({
        hash: 'deadbeef',
        branches: [
          {
            hops: 2,
            points: [
              { publicKey: 'pk1', name: 'RealFix', lat: 56.0, lon: 10.0 },
              { publicKey: 'pk2', name: 'GhostRepeater', lat: 56.1, lon: 10.1, approx: true },
            ],
            observer: { name: 'FarObserver', lat: 56.2, lon: 10.2 },
          },
        ],
      }));
      const removedLayers = [];
      const mapObj = {
        setView() { return this; }, fitBounds() {}, invalidateSize() {}, remove() {},
        removeLayer(layer) { removedLayers.push(layer); },
      };
      ctx.L = {
        map: () => mapObj,
        tileLayer: () => ({ addTo() { return this; } }),
        circleMarker: () => ({ addTo() { return this; }, bindTooltip() { return this; }, on() { return this; } }),
        polyline: () => ({ addTo() { return this; } }),
      };

      await ctx.window.PacketPathMap.open('deadbeef');
      const checkbox = ctx.document.getElementById('packetPathApproxOnly');
      assert.ok(checkbox, 'expected the approx-only checkbox to exist when at least one marker is approximate');
      const controlsEl = ctx.document.getElementById('packetPathControls');
      assert.ok(controlsEl.innerHTML.includes('1 estimated from neighbors'), 'label should count the approximate markers, got: ' + controlsEl.innerHTML);

      checkbox.checked = true;
      (checkbox._listeners.change || []).forEach((fn) => fn());
      assert.strictEqual(removedLayers.length, 2, 'expected 2 removeLayer calls: RealFix and FarObserver (both non-approx) -- GhostRepeater (approx) and the polyline (not approx-filtered) must stay, got ' + removedLayers.length);
      passed++;
      console.log('  ✅ "show only approximate positions" toggle hides exactly the non-approx markers, leaves the route line');
    } catch (e) { failed++; console.log('  ❌ "show only approximate positions" toggle hides exactly the non-approx markers, leaves the route line: ' + e.message); }
  })();

  await (async () => {
    try {
      // No approximate positions at all -- the approx-only checkbox must
      // not appear (nothing to filter to).
      const ctx = makeSandbox(() => Promise.resolve({
        hash: 'deadbeef',
        branches: [{ hops: 1, points: [], observer: { name: 'OnlyObserver', lat: 56.0, lon: 10.0 } }],
      }));
      ctx.L = {
        map: () => ({ setView() { return this; }, fitBounds() {}, invalidateSize() {}, remove() {} }),
        tileLayer: () => ({ addTo() { return this; } }),
        circleMarker: () => ({ addTo() { return this; }, bindTooltip() { return this; }, on() { return this; } }),
        polyline: () => ({ addTo() { return this; } }),
      };

      await ctx.window.PacketPathMap.open('deadbeef');
      const checkbox = ctx.document.getElementById('packetPathApproxOnly');
      assert.ok(!checkbox, 'expected no approx-only checkbox when nothing is approximate, found one');
      passed++;
      console.log('  ✅ approx-only toggle is absent when no marker is approximate');
    } catch (e) { failed++; console.log('  ❌ approx-only toggle is absent when no marker is approximate: ' + e.message); }
  })();

  await (async () => {
    try {
      // "Show area boundaries" starts checked (shapes visible by
      // default, matching prior always-on behavior) and unchecking it
      // removes exactly the area shape layers -- markers/polylines must
      // be untouched.
      const ctx = makeSandbox(() => Promise.resolve({
        hash: 'deadbeef',
        branches: [{ hops: 0, points: [], observer: { name: 'Obs', lat: 56.0, lon: 10.0 } }],
        touchedAreas: [{ label: 'Aarhus by', latMin: 56.05, latMax: 56.25, lonMin: 9.95, lonMax: 10.35 }],
      }));
      const removedLayers = [];
      const mapObj = {
        setView() { return this; }, fitBounds() {}, invalidateSize() {}, remove() {},
        removeLayer(layer) { removedLayers.push(layer); },
      };
      ctx.L = {
        map: () => mapObj,
        tileLayer: () => ({ addTo() { return this; } }),
        circleMarker: () => ({ addTo() { return this; }, bindTooltip() { return this; }, on() { return this; } }),
        polyline: () => ({ addTo() { return this; } }),
        rectangle: () => ({ addTo() { return this; } }),
      };

      await ctx.window.PacketPathMap.open('deadbeef');
      const checkbox = ctx.document.getElementById('packetPathShowAreas');
      assert.ok(checkbox, 'expected the show-areas checkbox to exist when touchedAreas is present');
      assert.strictEqual(checkbox.checked, true, 'show-areas checkbox should start checked (shapes visible by default)');

      checkbox.checked = false;
      (checkbox._listeners.change || []).forEach((fn) => fn());
      assert.strictEqual(removedLayers.length, 1, 'expected exactly 1 removeLayer call for the single area shape, got ' + removedLayers.length);
      passed++;
      console.log('  ✅ "show area boundaries" toggle starts checked and hides exactly the area shapes when unchecked');
    } catch (e) { failed++; console.log('  ❌ "show area boundaries" toggle starts checked and hides exactly the area shapes when unchecked: ' + e.message); }
  })();

  await (async () => {
    try {
      // The declutter and approx-only filters are independent and
      // combinable -- checking BOTH, then unchecking just one, must
      // recompute visibility from the OTHER filter's still-active state
      // rather than blindly re-adding everything. A naive
      // "each checkbox owns its own add/remove" implementation breaks
      // exactly this case.
      const ctx = makeSandbox(() => Promise.resolve({
        hash: 'deadbeef',
        branches: [
          {
            hops: 1,
            points: [],
            observer: { name: 'PrimaryObserver', lat: 56.0, lon: 10.0 },
          },
          {
            hops: 1,
            points: [],
            observer: { name: 'SecondaryApprox', lat: 56.1, lon: 10.1, approx: true },
          },
        ],
      }));
      let secondaryApproxOnMap = true; // starts added via .addTo() at draw time
      const secondaryApproxMarker = {
        addTo() { secondaryApproxOnMap = true; return this; },
        bindTooltip() { return this; },
        on() { return this; },
      };
      let callIndex = 0;
      const mapObj = {
        setView() { return this; }, fitBounds() {}, invalidateSize() {}, remove() {},
        removeLayer(layer) { if (layer === secondaryApproxMarker) secondaryApproxOnMap = false; },
      };
      ctx.L = {
        map: () => mapObj,
        tileLayer: () => ({ addTo() { return this; } }),
        circleMarker: () => {
          callIndex++;
          // Secondary branches are drawn FIRST (so the primary ends up
          // drawn last, on top) -- the 1st circleMarker created is
          // SecondaryApprox's observer marker, not the 2nd.
          if (callIndex === 1) return secondaryApproxMarker;
          return { addTo() { return this; }, bindTooltip() { return this; }, on() { return this; } };
        },
        polyline: () => ({ addTo() { return this; } }),
      };

      await ctx.window.PacketPathMap.open('deadbeef');
      const primaryOnly = ctx.document.getElementById('packetPathPrimaryOnly');
      const approxOnly = ctx.document.getElementById('packetPathApproxOnly');
      assert.ok(primaryOnly && approxOnly, 'expected both filter checkboxes to exist');

      // Check both -- the secondary+approx marker is hidden by primaryOnly.
      primaryOnly.checked = true;
      (primaryOnly._listeners.change || []).forEach((fn) => fn());
      approxOnly.checked = true;
      (approxOnly._listeners.change || []).forEach((fn) => fn());
      assert.strictEqual(secondaryApproxOnMap, false, 'secondary+approx marker should be hidden once primaryOnly is checked');

      // Uncheck primaryOnly -- approxOnly is STILL checked, and this
      // marker IS approx, so it must reappear (approxOnly alone doesn't
      // exclude it).
      primaryOnly.checked = false;
      (primaryOnly._listeners.change || []).forEach((fn) => fn());
      assert.strictEqual(secondaryApproxOnMap, true, 'secondary+approx marker should reappear once primaryOnly is unchecked -- approxOnly alone does not hide an approx marker');
      passed++;
      console.log('  ✅ declutter and approx-only filters combine correctly instead of fighting over the same layer');
    } catch (e) { failed++; console.log('  ❌ declutter and approx-only filters combine correctly instead of fighting over the same layer: ' + e.message); }
  })();

  await (async () => {
    try {
      // `first` is the earliest-arriving observation (usually 0 hops,
      // close to the sender) -- distinct from the deepest branch. It
      // should get its own extra landmark marker on top of the branch
      // dots, and be called out in the status line.
      const ctx = makeSandbox(() => Promise.resolve({
        hash: 'deadbeef',
        branches: [
          { hops: 5, points: [], observer: { name: 'FarObserver', lat: 56.2, lon: 10.2 } },
        ],
        first: { hops: 0, points: [], observer: { name: 'NearObserver', lat: 55.9, lon: 9.9 } },
      }));

      let markerCount = 0;
      ctx.L = {
        map: () => ({
          setView() { return this; },
          fitBounds() {},
          invalidateSize() {},
          remove() {},
        }),
        tileLayer: () => ({ addTo() { return this; } }),
        circleMarker: () => { markerCount++; return { addTo() { return this; }, bindTooltip() { return this; }, on() { return this; } }; },
        polyline: () => ({ addTo() { return this; } }),
      };

      await ctx.window.PacketPathMap.open('deadbeef');
      const status = ctx.document.getElementById('packetPathStatus');
      assert.strictEqual(markerCount, 2, 'expected 2 markers: the branch observer dot plus the extra first-observer landmark ring, got ' + markerCount);
      assert.ok(status.textContent.includes('entered near NearObserver'), 'status should call out the first observer, got: ' + status.textContent);
      passed++;
      console.log('  ✅ the earliest-arriving observation gets its own landmark marker and status callout');
    } catch (e) { failed++; console.log('  ❌ the earliest-arriving observation gets its own landmark marker and status callout: ' + e.message); }
  })();

  await (async () => {
    try {
      // A hop point and an observer with `approx: true` (server borrowed
      // the position from their strongest neighbor -- see GetPacketPath's
      // nearestPositionedNeighbor). Must render hollow/dashed, not as a
      // solid dot indistinguishable from a real fix, and get called out
      // in the status line.
      const ctx = makeSandbox(() => Promise.resolve({
        hash: 'deadbeef',
        branches: [
          {
            hops: 1,
            points: [{ publicKey: 'pk1', name: 'GhostRepeater', lat: 56.0, lon: 10.0, approx: true }],
            observer: { name: 'GhostObserver', lat: 56.1, lon: 10.1, approx: true },
          },
        ],
      }));

      let approxMarkerCalls = 0, solidMarkerCalls = 0;
      ctx.L = {
        map: () => ({
          setView() { return this; },
          fitBounds() {},
          invalidateSize() {},
          remove() {},
        }),
        tileLayer: () => ({ addTo() { return this; } }),
        circleMarker: (latlng, opts) => {
          if (opts && opts.dashArray) approxMarkerCalls++;
          else solidMarkerCalls++;
          return { addTo() { return this; }, bindTooltip() { return this; }, on() { return this; } };
        },
        polyline: () => ({ addTo() { return this; } }),
      };

      await ctx.window.PacketPathMap.open('deadbeef');
      const status = ctx.document.getElementById('packetPathStatus');
      assert.strictEqual(approxMarkerCalls, 2, 'expected both the hop point and the observer to render as hollow/dashed (approx), got ' + approxMarkerCalls);
      assert.strictEqual(solidMarkerCalls, 0, 'expected no solid markers -- both points in this branch are approximate, got ' + solidMarkerCalls);
      assert.ok(status.textContent.includes('2 approximate'), 'status should call out the approximate count, got: ' + status.textContent);
      passed++;
      console.log('  ✅ approximate (neighbor-borrowed) positions render hollow/dashed and are called out in status');
    } catch (e) { failed++; console.log('  ❌ approximate (neighbor-borrowed) positions render hollow/dashed and are called out in status: ' + e.message); }
  })();

  await (async () => {
    try {
      // A shared entry-point node with an approximate position commonly
      // appears in MANY branches' chains (e.g. one repeater near the
      // sender that a dozen stations all relayed through). The status
      // count must dedupe by identity -- 1 distinct approximate node
      // showing up in 3 branches is "1 approximate", not "3".
      const ctx = makeSandbox(() => Promise.resolve({
        hash: 'deadbeef',
        branches: [
          {
            hops: 2,
            points: [{ publicKey: 'pk-shared', name: 'SharedRepeater', lat: 56.0, lon: 10.0, approx: true }],
            observer: { name: 'ObserverA', lat: 56.1, lon: 10.1 },
          },
          {
            hops: 1,
            points: [{ publicKey: 'pk-shared', name: 'SharedRepeater', lat: 56.0, lon: 10.0, approx: true }],
            observer: { name: 'ObserverB', lat: 56.2, lon: 10.2 },
          },
          {
            hops: 1,
            points: [{ publicKey: 'pk-shared', name: 'SharedRepeater', lat: 56.0, lon: 10.0, approx: true }],
            observer: { name: 'ObserverC', lat: 56.3, lon: 10.3 },
          },
        ],
      }));

      ctx.L = {
        map: () => ({ setView() { return this; }, fitBounds() {}, invalidateSize() {}, remove() {} }),
        tileLayer: () => ({ addTo() { return this; } }),
        circleMarker: () => ({ addTo() { return this; }, bindTooltip() { return this; }, on() { return this; } }),
        polyline: () => ({ addTo() { return this; } }),
      };

      await ctx.window.PacketPathMap.open('deadbeef');
      const status = ctx.document.getElementById('packetPathStatus');
      assert.ok(status.textContent.includes('1 approximate'), 'status should count the shared node once (1 approximate), not once per branch it appears in, got: ' + status.textContent);
      assert.ok(!status.textContent.includes('3 approximate'), 'status should NOT count 3 -- that would be counting chain appearances, not distinct nodes, got: ' + status.textContent);
      passed++;
      console.log('  ✅ "N approximate" dedupes a shared node across branches instead of counting each chain appearance');
    } catch (e) { failed++; console.log('  ❌ "N approximate" dedupes a shared node across branches instead of counting each chain appearance: ' + e.message); }
  })();

  await (async () => {
    try {
      // branch.secondsAfterFirst (0 for the earliest arrival, positive
      // for later ones) should show up in the observer's tooltip label.
      const ctx = makeSandbox(() => Promise.resolve({
        hash: 'deadbeef',
        branches: [
          { hops: 2, points: [], observer: { name: 'LateObserver', lat: 56.0, lon: 10.0 }, secondsAfterFirst: 4.7 },
        ],
        first: { hops: 0, points: [], observer: { name: 'LateObserver', lat: 56.0, lon: 10.0 }, secondsAfterFirst: 0 },
      }));

      let tooltips = [];
      ctx.L = {
        map: () => ({ setView() { return this; }, fitBounds() {}, invalidateSize() {}, remove() {} }),
        tileLayer: () => ({ addTo() { return this; } }),
        circleMarker: () => ({ addTo() { return this; }, bindTooltip(t) { tooltips.push(t); return this; }, on() { return this; } }),
        polyline: () => ({ addTo() { return this; } }),
      };

      await ctx.window.PacketPathMap.open('deadbeef');
      assert.ok(tooltips.some((t) => t.includes('+4.7s')), 'expected a tooltip with the +4.7s elapsed time, got: ' + JSON.stringify(tooltips));
      passed++;
      console.log('  ✅ secondsAfterFirst renders as an elapsed-time label in the tooltip');
    } catch (e) { failed++; console.log('  ❌ secondsAfterFirst renders as an elapsed-time label in the tooltip: ' + e.message); }
  })();

  await (async () => {
    try {
      // branch.distanceFromFirstKm (> 0) should show up in the observer's
      // tooltip label; exactly 0 (First itself) should not add a
      // redundant "0.0 km away".
      const ctx = makeSandbox(() => Promise.resolve({
        hash: 'deadbeef',
        branches: [
          { hops: 2, points: [], observer: { name: 'FarObserver', lat: 56.0, lon: 10.0 }, distanceFromFirstKm: 42.3 },
        ],
        first: { hops: 0, points: [], observer: { name: 'FarObserver', lat: 56.0, lon: 10.0 }, distanceFromFirstKm: 0 },
      }));

      let tooltips = [];
      ctx.L = {
        map: () => ({ setView() { return this; }, fitBounds() {}, invalidateSize() {}, remove() {} }),
        tileLayer: () => ({ addTo() { return this; } }),
        circleMarker: () => ({ addTo() { return this; }, bindTooltip(t) { tooltips.push(t); return this; }, on() { return this; } }),
        polyline: () => ({ addTo() { return this; } }),
      };

      await ctx.window.PacketPathMap.open('deadbeef');
      assert.ok(tooltips.some((t) => t.includes('42.3 km away')), 'expected a tooltip with the 42.3 km distance, got: ' + JSON.stringify(tooltips));
      assert.ok(!tooltips.some((t) => t.includes('0.0 km away')), 'did not expect a "0.0 km away" label, got: ' + JSON.stringify(tooltips));
      passed++;
      console.log('  ✅ distanceFromFirstKm renders as a "N km away" label in the tooltip');
    } catch (e) { failed++; console.log('  ❌ distanceFromFirstKm renders as a "N km away" label in the tooltip: ' + e.message); }
  })();

  await (async () => {
    try {
      // More hops does not mean more geographic distance, and they're not
      // always the same branch -- both deserve their own highlight. Here
      // the SHALLOWER branch (2 hops) travels much farther (200km) than
      // the DEEPER one (5 hops, only 50km): a dense-mesh short-hop chain
      // vs. a couple of long-range links. Both get weight-2 (highlighted)
      // styling, but in DIFFERENT colors -- farthest keeps the accent
      // color, deepest gets the second (purple) color -- and the legend
      // shows both.
      const ctx = makeSandbox(() => Promise.resolve({
        hash: 'deadbeef',
        branches: [
          { hops: 5, points: [], observer: { name: 'DeepButClose', lat: 56.0, lon: 10.0 }, distanceFromFirstKm: 50 },
          { hops: 2, points: [], observer: { name: 'ShallowButFar', lat: 57.0, lon: 11.0 }, distanceFromFirstKm: 200 },
          // A third, fully-secondary branch -- neither farthest nor
          // deepest -- so there's something left for the declutter
          // toggle to actually hide (with only the two highlighted
          // branches, hiddenCount would be 0 and no toggle appears at
          // all, same as the existing single-branch case).
          { hops: 3, points: [], observer: { name: 'PlainSecondary', lat: 56.5, lon: 10.5 }, distanceFromFirstKm: 80 },
        ],
      }));

      const markerCalls = [];
      ctx.L = {
        map: () => ({ setView() { return this; }, fitBounds() {}, invalidateSize() {}, remove() {} }),
        tileLayer: () => ({ addTo() { return this; } }),
        circleMarker: (latlng, opts) => {
          const entry = { opts, tooltip: null };
          markerCalls.push(entry);
          return { addTo() { return this; }, bindTooltip(t) { entry.tooltip = t; return this; }, on() { return this; } };
        },
        polyline: () => ({ addTo() { return this; } }),
      };

      await ctx.window.PacketPathMap.open('deadbeef');
      const deepMarker = markerCalls.find((m) => m.tooltip && m.tooltip.includes('DeepButClose'));
      const farMarker = markerCalls.find((m) => m.tooltip && m.tooltip.includes('ShallowButFar'));
      assert.ok(deepMarker && farMarker, 'expected markers for both observers, got: ' + JSON.stringify(markerCalls.map((m) => m.tooltip)));
      assert.strictEqual(farMarker.opts.weight, 2, 'ShallowButFar (200km, actually farthest) should get highlighted styling (weight 2), got ' + farMarker.opts.weight);
      assert.strictEqual(deepMarker.opts.weight, 2, 'DeepButClose (5 hops, most hops) should ALSO get highlighted styling (weight 2) via its own "deepest" role, got ' + deepMarker.opts.weight);
      // Both are observer-only (no relay hop) points, so both FILL
      // yellow (the constant "this is an observer" color) -- the role
      // distinction shows up in the ring (stroke) color instead.
      assert.notStrictEqual(farMarker.opts.color, deepMarker.opts.color, 'farthest and deepest are different branches here, so their marker rings must use different colors, got matching color ' + farMarker.opts.color);

      const legendLabel = ctx.document.getElementById('packetPathPrimaryLegendLabel');
      assert.ok(legendLabel && legendLabel.textContent.includes('farthest-traveled') && !legendLabel.textContent.includes('deepest'), 'primary legend slot should say just "farthest-traveled" (deepest gets its own slot) when they diverge, got: ' + (legendLabel && legendLabel.textContent));
      const deepestLegendItem = ctx.document.getElementById('packetPathDeepestLegendItem');
      assert.strictEqual(deepestLegendItem.style.display, 'inline-flex', 'the second "deepest" legend swatch should be shown when farthest and deepest are different branches');
      const controlsEl = ctx.document.getElementById('packetPathControls');
      assert.ok(controlsEl.innerHTML.includes('farthest-traveled and deepest routes'), 'declutter checkbox should mention both routes when they diverge, got: ' + controlsEl.innerHTML);
      passed++;
      console.log('  ✅ farthest and deepest get their own distinct highlight (color + legend entry) when they are different branches');
    } catch (e) { failed++; console.log('  ❌ farthest and deepest get their own distinct highlight (color + legend entry) when they are different branches: ' + e.message); }
  })();

  await (async () => {
    try {
      // The common case: the deepest branch IS also the farthest one.
      // Must show a SINGLE combined highlight (not two), still only the
      // accent color, with a combined label -- not a second purple
      // "deepest" swatch for a branch that's already shown.
      const ctx = makeSandbox(() => Promise.resolve({
        hash: 'deadbeef',
        branches: [
          { hops: 5, points: [], observer: { name: 'DeepAndFar', lat: 57.0, lon: 11.0 }, distanceFromFirstKm: 200 },
          { hops: 2, points: [], observer: { name: 'ShallowAndClose', lat: 56.0, lon: 10.0 }, distanceFromFirstKm: 20 },
        ],
      }));
      ctx.L = {
        map: () => ({ setView() { return this; }, fitBounds() {}, invalidateSize() {}, remove() {} }),
        tileLayer: () => ({ addTo() { return this; } }),
        circleMarker: () => ({ addTo() { return this; }, bindTooltip() { return this; }, on() { return this; } }),
        polyline: () => ({ addTo() { return this; } }),
      };

      await ctx.window.PacketPathMap.open('deadbeef');
      const legendLabel = ctx.document.getElementById('packetPathPrimaryLegendLabel');
      assert.ok(legendLabel && legendLabel.textContent.includes('farthest-traveled & deepest'), 'legend should combine both facts into one label for the same branch, got: ' + (legendLabel && legendLabel.textContent));
      const deepestLegendItem = ctx.document.getElementById('packetPathDeepestLegendItem');
      assert.notStrictEqual(deepestLegendItem.style.display, 'inline-flex', 'the second "deepest" legend swatch must stay hidden when it is the same branch as farthest, got display=' + deepestLegendItem.style.display);
      passed++;
      console.log('  ✅ shows one combined highlight (not two) when the deepest branch is also the farthest one');
    } catch (e) { failed++; console.log('  ❌ shows one combined highlight (not two) when the deepest branch is also the farthest one: ' + e.message); }
  })();

  await (async () => {
    try {
      // No branch has distanceFromFirstKm at all (e.g. sparse GPS
      // coverage) -- falls back to the old hops-based pick, and the
      // legend/checkbox wording must say so honestly rather than still
      // claiming "farthest-traveled" for a branch nobody measured.
      const ctx = makeSandbox(() => Promise.resolve({
        hash: 'deadbeef',
        branches: [
          { hops: 3, points: [], observer: { name: 'ObsA', lat: 56.0, lon: 10.0 } },
          { hops: 1, points: [], observer: { name: 'ObsB', lat: 56.1, lon: 10.1 } },
        ],
      }));
      ctx.L = {
        map: () => ({ setView() { return this; }, fitBounds() {}, invalidateSize() {}, remove() {} }),
        tileLayer: () => ({ addTo() { return this; } }),
        circleMarker: () => ({ addTo() { return this; }, bindTooltip() { return this; }, on() { return this; } }),
        polyline: () => ({ addTo() { return this; } }),
      };

      await ctx.window.PacketPathMap.open('deadbeef');
      const legendLabel = ctx.document.getElementById('packetPathPrimaryLegendLabel');
      assert.ok(legendLabel && legendLabel.textContent.includes('deepest (most hops)'), 'legend should honestly say "deepest (most hops)" when no branch has distance data, got: ' + (legendLabel && legendLabel.textContent));
      assert.ok(!legendLabel.textContent.includes('farthest'), 'legend must not claim "farthest" when nothing was actually measured by distance, got: ' + legendLabel.textContent);
      const controlsEl = ctx.document.getElementById('packetPathControls');
      assert.ok(controlsEl.innerHTML.includes('deepest (most hops)'), 'declutter checkbox label should also use the honest wording, got: ' + controlsEl.innerHTML);
      passed++;
      console.log('  ✅ falls back to hop-based selection with honest "deepest (most hops)" wording when no branch has distance data');
    } catch (e) { failed++; console.log('  ❌ falls back to hop-based selection with honest "deepest (most hops)" wording when no branch has distance data: ' + e.message); }
  })();

  await (async () => {
    try {
      // Status line should report how long it took to reach the deepest
      // and farthest stations, plus the overall spread duration (the
      // largest secondsAfterFirst across ALL branches, not just those
      // two -- a station that's neither can still be the last to hear
      // it, e.g. a middling branch stuck behind a slow relay).
      const ctx = makeSandbox(() => Promise.resolve({
        hash: 'deadbeef',
        branches: [
          { hops: 5, points: [], observer: { name: 'DeepButClose', lat: 56.0, lon: 10.0 }, distanceFromFirstKm: 50, secondsAfterFirst: 4.1 },
          { hops: 2, points: [], observer: { name: 'ShallowButFar', lat: 57.0, lon: 11.0 }, distanceFromFirstKm: 200, secondsAfterFirst: 3.2 },
          { hops: 3, points: [], observer: { name: 'SlowestOfAll', lat: 56.5, lon: 10.5 }, distanceFromFirstKm: 80, secondsAfterFirst: 9.7 },
        ],
      }));
      ctx.L = {
        map: () => ({ setView() { return this; }, fitBounds() {}, invalidateSize() {}, remove() {} }),
        tileLayer: () => ({ addTo() { return this; } }),
        circleMarker: () => ({ addTo() { return this; }, bindTooltip() { return this; }, on() { return this; } }),
        polyline: () => ({ addTo() { return this; } }),
      };

      await ctx.window.PacketPathMap.open('deadbeef');
      const status = ctx.document.getElementById('packetPathStatus');
      assert.ok(status.textContent.includes('deepest reached 5 hops (4.1s)'), 'status should show the deepest branch\'s own elapsed time, got: ' + status.textContent);
      assert.ok(status.textContent.includes('farthest reached 200.0km (3.2s)'), 'status should show the farthest branch\'s distance and its own elapsed time, got: ' + status.textContent);
      assert.ok(status.textContent.includes('fully spread in 9.7s'), 'status should report the LARGEST elapsed time across all branches (SlowestOfAll, neither deepest nor farthest), not just the deepest/farthest ones, got: ' + status.textContent);
      passed++;
      console.log('  ✅ status line reports deepest/farthest elapsed time plus overall spread duration (max across all branches)');
    } catch (e) { failed++; console.log('  ❌ status line reports deepest/farthest elapsed time plus overall spread duration (max across all branches): ' + e.message); }
  })();

  await (async () => {
    try {
      // No branch has secondsAfterFirst at all -- must omit all three
      // timing additions cleanly rather than showing "(NaNs)" or a
      // spurious "fully spread in 0.0s".
      const ctx = makeSandbox(() => Promise.resolve({
        hash: 'deadbeef',
        branches: [
          { hops: 3, points: [], observer: { name: 'ObsA', lat: 56.0, lon: 10.0 }, distanceFromFirstKm: 40 },
        ],
      }));
      ctx.L = {
        map: () => ({ setView() { return this; }, fitBounds() {}, invalidateSize() {}, remove() {} }),
        tileLayer: () => ({ addTo() { return this; } }),
        circleMarker: () => ({ addTo() { return this; }, bindTooltip() { return this; }, on() { return this; } }),
        polyline: () => ({ addTo() { return this; } }),
      };

      await ctx.window.PacketPathMap.open('deadbeef');
      const status = ctx.document.getElementById('packetPathStatus');
      assert.ok(status.textContent.includes('deepest reached 3 hops') && !status.textContent.includes('deepest reached 3 hops ('), 'deepest line should have no "(Xs)" suffix when secondsAfterFirst is unknown, got: ' + status.textContent);
      assert.ok(status.textContent.includes('farthest reached 40.0km') && !status.textContent.includes('farthest reached 40.0km ('), 'farthest line should have no "(Xs)" suffix when secondsAfterFirst is unknown, got: ' + status.textContent);
      assert.ok(!status.textContent.includes('fully spread'), 'should not claim a spread duration when no branch has timing data, got: ' + status.textContent);
      passed++;
      console.log('  ✅ omits timing suffixes and the spread-duration stat when no branch has secondsAfterFirst');
    } catch (e) { failed++; console.log('  ❌ omits timing suffixes and the spread-duration stat when no branch has secondsAfterFirst: ' + e.message); }
  })();

  await (async () => {
    try {
      // Status line should surface the backend's estimated LoRa
      // Time-on-Air x distinct-relay-count for the whole flood, formatted
      // the same way analytics.js's Relay Airtime Share tab does (sub-1s
      // in ms, otherwise one decimal in s), with a "~" prefix flagging it
      // as an estimate.
      const ctx = makeSandbox(() => Promise.resolve({
        hash: 'deadbeef',
        branches: [
          { hops: 2, points: [], observer: { name: 'ObsA', lat: 56.0, lon: 10.0 } },
        ],
        estimatedAirtimeMs: 340.4,
        airtimeRelayCount: 3,
      }));
      ctx.L = {
        map: () => ({ setView() { return this; }, fitBounds() {}, invalidateSize() {}, remove() {} }),
        tileLayer: () => ({ addTo() { return this; } }),
        circleMarker: () => ({ addTo() { return this; }, bindTooltip() { return this; }, on() { return this; } }),
        polyline: () => ({ addTo() { return this; } }),
      };

      await ctx.window.PacketPathMap.open('deadbeef');
      const status = ctx.document.getElementById('packetPathStatus');
      assert.ok(status.textContent.includes('~340ms estimated airtime (3 relays)'), 'status should show the estimated airtime and relay count, got: ' + status.textContent);
      passed++;
      console.log('  ✅ status line reports estimated airtime and relay count when the backend provides them');
    } catch (e) { failed++; console.log('  ❌ status line reports estimated airtime and relay count when the backend provides them: ' + e.message); }
  })();

  await (async () => {
    try {
      // Singular "relay" (not "relays") when the count is exactly 1, and
      // the seconds-formatted branch (>=1000ms) when airtime is large.
      const ctx = makeSandbox(() => Promise.resolve({
        hash: 'deadbeef',
        branches: [
          { hops: 1, points: [], observer: { name: 'ObsA', lat: 56.0, lon: 10.0 } },
        ],
        estimatedAirtimeMs: 1234,
        airtimeRelayCount: 1,
      }));
      ctx.L = {
        map: () => ({ setView() { return this; }, fitBounds() {}, invalidateSize() {}, remove() {} }),
        tileLayer: () => ({ addTo() { return this; } }),
        circleMarker: () => ({ addTo() { return this; }, bindTooltip() { return this; }, on() { return this; } }),
        polyline: () => ({ addTo() { return this; } }),
      };

      await ctx.window.PacketPathMap.open('deadbeef');
      const status = ctx.document.getElementById('packetPathStatus');
      assert.ok(status.textContent.includes('~1.2s estimated airtime (1 relay)'), 'status should use singular "relay" and seconds formatting above 1000ms, got: ' + status.textContent);
      assert.ok(!status.textContent.includes('1 relays'), 'must not pluralize "relay" for a count of 1, got: ' + status.textContent);
      passed++;
      console.log('  ✅ status line uses singular "relay" and seconds formatting for airtime >= 1000ms');
    } catch (e) { failed++; console.log('  ❌ status line uses singular "relay" and seconds formatting for airtime >= 1000ms: ' + e.message); }
  })();

  await (async () => {
    try {
      // No estimatedAirtimeMs in the response (store unavailable /
      // DB-only mode) -- must omit the stat cleanly, not show "~undefined".
      const ctx = makeSandbox(() => Promise.resolve({
        hash: 'deadbeef',
        branches: [
          { hops: 1, points: [], observer: { name: 'ObsA', lat: 56.0, lon: 10.0 } },
        ],
      }));
      ctx.L = {
        map: () => ({ setView() { return this; }, fitBounds() {}, invalidateSize() {}, remove() {} }),
        tileLayer: () => ({ addTo() { return this; } }),
        circleMarker: () => ({ addTo() { return this; }, bindTooltip() { return this; }, on() { return this; } }),
        polyline: () => ({ addTo() { return this; } }),
      };

      await ctx.window.PacketPathMap.open('deadbeef');
      const status = ctx.document.getElementById('packetPathStatus');
      assert.ok(!status.textContent.includes('airtime'), 'should not mention airtime when the backend omitted the field, got: ' + status.textContent);
      passed++;
      console.log('  ✅ omits the airtime stat cleanly when the backend response has no estimatedAirtimeMs');
    } catch (e) { failed++; console.log('  ❌ omits the airtime stat cleanly when the backend response has no estimatedAirtimeMs: ' + e.message); }
  })();

  await (async () => {
    try {
      // A single-neighbor approx point should render with a bigger,
      // fainter ring than a 4-neighbor approx point -- more agreeing
      // neighbors means more confidence, so a tighter, more solid marker.
      const ctx = makeSandbox(() => Promise.resolve({
        hash: 'deadbeef',
        branches: [
          {
            hops: 2,
            points: [
              { publicKey: 'pk1', name: 'LowConfidence', lat: 56.0, lon: 10.0, approx: true, approxNeighborCount: 1 },
              { publicKey: 'pk2', name: 'HighConfidence', lat: 56.1, lon: 10.1, approx: true, approxNeighborCount: 4, approxSpreadKm: 5 },
            ],
            observer: null,
          },
        ],
      }));

      const markerOptsByName = {};
      const tooltipByCall = [];
      ctx.L = {
        map: () => ({ setView() { return this; }, fitBounds() {}, invalidateSize() {}, remove() {} }),
        tileLayer: () => ({ addTo() { return this; } }),
        circleMarker: (latlng, opts) => {
          tooltipByCall.push(opts);
          return { addTo() { return this; }, bindTooltip(t) { markerOptsByName[t] = opts; return this; }, on() { return this; } };
        },
        polyline: () => ({ addTo() { return this; } }),
      };

      await ctx.window.PacketPathMap.open('deadbeef');
      const lowKey = Object.keys(markerOptsByName).find((k) => k.includes('LowConfidence'));
      const highKey = Object.keys(markerOptsByName).find((k) => k.includes('HighConfidence'));
      assert.ok(lowKey, 'expected a tooltip for LowConfidence');
      assert.ok(highKey, 'expected a tooltip for HighConfidence');
      assert.ok(markerOptsByName[lowKey].radius > markerOptsByName[highKey].radius,
        'expected the 1-neighbor marker to be larger than the 4-neighbor marker, got radii ' + markerOptsByName[lowKey].radius + ' vs ' + markerOptsByName[highKey].radius);
      assert.ok(markerOptsByName[lowKey].fillOpacity < markerOptsByName[highKey].fillOpacity,
        'expected the 1-neighbor marker to be fainter than the 4-neighbor marker');
      assert.ok(lowKey.includes('from 1 neighbor'), 'expected the tooltip to mention the neighbor count, got: ' + lowKey);
      assert.ok(highKey.includes('from 4 neighbors'), 'expected the tooltip to mention the neighbor count, got: ' + highKey);
      passed++;
      console.log('  ✅ approximate markers scale size/opacity by neighbor confidence');
    } catch (e) { failed++; console.log('  ❌ approximate markers scale size/opacity by neighbor confidence: ' + e.message); }
  })();

  await (async () => {
    try {
      // A hop point and an observer with a known `role` should get a
      // role-specific icon prefix in their tooltip.
      const ctx = makeSandbox(() => Promise.resolve({
        hash: 'deadbeef',
        branches: [
          {
            hops: 1,
            points: [{ publicKey: 'pk1', name: 'RepeaterA', lat: 56.0, lon: 10.0, role: 'repeater' }],
            observer: { name: 'RoomObserver', lat: 56.1, lon: 10.1, role: 'room' },
          },
        ],
      }));

      const tooltips = [];
      ctx.L = {
        map: () => ({ setView() { return this; }, fitBounds() {}, invalidateSize() {}, remove() {} }),
        tileLayer: () => ({ addTo() { return this; } }),
        circleMarker: () => ({ addTo() { return this; }, bindTooltip(t) { tooltips.push(t); return this; }, on() { return this; } }),
        polyline: () => ({ addTo() { return this; } }),
      };

      await ctx.window.PacketPathMap.open('deadbeef');
      assert.ok(tooltips.some((t) => t.includes('#ph-broadcast') && t.includes('RepeaterA')), 'expected a repeater icon on RepeaterA, got: ' + JSON.stringify(tooltips));
      assert.ok(tooltips.some((t) => t.includes('#ph-house-line') && t.includes('RoomObserver')), 'expected a room icon on RoomObserver, got: ' + JSON.stringify(tooltips));
      passed++;
      console.log('  ✅ nodes with a known role get a role icon in their tooltip');
    } catch (e) { failed++; console.log('  ❌ nodes with a known role get a role icon in their tooltip: ' + e.message); }
  })();

  await (async () => {
    try {
      // #174 (review of #177): every role icon and the first-to-hear flag
      // are decorative sprite icons, hidden from screen readers.
      const first = {
        hops: 2,
        points: [{ publicKey: 'pk3', name: 'SensorC', lat: 56.2, lon: 10.2, role: 'sensor' }],
        observer: { name: 'FirstObs', lat: 56.3, lon: 10.3, role: 'client' },
      };
      const ctx = makeSandbox(() => Promise.resolve({
        hash: 'deadbeef',
        branches: [
          {
            hops: 1,
            points: [{ publicKey: 'pk1', name: 'RepeaterA', lat: 56.0, lon: 10.0, role: 'repeater' }],
            observer: { name: 'RoomObserver', lat: 56.1, lon: 10.1, role: 'room' },
          },
          first,
        ],
        first,
      }));

      const tooltips = [];
      ctx.L = {
        map: () => ({ setView() { return this; }, fitBounds() {}, invalidateSize() {}, remove() {} }),
        tileLayer: () => ({ addTo() { return this; } }),
        circleMarker: () => ({ addTo() { return this; }, bindTooltip(t) { tooltips.push(t); return this; }, on() { return this; } }),
        polyline: () => ({ addTo() { return this; } }),
      };

      await ctx.window.PacketPathMap.open('deadbeef');
      const icon = (name) => '<svg class="ph-icon" aria-hidden="true"><use href="/icons/phosphor-sprite.svg#ph-' + name + '"/></svg> ';
      for (const [name, glyph] of [['RepeaterA', 'broadcast'], ['RoomObserver', 'house-line'], ['SensorC', 'thermometer'], ['FirstObs', 'radio']]) {
        assert.ok(tooltips.some((t) => t.startsWith(icon(glyph)) && t.includes(name)),
          'expected ' + name + ' to start with an aria-hidden #ph-' + glyph + ' icon, got: ' + JSON.stringify(tooltips));
      }
      const flag = tooltips.filter((t) => t.includes('First to hear it: FirstObs'));
      assert.strictEqual(flag.length, 1, 'expected one first-to-hear tooltip, got: ' + JSON.stringify(tooltips));
      assert.ok(flag[0].startsWith(icon('flag') + 'First to hear it: FirstObs (2 hops)'),
        'expected the first-to-hear tooltip to start with an aria-hidden #ph-flag icon, got: ' + flag[0]);
      const svgs = tooltips.join('').match(/<svg\b[^>]*>/g) || [];
      assert.ok(svgs.length >= 5, 'expected at least 5 icons, got ' + svgs.length);
      assert.deepStrictEqual(svgs.filter((t) => !/\baria-hidden="true"/.test(t)), [], 'every tooltip icon must be aria-hidden');
      passed++;
      console.log('  ✅ role icons and the first-to-hear flag render as aria-hidden sprite icons');
    } catch (e) { failed++; console.log('  ❌ role icons and the first-to-hear flag render as aria-hidden sprite icons: ' + e.message); }
  })();

  await (async () => {
    try {
      // A marker with a publicKey should register a click handler that
      // navigates to #/nodes/{pubkey} (closing the modal first); one
      // without a publicKey should register no click handler at all.
      const ctx = makeSandbox(() => Promise.resolve({
        hash: 'deadbeef',
        branches: [
          {
            hops: 1,
            points: [{ publicKey: 'pk-with-key', name: 'HasKey', lat: 56.0, lon: 10.0 }],
            observer: { name: 'NoKeyObserver', lat: 56.1, lon: 10.1 }, // no publicKey
          },
        ],
      }));
      ctx.window.location = { hash: '' };

      const clickHandlersByTooltip = {};
      let lastTooltip = null;
      ctx.L = {
        map: () => ({ setView() { return this; }, fitBounds() {}, invalidateSize() {}, remove() {} }),
        tileLayer: () => ({ addTo() { return this; } }),
        circleMarker: () => ({
          addTo() { return this; },
          bindTooltip(t) { lastTooltip = t; return this; },
          on(evt, fn) { if (evt === 'click') clickHandlersByTooltip[lastTooltip] = fn; return this; },
        }),
        polyline: () => ({ addTo() { return this; } }),
      };

      await ctx.window.PacketPathMap.open('deadbeef');
      const hasKeyTooltip = Object.keys(clickHandlersByTooltip).find((t) => t.includes('HasKey'));
      assert.ok(hasKeyTooltip, 'expected a click handler registered for the HasKey marker, got: ' + JSON.stringify(Object.keys(clickHandlersByTooltip)));
      assert.ok(hasKeyTooltip.includes('click for node detail'), 'expected the tooltip to hint it is clickable, got: ' + hasKeyTooltip);
      assert.ok(!Object.keys(clickHandlersByTooltip).some((t) => t.includes('NoKeyObserver')), 'expected NO click handler for the keyless observer');

      clickHandlersByTooltip[hasKeyTooltip]();
      assert.strictEqual(ctx.window.location.hash, '#/nodes/pk-with-key', 'expected clicking the marker to navigate to the node detail hash route, got: ' + ctx.window.location.hash);
      assert.ok(!ctx.document.getElementById('packetPathModal'), 'expected the modal to close after navigating away');
      passed++;
      console.log('  ✅ markers with a publicKey are clickable and navigate to node detail, closing the modal');
    } catch (e) { failed++; console.log('  ❌ markers with a publicKey are clickable and navigate to node detail, closing the modal: ' + e.message); }
  })();

  await (async () => {
    try {
      // touchedAreas is the server-resolved, uncapped list of every
      // configured area any point/observer on the path fell in -- View
      // Path has room to show all of them (unlike the pong reply's
      // capped "+N more" version). Each entry is an object (label +
      // boundary), not a plain string -- the footer text uses only .label.
      const ctx = makeSandbox(() => Promise.resolve({
        hash: 'deadbeef',
        branches: [{ hops: 0, points: [], observer: { name: 'Obs', lat: 56.0, lon: 10.0 } }],
        touchedAreas: [
          { label: 'Aarhus by', latMin: 56.05, latMax: 56.25, lonMin: 9.95, lonMax: 10.35 },
          { label: 'Djursland', latMin: 56.20, latMax: 56.55, lonMin: 10.35, lonMax: 10.90 },
          { label: 'Odense by', latMin: 55.30, latMax: 55.50, lonMin: 10.25, lonMax: 10.45 },
        ],
      }));
      ctx.L = {
        map: () => ({ setView() { return this; }, fitBounds() {}, invalidateSize() {}, remove() {} }),
        tileLayer: () => ({ addTo() { return this; } }),
        circleMarker: () => ({ addTo() { return this; }, bindTooltip() { return this; }, on() { return this; } }),
        polyline: () => ({ addTo() { return this; } }),
        polygon: () => ({ addTo() { return this; } }),
        rectangle: () => ({ addTo() { return this; } }),
      };

      await ctx.window.PacketPathMap.open('deadbeef');
      const status = ctx.document.getElementById('packetPathStatus');
      assert.ok(status.textContent.includes('touched Aarhus by, Djursland, Odense by'), 'status should list every touched area\'s label uncapped, got: ' + status.textContent);
      passed++;
      console.log('  ✅ touchedAreas renders as an uncapped, comma-joined list of labels in the status line');
    } catch (e) { failed++; console.log('  ❌ touchedAreas renders as an uncapped, comma-joined list of labels in the status line: ' + e.message); }
  })();

  await (async () => {
    try {
      // No touchedAreas field at all (no areas configured server-side, or
      // none resolved) -- must not add a stray "touched" fragment, try to
      // shade anything, or throw.
      const ctx = makeSandbox(() => Promise.resolve({
        hash: 'deadbeef',
        branches: [{ hops: 0, points: [], observer: { name: 'Obs', lat: 56.0, lon: 10.0 } }],
      }));
      ctx.L = {
        map: () => ({ setView() { return this; }, fitBounds() {}, invalidateSize() {}, remove() {} }),
        tileLayer: () => ({ addTo() { return this; } }),
        circleMarker: () => ({ addTo() { return this; }, bindTooltip() { return this; }, on() { return this; } }),
        polyline: () => ({ addTo() { return this; } }),
      };

      await ctx.window.PacketPathMap.open('deadbeef');
      const status = ctx.document.getElementById('packetPathStatus');
      assert.ok(!status.textContent.includes('touched'), 'status should have no "touched" fragment when touchedAreas is absent, got: ' + status.textContent);
      passed++;
      console.log('  ✅ omits the "touched" fragment when touchedAreas is absent');
    } catch (e) { failed++; console.log('  ❌ omits the "touched" fragment when touchedAreas is absent: ' + e.message); }
  })();

  await (async () => {
    try {
      // Each touched area is shaded on the map: a polygon area draws via
      // L.polygon (its actual drawn boundary), a bbox-only area falls
      // back to L.rectangle -- and every shape must be non-interactive so
      // it never steals a click meant for a branch marker underneath it.
      const ctx = makeSandbox(() => Promise.resolve({
        hash: 'deadbeef',
        branches: [{ hops: 0, points: [], observer: { name: 'Obs', lat: 56.0, lon: 10.0 } }],
        touchedAreas: [
          { label: 'Aarhus by', polygon: [[56.05, 9.95], [56.05, 10.35], [56.25, 10.35], [56.25, 9.95]] },
          { label: 'Djursland', latMin: 56.20, latMax: 56.55, lonMin: 10.35, lonMax: 10.90 },
        ],
      }));
      const polygonCalls = [];
      const rectangleCalls = [];
      ctx.L = {
        map: () => ({ setView() { return this; }, fitBounds() {}, invalidateSize() {}, remove() {} }),
        tileLayer: () => ({ addTo() { return this; } }),
        circleMarker: () => ({ addTo() { return this; }, bindTooltip() { return this; }, on() { return this; } }),
        polyline: () => ({ addTo() { return this; } }),
        polygon: (latlngs, opts) => { polygonCalls.push({ latlngs, opts }); return { addTo() { return this; } }; },
        rectangle: (bounds, opts) => { rectangleCalls.push({ bounds, opts }); return { addTo() { return this; } }; },
      };

      await ctx.window.PacketPathMap.open('deadbeef');
      assert.strictEqual(polygonCalls.length, 1, 'expected exactly 1 L.polygon call for the area with a drawn polygon, got ' + polygonCalls.length);
      assert.strictEqual(polygonCalls[0].latlngs.length, 4, 'expected the polygon\'s own 4 points passed through, got ' + JSON.stringify(polygonCalls[0].latlngs));
      assert.strictEqual(rectangleCalls.length, 1, 'expected exactly 1 L.rectangle call for the bbox-only area, got ' + rectangleCalls.length);
      // Compared field-by-field rather than via deepStrictEqual: the
      // array came out of a separate vm context (a different Array
      // realm), which deepStrictEqual treats as unequal even for
      // identical primitive contents.
      const rectBounds = rectangleCalls[0].bounds;
      assert.strictEqual(rectBounds[0][0], 56.20, 'rectangle bounds[0][0] (latMin) mismatch, got ' + JSON.stringify(rectBounds));
      assert.strictEqual(rectBounds[0][1], 10.35, 'rectangle bounds[0][1] (lonMin) mismatch, got ' + JSON.stringify(rectBounds));
      assert.strictEqual(rectBounds[1][0], 56.55, 'rectangle bounds[1][0] (latMax) mismatch, got ' + JSON.stringify(rectBounds));
      assert.strictEqual(rectBounds[1][1], 10.90, 'rectangle bounds[1][1] (lonMax) mismatch, got ' + JSON.stringify(rectBounds));
      assert.strictEqual(polygonCalls[0].opts.interactive, false, 'area shapes must be non-interactive so they never steal clicks from branch markers');
      assert.strictEqual(rectangleCalls[0].opts.interactive, false, 'area shapes must be non-interactive so they never steal clicks from branch markers');
      passed++;
      console.log('  ✅ shades each touched area on the map: polygon when drawn, rectangle fallback for bbox-only areas, both non-interactive');
    } catch (e) { failed++; console.log('  ❌ shades each touched area on the map: polygon when drawn, rectangle fallback for bbox-only areas, both non-interactive: ' + e.message); }
  })();

  // #147: ?viewPath=1 on a #/packets/<hash> URL describes this open modal.
  // Closing it drops that one param (others verbatim) so a refresh or a
  // copied address-bar link does not reopen a modal that is no longer shown.
  async function viewPathCase(name, startHash, act, expectHash, expectWrites) {
    try {
      const ctx = makeSandbox(() => Promise.reject(new Error('boom')));
      ctx.location.hash = startHash;
      await act(ctx);
      assert.strictEqual(ctx.location.hash, expectHash);
      assert.strictEqual(ctx.__replaced.length, expectWrites, 'replaceState calls: ' + JSON.stringify(ctx.__replaced));
      passed++;
      console.log('  ✅ ' + name);
    } catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
  }
  await viewPathCase('close() drops ?viewPath=1 from a #/packets/<hash> URL, keeping the other params (#147)',
    '#/packets/deadbeef?timeWindow=60&obs=123&viewPath=1',
    async (ctx) => { await ctx.window.PacketPathMap.open('deadbeef'); ctx.window.PacketPathMap.close(); },
    '#/packets/deadbeef?timeWindow=60&obs=123', 1);
  await viewPathCase('open() on a cold-loaded ?viewPath=1 link keeps it while the modal is shown (#147)',
    '#/packets/deadbeef?obs=123&viewPath=1',
    async (ctx) => { await ctx.window.PacketPathMap.open('deadbeef'); },
    '#/packets/deadbeef?obs=123&viewPath=1', 0);
  await viewPathCase('close() leaves non-packets pages (e.g. analytics distance) untouched (#147)',
    '#/analytics?tab=distance&viewPath=1',
    async (ctx) => { await ctx.window.PacketPathMap.open('deadbeef'); ctx.window.PacketPathMap.close(); },
    '#/analytics?tab=distance&viewPath=1', 0);
  // #167 r2: open() replaces a modal that is already open. That is not a
  // close, so it must not drop ?viewPath=1 (open() used to call close()).
  await viewPathCase('open() while the modal is already open keeps ?viewPath=1 (#167 r2)',
    '#/packets/deadbeef?obs=123&viewPath=1',
    async (ctx) => {
      await ctx.window.PacketPathMap.open('deadbeef');
      await ctx.window.PacketPathMap.open('deadbeef');
      assert(ctx.document.getElementById('packetPathModal'), 'modal not open after the second open()');
    },
    '#/packets/deadbeef?obs=123&viewPath=1', 0);

  // #167 r2: packets.js writes ?viewPath=1 while the modal is open on the
  // packet of the #/packets/<hash> subpath, so the overlay names its packet.
  await viewPathCase('the open modal records its packet hash (data-hash) (#167 r2)',
    '#/packets/deadbeef',
    async (ctx) => {
      await ctx.window.PacketPathMap.open('deadbeef');
      const overlay = ctx.document.getElementById('packetPathModal');
      assert.strictEqual(overlay && overlay.dataset.hash, 'deadbeef');
    },
    '#/packets/deadbeef', 0);

  // #167 r2: Escape closes only the top layer. The modal's Escape handler
  // runs in the capture phase and stops the event, so the packets detail
  // pane's and the SlideOver's own Escape handlers (bubble phase on
  // document) do not close as well. A second Escape reaches them.
  await viewPathCase('Escape closes only the modal: capture-phase handler stops the event (#167 r2)',
    '#/packets/deadbeef?obs=123&viewPath=1',
    async (ctx) => {
      await ctx.window.PacketPathMap.open('deadbeef');
      const keys = ctx.__docLog.filter(r => r.type === 'keydown');
      assert.strictEqual(keys.length, 1, 'keydown listeners: ' + keys.length);
      assert.strictEqual(keys[0].capture, true, 'Escape handler is not in the capture phase');
      let stopped = 0;
      keys[0].fn({ key: 'Escape', stopPropagation() { stopped++; } });
      assert.strictEqual(stopped, 1, 'Escape was not stopped');
      assert.strictEqual(ctx.document.getElementById('packetPathModal'), null, 'modal still open');
      assert.strictEqual(ctx.__docLog.filter(r => r.type === 'keydown').length, 0, 'keydown listener left behind');
      let stoppedAfter = 0;
      keys[0].fn({ key: 'Escape', stopPropagation() { stoppedAfter++; } });
      assert.strictEqual(stoppedAfter, 0, 'a stray handler call with no modal open swallowed Escape');
    },
    '#/packets/deadbeef?obs=123', 1);

  // #180: Escape is the top layer's. A layer opened over the modal later
  // (global search via Ctrl+K, a nav menu, the More sheet, a filter popover)
  // holds focus and has its own Escape handler: the modal leaves the event
  // alone then. Focus on the page, in the sticky top nav, or in the detail
  // surface the modal opened over (SlideOver, mobile sheet) still closes it.
  // fakeFocus: a focused element inside a chain of ancestors; each entry is
  // { pos, cls } from the element itself outwards.
  function fakeFocus(chain) {
    let parent = null;
    const nodes = chain.slice().reverse().map((c) => {
      const n = {
        _pos: c.pos || 'static', parentElement: parent, _cls: c.cls || '',
        matches(sel) { return sel.split(',').some((x) => x.trim() === '.' + this._cls); },
        contains(other) { for (let o = other; o; o = o.parentElement) if (o === this) return true; return false; },
        getBoundingClientRect: () => ({ left: 10, top: 10, width: 20, height: 20 }),
      };
      parent = n;
      return n;
    });
    return nodes[nodes.length - 1];
  }
  async function escapeCase(name, chain, topIs, expectClosed) {
    try {
      const ctx = makeSandbox(() => Promise.reject(new Error('boom')));
      ctx.location.hash = '#/packets/deadbeef?obs=1&viewPath=1';
      await ctx.window.PacketPathMap.open('deadbeef');
      const overlay = ctx.document.getElementById('packetPathModal');
      const target = fakeFocus(chain);
      ctx.__topAt = topIs === 'target' ? target : topIs === 'modal' ? overlay.children[0] || overlay : null;
      const key = ctx.__docLog.find(r => r.type === 'keydown');
      let stopped = 0;
      key.fn({ key: 'Escape', target, stopPropagation() { stopped++; } });
      const open = !!ctx.document.getElementById('packetPathModal');
      assert.strictEqual(!open, expectClosed, expectClosed ? 'modal still open' : 'modal closed');
      assert.strictEqual(stopped, expectClosed ? 1 : 0, 'stopPropagation calls: ' + stopped);
      assert.strictEqual(ctx.location.hash, expectClosed ? '#/packets/deadbeef?obs=1' : '#/packets/deadbeef?obs=1&viewPath=1');
      passed++;
      console.log('  ✅ ' + name);
    } catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
  }
  await escapeCase('Escape in the global search drawn over the modal is left to the search (#180)',
    [{}, { pos: 'fixed', cls: 'search-overlay' }], 'target', false);
  await escapeCase('Escape in a fixed nav menu / More sheet drawn over the modal is left to it (#180)',
    [{}, { cls: 'nav-more-wrap' }, { pos: 'fixed', cls: 'nav-more-menu' }], 'target', false);
  await escapeCase('Escape with focus on a control covered by the modal (e.g. the View Path button) closes the modal (#180)',
    [{}, { pos: 'fixed', cls: 'search-overlay' }], 'modal', true);
  await escapeCase('Escape with focus in the SlideOver the modal opened over closes the modal (#180)',
    [{}, { pos: 'fixed', cls: 'slide-over-panel' }], 'target', true);
  await escapeCase('Escape with focus in the mobile sheet the modal opened over closes the modal (#180)',
    [{}, { pos: 'fixed', cls: 'mobile-detail-sheet' }], 'target', true);
  await escapeCase('Escape with focus in the sticky top nav (no floating layer) closes the modal (#180)',
    [{}, { pos: 'sticky', cls: 'top-nav' }], 'target', true);

  // #208 item 4: focus inside the modal itself (its close or copy-link
  // button) is not a layer above it. The .modal-overlay is position:fixed and
  // topmost at the button, so without the overlay.contains() check the walk
  // would take the modal for a layer drawn over it and leave Escape alone.
  await (async () => {
    const name = 'Escape with focus inside the modal (its close button) closes the modal (#208)';
    try {
      const ctx = makeSandbox(() => Promise.reject(new Error('boom')));
      ctx.location.hash = '#/packets/deadbeef?obs=1&viewPath=1';
      await ctx.window.PacketPathMap.open('deadbeef');
      const overlay = ctx.document.getElementById('packetPathModal');
      const btn = ctx.document.getElementById('packetPathClose');
      assert.ok(overlay && btn && overlay.contains(btn), 'close button not inside the modal');
      overlay._pos = 'fixed';
      overlay.parentElement = ctx.document.body;
      overlay.matches = () => false;
      btn.parentElement = overlay;
      btn.getBoundingClientRect = () => ({ left: 10, top: 10, width: 20, height: 20 });
      ctx.__topAt = btn;
      const key = ctx.__docLog.find(r => r.type === 'keydown');
      let stopped = 0;
      key.fn({ key: 'Escape', target: btn, stopPropagation() { stopped++; } });
      assert.ok(!ctx.document.getElementById('packetPathModal'), 'modal still open');
      assert.strictEqual(stopped, 1, 'stopPropagation calls: ' + stopped);
      assert.strictEqual(ctx.location.hash, '#/packets/deadbeef?obs=1');
      passed++;
      console.log('  ✅ ' + name);
    } catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
  })();

  // #180: the modal closes on a route change, and Back/Forward onto the
  // #/packets/<hash>?…&viewPath=1 entry it was closed away from does not
  // reopen it. A new link to the same URL (an entry without that state)
  // and a reload of an entry whose modal was open still do.
  const ENTRY = '#/packets/deadbeef?obs=1&viewPath=1';
  async function routeCase(name, fn) {
    try {
      const ctx = makeSandbox(() => Promise.reject(new Error('boom')));
      ctx.location.hash = ENTRY;
      ctx.history.state = { other: 'kept' };
      await fn(ctx);
      passed++;
      console.log('  ✅ ' + name);
    } catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
  }
  const isOpen = (ctx) => !!ctx.document.getElementById('packetPathModal');
  const hashListeners = (ctx) => (ctx.__winListeners.hashchange || []).length;

  await routeCase('open() on a #/packets/<hash> entry marks it in history.state, keeping other state, without a URL write (#180)', async (ctx) => {
    await ctx.window.PacketPathMap.open('deadbeef');
    assert.ok(ctx.history.state.packetPathModal, 'entry not marked: ' + JSON.stringify(ctx.history.state));
    assert.strictEqual(ctx.history.state.other, 'kept');
    assert.strictEqual(ctx.__replaced.length, 0, 'URL written: ' + JSON.stringify(ctx.__replaced));
    assert.strictEqual(hashListeners(ctx), 1, 'no hashchange listener while open');
  });
  await routeCase('open() on another page (e.g. #/analytics) does not mark the entry (#180)', async (ctx) => {
    ctx.location.hash = '#/analytics?tab=distance';
    ctx.history.state = null;
    await ctx.window.PacketPathMap.open('deadbeef');
    assert.strictEqual(ctx.history.state, null);
  });
  await routeCase('a route change (Back to #/nodes) closes the modal and writes no URL (#180)', async (ctx) => {
    await ctx.window.PacketPathMap.open('deadbeef');
    ctx.__navigate('#/nodes', null);
    assert.ok(!isOpen(ctx), 'modal still open over #/nodes');
    assert.strictEqual(ctx.location.hash, '#/nodes');
    assert.strictEqual(ctx.__replaced.length, 0, 'URL written: ' + JSON.stringify(ctx.__replaced));
    assert.strictEqual(hashListeners(ctx), 0, 'hashchange listener left behind');
  });
  await routeCase('a hashchange that keeps the path (query only) keeps the modal (#180)', async (ctx) => {
    await ctx.window.PacketPathMap.open('deadbeef');
    ctx.__navigate('#/packets/deadbeef?obs=2&viewPath=1', ctx.history.state);
    assert.ok(isOpen(ctx), 'modal closed by a query-only hashchange');
  });
  await routeCase('Forward onto the entry whose modal closed on route change: restore() does not reopen it (#180)', async (ctx) => {
    await ctx.window.PacketPathMap.open('deadbeef');
    const entryState = ctx.history.state;
    ctx.__navigate('#/nodes', null);
    ctx.__navigate(ENTRY, entryState);
    assert.strictEqual(ctx.window.PacketPathMap.restore('deadbeef'), false);
    assert.ok(!isOpen(ctx), 'modal reopened from history');
  });
  await routeCase('a modal closed with Escape while another route is shown is not reopened by Back/Forward (#180)', async (ctx) => {
    await ctx.window.PacketPathMap.open('deadbeef');
    const entryState = ctx.history.state;
    // A route switch without a hashchange event (replaceState-based).
    ctx.location.hash = '#/nodes';
    ctx.history.state = null;
    ctx.__docLog.find(r => r.type === 'keydown').fn({ key: 'Escape', stopPropagation() {} });
    assert.ok(!isOpen(ctx), 'Escape did not close the modal');
    assert.strictEqual(ctx.location.hash, '#/nodes', 'the other route\'s URL was rewritten');
    ctx.__navigate(ENTRY, entryState);
    assert.strictEqual(ctx.window.PacketPathMap.restore('deadbeef'), false);
    assert.ok(!isOpen(ctx), 'modal reopened from history');
  });
  await routeCase('restore() on a new link to the same URL (entry without state) opens the modal (#180)', async (ctx) => {
    await ctx.window.PacketPathMap.open('deadbeef');
    ctx.__navigate('#/nodes', null);
    ctx.__navigate(ENTRY, null);
    assert.strictEqual(ctx.window.PacketPathMap.restore('deadbeef'), true);
    assert.ok(isOpen(ctx), 'a fresh link did not open the modal');
  });
  await routeCase('restore() on a reload of an entry whose modal was open reopens it (#180)', async (ctx) => {
    await ctx.window.PacketPathMap.open('deadbeef');
    const entryState = ctx.history.state;
    ctx.window.PacketPathMap.close();
    // A reload: same entry state, a new document (fresh module, no list).
    const ctx2 = makeSandbox(() => Promise.reject(new Error('boom')));
    ctx2.location.hash = ENTRY;
    ctx2.history.state = entryState;
    assert.strictEqual(ctx2.window.PacketPathMap.restore('deadbeef'), true);
    assert.ok(isOpen(ctx2), 'reload did not reopen the modal');
  });
  await routeCase('the closed-entry list is bounded: the oldest entry is forgotten after 50 newer ones (#180)', async (ctx) => {
    const states = [];
    for (let i = 0; i < 51; i++) {
      ctx.__navigate(ENTRY, null);
      await ctx.window.PacketPathMap.open('deadbeef');
      states.push(ctx.history.state);
      ctx.__navigate('#/nodes', null);
    }
    ctx.__navigate(ENTRY, states[1]);
    assert.strictEqual(ctx.window.PacketPathMap.restore('deadbeef'), false, 'entry 2 of 51 forgotten too early');
    ctx.window.PacketPathMap.close();
    ctx.__navigate('#/nodes', null);
    ctx.__navigate(ENTRY, states[0]);
    assert.strictEqual(ctx.window.PacketPathMap.restore('deadbeef'), true, 'the oldest of 51 entries is still remembered');
  });

  console.log('\n════════════════════════════════════════');
  console.log(`  packet-path-map.js: ${passed} passed, ${failed} failed`);
  console.log('════════════════════════════════════════');
  if (failed > 0) process.exit(1);
})();
