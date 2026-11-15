/* tests/skraafoto-fake-dom.js — a small DOM for Skraafoto.open() in the
 * vm sandbox, shared by test-issue-737-skraafoto-viewer.js (#818) and
 * test-issue-737-m3c-skraafoto-marks.js (#737 M3c). */
'use strict';

/* A small DOM for open(): elements with listeners, attributes (src
 * mirrored), children and focus, all recorded so a test can find them by
 * data-testid and click them. */
function fakeDom() {
  const all = [];
  function element(tag) {
    const e = {
      tagName: String(tag).toUpperCase(), attrs: {}, children: [], listeners: {}, parentNode: null, style: {},
      hidden: false, disabled: false, value: '', className: '', _text: '',
      classList: { _c: new Set(), add(c) { this._c.add(c); }, remove(c) { this._c.delete(c); },
        toggle(c, on) { if (on) this._c.add(c); else this._c.delete(c); }, contains(c) { return this._c.has(c); } },
      setAttribute(k, v) { e.attrs[k] = String(v); },
      getAttribute(k) { return Object.prototype.hasOwnProperty.call(e.attrs, k) ? e.attrs[k] : null; },
      removeAttribute(k) { delete e.attrs[k]; },
      appendChild(c) { e.children.push(c); c.parentNode = e; return c; },
      removeChild(c) { e.children = e.children.filter((x) => x !== c); c.parentNode = null; return c; },
      addEventListener(t, f) { (e.listeners[t] = e.listeners[t] || []).push(f); },
      fire(t) { (e.listeners[t] || []).forEach((f) => f({ target: e, preventDefault() {}, stopImmediatePropagation() {} })); },
      focus() { doc.activeElement = e; },
      contains(x) { for (let n = x; n; n = n.parentNode) if (n === e) return true; return false; },
      querySelectorAll() { return []; },
    };
    Object.defineProperty(e, 'textContent', {
      get() { return e._text + e.children.map((c) => c.textContent).join(''); },
      set(v) { e._text = v == null ? '' : String(v); e.children = []; },
    });
    Object.defineProperty(e, 'innerHTML', { set(v) { e._text = String(v); } });
    Object.defineProperty(e, 'src', { get() { return e.getAttribute('src') || ''; }, set(v) { e.setAttribute('src', v); } });
    all.push(e);
    return e;
  }
  const doc = {
    createElement: element, activeElement: null,
    // #737 M3c: the marks are SVG; the namespace is recorded.
    createElementNS: (ns, tag) => { const e = element(tag); e.namespaceURI = ns; return e; },
    addEventListener() {}, removeEventListener() {},
    querySelector: () => null,
  };
  doc.body = element('body');
  return { doc, byTid: (t) => all.find((e) => e.getAttribute('data-testid') === t) };
}

module.exports = { fakeDom };
