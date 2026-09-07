//! F33D3R Numbers — speakable, rotatable contact names that resolve to an identity.
//!
//! A Number is TWELVE DECIMAL DIGITS, rendered `0412-8837-2919` and spoken
//! `0412 8837 2919`. Eleven of those digits are payload drawn from the system
//! random source; the twelfth is a Luhn check digit over the other eleven.
//!
//! It is a policy address, not a bearer secret: resolving one yields an identity
//! node and nothing else. The bearer half of contact lives in elohim-veni's
//! 160-bit contact capabilities.
//!
//! # Why digits
//!
//! The previous shape was twelve Crockford base32 symbols plus a mod-37 check
//! symbol, `H8K2-9QRT-4VMXC`. Nobody memorises that, nobody reads it aloud
//! without spelling it, and no keypad exists for it. A Number that cannot be
//! said across a table is a Number that does not get used. Digits are what a
//! person already knows how to say, write down and type on a numeric keypad.
//!
//! # Why the check digit is one of the twelve
//!
//! Twelve is what a person reads aloud, so twelve is what is displayed — the
//! same convention as a payment card, where the stated length already includes
//! the check digit. That leaves ELEVEN payload digits and an address space of
//! 10^11 ≈ 2^36.54.
//!
//! State that consequence plainly rather than burying it: the base32 shape
//! carried 2^60, so this is about 11.5 million times smaller. The check digit,
//! the separate rate-limit buckets above it and the per-Number admission budget
//! are therefore no longer defence in depth. They ARE the defence.
//!
//! # What the check digit catches, and what it does not
//!
//! Luhn catches every single-digit substitution, in every position, always. It
//! catches every adjacent transposition EXCEPT `09` ↔ `90`, which it is blind to
//! because doubling 0 and doubling 9 differ by exactly 9 and Luhn's casting-out
//! step erases that. Both claims are proved over real minted Numbers by
//! `luhn_catches_every_single_digit_typo` and
//! `luhns_only_transposition_blind_spot_is_zero_nine` below, and the blind spot
//! is measured rather than asserted, so it cannot quietly widen.
//!
//! Damm's order-10 quasigroup would close that one gap and is otherwise
//! identical in cost. It was not chosen: Luhn is the check every long spoken
//! number in the world already uses, and the format is a one-way door once
//! Numbers are minted under it. The gap left open is a single adjacent digit
//! pair, and a Number that survives it resolves to nobody.
//!
//! # No geographic prefix
//!
//! Nothing in a Number encodes where its owner is, what they are, or when it was
//! minted. A prefix that carried location would leak it permanently — a Number
//! rotates, but the fact that its owner was in one place when they minted it
//! does not. Structure exists here for memorability and for nothing else.
//!
//! # The check digit runs before any I/O
//!
//! `normalise` is pure arithmetic over the caller's own bytes. Nine of every ten
//! blind guesses die inside it, before a database connection is touched. That
//! property is load-bearing and every caller of this module depends on it.
//!
//! # Numbers minted before this format existed
//!
//! They keep working, for ever, unchanged. A Number somebody has already shared
//! is a promise, and retiring one to force a remint would break every card,
//! badge and slide it is printed on. So `normalise` accepts BOTH shapes and the
//! two are disjoint by length: a legacy Number is thirteen symbols, a current
//! one is twelve digits. Nothing is reminted, nothing is migrated, no row
//! changes, and no live Number stops working. Only `mint` is one-way: every new
//! Number is digits, and `is_legacy` exists so an owner can be shown which of
//! theirs is the older shape and choose to move.

use rand::rngs::OsRng;
use rand::RngCore;

/// The namespace every Number is stored under.
pub const NAMESPACE: &str = "number";

/// What a person reads aloud: eleven payload digits and one check digit.
const TOTAL_LEN: usize = 12;

/// The digits the check digit is computed over.
const PAYLOAD_LEN: usize = TOTAL_LEN - 1;

/// 10^11 — every value eleven payload digits can take.
const PAYLOAD_SPACE: u64 = 100_000_000_000;

/// The largest multiple of `PAYLOAD_SPACE` that fits in a `u64`. A draw at or
/// above this is discarded rather than reduced, because `u64 % 10^11` over the
/// whole range would favour low payloads — a bias small enough to miss and large
/// enough to shrink the space an attacker has to search.
const DRAW_LIMIT: u64 = (u64::MAX / PAYLOAD_SPACE) * PAYLOAD_SPACE;

/// Draws before minting gives up. A draw is discarded only when it lands in the
/// bias-rejection tail, which is about four in a billion, so this bound is never
/// approached in practice — it exists so a failing system random source reports
/// a failure instead of looping for ever.
///
/// This is NOT the old `MINT_DRAWS`. That existed because five of thirty-seven
/// Crockford check symbols fall outside the payload alphabet, so a draw could
/// produce an unspeakable Number and had to be thrown away. Every base-10 check
/// digit is a digit, so that retry has no reason to exist and does not.
const DRAW_ATTEMPTS: usize = 32;

/// The longest input `normalise` will look at. A Number is twelve digits;
/// separators and the namespace prefix cannot plausibly quintuple that.
/// Anything longer is refused without being scanned.
const INPUT_MAX: usize = 64;

// ── The check digit ───────────────────────────────────────────────────────────

/// The Luhn sum of a run of ASCII digits, read right to left, doubling every
/// second one and casting out nines from the doubled value.
///
/// ONE function serves both directions. Minting appends a provisional zero and
/// asks what would make this sum a multiple of ten; validating asks whether it
/// already is. Luhn's doubling depends on a digit's distance from the RIGHT-hand
/// end, which is the classic way a hand-written mirror of a format drifts when
/// the length changes — so the position is derived here from `.rev()` and never
/// from the length, and there is exactly one place it could be wrong.
///
/// Returns `None` if any byte is not an ASCII digit.
fn luhn_sum(digits: &[u8]) -> Option<u32> {
    let mut sum = 0u32;
    for (from_right, b) in digits.iter().rev().enumerate() {
        if !b.is_ascii_digit() {
            return None;
        }
        let mut v = u32::from(*b - b'0');
        if from_right % 2 == 1 {
            v *= 2;
            if v > 9 {
                v -= 9;
            }
        }
        sum += v;
    }
    Some(sum)
}

/// The check digit that completes a payload, as an ASCII byte.
fn luhn_check_digit(payload: &[u8]) -> Option<u8> {
    // The check digit will sit at the right-hand end, so the payload's own
    // doubling positions are the ones it has with a digit already after it. A
    // provisional zero puts them there and contributes nothing to the sum.
    let mut with_slot = [0u8; TOTAL_LEN];
    with_slot[..payload.len()].copy_from_slice(payload);
    with_slot[payload.len()] = b'0';
    let sum = luhn_sum(&with_slot[..=payload.len()])?;
    Some(b'0' + ((10 - (sum % 10)) % 10) as u8)
}

/// Whether a complete Number satisfies its own check digit.
fn luhn_ok(digits: &[u8]) -> bool {
    matches!(luhn_sum(digits), Some(sum) if sum % 10 == 0)
}

// ── Legacy: the Crockford base32 shape ────────────────────────────────────────

/// Crockford base32. Exactly 32 symbols. Retained to resolve Numbers minted
/// before the digit format; never used to mint.
const LEGACY_ALPHABET: &[u8; 32] = b"0123456789ABCDEFGHJKMNPQRSTVWXYZ";

/// Crockford's check alphabet: the 32 payload symbols plus five check-only
/// symbols, giving the prime modulus 37.
const LEGACY_CHECK_ALPHABET: &[u8; 37] = b"0123456789ABCDEFGHJKMNPQRSTVWXYZ*~$=U";

/// Twelve payload symbols and one check symbol.
const LEGACY_PAYLOAD_LEN: usize = 12;
const LEGACY_TOTAL_LEN: usize = LEGACY_PAYLOAD_LEN + 1;

/// The index of a legacy payload symbol, or None if the byte is not one.
fn legacy_symbol_value(b: u8) -> Option<u8> {
    LEGACY_ALPHABET
        .iter()
        .position(|s| *s == b)
        .map(|i| i as u8)
}

/// The check value of a legacy payload, mod 37. Iterative so no wide integer is
/// needed.
fn legacy_check_value(payload: &[u8]) -> Option<u8> {
    let mut acc: u32 = 0;
    for b in payload {
        let v = u32::from(legacy_symbol_value(*b)?);
        acc = (acc * 32 + v) % 37;
    }
    Some(acc as u8)
}

// ── Shape ─────────────────────────────────────────────────────────────────────

/// Crockford's confusable folding: I and L read as 1, O reads as 0.
///
/// It earns its place in a digits-only format for the same reason it earned it
/// in base32 and for one more. Somebody copying a Number off a badge or writing
/// down what they heard writes the letters their handwriting produces, and a
/// twelve-digit Number containing an `O` is a `0` a person drew badly — not a
/// different Number and not an attack. Folding it costs one comparison and no
/// I/O, and the result is still digits.
fn fold(b: u8) -> u8 {
    match b.to_ascii_uppercase() {
        b'I' | b'L' => b'1',
        b'O' => b'0',
        other => other,
    }
}

/// Separators a person might type between groups. Spaces and hyphens are what
/// the Number is printed with; underscores and full stops are what people
/// actually type. None of them carry meaning, so all of them are removed.
///
/// A leading `+` is deliberately NOT a separator. It is what a pasted telephone
/// number begins with, and stripping it would turn one into a well-formed
/// F33D3R Number once in every ten attempts — so the person would be told their
/// contact was unreachable instead of that they pasted the wrong thing.
fn is_separator(b: u8) -> bool {
    matches!(b, b'-' | b' ' | b'_' | b'.')
}

/// Groups symbols as `XXXX-XXXX-…`: 4-4-4 for a Number, 4-4-5 for a legacy one.
fn canonical(symbols: &[u8]) -> String {
    let mut out = String::with_capacity(symbols.len() + 2);
    for (i, b) in symbols.iter().enumerate() {
        if i == 4 || i == 8 {
            out.push('-');
        }
        out.push(*b as char);
    }
    out
}

/// Mints a fresh Number: eleven payload digits from the system random source and
/// one Luhn check digit.
///
/// The payload is drawn as a single uniform value over the whole 10^11 space
/// rather than digit by digit, so there is exactly one place bias could enter
/// and `DRAW_LIMIT` closes it. Nothing about the owner — no identity, no handle,
/// no timestamp, no counter, no location — reaches this function, so no Number
/// can be derived from another or from who asked for it.
pub fn mint() -> Option<String> {
    for _ in 0..DRAW_ATTEMPTS {
        let mut raw = [0u8; 8];
        OsRng.fill_bytes(&mut raw);
        let drawn = u64::from_le_bytes(raw);
        if drawn >= DRAW_LIMIT {
            // In the tail that `% PAYLOAD_SPACE` would map unevenly. Discarded,
            // never folded, so every payload is exactly as likely as every other.
            continue;
        }

        let mut symbols = [0u8; TOTAL_LEN];
        let mut payload = drawn % PAYLOAD_SPACE;
        for slot in symbols[..PAYLOAD_LEN].iter_mut().rev() {
            *slot = b'0' + (payload % 10) as u8;
            payload /= 10;
        }
        symbols[PAYLOAD_LEN] = luhn_check_digit(&symbols[..PAYLOAD_LEN])?;
        return Some(canonical(&symbols));
    }
    None
}

/// Normalises caller input to the canonical form, rejecting anything whose check
/// digit does not match.
///
/// Accepts spaces, hyphens, underscores, full stops, any case, the confusable
/// letters, and the fully namespaced `number:` form. Accepts BOTH the current
/// twelve-digit shape and the legacy thirteen-symbol Crockford shape; the two
/// are told apart by length, which cannot collide.
///
/// Every branch is arithmetic over the caller's own bytes. No branch touches the
/// database, the network, or any shared state, so a malformed Number costs a
/// guesser exactly one function call.
pub fn normalise(input: &str) -> Option<String> {
    let trimmed = input.trim();
    if trimmed.len() > INPUT_MAX {
        return None;
    }
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
        .filter(|b| !is_separator(*b))
        .map(fold)
        .collect();

    match symbols.len() {
        TOTAL_LEN => {
            if !luhn_ok(&symbols) {
                return None;
            }
            Some(canonical(&symbols))
        }
        LEGACY_TOTAL_LEN => {
            if !symbols.iter().all(|b| LEGACY_ALPHABET.contains(b)) {
                return None;
            }
            let expected = legacy_check_value(&symbols[..LEGACY_PAYLOAD_LEN])?;
            if LEGACY_CHECK_ALPHABET[expected as usize] != symbols[LEGACY_PAYLOAD_LEN] {
                return None;
            }
            Some(canonical(&symbols))
        }
        _ => None,
    }
}

/// Whether a canonical Number is one of the base32 Numbers minted before the
/// digit format.
///
/// Owner-facing only. It exists so somebody holding an older Number can be shown
/// that it is older and choose to mint a new one — never so a resolver can tell
/// the two apart, which it has no endpoint to ask and no reason to know.
pub fn is_legacy(number: &str) -> bool {
    number.bytes().filter(|b| !is_separator(*b)).count() == LEGACY_TOTAL_LEN
}

/// The namespaced name a Number is stored under.
pub fn to_name(number: &str) -> String {
    format!("{NAMESPACE}:{number}")
}

/// The bare Number behind a stored name.
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

// ── The conformance corpus ────────────────────────────────────────────────────
//
// This format has two implementations: this module and its hand-written mirror
// in feed-engine (`canonicalNumber`). Two implementations of one format is a
// standing drift risk, and the only honest fix is to make one authoritative and
// give the other something mechanical to fail against.
//
// This module is authoritative. It GENERATES the corpus below; the Go mirror
// only ever READS it. Every case is derived from this module's own arithmetic
// and its own answers, so the file cannot say something this module does not do.
// If the mirror drifts by one digit, one separator, one folded letter or one
// check-digit rule, its own test suite fails against this file.
//
// The file is checked in and regenerated deliberately, never on the fly:
//   UPDATE_NUMBER_CONFORMANCE=1 cargo test -p manhattan conformance

/// Where the corpus lives, relative to this crate.
#[cfg(test)]
const CONFORMANCE_PATH: &str = concat!(env!("CARGO_MANIFEST_DIR"), "/number.conformance");

/// Deterministic payloads for the corpus. Not random: the corpus must be
/// byte-identical every time it is generated, or "the file changed" would stop
/// meaning "the format changed". The step is coprime to 10^11, so the sequence
/// walks the payload space rather than clustering in one corner of it.
#[cfg(test)]
const CORPUS_STEP: u64 = 7_919_243_311;

/// The Number a deterministic payload produces. Used only to build the corpus.
#[cfg(test)]
fn number_for_payload(payload: u64) -> String {
    let mut symbols = [0u8; TOTAL_LEN];
    let mut n = payload % PAYLOAD_SPACE;
    for slot in symbols[..PAYLOAD_LEN].iter_mut().rev() {
        *slot = b'0' + (n % 10) as u8;
        n /= 10;
    }
    symbols[PAYLOAD_LEN] = luhn_check_digit(&symbols[..PAYLOAD_LEN]).expect("digits");
    canonical(&symbols)
}

/// Builds the corpus text from this module's own behaviour.
#[cfg(test)]
fn conformance_corpus() -> String {
    let mut out = String::new();
    out.push_str("# F33D3R Number conformance corpus.\n");
    out.push_str("# Generated by manhattan/src/number.rs, which is authoritative for this\n");
    out.push_str("# format. Read by the Go mirror's test suite so the two cannot drift.\n");
    out.push_str("# Regenerate: UPDATE_NUMBER_CONFORMANCE=1 cargo test -p manhattan conformance\n");
    out.push_str("# OK<TAB>input<TAB>canonical   —  must normalise to canonical\n");
    out.push_str("# NO<TAB>input                 —  must be refused\n");

    fn ok(out: &mut String, input: &str, canonical: &str) {
        out.push_str("OK\t");
        out.push_str(input);
        out.push('\t');
        out.push_str(canonical);
        out.push('\n');
    }
    fn no(out: &mut String, input: &str) {
        out.push_str("NO\t");
        out.push_str(input);
        out.push('\n');
    }
    // Every generated case is routed through `normalise` itself, so a verdict in
    // the file is this module's answer and never a second opinion about it.
    fn record(out: &mut String, input: &str) {
        match normalise(input) {
            Some(c) => ok(out, input, &c),
            None => no(out, input),
        }
    }

    out.push_str("\n# Current format: twelve digits, every accepted input form.\n");
    let mut payload = 0u64;
    let mut numbers: Vec<String> = Vec::new();
    for _ in 0..64 {
        payload = (payload + CORPUS_STEP) % PAYLOAD_SPACE;
        numbers.push(number_for_payload(payload));
    }
    for n in &numbers {
        let bare: String = n.chars().filter(|ch| *ch != '-').collect();
        // Confusables, as somebody writing down what they heard produces them.
        let confused: String = bare
            .chars()
            .map(|ch| match ch {
                '0' => 'O',
                '1' => 'i',
                other => other,
            })
            .collect();
        for form in [
            n.clone(),
            bare.clone(),
            format!("{} {} {}", &bare[0..4], &bare[4..8], &bare[8..12]),
            format!("{}.{}.{}", &bare[0..4], &bare[4..8], &bare[8..12]),
            format!("{}_{}", &bare[0..6], &bare[6..12]),
            format!("number:{n}"),
            format!("  {n}  "),
            confused,
        ] {
            record(&mut out, &form);
        }
    }

    out.push_str("\n# Every single-digit substitution of one Number. Luhn refuses all of them.\n");
    let subject: Vec<u8> = numbers[0].bytes().filter(|b| *b != b'-').collect();
    for i in 0..TOTAL_LEN {
        for d in b'0'..=b'9' {
            if d == subject[i] {
                continue;
            }
            let mut broken = subject.clone();
            broken[i] = d;
            record(&mut out, std::str::from_utf8(&broken).unwrap());
        }
    }

    out.push_str(
        "\n# Every adjacent transposition, over Numbers chosen to include an 09/90 pair.\n",
    );
    out.push_str("# Luhn's one blind spot is that pair, so some of these are ACCEPTED — and the\n");
    out.push_str(
        "# mirror must agree about exactly which, or the two disagree about the format.\n",
    );
    for n in numbers.iter().take(24) {
        let bare: Vec<u8> = n.bytes().filter(|b| *b != b'-').collect();
        for i in 0..TOTAL_LEN - 1 {
            if bare[i] == bare[i + 1] {
                continue; // swapping equal digits is not a transposition
            }
            let mut swapped = bare.clone();
            swapped.swap(i, i + 1);
            record(&mut out, std::str::from_utf8(&swapped).unwrap());
        }
    }

    out.push_str("\n# Legacy Crockford Numbers. Minted before the digit format; still valid.\n");
    for legacy in ["96M1-XPA3-345TS", "EGCC-BPEY-K9KBP", "01AB-CDEF-GHJKG"] {
        let bare: String = legacy.chars().filter(|ch| *ch != '-').collect();
        let confused: String = bare
            .chars()
            .map(|ch| match ch {
                '0' => 'O',
                '1' => 'L',
                other => other,
            })
            .collect();
        for form in [
            legacy.to_string(),
            bare.clone(),
            bare.to_lowercase(),
            format!("{} {} {}", &bare[0..4], &bare[4..8], &bare[8..13]),
            format!("number:{legacy}"),
            confused,
        ] {
            record(&mut out, &form);
        }
    }

    out.push_str("\n# Not a Number. All refused, without any lookup.\n");
    for bad in [
        "",
        "0",
        "@someone",
        "number:",
        "number",
        "04128837291",     // eleven digits
        "0412883729140",   // thirteen digits, outside the legacy alphabet check
        "+441234567890",   // a pasted telephone number keeps its plus and dies
        "(415) 555-0132",  // and so does one with parentheses
        "0412-8837-291X",  // a letter that is not a confusable
        "96M1-XPA3-345TT", // legacy, wrong check symbol
        "96M1-XPA3-345TU", // legacy check-only symbol presented as payload
        "96M1-XPA3-345T$", // outside base32
        "96M1-XPA3-345Té", // multi-byte
    ] {
        record(&mut out, bad);
    }
    // Built rather than typed, so its length is not a guess.
    record(&mut out, &"1".repeat(INPUT_MAX + 1));

    out
}

#[cfg(test)]
mod tests {
    use super::*;

    // ── Minting ──────────────────────────────────────────────────────────────

    #[test]
    fn minted_numbers_are_twelve_digits_grouped_for_speech() {
        for _ in 0..2_000 {
            let n = mint().expect("mint");
            assert_eq!(n.len(), TOTAL_LEN + 2, "grouping: {n}");
            assert_eq!(n.as_bytes()[4], b'-', "first group is four digits: {n}");
            assert_eq!(n.as_bytes()[9], b'-', "second group is four digits: {n}");
            let bare: Vec<u8> = n.bytes().filter(|b| *b != b'-').collect();
            assert_eq!(bare.len(), TOTAL_LEN);
            assert!(
                bare.iter().all(|b| b.is_ascii_digit()),
                "a Number is digits only: {n}"
            );
            assert!(luhn_ok(&bare), "check digit: {n}");
            assert_eq!(normalise(&n).as_deref(), Some(n.as_str()));
            assert!(!is_legacy(&n));
        }
    }

    /// A Number must be unpredictable, so the whole payload space must be
    /// reachable and no region may be favoured. Minting is not sequential, not
    /// derived from an identity and not derived from a clock — what this test
    /// can observe is the consequence: a flat leading digit and no repeats.
    #[test]
    fn minting_draws_uniformly_from_the_whole_space() {
        let draws = 40_000usize;
        let mut leading = [0usize; 10];
        let mut seen = std::collections::HashSet::new();
        for _ in 0..draws {
            let n = mint().expect("mint");
            let bare: Vec<u8> = n.bytes().filter(|b| *b != b'-').collect();
            leading[usize::from(bare[0] - b'0')] += 1;
            seen.insert(n);
        }
        // 10^11 space against 40k draws: a repeat would be a broken source.
        assert_eq!(seen.len(), draws, "minting repeated a Number");
        let expected = draws as f64 / 10.0;
        for (digit, count) in leading.iter().enumerate() {
            let ratio = *count as f64 / expected;
            assert!(
                ratio > 0.85 && ratio < 1.15,
                "leading digit {digit} appeared {count} times, expected about {expected}"
            );
        }
    }

    // ── Reading one back ─────────────────────────────────────────────────────

    #[test]
    fn round_trips_through_every_accepted_form() {
        let n = mint().expect("mint");
        let bare: String = n.chars().filter(|c| *c != '-').collect();
        for form in [
            n.clone(),
            bare.clone(),
            format!("{} {} {}", &bare[0..4], &bare[4..8], &bare[8..12]),
            format!("{}.{}.{}", &bare[0..4], &bare[4..8], &bare[8..12]),
            format!("{}_{}", &bare[0..6], &bare[6..12]),
            format!("{} {}", &bare[0..3], &bare[3..12]),
            format!("number:{n}"),
            format!("NUMBER:{n}"),
            format!("  {n}  "),
        ] {
            assert_eq!(
                normalise(&form).as_deref(),
                Some(n.as_str()),
                "form: {form}"
            );
        }
    }

    #[test]
    fn confusable_letters_fold_to_the_digits_they_were_written_for() {
        // Built rather than minted, so the Number under test definitely carries
        // both a zero and a one.
        let payload = b"01010101010";
        let mut symbols = [0u8; TOTAL_LEN];
        symbols[..PAYLOAD_LEN].copy_from_slice(payload);
        symbols[PAYLOAD_LEN] = luhn_check_digit(payload).unwrap();
        let n = canonical(&symbols);
        let bare: String = n.chars().filter(|c| *c != '-').collect();
        for (zero, one) in [('O', 'I'), ('o', 'i'), ('O', 'L'), ('o', 'l')] {
            let confused: String = bare
                .chars()
                .map(|c| match c {
                    '0' => zero,
                    '1' => one,
                    other => other,
                })
                .collect();
            assert_eq!(
                normalise(&confused).as_deref(),
                Some(n.as_str()),
                "confused: {confused}"
            );
        }
    }

    /// The property the whole scheme exists for, over real minted Numbers:
    /// every single-digit typo, in every position, caught by arithmetic alone.
    #[test]
    fn luhn_catches_every_single_digit_typo() {
        let mut checked = 0usize;
        for _ in 0..500 {
            let n = mint().expect("mint");
            let bare: Vec<u8> = n.bytes().filter(|b| *b != b'-').collect();
            for i in 0..TOTAL_LEN {
                for d in b'0'..=b'9' {
                    if d == bare[i] {
                        continue;
                    }
                    let mut broken = bare.clone();
                    broken[i] = d;
                    let s = String::from_utf8(broken).unwrap();
                    assert!(normalise(&s).is_none(), "single-digit typo accepted: {s}");
                    checked += 1;
                }
            }
        }
        assert_eq!(checked, 500 * TOTAL_LEN * 9);
    }

    /// Luhn's boundary, measured rather than claimed. Every adjacent
    /// transposition is caught except the pair `09`/`90`, and EVERY such pair
    /// slips through — so the gap is exactly one pair wide and cannot quietly
    /// become two without this test failing.
    #[test]
    fn luhns_only_transposition_blind_spot_is_zero_nine() {
        let mut caught = 0usize;
        let mut missed = 0usize;
        for _ in 0..3_000 {
            let n = mint().expect("mint");
            let bare: Vec<u8> = n.bytes().filter(|b| *b != b'-').collect();
            for i in 0..TOTAL_LEN - 1 {
                if bare[i] == bare[i + 1] {
                    continue; // swapping equal digits is not a transposition
                }
                let pair = (bare[i], bare[i + 1]);
                let is_zero_nine = pair == (b'0', b'9') || pair == (b'9', b'0');
                let mut swapped = bare.clone();
                swapped.swap(i, i + 1);
                let s = String::from_utf8(swapped).unwrap();
                let accepted = normalise(&s).is_some();
                if is_zero_nine {
                    assert!(
                        accepted,
                        "09/90 is Luhn's blind spot; this one was caught: {s}"
                    );
                    missed += 1;
                } else {
                    assert!(
                        !accepted,
                        "transposition accepted outside the blind spot: {s}"
                    );
                    caught += 1;
                }
            }
        }
        assert!(caught > 20_000, "the sample was too small to mean anything");
        assert!(
            missed > 0,
            "no 09/90 pair appeared, so the blind spot was never exercised"
        );
    }

    #[test]
    fn random_digit_strings_pass_at_one_in_ten() {
        // The check digit is the whole point: a blind guess must almost always
        // die in arithmetic rather than in the database.
        let mut raw = [0u8; TOTAL_LEN];
        let mut accepted = 0usize;
        let trials = 40_000usize;
        for _ in 0..trials {
            OsRng.fill_bytes(&mut raw);
            let s: String = raw.iter().map(|b| (b'0' + (*b % 10)) as char).collect();
            if normalise(&s).is_some() {
                accepted += 1;
            }
        }
        let rate = accepted as f64 / trials as f64;
        assert!(
            rate > 0.07 && rate < 0.13,
            "acceptance rate {rate} is not one in ten"
        );
    }

    #[test]
    fn malformed_input_is_rejected() {
        for s in [
            "",
            "0",
            "ABC",
            "04128837291",
            "0412883729140",
            "number:",
            "number",
            "0412-8837-291X",
            "  ",
            "----",
        ] {
            assert!(normalise(s).is_none(), "must reject: {s}");
        }
        assert!(normalise(&"1".repeat(INPUT_MAX + 1)).is_none());
    }

    #[test]
    fn a_pasted_telephone_number_is_named_as_malformed_not_merely_unknown() {
        // Stripping a leading plus would turn an E.164 number into a well-formed
        // F33D3R Number one time in ten, and the person would be told their
        // contact refused them rather than that they pasted the wrong thing.
        for s in ["+441234567890", "+1 415 555 0132", "(415) 555-0132"] {
            assert!(normalise(s).is_none(), "must reject: {s}");
        }
    }

    // ── Legacy Numbers ───────────────────────────────────────────────────────

    #[test]
    fn a_number_minted_before_this_format_still_resolves() {
        for legacy in ["96M1-XPA3-345TS", "EGCC-BPEY-K9KBP", "01AB-CDEF-GHJKG"] {
            assert_eq!(normalise(legacy).as_deref(), Some(legacy));
            assert!(is_legacy(legacy), "{legacy} must be recognised as legacy");
            let bare: String = legacy.chars().filter(|c| *c != '-').collect();
            for form in [
                bare.clone(),
                bare.to_lowercase(),
                format!("{} {} {}", &bare[0..4], &bare[4..8], &bare[8..13]),
                format!("number:{legacy}"),
            ] {
                assert_eq!(normalise(&form).as_deref(), Some(legacy), "form: {form}");
            }
        }
    }

    #[test]
    fn a_legacy_number_with_a_broken_check_symbol_is_still_refused() {
        let n = "96M1-XPA3-345TS";
        let bare: Vec<u8> = n.bytes().filter(|b| *b != b'-').collect();
        for candidate in LEGACY_ALPHABET.iter() {
            if *candidate == bare[LEGACY_PAYLOAD_LEN] {
                continue;
            }
            let mut broken = bare.clone();
            broken[LEGACY_PAYLOAD_LEN] = *candidate;
            let s = String::from_utf8(broken).unwrap();
            assert!(normalise(&s).is_none(), "must reject: {s}");
        }
    }

    /// The two shapes cannot be confused for one another, and minting only ever
    /// produces the new one.
    #[test]
    fn the_two_shapes_are_disjoint_and_only_one_is_minted() {
        for _ in 0..1_000 {
            let n = mint().expect("mint");
            assert!(!is_legacy(&n));
            assert_eq!(n.bytes().filter(|b| *b != b'-').count(), TOTAL_LEN);
        }
        assert!(is_legacy("96M1-XPA3-345TS"));
        assert!(is_legacy("96M1XPA3345TS"));
    }

    #[test]
    fn names_round_trip() {
        let n = mint().expect("mint");
        assert_eq!(from_name(&to_name(&n)), n);
        assert_eq!(from_name("not-a-namespaced-name"), "not-a-namespaced-name");
        // A legacy name stored before this format still round-trips unchanged.
        assert_eq!(from_name("number:96M1-XPA3-345TS"), "96M1-XPA3-345TS");
    }

    // ── The corpus the Go mirror is held to ──────────────────────────────────

    /// The corpus on disk must be exactly what this module produces. Anything
    /// else means the format changed and the file the Go mirror is tested
    /// against did not.
    #[test]
    fn conformance_corpus_is_current() {
        let built = conformance_corpus();
        let on_disk = std::fs::read_to_string(CONFORMANCE_PATH).unwrap_or_default();
        if built != on_disk {
            if std::env::var("UPDATE_NUMBER_CONFORMANCE").is_ok() {
                std::fs::write(CONFORMANCE_PATH, &built).expect("writing the corpus");
                return;
            }
            panic!(
                "{CONFORMANCE_PATH} is out of date with manhattan/src/number.rs.\n\
                 The Go mirror is tested against that file, so regenerate it:\n\
                 \n    UPDATE_NUMBER_CONFORMANCE=1 cargo test -p manhattan conformance\n"
            );
        }
    }

    /// And the corpus must be big enough, and mixed enough, to be worth failing
    /// against — a file of nothing but accepted Numbers would catch no drift in
    /// the half of the format that refuses.
    #[test]
    fn the_corpus_covers_both_verdicts_in_quantity() {
        let text = conformance_corpus();
        let mut oks = 0usize;
        let mut nos = 0usize;
        for line in text.lines() {
            if line.is_empty() || line.starts_with('#') {
                continue;
            }
            match line.split('\t').next() {
                Some("OK") => oks += 1,
                Some("NO") => nos += 1,
                other => panic!("unknown corpus verdict {other:?}"),
            }
        }
        assert!(oks > 400, "the corpus accepts too little to be meaningful");
        assert!(nos > 200, "the corpus refuses too little to be meaningful");
    }
}
