# Vovin — Messaging Brain

The end-to-end encrypted messaging service for F33D3R.

**Brain name:** Vovin  
**Port:** 8092  
**Database:** f33d3r_msg (PostgreSQL 18)  
**Language:** Rust 1.95.0 (axum, sqlx, tokio)

Vovin handles encrypted message relay. It never sees plaintext — all encryption and decryption happens on the client using WebCrypto (ECDH-P256 + AES-256-GCM). Vovin stores and relays ciphertext only.

---

## Identity model

Vovin uses **PIAL UUIDs as messaging identities**, not handles or account IDs.

- Every user registers a public key against their PIAL UUID: `POST /v1/identity`
- The PIAL UUID is injected by Nantar as `X-Vovin-Identity` on every proxied request
- Handle → PIAL lookup: `GET /v1/identity/by-handle/:handle`
- This means messaging survives handle changes and account resets

---

## Encryption protocol (V1 — current)

```
Sender:
  1. Fetch recipient's public key from /v1/identity/:pial_id
  2. Derive shared secret: ECDH(sender_privkey, recipient_pubkey) → shared_secret
  3. Encrypt: AES-256-GCM(shared_secret, plaintext) → {iv, ciphertext}
  4. POST /v1/dm with {sender, recipient, sender_pub, iv, ciphertext}

Recipient:
  1. GET /v1/dm/:pial_id → list of encrypted messages
  2. For each message: ECDH(my_privkey, sender_pub) → shared_secret
  3. Decrypt: AES-256-GCM(shared_secret, {iv, ciphertext}) → plaintext
```

Vovin stores: sender PIAL, recipient PIAL, sender public key, IV, ciphertext.  
Vovin never has: plaintext, shared secret, private keys.

---

## Vault system (client-side key storage)

Private keys are generated in the browser via WebCrypto and stored in IndexedDB (`vovin_v1` database). The vault is protected by one of:

- **Device biometric / passkey** — Uses platform authenticator (Face ID, Touch ID, Windows Hello)
- **Passphrase** — PBKDF2(passphrase, salt, 210,000 rounds) → AES-GCM wrapping key
- **No lock** — Key stored in IndexedDB without wrapping (development only)

Key setup happens during onboarding (`/onboard/setup`) and can be reset from the Messages page.

**Important:** Keys are device-specific. Setting up Vovin on a new browser generates a new keypair and overwrites the registered public key. Messages encrypted with the old key become unreadable on the old device.

---

## API

### Identity

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/v1/identity` | Register identity (PIAL UUID + public key + handle) |
| `GET` | `/v1/identity/:pial_id` | Get identity by PIAL UUID (returns public key) |
| `GET` | `/v1/identity/by-handle/:handle` | Resolve handle → PIAL UUID + public key |

### Direct messages (V1 — ECDH relay)

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/v1/dm` | Send encrypted DM (stores ciphertext) |
| `GET` | `/v1/dm/:pial_id` | Fetch all DMs for a PIAL (inbox) |

### Pre-keys (V2 roadmap — X3DH)

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/v1/prekeys/signed` | Upload signed prekey |
| `POST` | `/v1/prekeys/onetime` | Upload one-time prekeys |
| `GET` | `/v1/prekeys/:identity` | Fetch a prekey bundle |

### Groups

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/v1/groups` | Create group chat |
| `GET` | `/v1/groups/:group_id/members` | List members |
| `POST` | `/v1/groups/:group_id/members` | Add member |

### Real-time

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/v1/push/:pial_id` | WebSocket — real-time message push |
| `GET` | `/v1/stats` | Protocol statistics |
| `GET` | `/health` | Service health |

---

## V1 → V2 roadmap

| Feature | V1 (current) | V2 (planned) |
|---------|-------------|--------------|
| Key agreement | ECDH-P256 per-message | X3DH (Extended Triple Diffie-Hellman) |
| Forward secrecy | None | Double Ratchet per message |
| Multi-device | Single key per PIAL | Per-device keys, fan-out delivery |
| Group encryption | Not implemented | Sender key protocol |

V2 is fully backward-compatible at the Vovin wire level.

---

## Nantar proxy

All browser Vovin calls go through Nantar's proxy at `/vovin/*`:

```
Browser → GET /vovin/v1/identity/by-handle/alice
  → Nantar validates session cookie
  → Injects X-Vovin-Identity: <pial_id>
  → Forwards to Vovin :8092/v1/identity/by-handle/alice
  → Returns response to browser
```

This means Vovin is never directly exposed to the internet in production.

---

## Project structure

```
aethyr-msg/
├── src/
│   ├── handlers.rs     ← All HTTP + WebSocket endpoints
│   ├── models.rs       ← Identity, prekey, message structs
│   ├── db.rs           ← PostgreSQL schema + queries
│   ├── relay.rs        ← Message relay logic + push registry
│   ├── crypto.rs       ← ECDH, AES-GCM helpers (server-side validation only)
│   ├── x3dh.rs         ← X3DH key agreement (V2 — in development)
│   ├── ratchet.rs      ← Double Ratchet (V2 — in development)
│   ├── protocol.rs     ← Message framing + versioning
│   └── main.rs         ← Server startup
└── docker/
    └── Dockerfile      ← rust:1.95.0-slim-bookworm builder
```

---

## Running locally

```bash
# Via root compose (recommended)
docker compose -f ../docker-compose.local.yml up -d aethyr-msg

# Check health
curl http://localhost:8092/health

# Check stats
curl http://localhost:8092/v1/stats
```
