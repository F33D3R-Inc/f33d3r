package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	_ "github.com/lib/pq"
)

// ─── the bar, pinned without a database ──────────────────────────────────────
//
// These read the statement a given viewer would actually be served. They are
// what stops the quality bar being quietly weakened: a clause removed from
// SuggestPeople fails here, and a second suggestion query added to the package
// fails here too.

const realViewerID = "0536d346-e7a5-4e73-9252-b4791c4f4ffb"

// TestEverySuggestionCarriesTheWholeBar proves the bar is not optional. Whatever
// a surface asks for — three rows or twenty, creators or everybody, signed in or
// not — the same alive/presentable/published clauses are in the statement.
func TestEverySuggestionCarriesTheWholeBar(t *testing.T) {
	cases := []struct {
		name   string
		viewer SuggestionViewer
		query  SuggestionQuery
	}{
		{"rail, signed in", SuggestionViewer{ID: realViewerID, ShowAdultCreators: true}, SuggestionQuery{Limit: 3}},
		{"rail, logged out", SuggestionViewer{ID: "demo_user"}, SuggestionQuery{Limit: 3}},
		{"creators panel", SuggestionViewer{ID: realViewerID}, SuggestionQuery{Limit: 3, CreatorsOnly: true}},
		{"search people tab", SuggestionViewer{ID: realViewerID}, SuggestionQuery{Limit: 20}},
		{"minor viewer", SuggestionViewer{ID: realViewerID, IsMinor: true}, SuggestionQuery{}},
		{"no options at all", SuggestionViewer{}, SuggestionQuery{}},
	}
	required := map[string]string{
		"alive (not deactivated, banned, suspended or cooling down)": suggestAliveSQL,
		"presentable (has an avatar)":                                suggestPresentableSQL,
		"published (has a visible non-reply work)":                   suggestPublishedSQL,
	}
	for _, c := range cases {
		stmt, _ := buildSuggestionQuery(c.viewer, c.query)
		for what, clause := range required {
			if !strings.Contains(stmt, clause) {
				t.Errorf("%s: the suggestion statement is missing the %s clause.\n"+
					"Every unprompted people surface goes through one bar. If this "+
					"clause is genuinely wrong, change it in suggest.go for everybody "+
					"— do not make it conditional on the caller.\nwant:\n%s\ngot:\n%s",
					c.name, what, clause, stmt)
			}
		}
	}
}

// TestAgeIsolationIsAlwaysDecided proves neither age direction can be forgotten:
// a minor viewer is shown only minors, and everybody else is never shown one.
func TestAgeIsolationIsAlwaysDecided(t *testing.T) {
	adult, _ := buildSuggestionQuery(SuggestionViewer{ID: realViewerID}, SuggestionQuery{})
	if !strings.Contains(adult, `COALESCE(pr.is_minor, FALSE) = FALSE`) {
		t.Error("an adult viewer's suggestion set does not exclude minor accounts")
	}
	minor, _ := buildSuggestionQuery(SuggestionViewer{ID: realViewerID, IsMinor: true}, SuggestionQuery{})
	if !strings.Contains(minor, `COALESCE(pr.is_minor, FALSE) = TRUE`) {
		t.Error("a minor viewer's suggestion set is not isolated to minor accounts")
	}
}

// TestAdultCreatorsAreOptIn proves the rail cannot suggest an adult creator to a
// viewer who has not opted in. The rail used to be the one people surface that
// never asked the question the feeds and search both ask.
func TestAdultCreatorsAreOptIn(t *testing.T) {
	const clause = `COALESCE(p.is_adult_creator, FALSE) = FALSE`
	closed, _ := buildSuggestionQuery(SuggestionViewer{ID: realViewerID}, SuggestionQuery{})
	if !strings.Contains(closed, clause) {
		t.Error("a viewer who has not opted in can be suggested an adult creator")
	}
	opened, _ := buildSuggestionQuery(SuggestionViewer{ID: realViewerID, ShowAdultCreators: true}, SuggestionQuery{})
	if strings.Contains(opened, clause) {
		t.Error("a viewer who HAS opted in is still having adult creators withheld")
	}
}

// TestAViewerWithNoAccountNeverReachesAUuidColumn is the demo_user bug, closed.
//
// The anonymous viewer is the "demo_user" sentinel. Sent to Postgres as a uuid
// parameter it aborts the entire statement — "pq: invalid input syntax for type
// uuid: \"demo_user\"" on every rail render for a logged-out visitor — so the
// panel was empty for exactly the audience with nobody to follow yet. It is
// rejected before the query now: a viewer with no account simply has no
// viewer-relative clauses.
func TestAViewerWithNoAccountNeverReachesAUuidColumn(t *testing.T) {
	for _, id := range []string{"demo_user", "", "you", "not-a-uuid"} {
		stmt, args := buildSuggestionQuery(SuggestionViewer{ID: id}, SuggestionQuery{Limit: 3})
		for _, a := range args {
			if s, ok := a.(string); ok && s == id && id != "" {
				t.Errorf("viewer %q was bound as a query argument; it is not an account id", id)
			}
		}
		if strings.Contains(stmt, "::uuid") && strings.Contains(stmt, "user_mutes") {
			t.Errorf("viewer %q produced viewer-relative uuid clauses:\n%s", id, stmt)
		}
	}

	// A real account keeps every viewer-relative exclusion.
	stmt, args := buildSuggestionQuery(SuggestionViewer{ID: realViewerID}, SuggestionQuery{Limit: 3})
	for _, want := range []string{"u.id <> $1::uuid", "FROM follows f", "FROM blocks b", "FROM user_mutes m"} {
		if !strings.Contains(stmt, want) {
			t.Errorf("a signed-in viewer's suggestion set is missing %q:\n%s", want, stmt)
		}
	}
	if len(args) == 0 || args[0] != realViewerID {
		t.Errorf("args = %v, want the viewer id first", args)
	}
}

// TestSuggestionOrderIsRankedOnRealSignals pins the ranking. With a quality bar
// in place the order is the rest of the job: an arbitrary order among qualified
// accounts is still the platform declining to have an opinion.
//
// It also pins what the ranking must NOT read. user_profiles.follower_count is a
// cache that has measurably drifted from the follows edges, and a ranking built
// on it is a ranking built on a stale number.
func TestSuggestionOrderIsRankedOnRealSignals(t *testing.T) {
	stmt, _ := buildSuggestionQuery(SuggestionViewer{ID: realViewerID}, SuggestionQuery{})
	for _, key := range []string{
		"fc.followers DESC",         // the audience's verdict, counted from the edges
		"COALESCE(u.realm, 1) DESC", // earned standing, as written by internal/realm
		"wk.last_at DESC",           // still posting
		"u.handle ASC",              // a total order, so the panel is stable
	} {
		if !strings.Contains(stmt, key) {
			t.Errorf("the suggestion ranking no longer uses %s", key)
		}
	}
	order := stmt[strings.Index(stmt, "ORDER BY"):]
	if strings.Contains(order, "p.follower_count") {
		t.Error("the ranking reads the cached user_profiles.follower_count; " +
			"count the follows edges instead — the cache drifts")
	}
	if strings.Contains(order, "RANDOM()") {
		t.Error("the ranking is random; qualified accounts deserve an opinion")
	}
}

// TestSuggestPeopleIsTheOnlyPeopleSuggestionQuery is the structural half of the
// fix. Convergence only holds while there is one query to converge on, so a
// second one reintroduced into this package fails here — including the two this
// replaced, GetSuggestedUsersFiltered and GetTrendingCreators, each of which had
// its own idea of who was worth showing.
func TestSuggestPeopleIsTheOnlyPeopleSuggestionQuery(t *testing.T) {
	entryPoint := regexp.MustCompile(`func (GetSuggested\w*|GetTrendingCreators|\w*SuggestedUsers\w*)\(`)
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("globbing package sources: %v", err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("reading %s: %v", f, err)
		}
		if m := entryPoint.FindString(string(body)); m != "" {
			t.Errorf("%s declares %s — a second people-suggestion entry point. "+
				"Every unprompted people surface goes through SuggestPeople so it "+
				"cannot miss the quality bar; narrow the one set with SuggestionQuery "+
				"instead of adding another query.", f, strings.TrimSuffix(m, "("))
		}
	}
}

// TestSuggestionLimitIsBounded proves no surface can turn the suggestion panel
// into a dump of the users table.
func TestSuggestionLimitIsBounded(t *testing.T) {
	_, args := buildSuggestionQuery(SuggestionViewer{}, SuggestionQuery{Limit: 100000})
	if got := args[len(args)-1]; got != maxSuggestionLimit {
		t.Errorf("limit = %v, want it capped at %d", got, maxSuggestionLimit)
	}
	_, args = buildSuggestionQuery(SuggestionViewer{}, SuggestionQuery{})
	if got := args[len(args)-1]; got != defaultSuggestionLimit {
		t.Errorf("unset limit = %v, want the default %d", got, defaultSuggestionLimit)
	}
}

// TestExcludedIdsAreAccountIdsOnly proves the same guard the viewer id gets
// applies to the ids one panel spends on behalf of another.
func TestExcludedIdsAreAccountIdsOnly(t *testing.T) {
	stmt, args := buildSuggestionQuery(SuggestionViewer{}, SuggestionQuery{
		ExcludeIDs: []string{"demo_user", "", "railprobe"},
	})
	if strings.Contains(stmt, "ALL(") {
		t.Errorf("non-account ids reached a uuid[] parameter:\n%s", stmt)
	}
	stmt, args = buildSuggestionQuery(SuggestionViewer{}, SuggestionQuery{
		ExcludeIDs: []string{realViewerID, "demo_user"},
	})
	if !strings.Contains(stmt, "u.id <> ALL($1::uuid[])") {
		t.Errorf("a real excluded id was dropped:\n%s", stmt)
	}
	if len(args) != 2 {
		t.Errorf("args = %v, want the id array and the limit", args)
	}
}

// ─── schema-backed proof ─────────────────────────────────────────────────────
//
// These need a throwaway PostgreSQL named by SUGGEST_TEST_DATABASE_URL. They
// apply the real embedded migrations and build real accounts, so they prove the
// bar against the actual schema rather than against the text of the statement.
// Never point this at a database that holds real rows.

func suggestTestDB(t *testing.T) *sql.DB {
	t.Helper()
	url := os.Getenv("SUGGEST_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("SUGGEST_TEST_DATABASE_URL not set — schema-backed suggestion tests skipped")
	}
	database, err := sql.Open("postgres", url)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if err := database.Ping(); err != nil {
		database.Close()
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Errorf("closing test database: %v", err)
		}
	})
	MigrateUp(database)
	return database
}

// execOrFail runs one setup statement and stops the test if the schema refuses
// it. A silently swallowed setup error makes a bar look like it is holding when
// nothing was ever set up to test it.
func execOrFail(t *testing.T, database *sql.DB, stmt string, args ...interface{}) {
	t.Helper()
	if _, err := database.Exec(stmt, args...); err != nil {
		t.Fatalf("%s: %v", strings.TrimSpace(stmt), err)
	}
}

// TestAnEmptyAccountIsNeverSuggested is the defect itself, reproduced against
// the real schema and closed: an account with no avatar and no works — which is
// every automation probe, and every real person ten seconds after signup — is
// not a recommendation.
func TestAnEmptyAccountIsNeverSuggested(t *testing.T) {
	database := suggestTestDB(t)
	viewer := makeAccount(t, database, "sugg_viewer", accountOpts{Avatar: true, Works: 1})

	empty := makeAccount(t, database, "sugg_empty", accountOpts{})
	nameOnly := makeAccount(t, database, "sugg_nameonly", accountOpts{DisplayName: "Totally Real"})
	avatarOnly := makeAccount(t, database, "sugg_avataronly", accountOpts{Avatar: true})
	worksOnly := makeAccount(t, database, "sugg_worksonly", accountOpts{Works: 2})
	complete := makeAccount(t, database, "sugg_complete", accountOpts{Avatar: true, Works: 2, DisplayName: "Real Person"})

	got := suggestedIDs(t, database, viewer)

	for name, id := range map[string]string{
		"an empty account":                empty,
		"an account with only a name":     nameOnly,
		"an account with only an avatar":  avatarOnly,
		"an account with only some works": worksOnly,
		"the viewer themselves":           viewer,
	} {
		if got[id] {
			t.Errorf("%s was suggested; the quality bar did not hold", name)
		}
	}
	if !got[complete] {
		t.Error("a complete, active, posting account was NOT suggested — the bar is now too high")
	}
}

// TestTheBarFallsWithTheAccount proves each half of ALIVE actually removes an
// otherwise-qualified account, through the platform's own mechanisms.
func TestTheBarFallsWithTheAccount(t *testing.T) {
	database := suggestTestDB(t)
	viewer := makeAccount(t, database, "sugg_viewer2", accountOpts{Avatar: true, Works: 1})
	target := makeAccount(t, database, "sugg_target", accountOpts{Avatar: true, Works: 1})

	if !suggestedIDs(t, database, viewer)[target] {
		t.Fatal("the target does not clear the bar to begin with")
	}

	execOrFail(t, database, `UPDATE users SET deactivated_at = NOW() WHERE id = $1`, target)
	if suggestedIDs(t, database, viewer)[target] {
		t.Error("a deactivated account is still suggested")
	}
	execOrFail(t, database, `UPDATE users SET deactivated_at = NULL WHERE id = $1`, target)

	execOrFail(t, database, `UPDATE users SET role = 'banned' WHERE id = $1`, target)
	if suggestedIDs(t, database, viewer)[target] {
		t.Error("a banned account is still suggested")
	}
	execOrFail(t, database, `UPDATE users SET role = 'user' WHERE id = $1`, target)

	execOrFail(t, database, `UPDATE user_profiles SET avatar_url = '' WHERE user_id = $1`, target)
	if suggestedIDs(t, database, viewer)[target] {
		t.Error("an account with no avatar is still suggested")
	}
	execOrFail(t, database, `UPDATE user_profiles SET avatar_url = '/static/x.webp' WHERE user_id = $1`, target)

	execOrFail(t, database, `UPDATE works SET deleted_at = NOW() WHERE author_id = $1`, target)
	if suggestedIDs(t, database, viewer)[target] {
		t.Error("an account whose only work is deleted is still suggested")
	}
	execOrFail(t, database, `UPDATE works SET deleted_at = NULL WHERE author_id = $1`, target)

	execOrFail(t, database, `INSERT INTO follows (follower_id, following_id) VALUES ($1, $2)`, viewer, target)
	if suggestedIDs(t, database, viewer)[target] {
		t.Error("somebody the viewer already follows is still suggested")
	}
	execOrFail(t, database, `DELETE FROM follows WHERE follower_id = $1 AND following_id = $2`, viewer, target)

	if !suggestedIDs(t, database, viewer)[target] {
		t.Error("the target stopped clearing the bar after every condition was restored")
	}
}

// TestALoggedOutViewerGetsSuggestionsNotAnError is the other half of the
// demo_user fix, proved end to end: the anonymous viewer gets the real set.
func TestALoggedOutViewerGetsSuggestionsNotAnError(t *testing.T) {
	database := suggestTestDB(t)
	complete := makeAccount(t, database, "sugg_public", accountOpts{Avatar: true, Works: 1})

	rows, err := SuggestPeople(database, SuggestionViewer{ID: "demo_user"}, SuggestionQuery{Limit: 10})
	if err != nil {
		t.Fatalf("the anonymous viewer errored instead of being served: %v", err)
	}
	found := false
	for _, r := range rows {
		if r.ID == complete {
			found = true
		}
	}
	if !found {
		t.Error("the anonymous viewer was served no suggestions at all")
	}
}

func suggestedIDs(t *testing.T, database *sql.DB, viewerID string) map[string]bool {
	t.Helper()
	rows, err := SuggestPeople(database, SuggestionViewer{ID: viewerID, ShowAdultCreators: true},
		SuggestionQuery{Limit: maxSuggestionLimit})
	if err != nil {
		t.Fatalf("SuggestPeople: %v", err)
	}
	out := map[string]bool{}
	for _, r := range rows {
		out[r.ID] = true
	}
	return out
}

type accountOpts struct {
	Avatar      bool
	DisplayName string
	Works       int
}

// makeAccount builds one account in whatever state the case needs and removes it
// again when the test ends.
func makeAccount(t *testing.T, database *sql.DB, handle string, o accountOpts) string {
	t.Helper()
	execOrFail(t, database, `DELETE FROM users WHERE handle = $1`, handle)

	var pial string
	if err := database.QueryRow(`
		INSERT INTO pial_roots (pial_id) VALUES (gen_random_uuid()) RETURNING pial_id
	`).Scan(&pial); err != nil {
		t.Fatalf("creating a PIAL for %s: %v", handle, err)
	}
	var id string
	if err := database.QueryRow(`
		INSERT INTO users (handle, pial_id) VALUES ($1, $2::uuid) RETURNING id::text
	`, handle, pial).Scan(&id); err != nil {
		t.Fatalf("creating %s: %v", handle, err)
	}
	t.Cleanup(func() {
		database.Exec(`DELETE FROM users WHERE handle = $1`, handle)
		database.Exec(`DELETE FROM pial_roots WHERE pial_id = $1::uuid`, pial)
	})

	avatar := ""
	if o.Avatar {
		avatar = "/static/media/avatars/" + handle + ".webp"
	}
	execOrFail(t, database, `
		INSERT INTO user_profiles (user_id, display_name, avatar_url) VALUES ($1, $2, $3)
	`, id, o.DisplayName, avatar)

	for i := 0; i < o.Works; i++ {
		execOrFail(t, database, `
			INSERT INTO works (cid, author_id, author_pial, body, kind, scan_state)
			VALUES ($1, $2, $3::uuid, $4, 'post', 'clean')
		`, fmt.Sprintf("cid-%s-%d", handle, i), id, pial, "work "+handle)
	}
	return id
}
