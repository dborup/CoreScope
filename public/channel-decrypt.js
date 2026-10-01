/**
 * Client-side MeshCore channel decryption module.
 *
 * Implements the same crypto as internal/channel/channel.go:
 *   - Key derivation: SHA-256("#channelname")[:16]
 *   - Channel hash: SHA-256(key)[0]
 *   - MAC: HMAC-SHA256 with 32-byte secret (key + 16 zero bytes), truncated to 2 bytes
 *   - Encryption: AES-128-ECB (block-by-block)
 *   - Plaintext: timestamp(4 LE) + flags(1) + "sender: message\0"
 *
 * Keys NEVER leave the browser. No fetch/XHR/network calls in this module.
 */
/* eslint-disable no-var */
window.ChannelDecrypt = (function () {
  'use strict';

  var STORAGE_KEY = 'corescope_channel_keys';
  var LABELS_KEY = 'corescope_channel_labels';
  var CACHE_KEY = 'corescope_channel_cache';

  // ---- Hex utilities ----

  function bytesToHex(bytes) {
    var hex = '';
    for (var i = 0; i < bytes.length; i++) {
      hex += (bytes[i] < 16 ? '0' : '') + bytes[i].toString(16);
    }
    return hex;
  }

  function hexToBytes(hex) {
    var bytes = new Uint8Array(hex.length / 2);
    for (var i = 0; i < hex.length; i += 2) {
      bytes[i / 2] = parseInt(hex.substring(i, i + 2), 16);
    }
    return bytes;
  }

  // ---- Key derivation ----

  // Detect whether SubtleCrypto is available. SubtleCrypto is only exposed
  // in **secure contexts** (HTTPS or localhost) — when CoreScope is served
  // over plain HTTP, `crypto.subtle` is undefined and any digest/HMAC call
  // throws. We fall back to the vendored pure-JS implementation in
  // public/vendor/sha256-hmac.js. PR #1021 did the same for AES-ECB.
  function hasSubtle() {
    return typeof crypto !== 'undefined' && crypto && crypto.subtle && typeof crypto.subtle.digest === 'function';
  }

  function pureCryptoOrThrow() {
    var host = (typeof window !== 'undefined') ? window
             : (typeof self !== 'undefined') ? self : null;
    if (!host || !host.PureCrypto || !host.PureCrypto.sha256 || !host.PureCrypto.hmacSha256) {
      throw new Error('PureCrypto vendor module not loaded (public/vendor/sha256-hmac.js). ' +
        'crypto.subtle is unavailable (HTTP context) and no fallback present.');
    }
    return host.PureCrypto;
  }

  /**
   * Derive AES-128 key from channel name: SHA-256("#channelname")[:16].
   * @param {string} channelName - e.g. "#LongFast"
   * @returns {Promise<Uint8Array>} 16-byte key
   */
  async function deriveKey(channelName) {
    var enc = new TextEncoder();
    var data = enc.encode(channelName);
    if (hasSubtle()) {
      var hash = await crypto.subtle.digest('SHA-256', data);
      return new Uint8Array(hash).slice(0, 16);
    }
    return pureCryptoOrThrow().sha256(data).slice(0, 16);
  }

  /**
   * Compute the 1-byte channel hash: SHA-256(key)[0].
   * @param {Uint8Array} key - 16-byte key
   * @returns {Promise<number>} single byte (0-255)
   */
  async function computeChannelHash(key) {
    if (hasSubtle()) {
      var hash = await crypto.subtle.digest('SHA-256', key);
      return new Uint8Array(hash)[0];
    }
    return pureCryptoOrThrow().sha256(key)[0];
  }

  // ---- AES-128-ECB via vendored pure-JS implementation ----
  //
  // Web Crypto exposes AES-CBC/CTR/GCM but NOT raw AES-ECB. The previous
  // implementation simulated ECB with AES-CBC + zero IV + a dummy PKCS7
  // padding block; that hack throws OperationError on real ciphertext
  // because Web Crypto validates PKCS7 padding on the decrypted output
  // and the dummy padding bytes rarely form a valid PKCS7 sequence
  // after decryption. We use a pure-JS AES-128 ECB core
  // (public/vendor/aes-ecb.js, MIT, derived from aes-js by Richard
  // Moore) so decryption is deterministic across browsers and works in
  // HTTP contexts.

  /**
   * Decrypt AES-128-ECB.
   * @param {Uint8Array} key - 16-byte AES key
   * @param {Uint8Array} ciphertext - must be a non-zero multiple of 16 bytes
   * @returns {Promise<Uint8Array|null>} plaintext, or null on invalid input
   */
  async function decryptECB(key, ciphertext) {
    if (!ciphertext || ciphertext.length === 0 || ciphertext.length % 16 !== 0) {
      return null;
    }
    var host = (typeof window !== 'undefined') ? window
             : (typeof self !== 'undefined') ? self : null;
    if (!host || !host.AES_ECB || !host.AES_ECB.decrypt) {
      throw new Error('AES_ECB vendor module not loaded (public/vendor/aes-ecb.js)');
    }
    return host.AES_ECB.decrypt(key, ciphertext);
  }

  // ---- MAC verification ----

  /**
   * Verify HMAC-SHA256 MAC (first 2 bytes) using 32-byte secret (key + 16 zero bytes).
   * @param {Uint8Array} key - 16-byte AES key
   * @param {Uint8Array} ciphertext - encrypted data
   * @param {string} macHex - 4-char hex string (2 bytes)
   * @returns {Promise<boolean>}
   */
  async function verifyMAC(key, ciphertext, macHex) {
    // Build 32-byte channel secret: key + 16 zero bytes
    var secret = new Uint8Array(32);
    secret.set(key, 0);
    // remaining 16 bytes are already 0

    var macBytes = hexToBytes(macHex);
    var sigBytes;
    if (hasSubtle() && typeof crypto.subtle.importKey === 'function' && typeof crypto.subtle.sign === 'function') {
      var cryptoKey = await crypto.subtle.importKey(
        'raw', secret, { name: 'HMAC', hash: 'SHA-256' }, false, ['sign']
      );
      var sig = await crypto.subtle.sign('HMAC', cryptoKey, ciphertext);
      sigBytes = new Uint8Array(sig);
    } else {
      sigBytes = pureCryptoOrThrow().hmacSha256(secret, ciphertext);
    }
    return sigBytes[0] === macBytes[0] && sigBytes[1] === macBytes[1];
  }

  // ---- Plaintext parsing ----

  /**
   * Parse decrypted plaintext: timestamp(4 LE) + flags(1) + "sender: message\0..."
   * @param {Uint8Array} plaintext
   * @returns {{ timestamp: number, flags: number, sender: string, message: string } | null}
   */
  function parsePlaintext(plaintext) {
    if (!plaintext || plaintext.length < 5) return null;

    var timestamp = plaintext[0] | (plaintext[1] << 8) | (plaintext[2] << 16) | ((plaintext[3] << 24) >>> 0);
    var flags = plaintext[4];

    // Extract text up to first null byte
    var textBytes = plaintext.slice(5);
    var nullIdx = -1;
    for (var i = 0; i < textBytes.length; i++) {
      if (textBytes[i] === 0) { nullIdx = i; break; }
    }
    var text = new TextDecoder().decode(nullIdx >= 0 ? textBytes.slice(0, nullIdx) : textBytes);

    // Count non-printable characters
    var nonPrintable = 0;
    for (var c = 0; c < text.length; c++) {
      var code = text.charCodeAt(c);
      if (code < 32 && code !== 10 && code !== 13 && code !== 9) nonPrintable++;
    }
    if (nonPrintable > 2) return null;

    // Parse "sender: message" format
    var colonIdx = text.indexOf(': ');
    if (colonIdx > 0 && colonIdx < 50) {
      var potentialSender = text.substring(0, colonIdx);
      if (potentialSender.indexOf(':') < 0 && potentialSender.indexOf('[') < 0 && potentialSender.indexOf(']') < 0) {
        return { timestamp: timestamp, flags: flags, sender: potentialSender, message: text.substring(colonIdx + 2) };
      }
    }

    return { timestamp: timestamp, flags: flags, sender: '', message: text };
  }

  // ---- Full decrypt pipeline ----

  /**
   * Verify MAC, decrypt, and parse a single packet.
   * @param {Uint8Array} keyBytes - 16-byte key
   * @param {string} macHex - 4-char hex MAC
   * @param {string} encryptedHex - hex-encoded ciphertext
   * @returns {Promise<{ sender: string, message: string, timestamp: number } | null>}
   */
  async function decrypt(keyBytes, macHex, encryptedHex) {
    var ciphertext = hexToBytes(encryptedHex);
    if (ciphertext.length === 0 || ciphertext.length % 16 !== 0) return null;

    var macOk = await verifyMAC(keyBytes, ciphertext, macHex);
    if (!macOk) return null;

    var plaintext = await decryptECB(keyBytes, ciphertext);
    if (!plaintext) return null;

    return parsePlaintext(plaintext);
  }

  // Alias used by channels.js
  var decryptPacket = decrypt;

  // ---- Live PSK decrypt (WS path) ----
  //
  // Build a Map<channelHashByte, { channelName, keyBytes, keyHex }> from all
  // stored PSK keys so the WebSocket handler can do an O(1) lookup on each
  // incoming GRP_TXT packet. Hash byte derivation is async, so we cache the
  // map between calls and only rebuild when the stored-keys set changes.
  var _keyMapCache = null;
  var _keyMapSig = '';

  function _keysSignature(keys) {
    var names = Object.keys(keys).sort();
    var sig = '';
    for (var i = 0; i < names.length; i++) {
      sig += names[i] + '=' + keys[names[i]] + ';';
    }
    return sig;
  }

  async function buildKeyMap() {
    var keys = getKeys();
    var sig = _keysSignature(keys);
    if (_keyMapCache && _keyMapSig === sig) return _keyMapCache;
    var map = new Map();
    var names = Object.keys(keys);
    for (var i = 0; i < names.length; i++) {
      var channelName = names[i];
      var keyHex = keys[channelName];
      if (!keyHex || typeof keyHex !== 'string') continue;
      var keyBytes;
      try { keyBytes = hexToBytes(keyHex); } catch (e) { continue; }
      if (keyBytes.length !== 16) continue;
      var hashByte;
      try { hashByte = await computeChannelHash(keyBytes); } catch (e) { continue; }
      // First-write-wins on collision (rare): different channel names can
      // hash to the same byte. The downstream MAC check still gates rendering.
      if (!map.has(hashByte)) {
        map.set(hashByte, { channelName: channelName, keyBytes: keyBytes, keyHex: keyHex });
      }
    }
    _keyMapCache = map;
    _keyMapSig = sig;
    return map;
  }

  /**
   * Attempt to decrypt a live GRP_TXT payload using a prebuilt key map.
   * Returns { sender, text, channelName, channelHashByte } on success,
   * or null when no key matches, MAC verification fails, or the payload
   * is not an encrypted GRP_TXT.
   */
  async function tryDecryptLive(payload, keyMap) {
    if (!payload || payload.type !== 'GRP_TXT') return null;
    if (!payload.encryptedData || !payload.mac) return null;
    if (!keyMap || typeof keyMap.get !== 'function') return null;
    var hashByte = payload.channelHash;
    // channelHash arrives as either a number or a hex string in some paths;
    // normalize to number so Map.get hits.
    if (typeof hashByte === 'string') {
      var n = parseInt(hashByte, 16);
      if (!isFinite(n)) return null;
      hashByte = n;
    }
    if (typeof hashByte !== 'number') return null;
    var entry = keyMap.get(hashByte);
    if (!entry) return null;
    var result;
    try {
      result = await decrypt(entry.keyBytes, payload.mac, payload.encryptedData);
    } catch (e) { return null; }
    if (!result) return null;
    return {
      sender: result.sender || 'Unknown',
      text: result.message || '',
      channelName: entry.channelName,
      channelHashByte: hashByte,
      timestamp: result.timestamp || null
    };
  }


  // ---- Key storage (localStorage) ----

  function saveKey(channelName, keyHex, label) {
    var keys = getKeys();
    keys[channelName] = keyHex;
    setItemMakingRoom(STORAGE_KEY, JSON.stringify(keys));
    _keyMapCache = null; // invalidate live-decrypt index
    if (typeof label === 'string' && label.trim()) {
      saveLabel(channelName, label.trim());
    }
  }

  // Alias used by channels.js
  var storeKey = saveKey;

  function getKeys() {
    try {
      var raw = localStorage.getItem(STORAGE_KEY);
      return raw ? JSON.parse(raw) : {};
    } catch (e) { return {}; }
  }

  // Alias used by channels.js
  var getStoredKeys = getKeys;

  function removeKey(channelName) {
    var keys = getKeys();
    delete keys[channelName];
    setItemMakingRoom(STORAGE_KEY, JSON.stringify(keys));
    _keyMapCache = null; // invalidate live-decrypt index
    // Also clear cached messages and any label for this channel (#1020)
    clearChannelCache(channelName);
    var labels = getLabels();
    if (labels[channelName]) {
      delete labels[channelName];
      setItemMakingRoom(LABELS_KEY, JSON.stringify(labels));
    }
  }

  // ---- User-supplied display labels (#1020) ----
  // Stored separately from keys so we can display friendly names instead of
  // psk:<hex8> for user-added PSK channels.
  function getLabels() {
    try {
      var raw = localStorage.getItem(LABELS_KEY);
      return raw ? JSON.parse(raw) : {};
    } catch (e) { return {}; }
  }

  function getLabel(channelName) {
    var labels = getLabels();
    return labels[channelName] || '';
  }

  function saveLabel(channelName, label) {
    var labels = getLabels();
    if (typeof label === 'string' && label.trim()) {
      labels[channelName] = label.trim();
    } else {
      delete labels[channelName];
    }
    setItemMakingRoom(LABELS_KEY, JSON.stringify(labels));
  }

  // N2 (#152 follow-up): decrypted messages are cached per channel AND per
  // region selection, as "<channel>|<sorted regions>" ("<channel>|" for all
  // regions). Region order doesn't change which observers are included, so
  // it must not change the key either.
  var CACHE_REGION_SEP = '|';

  function channelCacheKey(channelName, regionParam) {
    var regions = regionParam ? String(regionParam).split(',').filter(Boolean).sort().join(',') : '';
    return channelName + CACHE_REGION_SEP + regions;
  }

  // ---- Message cache (localStorage) ----
  //
  // One JSON blob under CACHE_KEY: { "<channel>|<regions>": entry }. It is
  // only a cache, so it always gives way: it is kept under a fixed
  // character budget, a quota failure evicts and retries, and keys and
  // labels evict it before they would fail to save.

  // Cache with lastTimestamp and count (used by channels.js via getCache/setCache)
  var MAX_CACHED_MESSAGES = 1000;
  // N2 (#152 follow-up): keys are region-scoped, so one channel can occupy
  // several entries. Cap the number of distinct entries so visiting many
  // region combinations over time can't grow the blob unboundedly.
  var MAX_CACHE_KEYS = 50;
  // R4-2 (#153 review round 4): one entry can reach ~365 KiB (1000
  // messages) and localStorage holds ~5.2M characters per origin, shared
  // with the keys and labels. Keep the whole blob under this many
  // characters, least recently used entries going first.
  var CACHE_BUDGET_CHARS = 1500000;
  // Set once the pre-N2 entries (keyed by channel name alone) are dropped.
  var CACHE_VERSION_KEY = 'corescope_channel_cache_v';
  var CACHE_VERSION = '2';

  var _cacheMigrated = false;
  var _cacheUseSeq = 0;
  // Last-read stamps of this page load, folded into the entries' `at` on
  // the next write so a read doesn't cost a rewrite of the whole blob.
  var _cacheReadAt = {};

  // Strictly increasing, so entries written or read in the same
  // millisecond still have a defined least-recently-used order.
  function nextCacheUse() {
    _cacheUseSeq = Math.max(Date.now(), _cacheUseSeq + 1);
    return _cacheUseSeq;
  }

  function cacheLastUse(cache, key) {
    var entry = cache[key] || {};
    return Math.max(entry.at || entry.ts || 0, _cacheReadAt[key] || 0);
  }

  function readCacheBlob() {
    try {
      var cache = JSON.parse(localStorage.getItem(CACHE_KEY) || '{}');
      return (cache && typeof cache === 'object') ? cache : {};
    } catch (e) { return {}; }
  }

  // Drop the pre-N2 entries once: they are keyed by channel name alone, so
  // nothing reads them any more, and removeKey() of a later version would
  // have no reason to look for them.
  function ensureCacheMigrated() {
    if (_cacheMigrated) return;
    _cacheMigrated = true;
    try {
      if (localStorage.getItem(CACHE_VERSION_KEY) === CACHE_VERSION) return;
    } catch (e) { return; }
    var cache = readCacheBlob();
    var legacy = Object.keys(cache).filter(function (k) { return k.indexOf(CACHE_REGION_SEP) === -1; });
    if (legacy.length) {
      legacy.forEach(function (k) { delete cache[k]; });
      writeCacheBlob(cache);
    }
    try { localStorage.setItem(CACHE_VERSION_KEY, CACHE_VERSION); } catch (e) { /* retried next load */ }
  }

  /**
   * Persist `cache`, evicting least recently used entries until it is within
   * MAX_CACHE_KEYS and CACHE_BUDGET_CHARS and localStorage accepts it. Each
   * entry is serialised once; setItem() either stores the whole new blob or
   * leaves the old one, so the blob is never half-written. Returns whether
   * the blob was written.
   */
  function writeCacheBlob(cache) {
    var parts = {};
    var size = 2; // "{}"
    var keys = Object.keys(cache);
    keys.forEach(function (k) {
      if (cache[k] && _cacheReadAt[k]) cache[k].at = cacheLastUse(cache, k);
      parts[k] = JSON.stringify(k) + ':' + JSON.stringify(cache[k]);
      size += parts[k].length + 1; // + separating comma
    });
    _cacheReadAt = {}; // folded into the entries above
    keys.sort(function (a, b) { return cacheLastUse(cache, a) - cacheLastUse(cache, b); });
    while (keys.length > MAX_CACHE_KEYS || (keys.length && size - 1 > CACHE_BUDGET_CHARS)) {
      size -= parts[keys.shift()].length + 1;
    }
    for (;;) {
      try {
        localStorage.setItem(CACHE_KEY, '{' + keys.map(function (k) { return parts[k]; }).join(',') + '}');
        return true;
      } catch (e) {
        // QuotaExceededError: localStorage is shared with the keys and
        // labels, so give up room until the blob fits.
        if (!keys.length) break;
        keys.shift();
      }
    }
    try { localStorage.removeItem(CACHE_KEY); } catch (e) { /* nothing cached */ }
    return false;
  }

  // Drop the least recently used cache entry (or the empty blob itself).
  // Returns false once there is no cache left to give up.
  function evictOldestCacheEntry() {
    var raw;
    try { raw = localStorage.getItem(CACHE_KEY); } catch (e) { return false; }
    if (raw === null) return false;
    var cache = readCacheBlob();
    var keys = Object.keys(cache);
    if (!keys.length) {
      try { localStorage.removeItem(CACHE_KEY); } catch (e) { return false; }
      return true;
    }
    keys.sort(function (a, b) { return cacheLastUse(cache, a) - cacheLastUse(cache, b); });
    delete cache[keys[0]];
    writeCacheBlob(cache);
    return true;
  }

  // setItem() for the keys and labels: when the quota is hit, evict decrypt
  // cache until the write fits, so the cache can never cost the user a key.
  function setItemMakingRoom(storageKey, value) {
    for (;;) {
      try {
        localStorage.setItem(storageKey, value);
        return true;
      } catch (e) {
        if (!evictOldestCacheEntry()) return false;
      }
    }
  }

  /**
   * Remove every cached message set of a channel (by name or hash): each
   * region-scoped "<channel>|<regions>" entry plus a pre-N2 "<channel>" one.
   */
  function clearChannelCache(channelKey) {
    ensureCacheMigrated();
    var cache = readCacheBlob();
    var prefix = channelKey + CACHE_REGION_SEP;
    Object.keys(cache).forEach(function (k) {
      if (k === channelKey || k.indexOf(prefix) === 0) delete cache[k];
    });
    writeCacheBlob(cache);
  }

  function cacheMessages(channelHash, messages) {
    ensureCacheMigrated();
    var cache = readCacheBlob();
    cache[channelHash] = { messages: messages, ts: Date.now(), at: nextCacheUse() };
    writeCacheBlob(cache);
  }

  function getCachedMessages(channelHash) {
    var entry = getCache(channelHash);
    return entry ? entry.messages : null;
  }

  function setCache(key, messages, lastTimestamp, totalCount) {
    ensureCacheMigrated();
    // Enforce cache size limit: only keep most recent MAX_CACHED_MESSAGES
    var toStore = messages;
    if (messages.length > MAX_CACHED_MESSAGES) {
      toStore = messages.slice(messages.length - MAX_CACHED_MESSAGES);
    }
    var cache = readCacheBlob();
    cache[key] = {
      messages: toStore,
      lastTimestamp: lastTimestamp,
      count: totalCount || toStore.length,
      ts: Date.now(),
      at: nextCacheUse()
    };
    writeCacheBlob(cache);
  }

  function getCache(key) {
    ensureCacheMigrated();
    var entry = readCacheBlob()[key] || null;
    if (entry) _cacheReadAt[key] = nextCacheUse();
    return entry;
  }

  return {
    deriveKey: deriveKey,
    decrypt: decrypt,
    decryptPacket: decryptPacket,
    decryptECB: decryptECB,
    verifyMAC: verifyMAC,
    parsePlaintext: parsePlaintext,
    computeChannelHash: computeChannelHash,
    bytesToHex: bytesToHex,
    hexToBytes: hexToBytes,
    saveKey: saveKey,
    storeKey: storeKey,
    getKeys: getKeys,
    getStoredKeys: getStoredKeys,
    removeKey: removeKey,
    // #1020: optional user-friendly display labels for stored keys
    saveLabel: saveLabel,
    getLabel: getLabel,
    getLabels: getLabels,
    channelCacheKey: channelCacheKey,
    clearChannelCache: clearChannelCache,
    cacheMessages: cacheMessages,
    getCachedMessages: getCachedMessages,
    setCache: setCache,
    getCache: getCache,
    buildKeyMap: buildKeyMap,
    tryDecryptLive: tryDecryptLive
  };
})();
