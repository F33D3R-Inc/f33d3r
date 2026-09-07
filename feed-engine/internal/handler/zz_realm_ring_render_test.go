package handler

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/f33d3r/feed-engine/internal/model"
	"github.com/f33d3r/feed-engine/internal/realm"
)

// The realm ring has exactly one authority — realm.Index, reached from the
// avatar Facet through realmRing — and this file is the proof that every
// surface drawing a person goes through it.
//
// The bug this pins shut: realm used to be an argument every render site had to
// carry, so a new surface was correct only if whoever wrote it remembered. A
// surface added after this test either draws user_avatar, and is right for
// free, or draws its own markup, and is caught by TestEveryAvatarIsTheOneFacet.

const (
	ringHandleR5 = "ringtest_guardian"
	ringHandleR3 = "ringtest_seeker"
	ringHandleR1 = "ringtest_wanderer"
)

// realmRingTestHandler builds the real template set and seeds the realm index
// with three accounts: one at the top of the scale, one in the middle, one on
// the floor (by being absent, which is what the floor means).
func realmRingTestHandler(t *testing.T) *Handler {
	t.Helper()
	h := liveTestHandler(t)
	ix := realm.Start(context.Background(), nil, 0)
	ix.Note(ringHandleR5, 5)
	ix.Note(ringHandleR3, 3)
	return h
}

func renderRingFacet(t *testing.T, h *Handler, name string, data interface{}) string {
	t.Helper()
	var b strings.Builder
	if err := h.partial.ExecuteTemplate(&b, name, data); err != nil {
		t.Fatalf("%s: render: %v", name, err)
	}
	return b.String()
}

// TestEverySurfaceDrawsTheAuthorsRealm renders every Facet that draws a person
// and asserts the ring matches the realm the index holds — with no surface
// passing a realm in, because no surface can.
func TestEverySurfaceDrawsTheAuthorsRealm(t *testing.T) {
	h := realmRingTestHandler(t)

	now := time.Now()
	work := func(handle string) *model.Work {
		return &model.Work{
			ID: "11111111-1111-1111-1111-111111111111", CID: "cid1",
			AuthorID:     "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
			AuthorPIAL:   "22222222-2222-2222-2222-222222222222",
			AuthorHandle: handle, AuthorName: "Ring Test",
			Body: "a work", Kind: "work", CreatedAt: now, TimeAgo: "1m",
		}
	}
	user := func(handle string) *model.User {
		return &model.User{
			ID:     "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
			PIALID: "22222222-2222-2222-2222-222222222222",
			Handle: handle, DisplayName: "Ring Test",
		}
	}

	// Each case names a surface, the Facet that draws it, and how that Facet is
	// handed a person. The realm is never among the inputs — that is the point.
	cases := []struct {
		surface string
		facet   string
		data    func(handle string) interface{}
	}{
		{"profile header", "profile_header", func(h string) interface{} {
			return map[string]interface{}{
				"ProfileUser": user(h), "IsAnon": true, "ProfileIsOnline": false,
				"IsOwner": false, "User": user("viewer"),
			}
		}},
		{"works feed card", "work_card", func(h string) interface{} {
			return map[string]interface{}{"W": work(h), "Ctx": nil}
		}},
		{"work detail focus", "work_detail_focus", func(h string) interface{} {
			return map[string]interface{}{"W": work(h), "Ctx": nil}
		}},
		{"work focus sidebar", "work_focus_sidebar", func(h string) interface{} {
			return map[string]interface{}{"W": work(h), "Ctx": nil}
		}},
		{"post avatar", "post_avatar", func(h string) interface{} {
			return map[string]interface{}{"Handle": h, "AvatarURL": "", "AuthorName": "Ring Test", "IsOnline": false}
		}},
		{"quoted work", "quoted_work", func(h string) interface{} {
			return map[string]interface{}{
				"AuthorHandle": h, "AuthorName": "Ring Test", "AvatarURL": "",
				"ID": "q1", "Body": "quoted", "TimeAgo": "2m",
			}
		}},
		{"quoted work nested", "quoted_work_nested", func(h string) interface{} {
			return map[string]interface{}{
				"AuthorHandle": h, "AuthorName": "Ring Test", "AvatarURL": "",
				"ID": "q1", "Body": "quoted", "TimeAgo": "2m",
			}
		}},
		{"follow list row", "follow_list_row", func(h string) interface{} {
			return map[string]interface{}{
				"Handle": h, "DisplayName": "Ring Test", "AvatarURL": "",
				"PIALID": "22222222-2222-2222-2222-222222222222", "Bio": "",
			}
		}},
		{"suggested user row", "suggested_user_row", func(h string) interface{} {
			return map[string]interface{}{
				"Handle": h, "DisplayName": "Ring Test", "AvatarURL": "",
				"PIALID": "22222222-2222-2222-2222-222222222222", "FollowerCount": 3,
			}
		}},
		{"user list sheet", "user_list_sheet", func(h string) interface{} {
			return map[string]interface{}{
				"Title": "Likes",
				"Users": []map[string]interface{}{{
					"Handle": h, "DisplayName": "Ring Test", "AvatarURL": "",
					"PIALID": "22222222-2222-2222-2222-222222222222",
				}},
			}
		}},
		{"account switcher", "account_switcher", func(h string) interface{} {
			return map[string]interface{}{
				"LinkedAccounts": []map[string]interface{}{{
					"Handle": h, "DisplayName": "Ring Test", "AvatarURL": "", "IsActive": true,
					"ID": "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "IsPrimary": true,
				}},
			}
		}},
		{"nav account", "nav_account", func(h string) interface{} {
			return map[string]interface{}{"User": user(h)}
		}},
		{"compose modal", "compose_modal", func(h string) interface{} {
			return map[string]interface{}{"User": user(h)}
		}},
		{"reply compose row", "reply_compose_row", func(h string) interface{} {
			return map[string]interface{}{
				"User": user(h), "PostID": "p1", "AuthorHandle": h, "AuthorName": "Ring Test",
			}
		}},
		{"compose quote row", "compose_quote_row", func(h string) interface{} {
			return map[string]interface{}{
				"ID": "p1", "AuthorHandle": h, "AvatarURL": "", "Body": "body", "TimeAgo": "3m",
			}
		}},
		{"search person row", "search_person_row", func(h string) interface{} {
			return map[string]interface{}{
				"Handle": h, "DisplayName": "Ring Test", "AvatarURL": "", "IsVerified": false,
			}
		}},
		{"creators rail", "rail_trending_creators", func(h string) interface{} {
			return map[string]interface{}{
				"RailCreators": []map[string]interface{}{{
					"Handle": h, "DisplayName": "Ring Test", "AvatarURL": "",
					"FollowerCount": 9, "IsVerified": false, "IsCreator": true,
				}},
			}
		}},
		{"leaderboard row", "leaderboard_row", func(h string) interface{} {
			return map[string]interface{}{
				"Rank": 1, "Handle": h, "DisplayName": "Ring Test", "AvatarURL": "", "XPToday": int64(40),
			}
		}},
		{"notification", "notif_item", func(h string) interface{} {
			return map[string]interface{}{
				"Type": "follow", "ActorHandle": h, "ActorName": "Ring Test", "ActorAvatar": "",
				"IsRead": true, "TargetID": "", "TimeAgo": "now", "CreatedAt": now,
			}
		}},
		{"org member row", "org_member_row", func(h string) interface{} {
			return map[string]interface{}{
				"Row": map[string]interface{}{
					"MembershipID": "m1", "Handle": h, "DisplayName": "Ring Test", "AvatarURL": "",
				},
				"Mode": "approved",
			}
		}},
		{"avatar group", "avatar_group", func(h string) interface{} {
			return map[string]interface{}{
				"Users": []map[string]interface{}{{"Handle": h, "AvatarURL": ""}},
			}
		}},
		{"reply preview facepile", "reply_preview", func(h string) interface{} {
			return map[string]interface{}{
				"ID": "p1", "Comments": 1,
				"LatestReplierHandles": []string{h},
				"LatestReplierAvatars": []string{""},
			}
		}},
		{"microconversation", "microconversation", func(h string) interface{} {
			return map[string]interface{}{
				"MC": &model.Microconversation{
					ConversationID: "p1", ReplyCount: 2, AccentClass: "mc-cobalt",
					Participants: []model.MicroconversationParticipant{{Handle: h}},
					SeedWork:     work(h),
					Exchanges:    []*model.Work{work(h)},
				},
			}
		}},
		{"moderation queue", "admin_panel_moderation", func(h string) interface{} {
			return map[string]interface{}{
				"ReviewQueue": []map[string]interface{}{{
					"post_id": "p1", "body": "b", "author_handle": h, "author_avatar": "",
					"scan_state": "human_review", "risk_level": "high", "created_at": now,
					"nudity_score": 0.1, "gore_score": 0.0, "clickbait_score": 0.0,
					"signals": "", "thumb_url": "",
				}},
			}
		}},
		{"DM conversation list", "gnosis_convo_item", func(h string) interface{} {
			return map[string]interface{}{
				"ConversationID": "c1", "OtherHandle": h, "OtherDisplay": "Ring Test",
				"OtherAvatar": "", "IsGroup": false, "Title": "", "Mode": "standard",
				"Preview": "hello", "Unread": 0,
			}
		}},
		{"DM thread head", "gnosis_thread_head", func(h string) interface{} {
			return map[string]interface{}{
				"Convo": map[string]interface{}{
					"ID": "c1", "Mode": "standard", "IsGroup": false, "Title": "",
				},
				"OtherHandle": h, "OtherDisplay": "Ring Test", "OtherAvatar": "",
				"Sealed": false, "ViewerID": "v1",
				"Messages": []map[string]interface{}{},
			}
		}},
		{"live card", "live_card", func(h string) interface{} {
			return liveRingTestView(h)
		}},
		{"live author row", "live_author_row", func(h string) interface{} {
			return liveRingTestView(h)
		}},
		{"vision card", "vision_card", func(h string) interface{} {
			return work(h)
		}},
		{"vision profile ring", "vision_profile_ring", func(h string) interface{} {
			return visionProfileRingView{
				AuthorID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
				Handle:   h, Name: "Ring Test",
			}
		}},
		{"article card", "article_card", func(h string) interface{} {
			return map[string]interface{}{
				"ID": "a1", "Slug": "s", "Title": "T", "AuthorHandle": h,
				"AuthorDisplay": "Ring Test", "AuthorAvatar": "", "CoverURL": "",
				"ViewCount": 0, "ReadMinutes": 3, "Excerpt": "e",
			}
		}},
		{"admin lookup", "admin_lookup_result", func(h string) interface{} {
			return map[string]interface{}{
				"LookupUser": map[string]interface{}{
					"Handle": h, "DisplayName": "Ring Test", "AvatarURL": "",
					"Role": "user", "OfficialType": "", "IsVerified": false, "IsCreator": false,
					"ID": "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "PIALID": "",
				},
			}
		}},
	}

	for _, c := range cases {
		for _, seed := range []struct {
			handle string
			want   string
		}{
			{ringHandleR5, "realm-ring--r5"},
			{ringHandleR3, "realm-ring--r3"},
		} {
			out := renderRingFacet(t, h, c.facet, c.data(seed.handle))
			if !strings.Contains(out, seed.want) {
				t.Errorf("%s (%s): handle at %s draws no %s", c.surface, c.facet, seed.want, seed.want)
			}
		}

		// The floor draws no ring at all — a defined default, not a colour.
		out := renderRingFacet(t, h, c.facet, c.data(ringHandleR1))
		if strings.Contains(out, "realm-ring") {
			t.Errorf("%s (%s): an account on the floor drew a ring", c.surface, c.facet)
		}
	}
}

// TestARenderSiteCannotOverrideTheRing proves the ring is not an input. A caller
// that passes a realm — as several surfaces still do, including live Facets this
// change does not own — is ignored, so a stale or wrong value in a view struct
// can no longer reach the page.
func TestARenderSiteCannotOverrideTheRing(t *testing.T) {
	h := realmRingTestHandler(t)

	out := renderRingFacet(t, h, "user_avatar", map[string]interface{}{
		"Handle": ringHandleR5, "DisplayName": "Ring Test", "Size": "sm",
		// Every wrong answer a call site has ever passed:
		"Realm": 1,
	})
	if !strings.Contains(out, "realm-ring--r5") {
		t.Error("a caller-supplied realm overrode the index")
	}

	out = renderRingFacet(t, h, "user_avatar", map[string]interface{}{
		"Handle": ringHandleR1, "DisplayName": "Ring Test", "Size": "sm",
		"Realm": 5,
	})
	if strings.Contains(out, "realm-ring") {
		t.Error("a caller-supplied realm invented standing the account has not earned")
	}
}

// TestTheRingSurvivesTheHandleItIsRenderedUnder pins the normalisation the index
// performs, because handles reach Facets in whatever case and form the surface
// that fetched them happened to use.
func TestTheRingSurvivesTheHandleItIsRenderedUnder(t *testing.T) {
	h := realmRingTestHandler(t)
	for _, handle := range []string{ringHandleR5, strings.ToUpper(ringHandleR5), "@" + ringHandleR5, " " + ringHandleR5 + " "} {
		out := renderRingFacet(t, h, "user_avatar", map[string]interface{}{
			"Handle": handle, "DisplayName": "Ring Test", "Size": "sm",
		})
		if !strings.Contains(out, "realm-ring--r5") {
			t.Errorf("handle %q lost its ring", handle)
		}
	}
}

// TestEveryAvatarIsTheOneFacet is the structural half of the fix.
//
// Resolving the ring inside user_avatar makes it impossible for a surface that
// CALLS user_avatar to get the ring wrong. This test closes the other door: a
// surface that draws its own <img src="{{…Avatar…}}"> or its own gradient-
// initials fallback has left the one avatar path, and an avatar off that path
// is an avatar with no ring — which is the exact bug this change exists to end.
//
// Adding a template to the allowlist is a deliberate act and needs a reason
// written next to it. "It was easier" is not one.
func TestEveryAvatarIsTheOneFacet(t *testing.T) {
	cwd, _ := os.Getwd()
	if err := os.Chdir("../.."); err != nil {
		t.Skipf("cannot chdir to repo root: %v", err)
	}
	t.Cleanup(func() { os.Chdir(cwd) })

	// Templates permitted to contain avatar markup of their own, and why.
	allowed := map[string]string{
		"partials/_user_avatar.html":        "is the one avatar Facet",
		"partials/_avatar_img.html":         "the bare image layer, drawn by the live lane's chat rows",
		"partials/_avatar_ring.html":        "documents the ring; contains no avatar",
		"partials/_skeleton_avatar.html":    "a loading placeholder standing for nobody",
		"partials/_edit_profile_modal.html": "the upload and crop control, addressed by element id",
		"partials/_org_badge.html":          "an organisation's mark, not a person's ring",
		"partials/_live_chat_row.html":      "owned by the live lane",
		"explore.html":                      "avatarColors seeds tag tiles, not people",
		"partials/_explore_canvas.html":     "the Explore Facet extracted from explore.html: the same tag tiles, not people",
	}

	var offenders []string
	err := filepath.Walk(filepath.Join("web", "templates"), func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".html") {
			return err
		}
		rel := strings.TrimPrefix(filepath.ToSlash(path), "web/templates/")
		if _, ok := allowed[rel]; ok {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		src := string(body)
		if avatarImgRe.MatchString(src) {
			offenders = append(offenders, rel+": draws its own <img> from an avatar field")
		}
		if strings.Contains(src, "avatarColors") {
			offenders = append(offenders, rel+": draws its own gradient-initials avatar fallback")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking templates: %v", err)
	}
	for _, o := range offenders {
		t.Errorf("%s — render it through user_avatar so it wears the author's ring", o)
	}
}

// avatarImgRe matches an <img> whose src interpolates an avatar field, which is
// how every hand-rolled avatar in this tree was written.
var avatarImgRe = regexp.MustCompile(`(?i)<img[^>]*src="\{\{[^"}]*avatar`)

// liveRingTestView is the live lane's own view struct, built here so the live
// Facets — which this change does not own and does not edit — are proved to
// draw the author's ring through the same one authority as everything else.
func liveRingTestView(handle string) *liveStreamView {
	started := time.Now().Add(-12 * time.Minute)
	return &liveStreamView{
		S: &model.LiveStream{
			ID:         "11111111-1111-1111-1111-111111111111",
			AuthorID:   "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
			AuthorPIAL: "22222222-2222-2222-2222-222222222222",
			Title:      "Ring test", Status: model.LiveStatusLive, StartedAt: &started,
			ViewerCount: 3, PosterURL: "", PlaylistURL: "", SourceHeight: 720,
			AuthorHandle: handle, AuthorName: "Ring Test",
		},
		AuthorHandle: handle, AuthorName: "Ring Test",
		AuthorPIAL: "22222222-2222-2222-2222-222222222222",
		// Deliberately wrong: the live view still carries a realm, and the ring
		// must come from the index regardless of what this says.
		AuthorRealm:  1,
		VerifiedType: "blue", Viewers: "3", ViewerCount: 3, TimeLabel: "live 12m",
		Ladder: []int{720}, ViewerToken: "tok", IsAnon: true,
	}
}
