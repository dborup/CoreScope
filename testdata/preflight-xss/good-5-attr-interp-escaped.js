// good-5-attr-interp-escaped.js — the escaped counterpart of bad-13. The
// node-controlled field inside the quoted attribute is wrapped in escapeHtml,
// so the N4 fix must NOT false-positive here.
/* eslint-disable */
function escapeHtml(s) {
  if (s == null) return '';
  return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;').replace(/'/g,'&#39;');
}
function render(el, o) {
  el.innerHTML = `<span title="${escapeHtml(o.observer_name)}">x</span>`;
}
