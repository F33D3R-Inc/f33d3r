> ⛔️ **SUPERSEDED / OLD — DO NOT IMPLEMENT.** Vovin (aethyr-msg) and this V4 design
> (X3DH/PQXDH + Double Ratchet) are retired. The messaging pillar is rebuilt as **Gnosis**:
> a server-authoritative Facet-Architecture pipe inside feed-engine with a zero-knowledge
> **PIAL two-key, epoch-rotated** model (no Double Ratchet), crypto in the Rust→WASM
> `gnosis-malkuth` core. See the plan and `internal/gnosis/`. Kept only for historical reference.

# VOVIN V4 — Cryptographic Specification

**Status:** Specification — not yet implemented  
**Replaces:** Vovin V3 (ECDH-P256 + X3DH + Double Ratchet)  
**Target:** Sprint 2, Milestone M1  
**Author:** F33D3R Engineering

---

## Overview

Vovin V4 upgrades the session establishment and ratchet layers to match Signal's 2026 protocol stack:

| Layer | V3 (current) | V4 (this spec) |
|-------|-------------|----------------|
| Session establishment | X3DH | PQXDH (X25519 + Kyber-768) |
| Ratchet | Double Ratchet | Triple Ratchet + SPQR |
| DH key type | ECDH-P256 | X25519 |
| Signing | ECDSA-P256 | ECDSA-P256 (unchanged) |
| Sealed Sender | ✗ | ✓ |

All V3 sessions remain valid indefinitely. New sessions use V4 protocol only. Peers negotiate version via `protocol_version` in prekey bundles.

---

## Part 1 — PQXDH Session Establishment

### 1.1 Why PQXDH

X3DH relies entirely on Elliptic Curve Diffie-Hellman. A large-scale quantum computer running Shor's algorithm breaks ECDH in polynomial time. The attack model is **harvest-now-decrypt-later**: adversaries record ciphertext today and decrypt it once quantum hardware matures (~2030–2035). Messages sent today must resist that.

PQXDH combines:
- **X25519** — classical DH, constant-time, efficient, standard
- **CRYSTALS-Kyber-768** — post-quantum KEM, IND-CCA2 secure under MLWE assumption

An adversary must break **both** to recover the session shared secret.

### 1.2 Key Material

Each registered device uploads a prekey bundle containing:

```
PreKeyBundle {
  identity_key_pub:        bytes   // X25519 long-term identity key
  signed_prekey_pub:       bytes   // X25519 signed prekey
  signed_prekey_sig:       bytes   // ECDSA-P256 signature over signed_prekey_pub
  kyber_prekey_pub:        bytes   // Kyber-768 encapsulation key
  kyber_prekey_sig:        bytes   // ECDSA-P256 signature over kyber_prekey_pub
  one_time_prekeys:        bytes[] // X25519 OTPKs (consumed one per session)
  protocol_version:        u16     // 4 for V4, 3 for V3
}
```

Signing key (Glyph, ECDSA-P256) signs both DH and KEM prekeys. The identity key is X25519 in V4 (was ECDH-P256 in V3).

### 1.3 Session Initiation (sender side)

Given recipient prekey bundle `B`:

```
// 1. Classical DH component
ephemeral_dh_kp  = X25519.generateKeyPair()
dh1 = X25519.dh(sender.identity_key.priv,   B.signed_prekey_pub)
dh2 = X25519.dh(ephemeral_dh_kp.priv,       B.identity_key_pub)
dh3 = X25519.dh(ephemeral_dh_kp.priv,       B.signed_prekey_pub)
dh4 = X25519.dh(ephemeral_dh_kp.priv,       B.one_time_prekey_pub)  // if available
dh_out = dh1 || dh2 || dh3 [|| dh4]

// 2. Post-quantum KEM component
(kyber_ciphertext, kyber_shared_secret) = Kyber768.encapsulate(B.kyber_prekey_pub)

// 3. Combined shared secret
shared_secret = HKDF-SHA256(
  ikm  = dh_out || kyber_shared_secret,
  salt = PQXDH_PROTOCOL_INFO,
  info = "PQXDH_v4_F33DR",
  len  = 64
)
// → 64 bytes: first 32 = root key, second 32 = chain key seed

PQXDH_PROTOCOL_INFO = "F33DR_VOVIN_V4_PQXDH_2026"  // unique to F33D3R
```

The sender includes `ephemeral_dh_kp.pub` and `kyber_ciphertext` in the message envelope so the recipient can derive the same shared secret.

### 1.4 Session Initiation (recipient side)

On receiving the first message:

```
// Reconstruct DH outputs
dh1 = X25519.dh(recipient.signed_prekey.priv,   sender_identity_key_pub)
dh2 = X25519.dh(recipient.identity_key.priv,    sender_ephemeral_dh_pub)
dh3 = X25519.dh(recipient.signed_prekey.priv,   sender_ephemeral_dh_pub)
dh4 = X25519.dh(recipient.one_time_prekey.priv, sender_ephemeral_dh_pub)  // if used
dh_out = dh1 || dh2 || dh3 [|| dh4]

// Post-quantum KEM
kyber_shared_secret = Kyber768.decapsulate(recipient.kyber_prekey.priv, kyber_ciphertext)

// Same HKDF derivation → identical shared_secret
```

### 1.5 Protocol Version Negotiation

Prekey bundles include `protocol_version`. Client behaviour:

- If peer bundle has `protocol_version = 4`: initiate PQXDH session
- If peer bundle has `protocol_version = 3` or missing: initiate X3DH session (V3 path)
- Both paths are supported simultaneously during the migration window

The session type is stored in the ratchet session metadata (`session.protocol_version`). It is not encrypted — the server may observe which version is used.

### 1.6 WebCrypto Implementation Notes

WebCrypto (as of 2026) does **not** natively support Kyber-768. The client implementation requires:

- **X25519**: use `{name:'X25519'}` via WebCrypto `generateKey` / `deriveBits` (supported in Chrome 113+, Firefox 119+, Safari 17.4+)
- **Kyber-768**: WASM build of `liboqs` (Open Quantum Safe) compiled to WebAssembly

```js
// X25519 in WebCrypto
const ikp = await crypto.subtle.generateKey({name:'X25519'}, true, ['deriveBits']);
const dh = await crypto.subtle.deriveBits({name:'X25519', public: theirPub}, myPriv, 256);

// Kyber-768 via WASM
import { Kyber768 } from './kyber768.wasm.js';  // ~85 KB gzipped
const { ciphertext, sharedSecret } = await Kyber768.encapsulate(recipientPublicKey);
```

The WASM module is loaded once on messages page load. It must be served from the same origin (no CDN — CSP `script-src 'self'`).

---

## Part 2 — Triple Ratchet + SPQR

### 2.1 Architecture

The Triple Ratchet runs three ratchets simultaneously:

```
Ratchet 1 (DH):       X25519 key exchange — fires every ratchet step
Ratchet 2 (Symmetric): HKDF chain — fires every message
Ratchet 3 (SPQR):     Kyber-768 KEM — fires every N=50 messages
```

Per-message key derivation:

```
mk = HKDF-SHA256(
  ikm  = DH_ratchet_output || symmetric_chain_output || SPQR_output,
  salt = TR_PROTOCOL_INFO,
  info = "TR_msg_key_F33DR_v4",
  len  = 32
)
```

where `SPQR_output` is zero-padded to 32 bytes when SPQR has not yet fired in the current window.

### 2.2 Protocol Info Constants

```
PQXDH_PROTOCOL_INFO = b"F33DR_VOVIN_V4_PQXDH_2026"
TR_PROTOCOL_INFO    = b"F33DR_VOVIN_V4_TR_2026"
SPQR_PROTOCOL_INFO  = b"F33DR_VOVIN_V4_SPQR_2026"
```

These strings uniquely identify F33D3R's protocol version and must appear verbatim in key derivation. Do not change them after rollout — changing them invalidates all existing sessions.

### 2.3 DH Ratchet (unchanged from V3, key type upgraded to X25519)

```
// On sending:
if ratchet_step_needed:
  new_dh_kp = X25519.generateKeyPair()
  dh_output = X25519.dh(new_dh_kp.priv, remote_ratchet_pub)
  (new_rk, new_ck_s) = HKDF(rk || dh_output, TR_PROTOCOL_INFO, "ratchet_step")
  rk = new_rk
  ck_s = new_ck_s
  local_ratchet_pub = new_dh_kp.pub

// Per message:
(new_ck, mk) = HKDF(ck_s, TR_PROTOCOL_INFO, "chain_step")
ck_s = new_ck
```

### 2.4 SPQR — Sparse Post-Quantum Ratchet

SPQR fires every **N = 50** messages. Frequency rationale:

- Kyber-768 encap/decap: ~0.3 ms on modern hardware
- At N=50: amortised cost = 0.006 ms/message — imperceptible
- Attack window if DH is broken: at most 50 messages
- If SPQR is broken AND DH is broken: session compromised (both must break simultaneously)

```
// On message i where i % 50 === 0 (SPQR fire):
(kyber_ct, spqr_secret) = Kyber768.encapsulate(remote_spqr_pub)
// Include kyber_ct in message header (unencrypted, small ~1 KB)

// On receiving a message with kyber_ct present:
spqr_secret = Kyber768.decapsulate(local_spqr_priv, kyber_ct)
// Derive new SPQR keypair for next window
local_spqr_kp = Kyber768.generateKeyPair()
// Send new public key in next outbound message header
```

SPQR keys are stored in the ratchet session state (`session.spqr_pub`, `session.spqr_priv_jwk`). New SPQR keypairs are generated after each fire.

### 2.5 Combined Per-Message Key

```
// If SPQR fired this message:
mk = HKDF(dh_output || chain_mk || spqr_secret, TR_PROTOCOL_INFO, "TR_msg_key_F33DR_v4")

// Between SPQR fires:
mk = HKDF(dh_output || chain_mk || zeros_32, TR_PROTOCOL_INFO, "TR_msg_key_F33DR_v4")
```

Encryption: AES-256-GCM with random 12-byte IV.

---

## Part 3 — X25519 Migration

### 3.1 Key Generation Change

V4 identity keys are **X25519**, not ECDH-P256.

```js
// V4 identity key
const ikp = await crypto.subtle.generateKey({name:'X25519'}, true, ['deriveBits']);

// V3 identity key (keep for reference during migration)
const ikp = await crypto.subtle.generateKey({name:'ECDH', namedCurve:'P-256'}, true, ['deriveBits']);
```

The Glyph signing key remains **ECDSA-P256**. Identity signing is infrequent; P-256 is acceptable.

### 3.2 Migration Path

1. Existing V3 sessions: remain valid. V3 ratchet continues using ECDH-P256.
2. New sessions: detected by `protocol_version = 4` in peer's prekey bundle.
3. When peer uploads V4 bundle: next message exchange uses PQXDH + X25519.
4. Key derivation for IDB storage key (`vovin_vault_key`): remains PBKDF2/passphrase — agnostic to DH key type.

No forced session renegotiation. Sessions naturally upgrade as they expire and are re-established.

---

## Part 4 — Sealed Sender

### 4.1 Problem

The server currently stores:

```sql
-- Current DM record (V3)
sender_pial    VARCHAR   -- server knows who sent every message
recipient_pial VARCHAR
ciphertext     BYTEA
```

This is metadata. An adversary with server access knows the social graph of every conversation.

### 4.2 Design

In V4, the sender's identity is encrypted inside the message envelope. The server sees only:

```
SealedEnvelope {
  destination_shard_id:  bytes    // recipient shard — server can deliver
  sealed_sender_payload: bytes    // encrypted: sender identity + message
  server_timestamp:      u64
  expires_at:            u64 (nullable)
}
```

The `sealed_sender_payload` is:

```
inner = {
  sender_identity_key_pub: bytes,   // recipient verifies sender
  sender_ephemeral_pub:    bytes,   // for PQXDH
  kyber_ciphertext:        bytes,   // PQXDH KEM output
  message_ciphertext:      bytes,   // AES-256-GCM encrypted body
  message_iv:              bytes,
  message_type:            u8,
}

// Encrypt inner to recipient's identity key
sealed_sender_payload = AES-256-GCM(
  key = HKDF(X25519.dh(ephemeral.priv, recipient.identity_key_pub),
             PQXDH_PROTOCOL_INFO, "sealed_sender_v4"),
  plaintext = inner
)
```

### 4.3 What the Server Stores Per Message (V4)

```
message_id          UUID
destination_shard_id VARCHAR      // only the recipient shard — no sender ID
ciphertext          BYTEA         // sealed envelope
server_timestamp    TIMESTAMPTZ
expires_at          TIMESTAMPTZ   // nullable, for disappearing messages
```

**Not stored:** sender identity, sender IP (handled at Caddy/Nantar level), message type in cleartext, content.

### 4.4 Sender Verification

Recipient decrypts `sealed_sender_payload` and finds `sender_identity_key_pub`. Recipient verifies this matches the prekey bundle on record for that identity. If mismatch: warning displayed (possible key substitution / MITM).

### 4.5 WebCrypto Flow

```js
async function sealMessage(recipientBundle, plaintext) {
  const ephKP = await crypto.subtle.generateKey({name:'X25519'}, true, ['deriveBits']);
  const dh = await crypto.subtle.deriveBits(
    {name:'X25519', public: recipientBundle.identity_key_pub}, ephKP.privateKey, 256
  );
  const sealKey = await hkdf(dh, PQXDH_PROTOCOL_INFO, 'sealed_sender_v4', 32);
  const iv = crypto.getRandomValues(new Uint8Array(12));
  const inner = buildInner(plaintext, ephKP.publicKey);
  const ct = await aesGcmEncrypt(sealKey, iv, inner);
  return { destination_shard_id: recipientBundle.shard_id, sealed_sender_payload: ct, iv };
}
```

---

## Part 5 — Safety Numbers

Safety numbers allow users to verify they are talking to the person they think they are, catching MITM attacks and key substitution.

### 5.1 Derivation

```
safety_number = HKDF-SHA256(
  ikm  = viewer_identity_key_pub || peer_identity_key_pub,
  salt = b"F33DR_SAFETY_NUMBER_V4",
  info = viewer_pial_id || peer_pial_id,
  len  = 30
)

// Formatted as 12 groups of 5 decimal digits:
display = chunks(decimal_encode(safety_number), 5).join(' ')
// Example: "12345 67890 12345 67890 12345 67890 12345 67890 12345 67890 12345 67890"
```

`decimal_encode`: convert bytes to decimal, zero-pad each byte to 3 digits, concatenate.

### 5.2 Verification UI

In conversation settings → "Verify safety number":
- Display as QR code (scan with Signal, WhatsApp, or F33D3R mobile) AND as numeric string
- "Compare with [Name] in person or via a different channel"

### 5.3 Change Warning

When the recipient's identity key changes (new device registered without session renegotiation, or key compromise):

```
⚠️ Security keys changed for @handle.
Verify safety number before continuing.
```

The conversation input is not blocked — users can dismiss. The warning is logged in the conversation thread as a system message.

---

## Part 6 — Migration from V3

### 6.1 Zero-Downtime Strategy

1. **Phase 1 (ship):** V4 prekey bundle format supported server-side. Clients continue uploading V3 bundles.
2. **Phase 2:** Clients upload V4 bundles (`protocol_version = 4`) after vault unlock. V3 peers get V3 path; V4 peers get V4 path.
3. **Phase 3:** After 90 days, V3 bundle uploads deprecated. Existing V3 sessions continue until natural expiry.
4. **Phase 4:** V3 session path removed from client code.

### 6.2 Session Metadata

```js
// V3 session (current)
{ version: 2, rk: [...], ck_s: [...], ck_r: [...], dh_pub_b64: '...', ... }

// V4 session
{ version: 4, rk: [...], ck_s: [...], ck_r: [...],
  dh_pub_x25519: '...', spqr_pub: '...', spqr_priv_jwk: {...},
  messages_since_spqr: 0, protocol: 'triple_ratchet_v4' }
```

Sessions are stored in IDB `conversation_keys` store, keyed by `[conversation_id, device_id]`.

### 6.3 WebCrypto Availability

| Feature | Chrome | Firefox | Safari |
|---------|--------|---------|--------|
| X25519 (ECDH) | 113+ | 119+ | 17.4+ |
| ECDSA-P256 | All | All | All |
| AES-GCM | All | All | All |
| PBKDF2 | All | All | All |
| Kyber-768 | ✗ (WASM required) | ✗ | ✗ |

The Kyber WASM bundle (`kyber768.wasm.js`) must be bundled with the app at `/static/js/kyber768.wasm.js`. Approximate size: 85 KB gzipped. Load it asynchronously on messages page mount; do not block vault unlock on WASM load.

---

## Implementation Order

1. Server: accept V4 prekey bundle format (schema change, no client impact)
2. Client: generate X25519 identity keys for new vault setups (V3 vaults keep P-256 until reset)
3. Client: PQXDH session initiation for V4 peers
4. Client: Triple Ratchet + SPQR ratchet step
5. Server: Sealed Sender schema (strip sender from DB, new envelope format)
6. Client: Sealed Sender envelope wrapping/unwrapping
7. UI: Safety number display + verification flow
8. Client: WASM Kyber integration

Items 1–4 can ship together as V4-alpha. Items 5–6 are Sealed Sender and require a server schema migration. Items 7–8 are independent.

---

*Document version: 2026-05-10. Review required before implementation begins.*
