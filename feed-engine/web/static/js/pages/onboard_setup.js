// Server values from <meta> tags — no template injection in JS
const SESSION_ID    = document.querySelector('meta[name="f33d3r:session"]')?.content || '';
const MY_PIAL       = document.querySelector('meta[name="f33d3r:pial"]')?.content || '';
const MY_HANDLE     = document.querySelector('meta[name="f33d3r:handle"]')?.content || '';
const CURRENT_SURFACE = document.querySelector('meta[name="f33d3r:surface"]')?.content || 'feed';



// ── Minimal IndexedDB / WebCrypto layer ───────────────────────────────────────
// DB name MUST match messages.html exactly: vovin_v1_{PIAL_UUID}
// This is the fix for cross-account key collision on shared browsers.
const IDB_NAME = MY_PIAL ? `vovin_v1_${MY_PIAL}` : 'vovin_v1_unknown';

const IDB = {
  _open() {
    return new Promise((res, rej) => {
      const r = indexedDB.open(IDB_NAME, 1);
      r.onupgradeneeded = e => e.target.result.createObjectStore('kv');
      r.onsuccess = e => res(e.target.result);
      r.onerror   = e => rej(e.target.error);
    });
  },
  async set(k, v) {
    const db = await this._open();
    return new Promise((res, rej) => {
      const tx = db.transaction('kv', 'readwrite');
      tx.objectStore('kv').put(v, k);
      tx.oncomplete = res; tx.onerror = e => rej(e.target.error);
    });
  },
};
const VC = {
  genKeyPair() {
    return crypto.subtle.generateKey({name:'ECDH',namedCurve:'P-256'}, true, ['deriveKey','deriveBits']);
  },
  exportPub(pub) {
    return crypto.subtle.exportKey('raw', pub).then(b => btoa(String.fromCharCode(...new Uint8Array(b))));
  },
  exportPrivJwk(priv) {
    return crypto.subtle.exportKey('jwk', priv);
  },
  pbkdf2Key(pass, salt) {
    return crypto.subtle.importKey('raw', new TextEncoder().encode(pass), 'PBKDF2', false, ['deriveBits'])
      .then(k => crypto.subtle.deriveBits(
        {name:'PBKDF2', salt, iterations:200000, hash:'SHA-256'}, k, 256
      ))
      .then(bits => crypto.subtle.importKey('raw', bits, {name:'AES-GCM'}, false, ['encrypt','decrypt']));
  },
  wrap(jwk, wk) {
    const iv = crypto.getRandomValues(new Uint8Array(12));
    const enc = new TextEncoder().encode(JSON.stringify(jwk));
    return crypto.subtle.encrypt({name:'AES-GCM', iv}, wk, enc).then(ct => ({iv:[...iv], ct:[...new Uint8Array(ct)]}));
  },
};

function show(id) { const e = document.getElementById(id); if(e) { e.style.display=''; } }
function hide(id) { const e = document.getElementById(id); if(e) e.style.display='none'; }
function setMsg(id, t) { const e = document.getElementById(id); if(e) e.textContent = t; }
function backToMethod() { hide('step-passphrase'); show('step-method'); }
function showPassphraseStep() { hide('step-method'); show('step-passphrase'); }

async function setupPasskey() { await doGenerate('passkey', null); }
async function setupNone()    { await doGenerate('none', null); }

async function confirmPassphrase() {
  const p1 = document.getElementById('pp1').value;
  const p2 = document.getElementById('pp2').value;
  document.getElementById('pp-err').textContent = '';
  if (p1.length < 8) { document.getElementById('pp-err').textContent = 'Min 8 characters'; return; }
  if (p1 !== p2)     { document.getElementById('pp-err').textContent = 'Passphrases do not match'; return; }
  await doGenerate('passphrase', p1);
}

async function doGenerate(method, passphrase) {
  hide('step-method'); hide('step-passphrase'); show('step-generating');
  setMsg('gen-msg', 'Generating ECDH identity keypair…');

  try {
    const kp  = await VC.genKeyPair();
    const pub = await VC.exportPub(kp.publicKey);
    const privJwk = await VC.exportPrivJwk(kp.privateKey);

    // Stable device ID — namespaced per PIAL, persists in localStorage
    const deviceKey = `f33d3r_device_${MY_PIAL}`;
    let deviceId = localStorage.getItem(deviceKey);
    if (!deviceId) {
      deviceId = (typeof crypto !== 'undefined' && crypto.randomUUID)
        ? crypto.randomUUID()
        : Math.random().toString(36).slice(2) + Date.now().toString(36);
      localStorage.setItem(deviceKey, deviceId);
    }

    setMsg('gen-msg', 'Registering identity with Vovin…');
    try {
      const regResp = await fetch('/vovin/v1/identity', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          identity: MY_PIAL,
          public_key_b64: pub,
          handle: MY_HANDLE,
          device_id: deviceId,
        }),
      });
      if (!regResp.ok) {
        const errText = await regResp.text().catch(()=>'');
        throw new Error('Vovin registration failed (' + regResp.status + '). ' + errText);
      }
    } catch(e) {
      hide('step-generating');
      show('step-method');
      alert('Could not register with the messaging service.\n\nMake sure F33D3R is running and try again.\n\n' + e.message);
      return;
    }

    setMsg('gen-msg', 'Encrypting key vault…');
    if (method === 'passphrase' && passphrase) {
      const salt = crypto.getRandomValues(new Uint8Array(16));
      const wk = await VC.pbkdf2Key(passphrase, salt);
      const w  = await VC.wrap(privJwk, wk);
      await IDB.set('pk_wrapped', {salt:[...salt], ...w});
    } else if (method === 'passkey') {
      const seed = crypto.getRandomValues(new Uint8Array(32));
      const wk = await VC.pbkdf2Key(btoa(String.fromCharCode(...seed)), new Uint8Array(16));
      const w  = await VC.wrap(privJwk, wk);
      await IDB.set('pk_passkey', {seed:[...seed], ...w});
    } else {
      await IDB.set('pk_plain', privJwk);
    }
    await IDB.set('meta', {method, pubB64: pub, identId: MY_PIAL});

    hide('step-generating');
    show('step-done');
  } catch(err) {
    hide('step-generating');
    show('step-method');
    alert('Setup failed: ' + err.message);
  }
}
