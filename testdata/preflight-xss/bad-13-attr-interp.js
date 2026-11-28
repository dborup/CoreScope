// bad-13-attr-interp.js — XSS fixture for check-xss-sinks (#333 / #351 N4).
// Unescaped node-controlled ${observer_name} interpolated into a *quoted
// attribute* inside a template literal on a sink line. The quote-stripper
// used to erase the interpolation before the audit, so this passed the gate.
// EXPECTED: flagged by check-xss-sinks.
/* eslint-disable */
function render(el, o) {
  el.innerHTML = `<span title="${o.observer_name}">x</span>`;
}
