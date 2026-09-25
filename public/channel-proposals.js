/* Shared channel proposals — suggest public hashtag channels for everyone,
 * and let an administrator (the existing apiKey) approve or reject them.
 *
 * Server contract (cmd/server/channel_proposals.go):
 *   GET  /api/channel-proposals/config                 → {enabled}
 *   POST /api/channel-proposals {name}                 → 202 {requestId}
 *   GET  /api/channel-proposals/requests/{requestId}   → {status, proposal, error}
 *   GET  /api/admin/channel-proposals[?status=]        → {proposals, enabled}   (X-API-Key)
 *   POST /api/admin/channel-proposals/{id}/approve|reject → 202 {requestId}   (X-API-Key)
 *   POST /api/admin/channel-proposals/{id}/revoke      → 202 {requestId} or 409 (not approved, nothing queued) (X-API-Key)
 *   GET  /api/channels → approvedChannels: [{name, hash}]
 *
 * The admin key lives only in this module's memory (never localStorage,
 * sessionStorage or the URL) until "Lock" or a page reload.
 *
 * Pure helpers (normalizeName, mergeApprovedChannels, pollDelay,
 * createPoller) have no DOM or network access so test-channel-proposals.js
 * can run them in Node.
 */
(function (root) {
  'use strict';

  // Firmware limit: ChannelDetails.name is char[32] including the NUL, so a
  // name is at most 31 bytes of UTF-8 including the '#'. Mirrors
  // internal/channelregistry.NormalizeName; the server stays authoritative.
  var MAX_NAME_BYTES = 31;
  var CONTROL_RE = /[\u0000-\u001F\u007F-\u009F\u061C\u200E\u200F\u202A-\u202E\u2066-\u2069]/;
  var LEADING_SPACE_RE = /^\s/;

  function utf8Length(s) {
    // encodeURIComponent throws on lone surrogates, which are not valid UTF-8.
    return encodeURIComponent(s).replace(/%[0-9A-F]{2}/gi, 'x').length;
  }

  function normalizeName(raw) {
    var s = String(raw == null ? '' : raw).trim();
    if (s.charAt(0) !== '#') s = '#' + s;
    var body = s.slice(1);
    if (!body) return { error: 'Enter a channel name.' };
    var bytes;
    try { bytes = utf8Length(s); } catch (e) { return { error: 'The channel name contains invalid characters.' }; }
    if (bytes > MAX_NAME_BYTES) return { error: 'Channel names can be at most ' + MAX_NAME_BYTES + ' bytes including the #.' };
    if (CONTROL_RE.test(s)) return { error: 'The channel name contains control characters.' };
    if (LEADING_SPACE_RE.test(body)) return { error: 'The channel name must not start with a space.' };
    return { name: s };
  }

  // Merge the server's approved shared channels into the channel list in
  // O(n + m). Inputs are never mutated: channels that are also shared are
  // shallow-copied with `shared: true`, and approved channels without traffic
  // become new zero-count entries.
  function mergeApprovedChannels(channels, approved) {
    var list = Array.isArray(channels) ? channels : [];
    if (!Array.isArray(approved) || approved.length === 0) return list.slice();
    var byHash = new Map();
    for (var i = 0; i < approved.length; i++) {
      var a = approved[i];
      if (a && typeof a.hash === 'string' && a.hash && !byHash.has(a.hash)) byHash.set(a.hash, a);
    }
    var matched = new Set();
    var out = new Array(list.length);
    for (var j = 0; j < list.length; j++) {
      var ch = list[j];
      var hit = ch ? (byHash.get(ch.hash) || (ch.name ? byHash.get(ch.name) : undefined)) : undefined;
      if (hit) {
        out[j] = Object.assign({}, ch, { shared: true });
        matched.add(hit.hash);
      } else {
        out[j] = ch;
      }
    }
    byHash.forEach(function (a, hash) {
      if (matched.has(hash)) return;
      out.push({
        hash: hash,
        name: typeof a.name === 'string' && a.name ? a.name : hash,
        messageCount: 0,
        lastActivity: null,
        lastActivityMs: 0,
        lastMessage: null,
        lastSender: null,
        shared: true
      });
    });
    return out;
  }

  // Bounded backoff for polling one request: 1s, 1.5s, 2.25s … capped at 10s.
  var MAX_POLL_ATTEMPTS = 20; // ≈ 2.5 minutes in total
  function pollDelay(attempt) {
    return Math.min(Math.round(1000 * Math.pow(1.5, Math.max(0, attempt))), 10000);
  }

  // One poller per slot: starting a new request cancels the previous one.
  // opts.fetchStatus(id) → Promise<status>, opts.onUpdate(status),
  // opts.onGiveUp(); timers are injectable for tests.
  function createPoller(opts) {
    var setT = opts.setTimeout || root.setTimeout.bind(root);
    var clearT = opts.clearTimeout || root.clearTimeout.bind(root);
    var maxAttempts = opts.maxAttempts || MAX_POLL_ATTEMPTS;
    var timer = null;
    var generation = 0;

    function cancel() {
      generation++;
      if (timer !== null) { clearT(timer); timer = null; }
    }

    function start(requestId) {
      cancel();
      var gen = generation;
      var attempt = 0;
      function schedule() {
        if (attempt >= maxAttempts) {
          timer = null;
          if (opts.onGiveUp) opts.onGiveUp();
          return;
        }
        timer = setT(tick, pollDelay(attempt));
        attempt++;
      }
      function tick() {
        timer = null;
        Promise.resolve().then(function () { return opts.fetchStatus(requestId); }).then(function (st) {
          if (gen !== generation) return;
          opts.onUpdate(st);
          if (st && st.status === 'queued') schedule();
        }, function (err) {
          if (gen !== generation) return;
          if (err && err.status === 404) {
            opts.onUpdate({ status: 'error', error: 'The request expired. Please try again.' });
            return;
          }
          schedule(); // transient network/server error: keep backing off
        });
      }
      schedule();
    }

    return { start: start, cancel: cancel, isActive: function () { return timer !== null; } };
  }

  // ── Network ────────────────────────────────────────────────────────────
  var adminKey = null; // memory only

  function request(method, path, opts) {
    opts = opts || {};
    var headers = { 'Accept': 'application/json' };
    if (opts.body !== undefined) headers['Content-Type'] = 'application/json';
    if (opts.admin && adminKey) headers['X-API-Key'] = adminKey;
    return fetch('/api' + path, {
      method: method,
      headers: headers,
      body: opts.body !== undefined ? JSON.stringify(opts.body) : undefined,
      cache: 'no-store',
      signal: opts.signal
    }).then(function (res) {
      return res.text().then(function (text) {
        var data = null;
        try { data = text ? JSON.parse(text) : null; } catch (e) { data = null; }
        if (!res.ok) {
          var err = new Error((data && data.error) || ('Request failed (' + res.status + ')'));
          err.status = res.status;
          throw err;
        }
        return data;
      });
    });
  }

  function statusOf(requestId) {
    return request('GET', '/channel-proposals/requests/' + encodeURIComponent(requestId));
  }

  // ── Shared state for the mounted page ─────────────────────────────────
  var state = null; // { root, onApproved, suggestPoller, adminPoller, cleanups: [] }

  function esc(s) {
    return String(s == null ? '' : s)
      .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
  }

  function fmtTime(ms) {
    if (!ms) return '';
    try { return new Date(ms).toLocaleString(); } catch (e) { return ''; }
  }

  function listen(el, type, fn) {
    el.addEventListener(type, fn);
    if (state) state.cleanups.push(function () { el.removeEventListener(type, fn); });
  }

  // ── Suggest form (a section of the Add Channel modal) ─────────────────
  function suggestMessage(st, fallbackName) {
    var p = st && st.proposal;
    var name = (p && p.name) || fallbackName || 'The channel';
    switch (st && st.status) {
      case 'queued': return { text: 'Sending your suggestion…', kind: 'info' };
      case 'pending': return { text: 'Thanks! ' + name + ' is waiting for an administrator to review it.', kind: 'success' };
      case 'approved': return { text: name + ' is already shared with everyone.', kind: 'success' };
      case 'rejected': return { text: name + ' was not accepted as a shared channel.', kind: 'warn' };
      default: return { text: (st && st.error) || 'The suggestion could not be processed.', kind: 'error' };
    }
  }

  function renderSuggestSection(section) {
    section.innerHTML =
      '<h4 id="chSecSuggestTitle" class="ch-modal-section-title">Suggest a Shared Channel</h4>' +
      '<p class="ch-modal-section-hint">Propose a public hashtag channel for everyone on this site. ' +
      'An administrator reviews each suggestion before it is shared. Only public hashtag channels can be suggested — never a private key.</p>' +
      '<form id="chSuggestForm" class="ch-modal-row ch-hashtag-row" novalidate>' +
        '<span class="ch-hashtag-prefix" aria-hidden="true">#</span>' +
        '<input type="text" id="chSuggestName" class="ch-modal-input" placeholder="MeshCore" maxlength="64"' +
        ' aria-label="Hashtag channel to suggest (without #)" aria-describedby="chSuggestHint chSuggestStatus"' +
        ' spellcheck="false" autocomplete="off">' +
        '<button type="submit" id="chSuggestBtn" class="btn-primary">Suggest</button>' +
      '</form>' +
      '<div id="chSuggestHint" class="ch-modal-warn">Case-sensitive, up to ' + MAX_NAME_BYTES + ' bytes including the #.</div>' +
      '<div id="chSuggestStatus" class="ch-proposal-status" role="status" aria-live="polite"></div>' +
      '<a href="#/channels?view=proposals" class="ch-proposal-admin-link">Review suggestions (admin)</a>';
    section.hidden = false;

    var form = section.querySelector('#chSuggestForm');
    var input = section.querySelector('#chSuggestName');
    var btn = section.querySelector('#chSuggestBtn');
    var status = section.querySelector('#chSuggestStatus');
    function show(msg) {
      status.textContent = msg.text;
      status.setAttribute('data-kind', msg.kind);
    }
    listen(form, 'submit', function (e) {
      e.preventDefault();
      var n = normalizeName(input.value);
      if (n.error) { show({ text: n.error, kind: 'error' }); input.focus(); return; }
      btn.disabled = true;
      show({ text: 'Sending your suggestion…', kind: 'info' });
      request('POST', '/channel-proposals', { body: { name: n.name } }).then(function (res) {
        if (!state) return;
        state.suggestPoller.start(res.requestId);
        state.suggestName = n.name;
        input.value = '';
      }, function (err) {
        if (err.status === 429) show({ text: 'Too many suggestions right now. Please try again later.', kind: 'error' });
        else if (err.status === 403) show({ text: 'Channel suggestions are closed.', kind: 'error' });
        else show({ text: err.message, kind: 'error' });
      }).then(function () { btn.disabled = false; });
    });
    state.suggestShow = show;
  }

  // ── Admin dialog (#/channels?view=proposals) ───────────────────────────
  var FILTERS = [['pending', 'Pending'], ['approved', 'Approved'], ['rejected', 'Rejected'], ['revoked', 'Revoked']];

  function adminEl() { return document.getElementById('chProposalsAdmin'); }
  function confirmEl() { return document.getElementById('chProposalsConfirm'); }

  function focusables(container) {
    return Array.prototype.filter.call(
      container.querySelectorAll('button, [href], input, [tabindex]:not([tabindex="-1"])'),
      function (el) { return !el.disabled && el.offsetParent !== null; });
  }

  function openAdmin() {
    if (!state || adminEl()) return;
    state.adminTrigger = document.activeElement;
    state.adminFilter = state.adminFilter || 'pending';
    var overlay = document.createElement('div');
    overlay.id = 'chProposalsAdmin';
    overlay.className = 'modal-overlay ch-modal-overlay ch-proposals-overlay';
    overlay.setAttribute('role', 'dialog');
    overlay.setAttribute('aria-modal', 'true');
    overlay.setAttribute('aria-labelledby', 'chProposalsTitle');
    overlay.innerHTML =
      '<div class="modal ch-modal ch-proposals-modal" role="document">' +
        '<button type="button" class="modal-close ch-modal-close" data-proposals-action="close" aria-label="Close">' +
          '<svg class="ph-icon" aria-hidden="true"><use href="/icons/phosphor-sprite.svg#ph-x"/></svg></button>' +
        '<h3 id="chProposalsTitle">Channel Suggestions</h3>' +
        '<div id="chProposalsBody"></div>' +
        '<div id="chProposalsStatus" class="ch-proposal-status" role="status" aria-live="polite"></div>' +
      '</div>';
    state.root.appendChild(overlay);
    overlay.addEventListener('click', onAdminClick);
    overlay.addEventListener('submit', onAdminSubmit);
    document.addEventListener('keydown', onAdminKeydown);
    renderAdminBody();
  }

  function closeAdmin(opts) {
    var el = adminEl();
    if (!el) return;
    closeConfirm({ skipFocusReturn: true }); // never leave an orphaned confirm layer behind
    if (state) state.adminPoller.cancel();
    document.removeEventListener('keydown', onAdminKeydown);
    el.remove();
    // Leave the deep link without re-running the page's init.
    if (!(opts && opts.keepHash) && /[?&]view=proposals\b/.test(location.hash)) {
      history.replaceState(null, '', '#/channels');
    }
    var back = state && state.adminTrigger;
    if (back && back !== document.body && document.contains(back) && typeof back.focus === 'function') back.focus();
    else {
      var addBtn = document.getElementById('chAddChannelBtn');
      if (addBtn) addBtn.focus();
    }
  }

  function adminStatus(text, kind) {
    var s = document.getElementById('chProposalsStatus');
    if (!s) return;
    s.textContent = text || '';
    s.setAttribute('data-kind', kind || 'info');
  }

  function renderAdminBody() {
    var body = document.getElementById('chProposalsBody');
    if (!body) return;
    if (!adminKey) {
      body.innerHTML =
        '<form id="chProposalsKeyForm" class="ch-proposals-key" novalidate>' +
          '<label for="chProposalsKey" class="ch-proposals-label">Admin API key</label>' +
          '<div class="ch-modal-row">' +
            '<input type="password" id="chProposalsKey" class="ch-modal-input" autocomplete="off" spellcheck="false"' +
            ' aria-describedby="chProposalsKeyHint">' +
            '<button type="submit" class="btn-primary">Unlock</button>' +
          '</div>' +
          '<div id="chProposalsKeyHint" class="ch-modal-warn">The key is kept in this tab\'s memory only and is forgotten on reload.</div>' +
        '</form>';
      var k = document.getElementById('chProposalsKey');
      if (k) k.focus();
      return;
    }
    body.innerHTML =
      '<div class="ch-proposals-toolbar">' +
        '<div class="ch-proposals-filters" role="group" aria-label="Show suggestions">' +
          FILTERS.map(function (f) {
            var on = state.adminFilter === f[0];
            return '<button type="button" class="ch-modal-btn-secondary ch-proposals-filter" data-proposals-filter="' + f[0] + '"' +
              ' aria-pressed="' + (on ? 'true' : 'false') + '">' + f[1] + '</button>';
          }).join('') +
        '</div>' +
        '<button type="button" class="ch-modal-btn-secondary" data-proposals-action="refresh">Refresh</button>' +
        '<button type="button" class="ch-modal-btn-secondary" data-proposals-action="lock">Lock</button>' +
      '</div>' +
      '<ul id="chProposalsList" class="ch-proposals-list" aria-busy="true"></ul>';
    var active = body.querySelector('[aria-pressed="true"]');
    if (active) active.focus();
    loadAdminList();
  }

  function loadAdminList() {
    var list = document.getElementById('chProposalsList');
    if (!list) return;
    list.setAttribute('aria-busy', 'true');
    var filter = state.adminFilter;
    request('GET', '/admin/channel-proposals?status=' + encodeURIComponent(filter), { admin: true }).then(function (data) {
      if (!state || state.adminFilter !== filter || !document.getElementById('chProposalsList')) return;
      var items = (data && Array.isArray(data.proposals)) ? data.proposals : [];
      list.innerHTML = items.length ? items.map(renderAdminRow).join('') :
        '<li class="ch-proposals-empty">No ' + esc(filter) + ' suggestions.</li>';
      list.setAttribute('aria-busy', 'false');
      if (data && data.enabled === false && filter === 'pending') {
        adminStatus('Public suggestions are currently closed; existing ones can still be reviewed.', 'info');
      }
    }, onAdminError);
  }

  function renderAdminRow(p) {
    var when = p.status === 'pending'
      ? 'Suggested ' + esc(fmtTime(p.createdAt))
      : 'Reviewed ' + esc(fmtTime(p.reviewedAt || p.createdAt));
    var actions;
    if (p.status === 'pending') {
      actions = '<div class="ch-proposals-actions">' +
          '<button type="button" class="btn-primary" data-proposals-decide="approve" data-proposal-id="' + esc(p.id) + '"' +
          ' aria-label="Approve ' + esc(p.name) + '">Approve</button>' +
          '<button type="button" class="ch-modal-btn-secondary" data-proposals-decide="reject" data-proposal-id="' + esc(p.id) + '"' +
          ' aria-label="Reject ' + esc(p.name) + '">Reject</button>' +
        '</div>';
    } else if (p.status === 'approved') {
      // Approved rows additionally get a Remove (revoke) action, guarded by
      // a confirmation step (see openConfirm) before anything is sent.
      actions = '<div class="ch-proposals-actions">' +
          '<span class="ch-proposals-state" data-state="approved">approved</span>' +
          '<button type="button" class="ch-modal-btn-secondary ch-proposals-remove" data-proposals-decide="revoke"' +
          ' data-proposal-id="' + esc(p.id) + '" data-proposal-name="' + esc(p.name) + '"' +
          ' aria-label="Remove ' + esc(p.name) + '">Remove</button>' +
        '</div>';
    } else {
      actions = '<span class="ch-proposals-state" data-state="' + esc(p.status) + '">' + esc(p.status) + '</span>';
    }
    return '<li class="ch-proposals-item" data-proposal-id="' + esc(p.id) + '">' +
      '<div class="ch-proposals-main"><span class="ch-proposals-name">' + esc(p.name) + '</span>' +
      '<span class="ch-proposals-meta">' + when + '</span></div>' + actions + '</li>';
  }

  // ── Remove (revoke) confirmation layer, nested inside the admin dialog ──
  // Reuses the admin dialog's own overlay/modal classes and its single
  // document keydown listener (onAdminKeydown), which checks confirmEl()
  // first so Escape/Tab-trapping apply to whichever layer is on top —
  // never two independent keydown listeners fighting over the same keys.
  function openConfirm(id, name) {
    if (!state || !adminEl() || confirmEl()) return;
    state.confirmTrigger = document.activeElement;
    var dlg = document.createElement('div');
    dlg.id = 'chProposalsConfirm';
    dlg.className = 'modal-overlay ch-modal-overlay ch-proposals-confirm-overlay';
    dlg.setAttribute('role', 'alertdialog');
    dlg.setAttribute('aria-modal', 'true');
    dlg.setAttribute('aria-labelledby', 'chProposalsConfirmTitle');
    dlg.innerHTML =
      '<div class="modal ch-modal ch-proposals-confirm" role="document">' +
        '<h4 id="chProposalsConfirmTitle">Remove ' + esc(name) + '?</h4>' +
        '<p class="ch-modal-section-hint">It will stop being shared with everyone.</p>' +
        '<div class="ch-modal-row ch-proposals-confirm-actions">' +
          '<button type="button" class="ch-modal-btn-secondary" data-proposals-confirm-action="cancel">Cancel</button>' +
          '<button type="button" class="btn-primary" data-proposals-confirm-action="confirm"' +
          ' data-proposal-id="' + esc(id) + '" data-proposal-name="' + esc(name) + '">Remove</button>' +
        '</div>' +
      '</div>';
    adminEl().appendChild(dlg);
    var confirmBtn = dlg.querySelector('[data-proposals-confirm-action="confirm"]');
    if (confirmBtn) confirmBtn.focus();
  }

  function closeConfirm(opts) {
    var el = confirmEl();
    if (!el) return;
    el.remove();
    if (opts && opts.skipFocusReturn) return;
    var back = state && state.confirmTrigger;
    if (back && document.contains(back) && typeof back.focus === 'function') back.focus();
  }

  function onAdminError(err) {
    if (err && (err.status === 401 || err.status === 403)) {
      adminKey = null;
      renderAdminBody();
      adminStatus(err.status === 401 ? 'The API key was not accepted.' : err.message, 'error');
      return;
    }
    adminStatus((err && err.message) || 'Something went wrong.', 'error');
  }

  function decide(id, op, btn) {
    var row = btn.closest('.ch-proposals-item');
    var name = row ? (row.querySelector('.ch-proposals-name') || {}).textContent : '';
    if (row) Array.prototype.forEach.call(row.querySelectorAll('button'), function (b) { b.disabled = true; });
    var verb = op === 'approve' ? 'Approving ' : op === 'revoke' ? 'Removing ' : 'Rejecting ';
    adminStatus(verb + name + '…', 'info');
    request('POST', '/admin/channel-proposals/' + encodeURIComponent(id) + '/' + op, { admin: true }).then(function (res) {
      if (!state) return;
      state.adminPending = { op: op, name: name };
      state.adminPoller.start(res.requestId);
    }, function (err) {
      if (row) Array.prototype.forEach.call(row.querySelectorAll('button'), function (b) { b.disabled = false; });
      onAdminError(err);
    });
  }

  function onAdminDecision(st) {
    if (!state) return;
    if (st.status === 'queued') return;
    var name = (st.proposal && st.proposal.name) || (state.adminPending && state.adminPending.name) || 'The channel';
    if (st.status === 'approved') {
      adminStatus(name + ' is now shared with everyone.', 'success');
      if (typeof state.onApproved === 'function') state.onApproved(name);
    } else if (st.status === 'rejected') {
      adminStatus(name + ' was rejected.', 'success');
    } else if (st.status === 'revoked') {
      adminStatus(name + ' was removed and is no longer shared.', 'success');
    } else {
      adminStatus(st.error || 'The decision could not be applied.', 'error');
    }
    loadAdminList();
  }

  function onAdminClick(e) {
    var overlay = adminEl();
    var t = e.target;
    // The confirm layer sits on top when open: its own backdrop click and
    // its Cancel/Confirm buttons are handled here first, before anything
    // that would act on the admin dialog underneath it.
    if (confirmEl()) {
      if (t === confirmEl()) { closeConfirm(); return; }
      var ca = t.closest && t.closest('[data-proposals-confirm-action]');
      if (ca) {
        var action = ca.getAttribute('data-proposals-confirm-action');
        if (action === 'cancel') { closeConfirm(); return; }
        if (action === 'confirm') {
          var confirmId = ca.getAttribute('data-proposal-id');
          var trigger = state && state.confirmTrigger;
          closeConfirm({ skipFocusReturn: true });
          if (trigger) decide(confirmId, 'revoke', trigger);
          return;
        }
      }
      return; // swallow everything else while the confirm layer is open
    }
    if (t === overlay || (t.closest && t.closest('[data-proposals-action="close"]'))) {
      e.preventDefault();
      closeAdmin();
      return;
    }
    var f = t.closest && t.closest('[data-proposals-filter]');
    if (f) {
      state.adminFilter = f.getAttribute('data-proposals-filter');
      Array.prototype.forEach.call(overlay.querySelectorAll('[data-proposals-filter]'), function (b) {
        b.setAttribute('aria-pressed', b === f ? 'true' : 'false');
      });
      adminStatus('');
      loadAdminList();
      return;
    }
    var a = t.closest && t.closest('[data-proposals-action]');
    if (a && a.getAttribute('data-proposals-action') === 'refresh') { loadAdminList(); return; }
    if (a && a.getAttribute('data-proposals-action') === 'lock') {
      adminKey = null;
      state.adminPoller.cancel();
      renderAdminBody();
      adminStatus('Locked. The key was forgotten.', 'info');
      return;
    }
    var d = t.closest && t.closest('[data-proposals-decide]');
    if (d && !d.disabled) {
      var op = d.getAttribute('data-proposals-decide');
      if (op === 'revoke') {
        openConfirm(d.getAttribute('data-proposal-id'), d.getAttribute('data-proposal-name') || '');
        return;
      }
      decide(d.getAttribute('data-proposal-id'), op, d);
    }
  }

  function onAdminSubmit(e) {
    if (e.target.id !== 'chProposalsKeyForm') return;
    e.preventDefault();
    var input = document.getElementById('chProposalsKey');
    var key = input ? input.value.trim() : '';
    if (!key) { adminStatus('Enter the admin API key.', 'error'); return; }
    adminKey = key;
    if (input) input.value = '';
    adminStatus('');
    renderAdminBody();
  }

  // Single document-level keydown listener for the whole admin dialog,
  // including the confirm layer nested inside it: whichever is currently on
  // top handles Escape and Tab-trapping, so there is never a second,
  // independently-stacked listener fighting this one over the same keys.
  function onAdminKeydown(e) {
    var overlay = adminEl();
    if (!overlay) return;
    var layer = confirmEl() || overlay;
    if (e.key === 'Escape') {
      e.preventDefault();
      e.stopPropagation();
      if (layer === overlay) closeAdmin(); else closeConfirm();
      return;
    }
    if (e.key !== 'Tab') return;
    var f = focusables(layer);
    if (!f.length) return;
    var first = f[0];
    var last = f[f.length - 1];
    if (!layer.contains(document.activeElement)) { e.preventDefault(); first.focus(); return; }
    if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
    else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
  }

  // ── Lifecycle ─────────────────────────────────────────────────────────
  // mount({root, suggestSection, view, onApproved}) is called from the
  // Channels page init; unmount() from its destroy, which also stops polling.
  function mount(opts) {
    unmount();
    state = {
      root: opts.root,
      onApproved: opts.onApproved,
      cleanups: [],
      adminFilter: 'pending'
    };
    state.suggestPoller = createPoller({
      fetchStatus: statusOf,
      onUpdate: function (st) {
        if (state && state.suggestShow) state.suggestShow(suggestMessage(st, state.suggestName));
      },
      onGiveUp: function () {
        if (state && state.suggestShow) state.suggestShow({ text: 'Your suggestion is still being processed. Check back later.', kind: 'info' });
      }
    });
    state.adminPoller = createPoller({
      fetchStatus: statusOf,
      onUpdate: onAdminDecision,
      onGiveUp: function () { adminStatus('The decision is still being processed. Refresh in a moment.', 'info'); }
    });
    var section = opts.suggestSection;
    if (section) {
      var mine = state;
      request('GET', '/channel-proposals/config').then(function (cfg) {
        if (state !== mine || !cfg || cfg.enabled !== true) return;
        renderSuggestSection(section);
      }, function () { /* feature unavailable: keep the section hidden */ });
    }
    if (opts.view === 'proposals') openAdmin();
  }

  function unmount() {
    if (!state) return;
    state.suggestPoller.cancel();
    state.adminPoller.cancel();
    var el = adminEl();
    if (el) el.remove();
    document.removeEventListener('keydown', onAdminKeydown);
    state.cleanups.forEach(function (fn) { fn(); });
    state = null;
  }

  root.ChannelProposals = {
    MAX_NAME_BYTES: MAX_NAME_BYTES,
    MAX_POLL_ATTEMPTS: MAX_POLL_ATTEMPTS,
    FILTERS: FILTERS,
    normalizeName: normalizeName,
    mergeApprovedChannels: mergeApprovedChannels,
    pollDelay: pollDelay,
    createPoller: createPoller,
    renderAdminRow: renderAdminRow,
    mount: mount,
    unmount: unmount,
    openAdmin: openAdmin,
    closeAdmin: closeAdmin
  };
})(typeof window !== 'undefined' ? window : this);
