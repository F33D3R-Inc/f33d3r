package handler

import (
	"encoding/json"
	"strings"
	"time"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
	"github.com/f33d3r/feed-engine/internal/realm"
)

// ── /api/v1 DTOs ──────────────────────────────────────────────────────────────
//
// The DTOs are the privacy boundary of the JSON surface the native clients
// read. They are hand-written with explicit json tags and built by the mappers
// below; nothing here is `json.Marshal` of a model type. Keys match the Swift
// CodingKeys in mobile/f33d3r_iOS/F33D3RKit/Sources/F33D3RKit/Models exactly,
// and the golden fixtures under F33D3RKit/Tests check them from both sides
// (api_v1_contract_test.go here, ContractTests there).
//
// Not present, on purpose: account UUIDs, anyone else's PIAL, the ranking
// vocabulary (Jung axes, archetypes, interest vectors, shadow caps). The one
// identity field that crosses is `pial_id` on MeDTO, to the authenticated
// owner only, because `author_pial` is inside the signed work payload and the
// device has to know what to sign as.

// UserDTO is a user as any client may see them.
type UserDTO struct {
	Handle         string  `json:"handle"`
	DisplayName    string  `json:"display_name"`
	Bio            *string `json:"bio,omitempty"`
	Pronouns       *string `json:"pronouns,omitempty"`
	Location       *string `json:"location,omitempty"`
	Website        *string `json:"website,omitempty"`
	AvatarURL      *string `json:"avatar_url,omitempty"`
	HeaderURL      *string `json:"header_url,omitempty"`
	IsVerified     bool    `json:"is_verified"`
	IsCreator      bool    `json:"is_creator"`
	OfficialType   *string `json:"official_type,omitempty"`
	Role           string  `json:"role"`
	FollowerCount  int     `json:"follower_count"`
	FollowingCount int     `json:"following_count"`
	PostCount      int     `json:"post_count"`
	Realm          int     `json:"realm"`
	RealmName      string  `json:"realm_name"`
	XP             int64   `json:"xp"`
	ThemeID        *string `json:"theme_id,omitempty"`
	AccentHex      *string `json:"accent_hex,omitempty"`
	IsPrivate      bool    `json:"is_private"`
	// LiveFrequencyID is the Frequency this person is hosting at this moment,
	// and absent whenever they are not: the accent ring and mic badge drawn
	// around their avatar, and where tapping that badge goes. It is the room's
	// public uuid — the id GET /api/v1/frequencies/{id} takes — never a PIAL.
	LiveFrequencyID *string `json:"live_frequency_id,omitempty"`
}

// MeDTO flattens UserDTO with the owner's own settings.
type MeDTO struct {
	UserDTO
	ContentSetting      string `json:"content_setting"`
	ShowSensitive       bool   `json:"show_sensitive"`
	Tier                string `json:"tier"`
	UnreadCount         int    `json:"unread_count"`
	IsAdult             bool   `json:"is_adult"`
	IsMinor             bool   `json:"is_minor"`
	IsAgeVerified       bool   `json:"is_age_verified"`
	IsAdultCreator      bool   `json:"is_adult_creator"`
	TwoFAEnabled        bool   `json:"two_fa_enabled"`
	CelebrationsEnabled bool   `json:"celebrations_enabled"`
	// HasPassword is false for an account created before passwords were set on
	// this device — the change-password screen drops its "current password"
	// field rather than asking for one that does not exist.
	HasPassword bool `json:"has_password"`
	// Who may see the two halves of the birthday: everyone | followers |
	// mutual_followers | only_me.
	BirthdayMDVisibility   string `json:"birthday_md_visibility"`
	BirthdayYearVisibility string `json:"birthday_year_visibility"`
	// Identity standing, as the account's own PIAL root records it. The tier
	// name is none | basic | soft | full; null when there is no identity root
	// to read. No score, no band, no reason — the tier and when it was set.
	KYCStatus      *string    `json:"kyc_status"`
	KYCSubmittedAt *time.Time `json:"kyc_submitted_at"`
	PayoutEnabled  bool       `json:"payout_enabled"`
	// The profile's own links, decoded from the JSON the profile row holds.
	SocialLinks      map[string]string `json:"social_links"`
	ExternalTipLinks map[string]string `json:"external_tip_links"`
	// ISO 3166-1 alpha-2, or null when the account has not said.
	CountryCode *string `json:"country_code"`
	// The identity the device signs as. Owner-only; see the file comment.
	PIALID string `json:"pial_id,omitempty"`
}

// SessionDTO is the login answer.
type SessionDTO struct {
	Token            string    `json:"token"`
	ExpiresAt        time.Time `json:"expires_at"`
	User             MeDTO     `json:"user"`
	NeedsBackupCodes bool      `json:"needs_backup_codes"`
}

// WorkAuthorDTO is the author strip on a card.
type WorkAuthorDTO struct {
	Handle       string  `json:"handle"`
	DisplayName  string  `json:"display_name"`
	AvatarURL    *string `json:"avatar_url,omitempty"`
	IsVerified   bool    `json:"is_verified"`
	IsCreator    bool    `json:"is_creator"`
	OfficialType *string `json:"official_type,omitempty"`
	Role         string  `json:"role"`
	Realm        int     `json:"realm"`
	// LiveFrequencyID is the Frequency this person is hosting at this moment,
	// and absent whenever they are not: the accent ring and mic badge drawn
	// around their avatar, and where tapping that badge goes. It is the room's
	// public uuid — the id GET /api/v1/frequencies/{id} takes — never a PIAL.
	LiveFrequencyID *string `json:"live_frequency_id,omitempty"`
}

type VideoDTO struct {
	MasterURL    string  `json:"master_url"`
	PosterURL    *string `json:"poster_url,omitempty"`
	DurationSecs float64 `json:"duration_secs"`
	Width        int     `json:"width"`
	Height       int     `json:"height"`
}

type VoiceDTO struct {
	URL          string  `json:"url"`
	DurationSecs float64 `json:"duration_secs"`
}

type PollResultDTO struct {
	Index    int    `json:"index"`
	Label    string `json:"label"`
	Votes    int    `json:"votes"`
	Pct      int    `json:"pct"`
	Voted    bool   `json:"voted"`
	IsWinner bool   `json:"is_winner"`
}

type PollDTO struct {
	Results    []PollResultDTO `json:"results"`
	TotalVotes int             `json:"total_votes"`
	// -1 when the viewer has not voted; the Swift side turns that into nil.
	ViewerVote int        `json:"viewer_vote"`
	EndsAt     *time.Time `json:"ends_at,omitempty"`
	Closed     bool       `json:"closed"`
	TimeLeft   string     `json:"time_left"`
}

type QuotedWorkDTO struct {
	ID        string        `json:"id"`
	CID       string        `json:"cid"`
	Author    WorkAuthorDTO `json:"author"`
	Body      string        `json:"body"`
	CreatedAt time.Time     `json:"created_at"`
	MediaURLs []string      `json:"media_urls,omitempty"`
	Video     *VideoDTO     `json:"video,omitempty"`
	IsNSFW    bool          `json:"is_nsfw"`
	IsGore    bool          `json:"is_gore"`
	// Nested is the work the quoted work itself quotes — the second and last
	// level of a chain, which the web draws as a bare rail inside the quote
	// card. It ends where the data ends: EnrichWorksWithQuotes loads
	// dbpkg.QuoteRenderDepth rounds and quotedWorkDTO follows the chain that
	// many levels and no further, so a client never receives a level nothing
	// loaded, and never has to decide for itself where a chain stops.
	Nested *QuotedWorkDTO `json:"nested,omitempty"`
}

// LinkPreviewDTO is the Open Graph card for the first external link in a body.
type LinkPreviewDTO struct {
	URL         string  `json:"url"`
	Title       string  `json:"title"`
	Description *string `json:"description,omitempty"`
	ImageURL    *string `json:"image_url,omitempty"`
	SiteName    *string `json:"site_name,omitempty"`
}

func linkPreviewDTO(p *model.LinkPreview) *LinkPreviewDTO {
	if p == nil || p.URL == "" {
		return nil
	}
	return &LinkPreviewDTO{
		URL:         p.URL,
		Title:       p.Title,
		Description: strPtr(p.Description),
		ImageURL:    strPtr(p.ImageURL),
		SiteName:    strPtr(p.SiteName),
	}
}

// ProvenanceDTO is the surface's one-line account of why a row is on it. Only
// what the server actually knows: a follow, a repost by someone followed, a
// rank. Never a description of how ranking works.
type ProvenanceDTO struct {
	Kind   string  `json:"kind"`
	Text   string  `json:"text"`
	Handle *string `json:"handle,omitempty"`
}

// WorkDTO is one work as the card draws it.
type WorkDTO struct {
	ID        string        `json:"id"`
	CID       string        `json:"cid"`
	Kind      string        `json:"kind"`
	Author    WorkAuthorDTO `json:"author"`
	Body      string        `json:"body"`
	CreatedAt time.Time     `json:"created_at"`
	IsEdited  bool          `json:"is_edited"`
	EditedAt  *time.Time    `json:"edited_at,omitempty"`
	ExpiresAt *time.Time    `json:"expires_at,omitempty"`

	MediaURLs []string       `json:"media_urls,omitempty"`
	Video     *VideoDTO      `json:"video,omitempty"`
	Voice     *VoiceDTO      `json:"voice,omitempty"`
	Poll      *PollDTO       `json:"poll,omitempty"`
	Quoted    *QuotedWorkDTO `json:"quoted,omitempty"`
	ParentCID *string        `json:"parent_cid,omitempty"`
	QuotedCID *string        `json:"quoted_cid,omitempty"`

	Tags              []string        `json:"tags,omitempty"`
	LinkPreview       *LinkPreviewDTO `json:"link_preview,omitempty"`
	YouTubeID         *string         `json:"youtube_id,omitempty"`
	YouTubePlaylistID *string         `json:"youtube_playlist_id,omitempty"`
	ContentType       string          `json:"content_type"`
	IsNSFW            bool            `json:"is_nsfw"`
	IsGore            bool            `json:"is_gore"`
	IsSensitive       bool            `json:"is_sensitive"`
	SubscriberOnly    bool            `json:"subscriber_only"`
	IsPinned          bool            `json:"is_pinned"`
	CommentGating     string          `json:"comment_gating"`
	ScanState         string          `json:"scan_state"`
	ScoreBand         *string         `json:"score_band,omitempty"`

	LikeCount     int `json:"like_count"`
	DislikeCount  int `json:"dislike_count"`
	RepostCount   int `json:"repost_count"`
	QuoteCount    int `json:"quote_count"`
	ReplyCount    int `json:"reply_count"`
	BookmarkCount int `json:"bookmark_count"`
	ViewCount     int `json:"view_count"`

	LikedByViewer       bool `json:"liked_by_viewer"`
	DislikedByViewer    bool `json:"disliked_by_viewer"`
	RepostedByViewer    bool `json:"reposted_by_viewer"`
	BookmarkedByViewer  bool `json:"bookmarked_by_viewer"`
	ViewerFollowsAuthor bool `json:"viewer_follows_author"`

	RepostedBy *WorkAuthorDTO `json:"reposted_by,omitempty"`
	RepostedAt *time.Time     `json:"reposted_at,omitempty"`

	LineageHandle      *string `json:"lineage_handle,omitempty"`
	LineageDisplayName *string `json:"lineage_display_name,omitempty"`

	LatestReplierHandles []string `json:"latest_replier_handles,omitempty"`
	LatestReplierAvatars []string `json:"latest_replier_avatars,omitempty"`

	Provenance *ProvenanceDTO `json:"provenance,omitempty"`

	TipTotalUAET *int64 `json:"tip_total_uaet,omitempty"`
	PriceUAET    *int64 `json:"price_uaet,omitempty"`
	// Paid works are sold through the Themis marketplace, not a per-work
	// price, so nothing is ever purchased through this lane: always false.
	PurchasedByViewer bool `json:"work_purchased_by_viewer"`
}

type WorkPageDTO struct {
	Works      []WorkDTO `json:"works"`
	NextCursor string    `json:"next_cursor,omitempty"`
	LiveCount  *int      `json:"live_count,omitempty"`
}

type WorkThreadDTO struct {
	Work          WorkDTO   `json:"work"`
	Ancestors     []WorkDTO `json:"ancestors"`
	Replies       []WorkDTO `json:"replies"`
	RepliesCursor string    `json:"replies_cursor,omitempty"`
}

type ProfileDTO struct {
	User            UserDTO  `json:"user"`
	ViewerFollows   bool     `json:"viewer_follows"`
	FollowsViewer   bool     `json:"follows_viewer"`
	ViewerIsBlocked bool     `json:"viewer_is_blocked"`
	PinnedWork      *WorkDTO `json:"pinned_work,omitempty"`
}

type NotificationDTO struct {
	ID         string          `json:"id"`
	Kind       string          `json:"kind"`
	Actors     []WorkAuthorDTO `json:"actors"`
	ActorCount int             `json:"actor_count"`
	TargetID   *string         `json:"target_id,omitempty"`
	TargetType string          `json:"target_type"`
	Preview    *string         `json:"preview,omitempty"`
	AmountUAET *int64          `json:"amount_uaet,omitempty"`
	IsRead     bool            `json:"is_read"`
	CreatedAt  time.Time       `json:"created_at"`
}

type NotificationPageDTO struct {
	Notifications []NotificationDTO `json:"notifications"`
	NextCursor    string            `json:"next_cursor,omitempty"`
	UnreadCount   int               `json:"unread_count"`
}

type WalletEntryDTO struct {
	ID                 string    `json:"id"`
	Kind               string    `json:"kind"`
	AmountUAET         int64     `json:"amount_uaet"`
	CounterpartyHandle *string   `json:"counterparty_handle,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
}

type WalletDTO struct {
	BalanceUAET int64            `json:"balance_uaet"`
	PendingUAET int64            `json:"pending_uaet"`
	Entries     []WalletEntryDTO `json:"entries"`
}

type SearchDTO struct {
	Works  []WorkDTO `json:"works"`
	People []UserDTO `json:"people"`
	Tags   []TagDTO  `json:"tags"`
}

// TagDTO is a hashtag and how many live works carry it.
type TagDTO struct {
	Tag   string `json:"tag"`
	Count int    `json:"count"`
}

// UserPageDTO is one page of a followers or following list. ViewerFollows
// names, by handle, the ones the viewer already follows.
type UserPageDTO struct {
	Users         []UserDTO `json:"users"`
	NextCursor    string    `json:"next_cursor,omitempty"`
	ViewerFollows []string  `json:"viewer_follows"`
}

// SessionInfoDTO is one signed-in device.
type SessionInfoDTO struct {
	ID         string    `json:"id"`
	DeviceName string    `json:"device_name"`
	IPAddress  *string   `json:"ip_address,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
	IsCurrent  bool      `json:"is_current"`
}

// ── Mappers ───────────────────────────────────────────────────────────────────

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// userRealm is the public realm of an account: the derived level, with a
// granted floor and the founder's top-of-scale rule applied by package realm.
func userRealm(u *model.User) (int, string) {
	r := realm.Effective(u.XP, u.RealmGrant, u.Role)
	return r, realm.RealmName(r)
}

func userDTO(u *model.User) UserDTO {
	r, name := userRealm(u)
	return UserDTO{
		Handle:          u.Handle,
		DisplayName:     u.DisplayName,
		Bio:             strPtr(u.Bio),
		Pronouns:        strPtr(u.Pronouns),
		Location:        strPtr(u.Location),
		Website:         strPtr(u.Website),
		AvatarURL:       strPtr(u.AvatarURL),
		HeaderURL:       strPtr(u.HeaderURL),
		IsVerified:      u.IsVerified,
		IsCreator:       u.IsCreator,
		OfficialType:    strPtr(u.OfficialType),
		Role:            u.Role,
		FollowerCount:   u.FollowerCount,
		FollowingCount:  u.FollowingCount,
		PostCount:       u.PostCount,
		Realm:           r,
		RealmName:       name,
		XP:              u.XP,
		ThemeID:         strPtr(u.ThemeID),
		AccentHex:       strPtr(u.AccentHex),
		IsPrivate:       u.IsPrivate,
		LiveFrequencyID: liveFrequencyFor(u.PIALID),
	}
}

// meStanding is what the owner's view needs that the User model does not
// carry: facts held in other tables, looked up once by meDTOFor.
type meStanding struct {
	Unread         int
	TwoFA          bool
	HasPassword    bool
	KYCSubmittedAt *time.Time
}

// meDTO is the owner's own view: the public user plus the settings and
// standing only they may read.
func meDTO(u *model.User, s meStanding) MeDTO {
	return MeDTO{
		UserDTO:                userDTO(u),
		ContentSetting:         u.ContentSetting,
		ShowSensitive:          u.ShowSensitive,
		Tier:                   u.Tier,
		UnreadCount:            s.Unread,
		IsAdult:                u.IsAdult,
		IsMinor:                u.IsMinor,
		IsAgeVerified:          u.IsAgeVerified,
		IsAdultCreator:         u.IsAdultCreator,
		TwoFAEnabled:           s.TwoFA,
		CelebrationsEnabled:    u.CelebrationsEnabled,
		HasPassword:            s.HasPassword,
		BirthdayMDVisibility:   visibilityOr(u.BirthdayMdVisibility, "everyone"),
		BirthdayYearVisibility: visibilityOr(u.BirthdayYearVisibility, "only_me"),
		KYCStatus:              strPtr(u.KYCTier),
		KYCSubmittedAt:         s.KYCSubmittedAt,
		PayoutEnabled:          u.CanMonetize(),
		SocialLinks:            decodeLinkMap(u.SocialLinksRaw),
		ExternalTipLinks:       decodeLinkMap(u.ExternalTipLinksRaw),
		CountryCode:            strPtr(u.CountryCode),
		PIALID:                 u.PIALID,
	}
}

// visibilityOr is the stored visibility choice, or the platform default when
// the profile row predates the column.
func visibilityOr(v, def string) string {
	switch v {
	case "everyone", "followers", "mutual_followers", "only_me":
		return v
	}
	return def
}

// decodeLinkMap reads one of the profile's link objects out of the raw JSON the
// row holds. An unreadable or empty value is an empty object, never null: the
// client edits these in place and needs something to edit.
func decodeLinkMap(raw string) map[string]string {
	out := map[string]string{}
	if strings.TrimSpace(raw) == "" {
		return out
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return map[string]string{}
	}
	for k, v := range out {
		if strings.TrimSpace(v) == "" {
			delete(out, k)
		}
	}
	return out
}

// authorDTO is the author strip from the fields the works query joins in.
func authorDTO(w *model.Work) WorkAuthorDTO {
	if w == nil || w.AuthorHandle == "" {
		return WorkAuthorDTO{Handle: "deleted", DisplayName: "[deleted]", Role: "user", Realm: 1}
	}
	role := w.AuthorRole
	if role == "" || role == "creator" {
		// AuthorRole folds creator status into the role string for templates;
		// the DTO carries is_creator separately and role stays a platform role.
		if role == "creator" {
			role = model.RoleUser
		} else {
			role = model.RoleUser
		}
	}
	r := w.AuthorRealm
	if r < 1 {
		r = 1
	}
	return WorkAuthorDTO{
		Handle:          w.AuthorHandle,
		DisplayName:     w.AuthorName,
		AvatarURL:       strPtr(w.AvatarURL),
		IsVerified:      w.IsVerified,
		IsCreator:       w.AuthorIsCreator,
		OfficialType:    strPtr(w.AuthorOfficialType),
		Role:            role,
		Realm:           r,
		LiveFrequencyID: liveFrequencyFor(w.AuthorPIAL),
	}
}

// userAuthorDTO is the author strip built from a full user row — the actors
// on a notification, where there is no work to read them off.
func userAuthorDTO(u *model.User) WorkAuthorDTO {
	if u == nil {
		return WorkAuthorDTO{Handle: "deleted", DisplayName: "[deleted]", Role: "user", Realm: 1}
	}
	r, _ := userRealm(u)
	return WorkAuthorDTO{
		Handle:          u.Handle,
		DisplayName:     u.DisplayName,
		AvatarURL:       strPtr(u.AvatarURL),
		IsVerified:      u.IsVerified,
		IsCreator:       u.IsCreator,
		OfficialType:    strPtr(u.OfficialType),
		Role:            u.Role,
		Realm:           r,
		LiveFrequencyID: liveFrequencyFor(u.PIALID),
	}
}

func videoDTO(w *model.Work) *VideoDTO {
	if w.VideoMasterURL == "" {
		return nil
	}
	return &VideoDTO{
		MasterURL:    w.VideoMasterURL,
		PosterURL:    strPtr(w.VideoPosterURL),
		DurationSecs: float64(w.VideoDurationSecs),
		Width:        w.VideoWidth,
		Height:       w.VideoHeight,
	}
}

func pollDTO(p *model.Poll) *PollDTO {
	if p == nil {
		return nil
	}
	out := &PollDTO{
		Results:    make([]PollResultDTO, len(p.Results)),
		TotalVotes: p.TotalVotes,
		ViewerVote: p.UserVote,
		EndsAt:     p.EndsAt,
		Closed:     p.Closed,
		TimeLeft:   p.TimeLeft,
	}
	for i, r := range p.Results {
		out.Results[i] = PollResultDTO{Index: i, Label: r.Label, Votes: r.Votes, Pct: r.Pct, Voted: r.Voted, IsWinner: r.IsWinner}
	}
	return out
}

// commentGatingDTO is the vocabulary the clients speak. The works table stores
// "open" for the unrestricted case (work_event.go); the card and the compose
// sheet call that "everyone".
func commentGatingDTO(g string) string {
	switch g {
	case "", "open":
		return "everyone"
	}
	return g
}

// workDTO projects an enriched work for a viewer. `provenance` is the
// surface's account of why the row is there; nil when the surface has none to
// give. The work must already have been through enrichWorks: reactions,
// quotes, repliers, polls and the pinned mark are read, never computed, here.
func workDTO(w *model.Work, provenance *ProvenanceDTO) WorkDTO {
	d := WorkDTO{
		ID:                   w.ID,
		CID:                  w.CID,
		Kind:                 w.Kind,
		Author:               authorDTO(w),
		Body:                 w.Body,
		CreatedAt:            w.CreatedAt,
		IsEdited:             w.IsEdited,
		EditedAt:             w.EditedAt,
		ExpiresAt:            w.ExpiresAt,
		MediaURLs:            w.MediaURLs,
		Video:                videoDTO(w),
		Poll:                 pollDTO(w.Poll),
		ParentCID:            strPtr(w.ParentCID),
		QuotedCID:            strPtr(w.QuotedCID),
		Tags:                 w.Tags,
		LinkPreview:          linkPreviewDTO(w.LinkPreview),
		YouTubeID:            strPtr(w.YouTubeID),
		YouTubePlaylistID:    strPtr(w.YouTubePlaylistID),
		ContentType:          w.ContentType,
		IsNSFW:               w.IsNSFW,
		IsGore:               w.IsGore,
		IsSensitive:          w.IsSensitive,
		SubscriberOnly:       w.SubscriberOnly,
		IsPinned:             w.IsPinned,
		CommentGating:        commentGatingDTO(w.CommentGating),
		ScanState:            w.ScanState,
		ScoreBand:            strPtr(w.ScoreBand),
		LikeCount:            w.LikeCount,
		DislikeCount:         w.DislikeCount,
		RepostCount:          w.RepostCount,
		QuoteCount:           w.QuoteCount,
		ReplyCount:           w.ReplyCount,
		BookmarkCount:        w.BookmarkCount,
		ViewCount:            w.ViewCount,
		LikedByViewer:        w.LikedByUser,
		DislikedByViewer:     w.DislikedByUser,
		RepostedByViewer:     w.RepostedByUser,
		BookmarkedByViewer:   w.BookmarkedByUser,
		ViewerFollowsAuthor:  w.ViewerFollowsAuthor,
		LineageHandle:        strPtr(w.LineageHandle),
		LineageDisplayName:   strPtr(w.LineageDisplayName),
		LatestReplierHandles: w.LatestReplierHandles,
		LatestReplierAvatars: w.LatestReplierAvatars,
		Provenance:           provenance,
	}
	if d.ContentType == "" {
		d.ContentType = "text"
	}
	if d.ScanState == "" {
		d.ScanState = "clean"
	}
	if w.VoiceURL != "" {
		d.Voice = &VoiceDTO{URL: w.VoiceURL, DurationSecs: float64(w.VoiceDurationSecs)}
	}
	if w.RepostedByHandle != "" {
		name := w.RepostedByName
		if name == "" {
			name = w.RepostedByHandle
		}
		d.RepostedBy = &WorkAuthorDTO{Handle: w.RepostedByHandle, DisplayName: name, Role: model.RoleUser, Realm: 1}
		if !w.RepostedAt.IsZero() {
			t := w.RepostedAt
			d.RepostedAt = &t
		}
	}
	d.Quoted = quotedWorkDTO(w.QuotedWork, dbpkg.QuoteRenderDepth)
	return d
}

// quotedWorkDTO flattens a quote chain for the wire, following it `depth`
// levels down. The bound is dbpkg.QuoteRenderDepth — the number of rounds the
// loader attaches — passed in rather than read here so the recursion counts
// down the same number the loader counted up, and a test can watch it stop.
// A nil work or an exhausted depth is the end of the chain: no key is emitted.
func quotedWorkDTO(q *model.Work, depth int) *QuotedWorkDTO {
	if q == nil || depth <= 0 {
		return nil
	}
	return &QuotedWorkDTO{
		ID:        q.ID,
		CID:       q.CID,
		Author:    authorDTO(q),
		Body:      q.Body,
		CreatedAt: q.CreatedAt,
		MediaURLs: q.MediaURLs,
		Video:     videoDTO(q),
		IsNSFW:    q.IsNSFW,
		IsGore:    q.IsGore,
		Nested:    quotedWorkDTO(q.QuotedWork, depth-1),
	}
}

func workDTOs(works []*model.Work, provenance func(*model.Work) *ProvenanceDTO) []WorkDTO {
	out := make([]WorkDTO, 0, len(works))
	for _, w := range works {
		if w == nil {
			continue
		}
		var p *ProvenanceDTO
		if provenance != nil {
			p = provenance(w)
		}
		out = append(out, workDTO(w, p))
	}
	return out
}

// notificationDTO projects one grouped notification. actors are the resolved
// accounts behind the group, in the order they acted.
func notificationDTO(g *dbpkg.NotificationGroup, actors []*model.User) NotificationDTO {
	a := make([]WorkAuthorDTO, 0, len(actors))
	for _, u := range actors {
		a = append(a, userAuthorDTO(u))
	}
	d := NotificationDTO{
		ID:         g.ID,
		Kind:       g.Kind,
		Actors:     a,
		ActorCount: g.ActorCount,
		TargetID:   strPtr(g.TargetID),
		TargetType: g.TargetType,
		Preview:    strPtr(g.Preview),
		IsRead:     g.IsRead,
		CreatedAt:  g.CreatedAt,
	}
	if g.AmountUAET != 0 {
		amt := g.AmountUAET
		d.AmountUAET = &amt
	}
	return d
}

// ── Envelopes ─────────────────────────────────────────────────────────────────
//
// Answers that wrap a list or a receipt in one object. Named so the shape is
// declared in one place and pinned by api_v1_contract_test.go, not implied by
// a map literal in a handler.

// MediaUploadDTO — POST /api/v1/media, 201.
type MediaUploadDTO struct {
	URL   string `json:"url"`
	Bytes int    `json:"bytes"`
}

// TrendingTagsDTO — GET /api/v1/tags/trending.
type TrendingTagsDTO struct {
	Tags []TagDTO `json:"tags"`
}

// SessionListDTO — GET /api/v1/sessions; the asking device first.
type SessionListDTO struct {
	Sessions []SessionInfoDTO `json:"sessions"`
}

// VisionViewersDTO — GET /api/v1/visions/{id}/viewers, the author's view.
type VisionViewersDTO struct {
	Viewers []WorkAuthorDTO `json:"viewers"`
	Count   int             `json:"count"`
}
