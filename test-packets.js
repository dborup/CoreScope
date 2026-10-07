/* Unit tests for packets.js functions (tested via VM sandbox) */
'use strict';
const vm = require('vm');
const fs = require('fs');
const assert = require('assert');

let passed = 0, failed = 0;
function test(name, fn) {
  try {
    fn();
    passed++;
    console.log(`  ✅ ${name}`);
  } catch (e) {
    failed++;
    console.log(`  ❌ ${name}: ${e.message}`);
  }
}

// A test of a bug that is known and not fixed yet. It must fail, and says so
// without failing the run. When it passes, the bug is fixed and the call has to
// become a plain test(): that is reported as a failure, so the test cannot rot.
let knownBugs = 0;
function knownBug(issue, name, fn) {
  let threw = null;
  try { fn(); } catch (e) { threw = e; }
  if (threw) {
    knownBugs++;
    console.log(`  XFAIL ${name} (known bug ${issue}): ${threw.message}`);
  } else {
    failed++;
    console.log(`  ❌ ${name}: passes now, so known bug ${issue} is fixed: change knownBug() to test()`);
  }
}

// The aria-hidden Phosphor sprite icon packets.js renders. 30627454 (#1648 M2)
// replaced the row emoji with these, one to one (💬 → chat-circle, 📡 →
// broadcast, 🔒 → lock, …).
function phIcon(name) {
  return '<svg class="ph-icon" aria-hidden="true"><use href="/icons/phosphor-sprite.svg#ph-' + name + '"/></svg>';
}

// The contents of a packet row's expand cell.
function expandCell(rowHtml) {
  const m = /<td class="col-expand"[^>]*>([\s\S]*?)<\/td>/.exec(rowHtml);
  assert(m, 'row has an expand cell');
  return m[1];
}

// Build a browser-like sandbox with all deps packets.js needs
function makeSandbox() {
  const registeredPages = {};
  const ctx = {
    window: {
      addEventListener: () => {},
      removeEventListener: () => {},
      dispatchEvent: () => {},
      innerWidth: 1200,
      PacketFilter: null,
    },
    document: {
      readyState: 'complete',
      createElement: (tag) => ({
        tagName: tag.toUpperCase(), id: '', textContent: '', innerHTML: '',
        className: '', style: {}, appendChild: () => {}, setAttribute: () => {},
        addEventListener: () => {}, querySelectorAll: () => [], querySelector: () => null,
        classList: { add: () => {}, remove: () => {}, contains: () => false },
      }),
      head: { appendChild: () => {} },
      getElementById: () => null,
      addEventListener: () => {},
      removeEventListener: () => {},
      querySelectorAll: () => [],
      querySelector: () => null,
      body: { appendChild: () => {} },
    },
    console,
    Date,
    Infinity,
    Math,
    Array,
    Object,
    String,
    Number,
    JSON,
    RegExp,
    Error,
    TypeError,
    RangeError,
    parseInt,
    parseFloat,
    isNaN,
    isFinite,
    encodeURIComponent,
    decodeURIComponent,
    setTimeout: () => {},
    clearTimeout: () => {},
    setInterval: () => {},
    clearInterval: () => {},
    fetch: () => Promise.resolve({ ok: true, json: () => Promise.resolve({}) }),
    performance: { now: () => Date.now() },
    localStorage: (() => {
      const store = {};
      return {
        getItem: k => store[k] || null,
        setItem: (k, v) => { store[k] = String(v); },
        removeItem: k => { delete store[k]; },
      };
    })(),
    location: { hash: '' },
    history: { replaceState: () => {} },
    CustomEvent: class CustomEvent {},
    Map,
    Set,
    Promise,
    URLSearchParams,
    addEventListener: () => {},
    removeEventListener: () => {},
    dispatchEvent: () => {},
    requestAnimationFrame: (cb) => setTimeout(cb, 0),
    _registeredPages: registeredPages,
    // Stub global functions packets.js depends on
    registerPage: (name, handler) => { registeredPages[name] = handler; },
  };
  vm.createContext(ctx);
  return ctx;
}

function loadInCtx(ctx, file) {
  vm.runInContext(fs.readFileSync(file, 'utf8'), ctx, { filename: file });
  for (const k of Object.keys(ctx.window)) {
    ctx[k] = ctx.window[k];
  }
}

function loadPacketsSandbox(captureRoutes = false) {
  const ctx = makeSandbox();
  // Load dependencies first
  loadInCtx(ctx, 'public/payload-labels.js');
  loadInCtx(ctx, 'public/roles.js');
  loadInCtx(ctx, 'public/app.js');
  loadInCtx(ctx, 'public/packet-helpers.js');
  if (captureRoutes) ctx.registerPage = (name, handler) => { ctx._registeredPages[name] = handler; };
  // HopDisplay stub (simpler than loading real file which may have DOM deps)
  vm.runInContext(`
    window.HopDisplay = {
      renderHop: function(h, entry, opts) {
        if (entry && entry.name) return '<span class="hop-named">' + entry.name + '</span>';
        return '<span class="hop-hex">' + h + '</span>';
      },
      _showFromBtn: function() {}
    };
  `, ctx);
  loadInCtx(ctx, 'public/packets.js');
  return ctx;
}

// ===== TESTS =====

console.log('\n=== packets.js: typeName ===');
{
  const ctx = loadPacketsSandbox();
  const api = ctx._packetsTestAPI;

  test('typeName returns known type', () => {
    assert.strictEqual(api.typeName(0), 'Request');
    assert.strictEqual(api.typeName(4), 'Advert');
    assert.strictEqual(api.typeName(5), 'Channel Msg');
  });

  test('typeName returns fallback for unknown', () => {
    assert.strictEqual(api.typeName(99), 'Type 99');
    assert.strictEqual(api.typeName(undefined), 'Type undefined');
  });
}

console.log('\n=== packets.js: obsName ===');
{
  const ctx = loadPacketsSandbox();
  const api = ctx._packetsTestAPI;

  test('obsName returns dash for falsy id', () => {
    assert.strictEqual(api.obsName(null), '—');
    assert.strictEqual(api.obsName(''), '—');
    assert.strictEqual(api.obsName(undefined), '—');
  });

  test('obsName returns id when not in observerMap', () => {
    assert.strictEqual(api.obsName('unknown-id'), 'unknown-id');
  });
}

console.log('\n=== packets.js: kv ===');
{
  const ctx = loadPacketsSandbox();
  const api = ctx._packetsTestAPI;

  test('kv produces correct HTML', () => {
    const result = api.kv('Route', 'Direct');
    assert(result.includes('byop-key'));
    assert(result.includes('Route'));
    assert(result.includes('Direct'));
    assert(result.includes('byop-val'));
  });
}

console.log('\n=== packets.js: sectionRow / fieldRow ===');
{
  const ctx = loadPacketsSandbox();
  const api = ctx._packetsTestAPI;

  test('sectionRow produces section HTML', () => {
    const result = api.sectionRow('Header');
    assert(result.includes('section-row'));
    assert(result.includes('Header'));
    assert(result.includes('colspan="4"'));
  });

  test('fieldRow produces field HTML', () => {
    const result = api.fieldRow(0, 'Header Byte', '0xFF', 'some desc');
    assert(result.includes('0'));
    assert(result.includes('Header Byte'));
    assert(result.includes('0xFF'));
    assert(result.includes('some desc'));
    assert(result.includes('mono'));
  });

  test('fieldRow handles empty description', () => {
    const result = api.fieldRow(5, 'Test', 'val', '');
    assert(result.includes('text-muted'));
  });
}

console.log('\n=== packets.js: getDetailPreview ===');
{
  const ctx = loadPacketsSandbox();
  const api = ctx._packetsTestAPI;

  test('getDetailPreview returns empty for null/undefined', () => {
    assert.strictEqual(api.getDetailPreview(null), '');
    assert.strictEqual(api.getDetailPreview(undefined), '');
  });

  test('getDetailPreview handles CHAN type', () => {
    const result = api.getDetailPreview({ type: 'CHAN', text: 'hello world', channel: 'general' });
    assert(result.includes(phIcon('chat-circle')));
    assert(result.includes('hello world'));
    assert(result.includes('chan-tag'));
    assert(result.includes('general'));
  });

  test('getDetailPreview truncates long CHAN text', () => {
    const longText = 'x'.repeat(100);
    const result = api.getDetailPreview({ type: 'CHAN', text: longText });
    assert(result.includes('…'));
    assert(!result.includes('x'.repeat(100)));
  });

  test('getDetailPreview handles ADVERT type', () => {
    const result = api.getDetailPreview({
      type: 'ADVERT', name: 'TestNode', pubKey: 'abc123',
      flags: { repeater: true }
    });
    assert(result.includes(phIcon('broadcast')));
    assert(result.includes('TestNode'));
    assert(result.includes('hop-link'));
  });

  test('getDetailPreview handles ADVERT room', () => {
    const result = api.getDetailPreview({
      type: 'ADVERT', name: 'RoomNode', pubKey: 'abc',
      flags: { room: true }
    });
    assert(result.includes(phIcon('house-line')));
  });

  test('getDetailPreview handles ADVERT sensor', () => {
    const result = api.getDetailPreview({
      type: 'ADVERT', name: 'Sensor1', pubKey: 'abc',
      flags: { sensor: true }
    });
    assert(result.includes(phIcon('thermometer')));
  });

  test('getDetailPreview handles ADVERT companion (default)', () => {
    const result = api.getDetailPreview({
      type: 'ADVERT', name: 'Comp', pubKey: 'abc',
      flags: {}
    });
    assert(result.includes(phIcon('radio')));
  });

  test('getDetailPreview handles GRP_TXT with channelHash (no_key)', () => {
    const result = api.getDetailPreview({
      type: 'GRP_TXT', channelHash: 0xAB, decryptionStatus: 'no_key'
    });
    assert(result.includes(phIcon('lock')));
    assert(result.includes('0xAB'));
    assert(result.includes('no key'));
  });

  test('getDetailPreview handles GRP_TXT decryption_failed', () => {
    const result = api.getDetailPreview({
      type: 'GRP_TXT', channelHash: 5, decryptionStatus: 'decryption_failed'
    });
    assert(result.includes('decryption failed'));
  });

  test('getDetailPreview handles GRP_TXT with channelHashHex', () => {
    const result = api.getDetailPreview({
      type: 'GRP_TXT', channelHash: 0xFF, channelHashHex: 'FF'
    });
    assert(result.includes('0xFF'));
  });

  // #1792: GRP_DATA detail preview parity with GRP_TXT.
  test('getDetailPreview handles GRP_DATA with channelHash (no_key)', () => {
    const result = api.getDetailPreview({
      type: 'GRP_DATA', channelHash: 0xAB, channelHashHex: 'AB', decryptionStatus: 'no_key'
    });
    assert(result.includes('Ch 0xAB'), 'should render channel hash hex with Ch prefix');
    assert(result.includes('no key'), 'should render no key status');
  });

  test('getDetailPreview handles GRP_DATA decryption_failed', () => {
    const result = api.getDetailPreview({
      type: 'GRP_DATA', channelHash: 5, channelHashHex: '05', decryptionStatus: 'decryption_failed'
    });
    assert(result.includes('Ch 0x05'), 'should render channel hash hex with Ch prefix');
    assert(result.includes('decryption failed'), 'should render failure status');
  });

  // #1796 polish: explicit 'encrypted' fallback when decryptionStatus is absent/pending.
  test('getDetailPreview handles GRP_DATA encrypted fallback (no decryptionStatus)', () => {
    const result = api.getDetailPreview({
      type: 'GRP_DATA', channelHash: 0xAB, channelHashHex: 'AB'
    });
    assert(result.includes('Ch 0xAB'), 'should render channel hash hex with Ch prefix');
    assert(result.includes('encrypted'), 'should render encrypted fallback label');
  });

  // #1796 polish: decrypted-but-malformed — status is 'decrypted' but dataType is null
  // because inner payload was too short to parse (cmd/ingestor/decoder.go:619-654).
  test('getDetailPreview handles GRP_DATA decrypted-but-malformed (dataType null)', () => {
    const result = api.getDetailPreview({
      type: 'GRP_DATA',
      channelHash: 0xAB,
      channelHashHex: 'AB',
      decryptionStatus: 'decrypted',
      dataType: null
    });
    assert(result.includes('Ch 0xAB'), 'should render channel hash hex with Ch prefix');
    assert(result.includes('malformed'), 'should label decrypted-but-malformed inner explicitly');
    assert(!result.includes('encrypted'), 'must NOT mislabel decrypted packet as encrypted');
  });

  // #1796 r1 adversarial — pin the `!decoded.error` guard on the happy-path branch
  // (public/packets.js:2841). Backend cmd/ingestor/decoder.go:619-654 sets BOTH
  // DataType=0xNN AND Error='data_len exceeds buffer' for the malformed inner case
  // where data_len > available_len. Without the `!decoded.error` guard, this row
  // would mis-render as `type=0x0001 len=0` (a confident-looking happy-path label)
  // instead of falling through to the explicit `(decrypted, malformed)` branch.
  // Regression pin: if a future refactor drops the `!decoded.error` clause, this
  // test fails. (Verified locally: removing `&& !decoded.error` makes this fail.)
  test('getDetailPreview routes decrypted+dataType+error through malformed branch (#1796 r1 adv)', () => {
    const result = api.getDetailPreview({
      type: 'GRP_DATA',
      channelHash: 0x12,
      channelHashHex: '12',
      decryptionStatus: 'decrypted',
      dataType: 0x0001,
      dataLen: 0,
      error: 'data_len exceeds buffer'
      // decryptedBlob absent (omitempty); backend Error field set.
    });
    assert(result.includes('Ch 0x12'), 'should still render channel hash hex');
    assert(result.includes('malformed'),
      'must label as malformed when Error is set, NOT confidently render type=0xNN');
    assert(!result.includes('type=0x0001'),
      'must NOT show a happy-path type=0xNN header when inner parse errored');
    assert(!result.includes('len=0'),
      'must NOT show a happy-path len=N header when inner parse errored');
  });

  // #1796 r1 regression — data_len=0 is a LEGITIMATE empty datagram per firmware
  // (BaseChatMesh.cpp:387: `data_len > available_len` is the only reject; 0 is allowed).
  // Backend cmd/ingestor/decoder.go:142 marshals DecryptedBlob with `omitempty`, so a
  // valid data_len=0 packet arrives with an empty/absent blob and no error. Frontend
  // must render the header (type=...len=0) WITHOUT a <code> block and MUST NOT label
  // it 'malformed'. (Same assertion covers the round-0 'tightened gate' regression.)
  test('getDetailPreview renders GRP_DATA data_len=0 empty datagram (no code block, not malformed)', () => {
    const result = api.getDetailPreview({
      type: 'GRP_DATA',
      channelHash: 0x12,
      channelHashHex: '12',
      decryptionStatus: 'decrypted',
      dataType: 0x0001,
      dataLen: 0
      // decryptedBlob absent (backend `omitempty`); no error.
    });
    assert(result.includes('Ch 0x12'), 'should render channel hash hex');
    assert(result.includes('type=0x0001'), 'should render data_type as hex');
    assert(result.includes('len=0'), 'should render data_len=0');
    assert(!result.includes('<code>'), 'must NOT render any <code> block when blob is empty');
    assert(!result.includes('malformed'), 'data_len=0 is a legitimate empty datagram, NOT malformed');
  });

  test('getDetailPreview handles GRP_DATA decrypted with data_type and blob', () => {
    const result = api.getDetailPreview({
      type: 'GRP_DATA',
      channelHash: 0x12,
      channelHashHex: '12',
      decryptionStatus: 'decrypted',
      dataType: 0x0001,
      dataLen: 4,
      decryptedBlob: 'deadbeef'
    });
    assert(result.includes('0x12'), 'should render channel hash hex');
    assert(result.includes('0x0001'), 'should render data_type as hex');
    assert(result.includes('len=4'), 'should render data_len with len= label');
    assert(result.includes('deadbeef'), 'should render blob hex');
  });

  test('getDetailPreview handles GRP_DATA decrypted truncates long blob', () => {
    const longBlob = 'ab'.repeat(64); // 128 hex chars = 64 bytes
    const result = api.getDetailPreview({
      type: 'GRP_DATA',
      channelHash: 0x12,
      channelHashHex: '12',
      decryptionStatus: 'decrypted',
      dataType: 0,
      dataLen: 64,
      decryptedBlob: longBlob
    });
    // Adversarial #3: assert EXACT rendered <code> content via regex, not substring.
    // Substring match would still pass at cutoff 40/48 because 'ab'.repeat(16)+'…'
    // is a prefix of any longer rendered blob. Pin: exactly 32 hex chars + ellipsis,
    // and nothing else inside the <code> tag.
    const codeMatch = result.match(/<code>([^<]*)<\/code>/);
    assert(codeMatch, 'should render a <code> block');
    assert.strictEqual(codeMatch[1], 'ab'.repeat(16) + '…',
      `<code> content must be exactly 32 hex chars + ellipsis, got: ${JSON.stringify(codeMatch[1])}`);
  });

  // Boundary: blob with EXACTLY 32 hex chars renders WITHOUT ellipsis (.length > 32 is strict).
  test('getDetailPreview renders GRP_DATA blob of exactly 32 hex chars without ellipsis', () => {
    const exactBlob = 'cd'.repeat(16); // 32 hex chars
    const result = api.getDetailPreview({
      type: 'GRP_DATA',
      channelHash: 0x12,
      channelHashHex: '12',
      decryptionStatus: 'decrypted',
      dataType: 0,
      dataLen: 16,
      decryptedBlob: exactBlob
    });
    const codeMatch = result.match(/<code>([^<]*)<\/code>/);
    assert(codeMatch, 'should render a <code> block at the 32-char boundary');
    assert.strictEqual(codeMatch[1], exactBlob,
      'exactly-32-char blob must render verbatim, no ellipsis');
    assert(!result.includes('…'), 'must NOT append ellipsis at the boundary');
  });

  // Item 6: channelHash=0 — confirms the `!= null` gate (not truthy check) so falsy 0
  // still enters the GRP_DATA branch and renders `Ch 0x00`.
  test('getDetailPreview handles GRP_DATA channelHash=0 (falsy but valid)', () => {
    const result = api.getDetailPreview({
      type: 'GRP_DATA',
      channelHash: 0,
      decryptionStatus: 'no_key'
    });
    assert(result.includes('Ch 0x00'),
      'channelHash=0 must render Ch 0x00 (falsy 0 passes `!= null` gate)');
    assert(result.includes('no key'), 'should render no key status');
  });

  test('getDetailPreview handles TXT_MSG', () => {
    const result = api.getDetailPreview({
      type: 'TXT_MSG', srcHash: 'abcdef01', destHash: '12345678'
    });
    assert(result.includes(phIcon('envelope')));
    assert(result.includes('abcdef01'));
    assert(result.includes('12345678'));
  });

  test('getDetailPreview handles PATH', () => {
    const result = api.getDetailPreview({
      type: 'PATH', srcHash: 'aabb', destHash: 'ccdd'
    });
    assert(result.includes(phIcon('shuffle')));
  });

  test('getDetailPreview handles REQ', () => {
    const result = api.getDetailPreview({
      type: 'REQ', srcHash: 'aa', destHash: 'bb'
    });
    assert(result.includes(phIcon('lock')));
    assert(result.includes('aa'));
  });

  test('getDetailPreview handles RESPONSE', () => {
    const result = api.getDetailPreview({
      type: 'RESPONSE', srcHash: 'aa', destHash: 'bb'
    });
    assert(result.includes(phIcon('lock')));
  });

  test('getDetailPreview handles ANON_REQ', () => {
    const result = api.getDetailPreview({
      type: 'ANON_REQ', destHash: 'dd'
    });
    assert(result.includes('anon'));
    assert(result.includes('dd'));
  });

  // #1864 — ANON_REQ carries the sender's FULL source pubkey (unlike
  // REQ/RESPONSE's 1-byte srcHash), so it's not really anonymous. Once
  // known, the row preview should show the truncated pubkey, not "anon".
  // ephemeralPubKey is the legacy field name (pre-#1866 backend rename) —
  // packets decoded before the rename still carry it, so it must still work.
  test('getDetailPreview shows truncated ephemeralPubKey for ANON_REQ instead of "anon" (legacy field)', () => {
    const result = api.getDetailPreview({
      type: 'ANON_REQ', destHash: 'dd', ephemeralPubKey: 'ab'.repeat(32),
    });
    assert(result.includes('abababab'), 'should show the truncated pubkey prefix, got: ' + result);
    assert(!result.includes('anon'), 'should not show "anon" when a pubkey is present, got: ' + result);
    assert(!result.includes('ab'.repeat(32)), 'should NOT render the full raw pubkey in the row preview, got: ' + result);
  });

  // #1866 — srcPubKey is the current backend field name for ANON_REQ's
  // sender key (renamed from ephemeralPubKey so store.go's node indexer
  // picks it up). Row preview must read it the same way.
  test('getDetailPreview shows truncated srcPubKey for ANON_REQ (current field name)', () => {
    const result = api.getDetailPreview({
      type: 'ANON_REQ', destHash: 'dd', srcPubKey: 'ab'.repeat(32),
    });
    assert(result.includes('abababab'), 'should show the truncated pubkey prefix, got: ' + result);
    assert(!result.includes('anon'), 'should not show "anon" when a pubkey is present, got: ' + result);
  });

  test('getDetailPreview prefers srcPubKey over legacy ephemeralPubKey when both are present', () => {
    const result = api.getDetailPreview({
      type: 'ANON_REQ', destHash: 'dd', srcPubKey: 'ab'.repeat(32), ephemeralPubKey: 'cd'.repeat(32),
    });
    assert(result.includes('abababab'), 'should use srcPubKey, got: ' + result);
    assert(!result.includes('cdcdcdcd'), 'should NOT use the legacy field when srcPubKey is present, got: ' + result);
  });

  test('getDetailPreview resolves ANON_REQ sender to a node name via HopResolver.nameForKey when known', () => {
    const key = 'ab'.repeat(32);
    // Real browsers alias bare globals with window.<name>; the code checks
    // both (window.HopResolver as an existence guard, bare HopResolver for
    // the call) so the sandbox mock needs both bound to the same object.
    const mock = { nameForKey: (k) => (k === key ? 'KnownSender' : null) };
    ctx.window.HopResolver = mock;
    ctx.HopResolver = mock;
    try {
      const result = api.getDetailPreview({ type: 'ANON_REQ', destHash: 'dd', srcPubKey: key });
      assert(result.includes('KnownSender'), 'should show the resolved node name, got: ' + result);
      assert(!result.includes('abababab'), 'should not show truncated hex once resolved to a name, got: ' + result);
    } finally {
      ctx.window.HopResolver = null;
      delete ctx.HopResolver;
    }
  });

  test('getDetailPreview handles text fallback', () => {
    const result = api.getDetailPreview({ text: 'some message' });
    assert(result.includes('some message'));
  });

  test('getDetailPreview truncates long text fallback', () => {
    const result = api.getDetailPreview({ text: 'z'.repeat(100) });
    assert(result.includes('…'));
  });

  test('getDetailPreview handles public_key fallback', () => {
    const result = api.getDetailPreview({ public_key: 'abcdef1234567890abcdef' });
    assert(result.includes(phIcon('broadcast')));
    assert(result.includes('abcdef1234567890'));
  });

  test('getDetailPreview returns empty for empty decoded', () => {
    assert.strictEqual(api.getDetailPreview({}), '');
  });

  // #1802 — CONTROL DISCOVER_REQ / DISCOVER_RESP should be rendered (not just
  // hex). Backend cmd/ingestor/decoder.go decodeControl() emits ctrlSubtype +
  // body fields; the detail preview must surface them.
  test('getDetailPreview handles CONTROL DISCOVER_REQ', () => {
    const result = api.getDetailPreview({
      type: 'CONTROL',
      ctrlSubtype: 'DISCOVER_REQ',
      ctrlFilter: 2,
      ctrlTag: 0xDEADBEEF,
      ctrlSince: 0x11223344,
    });
    assert(result.includes('DISCOVER_REQ'), 'should label subtype');
    assert(result.includes('filter'), 'should render filter field');
    assert(result.includes('tag'), 'should render tag field');
  });

  // #1868 — filter is a bitmask (ADV_TYPE_* bit-per-type, per firmware's
  // `filter & (1 << ADV_TYPE_x)`); bit 2 = ADV_TYPE_REPEATER, so filter=4
  // (1<<2) must render as the human-readable type name, not raw hex.
  test('getDetailPreview renders CONTROL DISCOVER_REQ filter as type name(s), not raw hex', () => {
    const result = api.getDetailPreview({
      type: 'CONTROL',
      ctrlSubtype: 'DISCOVER_REQ',
      ctrlFilter: 4, // 1 << 2 = ADV_TYPE_REPEATER
    });
    assert(result.includes('Repeater'), 'should show "Repeater" for filter bit 2, got: ' + result);
    assert(!/filter=0x/.test(result), 'should not fall back to raw hex when bits are known, got: ' + result);
  });

  test('getDetailPreview handles CONTROL DISCOVER_RESP', () => {
    const result = api.getDetailPreview({
      type: 'CONTROL',
      ctrlSubtype: 'DISCOVER_RESP',
      ctrlNodeType: 2,
      ctrlSNR: 16,
      ctrlTag: 0x11223344,
      ctrlPubKey: '0001020304050607',
    });
    assert(result.includes('DISCOVER_RESP'), 'should label subtype');
    assert(result.includes('snr') || result.includes('SNR'), 'should render snr');
    // #1868: pubkey truncated to first 8 hex chars for the per-row preview
    // (no live node lookup per row -- see the async detail-panel resolution
    // instead), and full raw hex must NOT leak into the row.
    assert(result.includes('00010203'), 'should render truncated pubkey prefix, got: ' + result);
    assert(!result.includes('0001020304050607'), 'should NOT render the full raw pubkey in the row preview, got: ' + result);
  });

  // #1868 — node_type (ADV_TYPE_REPEATER=2) must render as "Repeater", not
  // the raw number.
  test('getDetailPreview renders CONTROL DISCOVER_RESP node type as a name, not a raw number', () => {
    const result = api.getDetailPreview({
      type: 'CONTROL',
      ctrlSubtype: 'DISCOVER_RESP',
      ctrlNodeType: 2,
    });
    assert(result.includes('Repeater'), 'should show "Repeater" for node type 2, got: ' + result);
    assert(!/type=2\b/.test(result), 'should not show the raw type number, got: ' + result);
  });

  // #1868 — SNR is wire-encoded (value * 4); a raw 16 must display as 4.00 dB.
  test('getDetailPreview converts CONTROL DISCOVER_RESP SNR from wire units to dB', () => {
    const result = api.getDetailPreview({
      type: 'CONTROL',
      ctrlSubtype: 'DISCOVER_RESP',
      ctrlSNR: 16,
    });
    assert(result.includes('4.00dB') || result.includes('4.00 dB'), 'should show 16/4.0=4.00 dB, got: ' + result);
    assert(!/snr=16(?!\.)/.test(result), 'should not show the raw wire SNR value, got: ' + result);
  });

  test('getDetailPreview handles CONTROL UNKNOWN subtype', () => {
    const result = api.getDetailPreview({
      type: 'CONTROL',
      ctrlSubtype: 'UNKNOWN',
      ctrlFlags: 'a0',
    });
    assert(result.includes('UNKNOWN') || result.includes('CONTROL'),
      'should at least label the unknown subtype');
  });
}

console.log('\n=== packets.js: getPathHopCount ===');
{
  const ctx = loadPacketsSandbox();
  const api = ctx._packetsTestAPI;

  test('getPathHopCount with valid path', () => {
    assert.strictEqual(api.getPathHopCount({ path_json: '["a","b","c"]' }), 3);
  });

  test('getPathHopCount with empty path', () => {
    assert.strictEqual(api.getPathHopCount({ path_json: '[]' }), 0);
  });

  test('getPathHopCount with null/missing', () => {
    assert.strictEqual(api.getPathHopCount({}), 0);
    assert.strictEqual(api.getPathHopCount({ path_json: null }), 0);
  });

  test('getPathHopCount with invalid JSON', () => {
    assert.strictEqual(api.getPathHopCount({ path_json: 'not json' }), 0);
  });
}

console.log('\n=== packets.js: sortGroupChildren ===');
{
  const ctx = loadPacketsSandbox();
  const api = ctx._packetsTestAPI;

  test('sortGroupChildren handles null/empty gracefully', () => {
    api.sortGroupChildren(null);
    api.sortGroupChildren({});
    api.sortGroupChildren({ _children: [] });
    // No throw
  });

  test('sortGroupChildren default sort groups by observer earliest-first', () => {
    // Need to set obsSortMode — it reads from closure. Default is 'observer'.
    const group = {
      _children: [
        { observer_name: 'B', timestamp: '2024-01-01T02:00:00Z' },
        { observer_name: 'A', timestamp: '2024-01-01T01:00:00Z' },
        { observer_name: 'B', timestamp: '2024-01-01T01:30:00Z' },
      ]
    };
    api.sortGroupChildren(group);
    // A has earliest timestamp, should be first
    assert.strictEqual(group._children[0].observer_name, 'A');
    // Then B entries
    assert.strictEqual(group._children[1].observer_name, 'B');
    assert.strictEqual(group._children[2].observer_name, 'B');
    // B entries should be time-ascending within group
    assert(group._children[1].timestamp < group._children[2].timestamp);
  });

  test('sortGroupChildren updates header from first child', () => {
    const group = {
      observer_id: 'old',
      _children: [
        { observer_name: 'A', observer_id: 'new-id', timestamp: '2024-01-01T01:00:00Z', snr: 10, rssi: -50, path_json: '["x"]', direction: 'rx' },
      ]
    };
    api.sortGroupChildren(group);
    assert.strictEqual(group.observer_id, 'new-id');
    assert.strictEqual(group.snr, 10);
    assert.strictEqual(group.rssi, -50);
    assert.strictEqual(group.path_json, '["x"]');
    assert.strictEqual(group.direction, 'rx');
  });
}

console.log('\n=== packets.js: renderTimestampCell ===');
{
  const ctx = loadPacketsSandbox();
  const api = ctx._packetsTestAPI;

  test('renderTimestampCell produces HTML with timestamp-text', () => {
    const result = api.renderTimestampCell('2024-01-15T10:30:00Z');
    assert(result.includes('timestamp-text'));
  });

  test('renderTimestampCell handles null gracefully', () => {
    const result = api.renderTimestampCell(null);
    // Should not throw, produces some output
    assert(typeof result === 'string');
  });
}

console.log('\n=== packets.js: renderPath ===');
{
  const ctx = loadPacketsSandbox();
  const api = ctx._packetsTestAPI;

  test('renderPath returns dash for empty/null', () => {
    assert.strictEqual(api.renderPath(null, null), '—');
    assert.strictEqual(api.renderPath([], null), '—');
  });

  test('renderPath renders hops with arrows', () => {
    const result = api.renderPath(['aa', 'bb'], null);
    assert(result.includes('arrow'));
    assert(result.includes('aa'));
    assert(result.includes('bb'));
  });

  test('renderPath renders single hop without arrow', () => {
    const result = api.renderPath(['cc'], null);
    assert(result.includes('cc'));
    assert(!result.includes('arrow'));
  });
}

console.log('\n=== packets.js: renderDecodedPacket ===');
{
  const ctx = loadPacketsSandbox();
  const api = ctx._packetsTestAPI;

  test('renderDecodedPacket produces header section', () => {
    const decoded = {
      header: { routeType: 0, payloadType: 4, payloadVersion: 1 },
      payload: { name: 'TestNode' },
      path: { hops: [] }
    };
    const hex = 'aabbccdd';
    const result = api.renderDecodedPacket(decoded, hex);
    assert(result.includes('byop-decoded'));
    assert(result.includes('Header'));
    assert(result.includes('4 bytes'));
  });

  test('renderDecodedPacket renders path hops', () => {
    const decoded = {
      header: { routeType: 0, payloadType: 4 },
      payload: {},
      path: { hops: ['aa', 'bb'] }
    };
    const hex = 'aabbccdd';
    const result = api.renderDecodedPacket(decoded, hex);
    assert(result.includes('Path (2 hops)'));
    assert(result.includes('aa'));
    assert(result.includes('bb'));
  });

  test('renderDecodedPacket renders payload fields', () => {
    const decoded = {
      header: { routeType: 0, payloadType: 5 },
      payload: { channel: 'general', text: 'hello' },
      path: { hops: [] }
    };
    const hex = 'aabb';
    const result = api.renderDecodedPacket(decoded, hex);
    assert(result.includes('channel'));
    assert(result.includes('general'));
    assert(result.includes('hello'));
  });

  test('renderDecodedPacket renders nested objects as JSON', () => {
    const decoded = {
      header: { routeType: 0, payloadType: 0 },
      payload: { flags: { repeater: true } },
      path: { hops: [] }
    };
    const hex = 'aa';
    const result = api.renderDecodedPacket(decoded, hex);
    assert(result.includes('byop-pre'));
    assert(result.includes('repeater'));
  });

  test('renderDecodedPacket skips null payload values', () => {
    const decoded = {
      header: { routeType: 0, payloadType: 0 },
      payload: { a: null, b: undefined, c: 'visible' },
      path: { hops: [] }
    };
    const hex = 'aa';
    const result = api.renderDecodedPacket(decoded, hex);
    assert(result.includes('visible'));
    // null/undefined values should be skipped
    const kvCount = (result.match(/byop-row/g) || []).length;
    // Only 'c' should appear in payload (a and b are null/undefined), plus header fields
    assert(kvCount >= 1);
  });

  test('renderDecodedPacket renders raw hex', () => {
    const decoded = {
      header: { routeType: 0, payloadType: 0 },
      payload: {},
      path: { hops: [] }
    };
    const hex = 'aabbcc';
    const result = api.renderDecodedPacket(decoded, hex);
    assert(result.includes('AA BB CC'));
    assert(result.includes('byop-hex'));
  });
}

console.log('\n=== packets.js: buildFieldTable ===');
{
  const ctx = loadPacketsSandbox();
  const api = ctx._packetsTestAPI;

  test('buildFieldTable produces table HTML', () => {
    const pkt = { raw_hex: 'c0400102', route_type: 1, payload_type: 4 };
    const decoded = { type: 'ADVERT', name: 'Node', pubKey: 'abc', flags: { type: 2, hasLocation: false, hasName: true, raw: 0x22 } };
    const result = api.buildFieldTable(pkt, decoded, [], []);
    assert(result.includes('field-table'));
    assert(result.includes('Header'));
    assert(result.includes('Header Byte'));
    assert(result.includes('Path Length'));
  });

  test('buildFieldTable handles transport codes (route_type 0)', () => {
    const pkt = { raw_hex: 'c0400102030405060708', route_type: 0, payload_type: 0 };
    const decoded = { destHash: 'aa', srcHash: 'bb', mac: 'cc', encryptedData: 'dd' };
    const result = api.buildFieldTable(pkt, decoded, [], []);
    assert(result.includes('Transport Codes'));
    assert(result.includes('Next Hop'));
    assert(result.includes('Last Hop'));
  });

  test('buildFieldTable renders path hops', () => {
    const pkt = { raw_hex: 'c042aabb', route_type: 1, payload_type: 0 };
    const decoded = { destHash: 'xx' };
    const result = api.buildFieldTable(pkt, decoded, ['aa', 'bb'], []);
    assert(result.includes('Path (2 hops)'));
    assert(result.includes('Hop 0'));
    assert(result.includes('Hop 1'));
  });

  test('buildFieldTable renders ADVERT payload', () => {
    const pkt = { raw_hex: 'c040', route_type: 1, payload_type: 4 };
    const decoded = {
      type: 'ADVERT', pubKey: 'abc123', timestamp: 1234567890,
      timestampISO: '2009-02-13T23:31:30Z', signature: 'sig',
      name: 'TestNode',
      flags: { type: 1, hasLocation: true, hasName: true, raw: 0x55 }
    };
    const result = api.buildFieldTable(pkt, decoded, [], []);
    assert(result.includes('Public Key'));
    assert(result.includes('Timestamp'));
    assert(result.includes('Signature'));
    assert(result.includes('App Flags'));
    assert(result.includes('Companion'));
    assert(result.includes('Latitude'));
    assert(result.includes('Node Name'));
  });

  test('buildFieldTable renders GRP_TXT payload', () => {
    const pkt = { raw_hex: 'c040', route_type: 1, payload_type: 5 };
    const decoded = { type: 'GRP_TXT', channelHash: 0xAB, mac: 'AABB', encryptedData: 'data', decryptionStatus: 'no_key' };
    const result = api.buildFieldTable(pkt, decoded, [], []);
    assert(result.includes('Channel Hash'));
    assert(result.includes('MAC'));
    assert(result.includes('Encrypted Data'));
  });

  test('buildFieldTable renders CHAN payload', () => {
    const pkt = { raw_hex: 'c040', route_type: 1, payload_type: 5 };
    const decoded = { type: 'CHAN', channel: 'general', sender: 'Alice', sender_timestamp: '12:00' };
    const result = api.buildFieldTable(pkt, decoded, [], []);
    assert(result.includes('Channel'));
    assert(result.includes('general'));
    assert(result.includes('Sender'));
    assert(result.includes('Sender Time'));
  });

  test('buildFieldTable renders ACK payload', () => {
    const pkt = { raw_hex: 'c040', route_type: 1, payload_type: 3 };
    const decoded = { type: 'ACK', ackChecksum: 'DEADBEEF' };
    const result = api.buildFieldTable(pkt, decoded, [], []);
    assert(result.includes('Checksum'));
    assert(result.includes('DEADBEEF'));
  });

  test('buildFieldTable renders destHash-based payload', () => {
    const pkt = { raw_hex: 'c040', route_type: 1, payload_type: 2 };
    const decoded = { destHash: 'DD', srcHash: 'SS', mac: 'MM', encryptedData: 'EE' };
    const result = api.buildFieldTable(pkt, decoded, [], []);
    assert(result.includes('Dest Hash'));
    assert(result.includes('Src Hash'));
  });

  test('buildFieldTable renders raw fallback for unknown payload', () => {
    const pkt = { raw_hex: 'c040aabbccdd', route_type: 1, payload_type: 99 };
    const decoded = {};
    const result = api.buildFieldTable(pkt, decoded, [], []);
    assert(result.includes('Raw'));
  });

  // #1868 — CONTROL no longer falls through to the generic "Raw" row; it
  // gets a proper field breakdown matching decodeControl()'s byte layout.
  test('buildFieldTable renders CONTROL DISCOVER_REQ with human-readable filter', () => {
    const pkt = { raw_hex: 'c040', route_type: 1, payload_type: 11 };
    const decoded = { type: 'CONTROL', ctrlSubtype: 'DISCOVER_REQ', ctrlFilter: 4, ctrlTag: 0xDEADBEEF, ctrlSince: 0x11223344 };
    const result = api.buildFieldTable(pkt, decoded, [], []);
    assert(!result.includes('>Raw<'), 'should not fall through to the generic Raw row, got: ' + result);
    assert(result.includes('DISCOVER_REQ'));
    assert(result.includes('Repeater'), 'filter=4 (1<<2) should show "Repeater", got: ' + result);
    assert(result.includes('DEADBEEF'));
  });

  test('buildFieldTable renders CONTROL DISCOVER_RESP with converted SNR and truncated pubkey when node is unresolved', () => {
    const pkt = { raw_hex: 'c040', route_type: 1, payload_type: 11 };
    const decoded = { type: 'CONTROL', ctrlSubtype: 'DISCOVER_RESP', ctrlNodeType: 2, ctrlSNR: 16, ctrlPubKey: '00'.repeat(32) };
    // 5th arg (ctrlPubKeyNode) omitted -- unresolved case.
    const result = api.buildFieldTable(pkt, decoded, [], []);
    assert(result.includes('Repeater'), 'node type 2 should show "Repeater", got: ' + result);
    assert(result.includes('4.00 dB'), 'SNR 16/4.0 should show 4.00 dB, got: ' + result);
    assert(!result.includes('#/nodes/'), 'should not render a node link when unresolved, got: ' + result);
  });

  test('buildFieldTable renders CONTROL DISCOVER_RESP pubkey as a clickable node link when resolved', () => {
    const pkt = { raw_hex: 'c040', route_type: 1, payload_type: 11 };
    const decoded = { type: 'CONTROL', ctrlSubtype: 'DISCOVER_RESP', ctrlPubKey: 'ab'.repeat(32) };
    const ctrlPubKeyNode = { public_key: 'ab'.repeat(32), name: 'KnownRepeater' };
    const result = api.buildFieldTable(pkt, decoded, [], [], ctrlPubKeyNode);
    assert(result.includes('#/nodes/' + ctrlPubKeyNode.public_key), 'should link to the resolved node, got: ' + result);
    assert(result.includes('KnownRepeater'), 'should show the resolved node name, got: ' + result);
  });

  // #1864/#1866 — ANON_REQ must NOT fall into the generic destHash
  // (REQ/RESPONSE) branch: it has a full 32B source pubkey where
  // REQ/RESPONSE has a 1B srcHash, so the MAC/Encrypted Data byte offsets
  // differ (33/35 vs 2/4). srcPubKey is the current field name (#1866
  // backend rename, so store.go's node indexer picks up ANON_REQ senders).
  test('buildFieldTable renders ANON_REQ with its own field layout, not the generic destHash one', () => {
    const pkt = { raw_hex: 'c040', route_type: 1, payload_type: 7 };
    const decoded = { type: 'ANON_REQ', destHash: 'dd', srcPubKey: 'ab'.repeat(32), mac: 'CAFE', encryptedData: 'beefbeef' };
    const result = api.buildFieldTable(pkt, decoded, [], []);
    assert(result.includes('Dest Hash'));
    assert(result.includes('Src Public Key'), 'should have its own pubkey field, not fall through to Src Hash, got: ' + result);
    assert(!result.includes('Src Hash'), 'should NOT show the REQ/RESPONSE Src Hash field, got: ' + result);
    assert(result.includes('CAFE'));
  });

  test('buildFieldTable renders ANON_REQ srcPubKey as truncated hex when unresolved', () => {
    const pkt = { raw_hex: 'c040', route_type: 1, payload_type: 7 };
    const decoded = { type: 'ANON_REQ', destHash: 'dd', srcPubKey: 'cd'.repeat(32) };
    const result = api.buildFieldTable(pkt, decoded, [], []);
    assert(!result.includes('#/nodes/'), 'should not render a node link when unresolved, got: ' + result);
    assert(result.includes('cdcdcdcd'), 'should show truncated pubkey hex, got: ' + result);
  });

  test('buildFieldTable renders ANON_REQ srcPubKey as a clickable node link when resolved', () => {
    const pkt = { raw_hex: 'c040', route_type: 1, payload_type: 7 };
    const decoded = { type: 'ANON_REQ', destHash: 'dd', srcPubKey: 'ef'.repeat(32) };
    const anonReqSenderNode = { public_key: 'ef'.repeat(32), name: 'KnownSender' };
    const result = api.buildFieldTable(pkt, decoded, [], [], null, anonReqSenderNode);
    assert(result.includes('#/nodes/' + anonReqSenderNode.public_key), 'should link to the resolved node, got: ' + result);
    assert(result.includes('KnownSender'), 'should show the resolved node name, got: ' + result);
  });

  // #1866 — packets decoded before the backend rename only carry the legacy
  // ephemeralPubKey field; buildFieldTable must still render them correctly.
  test('buildFieldTable falls back to legacy ephemeralPubKey when srcPubKey is absent', () => {
    const pkt = { raw_hex: 'c040', route_type: 1, payload_type: 7 };
    const decoded = { type: 'ANON_REQ', destHash: 'dd', ephemeralPubKey: 'cd'.repeat(32) };
    const result = api.buildFieldTable(pkt, decoded, [], []);
    assert(result.includes('Src Public Key'), 'should still render the field under its current label, got: ' + result);
    assert(result.includes('cdcdcdcd'), 'should show the legacy field\'s truncated pubkey hex, got: ' + result);
  });

  // PR #212 replaced the "direct advert" wording: the path byte encodes the
  // sender's width even with zero relay hops, so a flood's width is reported
  // and only the cases that genuinely carry no width stay unknown. The byte
  // breakdown must still say WHICH case it is, not just "unknown".
  test('buildFieldTable reports a zero-hop flood width instead of "direct advert"', () => {
    // header 0x01 = route 1 (FLOOD), payload 0 (ADVERT); path byte 0x40 →
    // bits 7-6 = 1 → hash_size 2, hash_count = 0.
    const pkt = { raw_hex: '0140', route_type: 1, payload_type: 0 };
    const result = api.buildFieldTable(pkt, {}, [], []);
    assert(result.includes('hash_size=2 bytes, hash_count=0'),
      'zero-hop flood must keep its encoded width, got: ' + result);
    assert(!result.includes('direct advert'), 'stale "direct advert" wording resurfaced');
  });

  test('buildFieldTable marks a 0b11 width field as invalid rather than hiding it', () => {
    // Path byte 0xC1 → bits 7-6 = 3, which is no width at all: the evidence
    // model (cmd/server/observed_path_hash_sizes.go) knows only 1/2/3 bytes.
    const pkt = { raw_hex: '01C1aabbccdd', route_type: 1, payload_type: 0 };
    const result = api.buildFieldTable(pkt, {}, [], []);
    assert(result.includes('hash_count=1'), 'hop count lost, got: ' + result);
    assert(result.includes('width bits 7-6 = 3'), 'invalid width field not explained, got: ' + result);
    assert(!result.includes('hash_size=4'), 'a 4-byte hash size must not be claimed');
  });

  test('buildFieldTable keeps the direct zero-hop marker distinct from an invalid width', () => {
    // header 0x02 = route 2 (DIRECT), path byte 0x00 = sendZeroHop's marker.
    const pkt = { raw_hex: '0200', route_type: 2, payload_type: 0 };
    const result = api.buildFieldTable(pkt, {}, [], []);
    assert(result.includes('hash_count=0 (no encoded hash size)'),
      'direct zero-hop marker description drifted, got: ' + result);
  });

  // #322 (2): the zero-hop 0x00 marker is also valid on TRANSPORT_DIRECT
  // (route 3, firmware/src/Packet.h isRouteDirect()), where the path-length byte
  // sits at offset 5 behind the transport codes. Nothing covered route 3, so the
  // direct-marker mutant `(route === 2 || route === 3)` → `(route === 2)`
  // survived: it would relabel this 0x00 as hash_size=1. This case kills it.
  test('buildFieldTable keeps the direct zero-hop marker on TRANSPORT_DIRECT (route 3, byte 5)', () => {
    // header 0x17 = route 3 (TRANSPORT_DIRECT), path-type 5 (hops). 4 transport
    // code bytes (aabbccdd), then path byte 0x00 at offset 5 = sendZeroHop's
    // marker. route 3 must be treated like route 2: no encoded hash size.
    const pkt = { raw_hex: '17aabbccdd00', route_type: 3, payload_type: 5 };
    const result = api.buildFieldTable(pkt, {}, [], []);
    assert(result.includes('hash_count=0 (no encoded hash size)'),
      'TRANSPORT_DIRECT zero-hop marker must read as no encoded hash size, got: ' + result);
    assert(!/hash_size=\d/.test(result),
      'the 0x00 direct marker must not be read as a hash width on route 3, got: ' + result);
  });

  test('buildFieldTable does not read a hash width out of TRACE SNR bytes', () => {
    // header 0x25 = payload 9 (TRACE), route 1. Its path bytes are SNR
    // readings (internal/packetpath/route.go PathBytesAreHops), not hops.
    const pkt = { raw_hex: '2541aabbccdd', route_type: 1, payload_type: 9 };
    const result = api.buildFieldTable(pkt, {}, [], []);
    assert(result.includes('TRACE: path bytes are SNR'), 'TRACE path bytes mislabelled, got: ' + result);
    assert(!result.includes('hash_size='), 'TRACE must not claim a hash size');
  });

  test('buildFieldTable handles empty raw_hex', () => {
    const pkt = { raw_hex: '', route_type: 1, payload_type: 0 };
    const decoded = {};
    const result = api.buildFieldTable(pkt, decoded, [], []);
    assert(result.includes('field-table'));
    assert(result.includes('0B') || result.includes('0 bytes') || result.includes('??'));
  });

  // #282 (7): the Path Length row must read ONE offset source. The byte it
  // prints comes from `off` (derived from pkt.route_type); the width label used
  // to come from senderPathHashSize(buf), which independently re-derives the
  // offset from the raw_hex header byte. A transport route whose stored
  // route_type disagrees with its on-wire header route bits splits the two: they
  // point at different bytes. The fix reads the width from the byte it prints,
  // so the value and its hash_size label always describe the same byte 5.
  test('buildFieldTable reads the transport path-length width from the byte it prints (#282 one offset source)', () => {
    // route_type 0 = TRANSPORT_FLOOD → path length at byte 5. Header byte 0x15:
    // route bits 1 (FLOOD → byte-1 offset), path-type 5 (hops). Byte 1 = 0x40
    // (width bits 1 → 2 bytes), byte 5 = 0x80 (width bits 2 → 3 bytes). The row
    // prints byte 5 (0x80), so its label must say hash_size=3 — not 2, which is
    // byte 1's width reached via the second (header-derived) offset.
    const pkt = { raw_hex: '1540aabbcc80', route_type: 0, payload_type: 5 };
    const result = api.buildFieldTable(pkt, {}, [], []);
    const m = /<td>Path Length<\/td><td class="mono">([^<]*)<\/td><td class="text-muted">([^<]*)<\/td>/.exec(result);
    assert(m, 'has a Path Length row, got: ' + result);
    assert.strictEqual(m[1], '0x80', 'prints the byte at the route_type offset (byte 5), got: ' + m[1]);
    assert(/hash_size=3 bytes?\b/.test(m[2]),
      'width must be read from the printed byte (0x80 → 3 bytes), got: ' + m[2]);
    assert(!/hash_size=2\b/.test(m[2]),
      'must not read byte 1 (0x40 → 2 bytes) via a second offset source, got: ' + m[2]);
  });
}

console.log('\n=== packets.js: _getRowCount ===');
{
  const ctx = loadPacketsSandbox();
  const api = ctx._packetsTestAPI;

  test('_getRowCount returns 1 for ungrouped', () => {
    // _displayGrouped is internal, but when not grouped, should return 1
    // Since we can't easily control _displayGrouped, test the function behavior
    const result = api._getRowCount({ hash: 'abc', _children: [{ observer_id: '1' }] });
    // Default _displayGrouped depends on initialization, but the function should not throw
    assert(typeof result === 'number');
    assert(result >= 1);
  });
}

console.log('\n=== packets.js: buildFlatRowHtml ===');
{
  const ctx = loadPacketsSandbox();
  const api = ctx._packetsTestAPI;

  test('buildFlatRowHtml produces table row', () => {
    const p = {
      id: 1, hash: 'abc123', timestamp: '2024-01-01T00:00:00Z',
      observer_id: null, raw_hex: 'aabb', payload_type: 4,
      route_type: 1, decoded_json: '{}', path_json: '[]'
    };
    const result = api.buildFlatRowHtml(p);
    assert(result.includes('<tr'));
    assert(result.includes('data-id="1"'));
    assert(result.includes('data-hash="abc123"'));
  });

  test('buildFlatRowHtml calculates size from hex', () => {
    const p = {
      id: 2, hash: 'x', timestamp: '', observer_id: null,
      raw_hex: 'aabbccdd', payload_type: 0, route_type: 0,
      decoded_json: '{}', path_json: '[]'
    };
    const result = api.buildFlatRowHtml(p);
    assert(result.includes('4B'));  // 8 hex chars = 4 bytes
  });

  test('buildFlatRowHtml handles missing raw_hex', () => {
    const p = {
      id: 3, hash: 'y', timestamp: '', observer_id: null,
      raw_hex: null, payload_type: 0, route_type: 0,
      decoded_json: '{}', path_json: '[]'
    };
    const result = api.buildFlatRowHtml(p);
    assert(result.includes('0B'));
  });

  test('buildFlatRowHtml emits data-entry-idx when provided', () => {
    const p = {
      id: 4, hash: 'z', timestamp: '', observer_id: null,
      raw_hex: 'aabb', payload_type: 0, route_type: 0,
      decoded_json: '{}', path_json: '[]'
    };
    const result = api.buildFlatRowHtml(p, 42);
    assert(result.includes('data-entry-idx="42"'));
  });

  test('buildFlatRowHtml emits data-entry-idx=-1 by default', () => {
    const p = {
      id: 5, hash: 'w', timestamp: '', observer_id: null,
      raw_hex: 'aabb', payload_type: 0, route_type: 0,
      decoded_json: '{}', path_json: '[]'
    };
    const result = api.buildFlatRowHtml(p);
    assert(result.includes('data-entry-idx="-1"'));
  });
}

// #258: makeColumnsResizable() (app.js) measures inside TableResponsive.unhidden,
// so a re-measure sees the columns as the first measure did, before register().
console.log('\n=== packets.js: TableResponsive.unhidden (#258) ===');
{
  const ctx = loadPacketsSandbox();
  const TR = ctx.window.TableResponsive;
  const el = (classes) => {
    const set = new Set(classes);
    return { style: { display: '' }, classList: { add: (c) => set.add(c), remove: (c) => set.delete(c), contains: (c) => set.has(c) } };
  };
  const makeTable = () => {
    const els = [el(['col-observer', 'col-hidden']), el(['col-observer', 'col-hidden']), el(['col-time']), el(['col-hidden-pill']), el(['col-hidden-pill', 'col-rehide-pill'])];
    els[3].style.display = 'inline-block';
    return {
      els,
      querySelectorAll: (sel) => els.filter((e) => e.classList.contains(sel.replace(/^\./, ''))),
    };
  };
  const state = (t) => t.els.map((e) => ['col-hidden', 'col-hidden-pill'].filter((c) => e.classList.contains(c)).join('+') + ':' + e.style.display);

  test('#258: unhidden lifts col-hidden and hides the pills only while fn runs', () => {
    assert.strictEqual(typeof TR.unhidden, 'function', 'TableResponsive.unhidden is exported');
    const t = makeTable();
    const before = state(t);
    let during = null;
    const out = TR.unhidden(t, () => { during = state(t); return 42; });
    assert.strictEqual(out, 42, 'returns what fn returns');
    assert.deepStrictEqual(during, [':', ':', ':', 'col-hidden-pill:none', 'col-hidden-pill:none'], 'during: ' + JSON.stringify(during));
    assert.deepStrictEqual(state(t), before, 'restored: ' + JSON.stringify(state(t)));
  });

  test('#258: unhidden restores the hiding when fn throws', () => {
    const t = makeTable();
    const before = state(t);
    assert.throws(() => TR.unhidden(t, () => { throw new Error('boom'); }), /boom/);
    assert.deepStrictEqual(state(t), before);
  });
}

console.log('\n=== packets.js: buildGroupRowHtml ===');
{
  const ctx = loadPacketsSandbox();
  const api = ctx._packetsTestAPI;

  test('buildGroupRowHtml renders single-count group', () => {
    const p = {
      hash: 'abc', count: 1, latest: '2024-01-01T00:00:00Z',
      observer_id: null, raw_hex: 'aabb', payload_type: 4,
      route_type: 1, decoded_json: '{}', path_json: '[]',
      observation_count: 1, observer_count: 1
    };
    const result = api.buildGroupRowHtml(p);
    assert(result.includes('<tr'));
    assert(result.includes('data-hash="abc"'));
    // Single count: no expand arrow, no group-header class
    assert(!result.includes('group-header'));
  });

  const collapsedGroup = {
    hash: 'xyz', count: 3, latest: '2024-01-01T00:00:00Z',
    observer_id: null, raw_hex: 'aabbcc', payload_type: 0,
    route_type: 0, decoded_json: '{}', path_json: '[]',
    observation_count: 3, observer_count: 2
  };

  test('buildGroupRowHtml renders multi-count group with expand arrow', () => {
    const result = api.buildGroupRowHtml(collapsedGroup);
    assert(result.includes('group-header'));
    // The expand cell of a collapsed group holds a caret, and it is not the
    // expanded one.
    const cell = /<td class="col-expand"[^>]*>([\s\S]*?)<\/td>/.exec(result);
    assert(cell && /#ph-caret-/.test(cell[1]), 'collapsed group has a caret in its expand cell');
    assert(!result.includes(phIcon('caret-down')), 'collapsed group does not show the expanded caret');
  });

  // #189: before 30627454 (#1648 M2, emoji to Phosphor sprites) a collapsed
  // group showed ▶ and an expanded one ▼. The migration mapped ▶ to
  // #ph-caret-up, so a collapsed group pointed up. The disclosure convention in
  // the front end is caret-right when collapsed and caret-down when expanded
  // (channels.js, network-digest.js, analytics.js #ptOverviewChevron,
  // route-view.js paths chevron).
  test('buildGroupRowHtml shows a right-pointing caret on a collapsed group', () => {
    const cell = expandCell(api.buildGroupRowHtml(collapsedGroup));
    assert(cell.includes(phIcon('caret-right')), 'collapsed group shows a right caret');
    assert(!cell.includes(phIcon('caret-up')), 'collapsed group does not point up');
    assert(!cell.includes(phIcon('caret-down')), 'collapsed group does not show the expanded caret');
  });

  test('buildGroupRowHtml shows a down-pointing caret on an expanded group', () => {
    api._setExpanded(collapsedGroup.hash, true);
    try {
      const cell = expandCell(api.buildGroupRowHtml(collapsedGroup));
      assert(cell.includes(phIcon('caret-down')), 'expanded group shows a down caret');
      assert(!cell.includes(phIcon('caret-right')), 'expanded group does not show the collapsed caret');
      assert(!cell.includes(phIcon('caret-up')), 'expanded group does not point up');
    } finally { api._setExpanded(collapsedGroup.hash, false); }
  });

  test('buildGroupRowHtml: the group toggle row reports its state in aria-expanded', () => {
    const header = (html) => /<tr class="group-header[^>]*>/.exec(html)[0];
    assert(header(api.buildGroupRowHtml(collapsedGroup)).includes('aria-expanded="false"'), 'collapsed: aria-expanded=false');
    api._setExpanded(collapsedGroup.hash, true);
    try {
      assert(header(api.buildGroupRowHtml(collapsedGroup)).includes('aria-expanded="true"'), 'expanded: aria-expanded=true');
    } finally { api._setExpanded(collapsedGroup.hash, false); }
  });

  test('buildGroupRowHtml: a single-observation row has no caret and no aria-expanded', () => {
    const single = Object.assign({}, collapsedGroup, { hash: 'single1', count: 1 });
    const html = api.buildGroupRowHtml(single);
    assert(!html.includes('aria-expanded'), 'a row that cannot expand does not claim a state');
    assert(!/#ph-caret-/.test(expandCell(html)), 'no caret in the expand cell');
  });
}

// #254: under the mobile breakpoint mobile-page-actions.js (#1461 #7) hides the
// expand column and turns a click on a group row into select-hash, so the row
// selects instead of expanding. There the row must not announce aria-expanded,
// and its action is select-hash for every activation (tap, Enter, Space).
// Above the breakpoint the row stays the #189 toggle.
console.log('\n=== packets.js: group row action and aria-expanded by viewport (#254) ===');
{
  const ctx = loadPacketsSandbox();
  loadInCtx(ctx, 'public/mobile-page-actions.js');
  const api = ctx._packetsTestAPI;
  const group = {
    hash: 'mob254', count: 3, latest: '2024-01-01T00:00:00Z',
    observer_id: null, raw_hex: 'aabbcc', payload_type: 0,
    route_type: 0, decoded_json: '{}', path_json: '[]',
    observation_count: 3, observer_count: 2
  };
  const header = (html) => /<tr [^>]*>/.exec(html)[0];
  const atWidth = (w, fn) => {
    const prev = ctx.window.innerWidth;
    ctx.window.innerWidth = w;
    try { return fn(); } finally { ctx.window.innerWidth = prev; }
  };

  test('#254: at 390 px a group row selects and carries no aria-expanded', () => atWidth(390, () => {
    const tr = header(api.buildGroupRowHtml(group));
    assert(tr.includes('data-action="select-hash"'), 'mobile group row selects: ' + tr);
    assert(!tr.includes('aria-expanded'), 'a row that selects does not announce an expanded state: ' + tr);
  }));

  test('#254: at 390 px an expanded group row still carries no aria-expanded', () => atWidth(390, () => {
    api._setExpanded(group.hash, true);
    try {
      const tr = header(api.buildGroupRowHtml(group));
      assert(tr.includes('data-action="select-hash"'), 'mobile group row selects');
      assert(!tr.includes('aria-expanded'), 'no aria-expanded on mobile, expanded or not');
    } finally { api._setExpanded(group.hash, false); }
  }));

  test('#254: at the 600 px breakpoint the row is still the mobile one', () => atWidth(600, () => {
    const tr = header(api.buildGroupRowHtml(group));
    assert(tr.includes('data-action="select-hash"') && !tr.includes('aria-expanded'), tr);
  }));

  test('#254: at 1400 px the group row is the #189 toggle with aria-expanded', () => atWidth(1400, () => {
    const tr = header(api.buildGroupRowHtml(group));
    assert(tr.includes('data-action="toggle-select"'), 'desktop group row toggles: ' + tr);
    assert(tr.includes('aria-expanded="false"'), 'desktop collapsed row: aria-expanded=false');
    api._setExpanded(group.hash, true);
    try {
      assert(header(api.buildGroupRowHtml(group)).includes('aria-expanded="true"'), 'desktop expanded row: aria-expanded=true');
    } finally { api._setExpanded(group.hash, false); }
  }));

  test('#254: at 601 px the group row toggles', () => atWidth(601, () => {
    const tr = header(api.buildGroupRowHtml(group));
    assert(tr.includes('data-action="toggle-select"') && tr.includes('aria-expanded="false"'), tr);
  }));

  test('#254: a single-observation row is select-hash without aria-expanded at both widths', () => {
    const single = Object.assign({}, group, { hash: 'single254', count: 1 });
    for (const w of [390, 1400]) atWidth(w, () => {
      const tr = header(api.buildGroupRowHtml(single));
      assert(tr.includes('data-action="select-hash"') && !tr.includes('aria-expanded'), w + ' px: ' + tr);
    });
  });
}

// #259 (1): a group expanded on desktop must not leave visible child rows behind
// when the layout flips to the mobile mode, where the expand column is hidden
// and the row only selects — there would be no way to collapse it again. The
// hash stays in expandedHashes, so the children come back on desktop; the
// rendered row is the collapsed one while the mobile mode is active.
console.log('\n=== packets.js: an expanded group across the mobile breakpoint (#259) ===');
{
  const ctx = loadPacketsSandbox();
  loadInCtx(ctx, 'public/mobile-page-actions.js');
  const api = ctx._packetsTestAPI;
  const mkChild = (id, obs) => ({
    id, observer_id: obs, hash: 'grp259', raw_hex: 'aabbcc', payload_type: 0,
    route_type: 0, decoded_json: '{}', path_json: '[]', timestamp: '2024-01-01T00:00:00Z'
  });
  const group = {
    hash: 'grp259', count: 3, latest: '2024-01-01T00:00:00Z',
    observer_id: null, raw_hex: 'aabbcc', payload_type: 0,
    route_type: 0, decoded_json: '{}', path_json: '[]',
    observation_count: 3, observer_count: 3,
    _children: [mkChild(1, '1'), mkChild(2, '2'), mkChild(3, '3')]
  };
  const header = (html) => /<tr [^>]*>/.exec(html)[0];
  const rowClass = (html) => (/<tr class="([^"]*)"/.exec(header(html)) || [, ''])[1];
  const childRows = (html) => (html.match(/<tr class="group-child"/g) || []).length;
  const atWidth = (w, fn) => {
    const prev = ctx.window.innerWidth;
    ctx.window.innerWidth = w;
    try { return fn(); } finally { ctx.window.innerWidth = prev; }
  };

  // _getRowCount only counts children in grouped mode; the hook lets the
  // sandbox say so. Guarded so this file still runs against a tree without it.
  if (typeof api._setDisplayGrouped === 'function') api._setDisplayGrouped(true);
  api._setExpanded(group.hash, true);

  test('#259: at 1400 px the expanded group renders its children (#248 unchanged)', () => atWidth(1400, () => {
    const html = api.buildGroupRowHtml(group);
    assert.strictEqual(childRows(html), 3, 'three child rows: ' + childRows(html));
    assert(/\bexpanded\b/.test(rowClass(html)), 'the row is marked expanded: ' + rowClass(html));
    assert(header(html).includes('aria-expanded="true"'), header(html));
    assert(expandCell(html).includes(phIcon('caret-down')), 'down caret while expanded');
  }));

  test('#259: at 390 px the same expanded group renders no child rows', () => atWidth(390, () => {
    const html = api.buildGroupRowHtml(group);
    assert.strictEqual(childRows(html), 0, 'no visible children on mobile, got ' + childRows(html));
  }));

  test('#259: at 390 px the row does not claim the expanded class or caret', () => atWidth(390, () => {
    const html = api.buildGroupRowHtml(group);
    assert(!/\bexpanded\b/.test(rowClass(html)), 'no expanded class on mobile: ' + rowClass(html));
    assert(header(html).includes('data-action="select-hash"'), header(html));
    assert(!header(html).includes('aria-expanded'), 'still no aria-expanded on mobile');
    assert(!expandCell(html).includes(phIcon('caret-down')), 'no down caret on mobile');
  }));

  test('#259: _getRowCount matches the rendered rows on both sides of the breakpoint', () => {
    atWidth(390, () => {
      assert.strictEqual(api._getRowCount(group), 1, 'mobile: the group is one row');
    });
    atWidth(1400, () => {
      assert.strictEqual(api._getRowCount(group), 4, 'desktop: the group plus three children');
    });
  });

  test('#259: 600 px hides the children and 601 px shows them again', () => {
    atWidth(600, () => {
      assert.strictEqual(childRows(api.buildGroupRowHtml(group)), 0, 'at the breakpoint the children are hidden');
    });
    atWidth(601, () => {
      assert.strictEqual(childRows(api.buildGroupRowHtml(group)), 3, 'just above it they are back');
    });
  });

  test('#259: the expansion survives 1400 -> 390 -> 1400, it is not cleared', () => {
    atWidth(390, () => { api.buildGroupRowHtml(group); });
    atWidth(1400, () => {
      const html = api.buildGroupRowHtml(group);
      assert.strictEqual(childRows(html), 3, 'the children are back on desktop: ' + childRows(html));
      assert(header(html).includes('aria-expanded="true"'), 'and the state is still expanded');
    });
  });

  test('#259: a collapsed group is unaffected at either width', () => {
    api._setExpanded(group.hash, false);
    try {
      for (const w of [390, 1400]) atWidth(w, () => {
        const html = api.buildGroupRowHtml(group);
        assert.strictEqual(childRows(html), 0, w + ' px: a collapsed group has no children');
        assert(!/\bexpanded\b/.test(rowClass(html)), w + ' px: ' + rowClass(html));
      });
    } finally { api._setExpanded(group.hash, true); }
  });
}

// Without mobile-page-actions.js there is no #1461 #7 redirect, so a group row
// toggles at any width.
console.log('\n=== packets.js: group row without mobile-page-actions.js (#254) ===');
{
  const ctx = loadPacketsSandbox();
  const api = ctx._packetsTestAPI;
  ctx.window.innerWidth = 390;
  test('#254: no redirect module loaded, so the row toggles even at 390 px', () => {
    const tr = /<tr [^>]*>/.exec(api.buildGroupRowHtml({
      hash: 'nompa', count: 2, latest: '2024-01-01T00:00:00Z', observer_id: null, raw_hex: 'aabb',
      payload_type: 0, route_type: 0, decoded_json: '{}', path_json: '[]', observation_count: 2, observer_count: 1
    }))[0];
    assert(tr.includes('data-action="toggle-select"') && tr.includes('aria-expanded="false"'), tr);
  });
}

{
  const ctx = loadPacketsSandbox();
  const api = ctx._packetsTestAPI;

  test('buildGroupRowHtml shows observation count badge', () => {
    const p = {
      hash: 'obs', count: 1, latest: '2024-01-01T00:00:00Z',
      observer_id: null, raw_hex: 'aa', payload_type: 0,
      route_type: 0, decoded_json: '{}', path_json: '[]',
      observation_count: 5, observer_count: 1
    };
    const result = api.buildGroupRowHtml(p);
    assert(result.includes('badge-obs'));
    assert(result.includes(phIcon('eye')));
    assert(result.includes('5'));
  });

  test('buildGroupRowHtml emits data-entry-idx on header row', () => {
    const p = {
      hash: 'ei1', count: 1, latest: '2024-01-01T00:00:00Z',
      observer_id: null, raw_hex: 'aa', payload_type: 0,
      route_type: 0, decoded_json: '{}', path_json: '[]',
      observation_count: 1, observer_count: 1
    };
    const result = api.buildGroupRowHtml(p, 7);
    assert(result.includes('data-entry-idx="7"'));
  });

  test('buildGroupRowHtml emits data-entry-idx on child rows', () => {
    const ctx2 = loadPacketsSandbox();
    const api2 = ctx2._packetsTestAPI;
    // Simulate expandedHashes having this hash
    // We can't easily toggle expandedHashes from outside, so test via the
    // fact that children only render when isExpanded is true.
    // For this test, just verify the header row has the attribute (child rows
    // are conditional on expandedHashes which we can't set from tests).
    const p = {
      hash: 'ei2', count: 3, latest: '2024-01-01T00:00:00Z',
      observer_id: null, raw_hex: 'aabb', payload_type: 0,
      route_type: 0, decoded_json: '{}', path_json: '[]',
      observation_count: 3, observer_count: 2,
      _children: []
    };
    const result = api2.buildGroupRowHtml(p, 15);
    assert(result.includes('data-entry-idx="15"'));
  });
}

console.log('\n=== packets.js: page registration ===');
{
  const ctx = loadPacketsSandbox();
  // registerPage is defined in app.js and stores in its own `pages` closure.
  // We verify via the navigateTo mechanism or by checking the pages object isn't empty.
  // Since we can't easily access the closure, just verify the test API is exposed.
  test('_packetsTestAPI is exposed on window', () => {
    assert(ctx._packetsTestAPI);
    assert(typeof ctx._packetsTestAPI.typeName === 'function');
    assert(typeof ctx._packetsTestAPI.getDetailPreview === 'function');
    assert(typeof ctx._packetsTestAPI.sortGroupChildren === 'function');
    assert(typeof ctx._packetsTestAPI.buildFieldTable === 'function');
  });
}

console.log('\n=== packets.js: reconcileVisibleCols (column-prefs backfill) ===');
{
  const ctx = loadPacketsSandbox();
  const api = ctx._packetsTestAPI;
  const COL_DEFS = [
    { key: 'region' }, { key: 'time' }, { key: 'hash' }, { key: 'size' },
    { key: 'type' }, { key: 'scope' }, { key: 'observer' }, { key: 'path' },
    { key: 'rpt' }, { key: 'details' },
  ];
  const defaultHidden = ['region'];

  test('reconcileVisibleCols is exported', () => {
    assert(typeof api.reconcileVisibleCols === 'function');
  });

  test('no saved prefs: falls back to all columns minus defaultHidden', () => {
    const result = api.reconcileVisibleCols(null, null, COL_DEFS, defaultHidden);
    assert.deepStrictEqual(result, COL_DEFS.map(c => c.key).filter(k => k !== 'region'));
  });

  test('legacy saved prefs (no known-cols baseline yet) get the scope column backfilled', () => {
    // Reproduces a real saved localStorage value from before the Scope
    // column (#1852) existed, and before packets-visible-cols-known was
    // ever persisted — must not stay permanently hidden.
    const legacy = ['time', 'hash', 'size', 'type', 'observer', 'path', 'rpt', 'details'];
    const result = api.reconcileVisibleCols(legacy, null, COL_DEFS, defaultHidden);
    assert.ok(result.includes('scope'), 'scope column should be backfilled into a legacy saved array');
  });

  test('a column the user explicitly hid (present in known, absent from saved) stays hidden', () => {
    const known = ['region', 'time', 'hash', 'size', 'type', 'scope', 'observer', 'path', 'rpt', 'details'];
    const saved = ['time', 'hash', 'size', 'type', 'scope', 'path', 'rpt', 'details']; // observer unchecked by user
    const result = api.reconcileVisibleCols(saved, known, COL_DEFS, defaultHidden);
    assert.ok(!result.includes('observer'), 'user-hidden column must not be resurrected');
  });

  test('a genuinely new column (absent from known) gets backfilled even with a known-cols baseline', () => {
    const known = ['region', 'time', 'hash', 'size', 'type', 'observer', 'path', 'rpt', 'details']; // no scope yet
    const saved = ['time', 'hash', 'size', 'type', 'observer', 'path', 'rpt', 'details'];
    const result = api.reconcileVisibleCols(saved, known, COL_DEFS, defaultHidden);
    assert.ok(result.includes('scope'), 'a column absent from the known baseline should be backfilled');
  });

  test('a column in defaultHidden is not force-added even if missing from saved and known', () => {
    const legacy = ['time', 'hash', 'type', 'observer', 'path', 'rpt', 'details']; // no size, no scope
    const narrowDefaultHidden = ['region', 'size', 'scope'];
    const result = api.reconcileVisibleCols(legacy, null, COL_DEFS, narrowDefaultHidden);
    assert.ok(!result.includes('scope'), 'columns in defaultHidden should not be backfilled');
    assert.ok(!result.includes('size'), 'columns in defaultHidden should not be backfilled');
  });
}

console.log('\n=== packets.js: _invalidateRowCounts / _refreshRowCountsIfDirty (#410) ===');
{
  const ctx = loadPacketsSandbox();
  const api = ctx._packetsTestAPI;

  test('_invalidateRowCounts and _refreshRowCountsIfDirty are exported', () => {
    assert(typeof api._invalidateRowCounts === 'function');
    assert(typeof api._refreshRowCountsIfDirty === 'function');
  });

  test('_invalidateRowCounts does not throw', () => {
    api._invalidateRowCounts();
  });

  test('_refreshRowCountsIfDirty does not throw when no display packets', () => {
    api._invalidateRowCounts();
    api._refreshRowCountsIfDirty();
  });

  test('_cumulativeRowOffsets returns valid offsets after invalidation cycle', () => {
    // Even with no display packets, should return valid array
    const offsets = api._cumulativeRowOffsets();
    assert(Array.isArray(offsets));
    assert(offsets[0] === 0);
  });
}

console.log('\n=== packets.js: buildPacketsParams ===');
{
  const ctx = loadPacketsSandbox();
  const api = ctx._packetsTestAPI;
  assert(typeof api.buildPacketsParams === 'function', 'buildPacketsParams must be exported');

  test('hash filter suppresses region — direct hash links work regardless of saved region', () => {
    // This is the bug from URL https://analyzer.../#/packets?hash=178525e9f693aa7e
    // when the user's saved RegionFilter excludes the packet's observer region.
    // The hash is an exact identifier; ALL other filters must be ignored.
    const p = api.buildPacketsParams({
      filters: { hash: 'abc123' },
      regionParam: 'SJC,SFO,OAK,MRY',
      windowMin: 60,
      groupByHash: false,
      limit: 200,
    });
    assert.strictEqual(p.get('hash'), 'abc123');
    assert.strictEqual(p.get('region'), null, 'region must NOT be set when hash is present');
    assert.strictEqual(p.get('since'), null, 'since must NOT be set when hash is present');
  });

  test('hash filter suppresses ALL other filters — observer, node, channel too', () => {
    const p = api.buildPacketsParams({
      filters: { hash: 'h', node: 'n', observer: 'o', channel: 'c' },
      regionParam: 'SJC',
      windowMin: 60,
      groupByHash: false,
      limit: 200,
    });
    assert.strictEqual(p.get('hash'), 'h');
    assert.strictEqual(p.get('node'), null);
    assert.strictEqual(p.get('observer'), null);
    assert.strictEqual(p.get('channel'), null);
    assert.strictEqual(p.get('region'), null);
    assert.strictEqual(p.get('since'), null);
  });

  test('hash filter suppresses region with default windowMin=0', () => {
    const p = api.buildPacketsParams({
      filters: { hash: 'deadbeef' },
      regionParam: 'COA',
      windowMin: 0,
      groupByHash: false,
      limit: 50,
    });
    assert.strictEqual(p.get('hash'), 'deadbeef');
    assert.strictEqual(p.get('region'), null);
  });

  test('region applied normally when hash filter is absent', () => {
    const p = api.buildPacketsParams({
      filters: {},
      regionParam: 'SJC,SFO',
      windowMin: 60,
      groupByHash: false,
      limit: 200,
    });
    assert.strictEqual(p.get('region'), 'SJC,SFO', 'region must apply when no hash');
    assert.strictEqual(p.get('hash'), null);
    assert(p.get('since'), 'since must apply when no hash and windowMin>0');
  });

  test('observer/node/channel pass through normally when no hash', () => {
    const p = api.buildPacketsParams({
      filters: { observer: 'obs1', node: 'node1', channel: '#test' },
      regionParam: '',
      windowMin: 0,
      groupByHash: false,
      limit: 50,
    });
    assert.strictEqual(p.get('observer'), 'obs1');
    assert.strictEqual(p.get('node'), 'node1');
    assert.strictEqual(p.get('channel'), '#test');
  });

  test('region absent when regionParam empty — no spurious empty region= param', () => {
    const p = api.buildPacketsParams({
      filters: {},
      regionParam: '',
      windowMin: 0,
      groupByHash: false,
      limit: 50,
    });
    assert.strictEqual(p.get('region'), null);
  });

  test('groupByHash=true with hash sets groupByHash and omits expand', () => {
    const p = api.buildPacketsParams({
      filters: { hash: 'h' }, regionParam: '', windowMin: 0, groupByHash: true, limit: 50,
    });
    assert.strictEqual(p.get('groupByHash'), 'true');
    assert.strictEqual(p.get('expand'), null);
    assert.strictEqual(p.get('hash'), 'h');
  });

  test('groupByHash=false with hash sets expand=observations', () => {
    const p = api.buildPacketsParams({
      filters: { hash: 'h' }, regionParam: '', windowMin: 0, groupByHash: false, limit: 50,
    });
    assert.strictEqual(p.get('expand'), 'observations');
    assert.strictEqual(p.get('groupByHash'), null);
    assert.strictEqual(p.get('hash'), 'h');
  });

  test('groupByHash=false without hash sets expand=observations', () => {
    const p = api.buildPacketsParams({
      filters: {}, regionParam: '', windowMin: 0, groupByHash: false, limit: 50,
    });
    assert.strictEqual(p.get('expand'), 'observations');
    assert.strictEqual(p.get('groupByHash'), null);
  });
}

console.log('\n=== packets.js: scroll position preserved across renderTableRows (#431) ===');
{
  // Build a richer sandbox with DOM elements that renderTableRows needs
  const ctx = makeSandbox();
  // Mock DOM elements needed by renderTableRows and renderVisibleRows
  let pktLeftScrollTop = 500;
  const pktBody = {
    tagName: 'TBODY', id: 'pktBody', _innerHTML: '', children: [],
    get innerHTML() { return this._innerHTML; },
    set innerHTML(v) { this._innerHTML = v; pktLeftScrollTop = 0; }, // Simulate browser scroll reset on DOM rebuild
    appendChild: () => {}, insertBefore: () => {}, removeChild: () => {},
    querySelectorAll: () => [], querySelector: () => null,
    style: {},
  };
  const pktLeft = {
    tagName: 'DIV', id: 'pktLeft', className: '',
    get scrollTop() { return pktLeftScrollTop; },
    set scrollTop(v) { pktLeftScrollTop = v; },
    clientHeight: 800,
    offsetHeight: 800,
    querySelector: (sel) => {
      if (sel === 'thead') return { offsetHeight: 40 };
      if (sel === '.count' || sel === '#pktLeft .count') return { textContent: '' };
      return null;
    },
    querySelectorAll: () => [],
    addEventListener: () => {},
    removeEventListener: () => {},
    style: {},
  };
  const origGetById = ctx.document.getElementById;
  ctx.document.getElementById = (id) => {
    if (id === 'pktBody') return pktBody;
    if (id === 'pktLeft') return pktLeft;
    if (id === 'fGroup') return { classList: { toggle: () => {}, add: () => {}, remove: () => {}, contains: () => false } };
    if (id === 'packetFilterCount') return { style: {}, textContent: '' };
    if (id === 'vscroll-top') return null;
    if (id === 'vscroll-bottom') return null;
    return null;
  };
  ctx.document.querySelector = (sel) => {
    if (sel === '#pktLeft .count') return { textContent: '', set textContent(v) {} };
    if (sel === '#pktLeft') return pktLeft;
    return null;
  };

  loadInCtx(ctx, 'public/payload-labels.js');
  loadInCtx(ctx, 'public/roles.js');
  loadInCtx(ctx, 'public/app.js');
  loadInCtx(ctx, 'public/packet-helpers.js');
  vm.runInContext(`
    window.HopDisplay = {
      renderHop: function(h, entry, opts) { return '<span>' + h + '</span>'; },
      _showFromBtn: function() {}
    };
  `, ctx);
  loadInCtx(ctx, 'public/packets.js');

  const api = ctx._packetsTestAPI;

  test('scroll position preserved after renderTableRows (#431)', () => {
    // Inject packets that will ALL be filtered out by type filter,
    // triggering the empty-state path which sets tbody.innerHTML (resetting scroll in browser)
    api._setPackets([
      { id: 1, hash: 'aaa', payload_type: 4, timestamp: '2024-01-01T00:00:00Z', observer_id: 'obs1', path_len: 2, decoded_json: '{}' },
      { id: 2, hash: 'bbb', payload_type: 4, timestamp: '2024-01-01T00:01:00Z', observer_id: 'obs1', path_len: 1, decoded_json: '{}' },
    ]);

    // Set scroll position to 500
    pktLeftScrollTop = 500;

    // Filter by type 99 (no packets match) — this triggers tbody.innerHTML assignment
    api._setFilter('type', '99');
    try { api.renderTableRows(); } catch(e) { /* swallow DOM stub errors */ }

    // scrollTop must be preserved (not reset to 0)
    assert.strictEqual(pktLeftScrollTop, 500, 'scrollTop should be preserved after renderTableRows, got ' + pktLeftScrollTop);
  });
}

// ===== packets.js: detail Hash Size source (PR #212 review) =====
console.log('\n=== packets.js: detail Hash Size reads the selected observation ===');
{
  const src = fs.readFileSync('public/packets.js', 'utf8');

  // Behavioural coverage lives in
  // test-packet-detail-sender-hash-size-obs-e2e.js, which only runs in the
  // Playwright job. This is the fast guard: observations of one transmission
  // carry their own frames, so the "Hash Size" summary and the byte table
  // below it must read the SAME raw_hex, or the panel contradicts itself.
  test('renderDetail derives Hash Size from the selected observation, not the original frame', () => {
    assert.ok(src.includes('const hashSize = senderPathHashSize(effectivePkt.raw_hex || pkt.raw_hex);'),
      'the Hash Size summary must use the effective observation\'s frame');
    assert.ok(!/const hashSize = senderPathHashSize\(pkt\.raw_hex\)/.test(src),
      'reading the original transmission reintroduces the observation mismatch');
  });

  test('the byte table is built from the same frame the summary reads', () => {
    assert.ok(src.includes('buildFieldTable(effectivePkt.raw_hex ? effectivePkt : pkt,'),
      'buildFieldTable must receive the effective observation');
  });
}

// ===== packets.js: detail Hash Size warns on 1-byte (#353) =====
console.log('\n=== packets.js: detail Hash Size row warns on 1-byte (#353) ===');
{
  const api = loadPacketsSandbox()._packetsTestAPI;
  const src = fs.readFileSync('public/packets.js', 'utf8');

  test('1-byte Hash Size row gets the warn class, the warning icon and the warn tooltip', () => {
    const html = api.hashSizeDetailHtml(1);
    assert.ok(html.startsWith('<dt>Hash Size</dt><dd>'), html);
    assert.match(html, /class="detail-hash-size detail-hash-size--warn path-hash-warn"/);
    assert.match(html, /#ph-warning/);
    assert.match(html, /<span class="sr-only">Warning: <\/span>1 byte<\/span><\/dd>$/);
    assert.match(html, /title="[^"]*2- or 3-byte/);
  });

  test('2- and 3-byte Hash Size rows stay neutral', () => {
    assert.strictEqual(api.hashSizeDetailHtml(2),
      '<dt>Hash Size</dt><dd><span class="detail-hash-size">2 bytes</span></dd>');
    assert.strictEqual(api.hashSizeDetailHtml(3),
      '<dt>Hash Size</dt><dd><span class="detail-hash-size">3 bytes</span></dd>');
  });

  test('no encoded width renders no Hash Size row, as before', () => {
    for (const v of [null, undefined, 0]) assert.strictEqual(api.hashSizeDetailHtml(v), '', 'for ' + v);
  });

  // PR #356 review F2: a width outside 1-3 must not leave an empty
  // <dt>Hash Size</dt><dd></dd> row behind.
  test('an out-of-range width renders no Hash Size row, never an empty one', () => {
    for (const v of [4, 5, -1, 255, NaN, 'x']) {
      assert.strictEqual(api.hashSizeDetailHtml(v), '', 'for ' + String(v));
    }
  });

  test('renderDetail emits the Hash Size row through hashSizeDetailHtml', () => {
    assert.ok(src.includes('${hashSizeDetailHtml(hashSize)}'),
      'the detail-meta list must use the shared #353 row helper');
    assert.ok(!src.includes('<dt>Hash Size</dt><dd>${hashSize}'),
      'the old inline Hash Size row must be gone');
  });
}

// ===== packets.js: View Path button (detail panel) =====
console.log('\n=== packets.js: View Path button ===');
{
  const src = fs.readFileSync('public/packets.js', 'utf8');

  test('detail-actions renders a View Path button gated on pkt.hash', () => {
    assert.ok(src.includes('${pkt.hash ? `<button class="detail-map-link" data-view-path="${escapeHtml(pkt.hash)}"'),
      'renderDetail must emit a data-view-path button for any packet with a hash');
  });

  test('View Path button is not gated on pathHops.length, unlike View route on map', () => {
    // The packet-path-map.js modal plots every station that heard the packet,
    // not just the deepest relay chain, so a direct-only (0-hop) packet still
    // has a spread worth visualizing (same rationale as channels.js's bot-reply
    // "View path" link). Pin that the two buttons use different gates.
    const viewPathIdx = src.indexOf('data-view-path="${escapeHtml(pkt.hash)}"');
    const viewRouteIdx = src.indexOf('id="viewRouteBtn"');
    assert.ok(viewPathIdx > -1 && viewRouteIdx > -1, 'both buttons must be present in the template');
    const between = src.slice(Math.max(0, viewPathIdx - 80), viewPathIdx);
    assert.ok(!between.includes('pathHops.length'), 'View Path button must not share the pathHops.length gate');
  });

  test('View Path button opens window.PacketPathMap.open with its own hash, not a full navigation', () => {
    assert.ok(src.includes("if (window.PacketPathMap) window.PacketPathMap.open(viewPathBtn.dataset.viewPath);"),
      'clicking View Path must call PacketPathMap.open in place, matching the ?viewPath=1 deep-link behavior');
  });

  test('View Path button wiring is queried via the shared [data-view-path] selector', () => {
    assert.ok(src.includes("panel.querySelector('[data-view-path]')"),
      'must reuse the same attribute selector pattern as channels.js/ping-scores.js');
  });
}

// Exercise the real route and renderDetail, awaiting completion so rejected
// renders cannot accidentally count as passing synchronous assertions.
async function testChannelDestinations() {
  console.log('\n=== packets.js: channel destination (#20) ===');
  const cases = [
    ['unprefixed channel', { channel: 'test' }, '#test'],
    ['already-prefixed channel', { channel: '#test' }, '#test'],
    ['missing channel', {}, '?'],
    ['empty channel', { channel: '' }, '?'],
    ['channel HTML is escaped', { channel: '<test>&' }, '#&lt;test&gt;&amp;'],
    ['prefixed channel HTML is escaped', { channel: '#<test>&' }, '#&lt;test&gt;&amp;'],
    ['recipient wins over channel and hash', { channel: '#test', recipient: '<recipient>', destHash: '1234567890' }, '&lt;recipient&gt;'],
    ['destination hash wins over channel', { channel: '#test', destHash: '1234567890' }, '12345678'],
  ];
  for (const [name, fields, expected] of cases) {
    try {
      const ctx = loadPacketsSandbox(true);
      const elements = [];
      const createElement = ctx.document.createElement;
      ctx.document.createElement = tag => {
        const element = createElement(tag);
        elements.push(element);
        return element;
      };
      ctx.api = async path => {
        if (path === '/observers') return [];
        if (path === '/packets/channel-fixture') return {
          packet: { id: 1, hash: 'channel-fixture', payload_type: 5,
            route_type: 1, timestamp: '2026-01-01T00:00:00Z', path_json: '[]',
            decoded_json: JSON.stringify({ type: 'GRP_TXT', sender: 'Sender', ...fields }) },
          observations: [],
        };
        throw new Error('Unexpected API request: ' + path);
      };
      const app = createElement('div');
      await ctx._registeredPages['packet-detail'].init(app, 'channel-fixture');
      const detail = elements.find(el => el.innerHTML.includes('class="detail-srcdst"'));
      assert.ok(detail, 'real packet detail must render, got: ' + app.innerHTML);
      const row = detail.innerHTML.match(/<div class="detail-srcdst">(.*?)<\/div>/)[1];
      assert.strictEqual(row, 'Sender <span class="arrow">→</span> ' + expected);
      passed++;
      console.log('  ✅ ' + name);
    } catch (e) {
      failed++;
      console.log('  ❌ ' + name + ': ' + e.message);
    }
  }
}

console.log('\n=== #242 server-side type exclusions ===');
{
  const api = loadPacketsSandbox()._packetsTestAPI;
  const params = (filters, hideControl = false, groupByHash = true) => api.buildPacketsParams({ filters, hideControl, groupByHash, limit: 100 });
  test('default leaves exclusion absent', () => assert.strictEqual(params({}).get('excludeTypes'), null));
  test('Hide CONTROL excludes only 11 in raw and grouped requests', () => {
    for (const grouped of [false, true]) assert.strictEqual(params({}, true, grouped).get('excludeTypes'), '11');
  });
  test('selected types exclude their four-bit complement, union Hide CONTROL', () => {
    assert.strictEqual(params({type: '5,11'}, true).get('excludeTypes'), '0,1,2,3,4,6,7,8,9,10,11,12,13,14,15');
    assert.strictEqual(params({type: '5,11'}).get('excludeTypes'), '0,1,2,3,4,6,7,8,9,10,12,13,14,15');
  });
  test('all sixteen types impose no exclusion', () => assert.strictEqual(params({type: Array.from({length:16}, (_, i) => i).join(',')}).get('excludeTypes'), null));
  test('invalid stored selection stays restrictive rather than broadening', () => assert.strictEqual(params({type:'invalid'}).get('excludeTypes'), Array.from({length:16}, (_, i) => i).join(',')));
  test('duplicate selected types produce canonical exclusions', () => assert.strictEqual(params({type:'0,0,15'}).get('excludeTypes'), '1,2,3,4,5,6,7,8,9,10,11,12,13,14'));
  test('pinned hash bypasses both exclusions', () => assert.strictEqual(params({hash:'ABC', type:'5'}, true).get('excludeTypes'), null));
}

testChannelDestinations().then(() => {
  console.log(`\n${'='.repeat(40)}`);
  console.log(`packets.js tests: ${passed} passed, ${failed} failed, ${knownBugs} known bug(s) still failing`);
  if (failed > 0) process.exit(1);
}).catch(error => { console.error(error); process.exit(1); });
