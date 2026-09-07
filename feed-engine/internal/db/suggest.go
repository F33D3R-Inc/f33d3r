package db

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/lib/pq"
)

// ── Who may be suggested ─────────────────────────────────────────────────────
//
// This file is the ONE definition of "this account is worth putting in front of
// somebody who has not asked for it". Every unprompted people surface — the
// right rail's "Who to follow", the rail's "Creators to follow", the People tab
// of search with no query typed — resolves through SuggestPeople and therefore
// through the same bar. A new surface cannot ship without the bar because there
// is no second query to copy.
//
// It exists because there used to be no bar at all. The rail's suggestion query
// selected FROM users with three exclusions — self, already-followed, blocked —
// and recommended whatever was left. Anything that could hold a row in users was
// therefore a recommendation: automation probes with no avatar, no posts and no
// followers rendered as a bare coloured initial at the top of the owner's rail,
// above real people. Deleting those accounts would have fixed nothing; the next
// empty account, including a real person who signed up ten seconds ago, walked
// the identical path.
//
// A suggestion is a claim the platform makes on its own authority: "follow this
// person". The bar is what makes the claim honest.
//
//   ALIVE       — the account is somewhere a follow can still land. Not
//                 deactivated, not banned, not serving a suspension or a
//                 cooldown. Enforcement lives in two places and both are read:
//                 users.deactivated_at / users.role is the account lane,
//                 trust_scores.enforcement_state / cooldown_until is the PIAL
//                 lane. Recommending an account the platform has itself switched
//                 off is the platform contradicting itself.
//
//   PRESENTABLE — the account has an avatar. The suggestion row is a portrait
//                 row: a face, a name, a follow button. With no avatar it draws
//                 a coloured letter, which is the exact garbage this bar exists
//                 to stop. An avatar is also the clearest single proof that a
//                 human configured the account, because nothing sets one for
//                 you. A display name deliberately does NOT qualify on its own:
//                 every render path derives one from the handle when the column
//                 is empty, so "has a display name" is true of every row in the
//                 table and is not a signal.
//
//   PUBLISHED   — the account has at least one visible, non-reply work. There
//                 has to be something to arrive at. Following an empty profile
//                 is a wasted follow, and the same moderation predicate the
//                 feeds read is used here, so a person whose only work is
//                 blocked, deleted, expired or still scheduled is correctly not
//                 yet suggestible.
//
//   PERMITTED   — the viewer is allowed to be shown this account: age isolation
//                 both ways (minors see only minors, adults never see minors),
//                 and adult creators only to a viewer who has opted in. This is
//                 the same rule the feeds and search already enforce; before
//                 convergence the rail was the one surface that did not.
//
//   RELEVANT    — not the viewer, not already followed, not blocked in either
//                 direction, not muted by the viewer. Suggesting somebody the
//                 viewer has already made a decision about is noise.
//
// Everything above is a signal an account earns or loses by its own state. None
// of it is a list of names, so nothing here has to be maintained when the next
// probe account is created.

// SuggestedUser is one person the platform is prepared to recommend. It is the
// row type for every suggestion surface, so a surface cannot render a person the
// bar has not passed.
type SuggestedUser struct {
	ID            string
	Handle        string
	DisplayName   string
	AvatarURL     string
	FollowerCount int    // counted from the follows edges, not the drifting cache
	PIALID        string // required for D-070 follow event: {"event_type":"follow","target_pial":"..."}
	Realm         int    // effective standing as written by internal/realm; drives the ring
	IsVerified    bool   // PIAL kyc_tier=='full' — the documented-identity badge
	IsCreator     bool
	ViewerFollows bool // resolved by FollowStateFor — the row renders its own follow state
}

// SuggestionViewer is the person being suggested TO.
//
// ID is an account uuid. Anything else — the anonymous "demo_user" sentinel, a
// dev-build handle — means "no account", and every viewer-relative clause is
// then left out of the statement entirely rather than handed to Postgres as a
// uuid parameter. Handing it over is what produced
//
//	[rail] suggested users for demo_user: pq: invalid input syntax for type uuid: "demo_user"
//
// on every rail render for a logged-out visitor: the whole statement aborted, so
// the panel was empty for exactly the audience that has nobody to follow yet.
// A viewer with no account simply has nobody to exclude.
type SuggestionViewer struct {
	ID                string
	IsMinor           bool
	ShowAdultCreators bool
}

// HasAccount reports whether the viewer is a real account the viewer-relative
// clauses can be written against.
func (v SuggestionViewer) HasAccount() bool { return uuidShapeRe.MatchString(v.ID) }

// SuggestionQuery is what a surface wants out of the one suggestion set.
//
// CreatorsOnly narrows the same bar to creator accounts — the rail's "Creators
// to follow" panel is a subset of who-to-follow, never a second bar.
// ExcludeIDs lets a surface that renders two suggestion panels keep the same
// face out of both.
type SuggestionQuery struct {
	Limit        int
	CreatorsOnly bool
	ExcludeIDs   []string
}

// maxSuggestionLimit caps what any surface can ask for. A suggestion panel is a
// handful of people; an unbounded limit is a user dump.
const maxSuggestionLimit = 50

// defaultSuggestionLimit is what a surface gets if it names no limit.
const defaultSuggestionLimit = 3

// ── The bar, clause by clause ────────────────────────────────────────────────

// suggestAliveSQL — the account is still a place a follow can land.
const suggestAliveSQL = `u.deactivated_at IS NULL
	  AND COALESCE(u.role, '') <> 'banned'
	  AND COALESCE(ts.enforcement_state, 'none') NOT IN ('suspended', 'terminated')
	  AND (ts.cooldown_until IS NULL OR ts.cooldown_until <= NOW())`

// suggestPresentableSQL — the portrait row has a portrait.
const suggestPresentableSQL = `TRIM(COALESCE(p.avatar_url, '')) <> ''`

// suggestPublishedSQL — there is something to arrive at. Same moderation and
// publication predicates the feeds read, so "visible" means one thing service-wide.
const suggestPublishedSQL = `EXISTS (
	      SELECT 1
	      FROM works w
	      WHERE w.author_id = u.id
	        AND w.deleted_at IS NULL
	        AND w.kind <> 'reply'
	        AND ` + workNotBlockedSQL + `
	        AND ` + workLiveSQL + `
	        AND (w.scheduled_at IS NULL OR w.scheduled_at <= NOW())
	  )`

// suggestOrderSQL is the ranking, and it is a strict lexicographic list of real
// signals rather than a weighted score nobody can reason about:
//
//  1. followers — counted live from the follows edges. The audience's own
//     verdict is the strongest available evidence that a stranger is worth
//     following. It is counted rather than read from user_profiles.follower_count
//     because that cached column has measurably drifted (an account with five
//     cached followers and two real edges, a probe with one cached follower and
//     none at all), and a ranking built on a drifted counter is a ranking built
//     on a lie. There is an admin resync for the cache; the ranking should not
//     depend on when it was last run.
//  2. realm — the account's effective standing as written by internal/realm,
//     the platform's own earned-and-granted ladder. Read, never recomputed:
//     realm.Effective is the one definition and this query does not take a max()
//     of its own. It separates accounts the audience has not separated yet.
//  3. last work — among otherwise equal accounts, prefer the one still posting.
//     A dormant account is a wasted slot. Recency is a tiebreak and not a
//     primary key on purpose: a well-followed account that has not posted this
//     month is still a better suggestion than a stranger who posted this morning.
//  4. handle — a total order. Handles are unique, so the panel is stable across
//     renders and across replicas, and a test can pin it.
const suggestOrderSQL = `ORDER BY fc.followers DESC,
		 COALESCE(u.realm, 1) DESC,
		 wk.last_at DESC NULLS LAST,
		 u.handle ASC`

// SuggestPeople returns the people this viewer may be shown, best first.
//
// It is the only people-suggestion query in the service. A caller that wants a
// different slice of the same set narrows it through SuggestionQuery; a caller
// that wants a different bar is wrong.
func SuggestPeople(database *sql.DB, viewer SuggestionViewer, q SuggestionQuery) ([]SuggestedUser, error) {
	if database == nil {
		return nil, nil
	}
	query, args := buildSuggestionQuery(viewer, q)

	rows, err := database.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SuggestedUser
	for rows.Next() {
		var s SuggestedUser
		if err := rows.Scan(&s.ID, &s.Handle, &s.DisplayName, &s.AvatarURL,
			&s.FollowerCount, &s.PIALID, &s.Realm, &s.IsVerified, &s.IsCreator); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// The set already excludes people the viewer follows, so this resolves to
	// all-false today. It is still populated from the one follow-state owner
	// rather than assumed: the row renders a real follow state, so if a surface
	// ever reuses these rows outside the suggestion set, the viewer can never be
	// shown "Follow" for somebody they already follow.
	ids := make([]string, 0, len(out))
	for _, s := range out {
		ids = append(ids, s.ID)
	}
	state, err := FollowStateFor(database, viewer.ID, ids)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].ViewerFollows = state[out[i].ID]
	}
	return out, nil
}

// buildSuggestionQuery composes the one suggestion statement and its arguments.
//
// It is separated from the execution so the bar itself can be asserted on
// without a database: a test can read the statement a given viewer would
// actually be served and prove every clause of the bar is in it, and prove that
// a viewer with no account contributes no uuid parameter.
func buildSuggestionQuery(viewer SuggestionViewer, q SuggestionQuery) (string, []interface{}) {
	limit := q.Limit
	if limit <= 0 {
		limit = defaultSuggestionLimit
	}
	if limit > maxSuggestionLimit {
		limit = maxSuggestionLimit
	}

	var args []interface{}
	arg := func(v interface{}) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}

	where := []string{
		suggestAliveSQL,
		suggestPresentableSQL,
		suggestPublishedSQL,
	}

	// Age isolation — the same two-way rule the feeds and search enforce.
	if viewer.IsMinor {
		where = append(where, `COALESCE(pr.is_minor, FALSE) = TRUE`)
	} else {
		where = append(where, `COALESCE(pr.is_minor, FALSE) = FALSE`)
	}
	// Adult creators are shown only to a viewer who has opted in.
	if !viewer.ShowAdultCreators {
		where = append(where, `COALESCE(p.is_adult_creator, FALSE) = FALSE`)
	}
	if q.CreatorsOnly {
		where = append(where, `COALESCE(p.is_creator, FALSE) = TRUE`)
	}

	// Viewer-relative exclusions. Written only for a real account: a viewer with
	// no account has nobody to exclude, and its identifier never reaches a uuid
	// column.
	if viewer.HasAccount() {
		v := arg(viewer.ID)
		where = append(where,
			`u.id <> `+v+`::uuid`,
			`NOT EXISTS (SELECT 1 FROM follows f WHERE f.follower_id = `+v+`::uuid AND f.following_id = u.id)`,
			`NOT EXISTS (
		      SELECT 1 FROM blocks b
		      WHERE (b.blocker_id = `+v+`::uuid AND b.blocked_id = u.id)
		         OR (b.blocker_id = u.id AND b.blocked_id = `+v+`::uuid)
		  )`,
			`NOT EXISTS (SELECT 1 FROM user_mutes m WHERE m.muter_id = `+v+`::uuid AND m.muted_id = u.id)`,
		)
	}

	// Ids another panel on the same surface has already spent. Non-account ids
	// are dropped rather than cast, for the same reason the viewer id is.
	excluded := make([]string, 0, len(q.ExcludeIDs))
	for _, id := range q.ExcludeIDs {
		if uuidShapeRe.MatchString(id) {
			excluded = append(excluded, id)
		}
	}
	if len(excluded) > 0 {
		where = append(where, `u.id <> ALL(`+arg(pq.Array(excluded))+`::uuid[])`)
	}

	query := `
		SELECT u.id::text,
		       u.handle,
		       COALESCE(NULLIF(TRIM(p.display_name), ''), INITCAP(REPLACE(u.handle, '_', ' '))),
		       p.avatar_url,
		       fc.followers,
		       COALESCE(u.pial_id::text, ''),
		       COALESCE(u.realm, 1),
		       (COALESCE(pr.kyc_tier, 'none') = 'full'),
		       COALESCE(p.is_creator, FALSE)
		FROM users u
		JOIN user_profiles p ON p.user_id = u.id
		LEFT JOIN pial_roots pr ON pr.pial_id = u.pial_id
		LEFT JOIN trust_scores ts ON ts.pial_id = u.pial_id
		LEFT JOIN LATERAL (
		    SELECT COUNT(*)::int AS followers
		    FROM follows f2 WHERE f2.following_id = u.id
		) fc ON TRUE
		LEFT JOIN LATERAL (
		    SELECT MAX(w.created_at) AS last_at
		    FROM works w
		    WHERE w.author_id = u.id
		      AND w.deleted_at IS NULL
		      AND w.kind <> 'reply'
		      AND ` + workNotBlockedSQL + `
		      AND ` + workLiveSQL + `
		      AND (w.scheduled_at IS NULL OR w.scheduled_at <= NOW())
		) wk ON TRUE
		WHERE ` + strings.Join(where, "\n		  AND ") + `
		` + suggestOrderSQL + `
		LIMIT ` + arg(limit)

	return query, args
}
