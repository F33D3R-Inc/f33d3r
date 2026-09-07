package handler

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
)

// ── /api/v1/visions — the ring tray and the room around a Vision ────────────
//
// The web draws Visions as Facets (vision_compose.go); the native clients draw
// them from these DTOs. Same rows, same admission rules: the artboard presets,
// the body limit, the 18+ clearance and the media rules are the ones
// visionCreate applies, and a Vision posted here shows in the web's rail the
// same as one posted there.
//
// A Vision has no id of its own — the wire "id" is the canonical
// `<author_pial>.<seq>` address (dbpkg.VisionRef.String()), the same string
// devserver's store.VisionRef carries.
//
// Shape is F33D3RKit/Sources/F33D3RKit/Models/Vision.swift.

// VisionArtboardDTO is the server-resolved artboard of a text Vision.
type VisionArtboardDTO struct {
	Background string `json:"background"`
	Typeface   string `json:"typeface"`
	TypeScale  string `json:"type_scale"`
	Align      string `json:"align"`
}

// VisionDTO is one Vision as the viewer draws it.
type VisionDTO struct {
	ID           string            `json:"id"`
	Author       WorkAuthorDTO     `json:"author"`
	ContentType  string            `json:"content_type"`
	Body         string            `json:"body"`
	Artboard     VisionArtboardDTO `json:"artboard"`
	MediaURLs    []string          `json:"media_urls,omitempty"`
	Poll         *PollDTO          `json:"poll,omitempty"`
	IsNSFW       bool              `json:"is_nsfw"`
	Audience     string            `json:"audience"`
	AllowReplies bool              `json:"allow_replies"`
	CreatedAt    time.Time         `json:"created_at"`
	ExpiresAt    time.Time         `json:"expires_at"`
	Seen         bool              `json:"seen"`
	// Owner-only: how many people watched.
	ViewCount *int `json:"view_count,omitempty"`
}

// VisionRingDTO is one author's ring on the tray.
type VisionRingDTO struct {
	Author      WorkAuthorDTO  `json:"author"`
	State       string         `json:"state"`
	Count       int            `json:"count"`
	UnseenCount int            `json:"unseen_count"`
	LatestAt    time.Time      `json:"latest_at"`
	IsLive      bool           `json:"is_live"`
	Provenance  *ProvenanceDTO `json:"provenance,omitempty"`
	Visions     []VisionDTO    `json:"visions"`
}

// VisionTrayDTO is the tray: the viewer's own ring first, then the rings of
// the accounts they follow, unseen first.
type VisionTrayDTO struct {
	Own   *VisionRingDTO  `json:"own,omitempty"`
	Rings []VisionRingDTO `json:"rings"`
}

// visionAuthorDTO is the author strip from the columns the vision query joins.
func visionAuthorDTO(handle, name, avatar string) WorkAuthorDTO {
	if name == "" {
		name = handle
	}
	return WorkAuthorDTO{
		Handle:      handle,
		DisplayName: name,
		AvatarURL:   strPtr(avatar),
		Role:        model.RoleUser,
		Realm:       1,
	}
}

// visionPollDTO builds the ballot for a Vision that carries one; nil otherwise.
func (h *Handler) visionPollDTO(v *model.Vision) *PollDTO {
	if len(v.PollOptions) == 0 {
		return nil
	}
	votes, err := dbpkg.VisionPollVotes(h.db, visionRef(v), len(v.PollOptions))
	if err != nil {
		log.Printf("[api/v1] vision poll votes %s: %v", visionRef(v), err)
		votes = make([]int, len(v.PollOptions))
	}
	ends := v.ExpiresAt
	poll := &model.Poll{
		Options:  v.PollOptions,
		Votes:    votes,
		UserVote: v.ViewerVote,
		EndsAt:   &ends,
	}
	poll.Project(time.Now())
	return pollDTO(poll)
}

// visionDTO projects one Vision for the viewer. Author display comes from the
// query's join; the realm is the base realm because the join carries no XP.
func (h *Handler) visionDTO(v *model.Vision, viewer *model.User) VisionDTO {
	author := visionAuthorDTO(v.AuthorHandle, v.AuthorName, v.AvatarURL)
	isOwner := viewer != nil && v.AuthorPIAL == viewer.PIALID
	if isOwner {
		author = userAuthorDTO(viewer)
	}
	d := VisionDTO{
		ID:          visionRef(v).String(),
		Author:      author,
		ContentType: v.ContentType,
		Body:        v.Body,
		Artboard: VisionArtboardDTO{
			Background: v.ArtboardBackground,
			Typeface:   v.ArtboardTypeface,
			TypeScale:  resolveVisionTypeScale(v.Body, v.ArtboardTypeScale),
			Align:      v.ArtboardAlign,
		},
		MediaURLs:    v.MediaURLs,
		Poll:         h.visionPollDTO(v),
		IsNSFW:       v.IsNSFW,
		Audience:     v.Audience,
		AllowReplies: v.AllowReplies,
		CreatedAt:    v.CreatedAt,
		ExpiresAt:    v.ExpiresAt,
		Seen:         v.ViewedByViewer,
	}
	if d.Audience == "" {
		d.Audience = "everyone"
	}
	if isOwner {
		n := v.ViewCount
		d.ViewCount = &n
	}
	return d
}

// visionRingDTO assembles one ring from its author's live Visions, already
// filtered for the viewer.
func (h *Handler) visionRingDTO(author WorkAuthorDTO, authorPIAL string, visions []*model.Vision, viewer *model.User, isLive bool) VisionRingDTO {
	ring := VisionRingDTO{
		Author:  author,
		State:   model.VisionRingSeen,
		Count:   len(visions),
		IsLive:  isLive,
		Visions: make([]VisionDTO, 0, len(visions)),
	}
	for _, v := range visions {
		d := h.visionDTO(v, viewer)
		if !v.ViewedByViewer {
			ring.UnseenCount++
		}
		if v.CreatedAt.After(ring.LatestAt) {
			ring.LatestAt = v.CreatedAt
		}
		ring.Visions = append(ring.Visions, d)
	}
	if ring.UnseenCount > 0 {
		ring.State = model.VisionRingUnseen
	}
	if viewer == nil || authorPIAL != viewer.PIALID {
		ring.Provenance = &ProvenanceDTO{Kind: "you_follow", Text: "You follow @" + author.Handle, Handle: strPtr(author.Handle)}
	}
	return ring
}

// visionsForViewer drops what the viewer must not see: 18+ Visions for an
// account that is not cleared for them.
func visionsForViewer(visions []*model.Vision, viewer *model.User) []*model.Vision {
	if !excludeNSFW(viewer) {
		return visions
	}
	kept := visions[:0]
	for _, v := range visions {
		if v != nil && !v.IsNSFW {
			kept = append(kept, v)
		}
	}
	return kept
}

// apiV1Visions — GET /api/v1/visions: the tray. LiveAuthorIDs answers by
// users.id, so a ring's liveness is looked up through the author's account id
// (resolved from their PIAL) rather than the PIAL itself.
func (h *Handler) apiV1Visions(w http.ResponseWriter, r *http.Request, u *model.User) {
	out := VisionTrayDTO{Rings: []VisionRingDTO{}}
	liveByID, err := dbpkg.LiveAuthorIDs(h.db)
	if err != nil {
		log.Printf("[api/v1] live authors: %v", err)
		liveByID = map[string]bool{}
	}

	if u.PIALID != "" {
		own, err := dbpkg.GetActiveVisionsForAuthor(h.db, u.PIALID, u.PIALID, 50)
		if err != nil {
			apiServerError(w, err)
			return
		}
		if len(own) > 0 {
			ring := h.visionRingDTO(userAuthorDTO(u), u.PIALID, own, u, liveByID[u.ID])
			out.Own = &ring
		}
	}

	rings, err := dbpkg.GetVisionRingsForViewer(h.db, u.ID, u.PIALID, 100)
	if err != nil {
		apiServerError(w, err)
		return
	}
	for _, ring := range rings {
		if ring == nil || ring.AuthorPIAL == u.PIALID {
			continue
		}
		visions, err := dbpkg.GetActiveVisionsForAuthor(h.db, ring.AuthorPIAL, u.PIALID, 50)
		if err != nil {
			apiServerError(w, err)
			return
		}
		visions = visionsForViewer(visions, u)
		if len(visions) == 0 {
			continue
		}
		author := visionAuthorDTO(ring.AuthorHandle, ring.AuthorName, ring.AvatarURL)
		authorID, _ := dbpkg.GetUserIDFromPIAL(h.db, ring.AuthorPIAL)
		out.Rings = append(out.Rings, h.visionRingDTO(author, ring.AuthorPIAL, visions, u, liveByID[authorID]))
	}
	apiJSON(w, http.StatusOK, out)
}

// apiV1VisionViewers — GET /api/v1/visions/{id}/viewers: who watched. The
// author's alone.
func (h *Handler) apiV1VisionViewers(w http.ResponseWriter, r *http.Request, u *model.User) {
	ref, err := dbpkg.ParseVisionRef(r.PathValue("id"))
	if err != nil {
		apiError(w, http.StatusNotFound, "not_found", "That vision is gone.")
		return
	}
	v, err := dbpkg.GetVisionByRef(h.db, ref, u.PIALID)
	if err != nil {
		if errors.Is(err, dbpkg.ErrVisionNotLive) {
			apiError(w, http.StatusNotFound, "not_found", "That vision is gone.")
			return
		}
		apiServerError(w, err)
		return
	}
	if v.AuthorPIAL != u.PIALID {
		apiError(w, http.StatusForbidden, "forbidden", "Only the author can see who watched.")
		return
	}
	viewers, err := dbpkg.GetVisionViewers(h.db, ref, 200)
	if err != nil {
		apiServerError(w, err)
		return
	}
	out := make([]WorkAuthorDTO, 0, len(viewers))
	for _, viewer := range viewers {
		if viewer.Handle == "" {
			continue
		}
		out = append(out, visionAuthorDTO(viewer.Handle, viewer.Name, viewer.AvatarURL))
	}
	apiJSON(w, http.StatusOK, VisionViewersDTO{Viewers: out, Count: v.ViewCount})
}

// ── The event lane ────────────────────────────────────────────────────────────

// apiV1VisionEvent handles the Vision events the native clients post to
// POST /events. It returns false for any other event_type, untouched.
func (h *Handler) apiV1VisionEvent(w http.ResponseWriter, r *http.Request, eventType string, rawBodyMap map[string]json.RawMessage) bool {
	switch eventType {
	case "vision", "vision_seen", "vision_delete", "vision_reply", "vision_poll_vote", "vision_mute", "vision_unmute":
	default:
		return false
	}
	if h.db == nil {
		http.Error(w, "db unavailable", http.StatusServiceUnavailable)
		return true
	}
	user := visionActor(h.userFromRequest(w, r))
	if user == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return true
	}
	switch eventType {
	case "vision":
		h.visionCreateEvent(w, r, user, rawBodyMap)
	case "vision_seen":
		_, ref, ok := h.visionForViewer(w, r, user, rawBodyMap)
		if !ok {
			return true
		}
		if err := dbpkg.MarkVisionViewed(h.db, ref, user.PIALID); err != nil {
			if errors.Is(err, dbpkg.ErrVisionNotLive) {
				http.Error(w, "vision not found", http.StatusNotFound)
				return true
			}
			log.Printf("[vision] mark viewed %s by %s: %v", ref, user.PIALID, err)
			http.Error(w, "server error", http.StatusInternalServerError)
			return true
		}
		w.WriteHeader(http.StatusNoContent)
	case "vision_delete":
		id := eventField(r, rawBodyMap, "vision_id")
		ref, perr := dbpkg.ParseVisionRef(id)
		if perr != nil {
			http.Error(w, perr.Error(), http.StatusBadRequest)
			return true
		}
		if err := dbpkg.SoftDeleteVision(h.db, ref, user.PIALID); err != nil {
			if errors.Is(err, dbpkg.ErrVisionNotLive) {
				http.Error(w, "Vision not found or you do not own it", http.StatusForbidden)
				return true
			}
			log.Printf("[vision] delete %s by %s: %v", ref, user.PIALID, err)
			http.Error(w, "server error", http.StatusInternalServerError)
			return true
		}
		w.WriteHeader(http.StatusNoContent)
	case "vision_reply":
		v, ref, ok := h.visionForViewer(w, r, user, rawBodyMap)
		if !ok {
			return true
		}
		if !v.AllowReplies {
			http.Error(w, "this vision does not take replies", http.StatusForbidden)
			return true
		}
		text := strings.TrimSpace(eventField(r, rawBodyMap, "body"))
		if text == "" {
			http.Error(w, "vision_id and body required", http.StatusBadRequest)
			return true
		}
		if runes := []rune(text); len(runes) > 500 {
			text = string(runes[:500])
		}
		authorID, _ := dbpkg.GetUserIDFromPIAL(h.db, v.AuthorPIAL)
		h.notifyUserWithPayload(authorID, "vision_reply", user.ID, ref.String(), "vision", map[string]interface{}{"preview": text})
		w.WriteHeader(http.StatusNoContent)
	case "vision_poll_vote":
		_, ref, ok := h.visionForViewer(w, r, user, rawBodyMap)
		if !ok {
			return true
		}
		idx, ok := eventIntField(r, rawBodyMap, "option_idx")
		if !ok {
			http.Error(w, "vision_id and option_idx required", http.StatusBadRequest)
			return true
		}
		if err := dbpkg.CastVisionPollVote(h.db, ref, user.PIALID, idx); err != nil {
			switch {
			case errors.Is(err, dbpkg.ErrVisionNotLive):
				http.Error(w, "vision not found", http.StatusNotFound)
			case errors.Is(err, dbpkg.ErrVisionNoPoll), errors.Is(err, dbpkg.ErrVisionPollOption):
				http.Error(w, err.Error(), http.StatusBadRequest)
			default:
				log.Printf("[vision] poll vote %s by %s: %v", ref, user.PIALID, err)
				http.Error(w, "server error", http.StatusInternalServerError)
			}
			return true
		}
		w.WriteHeader(http.StatusNoContent)
	case "vision_mute", "vision_unmute":
		handle := strings.TrimPrefix(strings.ToLower(eventField(r, rawBodyMap, "target_handle")), "@")
		target, err := dbpkg.GetUserByHandle(h.db, handle)
		if err != nil || target == nil || !handleRe.MatchString(handle) {
			http.Error(w, "user not found", http.StatusNotFound)
			return true
		}
		if err := dbpkg.SetVisionMuted(h.db, user.PIALID, target.PIALID, eventType == "vision_mute"); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return true
		}
		w.WriteHeader(http.StatusNoContent)
	}
	return true
}

// visionForViewer loads the Vision named by vision_id and applies the audience
// rule: a followers-only Vision is a 404 to anyone who is neither a follower
// nor the author. It answers the request itself on refusal.
func (h *Handler) visionForViewer(w http.ResponseWriter, r *http.Request, user *model.User, rawBodyMap map[string]json.RawMessage) (*model.Vision, dbpkg.VisionRef, bool) {
	id := eventField(r, rawBodyMap, "vision_id")
	if id == "" {
		http.Error(w, "vision_id required", http.StatusBadRequest)
		return nil, dbpkg.VisionRef{}, false
	}
	ref, perr := dbpkg.ParseVisionRef(id)
	if perr != nil {
		http.Error(w, perr.Error(), http.StatusBadRequest)
		return nil, dbpkg.VisionRef{}, false
	}
	v, err := dbpkg.GetVisionByRef(h.db, ref, user.PIALID)
	if err != nil {
		if errors.Is(err, dbpkg.ErrVisionNotLive) {
			http.Error(w, "vision not found", http.StatusNotFound)
			return nil, dbpkg.VisionRef{}, false
		}
		log.Printf("[vision] load %s: %v", ref, err)
		http.Error(w, "server error", http.StatusInternalServerError)
		return nil, dbpkg.VisionRef{}, false
	}
	if v.Audience == "followers" && v.AuthorPIAL != user.PIALID {
		authorID, _ := dbpkg.GetUserIDFromPIAL(h.db, v.AuthorPIAL)
		follows, ferr := dbpkg.IsFollowing(h.db, user.ID, authorID)
		if ferr != nil || !follows {
			http.Error(w, "vision not found", http.StatusNotFound)
			return nil, dbpkg.VisionRef{}, false
		}
	}
	return v, ref, true
}

// visionCreateEvent — {event_type:"vision", ...} → 201 {"vision_id": id}. The
// same rules as the web composer (visionCreate), read from a JSON body.
func (h *Handler) visionCreateEvent(w http.ResponseWriter, r *http.Request, user *model.User, rawBodyMap map[string]json.RawMessage) {
	contentType := eventField(r, rawBodyMap, "content_type")
	if contentType == "" {
		contentType = "text"
	}
	if !visionComposeContentTypes[contentType] || contentType == "video" {
		http.Error(w, "unknown Vision content type", http.StatusBadRequest)
		return
	}

	body := strings.TrimSpace(eventField(r, rawBodyMap, "body"))
	if runes := []rune(body); len(runes) > visionBodyMax {
		body = string(runes[:visionBodyMax])
	}

	background, err := visionPresetOrError("artboard_background", eventField(r, rawBodyMap, "artboard_background"), visionBackgroundSet, visionDefaultBackground)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	typeface, err := visionPresetOrError("artboard_typeface", eventField(r, rawBodyMap, "artboard_typeface"), visionTypefaceSet, visionDefaultTypeface)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	typeScale, err := visionPresetOrError("artboard_type_scale", eventField(r, rawBodyMap, "artboard_type_scale"), visionTypeScaleSet, visionDefaultTypeScale)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	align, err := visionPresetOrError("artboard_align", eventField(r, rawBodyMap, "artboard_align"), visionAlignSet, visionDefaultAlign)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	isNSFW, _ := eventBool(r, rawBodyMap, "is_nsfw", "nsfw")
	if isNSFW && !visionCanPostNSFW(user) {
		http.Error(w, "Your account is not cleared to post 18+ content.", http.StatusForbidden)
		return
	}

	audience := eventField(r, rawBodyMap, "audience")
	if audience == "" {
		audience = "everyone"
	}
	if audience != "everyone" && audience != "followers" {
		http.Error(w, "audience must be everyone or followers", http.StatusBadRequest)
		return
	}
	allowReplies := true
	if v, present := eventBool(r, rawBodyMap, "allow_replies"); present {
		allowReplies = v
	}

	in := dbpkg.VisionInput{
		AuthorPIAL:         user.PIALID,
		ContentType:        contentType,
		Body:               body,
		ArtboardBackground: background,
		ArtboardTypeface:   typeface,
		ArtboardTypeScale:  typeScale,
		ArtboardAlign:      align,
		IsNSFW:             isNSFW,
		Audience:           audience,
		AllowReplies:       allowReplies,
	}

	if hrs, ok := eventIntField(r, rawBodyMap, "ttl_hours"); ok {
		if hrs < 1 || hrs > 48 {
			http.Error(w, "ttl_hours must be between 1 and 48", http.StatusBadRequest)
			return
		}
		in.TTL = time.Duration(hrs) * time.Hour
	}

	source := eventField(r, rawBodyMap, "source")
	if source != "camera" {
		source = "composer"
	}
	metadata, err := json.Marshal(map[string]string{"source": source})
	if err != nil {
		http.Error(w, "the Vision could not be prepared", http.StatusInternalServerError)
		return
	}
	in.Metadata = metadata

	if raw, ok := rawBodyMap["poll_options"]; ok && len(raw) > 0 && string(raw) != "null" {
		var options []string
		if err := json.Unmarshal(raw, &options); err != nil {
			http.Error(w, "poll_options must be a list of strings", http.StatusBadRequest)
			return
		}
		if contentType != "text" {
			http.Error(w, "a poll goes on a text Vision", http.StatusBadRequest)
			return
		}
		clean := make([]string, 0, len(options))
		for _, o := range options {
			o = strings.TrimSpace(o)
			if o == "" {
				http.Error(w, "poll options cannot be empty", http.StatusBadRequest)
				return
			}
			if runes := []rune(o); len(runes) > 60 {
				o = string(runes[:60])
			}
			clean = append(clean, o)
		}
		if len(clean) < 2 || len(clean) > dbpkg.VisionPollMaxOptions {
			http.Error(w, "a poll has 2 to 4 options", http.StatusBadRequest)
			return
		}
		in.PollOptions = clean
	}

	switch contentType {
	case "text":
		if body == "" {
			http.Error(w, "Write something first — a text Vision needs words.", http.StatusBadRequest)
			return
		}
	case "image":
		mediaURL, uErr := visionLocalAssetURL(eventField(r, rawBodyMap, "media_url"))
		if uErr != nil {
			http.Error(w, uErr.Error(), http.StatusBadRequest)
			return
		}
		in.MediaURLs = []string{mediaURL}
		in.Media = []dbpkg.VisionMediaInput{{
			ObjectKey: mediaURL,
			AssetURL:  mediaURL,
			MediaKind: "image",
		}}
	case "share":
		workID, idErr := visionSharedWorkID(eventField(r, rawBodyMap, "shared_work_id"))
		if idErr != nil {
			http.Error(w, idErr.Error(), http.StatusBadRequest)
			return
		}
		work, wErr := dbpkg.GetWorkByID(h.db, workID, user.ID)
		if wErr != nil {
			if errors.Is(wErr, sql.ErrNoRows) {
				http.Error(w, "That work is not available to share.", http.StatusBadRequest)
				return
			}
			log.Printf("[vision] load shared work %s: %v", workID, wErr)
			http.Error(w, "the shared work could not be read", http.StatusInternalServerError)
			return
		}
		in.SharedWorkID = work.ID
		if work.IsNSFW {
			in.IsNSFW = true
		}
	}

	ref, err := dbpkg.InsertVision(h.db, in)
	if err != nil {
		log.Printf("[vision] insert for %s: %v", user.ID, err)
		http.Error(w, "The Vision could not be posted. Try again.", http.StatusInternalServerError)
		return
	}

	go dbpkg.TryAwardVisionAchievements(h.db, user.ID, user.PIALID)
	go h.publishVisionContentEvent(ref.String(), user.PIALID, in.Body, in.MediaURLs)

	apiJSON(w, http.StatusCreated, map[string]string{"vision_id": ref.String()})
}

// eventBool reads a boolean field under any of names, whether it arrived as
// a JSON boolean, a JSON string, or a form value. present is false when no
// name is in the body at all, so a caller can keep its default.
func eventBool(r *http.Request, rawBodyMap map[string]json.RawMessage, names ...string) (value, present bool) {
	for _, name := range names {
		if raw, ok := rawBodyMap[name]; ok {
			var b bool
			if err := json.Unmarshal(raw, &b); err == nil {
				return b, true
			}
		}
	}
	v := strings.ToLower(eventField(r, rawBodyMap, names...))
	if v == "" {
		return false, false
	}
	switch v {
	case "1", "true", "on", "yes":
		return true, true
	}
	return false, true
}
