/* === CoreScope — live-scope-color.js === */
/* #882 — Live "Color by scope". Pure logic, no DOM: live.js owns the
 * checkboxes, the legend and the canvas.
 *
 * Classification (cmd/ingestor stores three states in transmissions.scope_name;
 * the server sends "" as null, so route_type is what separates them):
 *   route 0/3 (TRANSPORT_FLOOD / TRANSPORT_DIRECT) + name → 'named'
 *   route 0/3 without a name (unknown or ambiguous region) → 'unknown'
 *   anything else (FLOOD, DIRECT, missing)                → 'none'
 *
 * Name → colour: FNV-1a 32-bit mod 12 is the preferred slot. A per-session
 * registry keeps the slot a name got (sticky) and, when the preferred slot is
 * held by another scope seen in the legend window, probes to the next free
 * one. With all 12 slots held by active scopes, the next one shares the
 * "other" colour. The registry is an LRU capped at CAP entries.
 *
 * Hardcoded for now (window, rows, cap); exposing them and the palette in the
 * customizer is M2 (AGENTS.md rule 8).
 */
'use strict';

(function () {
  var SLOTS = 12;
  var CAP = 64;
  var WINDOW_MS = 15 * 60000;
  var LEGEND_ROWS = 12;
  var OTHER = -1;
  var PALETTE_VARS = [];
  for (var i = 1; i <= SLOTS; i++) PALETTE_VARS.push('--scope-' + i);
  PALETTE_VARS.push('--scope-none', '--scope-unknown', '--scope-other');

  function isScoped(routeType) {
    var rt = Number(routeType);
    return routeType != null && routeType !== '' && (rt === 0 || rt === 3);
  }

  function classify(routeType, scopeName) {
    if (!isScoped(routeType)) return { kind: 'none', name: '' };
    if (typeof scopeName === 'string' && scopeName) return { kind: 'named', name: scopeName };
    return { kind: 'unknown', name: '' };
  }

  function fnv1a(str) {
    var h = 0x811c9dc5;
    for (var i = 0; i < str.length; i++) {
      h ^= str.charCodeAt(i);
      h = Math.imul(h, 0x01000193);
    }
    return h >>> 0;
  }

  function preferredSlot(name) { return fnv1a(String(name)) % SLOTS; }

  // Session registry: Map<name, { slot, lastTs }> in LRU order (oldest first),
  // plus slotOwner[slot] = name. Every operation is O(1) or O(SLOTS); legend()
  // is O(CAP log CAP), called at most once a second.
  function createRegistry() {
    var entries = new Map();
    var slotOwner = new Array(SLOTS);

    function active(e, now) { return !!e && now - e.lastTs <= WINDOW_MS; }

    function slotFreeFor(slot, name, now) {
      var owner = slotOwner[slot];
      if (owner === undefined || owner === name) return true;
      return !active(entries.get(owner), now);
    }

    function probe(name, now) {
      var start = preferredSlot(name);
      for (var k = 0; k < SLOTS; k++) {
        var s = (start + k) % SLOTS;
        if (slotFreeFor(s, name, now)) return s;
      }
      return OTHER;
    }

    function touch(name, now) {
      var e = entries.get(name);
      if (e) entries.delete(name);
      else e = { slot: OTHER, lastTs: now };
      if (e.slot === OTHER || slotOwner[e.slot] !== name) {
        e.slot = probe(name, now);
        if (e.slot !== OTHER) slotOwner[e.slot] = name;
      }
      e.lastTs = now;
      entries.set(name, e);
      if (entries.size > CAP) {
        var oldest = entries.keys().next().value;
        var oe = entries.get(oldest);
        if (oe && oe.slot !== OTHER && slotOwner[oe.slot] === oldest) slotOwner[oe.slot] = undefined;
        entries.delete(oldest);
      }
      return e.slot;
    }

    // Colour for a name without registering it (VCR replay).
    function slotOf(name) {
      var e = entries.get(name);
      return e ? e.slot : preferredSlot(name);
    }

    function legend(now) {
      var act = [];
      entries.forEach(function (e, name) { if (active(e, now)) act.push({ name: name, slot: e.slot, lastTs: e.lastTs }); });
      act.sort(function (a, b) { return b.lastTs - a.lastTs; });
      var rows = act.slice(0, LEGEND_ROWS).map(function (x) { return { name: x.name, slot: x.slot }; });
      rows.sort(function (a, b) { return a.name < b.name ? -1 : a.name > b.name ? 1 : 0; });
      var more = act.length - rows.length;
      var sig = rows.map(function (r) { return r.name + '\u0000' + r.slot; }).join('\u0001') + '\u0002' + more;
      return { rows: rows, more: more, sig: sig };
    }

    return {
      touch: touch, slotOf: slotOf, legend: legend,
      size: function () { return entries.size; },
      has: function (n) { return entries.has(n); },
    };
  }

  function paletteVar(cls, slot) {
    if (cls.kind === 'none') return '--scope-none';
    if (cls.kind === 'unknown') return '--scope-unknown';
    return slot >= 0 && slot < SLOTS ? '--scope-' + (slot + 1) : '--scope-other';
  }

  // readVar(name) → computed value of a CSS custom property.
  function readPalette(readVar) {
    var out = {};
    var fallback = String(readVar('--scope-none') || '').trim();
    for (var i = 0; i < PALETTE_VARS.length; i++) {
      var v = String(readVar(PALETTE_VARS[i]) || '').trim();
      out[PALETTE_VARS[i]] = v || fallback;
    }
    return out;
  }

  // ?color=scope|hash|type wins over the stored checkboxes; anything else is
  // ignored. 'hash' and 'type' turn scope off and set hash on/off.
  function resolveColorMode(param, stored) {
    if (param === 'scope') return { scope: true, hash: !!stored.hash, fromHash: true };
    if (param === 'hash') return { scope: false, hash: true, fromHash: true };
    if (param === 'type') return { scope: false, hash: false, fromHash: true };
    return { scope: !!stored.scope, hash: !!stored.hash, fromHash: false };
  }

  function resolveScopedOnly(param, stored) {
    if (param === '1') return { on: true, fromHash: true };
    if (param === '0') return { on: false, fromHash: true };
    return { on: !!stored, fromHash: false };
  }

  window.LiveScopeColor = {
    SLOTS: SLOTS, CAP: CAP, WINDOW_MS: WINDOW_MS, LEGEND_ROWS: LEGEND_ROWS, OTHER: OTHER,
    PALETTE_VARS: PALETTE_VARS,
    STORAGE_KEY: 'meshcore-color-packets-by-scope',
    SCOPED_STORAGE_KEY: 'live-scoped-only',
    COLOR_PARAM: 'color', SCOPED_PARAM: 'scoped',
    isScoped: isScoped, classify: classify, fnv1a: fnv1a, preferredSlot: preferredSlot,
    createRegistry: createRegistry, paletteVar: paletteVar, readPalette: readPalette,
    resolveColorMode: resolveColorMode, resolveScopedOnly: resolveScopedOnly,
  };
})();
