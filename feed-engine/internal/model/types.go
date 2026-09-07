package model

import (
	"fmt"
	"time"
)

// ── User ──────────────────────────────────────────────────────────────────────

// User is the full profile view returned by GetUserByHandle.
type User struct {
	ID                  string
	Handle              string
	DisplayName         string
	Bio                 string
	Pronouns            string
	Location            string
	CountryCode         string // ISO 3166-1 alpha-2, used for leaderboard grouping only
	Website             string
	AvatarURL           string
	AvatarAnimated      bool
	HeaderURL           string
	ThemeID             string // void|aurora|sakura|obsidian|moss|dusk
	AccentHex           string
	JungArchetype       string
	PinnedTrackID       string
	SocialLinksRaw      string // raw JSON from DB
	ExternalTipLinksRaw string // raw JSON: {"cashapp":"user","venmo":"user",...}
	IsCreator           bool
	IsVerified          bool
	// KYCTier is the identity tier recorded on this account's PIAL root:
	// KYCTierNone | KYCTierBasic | KYCTierSoft | KYCTierFull. It is folded on
	// from PIAL state, never read off a users/user_profiles column, because PIAL
	// is the single system of record for identity. Read it through CanMonetize()
	// rather than comparing the string — see the tier ladder below.
	KYCTier        string
	OfficialType   string // '' | 'government' | 'business'
	FollowerCount  int
	FollowingCount int
	PostCount      int
	// Computed at app layer — not stored
	IsColdStart       bool
	InterestVector    []float32
	RecentContentIDs  []string
	CreatorAffinities map[string]float32
	// Safety epsilon: set from content_setting
	// safe_mode → 1e-6, default → 0.10, adult_enabled → 1.0
	SafetyEpsilon          float64
	Tier                   string // free|subscriber|creator
	ContentSetting         string // safe_mode|default|adult_enabled
	Realm                  int    // 1–5 progression level — DERIVED (realm.Effective), never hand-set
	RealmGrant             int    // 0 = none, else 1–5: a granted realm FLOOR, see package realm
	XP                     int64  // cumulative experience points
	UnreadCount            int
	Role                   string          // user|admin|founder
	PIALID                 string          // PIAL root anchor UUID
	IsMinor                bool            // true when user_roles.is_minor=true — enforced server-side
	IsAgeVerified          bool            // true when KYC age check passed (user_roles.is_age_verified)
	IsAdult                bool            // true when birthday confirms 18+ (user_roles.is_adult)
	Birthday               *time.Time      // nil if not set
	ShowBirthday           bool            // legacy: whether to display birthday on profile
	BirthdayMdVisibility   string          // "everyone"|"followers"|"mutual_followers"|"only_me"
	BirthdayYearVisibility string          // "everyone"|"followers"|"mutual_followers"|"only_me"
	OrgMemberships         []OrgMembership // populated for profile pages
	MobileFeedView         string          // "standard" | "reels" — user opt-in mobile feed style
	ShowSensitive          bool            // 18+ only: gore/sensitive (IsGore) shown without warning when true
	CelebrationsEnabled    bool            // opt-in: show the active month's celebration theme (June=Pride, etc.)
	IsPrivate              bool            // account privacy: TRUE = profile/posts hidden from public lookup (followers only)
	TwoFAEnabled           bool            // true when TOTP 2FA is active for this account
	IsAdultCreator         bool            // Verity-verified adult creator — all content gated
	AdultCreatorPending    bool            // selected Adult Creator but Verity not yet passed
	IsFoundingCreator      bool            // first-wave creator — permanent gold badge
	FoundingCreatorAt      *time.Time      // when the founding creator badge was granted
	ReferralCode           string          // user's personal referral code
}

// Platform roles. These are the only values users.role is allowed to hold, and
// they are named here so no other package has to spell one as a string literal.
const (
	RoleUser    = "user"
	RoleAdmin   = "admin"
	RoleFounder = "founder"
)

func (u *User) IsAdmin() bool   { return u != nil && (u.Role == RoleAdmin || u.Role == RoleFounder) }
func (u *User) IsFounder() bool { return u != nil && u.Role == RoleFounder }

// ── The identity ladder ───────────────────────────────────────────────────────
//
// pial_roots.kyc_tier holds exactly these four values, and they are not four
// degrees of the same thing. The first two answer "how old does this person say
// they are"; the last two answer "who is this person". A gate that needs the
// second question answered cannot be built out of the first two.
const (
	// KYCTierNone — nothing recorded. No date of birth, no document.
	KYCTierNone = "none"
	// KYCTierBasic — a date of birth typed into the signup form by the account
	// holder. Self-attested: nothing was checked against anything. Every account
	// that completes signup reaches this tier, so it distinguishes nobody from
	// anybody.
	KYCTierBasic = "basic"
	// KYCTierSoft — a government document was read (MRZ) and confirmed 18+.
	// Explicitly no face match and no liveness (see handleMRZAgeVerify), so it
	// proves a real document exists and says 18+; it does not prove the person
	// holding this account is the person named on it.
	KYCTierSoft = "soft"
	// KYCTierFull — government document, face match against that document, and
	// a live challenge-response scan. This is the only tier that binds a named
	// human to the account operating it.
	KYCTierFull = "full"
)

// kycTierRank orders the ladder. Unknown values rank below everything, so a tier
// this build has never heard of can never satisfy a gate by accident.
func kycTierRank(tier string) int {
	switch tier {
	case KYCTierFull:
		return 3
	case KYCTierSoft:
		return 2
	case KYCTierBasic:
		return 1
	case KYCTierNone:
		return 0
	default:
		return -1
	}
}

// KYCTierIsIdentity reports whether a raw pial_roots.kyc_tier value answers the
// identity question — is this account operated by the human named on a
// government document.
//
// The bar is KYCTierFull. KYCTierSoft reads a document but never looks at the
// face holding it, so a scraped passport image satisfies it; that is the exact
// substitution a creator gate exists to prevent. KYCTierBasic is a typed-in
// birthday. The default /kyc walk already produces KYCTierFull in one pass
// (document → face match → liveness → kycSubmit), so this bar adds no door that
// does not already exist.
//
// This is the one place the bar is written down. Call it, or call one of the
// predicates below — never compare kyc_tier at a call site.
func KYCTierIsIdentity(tier string) bool {
	return kycTierRank(tier) >= kycTierRank(KYCTierFull)
}

// IsIdentityVerified reports whether this account carries a documented identity.
// It is the identity fact, with no policy attached.
func (u *User) IsIdentityVerified() bool {
	return u != nil && KYCTierIsIdentity(u.KYCTier)
}

// CanMonetize is THE creator/payout gate. Every surface that makes a person a
// creator, lets them be paid, or hands them a payout address asks this and
// nothing else.
//
// It asks the identity predicate directly rather than going through IsVerified.
// IsVerified is now folded from the same kyc_tier=='full' bar this function
// checks, so the two agree — but IsVerified used to be PIAL age_verified, which
// SetPIALBirthday sets to TRUE the moment a self-reported birthday is typed into
// the signup form, so gating creator status on it let every new signup through
// and could not tell a verified public figure from an anonymous account. This
// function calls IsIdentityVerified() directly rather than IsVerified so it
// never regresses back to that if IsVerified's source ever drifts again.
//
// Admins keep the bypass they hold on every other creator surface in this
// codebase, so operators can work an account that has not been through eKYC.
func (u *User) CanMonetize() bool {
	return u != nil && (u.IsIdentityVerified() || u.IsAdmin())
}

// ── PIAL: Persistent Identity + Access Layer ──────────────────────────────────

// Capability constants — enforcement layer uses only these, never Jung data.
const (
	CapPosting          = "POSTING"
	CapNSFWAccess       = "NSFW_ACCESS"
	CapMonetization     = "MONETIZATION"
	CapRealmProgression = "REALM_PROGRESSION"
	CapNewAccountTrust  = "NEW_ACCOUNT_TRUST"
	CapMessaging        = "MESSAGING"
	CapMusicUpload      = "MUSIC_UPLOAD"
)

// DefaultCapabilities are granted to every new PIAL at onboarding.
var DefaultCapabilities = []string{
	CapPosting, CapRealmProgression, CapNewAccountTrust, CapMessaging, CapMusicUpload, CapMonetization,
}

// CapState values.
const (
	CapStateGranted    = "granted"
	CapStateRestricted = "restricted"
	CapStateRevoked    = "revoked"
	CapStateCooldown   = "cooldown"
)

// PIALRoot is the identity anchor. Carries human-level attributes — one per human, not per account.
type PIALRoot struct {
	PIALID        string
	PublicKey     string
	StateHash     string
	CreatedAt     time.Time
	IsTombstoned  bool   // deprecated: use Status
	Status        string // active | suspended | tombstoned
	DateOfBirth   *time.Time
	KYCTier       string // none | basic | soft | full
	KYCVerifiedAt *time.Time
	AgeVerified   bool
}

// PIALCapability is one node in the capability tree.
type PIALCapability struct {
	PIALID     string
	Capability string
	State      string
	ExpiresAt  *time.Time
	Reason     string
	GrantedBy  string
	UpdatedAt  time.Time
}

func (c *PIALCapability) IsGranted() bool {
	if c == nil {
		return false
	}
	if c.State != CapStateGranted {
		return false
	}
	if c.ExpiresAt != nil && time.Now().After(*c.ExpiresAt) {
		return false
	}
	return true
}

// PIALCapabilityMap is the full capability tree for one PIAL root.
type PIALCapabilityMap map[string]*PIALCapability

func (m PIALCapabilityMap) Can(cap string) bool {
	c, ok := m[cap]
	return ok && c.IsGranted()
}

// LinkedAccount is one account bound to a PIAL root — used in the account switcher.
type LinkedAccount struct {
	ID          string
	Handle      string
	DisplayName string
	AvatarURL   string
	IsPrimary   bool
	IsActive    bool // true when this is the current session's active_account_id
}

// ProfileSave is the write model for saving a profile.
type ProfileSave struct {
	UserID               string
	DisplayName          string
	Bio                  string
	Pronouns             string
	Location             string
	CountryCode          string // ISO 3166-1 alpha-2
	Website              string
	AvatarURL            string
	AvatarAnimated       bool
	HeaderURL            string
	ThemeID              string
	AccentHex            string
	JungArchetype        string
	PinnedTrackID        string
	SocialLinksJSON      string // validated JSON
	ExternalTipLinksJSON string // validated JSON: {provider: username}
	IsAdultCreator       bool   // user-declared adult content creator
}

// ── Post ─────────────────────────────────────────────────────────────────────

// Post is the full post view used in templates.
type Post struct {
	ID                   string
	AuthorID             string
	AuthorHandle         string
	AuthorName           string
	AvatarURL            string
	AvatarSeed           string // handle used to derive gradient avatar colour
	AvatarAnimated       bool
	AuthorRealm          int // 1–5 realm tier for ring display
	IsVerified           bool
	AuthorRole           string // admin | founder | creator | user
	AuthorOfficialType   string // '' | 'government' | 'business'
	AuthorIsAdultCreator bool   // true when author has active adult creator role
	Body                 string
	ContentType          string
	Tags                 []string
	MediaURLs            []string
	// HLS video asset (Sprint 0 / S0.6). Empty when post is not a video.
	// VideoMasterURL is master.m3u8; player loads variants from there.
	VideoMasterURL    string
	VideoPosterURL    string
	VideoDurationSecs float32
	VideoWidth        int
	VideoHeight       int
	// Content lineage — canonical media entity this post references.
	// MediaCreatorHandle is the original uploader (survives reposts).
	CanonicalMediaID   string
	MediaCreatorHandle string
	IsEdited           bool
	IsReply            bool
	ParentID           string
	QuotedPostID       string
	QuotedPost         *Post // populated when QuotedPostID is set
	CreatedAt          time.Time
	TimeAgo            string // e.g. "2m", "4h", "Jan 5"
	Likes              int
	DislikeCount       int
	Reposts            int
	Comments           int
	Saves              int
	Impressions        int
	ViewTimeSeconds    float64 // mean view time in seconds for engagement signal
	// AethyrRank metadata
	ContentID       string
	FinalScore      float64
	ExplorationSlot bool
	AesqAlignment   float64
	VelocityBoost   float64
	Explanation     string
	// Pre-computed from feedback_events before sending to AethyrRank (overrides static metrics)
	FeedVelocity float64
	// Comment audience control — set by post author
	// Values: "open" | "followers" | "verified" | "none"
	CommentGating           string
	IsNSFW                  bool
	ContentIsSensitive      bool
	ContentIsSubscriberOnly bool
	// ScanState: pending_scan | clean | age_gated | flagged | human_review | blocked
	ScanState            string
	ViewerIsSubscribed   bool
	LatestReplierHandles []string
	LatestReplierAvatars []string
	AuthorPIAL           string
	IsAuthorOnline       bool
	// Interaction state for current viewer
	LikedByUser         bool
	DislikedByUser      bool
	SavedByUser         bool
	RepostedByUser      bool
	BookmarkedByUser    bool
	ViewerFollowsAuthor bool // viewer follows this post's author — used in focus follow dot
	// Repost context — set when this post appears in a profile's Reposts tab.
	// RepostedByHandle is the handle of the user who reposted (not the author).
	RepostedByHandle string
	RepostedAt       time.Time
	// Voice post: audio-only post with a waveform player. Empty when not a voice post.
	VoiceURL      string
	VoiceDuration float32
	// Poll data (nil if post has no poll)
	Poll *Poll
	// Thread: number of continuation posts by the same author chained below this one.
	// 0 means not a thread root (or thread with only one segment).
	ThreadCount int
	// IsPinned is set to true when this post is pinned on the author's profile page.
	IsPinned bool
	// ScheduledAt is the time the post should be published. Nil means immediate.
	ScheduledAt *time.Time
	// LinkPreview is populated when the post body contains a URL whose OG metadata
	// was successfully fetched. Nil when no preview is available.
	LinkPreview *LinkPreview
	// Cashtag is populated when the post was composed with a $TICKER attachment.
	Cashtag *CashtagEmbed
	// YouTubeID is extracted at render time from the post body when a YouTube URL is present.
	// It is never stored — populated during scanPosts / GetPostByID.
	YouTubeID string
	// Vision: ephemeral post from the Visions camera. Expires 24h after creation.
	IsVision  bool
	ExpiresAt *time.Time
	// Repost: IsRepost=true when this post is a reshare of another post.
	// RepostSourceID points to the original. Body is empty for reposts.
	IsRepost       bool
	RepostSourceID string
	// Content lineage from content-scan dedup engine.
	// LineageHandle is the handle of the ORIGINAL uploader when this video is a duplicate.
	// Empty means this video is an original (or scan not yet completed).
	LineagePIAL        string
	LineageHandle      string
	LineageDisplayName string // display name of the original creator (resolved at query time)
	// Primary org badge — the employee's primary org account, shown on posts and profile
	PrimaryOrgAvatarURL   string
	PrimaryOrgHandle      string
	PrimaryOrgDisplayName string
	PrimaryOrgType        string // 'business' | 'government'
}

// CashtagEmbed is the stock ticker card stored with a post at time of posting.
type CashtagEmbed struct {
	Ticker      string
	CompanyName string
	PriceAtPost float64
	ChangePct   float64
	// PriceStr and ChangeStr are pre-formatted for the template.
	PriceStr   string
	ChangeStr  string
	IsPositive bool
}

// LinkPreview holds Open Graph metadata fetched server-side for the first URL in a post.
type LinkPreview struct {
	URL         string
	Title       string
	Description string
	ImageURL    string
	SiteName    string
}

// Poll is embedded in Post when the post has poll_options set.
type Poll struct {
	Options    []string     // the option labels
	Votes      []int        // vote count per option (same index as Options)
	TotalVotes int          // sum of all votes
	UserVote   int          // index user voted for, -1 if not voted
	EndsAt     *time.Time   // nil = no expiry
	Ended      bool         // true if ends_at is in the past
	Results    []PollResult // precomputed display data
	// TimeLeft is the pre-formatted remaining-time label ("3 hours left",
	// "2 days left", "Final results"). The server owns this string so the
	// fragment carries it rendered — the browser never computes poll state.
	TimeLeft string
	// Closed is true when the ballot no longer accepts votes: the poll ended.
	// The renderer reads this instead of re-deriving it from EndsAt.
	Closed bool
}

// PollResult is a single option with precomputed display values.
type PollResult struct {
	Label    string
	Votes    int
	Pct      int  // 0–100, precomputed
	Voted    bool // true if this is what the current user voted
	IsWinner bool // true for the leading option (ties: every tied option)
}

// Project computes every rendered value the ballot facet draws — tallies,
// percentages, the winning option, the closed flag, and the remaining-time
// label — from the raw inputs (Options, Votes, UserVote, EndsAt).
//
// This is the ONLY definition of a poll's display state. Both the batch feed
// enrichment and the single-poll read after a vote call it, so the ballot a
// voter sees swapped in is the same ballot the feed drew. The browser never
// computes any of it.
func (p *Poll) Project(now time.Time) {
	if p == nil {
		return
	}
	if len(p.Votes) != len(p.Options) {
		votes := make([]int, len(p.Options))
		copy(votes, p.Votes)
		p.Votes = votes
	}

	p.TotalVotes = 0
	best := 0
	for _, v := range p.Votes {
		p.TotalVotes += v
		if v > best {
			best = v
		}
	}

	p.Ended = p.EndsAt != nil && !p.EndsAt.After(now)
	p.Closed = p.Ended
	p.TimeLeft = pollTimeLeft(p.EndsAt, now)

	p.Results = make([]PollResult, len(p.Options))
	for i, opt := range p.Options {
		pct := 0
		if p.TotalVotes > 0 {
			// Rounded, not truncated: a two-of-three lead reads 67%, not 66%.
			pct = int((float64(p.Votes[i])*100.0)/float64(p.TotalVotes) + 0.5)
		}
		p.Results[i] = PollResult{
			Label:    opt,
			Votes:    p.Votes[i],
			Pct:      pct,
			Voted:    p.UserVote == i,
			IsWinner: p.TotalVotes > 0 && p.Votes[i] == best,
		}
	}
}

// pollTimeLeft renders the ballot's remaining-time label. A poll with no expiry
// never closes and carries no label; a closed poll reads "Final results".
func pollTimeLeft(endsAt *time.Time, now time.Time) string {
	if endsAt == nil {
		return ""
	}
	d := endsAt.Sub(now)
	if d <= 0 {
		return "Final results"
	}
	// Rounded up, never down: a ballot with 2h59m to run has three hours left,
	// and truncation would tell the voter it has two.
	switch {
	case d <= time.Minute:
		return "Less than a minute left"
	case d <= time.Hour:
		return plural(ceilUnits(d, time.Minute), "minute") + " left"
	case d <= 24*time.Hour:
		return plural(ceilUnits(d, time.Hour), "hour") + " left"
	default:
		return plural(ceilUnits(d, 24*time.Hour), "day") + " left"
	}
}

// ceilUnits divides d by unit, rounding up. d is always positive here.
func ceilUnits(d, unit time.Duration) int {
	return int((d + unit - 1) / unit)
}

// plural renders "1 hour" / "3 hours" — the server owns the string so the
// fragment carries it finished.
func plural(n int, unit string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", unit)
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

// ThreadPost wraps a Post with precomputed thread-line state for the post detail page.
// HasLineAbove/HasLineBelow drive .thread-line-above / .thread-line-below rendering.
// The template reads these flags directly — it never computes line state itself.
type ThreadPost struct {
	Post                     *Post
	HasLineAbove             bool
	HasLineBelow             bool
	IsFocused                bool
	IsAncestor               bool
	IsReply                  bool
	IsSameAuthorContinuation bool
	Depth                    int
}

// ── Feed page data ────────────────────────────────────────────────────────────

// NexusPersona represents one account linked under a NEXUS identity.
// nexus_id is NEVER stored here — only the blinded pial_shard_id.
type NexusPersona struct {
	PIALShardID  string
	Handle       string
	DisplayName  string
	AvatarURL    string
	PersonaType  string // "personal" | "creator" | "business"
	DisplayLabel string
	IsActive     bool
	IsPrimary    bool
	UnreadCount  int
}

// NexusContext carries multi-persona state for the sidebar persona switcher.
// Nil when the user has no NEXUS (single-account path).
type NexusContext struct {
	Personas            []NexusPersona
	ActivePersona       *NexusPersona
	TotalUnreadCount    int
	TotalUnreadMessages int
}

type FeedPage struct {
	User       *User
	Posts      []*Post
	Surface    string
	SessionID  string
	After      string
	Title      string
	ShowScores bool
	// Engine status
	EngineOnline bool
	// 6 available themes for the switcher
	Themes []Theme
	// NEXUS multi-persona context — nil for single-account users
	Nexus *NexusContext
}

// Theme represents one of the 6 F33D3R visual themes.
type Theme struct {
	ID          string
	Name        string
	Vibe        string
	Surface     string // hex background colour
	Accent      string // hex accent colour
	AccentMuted string // hex muted accent
	Active      bool   // is this the user's current theme
}

// AllThemes returns the complete set of 6 F33D3R themes.
func AllThemes() []Theme {
	return []Theme{
		{ID: "void", Name: "Void", Vibe: "Deep Space", Surface: "#08080F", Accent: "#7B68EE", AccentMuted: "#4A3F9A"},
		{ID: "aurora", Name: "Aurora", Vibe: "Flow State", Surface: "#06101A", Accent: "#3DD4BE", AccentMuted: "#238A7B"},
		{ID: "sakura", Name: "Sakura", Vibe: "Soft Power", Surface: "#120A0F", Accent: "#E8A0B4", AccentMuted: "#9E5E70"},
		{ID: "obsidian", Name: "Obsidian", Vibe: "Sharp Edge", Surface: "#0A0A18", Accent: "#8B9BE8", AccentMuted: "#4E5BA0"},
		{ID: "moss", Name: "Moss", Vibe: "Root System", Surface: "#080F08", Accent: "#7DC98A", AccentMuted: "#427D4D"},
		{ID: "dusk", Name: "Dusk", Vibe: "Golden Hour", Surface: "#120E07", Accent: "#D4A96A", AccentMuted: "#8A6535"},
	}
}

// ── Org membership ────────────────────────────────────────────────────────────

// OrgInfo is a lightweight view of a business/government account.
type OrgInfo struct {
	ID          string
	Handle      string
	DisplayName string
	AvatarURL   string
	OrgType     string // 'business' | 'government'
}

// OrgMembership is one user↔org association record.
type OrgMembership struct {
	ID          string
	Org         OrgInfo
	Status      string // pending_employee | pending_org | approved | rejected | revoked | suspended
	IsPrimary   bool
	InitiatedBy string // 'employee' | 'org'
	CreatedAt   time.Time
}

// OrgVerificationApplication is a request from a user to obtain an official_type badge.
type OrgVerificationApplication struct {
	ID          string
	UserID      string
	UserHandle  string
	DisplayName string
	AvatarURL   string
	OrgType     string // 'business' | 'government'
	OrgName     string
	OrgWebsite  string
	Description string
	EvidenceURL string
	Status      string // pending | approved | rejected
	AdminNotes  string
	CreatedAt   time.Time
}

// OrgApplicationRow is a row in the org owner panel (pending/approved members).
type OrgApplicationRow struct {
	MembershipID string
	UserID       string
	Handle       string
	DisplayName  string
	AvatarURL    string
	IsVerified   bool
	Status       string
	InitiatedBy  string
	CreatedAt    time.Time
}

// ── Track ─────────────────────────────────────────────────────────────────────

// Track is a music upload — the core content unit of the Zior brain.
type Track struct {
	ID           string
	AuthorID     string
	AuthorHandle string
	AuthorName   string
	AvatarURL    string
	IsVerified   bool
	Title        string
	Description  string
	AudioURL     string
	CoverURL     string
	DurationSecs int
	Genre        string
	Tags         []string
	PriceCents   int
	IsFree       bool
	PlayCount    int
	LikeCount    int
	CreatedAt    time.Time
	TimeAgo      string
	LikedByUser  bool
}

// ── Feed surfaces ─────────────────────────────────────────────────────────────

// FeedSurface is a named interest surface users can pin to their tab bar.
// Phase 1: tag/content_type bridge routing. Phase 2: Zior semantic scores.
type FeedSurface struct {
	ID          string
	Label       string
	Emoji       string
	SurfaceType string   // "interest" | "behavioral"
	Tags        []string // Phase 1 bridge: posts WHERE tags && these tags
	ContentType string   // Phase 1 bridge: posts WHERE content_type = this
	SortOrder   int
	IsPinned    bool // populated by GetAllSurfaces for the current user
}

// ── Music page data ───────────────────────────────────────────────────────────

type MusicPage struct {
	User               *User
	PersonalizedTracks []*Post
	Clusters           []MusicCluster
	Surface            string
	SessionID          string
	ShowScores         bool
}

// MusicCluster is a Zior-detected micro-community of tracks.
type MusicCluster struct {
	ClusterID    uint32
	TrackCount   int
	DominantMood string
	TrackIDs     []string
}

// ── Profile page data ─────────────────────────────────────────────────────────

type ProfilePage struct {
	User       *User
	Posts      []*Post
	IsOwner    bool
	ShowScores bool
	Themes     []Theme
}

// ── Settings page data ────────────────────────────────────────────────────────

type SettingsPage struct {
	User        *User
	Themes      []Theme
	ShowScores  bool
	SaveSuccess bool
}

// ── AethyrRank wire types ─────────────────────────────────────────────────────
// Mirror of aethyrrank-engine src/api/types.rs — must stay in sync.

type AethyrRankRequest struct {
	UserState      AethyrUserState `json:"user_state"`
	ContentPool    []AethyrContent `json:"content_pool"`
	SessionContext AethyrSession   `json:"session_context"`
}

type AethyrUserState struct {
	UserID             string             `json:"user_id"`
	InterestVector     []float32          `json:"interest_vector"`
	InteractionHistory []string           `json:"interaction_history"`
	IsColdStart        bool               `json:"is_cold_start"`
	CreatorAffinities  map[string]float32 `json:"creator_affinities"`
	SafetyEpsilon      float64            `json:"safety_epsilon"`
	UserTier           string             `json:"user_tier"`
	RealmLevel         int                `json:"realm_level"` // 1–5; used as trust weight in bandit
}

type AethyrContent struct {
	ContentID             string           `json:"content_id"`
	CreatorID             string           `json:"creator_id"`
	TopicVector           []float32        `json:"topic_vector"`
	PublishedAt           time.Time        `json:"published_at"`
	Engagement            AethyrEngagement `json:"engagement"`
	ExposureCount         uint64           `json:"exposure_count"`
	CreatorExposure       uint64           `json:"creator_exposure"`
	Tags                  []string         `json:"tags"`
	ContentType           string           `json:"content_type"`
	VelocityScore         float64          `json:"velocity_score"`
	EarlyRetention        float64          `json:"early_retention"`
	CompletionRate        float64          `json:"completion_rate"`
	ConversionProbability float64          `json:"conversion_probability"`
	CreatorRevenueRate    float64          `json:"creator_revenue_rate"`
	LtvEstimate           float64          `json:"ltv_estimate"`
	AdultProbability      float64          `json:"adult_probability"`
	// Content quality signals
	PostsLast24h     uint64  `json:"posts_last_24h"`
	SelfReplyCadence float64 `json:"self_reply_cadence"`
	CharCount        uint64  `json:"char_count"`
	IsInNetwork      bool    `json:"is_in_network"`
}

type AethyrEngagement struct {
	Likes           float64 `json:"likes"`
	Shares          float64 `json:"shares"`
	Comments        float64 `json:"comments"`
	Saves           float64 `json:"saves"`
	ViewTimeSeconds float64 `json:"view_time_seconds"`
	Impressions     float64 `json:"impressions"`
}

type AethyrSession struct {
	Surface   string    `json:"surface"`
	RequestID string    `json:"request_id"`
	SessionID string    `json:"session_id"`
	MaxItems  int       `json:"max_items"`
	Timestamp time.Time `json:"timestamp"`
}

type AethyrRankResponse struct {
	RequestID   string         `json:"request_id"`
	RankedItems []AethyrRanked `json:"ranked_items"`
	Confidence  float64        `json:"confidence"`
	LatencyMs   uint64         `json:"latency_ms"`
}

type AethyrRanked struct {
	ContentID       string          `json:"content_id"`
	Rank            int             `json:"rank"`
	FinalScore      float64         `json:"final_score"`
	ScoreBreakdown  AethyrBreakdown `json:"score_breakdown"`
	Explanation     string          `json:"explanation"`
	ExplorationSlot bool            `json:"exploration_slot"`
	SafetyBlocked   bool            `json:"safety_blocked"`
}

type AethyrBreakdown struct {
	FastScore     float64 `json:"fast_score"`
	NeuralScore   float64 `json:"neural_score"`
	AesqAlignment float64 `json:"aesq_alignment"`
	VelocityBoost float64 `json:"velocity_boost"`
	RevenueAdj    float64 `json:"revenue_adj"`
	FinalScore    float64 `json:"final_score"`
}

// AethyrFeedbackRequest mirrors FeedbackRequest in the engine.
type AethyrFeedbackRequest struct {
	UserID    string                `json:"user_id"`
	SessionID string                `json:"session_id"`
	Surface   string                `json:"surface"`
	Events    []AethyrFeedbackEvent `json:"events"`
}

type AethyrFeedbackEvent struct {
	ContentID         string    `json:"content_id"`
	EventType         string    `json:"event_type"`
	PositionAtDisplay int       `json:"position_at_display"`
	Timestamp         time.Time `json:"timestamp"`
	DwellMs           *int64    `json:"dwell_ms,omitempty"`
	ExplorationSlot   bool      `json:"exploration_slot"`
}

// BrainStatus is the response from AethyrRank GET /brain/status.
type BrainStatus struct {
	Brains []BrainEntry `json:"brains"`
}

type BrainEntry struct {
	BrainID     string `json:"brain_id"`
	Status      string `json:"status"`
	Port        int    `json:"port"`
	Description string `json:"description"`
}

// LeaderboardEntry is one row in the daily XP leaderboard.
type LeaderboardEntry struct {
	Rank        int
	UserID      string
	Handle      string
	DisplayName string
	AvatarURL   string
	Realm       int
	XPToday     int64
}

// Achievement represents a single platform achievement definition.
type Achievement struct {
	ID          string
	Name        string
	Description string
	Icon        string
	Tier        string // bronze | silver | gold | platinum (legacy)
	Rarity      string // common | uncommon | rare | epic | legendary | mythic
	Category    string // onboarding | social | creator | music | wellness | culture | real_life | community | economy | secret
	XPReward    int
	EarnedAt    *time.Time // nil if not earned
}

// ── Shop page data ────────────────────────────────────────────────────────────

type ShopPlan struct {
	ID           string
	Name         string
	Description  string
	PriceAET     int
	PriceDisplay string
	IsActive     bool
}

type ShopEarnings struct {
	TotalDisplay string
	TxCount      int
	SubsDisplay  string
	TipsDisplay  string
}

type PPVItem struct {
	ID           string
	Title        string
	PriceAET     int
	PriceDisplay string
	IsActive     bool
}

type ShopPage struct {
	User         *User
	ShopUser     *User
	IsOwner      bool
	IsEnabled    bool
	Plans        []ShopPlan
	Earnings     *ShopEarnings
	IsSubscribed bool
	PPVItems     []PPVItem
}

// ── KYC / verification page data ─────────────────────────────────────────────

type KYCPage struct {
	User          *User
	VerityTier    int
	AgeBand       string
	PayoutEnabled bool
	NSFWAccess    bool
	Has2257       bool
}

// ── Security / threat detection ───────────────────────────────────────────────

type SecurityEvent struct {
	ID        string
	EventType string
	Severity  string
	UserID    string
	IPAddress string
	UserAgent string
	Path      string
	Details   map[string]interface{}
	CreatedAt time.Time
}

type BlockedIP struct {
	IPAddress   string
	Reason      string
	AutoBlocked bool
	CreatedAt   time.Time
	ExpiresAt   *time.Time
}

type AdminAuditEntry struct {
	ID          string
	AdminID     string
	AdminHandle string
	Action      string
	TargetType  string
	TargetID    string
	IPAddress   string
	CreatedAt   time.Time
}

type AdminSecurityPage struct {
	User                 *User
	TotalEventsToday     int
	CriticalEvents       int
	BlockedIPCount       int
	AdminActionCount     int
	RecentEvents         []SecurityEvent
	BlockedIPs           []BlockedIP
	AuditLog             []AdminAuditEntry
	PendingSecurityCount int
}

// ── Achievements page data ────────────────────────────────────────────────────

type AchievementsPage struct {
	User         *User
	Achievements []Achievement
	EarnedCount  int
	TotalCount   int
	XPTotal      int
	XPPercent    int
	CurrentTier  string
	NextTier     string
	XPToNext     int
}

// ── Articles ──────────────────────────────────────────────────────────────────

type Article struct {
	ID            string
	AuthorID      string
	AuthorHandle  string
	AuthorDisplay string
	AuthorAvatar  string
	Slug          string
	Title         string
	Body          string // raw Markdown — used in editor
	BodyHTML      string // goldmark-rendered HTML — used in reader (safeHTML in template)
	Excerpt       string
	CoverURL      string
	Status        string // draft | published | archived
	PublishedAt   *time.Time
	ViewCount     int64
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// ── Works (content-addressed, signed) ─────────────────────────────────────────

// Work is a content-addressed authored work signed by Malkuth.
type Work struct {
	ID         string
	CID        string // "sha256:{hex}" — content-addressed identity
	AuthorID   string
	AuthorPIAL string
	Body       string
	Kind       string // closed set: post|reply|quote|poll|video|voice|thread_post|react_video
	MediaURLs  []string
	// ReactLayout is the React-With-Video arrangement applied at view time
	// (presenter|pip|split|card_over|media_only|green_screen). Empty for non-RWV works.
	ReactLayout string
	IsNSFW      bool
	IsGore      bool
	IsBlocked   bool
	ExpiresAt   *time.Time
	CreatedAt   time.Time
	DeletedAt   *time.Time
	// Citations (populated when present)
	ParentCID  string // reply-to CID, empty when not a reply
	QuotedCID  string // quoted-work CID, empty when not a quote
	QuotedWork *Work  // full quoted work, populated by EnrichWorksWithQuotes
	// Editions (populated on-demand for detail view)
	Editions []Edition
	// Author display — joined at query time
	AuthorHandle         string
	AuthorName           string
	AvatarURL            string
	IsVerified           bool
	AuthorRole           string // user|creator|admin|founder
	AuthorRealm          int    // 1–5
	AuthorOfficialType   string // ''|'government'|'business'
	AuthorIsCreator      bool
	AuthorIsAdultCreator bool // true when author has active adult creator role — used for feed filtering
	// Aggregated reaction counts
	LikeCount     int
	RepostCount   int
	BookmarkCount int
	// Viewer interaction state
	LikedByUser         bool
	RepostedByUser      bool
	BookmarkedByUser    bool
	DislikedByUser      bool
	ViewerFollowsAuthor bool
	// Repost attribution — set when this work is surfaced in a feed because
	// someone reposted it (X-style "<name> reposted" header). The reposter, not
	// the author; RepostedAt is when the repost happened (the feed sort time).
	RepostedByHandle string
	RepostedByName   string
	RepostedAt       time.Time
	// Formatted
	TimeAgo string

	// ── Extended fields (migrated from posts) ─────────────────────────────────

	// Content classification
	IsRepost       bool
	RepostSourceID string // UUID of the original work; empty when not a repost
	IsSensitive    bool
	SubscriberOnly bool
	CommentGating  string // open | followers | verified | none
	ScheduledAt    *time.Time

	// Taxonomy
	Tags        []string
	ContentType string // text | image | video | voice | poll | article

	// Poll (nil when not a poll)
	PollOptions []string   // option labels, as stored on works.poll_options
	PollEndsAt  *time.Time // works.poll_ends_at; nil = never closes

	// Poll holds the rendered ballot state — tallies, percentages, the viewer's
	// own choice, closed/time-remaining — computed server-side by
	// db.EnrichWorksWithPolls. Nil until enriched, and always nil when
	// PollOptions is empty.
	Poll *Poll

	// Voice post
	VoiceURL          string
	VoiceDurationSecs float32

	// Video (HLS) — two-file architecture:
	// VideoMasterURL is the clean stream (no burn), served to the in-app player.
	// VideoWatermarkedURL is the moving-watermark stream, what escapes the platform.
	VideoMasterURL      string
	VideoWatermarkedURL string
	VideoPosterURL      string
	VideoDurationSecs   float32
	VideoWidth          int
	VideoHeight         int

	// Lineage — original uploader when content-scan detects a duplicate
	LineagePIAL        string
	LineageHandle      string
	LineageDisplayName string // display name of the original creator (resolved at query time)
	MediaCreatorHandle string // canonical original uploader handle (survives reposts)

	// Legacy link — original posts.id for rows migrated from the posts table
	LegacyPostID string

	// Denormalised counters (updated by trigger or batch)
	DislikeCount int
	QuoteCount   int
	ReplyCount   int
	ViewCount    int

	// Scan / moderation
	ScanState string // pending | clean | age_gated | flagged | human_review | blocked
	ScoreBand string // trending | rising | steady | fading

	// Reply preview — up to 3 recent repliers, populated by EnrichWorksWithRepliers
	LatestReplierHandles []string
	LatestReplierAvatars []string

	// Rich content — populated at query time, never stored
	YouTubeID         string        // extracted from body when a YouTube URL is present
	YouTubePlaylistID string        // list= param from YouTube URL, enables playlist continuation in player
	LinkPreview       *LinkPreview  // OG metadata for the first external URL in body (nil if none)
	Cashtag           *CashtagEmbed // first $TICKER in body — lazy-loads the cashtag_card facet (nil if none)

	// IsPinned is set to true when this work is the author's pinned post on their profile.
	IsPinned bool

	// PsychVector is the work's position on the eight Jung axes
	// (internal/jung, mirroring zior-engine/src/jung/axes.rs), as stored in
	// works.psych_vector. Nil when the column is NULL — a work inserted before
	// the Jung layer existed — in which case the feed computes it with
	// jung.MapWork on first read and writes it back. It is the topic_vector
	// AethyrRank scores against a person's interest vector.
	PsychVector []float32

	// IsEdited is true when the work has been updated after initial posting.
	IsEdited bool
	// EditedAt is the timestamp of the most recent edit (nil if never edited).
	EditedAt *time.Time
}

// Edition is one immutable snapshot of a Work's body.
type Edition struct {
	ID            string
	WorkID        string
	CID           string
	Body          string
	EditionNumber int
	CreatedAt     time.Time
}

// MicroconversationParticipant is one distinct author in a microconversation.
type MicroconversationParticipant struct {
	Handle    string
	AvatarURL string
}

// Microconversation is a reply subtree rooted at a direct reply to a focal work.
// All posts in this exchange share a colored left-border container on the detail page
// instead of thread lines drawn between elements.
type Microconversation struct {
	ConversationID string // seed reply ID
	AccentClass    string // mc-cobalt | mc-violet | mc-rose | mc-amber | mc-emerald | mc-sky | mc-fuchsia | mc-teal
	SeedWork       *Work
	Participants   []MicroconversationParticipant
	ReplyCount     int // total sub-replies in this subtree
	MoreCount      int // ReplyCount minus shown exchanges — > 0 triggers "View N more" button
	Exchanges      []*Work
	IsExpanded     bool
}

// ReplyStreamItem is one entry in the merged reply stream on the work detail page.
// Either a microconversation container (IsMicroconversation=true) or a lone reply card.
// The stream is sorted by SortTime so temporal order is preserved across both types.
type ReplyStreamItem struct {
	IsMicroconversation bool
	Microconversation   *Microconversation
	Work                *Work
	SortTime            time.Time
}

// ── Vision (ephemeral lane) ───────────────────────────────────────────────────

// Vision is a 24-hour object. It is not a Work and carries no Work lifecycle:
// no catalog entry, no edition, no content receipt, no naming-plane edge. It
// has no id of its own: it is a position in its author's PIAL-owned lane,
// addressed as (AuthorPIAL, Seq) — see db.VisionRef.
type Vision struct {
	AuthorPIAL  string
	Seq         int64
	ContentType string // text | image | video | share
	Body        string

	// Server-side artboard selection for text Visions. Preset keys, resolved
	// by the renderer — never CSS supplied by the browser.
	ArtboardBackground string
	ArtboardTypeface   string
	ArtboardTypeScale  string // auto | s | m | l | xl
	ArtboardAlign      string // left | center | right

	MediaURLs    []string
	SharedWorkID string // UUID of a Work shared into this Vision; empty when none
	ScanState    string
	IsNSFW       bool
	IsBlocked    bool
	CreatedAt    time.Time
	ExpiresAt    time.Time
	DeletedAt    *time.Time
	Metadata     []byte // raw JSONB

	// The room around the Vision, as the native clients draw it (migration
	// 0022). PollOptions is empty when the Vision carries no poll; ViewerVote
	// is -1 until the viewer has voted.
	PollOptions  []string
	Audience     string // everyone | followers
	AllowReplies bool
	ViewerVote   int

	// Author display — joined at query time.
	AuthorHandle string
	AuthorName   string
	AvatarURL    string

	// Viewer state — computed server-side, never in the browser.
	ViewedByViewer bool
	ViewCount      int
	TimeAgo        string
}

// Ring states for an author's avatar.
const (
	VisionRingNone   = "none"
	VisionRingUnseen = "unseen"
	VisionRingSeen   = "seen"
)

// VisionRing is one author's live Vision set as the viewer sees it.
type VisionRing struct {
	AuthorPIAL    string
	AuthorHandle  string
	AuthorName    string
	AvatarURL     string
	State         string // VisionRingNone | VisionRingUnseen | VisionRingSeen
	Count         int
	UnseenCount   int
	NextVisionRef string // canonical `<author_pial>.<seq>` address where the sequential viewer opens
	LatestAt      time.Time
}

// VisionViewer is one entry in a Vision's viewer list.
type VisionViewer struct {
	ViewerPIAL string
	Handle     string
	Name       string
	AvatarURL  string
	ViewedAt   time.Time
}

// ── Live lane ────────────────────────────────────────────────────────────────

// Live stream statuses. Only the server sets these: idle -> live -> ended.
const (
	LiveStatusIdle  = "idle"
	LiveStatusLive  = "live"
	LiveStatusEnded = "ended"
)

// LiveStream is one broadcast as the server knows it. It carries no video and no
// ingest secret — the ingest key lives in live_ingest_keys and is never rendered.
type LiveStream struct {
	ID          string
	AuthorID    string
	AuthorPIAL  string
	Title       string
	Description string
	Status      string // LiveStatusIdle | LiveStatusLive | LiveStatusEnded
	StartedAt   *time.Time
	EndedAt     *time.Time
	ViewerCount int
	PeakViewers int
	PosterURL   string
	PlaylistURL string // master playlist, ABR
	IsNSFW      bool
	IsBlocked   bool
	ScanState   string
	CreatedAt   time.Time

	// Source geometry as the media server observed it. Zero until the first
	// keyframe arrives; the ABR ladder is derived from it and never upscales.
	SourceWidth  int
	SourceHeight int
	IngestProto  string

	// The ladder that is actually being produced, as the capacity governor
	// planned it — not the ladder the source could support. Under load the two
	// differ, and a surface that renders the second is telling a viewer a rung
	// exists that nothing is writing.
	Ladder  string
	Encoder string
	// Degraded reports that this broadcast is being served less than the full
	// ladder its source could support because the platform is at capacity.
	Degraded bool
	// CapacityNote is what to tell the broadcaster, in their words: why a
	// ladder was reduced, or why a publish was refused. Empty when there is
	// nothing to say.
	CapacityNote string

	// The room around the broadcast (migration 0022): who may watch, the
	// lane it was filed under, the tip goal and running total in µAET, the
	// broadcaster's pinned line, hearts, chat lines, and whether the replay
	// is kept.
	Audience     string // everyone | followers | subscribers
	Lane         string
	TipGoalUAET  int64
	TipTotalUAET int64
	PinnedBody   string
	HeartCount   int
	ChatLines    int
	SaveReplay   bool

	// Author display — joined at query time.
	AuthorHandle string
	AuthorName   string
	AvatarURL    string
}
