// #918: the server-side health buckets must credit reporting observers exactly
// like the client (#902). Runs the REAL public/roles.js and my-mesh-observers.js
// against the cases in test-fixtures/observer-credit-parity.json, which
// cmd/server/observer_credit_test.go reads too.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const path = require('node:path');

const fx = JSON.parse(fs.readFileSync(path.join(__dirname, 'test-fixtures', 'observer-credit-parity.json'), 'utf8'));
const NOW = Date.UTC(2026, 10, 18, 9);
const HOUR = 3600000;
const KEY = 'ab'.repeat(32);
const iso = agoSec => new Date(NOW - agoSec * 1000).toISOString();

async function load(thresholds, observerOnlineMinutes) {
  const ctx = {
    console, Date, Map, Set,
    fetch: async () => ({ json: async () => ({}) }),
    document: { readyState: 'complete', createElement: () => ({}), head: { appendChild() {} }, getElementById: () => null, addEventListener() {} },
  };
  ctx.window = ctx;
  for (const f of ['roles.js', 'my-mesh-observers.js']) vm.runInNewContext(fs.readFileSync(path.join(__dirname, 'public', f), 'utf8'), ctx);
  await ctx.MeshConfigReady;
  Object.assign(ctx.HEALTH_THRESHOLDS, {
    infraDegradedMs: thresholds.infraDegradedHours * HOUR, infraSilentMs: thresholds.infraSilentHours * HOUR,
    nodeDegradedMs: thresholds.nodeDegradedHours * HOUR, nodeSilentMs: thresholds.nodeSilentHours * HOUR,
    observerOnlineMs: observerOnlineMinutes * 60000,
  });
  return ctx;
}

(async () => {
  let passed = 0;
  for (const c of fx.cases) {
    const r = await load(fx.thresholds, c.observerOnlineMinutes || fx.thresholds.observerOnlineMinutes);
    const obsId = (c.observerIdUpper ? KEY.toUpperCase() : KEY).slice(0, 64 - (c.observerIdTrim || 0));
    // The client indexes the /api/observers payload. A row the database holds
    // but the endpoint does not serve (observerRoster: soft-deleted or
    // blacklisted) is absent from it; the Go test checks the endpoint really
    // omits it and that both server buckets then give no credit either.
    const served = c.observerAgoSec != null && !c.observerRoster;
    const idx = r.MyMeshObservers.index({ observers: served ? [{ id: obsId, last_seen: iso(c.observerAgoSec) }] : [] });
    const heard = c.heardAgoSec == null ? null : iso(c.heardAgoSec);
    const got = r.getNodeHealth(c.role, heard, r.MyMeshObservers.reportedAt(idx, KEY), NOW).status;
    assert.equal(got, c.status, c.name);
    passed++;
    console.log('PASS ' + c.name);
  }
  console.log(`${passed} client parity cases passed`);
})().catch(e => { console.error(e); process.exit(1); });
