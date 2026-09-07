package handler

// atlas_fixtures.go — sample data for the Facet Atlas live stage.
//
// This is the ONLY file edited to enrich the atlas. The engine (atlas.go) never
// changes when facets are added. A facet with no fixture still appears in the
// catalog with its id, tier, contract, and links — it just shows a "needs
// fixture" stub on the stage instead of a live render.
//
// To make a facet render live (and show multiple configs), add an entry to
// atlasFixtures keyed by the facet name with one AtlasVariant per config.

import (
	"bytes"
	"fmt"
	"html/template"
	"time"

	"github.com/f33d3r/feed-engine/internal/model"
)

// AtlasVariant is one rendered configuration of a facet.
type AtlasVariant struct {
	Label string
	Data  any
}

// ── Canonical samples ─────────────────────────────────────────────────────────
// Built fresh per call so a template that mutates its input can never bleed into
// another card.

func sampleUser() *model.User {
	bd := time.Date(1996, 4, 12, 0, 0, 0, 0, time.UTC)
	return &model.User{
		ID:                "00000000-0000-0000-0000-0000000000aa",
		Handle:            "mia",
		DisplayName:       "Mia 🎀",
		Bio:               "atlas sample user — server-rendered, no live data",
		Location:          "Neon City",
		Website:           "f33d3r.com",
		AvatarURL:         "",
		ThemeID:           "void",
		IsCreator:         true,
		IsVerified:        true,
		FollowerCount:     12400,
		FollowingCount:    318,
		PostCount:         742,
		Tier:              "creator",
		Realm:             4,
		XP:                8650,
		Role:              "user",
		PIALID:            "00000000-0000-0000-0000-0000000000bb",
		IsAgeVerified:     true,
		IsAdult:           true,
		Birthday:          &bd,
		IsFoundingCreator: true,
	}
}

func sampleWork() *model.Work {
	return &model.Work{
		ID:              "00000000-0000-0000-0000-0000000000cc",
		CID:             "sha256:demo",
		AuthorID:        "00000000-0000-0000-0000-0000000000aa",
		AuthorPIAL:      "00000000-0000-0000-0000-0000000000bb",
		Body:            "Walking the whole facet library top to bottom — every tile here is a real server-rendered Facet. #facetarchitecture",
		Kind:            "post",
		AuthorHandle:    "mia",
		AuthorName:      "Mia 🎀",
		AuthorRole:      "creator",
		AuthorRealm:     4,
		IsVerified:      true,
		AuthorIsCreator: true,
		LikeCount:       1284,
		RepostCount:     211,
		BookmarkCount:   96,
		ReplyCount:      48,
		DislikeCount:    3,
		QuoteCount:      12,
		ViewCount:       53000,
		TimeAgo:         "2h",
		ContentType:     "text",
		ScanState:       "clean",
		ScoreBand:       "trending",
		CreatedAt:       time.Now().Add(-2 * time.Hour),
	}
}

// sampleCtx mirrors the {"CurrentUserHandle","User"} map work_card et al. expect.
func sampleCtx() map[string]any {
	u := sampleUser()
	return map[string]any{"CurrentUserHandle": u.Handle, "User": u}
}

// workCardData wraps a work in the {W, Ctx} envelope the card consumes.
func workCardData(w *model.Work) map[string]any {
	return map[string]any{"W": w, "Ctx": sampleCtx()}
}

func nsfwWork() *model.Work {
	w := sampleWork()
	w.IsNSFW = true
	w.IsSensitive = true
	w.Body = "Sensitive sample work (gated)."
	return w
}

// ── Fixtures ──────────────────────────────────────────────────────────────────
// facet name -> configurations rendered on the stage. A facet absent here still
// appears in the catalog with a "needs fixture" stub. The engine never changes.

var atlasFixtures = map[string][]AtlasVariant{
	// ── Atomic: avatars ──
	"post_avatar": {
		{Label: "with realm ring + online", Data: map[string]any{
			"Handle": "mia", "AvatarURL": "", "AuthorName": "Mia 🎀", "IsOnline": true,
		}},
		{Label: "offline, no realm", Data: map[string]any{
			"Handle": "guest", "AvatarURL": "", "AuthorName": "Guest", "IsOnline": false,
		}},
	},
	"avatar_img": {
		{Label: "sm", Data: map[string]any{"URL": "", "Handle": "mia", "Size": "sm"}},
		{Label: "md", Data: map[string]any{"URL": "", "Handle": "mia", "Size": "md"}},
		{Label: "lg", Data: map[string]any{"URL": "", "Handle": "mia", "Size": "lg"}},
		{Label: "xl", Data: map[string]any{"URL": "", "Handle": "mia", "Size": "xl"}},
	},
	"user_avatar": {
		{Label: "creator, realm 4, online", Data: map[string]any{
			"Handle": "mia", "AvatarURL": "", "DisplayName": "Mia 🎀", "Size": "lg", "IsOnline": true,
		}},
	},

	// ── Atomic: badges ──
	"badge_pill": {
		{Label: "founder", Data: map[string]any{"Role": "founder", "IsVerified": true}},
		{Label: "admin", Data: map[string]any{"Role": "admin", "IsVerified": true}},
		{Label: "verified creator", Data: map[string]any{"Role": "user", "IsVerified": true, "IsCreator": true}},
		{Label: "government official", Data: map[string]any{"Role": "user", "OfficialType": "government", "IsVerified": true}},
		{Label: "plain user", Data: map[string]any{"Role": "user"}},
	},
	"badge_verified": {
		{Label: "blue", Data: map[string]any{"Type": "blue"}},
		{Label: "gold", Data: map[string]any{"Type": "gold"}},
	},
	"badge_nsfw":   {{Label: "default", Data: map[string]any{}}},
	"badge_live":   {{Label: "default", Data: map[string]any{}}},
	"badge_pinned": {{Label: "default", Data: map[string]any{}}},
	"content_type_chip": {
		{Label: "video (selected)", Data: map[string]any{"Value": "video", "Label": "Video", "Emoji": "🎬", "Checked": true}},
		{Label: "photos", Data: map[string]any{"Value": "photos", "Label": "Photos", "Emoji": "📸", "Checked": false}},
		{Label: "music", Data: map[string]any{"Value": "music", "Label": "Music", "Emoji": "🎵", "Checked": false}},
	},

	// ── Atomic: action buttons ──
	"like_btn": {
		{Label: "default", Data: map[string]any{"WorkID": "demo", "LikedByUser": false, "LikeCount": 1284}},
		{Label: "liked", Data: map[string]any{"WorkID": "demo", "LikedByUser": true, "LikeCount": 1285}},
	},
	"bookmark_btn": {
		{Label: "default", Data: map[string]any{"WorkID": "demo", "BookmarkedByUser": false, "BookmarkCount": 96}},
		{Label: "saved", Data: map[string]any{"WorkID": "demo", "BookmarkedByUser": true, "BookmarkCount": 97}},
	},
	"dislike_btn": {
		{Label: "default", Data: map[string]any{"WorkID": "demo", "DislikedByUser": false, "DislikeCount": 3}},
		{Label: "active", Data: map[string]any{"WorkID": "demo", "DislikedByUser": true, "DislikeCount": 4}},
	},
	"btn_follow": {
		{Label: "not following", Data: map[string]any{"TargetPIAL": "demo", "TargetHandle": "mia", "IsFollowing": false, "Surface": "profile"}},
		{Label: "following", Data: map[string]any{"TargetPIAL": "demo", "TargetHandle": "mia", "IsFollowing": true, "Surface": "profile"}},
		{Label: "compact (list / rail row)", Data: map[string]any{"TargetPIAL": "demo", "TargetHandle": "mia", "IsFollowing": false, "Surface": "compact"}},
		{Label: "card (work card author row)", Data: map[string]any{"TargetPIAL": "demo", "TargetHandle": "mia", "IsFollowing": false, "Surface": "card"}},
		{Label: "dot (work focus avatar)", Data: map[string]any{"TargetPIAL": "demo", "TargetHandle": "mia", "IsFollowing": false, "Surface": "dot"}},
		{Label: "handle-addressed (no PIAL)", Data: map[string]any{"TargetPIAL": "", "TargetHandle": "mia", "IsFollowing": false, "Surface": "compact"}},
	},
	"btn_follow_back": {
		{Label: "follows you", Data: map[string]any{"TargetPIAL": "demo", "TargetHandle": "mia", "IsFollowing": false, "FollowsYouBack": true}},
		{Label: "following back", Data: map[string]any{"TargetPIAL": "demo", "TargetHandle": "mia", "IsFollowing": true, "FollowsYouBack": true}},
	},
	"btn_follow_topic": {
		{Label: "not following", Data: map[string]any{"Tag": "aethyr", "IsFollowing": false}},
		{Label: "following", Data: map[string]any{"Tag": "aethyr", "IsFollowing": true}},
	},
	"btn_share": {
		{Label: "default", Data: map[string]any{"PostID": "demo", "PostBody": "Atlas sample share text"}},
	},
	"status_dot": {
		{Label: "online", Data: map[string]any{"IsOnline": true}},
		{Label: "offline (empty)", Data: map[string]any{"IsOnline": false}},
	},
	"toggle": {
		{Label: "off", Data: map[string]any{"Name": "demo", "Checked": false, "Label": "Enable thing"}},
		{Label: "on", Data: map[string]any{"Name": "demo", "Checked": true, "Label": "Enable thing"}},
	},
	"xp_bar": {
		{Label: "realm 4", Data: map[string]any{"User": sampleUser()}},
	},

	// ── Template: the flagship card ──
	"work_card": {
		{Label: "default", Data: workCardData(sampleWork())},
		{Label: "nsfw / gated", Data: workCardData(nsfwWork())},
	},
}

// renderFacetVariants renders every fixture variant for a card through the real
// app template set (h.partial). Each render is recover-guarded so a single bad
// fixture can never take down the page. Facets with no fixture get no variants
// (the template shows a stub).
func (h *Handler) renderFacetVariants(card *atlasCard) {
	variants, ok := atlasFixtures[card.Name]
	if !ok {
		return
	}
	for _, v := range variants {
		out := h.renderOneFacet(card.Name, v.Data)
		out.Label = v.Label
		card.Variants = append(card.Variants, out)
		if out.Err == "" {
			card.Rendered = true
		}
	}
}

// renderOneFacet executes a single facet template with the given data, capturing
// any panic or error as a stage-visible message rather than crashing the page.
func (h *Handler) renderOneFacet(name string, data any) (out atlasVariantView) {
	defer func() {
		if rec := recover(); rec != nil {
			out.HTML = ""
			out.Err = fmt.Sprintf("panic: %v", rec)
		}
	}()
	var buf bytes.Buffer
	if err := h.partial.ExecuteTemplate(&buf, name, data); err != nil {
		out.Err = err.Error()
		return
	}
	out.HTML = template.HTML(buf.String())
	return
}
