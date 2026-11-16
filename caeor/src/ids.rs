//! Asset short ids and the handle text that goes into a watermark.

use uuid::Uuid;

/// Crockford base32, lowercase: no i, l, o or u, so a short id read off a
/// screen recording cannot be mistaken for another one.
const ALPHABET: &[u8; 32] = b"0123456789abcdefghjkmnpqrstvwxyz";

/// Eight characters, 40 random bits. Uniqueness is the database's to enforce
/// (assets.short_id is UNIQUE); a collision is retried by the caller.
pub fn new_short_id() -> String {
    // A v4 UUID is 122 bits from the OS CSPRNG; the first five bytes are random.
    let bytes = Uuid::new_v4().into_bytes();
    let mut v: u64 = 0;
    for b in &bytes[..5] {
        v = (v << 8) | u64::from(*b);
    }
    let mut out = [0u8; 8];
    for i in (0..8).rev() {
        out[i] = ALPHABET[(v & 31) as usize];
        v >>= 5;
    }
    // ALPHABET is ASCII, so this cannot fail.
    String::from_utf8(out.to_vec()).expect("short id alphabet is ASCII")
}

/// A handle as it may appear in a watermark: the characters a handle may
/// contain and nothing that could be read as filter syntax. A leading '@' is
/// dropped; the watermark adds its own.
pub fn mark_handle(handle: &str) -> String {
    handle
        .trim()
        .trim_start_matches('@')
        .chars()
        .filter(|c| c.is_ascii_alphanumeric() || *c == '_' || *c == '.' || *c == '-')
        .take(64)
        .collect()
}

/// The visible watermark every viewer-facing image and video copy carries.
/// An asset with no handle on record (imported from before handles were
/// kept) still carries the site and its short id.
pub fn mark_text(handle: &str, short_id: &str) -> String {
    if handle.is_empty() {
        format!("f33d3r.com \u{2022} {short_id}")
    } else {
        format!("@{handle} \u{2022} f33d3r.com \u{2022} {short_id}")
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn short_ids_are_eight_crockford_chars() {
        for _ in 0..1000 {
            let s = new_short_id();
            assert_eq!(s.len(), 8);
            assert!(s.bytes().all(|b| ALPHABET.contains(&b)));
        }
    }

    #[test]
    fn handles_are_filter_safe() {
        assert_eq!(mark_handle("@al'ice:x"), "alicex");
    }
}
