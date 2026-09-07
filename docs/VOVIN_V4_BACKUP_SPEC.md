# VOVIN V4 — Backup and Recovery Specification

**Status:** Specification — not yet implemented  
**Depends on:** VOVIN_V4_CRYPTO_SPEC.md  
**Target:** Sprint 2, Milestone M2

---

## Problem

Vovin stores all message history in IndexedDB on the user's device. If a user loses their device, clears their browser, or switches to a new browser, their entire message history — and the encryption keys to read it — are gone permanently. There is no recovery path.

This is the single biggest reason messaging feels untrustworthy. Users instinctively distrust a messenger where losing your phone means losing everything, even if the cryptographic design is correct.

---

## Design Philosophy

The backup system is modelled on Signal's 2025 Secure Backup:

- **Zero-knowledge**: F33D3R servers store only an encrypted blob. They cannot decrypt it, link it to a user account, or read its contents.
- **Single recovery key**: The user generates a 64-character key on their device. It never touches the server. Losing it means losing the backup — permanently. F33D3R cannot help recover it.
- **User owns the key**: F33D3R cannot revoke, reset, or generate the key for the user. The server is a dumb blob store.

---

## Part 1 — Recovery Key

### 1.1 Generation

```js
// 64-character alphanumeric key, generated entirely client-side
const alphabet = 'ABCDEFGHJKLMNPQRSTUVWXYZ23456789'; // 32 chars, no ambiguous chars
const bytes = crypto.getRandomValues(new Uint8Array(64));
const key = Array.from(bytes).map(b => alphabet[b % 32]).join('');
// → 64 chars, e.g. "XXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX"
```

Why 64 characters from a 32-char alphabet: `log2(32^64) = 320 bits` of entropy. Brute-force infeasible.

### 1.2 Display Format

Displayed to user as 8 groups of 8, separated by spaces:

```
XXXXXXXX XXXXXXXX XXXXXXXX XXXXXXXX
XXXXXXXX XXXXXXXX XXXXXXXX XXXXXXXX
```

Shown in a monospace font. Copy button provided. User must tap "I've saved this key" before backup activates. This confirmation is stored in IDB `backup_state.recovery_key_confirmed = true`.

### 1.3 The Key Is Never Uploaded

The recovery key is never sent to any server in any form. It is displayed once during setup and stored only where the user saves it (password manager, written down, etc.). F33D3R has no copy. There is no "forgot recovery key" flow — if lost, the backup is inaccessible.

### 1.4 IDB Storage

The recovery key itself is **never stored in IDB**. IDB stores only:

```js
backup_state = {
  backup_enabled: true,
  last_backup_at: ISO8601,
  backup_id_hash: hex_string,  // HKDF(key, "backup-id") — for fetching the blob
  recovery_key_confirmed: true
  // NOT: recovery_key itself
}
```

---

## Part 2 — Backup Encryption Architecture

### 2.1 Key Derivation Chain

```
recovery_key (64-char string, user-held)
    │
    ├─ HKDF(key, "F33DR_BACKUP_ID_V4",    salt="backup") → backup_id (32 bytes)
    │   Used to identify the blob on the server without revealing user identity.
    │   backup_id is the only server-side identifier. No PIAL association.
    │
    └─ HKDF(key, "F33DR_BACKUP_KEY_V4",   salt="backup") → backup_key (32 bytes)
        │
        └─ AES-256-GCM key for encrypting all backup content
```

### 2.2 What Is Backed Up

**Included:**

| Data | Notes |
|------|-------|
| Message ciphertext + IV | Already encrypted E2E — double-encrypted in backup |
| Conversation metadata | Participant list, last_read_id, disappearing timer settings |
| Ratchet session state | Allows session resumption on new device |
| Prekey bundle state | Signed prekeys, SPQR state |
| Disappearing message settings | Per-conversation timer |
| Pinned conversation list | Ordering only |

**Excluded:**

| Data | Reason |
|------|--------|
| View-once messages | Privacy design — deliberately ephemeral |
| Messages expiring within 24 hours | Would expire before restoration |
| The recovery key | Only on user's device |
| Voice/media blobs > 45 days old | Storage budget |

### 2.3 Backup Payload Structure

```js
// Assembled client-side before encryption
const payload = {
  version: 4,
  created_at: Date.now(),
  messages: [/* raw encrypted message objects from IDB */],
  conversations: [/* conversation metadata objects */],
  ratchet_sessions: [/* per-conversation ratchet state */],
  prekey_state: { /* current signed prekeys, SPQR keys */ },
  pinned_conversations: [/* pial_ids in order */],
  media_cutoff_at: ISO8601,  // 45 days ago — media refs after this are included
};

// Encrypt
const iv = crypto.getRandomValues(new Uint8Array(12));
const ct = await aesGcmEncrypt(backup_key, iv, JSON.stringify(payload));
const blob = concat(iv, ct);  // 12 bytes IV + ciphertext
```

### 2.4 Media Backup

Voice notes and images sent/received in the last 45 days are included in the backup. Each media item:

1. Is already encrypted at rest (AFF chunks with per-file key, or voice blob)
2. Gets an additional layer of AES-256-GCM encryption with `backup_key`
3. Gets random padding (nearest 256-byte boundary) before encryption to obscure size

```js
// Pad to nearest multiple of 256 bytes to prevent size correlation
const padded = padToMultiple(mediaBytes, 256);
const mediaBackup = await aesGcmEncrypt(backup_key, randomIv(), padded);
```

Padding prevents cross-user correlation attacks where an adversary observes backup blob sizes to infer media content.

---

## Part 3 — Backup Storage

### 3.1 Server API

```
PUT /vovin/v1/backup/{backup_id_hex}
  Body: encrypted blob (binary)
  Max size: 100 MB
  Response: { stored_at, size_bytes }

GET /vovin/v1/backup/{backup_id_hex}
  Response: encrypted blob (binary) or 404
  No auth required — backup_id is the credential

DELETE /vovin/v1/backup/{backup_id_hex}
  Used during key rotation
```

The server does not authenticate these requests beyond the `backup_id` itself. Knowing the `backup_id` (derived from the recovery key) is the credential. This means brute-forcing `backup_id` would require breaking 256 bits of HKDF output — infeasible.

### 3.2 MinIO Bucket

```
vovin-backups/
  {backup_id_hex}.enc   // encrypted blob
  
// Separate from:
vovin-media/            // message media (not backed up via this mechanism)
```

Retention: latest backup only. New upload overwrites previous. No versioning.

### 3.3 Backup Tiers

| Tier | Max size | Notes |
|------|----------|-------|
| Free | 100 MB text + 45-day media | All users, no payment required |
| Paid | TBD | Defer — design only |

At 10,000 users, worst-case free tier storage: `10,000 × 100 MB = 1 TB`. This is manageable on current infrastructure. Monitor via `vovin_backup_archives_total` and `vovin_backup_size_bytes_total`.

### 3.4 Backup Refresh Cycle

Backup refreshes automatically:
- After vault unlock if last backup > 24 hours ago
- On explicit user tap in Settings → Messaging → Backup → Back Up Now
- Before major app updates (client-triggered)

```js
async function maybeRefreshBackup() {
  const state = await IDB.get('backup_state');
  if (!state?.backup_enabled) return;
  const age = Date.now() - new Date(state.last_backup_at).getTime();
  if (age < 24 * 3600 * 1000) return;
  await performBackup();
}
```

---

## Part 4 — Recovery Flow

### 4.1 Trigger

User installs fresh browser / new device. They navigate to f33d3r.com and log in with their F33D3R credentials (PIAL account). The messages page runs `boot()` which calls `IDB.get('meta')` — no vault metadata found. Instead of showing the setup overlay immediately, the system first asks: "Restore from backup?"

```
┌─────────────────────────────────────────┐
│  🔒 Your messages are end-to-end        │
│     encrypted and backed up.           │
│                                         │
│  Do you have a recovery key?            │
│                                         │
│  [Enter recovery key]  [Start fresh]   │
└─────────────────────────────────────────┘
```

### 4.2 Recovery Steps

```js
async function restoreFromBackup(recoveryKey) {
  // 1. Derive backup_id from key
  const keyBytes = encodeRecoveryKey(recoveryKey.toUpperCase().replace(/\s/g,''));
  const backupId = await hkdf(keyBytes, 'F33DR_BACKUP_ID_V4', 'backup', 32);
  const backupIdHex = toHex(backupId);

  // 2. Fetch encrypted blob from server
  const r = await fetch(`/vovin/v1/backup/${backupIdHex}`);
  if (r.status === 404) {
    showError('No backup found for this key. Check the key and try again.');
    return;
  }
  const blob = await r.arrayBuffer();

  // 3. Derive backup_key and decrypt — entirely client-side
  const backupKey = await hkdf(keyBytes, 'F33DR_BACKUP_KEY_V4', 'backup', 32);
  let payload;
  try {
    const iv = blob.slice(0, 12);
    const ct = blob.slice(12);
    const pt = await aesGcmDecrypt(backupKey, iv, ct);
    payload = JSON.parse(pt);
  } catch {
    showError('Incorrect recovery key. Decryption failed.');
    return;
  }

  // 4. Restore to IDB
  await restoreIDB(payload);

  // 5. Generate new device keys — do NOT reuse backed-up keys
  await doGenerate('none', null);  // or prompt for vault method

  // 6. Renegotiate sessions — old ratchet state restores conversation history,
  //    but new messages will use fresh sessions
  await syncFromServer();

  // 7. Update backup_state
  await IDB.set('backup_state', {
    backup_enabled: true,
    last_backup_at: new Date().toISOString(),
    backup_id_hash: backupIdHex,
    recovery_key_confirmed: true,
  });
}
```

### 4.3 Wrong Key Handling

If decryption fails (wrong key), the error is caught client-side. The server returns a blob regardless — it cannot tell if the key is correct. The 404 means no backup exists for that `backup_id`, not that the key is wrong.

The client must not reveal to the server whether decryption succeeded. All error handling is local.

### 4.4 After Restoration

- All historical messages are decrypted and visible (vault unlocked, session state restored)
- New messages from peers arrive on existing sessions (if ratchet state was backed up and sessions are still valid)
- If sessions have expired: peer sends a new session initiation on first message; historical messages remain readable, new ones start fresh sessions
- Media: downloaded lazily on demand from Vovin (voice files, AFF chunks still on server for 90-day retention window)

---

## Part 5 — Recovery Key Rotation

User can generate a new recovery key at any time. Old backup is deleted, new backup is created.

```
Settings → Messaging → Backup → Rotate Recovery Key
  1. Generate new 64-char key
  2. Display to user (MUST save before confirming)
  3. User taps "I've saved the new key"
  4. DELETE /vovin/v1/backup/{old_backup_id_hex}
  5. PUT /vovin/v1/backup/{new_backup_id_hex}  with full re-encrypted backup
  6. Update IDB backup_state
```

Old key is immediately invalidated (old backup deleted from server). There is no grace period.

---

## Part 6 — UI Specification

### 6.1 First-Time Display

Shown after the user sends their **first message** (not at signup — users haven't invested yet):

```
┌─────────────────────────────────────────────────────┐
│  🔒 Save your recovery key                         │
│                                                      │
│  This key is the only way to restore your messages  │
│  on a new device. F33D3R does not have a copy.      │
│                                                      │
│  XXXXXXXX XXXXXXXX XXXXXXXX XXXXXXXX               │
│  XXXXXXXX XXXXXXXX XXXXXXXX XXXXXXXX               │
│                                              [Copy] │
│                                                      │
│  ⚠️  If you lose this key, your backup is gone.    │
│                                                      │
│  [ I've saved this key ]   [ Remind me later ]     │
└─────────────────────────────────────────────────────┘
```

"Remind me later" shows the warning banner (below) on every messages page load until confirmed.

### 6.2 Settings Card

```
Settings → Messaging → Backup

  Backup status:  ● Active (last backed up 2 hours ago)
  Storage used:   4.2 MB of 100 MB

  [View recovery key]     ← requires vault unlock, shows key again
  [Rotate recovery key]
  [Back up now]
  [Disable backup]        ← with strong warning
```

### 6.3 Warning Banner (backup not enabled)

Shown at top of messages page when `backup_state.backup_enabled = false` or `recovery_key_confirmed = false`:

```
┌─────────────────────────────────────────────────────────────────┐
│ ⚠️  Your messages aren't backed up.                            │
│    If you lose this device, your history is gone.  [Set up →] │
└─────────────────────────────────────────────────────────────────┘
```

### 6.4 Recovery Key View

Settings → Messaging → Backup → View recovery key:

- Requires vault unlock (passphrase/passkey)
- Derives backup_id from stored backup_state, shows the key stored in the user's password manager... wait — the key is NOT stored anywhere by us. We cannot show it again.
- **Correction**: after the first display, the key is gone. We cannot re-display it. The Settings card shows: "Recovery key saved. To change your key, use Rotate Recovery Key." If the user forgot their key, the only option is to rotate (generating a new backup, losing the old history link).

This is correct and expected — and should be clearly communicated on first display:  
*"Save this key now. We cannot show it to you again."*

---

## Implementation Notes

### Vovin Schema Changes

```sql
CREATE TABLE backup_archives (
  backup_id    VARCHAR(64) PRIMARY KEY,  -- hex(HKDF(key, "backup-id"))
  blob_path    VARCHAR NOT NULL,          -- MinIO path
  size_bytes   BIGINT NOT NULL,
  created_at   TIMESTAMPTZ DEFAULT NOW(),
  updated_at   TIMESTAMPTZ DEFAULT NOW()
  -- No pial_shard_id — zero-knowledge by design
);
```

### Nantar Routes (new)

```go
mux.HandleFunc("PUT /vovin/v1/backup/", h.rlWrite.Limit(h.requireHandle(h.vovinProxy)))
mux.HandleFunc("GET /vovin/v1/backup/", h.rlRead.Limit(h.requireHandle(h.vovinProxy)))
mux.HandleFunc("DELETE /vovin/v1/backup/", h.rlWrite.Limit(h.requireHandle(h.vovinProxy)))
```

Note: `requireHandle` checks the F33D3R session (ensures user is logged in) but does not inject PIAL into the request for backup endpoints — the backup_id is the credential, not the PIAL.

---

*Document version: 2026-05-10. Review required before implementation begins.*
