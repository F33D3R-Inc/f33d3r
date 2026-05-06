package db

import (
	"database/sql"
	"time"

	"github.com/f33d3r/feed-engine/internal/model"
)

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
	database.QueryRow(`SELECT COUNT(*) FROM posts WHERE author_id = $1 AND is_reply = FALSE`, userID).Scan(&n)
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
