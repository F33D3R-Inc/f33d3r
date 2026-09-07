package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
)

// ── /api/v1: the social graph, tags, sessions and the account's own event
// stream, plus the account events on POST /events ─────────────────────────────
//
// Reads here mirror the web's follower lists, the explore rail's trending
// tags and the settings page's device list. The event stream is the native
// form of /events/stream: the same registry, the same publishes, but only the
// JSON twin of each event is forwarded, and only the kinds the app decodes.

// apiFollowWall is the one rule for who may read an account's follow lists:
// the owner always, anyone for a public account, followers for a private one.
func (h *Handler) apiFollowWall(target, viewer *model.User) bool {
	if target.ID == viewer.ID || !target.IsPrivate {
		return true
	}
	following, _ := dbpkg.IsFollowing(h.db, viewer.ID, target.ID)
	return following
}

// apiUserPage projects a page of follow edges: full user rows so the card
// carries the same fields as a profile header, deactivated accounts dropped,
// and the viewer's own follow state alongside.
func (h *Handler) apiUserPage(w http.ResponseWriter, viewer *model.User, edges []dbpkg.FollowEdge, limit int) {
	out := UserPageDTO{Users: make([]UserDTO, 0, len(edges)), ViewerFollows: []string{}}
	ids := make([]string, 0, len(edges))
	byID := map[string]string{}
	for _, e := range edges {
		u, err := dbpkg.GetUserByHandle(h.db, e.Handle)
		if err != nil || u == nil || dbpkg.IsDeactivated(h.db, u.ID) {
			continue
		}
		if ps := dbpkg.LoadPIALState(h.db, u.PIALID); ps != nil {
			u.IsAgeVerified = ps.AgeVerified
			u.IsVerified = model.KYCTierIsIdentity(ps.KYCTier)
			u.IsMinor = ps.IsMinor
			u.IsAdult = ps.IsAdult
			u.Role = ps.Role
		}
		ids = append(ids, u.ID)
		byID[u.ID] = u.Handle
		out.Users = append(out.Users, userDTO(u))
	}
	followed, err := dbpkg.FollowedSet(h.db, viewer.ID, ids)
	if err != nil {
		apiServerError(w, err)
		return
	}
	for _, id := range ids {
		if followed[id] {
			out.ViewerFollows = append(out.ViewerFollows, byID[id])
		}
	}
	if len(edges) >= limit && len(edges) > 0 {
		out.NextCursor = edges[len(edges)-1].FollowedAt.Format(time.RFC3339Nano)
	}
	apiJSON(w, http.StatusOK, out)
}

func (h *Handler) apiFollowList(w http.ResponseWriter, r *http.Request, viewer *model.User,
	load func(*sql.DB, string, int, time.Time) ([]dbpkg.FollowEdge, error)) {
	target, ok := h.apiLoadProfile(w, r.PathValue("handle"))
	if !ok {
		return
	}
	if !h.apiFollowWall(target, viewer) {
		apiError(w, http.StatusForbidden, "private_account", "This account is private.")
		return
	}
	before, ok := apiCursor(r.URL.Query().Get("cursor"))
	if !ok {
		apiError(w, http.StatusBadRequest, "bad_cursor", "That page cursor is not valid.")
		return
	}
	limit := apiLimit(r)
	edges, err := load(h.db, target.ID, limit, before)
	if err != nil {
		apiServerError(w, err)
		return
	}
	h.apiUserPage(w, viewer, edges, limit)
}

// apiV1Followers — GET /api/v1/users/{handle}/followers?cursor=&limit=
func (h *Handler) apiV1Followers(w http.ResponseWriter, r *http.Request, viewer *model.User) {
	h.apiFollowList(w, r, viewer, dbpkg.GetFollowersBefore)
}

// apiV1Following — GET /api/v1/users/{handle}/following?cursor=&limit=
func (h *Handler) apiV1Following(w http.ResponseWriter, r *http.Request, viewer *model.User) {
	h.apiFollowList(w, r, viewer, dbpkg.GetFollowingBefore)
}

// apiV1TrendingTags — GET /api/v1/tags/trending → TrendingTagsDTO.
func (h *Handler) apiV1TrendingTags(w http.ResponseWriter, r *http.Request, _ *model.User) {
	tags, err := dbpkg.GetTrendingTags(h.db, 12)
	if err != nil {
		apiServerError(w, err)
		return
	}
	out := make([]TagDTO, 0, len(tags))
	for _, t := range tags {
		out = append(out, TagDTO{Tag: t.Tag, Count: t.Count})
	}
	apiJSON(w, http.StatusOK, TrendingTagsDTO{Tags: out})
}

// apiV1Sessions — GET /api/v1/sessions → SessionListDTO, the device that
// asked first.
func (h *Handler) apiV1Sessions(w http.ResponseWriter, r *http.Request, u *model.User) {
	list, err := dbpkg.ListUserSessions(h.db, u.ID)
	if err != nil {
		apiServerError(w, err)
		return
	}
	current := dbpkg.SessionIDForToken(h.db, GetSessionToken(r))
	out := make([]SessionInfoDTO, 0, len(list))
	for _, s := range list {
		d := SessionInfoDTO{
			ID:         s.ID,
			DeviceName: s.DeviceName,
			IPAddress:  strPtr(s.IPAddress),
			CreatedAt:  s.CreatedAt,
			LastSeenAt: s.LastSeenAt,
			IsCurrent:  current != "" && s.ID == current,
		}
		if d.IsCurrent {
			out = append([]SessionInfoDTO{d}, out...)
			continue
		}
		out = append(out, d)
	}
	apiJSON(w, http.StatusOK, SessionListDTO{Sessions: out})
}

// apiUserStreamKinds are the events the app's user stream decodes.
//
// gnosis_message rides the ACCOUNT pipe, not the person's: a message reaches
// the persona it was addressed to and none of that person's others. Its twin
// carries the message, never an unread count — the badge is re-read, for the
// same reason notify's is.
var apiUserStreamKinds = map[string]bool{
	"notify": true, "new_post": true, "post_deleted": true, "balance": true, "live_start": true,
	"gnosis_message": true, "work_engagement": true, "frequency_start": true,
}

// apiStreamable reports whether an event belongs on a native client's stream:
// a kind the app decodes, carrying the structured twin it decodes it from. An
// event with no twin is a web-only projection — a rendered fragment — and
// forwarding it would hand the app markup it has no use for.
func apiStreamable(ev SSEEvent) bool {
	return apiUserStreamKinds[ev.Type] && ev.JSON != ""
}

// lastEventID reads the resume point the client is asking from. The header is
// the protocol's own — a client re-sends the last `id:` it saw verbatim after a
// drop — and anything that is not a positive number is no resume point at all.
func lastEventID(r *http.Request) int64 {
	raw := strings.TrimSpace(r.Header.Get("Last-Event-ID"))
	if raw == "" {
		return 0
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// apiV1UserEvents — GET /api/v1/events: the account's own event stream.
func (h *Handler) apiV1UserEvents(w http.ResponseWriter, r *http.Request, u *model.User) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		apiError(w, http.StatusInternalServerError, "no_stream", "SSE not supported.")
		return
	}
	registryCh, registryDone, cleanup := RegisterSSESession(u.ID, u.PIALID)
	defer cleanup()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	// What the client missed while it was away, before anything that happens
	// now. Frames are replayed under their original numbers, so the client's
	// resume point keeps advancing through the same sequence; a gap the ring can
	// no longer prove it holds in full is replayed as nothing at all, and the
	// connect-time snapshot below is what puts the client back on its feet.
	//
	// replayedTo is the last number this connection has already written. The
	// session is registered before the replay is read, so an event published in
	// between is both queued to this connection AND held in the ring: without
	// this the client would be handed the same new_post twice and prepend two
	// cards for one work.
	var replayedTo int64
	if last := lastEventID(r); last > 0 {
		if missed, ok := ReplayAfter(u.PIALID, last); ok {
			for _, f := range missed {
				replayedTo = f.ID
				if !apiStreamable(f.Event) {
					continue
				}
				writeSSEFrameID(w, f.ID, f.Event.Type, f.Event.JSON)
			}
			flusher.Flush()
		} else {
			log.Printf("[stream] %s resumed at %d, outside the replay window — sending state instead",
				u.Handle, last)
		}
	}

	// The snapshot frames carry no id: they describe the state at connect time
	// rather than an event anyone else received, and numbering them would let a
	// client resume from a position no other connection shares.
	sendNotify := func(id int64) {
		writeSSEFrameID(w, id, "notify", fmt.Sprintf(`{"unread":%d}`, dbpkg.CountUnreadNotifications(h.db, u.ID)))
		flusher.Flush()
	}
	sendNotify(0)
	if twin := h.apiBalanceTwin(r.Context(), u); twin != "" {
		writeSSEFrame(w, "balance", twin)
		flusher.Flush()
	}

	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-registryDone:
			return
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case ev, open := <-registryCh:
			if !open {
				return
			}
			if !apiStreamable(ev) || (ev.ID > 0 && ev.ID <= replayedTo) {
				continue
			}
			if ev.Type == "notify" {
				// The count is re-read, not trusted: a twin rendered for an
				// earlier state must never mis-badge the app. The event's own
				// number still goes out, so the client's resume point advances
				// past a notify it has already been told about.
				sendNotify(ev.ID)
				continue
			}
			writeSSEFrameID(w, ev.ID, ev.Type, ev.JSON)
			flusher.Flush()
		}
	}
}

// apiBalanceTwin is the settled balance as the app's stream carries it, or
// "" when Ain Soph is not configured or does not answer.
func (h *Handler) apiBalanceTwin(ctx context.Context, u *model.User) string {
	if h.cfg == nil || h.cfg.AinSophURL == "" || u.PIALID == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var bal struct {
		BalanceUnits int64 `json:"balance_units"`
	}
	if err := h.ainSophGet(ctx, "/v1/balance/"+u.PIALID, &bal); err != nil {
		return ""
	}
	return fmt.Sprintf(`{"balance_uaet":%d,"pending_uaet":0}`, bal.BalanceUnits*uaetPerAinSophUnit)
}

// ── Account events on POST /events ───────────────────────────────────────────

// apiV1AccountEvent answers the account events the native clients post.
// It reports false for an event type it does not own.
func (h *Handler) apiV1AccountEvent(w http.ResponseWriter, r *http.Request, eventType string, rawBodyMap map[string]json.RawMessage) bool {
	switch eventType {
	case "profile_update", "password_change", "session_revoke", "session.revoke",
		"sessions_revoke_others", "work_edit", "work_purchase",
		"notification_prefs", "two_fa_enable", "two_fa_disable", "two_fa_backup_codes",
		"account_delete":
	default:
		return false
	}
	user := h.userFromRequest(w, r)
	if user == nil || user.ID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return true
	}
	if h.db == nil {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return true
	}
	switch eventType {
	case "profile_update":
		h.apiProfileUpdateEvent(w, r, user, rawBodyMap)
	case "password_change":
		next := eventField(r, rawBodyMap, "new_password")
		status, msg := h.changePassword(user, eventField(r, rawBodyMap, "current_password"), next, next)
		if status != http.StatusOK {
			http.Error(w, msg, status)
			return true
		}
		w.WriteHeader(http.StatusNoContent)
	case "session_revoke", "session.revoke":
		// The web's session card posts the dotted name and nothing handled it,
		// so "sign out this device" answered 404. Both names, one rule.
		if facetRequest(r) {
			h.sessionRevokeEvent(w, r, rawBodyMap)
			return true
		}
		id := eventField(r, rawBodyMap, "session_id")
		if id == "" {
			http.Error(w, "session_id required", http.StatusBadRequest)
			return true
		}
		if err := dbpkg.RevokeSession(h.db, id, user.ID); err != nil {
			http.Error(w, "session not found", http.StatusNotFound)
			return true
		}
		w.WriteHeader(http.StatusNoContent)
	case "sessions_revoke_others":
		if err := dbpkg.RevokeOtherSessions(h.db, user.ID, GetSessionToken(r)); err != nil {
			log.Printf("[sessions] revoke others for %s: %v", user.Handle, err)
			http.Error(w, "server error", http.StatusInternalServerError)
			return true
		}
		w.WriteHeader(http.StatusNoContent)
	case "work_edit":
		status, msg := h.applyWorkEdit(user, eventField(r, rawBodyMap, "work_id"), eventField(r, rawBodyMap, "body"))
		if status != http.StatusOK {
			http.Error(w, msg, status)
			return true
		}
		w.WriteHeader(http.StatusNoContent)
	case "work_purchase":
		// No work on this stack carries a price: WorkDTO.price_uaet is never
		// set, and paid content runs through the Themis marketplace in BTC
		// and XRP, never an AET balance. So a purchase against a work that
		// exists is refused as not for sale, which is the truth of it.
		workID := eventField(r, rawBodyMap, "work_id")
		if workID == "" {
			http.Error(w, "work_id required", http.StatusBadRequest)
			return true
		}
		if _, _, err := dbpkg.GetWorkAuthor(h.db, workID); err != nil {
			http.Error(w, "work not found", http.StatusNotFound)
			return true
		}
		http.Error(w, "work is not for sale", http.StatusBadRequest)
	case "notification_prefs":
		h.apiNotificationPrefsEvent(w, r, user, rawBodyMap)
	case "two_fa_enable":
		h.api2FAEvent(w, r, user, rawBodyMap, true)
	case "two_fa_disable":
		h.api2FAEvent(w, r, user, rawBodyMap, false)
	case "two_fa_backup_codes":
		h.apiBackupCodesEvent(w, r, user)
	case "account_delete":
		h.apiAccountDeleteEvent(w, r, user, rawBodyMap)
	}
	return true
}

// apiProfileUpdateEvent applies a partial profile edit: only the fields the
// body names change, everything else on the row stays as it was.
func (h *Handler) apiProfileUpdateEvent(w http.ResponseWriter, r *http.Request, user *model.User, rawBodyMap map[string]json.RawMessage) {
	_ = r.ParseForm()
	present := func(key string) (string, bool) {
		if rawBodyMap != nil {
			if _, ok := rawBodyMap[key]; ok {
				return eventField(r, rawBodyMap, key), true
			}
			return "", false
		}
		if _, ok := r.Form[key]; ok {
			return strings.TrimSpace(r.FormValue(key)), true
		}
		return "", false
	}
	save := &model.ProfileSave{
		UserID:               user.ID,
		DisplayName:          user.DisplayName,
		Bio:                  user.Bio,
		Pronouns:             user.Pronouns,
		Location:             user.Location,
		CountryCode:          user.CountryCode,
		Website:              user.Website,
		AvatarURL:            user.AvatarURL,
		HeaderURL:            user.HeaderURL,
		ThemeID:              user.ThemeID,
		AccentHex:            user.AccentHex,
		JungArchetype:        user.JungArchetype,
		PinnedTrackID:        user.PinnedTrackID,
		SocialLinksJSON:      user.SocialLinksRaw,
		ExternalTipLinksJSON: user.ExternalTipLinksRaw,
		IsAdultCreator:       user.IsAdultCreator,
	}
	if !isValidTheme(save.ThemeID) {
		save.ThemeID = "void"
	}
	if v, ok := present("display_name"); ok {
		save.DisplayName = truncate(v, 100)
	}
	if v, ok := present("bio"); ok {
		save.Bio = truncate(v, 160)
	}
	if v, ok := present("pronouns"); ok {
		save.Pronouns = truncate(v, 50)
	}
	if v, ok := present("location"); ok {
		save.Location = truncate(v, 100)
	}
	if v, ok := present("website"); ok {
		save.Website = truncate(v, 200)
	}
	if v, ok := present("accent_hex"); ok {
		if v != "" && !accentHexRe.MatchString(v) {
			http.Error(w, "accent_hex must be #RRGGBB", http.StatusBadRequest)
			return
		}
		save.AccentHex = v
	}
	if v, ok := present("theme_id"); ok {
		if !isValidTheme(v) {
			http.Error(w, "unknown theme_id", http.StatusBadRequest)
			return
		}
		save.ThemeID = v
	}
	for _, key := range []string{"avatar_url", "header_url"} {
		v, ok := present(key)
		if !ok {
			continue
		}
		if v != "" && (!strings.HasPrefix(v, "/") || strings.HasPrefix(v, "//") || strings.Contains(v, "..")) {
			http.Error(w, key+" must be a path served by this platform", http.StatusBadRequest)
			return
		}
		if key == "avatar_url" {
			save.AvatarURL = v
		} else {
			save.HeaderURL = v
		}
	}
	if v, ok := present("country_code"); ok {
		save.CountryCode = strings.ToUpper(truncate(v, 2))
	}
	if v, present := eventBool(r, rawBodyMap, "is_adult_creator"); present {
		save.IsAdultCreator = v
	}
	// The link groups arrive as one object each from a JSON client, and as the
	// web form's flat social_*/tip_* fields otherwise. Either way they are read
	// through the profile's own normalisers — a tip address is validated here or
	// it is not stored.
	if get, ok := linkGroupReader(r, rawBodyMap, "social_links", "social_"); ok {
		save.SocialLinksJSON = linksJSON(normalizeSocialLinks(get))
	}
	if get, ok := linkGroupReader(r, rawBodyMap, "external_tip_links", "tip_"); ok {
		save.ExternalTipLinksJSON = linksJSON(normalizeTipLinks(get))
	}
	// Birthday visibility lives on its own writer, not on ProfileSave.
	mdVis, mdSet := present("birthday_md_visibility")
	yrVis, yrSet := present("birthday_year_visibility")
	if mdSet || yrSet {
		if !mdSet {
			mdVis = user.BirthdayMdVisibility
		}
		if !yrSet {
			yrVis = user.BirthdayYearVisibility
		}
		if err := dbpkg.SetBirthdayVisibility(h.db, user.ID, user.ShowBirthday, mdVis, yrVis); err != nil {
			log.Printf("[profile] birthday visibility for %s: %v", user.Handle, err)
			http.Error(w, "could not save profile", http.StatusInternalServerError)
			return
		}
	}
	if err := dbpkg.SaveProfile(h.db, save); err != nil {
		log.Printf("[profile] api save for %s: %v", user.Handle, err)
		http.Error(w, "could not save profile", http.StatusInternalServerError)
		return
	}
	if tok := GetSessionToken(r); tok != "" {
		h.sessionCache.Delete(tok)
	}
	fresh := h.userFromRequest(w, r)
	if viewerAccountID(fresh) == "" {
		fresh = user
	}
	apiJSON(w, http.StatusOK, h.meDTOFor(fresh))
}

// linkGroupReader resolves one group of profile links out of whichever body
// arrived. A JSON client sends {"social_links":{"youtube":"…"}}; the web form
// sends social_youtube=…. ok is false when the body names neither, so a partial
// edit leaves the group alone.
func linkGroupReader(r *http.Request, rawBodyMap map[string]json.RawMessage, object, formPrefix string) (func(string) string, bool) {
	if raw, present := rawBodyMap[object]; present {
		group := map[string]string{}
		if err := json.Unmarshal(raw, &group); err != nil {
			return nil, false
		}
		return func(k string) string { return group[k] }, true
	}
	if rawBodyMap != nil {
		return nil, false
	}
	named := false
	for key := range r.Form {
		if strings.HasPrefix(key, formPrefix) {
			named = true
			break
		}
	}
	if !named {
		return nil, false
	}
	return func(k string) string { return r.FormValue(formPrefix + k) }, true
}

// ── Shared rules: password change, work edit ─────────────────────────────────

// changePassword is the one set of rules for changing a password, whichever
// surface asked. 200 means it changed; any other status carries the sentence
// to show.
func (h *Handler) changePassword(user *model.User, current, next, confirm string) (int, string) {
	if h.db == nil {
		return http.StatusServiceUnavailable, "Database unavailable."
	}
	if user == nil || user.ID == "" {
		return http.StatusUnauthorized, "Sign in to change your password."
	}
	if next == "" || len(next) < 8 {
		return http.StatusBadRequest, "New password must be at least 8 characters."
	}
	if next != confirm {
		return http.StatusBadRequest, "Passwords do not match."
	}
	if dbpkg.HasPassword(h.db, user.ID) {
		if current == "" {
			return http.StatusBadRequest, "Please enter your current password."
		}
		confirmedUID, err := dbpkg.CheckPassword(h.db, user.Handle, current)
		if err != nil || confirmedUID == "" {
			return http.StatusForbidden, "Current password is incorrect."
		}
	}
	if err := dbpkg.SetPassword(h.db, user.ID, next); err != nil {
		log.Printf("[settings] SetPassword error: %v", err)
		return http.StatusInternalServerError, "Could not save password — try again."
	}
	return http.StatusOK, "Password updated successfully."
}

// workEditWindow is how long after posting a work may still be edited.
const workEditWindow = 60 * time.Minute

// applyWorkEdit is the one set of rules for editing a work: the author only,
// within the edit window, with the edition recorded alongside the new body
// in one transaction. 200 means it changed.
func (h *Handler) applyWorkEdit(user *model.User, workID, body string) (int, string) {
	if h.db == nil {
		return http.StatusServiceUnavailable, "db unavailable"
	}
	if user == nil || user.ID == "" {
		return http.StatusUnauthorized, "unauthorized"
	}
	body = strings.TrimSpace(body)
	if workID == "" || body == "" {
		return http.StatusBadRequest, "work_id and body required"
	}
	if n := len([]rune(body)); n > workBodyMaxRunes {
		return http.StatusBadRequest, fmt.Sprintf("body exceeds %d characters", workBodyMaxRunes)
	}
	var authorID string
	var createdAt time.Time
	err := h.db.QueryRow(
		`SELECT author_id, created_at FROM works WHERE id = $1 AND deleted_at IS NULL`, workID,
	).Scan(&authorID, &createdAt)
	if err != nil {
		return http.StatusNotFound, "work not found"
	}
	if authorID != user.ID {
		return http.StatusForbidden, "forbidden"
	}
	if time.Since(createdAt) > workEditWindow {
		return http.StatusConflict, "edit window closed"
	}
	var editionCount int
	_ = h.db.QueryRow(`SELECT COUNT(*) FROM editions WHERE work_id = $1::uuid`, workID).Scan(&editionCount)
	nextEdition := editionCount + 1
	editionCID := workEditionCID(workID, nextEdition, body)
	if err := dbpkg.RecordWorkEdit(h.db, workID, user.ID, body, editionCID, nextEdition); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return http.StatusNotFound, "work not found"
		}
		log.Printf("[editWork] record edit work=%s: %v", workID, err)
		return http.StatusInternalServerError, "update failed"
	}
	return http.StatusOK, ""
}
