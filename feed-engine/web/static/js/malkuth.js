/**
 * MALKUTH — Browser Cryptographic Substrate
 * version: 1.0 | status: stable
 *
 * Provides ECDSA-P256 work signing and SHA-256 content addressing.
 * Keys live in IndexedDB, namespaced per signed-in account. Private keys are
 * non-extractable.
 *
 * The browser never knows its PIAL. The PIAL is a person's root identity and is
 * never written into a page; the server binds every registration and every
 * signature to the session's PIAL itself. What the page knows is its account id
 * (<meta name="f33d3r:account">), and that is what namespaces the keys here and
 * what a signed work names as its author (author_account).
 *
 * Usage:
 *   await Malkuth.init(accountID)       — call once on page load
 *   await Malkuth.buildSignedWork(...)  — call before POST /events
 *
 * Key storage: IndexedDB "malkuth_v1" / store "keys" / id "signing_account_{accountID}"
 * Registration: POST /api/pial/signing-key/register on every init
 */

(function () {
  'use strict';

  // ─── Internal state ───────────────────────────────────────────────────────

  var _privateKey   = null;
  var _publicKeyB64 = null;

  // ─── Work text (mirror of feed-engine/internal/worktext, normalizer v1) ───
  //
  // The body is inside the signed content address, so the server never rewrites
  // it: it refuses a body that is not already normalized. buildSignedWork signs
  // normalize(body). NFC is applied here as well; the server adds the NFC check
  // in normalizer v2. weightedLength is the measure of the one body limit.

  var WORK_TEXT_VERSION   = 1;
  var WORK_MAX_WEIGHTED   = 2000;
  var WORK_URL_WEIGHT     = 23;
  var WORK_URL_RE         = /https?:\/\/[^\s"'<>\x00-\x1f]+/g;
  var WORK_URL_TRAILING   = /[.,;:!?)'"]+$/;
  var WORK_SPACE_RE       = /^[\t\n\v\f\r \u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]*$/;
  var WORK_TRIM_START_RE  = /^[\t\n\v\f\r \u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]+/;
  var WORK_TRIM_END_RE    = /[\t\n\v\f\r \u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]+$/;
  var WORK_MARK_RE        = /[\p{Mn}\p{Me}]/u;

  function workStripped(cp) {
    if (cp === 0x0A || cp === 0x09) return false;
    if (cp < 0x20 || cp === 0x7F || (cp >= 0x80 && cp <= 0x9F)) return true;
    if (cp === 0x061C || cp === 0x200E || cp === 0x200F ||
        (cp >= 0x202A && cp <= 0x202E) || (cp >= 0x2066 && cp <= 0x2069)) return true;
    if (cp === 0x00AD || cp === 0x180E || cp === 0x200B ||
        (cp >= 0x2060 && cp <= 0x2064) || (cp >= 0x206A && cp <= 0x206F) ||
        cp === 0x3164 || cp === 0xFFA0 || cp === 0xFEFF ||
        (cp >= 0xFFF9 && cp <= 0xFFFB)) return true;
    return false;
  }

  function workNormalize(s) {
    s = String(s || '').normalize('NFC').replace(/\r\n?/g, '\n');
    var kept = '';
    for (var ch of s) {
      if (!workStripped(ch.codePointAt(0))) kept += ch;
    }
    var out = [];
    var blank = false;
    kept.split('\n').forEach(function (line) {
      if (WORK_SPACE_RE.test(line)) {
        if (!blank) { out.push(''); }
        blank = true;
        return;
      }
      blank = false;
      out.push(line);
    });
    return out.join('\n').replace(WORK_TRIM_START_RE, '').replace(WORK_TRIM_END_RE, '');
  }

  function workRuneWeight(ch) {
    var cp = ch.codePointAt(0);
    if (WORK_MARK_RE.test(ch) || cp === 0x200C || cp === 0x200D ||
        (cp >= 0xFE00 && cp <= 0xFE0F) || (cp >= 0xE0100 && cp <= 0xE01EF) ||
        (cp >= 0x1F3FB && cp <= 0x1F3FF) || (cp >= 0xE0020 && cp <= 0xE007F)) return 0;
    if (cp <= 0x10FF || (cp >= 0x2000 && cp <= 0x200D) ||
        (cp >= 0x2010 && cp <= 0x201F) || (cp >= 0x2032 && cp <= 0x2037)) return 1;
    return 2;
  }

  function workTextWeight(s) {
    var n = 0;
    for (var ch of s) n += workRuneWeight(ch);
    return n;
  }

  function workWeightedLength(s) {
    var n = 0, last = 0, m;
    WORK_URL_RE.lastIndex = 0;
    while ((m = WORK_URL_RE.exec(s)) !== null) {
      var link = m[0].replace(WORK_URL_TRAILING, '');
      n += workTextWeight(s.slice(last, m.index)) + WORK_URL_WEIGHT;
      last = m.index + link.length;
    }
    return n + workTextWeight(s.slice(last));
  }

  window.F33WorkText = {
    version:        WORK_TEXT_VERSION,
    maxWeighted:    WORK_MAX_WEIGHTED,
    normalize:      workNormalize,
    weightedLength: workWeightedLength,
  };

  // ─── IndexedDB helpers ────────────────────────────────────────────────────

  function openDB() {
    return new Promise(function (resolve, reject) {
      var req = indexedDB.open('malkuth_v1', 1);
      req.onupgradeneeded = function (e) {
        var db = e.target.result;
        if (!db.objectStoreNames.contains('keys')) {
          db.createObjectStore('keys');
        }
      };
      req.onsuccess = function (e) { resolve(e.target.result); };
      req.onerror   = function (e) { reject(e.target.error); };
    });
  }

  function idbGet(db, key) {
    return new Promise(function (resolve, reject) {
      var tx  = db.transaction('keys', 'readonly');
      var req = tx.objectStore('keys').get(key);
      req.onsuccess = function (e) { resolve(e.target.result); };
      req.onerror   = function (e) { reject(e.target.error); };
    });
  }

  function idbPut(db, key, value) {
    return new Promise(function (resolve, reject) {
      var tx  = db.transaction('keys', 'readwrite');
      var req = tx.objectStore('keys').put(value, key);
      req.onsuccess = function () { resolve(); };
      req.onerror   = function (e) { reject(e.target.error); };
    });
  }

  function idbKeys(db) {
    return new Promise(function (resolve, reject) {
      var tx  = db.transaction('keys', 'readonly');
      var req = tx.objectStore('keys').getAllKeys();
      req.onsuccess = function (e) { resolve(e.target.result || []); };
      req.onerror   = function (e) { reject(e.target.error); };
    });
  }

  // adoptLegacyEcdhKey moves a pre-account ECDH key to its account name.
  //
  // Keys used to be stored as "ecdh_<pial>". The page no longer knows the PIAL
  // (it is never sent to a browser), so the stored key that belongs to this
  // person is recognised by its public half: it is the one the key authority
  // holds for them. Adopting it keeps every marketplace purchase wrapped to that
  // key decryptable. It must run before a new key is generated, because
  // registering a new key replaces the one the authority holds.
  //
  // Returns the adopted record, or null when this browser holds no matching key.
  async function adoptLegacyEcdhKey(db, storageID) {
    var resp = await fetch('/api/pial/ecdh-key', { credentials: 'same-origin' });
    if (resp.status === 404) return null;
    if (!resp.ok) throw new Error('ecdh key lookup failed: ' + resp.status);
    var registered = (await resp.json()).public_key_b64;
    var keys = await idbKeys(db);
    for (var i = 0; i < keys.length; i++) {
      var k = keys[i];
      if (typeof k !== 'string' || k.indexOf('ecdh_') !== 0 || k.indexOf('ecdh_account_') === 0) continue;
      var rec = await idbGet(db, k);
      if (rec && rec.privateKey && rec.publicKeyB64 === registered) {
        await idbPut(db, storageID, rec);
        return rec;
      }
    }
    return null;
  }

  // ─── Encoding helpers ─────────────────────────────────────────────────────

  function bufToBase64(buf) {
    return btoa(String.fromCharCode.apply(null, new Uint8Array(buf)));
  }

  function bufToBase64url(buf) {
    return bufToBase64(buf)
      .replace(/\+/g, '-')
      .replace(/\//g, '_')
      .replace(/=/g, '');
  }

  // ─── Key generation & storage ─────────────────────────────────────────────

  var ECDSA_PARAMS = { name: 'ECDSA', namedCurve: 'P-256' };

  async function generateKeyPair() {
    return crypto.subtle.generateKey(
      ECDSA_PARAMS,
      /* extractable for public only — private is non-extractable */
      false,
      ['sign', 'verify']
    );
  }

  // generateKey with extractable:false makes BOTH keys non-extractable.
  // We need the public key extractable for registration. The spec allows
  // generateKey with extractable:true and then we export only the public key;
  // the private key CryptoKey object stays in memory / IDB as an opaque handle.
  // However, IDB can only store CryptoKey objects directly when using structured
  // clone — no need to export the private key at all.
  async function generateAndStoreKeyPair(db, storageID) {
    // extractable:true lets us export the *public* key; the private key
    // CryptoKey itself is structured-cloned into IDB (never serialised to bytes).
    var kp = await crypto.subtle.generateKey(ECDSA_PARAMS, true, ['sign', 'verify']);

    // Export public key to SPKI → base64 for server registration
    var spkiBuf  = await crypto.subtle.exportKey('spki', kp.publicKey);
    var pubB64   = bufToBase64(spkiBuf);

    // Re-import the private key as non-extractable so it cannot be exported later
    // (structured clone into IDB preserves the extractable flag of the original key).
    // The safest approach: store the full CryptoKeyPair; browsers structured-clone it.
    await idbPut(db, storageID, { privateKey: kp.privateKey, publicKeyB64: pubB64 });

    return { privateKey: kp.privateKey, publicKeyB64: pubB64 };
  }

  // ─── Server registration ──────────────────────────────────────────────────

  async function registerPublicKey(pubKeyB64) {
    try {
      await fetch('/api/pial/signing-key/register', {
        method:  'POST',
        headers: { 'Content-Type': 'application/json' },
        body:    JSON.stringify({ public_key_b64: pubKeyB64, algorithm: 'ECDSA-P256' }),
      });
    } catch (err) {
      // Non-fatal: the key is available locally; registration will retry on next init.
      console.warn('[Malkuth] public-key registration failed — will retry on next init:', err);
    }
  }

  // ─── ECDH-P256 state (marketplace content encryption) ────────────────────

  var ECDH_PARAMS     = { name: 'ECDH', namedCurve: 'P-256' };
  var _ecdhPrivateKey = null;
  var _ecdhPubKeyB64  = null;

  // Cached Themis server ECDH public key (CryptoKey)
  var _themisEcdhPubKey = null;

  // The key arrives as a rendered attribute on the listing form Facet
  // (data-themis-pubkey, written by the server from its own Themis call); the
  // browser reads what the server drew and asks no JSON endpoint.
  async function _getThemisEcdhPubKey() {
    if (_themisEcdhPubKey) return _themisEcdhPubKey;
    var carrier = document.querySelector('[data-themis-pubkey]');
    var b64 = carrier ? carrier.getAttribute('data-themis-pubkey') : '';
    if (!b64) throw new Error('Themis public key is not available — the marketplace is offline');
    var raw  = _base64ToBuffer(b64);
    _themisEcdhPubKey = await crypto.subtle.importKey(
      'raw', raw, ECDH_PARAMS, false, []
    );
    return _themisEcdhPubKey;
  }

  function _base64ToBuffer(b64) {
    var bin = atob(b64);
    var buf = new Uint8Array(bin.length);
    for (var i = 0; i < bin.length; i++) buf[i] = bin.charCodeAt(i);
    return buf.buffer;
  }

  // ─── Public API ───────────────────────────────────────────────────────────

  window.Malkuth = {

    /**
     * Load or generate the ECDSA-P256 signing key pair from IndexedDB.
     * Registers the public key with the server if newly generated.
     * Returns { publicKeyB64, hasKey: true } on success, { hasKey: false } on failure.
     */
    init: async function (accountID) {
      if (!accountID) {
        console.warn('[Malkuth] init called without an account id — skipping');
        return { hasKey: false };
      }

      if (!window.crypto || !window.crypto.subtle) {
        console.warn('[Malkuth] WebCrypto not available in this context');
        return { hasKey: false };
      }

      var storageID = 'signing_account_' + accountID;
      var db = null;

      try {
        db = await openDB();
      } catch (err) {
        console.warn('[Malkuth] IndexedDB unavailable — falling back to in-memory key:', err);
        // Fall back: generate a transient key pair (lost on reload)
        try {
          var kp = await crypto.subtle.generateKey(ECDSA_PARAMS, true, ['sign', 'verify']);
          var spkiBuf = await crypto.subtle.exportKey('spki', kp.publicKey);
          _privateKey   = kp.privateKey;
          _publicKeyB64 = bufToBase64(spkiBuf);
          await registerPublicKey(_publicKeyB64);
          return { publicKeyB64: _publicKeyB64, hasKey: true };
        } catch (genErr) {
          console.warn('[Malkuth] in-memory key generation failed:', genErr);
          return { hasKey: false };
        }
      }

      // Try to load existing key from IDB
      try {
        var stored = await idbGet(db, storageID);
        if (stored && stored.privateKey && stored.publicKeyB64) {
          _privateKey   = stored.privateKey;
          _publicKeyB64 = stored.publicKeyB64;
          // Re-register on every load so the key authority always holds the key
          // this page signs with for the session's identity.
          await registerPublicKey(_publicKeyB64);
          return { publicKeyB64: _publicKeyB64, hasKey: true };
        }
      } catch (loadErr) {
        console.warn('[Malkuth] failed to load key from IDB — regenerating:', loadErr);
      }

      // Generate a new key pair, store it, and register with the server
      try {
        var generated = await generateAndStoreKeyPair(db, storageID);
        _privateKey   = generated.privateKey;
        _publicKeyB64 = generated.publicKeyB64;
        await registerPublicKey(_publicKeyB64);
        return { publicKeyB64: _publicKeyB64, hasKey: true };
      } catch (genErr) {
        console.warn('[Malkuth] key generation failed:', genErr);
        return { hasKey: false };
      }
    },

    /**
     * Compute SHA-256 CID of a canonical work payload.
     *
     * The object literal key order MUST match the Go workCanonicalPayload struct
     * field declaration order exactly — Go marshals struct fields in declaration
     * order, and JS objects maintain insertion order in modern engines. Both sides
     * must produce byte-for-byte identical JSON.
     *
     * Keys are in alphabetical order by JSON name:
     *   author_account, body, comment_gating, is_repost, kind, media_urls,
     *   parent_cid, poll_ends_at, poll_options, repost_source_id, scheduled_at,
     *   subscriber_only, tags, timestamp_ms, video_duration_secs,
     *   video_master_url, video_poster_url, voice_duration_secs, voice_url
     *
     * Returns "sha256:{hex}"
     */
    computeCID: async function (payload) {
      var canonical = JSON.stringify({
        author_account:     payload.author_account,
        body:               payload.body               || '',
        comment_gating:     payload.comment_gating     || 'everyone',
        is_repost:          payload.is_repost           ? true : false,
        kind:               payload.kind               || 'post',
        media_urls:         (payload.media_urls        || []).slice().sort(),
        parent_cid:         payload.parent_cid         || null,
        poll_ends_at:       payload.poll_ends_at       || null,
        poll_options:       payload.poll_options       || null,
        repost_source_id:   payload.repost_source_id   || null,
        scheduled_at:       payload.scheduled_at       || null,
        subscriber_only:    payload.subscriber_only     ? true : false,
        tags:               (payload.tags              || []).slice(),
        timestamp_ms:       payload.timestamp_ms,
        video_duration_secs: payload.video_duration_secs || null,
        video_master_url:   payload.video_master_url   || null,
        video_poster_url:   payload.video_poster_url   || null,
        voice_duration_secs: payload.voice_duration_secs || null,
        voice_url:          payload.voice_url          || null,
      });
      var encoded = new TextEncoder().encode(canonical);
      var hashBuf = await crypto.subtle.digest('SHA-256', encoded);
      var hex = Array.from(new Uint8Array(hashBuf))
        .map(function (b) { return b.toString(16).padStart(2, '0'); })
        .join('');
      return 'sha256:' + hex;
    },

    /**
     * Sign a CID string with this account's signing key.
     * Returns base64url-encoded signature string.
     */
    sign: async function (cid) {
      if (!_privateKey) {
        throw new Error('[Malkuth] sign() called before successful init()');
      }
      var data = new TextEncoder().encode(cid);
      var sig  = await crypto.subtle.sign(
        { name: 'ECDSA', hash: { name: 'SHA-256' } },
        _privateKey,
        data
      );
      return bufToBase64url(sig);
    },

    /**
     * Build a complete signed work envelope ready to POST to /events.
     *
     * @param {string}  eventType  — e.g. 'post', 'reply', 'quote', 'repost'
     * @param {string}  body       — post text body
     * @param {string}  kind       — content kind (post|reply|quote|repost|thread_part|vision|voice|video)
     * @param {string[]} mediaURLs — attached media URL array
     * @param {string|null} parentCID — CID of parent work for replies; null otherwise
     * @param {object}  [opts]     — optional extended fields:
     *   tags            {string[]}    — hashtags (default [])
     *   poll_options    {string[]|null} — poll option labels (default null)
     *   poll_ends_at    {string|null} — RFC3339 expiry for poll (default null)
     *   scheduled_at    {string|null} — RFC3339 publish time; null = immediate (default null)
     *   comment_gating  {string}      — 'everyone'|'followers'|'circle'|'verified'|'none' (default 'everyone')
     *   subscriber_only {boolean}     — subscriber-only post (default false)
     *   (is_repost / repost_source_id are always false / null: a repost is a
     *    reaction, and the server refuses a signed one)
     *   voice_url       {string|null} — URL of voice audio (default null)
     *   voice_duration_secs {number|null} — duration in seconds (default null)
     *   video_master_url {string|null}  — HLS master playlist URL (default null)
     *   video_poster_url {string|null}  — video poster image URL (default null)
     *   video_duration_secs {number|null} — video duration in seconds (default null)
     *   is_nsfw         {boolean}     — 18+ adult content flag (default false)
     *
     * Returns { event_type, cid, signature, is_nsfw, payload }
     */
    // ─── ECDH-P256 for marketplace content encryption ─────────────────────────

    /**
     * Initialise ECDH key for marketplace content encryption.
     * Loads from IndexedDB if present, otherwise generates and registers.
     * Call once per page load on marketplace pages.
     *
     * @param {string} accountID — the signed-in account's id
     * @returns {{ publicKeyB64: string, hasKey: boolean }}
     */
    initEcdh: async function(accountID) {
      if (!accountID) return { hasKey: false };
      var storageID = 'ecdh_account_' + accountID;
      var db;
      try { db = await openDB(); } catch(e) { db = null; }

      if (db) {
        try {
          var stored = await idbGet(db, storageID);
          if (stored && stored.privateKey && stored.publicKeyB64) {
            _ecdhPrivateKey = stored.privateKey;
            _ecdhPubKeyB64  = stored.publicKeyB64;
            fetch('/api/pial/ecdh-key/register', {
              method: 'POST',
              headers: {'Content-Type':'application/json'},
              body: JSON.stringify({public_key_b64: _ecdhPubKeyB64})
            }).catch(function(){});
            return { publicKeyB64: _ecdhPubKeyB64, hasKey: true };
          }
        } catch(e) {
          console.error('[malkuth] reading the stored ECDH key failed:', e);
        }
        try {
          var adopted = await adoptLegacyEcdhKey(db, storageID);
          if (adopted) {
            _ecdhPrivateKey = adopted.privateKey;
            _ecdhPubKeyB64  = adopted.publicKeyB64;
            return { publicKeyB64: _ecdhPubKeyB64, hasKey: true };
          }
        } catch(e) {
          // Generating a key now would replace the one the authority holds and
          // strand every purchase wrapped to it. Stop instead; the next page
          // load tries again.
          console.error('[malkuth] could not check for this person\'s existing ECDH key:', e);
          return { hasKey: false };
        }
      }

      // Generate new ECDH keypair
      var kp = await crypto.subtle.generateKey(ECDH_PARAMS, true, ['deriveBits']);
      var raw = await crypto.subtle.exportKey('raw', kp.publicKey);
      _ecdhPubKeyB64  = bufToBase64(raw);
      _ecdhPrivateKey = kp.privateKey;

      if (db) {
        try { await idbPut(db, storageID, { privateKey: kp.privateKey, publicKeyB64: _ecdhPubKeyB64 }); } catch(e) {}
      }

      try {
        await fetch('/api/pial/ecdh-key/register', {
          method: 'POST',
          headers: {'Content-Type':'application/json'},
          body: JSON.stringify({public_key_b64: _ecdhPubKeyB64})
        });
      } catch(e) {}

      return { publicKeyB64: _ecdhPubKeyB64, hasKey: true };
    },

    /**
     * Encrypt a File object for listing creation using Themis's ECDH public key.
     * Returns { ciphertext: Blob, cek_encrypted_b64: string, content_hash: string }
     *
     * @param {File} file — the content file to encrypt
     */
    encryptContent: async function(file) {
      var themisKey = await _getThemisEcdhPubKey();

      // Generate CEK (content encryption key)
      var cekKey = await crypto.subtle.generateKey({name:'AES-GCM',length:256}, true, ['encrypt','decrypt']);
      var cekRaw = await crypto.subtle.exportKey('raw', cekKey);

      // Encrypt file content
      var fileBytes = await file.arrayBuffer();
      var iv = crypto.getRandomValues(new Uint8Array(12));
      var ciphertext = await crypto.subtle.encrypt({name:'AES-GCM',iv:iv}, cekKey, fileBytes);

      // ECIES: wrap CEK for Themis
      var ephemeral = await crypto.subtle.generateKey(ECDH_PARAMS, true, ['deriveBits']);
      var sharedBits = await crypto.subtle.deriveBits({name:'ECDH', public:themisKey}, ephemeral.privateKey, 256);
      var hkdfKey = await crypto.subtle.importKey('raw', sharedBits, 'HKDF', false, ['deriveKey']);
      var wrapKey = await crypto.subtle.deriveKey(
        {name:'HKDF', hash:'SHA-256', salt: new Uint8Array(32), info: new TextEncoder().encode('themis-cek-wrap-v1')},
        hkdfKey, {name:'AES-GCM',length:256}, false, ['encrypt']
      );
      var cekIv = crypto.getRandomValues(new Uint8Array(12));
      var cekCiphertext = await crypto.subtle.encrypt({name:'AES-GCM',iv:cekIv}, wrapKey, cekRaw);
      var ephPubRaw = await crypto.subtle.exportKey('raw', ephemeral.publicKey);

      var cekEncryptedObj = {
        ephemeral_pub: bufToBase64(ephPubRaw),
        iv:            bufToBase64(cekIv),
        ciphertext:    bufToBase64(cekCiphertext),
      };

      // Content hash: SHA-256 of full ciphertext (iv prepended)
      var combined = new Uint8Array(iv.byteLength + ciphertext.byteLength);
      combined.set(iv, 0);
      combined.set(new Uint8Array(ciphertext), iv.byteLength);
      var hashBuf = await crypto.subtle.digest('SHA-256', combined);
      var hashHex = Array.from(new Uint8Array(hashBuf)).map(function(b){return b.toString(16).padStart(2,'0');}).join('');

      return {
        ciphertext:        new Blob([iv, ciphertext]),  // iv prepended to ciphertext
        cek_encrypted_b64: bufToBase64(new TextEncoder().encode(JSON.stringify(cekEncryptedObj))),
        content_hash:      'sha256:' + hashHex,
      };
    },

    /**
     * Decrypt content after purchase.
     * content_url points to the encrypted blob (iv || ciphertext).
     * cek_for_buyer_b64 is base64 of the ECIES-wrapped CEK delivered via SSE.
     *
     * @param {string} cek_for_buyer_b64 — base64 of wrapped CEK JSON
     * @param {string} content_url — URL of encrypted blob
     * @param {string} content_hash — sha256: hash for integrity verification
     * @returns {ArrayBuffer} — decrypted content
     */
    decryptContent: async function(cek_for_buyer_b64, content_url, content_hash) {
      if (!_ecdhPrivateKey) throw new Error('[Malkuth] ECDH key not initialized');

      // Download encrypted blob
      var resp = await fetch(content_url);
      var blob = await resp.arrayBuffer();

      // Parse wrapped CEK
      var cekWrappedBytes = _base64ToBuffer(cek_for_buyer_b64);
      var cekWrapped = JSON.parse(new TextDecoder().decode(cekWrappedBytes));
      var ephPubRaw = _base64ToBuffer(cekWrapped.ephemeral_pub);
      var cekIv     = _base64ToBuffer(cekWrapped.iv);
      var cekCipher = _base64ToBuffer(cekWrapped.ciphertext);

      // ECDH shared secret
      var ephPubKey = await crypto.subtle.importKey('raw', ephPubRaw, ECDH_PARAMS, false, []);
      var sharedBits = await crypto.subtle.deriveBits({name:'ECDH', public:ephPubKey}, _ecdhPrivateKey, 256);
      var hkdfKey = await crypto.subtle.importKey('raw', sharedBits, 'HKDF', false, ['deriveKey']);
      var unwrapKey = await crypto.subtle.deriveKey(
        {name:'HKDF', hash:'SHA-256', salt: new Uint8Array(32), info: new TextEncoder().encode('themis-cek-wrap-v1')},
        hkdfKey, {name:'AES-GCM',length:256}, false, ['decrypt']
      );
      var cekRaw = await crypto.subtle.decrypt({name:'AES-GCM', iv:cekIv}, unwrapKey, cekCipher);
      var cekKey = await crypto.subtle.importKey('raw', cekRaw, {name:'AES-GCM',length:256}, false, ['decrypt']);

      // Verify content hash
      var hashBuf = await crypto.subtle.digest('SHA-256', blob);
      var hashHex = Array.from(new Uint8Array(hashBuf)).map(function(b){return b.toString(16).padStart(2,'0');}).join('');
      if ('sha256:' + hashHex !== content_hash) throw new Error('[Malkuth] content hash mismatch');

      // Decrypt: first 12 bytes are IV, rest is ciphertext
      var blobArr = new Uint8Array(blob);
      var fileIv   = blobArr.slice(0, 12);
      var cipher   = blobArr.slice(12);
      var plaintext = await crypto.subtle.decrypt({name:'AES-GCM', iv:fileIv}, cekKey, cipher);

      return plaintext;  // ArrayBuffer of decrypted content
    },

    /**
     * Sign a marketplace event payload and return the payload augmented with cid + pial_sig.
     * Call this for all high-value marketplace mutations before POSTing to /events.
     *
     * @param {object} payload — the event object (must include event_type)
     * @returns {object} — payload with cid and pial_sig fields added
     */
    signMarketplaceEvent: async function(payload) {
      if (!_privateKey) {
        // Signing key not initialised — return unsigned (Themis will reject high-value ops)
        return payload;
      }
      // Canonical CID: sha256 of deterministic JSON (sorted keys, no whitespace)
      var canonical = JSON.stringify(payload, Object.keys(payload).sort());
      var encoded = new TextEncoder().encode(canonical);
      var hashBuf = await crypto.subtle.digest('SHA-256', encoded);
      var hex = Array.from(new Uint8Array(hashBuf))
        .map(function(b) { return b.toString(16).padStart(2, '0'); })
        .join('');
      var cid = 'sha256:' + hex;
      var sig = await Malkuth.sign(cid);
      return Object.assign({}, payload, { cid: cid, pial_sig: sig });
    },

    buildSignedWork: async function (eventType, body, kind, mediaURLs, parentCID, opts) {
      var o = opts || {};
      // The author is named by account. The server checks it is the session's
      // account and verifies the signature against the session's own PIAL key,
      // so the page never needs — and is never given — the PIAL.
      var accountID = document.querySelector('meta[name="f33d3r:account"]')?.content || '';
      // The signed body and poll options are the normalized text: the server
      // refuses anything else, because it can never rewrite what was signed.
      var pollOptions = o.poll_options ? o.poll_options.map(workNormalize) : null;
      var payload = {
        author_account:      accountID,
        body:                workNormalize(body),
        comment_gating:      o.comment_gating             || 'everyone',
        is_repost:           false,
        kind:                kind                          || 'post',
        media_urls:          mediaURLs                    || [],
        parent_cid:          parentCID                    || null,
        poll_ends_at:        o.poll_ends_at               || null,
        poll_options:        pollOptions,
        repost_source_id:    null,
        scheduled_at:        o.scheduled_at               || null,
        subscriber_only:     o.subscriber_only             ? true : false,
        tags:                o.tags                       || [],
        timestamp_ms:        Date.now(),
        video_duration_secs: o.video_duration_secs        || null,
        video_master_url:    o.video_master_url           || null,
        video_poster_url:    o.video_poster_url           || null,
        voice_duration_secs: o.voice_duration_secs        || null,
        voice_url:           o.voice_url                  || null,
      };
      var cid       = await this.computeCID(payload);
      var signature = await this.sign(cid);
      return {
        event_type:            eventType,
        cid:                   cid,
        signature:             signature,
        is_nsfw:               o.is_nsfw ? true : false,
        // The downloadable copy is the server's, handed out by the download
        // route behind the work's wall; the client never names a file.
        // video_width/video_height are transcoder-derived metadata, not user content.
        // NOT in the CID payload — sent alongside the envelope so the server can store them.
        video_width:           o.video_width  || 0,
        video_height:          o.video_height || 0,
        // quoted_work_id is NOT in the CID payload — server resolves it to a CID for work_citations.
        quoted_work_id:        o.quoted_work_id || null,
        // react_layout is NOT in the CID payload — view-time arrangement metadata for React With Video.
        react_layout:          o.react_layout || null,
        // downloads_allowed is NOT in the CID payload — the author's per-work
        // download switch, on unless turned off.
        downloads_allowed:     o.downloads_allowed === false ? false : true,
        payload:               payload,
      };
    },
  };

  // ── Global auto-init ────────────────────────────────────────────────────────
  // When a session is signed in, both signing and ECDH keys are always ready
  // on every page — not deferred to marketplace-specific code.
  document.addEventListener('DOMContentLoaded', async function () {
    var accountMeta = document.querySelector('meta[name="f33d3r:account"]');
    if (!accountMeta || !accountMeta.content) return;
    var accountID = accountMeta.content;
    // Both awaited so keys are registered before any interaction is possible.
    await Malkuth.init(accountID);
    await Malkuth.initEcdh(accountID);
  });

})();
