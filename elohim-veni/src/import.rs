//! `elohim-veni import` — the one-shot move of identity into f33d3r_security.
//!
//! Reads, each from one consistent snapshot (REPEATABLE READ, READ ONLY):
//!   FEED_DATABASE_URL (f33d3r_feed):  pial_roots, users, pial_account_bindings,
//!       user_credentials (password + TOTP), backup_codes, pial_capabilities,
//!       trust_scores, pial_event_ledger
//!   REGISTRY_DATABASE_URL (f33d3r_handles): handles, handle_events,
//!       handle_auctions, auction_bids, reserved_handles
//! and writes DATABASE_URL (f33d3r_security) in ONE transaction:
//!
//!   1. stage every source row in temporary tables;
//!   2. check the staged data against every rule this database enforces, and
//!      stop with a report naming each problem — nothing is written;
//!   3. merge, every statement ON CONFLICT, so a re-run converges instead of
//!      duplicating;
//!   4. announce every identity on identity.events, so feed-engine's
//!      projection is built from what was actually committed;
//!   5. verify row counts against the staged sources, and stop if any differ;
//!   6. open genesis (account creation), then commit.
//!
//! Run it before feed-engine starts sending signups here: until it commits,
//! POST /v1/accounts answers 503 identity_plane_sealed.

use anyhow::{bail, Context, Result};
use serde_json::{json, Value};
use sqlx::{PgConnection, PgPool, Postgres, Transaction};
use uuid::Uuid;

use crate::identity::{events, vocab};

const PAGE: i64 = 5000;

struct Stage {
    name: &'static str,
    /// A query over the source returning exactly `columns`, with a total order.
    source: &'static str,
    /// Column definitions, shared by the temp table and jsonb_to_recordset.
    columns: &'static str,
}

const FEED_STAGES: &[Stage] = &[
    Stage {
        name: "stage_pial_roots",
        source: "SELECT pial_id, date_of_birth, dob_locked, age_verified, is_adult, is_minor, kyc_tier,
                        kyc_verified_at, role, status, is_tombstoned, deleted_at, created_at
                   FROM pial_roots ORDER BY pial_id",
        columns: "pial_id uuid, date_of_birth date, dob_locked boolean, age_verified boolean,
                  is_adult boolean, is_minor boolean, kyc_tier text, kyc_verified_at timestamptz,
                  role text, status text, is_tombstoned boolean, deleted_at timestamptz,
                  created_at timestamptz",
    },
    Stage {
        name: "stage_users",
        source: "SELECT id, handle, pial_id, deactivated_at, deactivated_reason, created_at
                   FROM users ORDER BY id",
        columns: "id uuid, handle text, pial_id uuid, deactivated_at timestamptz,
                  deactivated_reason text, created_at timestamptz",
    },
    Stage {
        name: "stage_bindings",
        source: "SELECT account_id, pial_id, status, is_primary, bound_at
                   FROM pial_account_bindings ORDER BY id",
        columns: "account_id uuid, pial_id uuid, status text, is_primary boolean, bound_at timestamptz",
    },
    Stage {
        name: "stage_credentials",
        source: "SELECT user_id, password_hash, totp_secret, totp_enabled, updated_at
                   FROM user_credentials ORDER BY user_id",
        columns: "user_id uuid, password_hash text, totp_secret text, totp_enabled boolean,
                  updated_at timestamptz",
    },
    Stage {
        name: "stage_backup_codes",
        source: "SELECT id, user_id, code_hash, used, created_at FROM backup_codes ORDER BY id",
        columns: "id uuid, user_id uuid, code_hash text, used boolean, created_at timestamptz",
    },
    Stage {
        name: "stage_caps",
        source: "SELECT pial_id, capability, state, expires_at, reason, granted_by, created_at, updated_at
                   FROM pial_capabilities ORDER BY pial_id, capability",
        columns: "pial_id uuid, capability text, state text, expires_at timestamptz, reason text,
                  granted_by text, created_at timestamptz, updated_at timestamptz",
    },
    Stage {
        name: "stage_trust",
        source: "SELECT pial_id, trust_level, nsfw_tier, enforcement_state, violation_count,
                        last_violation_at, cooldown_until, updated_at
                   FROM trust_scores ORDER BY pial_id",
        columns: "pial_id uuid, trust_level int, nsfw_tier int, enforcement_state text,
                  violation_count int, last_violation_at timestamptz, cooldown_until timestamptz,
                  updated_at timestamptz",
    },
    Stage {
        name: "stage_ledger",
        source: "SELECT id, pial_id, account_id, event_type, payload, source, created_at
                   FROM pial_event_ledger ORDER BY id",
        columns: "id uuid, pial_id uuid, account_id uuid, event_type text, payload jsonb,
                  source text, created_at timestamptz",
    },
];

const REGISTRY_STAGES: &[Stage] = &[
    Stage {
        name: "stage_handles",
        source: "SELECT id, handle::text AS handle, pial_id, status, tier, created_at, updated_at, last_bound_at
                   FROM handles ORDER BY id",
        columns: "id uuid, handle text, pial_id uuid, status text, tier text, created_at timestamptz,
                  updated_at timestamptz, last_bound_at timestamptz",
    },
    Stage {
        name: "stage_handle_events",
        source: "SELECT id, handle_id, event_type, from_pial_id, to_pial_id, metadata, created_at
                   FROM handle_events ORDER BY id",
        columns: "id uuid, handle_id uuid, event_type text, from_pial_id uuid, to_pial_id uuid,
                  metadata jsonb, created_at timestamptz",
    },
    Stage {
        name: "stage_auctions",
        source: "SELECT id, handle_id, start_time, end_time, starting_price_aet, current_price_aet,
                        highest_bidder_pial, status, created_at
                   FROM handle_auctions ORDER BY id",
        columns: "id uuid, handle_id uuid, start_time timestamptz, end_time timestamptz,
                  starting_price_aet int, current_price_aet int, highest_bidder_pial uuid,
                  status text, created_at timestamptz",
    },
    Stage {
        name: "stage_bids",
        source: "SELECT id, auction_id, bidder_pial, amount_aet, created_at FROM auction_bids ORDER BY id",
        columns: "id uuid, auction_id uuid, bidder_pial uuid, amount_aet int, created_at timestamptz",
    },
    Stage {
        name: "stage_reserved",
        source: "SELECT handle::text AS handle, reason FROM reserved_handles ORDER BY handle",
        columns: "handle text, reason text",
    },
];

async fn snapshot(pool: &PgPool) -> Result<Transaction<'static, Postgres>> {
    let mut tx = pool.begin().await?;
    sqlx::query("SET TRANSACTION ISOLATION LEVEL REPEATABLE READ, READ ONLY")
        .execute(&mut *tx)
        .await?;
    Ok(tx)
}

/// Copies one source query into a temp table on the target, page by page.
async fn stage(src: &mut PgConnection, dst: &mut PgConnection, s: &Stage) -> Result<i64> {
    sqlx::query(&format!("CREATE TEMP TABLE {} ({}) ON COMMIT DROP", s.name, s.columns))
        .execute(&mut *dst)
        .await
        .with_context(|| format!("creating {}", s.name))?;
    let mut offset = 0i64;
    loop {
        let page: Value = sqlx::query_scalar(&format!(
            "SELECT COALESCE(jsonb_agg(to_jsonb(t)), '[]'::jsonb)
               FROM ({} LIMIT $1 OFFSET $2) t",
            s.source
        ))
        .bind(PAGE)
        .bind(offset)
        .fetch_one(&mut *src)
        .await
        .with_context(|| format!("reading source for {}", s.name))?;
        let n = page.as_array().map(|a| a.len()).unwrap_or(0) as i64;
        if n == 0 {
            break;
        }
        sqlx::query(&format!(
            "INSERT INTO {} SELECT * FROM jsonb_to_recordset($1) AS r({})",
            s.name, s.columns
        ))
        .bind(page)
        .execute(&mut *dst)
        .await
        .with_context(|| format!("staging {}", s.name))?;
        offset += n;
        if n < PAGE {
            break;
        }
    }
    tracing::info!(stage = s.name, rows = offset, "staged");
    Ok(offset)
}

/// Each check is a query returning one text row per problem.
const CHECKS: &[(&str, &str)] = &[
    ("PIAL status outside active|suspended|tombstoned|purged",
     "SELECT pial_id || ': ' || status FROM stage_pial_roots
       WHERE status NOT IN ('active','suspended','tombstoned','purged')"),
    ("PIAL role outside user|admin|founder",
     "SELECT pial_id || ': ' || role FROM stage_pial_roots WHERE role NOT IN ('user','admin','founder')"),
    ("PIAL kyc_tier outside none|basic|full",
     "SELECT pial_id || ': ' || kyc_tier FROM stage_pial_roots WHERE kyc_tier NOT IN ('none','basic','full')"),
    ("PIAL both adult and minor",
     "SELECT pial_id::text FROM stage_pial_roots WHERE is_adult AND is_minor"),
    ("more than one founder in f33d3r_feed",
     "SELECT string_agg(pial_id::text, ', ') FROM stage_pial_roots WHERE role = 'founder' HAVING COUNT(*) > 1"),
    ("a different founder already holds the role in f33d3r_security",
     "SELECT p.pial_id || ' (security) vs ' || s.pial_id || ' (feed)'
        FROM pial_states p, stage_pial_roots s
       WHERE p.role = 'founder' AND s.role = 'founder' AND p.pial_id <> s.pial_id"),
    ("account handles that collide case-insensitively",
     "SELECT string_agg(handle || ' (' || id || ')', ', ') FROM stage_users
       GROUP BY lower(handle) HAVING COUNT(*) > 1"),
    ("account resolves to a PIAL that f33d3r_feed does not have",
     "SELECT a.id || ' -> ' || a.pial_id FROM import_accounts a
       WHERE a.pial_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM stage_pial_roots r WHERE r.pial_id = a.pial_id)"),
    ("account handle bound in the registry to a different PIAL",
     "SELECT a.handle || ': account ' || a.id || ' on ' || a.pial_id || ', registry on ' || h.pial_id
        FROM import_accounts a JOIN stage_handles h ON lower(h.handle) = lower(a.handle)
       WHERE a.pial_id IS NOT NULL AND h.pial_id <> a.pial_id"),
    ("registry handle already present here under another id",
     "SELECT s.handle || ': ' || s.id || ' vs ' || h.id FROM stage_handles s
        JOIN handles h ON h.handle = s.handle::citext WHERE h.id <> s.id"),
    ("account id already present here with another handle",
     "SELECT s.id || ': ' || s.handle || ' vs ' || a.handle FROM stage_users s
        JOIN accounts a ON a.id = s.id WHERE a.handle <> s.handle::citext"),
    ("capability outside the vocabulary",
     "SELECT pial_id || ': ' || capability FROM stage_caps WHERE capability NOT IN
        ('POSTING','NSFW_ACCESS','MONETIZATION','REALM_PROGRESSION','NEW_ACCOUNT_TRUST','MESSAGING','MUSIC_UPLOAD')"),
    ("capability state outside granted|restricted|revoked|cooldown",
     "SELECT pial_id || ': ' || capability || '=' || state FROM stage_caps
       WHERE state NOT IN ('granted','restricted','revoked','cooldown')"),
    ("cooldown without an end",
     "SELECT pial_id || ': ' || capability FROM stage_caps WHERE state = 'cooldown' AND expires_at IS NULL"),
    ("trust row out of range",
     "SELECT pial_id::text FROM stage_trust WHERE trust_level NOT BETWEEN 0 AND 100
         OR nsfw_tier NOT BETWEEN 0 AND 4
         OR enforcement_state NOT IN ('none','warning','restricted','suspended','terminated')"),
    ("password hash that is not bcrypt",
     "SELECT user_id::text FROM stage_credentials WHERE password_hash <> '' AND password_hash NOT LIKE '$2%'"),
    ("backup code hash that is not bcrypt",
     "SELECT id::text FROM stage_backup_codes WHERE code_hash NOT LIKE '$2%'"),
    ("credentials for an account that does not exist",
     "SELECT user_id::text FROM stage_credentials c WHERE NOT EXISTS (SELECT 1 FROM stage_users u WHERE u.id = c.user_id)"),
    ("registry handle status or tier outside the vocabulary",
     "SELECT handle || ': ' || status || '/' || tier FROM stage_handles
       WHERE status NOT IN ('active','inactive','frozen','quarantined','auction','reserved')
          OR tier NOT IN ('standard','premium','elite')"),
];

pub async fn run(target_url: &str) -> Result<()> {
    let feed_url = std::env::var("FEED_DATABASE_URL").context("FEED_DATABASE_URL must be set")?;
    let registry_url = std::env::var("REGISTRY_DATABASE_URL").context("REGISTRY_DATABASE_URL must be set")?;

    let target = PgPool::connect(target_url).await.context("connecting to DATABASE_URL")?;
    crate::db::migrate(&target).await?;
    let feed = PgPool::connect(&feed_url).await.context("connecting to FEED_DATABASE_URL")?;
    let registry = PgPool::connect(&registry_url).await.context("connecting to REGISTRY_DATABASE_URL")?;

    let mut tx = target.begin().await?;
    sqlx::query("SET LOCAL statement_timeout = 0").execute(&mut *tx).await?;
    sqlx::query("SET LOCAL elohim.identity_import = 'on'").execute(&mut *tx).await?;

    let mut staged = serde_json::Map::new();
    {
        let mut src = snapshot(&feed).await?;
        for s in FEED_STAGES {
            staged.insert(s.name.into(), json!(stage(&mut src, &mut tx, s).await?));
        }
        src.commit().await?;
    }
    {
        let mut src = snapshot(&registry).await?;
        for s in REGISTRY_STAGES {
            staged.insert(s.name.into(), json!(stage(&mut src, &mut tx, s).await?));
        }
        src.commit().await?;
    }

    // Which PIAL each account belongs to: its active binding (authoritative in
    // f33d3r_feed), else users.pial_id; and which of a PIAL's accounts is
    // primary: the earliest binding marked primary, else the earliest account.
    sqlx::raw_sql(
        "CREATE TEMP TABLE import_accounts ON COMMIT DROP AS
         SELECT u.id, u.handle, COALESCE(b.pial_id, u.pial_id) AS pial_id,
                COALESCE(b.bound_at, u.created_at) AS bound_at, u.created_at,
                u.deactivated_at, COALESCE(u.deactivated_reason, '') AS deactivated_reason,
                COALESCE(b.is_primary, FALSE) AS marked_primary
           FROM stage_users u
           LEFT JOIN stage_bindings b ON b.account_id = u.id AND b.status = 'active';",
    )
    .execute(&mut *tx)
    .await?;

    let mut problems: Vec<String> = Vec::new();
    for (what, sql) in CHECKS {
        let rows: Vec<Option<String>> = sqlx::query_scalar(sql).fetch_all(&mut *tx).await?;
        for row in rows.into_iter().flatten() {
            problems.push(format!("{what}: {row}"));
        }
    }
    if !problems.is_empty() {
        for p in &problems {
            tracing::error!(problem = %p, "import refused");
        }
        bail!(
            "import refused: {} problem(s) in the source data; nothing was written:\n  {}",
            problems.len(),
            problems.join("\n  ")
        );
    }

    merge(&mut tx).await?;
    let announced = announce(&mut tx).await?;
    verify(&mut tx).await?;

    sqlx::query(
        "INSERT INTO identity_genesis (opened_by, detail) VALUES ('import', $1)
         ON CONFLICT (singleton) DO NOTHING",
    )
    .bind(Value::Object(staged.clone()))
    .execute(&mut *tx)
    .await?;
    tx.commit().await?;
    let staged = Value::Object(staged);
    tracing::info!(staged = %staged, announced, "identity import committed; account creation is open");
    Ok(())
}

async fn merge(tx: &mut PgConnection) -> Result<()> {
    let steps: &[(&str, &str)] = &[
        ("pial facts", "
            INSERT INTO pial_states (pial_id, status, role, dob, dob_locked, age_verified, is_adult, is_minor,
                                     kyc_tier, kyc_verified_at, tombstoned_at, created_at)
            SELECT pial_id, st, role, date_of_birth, dob_locked, age_verified, is_adult, is_minor,
                   kyc_tier, kyc_verified_at,
                   CASE WHEN st IN ('tombstoned','purged') THEN COALESCE(deleted_at, created_at) END,
                   created_at
              FROM (SELECT *, CASE WHEN is_tombstoned AND status = 'active' THEN 'tombstoned' ELSE status END AS st
                      FROM stage_pial_roots) r
            ON CONFLICT (pial_id) DO UPDATE SET
                -- An identity ended in f33d3r_feed is ended; an enforcement
                -- applied here (a ban, a suspension) outlives the import.
                status = CASE WHEN EXCLUDED.status IN ('tombstoned','purged') THEN EXCLUDED.status
                              WHEN pial_states.status IN ('banned','suspended') THEN pial_states.status
                              ELSE EXCLUDED.status END,
                tombstoned_at = CASE WHEN EXCLUDED.status IN ('tombstoned','purged') THEN EXCLUDED.tombstoned_at
                                     WHEN pial_states.status IN ('banned','suspended') THEN NULL
                                     ELSE EXCLUDED.tombstoned_at END,
                role = EXCLUDED.role, dob = EXCLUDED.dob, dob_locked = EXCLUDED.dob_locked,
                age_verified = EXCLUDED.age_verified, is_adult = EXCLUDED.is_adult,
                is_minor = EXCLUDED.is_minor, kyc_tier = EXCLUDED.kyc_tier,
                kyc_verified_at = EXCLUDED.kyc_verified_at,
                created_at = LEAST(pial_states.created_at, EXCLUDED.created_at),
                updated_at = NOW()"),
        // An account f33d3r_feed never gave a PIAL takes the identity the
        // registrar granted its handle to, or gets one minted now: an account
        // without an identity cannot exist here. Deterministic, so a re-run
        // mints the same one.
        ("PIALs for unbound accounts: the registrar's", "
            UPDATE import_accounts a SET pial_id = h.pial_id
              FROM stage_handles h
             WHERE a.pial_id IS NULL AND lower(h.handle) = lower(a.handle)"),
        ("PIALs for unbound accounts: minted", "
            UPDATE import_accounts SET pial_id = md5('elohim-import-pial:' || id::text)::uuid
             WHERE pial_id IS NULL"),
        ("minted PIALs", "
            INSERT INTO pial_states (pial_id, created_at)
            SELECT pial_id, created_at FROM import_accounts a
             WHERE NOT EXISTS (SELECT 1 FROM stage_pial_roots r WHERE r.pial_id = a.pial_id)
            ON CONFLICT (pial_id) DO NOTHING"),
        ("trust", "
            UPDATE trust_scores t SET
                trust_level = s.trust_level, nsfw_tier = s.nsfw_tier,
                enforcement_state = s.enforcement_state,
                violation_count = GREATEST(t.violation_count, s.violation_count),
                last_violation_at = s.last_violation_at, cooldown_until = s.cooldown_until,
                updated_at = NOW()
              FROM stage_trust s WHERE s.pial_id = t.pial_id"),
        // A PIAL with capability rows in f33d3r_feed has exactly those rows.
        ("capabilities: drop rows feed does not have", "
            DELETE FROM pial_capabilities c
             WHERE c.pial_id IN (SELECT DISTINCT pial_id FROM stage_caps)
               AND NOT EXISTS (SELECT 1 FROM stage_caps s WHERE s.pial_id = c.pial_id AND s.capability = c.capability)"),
        ("capabilities", "
            INSERT INTO pial_capabilities (pial_id, capability, state, expires_at, reason, granted_by, created_at, updated_at)
            SELECT pial_id, capability, state, expires_at, COALESCE(reason,''), COALESCE(granted_by,'system'), created_at, updated_at
              FROM stage_caps
            ON CONFLICT (pial_id, capability) DO UPDATE SET
                state = EXCLUDED.state, expires_at = EXCLUDED.expires_at, reason = EXCLUDED.reason,
                granted_by = EXCLUDED.granted_by, updated_at = EXCLUDED.updated_at"),
        // One never seeded in f33d3r_feed is seeded now, as CheckCapability
        // would have on first use.
        ("default capabilities for unseeded PIALs", "
            INSERT INTO pial_capabilities (pial_id, capability, state, granted_by)
            SELECT p.pial_id, c, 'granted', 'system'
              FROM (SELECT pial_id FROM stage_pial_roots UNION SELECT pial_id FROM import_accounts) p
             CROSS JOIN unnest($1::text[]) AS c
             WHERE NOT EXISTS (SELECT 1 FROM stage_caps s WHERE s.pial_id = p.pial_id)
               AND NOT EXISTS (SELECT 1 FROM pial_capabilities x WHERE x.pial_id = p.pial_id)
            ON CONFLICT (pial_id, capability) DO NOTHING"),
        ("reserved handles", "
            INSERT INTO reserved_handles (handle, reason)
            SELECT handle::citext, reason FROM stage_reserved ON CONFLICT (handle) DO NOTHING"),
        ("registry handles", "
            INSERT INTO handles (id, handle, pial_id, status, tier, created_at, updated_at, last_bound_at)
            SELECT id, handle::citext, pial_id, status, tier, created_at, updated_at, last_bound_at FROM stage_handles
            ON CONFLICT (id) DO UPDATE SET
                pial_id = EXCLUDED.pial_id, status = EXCLUDED.status, tier = EXCLUDED.tier,
                updated_at = EXCLUDED.updated_at, last_bound_at = EXCLUDED.last_bound_at"),
        // Handles f33d3r_feed allocated before it asked the registrar.
        ("account handles the registry never granted", "
            INSERT INTO handles (handle, pial_id, status, tier, created_at, last_bound_at)
            SELECT handle::citext, pial_id, 'active', 'standard', created_at, created_at
              FROM import_accounts
            ON CONFLICT (handle) DO NOTHING"),
        ("registry handle events", "
            INSERT INTO handle_events (id, handle_id, event_type, from_pial_id, to_pial_id, metadata, created_at)
            SELECT id, handle_id, event_type, from_pial_id, to_pial_id, COALESCE(metadata,'{}'), created_at
              FROM stage_handle_events ON CONFLICT (id) DO NOTHING"),
        ("import events for handles the registry never granted", "
            INSERT INTO handle_events (id, handle_id, event_type, to_pial_id, metadata)
            SELECT md5('elohim-import-handle:' || h.id::text)::uuid, h.id, 'import', h.pial_id,
                   jsonb_build_object('source', 'f33d3r_feed.users', 'note', 'allocated before the registrar was asked')
              FROM handles h
             WHERE NOT EXISTS (SELECT 1 FROM stage_handles s WHERE s.id = h.id)
               AND EXISTS (SELECT 1 FROM import_accounts a WHERE a.handle::citext = h.handle)
            ON CONFLICT (id) DO NOTHING"),
        ("auctions", "
            INSERT INTO handle_auctions (id, handle_id, start_time, end_time, starting_price_aet,
                                         current_price_aet, highest_bidder_pial, status, created_at)
            SELECT id, handle_id, start_time, end_time, starting_price_aet, current_price_aet,
                   highest_bidder_pial, status, created_at
              FROM stage_auctions ON CONFLICT (id) DO NOTHING"),
        ("bids", "
            INSERT INTO auction_bids (id, auction_id, bidder_pial, amount_aet, created_at)
            SELECT id, auction_id, bidder_pial, amount_aet, created_at FROM stage_bids
            ON CONFLICT (id) DO NOTHING"),
        // Primary flags are cleared first so the one-primary index never sees
        // two at once while rows are rewritten.
        ("clear primaries", "
            UPDATE accounts SET is_primary = FALSE WHERE id IN (SELECT id FROM import_accounts)"),
        ("accounts", "
            INSERT INTO accounts (id, pial_id, handle, is_primary, bound_at, created_at, deactivated_at, deactivated_reason)
            SELECT id, pial_id, handle::citext,
                   ROW_NUMBER() OVER (PARTITION BY pial_id
                                      ORDER BY marked_primary DESC, bound_at, created_at, id) = 1,
                   bound_at, created_at, deactivated_at, deactivated_reason
              FROM import_accounts
            ON CONFLICT (id) DO UPDATE SET
                pial_id = EXCLUDED.pial_id, handle = EXCLUDED.handle, is_primary = EXCLUDED.is_primary,
                bound_at = EXCLUDED.bound_at, deactivated_at = EXCLUDED.deactivated_at,
                deactivated_reason = EXCLUDED.deactivated_reason"),
        ("passwords", "
            INSERT INTO credentials (account_id, password_hash, updated_at)
            SELECT user_id, password_hash, updated_at FROM stage_credentials WHERE password_hash <> ''
            ON CONFLICT (account_id) DO UPDATE SET password_hash = EXCLUDED.password_hash, updated_at = EXCLUDED.updated_at"),
        ("second factors", "
            INSERT INTO account_totp (account_id, secret, enabled, updated_at)
            SELECT user_id, totp_secret, totp_enabled, updated_at FROM stage_credentials WHERE totp_secret <> ''
            ON CONFLICT (account_id) DO UPDATE SET secret = EXCLUDED.secret, enabled = EXCLUDED.enabled,
                                                   updated_at = EXCLUDED.updated_at"),
        ("backup codes", "
            INSERT INTO backup_codes (id, account_id, code_hash, used, created_at)
            SELECT id, user_id, code_hash, used, created_at FROM stage_backup_codes
            ON CONFLICT (id) DO UPDATE SET used = EXCLUDED.used"),
        ("identity ledger", "
            INSERT INTO pial_event_ledger (id, pial_id, account_id, event_type, payload, source, created_at)
            SELECT id, pial_id, account_id, event_type, COALESCE(payload,'{}'), COALESCE(source,'system'), created_at
              FROM stage_ledger ON CONFLICT (id) DO NOTHING"),
        ("ledger: the import itself", "
            INSERT INTO pial_event_ledger (id, pial_id, event_type, payload, source)
            SELECT md5('elohim-import-ledger:' || p.pial_id::text)::uuid, p.pial_id, 'identity_imported',
                   jsonb_build_object('from', 'f33d3r_feed', 'minted_here',
                                      NOT EXISTS (SELECT 1 FROM stage_pial_roots r WHERE r.pial_id = p.pial_id)),
                   'import'
              FROM (SELECT pial_id FROM stage_pial_roots UNION SELECT pial_id FROM import_accounts) p
            ON CONFLICT (id) DO NOTHING"),
    ];
    let defaults: Vec<String> = vocab::DEFAULT_CAPABILITIES.iter().map(|c| c.to_string()).collect();
    for (what, sql) in steps {
        let mut q = sqlx::query(sql);
        if sql.contains("$1") {
            q = q.bind(&defaults);
        }
        let n = q
            .execute(&mut *tx)
            .await
            .with_context(|| format!("import step: {what}"))?
            .rows_affected();
        tracing::info!(step = what, rows = n, "merged");
    }
    // The deferred handle-binding check runs now rather than at commit, so a
    // failure names the step it belongs to.
    sqlx::query("SET CONSTRAINTS ALL IMMEDIATE")
        .execute(&mut *tx)
        .await
        .context("import: an imported account's handle is bound to another identity")?;
    Ok(())
}

/// Every imported identity on identity.events, so feed-engine's projection is
/// built from what this transaction committed — including PIALs minted here.
async fn announce(tx: &mut PgConnection) -> Result<usize> {
    let pials: Vec<Uuid> = sqlx::query_scalar(
        "SELECT pial_id FROM stage_pial_roots UNION SELECT pial_id FROM import_accounts",
    )
    .fetch_all(&mut *tx)
    .await?;
    for (i, pial_id) in pials.iter().enumerate() {
        events::emit_pial(tx, *pial_id, "identity.imported", json!({ "source": "f33d3r_feed" })).await?;
        if (i + 1) % 10_000 == 0 {
            tracing::info!(announced = i + 1, of = pials.len(), "announcing");
        }
    }
    Ok(pials.len())
}

/// Every staged source row must be here after the merge.
async fn verify(tx: &mut PgConnection) -> Result<()> {
    let checks: &[(&str, &str)] = &[
        ("PIALs", "SELECT (SELECT COUNT(*) FROM stage_pial_roots),
                          (SELECT COUNT(*) FROM stage_pial_roots s JOIN pial_states p USING (pial_id))"),
        ("accounts", "SELECT (SELECT COUNT(*) FROM stage_users),
                             (SELECT COUNT(*) FROM stage_users s JOIN accounts a ON a.id = s.id
                               AND a.handle = s.handle::citext)"),
        ("bindings", "SELECT (SELECT COUNT(*) FROM import_accounts),
                             (SELECT COUNT(*) FROM import_accounts i JOIN accounts a ON a.id = i.id AND a.pial_id = i.pial_id)"),
        ("passwords", "SELECT (SELECT COUNT(*) FROM stage_credentials WHERE password_hash <> ''),
                              (SELECT COUNT(*) FROM stage_credentials s JOIN credentials c ON c.account_id = s.user_id
                                WHERE s.password_hash <> '' AND c.password_hash = s.password_hash)"),
        ("second factors", "SELECT (SELECT COUNT(*) FROM stage_credentials WHERE totp_secret <> ''),
                                   (SELECT COUNT(*) FROM stage_credentials s JOIN account_totp t ON t.account_id = s.user_id
                                     WHERE s.totp_secret <> '' AND t.secret = s.totp_secret AND t.enabled = s.totp_enabled)"),
        ("backup codes", "SELECT (SELECT COUNT(*) FROM stage_backup_codes),
                                 (SELECT COUNT(*) FROM stage_backup_codes s JOIN backup_codes b USING (id))"),
        ("capabilities", "SELECT (SELECT COUNT(*) FROM stage_caps),
                                 (SELECT COUNT(*) FROM stage_caps s JOIN pial_capabilities c
                                   ON c.pial_id = s.pial_id AND c.capability = s.capability AND c.state = s.state)"),
        ("trust", "SELECT (SELECT COUNT(*) FROM stage_trust),
                          (SELECT COUNT(*) FROM stage_trust s JOIN trust_scores t ON t.pial_id = s.pial_id
                            AND t.trust_level = s.trust_level AND t.nsfw_tier = s.nsfw_tier
                            AND t.enforcement_state = s.enforcement_state)"),
        ("ledger", "SELECT (SELECT COUNT(*) FROM stage_ledger),
                           (SELECT COUNT(*) FROM stage_ledger s JOIN pial_event_ledger l USING (id))"),
        ("handles", "SELECT (SELECT COUNT(*) FROM stage_handles),
                            (SELECT COUNT(*) FROM stage_handles s JOIN handles h ON h.id = s.id
                              AND h.pial_id = s.pial_id AND h.status = s.status)"),
        ("handle events", "SELECT (SELECT COUNT(*) FROM stage_handle_events),
                                  (SELECT COUNT(*) FROM stage_handle_events s JOIN handle_events e USING (id))"),
        ("auctions", "SELECT (SELECT COUNT(*) FROM stage_auctions),
                             (SELECT COUNT(*) FROM stage_auctions s JOIN handle_auctions a USING (id))"),
        ("bids", "SELECT (SELECT COUNT(*) FROM stage_bids),
                         (SELECT COUNT(*) FROM stage_bids s JOIN auction_bids b USING (id))"),
        ("account handles in the registry", "SELECT (SELECT COUNT(*) FROM import_accounts),
                (SELECT COUNT(*) FROM import_accounts i JOIN handles h ON h.handle = i.handle::citext AND h.pial_id = i.pial_id)"),
        ("one primary per identity", "SELECT (SELECT COUNT(DISTINCT pial_id) FROM import_accounts),
                (SELECT COUNT(*) FROM accounts WHERE is_primary AND pial_id IN (SELECT pial_id FROM import_accounts))"),
    ];
    let mut failures = Vec::new();
    for (what, sql) in checks {
        let (want, got): (i64, i64) = sqlx::query_as(sql)
            .fetch_one(&mut *tx)
            .await
            .with_context(|| format!("verifying {what}"))?;
        tracing::info!(check = what, source = want, imported = got, "verified");
        if want != got {
            failures.push(format!("{what}: source {want}, imported {got}"));
        }
    }
    if !failures.is_empty() {
        bail!("import verification failed; rolled back:\n  {}", failures.join("\n  "));
    }
    Ok(())
}
