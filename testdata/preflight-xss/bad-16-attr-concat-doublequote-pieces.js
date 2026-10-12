// bad-16-attr-concat-doublequote-pieces.js — XSS fixture (#333 F4).
// Mirror of bad-15: double-quoted string pieces around a single-quoted
// attribute. The '…' pass used to swallow `' + o.observer_name + "'`.
// EXPECTED: flagged by check-xss-sinks.
/* eslint-disable */
function render(el, o) {
  el.innerHTML = "<span title='" + o.observer_name + "'>x</span>";
}
