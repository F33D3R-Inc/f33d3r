// ══════════════════════════════════════════════════════════════════════════════
// VOVIN — End-to-End Encrypted Messaging
//
// Crypto stack:
//   GL  — Glyph ECDSA-P256 signing key (device identity binding)
//   DR  — Double Ratchet over WebCrypto ECDH-P256
//   VC  — ECDH-P256 vault (identity keys, wrapping)
//   IDB — IndexedDB persistence, namespaced per PIAL UUID
//
// Session protocol (simplified Signal without SPK distribution):
//   1. Sender fetches recipient's identity public key from Vovin.
//   2. SK = ECDH(sender_ident_priv, recip_ident_pub) — shared secret.
//   3. Sender generates fresh DR keypair (alice_DR).
//   4. CK_s = KDF_RK(SK, DH(alice_DR_priv, recip_ident_pub)).ck
//   5. Sender includes dh_pub=alice_DR_pub + sender_pub=sender_ident_pub in message.
//   6. Receiver: SK = ECDH(recip_ident_priv, sender_ident_pub) — same secret.
//   7. CK_r = KDF_RK(SK, DH(recip_ident_priv, alice_DR_pub)).ck — matches CK_s!
//   8. Subsequent messages: full Double Ratchet with fresh DH pairs each step.
// ══════════════════════════════════════════════════════════════════════════════

// ── Glyph: ECDSA-P256 signing key ────────────────────────────────────────────
const GL = {
  async genKeyPair() {
    return crypto.subtle.generateKey(
      {name:'ECDSA', namedCurve:'P-256'}, true, ['sign','verify']
    );
  },
  async exportPubSpki(k) {
    const r = await crypto.subtle.exportKey('spki', k);
    return btoa(String.fromCharCode(...new Uint8Array(r)));
  },
  async exportPrivJwk(k) { return crypto.subtle.exportKey('jwk', k); },
  async importPriv(jwk) {
    return crypto.subtle.importKey('jwk', jwk, {name:'ECDSA',namedCurve:'P-256'}, false, ['sign']);
  },
  async sign(privKey, data) {
    const sig = await crypto.subtle.sign(
      {name:'ECDSA', hash:{name:'SHA-256'}},
      privKey,
      typeof data === 'string' ? new TextEncoder().encode(data) : data
    );
    return btoa(String.fromCharCode(...new Uint8Array(sig)));
  }
};

// ── Double Ratchet primitives ─────────────────────────────────────────────────
const DR = {
  async kdf_ck(ck_bytes) {
    const ikm = await crypto.subtle.importKey('raw', ck_bytes, {name:'HKDF'}, false, ['deriveBits']);
    const out = await crypto.subtle.deriveBits(
      {name:'HKDF', hash:'SHA-256', salt: new Uint8Array(32), info: new TextEncoder().encode('AMP_CK_v1')},
      ikm, 512
    );
    return { ck: new Uint8Array(out, 0, 32), mk: new Uint8Array(out, 32, 32) };
  },

  async kdf_rk(rk_bytes, dh_bytes) {
    const combined = new Uint8Array(rk_bytes.length + dh_bytes.length);
    combined.set(rk_bytes); combined.set(dh_bytes, rk_bytes.length);
    const ikm = await crypto.subtle.importKey('raw', combined, {name:'HKDF'}, false, ['deriveBits']);
    const out = await crypto.subtle.deriveBits(
      {name:'HKDF', hash:'SHA-256', salt: rk_bytes, info: new TextEncoder().encode('AMP_RK_v1')},
      ikm, 512
    );
    return { rk: new Uint8Array(out, 0, 32), ck: new Uint8Array(out, 32, 32) };
  },

  async encrypt(mk_bytes, plaintext) {
    const mk  = await crypto.subtle.importKey('raw', mk_bytes, {name:'AES-GCM'}, false, ['encrypt']);
    const iv  = crypto.getRandomValues(new Uint8Array(12));
    const ct  = await crypto.subtle.encrypt({name:'AES-GCM', iv}, mk, new TextEncoder().encode(plaintext));
    return { iv: btoa(String.fromCharCode(...iv)), ct: btoa(String.fromCharCode(...new Uint8Array(ct))) };
  },

  async decrypt(mk_bytes, iv_b64, ct_b64) {
    const mk  = await crypto.subtle.importKey('raw', mk_bytes, {name:'AES-GCM'}, false, ['decrypt']);
    const iv  = Uint8Array.from(atob(iv_b64), c => c.charCodeAt(0));
    const ct  = Uint8Array.from(atob(ct_b64), c => c.charCodeAt(0));
    const pt  = await crypto.subtle.decrypt({name:'AES-GCM', iv}, mk, ct);
    return new TextDecoder().decode(pt);
  },

  async dh(myPrivKey, theirPubKey) {
    const bits = await crypto.subtle.deriveBits({name:'ECDH', public: theirPubKey}, myPrivKey, 256);
    return new Uint8Array(bits);
  },

  async genDHKeyPair() {
    return crypto.subtle.generateKey({name:'ECDH',namedCurve:'P-256'}, true, ['deriveBits']);
  },
  async exportDHPub(k) {
    const r = await crypto.subtle.exportKey('raw', k);
    return btoa(String.fromCharCode(...new Uint8Array(r)));
  },
  async importDHPub(b64) {
    const r = Uint8Array.from(atob(b64), c => c.charCodeAt(0));
    return crypto.subtle.importKey('raw', r, {name:'ECDH',namedCurve:'P-256'}, false, []);
  },
  async exportDHPrivJwk(k) { return crypto.subtle.exportKey('jwk', k); },
  async importDHPriv(jwk) {
    return crypto.subtle.importKey('jwk', jwk, {name:'ECDH',namedCurve:'P-256'}, false, ['deriveBits']);
  },
};

// ── Vovin WebCrypto Layer ─────────────────────────────────────────────────────
const VC = {
  async genKeyPair() {
    return crypto.subtle.generateKey({name:'ECDH',namedCurve:'P-256'}, true, ['deriveBits']);
  },
  async exportPub(k) {
    const r = await crypto.subtle.exportKey('raw', k);
    return btoa(String.fromCharCode(...new Uint8Array(r)));
  },
  async exportPrivJwk(k) { return crypto.subtle.exportKey('jwk', k); },
  async importPriv(jwk) {
    return crypto.subtle.importKey('jwk', jwk, {name:'ECDH',namedCurve:'P-256'}, false, ['deriveBits']);
  },
  async importPub(b64) {
    const r = Uint8Array.from(atob(b64), c => c.charCodeAt(0));
    return crypto.subtle.importKey('raw', r, {name:'ECDH',namedCurve:'P-256'}, false, []);
  },
  async pbkdf2Key(passphrase, salt) {
    const km = await crypto.subtle.importKey('raw', new TextEncoder().encode(passphrase), {name:'PBKDF2'}, false, ['deriveBits']);
    const bits = await crypto.subtle.deriveBits({name:'PBKDF2', salt, iterations:210000, hash:'SHA-256'}, km, 256);
    return crypto.subtle.importKey('raw', bits, {name:'AES-GCM'}, false, ['encrypt','decrypt']);
  },
  async wrap(jwk, wk) {
    const iv = crypto.getRandomValues(new Uint8Array(12));
    const ct = await crypto.subtle.encrypt({name:'AES-GCM',iv}, wk, new TextEncoder().encode(JSON.stringify(jwk)));
    return {iv:[...iv], ct:[...new Uint8Array(ct)]};
  },
  async unwrap(w, wk) {
    const p = await crypto.subtle.decrypt({name:'AES-GCM',iv:new Uint8Array(w.iv)}, wk, new Uint8Array(w.ct));
    return JSON.parse(new TextDecoder().decode(p));
  },
  async sharedBytesRaw(myPriv, theirPub) {
    return new Uint8Array(await crypto.subtle.deriveBits({name:'ECDH', public:theirPub}, myPriv, 256));
  },
  async sharedKey(myPriv, theirPub) {
    const bits = await crypto.subtle.deriveBits({name:'ECDH', public:theirPub}, myPriv, 256);
    return crypto.subtle.importKey('raw', bits, {name:'AES-GCM'}, false, ['encrypt','decrypt']);
  },
  async encrypt(sk, text) {
    const iv = crypto.getRandomValues(new Uint8Array(12));
    const ct = await crypto.subtle.encrypt({name:'AES-GCM',iv}, sk, new TextEncoder().encode(text));
    return {iv:btoa(String.fromCharCode(...iv)), ct:btoa(String.fromCharCode(...new Uint8Array(ct)))};
  },
  async decrypt(sk, iv_b64, ct_b64) {
    const d = await crypto.subtle.decrypt(
      {name:'AES-GCM', iv:Uint8Array.from(atob(iv_b64), c=>c.charCodeAt(0))},
      sk, Uint8Array.from(atob(ct_b64), c=>c.charCodeAt(0)));
    return new TextDecoder().decode(d);
  }
};

// ── IndexedDB vault — namespaced per PIAL ─────────────────────────────────────
const MY_PIAL   = document.querySelector('meta[name="f33d3r:pial"]')?.content || '';
const MY_HANDLE = document.querySelector('meta[name="f33d3r:handle"]')?.content || '';
const IDB_NAME  = MY_PIAL ? `vovin_v1_${MY_PIAL}` : 'vovin_v1_unknown';

const DEVICE_ID = (() => {
  const key = `f33d3r_device_${MY_PIAL}`;
  let id = localStorage.getItem(key);
  if (!id) {
    id = crypto.randomUUID ? crypto.randomUUID() : Math.random().toString(36).slice(2);
    localStorage.setItem(key, id);
  }
  return id;
})();

const IDB = {
  _open() {
    return new Promise((res, rej) => {
      const r = indexedDB.open(IDB_NAME, 1);
      r.onupgradeneeded = e => e.target.result.createObjectStore('kv');
      r.onsuccess = e => res(e.target.result);
      r.onerror = e => rej(e.target.error);
    });
  },
  async get(k) {
    const db = await this._open();
    return new Promise((res, rej) => {
      const r = db.transaction('kv','readonly').objectStore('kv').get(k);
      r.onsuccess = e => res(e.target.result); r.onerror = e => rej(e.target.error);
    });
  },
  async set(k, v) {
    const db = await this._open();
    return new Promise((res, rej) => {
      const r = db.transaction('kv','readwrite').objectStore('kv').put(v, k);
      r.onsuccess = () => res(); r.onerror = e => rej(e.target.error);
    });
  },
  async clear() {
    const db = await this._open();
    return new Promise((res, rej) => {
      const tx = db.transaction('kv', 'readwrite');
      tx.objectStore('kv').clear();
      tx.oncomplete = res; tx.onerror = e => rej(e.target.error);
    });
  }
};

// ── State ─────────────────────────────────────────────────────────────────────
let VS = {locked:true, method:null, privKey:null, pubB64:null, identId:null};
let activeConvo  = null;
let receivedMsgs = {};   // pialId → [DmResp]
let sentCache    = {};   // pialId → [{text, ts}]

const _seenMsgIds   = new Set();
const _identCache   = {};  // pialId → identity_public_key_b64 (cached Vovin lookups)
const _ratchetCache = {};  // pialId → session (in-memory, also persisted to IDB)

async function persistSentCache() {
  try { await IDB.set('sent_cache', sentCache); } catch(_) {}
}
async function loadSentCache() {
  try {
    const saved = await IDB.get('sent_cache');
    if (saved && typeof saved === 'object') sentCache = saved;
  } catch(_) {}
}

// ── Ratchet session storage ───────────────────────────────────────────────────
async function getRatchetSession(pialId) {
  if (_ratchetCache[pialId]) return _ratchetCache[pialId];
  try {
    const stored = await IDB.get('ratchet_' + pialId);
    if (stored) { _ratchetCache[pialId] = stored; return stored; }
  } catch(_) {}
  return null;
}

async function saveRatchetSession(pialId, session) {
  _ratchetCache[pialId] = session;
  try { await IDB.set('ratchet_' + pialId, session); } catch(_) {}
  // Persist encrypted blob to server (fire-and-forget)
  if (VS.identId && VS.privKey && VS.pubB64) {
    try {
      const sk  = await VC.sharedKey(VS.privKey, await VC.importPub(VS.pubB64));
      const enc = await VC.encrypt(sk, JSON.stringify(session));
      fetch('/vovin/v1/ratchet/' + encodeURIComponent(VS.identId) + '/' + encodeURIComponent(pialId), {
        method: 'POST',
        headers: {'Content-Type':'application/json'},
        body: JSON.stringify({local_identity: VS.identId, remote_identity: pialId, state_b64: btoa(JSON.stringify(enc))})
      }).catch(() => {});
    } catch(_) {}
  }
}

// ── encryptDR — encrypt plaintext using Double Ratchet ────────────────────────
// Creates or uses the existing DR session.
// On first use: sender generates fresh DR keypair, derives CK_s from ECDH shared secret.
async function encryptDR(pialId, remoteIdentPubB64, plaintext) {
  if (!VS.privKey) return null;
  let session = await getRatchetSession(pialId);

  if (!session || session.version !== 2) {
    // ── Init new session as sender ──
    const remoteIdentPub = await VC.importPub(remoteIdentPubB64);
    const sk = await VC.sharedBytesRaw(VS.privKey, remoteIdentPub);

    // Fresh DH keypair for this ratchet
    const myDRKP    = await DR.genDHKeyPair();
    const myDRPub   = await DR.exportDHPub(myDRKP.publicKey);
    const myDRPrivJwk = await DR.exportDHPrivJwk(myDRKP.privateKey);

    // CK_s = KDF_RK(SK, DH(my_DR_priv, remote_ident_pub)).ck
    // Receiver will compute: CK_r = KDF_RK(SK, DH(remote_ident_priv, my_DR_pub)).ck = same!
    const dh_out = await DR.dh(myDRKP.privateKey, remoteIdentPub);
    const {rk, ck} = await DR.kdf_rk(sk, dh_out);

    session = {
      rk:          Array.from(rk),
      ck_s:        Array.from(ck),
      ck_r:        null,
      n_s: 0, n_r: 0, prev_n_s: 0,
      dh_pub_b64:  myDRPub,
      dh_priv_jwk: myDRPrivJwk,
      dh_remote:   remoteIdentPubB64,  // initial remote key = their identity key
      initiator:   true,
      version:     2
    };
    await saveRatchetSession(pialId, session);
  }

  // If no sending chain (e.g. we've only been receiving), do a DH ratchet step
  if (!session.ck_s) {
    if (!session.dh_remote || !session.dh_priv_jwk) return null;
    const remPub  = await DR.importDHPub(session.dh_remote);
    const myPriv  = await DR.importDHPriv(session.dh_priv_jwk);
    const dh      = await DR.dh(myPriv, remPub);
    const {rk: rk1} = await DR.kdf_rk(new Uint8Array(session.rk), dh);

    const newKP    = await DR.genDHKeyPair();
    const newPub   = await DR.exportDHPub(newKP.publicKey);
    const newPrivJwk = await DR.exportDHPrivJwk(newKP.privateKey);
    const dh2      = await DR.dh(newKP.privateKey, remPub);
    const {rk: rk2, ck: ck_s} = await DR.kdf_rk(rk1, dh2);

    session.rk          = Array.from(rk2);
    session.ck_s        = Array.from(ck_s);
    session.prev_n_s    = session.n_s;
    session.n_s         = 0;
    session.dh_pub_b64  = newPub;
    session.dh_priv_jwk = newPrivJwk;
    await saveRatchetSession(pialId, session);
  }

  const {ck: new_ck_s, mk} = await DR.kdf_ck(new Uint8Array(session.ck_s));
  const enc = await DR.encrypt(mk, plaintext);

  session.ck_s = Array.from(new_ck_s);
  session.n_s++;
  await saveRatchetSession(pialId, session);

  return {
    ct:          enc.ct,
    iv:          enc.iv,
    ratchet_pub: session.dh_pub_b64,
    version:     2
  };
}

// ── decryptDR — decrypt a V2 message using Double Ratchet ─────────────────────
// Handles: fresh session init from first message, DH ratchet steps, chain advance.
async function decryptDR(pialId, msg) {
  if (!msg.ratchet_pub || !msg.ciphertext || !msg.iv) return null;
  if (!VS.privKey) return null;

  let session = await getRatchetSession(pialId);

  if (!session || session.version !== 2) {
    // ── Init as receiver from first message ──
    if (!msg.sender_pub) return null;
    try {
      const senderIdentPub = await VC.importPub(msg.sender_pub);
      const sk = await VC.sharedBytesRaw(VS.privKey, senderIdentPub);

      // CK_r = KDF_RK(SK, DH(my_ident_priv, sender_DR_pub)).ck
      // Mirror of what sender computed: DH(sender_DR_priv, my_ident_pub) = same!
      const senderDRPub = await DR.importDHPub(msg.ratchet_pub);
      const dh_r = await DR.dh(VS.privKey, senderDRPub);
      const {rk: rk1, ck: ck_r} = await DR.kdf_rk(new Uint8Array(sk), dh_r);

      // Build reply chain immediately: KDF_RK(rk1, DH(my_new_DR_priv, sender_DR_pub))
      const myDRKP    = await DR.genDHKeyPair();
      const myDRPub   = await DR.exportDHPub(myDRKP.publicKey);
      const myDRPrivJwk = await DR.exportDHPrivJwk(myDRKP.privateKey);
      const dh_s = await DR.dh(myDRKP.privateKey, senderDRPub);
      const {rk: rk2, ck: ck_s} = await DR.kdf_rk(rk1, dh_s);

      session = {
        rk:          Array.from(rk2),
        ck_s:        Array.from(ck_s),
        ck_r:        Array.from(ck_r),
        n_s: 0, n_r: 0, prev_n_s: 0,
        dh_pub_b64:  myDRPub,
        dh_priv_jwk: myDRPrivJwk,
        dh_remote:   msg.ratchet_pub,
        initiator:   false,
        version:     2
      };
      await saveRatchetSession(pialId, session);
    } catch(e) {
      console.warn('decryptDR init failed:', e);
      return null;
    }
  } else if (session.dh_remote !== msg.ratchet_pub) {
    // ── DH ratchet step: sender generated a new DR keypair ──
    try {
      const remPub  = await DR.importDHPub(msg.ratchet_pub);
      const myPriv  = await DR.importDHPriv(session.dh_priv_jwk);
      const dh      = await DR.dh(myPriv, remPub);
      const {rk: rk1, ck: new_ck_r} = await DR.kdf_rk(new Uint8Array(session.rk), dh);

      const newKP    = await DR.genDHKeyPair();
      const newPub   = await DR.exportDHPub(newKP.publicKey);
      const newPrivJwk = await DR.exportDHPrivJwk(newKP.privateKey);
      const dh2      = await DR.dh(newKP.privateKey, remPub);
      const {rk: rk2, ck: new_ck_s} = await DR.kdf_rk(rk1, dh2);

      session.prev_n_s    = session.n_s;
      session.n_s         = 0;
      session.n_r         = 0;
      session.rk          = Array.from(rk2);
      session.ck_r        = Array.from(new_ck_r);
      session.ck_s        = Array.from(new_ck_s);
      session.dh_remote   = msg.ratchet_pub;
      session.dh_pub_b64  = newPub;
      session.dh_priv_jwk = newPrivJwk;
    } catch(e) {
      console.warn('decryptDR ratchet step failed:', e);
      return null;
    }
  }

  if (!session.ck_r) return null;

  try {
    const {ck: new_ck_r, mk} = await DR.kdf_ck(new Uint8Array(session.ck_r));
    const pt = await DR.decrypt(mk, msg.iv, msg.ciphertext);
    session.ck_r = Array.from(new_ck_r);
    session.n_r++;
    await saveRatchetSession(pialId, session);
    return pt;
  } catch(e) {
    console.warn('decryptDR decrypt failed:', e);
    return null;
  }
}

// ── WebSocket push ─────────────────────────────────────────────────────────────
let _ws = null, _wsRetry = 0;

function connectWS() {
  if (!VS.identId || !VS.privKey) return;
  if (_ws && _ws.readyState <= 1) return;
  const scheme = location.protocol === 'https:' ? 'wss:' : 'ws:';
  _ws = new WebSocket(`${scheme}//${location.host}/vovin/v1/push/${encodeURIComponent(VS.identId)}`);
  _ws.onopen  = () => {
    _wsRetry = 0;
    const el = get('vault-strip-text');
    if (el) el.textContent = 'Vault unlocked — live';
  };
  _ws.onmessage = async e => {
    try { const ev = JSON.parse(e.data); if (ev.type === 'new_message') await refreshDMs(); } catch(_) {}
  };
  _ws.onclose = () => {
    const d = Math.min(1000 * (2 ** Math.min(_wsRetry, 5)), 30000);
    _wsRetry++;
    setTimeout(connectWS, d);
  };
  _ws.onerror = () => _ws.close();
}

async function refreshDMs() {
  if (!VS.identId) return;
  try {
    const r = await fetch(`/vovin/v1/dm/${encodeURIComponent(VS.identId)}?device_id=${encodeURIComponent(DEVICE_ID)}`);
    if (!r.ok) return;
    const msgs = await r.json();
    if (!msgs || !msgs.length) return;
    let updated = false;
    msgs.forEach(m => {
      const id = String(m.id);
      if (_seenMsgIds.has(id)) return;
      _seenMsgIds.add(id);
      const key = m.sender;
      if (!receivedMsgs[key]) receivedMsgs[key] = [];
      receivedMsgs[key].push(m);
      updated = true;
    });
    if (updated) {
      buildConvList();
      if (activeConvo && receivedMsgs[activeConvo.pialId]) await renderThread(activeConvo.pialId);
    }
  } catch(e) { console.warn('refreshDMs:', e); }
}

// ── Boot ──────────────────────────────────────────────────────────────────────
async function boot() {
  if (!MY_PIAL) {
    setStrip('Identity not ready', false);
    setVaultInfo('setup');
    get('vault-info').innerHTML = '<p style="color:#ef4444;font-size:12px;margin-bottom:10px">Your PIAL identity is still being set up.</p><button onclick="location.reload()" class="btn-primary" style="font-size:12px;padding:7px 14px;width:100%">Reload page</button>';
    return;
  }
  const meta = await IDB.get('meta');
  if (!meta) { show('ov-setup'); show('step-method'); setStrip('Setup required', false); setVaultInfo('setup'); return; }
  VS.method = meta.method; VS.pubB64 = meta.pubB64; VS.identId = meta.identId || MY_PIAL;
  if (meta.method === 'none') {
    const jwk = await IDB.get('pk_plain');
    if (jwk) { VS.privKey = await VC.importPriv(jwk); await onUnlocked(); }
    else { await resetVault(); }
  } else {
    show('ov-unlock');
    if (meta.method !== 'passkey') get('unlock-passkey-row').style.display = 'none';
    setStrip('Vault locked — tap to unlock', false); setVaultInfo('locked');
  }
}

// ── Setup ─────────────────────────────────────────────────────────────────────
function backToMethod() { hide('step-passphrase'); show('step-method'); }
function showPassphraseStep() { hide('step-method'); show('step-passphrase'); }

async function setupPasskey() {
  if (!navigator.credentials) { alert('Passkeys not supported here. Use passphrase.'); return; }
  try {
    await navigator.credentials.create({publicKey:{
      challenge: crypto.getRandomValues(new Uint8Array(32)),
      rp: {name:'F33D3R Vovin', id:location.hostname},
      user: {id:new TextEncoder().encode(MY_PIAL), name:MY_HANDLE, displayName:MY_HANDLE},
      pubKeyCredParams: [{alg:-7,type:'public-key'},{alg:-257,type:'public-key'}],
      authenticatorSelection: {authenticatorAttachment:'platform',userVerification:'required',residentKey:'required'},
      timeout: 60000
    }});
    await doGenerate('passkey', null);
  } catch(e) { alert('Passkey setup: '+e.message); }
}

async function confirmPassphrase() {
  const p1 = get('pp1').value, p2 = get('pp2').value;
  get('pp-err').textContent = '';
  if (p1.length < 8) { get('pp-err').textContent = 'Min 8 characters'; return; }
  if (p1 !== p2) { get('pp-err').textContent = 'Passphrases do not match'; return; }
  await doGenerate('passphrase', p1);
}

async function setupNone() { await doGenerate('none', null); }

async function doGenerate(method, passphrase) {
  hide('step-method'); hide('step-passphrase'); show('step-generating');
  try {
    if (!window.crypto || !window.crypto.subtle) {
      const isLocalIP = location.hostname !== 'localhost' && location.hostname !== '127.0.0.1';
      if (isLocalIP) {
        throw new Error('Encryption unavailable on ' + location.hostname + '. Open http://localhost:' + location.port + '/messages on this machine instead.');
      } else {
        throw new Error('Your browser does not support WebCrypto. Please use Chrome, Firefox, or Safari 14+.');
      }
    }
    await _doGenerateInner(method, passphrase);
  } catch(e) {
    console.error('doGenerate failed:', e);
    setMsg('gen-msg', '❌ ' + (e.message || String(e)));
    await new Promise(r => setTimeout(r, 6000));
    hide('step-generating'); show('step-method');
  }
}

async function _doGenerateInner(method, passphrase) {
  setMsg('gen-msg', 'Generating ECDH identity keypair…');
  const kp = await VC.genKeyPair();
  const pub = await VC.exportPub(kp.publicKey);
  const privJwk = await VC.exportPrivJwk(kp.privateKey);

  setMsg('gen-msg', 'Generating Glyph identity key…');
  let glyphPubB64 = null;
  try {
    const glyphKP = await GL.genKeyPair();
    glyphPubB64   = await GL.exportPubSpki(glyphKP.publicKey);
    const glyphPrivJwk = await GL.exportPrivJwk(glyphKP.privateKey);
    if (method === 'passphrase' && passphrase) {
      const salt = crypto.getRandomValues(new Uint8Array(16));
      const wk   = await VC.pbkdf2Key(passphrase, salt);
      const w    = await VC.wrap(glyphPrivJwk, wk);
      await IDB.set('glyph_priv_wrapped', {salt:[...salt], ...w});
    } else {
      await IDB.set('glyph_priv_plain', glyphPrivJwk);
    }
    await IDB.set('glyph_pub_b64', glyphPubB64);
  } catch(e) { console.warn('Glyph key gen failed (non-fatal):', e.message); }

  setMsg('gen-msg', 'Registering device with Vovin…');
  try {
    const r = await fetch('/vovin/v1/devices', {
      method:'POST', headers:{'Content-Type':'application/json'},
      body: JSON.stringify({
        pial_id:        MY_PIAL,
        device_id:      DEVICE_ID,
        public_key_b64: pub,
        glyph_pub_b64:  glyphPubB64 || null,
        handle:         MY_HANDLE,
        device_name:    navigator.userAgent.slice(0, 60),
      })
    });
    if (!r.ok) {
      const errBody = await r.text().catch(()=>'');
      setMsg('gen-msg', '❌ Device registration failed (' + r.status + '): ' + (errBody || 'messaging service error'));
      await new Promise(res => setTimeout(res, 3000));
      hide('step-generating'); show('step-method');
      return;
    }
    // Legacy identity endpoint (backward compat)
    fetch('/vovin/v1/identity', {
      method:'POST', headers:{'Content-Type':'application/json'},
      body: JSON.stringify({ identity: MY_PIAL, public_key_b64: pub, handle: MY_HANDLE, device_id: DEVICE_ID })
    }).catch(() => {});
  } catch(e) {
    setMsg('gen-msg', '❌ Could not reach Vovin: ' + e.message);
    await new Promise(res => setTimeout(res, 3000));
    hide('step-generating'); show('step-method');
    return;
  }

  if (glyphPubB64) {
    fetch('/vovin/v1/identity/' + encodeURIComponent(MY_PIAL) + '/glyph', {
      method: 'POST', headers: {'Content-Type':'application/json'},
      body: JSON.stringify({ identity: MY_PIAL, public_key_b64: pub, glyph_public_key: glyphPubB64, glyph_algorithm: 'ECDSA-P256' })
    }).catch(() => {});
  }

  setMsg('gen-msg', 'Encrypting key vault…');
  if (method === 'passphrase' && passphrase) {
    const salt = crypto.getRandomValues(new Uint8Array(16));
    const wk = await VC.pbkdf2Key(passphrase, salt);
    const w = await VC.wrap(privJwk, wk);
    await IDB.set('pk_wrapped', {salt:[...salt], ...w});
  } else if (method === 'passkey') {
    const seed = crypto.getRandomValues(new Uint8Array(32));
    const wk = await VC.pbkdf2Key(btoa(String.fromCharCode(...seed)), new Uint8Array(16));
    const w = await VC.wrap(privJwk, wk);
    await IDB.set('pk_passkey', {seed:[...seed], ...w});
  } else {
    await IDB.set('pk_plain', privJwk);
  }
  await IDB.set('meta', {method, pubB64:pub, identId:MY_PIAL});
  VS = {locked:false, method, privKey:kp.privateKey, pubB64:pub, identId:MY_PIAL};

  setMsg('gen-msg', 'Done!');
  await new Promise(r => setTimeout(r, 500));
  hide('ov-setup');
  await onUnlocked();
}

// ── Unlock ────────────────────────────────────────────────────────────────────
async function unlockPasskey() {
  try {
    const stored = await IDB.get('pk_passkey');
    if (!stored) { get('unlock-err').textContent = 'No passkey vault found'; return; }
    const wk = await VC.pbkdf2Key(btoa(String.fromCharCode(...new Uint8Array(stored.seed))), new Uint8Array(16));
    const jwk = await VC.unwrap({iv:stored.iv, ct:stored.ct}, wk);
    VS.privKey = await VC.importPriv(jwk); VS.locked = false;
    hide('ov-unlock'); await onUnlocked();
  } catch(_) { get('unlock-err').textContent = 'Unlock failed'; }
}

async function unlockPassphrase() {
  get('unlock-err').textContent = '';
  const pass = get('unlock-pp').value;
  if (!pass) { get('unlock-err').textContent = 'Enter passphrase'; return; }
  try {
    const stored = await IDB.get('pk_wrapped');
    if (!stored) { get('unlock-err').textContent = 'No passphrase vault found'; return; }
    const wk = await VC.pbkdf2Key(pass, new Uint8Array(stored.salt));
    const jwk = await VC.unwrap({iv:stored.iv, ct:stored.ct}, wk);
    VS.privKey = await VC.importPriv(jwk); VS.locked = false;
    hide('ov-unlock'); await onUnlocked();
  } catch(_) { get('unlock-err').textContent = 'Wrong passphrase'; }
}

async function resetVault() {
  if (!confirm('This will delete your local vault and generate a new keypair. Any messages encrypted with the old key will be unreadable. Continue?')) return;
  await IDB.clear();
  VS = {locked:true, method:null, privKey:null, pubB64:null, identId:null};
  hide('ov-unlock');
  show('ov-setup'); show('step-method');
  setStrip('Setup required', false); setVaultInfo('setup');
}

function onVaultStripClick() {
  if (VS.locked) show('ov-unlock');
  else {
    VS.locked = true; VS.privKey = null;
    setStrip('Vault locked', false); setVaultInfo('locked');
    if (_ws) { _ws.onclose = null; _ws.close(); _ws = null; }
  }
}

async function onUnlocked() {
  VS.locked = false;
  VS.identId = VS.identId || MY_PIAL;
  setStrip('Vault unlocked — live', true);
  setVaultInfo('unlocked');
  await loadSentCache();
  await loadConvos();
  connectWS();
  const dmHandle = new URLSearchParams(location.search).get('dm');
  if (dmHandle) { get('new-handle').value = dmHandle.replace(/^@/, ''); show('ov-new'); }
}

// ── Conversations ─────────────────────────────────────────────────────────────
async function loadConvos() {
  if (!VS.identId) return;
  try {
    const r = await fetch(`/vovin/v1/dm/${encodeURIComponent(VS.identId)}?device_id=${encodeURIComponent(DEVICE_ID)}`);
    if (!r.ok) return;
    const msgs = await r.json();
    if (!msgs || !msgs.length) return;
    msgs.forEach(m => {
      const id = String(m.id);
      if (_seenMsgIds.has(id)) return;
      _seenMsgIds.add(id);
      const key = m.sender;
      if (!receivedMsgs[key]) receivedMsgs[key] = [];
      receivedMsgs[key].push(m);
    });
    buildConvList();
  } catch(e) { console.warn('loadConvos:', e); }
}

function buildConvList() {
  const senders = Object.keys(receivedMsgs);
  if (!senders.length) return;
  hide('conv-empty');
  const list = get('conv-list');
  list.querySelectorAll('.msg-conv-item').forEach(x => x.remove());
  senders.forEach(senderPialId => {
    const msgs = receivedMsgs[senderPialId];
    const last = msgs[msgs.length - 1];
    const handle = last.sender_handle || senderPialId.slice(0, 8);
    const el = document.createElement('div');
    el.className = 'msg-conv-item';
    el.dataset.pialId = senderPialId;
    el.onclick = () => openConvo({handle, pialId:senderPialId}, el);
    el.innerHTML = `
      <div class="avatar-sm"><span style="width:100%;height:100%;border-radius:50%;background:var(--accent);display:flex;align-items:center;justify-content:center;font-size:13px;font-weight:700;color:var(--surface)">${esc(handle)[0].toUpperCase()}</span></div>
      <div class="msg-conv-meta">
        <p class="msg-conv-name">@${esc(handle)}</p>
        <p class="msg-conv-preview">🔒 Encrypted</p>
      </div>
      <span class="msg-conv-time">${ago(last.created_at)}</span>`;
    list.insertBefore(el, get('conv-empty'));
  });
}

async function openConvo(convo, el) {
  activeConvo = convo;
  document.querySelectorAll('.msg-conv-item').forEach(x => x.classList.remove('active'));
  if (el) el.classList.add('active');
  hide('thread-empty');
  const tv = get('thread-view'); tv.style.display = 'flex';
  get('thread-name').textContent = '@' + convo.handle;
  get('thread-av').innerHTML = `<span style="width:100%;height:100%;border-radius:50%;background:var(--accent);display:flex;align-items:center;justify-content:center;font-size:13px;font-weight:700;color:var(--surface)">${convo.handle[0].toUpperCase()}</span>`;
  await renderThread(convo.pialId);
}

async function renderThread(pialId) {
  const body = get('thread-body');
  body.innerHTML = '';

  // Received messages — decrypt each
  for (const m of (receivedMsgs[pialId] || [])) {
    let text = '🔒 Encrypted';
    if (!VS.privKey) {
      text = '🔒 Unlock your vault above to read messages';
    } else if (m.ciphertext && m.iv) {
      try {
        if (m.msg_version === 2 && m.ratchet_pub) {
          // V2: Double Ratchet (legacy — new sends use V1 per-device)
          const pt = await decryptDR(pialId, m);
          if (pt !== null) {
            text = pt;
          } else {
            // DR session state is device-local — messages sent before this device
            // was registered can't be decrypted here. This is expected.
            text = '🔒 Sent before this device was linked — not readable here';
          }
        } else if (m.sender_pub) {
          // V1: ECDH(myPriv, senderPub) — each device has its own keypair.
          // If this message was encrypted for a different device, decryption will fail.
          const senderPub = await VC.importPub(m.sender_pub);
          const sk = await VC.sharedKey(VS.privKey, senderPub);
          text = await VC.decrypt(sk, m.iv, m.ciphertext);
        } else {
          text = '🔒 Encrypted (no sender key)';
        }
      } catch(_) {
        // Decryption failed — most likely this message was encrypted for a different
        // device's keypair. New messages sent after this device registered will be readable.
        text = '🔒 Encrypted for another device — new messages will be readable here';
      }
    }
    const d = document.createElement('div');
    d.className = 'msg-row';
    d.innerHTML = `<div class="msg-bubble theirs">${renderMsgContent(text)}<span class="msg-ts">${ago(m.created_at)}</span></div>`;
    body.appendChild(d);
  }

  // Sent messages (local optimistic cache)
  for (const m of (sentCache[pialId] || [])) {
    const d = document.createElement('div');
    d.className = 'msg-row mine';
    d.innerHTML = `<div class="msg-bubble mine">${renderMsgContent(m.text)}<span class="msg-ts">${ago(m.ts)}</span></div>`;
    body.appendChild(d);
  }
  body.scrollTop = body.scrollHeight;
}

// ── Send ──────────────────────────────────────────────────────────────────────
async function sendMsg() {
  if (!activeConvo || !VS.privKey) return;
  const inp = get('msg-inp');
  const text = inp.value.trim();
  if (!text) return;

  const isSelf = activeConvo.pialId === MY_PIAL;

  try {
    if (isSelf) {
      // Self-message: V1 ECDH with own key
      const sk  = await VC.sharedKey(VS.privKey, await VC.importPub(VS.pubB64));
      const enc = await VC.encrypt(sk, text);
      const r   = await fetch('/vovin/v1/dm', {
        method: 'POST', headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({
          sender: VS.identId, sender_handle: MY_HANDLE,
          recipient: MY_PIAL, sender_pub: VS.pubB64,
          ciphertext: enc.ct, iv: enc.iv, msg_version: 1,
        })
      });
      if (!r.ok) { alert('Send failed (' + r.status + ')'); return; }
    } else {
      // Fetch all recipient devices with their individual public keys.
      // Each device has its own ECDH keypair — encrypt separately per device
      // so every device can decrypt with its own private key.
      let recipientDevices = [];
      try {
        const dr = await fetch(`/vovin/v1/devices/${encodeURIComponent(activeConvo.pialId)}`);
        if (dr.ok) {
          const devs = await dr.json();
          if (Array.isArray(devs)) recipientDevices = devs.filter(d => d.public_key_b64);
        }
      } catch(_) {}

      // Fall back to primary identity key if device list empty
      if (!recipientDevices.length) {
        if (!_identCache[activeConvo.pialId]) {
          const r = await fetch(`/vovin/v1/identity/${encodeURIComponent(activeConvo.pialId)}`);
          if (!r.ok) {
            alert('Recipient not found on Vovin. They need to visit Messages and set up their vault first.');
            return;
          }
          const ident = await r.json();
          _identCache[activeConvo.pialId] = ident.public_key_b64;
        }
        recipientDevices = [{ public_key_b64: _identCache[activeConvo.pialId], device_id: null }];
      }

      // V1 ECDH per-device: ECDH(senderPriv, devicePub) == ECDH(devicePriv, senderPub)
      // Each device decrypts with its own private key + the included sender_pub.
      const sends = await Promise.all(recipientDevices.map(async dev => {
        const devPub = await VC.importPub(dev.public_key_b64);
        const sk     = await VC.sharedKey(VS.privKey, devPub);
        const enc    = await VC.encrypt(sk, text);
        const body   = {
          sender: VS.identId, sender_handle: MY_HANDLE,
          recipient: activeConvo.pialId, sender_pub: VS.pubB64,
          ciphertext: enc.ct, iv: enc.iv, msg_version: 1,
        };
        if (dev.device_id) body.recipient_device_id = dev.device_id;
        return fetch('/vovin/v1/dm', {
          method: 'POST', headers: {'Content-Type': 'application/json'},
          body: JSON.stringify(body)
        });
      }));

      const first = await sends[0];
      if (!first.ok) { alert('Send failed (' + first.status + ')'); return; }
    }
  } catch(e) {
    alert('Encryption failed: ' + e.message);
    return;
  }

  // Optimistic render + persist
  if (!sentCache[activeConvo.pialId]) sentCache[activeConvo.pialId] = [];
  sentCache[activeConvo.pialId].push({text, ts: new Date().toISOString()});
  persistSentCache();
  const body = get('thread-body');
  const d = document.createElement('div');
  d.className = 'msg-row mine';
  d.innerHTML = `<div class="msg-bubble mine">${renderMsgContent(text)}<span class="msg-ts">just now</span></div>`;
  body.appendChild(d); body.scrollTop = body.scrollHeight;
  inp.value = ''; autoGrowMsg(inp);
}

function onMsgKey(e) {
  if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); sendMsg(); }
}
function autoGrowMsg(el) {
  el.style.height = 'auto';
  el.style.height = Math.min(el.scrollHeight, 120) + 'px';
}

// ── AFF — client-side encrypted file upload ───────────────────────────────────
const AFF_CHUNK_SIZE = 1024 * 1024;

async function onMsgFileSelect(input) {
  const file = input.files[0];
  if (!file || !activeConvo || !VS.privKey) return;
  input.value = '';
  if (file.size > 100 * 1024 * 1024) { toast('Max file size: 100 MB', 'error'); return; }

  const inp = get('msg-inp');
  inp.disabled = true;
  inp.placeholder = `Uploading ${file.name}…`;

  try {
    const chunkCount = Math.ceil(file.size / AFF_CHUNK_SIZE);
    let fileId = null;

    for (let i = 0; i < chunkCount; i++) {
      const start = i * AFF_CHUNK_SIZE;
      const slice = file.slice(start, start + AFF_CHUNK_SIZE);
      const buf   = await slice.arrayBuffer();

      const nonce = crypto.getRandomValues(new Uint8Array(12));
      const chunkKey = await crypto.subtle.importKey('raw',
        await crypto.subtle.digest('SHA-256', new TextEncoder().encode(VS.pubB64 + i)),
        {name:'AES-GCM'}, false, ['encrypt']);
      const ct = await crypto.subtle.encrypt({name:'AES-GCM', iv:nonce}, chunkKey, buf);

      const hashBuf  = await crypto.subtle.digest('SHA-256', ct);
      const chunkHash = btoa(String.fromCharCode(...new Uint8Array(hashBuf)));
      const b64 = a => btoa(String.fromCharCode(...new Uint8Array(a)));

      const body = {
        pial_id:        MY_PIAL,
        file_id:        fileId || undefined,
        chunk_index:    i,
        chunk_hash:     chunkHash,
        ciphertext_b64: b64(ct),
        nonce_b64:      b64(nonce.buffer),
      };
      if (i === 0) {
        body.filename    = file.name;
        body.mime_type   = file.type || 'application/octet-stream';
        body.size_bytes  = file.size;
        body.chunk_count = chunkCount;
        body.hash_root   = '';
      }

      const r = await fetch('/vovin/v1/aff/upload', {
        method: 'POST', headers: {'Content-Type':'application/json'},
        body: JSON.stringify(body)
      });
      if (!r.ok) throw new Error('Upload failed at chunk ' + i);
      const resp = await r.json();
      if (!fileId) fileId = resp.file_id;

      inp.placeholder = `Uploading ${file.name}… ${i+1}/${chunkCount}`;
    }

    const fileMsg = JSON.stringify({
      type: 'file',
      file_id:    fileId,
      filename:   file.name,
      mime_type:  file.type,
      size_bytes: file.size,
      chunk_count: chunkCount,
    });
    inp.value = fileMsg;
    await sendMsg();
  } catch(e) {
    toast('File upload failed: ' + e.message, 'error');
  } finally {
    inp.disabled = false;
    inp.placeholder = 'Message…';
  }
}

async function affDownload(fileId, filename) {
  toast('Downloading ' + filename + '…');
  try {
    const mr = await fetch(`/vovin/v1/aff/${fileId}`);
    if (!mr.ok) throw new Error('Manifest not found');
    const manifest = await mr.json();

    const chunks = [];
    for (let i = 0; i < manifest.chunk_count; i++) {
      const cr = await fetch(`/vovin/v1/aff/${fileId}/chunk/${i}`);
      if (!cr.ok) throw new Error('Chunk ' + i + ' not found');
      const chunk = await cr.json();
      const ct    = Uint8Array.from(atob(chunk.ciphertext_b64), c => c.charCodeAt(0));
      const nonce = Uint8Array.from(atob(chunk.nonce_b64), c => c.charCodeAt(0));
      const chunkKey = await crypto.subtle.importKey('raw',
        await crypto.subtle.digest('SHA-256', new TextEncoder().encode(VS.pubB64 + i)),
        {name:'AES-GCM'}, false, ['decrypt']);
      chunks.push(await crypto.subtle.decrypt({name:'AES-GCM', iv:nonce}, chunkKey, ct));
    }

    const blob = new Blob(chunks.map(c => new Uint8Array(c)), {type: manifest.mime_type});
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url; a.download = filename; a.click();
    setTimeout(() => URL.revokeObjectURL(url), 5000);
    toast(filename + ' downloaded');
  } catch(e) {
    toast('Download failed: ' + e.message, 'error');
  }
}

// ── Saved (message yourself) ─────────────────────────────────────────────────
function openSavedConvo() {
  if (!VS.privKey) { toast('Unlock your vault first', 'error'); return; }
  const convo = {handle: MY_HANDLE + ' (Saved)', pialId: MY_PIAL, isSelf: true};
  document.querySelectorAll('.msg-conv-item').forEach(x => x.classList.remove('active'));
  get('saved-convo').classList.add('active');
  hide('thread-empty');
  const tv = get('thread-view'); tv.style.display = 'flex';
  get('thread-name').textContent = 'Saved';
  get('thread-sub').textContent  = 'Notes, links, reminders — only you can see these';
  get('thread-av').innerHTML = `<div style="width:100%;height:100%;border-radius:50%;background:var(--accent);display:flex;align-items:center;justify-content:center"><svg fill="none" stroke="white" stroke-width="2" viewBox="0 0 24 24" style="width:14px;height:14px"><path d="M19 21l-7-5-7 5V5a2 2 0 012-2h10a2 2 0 012 2z"/></svg></div>`;
  activeConvo = convo;
  renderThread(MY_PIAL);
}

// ── New conversation ──────────────────────────────────────────────────────────
function openNewConvo() { show('ov-new'); }
function closeNew() { hide('ov-new'); }

async function startConvo() {
  const handleRaw = get('new-handle').value.replace(/^@/, '').trim();
  const text = get('new-msg').value.trim();
  if (!handleRaw || !text) return;
  if (!VS.privKey) { alert('Unlock your vault first'); return; }

  // Resolve handle → PIAL ID
  let recipPialId = null;
  try {
    const r = await fetch(`/vovin/v1/identity/by-handle/${encodeURIComponent(handleRaw)}`);
    if (r.ok) {
      const j = await r.json();
      recipPialId = j.identity;
    } else if (r.status === 404) {
      alert(`@${handleRaw} hasn't set up secure messaging yet.\n\nThey need to visit Messages and complete their vault setup.`);
      return;
    } else {
      alert(`Could not reach the messaging service (${r.status}). Please try again.`);
      return;
    }
  } catch(e) { alert('Could not look up recipient.\n\n' + e.message); return; }

  closeNew();
  get('new-msg').value = '';

  const convo = {handle: handleRaw, pialId: recipPialId};
  // Add sidebar entry if this is a new conversation
  if (!receivedMsgs[recipPialId] && !get('conv-list').querySelector(`[data-pial-id="${recipPialId}"]`)) {
    hide('conv-empty');
    const list = get('conv-list');
    const el = document.createElement('div');
    el.className = 'msg-conv-item';
    el.dataset.pialId = recipPialId;
    el.onclick = () => openConvo(convo, el);
    el.innerHTML = `
      <div class="avatar-sm"><span style="width:100%;height:100%;border-radius:50%;background:var(--accent);display:flex;align-items:center;justify-content:center;font-size:13px;font-weight:700;color:var(--surface)">${esc(handleRaw)[0].toUpperCase()}</span></div>
      <div class="msg-conv-meta">
        <p class="msg-conv-name">@${esc(handleRaw)}</p>
        <p class="msg-conv-preview">🔒 Encrypted</p>
      </div>
      <span class="msg-conv-time">now</span>`;
    list.insertBefore(el, get('conv-empty'));
  }

  const sidebarEl = get('conv-list').querySelector(`[data-pial-id="${recipPialId}"]`);
  await openConvo(convo, sidebarEl);
  const inp = get('msg-inp');
  if (inp) { inp.value = text; autoGrowMsg(inp); }
  await sendMsg();
}

// ── Message content renderer ──────────────────────────────────────────────────
function renderMsgContent(text) {
  if (!text) return '';
  try {
    const j = JSON.parse(text);
    if (j.type === 'file' && j.file_id) {
      const icon = j.mime_type?.startsWith('image/') ? '🖼️' :
                   j.mime_type?.startsWith('video/') ? '🎬' :
                   j.mime_type?.startsWith('audio/') ? '🎵' : '📎';
      const sizeMB = j.size_bytes ? (j.size_bytes / (1024*1024)).toFixed(1) + ' MB' : '';
      return `<div style="display:flex;align-items:center;gap:8px;padding:6px 0">
        <span style="font-size:20px">${icon}</span>
        <div style="flex:1;min-width:0">
          <p style="font-size:12px;font-weight:600;margin:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">${esc(j.filename)}</p>
          <p style="font-size:11px;opacity:.7;margin:0">${sizeMB}</p>
        </div>
        <button onclick="affDownload('${esc(j.file_id)}','${esc(j.filename)}')" style="padding:4px 10px;border-radius:8px;border:none;background:rgba(255,255,255,.2);color:inherit;font-size:11px;cursor:pointer;flex-shrink:0">Download</button>
      </div>`;
    }
  } catch(_) {}
  return esc(text);
}

// ── UI helpers ────────────────────────────────────────────────────────────────
function setStrip(text, unlocked) {
  get('vault-strip-text').textContent = text;
  get('vault-icon').style.color = unlocked ? '#22c55e' : 'var(--accent)';
  get('vault-icon').innerHTML = unlocked
    ? '<rect x="3" y="11" width="18" height="11" rx="2" ry="2"/><path d="M7 11V7a5 5 0 0 1 9.9-1"/>'
    : '<rect x="3" y="11" width="18" height="11" rx="2" ry="2"/><path d="M7 11V7a5 5 0 0 1 10 0v4"/>';
}

function setVaultInfo(state) {
  const b = get('vault-info');
  if (state === 'setup') {
    b.innerHTML = '<p style="color:var(--text-muted)">Not configured. Choose a protection method above.</p>';
  } else if (state === 'locked') {
    b.innerHTML = '<p style="color:var(--accent)">🔒 Locked</p><p style="margin-top:6px;font-size:11px;color:var(--text-muted)">Tap the lock strip to unlock.</p>';
  } else {
    const pub = VS.pubB64 ? VS.pubB64.slice(0, 16) + '…' : '–';
    b.innerHTML = `<p style="color:#22c55e;margin-bottom:8px">🔓 Unlocked</p>
      <div class="engine-status-row" style="padding:3px 0"><span class="engine-status-label">Method</span><span class="engine-status-val">${VS.method||'–'}</span></div>
      <div class="engine-status-row" style="padding:3px 0"><span class="engine-status-label">Public key</span><span class="engine-status-val" style="font-family:monospace;font-size:10px">${pub}</span></div>
      <div class="engine-status-row" style="padding:3px 0"><span class="engine-status-label">PIAL</span><span class="engine-status-val" style="font-family:monospace;font-size:10px">${(VS.identId||'–').slice(0,12)+'…'}</span></div>`;
  }
}

function get(s)  { return document.getElementById(s); }
function show(s) { const e = get(s); if (e) { e.style.display = ''; e.classList.add('open'); } }
function hide(s) { const e = get(s); if (e) { e.style.display = 'none'; e.classList.remove('open'); } }
function setMsg(s, t) { const e = get(s); if (e) e.textContent = t; }
function esc(s)  { return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;'); }
function ago(ts) {
  if (!ts) return '';
  const s = Math.floor((Date.now() - new Date(ts)) / 1000);
  if (s < 60) return 'just now';
  if (s < 3600) return Math.floor(s/60)+'m';
  if (s < 86400) return Math.floor(s/3600)+'h';
  return Math.floor(s/86400)+'d';
}

document.addEventListener('DOMContentLoaded', boot);
