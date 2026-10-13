// good-7-literal-dollar-brace.js — a plain quoted string whose TEXT contains
// "${name}" is not an interpolation. The round-1 N4 workaround preserved such
// literals and could flag them; the left-to-right parse does not.
/* eslint-disable */
function render(el) {
  el.innerHTML = '<code>${name}</code> is a template placeholder';
}
