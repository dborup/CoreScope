'use strict';
const assert = require('assert');
const audit = require('./public/scope-audit.js');
assert.strictEqual(audit.windowValue('bad'), '24h');
assert.strictEqual(audit.windowValue('7d'), '7d');
assert.strictEqual(audit.searchRows([{name: 'ALPHA', publicKey: 'abc'}, {name: 'Beta', publicKey: 'def'}], 'alpha').length, 1);
assert.strictEqual(audit.searchRows([{name: null, publicKey: 123}], '123').length, 1);
assert.strictEqual(audit.declarationAge('86400'), '1d');
assert.strictEqual(audit.declarationAge(null), 'Unknown');
assert.strictEqual(audit.stateFromHash('#/analytics?tab=scopes&sub=audit&swin=7d&saq=a%26b').query, 'a&b');
assert.strictEqual(audit.stateFromHash('#/analytics?tab=scopes&sub=audit&swin=no').window, '24h');
console.log('Scope Audit helpers passed');
assert.strictEqual(audit.assessment({forwarded: '4', status: 'consistent', unknownScopeObserved: '2'}), 'Incomplete evidence');
assert.strictEqual(audit.assessment({forwarded: '4', staleDeclaration: true, wildcardContradiction: true}), 'Findings · incomplete evidence');
assert.strictEqual(audit.assessment({forwarded: '0', status: 'consistent'}), 'No forwarding evidence');
// Minimal DOM tracks node identity and treats innerHTML writes as a test failure.
class Element {
  constructor(tag, doc) { this.tagName = tag; this.ownerDocument = doc; this.children = []; this.dataset = {}; this.handlers = {}; this._text = ''; this.isConnected = true; this.classList = { toggle() {} }; }
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
function fixture(apiFn, hash = '#/analytics?tab=scopes&sub=audit') {
  const doc = {createElement: tag => new Element(tag, doc)};
  const container = new Element('div', doc); let writes = [];
  const controller = audit.mount(container, {api: apiFn, hash: () => hash, onWindow() {}, onState: (...v) => writes.push(v)});
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
})().catch(err => { console.error(err); process.exitCode = 1; });
