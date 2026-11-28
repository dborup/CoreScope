#!/usr/bin/env node
/**
 * #875: the Live legend toggle lives in the top-right Leaflet control stack
 * (under Fullscreen and Settings) and the legend panel has its own x close
 * button. Source-level checks; geometry is covered by
 * test-issue-875-legend-toggle-e2e.js.
 */
'use strict';
const fs = require('fs');
const path = require('path');
const js = fs.readFileSync(path.join(__dirname, 'public/live.js'), 'utf8');
const css = fs.readFileSync(path.join(__dirname, 'public/live.css'), 'utf8');

let failed = 0;
function step(name, fn) {
  try { fn(); console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

console.log('#875: legend toggle in map controls + legend close button');

step('legend toggle is built by a topright Leaflet control, not static markup', () => {
  assert(!/<button[^>]*id="legendToggleBtn"/.test(js), 'static #legendToggleBtn markup still present');
  const m = js.match(/LiveLegendControl\s*=\s*L\.Control\.extend\(\{([\s\S]*?)\n    \}\);/);
  assert(m, 'LiveLegendControl missing');
  assert(/position:\s*'topright'/.test(m[1]), 'LiveLegendControl is not topright');
  assert(/live-leaflet-toggle/.test(m[1]), 'LiveLegendControl lacks live-leaflet-toggle');
  assert(/legendToggleBtn/.test(m[1]), 'LiveLegendControl lacks #legendToggleBtn');
});

step('legend control is added after Settings (stacks under Fullscreen and Settings)', () => {
  const s = js.indexOf('map.addControl(new LiveSettingsControl())');
  const l = js.indexOf('map.addControl(new LiveLegendControl())');
  assert(s !== -1 && l !== -1 && l > s, 'legend control must be added after the settings control');
});

step('legend panel header has a close button wired to the shared setter', () => {
  const panel = js.match(/id="liveLegend"[\s\S]*?<div class="panel-content">/);
  assert(panel && /id="legendCloseBtn"/.test(panel[0]), '#legendCloseBtn missing from legend header');
  assert(/class="feed-hide-btn"[^>]*id="legendCloseBtn"|id="legendCloseBtn"[^>]*class="feed-hide-btn"/.test(panel[0]),
    'close button must reuse the feed-hide-btn pattern');
  assert(/legendCloseBtn[\s\S]{0,200}addEventListener\('click'/.test(js) || /getElementById\('legendCloseBtn'\)/.test(js),
    'close button not wired');
});

step('legend-hidden state still persisted under live-legend-hidden', () => {
  assert(/localStorage\.setItem\('live-legend-hidden'/.test(js), 'setItem missing');
  assert(/localStorage\.getItem\('live-legend-hidden'\)/.test(js), 'getItem missing');
});

step('old absolute-positioned .legend-toggle-btn CSS is gone', () => {
  assert(!/\.legend-toggle-btn\s*\{[^}]*position:\s*absolute/.test(css), 'absolute .legend-toggle-btn rule remains');
});

step('legend clears the right-hand control stack in right corners', () => {
  assert(/\.live-legend\[data-position="tr"\],\s*\.live-legend\[data-position="br"\]\s*\{[^}]*right:\s*var\(--live-ctrl-rail-legend,\s*68px\)/.test(css),
    'no right-corner legend offset past the control stack');
});

step('legend toggle control hidden on phone and in fullscreen like before', () => {
  assert(/body\.live-fullscreen[^{]*\.live-legend-toggle-ctl/.test(css), 'fullscreen hide missing');
  assert(/@media \(max-width: 640px\)[\s\S]*?\.live-legend-toggle-ctl\s*\{\s*display:\s*none/.test(css), 'phone hide missing');
});

step('legend is height-capped with an internal scroll, and the landscape rail fits five controls', () => {
  assert(/\.live-legend\s*\{[^}]*max-height:\s*max\([^;]*calc\(100%/.test(css), 'no max-height on .live-legend');
  assert(/--live-ctrl-rail:\s*312px/.test(css), 'landscape rail not sized for five controls');
});

step('toggle title and aria-expanded follow the state; x hands focus to the toggle', () => {
  assert(/legendToggleBtn\.title\s*=\s*label/.test(js), 'title not dynamic');
  assert(/setAttribute\('aria-expanded'/.test(js) && /setAttribute\('aria-controls',\s*'liveLegend'\)/.test(js), 'aria-expanded/controls missing');
  assert(/legendCloseBtn[\s\S]{0,160}legendToggleBtn\.focus\(\)/.test(js), 'focus not moved on close');
});

console.log(failed ? '\n' + failed + ' failed' : '\nall passed');
process.exit(failed ? 1 : 0);
