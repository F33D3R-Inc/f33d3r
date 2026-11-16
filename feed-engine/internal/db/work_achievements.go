package db

// work_achievements.go — the achievements a published work can earn, for its
// author and for the author of the work it replies to.
//
// This is the achievements obligation of the work outbox, so every query reports
// its failure and the obligation is retried; nothing degrades to zero. Thread
// structure is read from work_citations (root_id, target_id) and replies from
// works.is_reply — the earlier checks read works.parent_cid, root_cid and
// parent_id, columns works never had, so every one of them failed and counted 0.

import (
	"database/sql"
	"errors"
	"fmt"
	"unicode/utf8"
)

type achievementCounter struct {
	db  *sql.DB
	err error
}

func (c *achievementCounter) count(q string, args ...interface{}) int {
	if c.err != nil {
		return 0
	}
	var n int
	if err := c.db.QueryRow(q, args...).Scan(&n); err != nil {
		c.err = fmt.Errorf("achievement count: %w", err)
		return 0
	}
	return n
}

func (c *achievementCounter) grant(userID, achievementID string, earned bool) {
	if c.err != nil || !earned {
		return
	}
	if _, err := c.db.Exec(`
		INSERT INTO user_achievements (user_id, achievement_id)
		VALUES ($1::uuid, $2) ON CONFLICT DO NOTHING`, userID, achievementID); err != nil {
		c.err = fmt.Errorf("granting %s to %s: %w", achievementID, userID, err)
	}
}

// AwardWorkAchievements checks what one newly published work earns.
func AwardWorkAchievements(database *sql.DB, workID string) error {
	var authorID, authorPIAL, body string
	var isReply bool
	err := database.QueryRow(`
		SELECT author_id::text, author_pial::text, body, is_reply
		  FROM works WHERE id = $1::uuid AND deleted_at IS NULL`, workID).
		Scan(&authorID, &authorPIAL, &body, &isReply)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // deleted before it was checked; it earns nothing
	}
	if err != nil {
		return fmt.Errorf("reading work %s for achievements: %w", workID, err)
	}
	c := &achievementCounter{db: database}

	// Post milestones count the author's top-level works.
	posts := c.count(`SELECT COUNT(*) FROM works WHERE author_id = $1::uuid AND NOT is_reply AND deleted_at IS NULL`, authorID)
	c.grant(authorID, "first_post", posts >= 1)
	c.grant(authorID, "post_10", posts >= 10)
	c.grant(authorID, "post_100", posts >= 100)

	// Wall Of Text Warrior — a single work of 5000+ characters.
	c.grant(authorID, "troll_wall_of_text", utf8.RuneCountInString(body) >= 5000)

	// Off The Meds — 100 works between 02:00 and 04:59 UTC.
	c.grant(authorID, "troll_off_the_meds", c.count(`
		SELECT COUNT(*) FROM works WHERE author_pial = $1::uuid AND deleted_at IS NULL
		   AND EXTRACT(HOUR FROM created_at AT TIME ZONE 'UTC') BETWEEN 2 AND 4`, authorPIAL) >= 100)

	// Conspiracy Department — 50 works with a hashtag.
	c.grant(authorID, "troll_conspiracy_dept", c.count(`
		SELECT COUNT(*) FROM works WHERE author_pial = $1::uuid AND deleted_at IS NULL AND strpos(body, '#') > 0`, authorPIAL) >= 50)

	// Paranoid Legend — the word "bot" in 100 works.
	c.grant(authorID, "troll_paranoid_legend", c.count(`
		SELECT COUNT(*) FROM works WHERE author_pial = $1::uuid AND deleted_at IS NULL AND body ILIKE '%bot%'`, authorPIAL) >= 100)

	if isReply {
		awardReplyAchievements(c, workID, authorID, authorPIAL, body)
	}
	return c.err
}

// awardReplyAchievements: the replier's thread achievements, and what the reply
// earns the author of the work it answers.
func awardReplyAchievements(c *achievementCounter, workID, authorID, authorPIAL, body string) {
	// The author's replies, each with the root of its thread.
	const myReplies = `
		FROM works w
		JOIN work_citations c ON c.work_id = w.id AND c.citation_type = 'reply'
		WHERE w.author_pial = $1::uuid AND w.is_reply AND w.deleted_at IS NULL`

	// Gaslight Specialist — 50+ replies in threads somebody else started.
	c.grant(authorID, "troll_gaslight_specialist", c.count(`
		SELECT COUNT(*) `+myReplies+`
		   AND NOT EXISTS (SELECT 1 FROM works r WHERE r.id = c.root_id AND r.author_pial = $1::uuid)`, authorPIAL) >= 50)

	// Bad Faith Actor / Goalpost Olympics / Feigning Ignorance — replies in one thread.
	most := c.count(`SELECT COALESCE(MAX(n), 0) FROM (SELECT COUNT(*) AS n `+myReplies+` GROUP BY c.root_id) t`, authorPIAL)
	c.grant(authorID, "troll_bad_faith_actor", most >= 10)
	c.grant(authorID, "troll_goalpost_olympics", most >= 15)
	c.grant(authorID, "troll_feigning_ignorance", most >= 20)

	// Digital Cigarette Smoker — 50+ replies between 23:00 and 03:59 UTC.
	c.grant(authorID, "troll_digital_cigarette", c.count(`
		SELECT COUNT(*) `+myReplies+`
		   AND (EXTRACT(HOUR FROM w.created_at AT TIME ZONE 'UTC') >= 23
		     OR EXTRACT(HOUR FROM w.created_at AT TIME ZONE 'UTC') <= 3)`, authorPIAL) >= 50)

	// Forum Demon — replies in 10+ distinct threads within a day.
	c.grant(authorID, "troll_forum_demon", c.count(`
		SELECT COUNT(DISTINCT c.root_id) `+myReplies+`
		   AND w.created_at >= NOW() - INTERVAL '24 hours'`, authorPIAL) >= 10)

	// The work this reply answers.
	var parentID, parentAuthorID, parentPIAL string
	var parentAge int
	err := c.db.QueryRow(`
		SELECT p.id::text, p.author_id::text, p.author_pial::text,
		       EXTRACT(EPOCH FROM (w.created_at - p.created_at))::int
		  FROM work_citations c
		  JOIN works p ON p.id = c.target_id
		  JOIN works w ON w.id = c.work_id
		 WHERE c.work_id = $1::uuid AND c.citation_type = 'reply'`, workID).
		Scan(&parentID, &parentAuthorID, &parentPIAL, &parentAge)
	if errors.Is(err, sql.ErrNoRows) {
		return // the parent was removed; nothing more to earn from it
	}
	if err != nil {
		if c.err == nil {
			c.err = fmt.Errorf("reading parent of reply %s: %w", workID, err)
		}
		return
	}

	// Weaponized Autism / Archive Diver — replying to a work 2+ / 1+ years old.
	c.grant(authorID, "troll_weaponized_autism", parentAge >= 63072000)
	c.grant(authorID, "troll_archive_diver", parentAge >= 31536000)

	if parentAuthorID == authorID {
		return // answering yourself earns the parent's author nothing
	}

	// I Own You — someone writes a 500+ character reply to your work.
	c.grant(parentAuthorID, "troll_i_own_you", utf8.RuneCountInString(body) >= 500)

	// Replies anyone wrote to works by the parent's author.
	const repliesToThem = `
		FROM works r
		JOIN work_citations c ON c.work_id = r.id AND c.citation_type = 'reply'
		JOIN works t ON t.id = c.target_id
		WHERE t.author_pial = $1::uuid AND r.author_pial <> $1::uuid
		  AND r.is_reply AND r.deleted_at IS NULL`

	// Master Baiter — 100 distinct repliers across your works.
	c.grant(parentAuthorID, "troll_master_baiter", c.count(`
		SELECT COUNT(DISTINCT r.author_pial) `+repliesToThem, parentPIAL) >= 100)

	// Demon Time — 1000 replies to one work within an hour.
	c.grant(parentAuthorID, "troll_demon_time", c.count(`
		SELECT COUNT(*) FROM work_citations c JOIN works r ON r.id = c.work_id
		 WHERE c.target_id = $1::uuid AND c.citation_type = 'reply'
		   AND r.deleted_at IS NULL AND r.created_at >= NOW() - INTERVAL '60 minutes'`, parentID) >= 1000)

	// The thread the reply joined, by its root.
	var rootID, rootAuthorID, rootPIAL string
	err = c.db.QueryRow(`
		SELECT r.id::text, r.author_id::text, r.author_pial::text
		  FROM work_citations c JOIN works r ON r.id = c.root_id
		 WHERE c.work_id = $1::uuid AND c.citation_type = 'reply'`, workID).
		Scan(&rootID, &rootAuthorID, &rootPIAL)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		if c.err == nil {
			c.err = fmt.Errorf("reading thread root of reply %s: %w", workID, err)
		}
		return
	}
	if err == nil {
		const thread = `
			FROM work_citations c JOIN works r ON r.id = c.work_id
			WHERE c.root_id = $1::uuid AND c.citation_type = 'reply' AND r.deleted_at IS NULL`
		// Crashout Inducer — a thread you started reaches 50+ replies.
		c.grant(rootAuthorID, "troll_crashout_inducer", c.count(`SELECT COUNT(*) `+thread, rootID) >= 50)
		// Cartman Method — 50+ other people in a thread you started.
		c.grant(rootAuthorID, "troll_cartman_method", c.count(`
			SELECT COUNT(DISTINCT r.author_pial) `+thread+` AND r.author_pial <> $2::uuid`, rootID, rootPIAL) >= 50)
	}

	// Hallucination Engine — 50 replies to one work from people who had never
	// replied to any other work of its author.
	c.grant(parentAuthorID, "troll_hallucination_engine", c.count(`
		SELECT COUNT(DISTINCT r.author_pial)
		  FROM work_citations c JOIN works r ON r.id = c.work_id
		 WHERE c.target_id = $1::uuid AND c.citation_type = 'reply'
		   AND r.deleted_at IS NULL AND r.author_pial <> $2::uuid
		   AND NOT EXISTS (
		       SELECT 1 FROM work_citations c2
		         JOIN works r2 ON r2.id = c2.work_id
		         JOIN works t2 ON t2.id = c2.target_id
		        WHERE c2.citation_type = 'reply' AND r2.deleted_at IS NULL
		          AND r2.author_pial = r.author_pial
		          AND t2.author_pial = $2::uuid AND t2.id <> $1::uuid)`, parentID, parentPIAL) >= 50)
}
