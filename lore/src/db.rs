use anyhow::Result;
use sqlx::PgPool;

use crate::models::{AchievementDef, UserAchievement, UserAchievementView, UserXP, XPStatus};

// XP tier thresholds — ordered ascending
const TIERS: &[(&str, i32)] = &[
    ("Civilian",    0),
    ("Creator",     500),
    ("Merchant",    1_500),
    ("Scout",       4_000),
    ("Journalist",  8_000),
    ("Archivist",   15_000),
    ("Broadcaster", 25_000),
    ("Veteran",     50_000),
    ("Elite",       100_000),
    ("Mythic",      250_000),
];

pub fn tier_for_xp(xp: i32) -> &'static str {
    let mut tier = TIERS[0].0;
    for &(name, threshold) in TIERS {
        if xp >= threshold { tier = name; } else { break; }
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
                xp_percent = if range > 0 { (progress * 100 / range).clamp(0, 100) } else { 100 };
            }
            break;
        }
    }

    XPStatus { total_xp, current_tier, next_tier, xp_to_next, xp_percent }
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

CREATE TABLE IF NOT EXISTS user_achievements (
    earn_id           UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    pial_shard_id     VARCHAR(64)  NOT NULL,
    achievement_id    VARCHAR(50)  REFERENCES achievement_definitions(achievement_id),
    earned_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    context_data      JSONB,
    UNIQUE (pial_shard_id, achievement_id)
);
CREATE INDEX IF NOT EXISTS idx_ua_pial ON user_achievements(pial_shard_id);

CREATE TABLE IF NOT EXISTS user_xp (
    pial_shard_id     VARCHAR(64)  PRIMARY KEY,
    total_xp          INT          NOT NULL DEFAULT 0,
    current_tier      VARCHAR(20)  NOT NULL DEFAULT 'Civilian',
    updated_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS user_progress (
    pial_shard_id     VARCHAR(64)  NOT NULL,
    achievement_id    VARCHAR(50)  NOT NULL,
    current_value     INT          NOT NULL DEFAULT 0,
    target_value      INT          NOT NULL,
    updated_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    PRIMARY KEY (pial_shard_id, achievement_id)
);
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
    sqlx::raw_sql(SEED).execute(pool).await?;
    Ok(())
}

pub async fn list_achievements(pool: &PgPool) -> Result<Vec<AchievementDef>> {
    let rows = sqlx::query_as::<_, AchievementDef>(
        "SELECT achievement_id, category, name, description, icon_emoji, rarity, xp_reward, \
                is_secret, is_repeatable, unlock_condition, created_at \
         FROM achievement_definitions ORDER BY category, rarity DESC, name"
    ).fetch_all(pool).await?;
    Ok(rows)
}

pub async fn user_achievements(pool: &PgPool, pial_shard_id: &str) -> Result<Vec<UserAchievementView>> {
    let rows = sqlx::query_as::<_, (String, String, String, String, Option<String>, String, i32, bool, Option<chrono::DateTime<chrono::Utc>>, Option<i32>, Option<i32>)>(
        "SELECT d.achievement_id, d.category, d.name, d.description, d.icon_emoji, d.rarity, \
                d.xp_reward, d.is_secret, ua.earned_at, up.current_value, up.target_value \
         FROM achievement_definitions d \
         LEFT JOIN user_achievements ua ON ua.achievement_id = d.achievement_id AND ua.pial_shard_id = $1 \
         LEFT JOIN user_progress up ON up.achievement_id = d.achievement_id AND up.pial_shard_id = $1 \
         ORDER BY d.category, ua.earned_at DESC NULLS LAST"
    ).bind(pial_shard_id).fetch_all(pool).await?;

    Ok(rows.into_iter().map(|(id, cat, name, desc, icon, rarity, xp, secret, earned, prog_cur, prog_tgt)| {
        UserAchievementView {
            achievement_id: id, category: cat, name, description: desc,
            icon_emoji: icon.unwrap_or_default(), rarity, xp_reward: xp, is_secret: secret,
            earned_at: earned, progress_current: prog_cur, progress_target: prog_tgt,
        }
    }).collect())
}

pub async fn award_achievement(pool: &PgPool, pial_shard_id: &str, achievement_id: &str) -> Result<Option<i32>> {
    // Check if already earned
    let existing: Option<(uuid::Uuid,)> = sqlx::query_as(
        "SELECT earn_id FROM user_achievements WHERE pial_shard_id = $1 AND achievement_id = $2"
    ).bind(pial_shard_id).bind(achievement_id).fetch_optional(pool).await?;

    if existing.is_some() { return Ok(None); }

    // Get XP reward
    let xp_reward: Option<(i32,)> = sqlx::query_as(
        "SELECT xp_reward FROM achievement_definitions WHERE achievement_id = $1"
    ).bind(achievement_id).fetch_optional(pool).await?;

    let xp = xp_reward.map(|r| r.0).unwrap_or(0);

    // Insert achievement
    sqlx::query(
        "INSERT INTO user_achievements (pial_shard_id, achievement_id) VALUES ($1, $2) ON CONFLICT DO NOTHING"
    ).bind(pial_shard_id).bind(achievement_id).execute(pool).await?;

    // Upsert XP
    sqlx::query(
        "INSERT INTO user_xp (pial_shard_id, total_xp, current_tier) VALUES ($1, $2, 'Civilian') \
         ON CONFLICT (pial_shard_id) DO UPDATE SET total_xp = user_xp.total_xp + $2, updated_at = NOW()"
    ).bind(pial_shard_id).bind(xp).execute(pool).await?;

    Ok(Some(xp))
}

pub async fn get_user_xp(pool: &PgPool, pial_shard_id: &str) -> Result<UserXP> {
    let row: Option<UserXP> = sqlx::query_as::<_, UserXP>(
        "SELECT pial_shard_id, total_xp, current_tier, updated_at FROM user_xp WHERE pial_shard_id = $1"
    ).bind(pial_shard_id).fetch_optional(pool).await?;

    Ok(row.unwrap_or(UserXP {
        pial_shard_id: pial_shard_id.to_string(),
        total_xp: 0,
        current_tier: "Civilian".to_string(),
        updated_at: chrono::Utc::now(),
    }))
}
