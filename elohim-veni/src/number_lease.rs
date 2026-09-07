//! Number leases — the three properties that make a Number more than a single
//! rotatable code: a LABEL, an EXPIRY and a USE BUDGET.
//!
//! WHY THESE LIVE HERE. Manhattan allocates the Number node and owns the name
//! plane; its own doctrine (migrations/0001) is explicit that "a node has no
//! payload — it is an identity and an ownership boundary, nothing more". A
//! label, a lease and a budget are none of those things: they are statements
//! about who may reach this identity through this Number, which is precisely
//! what this brain owns. So they attach to `number_policies`, keyed by the
//! Number's node id, and Manhattan learns nothing about them.
//!
//! WHAT EACH ONE IS.
//!
//!   LABEL is owner-private. It exists so the person who minted five Numbers
//!   knows which one to retire. It is never returned by a resolve, never
//!   carried in a decision, never signed into a key bundle and never logged.
//!   `evaluate_contact` reads the policy row and discards the label; the only
//!   readers are the owner's own list and this module's tests.
//!
//!   EXPIRY is a lease, not a chore. A conference Number stops admitting new
//!   contact on Sunday because the owner said so on Thursday. It is enforced in
//!   TWO places and the split matters:
//!
//!     * At decision time, in SQL, against the database's own NOW(). This is
//!       the load-bearing enforcement. There is exactly one clock — the same
//!       one that stamped `expires_at` — so there is no skew to reason about,
//!       and a sweeper that stops running cannot resurrect an expired Number.
//!
//!     * By a sweeper, which retires the expired Number in Manhattan so the
//!       name stops resolving at all. This is hygiene, never security: it
//!       revokes FIRST and only then forgets the policy row, because forgetting
//!       first would leave a live Number falling back to the identity's default
//!       policy — an expiry that re-opened the door it was meant to close.
//!
//!   USE BUDGET caps how many NEW people this Number introduces. It is counted
//!   in DISTINCT INITIATING IDENTITIES ADMITTED, never in resolve attempts.
//!   Counting attempts would hand any stranger holding a screenshot of the
//!   Number the power to burn the owner's reachability to zero — a denial of
//!   service on the owner, delivered through the owner's own safety feature.
//!   Counting admissions costs an attacker one account per unit, which is the
//!   same price a genuine attendee pays, so a spent budget means the Number
//!   really did introduce that many people.
//!
//! WHAT DOES NOT SPEND BUDGET, and why. A contact on F33D3R is a mutual follow;
//! there is no separate address book. Someone who could already reach the owner
//! without the Number did not need it, so presenting it is a no-op that costs
//! nothing: a mutual follow, a standing grant, a redeemed contact link. The
//! test is not a list of cases but a question asked of the same pure decision
//! function — "what would this person have got WITHOUT the Number?" — so the
//! rule cannot drift away from the policy vocabulary it depends on.
//!
//! A knock does not spend budget either. Only an `allow` does. Charging the
//! knock would let a hostile crowd exhaust a Number the owner never agreed to
//! open for anyone, which is the same denial of service in a smaller costume.

use std::time::Duration;

use anyhow::Result;
use chrono::{DateTime, Utc};
use sqlx::PgPool;
use tracing::{error, info, warn};
use uuid::Uuid;

use crate::contact::ContactFacts;
use manhattan_client::{ConflictCode, Manhattan, ManhattanError};

// ── Schema ────────────────────────────────────────────────────────────────────

/// Applied by `db::migrate` after the contact plane, and idempotent like every
/// other blob this brain replays on boot.
pub const SCHEMA: &str = r#"
-- ── The lease on a Number ────────────────────────────────────────────────────
-- Both columns are NULL by default, and NULL means "no limit". A Number that
-- existed before this migration keeps behaving exactly as it did: a lease is an
-- offer, never something applied retroactively to a promise already made.
ALTER TABLE number_policies ADD COLUMN IF NOT EXISTS expires_at     TIMESTAMPTZ;
ALTER TABLE number_policies ADD COLUMN IF NOT EXISTS max_admissions INT;

ALTER TABLE number_policies DROP CONSTRAINT IF EXISTS number_policies_budget_positive;
ALTER TABLE number_policies ADD CONSTRAINT number_policies_budget_positive
    CHECK (max_admissions IS NULL OR max_admissions > 0);

COMMENT ON COLUMN number_policies.expires_at IS
    'When this Number stops admitting new contact. NULL is never, and never is the default. Compared against the database''s own NOW() at decision time, so there is one clock and no skew.';
COMMENT ON COLUMN number_policies.max_admissions IS
    'How many DISTINCT identities this Number may admit. NULL is unlimited. Counted in admissions, never in resolve attempts: a counter a stranger can burn down is a denial of service on the owner.';

-- The sweeper's only query.
CREATE INDEX IF NOT EXISTS idx_number_policies_expiring
    ON number_policies (expires_at) WHERE expires_at IS NOT NULL;

-- ── The admission ledger ─────────────────────────────────────────────────────
-- One row per (Number, identity admitted). The primary key is the whole budget
-- rule: an identity admitted twice through the same Number is the same
-- admission, so a person who re-resolves a Number they already used spends
-- nothing. The row is a fact about an introduction that happened, so it is
-- never removed to give a slot back — a later mutual follow does not un-admit
-- anybody.
--
-- ON DELETE CASCADE from number_policies: retiring a Number drops its policy
-- row, and the `number` namespace is not reusable, so this Number can never
-- come back and its ledger has nothing left to say.
CREATE TABLE IF NOT EXISTS number_admissions (
    number_node_id UUID        NOT NULL
                   REFERENCES number_policies(number_node_id) ON DELETE CASCADE,
    initiator_pial UUID        NOT NULL,
    admitted_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (number_node_id, initiator_pial)
);
CREATE INDEX IF NOT EXISTS idx_number_admissions_initiator
    ON number_admissions (initiator_pial);
"#;

// ── Limits ────────────────────────────────────────────────────────────────────

/// A label is rendered beside the Number in the owner's own list. Sixty-four
/// characters holds "ACL Conference 2026 — reviewer badge" comfortably and
/// still leaves a list of ten Numbers readable at a glance.
pub const LABEL_MAX_CHARS: usize = 64;

/// A lease shorter than a minute is a mistake, not an intention.
pub const MIN_LEASE_SECONDS: i64 = 60;

/// A year. Past this, "never" is the honest answer and the owner should pick it
/// deliberately rather than express it as a very large number.
pub const MAX_LEASE_SECONDS: i64 = 365 * 24 * 60 * 60;

/// A budget of zero would mean "a Number that admits nobody", which is what the
/// `closed` policy already says.
pub const MIN_BUDGET: i32 = 1;

/// Well past any room, lecture hall or mailing list a Number is handed round in.
/// A cap this high is indistinguishable from unlimited in practice; its purpose
/// is to keep an absurd value out of the column.
pub const MAX_BUDGET: i32 = 10_000;

/// The budget a newly minted Number carries when its owner did not say.
///
/// WHY THERE IS ONE AT ALL. A Number used to be 2^60 addresses wide, and an
/// unlimited Number was defensible because harvesting one was infeasible: you
/// could not find somebody's Number, so an uncapped one was worth nothing to
/// you. A Number is now eleven payload digits and a Luhn check — 10^11, about
/// 11.5 million times smaller — and a harvested Number is worth exactly as much
/// as it can still admit. Bounding that is what turns a lucky guess into a
/// bounded, visible event instead of a permanent channel.
///
/// WHY SIXTY. Not because it is round. The largest cohort in any of the owner's
/// stated cases — a conference, a class, a project group, a one-to-one meet-up —
/// is a room of about thirty. A spoken Number spreads about one hop past the
/// room it was said in, because the person who has it tells the colleague who
/// missed it, so the honest ceiling is about twice the room. Sixty is that, and
/// it leaves a Number that has genuinely introduced sixty strangers as something
/// the owner should look at rather than something that keeps running quietly.
///
/// WHAT IT IS NOT. It is not applied to any Number that already exists: a Number
/// somebody has already shared is a promise made under the terms it was minted
/// with, and a budget that appeared under it afterwards would stop it working
/// for people it was given to — invisibly, because exhausted is deliberately
/// indistinguishable from unknown. The default is read at MINT and nowhere else.
///
/// And it is not a rule. An owner who wants an unlimited Number — a creator
/// putting one in their bio is the case, and it is legitimate — says so when
/// they mint it and gets one. See `numberBudgetOptions` in feed-engine, which is
/// where that choice is offered.
pub const DEFAULT_BUDGET: i32 = 60;

/// When the owner is warned that a Number is running out: the first admission
/// that leaves a quarter of the budget or less.
///
/// A quarter rather than a fixed count, because "fifteen left" means something
/// different on a budget of sixty and on a budget of twenty. The warning fires
/// exactly once, on the admission that lands on the threshold, because each
/// admission decrements the remainder by exactly one and so crosses it once.
const BUDGET_LOW_DIVISOR: i32 = 4;

// ── Label ─────────────────────────────────────────────────────────────────────

/// Characters removed outright rather than rendered.
///
/// Non-whitespace control characters corrupt logs and terminals. The
/// bidirectional formatting characters are the sharper problem: a label sits
/// directly beside the Number it names, and an embedded right-to-left override
/// can visually reorder the symbols the owner reads — so the owner retires the
/// wrong Number. Zero-width characters do the same trick by hiding a difference
/// between two labels that render identically.
fn is_stripped(c: char) -> bool {
    matches!(c,
        '\u{061C}'                  // ARABIC LETTER MARK
        | '\u{200B}'..='\u{200F}'   // zero-width space/joiners, LRM, RLM
        | '\u{202A}'..='\u{202E}'   // LRE, RLE, PDF, LRO, RLO
        | '\u{2066}'..='\u{2069}'   // LRI, RLI, FSI, PDI
        | '\u{FEFF}'                // zero-width no-break space
    ) || (c.is_control() && !c.is_whitespace())
}

/// Canonicalises an owner-supplied label. Untrusted input that is stored and
/// later rendered, so it is cleaned once here — at the authority that owns the
/// column — rather than at each of the surfaces that display it.
///
/// Escaping is still the renderer's job; this is about what may be stored at
/// all. Truncation counts characters, never bytes, so a label cannot be cut
/// through the middle of a UTF-8 sequence.
pub fn sanitise_label(raw: &str) -> String {
    let mut out = String::with_capacity(raw.len().min(LABEL_MAX_CHARS * 4));
    let mut kept = 0usize;
    let mut pending_space = false;
    for c in raw.chars() {
        if is_stripped(c) {
            continue;
        }
        if c.is_whitespace() {
            // Any run of whitespace, of any kind, reads as one space.
            if kept > 0 {
                pending_space = true;
            }
            continue;
        }
        if pending_space {
            if kept == LABEL_MAX_CHARS {
                break;
            }
            out.push(' ');
            kept += 1;
            pending_space = false;
        }
        if kept == LABEL_MAX_CHARS {
            break;
        }
        out.push(c);
        kept += 1;
    }
    out
}

// ── Validation ────────────────────────────────────────────────────────────────

/// A lease the owner asked for, as a duration rather than an instant. The wire
/// carries seconds and this brain turns them into an instant against its own
/// clock, so no other machine's idea of "now" can shorten or extend a lease.
pub fn validate_lease_seconds(seconds: i64) -> Result<i64, &'static str> {
    if seconds < MIN_LEASE_SECONDS {
        return Err("a Number's lease must be at least a minute");
    }
    if seconds > MAX_LEASE_SECONDS {
        return Err("a Number's lease may not exceed a year — choose no expiry instead");
    }
    Ok(seconds)
}

pub fn validate_budget(budget: i32) -> Result<i32, &'static str> {
    if budget < MIN_BUDGET {
        return Err("a Number's budget must admit at least one person");
    }
    if budget > MAX_BUDGET {
        return Err("a Number's budget is too large — leave it unset for no limit");
    }
    Ok(budget)
}

// ── State ─────────────────────────────────────────────────────────────────────

/// What a Number's lease says right now.
///
/// To a RESOLVER, `Expired` and `Exhausted` are not states at all: both produce
/// the same refusal a retired or unknown Number produces, in the same shape,
/// with the same status. The distinction exists only for the owner's own
/// surface, which is the one place it is safe to make.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum LeaseState {
    Live,
    Expired,
    Exhausted,
}

impl LeaseState {
    /// Whether this Number may still admit somebody new.
    pub fn admits(self) -> bool {
        matches!(self, LeaseState::Live)
    }

    /// The owner-facing name. Never reaches a resolver.
    pub fn as_str(self) -> &'static str {
        match self {
            LeaseState::Live => "live",
            LeaseState::Expired => "expired",
            LeaseState::Exhausted => "exhausted",
        }
    }
}

/// The lease rule, pure, so it is testable without a database and reads the
/// same way it is enforced. Expiry is checked before budget because an expired
/// Number is finished whatever its budget says.
pub fn lease_state(expired: bool, max_admissions: Option<i32>, admitted: i64) -> LeaseState {
    if expired {
        return LeaseState::Expired;
    }
    match max_admissions {
        Some(max) if admitted >= i64::from(max) => LeaseState::Exhausted,
        _ => LeaseState::Live,
    }
}

// ── Who is already a contact ──────────────────────────────────────────────────

/// Whether the initiator is ALREADY connected to the owner.
///
/// On F33D3R a CONTACT IS A MUTUAL FOLLOW. There is no separate address book and
/// no contacts entity; the follow graph is the whole relationship model. So the
/// people who do not spend a Number's budget are the people the owner is already
/// connected to, by any of the three routes this platform actually has:
///
///   a mutual follow — asserted by the brain that owns the follow graph;
///   a standing grant — an accepted contact request, which this brain owns;
///   a redeemed contact link — the owner's own invitation, which becomes a
///   standing grant in the same transaction that redeems it.
///
/// Presenting a Number when you are already one of these is a NO-OP, not an
/// error and not a charge. The budget bounds how many STRANGERS a shared Number
/// introduces; letting an owner's own people drain a conference Number would
/// make it mean something nobody asked for.
///
/// Note what is deliberately NOT here: a one-way follower. A follower is not a
/// contact on F33D3R — the mutual is — so a Number that admits one has done the
/// work the budget counts.
pub fn already_a_contact(facts: &ContactFacts) -> bool {
    facts.mutual || facts.granted || facts.capability_ok
}

/// Whether this contact attempt spends one unit of the Number's budget.
///
/// The whole budget rule, in one place, as a pure function of the decision that
/// was actually taken. Three conditions, and each one is load-bearing:
///
///   The decision is `allow`. A knock spends nothing and a refusal spends
///   nothing, so no party who was not admitted can move the counter. That is
///   the denial-of-service defence: a stranger holding a screenshot of the
///   Number cannot burn the owner's reachability down by resolving it, because
///   resolving is not what costs anything — being let in is. Buying one unit
///   costs an attacker one account, which is exactly what it costs a genuine
///   attendee, so a spent budget means the Number really did introduce that
///   many people.
///
///   The attempt came through the Number. The @handle path never touches a
///   Number's budget.
///
///   The initiator is not already a contact. See `already_a_contact`.
///
/// What it does NOT depend on is how many times anybody asked. The ledger is
/// keyed by identity, so the same person admitted twice is one admission — and
/// an admission is never given back, so somebody who reached the owner through
/// the Number and only later became a mutual follow does not free a slot. The
/// introduction happened; the count records that it happened.
pub fn spends_budget(decision: &str, via_number: bool, facts: &ContactFacts) -> bool {
    decision == "allow" && via_number && !already_a_contact(facts)
}

// ── Storage ───────────────────────────────────────────────────────────────────

/// A Number's policy and its lease, written as one row because they are one
/// decision the owner makes at one moment. Splitting them across two statements
/// would allow a Number that is live with a policy and no lease, or a lease
/// attached to a policy that failed to land.
///
/// `expires_in_seconds` and `max_admissions` are each TRI-STATE, and the
/// distinction is load-bearing rather than fussy:
///
///   `None`       — the caller said nothing about this. Leave the column
///                  exactly as it is. Changing a Number's POLICY from a form
///                  that never mentioned its lease must not silently throw the
///                  lease away, which is what a two-state parameter would do.
///   `Some(None)` — the caller cleared it. No expiry, or no budget.
///   `Some(v)`    — the caller set it.
///
/// The expiry crosses as a DURATION and becomes an instant here, against this
/// database's NOW() — the same clock every later comparison uses, so there is
/// one clock and no skew to reason about.
///
/// The `WHERE` on the conflict branch is the ownership check: a row already
/// held by a different identity is left alone rather than reassigned.
#[allow(clippy::too_many_arguments)]
pub async fn attach(
    pool: &PgPool,
    number_node: Uuid,
    pial: Uuid,
    policy: &str,
    label: &str,
    expires_in_seconds: Option<Option<i64>>,
    max_admissions: Option<Option<i32>>,
) -> Result<()> {
    sqlx::query(
        "INSERT INTO number_policies
             (number_node_id, pial_id, policy, label, expires_at, max_admissions)
         VALUES ($1, $2, $3, $4,
                 CASE WHEN $5::BOOL AND $6::BIGINT IS NOT NULL
                      THEN NOW() + ($6::BIGINT * INTERVAL '1 second')
                 END,
                 CASE WHEN $7::BOOL THEN $8::INT END)
         ON CONFLICT (number_node_id) DO UPDATE
            SET policy         = EXCLUDED.policy,
                label          = EXCLUDED.label,
                expires_at     = CASE WHEN $5::BOOL THEN EXCLUDED.expires_at
                                      ELSE number_policies.expires_at END,
                max_admissions = CASE WHEN $7::BOOL THEN EXCLUDED.max_admissions
                                      ELSE number_policies.max_admissions END,
                updated_at     = NOW()
          WHERE number_policies.pial_id = EXCLUDED.pial_id",
    )
    .bind(number_node)
    .bind(pial)
    .bind(policy)
    .bind(label)
    .bind(expires_in_seconds.is_some())
    .bind(expires_in_seconds.flatten())
    .bind(max_admissions.is_some())
    .bind(max_admissions.flatten())
    .execute(pool)
    .await?;
    Ok(())
}

/// The lease as the decision path needs it: whether this Number may still admit
/// anybody, and whether it has a budget that an admission could spend.
///
/// The second field exists to keep a round trip off the hot path. Deciding
/// whether an admission spends a unit needs to know whether the initiator is
/// already a contact, and that answer comes from the naming plane's association
/// graph — so it is worth asking only when there is a budget for it to protect.
#[derive(Debug, Clone, Copy)]
pub struct LeaseGate {
    pub state: LeaseState,
    pub has_budget: bool,
}

/// The lease state of one Number, read against the database clock.
///
/// A Number with no policy row has no lease: it falls back to the identity's
/// default policy and is Live. That is the pre-existing behaviour for a Number
/// minted before this plane existed, and it stays.
pub async fn gate(pool: &PgPool, number_node: Uuid) -> Result<LeaseGate> {
    let row: Option<(bool, Option<i32>, i64)> = sqlx::query_as(
        "SELECT (np.expires_at IS NOT NULL AND np.expires_at <= NOW()),
                np.max_admissions,
                (SELECT COUNT(*) FROM number_admissions a
                  WHERE a.number_node_id = np.number_node_id)
           FROM number_policies np
          WHERE np.number_node_id = $1",
    )
    .bind(number_node)
    .fetch_optional(pool)
    .await?;
    Ok(match row {
        Some((expired, max, admitted)) => LeaseGate {
            state: lease_state(expired, max, admitted),
            has_budget: max.is_some(),
        },
        None => LeaseGate {
            state: LeaseState::Live,
            has_budget: false,
        },
    })
}

/// What the OWNER should be told about a Number's budget after an admission.
///
/// The whole point of this type is WHO it is for. To the person who presented
/// the Number, a budget does not exist: they are admitted or they meet a refusal
/// identical to an unknown Number's. To the OWNER, a Number that has quietly
/// stopped working is the worst failure this feature has, so the moment it stops
/// is the moment they hear about it.
///
/// It rides back on an ALLOW and never on a refusal. That is not an accident of
/// where it was convenient to put it: an allow has already disclosed the target
/// identity to the calling brain, so nothing new crosses. A refusal discloses
/// nothing, and telling the caller's brain "that Number is exhausted" would hand
/// it the one fact the refusal exists to withhold.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum BudgetAlert {
    /// Nothing to say: no budget, or plenty of it left.
    None,
    /// This admission left a quarter of the budget or less.
    Low,
    /// This admission was the last one. The Number admits nobody new from here.
    Spent,
}

impl BudgetAlert {
    /// The name the owner's brain addresses a notification by. `None` has no
    /// name because it is never sent.
    pub fn as_str(self) -> Option<&'static str> {
        match self {
            BudgetAlert::None => None,
            BudgetAlert::Low => Some("low"),
            BudgetAlert::Spent => Some("spent"),
        }
    }
}

/// Which warning, if any, an admission that left `remaining` of `max` earns.
///
/// Pure, so the "exactly once" property is testable without a database: each
/// admission decrements `remaining` by one, so each threshold is landed on once.
pub fn budget_alert(max_admissions: Option<i32>, remaining: i64) -> BudgetAlert {
    let Some(max) = max_admissions else {
        return BudgetAlert::None;
    };
    if remaining <= 0 {
        return BudgetAlert::Spent;
    }
    let threshold = i64::from(max / BUDGET_LOW_DIVISOR);
    if threshold >= 1 && remaining == threshold {
        return BudgetAlert::Low;
    }
    BudgetAlert::None
}

/// What happened when a Number tried to admit somebody.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Admission {
    /// A new identity was admitted and one unit of budget is spent. Carries what
    /// the owner should be told about what is left.
    Recorded(BudgetAlert),
    /// This identity was already admitted through this Number. Nothing is
    /// spent — an introduction that already happened does not happen twice.
    AlreadyAdmitted,
    /// The lease ran out or the budget is gone. Indistinguishable from a
    /// retired Number to whoever asked.
    Refused,
    /// This Number carries no policy row, so it carries no budget: there is
    /// nothing to spend and nothing to refuse.
    NoLease,
}

/// Spends one unit of budget, or refuses.
///
/// This is the authoritative check, not the pre-gate in the decision path. The
/// pre-gate is an early exit that saves work; THIS is what makes the cap true,
/// because it takes a row lock on the policy first. Without that lock two
/// simultaneous fortieth admissions would both read 39 and both commit, and a
/// budget of 40 would admit 41. Every admission through one Number serialises
/// behind that lock; admissions through different Numbers do not contend.
///
/// A Number with no policy row has no budget to spend, so nothing is recorded
/// and nothing is refused.
pub async fn record_admission(
    pool: &PgPool,
    number_node: Uuid,
    initiator: Uuid,
) -> Result<Admission> {
    let mut tx = pool.begin().await?;

    let locked: Option<(Option<i32>, bool)> = sqlx::query_as(
        "SELECT max_admissions,
                (expires_at IS NOT NULL AND expires_at <= NOW())
           FROM number_policies
          WHERE number_node_id = $1
            FOR UPDATE",
    )
    .bind(number_node)
    .fetch_optional(&mut *tx)
    .await?;

    let Some((max_admissions, expired)) = locked else {
        tx.rollback().await?;
        return Ok(Admission::NoLease);
    };

    let already: Option<(bool,)> = sqlx::query_as(
        "SELECT TRUE FROM number_admissions
          WHERE number_node_id = $1 AND initiator_pial = $2",
    )
    .bind(number_node)
    .bind(initiator)
    .fetch_optional(&mut *tx)
    .await?;
    if already.is_some() {
        tx.commit().await?;
        return Ok(Admission::AlreadyAdmitted);
    }

    let admitted: (i64,) =
        sqlx::query_as("SELECT COUNT(*) FROM number_admissions WHERE number_node_id = $1")
            .bind(number_node)
            .fetch_one(&mut *tx)
            .await?;

    if !lease_state(expired, max_admissions, admitted.0).admits() {
        tx.rollback().await?;
        return Ok(Admission::Refused);
    }

    sqlx::query(
        "INSERT INTO number_admissions (number_node_id, initiator_pial)
         VALUES ($1, $2)
         ON CONFLICT (number_node_id, initiator_pial) DO NOTHING",
    )
    .bind(number_node)
    .bind(initiator)
    .execute(&mut *tx)
    .await?;

    tx.commit().await?;
    // Computed from the count read under the same lock that authorised the
    // insert, so two admissions racing cannot both believe they were the last
    // one and warn twice — or both believe they were not, and warn never.
    let remaining = max_admissions
        .map(|max| i64::from(max) - (admitted.0 + 1))
        .unwrap_or(i64::MAX);
    Ok(Admission::Recorded(budget_alert(max_admissions, remaining)))
}

/// One Number's lease as the OWNER sees it. Nothing in here ever crosses to a
/// resolver: the label is theirs, and how much of a budget is left is a fact
/// about their own Number.
#[derive(Debug, Clone)]
pub struct LeaseView {
    pub number_node_id: Uuid,
    pub expires_at: Option<DateTime<Utc>>,
    /// How long is left, measured against THIS database's clock — the same one
    /// that stamped the lease and the same one that enforces it. The surface
    /// that renders "six days left" therefore reads the authority's answer
    /// rather than consulting a second clock of its own.
    pub remaining_seconds: Option<i64>,
    pub max_admissions: Option<i32>,
    pub admissions: i64,
    pub state: LeaseState,
}

/// Every lease held by one identity, for that identity's own surface.
pub async fn leases_for(pool: &PgPool, pial: Uuid) -> Result<Vec<LeaseView>> {
    let rows: Vec<(
        Uuid,
        Option<DateTime<Utc>>,
        Option<i64>,
        Option<i32>,
        i64,
        bool,
    )> = sqlx::query_as(
        "SELECT np.number_node_id,
                    np.expires_at,
                    CASE WHEN np.expires_at IS NULL THEN NULL
                         ELSE GREATEST(EXTRACT(EPOCH FROM (np.expires_at - NOW())), 0)::BIGINT
                    END,
                    np.max_admissions,
                    (SELECT COUNT(*) FROM number_admissions a
                      WHERE a.number_node_id = np.number_node_id),
                    (np.expires_at IS NOT NULL AND np.expires_at <= NOW())
               FROM number_policies np
              WHERE np.pial_id = $1",
    )
    .bind(pial)
    .fetch_all(pool)
    .await?;
    Ok(rows
        .into_iter()
        .map(
            |(
                number_node_id,
                expires_at,
                remaining_seconds,
                max_admissions,
                admissions,
                expired,
            )| {
                LeaseView {
                    number_node_id,
                    expires_at,
                    remaining_seconds,
                    max_admissions,
                    admissions,
                    state: lease_state(expired, max_admissions, admissions),
                }
            },
        )
        .collect())
}

// ── The sweeper ───────────────────────────────────────────────────────────────

/// How often expired Numbers are retired in the naming plane. Nothing depends
/// on this being fast: the decision path has already stopped admitting through
/// them, in SQL, against the same clock that stamped the lease. This only
/// releases the name.
const SWEEP_INTERVAL: Duration = Duration::from_secs(60);

/// Numbers retired per tick. A bound, so one enormous backlog cannot hold the
/// task inside a single iteration indefinitely.
const SWEEP_BATCH: i64 = 100;

/// One expired Number, and the identity that holds it.
struct Expired {
    number_node_id: Uuid,
    pial_id: Uuid,
}

/// Starts the sweeper.
///
/// It is deliberately not required for correctness. If it never runs, an
/// expired Number still refuses every new contact, because expiry is enforced
/// where the decision is taken. What the sweeper adds is that the NAME stops
/// resolving in Manhattan, so an expired Number becomes byte-for-byte the same
/// answer as a retired one all the way down the stack rather than only at the
/// policy layer.
pub fn spawn(pool: PgPool, manhattan: Manhattan) {
    if !manhattan.configured() {
        warn!(
            "MANHATTAN_URL is not set: expired Numbers will keep refusing new contact \
             at the decision, but their names will not be retired in the naming plane"
        );
        return;
    }
    info!(
        interval_secs = SWEEP_INTERVAL.as_secs(),
        batch = SWEEP_BATCH,
        "number lease sweeper started"
    );
    tokio::spawn(async move {
        let mut ticker = tokio::time::interval(SWEEP_INTERVAL);
        ticker.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
        loop {
            ticker.tick().await;
            match sweep_once(&pool, &manhattan).await {
                Ok(0) => {}
                Ok(n) => info!(retired = n, "number lease sweep"),
                Err(e) => error!(error = %e, "number lease sweep"),
            }
        }
    });
}

/// Retires one batch of expired Numbers, returning how many were released.
async fn sweep_once(pool: &PgPool, manhattan: &Manhattan) -> Result<usize> {
    let rows: Vec<(Uuid, Uuid)> = sqlx::query_as(
        "SELECT number_node_id, pial_id
           FROM number_policies
          WHERE expires_at IS NOT NULL AND expires_at <= NOW()
          ORDER BY expires_at
          LIMIT $1",
    )
    .bind(SWEEP_BATCH)
    .fetch_all(pool)
    .await?;
    if rows.is_empty() {
        return Ok(0);
    }
    let expired: Vec<Expired> = rows
        .into_iter()
        .map(|(number_node_id, pial_id)| Expired {
            number_node_id,
            pial_id,
        })
        .collect();

    // Group by identity: the Number's own string lives in Manhattan and this
    // brain deliberately keeps no copy of it, so it is looked up one identity
    // at a time rather than one Number at a time.
    let mut retired = 0usize;
    let mut identities: Vec<Uuid> = expired.iter().map(|e| e.pial_id).collect();
    identities.sort_unstable();
    identities.dedup();

    for pial in identities {
        let node = match manhattan
            .ensure_node(
                manhattan_client::KIND_IDENTITY,
                &manhattan_client::pial_name(&pial.to_string()),
            )
            .await
        {
            Ok(n) => n,
            Err(e) => {
                warn!(%pial, error = %e, "number lease sweep: identity node");
                continue;
            }
        };
        let listed = match manhattan.list_numbers(&node.node_id).await {
            Ok(l) => l,
            Err(e) => {
                warn!(%pial, error = %e, "number lease sweep: listing Numbers");
                continue;
            }
        };
        for target in expired.iter().filter(|e| e.pial_id == pial) {
            let Some(entry) = listed
                .iter()
                .find(|l| l.number_node_id == target.number_node_id.to_string())
            else {
                // Manhattan does not list it: the name is already gone, so the
                // policy row is the only thing left to clear.
                forget(pool, target.number_node_id, pial).await?;
                continue;
            };
            if entry.status == "active" {
                match manhattan.revoke_number(&entry.number).await {
                    Ok(()) => {}
                    // Already retired by the owner between the list and this
                    // call, or gone entirely. Both are the end state this sweep
                    // wanted, so the policy row may be forgotten.
                    Err(ManhattanError::NotFound)
                    | Err(ManhattanError::Conflict(ConflictCode::NumberAlreadyRevoked)) => {}
                    Err(e) => {
                        warn!(%pial, error = %e, "number lease sweep: retiring an expired Number");
                        // Leave the policy row in place. Removing it while the
                        // name is still live would drop the Number back to the
                        // identity's DEFAULT policy — an expiry that reopened
                        // the door it exists to close.
                        continue;
                    }
                }
            }
            forget(pool, target.number_node_id, pial).await?;
            retired += 1;
        }
    }
    Ok(retired)
}

/// Drops a retired Number's policy row. Its admission ledger goes with it
/// through the foreign key: the `number` namespace is not reusable, so this
/// Number can never be minted again and its ledger has nothing left to answer.
async fn forget(pool: &PgPool, number_node: Uuid, pial: Uuid) -> Result<()> {
    sqlx::query("DELETE FROM number_policies WHERE number_node_id = $1 AND pial_id = $2")
        .bind(number_node)
        .bind(pial)
        .execute(pool)
        .await?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn facts() -> ContactFacts {
        ContactFacts {
            via_number: true,
            follows: false,
            mutual: false,
            granted: false,
            capability_ok: false,
        }
    }

    // ── Label ────────────────────────────────────────────────────────────────

    #[test]
    fn a_label_keeps_the_words_the_owner_typed() {
        assert_eq!(sanitise_label("ACL Conference 2026"), "ACL Conference 2026");
        assert_eq!(sanitise_label("  Design 101 class  "), "Design 101 class");
        assert_eq!(sanitise_label("a\t\tb\n\nc"), "a b c");
        assert_eq!(sanitise_label(""), "");
        assert_eq!(sanitise_label("   "), "");
    }

    #[test]
    fn a_label_never_carries_a_control_character() {
        let dirty = "ACL\u{0}Conf\u{7}erence\u{1b}[31m";
        let clean = sanitise_label(dirty);
        assert!(
            !clean.chars().any(|c| c.is_control()),
            "control survived: {clean:?}"
        );
        assert_eq!(clean, "ACLConference[31m");
    }

    #[test]
    fn a_label_never_carries_a_direction_override_or_a_zero_width_character() {
        // A right-to-left override beside a Number can visually reorder the
        // symbols the owner reads, so the owner retires the wrong Number.
        for c in [
            '\u{202E}', '\u{202D}', '\u{202A}', '\u{2066}', '\u{2069}', '\u{200B}', '\u{200E}',
            '\u{200F}', '\u{FEFF}', '\u{061C}',
        ] {
            let clean = sanitise_label(&format!("ACL{c}2026"));
            assert_eq!(clean, "ACL2026", "survived: U+{:04X}", c as u32);
        }
    }

    #[test]
    fn a_label_is_cut_by_characters_and_never_through_a_utf8_sequence() {
        let long = "é".repeat(LABEL_MAX_CHARS * 3);
        let clean = sanitise_label(&long);
        assert_eq!(clean.chars().count(), LABEL_MAX_CHARS);
        // A byte cut would have produced invalid UTF-8; getting here at all
        // proves it did not, and the character count proves it was not cut
        // short either.
        assert_eq!(clean, "é".repeat(LABEL_MAX_CHARS));
    }

    #[test]
    fn a_label_never_ends_on_the_space_that_truncation_exposed() {
        let raw = format!("{} tail", "x".repeat(LABEL_MAX_CHARS));
        let clean = sanitise_label(&raw);
        assert_eq!(clean, "x".repeat(LABEL_MAX_CHARS));
        assert!(!clean.ends_with(' '));
    }

    // ── Validation ───────────────────────────────────────────────────────────

    #[test]
    fn a_lease_must_be_between_a_minute_and_a_year() {
        assert!(validate_lease_seconds(0).is_err());
        assert!(validate_lease_seconds(59).is_err());
        assert!(validate_lease_seconds(-86_400).is_err());
        assert!(validate_lease_seconds(MIN_LEASE_SECONDS).is_ok());
        assert!(validate_lease_seconds(7 * 24 * 60 * 60).is_ok());
        assert!(validate_lease_seconds(MAX_LEASE_SECONDS).is_ok());
        assert!(validate_lease_seconds(MAX_LEASE_SECONDS + 1).is_err());
    }

    #[test]
    fn a_budget_must_admit_at_least_one_person_and_stay_sane() {
        assert!(validate_budget(0).is_err());
        assert!(validate_budget(-1).is_err());
        assert!(validate_budget(1).is_ok());
        assert!(validate_budget(40).is_ok());
        assert!(validate_budget(MAX_BUDGET).is_ok());
        assert!(validate_budget(MAX_BUDGET + 1).is_err());
    }

    // ── Lease state ──────────────────────────────────────────────────────────

    #[test]
    fn no_lease_and_no_budget_is_a_live_number_forever() {
        assert_eq!(lease_state(false, None, 0), LeaseState::Live);
        assert_eq!(lease_state(false, None, 10_000_000), LeaseState::Live);
        assert!(lease_state(false, None, 0).admits());
    }

    #[test]
    fn a_budget_exhausts_exactly_at_its_cap_and_never_before() {
        assert_eq!(lease_state(false, Some(40), 39), LeaseState::Live);
        assert_eq!(lease_state(false, Some(40), 40), LeaseState::Exhausted);
        // Defensive: a count above the cap is still exhausted, never wrapped
        // back to live.
        assert_eq!(lease_state(false, Some(40), 41), LeaseState::Exhausted);
        assert!(!lease_state(false, Some(1), 1).admits());
    }

    #[test]
    fn expiry_outranks_a_budget_that_still_has_room() {
        assert_eq!(lease_state(true, Some(40), 0), LeaseState::Expired);
        assert_eq!(lease_state(true, None, 0), LeaseState::Expired);
        assert!(!lease_state(true, None, 0).admits());
    }

    #[test]
    fn expired_and_exhausted_both_stop_admitting() {
        for s in [LeaseState::Expired, LeaseState::Exhausted] {
            assert!(!s.admits(), "{s:?} must not admit");
        }
        assert!(LeaseState::Live.admits());
    }

    // ── What spends budget ───────────────────────────────────────────────────

    #[test]
    fn a_stranger_spends_budget_because_the_number_is_what_let_them_in() {
        let f = facts();
        assert!(!already_a_contact(&f));
        assert!(spends_budget("allow", true, &f));
    }

    #[test]
    fn a_mutual_follow_spends_nothing_because_a_contact_never_needed_the_number() {
        // On F33D3R a contact IS a mutual follow. Someone who already reaches
        // the owner freely must not be able to drain a conference Number, and
        // presenting it must be a no-op rather than an error.
        let mut f = facts();
        f.follows = true;
        f.mutual = true;
        assert!(already_a_contact(&f));
        assert!(!spends_budget("allow", true, &f));
    }

    #[test]
    fn a_one_way_follower_is_not_a_contact_and_does_spend() {
        // The platform's own definition: a contact is the MUTUAL. A Number that
        // introduces somebody the owner does not follow back did the work the
        // budget exists to count.
        let mut f = facts();
        f.follows = true;
        assert!(!already_a_contact(&f));
        assert!(spends_budget("allow", true, &f));
    }

    #[test]
    fn a_standing_grant_and_a_redeemed_link_both_spend_nothing() {
        let mut granted = facts();
        granted.granted = true;
        let mut redeemed = facts();
        redeemed.capability_ok = true;
        for f in [&granted, &redeemed] {
            assert!(already_a_contact(f));
            assert!(!spends_budget("allow", true, f));
        }
    }

    // ── The denial-of-service defence ────────────────────────────────────────

    #[test]
    fn nobody_who_was_not_admitted_can_spend_a_unit_of_budget() {
        // The threat is the screenshot: the moment a Number is on a slide it is
        // public. If merely ASKING moved the counter, any stranger could empty a
        // conference Number before the conference started. So every outcome that
        // is not an admission must cost exactly nothing — whatever the
        // relationship, and however many times it is repeated.
        let mut f = facts();
        for decision in ["request", "deny"] {
            for (follows, mutual, granted, capability_ok) in [
                (false, false, false, false),
                (true, false, false, false),
                (true, true, false, false),
                (false, false, true, false),
                (false, false, false, true),
            ] {
                f.follows = follows;
                f.mutual = mutual;
                f.granted = granted;
                f.capability_ok = capability_ok;
                assert!(
                    !spends_budget(decision, true, &f),
                    "decision {decision} must spend nothing"
                );
            }
        }
    }

    #[test]
    fn the_handle_path_never_touches_a_numbers_budget() {
        assert!(!spends_budget("allow", false, &facts()));
    }

    // ── Telling the owner ────────────────────────────────────────────────────

    #[test]
    fn a_number_with_no_budget_never_warns_about_one() {
        for remaining in [0i64, 1, 1_000, i64::MAX] {
            assert_eq!(budget_alert(None, remaining), BudgetAlert::None);
        }
        assert_eq!(BudgetAlert::None.as_str(), None);
    }

    /// The property the whole warning rests on: walking a budget down one
    /// admission at a time fires "low" exactly once and "spent" exactly once. A
    /// warning that repeated would train the owner to ignore it, and one that
    /// never fired would let a Number stop working in silence.
    #[test]
    fn each_warning_fires_exactly_once_as_a_budget_is_spent() {
        for max in [4i32, 12, 20, DEFAULT_BUDGET, 100, MAX_BUDGET] {
            let mut lows = 0usize;
            let mut spents = 0usize;
            for admitted in 1..=i64::from(max) {
                match budget_alert(Some(max), i64::from(max) - admitted) {
                    BudgetAlert::Low => lows += 1,
                    BudgetAlert::Spent => spents += 1,
                    BudgetAlert::None => {}
                }
            }
            assert_eq!(lows, 1, "budget {max} warned {lows} times that it was low");
            assert_eq!(
                spents, 1,
                "budget {max} announced exhaustion {spents} times"
            );
        }
    }

    #[test]
    fn a_budget_too_small_to_run_low_only_announces_that_it_is_spent() {
        // A budget of one goes from full to spent in a single admission; there
        // is no "running low" to report and inventing one would be a lie.
        for max in [1i32, 2, 3] {
            assert_eq!(budget_alert(Some(max), 0), BudgetAlert::Spent);
            for remaining in 1..i64::from(max) {
                assert_eq!(budget_alert(Some(max), remaining), BudgetAlert::None);
            }
        }
    }

    #[test]
    fn exhaustion_outranks_running_low() {
        // Defensive: a count past the cap is spent, never wrapped back to a
        // warning.
        assert_eq!(budget_alert(Some(4), 0), BudgetAlert::Spent);
        assert_eq!(budget_alert(Some(4), -3), BudgetAlert::Spent);
        assert_eq!(budget_alert(Some(4), 1), BudgetAlert::Low);
    }

    /// The default is a policy decision with consequences, so it is pinned here:
    /// it must be a real cap the owner can actually reach, well inside the
    /// column's bounds, and large enough to carry the biggest room any of the
    /// owner's stated use cases puts a Number in front of.
    #[test]
    fn the_default_budget_is_a_real_bound_the_stated_use_cases_fit_inside() {
        assert!(validate_budget(DEFAULT_BUDGET).is_ok());
        assert!(
            DEFAULT_BUDGET >= 40,
            "a class or a conference room must fit inside the default"
        );
        assert!(
            DEFAULT_BUDGET < MAX_BUDGET / 10,
            "a default this close to the maximum is not a bound at all"
        );
        // And it must be able to warn before it stops: a default so small that
        // "running low" and "spent" are the same admission would give the owner
        // no notice at all.
        assert!(DEFAULT_BUDGET / BUDGET_LOW_DIVISOR >= 1);
    }
}
