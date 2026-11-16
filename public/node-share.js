/* === CoreScope — node-share.js ===
 * Node share links (#811 M4). Copy URL on the node page and in the side
 * pane hands out /p?node=<pk>, which previews as a node card in a chat,
 * only when the server publishes share.nodes in /api/config/client
 * (share.nodes.enabled), the node's role is one of the server's card roles,
 * the key is a full 64-hex key (the only form /p?node= accepts), and the
 * viewer has not turned "Copy link shares a preview" off in the customizer
 * (terrain.shareLinks, the #727 M4 switch, which covers both card types).
 * Otherwise it is the plain #/nodes/<pk> link, as before.
 *
 * Packet share links (#812 M4), window.PacketShare, follow the same rules
 * with the same switch: Copy link on the packet page, the standalone
 * #/packet/<id> page and View Path hands out /p?packet=<hash> (View Path:
 * &viewPath=1) only when the server publishes share.packets
 * (share.packets.enabled), the hash is exactly 16 hex characters (the only
 * form /p?packet= accepts), no observation is selected (/p cannot carry
 * ?obs=) and the viewer's switch is on. Otherwise it is the plain link of
 * before. */
(function (global) {
  'use strict';

  var FULL_KEY = /^[0-9a-fA-F]{64}$/;

  /** offered: the server serves /p?node= (share.nodes is true). */
  function offered(cfg) {
    return !!(cfg && cfg.nodes === true);
  }

  /** carded: role is one of the server's card roles (share.roles). */
  function carded(cfg, role) {
    return offered(cfg) && Array.isArray(cfg.roles) && typeof role === 'string' && role !== '' && cfg.roles.indexOf(role) >= 0;
  }

  /** viewerOn: the customizer's switch. The rule (on unless stored false)
   *  is TerrainProfile.themeDefaults', the one the Path profile's Copy link
   *  reads; without that module there is no preview link (fail closed). */
  function viewerOn(siteConfig) {
    var tp = global.TerrainProfile;
    if (!tp || typeof tp.themeDefaults !== 'function') return false;
    return tp.themeDefaults((siteConfig && siteConfig.terrain) || null).shareLinks === true;
  }

  function url(loc, pk, role, cfg, viewer) {
    var key = String(pk == null ? '' : pk);
    var origin = String((loc && loc.origin) || '');
    if (viewer === true && FULL_KEY.test(key) && carded(cfg, role)) {
      return origin + '/p?node=' + key.toLowerCase();
    }
    return origin + '/#/nodes/' + encodeURIComponent(key);
  }

  /** copyURL: the link for node n, read at copy time so a late config or
   *  a customizer change needs no re-render. */
  function copyURL(n) {
    return url(global.location, n && n.public_key, n && n.role, global.MC_SHARE_CFG, viewerOn(global.SITE_CONFIG));
  }

  global.NodeShare = { offered: offered, carded: carded, viewerOn: viewerOn, url: url, copyURL: copyURL };

  var FULL_HASH = /^[0-9a-fA-F]{16}$/;

  /** packetsOffered: the server serves /p?packet= (share.packets is true). */
  function packetsOffered(cfg) {
    return !!(cfg && cfg.packets === true);
  }

  /** packetURL: the link for a packet. opts: viewPath (the View Path
   *  modal's link), obs (the selected observation id, kept only by the
   *  plain link) and id (the plain link's target when there is no hash).
   *  The plain links are the ones packets.js and packet-path-map.js built
   *  before #812 M4. */
  function packetURL(loc, hash, opts, cfg, viewer) {
    var o = opts || {};
    var h = String(hash == null ? '' : hash);
    var origin = String((loc && loc.origin) || '');
    var obs = o.obs == null ? '' : String(o.obs);
    if (viewer === true && obs === '' && FULL_HASH.test(h) && packetsOffered(cfg)) {
      return origin + '/p?packet=' + h.toLowerCase() + (o.viewPath ? '&viewPath=1' : '');
    }
    if (o.viewPath) return origin + '/#/packets/' + encodeURIComponent(h) + '?viewPath=1';
    return origin + '/#/packets/' + (h || String(o.id == null ? '' : o.id)) + (obs ? '?obs=' + obs : '');
  }

  /** packetCopyURL: read at copy time, as copyURL. */
  function packetCopyURL(hash, opts) {
    return packetURL(global.location, hash, opts, global.MC_SHARE_CFG, viewerOn(global.SITE_CONFIG));
  }

  global.PacketShare = { offered: packetsOffered, url: packetURL, copyURL: packetCopyURL };
})(typeof window !== 'undefined' ? window : this);
