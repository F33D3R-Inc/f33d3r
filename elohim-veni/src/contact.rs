//! Contact policy, contact capabilities, and key-bundle signing.
//!
//! A Number resolves to an identity in Manhattan. What that resolution is worth
//! is decided here: possession of a Number buys an attempt at contact under that
//! Number's policy, and never key material by itself.

use anyhow::{bail, Result};
use base64::engine::general_purpose::STANDARD as B64;
use base64::Engine;
use ed25519_dalek::{Signer, SigningKey};
use rand::rngs::OsRng;
use rand::RngCore;
use serde::Serialize;
use sha2::{Digest, Sha256};
use sqlx::PgPool;
use uuid::Uuid;

// ── Policy vocabulary ─────────────────────────────────────────────────────────

/// Every contact policy, grounded in signals F33D3R actually holds: the follow
/// graph, a presented Number, and a presented capability.
pub const POLICIES: [&str; 6] = [
    "open",
    "followers",
    "mutuals",
    "number_only",
    "capability_only",
    "closed",
];

pub fn is_policy(value: &str) -> bool {
    POLICIES.contains(&value)
}

/// The default applied to a Number with no policy row of its own.
pub const DEFAULT_NUMBER_POLICY: &str = "number_only";

/// The default applied to the @handle path, preserving current behaviour for
/// every identity that has never set a policy.
pub const DEFAULT_HANDLE_POLICY: &str = "open";

// ── The decision ──────────────────────────────────────────────────────────────

/// Inputs to a contact decision.
///
/// `follows` and `mutual` are READ by this brain from Manhattan's association
/// plane — see `follow_graph` — never asserted to it by the brain that owns the
/// follow graph. They were once assertions on the wire, and a security decision
/// resting on a boolean the caller sets is not a decision. The graph is
/// feed-engine's to own; the fact is Manhattan's to publish; the rule applied to
/// it is this brain's.
pub struct ContactFacts {
    pub via_number: bool,
    pub follows: bool,
    pub mutual: bool,
    pub granted: bool,
    pub capability_ok: bool,
}

/// Pure decision, so the rule is testable without a database.
///
/// `allow` opens a channel. `request` permits exactly one contact request and no
/// conversation. `deny` yields nothing.
pub fn decide(policy: &str, f: &ContactFacts) -> (&'static str, &'static str) {
    if f.granted {
        return ("allow", "standing_grant");
    }
    if f.capability_ok {
        return ("allow", "capability_redeemed");
    }
    match policy {
        "open" => ("allow", "policy_open"),
        "followers" => {
            if f.follows {
                ("allow", "follows_owner")
            } else {
                ("request", "not_a_follower")
            }
        }
        "mutuals" => {
            if f.mutual {
                ("allow", "mutual_follow")
            } else {
                ("request", "not_mutual")
            }
        }
        "number_only" => {
            if f.via_number {
                ("allow", "number_presented")
            } else {
                ("request", "number_required")
            }
        }
        "capability_only" => ("deny", "capability_required"),
        "closed" => ("deny", "policy_closed"),
        _ => ("deny", "unknown_policy"),
    }
}

// ── Contact capabilities ──────────────────────────────────────────────────────

/// Crockford base32, matching the Number alphabet. 32 symbols × 5 bits = 160 bits.
const CAP_ALPHABET: &[u8; 32] = b"0123456789ABCDEFGHJKMNPQRSTVWXYZ";
const CAP_LEN: usize = 32;

/// Mints a 160-bit contact capability. Never spoken — it travels as a link or a
/// QR code, which is what lets it be long enough to be bearer-safe.
pub fn mint_capability_secret() -> String {
    let mut raw = [0u8; CAP_LEN];
    OsRng.fill_bytes(&mut raw);
    raw.iter()
        .map(|b| CAP_ALPHABET[(*b % 32) as usize] as char)
        .collect()
}

/// Canonicalises a presented capability, rejecting anything of the wrong shape
/// before it reaches the database.
pub fn normalise_capability(input: &str) -> Option<String> {
    let symbols: Vec<u8> = input
        .trim()
        .bytes()
        .filter(|b| !matches!(b, b'-' | b' ' | b'_'))
        .map(|b| b.to_ascii_uppercase())
        .collect();
    if symbols.len() != CAP_LEN || !symbols.iter().all(|b| CAP_ALPHABET.contains(b)) {
        return None;
    }
    String::from_utf8(symbols).ok()
}

/// What is stored. The secret itself never lands in a column.
pub fn capability_hash(secret: &str) -> Vec<u8> {
    Sha256::digest(secret.as_bytes()).to_vec()
}

// ── Key bundle ────────────────────────────────────────────────────────────────

/// One device's messaging key. Each device of an account registers its own row;
/// `device_id` is empty for the key an account registered before it named its
/// devices.
#[derive(Debug, Serialize)]
pub struct DeviceKey {
    pub device_id: String,
    pub public_key_b64: String,
    pub algorithm: String,
}

/// The signed payload: one ACCOUNT's per-device messaging keys and nothing else.
///
/// It carries no PIAL and no identity-wide key. A bundle is fetched by whoever
/// holds a contact address or a Number, and anything identity-wide in it — the
/// PIAL, or a key every persona of one person shares — would let that holder
/// link two of the person's accounts. The account id is what the holder is
/// already being introduced to.
#[derive(Debug, Serialize)]
pub struct KeyBundle {
    pub account_id: String,
    pub issued_at: String,
    pub devices: Vec<DeviceKey>,
}

/// Signs assembled bundles with an Ed25519 service key supplied by deployment
/// configuration. The key is read from the environment and never persisted.
#[derive(Clone)]
pub struct BundleSigner {
    key: Option<std::sync::Arc<SigningKey>>,
}

impl BundleSigner {
    /// Unset seed yields an unsigned signer, which reports itself as unsigned on
    /// every response. A seed that is present but malformed is a boot failure.
    pub fn from_env() -> Result<Self> {
        let raw = std::env::var("CONTACT_BUNDLE_SIGNING_SEED").unwrap_or_default();
        if raw.trim().is_empty() {
            return Ok(Self { key: None });
        }
        let bytes = B64
            .decode(raw.trim())
            .map_err(|e| anyhow::anyhow!("CONTACT_BUNDLE_SIGNING_SEED is not base64: {e}"))?;
        if bytes.len() != 32 {
            bail!(
                "CONTACT_BUNDLE_SIGNING_SEED must decode to 32 bytes, got {}",
                bytes.len()
            );
        }
        let mut seed = [0u8; 32];
        seed.copy_from_slice(&bytes);
        Ok(Self {
            key: Some(std::sync::Arc::new(SigningKey::from_bytes(&seed))),
        })
    }

    pub fn configured(&self) -> bool {
        self.key.is_some()
    }

    pub fn public_b64(&self) -> Option<String> {
        self.key
            .as_ref()
            .map(|k| B64.encode(k.verifying_key().to_bytes()))
    }

    pub fn sign(&self, payload: &[u8]) -> Option<String> {
        self.key
            .as_ref()
            .map(|k| B64.encode(k.sign(payload).to_bytes()))
    }
}

// ── Storage ───────────────────────────────────────────────────────────────────
//
// Every row on the contact plane records the ACCOUNT that acted or was acted on
// (migration 0004). A PIAL is one person's root identity and one person may hold
// several accounts; a grant, a request, a capability, a Number or a key that
// belonged to the PIAL would answer the same way for every persona of it, and
// that sameness is exactly what links them. The PIAL is kept only where the
// identity owns a row's lifecycle.

/// The identity a live account is bound to. `None` for an account that does not
/// exist, is deactivated or is purged: such an account takes no contact action.
pub async fn account_pial(pool: &PgPool, account: Uuid) -> Result<Option<Uuid>> {
    let row: Option<(Uuid,)> = sqlx::query_as(
        "SELECT pial_id FROM accounts
          WHERE id = $1 AND deactivated_at IS NULL AND purged_at IS NULL",
    )
    .bind(account)
    .fetch_optional(pool)
    .await?;
    Ok(row.map(|r| r.0))
}

/// The account a row made before the contact plane recorded accounts is
/// attributed to: the PIAL's primary account, or its earliest when it has no
/// primary — the rule migration 0004 backfilled with.
pub async fn primary_account(pool: &PgPool, pial: Uuid) -> Result<Option<Uuid>> {
    let row: Option<(Uuid,)> = sqlx::query_as(
        "SELECT id FROM accounts WHERE pial_id = $1
          ORDER BY is_primary DESC, created_at, id LIMIT 1",
    )
    .bind(pial)
    .fetch_optional(pool)
    .await?;
    Ok(row.map(|r| r.0))
}

pub async fn get_contact_policy(pool: &PgPool, account: Uuid) -> Result<(String, String)> {
    let row: Option<(String, String)> = sqlx::query_as(
        "SELECT handle_policy, default_number_policy FROM contact_account_policies WHERE account_id = $1",
    )
    .bind(account)
    .fetch_optional(pool)
    .await?;
    Ok(row.unwrap_or_else(|| {
        (
            DEFAULT_HANDLE_POLICY.to_string(),
            DEFAULT_NUMBER_POLICY.to_string(),
        )
    }))
}

pub async fn set_contact_policy(
    pool: &PgPool,
    account: Uuid,
    handle_policy: &str,
    default_number_policy: &str,
) -> Result<()> {
    sqlx::query(
        "INSERT INTO contact_account_policies (account_id, handle_policy, default_number_policy)
         VALUES ($1, $2, $3)
         ON CONFLICT (account_id) DO UPDATE
            SET handle_policy = EXCLUDED.handle_policy,
                default_number_policy = EXCLUDED.default_number_policy,
                updated_at = NOW()",
    )
    .bind(account)
    .bind(handle_policy)
    .bind(default_number_policy)
    .execute(pool)
    .await?;
    Ok(())
}

/// A Number's own policy row: the account that holds it, its policy, its label.
pub async fn get_number_policy(
    pool: &PgPool,
    number_node: Uuid,
) -> Result<Option<(Uuid, String, String)>> {
    let row: Option<(Uuid, String, String)> = sqlx::query_as(
        "SELECT account_id, policy, label FROM number_policies WHERE number_node_id = $1",
    )
    .bind(number_node)
    .fetch_optional(pool)
    .await?;
    Ok(row)
}

// A Number's policy is no longer written here. It is one column of a row that
// also carries the Number's label, its lease and its use budget, and those four
// are one decision the owner makes at one moment — so they are written by one
// statement, in number_lease::attach. Two writers for one row is how half of it
// ends up reflecting a choice nobody made.

/// The policy rows of every Number one account holds.
pub async fn number_policies_for(pool: &PgPool, account: Uuid) -> Result<Vec<(Uuid, String, String)>> {
    let rows: Vec<(Uuid, String, String)> = sqlx::query_as(
        "SELECT number_node_id, policy, label FROM number_policies WHERE account_id = $1",
    )
    .bind(account)
    .fetch_all(pool)
    .await?;
    Ok(rows)
}

/// Every Number node of an identity that has a policy row, with the account
/// holding it — how an identity-wide Manhattan listing is split per account.
pub async fn number_holders(pool: &PgPool, pial: Uuid) -> Result<Vec<(Uuid, Uuid)>> {
    let rows: Vec<(Uuid, Uuid)> = sqlx::query_as(
        "SELECT number_node_id, account_id FROM number_policies WHERE pial_id = $1",
    )
    .bind(pial)
    .fetch_all(pool)
    .await?;
    Ok(rows)
}

pub async fn drop_number_policy(pool: &PgPool, number_node: Uuid, account: Uuid) -> Result<()> {
    sqlx::query("DELETE FROM number_policies WHERE number_node_id = $1 AND account_id = $2")
        .bind(number_node)
        .bind(account)
        .execute(pool)
        .await?;
    Ok(())
}

pub async fn has_grant(pool: &PgPool, owner_account: Uuid, peer_account: Uuid) -> Result<bool> {
    let found: Option<(Uuid,)> = sqlx::query_as(
        "SELECT id FROM contact_grants
          WHERE owner_account = $1 AND peer_account = $2 AND revoked_at IS NULL",
    )
    .bind(owner_account)
    .bind(peer_account)
    .fetch_optional(pool)
    .await?;
    Ok(found.is_some())
}

pub async fn grant_contact(
    pool: &PgPool,
    owner_account: Uuid,
    owner_pial: Uuid,
    peer_account: Uuid,
    source: &str,
    number_node: Option<Uuid>,
) -> Result<()> {
    sqlx::query(
        "INSERT INTO contact_grants (owner_account, owner_pial, peer_account, source, number_node_id)
         VALUES ($1, $2, $3, $4, $5)
         ON CONFLICT (owner_account, peer_account) DO UPDATE
            SET revoked_at = NULL, source = EXCLUDED.source",
    )
    .bind(owner_account)
    .bind(owner_pial)
    .bind(peer_account)
    .bind(source)
    .bind(number_node)
    .execute(pool)
    .await?;
    Ok(())
}

pub async fn mint_capability(
    pool: &PgPool,
    account: Uuid,
    pial: Uuid,
    number_node: Option<Uuid>,
    label: &str,
    max_uses: Option<i32>,
) -> Result<String> {
    let secret = mint_capability_secret();
    sqlx::query(
        "INSERT INTO contact_capabilities (account_id, pial_id, number_node_id, secret_sha256, label, max_uses)
         VALUES ($1, $2, $3, $4, $5, $6)",
    )
    .bind(account)
    .bind(pial)
    .bind(number_node)
    .bind(capability_hash(&secret))
    .bind(label)
    .bind(max_uses)
    .execute(pool)
    .await?;
    Ok(secret)
}

pub async fn revoke_capability(pool: &PgPool, account: Uuid, capability_id: Uuid) -> Result<u64> {
    let done = sqlx::query(
        "UPDATE contact_capabilities SET revoked_at = NOW()
          WHERE id = $1 AND account_id = $2 AND revoked_at IS NULL",
    )
    .bind(capability_id)
    .bind(account)
    .execute(pool)
    .await?;
    Ok(done.rows_affected())
}

pub async fn list_capabilities(
    pool: &PgPool,
    account: Uuid,
) -> Result<Vec<(Uuid, String, Option<Uuid>, i32, Option<i32>, bool)>> {
    let rows: Vec<(Uuid, String, Option<Uuid>, i32, Option<i32>, bool)> = sqlx::query_as(
        "SELECT id, label, number_node_id, uses, max_uses, (revoked_at IS NOT NULL)
           FROM contact_capabilities WHERE account_id = $1 ORDER BY created_at DESC",
    )
    .bind(account)
    .fetch_all(pool)
    .await?;
    Ok(rows)
}

/// Redeems a presented capability against one owning account, consuming a use.
/// Returns the capability id when it was live, unexpired, within its use cap,
/// and — when the capability is scoped to a Number — presented with that Number.
/// A link minted by one persona never opens another persona of the same person.
pub async fn redeem_capability(
    pool: &PgPool,
    owner_account: Uuid,
    secret: &str,
    presented_number_node: Option<Uuid>,
) -> Result<Option<Uuid>> {
    let canonical = match normalise_capability(secret) {
        Some(c) => c,
        None => return Ok(None),
    };
    let row: Option<(Uuid,)> = sqlx::query_as(
        "UPDATE contact_capabilities
            SET uses = uses + 1
          WHERE secret_sha256 = $1
            AND account_id = $2
            AND revoked_at IS NULL
            AND (expires_at IS NULL OR expires_at > NOW())
            AND (max_uses IS NULL OR uses < max_uses)
            AND (number_node_id IS NULL OR number_node_id = $3)
      RETURNING id",
    )
    .bind(capability_hash(&canonical))
    .bind(owner_account)
    .bind(presented_number_node)
    .fetch_optional(pool)
    .await?;
    Ok(row.map(|r| r.0))
}

/// Opens one contact request from one account to another, and tells the
/// recipient account — the persona that was asked, never the person's primary.
pub async fn open_request(
    pool: &PgPool,
    owner_account: Uuid,
    owner_pial: Uuid,
    requester_account: Uuid,
    number_node: Option<Uuid>,
    note: &str,
) -> Result<Option<Uuid>> {
    let mut tx = pool.begin().await?;
    let row: Option<(Uuid,)> = sqlx::query_as(
        "INSERT INTO contact_requests (owner_account, owner_pial, requester_account, number_node_id, note)
         VALUES ($1, $2, $3, $4, $5)
         ON CONFLICT DO NOTHING
      RETURNING id",
    )
    .bind(owner_account)
    .bind(owner_pial)
    .bind(requester_account)
    .bind(number_node)
    .bind(note)
    .fetch_optional(&mut *tx)
    .await?;
    if let Some((id,)) = row {
        crate::notifications::enqueue(
            &mut *tx,
            &crate::notifications::Event {
                event_id: format!("elohim:contact_request:{id}"),
                recipient_account: Some(owner_account.to_string()),
                recipient_pial: owner_pial.to_string(),
                actor_account: Some(requester_account.to_string()),
                kind: "contact_request".into(),
                target_id: requester_account.to_string(),
                target_type: "user".into(),
                payload: serde_json::json!({}),
                push_title: "New contact request".into(),
                push_body: String::new(),
                push_url: "/settings/contact".into(),
            },
        )
        .await?;
    }
    tx.commit().await?;
    Ok(row.map(|r| r.0))
}

/// The pending requests waiting on one account: (id, requester account, note,
/// status, created).
pub async fn list_requests(
    pool: &PgPool,
    owner_account: Uuid,
) -> Result<Vec<(Uuid, Uuid, String, String, chrono::DateTime<chrono::Utc>)>> {
    let rows: Vec<(Uuid, Uuid, String, String, chrono::DateTime<chrono::Utc>)> = sqlx::query_as(
        "SELECT id, requester_account, note, status, created_at
           FROM contact_requests
          WHERE owner_account = $1 AND status = 'pending'
          ORDER BY created_at DESC",
    )
    .bind(owner_account)
    .fetch_all(pool)
    .await?;
    Ok(rows)
}

/// Settles a request addressed to one account. Accepting also writes the
/// standing grant between the two accounts, in one transaction, so an accepted
/// request can never leave contact unauthorised. Returns the requesting account.
pub async fn decide_request(
    pool: &PgPool,
    owner_account: Uuid,
    request_id: Uuid,
    accept: bool,
) -> Result<Option<Uuid>> {
    let mut tx = pool.begin().await?;
    let row: Option<(Uuid, Uuid, Option<Uuid>, Uuid)> = sqlx::query_as(
        "UPDATE contact_requests r
            SET status = CASE WHEN $3 THEN 'accepted' ELSE 'declined' END,
                decided_at = NOW()
           FROM accounts a
          WHERE r.id = $1 AND r.owner_account = $2 AND r.status = 'pending'
            AND a.id = r.requester_account
      RETURNING r.requester_account, r.owner_pial, r.number_node_id, a.pial_id",
    )
    .bind(request_id)
    .bind(owner_account)
    .bind(accept)
    .fetch_optional(&mut *tx)
    .await?;

    let Some((requester_account, owner_pial, number_node, requester_pial)) = row else {
        tx.rollback().await?;
        return Ok(None);
    };

    if accept {
        sqlx::query(
            "INSERT INTO contact_grants (owner_account, owner_pial, peer_account, source, number_node_id)
             VALUES ($1, $2, $3, 'request', $4)
             ON CONFLICT (owner_account, peer_account) DO UPDATE
                SET revoked_at = NULL, source = 'request'",
        )
        .bind(owner_account)
        .bind(owner_pial)
        .bind(requester_account)
        .bind(number_node)
        .execute(&mut *tx)
        .await?;

        crate::notifications::enqueue(
            &mut *tx,
            &crate::notifications::Event {
                event_id: format!("elohim:contact_accepted:{request_id}"),
                recipient_account: Some(requester_account.to_string()),
                recipient_pial: requester_pial.to_string(),
                actor_account: Some(owner_account.to_string()),
                kind: "contact_accepted".into(),
                target_id: owner_account.to_string(),
                target_type: "user".into(),
                payload: serde_json::json!({}),
                push_title: "Your contact request was accepted".into(),
                push_body: String::new(),
                push_url: "/messages".into(),
            },
        )
        .await?;
    }

    tx.commit().await?;
    Ok(Some(requester_account))
}

/// Registers one device's messaging public key for one account.
pub async fn upsert_messaging_key(
    pool: &PgPool,
    account: Uuid,
    pial: Uuid,
    device_id: &str,
    public_key_b64: &str,
) -> Result<()> {
    sqlx::query(
        "INSERT INTO pial_messaging_keys (account_id, pial_id, device_id, public_key_b64)
         VALUES ($1, $2, $3, $4)
         ON CONFLICT (account_id, device_id) DO UPDATE
            SET public_key_b64 = EXCLUDED.public_key_b64, updated_at = NOW()",
    )
    .bind(account)
    .bind(pial)
    .bind(device_id)
    .bind(public_key_b64)
    .execute(pool)
    .await?;
    Ok(())
}

/// One account's per-device messaging keys.
pub async fn messaging_keys(pool: &PgPool, account: Uuid) -> Result<Vec<DeviceKey>> {
    let rows: Vec<(String, String, String)> = sqlx::query_as(
        "SELECT device_id, public_key_b64, algorithm
           FROM pial_messaging_keys WHERE account_id = $1 ORDER BY device_id",
    )
    .bind(account)
    .fetch_all(pool)
    .await?;
    Ok(rows
        .into_iter()
        .map(|(device_id, public_key_b64, algorithm)| DeviceKey {
            device_id,
            public_key_b64,
            algorithm,
        })
        .collect())
}

/// Records which account minted a contact address.
pub async fn record_address(pool: &PgPool, address: &str, account: Uuid) -> Result<()> {
    sqlx::query(
        "INSERT INTO contact_address_accounts (address, account_id) VALUES ($1, $2)
         ON CONFLICT (address) DO NOTHING",
    )
    .bind(address)
    .bind(account)
    .execute(pool)
    .await?;
    Ok(())
}

/// The account a contact address belongs to. An address minted before this
/// brain recorded accounts is the identity's primary account's.
pub async fn address_account(pool: &PgPool, address: &str, pial: Uuid) -> Result<Option<Uuid>> {
    let row: Option<(Uuid,)> =
        sqlx::query_as("SELECT account_id FROM contact_address_accounts WHERE address = $1")
            .bind(address)
            .fetch_optional(pool)
            .await?;
    match row {
        Some((account,)) => Ok(Some(account)),
        None => primary_account(pool, pial).await,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn facts() -> ContactFacts {
        ContactFacts {
            via_number: false,
            follows: false,
            mutual: false,
            granted: false,
            capability_ok: false,
        }
    }

    #[test]
    fn closed_denies_even_with_the_number() {
        let mut f = facts();
        f.via_number = true;
        assert_eq!(decide("closed", &f).0, "deny");
    }

    #[test]
    fn a_number_alone_never_beats_capability_only() {
        let mut f = facts();
        f.via_number = true;
        assert_eq!(decide("capability_only", &f).0, "deny");
        f.capability_ok = true;
        assert_eq!(decide("capability_only", &f).0, "allow");
    }

    #[test]
    fn number_only_allows_the_number_path_and_asks_on_the_handle_path() {
        let mut f = facts();
        f.via_number = true;
        assert_eq!(decide("number_only", &f).0, "allow");
        f.via_number = false;
        assert_eq!(decide("number_only", &f).0, "request");
    }

    #[test]
    fn relationship_policies_read_the_follow_graph() {
        let mut f = facts();
        assert_eq!(decide("followers", &f).0, "request");
        f.follows = true;
        assert_eq!(decide("followers", &f).0, "allow");
        assert_eq!(decide("mutuals", &f).0, "request");
        f.mutual = true;
        assert_eq!(decide("mutuals", &f).0, "allow");
    }

    #[test]
    fn a_standing_grant_outranks_every_policy() {
        let mut f = facts();
        f.granted = true;
        for p in POLICIES {
            assert_eq!(decide(p, &f).0, "allow", "policy {p}");
        }
    }

    #[test]
    fn an_unknown_policy_denies_rather_than_defaulting_open() {
        assert_eq!(decide("whatever", &facts()).0, "deny");
    }

    #[test]
    fn capability_secrets_are_160_bits_of_crockford() {
        let s = mint_capability_secret();
        assert_eq!(s.len(), CAP_LEN);
        assert!(s.bytes().all(|b| CAP_ALPHABET.contains(&b)));
        assert_eq!(
            normalise_capability(&s.to_lowercase()).as_deref(),
            Some(s.as_str())
        );
        assert_eq!(normalise_capability("too-short"), None);
        assert_ne!(mint_capability_secret(), s);
    }

    #[test]
    fn the_stored_form_is_a_hash_and_not_the_secret() {
        let s = mint_capability_secret();
        let h = capability_hash(&s);
        assert_eq!(h.len(), 32);
        assert_ne!(h, s.as_bytes().to_vec());
    }

    #[test]
    fn an_unconfigured_signer_reports_itself_unsigned() {
        let signer = BundleSigner { key: None };
        assert!(!signer.configured());
        assert_eq!(signer.public_b64(), None);
        assert_eq!(signer.sign(b"payload"), None);
    }

    #[test]
    fn a_configured_signer_produces_a_verifiable_signature() {
        use ed25519_dalek::{Signature, Verifier};
        let seed = [7u8; 32];
        let key = SigningKey::from_bytes(&seed);
        let verifying = key.verifying_key();
        let signer = BundleSigner {
            key: Some(std::sync::Arc::new(key)),
        };
        let payload = b"{\"pial_id\":\"x\"}";
        let sig_b64 = signer.sign(payload).expect("signature");
        let raw = B64.decode(sig_b64).expect("base64");
        let sig = Signature::from_slice(&raw).expect("signature bytes");
        assert!(verifying.verify(payload, &sig).is_ok());
    }
}
