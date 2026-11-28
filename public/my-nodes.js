/* === CoreScope — my-nodes.js (#1018 M1) === */
'use strict';

// MyNodes — the ONE owner of the "My nodes" list (My Mesh). Every page
// reads it through this module; nothing else touches the storage keys.
//
// Signed out it is exactly the old browser-only list: `meshcore-my-nodes`
// in localStorage, entries {pubkey, name, addedAt}.
//
// Signed in (account.js calls setAccount after GET /api/account) the
// account's synced list is the source of truth: it is cached under
// `meshcore-account-my-nodes` so every page can read it synchronously at
// load, and changes go to the server through the writer account.js
// registers. The browser's own list is never deleted: it is what the
// one-time migration offers to add to the account, and what the page shows
// again after signing out.
//
// No DOM and no network: account.js owns both, so this file is testable in
// Node through vm.createContext.

(function (root) {
  var LOCAL_KEY = 'meshcore-my-nodes';
  var ACCOUNT_KEY = 'meshcore-account-my-nodes';
  // The migration banner's "Not now", per browser and account.
  var DISMISS_PREFIX = 'meshcore-my-nodes-migrate-dismissed:';

  var listeners = [];
  var writer = null;

  function store() {
    try {
      if (root.localStorage) return root.localStorage;
      return typeof localStorage !== 'undefined' ? localStorage : null;
    } catch (e) { return null; }
  }
  function readJSON(key) {
    var s = store();
    if (!s) return null;
    try { return JSON.parse(s.getItem(key)); } catch (e) { return null; }
  }
  function writeJSON(key, v) {
    var s = store();
    if (!s) return;
    try { s.setItem(key, JSON.stringify(v)); } catch (e) { /* quota or private mode */ }
  }

  function keyOf(pk) { return String(pk || '').toLowerCase(); }

  // normalize keeps the entries with a pubkey, one per key (first wins),
  // in the {pubkey, name, addedAt} shape; anything that is not a list is [].
  function normalize(list) {
    if (!Array.isArray(list)) return [];
    var seen = Object.create(null);
    var out = [];
    for (var i = 0; i < list.length; i++) {
      var n = list[i];
      if (!n || typeof n.pubkey !== 'string' || !n.pubkey) continue;
      var k = keyOf(n.pubkey);
      if (seen[k]) continue;
      seen[k] = true;
      out.push({ pubkey: n.pubkey, name: typeof n.name === 'string' ? n.name : '', addedAt: n.addedAt == null ? null : n.addedAt });
    }
    return out;
  }

  function accountState() {
    var st = readJSON(ACCOUNT_KEY);
    if (!st || typeof st !== 'object' || typeof st.identityPrefix !== 'string') return null;
    return { identityPrefix: st.identityPrefix, max: Number(st.max) > 0 ? Number(st.max) : 0, nodes: normalize(st.nodes) };
  }

  function emit() {
    var snapshot = list();
    for (var i = 0; i < listeners.length; i++) {
      try { listeners[i](snapshot); } catch (e) { /* one listener must not break the others */ }
    }
  }

  function localList() { return normalize(readJSON(LOCAL_KEY)); }
  function signedIn() { return accountState() !== null; }
  function list() {
    var st = accountState();
    return st ? st.nodes : localList();
  }
  function keys() { return list().map(function (n) { return n.pubkey; }); }
  function has(pubkey) {
    var k = keyOf(pubkey);
    return list().some(function (n) { return keyOf(n.pubkey) === k; });
  }

  // add puts a node on the current list. Signed in, a full list (the
  // account's maxMyNodes) refuses it and returns false.
  function add(pubkey, name) {
    if (!pubkey || has(pubkey)) return true;
    var entry = { pubkey: pubkey, name: name || String(pubkey).slice(0, 12), addedAt: new Date().toISOString() };
    var st = accountState();
    if (st) {
      if (st.max && st.nodes.length >= st.max) return false;
      st.nodes.push(entry);
      writeJSON(ACCOUNT_KEY, st);
      if (writer && writer.add) writer.add(entry);
    } else {
      var l = localList();
      l.push(entry);
      writeJSON(LOCAL_KEY, l);
    }
    emit();
    return true;
  }

  function remove(pubkey) {
    var k = keyOf(pubkey);
    var keep = function (n) { return keyOf(n.pubkey) !== k; };
    var st = accountState();
    if (st) {
      st.nodes = st.nodes.filter(keep);
      writeJSON(ACCOUNT_KEY, st);
      if (writer && writer.remove) writer.remove(pubkey);
    } else {
      writeJSON(LOCAL_KEY, localList().filter(keep));
    }
    emit();
  }

  // setAccount adopts the server's list (GET /api/account); clearAccount
  // returns to the browser's list (signed out, or the session ended).
  function setAccount(identityPrefix, nodes, max) {
    writeJSON(ACCOUNT_KEY, { identityPrefix: String(identityPrefix || ''), max: Number(max) || 0, nodes: normalize(nodes) });
    emit();
  }
  function clearAccount() {
    var s = store();
    if (s && s.getItem(ACCOUNT_KEY) !== null) {
      try { s.removeItem(ACCOUNT_KEY); } catch (e) { /* ignore */ }
      emit();
    }
  }
  function setWriter(w) { writer = w || null; }

  // migrationCandidates: the browser's entries the account does not have.
  function migrationCandidates() {
    var st = accountState();
    if (!st) return [];
    var have = Object.create(null);
    st.nodes.forEach(function (n) { have[keyOf(n.pubkey)] = true; });
    return localList().filter(function (n) { return !have[keyOf(n.pubkey)]; });
  }

  // mergedForMigration: the account's list, then the browser's new
  // entries, deduplicated by lower-case key and cut to max (0: no cut).
  function mergedForMigration(max) {
    var st = accountState();
    var merged = normalize((st ? st.nodes : []).concat(migrationCandidates()));
    return max > 0 ? merged.slice(0, max) : merged;
  }

  function dismissKey(prefix) { return DISMISS_PREFIX + String(prefix || ''); }
  function migrationDismissed() {
    var st = accountState();
    var s = store();
    return !!(st && s && s.getItem(dismissKey(st.identityPrefix)));
  }
  function dismissMigration() {
    var st = accountState();
    var s = store();
    if (st && s) { try { s.setItem(dismissKey(st.identityPrefix), '1'); } catch (e) { /* ignore */ } }
  }

  function onChange(fn) {
    listeners.push(fn);
    return function () { listeners = listeners.filter(function (f) { return f !== fn; }); };
  }

  // The settings export/import (customize-v2.js) carries the browser's own
  // list, as it always did.
  function exportLocal() { return localList(); }
  function importLocal(nodes) {
    if (!Array.isArray(nodes)) return;
    writeJSON(LOCAL_KEY, normalize(nodes));
    emit();
  }

  // The Home grid's page (#1018 owner decision 14): every card makes its own
  // health request, so a long synced list renders GRID_PAGE cards at a time.
  var GRID_PAGE = 24;
  function gridPage(nodes, shown) {
    var n = Math.max(GRID_PAGE, shown | 0);
    nodes = Array.isArray(nodes) ? nodes : [];
    return { visible: nodes.slice(0, n), more: Math.max(0, nodes.length - n) };
  }

  root.MyNodes = {
    GRID_PAGE: GRID_PAGE,
    gridPage: gridPage,
    LOCAL_KEY: LOCAL_KEY,
    ACCOUNT_KEY: ACCOUNT_KEY,
    list: list,
    keys: keys,
    has: has,
    add: add,
    remove: remove,
    signedIn: signedIn,
    localList: localList,
    setAccount: setAccount,
    clearAccount: clearAccount,
    setWriter: setWriter,
    migrationCandidates: migrationCandidates,
    mergedForMigration: mergedForMigration,
    migrationDismissed: migrationDismissed,
    dismissMigration: dismissMigration,
    onChange: onChange,
    exportLocal: exportLocal,
    importLocal: importLocal,
    normalize: normalize
  };
})(typeof window !== 'undefined' ? window : this);
