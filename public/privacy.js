'use strict';

// Privacy — opt-in GDPR/privacy-notice page (#/privacy). Content is driven
// by the operator's `privacy` config section, surfaced (only when enabled)
// through /api/config/client — see PrivacyConfig (cmd/server/config.go) and
// window.MC_PRIVACY (public/roles.js). Every config value is rendered
// through escapeHtml: the config fields are plain text by contract, never
// markup. Defaults are deliberately neutral so an operator can publish a
// usable notice with nothing but `"privacy": { "enabled": true }`.

(function () {
  // Phosphor icons, not emoji — see issue #1648. New files start clean.
  function phIcon(name) {
    return '<svg class="ph-icon" aria-hidden="true"><use href="/icons/phosphor-sprite.svg#ph-' + name + '"/></svg>';
  }

  var DEFAULT_OPERATOR = 'The operator of this site';
  var DEFAULT_RETENTION =
    'Packet data, telemetry, and decoded public-channel messages are kept ' +
    'as a historical archive used for long-term network analysis (coverage ' +
    'trends, node health over time). The node directory and map reflect the ' +
    'current state of the network; nodes that stop advertising disappear ' +
    'from the live view. Contact us (below) to have historical data about ' +
    'your node or your messages deleted.';

  // The default hidden-name convention (Config.HiddenNamePrefixes) is the
  // no-entry-sign character. Built via fromCodePoint so this source file
  // stays plain-ASCII (emoji-scan hygiene) while the page shows the real
  // character operators must prepend.
  var HIDDEN_PREFIX_CHAR = String.fromCodePoint(0x1F6AB);

  function section(icon, title, bodyHtml) {
    return '<h3 class="privacy-h">' + phIcon(icon) + ' ' + title + '</h3>' + bodyHtml;
  }

  function renderDisabled(container) {
    container.innerHTML =
      '<div class="privacy-page">' +
      '<h2>' + phIcon('lock') + ' Privacy Notice</h2>' +
      '<p class="text-muted">This deployment has not published a privacy notice.</p>' +
      '<p><a href="#/home">Back to Home</a></p>' +
      '</div>';
  }

  function render(container, cfg) {
    var operator = escapeHtml(String(cfg.operatorName || '').trim() || DEFAULT_OPERATOR);
    var email = escapeHtml(String(cfg.contactEmail || '').trim());
    var retention = escapeHtml(String(cfg.retentionText || '').trim() || DEFAULT_RETENTION);
    // escapeHtml covers the 5-char OWASP set (incl. quotes), so the same
    // escaped value is safe in both text and attribute context here.
    var contactInline = email
      ? '<a href="mailto:' + email + '">' + email + '</a>'
      : 'the contact listed by this site’s operator';

    var html =
      '<div class="privacy-page">' +
      '<h2>' + phIcon('lock') + ' Privacy Notice</h2>' +
      '<p>This site is a community dashboard for a MeshCore LoRa mesh ' +
      'network, built on the open-source CoreScope analyzer. Volunteer-run ' +
      'observer nodes forward the radio packets they hear to this site, ' +
      'which displays a live map and analysis of the network so operators ' +
      'can see coverage, diagnose problems, and keep the mesh healthy.</p>';

    html += section('info', 'Data controller',
      '<p><strong>' + operator + '</strong>' +
      (email ? '<br>Contact: <a href="mailto:' + email + '" class="mono">' + email + '</a>' : '') +
      '</p>');

    html += section('broadcast', 'What data this site processes',
      '<p>All data originates from radio packets that MeshCore devices broadcast themselves:</p>' +
      '<ul>' +
      '<li><strong>Node adverts</strong>: node name, role, public key, and the GPS position the node is configured to advertise. Node names are chosen by their operators and may contain a personal handle or name; an advertised position may reveal where the operator lives.</li>' +
      '<li><strong>Packet metadata</strong>: timestamps, packet types, routing paths, hop counts, and signal measurements (SNR/RSSI) as heard by observers.</li>' +
      '<li><strong>Node telemetry</strong>: values a node chooses to broadcast, such as battery level and uptime.</li>' +
      '<li><strong>Public channel messages</strong>: messages sent on well-known public channels (whose encryption keys are community knowledge) are decoded and shown, including the sender’s node name and timestamp. Direct (private) messages are end-to-end encrypted and are never decrypted or displayed.</li>' +
      '</ul>' +
      '<p><strong>Website visitors</strong>: this site itself sets no analytics, tracking, or advertising cookies. Display preferences (such as theme) are stored only in your own browser.</p>');

    html += section('scales', 'Why, and on what legal basis',
      '<p>This data is processed under <strong>legitimate interest</strong> (GDPR Art. 6(1)(f)): operating, mapping, and troubleshooting a community radio network — the same purpose for which node operators broadcast this information in the first place. The data shown is limited to what devices already transmit openly over the air, and an easy opt-out exists (below).</p>');

    html += section('clock', 'How long data is kept',
      '<p>' + retention + '</p>');

    html += section('prohibit', 'Your rights and opting out',
      '<p>If you operate a node and do not want it shown here, contact ' + contactInline + ' and it will be hidden or removed. You can also make your node effectively anonymous yourself: give it a name that does not identify you, and disable or coarsen its advertised position. By default, this software also hides any node whose name starts with <span class="mono">' + HIDDEN_PREFIX_CHAR + '</span> — rename your node with that prefix and it disappears from this site without waiting for data to age out.</p>' + // EMOJI-OK-LEGACY-RENDER: the actual configured hidden-name prefix character
      '<p>Under the GDPR you additionally have the right to access, rectify, erase, restrict, and object to the processing of your personal data (Arts. 15–21), and to lodge a complaint with your national data protection authority.</p>');

    html += section('chats', 'A note on public channels',
      '<p>Public MeshCore channels are receivable and readable by anyone with a radio. Please do not send personal information over them — this site, like any other listener, will pick it up. If something personal does end up here, contact ' + contactInline + ' to have it deleted.</p>');

    html += '</div>';
    container.innerHTML = html;
  }

  registerPage('privacy', {
    init: function (container) {
      container.innerHTML =
        '<div class="privacy-page"><h2>' + phIcon('lock') + ' Privacy Notice</h2>' +
        '<p class="text-muted">Loading…</p></div>';
      // Config arrives via roles.js's /api/config/client fetch. Gate on
      // MeshConfigReady so a direct deep-link to #/privacy renders after
      // the config (and window.MC_PRIVACY) is actually there.
      var ready = (window.MeshConfigReady && typeof window.MeshConfigReady.then === 'function')
        ? window.MeshConfigReady
        : Promise.resolve();
      return ready.then(function () {
        var cfg = window.MC_PRIVACY;
        // Server-side gate already omits the section unless enabled; the
        // explicit enabled check is belt-and-braces for stale caches.
        if (!cfg || cfg.enabled === false) { renderDisabled(container); return; }
        render(container, cfg);
      }).catch(function () { renderDisabled(container); });
    },
    destroy: function () {}
  });
})();
