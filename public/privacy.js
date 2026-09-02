'use strict';

// Privacy — opt-in privacy-notice page (#/privacy). Content is driven by the
// operator's `privacy` config section, surfaced (only when enabled AND
// complete) through /api/config/client — see PrivacyConfig
// (cmd/server/config.go) and window.MC_PRIVACY (public/roles.js).
//
// This page ships NO default legal text and NO stand-in operator identity.
// The controller, the purposes, the lawful basis, retention, recipients and
// the rest are facts only the operator knows; the server refuses to publish
// the notice unless they are all configured (PrivacyConfig.Validate), so
// this file never has to invent them. Nothing here is legal advice, and
// rendering this page does not by itself make a deployment compliant.
//
// SECURITY: every operator-supplied value is inserted as TEXT, never as
// markup. Values reach the DOM through txt() (escapeHtml) or, for the one
// href we emit, through mailtoHref() + escapeHtml. Config fields are plain
// text by contract; this file enforces that rather than trusting it.

(function () {
  // Phosphor icons, not emoji — see issue #1648. New files start clean.
  function phIcon(name) {
    return '<svg class="ph-icon" aria-hidden="true"><use href="/icons/phosphor-sprite.svg#ph-' + name + '"/></svg>';
  }

  // txt() is the ONLY way operator config becomes page content.
  function txt(v) {
    return escapeHtml(String(v == null ? '' : v).trim());
  }

  // Operator text may legitimately contain blank-line-separated paragraphs.
  // Split on the ESCAPED value so no markup can be assembled from config.
  function paras(v) {
    var s = txt(v);
    if (!s) return '';
    return s.split(/\n\s*\n/).map(function (p) {
      return '<p>' + p.replace(/\n/g, '<br>') + '</p>';
    }).join('');
  }

  // mailtoHref builds a mailto: URL that cannot be turned into a header or
  // query injection by a malformed config value. The server already
  // validates the address conservatively; this is the render-side belt:
  // percent-encode everything, then put "@" back so the href stays readable.
  // CR/LF -> %0D%0A, "?" -> %3F, "&" -> %26, quotes and spaces likewise, so
  // no extra mailto header (?subject=, &cc=) can be smuggled in.
  function mailtoHref(email) {
    return 'mailto:' + encodeURIComponent(String(email == null ? '' : email).trim()).replace(/%40/g, '@');
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
    // No fallbacks anywhere: the server does not publish the block unless
    // every required field is present, so reaching render() means they are.
    var controller = txt(cfg.controllerName);
    var rawEmail = String(cfg.contactEmail || '').trim();
    var email = txt(rawEmail);
    var emailHref = escapeHtml(mailtoHref(rawEmail));
    var contactInline = rawEmail ? '<a href="' + emailHref + '">' + email + '</a>' : email;

    var html =
      '<div class="privacy-page">' +
      '<h2>' + phIcon('lock') + ' Privacy Notice</h2>' +
      '<p class="text-muted">' +
      '<strong>Effective date:</strong> ' + txt(cfg.effectiveDate) + '<br>' +
      '<strong>Data controller:</strong> ' + controller + '<br>' +
      '<strong>Privacy contact:</strong> ' + contactInline +
      '</p>' +
      '<p>This CoreScope deployment provides status information and analysis for a ' +
      'community-operated MeshCore radio network. It receives packet and reception data ' +
      'from participating observer nodes and makes selected information available through ' +
      'this website and its API.</p>';

    if (txt(cfg.dpoName) || txt(cfg.dpoContact)) {
      html += section('user-circle', 'Data protection officer',
        '<p>' + [txt(cfg.dpoName), txt(cfg.dpoContact)].filter(Boolean).join('<br>') + '</p>');
    }

    // What data is processed. This list describes CoreScope's actual
    // pipeline: radio traffic AND the reception/observer metadata CoreScope
    // itself produces, plus derived analytics. Phrased as "may process,
    // depending on configuration" because several categories are opt-in.
    html += section('broadcast', 'What data this site processes',
      '<p>Depending on this deployment’s configuration, CoreScope may process:</p>' +
      '<ul>' +
      '<li><strong>Node information advertised over the radio network</strong>: public keys, node names, roles and advertised positions.</li>' +
      '<li><strong>Packet metadata</strong>: timestamps, packet types, routing paths and hop information.</li>' +
      '<li><strong>Reception metadata produced by observer nodes</strong>: which observer received a packet, and the associated SNR/RSSI measurements.</li>' +
      '<li><strong>Observer identity, status and operational metrics.</strong></li>' +
      '<li><strong>Derived information</strong> generated by this site: distances, routes, coverage estimates, node health and network statistics.</li>' +
      '<li><strong>Optional mobile client reception data</strong>, which may include the reception position reported by the client.</li>' +
      '<li><strong>Encrypted group or channel packets</strong>, stored as packet records.</li>' +
      '<li><strong>Message content from channels whose keys are known to this deployment</strong> — see below.</li>' +
      '</ul>' +
      '<p>A node name or advertised position may identify or reveal information about its ' +
      'operator. Do not include personal information in node names, positions or channel ' +
      'messages unless you intend it to be publicly visible.</p>');

    html += section('target', 'Purpose of processing', paras(cfg.purposesText));

    // Legal basis: the operator's structured choice plus their own wording.
    // The software never asserts a basis and never infers one from prose.
    var basisLabel = {
      consent: 'Consent',
      contract: 'Performance of a contract',
      legal_obligation: 'Legal obligation',
      vital_interests: 'Vital interests',
      public_task: 'Public task',
      legitimate_interests: 'Legitimate interests',
    }[String(cfg.legalBasisType || '').trim()] || txt(cfg.legalBasisType);
    var basisBody = '<p><strong>' + escapeHtml(basisLabel) + '</strong></p>' + paras(cfg.legalBasisText);
    if (String(cfg.legalBasisType || '').trim() === 'legitimate_interests') {
      basisBody += paras(cfg.legitimateInterestsText);
    }
    basisBody += '<p class="text-muted">The lawful basis and its description are stated by the operator of this deployment.</p>';
    html += section('scales', 'Legal basis', basisBody);

    html += section('arrow-down', 'Sources of the data', paras(cfg.dataSourcesText));
    html += section('users', 'Who can receive the data', paras(cfg.recipientsText));

    html += section('clock', 'Retention',
      paras(cfg.retentionText) +
      '<p class="text-muted">Different categories may have different retention rules. Moving a node ' +
      'to an inactive-node table, marking an observer inactive or hiding a node from ' +
      'dashboard and API views does not necessarily delete its historical packet or ' +
      'observation data.</p>');

    // Messages. Verified against the code: only group/channel payloads are
    // ever decrypted, and only with keys this deployment holds. Direct
    // message CONTENT is not decrypted -- but the surrounding metadata is
    // still processed. Stored ciphertext is retained, so a packet that is
    // unreadable today could be decoded later if a key becomes available.
    html += section('chats', 'Channel and direct messages',
      '<p>Messages on channels whose keys are available to this deployment may be decoded ' +
      'and published. Encrypted group or channel packets are stored as received, so a ' +
      'packet that cannot be read today may become readable later if a corresponding key ' +
      'becomes available to this deployment.</p>' +
      '<p>Direct-message content is not decrypted by CoreScope. Packet, route and reception ' +
      'metadata associated with direct messages — such as timestamps, packet type, routing ' +
      'path and which observers received it — may nevertheless be stored and displayed.</p>' +
      '<p>Avoid transmitting personal or sensitive information on shared channels.</p>');

    html += section('browser', 'Storage in your browser', paras(cfg.browserStorageText));
    html += section('file-text', 'Server and proxy logs', paras(cfg.serverLogsText));
    html += section('globe', 'External services', paras(cfg.thirdPartyServicesText));
    html += section('airplane', 'International transfers', paras(cfg.internationalTransfersText));

    // Self-service hiding: only offered when this deployment actually has
    // hide prefixes configured, and always described as a visibility filter
    // -- never as removal from the radio network or as deletion.
    var prefixes = Array.isArray(cfg.hiddenNamePrefixes) ? cfg.hiddenNamePrefixes.filter(function (x) {
      return typeof x === 'string' && x.trim() !== '';
    }) : [];
    var hiddenBody;
    if (prefixes.length) {
      var rendered = prefixes.map(function (x) {
        return '<span class="mono">' + escapeHtml(x) + '</span>';
      }).join(prefixes.length === 2 ? ' or ' : ', ');
      hiddenBody =
        '<p>This deployment hides nodes whose name begins with ' + rendered +
        ' from selected dashboard and API views.</p>';
    } else {
      hiddenBody =
        '<p>This deployment has no name-prefix hiding configured.</p>';
    }
    hiddenBody +=
      '<p class="text-muted">Prefix hiding is a visibility filter on this site’s dashboard and API only. ' +
      'It does not remove a node from the radio network — other listeners still receive its ' +
      'transmissions — and it does not by itself delete stored packets, observations or ' +
      'derived analytics.</p>';
    html += section('eye-slash', 'Hidden nodes', hiddenBody);

    html += section('cpu', 'Automated decision-making', paras(cfg.automatedDecisionMakingText));

    html += section('info', 'Changes to this notice',
      '<p>This notice may be updated when the deployment, its configuration or its ' +
      'data-processing practices change. The effective date shown at the top identifies the ' +
      'current version.</p>');

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
        // Server-side gate already omits the section unless enabled AND
        // complete; the explicit checks are belt-and-braces for stale
        // caches. controllerName has no fallback, so its absence alone is
        // enough to withhold the notice.
        if (!cfg || cfg.enabled === false) { renderDisabled(container); return; }
        if (!String(cfg.controllerName || '').trim()) { renderDisabled(container); return; }
        render(container, cfg);
      }).catch(function () { renderDisabled(container); });
    },
    destroy: function () {}
  });
})();
