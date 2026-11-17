/* #789 M2b — owner antenna-height claims: the owner flow on the node page
 * and the administrator's queue (Tools → Antenna Height Claims).
 *
 * Server contract (cmd/server/antenna_claims.go, #789 M2a):
 *   GET  /api/antenna-claims/config                → {enabled}
 *   POST /api/nodes/{pubkey}/antenna-claims {heightM} | {clear:true}
 *        → 202 {requestId, code, expiresAt, nameBudgetBytes, suggestedName}
 *          403 closed · 404 hidden/unknown/no position · 429/503 Retry-After
 *   GET  /api/antenna-claims/requests/{id}         → {status, expiresAt, error}
 *   GET  /api/admin/antenna-claims[?status=]       → {claims, enabled, autoApprove}  (X-API-Key)
 *   POST /api/admin/antenna-claims/{id}/approve|reject|revoke → 202 {requestId}
 *        or 409 (wrong status, or a newer approved claim for the node)    (X-API-Key)
 *
 * The owner proves control of the node by putting the code into the node's
 * advert name and sending an advert; the ingestor matches it. The code is
 * not a secret (the advert signature does the security work), so it is kept
 * in sessionStorage per request id to survive a reload of the tab. The admin
 * key lives only in this module's memory until "Lock" or a page reload.
 *
 * Every string from the server or the node reaches the page through
 * textContent or a non-URL attribute; there is no HTML built from data.
 *
 * Pure helpers (parseHeight, checkName, statusInfo, pollDelay,
 * createStatusPoller, parseFilter, …) have no DOM or network access so
 * test-antenna-claims.js can run them in Node.
 */
(function (root) {
  'use strict';

  // Firmware: an advert with a position leaves 23 bytes for the name
  // (MAX_ADVERT_DATA_SIZE 32 − flags − lat/lon). The 202 answer repeats it.
  var NAME_BUDGET = 23;
  var CODE_RE = /^[0-9A-HJKMNP-TV-Z]{6}$/;
  var ID_RE = /^[0-9a-f]{16,64}$/;
  var PUBKEY_RE = /^[0-9a-f]{64}$/;
  // The firmware's CLI `set name` refuses these (AdvertDataParser::isValidName).
  var CLI_BAD_RE = /[\[\]\\:,?*]/;
  var CLAIM_ROLES = { repeater: true, room: true };

  function maxHeight() {
    var m = root.MCAntennaHeight && root.MCAntennaHeight.MAX_M;
    return typeof m === 'number' ? m : 100;
  }

  function utf8Length(s) {
    try { return encodeURIComponent(s).replace(/%[0-9A-F]{2}/gi, 'x').length; } catch (e) { return Infinity; }
  }

  // parseHeight reads the owner's height: 0–100 m on the 0.1 m grid the
  // server accepts. A finer value is refused, not rounded: the owner must
  // see the number that will be stored. A decimal comma is accepted.
  function parseHeight(raw) {
    var s = String(raw == null ? '' : raw).trim().replace(',', '.');
    if (!s) return { error: 'Enter the antenna height in metres.' };
    if (!/^[0-9]{1,3}(\.[0-9]+)?$/.test(s)) return { error: 'Enter a number of metres, like 18.5.' };
    if (/\.[0-9]{2,}$/.test(s) && !/\.[0-9]0+$/.test(s)) return { error: 'Use steps of 0.1 m, like 18.5.' };
    var n = Math.round(Number(s) * 10) / 10;
    if (!(n >= 0) || n > maxHeight()) return { error: 'The height must be between 0 and ' + maxHeight() + ' m.' };
    return { heightM: n };
  }

  // utf8Prefix is the longest prefix of s that fits in max bytes.
  function utf8Prefix(s, max) {
    var out = '';
    var used = 0;
    for (var i = 0; i < s.length;) {
      var cp = s.codePointAt(i);
      var ch = String.fromCodePoint(cp);
      var b = cp < 0x80 ? 1 : cp < 0x800 ? 2 : cp < 0x10000 ? 3 : 4;
      if (used + b > max) break;
      out += ch;
      used += b;
      i += ch.length;
    }
    return out;
  }

  // checkName mirrors antennaclaim.ContainsCode: the code must be a whole
  // space-separated word inside the first `budget` bytes of the name,
  // because the firmware cuts the rest before it goes on air.
  function checkName(name, code, budget) {
    name = String(name == null ? '' : name);
    budget = budget > 0 ? budget : NAME_BUDGET;
    var bytes = utf8Length(name);
    var r = { bytes: bytes, budget: budget, ok: false, reason: '' };
    var words = name.split(' ');
    if (words.indexOf(code) < 0) { r.reason = 'missing'; return r; }
    if (utf8Prefix(name, budget).split(' ').indexOf(code) < 0) { r.reason = 'cut'; return r; }
    if (CLI_BAD_RE.test(name)) { r.reason = 'chars'; return r; }
    r.ok = true;
    return r;
  }

  var NAME_PROBLEMS = {
    missing: 'The code must appear in the name as a separate word.',
    cut: 'The name is too long: the node only sends the first ' + NAME_BUDGET + ' bytes, so the code would be cut off. Shorten the name before the code.',
    chars: 'The node’s CLI refuses names with [ ] \\ : , ? or *.'
  };

  // nameCheckText is the line under the name field for a checkName result.
  // Words after the code may run past the budget; the code still goes on
  // air, so that is said plainly instead of "38 / 23 bytes" in green.
  function nameCheckText(r) {
    if (r.ok && r.bytes > r.budget) return r.bytes + ' bytes; only the first ' + r.budget + ' go on air — the code is inside them.';
    return r.bytes + ' / ' + r.budget + ' bytes — ' + (r.ok ? 'the code fits.' : NAME_PROBLEMS[r.reason]);
  }

  // statusInfo is the owner-facing line for a request status. `done` stops
  // polling; `kind` styles the line.
  function statusInfo(st, opts) {
    var clear = !!(opts && opts.clear);
    var status = st && st.status;
    switch (status) {
      case 'queued': return { text: 'Registering your request…', kind: 'info', done: false };
      case 'pending': return { text: 'Waiting for an advert from the node with the code in its name…', kind: 'info', done: false };
      case 'proven': return { text: 'Proven: the node sent the code. An administrator of this site reviews the height next.', kind: 'success', done: false };
      case 'approved': return { text: clear ? 'Done: your antenna height is removed.' : 'Approved: profiles now use your antenna height.', kind: 'success', done: true };
      case 'rejected': return { text: 'An administrator rejected this claim.', kind: 'warn', done: true };
      case 'revoked': return { text: 'An administrator withdrew this height.', kind: 'warn', done: true };
      case 'expired': return { text: 'The code expired before the node sent it. Ask for a new code.', kind: 'warn', done: true };
      case 'superseded': return { text: 'A newer claim for this node replaced this one.', kind: 'info', done: true };
      case 'gone': return { text: 'This claim is no longer known here (expired or removed). Ask for a new code.', kind: 'warn', done: true };
      default: return { text: (st && st.error) || 'The claim could not be processed.', kind: 'error', done: true };
    }
  }

  // giveUpText replaces the status line when the poller stops on its own.
  // After `proven` the advert is done and an administrator decides next.
  function giveUpText(last) {
    if (last && last.status === 'proven') return 'Proven; waiting for review. Check again later.';
    return 'Stopped checking automatically. The claim keeps waiting; check again after sending the advert.';
  }

  // Bounded backoff: 2 s, 3 s, 4.5 s … capped at 30 s, at most
  // MAX_POLL_ATTEMPTS checks (about 25 minutes), then "Check again".
  var MAX_POLL_ATTEMPTS = 60;
  function pollDelay(attempt) {
    return Math.min(Math.round(2000 * Math.pow(1.5, Math.max(0, attempt))), 30000);
  }

  // One poller per view. opts.fetchStatus(id) → Promise<status>;
  // opts.onUpdate(status); opts.onGiveUp(); opts.isAlive() stops it once the
  // view is gone; opts.keepGoing(status) says whether to poll again (default:
  // until statusInfo says done). Timers are injectable for tests.
  function createStatusPoller(opts) {
    var setT = opts.setTimeout || root.setTimeout.bind(root);
    var clearT = opts.clearTimeout || root.clearTimeout.bind(root);
    var maxAttempts = opts.maxAttempts || MAX_POLL_ATTEMPTS;
    var keepGoing = opts.keepGoing || function (st) { return !statusInfo(st).done; };
    var alive = opts.isAlive || function () { return true; };
    var timer = null;
    var generation = 0;

    function cancel() {
      generation++;
      if (timer !== null) { clearT(timer); timer = null; }
    }

    function start(id) {
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
        if (!alive()) { cancel(); return; }
        Promise.resolve().then(function () { return opts.fetchStatus(id); }).then(function (st) {
          if (gen !== generation || !alive()) return;
          opts.onUpdate(st);
          if (keepGoing(st)) schedule();
        }, function (err) {
          if (gen !== generation || !alive()) return;
          if (err && err.status === 404) { opts.onUpdate({ status: 'gone' }); return; }
          schedule(); // a transient network or server error: keep backing off
        });
      }
      schedule();
    }

    return { start: start, cancel: cancel, isActive: function () { return timer !== null; } };
  }

  // Deep links. The node section: #/nodes/{pk}?section=node-antenna[&claim=id].
  function hashParams(hash) {
    var q = String(hash || '').split('?')[1] || '';
    try { return new URLSearchParams(q); } catch (e) { return null; }
  }
  function claimParam(hash) {
    var p = hashParams(hash);
    var id = p && p.get('claim');
    return id && ID_RE.test(id) ? id : null;
  }
  function claimHash(pubkey, id) {
    return '#/nodes/' + encodeURIComponent(pubkey) + '?section=node-antenna' + (id ? '&claim=' + id : '');
  }

  // The admin queue: #/tools/antenna-claims[?filter=…]. "proven" is the
  // review queue and the default.
  var FILTERS = [['proven', 'To review'], ['pending', 'Waiting for advert'], ['approved', 'Approved'],
    ['rejected', 'Rejected'], ['revoked', 'Revoked'], ['expired', 'Expired'], ['superseded', 'Superseded']];
  function parseFilter(v) {
    for (var i = 0; i < FILTERS.length; i++) if (FILTERS[i][0] === v) return v;
    return 'proven';
  }
  function adminHash(filter) {
    return '#/tools/antenna-claims' + (filter && filter !== 'proven' ? '?filter=' + encodeURIComponent(filter) : '');
  }
  // The decisions the server accepts per status (claimDecisionSources).
  function decisionsFor(status) {
    if (status === 'proven') return ['approve', 'reject'];
    if (status === 'pending') return ['reject'];
    if (status === 'approved') return ['revoke'];
    return [];
  }
  var NEWER_APPROVED = 'a newer claim for this node is already approved';
  // decisionError explains a refused decision; the 409 for a newer approved
  // claim (#866 F2) says what to do instead.
  function decisionError(err) {
    var msg = (err && err.message) || '';
    if (msg === NEWER_APPROVED) {
      return 'Not approved: a newer claim for this node is already approved (it may be the owner’s own later height or removal). ' +
        'Approving this older proof would undo it. Reject this claim instead.';
    }
    if (err && err.status === 409) return 'Not possible any more: the ' + msg + '. The list is refreshed.';
    if (err && err.status === 404) return 'This claim is no longer listed. The list is refreshed.';
    return msg || 'Something went wrong.';
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
      cache: 'no-store'
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

  function statusOf(id) {
    return request('GET', '/antenna-claims/requests/' + encodeURIComponent(id));
  }

  // One config read per page load; a failed read is retried next time.
  var configPromise = null;
  function loadConfig() {
    if (!configPromise) {
      configPromise = request('GET', '/antenna-claims/config').then(function (c) {
        return { enabled: !!(c && c.enabled === true) };
      }, function () {
        configPromise = null;
        return { enabled: false };
      });
    }
    return configPromise;
  }

  // The code survives a reload of the tab (it is not a secret).
  var STORE_PREFIX = 'mc-antenna-claim:';
  function remember(id, info) {
    try { root.sessionStorage.setItem(STORE_PREFIX + id, JSON.stringify(info)); } catch (e) { /* private mode */ }
  }
  function recall(id) {
    try {
      var v = JSON.parse(root.sessionStorage.getItem(STORE_PREFIX + id) || 'null');
      return v && typeof v.code === 'string' && CODE_RE.test(v.code) ? v : null;
    } catch (e) { return null; }
  }

  // ── DOM helpers ────────────────────────────────────────────────────────
  function el(tag, className, text) {
    var e = document.createElement(tag);
    if (className) e.className = className;
    if (text != null) e.textContent = text;
    return e;
  }
  function button(text, className, attrs) {
    var b = el('button', className || 'btn btn-secondary', text);
    b.type = 'button';
    for (var k in attrs || {}) b.setAttribute(k, attrs[k]);
    return b;
  }
  function fmtTime(ms) {
    if (!ms) return '';
    try { return new Date(ms).toLocaleString(); } catch (e) { return ''; }
  }
  function fmtHeight(h) {
    return h == null ? null : Number(h).toFixed(1).replace(/\.0$/, '') + ' m';
  }
  function setStatus(line, info) {
    line.textContent = info.text;
    line.setAttribute('data-kind', info.kind);
  }
  function copy(text, btn) {
    var done = function () { btn.textContent = 'Copied'; setTimeout(function () { btn.textContent = 'Copy'; }, 2000); };
    if (typeof root.copyToClipboard === 'function') root.copyToClipboard(text, done);
    else if (root.navigator && root.navigator.clipboard) root.navigator.clipboard.writeText(text).then(done, function () {});
  }

  // ── Owner section on the node page ─────────────────────────────────────
  // mountOwnerSection fills `section` (hidden until the feature is known to
  // be on) for node n and returns the view ({ pubkey, destroy }). The node
  // page rebuilds all its sections on some events (theme-refresh); passing
  // the previous view of the same node moves its content — typed values,
  // the code and the running poller — into the new section instead of
  // starting over. A view of another node is destroyed.
  function mountOwnerSection(section, n, prev) {
    var pubkey = n && n.public_key;
    if (prev && prev.pubkey === pubkey && pubkey && section && prev.adopt(section)) return prev;
    if (prev) prev.destroy();
    var view = { section: section, poller: null, alive: true, rendered: false };
    var api = {
      pubkey: pubkey,
      destroy: function () { view.alive = false; if (view.poller) view.poller.cancel(); },
      adopt: function (next) {
        if (!view.alive) return false;
        var old = view.section;
        view.section = next;
        if (view.rendered) {
          while (old.firstChild) next.appendChild(old.firstChild);
          next.setAttribute('data-testid', 'antenna-claim');
          next.hidden = false;
        }
        return true;
      }
    };
    if (!section || !n || !CLAIM_ROLES[n.role]) { view.alive = false; return api; }
    function alive() { return view.alive && view.section.isConnected; }
    view.poller = createStatusPoller({
      fetchStatus: statusOf,
      isAlive: alive,
      onUpdate: function (st) { if (view.onUpdate) view.onUpdate(st); },
      onGiveUp: function () { if (view.onGiveUp) view.onGiveUp(); }
    });
    loadConfig().then(function (cfg) {
      if (!alive() || !cfg.enabled) return;
      var target = view.section;
      renderOwnerSection(target, n, view);
      view.rendered = true;
      target.hidden = false;
      if ((hashParams(location.hash) || { get: function () {} }).get('section') === 'node-antenna') {
        setTimeout(function () { if (alive()) view.section.scrollIntoView({ block: 'start' }); }, 50);
      }
    });
    return api;
  }

  function renderOwnerSection(section, n, view) {
    section.textContent = '';
    section.setAttribute('data-testid', 'antenna-claim');
    var title = el('h4', null, 'Antenna height (owner)');
    title.id = 'antennaClaimTitle';
    section.appendChild(title);
    var body = el('div', 'ac-body');
    section.appendChild(body);
    var hasPos = n.lat != null && n.lon != null && !(n.lat === 0 && n.lon === 0);
    var id = claimParam(location.hash);
    if (id) { renderClaim(body, n, view, id, recall(id)); return; }
    if (!hasPos) {
      body.appendChild(el('p', 'text-muted', 'This node sends no position in its adverts, so an antenna height cannot be claimed for it.'));
      return;
    }
    renderForm(body, n, view);
  }

  function renderForm(body, n, view) {
    body.textContent = '';
    body.appendChild(el('p', 'ac-hint', 'Own this node? Tell this site how high its antenna is. Terrain profiles use it instead of an estimate. ' +
      'You prove you control the node by putting a short code into its name and sending an advert.'));
    var form = el('form', 'ac-form');
    form.noValidate = true;
    var modes = el('div', 'ac-modes');
    modes.setAttribute('role', 'radiogroup');
    modes.setAttribute('aria-label', 'What to do');
    function radio(value, label, checked) {
      var l = el('label', 'ac-mode');
      var r = el('input');
      r.type = 'radio'; r.name = 'acMode'; r.value = value; r.checked = checked;
      r.setAttribute('data-testid', 'ac-mode-' + value);
      l.appendChild(r);
      l.appendChild(document.createTextNode(' ' + label));
      modes.appendChild(l);
      return r;
    }
    var setR = radio('set', 'Set the height', true);
    var clearR = radio('clear', 'Remove my height', false);
    form.appendChild(modes);
    var row = el('div', 'ac-row');
    var lab = el('label', 'ac-label', 'Antenna height above ground (m)');
    lab.htmlFor = 'acHeight';
    var input = el('input', 'input ac-height');
    input.id = 'acHeight';
    input.type = 'text';
    input.setAttribute('inputmode', 'decimal');
    input.setAttribute('autocomplete', 'off');
    input.setAttribute('placeholder', '18.5');
    input.setAttribute('aria-describedby', 'acFormStatus');
    input.setAttribute('data-testid', 'ac-height');
    var submit = el('button', 'btn btn-primary ac-submit', 'Get code');
    submit.type = 'submit';
    submit.setAttribute('data-testid', 'ac-submit');
    row.appendChild(lab);
    row.appendChild(input);
    row.appendChild(submit);
    form.appendChild(row);
    var status = el('div', 'ac-status');
    status.id = 'acFormStatus';
    status.setAttribute('role', 'status');
    status.setAttribute('aria-live', 'polite');
    form.appendChild(status);
    body.appendChild(form);

    function sync() { input.disabled = clearR.checked; lab.classList.toggle('ac-disabled', clearR.checked); }
    setR.addEventListener('change', sync);
    clearR.addEventListener('change', sync);
    form.addEventListener('submit', function (e) {
      e.preventDefault();
      var claim;
      if (clearR.checked) claim = { clear: true };
      else {
        var h = parseHeight(input.value);
        if (h.error) { setStatus(status, { text: h.error, kind: 'error' }); input.focus(); return; }
        claim = { heightM: h.heightM };
      }
      submit.disabled = true;
      setStatus(status, { text: 'Asking for a code…', kind: 'info' });
      request('POST', '/nodes/' + encodeURIComponent(n.public_key) + '/antenna-claims', { body: claim }).then(function (res) {
        if (!view.alive || !res || !ID_RE.test(res.requestId) || !CODE_RE.test(res.code)) {
          if (view.alive) setStatus(status, { text: 'The server sent an unexpected answer.', kind: 'error' });
          return;
        }
        var info = {
          code: res.code, suggestedName: String(res.suggestedName || res.code), expiresAt: res.expiresAt,
          budget: res.nameBudgetBytes > 0 ? res.nameBudgetBytes : NAME_BUDGET,
          heightM: claim.heightM == null ? null : claim.heightM, clear: !!claim.clear, originalName: n.name || ''
        };
        remember(res.requestId, info);
        history.replaceState(null, '', claimHash(n.public_key, res.requestId));
        renderClaim(body, n, view, res.requestId, info);
      }, function (err) {
        var msg = err.status === 429 ? 'Too many claims right now. Please try again later.'
          : err.status === 403 ? 'Antenna height claims are closed on this site.'
          : err.status === 404 ? 'This node cannot take a claim (it may have no position).'
          : err.status === 503 ? 'The claim queue is full. Please try again later.'
          : err.message;
        setStatus(status, { text: msg, kind: 'error' });
      }).then(function () { submit.disabled = false; });
    });
  }

  function renderClaim(body, n, view, id, info) {
    body.textContent = '';
    var wrap = el('div', 'ac-claim');
    wrap.setAttribute('data-claim-id', id);
    body.appendChild(wrap);
    if (info) {
      wrap.appendChild(el('p', 'ac-what', info.clear ? 'Removing the owner antenna height of this node.'
        : 'Claiming an antenna height of ' + fmtHeight(info.heightM) + '.'));
      var codeRow = el('p', 'ac-code-row', 'Your code: ');
      var code = el('code', 'ac-code', info.code);
      code.setAttribute('data-testid', 'ac-code');
      codeRow.appendChild(code);
      wrap.appendChild(codeRow);
      wrap.appendChild(renderSteps(info));
      if (info.expiresAt) wrap.appendChild(el('p', 'text-muted ac-expiry', 'The code works once and expires ' + fmtTime(info.expiresAt) + '.'));
    } else {
      wrap.appendChild(el('p', 'text-muted', 'The code was shown in the tab where this claim was made. The status below still updates.'));
    }
    var status = el('div', 'ac-status');
    status.setAttribute('role', 'status');
    status.setAttribute('aria-live', 'polite');
    status.setAttribute('data-testid', 'ac-status');
    wrap.appendChild(status);
    var actions = el('div', 'ac-actions');
    var again = button('Check again', 'btn btn-secondary', { 'data-testid': 'ac-check-again' });
    again.hidden = true;
    var fresh = button('Start over', 'btn btn-secondary', { 'data-testid': 'ac-new' });
    actions.appendChild(again);
    actions.appendChild(fresh);
    wrap.appendChild(actions);
    var restore = el('p', 'ac-restore');
    restore.hidden = true;
    wrap.appendChild(restore);

    setStatus(status, statusInfo({ status: 'queued' }));
    var last = null;
    view.onUpdate = function (st) {
      last = st;
      var si = statusInfo(st, info);
      setStatus(status, si);
      again.hidden = true;
      if (st && (st.status === 'proven' || st.status === 'approved')) {
        restore.hidden = false;
        restore.textContent = 'You can now give the node its usual name back' +
          (info && info.originalName ? ' (set name ' + info.originalName + ')' : '') + ' and send an advert again.';
      }
    };
    view.onGiveUp = function () {
      setStatus(status, { text: giveUpText(last), kind: 'info' });
      again.hidden = false;
    };
    again.addEventListener('click', function () {
      again.hidden = true;
      setStatus(status, { text: 'Checking…', kind: 'info' });
      view.poller.start(id);
    });
    fresh.addEventListener('click', function () {
      view.poller.cancel();
      history.replaceState(null, '', claimHash(n.public_key, null));
      var hasPos = n.lat != null && n.lon != null && !(n.lat === 0 && n.lon === 0);
      if (hasPos) renderForm(body, n, view);
      else { body.textContent = ''; body.appendChild(el('p', 'text-muted', 'This node sends no position in its adverts, so an antenna height cannot be claimed for it.')); }
    });
    view.poller.start(id);
  }

  function renderSteps(info) {
    var steps = el('ol', 'ac-steps');
    var s1 = el('li');
    s1.appendChild(el('span', null, 'Give the node a name that ends with the code. Keep the code as a separate word at the end; the node only sends the first ' + info.budget + ' bytes of its name.'));
    var nameRow = el('div', 'ac-row');
    var nameLab = el('label', 'ac-label', 'Name to set');
    nameLab.htmlFor = 'acName';
    var name = el('input', 'input ac-name');
    name.id = 'acName';
    name.type = 'text';
    name.value = info.suggestedName;
    name.setAttribute('autocomplete', 'off');
    name.setAttribute('spellcheck', 'false');
    name.setAttribute('aria-describedby', 'acNameCheck');
    name.setAttribute('data-testid', 'ac-name');
    nameRow.appendChild(nameLab);
    nameRow.appendChild(name);
    s1.appendChild(nameRow);
    var check = el('div', 'ac-name-check');
    check.id = 'acNameCheck';
    check.setAttribute('aria-live', 'polite');
    check.setAttribute('data-testid', 'ac-name-check');
    s1.appendChild(check);
    var cmdRow = el('div', 'ac-cmd-row');
    var cmd = el('code', 'ac-cmd');
    cmd.setAttribute('data-testid', 'ac-cmd');
    var copyBtn = button('Copy', 'btn btn-secondary ac-copy');
    cmdRow.appendChild(cmd);
    cmdRow.appendChild(copyBtn);
    s1.appendChild(cmdRow);
    steps.appendChild(s1);
    var s2 = el('li');
    s2.appendChild(el('span', null, 'Send an advert: the CLI command '));
    s2.appendChild(el('code', 'ac-cmd', 'advert'));
    s2.appendChild(el('span', null, ', or “Send advert” in the app.'));
    steps.appendChild(s2);
    steps.appendChild(el('li', null, 'Run both on the node’s CLI over USB, or remotely: log in to the repeater as admin in the MeshCore app and use its command line. ' +
      'Until you change it back, the code is part of the node’s public name.'));
    function update() {
      var r = checkName(name.value, info.code, info.budget);
      check.textContent = nameCheckText(r);
      check.setAttribute('data-kind', r.ok ? 'success' : 'error');
      cmd.textContent = 'set name ' + name.value;
    }
    name.addEventListener('input', update);
    copyBtn.addEventListener('click', function () { copy(cmd.textContent, copyBtn); });
    update();
    return steps;
  }

  // ── Admin queue: #/tools/antenna-claims[?filter=…] ──────────────────────
  var admin = null; // { container, filter, poller, deciding: {id, text} }

  function adminInit(container) {
    admin = { container: container, filter: parseFilter((hashParams(location.hash) || { get: function () {} }).get('filter')), deciding: null };
    admin.poller = createStatusPoller({
      fetchStatus: statusOf,
      isAlive: function () { return !!admin && admin.container.isConnected; },
      // A decision is done once it has left the queue.
      keepGoing: function (st) { return !!st && st.status === 'queued'; },
      // The status route knows only the owner's claim requests: once the
      // ingestor has taken a decision off the queue it answers 404, which
      // the poller reports as `gone`. So `gone` means "applied" here (a
      // console 404 per decision is expected); a refused decision comes
      // back first as `error` with the ingestor's reason (#868 review N5).
      onUpdate: function (st) {
        if (!admin) return;
        var d = admin.deciding || {};
        var failed = st && st.status === 'error';
        loadAdminList().then(function () {
          showDecision(d.id, failed ? decisionError({ message: st.error }) : 'Done: ' + d.text + '.', failed ? 'error' : 'success');
        });
      },
      onGiveUp: function () { if (admin) showDecision((admin.deciding || {}).id, 'The decision is still queued. Refresh in a moment.', 'info'); }
    });
    container.textContent = '';
    var page = el('div', 'tools-landing ac-admin');
    page.appendChild(el('h2', null, 'Antenna Height Claims'));
    page.appendChild(el('p', 'help-text', 'Node owners claim an antenna height by putting a code into the node’s name and sending an advert. ' +
      'Review proven heights here. Each node can have at most 3 claims waiting for an advert; if unproven claims block the real owner, reject them under “Waiting for advert”.'));
    // List-level messages sit above the list, where a long queue cannot push
    // them out of sight; a decision's result goes into its row (#868 F2).
    var st = el('div', 'ac-status ac-admin-status');
    st.id = 'acAdminStatus';
    st.setAttribute('role', 'status');
    st.setAttribute('aria-live', 'polite');
    st.setAttribute('data-testid', 'ac-admin-status');
    page.appendChild(st);
    var bodyEl = el('div', 'ac-admin-body');
    bodyEl.id = 'acAdminBody';
    page.appendChild(bodyEl);
    container.appendChild(page);
    renderAdminBody();
  }

  function adminDestroy() {
    if (admin && admin.poller) admin.poller.cancel();
    admin = null;
  }

  function adminStatus(text, kind) {
    var s = document.getElementById('acAdminStatus');
    if (!s) return null;
    s.textContent = text || '';
    s.setAttribute('data-kind', kind || 'info');
    return s;
  }

  // reveal scrolls a feedback line into view only if it is not already;
  // its scroll-margin keeps it clear of the fixed top bar and bottom nav.
  function reveal(line) {
    if (line && line.textContent && line.scrollIntoView) line.scrollIntoView({ block: 'nearest' });
  }

  function rowOf(id) {
    var rows = document.querySelectorAll('#acAdminList .ac-item');
    for (var i = 0; i < rows.length; i++) if (id && rows[i].getAttribute('data-claim-id') === String(id)) return rows[i];
    return null;
  }

  // showDecision puts a decision's result in the row it was taken on, or,
  // once that row has left the list, in the line above the list. Nothing is
  // shown once the key was dropped (a 401 on the refresh says why).
  function showDecision(id, text, kind) {
    if (!admin || !adminKey) return;
    var row = rowOf(id);
    var line = row && row.querySelector('.ac-item-status');
    if (line) {
      adminStatus('', 'info');
      setStatus(line, { text: text, kind: kind });
    } else {
      line = adminStatus(text, kind);
    }
    reveal(line);
  }

  function renderAdminBody() {
    var body = document.getElementById('acAdminBody');
    if (!body || !admin) return;
    body.textContent = '';
    if (!adminKey) {
      var form = el('form', 'ac-key-form');
      form.noValidate = true;
      var lab = el('label', 'ac-label', 'Admin API key');
      lab.htmlFor = 'acAdminKey';
      var key = el('input', 'input');
      key.id = 'acAdminKey';
      key.type = 'password';
      key.setAttribute('autocomplete', 'off');
      key.setAttribute('data-testid', 'ac-admin-key');
      var unlock = el('button', 'btn btn-primary', 'Unlock');
      unlock.type = 'submit';
      var row = el('div', 'ac-row');
      row.appendChild(lab); row.appendChild(key); row.appendChild(unlock);
      form.appendChild(row);
      form.appendChild(el('p', 'text-muted', 'The key is kept in this tab’s memory only and is forgotten on reload.'));
      form.addEventListener('submit', function (e) {
        e.preventDefault();
        if (!key.value) return;
        adminKey = key.value;
        adminStatus('', 'info');
        renderAdminBody();
      });
      body.appendChild(form);
      return;
    }
    var bar = el('div', 'ac-toolbar');
    var group = el('div', 'ac-filters');
    group.setAttribute('role', 'group');
    group.setAttribute('aria-label', 'Show claims');
    FILTERS.forEach(function (f) {
      var b = button(f[1], 'btn btn-secondary ac-filter', { 'data-filter': f[0], 'aria-pressed': String(admin.filter === f[0]) });
      b.addEventListener('click', function () {
        admin.filter = f[0];
        history.replaceState(null, '', adminHash(f[0]));
        adminStatus('', 'info');
        renderAdminBody();
      });
      group.appendChild(b);
    });
    bar.appendChild(group);
    var refresh = button('Refresh', 'btn btn-secondary');
    refresh.addEventListener('click', loadAdminList);
    var lock = button('Lock', 'btn btn-secondary');
    lock.addEventListener('click', function () { adminKey = null; adminStatus('', 'info'); renderAdminBody(); });
    bar.appendChild(refresh);
    bar.appendChild(lock);
    body.appendChild(bar);
    var list = el('ul', 'ac-list');
    list.id = 'acAdminList';
    list.setAttribute('aria-busy', 'true');
    list.setAttribute('data-testid', 'ac-admin-list');
    body.appendChild(list);
    loadAdminList();
  }

  function loadAdminList() {
    var list = document.getElementById('acAdminList');
    if (!list || !admin) return Promise.resolve();
    var filter = admin.filter;
    list.setAttribute('aria-busy', 'true');
    return request('GET', '/admin/antenna-claims?status=' + encodeURIComponent(filter), { admin: true }).then(function (data) {
      if (!admin || admin.filter !== filter || !list.isConnected) return;
      var claims = data && Array.isArray(data.claims) ? data.claims : [];
      list.textContent = '';
      if (!claims.length) {
        var label = '';
        FILTERS.forEach(function (f) { if (f[0] === filter) label = f[1].toLowerCase(); });
        list.appendChild(el('li', 'ac-empty', 'No claims: ' + label + '.'));
      }
      claims.forEach(function (c) { list.appendChild(renderAdminRow(c)); });
      list.setAttribute('aria-busy', 'false');
      if (data && data.enabled === false && filter === 'proven') adminStatus('Owner claims are currently closed; existing ones can still be reviewed.', 'info');
    }, onAdminError);
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

  var DECISION_LABEL = { approve: 'Approve', reject: 'Reject', revoke: 'Revoke' };
  var DECISION_DONE = { approve: 'approved', reject: 'rejected', revoke: 'revoked' };

  function renderAdminRow(c) {
    var li = el('li', 'ac-item');
    li.setAttribute('data-claim-id', String(c.id || ''));
    li.setAttribute('data-status', String(c.status || ''));
    var main = el('div', 'ac-item-main');
    // Only a well-formed key becomes a link (the server sends lowercase hex).
    var key = PUBKEY_RE.test(c.pubkey) ? c.pubkey : '';
    var who = el(key ? 'a' : 'span', 'ac-item-name', c.name || key.slice(0, 12) || '(unknown node)');
    if (key) who.setAttribute('href', claimHash(key, null));
    main.appendChild(who);
    main.appendChild(el('span', 'ac-item-meta mono', key.slice(0, 8) + (c.role ? ' · ' + c.role : '')));
    main.appendChild(el('span', 'ac-item-height', c.heightM == null ? 'Remove the owner height' : fmtHeight(c.heightM)));
    var chip = el('span', 'ac-chip', String(c.status || ''));
    chip.setAttribute('data-state', String(c.status || ''));
    main.appendChild(chip);
    if (c.needsReview) main.appendChild(el('span', 'ac-flag', 'Check: far from the surface estimate'));
    if (c.stale) main.appendChild(el('span', 'ac-flag', 'Moved ' + Math.round(c.movedM) + ' m since the proof: height not used'));
    var meta = 'Asked ' + fmtTime(c.createdAt);
    if (c.provenAt) meta += ' · proven ' + fmtTime(c.provenAt);
    if (c.reviewedAt) meta += ' · decided ' + fmtTime(c.reviewedAt) + (c.approvedBy ? ' (' + c.approvedBy + ')' : '');
    main.appendChild(el('span', 'ac-item-meta', meta));
    if (c.proofHash && /^[0-9a-f]+$/i.test(c.proofHash)) {
      var proof = el('a', 'ac-item-proof', 'Proof advert');
      proof.setAttribute('href', '#/packets/' + c.proofHash);
      main.appendChild(proof);
    }
    li.appendChild(main);
    var actions = el('div', 'ac-item-actions');
    decisionsFor(c.status).forEach(function (op) {
      var b = button(DECISION_LABEL[op], op === 'approve' ? 'btn btn-primary' : 'btn btn-secondary', { 'data-decide': op });
      b.setAttribute('aria-label', DECISION_LABEL[op] + ' ' + (c.name || c.pubkey || ''));
      b.addEventListener('click', function () {
        if (op === 'revoke' && b.getAttribute('data-confirm') !== 'yes') {
          b.setAttribute('data-confirm', 'yes');
          b.textContent = 'Confirm revoke';
          return;
        }
        decide(c, op, li);
      });
      actions.appendChild(b);
    });
    li.appendChild(actions);
    var status = el('div', 'ac-status ac-item-status');
    status.setAttribute('role', 'status');
    status.setAttribute('aria-live', 'polite');
    li.appendChild(status);
    return li;
  }

  function decide(c, op, li) {
    var buttons = li.querySelectorAll('button');
    Array.prototype.forEach.call(buttons, function (b) { b.disabled = true; });
    var who = c.name || 'the claim';
    admin.deciding = { id: c.id, text: who + ' ' + DECISION_DONE[op] };
    showDecision(c.id, DECISION_LABEL[op] + ' ' + who + '…', 'info');
    request('POST', '/admin/antenna-claims/' + encodeURIComponent(c.id) + '/' + op, { admin: true }).then(function (res) {
      if (!admin) return;
      admin.poller.start(res.requestId);
    }, function (err) {
      Array.prototype.forEach.call(buttons, function (b) { b.disabled = false; });
      if (err && (err.status === 401 || err.status === 403)) { onAdminError(err); return; }
      var text = decisionError(err);
      if (err && (err.status === 409 || err.status === 404) && err.message !== NEWER_APPROVED) {
        loadAdminList().then(function () { showDecision(c.id, text, 'error'); });
      } else {
        showDecision(c.id, text, 'error');
      }
    });
  }

  if (typeof root.registerPage === 'function') root.registerPage('antenna-claims-tool', { init: adminInit, destroy: adminDestroy });

  root.AntennaClaims = {
    NAME_BUDGET: NAME_BUDGET,
    MAX_POLL_ATTEMPTS: MAX_POLL_ATTEMPTS,
    FILTERS: FILTERS,
    parseHeight: parseHeight,
    utf8Length: utf8Length,
    checkName: checkName,
    nameCheckText: nameCheckText,
    giveUpText: giveUpText,
    statusInfo: statusInfo,
    pollDelay: pollDelay,
    createStatusPoller: createStatusPoller,
    claimParam: claimParam,
    claimHash: claimHash,
    parseFilter: parseFilter,
    adminHash: adminHash,
    decisionsFor: decisionsFor,
    decisionError: decisionError,
    mountOwnerSection: mountOwnerSection,
    renderAdminRow: renderAdminRow
  };
})(typeof window !== 'undefined' ? window : this);
