package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
)

// ── /api/v1 reads ─────────────────────────────────────────────────────────────
//
// Each handler here is the JSON twin of a Facet fetch: it selects works with
// the same query, filters them for the same viewer with gateWorks,
// enriches them with the one enrichment sequence (enrichWorks) and projects
// them through the DTOs. Nothing here decides what a viewer may see that the
// HTML surface does not decide the same way.

// apiV1Feed — GET /api/v1/feed?surface=&limit=&cursor=
//
// The surface ids are the ones the app's lane strip uses: following, foryou,
// trending, music, visions, live. Anything else is `400 unknown_surface`.
func (h *Handler) apiV1Feed(w http.ResponseWriter, r *http.Request, u *model.User) {
	surface := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("surface")))
	if surface == "" {
		surface = "following"
	}
	limit := apiLimit(r)
	cursorRaw := r.URL.Query().Get("cursor")
	if _, ok := apiCursor(cursorRaw); !ok {
		apiError(w, http.StatusBadRequest, "bad_cursor", "That page cursor is not valid.")
		return
	}
	if surface == "live" {
		// The lane is known; no works live in it. The count is the number of broadcasts
		// on air right now.
		n := h.apiLiveCount()
		apiJSON(w, http.StatusOK, WorkPageDTO{Works: []WorkDTO{}, LiveCount: &n})
		return
	}
	q := &timelineQuery{Viewer: u, Limit: limit, Before: cursorRaw, SessionID: r.URL.Query().Get("session_id")}
	var spec *timelineSpec
	switch surface {
	case "tag":
		// One hashtag's works, newest first: the screen behind a tapped tag.
		q.Tag = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(r.URL.Query().Get("tag"), "#")))
		if q.Tag == "" {
			apiError(w, http.StatusBadRequest, "tag_required", "Which tag?")
			return
		}
		spec = specTagLane
	default:
		spec = h.timelineSurface(surface, q)
		if spec == nil && surface != "music" {
			// The app's lanes can name any surface the database defines.
			if def, derr := dbpkg.GetSurfaceByID(h.db, surface); derr == nil && def != nil {
				q.Def, spec = def, specInterest
			}
		}
		// The music lane is the pinned surface of that name when the deployment defines
		// one, and otherwise the audio works tagged music.
		if spec == nil && surface == "music" {
			spec = specMusic
		}
	}
	if spec == nil || spec.WebOnly || strings.HasPrefix(spec.Name, "profile_") {
		apiError(w, http.StatusBadRequest, "unknown_surface", fmt.Sprintf("No surface named %q.", surface))
		return
	}
	result, refusal, err := h.runTimeline(r.Context(), spec, q)
	if err != nil {
		apiServerError(w, fmt.Errorf("feed %s: %w", surface, err))
		return
	}
	if refusal != nil {
		apiError(w, refusal.Status, refusal.Code, refusal.Message)
		return
	}
	var prov func(*model.Work) *ProvenanceDTO
	if spec.Provenance != nil {
		prov = spec.Provenance(u)
	}
	// Every page carries the live count so the lane strip's badge is right whichever
	// lane the reader is on.
	liveCount := h.apiLiveCount()
	apiJSON(w, http.StatusOK, WorkPageDTO{
		Works:      workDTOs(result.Works, prov),
		NextCursor: result.Next,
		LiveCount:  &liveCount,
	})
}

// apiLiveCount is how many broadcasts are on air right now.
func (h *Handler) apiLiveCount() int {
	ids, err := dbpkg.ListLiveStreamIDs(h.db)
	if err != nil {
		log.Printf("[api/v1] live count: %v", err)
		return 0
	}
	return len(ids)
}

// apiPrepareWorks is the viewer-relative pipeline every list of works passes
// through before projection: adult stripping, the blocked and muted authors
// removed, then the full enrichment sequence.
func (h *Handler) apiPrepareWorks(works []*model.Work, u *model.User) []*model.Work {
	if len(works) == 0 {
		return works
	}
	works = h.gateWorks(u, works)
	h.enrichWorks(works, u)
	h.gateQuotes(u, works)
	return works
}

// Provenance is the surface's one-line account of why a row is on it: a
// follow, a repost by someone followed, a rank. Never how ranking works.

func apiFollowingProvenance(viewer *model.User) func(*model.Work) *ProvenanceDTO {
	return func(w *model.Work) *ProvenanceDTO {
		if w.RepostedByHandle != "" && w.RepostedByHandle != viewer.Handle {
			return &ProvenanceDTO{Kind: "reposted", Text: "Reposted by @" + w.RepostedByHandle, Handle: strPtr(w.RepostedByHandle)}
		}
		if w.AuthorID != viewer.ID {
			return &ProvenanceDTO{Kind: "you_follow", Text: "You follow @" + w.AuthorHandle, Handle: strPtr(w.AuthorHandle)}
		}
		return nil
	}
}

func apiDiscoveryProvenance(viewer *model.User) func(*model.Work) *ProvenanceDTO {
	return func(w *model.Work) *ProvenanceDTO {
		if w.RepostedByHandle != "" && w.RepostedByHandle != viewer.Handle {
			return &ProvenanceDTO{Kind: "reposted", Text: "Reposted by @" + w.RepostedByHandle, Handle: strPtr(w.RepostedByHandle)}
		}
		if w.ViewerFollowsAuthor {
			return &ProvenanceDTO{Kind: "you_follow", Text: "You follow @" + w.AuthorHandle, Handle: strPtr(w.AuthorHandle)}
		}
		return nil
	}
}

func apiTrendingProvenance() func(*model.Work) *ProvenanceDTO {
	return func(w *model.Work) *ProvenanceDTO {
		if w.ScoreBand == "trending" || w.ScoreBand == "rising" {
			return &ProvenanceDTO{Kind: "trending", Text: "Picking up on F33D3R"}
		}
		return nil
	}
}

// apiLoadWork loads one work for a viewer and applies the same walls the
// detail page applies: a private author the viewer does not follow, and adult
// content the viewer must never see. A walled work is a 404 to this viewer —
// the header the web still draws has no JSON counterpart that omits the body.
func (h *Handler) apiLoadWork(w http.ResponseWriter, id string, u *model.User) (*model.Work, bool) {
	if _, err := uuid.Parse(id); err != nil {
		apiError(w, http.StatusNotFound, "not_found", "That work doesn't exist.")
		return nil, false
	}
	work, err := dbpkg.GetWorkByID(h.db, id, u.ID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			apiError(w, http.StatusNotFound, "not_found", "That work doesn't exist.")
			return nil, false
		}
		apiServerError(w, err)
		return nil, false
	}
	author, _ := dbpkg.GetUserByHandle(h.db, work.AuthorHandle)
	privateWall, gate := h.workWall(work, author, u)
	if privateWall {
		apiError(w, http.StatusForbidden, "private_account", "This account is private.")
		return nil, false
	}
	if gate != "" {
		apiError(w, http.StatusForbidden, "restricted", "This work is not available to you.")
		return nil, false
	}
	if author != nil && (dbpkg.IsBlocked(h.db, author.ID, u.ID) || dbpkg.IsBlocked(h.db, u.ID, author.ID)) {
		apiError(w, http.StatusNotFound, "not_found", "That work doesn't exist.")
		return nil, false
	}
	return work, true
}

// apiV1Work — GET /api/v1/works/{id}: the work, its ancestors, the first
// page of replies.
func (h *Handler) apiV1Work(w http.ResponseWriter, r *http.Request, u *model.User) {
	id := r.PathValue("id")
	work, ok := h.apiLoadWork(w, id, u)
	if !ok {
		return
	}
	ancestors, err := dbpkg.GetWorkParentChain(h.db, id, u.ID, 10)
	if err != nil {
		apiServerError(w, err)
		return
	}
	replies, err := dbpkg.GetWorkRepliesBefore(h.db, id, apiV1DefaultPage, time.Now())
	if err != nil {
		apiServerError(w, err)
		return
	}
	single := h.apiPrepareWorks([]*model.Work{work}, u)
	if len(single) == 0 {
		apiError(w, http.StatusForbidden, "restricted", "This work is not available to you.")
		return
	}
	ancestors = h.apiPrepareWorks(ancestors, u)
	replies = h.apiPrepareWorks(replies, u)
	apiJSON(w, http.StatusOK, WorkThreadDTO{
		Work:          workDTO(single[0], nil),
		Ancestors:     workDTOs(ancestors, nil),
		Replies:       workDTOs(replies, nil),
		RepliesCursor: apiNextCursor(replies, apiV1DefaultPage, time.Time{}),
	})
}

// apiV1Replies — GET /api/v1/works/{id}/replies?cursor=&limit=
func (h *Handler) apiV1Replies(w http.ResponseWriter, r *http.Request, u *model.User) {
	id := r.PathValue("id")
	if _, ok := h.apiLoadWork(w, id, u); !ok {
		return
	}
	before, ok := apiCursor(r.URL.Query().Get("cursor"))
	if !ok {
		apiError(w, http.StatusBadRequest, "bad_cursor", "That page cursor is not valid.")
		return
	}
	limit := apiLimit(r)
	replies, err := dbpkg.GetWorkRepliesBefore(h.db, id, limit, before)
	if err != nil {
		apiServerError(w, err)
		return
	}
	replies = h.apiPrepareWorks(replies, u)
	apiJSON(w, http.StatusOK, WorkPageDTO{Works: workDTOs(replies, nil), NextCursor: apiNextCursor(replies, limit, time.Time{})})
}

// apiV1WorkQuotes — GET /api/v1/works/{id}/quotes?cursor=&limit=
//
// The works that quote this one, newest first, in the same page shape the
// replies route answers with. "View quotes" on a work's detail screen is the
// same list the web's quotes page draws, and it is a list of ordinary works —
// so it pages by the same cursor, prepares through the same viewer filter, and
// carries the same DTO. Nothing about a quote makes it a different kind of row.
func (h *Handler) apiV1WorkQuotes(w http.ResponseWriter, r *http.Request, u *model.User) {
	id := r.PathValue("id")
	if _, ok := h.apiLoadWork(w, id, u); !ok {
		return
	}
	before, ok := apiCursor(r.URL.Query().Get("cursor"))
	if !ok {
		apiError(w, http.StatusBadRequest, "bad_cursor", "That page cursor is not valid.")
		return
	}
	limit := apiLimit(r)
	quotes, err := dbpkg.GetWorkQuotesBefore(h.db, id, limit, before)
	if err != nil {
		apiServerError(w, err)
		return
	}
	quotes = h.apiPrepareWorks(quotes, u)
	apiJSON(w, http.StatusOK, WorkPageDTO{Works: workDTOs(quotes, nil), NextCursor: apiNextCursor(quotes, limit, time.Time{})})
}

// apiLoadProfile resolves a handle to the account, or answers 404.
func (h *Handler) apiLoadProfile(w http.ResponseWriter, handle string) (*model.User, bool) {
	handle = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(handle)), "@")
	if !handleRe.MatchString(handle) {
		apiError(w, http.StatusNotFound, "not_found", "No one by that handle.")
		return nil, false
	}
	target, err := dbpkg.GetUserByHandle(h.db, handle)
	if err != nil {
		apiServerError(w, err)
		return nil, false
	}
	if target == nil || dbpkg.IsDeactivated(h.db, target.ID) {
		apiError(w, http.StatusNotFound, "not_found", "No one by that handle.")
		return nil, false
	}
	// Standing folded on from PIAL, as userFromRequest does for the viewer:
	// the verified mark and the age facts are identity-plane facts. IsVerified is
	// the documented-identity bar (kyc_tier=='full'), not the age-report bar.
	if ps := dbpkg.LoadPIALState(h.db, target.PIALID); ps != nil {
		target.IsAgeVerified = ps.AgeVerified
		target.IsVerified = model.KYCTierIsIdentity(ps.KYCTier)
		target.IsMinor = ps.IsMinor
		target.IsAdult = ps.IsAdult
		target.Role = ps.Role
	}
	return target, true
}

// apiProfileWall reports whether the viewer is kept out of the target's
// works: a private account they do not follow, or an adult creator they are
// not cleared for. The profile header itself always answers.
func (h *Handler) apiProfileWall(target, viewer *model.User) (code, message string) {
	isOwner := target.ID == viewer.ID
	if isOwner {
		return "", ""
	}
	access, err := h.authorWall(target.ID, viewer)
	if err != nil {
		return "private_account", "This account is private."
	}
	if access.IsBlocked || privateWalled(target.IsPrivate, access) {
		return "private_account", "This account is private."
	}
	if target.IsAdultCreator {
		switch {
		case excludeNSFW(viewer):
			return "restricted", "This creator's works are not available to you."
		case !(viewer.IsAgeVerified || viewer.IsVerified):
			return "verify_required", "Verify your age to view this creator's works."
		case viewer.ContentSetting != "adult_enabled":
			return "adult_content_disabled", "Enable adult content in Settings to view this creator's works."
		}
	}
	return "", ""
}

// apiV1Profile — GET /api/v1/users/{handle}
func (h *Handler) apiV1Profile(w http.ResponseWriter, r *http.Request, viewer *model.User) {
	target, ok := h.apiLoadProfile(w, r.PathValue("handle"))
	if !ok {
		return
	}
	viewerFollows, _ := dbpkg.IsFollowing(h.db, viewer.ID, target.ID)
	followsViewer, _ := dbpkg.IsFollowing(h.db, target.ID, viewer.ID)
	dto := ProfileDTO{
		User:            userDTO(target),
		ViewerFollows:   viewerFollows,
		FollowsViewer:   followsViewer,
		ViewerIsBlocked: dbpkg.IsBlocked(h.db, target.ID, viewer.ID),
	}
	if target.ID != viewer.ID {
		go dbpkg.TryAwardTrollOnProfileView(h.db, target.ID)
	}
	if code, _ := h.apiProfileWall(target, viewer); code == "" {
		if pinnedID := dbpkg.GetPinnedPostID(h.db, target.ID); pinnedID != "" {
			if pinned, err := dbpkg.GetWorkByID(h.db, pinnedID, viewer.ID); err == nil && pinned != nil {
				if prepared := h.apiPrepareWorks([]*model.Work{pinned}, viewer); len(prepared) == 1 {
					// The mark is the profile owner's, not the viewer's.
					prepared[0].IsPinned = true
					p := workDTO(prepared[0], nil)
					dto.PinnedWork = &p
				}
			}
		}
	}
	apiJSON(w, http.StatusOK, dto)
}

// apiV1ProfileWorks — GET /api/v1/users/{handle}/works?tab=works|replies|media|likes
//
// Likes are the owner's alone: anyone else asking gets `403 likes_private`,
// which the client renders as a rule rather than a failure.
func (h *Handler) apiV1ProfileWorks(w http.ResponseWriter, r *http.Request, viewer *model.User) {
	target, ok := h.apiLoadProfile(w, r.PathValue("handle"))
	if !ok {
		return
	}
	tab := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("tab")))
	if tab == "" {
		tab = "works"
	}
	spec, known := map[string]*timelineSpec{
		"works":   specProfilePosts,
		"replies": specProfileReplies,
		"media":   specProfileMedia,
		"likes":   specProfileLikes,
		"saves":   specProfileSaves,
	}[tab]
	if !known {
		apiError(w, http.StatusBadRequest, "unknown_tab", fmt.Sprintf("No tab named %q.", tab))
		return
	}
	cursor := r.URL.Query().Get("cursor")
	if _, ok := apiCursor(cursor); !ok {
		apiError(w, http.StatusBadRequest, "bad_cursor", "That page cursor is not valid.")
		return
	}
	result, refusal, err := h.runTimeline(r.Context(), spec, &timelineQuery{
		Viewer: viewer, Limit: apiLimit(r), Before: cursor, Profile: target,
	})
	if err != nil {
		apiServerError(w, err)
		return
	}
	if refusal != nil {
		apiError(w, refusal.Status, refusal.Code, refusal.Message)
		return
	}
	if tab == "works" {
		if pinnedID := dbpkg.GetPinnedPostID(h.db, target.ID); pinnedID != "" {
			for _, wk := range result.Works {
				wk.IsPinned = wk.ID == pinnedID
			}
		}
	}
	apiJSON(w, http.StatusOK, WorkPageDTO{Works: workDTOs(result.Works, nil), NextCursor: result.Next})
}

// apiV1Search — GET /api/v1/search?q=&type=works|people|all
func (h *Handler) apiV1Search(w http.ResponseWriter, r *http.Request, viewer *model.User) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	out := SearchDTO{Works: []WorkDTO{}, People: []UserDTO{}, Tags: []TagDTO{}}
	if q == "" {
		apiJSON(w, http.StatusOK, out)
		return
	}
	kind := r.URL.Query().Get("type")
	term := strings.TrimPrefix(strings.TrimPrefix(q, "@"), "#")
	if kind == "" || kind == "all" || kind == "tags" {
		tags, err := dbpkg.SearchTags(h.db, term, 10)
		if err != nil {
			apiServerError(w, err)
			return
		}
		for _, t := range tags {
			out.Tags = append(out.Tags, TagDTO{Tag: t.Tag, Count: t.Count})
		}
	}
	if kind == "" || kind == "all" || kind == "works" {
		result, _, err := h.runTimeline(r.Context(), specSearch, &timelineQuery{
			Viewer: viewer, Limit: apiV1DefaultPage, Terms: []string{term},
		})
		if err != nil {
			apiServerError(w, err)
			return
		}
		out.Works = workDTOs(result.Works, nil)
	}
	if kind == "" || kind == "all" || kind == "people" {
		people, err := dbpkg.SearchHandlesFiltered(h.db, term, apiV1DefaultPage, viewer.IsMinor, viewer.IsAdult)
		if err != nil {
			apiServerError(w, err)
			return
		}
		for _, p := range people {
			u, err := dbpkg.GetUserByHandle(h.db, p.Handle)
			if err != nil || u == nil {
				continue
			}
			if ps := dbpkg.LoadPIALState(h.db, u.PIALID); ps != nil {
				u.IsVerified = model.KYCTierIsIdentity(ps.KYCTier)
				u.Role = ps.Role
			}
			out.People = append(out.People, userDTO(u))
		}
	}
	apiJSON(w, http.StatusOK, out)
}

// apiV1Notifications — GET /api/v1/notifications?limit=&cursor=
//
// Grouped server-side (like/repost/follow within an hour on one target), with
// the unread total alongside so the badge and the list come from one answer.
func (h *Handler) apiV1Notifications(w http.ResponseWriter, r *http.Request, u *model.User) {
	before, ok := apiCursor(r.URL.Query().Get("cursor"))
	if !ok {
		apiError(w, http.StatusBadRequest, "bad_cursor", "That page cursor is not valid.")
		return
	}
	groups, next, err := dbpkg.ListNotificationGroups(h.db, u.ID, apiLimit(r), before)
	if err != nil {
		apiServerError(w, err)
		return
	}
	var actorIDs []string
	for _, g := range groups {
		actorIDs = append(actorIDs, g.ActorIDs...)
	}
	actors, err := dbpkg.GetUsersByIDs(h.db, actorIDs)
	if err != nil {
		apiServerError(w, err)
		return
	}
	out := make([]NotificationDTO, 0, len(groups))
	for _, g := range groups {
		var as []*model.User
		for _, id := range g.ActorIDs {
			if a, ok := actors[id]; ok {
				as = append(as, a)
			}
		}
		if g.Preview == "" && g.TargetID != "" && (g.TargetType == "work" || g.TargetType == "post") {
			g.Preview = strings.TrimSpace(dbpkg.GetWorkBodyPreview(h.db, g.TargetID))
		}
		out = append(out, notificationDTO(g, as))
	}
	apiJSON(w, http.StatusOK, NotificationPageDTO{
		Notifications: out,
		NextCursor:    next,
		UnreadCount:   dbpkg.CountUnreadNotifications(h.db, u.ID),
	})
}

// uaetPerAinSophUnit converts Ain Soph's storage unit (a hundredth of an AET,
// models.rs AET_DECIMALS) to the µAET the clients carry (a millionth).
const uaetPerAinSophUnit = 1_000_000 / 100

// apiV1Wallet — GET /api/v1/wallet: settled and pending balances, never
// summed, and the recent ledger — read from Ain Soph, the ledger brain, keyed
// on the PIAL the wallet is opened for.
func (h *Handler) apiV1Wallet(w http.ResponseWriter, r *http.Request, u *model.User) {
	walletID := h.walletIdentity(u)
	if walletID == "" {
		apiError(w, http.StatusServiceUnavailable, "wallet_unavailable", "Wallet unavailable — no identity for this account.")
		return
	}
	if h.cfg == nil || h.cfg.AinSophURL == "" {
		apiError(w, http.StatusServiceUnavailable, "wallet_unavailable", "The wallet service is not configured.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
	defer cancel()

	var bal struct {
		BalanceUnits int64 `json:"balance_units"`
	}
	if err := h.ainSophGet(ctx, "/v1/balance/"+walletID, &bal); err != nil {
		log.Printf("[api/v1] wallet balance for %s: %v", walletID, err)
		apiError(w, http.StatusBadGateway, "wallet_unavailable", "The wallet service is unavailable.")
		return
	}
	var hist struct {
		Entries []struct {
			ID           string    `json:"id"`
			TxType       string    `json:"tx_type"`
			Direction    string    `json:"direction"`
			Counterparty *string   `json:"counterparty"`
			AmountUnits  int64     `json:"amount_units"`
			NetUnits     int64     `json:"net_units"`
			Status       string    `json:"status"`
			CreatedAt    time.Time `json:"created_at"`
		} `json:"entries"`
	}
	if err := h.ainSophGet(ctx, "/v1/transactions/"+walletID+"?limit=50", &hist); err != nil {
		log.Printf("[api/v1] wallet history for %s: %v", walletID, err)
		apiError(w, http.StatusBadGateway, "wallet_unavailable", "The wallet service is unavailable.")
		return
	}
	out := WalletDTO{
		BalanceUAET: bal.BalanceUnits * uaetPerAinSophUnit,
		Entries:     make([]WalletEntryDTO, 0, len(hist.Entries)),
	}
	handles := map[string]string{}
	for _, e := range hist.Entries {
		// What this account received or paid: the net credited on the way in,
		// the gross debited on the way out.
		amount := e.NetUnits
		if e.Direction == "out" {
			amount = -e.AmountUnits
		}
		amount *= uaetPerAinSophUnit
		if e.Status == "pending" {
			out.PendingUAET += amount
		}
		var counterparty *string
		if e.Counterparty != nil && *e.Counterparty != "" {
			handle, ok := handles[*e.Counterparty]
			if !ok {
				handle = dbpkg.GetHandleForPIAL(h.db, *e.Counterparty)
				handles[*e.Counterparty] = handle
			}
			counterparty = strPtr(handle)
		}
		out.Entries = append(out.Entries, WalletEntryDTO{
			ID:                 e.ID,
			Kind:               e.TxType,
			AmountUAET:         amount,
			CounterpartyHandle: counterparty,
			CreatedAt:          e.CreatedAt,
		})
	}
	apiJSON(w, http.StatusOK, out)
}

// ainSophGet performs one authenticated read against Ain Soph and decodes
// the JSON answer into v.
func (h *Handler) ainSophGet(ctx context.Context, path string, v interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.cfg.AinSophURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := h.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("ain-soph %s: %d %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// apiV1UploadMedia — POST /api/v1/media (multipart, field `file`) →
// 201 {"url": "...", "bytes": n}
//
// Images only: video has its own route, POST /api/v1/media/video, because it
// is answered with a job rather than a URL. The bytes pass through the same
// admission the web's compose box uses — magic-byte check, the banned content
// gate, Caeor, canonical ownership, the forensic hash — because an image is
// an image whichever screen sent it.
func (h *Handler) apiV1UploadMedia(w http.ResponseWriter, r *http.Request, _ *model.User) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		apiError(w, http.StatusBadRequest, "bad_request", "The upload could not be read.")
		return
	}
	file, fh, err := formFileAny(r, "file", "media")
	if err != nil {
		apiError(w, http.StatusBadRequest, "bad_request", "No file was attached.")
		return
	}
	defer file.Close()
	fileBytes, err := io.ReadAll(io.LimitReader(file, 20<<20))
	if err != nil {
		apiServerError(w, err)
		return
	}
	ext := imageExtensionFor(fileBytes)
	if ext == "" {
		apiError(w, http.StatusUnsupportedMediaType, "unsupported_media", "Only JPEG, PNG, GIF and WebP images are accepted.")
		return
	}
	mediaURL, gateErr, err := h.admitImageUpload(r, fh.Filename, ext, fileBytes)
	if gateErr != nil {
		apiGateError(w, gateErr)
		return
	}
	if err != nil {
		apiServerError(w, err)
		return
	}
	apiJSON(w, http.StatusCreated, MediaUploadDTO{URL: mediaURL, Bytes: len(fileBytes)})
}
