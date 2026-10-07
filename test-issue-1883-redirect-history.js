/* Regression tests for legacy-route history replacement (#1883). */
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');
const vm = require('vm');

const appPath = process.env.APP_JS || path.join(__dirname, 'public', 'app.js');

function makeHistorySandbox() {
  const entries = ['#/home'];
  let index = 0;
  let currentHash = entries[index];

  const location = {};
  Object.defineProperty(location, 'hash', {
    get() { return currentHash; },
    set(hash) {
      if (hash === currentHash) return;
      entries.splice(index + 1);
      entries.push(hash);
      index++;
      currentHash = hash;
    },
  });

  const history = {
    get length() { return entries.length; },
    replaceState(_state, _title, hash) {
      entries[index] = hash;
      currentHash = hash;
    },
    back() {
      if (index === 0) return;
      index--;
      currentHash = entries[index];
    },
  };

  const classList = { add() {}, remove() {}, toggle() {} };
  const window = { addEventListener() {}, dispatchEvent() {} };
  const document = {
    readyState: 'complete',
    body: { classList },
    createElement: () => ({ id: '', textContent: '', innerHTML: '' }),
    head: { appendChild() {} },
    getElementById: () => null,
    addEventListener() {},
    querySelectorAll: () => [],
    querySelector: () => null,
  };
  window.document = document;
  window.location = location;
  window.history = history;

  const sandbox = {
    window,
    document,
    location,
    history,
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
    parseInt,
    parseFloat,
    isNaN,
    isFinite,
    encodeURIComponent,
    decodeURIComponent,
    setTimeout() {},
    clearTimeout() {},
    setInterval() {},
    clearInterval() {},
    fetch: () => Promise.resolve({ json: () => Promise.resolve({}) }),
    performance: { now: () => Date.now() },
    localStorage: { getItem: () => null, setItem() {}, removeItem() {} },
    CustomEvent: class CustomEvent {},
    Map,
    Promise,
    URLSearchParams,
    addEventListener() {},
    dispatchEvent() {},
    requestAnimationFrame() {},
  };

  vm.createContext(sandbox);
  vm.runInContext(fs.readFileSync(appPath, 'utf8'), sandbox, { filename: appPath });
  return sandbox;
}

function isLegacyRedirect(hash) {
  return hash.startsWith('#/traces/') || hash === '#/roles' ||
    hash.startsWith('#/roles?') || hash.startsWith('#/roles/');
}

function assertRedirectReplacesHistory(legacyHash, expectedHash) {
  const sandbox = makeHistorySandbox();
  const productionNavigate = sandbox.navigate;
  let recursiveNavigateCalls = 0;

  // Keep the test focused on the redirect branch: the production redirect
  // deliberately re-enters navigate(), while rendering the target route is
  // covered by the browser suite.
  sandbox.navigate = () => { recursiveNavigateCalls++; };

  sandbox.location.hash = legacyHash;
  const historyLengthBeforeRedirect = sandbox.history.length;
  productionNavigate();

  assert.strictEqual(sandbox.location.hash, expectedHash,
    `${legacyHash} should redirect to ${expectedHash}`);
  assert.strictEqual(sandbox.history.length, historyLengthBeforeRedirect,
    `${legacyHash} must replace its history entry instead of adding one`);
  assert.strictEqual(recursiveNavigateCalls, 1,
    `${legacyHash} should render the replacement route immediately`);

  sandbox.history.back();
  // Browsers dispatch hashchange after Back. Re-run the production router only
  // when Back exposed the legacy entry; the old location.hash implementation
  // redirects forward again here and reproduces the loop.
  if (isLegacyRedirect(sandbox.location.hash)) productionNavigate();

  assert.strictEqual(sandbox.location.hash, '#/home',
    `Back from ${expectedHash} should return to #/home without a redirect loop`);
}

const cases = [
  ['#/traces/a1b2c3d4', '#/tools/trace/a1b2c3d4'],
  ['#/roles', '#/analytics?tab=roles'],
  ['#/roles?from=bookmark', '#/analytics?tab=roles'],
  ['#/roles/legacy', '#/analytics?tab=roles'],
];

for (const [legacyHash, expectedHash] of cases) {
  assertRedirectReplacesHistory(legacyHash, expectedHash);
  console.log(`  ✅ ${legacyHash} replaces history and Back returns to #/home`);
}

console.log(`\nredirect history: ${cases.length} passed, 0 failed`);
