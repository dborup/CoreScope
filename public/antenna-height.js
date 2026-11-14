/* #681 M6 — the one rule for a customizer antenna-height default.
 *
 * The customizer (customize-v2.js) stores the default and the Path profile
 * (terrain-profile.js) uses it, so both must read a value the same way: a
 * second, looser parser is how "0x1F4" got past the customizer's 0-100 m
 * check, was stored as 500 and came out of the profile as 100 m.
 *
 * A height is a finite number, or a plain decimal string, in 0-100 m. Hex,
 * exponents, padding, signs, NaN and Infinity are not heights, and an
 * out-of-range value is refused rather than clamped: a default nobody can
 * see in the customizer must not quietly become a different one here.
 *
 * Loaded before customize-v2.js and terrain-profile.js (index.html).
 */
(function (global) {
  'use strict';

  var MAX_M = 100;
  // Same digit limits as terrain-profile.js DECIMAL_RE, without the sign.
  var HEIGHT_RE = /^[0-9]{1,6}(\.[0-9]{1,10})?$/;

  /* parse returns the height in metres, rounded to 0.1 m as the profile
   * draws it, or null when `v` is not a usable default. */
  function parse(v) {
    var n;
    if (typeof v === 'number') n = v;
    else if (typeof v === 'string' && HEIGHT_RE.test(v)) n = Number(v);
    else return null;
    if (!isFinite(n) || n < 0 || n > MAX_M) return null;
    return Math.round(n * 10) / 10 || 0;
  }

  global.MCAntennaHeight = { MAX_M: MAX_M, parse: parse };
})(typeof window !== 'undefined' ? window : this);
