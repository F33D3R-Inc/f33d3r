package db

import (
	"database/sql"
	"log"
	"strings"
	"time"

	"github.com/f33d3r/feed-engine/internal/model"
)

// RecordFollowerEvent logs a follow or unfollow for history-based achievement checks.
func RecordFollowerEvent(database *sql.DB, actorID, targetID, eventType string) {
	database.Exec(
		`INSERT INTO follower_events (actor_id, target_id, event_type) VALUES ($1, $2, $3)`,
		actorID, targetID, eventType,
	)
}

// IncrementProfileViews atomically bumps the view counter and returns the new total.
func IncrementProfileViews(database *sql.DB, userID string) int64 {
	var n int64
	database.QueryRow(
		`UPDATE user_profiles SET profile_views = profile_views + 1 WHERE user_id = $1 RETURNING profile_views`,
		userID,
	).Scan(&n)
	return n
}

// DeactivateUser sets deactivated_at on the user record and expires all their sessions.
func DeactivateUser(database *sql.DB, userID, reason string) error {
	_, err := database.Exec(
		`UPDATE users SET deactivated_at = NOW(), deactivated_reason = $2 WHERE id = $1`,
		userID, reason,
	)
	if err != nil {
		return err
	}
	database.Exec(`DELETE FROM user_sessions WHERE user_id = $1`, userID)
	return nil
}

// IsDeactivated returns true if the user account has been deactivated.
func IsDeactivated(database *sql.DB, userID string) bool {
	var n int
	database.QueryRow(
		`SELECT COUNT(*) FROM users WHERE id = $1 AND deactivated_at IS NOT NULL`, userID,
	).Scan(&n)
	return n > 0
}

// ── Troll achievement helpers ────────────────────────────────────────────────

// countQuery runs a scalar COUNT. A failure degrades to 0 so a best-effort
// achievement check cannot break the write that triggered it, but is never silent.
func countQuery(database *sql.DB, q string, args ...interface{}) int {
	var n int
	if err := database.QueryRow(q, args...).Scan(&n); err != nil {
		log.Printf("[achievements] count failed: %v — query: %s", err, strings.Join(strings.Fields(q), " "))
		return 0
	}
	return n
}

// TryAwardTrollOnPost checks all post-creation troll achievements for userID.
// postID is the newly created work ID, body is its text content.
func TryAwardTrollOnPost(database *sql.DB, userID, pialID, postID, body string) {
	// Wall Of Text Warrior — single post >= 5000 chars
	if len([]rune(body)) >= 5000 {
		AwardAchievement(database, userID, "troll_wall_of_text")
	}

	// Off The Meds — 100 posts between 02:00–04:59 UTC
	lateNight := countQuery(database,
		`SELECT COUNT(*) FROM works WHERE author_pial = $1
		 AND EXTRACT(HOUR FROM created_at AT TIME ZONE 'UTC') BETWEEN 2 AND 4
		 AND deleted_at IS NULL`, pialID)
	if lateNight >= 100 {
		AwardAchievement(database, userID, "troll_off_the_meds")
	}

	// Conspiracy Department — 50 posts containing a hashtag
	hashtagPosts := countQuery(database,
		`SELECT COUNT(*) FROM works WHERE author_pial = $1 AND body LIKE '%#%' AND deleted_at IS NULL`, pialID)
	if hashtagPosts >= 50 {
		AwardAchievement(database, userID, "troll_conspiracy_dept")
	}

	// Paranoid Legend — used word "bot" in 100 posts
	botPosts := countQuery(database,
		`SELECT COUNT(*) FROM works WHERE author_pial = $1 AND body ILIKE '%bot%' AND deleted_at IS NULL`, pialID)
	if botPosts >= 100 {
		AwardAchievement(database, userID, "troll_paranoid_legend")
	}

	// Gaslight Specialist — 50+ replies in threads you didn't start
	replyOtherThreads := countQuery(database,
		`SELECT COUNT(*) FROM works w
		 LEFT JOIN works root ON root.cid = w.root_cid
		 WHERE w.author_pial = $1 AND w.kind = 'reply'
		   AND (root.id IS NULL OR root.author_pial != $1)
		   AND w.deleted_at IS NULL`, pialID)
	if replyOtherThreads >= 50 {
		AwardAchievement(database, userID, "troll_gaslight_specialist")
	}

	// Bad Faith Actor — 10+ replies by you in a single thread
	maxRepliesInThread := countQuery(database,
		`SELECT COALESCE(MAX(rc),0) FROM (
			SELECT COUNT(*) AS rc FROM works
			WHERE author_pial = $1 AND kind = 'reply' AND deleted_at IS NULL
			GROUP BY root_cid
		) t`, pialID)
	if maxRepliesInThread >= 10 {
		AwardAchievement(database, userID, "troll_bad_faith_actor")
	}
	if maxRepliesInThread >= 15 {
		AwardAchievement(database, userID, "troll_goalpost_olympics")
	}
	if maxRepliesInThread >= 20 {
		AwardAchievement(database, userID, "troll_feigning_ignorance")
	}

	// Weaponized Autism — reply to a post that is 2+ years old
	if postID != "" {
		var parentAge int
		database.QueryRow(
			`SELECT COALESCE(EXTRACT(EPOCH FROM (NOW() - p.created_at))::int, 0)
			 FROM works w JOIN works p ON p.cid = w.parent_cid
			 WHERE w.id = $1::uuid`, postID,
		).Scan(&parentAge)
		if parentAge >= 63072000 { // 2 years in seconds
			AwardAchievement(database, userID, "troll_weaponized_autism")
		}

		// Archive Diver — reply to a post 1+ year old
		if parentAge >= 31536000 {
			AwardAchievement(database, userID, "troll_archive_diver")
		}
	}

	// Digital Cigarette Smoker — 50+ replies between 23:00–03:59 UTC
	lateReplies := countQuery(database,
		`SELECT COUNT(*) FROM works
		 WHERE author_pial = $1 AND kind = 'reply' AND deleted_at IS NULL
		   AND (EXTRACT(HOUR FROM created_at AT TIME ZONE 'UTC') >= 23
		     OR EXTRACT(HOUR FROM created_at AT TIME ZONE 'UTC') <= 3)`, pialID)
	if lateReplies >= 50 {
		AwardAchievement(database, userID, "troll_digital_cigarette")
	}
}

// TryAwardTrollOnReplyReceived checks achievements triggered when a reply arrives on a post you authored.
// authorID = post author's userID, replyBody = the reply text, postID = the parent work ID.
func TryAwardTrollOnReplyReceived(database *sql.DB, authorID, authorPIAL, postID, replyBody string) {
	// I Own You — someone writes a 500+ char reply to your post
	if len([]rune(replyBody)) >= 500 {
		AwardAchievement(database, authorID, "troll_i_own_you")
	}

	// Master Baiter — 100 unique repliers across all your posts
	uniqueRepliers := countQuery(database,
		`SELECT COUNT(DISTINCT w.author_pial)
		 FROM works w
		 JOIN works parent ON parent.cid = w.parent_cid
		 WHERE parent.author_pial = $1
		   AND w.author_pial != $1
		   AND w.kind = 'reply' AND w.deleted_at IS NULL`, authorPIAL)
	if uniqueRepliers >= 100 {
		AwardAchievement(database, authorID, "troll_master_baiter")
	}

	// Demon Time — 1000 replies to one post within 60 minutes
	recentReplies := countQuery(database,
		`SELECT COUNT(*) FROM works
		 WHERE kind = 'reply' AND parent_id = $1::uuid
		   AND created_at >= NOW() - INTERVAL '60 minutes'`, postID)
	if recentReplies >= 1000 {
		AwardAchievement(database, authorID, "troll_demon_time")
	}

	// Crashout Inducer — thread you started has 50+ total replies
	totalThreadReplies := countQuery(database,
		`SELECT COUNT(*) FROM works w
		 WHERE w.root_cid = (SELECT cid FROM works WHERE id = $1::uuid)
		   AND w.kind = 'reply' AND w.deleted_at IS NULL`, postID)
	if totalThreadReplies >= 50 {
		AwardAchievement(database, authorID, "troll_crashout_inducer")
	}

	// Hallucination Engine — 50 replies to one post from users who never engaged you before
	newEngagers := countQuery(database,
		`SELECT COUNT(DISTINCT w.author_pial)
		 FROM works w
		 WHERE w.kind = 'reply' AND w.parent_id = $1::uuid
		   AND w.author_pial != $2
		   AND w.deleted_at IS NULL
		   AND w.author_pial NOT IN (
			   SELECT DISTINCT w2.author_pial FROM works w2
			   WHERE w2.kind = 'reply'
			     AND w2.parent_id IN (
				     SELECT id FROM works WHERE author_pial = $2 AND id != $1::uuid
			     )
			   AND w2.deleted_at IS NULL
		   )`, postID, authorPIAL)
	if newEngagers >= 50 {
		AwardAchievement(database, authorID, "troll_hallucination_engine")
	}

	// Forum Demon — replied in 10+ trending threads today (check from replier's perspective — called for author of reply)
	// This is checked in TryAwardTrollOnPost for the reply author instead.

	// Rent Free — mentioned by someone you've never interacted with
	// (approximated via notifications — checked in TryAwardTrollOnMention)
}

// TryAwardTrollOnPostStats checks engagement-count achievements on a post.
// Called after like/dislike/repost counts change on postID.
func TryAwardTrollOnPostStats(database *sql.DB, authorID, authorPIAL, postID string) {
	var likes, dislikes, reposts, impressions int
	database.QueryRow(
		`SELECT COALESCE(likes,0), COALESCE(dislikes,0), COALESCE(reposts,0), COALESCE(impressions,0)
		 FROM work_reactions_summary WHERE work_id = $1::uuid`, postID,
	).Scan(&likes, &dislikes, &reposts, &impressions)
	// Fallback: derive from work_reactions directly if summary view absent
	if likes == 0 && dislikes == 0 {
		database.QueryRow(
			`SELECT
				COUNT(*) FILTER (WHERE reaction_type = 'like'),
				COUNT(*) FILTER (WHERE reaction_type = 'dislike'),
				COUNT(*) FILTER (WHERE reaction_type = 'repost')
			 FROM work_reactions WHERE work_id = $1::uuid`, postID,
		).Scan(&likes, &dislikes, &reposts)
		// Impressions live on works.view_count (there is no work_metrics table).
		database.QueryRow(
			`SELECT COALESCE(view_count,0) FROM works WHERE id = $1::uuid`, postID,
		).Scan(&impressions)
	}

	total := likes + dislikes
	// Nuclear Take — 90%+ negative ratio with 100+ total reactions, post not deleted
	if total >= 100 && dislikes*100/total >= 90 {
		var deleted bool
		database.QueryRow(`SELECT deleted_at IS NOT NULL FROM works WHERE id = $1::uuid`, postID).Scan(&deleted)
		if !deleted {
			AwardAchievement(database, authorID, "troll_nuclear_take")
		}
	}

	// Reverse Ratio — 100+ dislikes AND 100+ likes on same post
	if likes >= 100 && dislikes >= 100 {
		AwardAchievement(database, authorID, "troll_reverse_ratio")
	}

	// Chaos God — 500+ likes AND 500+ dislikes
	if likes >= 500 && dislikes >= 500 {
		AwardAchievement(database, authorID, "troll_chaos_god")
	}

	// Infinite Aura Loss — a reply you authored gets 100+ dislikes
	var replyDislikes int
	database.QueryRow(
		`SELECT COUNT(*) FROM work_reactions wr
		 JOIN works w ON w.id = wr.work_id
		 WHERE wr.work_id = $1::uuid AND wr.reaction_type = 'dislike'`, postID,
	).Scan(&replyDislikes)
	var isReply bool
	database.QueryRow(`SELECT kind = 'reply' FROM works WHERE id = $1::uuid`, postID).Scan(&isReply)
	if isReply && replyDislikes >= 100 {
		AwardAchievement(database, authorID, "troll_infinite_aura_loss")
	}

	// The Clip Farm — single post reposted 50+ times
	if reposts == 0 {
		database.QueryRow(
			`SELECT COUNT(*) FROM work_reactions WHERE work_id = $1::uuid AND reaction_type = 'repost'`, postID,
		).Scan(&reposts)
	}
	if reposts >= 50 {
		AwardAchievement(database, authorID, "troll_clip_farm")
	}

	// Sitewide Event — 50,000 impressions on one post
	if impressions >= 50000 {
		AwardAchievement(database, authorID, "troll_sitewide_event")
	}
	// Everybody Saw It — 100,000 impressions
	if impressions >= 100000 {
		AwardAchievement(database, authorID, "troll_everybody_saw_it")
	}

	// Thread Hijacker — your reply has more likes than the OP (OP has >= 50 likes)
	var opLikes int
	database.QueryRow(
		`SELECT COUNT(*) FROM work_reactions wr
		 JOIN works w ON w.id = wr.work_id
		 JOIN works reply ON reply.parent_cid = w.cid
		 WHERE reply.id = $1::uuid AND wr.reaction_type = 'like'`, postID,
	).Scan(&opLikes)
	if isReply && opLikes >= 50 && likes > opLikes {
		AwardAchievement(database, authorID, "troll_thread_hijacker")
	}
	// Public Execution — your reply has 10x the likes of the post you replied to
	if isReply && opLikes > 0 && likes >= opLikes*10 {
		AwardAchievement(database, authorID, "troll_public_execution")
	}

	// Total reposts across all posts — Digital Herpes
	totalReposts := countQuery(database,
		`SELECT COUNT(*) FROM work_reactions wr
		 JOIN works w ON w.id = wr.work_id
		 WHERE w.author_pial = $1 AND wr.reaction_type = 'repost'`, authorPIAL)
	if totalReposts >= 500 {
		AwardAchievement(database, authorID, "troll_digital_herpes")
	}

	// Professional Instigator — a post hits 10,000 impressions (trending proxy)
	if impressions >= 10000 {
		AwardAchievement(database, authorID, "troll_professional_instigator")
	}

	// Final Boss Of The Timeline — 10,000 total interactions on one post
	if total+reposts >= 10000 {
		AwardAchievement(database, authorID, "troll_final_boss")
	}
}

// TryAwardTrollOnBlocked checks achievements for the user who got blocked.
func TryAwardTrollOnBlocked(database *sql.DB, blockedUserID string) {
	blockCount := countQuery(database,
		`SELECT COUNT(*) FROM blocks WHERE blocked_id = $1`, blockedUserID)

	if blockCount >= 5000 {
		AwardAchievement(database, blockedUserID, "troll_mute_magnet") // reuse for blocks too
	}

	// Most Hated — top 1% of most-blocked users
	totalUsers := countQuery(database, `SELECT COUNT(*) FROM users`)
	if totalUsers > 0 {
		rank := countQuery(database,
			`SELECT COUNT(DISTINCT blocked_id) FROM (
				SELECT blocked_id, COUNT(*) AS bc FROM blocks GROUP BY blocked_id
				HAVING COUNT(*) > (SELECT COUNT(*) FROM blocks WHERE blocked_id = $1)
			) t`, blockedUserID)
		if rank <= totalUsers/100 {
			AwardAchievement(database, blockedUserID, "troll_most_hated")
		}
	}

	// Moral Bankruptcy — blocked 1000+ AND reported 500+
	reportCount := countQuery(database,
		`SELECT COUNT(*) FROM content_reports cr
		 JOIN works w ON w.id = cr.content_id::uuid
		 WHERE w.author_id = $1`, blockedUserID)
	if blockCount >= 1000 && reportCount >= 500 {
		AwardAchievement(database, blockedUserID, "troll_moral_bankruptcy")
	}

	// The Main Villain — most blocked user today (check if you're #1)
	var topBlocked string
	database.QueryRow(
		`SELECT blocked_id FROM blocks
		 WHERE created_at >= NOW() - INTERVAL '24 hours'
		 GROUP BY blocked_id ORDER BY COUNT(*) DESC LIMIT 1`,
	).Scan(&topBlocked)
	if topBlocked == blockedUserID {
		AwardAchievement(database, blockedUserID, "troll_main_villain")
	}
}

// TryAwardTrollOnMuted checks achievements for the user who got muted.
func TryAwardTrollOnMuted(database *sql.DB, mutedUserID string) {
	muteCount := countQuery(database,
		`SELECT COUNT(*) FROM user_mutes WHERE muted_id = $1`, mutedUserID)
	if muteCount >= 5000 {
		AwardAchievement(database, mutedUserID, "troll_mute_magnet")
	}
}

// TryAwardTrollOnReported checks achievements for the user whose content was reported.
func TryAwardTrollOnReported(database *sql.DB, reportedUserID string) {
	reportCount := countQuery(database,
		`SELECT COUNT(*) FROM content_reports cr
		 JOIN works w ON w.id::text = cr.content_id
		 WHERE w.author_id = $1`, reportedUserID)

	// Fearless — reported 100+ times, still active
	if reportCount >= 100 && !IsDeactivated(database, reportedUserID) {
		AwardAchievement(database, reportedUserID, "troll_fearless")
	}
	// Community Threat — reported 1000+ times
	if reportCount >= 1000 {
		AwardAchievement(database, reportedUserID, "troll_community_threat")
	}
	// Walking Disaster — 200+ reports
	if reportCount >= 200 {
		AwardAchievement(database, reportedUserID, "troll_walking_disaster")
	}
	// It's Just A Joke Bro — 10+ reports in one week, no suspension
	weekReports := countQuery(database,
		`SELECT COUNT(*) FROM content_reports cr
		 JOIN works w ON w.id::text = cr.content_id
		 WHERE w.author_id = $1 AND cr.created_at >= NOW() - INTERVAL '7 days'`, reportedUserID)
	if weekReports >= 10 && !IsDeactivated(database, reportedUserID) {
		AwardAchievement(database, reportedUserID, "troll_its_just_a_joke")
	}
	// Uncancelable — survived 100+ reports
	if reportCount >= 100 && !IsDeactivated(database, reportedUserID) {
		AwardAchievement(database, reportedUserID, "troll_uncancelable")
	}
	// Peak 2004 Internet — 200+ reports, never banned
	if reportCount >= 200 && !IsDeactivated(database, reportedUserID) {
		AwardAchievement(database, reportedUserID, "troll_peak_2004_internet")
	}
	// Untouchable — survived 500+ reports
	if reportCount >= 500 && !IsDeactivated(database, reportedUserID) {
		AwardAchievement(database, reportedUserID, "troll_untouchable")
	}

	// Blacklist Material — 10+ works removed (is_blocked). The second count that
	// used to run against the posts table has gone with the table; it was adding
	// zero to this total on every check.
	removedPosts := countQuery(database,
		`SELECT COUNT(*) FROM works WHERE author_id = $1 AND is_blocked = TRUE`, reportedUserID)
	if removedPosts >= 10 {
		AwardAchievement(database, reportedUserID, "troll_blacklist_material")
	}

	// Internet Antichrist — top blocked AND top reported
	blockCount := countQuery(database, `SELECT COUNT(*) FROM blocks WHERE blocked_id = $1`, reportedUserID)
	if blockCount >= 1000 && reportCount >= 500 {
		AwardAchievement(database, reportedUserID, "troll_internet_antichrist")
	}
	// Historically Problematic — mentioned by 100+ unique users in 30 days
	recentMentions := countQuery(database,
		`SELECT COUNT(DISTINCT n.actor_id) FROM notifications n
		 WHERE n.user_id = $1 AND n.type = 'mention'
		   AND n.created_at >= NOW() - INTERVAL '30 days'`, reportedUserID)
	if recentMentions >= 100 {
		AwardAchievement(database, reportedUserID, "troll_historically_problematic")
	}
}

// TryAwardTrollOnFollow checks achievements triggered by follow/unfollow events.
// targetID is the user being followed/unfollowed.
func TryAwardTrollOnFollow(database *sql.DB, targetID string) {
	// Shadow Government — 10+ accounts followed you within the same 1-hour window
	clusterFollows := countQuery(database,
		`SELECT COUNT(*) FROM follower_events
		 WHERE target_id = $1 AND event_type = 'follow'
		   AND created_at >= NOW() - INTERVAL '1 hour'`, targetID)
	if clusterFollows >= 10 {
		AwardAchievement(database, targetID, "troll_shadow_government")
	}

	// Villain Arc v2 — lost 500 followers AND gained 1000 in same 7-day window
	gained := countQuery(database,
		`SELECT COUNT(*) FROM follower_events
		 WHERE target_id = $1 AND event_type = 'follow'
		   AND created_at >= NOW() - INTERVAL '7 days'`, targetID)
	lost := countQuery(database,
		`SELECT COUNT(*) FROM follower_events
		 WHERE target_id = $1 AND event_type = 'unfollow'
		   AND created_at >= NOW() - INTERVAL '7 days'`, targetID)
	if gained >= 1000 && lost >= 500 {
		AwardAchievement(database, targetID, "troll_villain_arc_v2")
	}
}

// TryAwardTrollOnDeactivation awards Emotional Damage to users whose posts
// the deactivating user engaged with in the 48 hours before deactivation.
func TryAwardTrollOnDeactivation(database *sql.DB, deactivatedUserID string) {
	rows, err := database.Query(
		`SELECT DISTINCT w.author_id
		 FROM works w
		 JOIN works reply ON reply.parent_cid = w.cid
		 WHERE reply.author_id = $1
		   AND reply.created_at >= NOW() - INTERVAL '48 hours'
		   AND w.author_id != $1
		   AND w.deleted_at IS NULL`,
		deactivatedUserID,
	)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var authorID string
		if rows.Scan(&authorID) == nil {
			AwardAchievement(database, authorID, "troll_emotional_damage")
		}
	}
}

// TryAwardTrollOnSessionUpdate checks session-duration achievements.
func TryAwardTrollOnSessionUpdate(database *sql.DB, userID, sessionID string) {
	var durationSecs float64
	database.QueryRow(
		`SELECT EXTRACT(EPOCH FROM (last_seen_at - created_at)) FROM user_sessions WHERE id = $1::uuid`,
		sessionID,
	).Scan(&durationSecs)
	// Unemployed Final Boss — 16 consecutive hours (57600 seconds)
	if durationSecs >= 57600 {
		AwardAchievement(database, userID, "troll_unemployed_final_boss")
	}
}

// TryAwardTrollOnProfileView checks the Known Terrorist achievement.
func TryAwardTrollOnProfileView(database *sql.DB, profileOwnerID string) {
	views := IncrementProfileViews(database, profileOwnerID)
	if views >= 50000 {
		AwardAchievement(database, profileOwnerID, "troll_known_terrorist")
	}
}

// TryAwardTrollOnMention checks Rent Free — mentioned by someone never interacted with.
func TryAwardTrollOnMention(database *sql.DB, mentionedUserID, mentionerUserID string) {
	// Check if these two have interacted (replied to each other's posts) before
	priorInteraction := countQuery(database,
		`SELECT COUNT(*) FROM works w
		 JOIN works parent ON parent.cid = w.parent_cid
		 WHERE (w.author_id = $1 AND parent.author_id = $2)
		    OR (w.author_id = $2 AND parent.author_id = $1)
		   AND w.deleted_at IS NULL`, mentionedUserID, mentionerUserID)
	if priorInteraction == 0 {
		AwardAchievement(database, mentionedUserID, "troll_rent_free")
	}
}

// TryAwardTrollForumDemon checks if a user replied in 10+ distinct trending threads today.
func TryAwardTrollForumDemon(database *sql.DB, userID, pialID string) {
	distinctThreadsToday := countQuery(database,
		`SELECT COUNT(DISTINCT root_cid) FROM works
		 WHERE author_pial = $1 AND kind = 'reply'
		   AND created_at >= NOW() - INTERVAL '24 hours'
		   AND deleted_at IS NULL`, pialID)
	if distinctThreadsToday >= 10 {
		AwardAchievement(database, userID, "troll_forum_demon")
	}
}

// TryAwardTrollCartmanMethod checks if your post drew 50+ unique participants.
func TryAwardTrollCartmanMethod(database *sql.DB, userID, authorPIAL, postID string) {
	uniqueParticipants := countQuery(database,
		`SELECT COUNT(DISTINCT author_pial) FROM works
		 WHERE root_cid = (SELECT cid FROM works WHERE id = $1::uuid)
		   AND author_pial != $2
		   AND kind = 'reply' AND deleted_at IS NULL`, postID, authorPIAL)
	if uniqueParticipants >= 50 {
		AwardAchievement(database, userID, "troll_cartman_method")
	}
}

// TryAwardSouthParkS4 checks if a user has earned all troll achievements (meta).
func TryAwardSouthParkS4(database *sql.DB, userID string) {
	trollIDs := []string{
		"troll_master_baiter", "troll_gaslight_specialist", "troll_professional_instigator",
		"troll_villain_arc_v2", "troll_thread_hijacker", "troll_public_execution",
		"troll_demon_time", "troll_emotional_damage", "troll_nuclear_take", "troll_fearless",
		"troll_off_the_meds", "troll_wall_of_text", "troll_conspiracy_dept",
		"troll_unemployed_final_boss", "troll_shadow_government", "troll_paranoid_legend",
		"troll_hallucination_engine", "troll_most_hated", "troll_mute_magnet",
		"troll_community_threat", "troll_digital_herpes", "troll_known_terrorist",
		"troll_blacklist_material", "troll_walking_disaster", "troll_i_own_you",
		"troll_rent_free", "troll_crashout_inducer", "troll_clip_farm",
		"troll_reverse_ratio", "troll_infinite_aura_loss", "troll_feigning_ignorance",
		"troll_goalpost_olympics", "troll_bad_faith_actor", "troll_cartman_method",
		"troll_its_just_a_joke", "troll_uncancelable", "troll_moral_bankruptcy",
		"troll_peak_2004_internet", "troll_weaponized_autism", "troll_forum_demon",
		"troll_archive_diver", "troll_digital_cigarette", "troll_final_boss",
		"troll_internet_antichrist", "troll_reason_we_need_rules", "troll_sitewide_event",
		"troll_everybody_saw_it", "troll_chaos_god", "troll_main_villain",
		"troll_historically_problematic", "troll_untouchable",
	}
	for _, id := range trollIDs {
		if !HasAchievement(database, userID, id) {
			return
		}
	}
	AwardAchievement(database, userID, "troll_south_park_s4")
}

// AwardAchievement gives a user an achievement if not already earned.
// Silent no-op if already earned or on any error.
func AwardAchievement(database *sql.DB, userID, achievementID string) bool {
	res, err := database.Exec(
		`INSERT INTO user_achievements (user_id, achievement_id)
		 VALUES ($1, $2) ON CONFLICT DO NOTHING`,
		userID, achievementID,
	)
	if err != nil {
		return false
	}
	n, _ := res.RowsAffected()
	return n > 0
}

// GetUserAchievements returns all achievements for a user, earned ones have EarnedAt set.
func GetUserAchievements(database *sql.DB, userID string) ([]model.Achievement, error) {
	rows, err := database.Query(`
		SELECT a.id, a.name, a.description, a.icon, a.tier,
		       ua.earned_at
		FROM achievements a
		LEFT JOIN user_achievements ua
		       ON ua.achievement_id = a.id AND ua.user_id = $1
		ORDER BY
		  CASE a.tier WHEN 'platinum' THEN 1 WHEN 'gold' THEN 2 WHEN 'silver' THEN 3 ELSE 4 END,
		  a.id
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Achievement
	for rows.Next() {
		var ach model.Achievement
		var earnedAt sql.NullTime
		if err := rows.Scan(&ach.ID, &ach.Name, &ach.Description, &ach.Icon, &ach.Tier, &earnedAt); err != nil {
			continue
		}
		if earnedAt.Valid {
			t := earnedAt.Time
			ach.EarnedAt = &t
		}
		out = append(out, ach)
	}
	return out, rows.Err()
}

// CountUserPosts returns number of posts a user has made.
func CountUserPosts(database *sql.DB, userID string) int {
	var n int
	database.QueryRow(`SELECT COUNT(*) FROM works WHERE author_id = $1 AND kind <> 'reply' AND deleted_at IS NULL`, userID).Scan(&n)
	return n
}

// CountUserFollowers returns follower count.
func CountUserFollowers(database *sql.DB, userID string) int {
	var n int
	database.QueryRow(`SELECT COUNT(*) FROM follows WHERE following_id = $1`, userID).Scan(&n)
	return n
}

// TryAwardPostAchievements checks post milestones and awards achievements.
func TryAwardPostAchievements(database *sql.DB, userID string) {
	n := CountUserPosts(database, userID)
	if n == 1 {
		AwardAchievement(database, userID, "first_post")
	}
	if n >= 10 {
		AwardAchievement(database, userID, "post_10")
	}
	if n >= 100 {
		AwardAchievement(database, userID, "post_100")
	}
}

// TryAwardFollowerAchievements checks follower milestones.
func TryAwardFollowerAchievements(database *sql.DB, userID string) {
	n := CountUserFollowers(database, userID)
	if n == 1 {
		AwardAchievement(database, userID, "first_follower")
	}
	if n >= 10 {
		AwardAchievement(database, userID, "followers_10")
	}
	if n >= 50 {
		AwardAchievement(database, userID, "followers_50")
	}
	if n >= 100 {
		AwardAchievement(database, userID, "followers_100")
	}
}

// TryAwardRealmAchievements checks realm milestone.
func TryAwardRealmAchievements(database *sql.DB, userID string, realm int) {
	if realm >= 4 {
		AwardAchievement(database, userID, "realm_adept")
	}
	if realm >= 5 {
		AwardAchievement(database, userID, "realm_guardian")
	}
}

// CountUserVisions returns how many Visions a user has authored, over their
// whole history. Expiry is not a filter: a milestone counts what was posted,
// and an expired Vision was still posted.
//
// This used to count works.kind='vision'. The ephemeral lane moved to its own
// table in migration 0008, then to PIAL-only addressing in migration 0023, so
// that count would now read 0 forever if it still queried the old shape.
func CountUserVisions(database *sql.DB, authorPIAL string) int {
	return countQuery(database,
		`SELECT COUNT(*) FROM visions WHERE author_pial = $1::uuid AND deleted_at IS NULL`,
		authorPIAL)
}

// TryAwardVisionAchievements checks Vision posting milestones and awards
// achievements. Call this after every successful Vision insertion.
//
// The achievement ids stay vision_* — they are stable keys already referenced by
// earned user_achievements rows. Migration 0024 relabels their display text.
func TryAwardVisionAchievements(database *sql.DB, userID, authorPIAL string) {
	n := CountUserVisions(database, authorPIAL)
	if n == 1 {
		AwardAchievement(database, userID, "vision_first")
	}
	if n >= 10 {
		AwardAchievement(database, userID, "vision_seer")
	}
	if n >= 50 {
		AwardAchievement(database, userID, "vision_oracle")
	}
	// vision_quest (7-day streak), vision_sight (100 views), vision_revelation (1000 views)
	// are event-driven and awarded from their respective trigger points when implemented.
}

// HasAchievement checks whether a user has a specific achievement.
func HasAchievement(database *sql.DB, userID, achievementID string) bool {
	var n int
	database.QueryRow(
		`SELECT COUNT(*) FROM user_achievements WHERE user_id = $1 AND achievement_id = $2`,
		userID, achievementID,
	).Scan(&n)
	return n > 0
}

// GetEarnedAchievements returns only the achievements a user has earned.
func GetEarnedAchievements(database *sql.DB, userID string) ([]model.Achievement, error) {
	rows, err := database.Query(`
		SELECT a.id, a.name, a.description, a.icon, a.tier, ua.earned_at
		FROM user_achievements ua
		JOIN achievements a ON a.id = ua.achievement_id
		WHERE ua.user_id = $1
		ORDER BY ua.earned_at DESC
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Achievement
	for rows.Next() {
		var ach model.Achievement
		var t time.Time
		if err := rows.Scan(&ach.ID, &ach.Name, &ach.Description, &ach.Icon, &ach.Tier, &t); err != nil {
			continue
		}
		ach.EarnedAt = &t
		out = append(out, ach)
	}
	return out, rows.Err()
}
