'use strict';

// Privacy — opt-in GDPR/privacy-notice page (#/privacy). Content is driven
// by the operator's `privacy` config section, surfaced (only when enabled)
// through /api/config/client — see PrivacyConfig (cmd/server/config.go) and
// window.MC_PRIVACY (public/roles.js). Every config value is rendered
// through escapeHtml: the config fields are plain text by contract, never
// markup.
//
// This page ships NO default legal text. Retention, legal basis and the
// contact address are facts only the operator knows, so the server refuses
// to publish the notice unless they are configured (PrivacyConfig.Validate)
// — the page therefore never has to invent them. Nothing here is legal
// advice, and rendering this page does not by itself make a deployment
// compliant.

(function () {
  // Phosphor icons, not emoji — see issue #1648. New files start clean.
  function phIcon(name) {
    return '<svg class="ph-icon" aria-hidden="true"><use href="/icons/phosphor-sprite.svg#ph-' + name + '"/></svg>';
  }

  // The ONLY default: a neutral stand-in when the operator chose not to
  // publish a name. Retention / legal basis / contact have no defaults by
  // design — the server withholds the whole notice when they are missing.
  var DEFAULT_OPERATOR = 'The operator of this site';

  // mailtoHref builds a mailto: URL that cannot be turned into a header or
  // query injection by a malformed config value. The server already
  // validates the address conservatively, but this is the render-side belt:
  // percent-encode everything, then put "@" back so the href stays readable.
  // CR/LF -> %0D%0A, "?" -> %3F, "&" -> %26, quotes and spaces likewise, so
  // no extra mailto header (?subject=, &cc=) can be smuggled in.
  function mailtoHref(email) {
    return 'mailto:' + encodeURIComponent(email).replace(/%40/g, '@');
  }

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
    var rawEmail = String(cfg.contactEmail || '').trim();
    var email = escapeHtml(rawEmail);
    // href and text are escaped separately: the href goes through
    // mailtoHref (percent-encoding) FIRST so a stray "?" or CR/LF cannot
    // open a mailto query, then through escapeHtml for attribute context.
    var emailHref = escapeHtml(mailtoHref(rawEmail));
    // No fallbacks: the server does not publish the notice unless these are
    // present, so reaching render() means they are.
    var retention = escapeHtml(String(cfg.retentionText || '').trim());
    var legalBasis = escapeHtml(String(cfg.legalBasisText || '').trim());
    var contactInline = rawEmail
      ? '<a href="' + emailHref + '">' + email + '</a>'
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
      (rawEmail ? '<br>Contact: <a href="' + emailHref + '" class="mono">' + email + '</a>' : '') +
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

    // The lawful basis is the operator's statement, not the software's.
    // CoreScope does not assert one on their behalf and does not claim the
    // wording is legal advice.
    html += section('scales', 'Why, and on what legal basis',
      '<p>' + legalBasis + '</p>' +
      '<p class="text-muted">This basis is stated by the operator of this deployment.</p>');

    html += section('clock', 'How long data is kept',
      '<p>' + retention + '</p>');

    // Self-service hiding is only offered when this deployment actually
    // has hide prefixes configured. The list comes from the server's live
    // Config.HiddenNamePrefixes, so the page names the real prefix instead
    // of hardcoding one -- and stays silent when there is none rather than
    // promising a remedy that would not work.
    var prefixes = Array.isArray(cfg.hiddenNamePrefixes) ? cfg.hiddenNamePrefixes.filter(function (x) {
      return typeof x === 'string' && x.trim() !== '';
    }) : [];
    var selfServiceHtml = '';
    if (prefixes.length) {
      var rendered = prefixes.map(function (x) {
        return '<span class="mono">' + escapeHtml(x) + '</span>';
      }).join(prefixes.length === 2 ? ' or ' : ', ');
      selfServiceHtml =
        '<p>You can also hide your node yourself: rename it so it starts with ' + rendered +
        '. This site then stops listing it, without waiting for data to age out.</p>' +
        '<p class="text-muted">Two caveats. This hides the node from <strong>this site’s</strong> dashboard and API only — your radio keeps transmitting and every other listener on the mesh still receives it. And it hides the node going forward; packets and observations already recorded may remain in this site’s database until they age out or the operator deletes them. For actual deletion, use the contact above.</p>';
    } else {
      selfServiceHtml =
        '<p class="text-muted">This deployment has no self-service name prefix configured, so hiding a node requires contacting the operator above.</p>';
    }

    html += section('prohibit', 'Your rights and opting out',
      '<p>If you operate a node and do not want it shown here, contact ' + contactInline + ' and it will be hidden or removed. You can also make your node effectively anonymous yourself: give it a name that does not identify you, and disable or coarsen its advertised position.</p>' +
      selfServiceHtml +
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
