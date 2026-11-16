package db

// analytics.go — creator and site analytics, read from feed-engine's own rows.
//
// These reads used to live in Astraon, a separate process that held a pool on
// f33d3r_feed and read these same tables across a service boundary it did not
// own. The numbers are facts about works, reactions, citations and follows, and
// every one of those is feed-engine's, so they are counted here.
//
// Every tally is a correlated count over row truth — the shape 0004 adopted when
// the cached counters were dropped — so there is no copy of a number to drift.
// What no row records is not reported: there is no watch-time column and no
// unfollow history, so neither appears.

import (
	"database/sql"
	"fmt"
	"time"
)

// Tallies of one work w, counted at read time.
const (
	anlLikes   = `(SELECT COUNT(*) FROM work_reactions wr WHERE wr.work_id = w.id AND wr.reaction_type = 'like')`
	anlReposts = `(SELECT COUNT(*) FROM work_reactions wr WHERE wr.work_id = w.id AND wr.reaction_type = 'repost')`
	anlSaves   = `(SELECT COUNT(*) FROM work_reactions wr WHERE wr.work_id = w.id AND wr.reaction_type = 'bookmark')`
	anlReplies = `(SELECT COUNT(*) FROM work_citations wc WHERE wc.target_id = w.id AND wc.citation_type = 'reply')`
	anlQuotes  = `(SELECT COUNT(*) FROM work_citations wc WHERE wc.target_id = w.id AND wc.citation_type = 'quote')`
	// Impressions have one owner on a work: works.view_count.
	anlImpressions = `COALESCE(w.view_count, 0)`
	// The media shape a work actually carries. works.content_type is never
	// written and works.kind is structural, so neither answers this.
	anlContentType = `CASE
	    WHEN COALESCE(w.video_master_url, '') <> '' THEN 'video'
	    WHEN COALESCE(w.voice_url, '') <> ''        THEN 'voice'
	    WHEN COALESCE(cardinality(w.poll_options), 0) > 0 THEN 'poll'
	    WHEN COALESCE(cardinality(w.media_urls), 0) > 0   THEN 'image'
	    ELSE 'text'
	END`
	// A creator's own published top-level works inside the period. $1 is the
	// account id, $2 the period in days (0 = all time).
	anlAuthored = `w.author_id = $1::uuid
	    AND NOT w.is_reply
	    AND w.deleted_at IS NULL
	    AND w.scheduled_at IS NULL
	    AND ($2::int = 0 OR w.created_at > NOW() - make_interval(days => $2::int))`
)

// AnalyticsPeriods are the periods a reader may ask for, in days; 0 is all time.
var AnalyticsPeriods = map[string]int{"7": 7, "30": 30, "90": 90, "all": 0}

// AnalyticsSorts maps a requested ordering of the work table to its SQL.
var AnalyticsSorts = map[string]string{
	"impressions": "impressions DESC, created_at DESC",
	"likes":       "likes DESC, created_at DESC",
	"reposts":     "reposts DESC, created_at DESC",
	"replies":     "replies DESC, created_at DESC",
	"saves":       "saves DESC, created_at DESC",
	"engagement":  "engagement_pct DESC, created_at DESC",
	"date":        "created_at DESC",
}

func engagementPct(likes, reposts, replies, impressions int64) float64 {
	if impressions <= 0 {
		return 0
	}
	return float64(likes+reposts+replies) / float64(impressions) * 100
}

// CreatorSummary is one creator's totals over a period.
type CreatorSummary struct {
	PeriodDays       int
	TotalPosts       int64
	Impressions      int64
	Likes            int64
	Reposts          int64
	Replies          int64
	Quotes           int64
	Saves            int64
	CurrentFollowers int64
	FollowersGained  int64 // follows that began inside the period; 0 for all time
	EngagementRate   float64
}

// GetCreatorSummary totals a creator's works and audience over a period.
func GetCreatorSummary(database *sql.DB, accountID string, days int) (*CreatorSummary, error) {
	s := &CreatorSummary{PeriodDays: days}
	err := database.QueryRow(fmt.Sprintf(`
		SELECT COUNT(w.id),
		       COALESCE(SUM(%s), 0), COALESCE(SUM(%s), 0), COALESCE(SUM(%s), 0),
		       COALESCE(SUM(%s), 0), COALESCE(SUM(%s), 0), COALESCE(SUM(%s), 0)
		  FROM works w
		 WHERE %s`, anlImpressions, anlLikes, anlReposts, anlReplies, anlQuotes, anlSaves, anlAuthored),
		accountID, days).Scan(&s.TotalPosts, &s.Impressions, &s.Likes, &s.Reposts, &s.Replies, &s.Quotes, &s.Saves)
	if err != nil {
		return nil, fmt.Errorf("creator summary of %s: %w", accountID, err)
	}
	err = database.QueryRow(`
		SELECT COUNT(*),
		       COUNT(*) FILTER (WHERE $2::int > 0 AND created_at > NOW() - make_interval(days => $2::int))
		  FROM follows WHERE following_id = $1::uuid`, accountID, days).Scan(&s.CurrentFollowers, &s.FollowersGained)
	if err != nil {
		return nil, fmt.Errorf("follower counts of %s: %w", accountID, err)
	}
	s.EngagementRate = engagementPct(s.Likes, s.Reposts, s.Replies, s.Impressions)
	return s, nil
}

// CreatorWorkRow is one work in the performance table.
type CreatorWorkRow struct {
	PostID        string
	BodyPreview   string
	ContentType   string
	CreatedAt     time.Time
	Impressions   int64
	Likes         int64
	Reposts       int64
	Replies       int64
	Quotes        int64
	Saves         int64
	EngagementPct float64
	BarPct        float64 // impressions relative to the table's top work
}

// CreatorWorks is a creator's work performance table.
type CreatorWorks struct {
	Posts  []CreatorWorkRow
	SortBy string
	Period int
}

// GetCreatorWorks returns up to limit of a creator's works in the period,
// ordered by sortBy, which must be a key of AnalyticsSorts.
func GetCreatorWorks(database *sql.DB, accountID string, days int, sortBy string, limit int) (*CreatorWorks, error) {
	order, ok := AnalyticsSorts[sortBy]
	if !ok {
		return nil, fmt.Errorf("creator works: unknown ordering %q", sortBy)
	}
	rows, err := database.Query(fmt.Sprintf(`
		SELECT t.*,
		       CASE WHEN t.impressions > 0
		            THEN (t.likes + t.reposts + t.replies)::float8 / t.impressions::float8 * 100
		            ELSE 0 END AS engagement_pct
		  FROM (SELECT w.id::text, LEFT(COALESCE(w.body, ''), 120), (%s), w.created_at,
		               %s::bigint AS impressions, %s AS likes, %s AS reposts,
		               %s AS replies, %s AS quotes, %s AS saves
		          FROM works w
		         WHERE %s) t
		 ORDER BY %s
		 LIMIT $3`, anlContentType, anlImpressions, anlLikes, anlReposts, anlReplies, anlQuotes, anlSaves, anlAuthored, order),
		accountID, days, limit)
	if err != nil {
		return nil, fmt.Errorf("creator works of %s: %w", accountID, err)
	}
	defer rows.Close()
	out := &CreatorWorks{Posts: []CreatorWorkRow{}, SortBy: sortBy, Period: days}
	var top int64
	for rows.Next() {
		var r CreatorWorkRow
		if err := rows.Scan(&r.PostID, &r.BodyPreview, &r.ContentType, &r.CreatedAt,
			&r.Impressions, &r.Likes, &r.Reposts, &r.Replies, &r.Quotes, &r.Saves, &r.EngagementPct); err != nil {
			return nil, fmt.Errorf("creator works of %s: %w", accountID, err)
		}
		if r.Impressions > top {
			top = r.Impressions
		}
		out.Posts = append(out.Posts, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("creator works of %s: %w", accountID, err)
	}
	if top > 0 {
		for i := range out.Posts {
			out.Posts[i].BarPct = float64(out.Posts[i].Impressions) / float64(top) * 100
		}
	}
	return out, nil
}

// TimelinePoint is one day of a creator's activity.
type TimelinePoint struct {
	Date        string // YYYY-MM-DD
	Posts       int64
	Impressions int64
	Likes       int64
	Reposts     int64
	Replies     int64
	Engagement  float64
}

// CreatorTimeline is a creator's day-by-day activity.
type CreatorTimeline struct {
	Points         []TimelinePoint
	PeriodDays     int
	MaxImpressions int64
	MaxEngagement  float64
}

// chartDays is the window a day-by-day chart covers: all time is drawn as the
// last 30 days, because a bar per day since the first work is not a chart.
func chartDays(days int) int {
	if days <= 0 {
		return 30
	}
	return days
}

// GetCreatorTimeline buckets a creator's works by the day they were published.
func GetCreatorTimeline(database *sql.DB, accountID string, days int) (*CreatorTimeline, error) {
	span := chartDays(days)
	rows, err := database.Query(fmt.Sprintf(`
		SELECT to_char(date_trunc('day', w.created_at), 'YYYY-MM-DD') AS day,
		       COUNT(w.id),
		       COALESCE(SUM(%s), 0), COALESCE(SUM(%s), 0),
		       COALESCE(SUM(%s), 0), COALESCE(SUM(%s), 0)
		  FROM works w
		 WHERE %s
		 GROUP BY 1
		 ORDER BY 1`, anlImpressions, anlLikes, anlReposts, anlReplies, anlAuthored),
		accountID, span)
	if err != nil {
		return nil, fmt.Errorf("creator timeline of %s: %w", accountID, err)
	}
	defer rows.Close()
	tl := &CreatorTimeline{Points: []TimelinePoint{}, PeriodDays: span}
	for rows.Next() {
		var p TimelinePoint
		if err := rows.Scan(&p.Date, &p.Posts, &p.Impressions, &p.Likes, &p.Reposts, &p.Replies); err != nil {
			return nil, fmt.Errorf("creator timeline of %s: %w", accountID, err)
		}
		p.Engagement = engagementPct(p.Likes, p.Reposts, p.Replies, p.Impressions)
		if p.Impressions > tl.MaxImpressions {
			tl.MaxImpressions = p.Impressions
		}
		if p.Engagement > tl.MaxEngagement {
			tl.MaxEngagement = p.Engagement
		}
		tl.Points = append(tl.Points, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("creator timeline of %s: %w", accountID, err)
	}
	return tl, nil
}

// AudiencePoint is one day of new followers.
type AudiencePoint struct {
	Date         string // YYYY-MM-DD
	NewFollowers int64
}

// CreatorAudience is a creator's follower growth.
type CreatorAudience struct {
	Points         []AudiencePoint
	TotalFollowers int64
	PeriodDays     int
}

// GetCreatorAudience buckets the follows a creator received by day.
func GetCreatorAudience(database *sql.DB, accountID string, days int) (*CreatorAudience, error) {
	span := chartDays(days)
	aud := &CreatorAudience{Points: []AudiencePoint{}, PeriodDays: span}
	if err := database.QueryRow(`SELECT COUNT(*) FROM follows WHERE following_id = $1::uuid`, accountID).
		Scan(&aud.TotalFollowers); err != nil {
		return nil, fmt.Errorf("follower count of %s: %w", accountID, err)
	}
	rows, err := database.Query(`
		SELECT to_char(date_trunc('day', created_at), 'YYYY-MM-DD'), COUNT(*)
		  FROM follows
		 WHERE following_id = $1::uuid
		   AND created_at > NOW() - make_interval(days => $2::int)
		 GROUP BY 1
		 ORDER BY 1`, accountID, span)
	if err != nil {
		return nil, fmt.Errorf("follower growth of %s: %w", accountID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var p AudiencePoint
		if err := rows.Scan(&p.Date, &p.NewFollowers); err != nil {
			return nil, fmt.Errorf("follower growth of %s: %w", accountID, err)
		}
		aud.Points = append(aud.Points, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("follower growth of %s: %w", accountID, err)
	}
	return aud, nil
}

// BreakdownItem is one media shape's share of a creator's works.
type BreakdownItem struct {
	ContentType   string
	Count         int64
	Impressions   int64
	AvgEngagement float64
	PctOfPosts    float64
}

// CreatorBreakdown is a creator's works by media shape.
type CreatorBreakdown struct {
	Types []BreakdownItem
}

// GetCreatorBreakdown groups a creator's works in the period by media shape.
func GetCreatorBreakdown(database *sql.DB, accountID string, days int) (*CreatorBreakdown, error) {
	rows, err := database.Query(fmt.Sprintf(`
		SELECT (%s), COUNT(w.id),
		       COALESCE(SUM(%s), 0), COALESCE(SUM(%s), 0),
		       COALESCE(SUM(%s), 0), COALESCE(SUM(%s), 0)
		  FROM works w
		 WHERE %s
		 GROUP BY 1
		 ORDER BY 2 DESC, 1`, anlContentType, anlImpressions, anlLikes, anlReposts, anlReplies, anlAuthored),
		accountID, days)
	if err != nil {
		return nil, fmt.Errorf("creator breakdown of %s: %w", accountID, err)
	}
	defer rows.Close()
	bd := &CreatorBreakdown{Types: []BreakdownItem{}}
	var total int64
	for rows.Next() {
		var it BreakdownItem
		var likes, reposts, replies int64
		if err := rows.Scan(&it.ContentType, &it.Count, &it.Impressions, &likes, &reposts, &replies); err != nil {
			return nil, fmt.Errorf("creator breakdown of %s: %w", accountID, err)
		}
		it.AvgEngagement = engagementPct(likes, reposts, replies, it.Impressions)
		total += it.Count
		bd.Types = append(bd.Types, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("creator breakdown of %s: %w", accountID, err)
	}
	for i := range bd.Types {
		bd.Types[i].PctOfPosts = float64(bd.Types[i].Count) / float64(total) * 100
	}
	return bd, nil
}

// SiteAnalytics is the platform's totals for the admin panel.
type SiteAnalytics struct {
	TotalUsers       int64
	TotalPosts       int64
	TotalImpressions int64
	TotalLikes       int64
	TotalReposts     int64
	TotalComments    int64
	NewUsersToday    int64
	NewUsersWeek     int64
	NewPostsToday    int64
	NewPostsWeek     int64
	ActiveUsersDay   int64 // distinct accounts that published in the last 24 hours
	ActiveUsersWeek  int64
}

// GetSiteAnalytics counts the platform in one statement.
func GetSiteAnalytics(database *sql.DB) (*SiteAnalytics, error) {
	s := &SiteAnalytics{}
	err := database.QueryRow(`
		WITH live AS (
		    SELECT author_id, is_reply, created_at, COALESCE(view_count, 0) AS views
		      FROM works WHERE deleted_at IS NULL AND scheduled_at IS NULL)
		SELECT (SELECT COUNT(*) FROM users),
		       (SELECT COUNT(*) FROM live WHERE NOT is_reply),
		       (SELECT COALESCE(SUM(views), 0) FROM live),
		       (SELECT COUNT(*) FROM work_reactions WHERE reaction_type = 'like'),
		       (SELECT COUNT(*) FROM work_reactions WHERE reaction_type = 'repost'),
		       (SELECT COUNT(*) FROM work_citations WHERE citation_type = 'reply'),
		       (SELECT COUNT(*) FROM users WHERE created_at > NOW() - INTERVAL '1 day'),
		       (SELECT COUNT(*) FROM users WHERE created_at > NOW() - INTERVAL '7 days'),
		       (SELECT COUNT(*) FROM live WHERE NOT is_reply AND created_at > NOW() - INTERVAL '1 day'),
		       (SELECT COUNT(*) FROM live WHERE NOT is_reply AND created_at > NOW() - INTERVAL '7 days'),
		       (SELECT COUNT(DISTINCT author_id) FROM live WHERE created_at > NOW() - INTERVAL '1 day'),
		       (SELECT COUNT(DISTINCT author_id) FROM live WHERE created_at > NOW() - INTERVAL '7 days')`).Scan(
		&s.TotalUsers, &s.TotalPosts, &s.TotalImpressions, &s.TotalLikes, &s.TotalReposts, &s.TotalComments,
		&s.NewUsersToday, &s.NewUsersWeek, &s.NewPostsToday, &s.NewPostsWeek, &s.ActiveUsersDay, &s.ActiveUsersWeek)
	if err != nil {
		return nil, fmt.Errorf("site analytics: %w", err)
	}
	return s, nil
}
