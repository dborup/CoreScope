/* === CoreScope — account.js (#1018 M1) === */
'use strict';

// Owner account: sign in with your companion, and the synced My nodes list.
//
// Route #/account. Signed out it is the sign-in card: choose the companion
// (search or paste its key), get a code, put it in the companion's name and
// send a FLOOD advert; an observer hears it, the ingestor proves it, and the
// page redeems the proven sign-in for an HttpOnly session cookie the page
// itself never sees. Signed in it is the account: who is signed in, the
// one-time offer to add this browser's saved nodes, the list, the display
// name, sign out, sign out everywhere and delete.
//
// The list itself lives in MyNodes (my-nodes.js). boot() runs once per page
// load: with accounts on it reads GET /api/account and hands the list to
// MyNodes, which every other page reads; MyNodes' writer sends each change to
// POST /api/account/my-nodes.
//
// The pending sign-in (request id, code, redeem secret) is kept in
// sessionStorage so a reload resumes it; never in the URL, never in
// localStorage. The pure helpers at the top have no DOM or network access
// and are tested in Node.

(function (root) {
  var PENDING_KEY = 'meshcore-account-signin';
  // After this long without a proof the page suggests sending again.
  var HINT_AFTER_MS = 2 * 60 * 1000;
  var REDEEM_WINDOW_MS = 10 * 60 * 1000;

  function esc(s) { return String(s == null ? '' : s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;').replace(/'/g, '&#39;'); }

  // ── Pure helpers ──────────────────────────────────────────────────────

  function utf8Bytes(s) {
    try { return encodeURIComponent(String(s)).replace(/%[0-9A-F]{2}/gi, 'x').length; } catch (e) { return Infinity; }
  }
  // utf8Prefix is the longest prefix of s within n bytes that ends on a
  // character boundary: what the firmware puts on air
  // (AdvertDataBuilder::encodeTo, mesh::validUtf8PrefixLength).
  function utf8Prefix(s, n) {
    s = String(s);
    var out = '';
    var used = 0;
    for (var i = 0; i < s.length; i++) {
      var c = s.charAt(i);
      var code = s.charCodeAt(i);
      if (code >= 0xd800 && code <= 0xdbff && i + 1 < s.length) { c = s.substr(i, 2); i++; }
      var b = utf8Bytes(c);
      if (used + b > n) break;
      out += c;
      used += b;
    }
    return out;
  }

  // nameFits checks the name the owner will set: the code must be a whole
  // space-separated word inside the bytes that go on air.
  function nameFits(name, code, budget) {
    name = String(name == null ? '' : name);
    var r = { bytes: utf8Bytes(name), budget: budget, ok: false, reason: '' };
    if (name.split(' ').indexOf(code) < 0) { r.reason = 'missing'; return r; }
    if (utf8Prefix(name, budget).split(' ').indexOf(code) < 0) { r.reason = 'cut'; return r; }
    r.ok = true;
    return r;
  }
  function nameFitsText(r) {
    if (r.reason === 'missing') return 'The code must be a separate word in the name.';
    if (r.reason === 'cut') return r.bytes + ' / ' + r.budget + ' bytes: the code is past what goes on air. Shorten the name.';
    return r.bytes + ' / ' + r.budget + ' bytes: the code fits.';
  }

  // statusLine is the owner-facing line for a sign-in status; done stops
  // polling, redeem says the session can be taken now.
  function statusLine(st) {
    var s = st && st.status;
    switch (s) {
      case 'queued': return { text: 'Registering your sign-in…', kind: 'info', done: false };
      case 'pending': return { text: 'Waiting for a flood advert from your companion with the code in its name…', kind: 'info', done: false };
      case 'proven': return { text: 'An observer heard your companion. Signing you in…', kind: 'success', done: true, redeem: true };
      case 'expired': return { text: 'The code expired before an observer heard it. Ask for a new code.', kind: 'warn', done: true };
      case 'error': return { text: (st.error ? st.error : 'The sign-in could not be started.'), kind: 'error', done: true };
      case 'gone': return { text: 'This sign-in is no longer known. Start again.', kind: 'warn', done: true };
      default: return { text: 'Checking…', kind: 'info', done: false };
    }
  }

  // loginError is the text for a refused POST /api/account/login.
  function loginError(err) {
    var st = err && err.status;
    if (st === 404) return 'No companion with that key has been heard here. Send a flood advert from it first, then try again.';
    if (st === 429) return 'Too many sign-ins right now. Try again in a few minutes.';
    if (st === 503) return 'Sign-in is not available right now. Try again in a few minutes.';
    if (st === 403) return 'Sign-in is switched off on this site.';
    return 'The sign-in could not be started.';
  }

  // normalizeKey accepts a pasted 64-hex public key, any case, with
  // surrounding space; anything else is ''.
  function normalizeKey(raw) {
    var s = String(raw == null ? '' : raw).trim().toLowerCase();
    return /^[0-9a-f]{64}$/.test(s) ? s : '';
  }

  // companions keeps the search results that may sign in.
  function companions(nodes) {
    return (Array.isArray(nodes) ? nodes : []).filter(function (n) {
      return n && typeof n.public_key === 'string' && String(n.role || '').toLowerCase() === 'companion';
    });
  }

  // toServerNode turns a MyNodes entry into the server's shape (addedAt in
  // Unix ms; the browser list stores an ISO date).
  function toServerNode(n) {
    var at = typeof n.addedAt === 'number' ? n.addedAt : Date.parse(n.addedAt || '');
    var out = { pubkey: String(n.pubkey).toLowerCase() };
    if (n.name) out.name = String(n.name).slice(0, 64);
    if (isFinite(at) && at > 0) out.addedAt = Math.floor(at);
    return out;
  }
  // fromServerNode is the reverse, for MyNodes.
  function fromServerNode(n) {
    return { pubkey: n.pubkey, name: n.name || '', addedAt: n.addedAt ? new Date(n.addedAt).toISOString() : null };
  }

  // pendingUsable: a stored sign-in can still be polled or redeemed.
  function pendingUsable(p, nowMs) {
    return !!(p && typeof p.requestId === 'string' && typeof p.redeemSecret === 'string' && p.expiresAt + REDEEM_WINDOW_MS > nowMs);
  }

  // ── Network ───────────────────────────────────────────────────────────

  function call(method, path, body) {
    var init = { method: method, credentials: 'same-origin', headers: {} };
    if (body !== undefined) {
      init.headers['Content-Type'] = 'application/json';
      init.body = JSON.stringify(body);
    }
    return fetch('/api' + path, init).then(function (r) {
      if (r.status === 204) return null;
      return r.json().catch(function () { return {}; }).then(function (b) {
        if (r.ok) return b;
        var e = new Error((b && b.error) || ('HTTP ' + r.status));
        e.status = r.status;
        throw e;
      });
    });
  }

  var state = { config: null, account: null, boot: null };

  // boot reads the config and, with accounts on, the account; it resolves
  // once MyNodes holds the right list. Once per page load.
  function boot() {
    if (state.boot) return state.boot;
    state.boot = call('GET', '/account/config').then(function (cfg) {
      state.config = cfg || {};
      if (!state.config.enabled) { signedOut(); return null; }
      return refreshAccount();
    }, function () {
      state.config = {};
      return null; // offline or an older server: keep what MyNodes has
    });
    return state.boot;
  }

  function refreshAccount() {
    return call('GET', '/account').then(function (acc) {
      state.account = acc;
      MyNodes.setAccount(acc.identityPrefix, (acc.myNodes || []).map(fromServerNode), acc.limits && acc.limits.maxMyNodes);
      MyNodes.setWriter(writer);
      return acc;
    }, function (err) {
      if (err && err.status === 401) signedOut();
      return null;
    });
  }

  function signedOut() {
    state.account = null;
    MyNodes.setWriter(null);
    MyNodes.clearAccount();
  }

  // writer sends a MyNodes change to the account; a refused session falls
  // back to the browser's list.
  function writeFailed(err) {
    if (err && err.status === 401) signedOut();
  }
  var writer = {
    add: function (entry) { call('POST', '/account/my-nodes', { op: 'add', nodes: [toServerNode(entry)] }).catch(writeFailed); },
    remove: function (pubkey) { call('POST', '/account/my-nodes', { op: 'remove', pubkey: String(pubkey).toLowerCase() }).catch(writeFailed); }
  };

  function readPending() {
    try { return JSON.parse(root.sessionStorage.getItem(PENDING_KEY)); } catch (e) { return null; }
  }
  function writePending(p) {
    try {
      if (p) root.sessionStorage.setItem(PENDING_KEY, JSON.stringify(p));
      else root.sessionStorage.removeItem(PENDING_KEY);
    } catch (e) { /* private mode */ }
  }

  // ── Home entry ────────────────────────────────────────────────────────

  // mountEntry fills el with the Home hero's account line once boot knows:
  // nothing with accounts off, a sign-in link signed out, who is signed in.
  function mountEntry(el) {
    if (!el) return;
    boot().then(function () {
      if (!el.isConnected || !state.config || !state.config.enabled) return;
      if (state.account) {
        var who = state.account.displayName || state.account.identityName || 'your companion';
        el.innerHTML = '<a class="home-account-link" href="#/account" data-testid="home-account">' +
          '<svg class="ph-icon" aria-hidden="true"><use href="/icons/phosphor-sprite.svg#ph-key"/></svg> ' + esc('Signed in as ' + who) + '</a>';
      } else {
        el.innerHTML = '<a class="home-account-link" href="#/account" data-testid="home-account">' +
          '<svg class="ph-icon" aria-hidden="true"><use href="/icons/phosphor-sprite.svg#ph-key"/></svg> ' +
          'Sign in with your companion to keep My Mesh on every device</a>';
      }
    });
  }

  // ── Page ──────────────────────────────────────────────────────────────

  var view = null; // { container, poller, alive, hintTimer, confirm }

  function init(container, param) {
    destroy();
    view = { container: container, alive: true, poller: null, hintTimer: null };
    container.innerHTML = '<div class="acct-page" data-testid="account-page"><p class="acct-muted">Loading…</p></div>';
    boot().then(function () { if (view && view.container === container) render(param); });
  }

  function destroy() {
    if (!view) return;
    view.alive = false;
    if (view.poller) view.poller.cancel();
    if (view.hintTimer) clearTimeout(view.hintTimer);
    view = null;
  }

  function page() { return view && view.container.querySelector('.acct-page'); }

  function hashParam(name) {
    var q = (root.location.hash.split('?')[1] || '');
    var m = new RegExp('(?:^|&)' + name + '=([^&]*)').exec(q);
    return m ? decodeURIComponent(m[1]) : '';
  }

  function render() {
    var el = page();
    if (!el) return;
    if (!state.config || !state.config.enabled) {
      el.innerHTML = '<section class="acct-card"><h1>Account</h1><p>Accounts are not switched on for this site.</p></section>';
      return;
    }
    if (state.account) { renderAccount(); return; }
    var p = readPending();
    if (pendingUsable(p, Date.now())) { renderCode(p); return; }
    writePending(null);
    renderChoose(normalizeKey(hashParam('pubkey')));
  }

  // Step 1: which companion?
  function renderChoose(prefill) {
    var el = page();
    el.innerHTML =
      '<section class="acct-card" data-testid="account-signin">' +
        '<h1>Sign in with your companion</h1>' +
        '<p class="acct-lead">Your companion radio is your account: no password and no e-mail. Your My Mesh list then follows you to every device.</p>' +
        '<label class="acct-label" for="acctSearch">Which companion?</label>' +
        '<input id="acctSearch" class="input acct-input" type="text" autocomplete="off" spellcheck="false" placeholder="Companion name or public key" value="' + esc(prefill) + '">' +
        '<ul class="acct-results" id="acctResults" aria-live="polite"></ul>' +
        '<p class="acct-status" id="acctError" role="alert"></p>' +
        '<p class="acct-note">Only a companion can sign in. Which nodes are on your list, and that your companion has an account, is never shown publicly.</p>' +
      '</section>';
    var input = el.querySelector('#acctSearch');
    var timer = null;
    var seq = 0;
    function update() {
      var q = input.value;
      var results = el.querySelector('#acctResults');
      var key = normalizeKey(q);
      if (key) {
        results.innerHTML = '<li><button type="button" class="btn btn-secondary acct-pick" data-key="' + esc(key) + '">Use key ' + esc(key.slice(0, 8)) + '…</button></li>';
        return;
      }
      if (String(q).trim().length < 2) { results.innerHTML = ''; return; }
      var mine = ++seq;
      root.api('/nodes/search?q=' + encodeURIComponent(String(q).trim()), { ttl: root.CLIENT_TTL && CLIENT_TTL.nodeSearch }).then(function (data) {
        if (mine !== seq || !view) return;
        var list = companions(data && data.nodes);
        results.innerHTML = list.length ? list.map(function (n) {
          return '<li><button type="button" class="btn btn-secondary acct-pick" data-key="' + esc(n.public_key) + '">' +
            '<span class="acct-pick-name">' + esc(n.name || 'Unnamed companion') + '</span> <span class="acct-muted">' + esc(String(n.public_key).slice(0, 8)) + '…</span></button></li>';
        }).join('') : '<li class="acct-muted">No companion found. Paste its public key, or send a flood advert from it first.</li>';
      }, function () { /* the search is a convenience: a pasted key still works */ });
    }
    input.addEventListener('input', function () { clearTimeout(timer); timer = setTimeout(update, 250); });
    el.querySelector('#acctResults').addEventListener('click', function (e) {
      var b = e.target.closest('.acct-pick');
      if (b) startSignIn(b.getAttribute('data-key'));
    });
    if (prefill) update();
  }

  function startSignIn(pubkey) {
    var err = page() && page().querySelector('#acctError');
    call('POST', '/account/login', { pubkey: pubkey }).then(function (res) {
      var p = {
        requestId: res.requestId, code: res.code, redeemSecret: res.redeemSecret, expiresAt: res.expiresAt,
        budget: res.nameBudgetBytes, suggestedName: res.suggestedName, startedAt: Date.now()
      };
      writePending(p);
      if (view) renderCode(p);
    }, function (e) {
      if (err) { err.textContent = loginError(e); err.setAttribute('data-kind', 'error'); }
    });
  }

  // Step 2 and 3: the code, then waiting for the advert.
  function renderCode(p) {
    var el = page();
    var fit = nameFits(p.suggestedName, p.code, p.budget);
    el.innerHTML =
      '<section class="acct-card" data-testid="account-code">' +
        '<h1>Your sign-in code</h1>' +
        '<p class="acct-code" data-testid="account-code-value">' + esc(p.code) + '</p>' +
        '<ol class="acct-steps">' +
          '<li>Set this as your companion’s name in the MeshCore app:' +
            '<div class="acct-name-row"><code class="acct-name" data-testid="account-suggested-name">' + esc(p.suggestedName) + '</code>' +
            '<button type="button" class="btn btn-secondary acct-copy" data-testid="account-copy">Copy name</button></div>' +
            '<p class="acct-muted acct-bytes" data-kind="' + (fit.ok ? 'success' : 'error') + '">' + esc(nameFitsText(fit)) + '</p></li>' +
          '<li>Send a <strong>Flood</strong> advert. A zero-hop advert only reaches radios next to you.</li>' +
        '</ol>' +
        '<p class="acct-status" id="acctStatus" aria-live="polite" data-testid="account-status"></p>' +
        '<p class="acct-hint" id="acctHint" hidden>No observer has heard it yet. Send the flood advert again, or try closer to a repeater.</p>' +
        '<p class="acct-note">Your name is public for a few minutes. Change it back afterwards.</p>' +
        '<div class="acct-actions"><button type="button" class="btn btn-secondary acct-cancel" data-testid="account-cancel">Cancel</button></div>' +
      '</section>';
    el.querySelector('.acct-copy').addEventListener('click', function () {
      var b = this;
      if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(p.suggestedName).then(function () { b.textContent = 'Copied'; }, function () {});
      }
    });
    el.querySelector('.acct-cancel').addEventListener('click', function () {
      if (view && view.poller) view.poller.cancel();
      writePending(null);
      renderChoose('');
    });
    var wait = Math.max(0, HINT_AFTER_MS - (Date.now() - (p.startedAt || Date.now())));
    view.hintTimer = setTimeout(function () {
      var h = page() && page().querySelector('#acctHint');
      if (h) h.hidden = false;
    }, wait);
    pollSignIn(p);
  }

  function showStatus(line) {
    var s = page() && page().querySelector('#acctStatus');
    if (!s) return;
    s.textContent = line.text;
    s.setAttribute('data-kind', line.kind);
  }

  function pollSignIn(p) {
    var AC = root.AntennaClaims;
    var v = view;
    v.poller = AC.createStatusPoller({
      settings: AC.DEFAULT_SETTINGS,
      isAlive: function () { return v.alive; },
      fetchStatus: function (id) { return call('GET', '/account/login/' + encodeURIComponent(id)); },
      keepGoing: function (st) { return !statusLine(st).done; },
      onUpdate: function (st) {
        var line = statusLine(st);
        showStatus(line);
        if (line.redeem) redeem(p);
        else if (line.done) writePending(null);
      },
      onGiveUp: function () { showStatus({ text: 'Still waiting. Send the flood advert again, then reload this page.', kind: 'warn' }); }
    });
    showStatus(statusLine({ status: 'queued' }));
    v.poller.start(p.requestId);
  }

  function redeem(p) {
    call('POST', '/account/session', { requestId: p.requestId, redeemSecret: p.redeemSecret }).then(function (who) {
      writePending(null);
      if (view) renderConfirm(who);
    }, function () {
      writePending(null);
      showStatus({ text: 'The sign-in could not be completed. Ask for a new code.', kind: 'error' });
    });
  }

  // Step 4: who signed in, so a wrong companion can still be backed out of.
  function renderConfirm(who) {
    var el = page();
    var name = who && who.identityName ? who.identityName : 'your companion';
    el.innerHTML =
      '<section class="acct-card" data-testid="account-confirm">' +
        '<h1>Signed in</h1>' +
        '<p>Signed in as <strong>' + esc(name) + '</strong> <span class="acct-muted">(' + esc(who && who.identityPrefix) + '…)</span>.</p>' +
        '<p class="acct-note">Remember to give your companion its old name back.</p>' +
        '<div class="acct-actions">' +
          '<button type="button" class="btn btn-primary acct-me" data-testid="account-this-is-me">This is me</button>' +
          '<button type="button" class="btn btn-secondary acct-not-me" data-testid="account-not-me">Not me – cancel</button>' +
        '</div>' +
      '</section>';
    el.querySelector('.acct-me').addEventListener('click', function () {
      state.boot = null;
      boot().then(function () { if (view) render(); });
    });
    el.querySelector('.acct-not-me').addEventListener('click', function () {
      call('DELETE', '/account/session').catch(function () {}).then(function () {
        signedOut();
        if (view) renderChoose('');
      });
    });
  }

  // The signed-in account.
  function renderAccount() {
    var el = page();
    var acc = state.account;
    var who = acc.identityName || 'your companion';
    var nodes = MyNodes.list();
    var max = (acc.limits && acc.limits.maxMyNodes) || 0;
    var candidates = MyNodes.migrationCandidates();
    var banner = candidates.length && !MyNodes.migrationDismissed()
      ? '<div class="acct-banner" role="region" aria-label="Saved nodes in this browser" data-testid="account-migrate">' +
          '<p>This browser has ' + candidates.length + ' saved node' + (candidates.length === 1 ? '' : 's') + ' that ' +
          (candidates.length === 1 ? 'is' : 'are') + ' not on your account. Add ' + (candidates.length === 1 ? 'it' : 'them') + '?</p>' +
          '<div class="acct-actions"><button type="button" class="btn btn-primary acct-migrate-add" data-testid="account-migrate-add">Add all</button>' +
          '<button type="button" class="btn btn-secondary acct-migrate-later" data-testid="account-migrate-later">Not now</button></div>' +
        '</div>'
      : '';
    var rows = nodes.map(function (n) {
      return '<li class="acct-node"><a href="#/nodes/' + esc(encodeURIComponent(n.pubkey)) + '">' + esc(n.name || String(n.pubkey).slice(0, 12)) + '</a>' +
        '<button type="button" class="btn btn-secondary acct-remove" data-key="' + esc(n.pubkey) + '" aria-label="Remove ' + esc(n.name || 'node') + ' from My nodes">Remove</button></li>';
    }).join('');
    el.innerHTML =
      '<section class="acct-card" data-testid="account-signed-in">' +
        '<h1>Your account</h1>' +
        '<p>Signed in as <strong>' + esc(acc.displayName || who) + '</strong> <span class="acct-muted">(' + esc(acc.identityPrefix) + '…)</span>' +
          (acc.signedInAt ? ' <span class="acct-muted">since ' + esc(new Date(acc.signedInAt).toLocaleDateString()) + '</span>' : '') + '</p>' +
        banner +
        '<h2 class="acct-h2">My nodes <span class="acct-muted" data-testid="account-node-count">' + nodes.length + (max ? ' / ' + max : '') + '</span></h2>' +
        (rows ? '<ul class="acct-nodes" data-testid="account-nodes">' + rows + '</ul>'
              : '<p class="acct-muted">No nodes yet. Add them from <a href="#/home">My Mesh</a>.</p>') +
        '<p><a href="#/home">Open My Mesh →</a></p>' +
        '<h2 class="acct-h2">Display name</h2>' +
        '<form class="acct-row acct-profile"><label class="acct-sr" for="acctName">Display name</label>' +
          '<input id="acctName" class="input acct-input" maxlength="32" autocomplete="off" value="' + esc(acc.displayName || '') + '" placeholder="Optional, not shown publicly">' +
          '<button type="submit" class="btn btn-secondary">Save</button></form>' +
        '<p class="acct-status" id="acctStatus" aria-live="polite"></p>' +
        '<div class="acct-actions acct-danger-zone">' +
          '<button type="button" class="btn btn-secondary acct-signout" data-testid="account-sign-out">Sign out</button>' +
          '<button type="button" class="btn btn-secondary acct-signout-all" data-testid="account-sign-out-all">Sign out everywhere</button>' +
          '<button type="button" class="btn btn-secondary acct-delete" data-testid="account-delete">Delete account</button>' +
        '</div>' +
        '<div class="acct-confirm" id="acctConfirm" hidden></div>' +
      '</section>';
    wireAccount(el, max);
  }

  function wireAccount(el, max) {
    var mig = el.querySelector('.acct-migrate-add');
    if (mig) mig.addEventListener('click', function () {
      var merged = MyNodes.mergedForMigration(max);
      mig.disabled = true;
      call('POST', '/account/my-nodes', { op: 'set', nodes: merged.map(toServerNode) }).then(function () {
        MyNodes.setAccount(state.account.identityPrefix, merged, max);
        MyNodes.dismissMigration();
        renderAccount();
        showStatus({ text: 'Added. Your list is saved to your account.', kind: 'success' });
      }, function (e) {
        mig.disabled = false;
        writeFailed(e);
        showStatus({ text: 'The nodes could not be added. Try again.', kind: 'error' });
      });
    });
    var later = el.querySelector('.acct-migrate-later');
    if (later) later.addEventListener('click', function () { MyNodes.dismissMigration(); renderAccount(); });
    var list = el.querySelector('.acct-nodes');
    if (list) list.addEventListener('click', function (e) {
      var b = e.target.closest('.acct-remove');
      if (!b) return;
      MyNodes.remove(b.getAttribute('data-key'));
      renderAccount();
    });
    el.querySelector('.acct-profile').addEventListener('submit', function (e) {
      e.preventDefault();
      var name = el.querySelector('#acctName').value.trim();
      call('POST', '/account/profile', { displayName: name }).then(function () {
        state.account.displayName = name;
        showStatus({ text: 'Saved.', kind: 'success' });
      }, function (er) {
        writeFailed(er);
        showStatus({ text: er && er.status === 400 ? 'Use 1-32 printable characters.' : 'Could not save the name.', kind: 'error' });
      });
    });
    el.querySelector('.acct-signout').addEventListener('click', function () {
      call('DELETE', '/account/session').catch(function () {}).then(afterSignOut);
    });
    el.querySelector('.acct-signout-all').addEventListener('click', function () {
      confirmStep('End every session of this account, on all devices?', 'Sign out everywhere', function () {
        return call('POST', '/account/sign-out-all');
      }, REVOKE_TEXT.signOutAll);
    });
    el.querySelector('.acct-delete').addEventListener('click', function () {
      confirmStep('Delete your account and its My nodes list? This browser keeps its own saved nodes.', 'Delete account', function () {
        return call('DELETE', '/account');
      }, REVOKE_TEXT.del);
    });
  }

  var REVOKE_TEXT = {
    signOutAll: {
      waiting: 'Signing out everywhere…',
      done: 'Signed out everywhere. Every session of this account has ended.',
      failed: 'Sign out everywhere did not complete, so other devices may still be signed in. Sign in again and retry.'
    },
    del: {
      waiting: 'Deleting your account…',
      done: 'Your account and its My nodes list are deleted.',
      failed: 'The account was not deleted. Sign in again and retry.'
    }
  };

  // revocationLine maps the status of a queued sign-out-everywhere or
  // delete (GET /api/account/requests/{id}) to the page. Only the server's
  // "done" is success: the 202 alone means queued, not applied (#1020
  // review N2). "gone" is the poller's 404: the request is unknown.
  function revocationLine(st, text) {
    var s = st && st.status;
    if (s === 'done') return { done: true, ok: true, text: text.done, kind: 'success' };
    if (s === 'queued') return { done: false, ok: false, text: text.waiting, kind: '' };
    var why = s === 'error' && st.error ? ' (' + String(st.error) + ')' : '';
    return { done: true, ok: false, text: text.failed + why, kind: 'error' };
  }

  function confirmStep(question, action, run, text) {
    var box = page() && page().querySelector('#acctConfirm');
    if (!box) return;
    box.hidden = false;
    box.innerHTML = '<p>' + esc(question) + '</p><div class="acct-actions">' +
      '<button type="button" class="btn btn-secondary acct-btn-danger acct-confirm-yes" data-testid="account-confirm-yes">' + esc(action) + '</button>' +
      '<button type="button" class="btn btn-secondary acct-confirm-no">Keep</button></div>' +
      '<p class="acct-status" data-testid="account-confirm-status" aria-live="polite"></p>';
    // At phone height the box opens under the fold (and the bottom nav):
    // bring the question to the middle of the screen (#1020 review N4).
    if (typeof box.scrollIntoView === 'function') box.scrollIntoView({ block: 'center' });
    var yes = box.querySelector('.acct-confirm-yes');
    var no = box.querySelector('.acct-confirm-no');
    var line = box.querySelector('.acct-status');
    function show(l) { line.textContent = l.text; line.setAttribute('data-kind', l.kind); }
    no.addEventListener('click', function () { box.hidden = true; box.innerHTML = ''; });
    yes.addEventListener('click', function () {
      yes.disabled = no.disabled = true;
      show({ text: text.waiting, kind: '' });
      run().then(function (res) {
        // The server ended this browser's cookie already; the others end
        // once the ingestor applies it, which the poll confirms.
        signedOut();
        waitForRevocation(res && res.requestId, text, show);
      }, function (e) {
        yes.disabled = no.disabled = false;
        writeFailed(e);
        show({ text: (e && e.status === 401) ? 'This session has already ended. Sign in again.' : 'That did not work. Try again.', kind: 'error' });
      });
    });
  }

  function waitForRevocation(id, text, show) {
    var v = view;
    if (!v) return;
    if (!id) { afterSignOut(revocationLine(null, text)); return; }
    v.poller = root.AntennaClaims.createStatusPoller({
      settings: root.AntennaClaims.DEFAULT_SETTINGS,
      isAlive: function () { return v.alive; },
      fetchStatus: function (rid) { return call('GET', '/account/requests/' + encodeURIComponent(rid)); },
      keepGoing: function (st) { return !revocationLine(st, text).done; },
      onUpdate: function (st) {
        var l = revocationLine(st, text);
        if (l.done) afterSignOut(l);
        else show(l);
      },
      onGiveUp: function () {
        afterSignOut({ text: 'Not confirmed yet: the server has not applied it. Sign in again and retry if other devices stay signed in.', kind: 'warn' });
      }
    });
    v.poller.start(id);
  }

  // afterSignOut shows the sign-in step, with line (the outcome) if given.
  function afterSignOut(line) {
    signedOut();
    if (!view) return;
    renderChoose('');
    var p = line && line.text && page() && page().querySelector('#acctError');
    if (p) {
      p.textContent = line.text;
      p.setAttribute('data-kind', line.kind);
      p.setAttribute('data-testid', 'account-outcome');
    }
  }

  if (typeof root.registerPage === 'function') root.registerPage('account', { init: init, destroy: destroy });
  if (root.MyNodes && typeof root.fetch === 'function' && root.document) boot();

  root.Account = {
    boot: boot,
    mountEntry: mountEntry,
    utf8Bytes: utf8Bytes,
    utf8Prefix: utf8Prefix,
    nameFits: nameFits,
    nameFitsText: nameFitsText,
    statusLine: statusLine,
    revocationLine: revocationLine,
    REVOKE_TEXT: REVOKE_TEXT,
    loginError: loginError,
    normalizeKey: normalizeKey,
    companions: companions,
    toServerNode: toServerNode,
    fromServerNode: fromServerNode,
    pendingUsable: pendingUsable
  };
})(typeof window !== 'undefined' ? window : this);
