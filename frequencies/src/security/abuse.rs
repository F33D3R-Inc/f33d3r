//! Input hygiene. Every string a browser can influence passes through here
//! before it reaches a query, and the CHECK constraints in the schema are the
//! backstop if it somehow did not.

use crate::domain::frequency::{DESCRIPTION_MAX_CHARS, REASON_MAX_CHARS, TITLE_MAX_CHARS};
use crate::error::AuralisError;

/// Largest request body any control-plane route accepts. Titles, reasons and
/// a handful of flags fit in a few hundred bytes; this is generous.
pub const MAX_BODY_BYTES: usize = 16 * 1024;

/// Idempotency keys are opaque but bounded.
pub const MAX_IDEMPOTENCY_KEY_CHARS: usize = 128;

/// Trim, collapse control characters to spaces, and enforce a character
/// limit. Counts chars, not bytes, so a limit of 120 is 120 for every script.
fn clean(
    input: &str,
    max_chars: usize,
    field: &str,
    required: bool,
) -> Result<String, AuralisError> {
    let cleaned: String = input
        .chars()
        .map(|c| if c.is_control() && c != '\n' { ' ' } else { c })
        .collect::<String>()
        .trim()
        .to_string();
    if required && cleaned.is_empty() {
        return Err(AuralisError::BadRequest(format!("{field} is required")));
    }
    if cleaned.chars().count() > max_chars {
        return Err(AuralisError::BadRequest(format!(
            "{field} exceeds {max_chars} characters"
        )));
    }
    Ok(cleaned)
}

pub fn title(input: &str) -> Result<String, AuralisError> {
    clean(input, TITLE_MAX_CHARS, "title", true)
}

pub fn description(input: &str) -> Result<String, AuralisError> {
    clean(input, DESCRIPTION_MAX_CHARS, "description", false)
}

pub fn reason(input: &str) -> Result<String, AuralisError> {
    clean(input, REASON_MAX_CHARS, "reason", false)
}

pub fn language(input: &str) -> Result<String, AuralisError> {
    let l = input.trim().to_ascii_lowercase();
    let ok =
        (2..=16).contains(&l.len()) && l.chars().all(|c| c.is_ascii_alphanumeric() || c == '-');
    if ok {
        Ok(l)
    } else {
        Err(AuralisError::BadRequest(
            "language must be a BCP-47 tag".into(),
        ))
    }
}

pub fn idempotency_key(input: Option<&str>) -> Result<Option<String>, AuralisError> {
    match input.map(str::trim) {
        None | Some("") => Ok(None),
        Some(k)
            if k.chars().count() <= MAX_IDEMPOTENCY_KEY_CHARS
                && !k.chars().any(char::is_control) =>
        {
            Ok(Some(k.to_string()))
        }
        Some(_) => Err(AuralisError::BadRequest(
            "Idempotency-Key is too long".into(),
        )),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn title_is_required_and_bounded() {
        assert!(title("   ").is_err());
        assert_eq!(
            title("  Late Night Tech Talk ").unwrap(),
            "Late Night Tech Talk"
        );
        assert!(title(&"x".repeat(121)).is_err());
        assert!(title(&"é".repeat(120)).is_ok());
    }

    #[test]
    fn control_characters_are_flattened() {
        assert_eq!(title("a\u{0}b\tc").unwrap(), "a b c");
        assert_eq!(description("line\nline").unwrap(), "line\nline");
    }

    #[test]
    fn reason_may_be_empty_but_not_long() {
        assert_eq!(reason("").unwrap(), "");
        assert!(reason(&"r".repeat(141)).is_err());
    }

    #[test]
    fn language_is_a_tag() {
        assert_eq!(language("EN").unwrap(), "en");
        assert_eq!(language("pt-BR").unwrap(), "pt-br");
        assert!(language("e").is_err());
        assert!(language("en us").is_err());
    }

    #[test]
    fn idempotency_key_bounds() {
        assert_eq!(idempotency_key(None).unwrap(), None);
        assert_eq!(idempotency_key(Some("  ")).unwrap(), None);
        assert_eq!(idempotency_key(Some("k1")).unwrap(), Some("k1".into()));
        assert!(idempotency_key(Some(&"k".repeat(129))).is_err());
    }
}
