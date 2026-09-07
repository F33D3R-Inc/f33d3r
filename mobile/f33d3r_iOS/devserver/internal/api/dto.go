package api

import (
	"time"

	"f33d3r.com/ios/devserver/internal/store"
)

// The DTOs are the privacy boundary. They are hand-written with explicit json
// tags and built by the functions below; nothing here is `json.Marshal` of a
// store type. Keys match the Swift CodingKeys in F33D3RKit/Sources/Models
// exactly, and the golden fixtures in F33D3RKit/Tests check them.
//
// Not present, on purpose: account UUIDs, anyone else's PIAL, ranking
// internals. The one identity field that crosses is `pial_id` on MeDTO, to
// the authenticated owner only, because `author_pial` is inside the signed
// payload and the device has to know what to sign.

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
	// HasPassword is false for an account created through a channel that set
	// none; the change-password form then asks for the new one only.
	HasPassword bool `json:"has_password"`
	// Who may see the birth date: month-and-day and year separately.
	BirthdayMDVisibility   string `json:"birthday_md_visibility"`
	BirthdayYearVisibility string `json:"birthday_year_visibility"`
	// Identity verification standing; null on a deployment that runs none.
	KYCStatus      *string    `json:"kyc_status"`
	KYCSubmittedAt *time.Time `json:"kyc_submitted_at"`
	PayoutEnabled  bool       `json:"payout_enabled"`
	// youtube | twitch | spotify | soundcloud | kick | other → URL.
	SocialLinks map[string]string `json:"social_links"`
	// cashapp | venmo | paypal | kofi | buymeacoffee | bitcoin | xrp → handle or address.
	ExternalTipLinks map[string]string `json:"external_tip_links"`
	// ISO-3166 alpha-2, null when unset.
	CountryCode *string `json:"country_code"`
	// The identity the device signs as. Owner-only; see the file comment.
	PIALID string `json:"pial_id"`
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
	// level of a chain, drawn as a bare rail inside the quoted card. It ends
	// where the load ends: hydrateQuotes attaches store.QuoteRenderDepth
	// levels and quotedWorkDTO follows exactly that many, so a client is never
	// handed a level nothing loaded and never has to decide for itself where a
	// chain stops.
	Nested *QuotedWorkDTO `json:"nested,omitempty"`
}

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

	Tags           []string `json:"tags,omitempty"`
	ContentType    string   `json:"content_type"`
	IsNSFW         bool     `json:"is_nsfw"`
	IsGore         bool     `json:"is_gore"`
	IsSensitive    bool     `json:"is_sensitive"`
	SubscriberOnly bool     `json:"subscriber_only"`
	IsPinned       bool     `json:"is_pinned"`
	CommentGating  string   `json:"comment_gating"`
	ScanState      string   `json:"scan_state"`
	ScoreBand      *string  `json:"score_band,omitempty"`

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
	// The viewer has bought this priced work outright. Always present, so the
	// card can draw "Owned" in place of the price without a second read.
	WorkPurchasedByViewer bool `json:"work_purchased_by_viewer"`

	RepostedBy *WorkAuthorDTO `json:"reposted_by,omitempty"`
	RepostedAt *time.Time     `json:"reposted_at,omitempty"`

	LineageHandle      *string `json:"lineage_handle,omitempty"`
	LineageDisplayName *string `json:"lineage_display_name,omitempty"`

	LatestReplierHandles []string `json:"latest_replier_handles,omitempty"`
	LatestReplierAvatars []string `json:"latest_replier_avatars,omitempty"`

	Provenance *ProvenanceDTO `json:"provenance,omitempty"`

	TipTotalUAET *int64 `json:"tip_total_uaet,omitempty"`
	PriceUAET    *int64 `json:"price_uaet,omitempty"`
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

type TagDTO struct {
	Tag   string `json:"tag"`
	Count int    `json:"count"`
}

type SearchDTO struct {
	Works  []WorkDTO `json:"works"`
	People []UserDTO `json:"people"`
	Tags   []TagDTO  `json:"tags"`
}

type UserPageDTO struct {
	Users      []UserDTO `json:"users"`
	NextCursor string    `json:"next_cursor,omitempty"`
	// Which of these the viewer follows, by handle.
	ViewerFollows []string `json:"viewer_follows"`
}

type SessionInfoDTO struct {
	ID         string    `json:"id"`
	DeviceName string    `json:"device_name"`
	IPAddress  string    `json:"ip_address,omitempty"`
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

func userDTO(u *store.User) UserDTO {
	realm, name := store.Realm(u.XP, u.Role)
	return UserDTO{
		Handle:         u.Handle,
		DisplayName:    u.DisplayName,
		Bio:            strPtr(u.Bio),
		Pronouns:       strPtr(u.Pronouns),
		Location:       strPtr(u.Location),
		Website:        strPtr(u.Website),
		AvatarURL:      strPtr(u.AvatarURL),
		HeaderURL:      strPtr(u.HeaderURL),
		IsVerified:     u.IsVerified,
		IsCreator:      u.IsCreator,
		OfficialType:   strPtr(u.OfficialType),
		Role:           u.Role,
		FollowerCount:  u.FollowerCount,
		FollowingCount: u.FollowingCount,
		PostCount:      u.PostCount,
		Realm:          realm,
		RealmName:      name,
		XP:             u.XP,
		ThemeID:        strPtr(u.ThemeID),
		AccentHex:      strPtr(u.AccentHex),
		IsPrivate:      u.IsPrivate,
	}
}

func meDTO(u *store.User, unread int) MeDTO {
	return MeDTO{
		UserDTO:                userDTO(u),
		ContentSetting:         u.ContentSetting,
		ShowSensitive:          u.ShowSensitive,
		Tier:                   u.Tier,
		UnreadCount:            unread,
		IsAdult:                u.IsAdult,
		IsMinor:                u.IsMinor,
		IsAgeVerified:          u.IsAgeVerified,
		IsAdultCreator:         u.IsAdultCreator,
		TwoFAEnabled:           u.TwoFAEnabled,
		CelebrationsEnabled:    u.CelebrationsEnabled,
		HasPassword:            u.HasPassword,
		BirthdayMDVisibility:   visibilityOr(u.BirthdayMdVisibility, "everyone"),
		BirthdayYearVisibility: visibilityOr(u.BirthdayYearVisibility, "only_me"),
		KYCStatus:              kycStatusPtr(u.KYCTier),
		KYCSubmittedAt:         u.KYCSubmittedAt,
		PayoutEnabled:          u.CanMonetize(),
		SocialLinks:            nonNilLinks(u.SocialLinks),
		ExternalTipLinks:       nonNilLinks(u.ExternalTipLinks),
		CountryCode:            strPtr(u.CountryCode),
		PIALID:                 u.PIALID,
	}
}

func authorDTO(u *store.User) WorkAuthorDTO {
	if u == nil {
		return WorkAuthorDTO{Handle: "deleted", DisplayName: "[deleted]", Role: "user", Realm: 1}
	}
	realm, _ := store.Realm(u.XP, u.Role)
	return WorkAuthorDTO{
		Handle:       u.Handle,
		DisplayName:  u.DisplayName,
		AvatarURL:    strPtr(u.AvatarURL),
		IsVerified:   u.IsVerified,
		IsCreator:    u.IsCreator,
		OfficialType: strPtr(u.OfficialType),
		Role:         u.Role,
		Realm:        realm,
	}
}

func videoDTO(w *store.Work) *VideoDTO {
	if w.VideoMasterURL == "" {
		return nil
	}
	return &VideoDTO{
		MasterURL:    w.VideoMasterURL,
		PosterURL:    strPtr(w.VideoPosterURL),
		DurationSecs: w.VideoDurationSecs,
		Width:        w.VideoWidth,
		Height:       w.VideoHeight,
	}
}

func pollDTO(p *store.Poll) *PollDTO {
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

// workDTO projects a hydrated work for a viewer. `provenance` is the surface's
// account of why the row is there; nil when the surface has none to give.
func workDTO(w *store.Work, provenance *ProvenanceDTO) WorkDTO {
	d := WorkDTO{
		ID:                    w.ID,
		CID:                   w.CID,
		Kind:                  w.Kind,
		Author:                authorDTO(w.Author),
		Body:                  w.Body,
		CreatedAt:             w.CreatedAt,
		IsEdited:              w.IsEdited,
		EditedAt:              w.EditedAt,
		ExpiresAt:             w.ExpiresAt,
		MediaURLs:             w.MediaURLs,
		Video:                 videoDTO(w),
		Poll:                  pollDTO(w.Poll),
		ParentCID:             strPtr(w.ParentCID),
		QuotedCID:             strPtr(w.QuotedCID),
		Tags:                  w.Tags,
		ContentType:           w.ContentType,
		IsNSFW:                w.IsNSFW,
		IsGore:                w.IsGore,
		IsSensitive:           w.IsSensitive,
		SubscriberOnly:        w.SubscriberOnly,
		IsPinned:              w.IsPinned,
		CommentGating:         w.CommentGating,
		ScanState:             w.ScanState,
		ScoreBand:             strPtr(w.ScoreBand),
		LikeCount:             w.LikeCount,
		DislikeCount:          w.DislikeCount,
		RepostCount:           w.RepostCount,
		QuoteCount:            w.QuoteCount,
		ReplyCount:            w.ReplyCount,
		BookmarkCount:         w.BookmarkCount,
		ViewCount:             w.ViewCount,
		LikedByViewer:         w.LikedByViewer,
		DislikedByViewer:      w.DislikedByViewer,
		RepostedByViewer:      w.RepostedByViewer,
		BookmarkedByViewer:    w.BookmarkedByViewer,
		ViewerFollowsAuthor:   w.ViewerFollowsAuthor,
		WorkPurchasedByViewer: w.PurchasedByViewer,
		RepostedAt:            w.RepostedAt,
		LineageHandle:         strPtr(w.LineageHandle),
		LineageDisplayName:    strPtr(w.LineageDisplayName),
		LatestReplierHandles:  w.LatestReplierHandles,
		LatestReplierAvatars:  w.LatestReplierAvatars,
		Provenance:            provenance,
	}
	if w.VoiceURL != "" {
		d.Voice = &VoiceDTO{URL: w.VoiceURL, DurationSecs: w.VoiceDurationSecs}
	}
	if w.RepostedBy != nil {
		a := authorDTO(w.RepostedBy)
		d.RepostedBy = &a
	}
	d.Quoted = quotedWorkDTO(w.QuotedWork, store.QuoteRenderDepth)
	if w.TipTotalUAET > 0 {
		t := w.TipTotalUAET
		d.TipTotalUAET = &t
	}
	if w.PriceUAET > 0 {
		p := w.PriceUAET
		d.PriceUAET = &p
	}
	return d
}

// quotedWorkDTO flattens a quote chain for the wire, following it `depth`
// levels down. The bound is store.QuoteRenderDepth — the number of rounds the
// loader attaches — passed in rather than read here so the recursion counts
// down the same number the loader counted up. A nil work or an exhausted depth
// is the end of the chain: no key is emitted.
func quotedWorkDTO(q *store.Work, depth int) *QuotedWorkDTO {
	if q == nil || depth <= 0 {
		return nil
	}
	return &QuotedWorkDTO{
		ID:        q.ID,
		CID:       q.CID,
		Author:    authorDTO(q.Author),
		Body:      q.Body,
		CreatedAt: q.CreatedAt,
		MediaURLs: q.MediaURLs,
		Video:     videoDTO(q),
		IsNSFW:    q.IsNSFW,
		IsGore:    q.IsGore,
		Nested:    quotedWorkDTO(q.QuotedWork, depth-1),
	}
}

func workDTOs(works []*store.Work, provenance func(*store.Work) *ProvenanceDTO) []WorkDTO {
	out := make([]WorkDTO, 0, len(works))
	for _, w := range works {
		var p *ProvenanceDTO
		if provenance != nil {
			p = provenance(w)
		}
		out = append(out, workDTO(w, p))
	}
	return out
}

func notificationDTO(g *store.NotificationGroup) NotificationDTO {
	actors := make([]WorkAuthorDTO, 0, len(g.Actors))
	for _, a := range g.Actors {
		actors = append(actors, authorDTO(a))
	}
	d := NotificationDTO{
		ID:         g.ID,
		Kind:       g.Kind,
		Actors:     actors,
		ActorCount: g.ActorCount,
		TargetID:   strPtr(g.TargetID),
		TargetType: g.TargetType,
		Preview:    strPtr(g.Preview),
		IsRead:     g.IsRead,
		CreatedAt:  g.CreatedAt,
	}
	if g.AmountUAET != 0 {
		a := g.AmountUAET
		d.AmountUAET = &a
	}
	return d
}

// visibilityOr is the stored visibility choice, or the platform default when
// the row predates the column.
func visibilityOr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// kycStatusPtr is the tier as the client reads it: absent (null) when no
// verification has been run, the tier's own word otherwise.
func kycStatusPtr(tier string) *string {
	if tier == "" || tier == "none" {
		return nil
	}
	return &tier
}

// nonNilLinks keeps an empty map an empty object on the wire, never null: the
// Swift side decodes `[String: String]` and treats null as absent.
func nonNilLinks(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}
