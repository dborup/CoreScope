/* === CoreScope — scope-member-filter.js (live side of a member-set filter) === */
'use strict';

/**
 * #990: a member-set filter (?area= today, the DAGI params in #740 M3b) keeps
 * the packets whose sender is a member of the selection. The server applies
 * it to /api/packets (filterPacketsPrepared and packetWhere, cmd/server), but
 * live WebSocket packets never pass that query, so a page that appends them
 * checks them here by the same rule.
 *
 * create({ fetchMembers, keyOf, emptyMatchesAll }) keeps one selection:
 *   fetchMembers(key)  a Promise of the selection's member public keys. It is
 *                      called once per change of selection, never per packet.
 *   keyOf(item)        the item's sender key ('' for none), e.g.
 *                      p => advertSender(getParsedDecoded(p)).
 *   emptyMatchesAll    what a selection with no member means. Area: true, as
 *                      the server's areaMemberSet then returns nil and filters
 *                      nothing. DAGI (#740 M2): false, an empty set matches
 *                      nothing.
 *
 * The instance:
 *   setScope(key)  selects key ('' for none). The same key again fetches
 *                  nothing, unless its last load failed. Resolves when loaded.
 *   accepts(item)  O(1). True with no selection. False while the set loads or
 *                  after a failed load: a packet is never shown unchecked.
 *                  Otherwise the sender must be a member; no sender is no
 *                  member.
 *   ready()        whether accepts() answers from a loaded set (or there is
 *                  no selection).
 *   scope()        the selected key.
 *
 * Several filters combine by requiring every accepts(), as the server ANDs
 * Area and DAGI (scopeNodes).
 */
(function () {
  // The server's sender rule: the decoded public_key, else pubKey, as a
  // string. Only ADVERTs name their sender, so any other packet has none.
  function advertSender(decoded) {
    if (!decoded || typeof decoded !== 'object') return '';
    var pk = typeof decoded.public_key === 'string' ? decoded.public_key : '';
    if (!pk && typeof decoded.pubKey === 'string') pk = decoded.pubKey;
    return pk;
  }

  function create(opts) {
    var fetchMembers = opts.fetchMembers;
    var keyOf = opts.keyOf;
    var emptyMatchesAll = !!opts.emptyMatchesAll;
    var scope = '';
    var members = null;   // Set of the selection's members; null while loading or after a failure
    var loading = null;   // the load in flight for scope
    var generation = 0;   // only the latest setScope() installs its set

    function setScope(key) {
      key = key || '';
      if (key === scope && (!key || members || loading)) return loading || Promise.resolve();
      var gen = ++generation;
      scope = key;
      members = null;
      loading = null;
      if (!key) return Promise.resolve();
      var request;
      try { request = Promise.resolve(fetchMembers(key)); } catch (e) { request = Promise.reject(e); }
      loading = request.then(function (list) {
        if (gen !== generation) return;
        members = new Set(list || []);
        loading = null;
      }, function () {
        if (gen !== generation) return;
        loading = null; // members stays null: drop packets until a retry loads
      });
      return loading;
    }

    function accepts(item) {
      if (!scope) return true;
      if (!members) return false;
      if (!members.size) return emptyMatchesAll;
      var pk = keyOf(item);
      return !!pk && members.has(pk);
    }

    return {
      setScope: setScope,
      accepts: accepts,
      ready: function () { return !scope || !!members; },
      scope: function () { return scope; },
    };
  }

  window.ScopeMemberFilter = { create: create, advertSender: advertSender };
})();
