//! Public contact addresses.
//!
//! An address is a capability, not an identity. Possession of a live address grants the
//! right to fetch that identity's current device key bundle and open an E2E channel —
//! nothing more. Rotating one mints a new address node and revokes the old NAME; the
//! identity node is never touched, so an established conversation never moves.
//!
//! Alphabet: Crockford base32 — the digits and letters that survive being read aloud,
//! written down, and retyped. I, L, O and U are absent by construction.

use rand::rngs::OsRng;
use rand::RngCore;

/// Crockford base32. Exactly 32 symbols, so `byte % 32` is unbiased.
const ALPHABET: &[u8; 32] = b"0123456789ABCDEFGHJKMNPQRSTVWXYZ";

/// Symbols in an address, excluding the separator. 32^8 = 2^40 possible addresses.
const ADDRESS_LEN: usize = 8;

/// The namespace every address name is stored under.
pub const NAMESPACE: &str = "addr";

/// Mints a fresh address rendered as `XXXX-XXXX`. Cryptographically random: an address is
/// a bearer capability, so it must not be guessable from another one.
pub fn mint() -> String {
    let mut raw = [0u8; ADDRESS_LEN];
    OsRng.fill_bytes(&mut raw);

    let mut out = String::with_capacity(ADDRESS_LEN + 1);
    for (i, b) in raw.iter().enumerate() {
        if i == ADDRESS_LEN / 2 {
            out.push('-');
        }
        out.push(ALPHABET[(*b % 32) as usize] as char);
    }
    out
}

/// Normalises caller input to the canonical `XXXX-XXXX` form.
///
/// Accepts the bare address, the hyphen-less form, and the fully namespaced
/// `addr:XXXX-XXXX` form, in any case. Returns None if the input is not an address —
/// an unparseable address is rejected before it ever reaches the database.
pub fn normalise(input: &str) -> Option<String> {
    let trimmed = input.trim();
    let raw = trimmed.as_bytes();
    // Byte-wise so a multi-byte character can never land mid-slice.
    let body = if raw.len() > NAMESPACE.len()
        && raw[..NAMESPACE.len()].eq_ignore_ascii_case(NAMESPACE.as_bytes())
        && raw[NAMESPACE.len()] == b':'
    {
        &trimmed[NAMESPACE.len() + 1..]
    } else {
        trimmed
    };

    let symbols: Vec<u8> = body
        .bytes()
        .filter(|b| *b != b'-')
        .map(|b| b.to_ascii_uppercase())
        .collect();

    if symbols.len() != ADDRESS_LEN {
        return None;
    }
    if !symbols.iter().all(|b| ALPHABET.contains(b)) {
        return None;
    }

    let mut out = String::with_capacity(ADDRESS_LEN + 1);
    for (i, b) in symbols.iter().enumerate() {
        if i == ADDRESS_LEN / 2 {
            out.push('-');
        }
        out.push(*b as char);
    }
    Some(out)
}

/// The namespaced name an address is stored under.
pub fn to_name(address: &str) -> String {
    format!("{NAMESPACE}:{address}")
}

/// The bare address behind a stored name. A name that is not in the address namespace is
/// returned unchanged — the caller asked for what the database holds.
pub fn from_name(name: &str) -> String {
    let raw = name.as_bytes();
    if raw.len() > NAMESPACE.len()
        && raw[..NAMESPACE.len()].eq_ignore_ascii_case(NAMESPACE.as_bytes())
        && raw[NAMESPACE.len()] == b':'
    {
        name[NAMESPACE.len() + 1..].to_string()
    } else {
        name.to_string()
    }
}
