//! PIAL assertion tokens: short-lived, Ed25519-signed statements of who a
//! session is, verifiable by any brain from the public keys at /v1/auth/jwks
//! without a call back here. The design of the retired pial/ crate, finished.
//!
//! Token: compact JWS, header {"alg":"EdDSA","typ":"PIAL","kid":…}, claims
//!   iss "elohim-veni", sub pial_id, acc account_id, hnd handle, rol role,
//!   cap sorted capabilities, sid session id, iat, exp = iat + 300.
//!
//! ── Key custody (decision) ──────────────────────────────────────────────────
//! The private keys live in files under PIAL_SIGNING_KEY_DIR, a dedicated
//! volume, and NOT in f33d3r_security. A database is copied into backups,
//! replicas and debugging sessions as a matter of routine; a signing key that
//! rides along with every copy is a key every copy can forge identities with.
//!
//!   * file: <kid>.key, mode 0600, holding the base64 32-byte Ed25519 seed.
//!   * kid:  pial-<unix seconds created>-<8 hex>, so the kid carries its age.
//!   * first boot: an empty directory gets one key, generated under a
//!     Postgres advisory lock so concurrent replicas sharing the volume make
//!     exactly one.
//!   * rotation: `elohim-veni rotate-signing-key` writes a new key. Every key
//!     in the directory is published in the JWKS at once, but a key only starts
//!     SIGNING once it is PUBLISH_LEAD old, so every verifier refreshing its
//!     JWKS on the advertised max-age has it before the first token it signs.
//!     (On a directory with no key that old — first boot — the newest key signs
//!     at once: no verifier holds a JWKS yet.) Running processes rescan the
//!     directory every RELOAD_EVERY.
//!   * retirement: delete the old file once its successor has been signing for
//!     longer than TOKEN_TTL; nothing it signed is still valid.

use std::fs;
use std::io::Write;
use std::os::unix::fs::OpenOptionsExt;
use std::path::{Path, PathBuf};
use std::sync::{Arc, RwLock};
use std::time::Duration;

use anyhow::{anyhow, bail, Context, Result};
use base64::engine::general_purpose::{STANDARD as B64, URL_SAFE_NO_PAD as B64URL};
use base64::Engine;
use ed25519_dalek::{Signer, SigningKey};
use rand::rngs::OsRng;
use rand::RngCore;
use serde::Serialize;
use serde_json::{json, Value};
use sqlx::PgPool;

pub const ISSUER: &str = "elohim-veni";
pub const TOKEN_TTL_SECS: i64 = 300;
/// How long a new key is published before it signs. Verifiers are told to
/// cache the JWKS for at most JWKS_MAX_AGE, which is half of this.
pub const PUBLISH_LEAD_SECS: i64 = 600;
pub const JWKS_MAX_AGE_SECS: u64 = 300;
const RELOAD_EVERY: Duration = Duration::from_secs(60);
const KEYGEN_LOCK: i64 = 7331_0012;

#[derive(Debug, Serialize)]
pub struct AccessClaims {
    pub iss: &'static str,
    pub sub: String,
    pub acc: String,
    pub hnd: String,
    pub rol: String,
    pub cap: Vec<String>,
    pub sid: String,
    pub iat: i64,
    pub exp: i64,
}

struct KeyEntry {
    kid: String,
    created: i64,
    key: SigningKey,
}

/// Holds the signing keys and signs tokens with the active one.
#[derive(Clone)]
pub struct TokenIssuer {
    dir: PathBuf,
    ring: Arc<RwLock<Vec<KeyEntry>>>,
}

fn kid_created(kid: &str) -> Result<i64> {
    let mut parts = kid.splitn(3, '-');
    match (parts.next(), parts.next(), parts.next()) {
        (Some("pial"), Some(secs), Some(tag)) if tag.len() == 8 && tag.bytes().all(|b| b.is_ascii_hexdigit()) => {
            secs.parse::<i64>().with_context(|| format!("key id {kid}: bad timestamp"))
        }
        _ => bail!("key id {kid} is not pial-<unix seconds>-<8 hex>"),
    }
}

/// Reads every key in the directory. Anything in it that is not a key file
/// (dotfiles aside, which is where an interrupted write leaves its temp) is an
/// error: a key directory with unexplained contents is not one to sign from.
fn load_dir(dir: &Path) -> Result<Vec<KeyEntry>> {
    let mut keys = Vec::new();
    for entry in fs::read_dir(dir).with_context(|| format!("reading key dir {}", dir.display()))? {
        let entry = entry?;
        let name = entry.file_name().to_string_lossy().into_owned();
        if name.starts_with('.') {
            continue;
        }
        let Some(kid) = name.strip_suffix(".key") else {
            bail!("unexpected file {name} in key dir {}", dir.display());
        };
        let created = kid_created(kid)?;
        let raw = fs::read_to_string(entry.path()).with_context(|| format!("reading key {name}"))?;
        let seed = B64
            .decode(raw.trim())
            .with_context(|| format!("key {name} is not base64"))?;
        let seed: [u8; 32] = seed
            .try_into()
            .map_err(|_| anyhow!("key {name} is not a 32-byte Ed25519 seed"))?;
        keys.push(KeyEntry {
            kid: kid.to_string(),
            created,
            key: SigningKey::from_bytes(&seed),
        });
    }
    keys.sort_by(|a, b| (a.created, &a.kid).cmp(&(b.created, &b.kid)));
    Ok(keys)
}

/// Writes a new key atomically: temp file (0600) → fsync → rename → fsync dir.
fn write_new_key(dir: &Path) -> Result<String> {
    let mut seed = [0u8; 32];
    OsRng.fill_bytes(&mut seed);
    let mut tag = [0u8; 4];
    OsRng.fill_bytes(&mut tag);
    let kid = format!("pial-{}-{}", chrono::Utc::now().timestamp(), hex::encode(tag));
    let tmp = dir.join(format!(".{kid}.tmp"));
    let path = dir.join(format!("{kid}.key"));
    {
        let mut f = fs::OpenOptions::new()
            .write(true)
            .create_new(true)
            .mode(0o600)
            .open(&tmp)
            .with_context(|| format!("creating {}", tmp.display()))?;
        f.write_all(format!("{}\n", B64.encode(seed)).as_bytes())?;
        f.sync_all()?;
    }
    fs::rename(&tmp, &path).with_context(|| format!("installing {}", path.display()))?;
    fs::File::open(dir)?.sync_all()?;
    Ok(kid)
}

/// Runs `f` while holding the key-generation lock, so replicas sharing one key
/// volume never generate concurrently.
async fn with_keygen_lock<T>(pool: &PgPool, f: impl FnOnce() -> Result<T>) -> Result<T> {
    let mut conn = pool.acquire().await?;
    sqlx::query("SELECT pg_advisory_lock($1)")
        .bind(KEYGEN_LOCK)
        .execute(&mut *conn)
        .await?;
    let out = f();
    sqlx::query("SELECT pg_advisory_unlock($1)")
        .bind(KEYGEN_LOCK)
        .execute(&mut *conn)
        .await?;
    out
}

fn ensure_dir(dir: &Path) -> Result<()> {
    fs::DirBuilder::new()
        .recursive(true)
        .mode(0o700)
        .create(dir)
        .with_context(|| format!("creating key dir {}", dir.display()))
}

use std::os::unix::fs::DirBuilderExt;

/// Generates a new signing key (rotation). It starts signing PUBLISH_LEAD
/// after it is written; until then it is only published.
pub async fn rotate(pool: &PgPool, dir: &Path) -> Result<String> {
    ensure_dir(dir)?;
    with_keygen_lock(pool, || write_new_key(dir)).await
}

impl TokenIssuer {
    /// Loads the key directory, generating the first key when it is empty.
    pub async fn open(pool: &PgPool, dir: PathBuf) -> Result<Self> {
        ensure_dir(&dir)?;
        let keys = with_keygen_lock(pool, || {
            let keys = load_dir(&dir)?;
            if !keys.is_empty() {
                return Ok(keys);
            }
            let kid = write_new_key(&dir)?;
            tracing::info!(kid = %kid, "generated the first PIAL signing key");
            load_dir(&dir)
        })
        .await?;
        Ok(Self {
            dir,
            ring: Arc::new(RwLock::new(keys)),
        })
    }

    /// Rescans the directory periodically, so a rotation or a retirement lands
    /// without a restart. A failed rescan keeps the keys already loaded — they
    /// are still valid — and says so on every attempt.
    pub fn spawn_reload(&self) {
        let me = self.clone();
        tokio::spawn(async move {
            loop {
                tokio::time::sleep(RELOAD_EVERY).await;
                match load_dir(&me.dir) {
                    Ok(keys) if keys.is_empty() => {
                        tracing::error!(dir = %me.dir.display(), "PIAL key dir is now empty; keeping the loaded keys");
                        metrics::counter!("pial_key_reload_failures_total").increment(1);
                    }
                    Ok(keys) => match me.ring.write() {
                        Ok(mut ring) => *ring = keys,
                        Err(e) => tracing::error!(error = %e, "PIAL key ring lock poisoned"),
                    },
                    Err(e) => {
                        tracing::error!(error = ?e, "PIAL key reload failed; keeping the loaded keys");
                        metrics::counter!("pial_key_reload_failures_total").increment(1);
                    }
                }
            }
        });
    }

    /// Signs a token for these claims with the active key.
    pub fn issue(&self, claims: &AccessClaims) -> Result<String> {
        let ring = self.ring.read().map_err(|e| anyhow!("key ring lock poisoned: {e}"))?;
        let now = chrono::Utc::now().timestamp();
        let active = ring
            .iter()
            .rev()
            .find(|k| k.created <= now - PUBLISH_LEAD_SECS)
            .or_else(|| ring.last())
            .ok_or_else(|| anyhow!("no PIAL signing key loaded"))?;
        let header = json!({ "alg": "EdDSA", "typ": "PIAL", "kid": active.kid });
        let signing_input = format!(
            "{}.{}",
            B64URL.encode(serde_json::to_vec(&header)?),
            B64URL.encode(serde_json::to_vec(claims)?)
        );
        let sig = active.key.sign(signing_input.as_bytes());
        Ok(format!("{signing_input}.{}", B64URL.encode(sig.to_bytes())))
    }

    /// Every loaded key as an RFC 8037 OKP JWK.
    pub fn jwks(&self) -> Result<Value> {
        let ring = self.ring.read().map_err(|e| anyhow!("key ring lock poisoned: {e}"))?;
        let keys: Vec<Value> = ring
            .iter()
            .map(|k| {
                json!({
                    "kty": "OKP",
                    "crv": "Ed25519",
                    "x": B64URL.encode(k.key.verifying_key().to_bytes()),
                    "kid": k.kid,
                    "alg": "EdDSA",
                    "use": "sig",
                })
            })
            .collect();
        Ok(json!({ "keys": keys }))
    }
}

/// Claims for a session acting as `account` on `pial`.
pub fn claims_for(
    pial: &super::view::PialView,
    account: &super::view::AccountRow,
    session_id: uuid::Uuid,
) -> AccessClaims {
    let now = chrono::Utc::now();
    AccessClaims {
        iss: ISSUER,
        sub: pial.pial_id.to_string(),
        acc: account.account_id.to_string(),
        hnd: account.handle.clone(),
        rol: pial.role.clone(),
        cap: pial.granted(now),
        sid: session_id.to_string(),
        iat: now.timestamp(),
        exp: now.timestamp() + TOKEN_TTL_SECS,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn kids_carry_their_age() {
        assert_eq!(kid_created("pial-1700000000-0a1b2c3d").unwrap(), 1_700_000_000);
        assert!(kid_created("pial-1700000000-xyz").is_err());
        assert!(kid_created("other-1-0a1b2c3d").is_err());
    }
}
