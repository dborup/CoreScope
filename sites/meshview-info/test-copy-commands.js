'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const source = fs.readFileSync(path.join(__dirname, 'assets/copy-commands.js'), 'utf8');

function fixture(options = {}) {
  const timers = new Map();
  let nextTimer = 0;
  const copied = [];
  const doc = { readyState: 'complete' };
  class Element {
    constructor(tag) { this.tagName = tag.toUpperCase(); this.children = []; this.attributes = {}; this.style = {}; this.listeners = {}; this.textContent = ''; }
    setAttribute(key, value) { this.attributes[key] = value; }
    appendChild(child) { this.children.push(child); child.parent = this; }
    remove() { this.parent.children = this.parent.children.filter(child => child !== this); }
    focus() { doc.activeElement = this; }
    select() { this.selected = true; }
    setSelectionRange(start, end) { this.range = [start, end]; }
    addEventListener(event, fn) { this.listeners[event] = fn; }
    querySelector(selector) {
      if (selector === 'pre code') return this.code;
      if (selector === '.code-bar') return this.bar;
      return this.children.concat(this.bar ? this.bar.children : []).find(child => '.' + child.className === selector);
    }
  }
  const block = new Element('div');
  block.code = new Element('code');
  block.code.textContent = 'set wifi.ssid NAVN_PÅ_WIFI\nset mqtt1.topic meshcore/{iata}/{device}/{type}\nreboot\n';
  block.bar = new Element('div');
  doc.body = new Element('body');
  doc.activeElement = new Element('a');
  doc.createElement = tag => new Element(tag);
  doc.querySelectorAll = () => [block];
  doc.execCommand = command => {
    assert.equal(command, 'copy');
    const textarea = doc.body.children[0];
    assert.ok(textarea.selected);
    assert.deepEqual(textarea.range, [0, block.code.textContent.length]);
    copied.push(textarea.value);
    if (options.execThrows) throw new Error('copy unavailable');
    return options.execResult !== false;
  };
  const context = vm.createContext({ document: doc, navigator: options.navigator || {}, setTimeout: fn => { timers.set(++nextTimer, fn); return nextTimer; }, clearTimeout: id => timers.delete(id) });
  function run() { vm.runInContext(source, context); }
  run();
  return { block, doc, copied, timers, run, button: block.querySelector('.copy-commands'), status: block.querySelector('.copy-status') };
}

(async function () {
  let written;
  const modern = fixture({ navigator: { clipboard: { writeText: async text => { written = text; } } } });
  assert.equal(modern.button.tagName, 'BUTTON');
  assert.equal(modern.button.type, 'button');
  assert.ok(modern.button.attributes['aria-label'].startsWith('Kopiér'));
  assert.equal(modern.status.attributes['aria-live'], 'polite');
  assert.equal(modern.status.attributes.role, 'status');
  assert.deepEqual(Object.keys(modern.button.listeners), ['click'], 'native button handles keyboard activation without extra key handlers');
  await modern.button.listeners.click();
  assert.equal(written, modern.block.code.textContent, 'preserve exact Unicode, newlines and placeholders');
  assert.equal(modern.copied.length, 0);
  assert.equal(modern.button.textContent, 'Kopieret!');
  assert.equal(modern.button.disabled, false);
  assert.equal(modern.status.textContent, 'Kommandoerne er kopieret.');
  modern.timers.forEach(fn => fn());
  assert.equal(modern.button.textContent, 'Kopiér');
  modern.run();
  assert.equal(modern.block.bar.children.length, 1, 'reinitialization does not duplicate button');
  assert.equal(modern.block.children.length, 1, 'reinitialization does not duplicate status');

  const rejected = fixture({ navigator: { clipboard: { writeText: async () => { throw new Error('denied'); } } } });
  rejected.button.focus();
  await rejected.button.listeners.click();
  assert.equal(rejected.copied[0], rejected.block.code.textContent);
  assert.equal(rejected.doc.activeElement, rejected.button, 'restore original focus');
  assert.equal(rejected.doc.body.children.length, 0, 'remove temporary textarea');
  assert.equal(rejected.button.textContent, 'Kopieret!');

  for (const options of [{ execResult: false }, { execThrows: true }]) {
    const failed = fixture(options);
    failed.button.focus();
    await failed.button.listeners.click();
    assert.equal(failed.button.textContent, 'Prøv igen');
    assert.match(failed.status.textContent, /Kunne ikke kopiere/);
    assert.equal(failed.button.disabled, false);
    assert.equal(failed.doc.activeElement, failed.button);
    assert.equal(failed.doc.body.children.length, 0);
    failed.timers.forEach(fn => fn());
    assert.equal(failed.button.textContent, 'Kopiér');
  }

  let resolve;
  let calls = 0;
  const pending = fixture({ navigator: { clipboard: { writeText: () => { calls++; return new Promise(done => { resolve = done; }); } } } });
  const first = pending.button.listeners.click();
  await pending.button.listeners.click();
  assert.equal(calls, 1, 'ignore repeat activation while pending');
  resolve();
  await first;
  console.log('PASS: copy exact text, native button, live status, fallback, failure, focus, reset, idempotence and pending guard');
}()).catch(error => { console.error(error); process.exitCode = 1; });
