/* #789 M3 — the admin-key session the admin tools share (Channels →
 * suggestions, Tools → Antenna Height Claims).
 *
 * The key lives only in the session object's closure: never localStorage,
 * sessionStorage, a cookie, a global or the URL. It is gone after "Lock"
 * (clear) or a page reload. Each page creates its own session, so unlocking
 * one tool does not unlock another.
 *
 * request(method, path, opts) calls /api + path and resolves to the parsed
 * JSON body (null when empty); a non-2xx answer rejects with an Error whose
 * message is the server's {error} and whose status is the HTTP status.
 * opts.admin adds the key as X-API-Key, opts.body is sent as JSON and
 * opts.signal is passed through.
 */
(function (root) {
  'use strict';

  function create() {
    var key = null; // memory only

    function request(method, path, opts) {
      opts = opts || {};
      var headers = { 'Accept': 'application/json' };
      if (opts.body !== undefined) headers['Content-Type'] = 'application/json';
      if (opts.admin && key) headers['X-API-Key'] = key;
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

    return {
      set: function (k) { key = k ? String(k) : null; },
      clear: function () { key = null; },
      has: function () { return key !== null; },
      request: request
    };
  }

  root.MCAdminKey = { create: create };
})(typeof window !== 'undefined' ? window : this);
