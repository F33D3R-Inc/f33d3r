// gnosis-seal.js — sealed-mode (E2EE) client. ES module.
//
// Runs on the login page (derive the unlock key from the password at sign-in) and
// the messages page (provision/unlock the identity, seal outgoing messages, open
// sealed bubbles). The X25519 private key is generated in-browser and only ever
// leaves the device wrapped under the login-derived key; the server stores blobs.

import init, * as S from '/static/js/sealcore/sealcore.js';

let _wasm = null;
function wasm() {
  if (!_wasm) _wasm = init().then(() => S);
  return _wasm;
}

// ── helpers ─────────────────────────────────────────────────────────────────
function b64(bytes) {
  let s = '';
  const a = new Uint8Array(bytes);
  for (let i = 0; i < a.length; i++) s += String.fromCharCode(a[i]);
  return btoa(s);
}
async function saltFromHandle(handle) {
  const d = new TextEncoder().encode((handle || '').toLowerCase());
  return b64(await crypto.subtle.digest('SHA-256', d)); // public, per-account unique
}
function meta(name) {
  const el = document.querySelector('meta[name="' + name + '"]');
  return el ? el.content : '';
}

// ── IndexedDB cache for the unwrapped private key (per PIAL) ─────────────────
function idb() {
  return new Promise((res, rej) => {
    const r = indexedDB.open('gnosis_seal_v1', 1);
    r.onupgradeneeded = () => r.result.createObjectStore('keys');
    r.onsuccess = () => res(r.result);
    r.onerror = () => rej(r.error);
  });
}
async function idbGet(k) {
  try {
    const db = await idb();
    return await new Promise((res) => {
      const t = db.transaction('keys', 'readonly').objectStore('keys').get(k);
      t.onsuccess = () => res(t.result || null);
      t.onerror = () => res(null);
    });
  } catch (_) { return null; }
}
async function idbSet(k, v) {
  try {
    const db = await idb();
    await new Promise((res) => {
      const t = db.transaction('keys', 'readwrite').objectStore('keys').put(v, k);
      t.onsuccess = () => res();
      t.onerror = () => res();
    });
  } catch (_) {}
}

// ── login hook: derive the unlock key from the password (fail-safe) ──────────
function hookLogin() {
  const form = document.querySelector('form[action="/login"]');
  if (!form) return;
  form.addEventListener('submit', function (e) {
    if (form.dataset.gnDone) return; // second pass after we resubmit
    const password = (form.querySelector('[name=password]') || {}).value || '';
    if (!password) return; // nothing to derive — let login proceed normally
    e.preventDefault();
    const handle = (form.querySelector('[name=handle]') || {}).value || '';
    let finished = false;
    const proceed = () => { if (!finished) { finished = true; form.dataset.gnDone = '1'; form.submit(); } };
    setTimeout(proceed, 1500); // never hang login on crypto
    (async () => {
      try {
        const salt = await saltFromHandle(handle);
        const s = await wasm();
        sessionStorage.setItem('gn_kek', s.derive_backup_key(password, salt));
      } catch (_) {}
      proceed();
    })();
  });
}

// ── identity: provision on first use, unlock on later logins ────────────────
let _keyPromise = null;
function ensureKey() {
  if (!_keyPromise) _keyPromise = bootstrap();
  return _keyPromise;
}
async function bootstrap() {
  const account = meta('f33d3r:account'); // per-account identity (not the human PIAL)
  if (!account) return null;
  const cached = await idbGet('priv:' + account);
  if (cached) return cached;

  const kek = sessionStorage.getItem('gn_kek'); // login-derived; only present right after sign-in
  const s = await wasm();
  const res = await fetch('/api/gnosis/bootstrap').then((r) => r.json()).catch(() => null);
  if (!res) return null;
  if (!kek) return null; // unlocked only by a fresh password login (no separate recovery)

  let priv = null;
  if (res.has && res.wrapped_priv) {
    try { priv = s.unwrap_with_key(kek, res.wrapped_priv, res.wrap_nonce); } catch (_) { priv = null; }
    // unwrap failure ⇒ password was changed via account recovery; fall through and
    // re-provision a fresh key (the server logs the rotation).
  }
  if (!priv) {
    const kp = JSON.parse(s.generate_keypair()); // {priv_b64, pub_b64}
    const wrapped = JSON.parse(s.wrap_with_key(kek, kp.priv_b64));
    const ok = await fetch('/api/gnosis/provision', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ pub_b64: kp.pub_b64, wrapped_priv: wrapped.ct_b64, wrap_nonce: wrapped.nonce_b64 }),
    }).then((r) => r.ok).catch(() => false);
    if (!ok) return null;
    priv = kp.priv_b64;
  }
  await idbSet('priv:' + account, priv);
  sessionStorage.removeItem('gn_kek'); // unwrapped key now cached locally; shrink XSS window
  return priv;
}

// TOFU: pin each recipient's public key on first contact and warn if it ever
// changes (new device — or a key-swap MITM). Returns false if the user declines.
async function checkPins(recipients) {
  for (const r of recipients) {
    const key = 'pin:' + r.account;
    const prev = await idbGet(key);
    if (prev && prev !== r.pub_b64) {
      if (!confirm('⚠️ ' + r.account + " 's encryption key changed (new device or possible interception). Send anyway?")) {
        return false;
      }
    }
    await idbSet(key, r.pub_b64);
  }
  return true;
}

// ── open a sealed bubble in place ───────────────────────────────────────────
async function openBubble(el) {
  if (!el || !el.querySelector) return;
  const slot = el.querySelector('.gn-sealed[data-sealed="1"]');
  if (!slot) return;
  const priv = await ensureKey();
  if (!priv) { slot.textContent = '🔒 Sign in again to read'; return; }
  try {
    const s = await wasm();
    const pt = s.open_message(priv, slot.dataset.eph, slot.dataset.sealedKey, slot.dataset.sealedNonce, slot.dataset.bodyCt, slot.dataset.bodyNonce);
    slot.textContent = pt;
    slot.removeAttribute('data-sealed');
    slot.classList.remove('gn-sealed');
  } catch (_) {
    slot.textContent = '🔒 (cannot decrypt)';
  }
}
async function openAll() {
  const list = document.getElementById('gnosis-messages');
  if (!list) return;
  const slots = list.querySelectorAll('.gn-bubble');
  for (const b of slots) await openBubble(b);
}

// ── seal + send ─────────────────────────────────────────────────────────────
async function sealSend(form) {
  const thread = form.closest('.gn-thread');
  const input = form.querySelector('[name=body]');
  const text = (input.value || '').trim();
  if (!text) return;
  const convo = form.dataset.convo;
  const btn = form.querySelector('button');
  if (btn) btn.disabled = true;
  try {
    const priv = await ensureKey(); // also ensures our pub is provisioned in the directory
    if (!priv) { alert('Encryption is locked — sign in again to send.'); return; }
    const s = await wasm();
    const dir = await fetch('/api/gnosis/directory?c=' + encodeURIComponent(convo)).then((r) => r.json()).catch(() => []);
    const recipients = (dir || []).filter((d) => d.pub_b64).map((d) => ({ account: d.account, pub_b64: d.pub_b64 }));
    if (!recipients.length) { alert('No encryption keys for the recipients yet.'); return; }
    if (!(await checkPins(recipients))) return; // TOFU key-change guard
    const envelope = JSON.parse(s.seal_message(JSON.stringify(recipients), text));
    const res = await fetch('/api/gnosis/send-sealed', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ c: convo, envelope }),
    });
    if (!res.ok) { alert('Send failed.'); return; }
    const html = (await res.text()).trim();
    const list = document.getElementById('gnosis-messages');
    if (list && html) {
      const tmp = document.createElement('div');
      tmp.innerHTML = html;
      const bub = tmp.firstElementChild;
      if (bub) { list.appendChild(bub); await openBubble(bub); list.scrollTop = list.scrollHeight; }
    }
    input.value = '';
  } finally {
    if (btn) btn.disabled = false;
  }
}

// ── wiring ──────────────────────────────────────────────────────────────────
hookLogin();

if (document.getElementById('gnosis-layout')) {
  // Let f33d3r.js's SSE listener hand new sealed bubbles to us for decryption.
  window._onGnosisBubble = openBubble;

  // Intercept sealed composers (they carry no hx-post).
  document.body.addEventListener('submit', function (e) {
    const form = e.target;
    if (form && form.classList && form.classList.contains('gn-composer-sealed')) {
      e.preventDefault();
      sealSend(form);
    }
  });

  // Decrypt whatever is on screen now, and after each thread swap.
  ensureKey().then(openAll);
  document.body.addEventListener('htmx:afterSwap', function (e) {
    if (e.target && (e.target.id === 'gnosis-thread' || e.target.id === 'gnosis-messages')) openAll();
  });
}
