// bad-17-url-in-string.js — XSS fixture (#333 F4).
// A `//` inside a string literal (a URL) is not a comment. The old stripper
// removed `//…` before it parsed strings, cutting the line at `https:` and
// dropping the unescaped node field that followed.
// EXPECTED: flagged by check-xss-sinks.
/* eslint-disable */
function render(el, o) {
  el.innerHTML = '<a href="https://example.org/">' + o.name + '</a>';
}
