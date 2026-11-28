'use strict';
const assert = require('assert');
const audit = require('./public/scope-audit.js');
assert.strictEqual(audit.windowValue('bad'), '24h');
assert.strictEqual(audit.windowValue('7d'), '7d');
assert.strictEqual(audit.searchRows([{name: 'ALPHA', publicKey: 'abc'}, {name: 'Beta', publicKey: 'def'}], 'alpha').length, 1);
assert.strictEqual(audit.searchRows([{name: null, publicKey: 123}], '123').length, 1);
assert.strictEqual(audit.declarationAge('86400'), '1d');
assert.strictEqual(audit.declarationAge('7200'), '2h');
assert.strictEqual(audit.declarationAge(null), 'Unknown');
assert.strictEqual(audit.stateFromHash('#/analytics?tab=scopes&sub=audit&swin=7d&saq=a%26b').query, 'a&b');
assert.strictEqual(audit.stateFromHash('#/analytics?tab=scopes&sub=audit&swin=no').window, '24h');
console.log('Scope Audit helpers passed');
assert.strictEqual(audit.assessment({forwarded: '4', status: 'consistent', unknownScopeObserved: '2'}), 'Incomplete evidence');
assert.strictEqual(audit.assessment({forwarded: '4', staleDeclaration: true, wildcardContradiction: true}), 'Findings · incomplete evidence');
assert.strictEqual(audit.assessment({forwarded: '0', status: 'consistent'}), 'No forwarding evidence');
// Minimal DOM tracks node identity and treats innerHTML writes as a test failure.
class Element {
  constructor(tag, doc) { this.tagName = tag; this.ownerDocument = doc; this.children = []; this.dataset = {}; this.handlers = {}; this._text = ''; this.isConnected = true; this.classes = new Set(); this.classList = { toggle: (c, on) => { if (on) this.classes.add(c); else this.classes.delete(c); } }; }
  set textContent(v) { this._text = String(v); this.children = []; }
  get textContent() { return this._text + this.children.map(c => c.textContent).join(''); }
  set innerHTML(v) { throw new Error('Unsafe HTML sink'); }
  appendChild(el) { return this.insertBefore(el, null); }
  insertBefore(el, before) { if (el.parentNode) el.remove(); let i = before ? this.children.indexOf(before) : this.children.length; this.children.splice(i, 0, el); el.parentNode = this; return el; }
  replaceChild(el, old) { let i = this.children.indexOf(old); this.children[i] = el; el.parentNode = this; old.parentNode = null; }
  remove() { if (this.parentNode) { let p = this.parentNode; p.children.splice(p.children.indexOf(this), 1); this.parentNode = null; } }
  setAttribute(k, v) { this[k] = v; }
  addEventListener(k, f) { this.handlers[k] = f; }
  querySelectorAll() { let result = []; this.children.forEach(c => { if (c.dataset.win) result.push(c); result.push(...c.querySelectorAll()); }); return result; }
}
function descendants(el) { return [el, ...el.children.flatMap(descendants)]; }
function fixture(apiFn, hash = '#/analytics?tab=scopes&sub=audit', region) {
  const doc = {createElement: tag => new Element(tag, doc)};
  const container = new Element('div', doc); let writes = [];
  const options = {api: apiFn, hash: () => hash, onWindow() {}, onState: (...v) => writes.push(v)};
  if (region) options.region = region;
  const controller = audit.mount(container, options);
  return {container, controller, writes, byId: id => descendants(container).find(e => e.id === id)};
}
const row = {publicKey: 'a'.repeat(64), name: '<img src=x onerror=alert(1)>', configuredScope: '#alpha', declaredScopes: ['#alpha'], declarationAgeSeconds: '7200', observedScopes: [{name: '#alpha', count: '3'}], unscopedObserved: '1', forwarded: '4', status: 'consistent'};
(async () => {
  let calls = [];
  const f = fixture(async (path) => { calls.push(path); return {rows: [row], summary: {total: '1'}}; });
  await f.controller.load('24h');
  assert.strictEqual(calls.length, 1);
  assert(f.container.textContent.includes(row.name), 'node-controlled names are literal text');
  assert(!descendants(f.container).some(e => e.tagName === 'img'), 'XSS cannot create an element');
  f.controller.syncState('#/analytics?tab=scopes&sub=audit&swin=7d&saq=alpha&sap=0');
  assert.strictEqual(f.byId('scope-audit-search').value, 'alpha', 'Current hash search restored on retained DOM');
  assert.strictEqual(calls.length, 1, 'State sync does not fetch');
  const original = f.byId('scope-audit-rows').children[0];
  await f.controller.load('24h');
  assert.strictEqual(f.byId('scope-audit-rows').children[0], original, 'unchanged row survives refresh');
  const input = f.byId('scope-audit-search'); input.value = 'missing'; input.handlers.input();
  await new Promise(r => setTimeout(r, 170));
  assert.strictEqual(calls.length, 2, 'search does not fetch');
  assert.strictEqual(f.byId('scope-audit-rows').children.length, 0);
  assert.strictEqual(f.writes.at(-1)[0], 'missing');
  input.value = 'hidden'; input.handlers.input(); const writeCount = f.writes.length; f.controller.setActive(false);
  await new Promise(r => setTimeout(r, 170));
  assert.strictEqual(f.writes.length, writeCount, 'inactive timer cannot rewrite URL');
  input.value = 'inactive'; input.handlers.input();
  assert.strictEqual(f.writes.length, writeCount, 'inactive events cannot rewrite URL');
  let resolves = [];
  const race = fixture(() => new Promise(r => resolves.push(r)));
  const first = race.controller.load('1h'), last = race.controller.load('7d');
  resolves[1]({rows: [{...row, name: 'Latest'}], summary: {total: 1}}); await last;
  resolves[0]({rows: [{...row, name: 'Obsolete'}], summary: {total: 1}}); await first;
  assert(race.container.textContent.includes('Latest') && !race.container.textContent.includes('Obsolete'));
  const bounded = fixture(async () => ({rows: Array.from({length: 2000}, (_, i) => ({...row, publicKey: String(i)})), summary: {total: 2000}}));
  await bounded.controller.load('24h'); assert.strictEqual(bounded.byId('scope-audit-rows').children.length, audit.pageSize);
  const empty = fixture(async () => ({rows: [], summary: {total: 0}})); await empty.controller.load('1h');
  assert(empty.container.textContent.includes('unknown declaration is not the same as an empty'));
  const error = fixture(async () => { throw new Error('<failure>'); }); await error.controller.load('24h');
  assert(error.container.textContent.includes('Failed to load scope audit: <failure>'));
  const late = fixture(() => new Promise(r => resolves.push(r)));
  const pending = late.controller.load('1h'); late.controller.destroy(); resolves[2]({rows: [row]}); await pending;
  assert.strictEqual(late.byId('scope-audit-rows').children.length, 0);
  console.log('Scope Audit DOM, bounded rendering, search, XSS and lifecycle tests passed');
  await transportTests();
})().catch(err => { console.error(err); process.exitCode = 1; });

// #414 — transport-carrier view.
async function transportTests() {
  const t = audit.stateFromHash('#/analytics?tab=scopes&sub=audit&sav=transport&scope=%23dk&swin=7d');
  assert.strictEqual(t.view, 'transport');
  assert.strictEqual(t.scope, 'dk', 'leading # is dropped');
  assert.strictEqual(audit.stateFromHash('#/analytics?sav=other&scope=DK').view, 'declared');
  assert.strictEqual(audit.stateFromHash('#/analytics?sav=transport&scope=DK').scope, 'DK', 'scope names keep their case (firmware strcmp)');
  assert.strictEqual(audit.transportPath({window: '7d', scope: 'dk'}, 'CPH,ARN'), '/scope-audit/transport?window=7d&scope=dk&region=CPH%2CARN');
  assert.strictEqual(audit.transportPath({window: '24h', scope: ''}, ''), '/scope-audit/transport?window=24h');
  const evidence = ['carrier', 'not-seen-carrying', 'ambiguous', 'incomplete', 'low-evidence', 'no-scope-traffic', 'forwarding'];
  evidence.forEach(e => {
    const label = audit.transportEvidence({evidence: e}, 'dk', 5);
    assert(label.title && !/misconfig|fault|wrong/i.test(label.title + label.detail), 'evidence never claims a fault: ' + e);
  });
  assert(/not a configuration finding/.test(audit.transportEvidence({evidence: 'not-seen-carrying'}, 'dk', 5).detail));
  assert(/Fewer than 5/.test(audit.transportEvidence({evidence: 'low-evidence'}, 'dk', 5).detail));
  assert.strictEqual(audit.searchTransportRows([{name: 'x', observedScopes: [{name: 'alpha'}]}, {name: 'y', declaration: {declaredScopes: ['beta']}}], 'beta').length, 1);

  const never = {publicKey: 'b'.repeat(64), name: '<b>never</b>', role: 'repeater', declaration: null, forwarded: 9, carrier: 0, nonCarrier: 9,
    unscoped: 9, unknownScope: 0, observedScopes: [], ambiguousHops: 0, ambiguousScopeHops: 0, evidence: 'not-seen-carrying'};
  const declared = {publicKey: 'c'.repeat(64), name: 'declared', role: 'repeater', declaration: {configuredScope: '', declaredScopes: [], declaresScope: false},
    forwarded: 6, carrier: 6, nonCarrier: 0, unscoped: 0, observedScopes: [{name: 'dk', count: 6}], evidence: 'carrier'};
  const response = {window: '24h', scope: 'dk', region: ['CPH'], minForwarded: 5, rows: [never, declared],
    summary: {total: 2, carrier: 1, notSeenCarrying: 1, neverQueried: 1, declared: 1},
    population: {transmissions: 15, observations: 20, scopeTransmissions: 6, ambiguousHops: 3, regionObservers: 1},
    coverage: {oldestObservation: '2026-10-25T10:00:00Z', complete: false}, scopes: [{name: 'dk', count: 6}, {name: 'eu', count: 2}]};
  let calls = [], region = 'CPH';
  const f = fixture(async path => { calls.push(path); return response; }, '#/analytics?tab=scopes&sub=audit&sav=transport&scope=dk', () => region);
  await f.controller.load('24h');
  assert.deepStrictEqual(calls, ['/scope-audit/transport?window=24h&scope=dk&region=CPH'], 'transport view sends scope and the global region');
  const text = f.container.textContent;
  assert(text.includes('Never asked') && text.includes('not the same as an empty declaration'), 'never asked is worded apart from empty');
  assert(text.includes('Empty declaration'), 'empty declaration shown');
  assert(text.includes('<b>never</b>') && !descendants(f.container).some(e => e.tagName === 'b'), 'names stay text');
  assert(text.includes('1 carrying dk') && text.includes('1 not seen carrying') && text.includes('1 never asked'), 'summary counts: ' + text);
  assert(text.includes('1 observer in CPH'), 'region population stated');
  assert(text.includes('not fully covered'), 'incomplete capture stated');
  assert(text.includes('never a configuration fault'), 'limitations text present');
  assert.strictEqual(f.byId('scope-audit-scope').value, 'dk');
  const options = descendants(f.byId('scope-audit-scope-list')).filter(e => e.tagName === 'option').map(e => e.value);
  assert.deepStrictEqual(options, ['dk', 'eu'], 'scope suggestions come from the population');
  // Region change reloads with the new region; declared view never sends one.
  region = 'ARN'; await f.controller.load('24h');
  assert(calls.at(-1).endsWith('region=ARN'));
  const scopeInput = f.byId('scope-audit-scope'); scopeInput.value = '#eu'; await scopeInput.handlers.change();
  assert.strictEqual(calls.at(-1), '/scope-audit/transport?window=24h&scope=eu&region=ARN');
  assert.deepStrictEqual(f.writes.at(-1).slice(2), ['transport', 'eu'], 'view and scope are written to the URL');
  const w0 = f.writes.length; region = 'OAK'; await f.controller.load('24h');
  assert(f.writes.length > w0 && f.writes.at(-1)[2] === 'transport', 'a region change rewrites the transport link state');
  assert(f.container.classes.has('scope-audit-transport'), 'transport class set on the container');
  const declaredBtn = descendants(f.container).find(e => e.dataset.auditView === 'declared');
  await declaredBtn.handlers.click();
  assert.strictEqual(calls.at(-1), '/scope-audit?window=24h', 'declared view keeps its own contract');
  assert.strictEqual(f.writes.at(-1)[2], 'declared');
  assert(f.byId('scope-audit-scope-label').hidden, 'scope box hidden in the declared view');
  // A stale transport response cannot overwrite the declared view.
  let resolves = [];
  const race = fixture(() => new Promise(r => resolves.push(r)), '#/analytics?tab=scopes&sub=audit&sav=transport&scope=dk');
  const first = race.controller.load('24h');
  // The view switches back before the transport answer lands, with no new load yet.
  race.controller.syncState('#/analytics?tab=scopes&sub=audit');
  resolves[0](response); await first;
  assert(!/Never asked|seen forwarding/.test(race.container.textContent), 'stale transport rows and summary ignored');
  const second = race.controller.load('24h');
  resolves[1]({rows: [], summary: {total: 0}}); await second;
  assert(race.container.textContent.includes('No confirmed observer declarations'), 'declared view renders its own answer');
  // Region changes reach the shareable link (review finding 3): load() re-writes the state in the transport view only.
  assert(!f.container.classes.has('scope-audit-transport'), 'transport class cleared in the declared view');
  const declaredWrites = f.writes.length;
  await f.controller.load('24h');
  assert.strictEqual(f.writes.length, declaredWrites, 'declared view load does not rewrite the hash');
  const base = '#/analytics?tab=scopes&sub=audit&sav=transport&scope=dk&saq=x';
  assert.strictEqual(audit.hashWithState(base, 'x', 0, 'transport', 'dk', 'OAK,SJC'), '#/analytics?tab=scopes&sub=audit&sav=transport&scope=dk&saq=x&region=OAK%2CSJC', 'transport view writes region=');
  assert.strictEqual(audit.hashWithState(base + '&region=SJC', 'x', 0, 'transport', 'dk', 'OAK'), '#/analytics?tab=scopes&sub=audit&sav=transport&scope=dk&saq=x&region=OAK', 'region= is replaced');
  assert(!/region=/.test(audit.hashWithState(base, 'x', 0, 'transport', 'dk', '')), 'no region, none written');
  assert(!/region=|scope=|sav=/.test(audit.hashWithState(base + '&region=SJC', '', 0, 'declared', '', 'SJC').replace('&region=SJC', '')), 'declared view drops transport params');
  assert(!/region=/.test(audit.hashWithState('#/analytics?tab=scopes&sub=audit', '', 0, 'declared', '', 'SJC')), 'declared view never adds region=');
  assert(/ScopeAudit\.hashWithState/.test(require('fs').readFileSync(__dirname + '/public/analytics.js', 'utf8')), 'analytics onState uses the tested helper');
  const css = require('fs').readFileSync(__dirname + '/public/style.css', 'utf8');
  const block = css.slice(css.indexOf('/* #414 scope transport */'));
  assert(/\.scope-audit-transport \.scope-audit-controls input \{ width:100%/.test(block) && !/\n\s*\.scope-audit-controls input/.test(block), '640px width rule limited to the transport view');
  assert(block.length > 30 && /@media \(max-width: ?640px\)/.test(block), 'mobile layout rule present');
  assert(!/#[0-9a-fA-F]{3,8}\b/.test(block.split('/* end #414 */')[0].replace(/\/\*[\s\S]*?\*\//g, '')), 'transport CSS uses variables only');
  console.log('Scope Audit transport view tests passed');
}
