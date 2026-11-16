//! The identity vocabulary, written down once.
//!
//! Capability names and states are feed-engine's (feed-engine/internal/model/types.go);
//! the database CHECKs in 0003 hold the same lists, so a name that is not here
//! cannot be stored either.

use chrono::{Datelike, NaiveDate, Utc};

pub const CAP_POSTING: &str = "POSTING";
pub const CAP_NSFW_ACCESS: &str = "NSFW_ACCESS";
pub const CAP_MONETIZATION: &str = "MONETIZATION";
pub const CAP_REALM_PROGRESSION: &str = "REALM_PROGRESSION";
pub const CAP_NEW_ACCOUNT_TRUST: &str = "NEW_ACCOUNT_TRUST";
pub const CAP_MESSAGING: &str = "MESSAGING";
pub const CAP_MUSIC_UPLOAD: &str = "MUSIC_UPLOAD";

pub const CAPABILITIES: &[&str] = &[
    CAP_POSTING,
    CAP_NSFW_ACCESS,
    CAP_MONETIZATION,
    CAP_REALM_PROGRESSION,
    CAP_NEW_ACCOUNT_TRUST,
    CAP_MESSAGING,
    CAP_MUSIC_UPLOAD,
];

/// Granted to every new PIAL (model.DefaultCapabilities).
pub const DEFAULT_CAPABILITIES: &[&str] = &[
    CAP_POSTING,
    CAP_REALM_PROGRESSION,
    CAP_NEW_ACCOUNT_TRUST,
    CAP_MESSAGING,
    CAP_MUSIC_UPLOAD,
    CAP_MONETIZATION,
];

pub const STATE_GRANTED: &str = "granted";
pub const STATE_RESTRICTED: &str = "restricted";
pub const STATE_REVOKED: &str = "revoked";
pub const STATE_COOLDOWN: &str = "cooldown";
pub const CAP_STATES: &[&str] = &[STATE_GRANTED, STATE_RESTRICTED, STATE_REVOKED, STATE_COOLDOWN];

pub const ROLE_USER: &str = "user";
pub const ROLE_ADMIN: &str = "admin";
pub const ROLE_FOUNDER: &str = "founder";

pub const STATUS_ACTIVE: &str = "active";
pub const STATUS_SUSPENDED: &str = "suspended";
pub const STATUS_BANNED: &str = "banned";
pub const STATUS_TOMBSTONED: &str = "tombstoned";
pub const STATUS_PURGED: &str = "purged";

pub const ENFORCEMENT_STATES: &[&str] = &["none", "warning", "restricted", "suspended", "terminated"];

/// A status in which the identity no longer exists for anyone: it cannot sign
/// in, and its sessions do not resolve.
pub fn is_gone(status: &str) -> bool {
    status == STATUS_TOMBSTONED || status == STATUS_PURGED
}

/// A status in which the identity exists but may not enter.
pub fn is_barred(status: &str) -> bool {
    status == STATUS_SUSPENDED || status == STATUS_BANNED
}

/// The status of one account as callers branch on it: the identity's state
/// where that bars or ends it, else the account's own. purged > banned >
/// suspended > deactivated > active. An identity tombstoned without a purge
/// has every account deactivated, which is what this says.
pub fn account_status(pial_status: &str, deactivated: bool, purged: bool) -> &'static str {
    if purged || pial_status == STATUS_PURGED {
        return "purged";
    }
    match pial_status {
        STATUS_BANNED => "banned",
        STATUS_SUSPENDED => "suspended",
        _ if deactivated || pial_status == STATUS_TOMBSTONED => "deactivated",
        _ => "active",
    }
}

pub fn is_capability(name: &str) -> bool {
    CAPABILITIES.contains(&name)
}

// ── Handles ───────────────────────────────────────────────────────────────────

/// The canonical form of a handle as a person typed it: trimmed, one leading
/// '@' dropped, lower-cased.
pub fn normalize_handle(raw: &str) -> String {
    raw.trim().trim_start_matches('@').to_lowercase()
}

/// The registry's rule (registry-brain validate_handle): 1–30 of a-z 0-9 _.
/// Applied to every handle claimed from now on; handles that predate it are
/// imported as they stand.
pub fn handle_is_valid(handle: &str) -> bool {
    !handle.is_empty()
        && handle.len() <= 30
        && handle
            .bytes()
            .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || b == b'_')
}

// ── Passwords ─────────────────────────────────────────────────────────────────

pub const PASSWORD_MIN_CHARS: usize = 8;
/// bcrypt reads at most 72 bytes; a longer password would be silently cut, so
/// two passwords sharing their first 72 bytes would be the same password.
pub const PASSWORD_MAX_BYTES: usize = 72;

pub enum PasswordProblem {
    TooShort,
    TooLong,
}

pub fn check_password(password: &str) -> Result<(), PasswordProblem> {
    if password.chars().count() < PASSWORD_MIN_CHARS {
        return Err(PasswordProblem::TooShort);
    }
    if password.len() > PASSWORD_MAX_BYTES {
        return Err(PasswordProblem::TooLong);
    }
    Ok(())
}

// ── Age ───────────────────────────────────────────────────────────────────────

/// The youngest age at which an account may be created (feed-engine createAccount).
pub const MINIMUM_AGE: i32 = 16;

/// Whole years between dob and today (UTC), as feed-engine's ageFromDOB.
pub fn age_on(dob: NaiveDate, today: NaiveDate) -> i32 {
    let mut age = today.year() - dob.year();
    if (today.month(), today.day()) < (dob.month(), dob.day()) {
        age -= 1;
    }
    age
}

pub fn age_today(dob: NaiveDate) -> i32 {
    age_on(dob, Utc::now().date_naive())
}

// ── Onboarding facts that are not identity ───────────────────────────────────

/// role_type as feed-engine normalises it: user | creator | official.
pub fn normalize_role_type(raw: &str) -> &'static str {
    match raw {
        "creator" => "creator",
        "official" => "official",
        _ => "user",
    }
}

/// content_setting as feed-engine normalises it, with the minor rule applied:
/// a minor may never enable adult content, whatever was submitted.
pub fn normalize_content_setting(raw: &str, is_minor: bool) -> &'static str {
    let setting = match raw {
        "safe_mode" => "safe_mode",
        "adult_enabled" => "adult_enabled",
        _ => "default",
    };
    if is_minor && setting == "adult_enabled" {
        "default"
    } else {
        setting
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn age_counts_whole_years() {
        let d = |y, m, day| NaiveDate::from_ymd_opt(y, m, day).unwrap();
        assert_eq!(age_on(d(2000, 6, 15), d(2018, 6, 14)), 17);
        assert_eq!(age_on(d(2000, 6, 15), d(2018, 6, 15)), 18);
    }

    #[test]
    fn handles_follow_the_registry_rule() {
        assert!(handle_is_valid("tehani_b"));
        assert!(!handle_is_valid("Tehani"));
        assert!(!handle_is_valid("a-b"));
        assert!(!handle_is_valid(""));
        assert_eq!(normalize_handle(" @Bob "), "bob");
    }
}
