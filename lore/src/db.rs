use anyhow::Result;
use sqlx::PgPool;

use crate::models::{AchievementDef, UserAchievementView, UserXP, XPStatus};

// XP tier thresholds — ordered ascending
const TIERS: &[(&str, i32)] = &[
    ("Civilian", 0),
    ("Creator", 500),
    ("Merchant", 1_500),
    ("Scout", 4_000),
    ("Journalist", 8_000),
    ("Archivist", 15_000),
    ("Broadcaster", 25_000),
    ("Veteran", 50_000),
    ("Elite", 100_000),
    ("Mythic", 250_000),
];

pub fn tier_for_xp(xp: i32) -> &'static str {
    let mut tier = TIERS[0].0;
    for &(name, threshold) in TIERS {
        if xp >= threshold {
            tier = name;
        } else {
            break;
        }
    }
    tier
}

pub fn xp_status(total_xp: i32) -> XPStatus {
    let current_tier = tier_for_xp(total_xp).to_string();
    let mut next_tier = None;
    let mut xp_to_next = 0;
    let mut xp_percent = 100;

    for i in 0..TIERS.len() {
        if TIERS[i].0 == current_tier {
            if i + 1 < TIERS.len() {
                next_tier = Some(TIERS[i + 1].0.to_string());
                let current_floor = TIERS[i].1;
                let next_floor = TIERS[i + 1].1;
                xp_to_next = next_floor - total_xp;
                let range = next_floor - current_floor;
                let progress = total_xp - current_floor;
                xp_percent = if range > 0 {
                    (progress * 100 / range).clamp(0, 100)
                } else {
                    100
                };
            }
            break;
        }
    }

    XPStatus {
        total_xp,
        current_tier,
        next_tier,
        xp_to_next,
        xp_percent,
    }
}

const SCHEMA: &str = r#"
CREATE EXTENSION IF NOT EXISTS "pgcrypto";

CREATE TABLE IF NOT EXISTS achievement_definitions (
    achievement_id    VARCHAR(50)  PRIMARY KEY,
    category          VARCHAR(30)  NOT NULL,
    name              VARCHAR(100) NOT NULL,
    description       TEXT         NOT NULL,
    icon_emoji        VARCHAR(10),
    rarity            VARCHAR(20)  NOT NULL DEFAULT 'common',
    xp_reward         INT          NOT NULL DEFAULT 0,
    is_secret         BOOL         NOT NULL DEFAULT false,
    is_repeatable     BOOL         NOT NULL DEFAULT false,
    unlock_condition  JSONB        NOT NULL DEFAULT '{}',
    created_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

-- ── identity_name ────────────────────────────────────────────────────────────
-- Progression is keyed on a NAME, never on another brain's row and never on a
-- bare identifier whose namespace has to be guessed from its length. The value
-- is always the canonical 'pial:<uuid>', produced by identity::resolve and by
-- nothing else.
--
-- The column these tables used to carry, pial_shard_id, was the platform's
-- second word for the same concept: hex(sha256(pial_id || ':nexus-pial-shard-v1')),
-- a blinded alias of one identity. An alias is a name, so it belongs in the
-- naming plane rather than in a column in twelve databases.

CREATE TABLE IF NOT EXISTS user_achievements (
    earn_id           UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    identity_name     TEXT         NOT NULL,
    achievement_id    VARCHAR(50)  REFERENCES achievement_definitions(achievement_id),
    earned_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    context_data      JSONB
);

CREATE TABLE IF NOT EXISTS user_xp (
    identity_name     TEXT         NOT NULL,
    total_xp          INT          NOT NULL DEFAULT 0,
    current_tier      VARCHAR(20)  NOT NULL DEFAULT 'Civilian',
    updated_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS user_progress (
    identity_name     TEXT         NOT NULL,
    achievement_id    VARCHAR(50)  NOT NULL,
    current_value     INT          NOT NULL DEFAULT 0,
    target_value      INT          NOT NULL,
    updated_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

-- ── pial_shard_id → identity_name ────────────────────────────────────────────
-- A database created before the naming plane still holds the old column. Each
-- value becomes a name in the namespace it actually belongs to: a PIAL uuid is
-- a 'pial:' name, a 64-hex NEXUS shard is a 'shard:' name. A shard is NOT
-- rewritten into a 'pial:' name — sha256 does not run backwards, and inventing
-- an identity for one would credit one person's XP to another.
--
-- Anything that is neither shape cannot be named, so the migration stops. A row
-- nobody can resolve is not something to move on from quietly.
DO $mig$
DECLARE
    tbl      TEXT;
    unnamed  BIGINT;
BEGIN
    FOREACH tbl IN ARRAY ARRAY['user_achievements', 'user_xp', 'user_progress'] LOOP
        CONTINUE WHEN NOT EXISTS (
            SELECT 1 FROM information_schema.columns
             WHERE table_schema = current_schema()
               AND table_name   = tbl
               AND column_name  = 'pial_shard_id');

        EXECUTE format('ALTER TABLE %I ADD COLUMN IF NOT EXISTS identity_name TEXT', tbl);

        EXECUTE format(
            'SELECT COUNT(*) FROM %I
              WHERE identity_name IS NULL
                AND pial_shard_id !~* $re$^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$$re$
                AND pial_shard_id !~* $re$^[0-9a-f]{64}$$re$', tbl)
        INTO unnamed;

        IF unnamed > 0 THEN
            RAISE EXCEPTION
                'lore: % row(s) in % hold a pial_shard_id that is neither a PIAL uuid nor a 64-hex NEXUS shard; refusing to guess an identity for them',
                unnamed, tbl;
        END IF;

        EXECUTE format(
            'UPDATE %I
                SET identity_name = CASE
                        WHEN pial_shard_id ~* $re$^[0-9a-f]{64}$$re$ THEN ''shard:'' || lower(pial_shard_id)
                        ELSE ''pial:'' || lower(pial_shard_id)
                    END
              WHERE identity_name IS NULL', tbl);

        EXECUTE format('ALTER TABLE %I DROP COLUMN pial_shard_id CASCADE', tbl);
        EXECUTE format('ALTER TABLE %I ALTER COLUMN identity_name SET NOT NULL', tbl);
    END LOOP;
END
$mig$;

-- Uniqueness is carried by indexes rather than inline constraints so that a
-- freshly created table and a migrated one converge on exactly one shape.
CREATE UNIQUE INDEX IF NOT EXISTS uq_user_achievements_identity
    ON user_achievements(identity_name, achievement_id);
CREATE INDEX IF NOT EXISTS idx_user_achievements_identity
    ON user_achievements(identity_name);
CREATE UNIQUE INDEX IF NOT EXISTS uq_user_xp_identity
    ON user_xp(identity_name);
CREATE UNIQUE INDEX IF NOT EXISTS uq_user_progress_identity
    ON user_progress(identity_name, achievement_id);
"#;

// ── Manhattan outbox ─────────────────────────────────────────────────────────
// The durable handoff to the naming plane, matching
// feed-engine/internal/db/migrations/0005_manhattan_outbox.sql row for row.
//
// Manhattan is a separate brain reached over HTTP. A registration sent
// fire-and-forget is a registration a restart, a timeout or a rolling deploy
// silently loses — and a naming plane that is silently missing rows is worse
// than no naming plane, because everything downstream trusts it.
//
// So the handoff is transactional. These rows are written by triggers on the
// tables they describe, inside the same transaction as the row that caused
// them. Either someone has XP AND their identity registration is queued, or
// neither happened. src/manhattan_outbox.rs delivers them in order.
//
// Payloads carry NAMES, never node ids. Manhattan assigns node ids; this side
// does not know them and must not learn them, or the two planes acquire a
// second shared identifier and we are back where we started.
const MANHATTAN_OUTBOX: &str = r#"
CREATE TABLE IF NOT EXISTS manhattan_outbox (
    id              BIGSERIAL   PRIMARY KEY,
    op              TEXT        NOT NULL CHECK (op IN ('node', 'name', 'edge', 'revoke_name')),
    payload         JSONB       NOT NULL,
    -- dedup_key makes enqueue idempotent, so a replayed insert or a backfill
    -- cannot queue the same registration twice.
    dedup_key       TEXT        NOT NULL UNIQUE,
    attempts        INT         NOT NULL DEFAULT 0,
    last_error      TEXT,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    delivered_at    TIMESTAMPTZ
);

-- The drain's only query: undelivered, due, oldest first.
CREATE INDEX IF NOT EXISTS idx_manhattan_outbox_pending
    ON manhattan_outbox(next_attempt_at, id) WHERE delivered_at IS NULL;

-- ── Quarantine ───────────────────────────────────────────────────────────────
-- Head-of-line blocking is correct while a row can still land: later rows depend
-- on earlier ones, and running ahead would write edges pointing at nodes that do
-- not exist. It stops being correct the moment a row cannot land at all — then
-- it is not ordering the queue, it is ending it. One unfixable row used to stop
-- this brain's entire naming-plane output indefinitely, with a log line as the
-- only signal and a hand-written migration as the only cure.
--
-- A quarantined row is neither dropped nor delivered. It keeps its payload, its
-- error and its id; the drain simply stops claiming it, so everything queued
-- behind it moves again. It represents a naming-plane write that did NOT happen,
-- so it is reported as an incident on every tick until a person clears
-- blocked_at — loud by construction rather than by anyone remembering to look.
--
--   what is stuck:  SELECT id, op, blocked_at, blocked_reason, payload
--                     FROM manhattan_outbox
--                    WHERE blocked_at IS NOT NULL AND delivered_at IS NULL
--                    ORDER BY id;
--   put one back:   UPDATE manhattan_outbox
--                      SET blocked_at = NULL, blocked_reason = NULL,
--                          attempts = 0, next_attempt_at = NOW()
--                    WHERE id = $1;
--
-- Returning a row to the queue re-enters it at its original id, so the ordering
-- the drain depends on survives the round trip.
ALTER TABLE manhattan_outbox ADD COLUMN IF NOT EXISTS blocked_at     TIMESTAMPTZ;
ALTER TABLE manhattan_outbox ADD COLUMN IF NOT EXISTS blocked_reason TEXT;

-- The drain's query is now "undelivered, not quarantined, due, oldest first", so
-- it gets its own partial index. The one above stays: the backlog counters still
-- ask for everything undelivered, quarantined rows included, because a write
-- that did not happen must not disappear from the depth a health check reports.
CREATE INDEX IF NOT EXISTS idx_manhattan_outbox_deliverable
    ON manhattan_outbox(next_attempt_at, id)
 WHERE delivered_at IS NULL AND blocked_at IS NULL;

-- What is quarantined, read on every drain tick.
CREATE INDEX IF NOT EXISTS idx_manhattan_outbox_blocked
    ON manhattan_outbox(id) WHERE blocked_at IS NOT NULL AND delivered_at IS NULL;

CREATE OR REPLACE FUNCTION manhattan_enqueue(p_op TEXT, p_dedup TEXT, p_payload JSONB)
RETURNS VOID AS $fn$
BEGIN
    INSERT INTO manhattan_outbox (op, dedup_key, payload)
    VALUES (p_op, p_dedup, p_payload)
    ON CONFLICT (dedup_key) DO NOTHING;
END;
$fn$ LANGUAGE plpgsql;

-- ── identities ───────────────────────────────────────────────────────────────
-- Lore registers the identity node it is about to award XP to, and nothing
-- else. Any brain may CREATE a node — registration has to work whoever
-- encounters someone first — but ownership is decided by Manhattan from the
-- node's kind, so the node this queues is owned by elohim-veni the moment it
-- exists. Lore never writes a fact about it.
--
-- 'pial:' is a DERIVED namespace (Manhattan migration 0004): its value is
-- computed from the entity, every brain that computes it agrees, and there is
-- no decision to make — so whichever brain meets someone first may bind it.
-- That is exactly why this registration is lore's to queue.
--
-- Only 'pial:' names produce a registration. A 'shard:' name is an alias of an
-- identity that already exists under its PIAL; minting a node for it would be
-- the exact failure the naming plane exists to prevent — two nodes for one
-- person, with their XP split between them. Nor may lore BIND a shard onto the
-- identity node: 'shard' governs no namespace authority, so Manhattan falls
-- back to node ownership, identity nodes belong to elohim-veni, and the answer
-- is 403. The drain stops on first failure, so a single such row would block
-- every registration queued behind it. Lore resolves shards; it never writes
-- them.
CREATE OR REPLACE FUNCTION lore_manhattan_register_identity() RETURNS TRIGGER AS $fn$
BEGIN
    IF NEW.identity_name IS NULL OR NEW.identity_name NOT LIKE 'pial:%' THEN
        RETURN NEW;
    END IF;

    PERFORM manhattan_enqueue(
        'node',
        'node:' || NEW.identity_name,
        jsonb_build_object(
            'kind',      'identity',
            'name',      NEW.identity_name,
            'namespace', 'pial'));

    RETURN NEW;
END;
$fn$ LANGUAGE plpgsql;

-- A trigger per table, because registering is a property of the table and not a
-- step a future caller must remember.
DROP TRIGGER IF EXISTS trg_lore_manhattan_identity_ua ON user_achievements;
CREATE TRIGGER trg_lore_manhattan_identity_ua
    AFTER INSERT OR UPDATE OF identity_name ON user_achievements
    FOR EACH ROW EXECUTE FUNCTION lore_manhattan_register_identity();

DROP TRIGGER IF EXISTS trg_lore_manhattan_identity_xp ON user_xp;
CREATE TRIGGER trg_lore_manhattan_identity_xp
    AFTER INSERT OR UPDATE OF identity_name ON user_xp
    FOR EACH ROW EXECUTE FUNCTION lore_manhattan_register_identity();

DROP TRIGGER IF EXISTS trg_lore_manhattan_identity_progress ON user_progress;
CREATE TRIGGER trg_lore_manhattan_identity_progress
    AFTER INSERT OR UPDATE OF identity_name ON user_progress
    FOR EACH ROW EXECUTE FUNCTION lore_manhattan_register_identity();

-- ── backfill ─────────────────────────────────────────────────────────────────
-- Everyone lore already knows is enqueued once, so the naming plane starts
-- complete instead of only knowing about people who earn something after this
-- deploy. ON CONFLICT DO NOTHING in the helper makes re-running this harmless.
DO $backfill$
DECLARE r RECORD;
BEGIN
    FOR r IN
        SELECT identity_name FROM user_achievements
        UNION
        SELECT identity_name FROM user_xp
        UNION
        SELECT identity_name FROM user_progress
    LOOP
        CONTINUE WHEN r.identity_name IS NULL OR r.identity_name NOT LIKE 'pial:%';
        PERFORM manhattan_enqueue(
            'node',
            'node:' || r.identity_name,
            jsonb_build_object('kind','identity','name',r.identity_name,'namespace','pial'));
    END LOOP;
END
$backfill$;
"#;

// Achievement seed — idempotent (ON CONFLICT DO NOTHING)
const SEED: &str = r#"
INSERT INTO achievement_definitions (achievement_id, category, name, description, icon_emoji, rarity, xp_reward, is_secret, is_repeatable, unlock_condition) VALUES
-- ONBOARDING
('first_pulse','onboarding','First Pulse','Created account and entered F33D3R for the first time.','💓','common',50,false,false,'{"trigger":"user.registered"}'),
('signal_found','onboarding','Signal Found','Completed profile setup.','📡','common',100,false,false,'{"trigger":"profile.completed"}'),
('face_reveal','onboarding','Face Reveal','Uploaded first profile photo or avatar.','🪞','common',75,false,false,'{"trigger":"avatar.uploaded"}'),
('echo_online','onboarding','Echo Online','Verified email or phone.','✅','common',100,false,false,'{"trigger":"verification.completed"}'),
('wanderer','onboarding','Wanderer','Returned to F33D3R 7 days in a row.','🚶','uncommon',200,false,false,'{"trigger":"daily_login","streak":7}'),
-- SOCIAL
('first_contact','social','First Contact','Sent first message.','📩','common',50,false,false,'{"trigger":"message.sent","count":1}'),
('crowd_surfer','social','Crowd Surfer','Received 100 total reactions.','🏄','uncommon',300,false,false,'{"trigger":"reactions.received","count":100}'),
('main_character','social','Main Character','First post breaks platform engagement threshold.','⭐','rare',500,false,false,'{"trigger":"post.engagement","threshold":1000}'),
('summoned','social','Summoned','Tagged by another user for the first time.','👆','common',100,false,false,'{"trigger":"mention.received","count":1}'),
('algorithm_knows_you','social','The Algorithm Knows You','Appeared on trending or discovery feed.','🤖','rare',750,false,false,'{"trigger":"trending.appeared","count":1}'),
('ratiod','social','Ratio''d','Received more replies than likes on a post.','📊','uncommon',150,false,false,'{"trigger":"post.ratio"}'),
('villain_arc','social','Villain Arc','Lost major follower count in 24 hours.','😈','uncommon',200,false,false,'{"trigger":"follower_loss","count":100,"window_hours":24}'),
('celebrity_sighting','social','Celebrity Sighting','Interacted with verified or high-rank account.','🌟','uncommon',250,false,false,'{"trigger":"interaction.verified_account","count":1}'),
-- CREATOR
('mic_check','creator','Mic Check','Uploaded first audio or song.','🎤','common',150,false,false,'{"trigger":"audio.uploaded","count":1}'),
('directors_cut','creator','Director''s Cut','Uploaded first video.','🎬','common',150,false,false,'{"trigger":"video.uploaded","count":1}'),
('first_sale','creator','First Sale','Sold first digital item or content.','💰','uncommon',500,false,false,'{"trigger":"sale.completed","count":1}'),
('paid_creator','creator','Paid Creator','Earned first payout.','💸','uncommon',750,false,false,'{"trigger":"payout.received","count":1}'),
('independent_artist','creator','Independent Artist','Reached monetization threshold without sponsors.','🎨','rare',1000,false,false,'{"trigger":"monetization.organic"}'),
('cult_following','creator','Cult Following','Maintained high repeat audience ratio.','🔮','rare',1000,false,false,'{"trigger":"audience.repeat_ratio","threshold":0.6}'),
('studio_session','creator','Studio Session','Uploaded content consistently for 30 days.','🎵','rare',1500,false,false,'{"trigger":"content.upload_streak","days":30}'),
('world_tour','creator','World Tour','Content viewed in 10 or more countries.','🌍','epic',2000,false,false,'{"trigger":"content.countries","count":10}'),
-- MUSIC
('bedroom_producer','music','Bedroom Producer','Published first song.','🎹','common',200,false,false,'{"trigger":"song.published","count":1}'),
('aux_cord_certified','music','Aux Cord Certified','Song replayed repeatedly by listeners.','🔁','uncommon',400,false,false,'{"trigger":"song.replays","count":50}'),
('local_legend','music','Local Legend','Trending in one city or region.','📍','rare',800,false,false,'{"trigger":"music.trending_local","count":1}'),
('headliner','music','Headliner','Hit major platform stream milestone.','🎪','epic',2500,false,false,'{"trigger":"song.streams","count":10000}'),
('soundtrack_of_summer','music','Soundtrack of Summer','Seasonal viral music trend achievement.','☀️','legendary',5000,false,false,'{"trigger":"music.seasonal_trending"}'),
-- WELLNESS
('touch_grass','wellness','Touch Grass','Reduced screen time for consecutive days.','🌿','uncommon',300,false,false,'{"trigger":"session.reduced","days":3}'),
('logged_off','wellness','Logged Off','Stayed off platform for a healthy duration.','🛌','common',100,false,false,'{"trigger":"session.absent","hours":24}'),
('balance_patch','wellness','Balance Patch','Maintained healthy online/offline ratio.','⚖️','uncommon',400,false,false,'{"trigger":"session.balance"}'),
('night_shift','wellness','Night Shift','Used platform during late-night hours consistently.','🌙','common',150,false,false,'{"trigger":"session.late_night","days":5}'),
('doomscroll_survivor','wellness','Doomscroll Survivor','Exceeded unhealthy scrolling threshold.','📱','common',100,false,false,'{"trigger":"session.scroll_depth"}'),
('digital_detox','wellness','Digital Detox','Took voluntary cooldown from app.','🧘','rare',500,false,false,'{"trigger":"session.voluntary_pause","hours":72}'),
-- CULTURE
('curiosity_killed_cat','culture','Curiosity Killed the Cat','Entered adult content section for first time.','🐱','common',100,false,false,'{"trigger":"nsfw.section.entered","count":1}'),
('after_dark','culture','After Dark','Active in adult content after midnight.','🌑','common',100,false,false,'{"trigger":"nsfw.late_night","count":1}'),
('incognito_failure','culture','Incognito Failure','Returned repeatedly to same creator or category.','🕵️','uncommon',200,false,false,'{"trigger":"content.repeat_visit","count":10}'),
('bonk','culture','Bonk','Triggered platform content detection systems.','🔨','uncommon',150,false,false,'{"trigger":"zodacare.flag.soft","count":1}'),
('post_nut_clarity','culture','Post-Nut Clarity','Immediately logged off after adult session.','💭','uncommon',200,false,false,'{"trigger":"nsfw.session_then_logout"}'),
('down_astronomical','culture','Down Astronomical','Excessive late-night time in adult content.','📉','uncommon',200,false,false,'{"trigger":"nsfw.late_night_duration","hours":3}'),
('grass_avoider','culture','Grass Avoider','Excessive consecutive adult-content browsing session.','🪴','uncommon',200,false,false,'{"trigger":"nsfw.consecutive_session","hours":2}'),
('chronically_online','culture','Chronically Online','Extreme weekly activity.','💻','common',200,false,false,'{"trigger":"session.weekly_hours","threshold":40}'),
('keyboard_warrior','culture','Keyboard Warrior','Entered excessive debate threads.','⌨️','common',150,false,false,'{"trigger":"replies.debate_thread","count":20}'),
('deleted_in_5','culture','Deleted in 5 Minutes','Removed embarrassing post quickly.','🗑️','common',100,false,false,'{"trigger":"post.deleted_fast","seconds":300}'),
('screenshot_immortalized','culture','Screenshot Immortalized','Post archived or shared widely.','📸','rare',750,false,false,'{"trigger":"post.external_shares","count":100}'),
('the_receipts','culture','The Receipts','Uploaded proof during drama or conflict.','🧾','uncommon',300,false,false,'{"trigger":"media.uploaded_in_thread","count":1}'),
('npc_behavior','culture','NPC Behavior','Repeated identical interactions excessively.','🤖','uncommon',200,false,false,'{"trigger":"interaction.repeated","count":50}'),
('lore_drop','culture','Lore Drop','Revealed major personal backstory.','📖','uncommon',350,false,false,'{"trigger":"post.long_form_personal","count":1}'),
-- REAL LIFE
('survivor','real_life','Survivor','Documented surviving natural disaster or crisis.','🌊','epic',2000,false,false,'{"trigger":"verified_badge.survivor"}'),
('veteran_rl','real_life','Veteran','Verified military service.','🎖️','epic',2000,false,false,'{"trigger":"verified_badge.veteran"}'),
('embedded','real_life','Embedded','Journalist reporting from conflict zone.','📰','epic',2500,false,false,'{"trigger":"verified_badge.embedded_journalist"}'),
('builder','real_life','Builder','Started verified business or project.','🏗️','rare',1000,false,false,'{"trigger":"verified_badge.business"}'),
('graduate','real_life','Graduate','Completed educational milestone.','🎓','uncommon',500,false,false,'{"trigger":"verified_badge.graduate"}'),
('passport_stamped','real_life','Passport Stamped','Verified international travel milestone.','✈️','uncommon',400,false,false,'{"trigger":"content.countries_visited","count":5}'),
-- COMMUNITY
('founding_member','community','Founding Member','Early adopter before public growth.','🏛️','legendary',5000,false,false,'{"trigger":"user.early_adopter"}'),
('guildmaster','community','Guildmaster','Owns successful community or group.','👑','epic',2000,false,false,'{"trigger":"community.owner_threshold","members":100}'),
('raid_leader','community','Raid Leader','Organized large event, stream, or community push.','⚔️','epic',1500,false,false,'{"trigger":"event.organized","participants":50}'),
('peacekeeper','community','Peacekeeper','Resolved moderation disputes successfully.','🕊️','rare',1000,false,false,'{"trigger":"moderation.dispute_resolved","count":5}'),
-- ECONOMY
('bread_winner','economy','Bread Winner','Earned first real income on platform.','🍞','uncommon',500,false,false,'{"trigger":"payout.first_real","count":1}'),
('whale','economy','Whale','Spent large amount supporting creators.','🐳','rare',1000,false,false,'{"trigger":"spending.total"}'),
('tip_jar_hero','economy','Tip Jar Hero','Supported many small creators.','💝','uncommon',400,false,false,'{"trigger":"tips.sent_to_unique_creators","count":20}'),
('hustler','economy','Hustler','Maintained monthly monetization streak.','💼','rare',1500,false,false,'{"trigger":"monetization.monthly_streak","months":3}'),
('black_market_energy','economy','Black Market Energy','Frequently traded rare digital items.','🖤','epic',2000,false,false,'{"trigger":"trades.rare_items","count":10}'),
-- SECRET
('ghost_in_machine','secret','Ghost in the Machine','Triggered hidden system event.','👻','legendary',5000,true,false,'{"trigger":"system.hidden_event"}'),
('chosen_one','secret','Chosen One','Extremely low unlock rate achievement.','⚡','mythic',10000,true,false,'{"trigger":"system.chosen","probability":0.0001}'),
('forbidden_knowledge','secret','Forbidden Knowledge','Found hidden feature or page.','📚','legendary',5000,true,false,'{"trigger":"navigation.hidden_page"}'),
('developer_is_watching','secret','Developer Is Watching','Direct interaction with official staff or system.','👁️','legendary',7500,true,false,'{"trigger":"staff.interaction"}')
ON CONFLICT (achievement_id) DO NOTHING;
"#;

pub async fn migrate(pool: &PgPool) -> Result<()> {
    sqlx::raw_sql(SCHEMA).execute(pool).await?;
    // The outbox and its triggers come after the tables they hang off, because
    // the triggers name identity_name and the block above is what puts it there.
    sqlx::raw_sql(MANHATTAN_OUTBOX).execute(pool).await?;
    sqlx::raw_sql(SEED).execute(pool).await?;
    Ok(())
}

pub async fn list_achievements(pool: &PgPool) -> Result<Vec<AchievementDef>> {
    let rows = sqlx::query_as::<_, AchievementDef>(
        "SELECT achievement_id, category, name, description, icon_emoji, rarity, xp_reward, \
                is_secret, is_repeatable, unlock_condition, created_at \
         FROM achievement_definitions ORDER BY category, rarity DESC, name",
    )
    .fetch_all(pool)
    .await?;
    Ok(rows)
}

/// `identity_name` is always the canonical `pial:<uuid>` returned by
/// `identity::resolve`. Nothing else may be passed here.
pub async fn user_achievements(
    pool: &PgPool,
    identity_name: &str,
) -> Result<Vec<UserAchievementView>> {
    let rows = sqlx::query_as::<_, (String, String, String, String, Option<String>, String, i32, bool, Option<chrono::DateTime<chrono::Utc>>, Option<i32>, Option<i32>)>(
        "SELECT d.achievement_id, d.category, d.name, d.description, d.icon_emoji, d.rarity, \
                d.xp_reward, d.is_secret, ua.earned_at, up.current_value, up.target_value \
         FROM achievement_definitions d \
         LEFT JOIN user_achievements ua ON ua.achievement_id = d.achievement_id AND ua.identity_name = $1 \
         LEFT JOIN user_progress up ON up.achievement_id = d.achievement_id AND up.identity_name = $1 \
         ORDER BY d.category, ua.earned_at DESC NULLS LAST"
    ).bind(identity_name).fetch_all(pool).await?;

    Ok(rows
        .into_iter()
        .map(
            |(id, cat, name, desc, icon, rarity, xp, secret, earned, prog_cur, prog_tgt)| {
                UserAchievementView {
                    achievement_id: id,
                    category: cat,
                    name,
                    description: desc,
                    icon_emoji: icon.unwrap_or_default(),
                    rarity,
                    xp_reward: xp,
                    is_secret: secret,
                    earned_at: earned,
                    progress_current: prog_cur,
                    progress_target: prog_tgt,
                }
            },
        )
        .collect())
}

/// What awarding actually did. Three outcomes rather than an Option, so a
/// caller naming an achievement that does not exist is told so instead of being
/// handed a silent zero-XP success.
#[derive(Debug)]
pub enum AwardOutcome {
    Awarded(i32),
    AlreadyEarned,
    UnknownAchievement,
}

/// Award an achievement and its XP in ONE transaction.
///
/// The grant, the XP it carries and the Manhattan identity registration the
/// trigger enqueues all commit together or not at all. That is the whole point
/// of the outbox: there is no window in which someone has XP that the naming
/// plane was never told about.
///
/// The insert itself decides whether this is a new grant. Reading first and
/// writing after left a window where two concurrent awards both saw nothing,
/// both inserted (one a no-op) and both added the XP.
pub async fn award_achievement(
    pool: &PgPool,
    identity_name: &str,
    achievement_id: &str,
) -> Result<AwardOutcome> {
    let mut tx = pool.begin().await?;

    let definition: Option<(i32,)> =
        sqlx::query_as("SELECT xp_reward FROM achievement_definitions WHERE achievement_id = $1")
            .bind(achievement_id)
            .fetch_optional(&mut *tx)
            .await?;

    let xp = match definition {
        Some(row) => row.0,
        None => {
            tx.rollback().await?;
            return Ok(AwardOutcome::UnknownAchievement);
        }
    };

    let inserted: Option<(uuid::Uuid,)> = sqlx::query_as(
        "INSERT INTO user_achievements (identity_name, achievement_id) VALUES ($1, $2) \
         ON CONFLICT (identity_name, achievement_id) DO NOTHING RETURNING earn_id",
    )
    .bind(identity_name)
    .bind(achievement_id)
    .fetch_optional(&mut *tx)
    .await?;

    if inserted.is_none() {
        tx.rollback().await?;
        return Ok(AwardOutcome::AlreadyEarned);
    }

    sqlx::query(
        "INSERT INTO user_xp (identity_name, total_xp, current_tier) VALUES ($1, $2, 'Civilian') \
         ON CONFLICT (identity_name) DO UPDATE SET total_xp = user_xp.total_xp + $2, updated_at = NOW()"
    ).bind(identity_name).bind(xp).execute(&mut *tx).await?;

    tx.commit().await?;
    Ok(AwardOutcome::Awarded(xp))
}

pub async fn get_user_xp(pool: &PgPool, identity_name: &str) -> Result<UserXP> {
    let row: Option<UserXP> = sqlx::query_as::<_, UserXP>(
        "SELECT identity_name AS identity, total_xp, current_tier, updated_at \
         FROM user_xp WHERE identity_name = $1",
    )
    .bind(identity_name)
    .fetch_optional(pool)
    .await?;

    Ok(row.unwrap_or(UserXP {
        identity: identity_name.to_string(),
        total_xp: 0,
        current_tier: "Civilian".to_string(),
        updated_at: chrono::Utc::now(),
    }))
}
