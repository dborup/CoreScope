// bad-14-attr-interp-singlequote.js — single-quoted-attribute variant of the
// #351 N4 gap. Same class as bad-13 but with a single-quoted attribute.
// EXPECTED: flagged by check-xss-sinks.
/* eslint-disable */
function render(el, o) {
  el.innerHTML = `<span title='${o.observer_name}'>x</span>`;
}
