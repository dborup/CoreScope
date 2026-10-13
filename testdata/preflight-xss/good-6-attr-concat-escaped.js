// good-6-attr-concat-escaped.js — the escaped counterpart of bad-15/bad-16.
// The left-to-right string parse must NOT false-positive on these.
/* eslint-disable */
function escapeHtml(s) {
  if (s == null) return '';
  return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;').replace(/'/g,'&#39;');
}
function render(el, o) {
  el.innerHTML = '<span title="' + escapeHtml(o.name) + '">x</span>';
  el.innerHTML = "<span title='" + escapeHtml(o.observer_name) + "'>x</span>";
  el.innerHTML = '<a href="https://example.org/">' + escapeHtml(o.name) + '</a>';
}
