// bad-15-attr-concat.js — XSS fixture for check-xss-sinks (#333 F4).
// The concatenation form of N4: a node-controlled field concatenated into a
// double-quoted attribute built from single-quoted string pieces. The old
// quote-stripper ran its "…" pass first, so `"' + o.name + '"` looked like
// one double-quoted string and the field was erased before the audit.
// EXPECTED: flagged by check-xss-sinks.
/* eslint-disable */
function render(el, o) {
  el.innerHTML = '<span title="' + o.name + '">x</span>';
}
