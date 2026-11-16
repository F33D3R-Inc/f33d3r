//! RFC 6238 TOTP (SHA-1, 6 digits, 30-second steps) — the parameters every
//! authenticator app enrolled through feed-engine already uses
//! (feed-engine/internal/handler/totp.go), so existing enrolments keep working.
//!
//! The HMAC-SHA1 is computed by pgcrypto's hmac(), which this database already
//! carries for bcrypt, rather than by a hash crate this brain does not depend
//! on. Dynamic truncation and the comparison are here.

use anyhow::{bail, Result};
use rand::rngs::OsRng;
use rand::RngCore;
use sqlx::PgConnection;
use subtle::ConstantTimeEq;

const ALPHABET: &[u8; 32] = b"ABCDEFGHIJKLMNOPQRSTUVWXYZ234567";
pub const STEP_SECS: i64 = 30;

/// RFC 4648 base32, unpadded.
pub fn base32_encode(data: &[u8]) -> String {
    let mut out = String::with_capacity(data.len().div_ceil(5) * 8);
    let (mut buf, mut bits) = (0u32, 0u32);
    for &b in data {
        buf = (buf << 8) | b as u32;
        bits += 8;
        while bits >= 5 {
            bits -= 5;
            out.push(ALPHABET[((buf >> bits) & 31) as usize] as char);
        }
    }
    if bits > 0 {
        out.push(ALPHABET[((buf << (5 - bits)) & 31) as usize] as char);
    }
    out
}

/// RFC 4648 base32, case-insensitive, padding and spaces tolerated.
pub fn base32_decode(s: &str) -> Result<Vec<u8>> {
    let mut out = Vec::with_capacity(s.len() * 5 / 8);
    let (mut buf, mut bits) = (0u32, 0u32);
    for c in s.trim().bytes().filter(|c| *c != b'=' && *c != b' ') {
        let c = c.to_ascii_uppercase();
        let Some(v) = ALPHABET.iter().position(|a| *a == c) else {
            bail!("not base32");
        };
        buf = (buf << 5) | v as u32;
        bits += 5;
        if bits >= 8 {
            bits -= 8;
            out.push((buf >> bits) as u8);
        }
        buf &= (1 << bits) - 1;
    }
    Ok(out)
}

/// A fresh 160-bit secret, base32 — the size totpGenSecret mints.
pub fn generate_secret() -> String {
    let mut b = [0u8; 20];
    OsRng.fill_bytes(&mut b);
    base32_encode(&b)
}

/// otpauth URI, the same string feed-engine's totpProvisioningURI produced.
pub fn provisioning_uri(secret: &str, handle: &str) -> String {
    format!(
        "otpauth://totp/F33D3R:@{handle}?secret={secret}&issuer=F33D3R&algorithm=SHA1&digits=6&period=30"
    )
}

/// The current 30-second step.
pub fn current_step() -> i64 {
    chrono::Utc::now().timestamp().div_euclid(STEP_SECS)
}

/// Normalises what a person typed: spaces removed. None unless six digits.
pub fn normalize_code(code: &str) -> Option<String> {
    let c: String = code.chars().filter(|c| !c.is_whitespace()).collect();
    (c.len() == 6 && c.bytes().all(|b| b.is_ascii_digit())).then_some(c)
}

/// The step at which `code` is valid for `secret`, checking the current step
/// and one either side (clock drift, as totpVerify allowed), restricted to
/// steps after `after_step` so an accepted code cannot be replayed.
pub async fn matching_step(
    conn: &mut PgConnection,
    secret: &str,
    code: &str,
    after_step: i64,
) -> Result<Option<i64>> {
    let Some(code) = normalize_code(code) else {
        return Ok(None);
    };
    let key = base32_decode(secret)?;
    let now = current_step();
    let steps = [now - 1, now, now + 1];
    let macs: (Vec<u8>, Vec<u8>, Vec<u8>) = sqlx::query_as(
        "SELECT hmac($1, $4, 'sha1'), hmac($2, $4, 'sha1'), hmac($3, $4, 'sha1')",
    )
    .bind(steps[0].to_be_bytes().to_vec())
    .bind(steps[1].to_be_bytes().to_vec())
    .bind(steps[2].to_be_bytes().to_vec())
    .bind(key)
    .fetch_one(conn)
    .await?;
    let mut found = None;
    for (step, mac) in steps.iter().zip([macs.0, macs.1, macs.2]) {
        let candidate = truncate(&mac)?;
        // Every candidate is compared, in constant time, so the answer's
        // timing does not say which step matched.
        let hit = bool::from(candidate.as_bytes().ct_eq(code.as_bytes()));
        if hit && *step > after_step && found.is_none() {
            found = Some(*step);
        }
    }
    Ok(found)
}

/// RFC 4226 dynamic truncation to six digits.
fn truncate(mac: &[u8]) -> Result<String> {
    if mac.len() != 20 {
        bail!("HMAC-SHA1 returned {} bytes", mac.len());
    }
    let offset = (mac[19] & 0x0f) as usize;
    let bin = u32::from_be_bytes([mac[offset], mac[offset + 1], mac[offset + 2], mac[offset + 3]])
        & 0x7fff_ffff;
    Ok(format!("{:06}", bin % 1_000_000))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn base32_round_trips() {
        let data = b"12345678901234567890";
        let enc = base32_encode(data);
        assert_eq!(enc, "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ");
        assert_eq!(base32_decode(&enc).unwrap(), data);
        assert_eq!(base32_decode(&enc.to_lowercase()).unwrap(), data);
    }

    #[test]
    fn truncation_matches_rfc4226() {
        // RFC 4226 appendix D, count 0 HMAC.
        let mac = hex::decode("cc93cf18508d94934c64b65d8ba7667fb7cde4b0").unwrap();
        assert_eq!(truncate(&mac).unwrap(), "755224");
    }
}
