package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"f33d3r.com/ios/devserver/internal/store"
)

// eventBody is a POST /events request read once, whichever encoding it came
// in. feed-engine's handlers read some fields through r.FormValue and some
// through the JSON body; here every event accepts both, which is a superset of
// what the real server does and cannot break a client written for it.
type eventBody struct {
	eventType string
	form      map[string]string
	raw       map[string]json.RawMessage
	rawBytes  []byte
}

func (b *eventBody) get(key string) string {
	if v, ok := b.form[key]; ok && v != "" {
		return v
	}
	if b.raw != nil {
		if rv, ok := b.raw[key]; ok {
			var s string
			if json.Unmarshal(rv, &s) == nil {
				return s
			}
			var n json.Number
			if json.Unmarshal(rv, &n) == nil {
				return n.String()
			}
			// A JSON boolean reads back as "true"/"false", which is what the
			// form encoding of the same field would have carried. Without
			// this a `{"locked": false}` is indistinguishable from an absent
			// key, and every flag the client can switch off would be stuck on.
			var flag bool
			if json.Unmarshal(rv, &flag) == nil {
				return strconv.FormatBool(flag)
			}
		}
	}
	return ""
}

// has reports whether the request carried the key at all, however it was
// encoded. A patch needs the difference between "set this to empty" and "leave
// it alone", and get() cannot tell them apart.
func (b *eventBody) has(key string) bool {
	if _, ok := b.form[key]; ok {
		return true
	}
	if b.raw != nil {
		if _, ok := b.raw[key]; ok {
			return true
		}
	}
	return false
}

// getStrings reads a JSON array of strings (or a form field holding a JSON
// array, or a comma-separated list). Nil when absent.
func (b *eventBody) getStrings(key string) []string {
	if b.raw != nil {
		if rv, ok := b.raw[key]; ok {
			var arr []string
			if json.Unmarshal(rv, &arr) == nil {
				return arr
			}
		}
	}
	if v, ok := b.form[key]; ok && v != "" {
		var arr []string
		if json.Unmarshal([]byte(v), &arr) == nil {
			return arr
		}
		return strings.Split(v, ",")
	}
	return nil
}

func readEvent(r *http.Request) (*eventBody, error) {
	b := &eventBody{form: map[string]string{}}
	ct := r.Header.Get("Content-Type")
	body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	b.rawBytes = body
	if strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		if err := r.ParseForm(); err != nil {
			return nil, err
		}
		for k, v := range r.PostForm {
			if len(v) > 0 {
				b.form[k] = v[0]
			}
		}
	} else if len(body) > 0 {
		if err := json.Unmarshal(body, &b.raw); err != nil {
			return nil, err
		}
	}
	b.eventType = b.get("event_type")
	return b, nil
}

// events — POST /events. The single mutation lane: every state change carries
// an `event_type`, and a body carrying a `cid` is a signed work whatever its
// `event_type` says.
func (s *Server) events(w http.ResponseWriter, r *http.Request, u *store.User) {
	body, err := readEvent(r)
	if err != nil {
		plainError(w, http.StatusBadRequest, "malformed request")
		return
	}
	if body.eventType == "" {
		plainError(w, http.StatusBadRequest, "event_type required")
		return
	}

	if cid := body.get("cid"); strings.HasPrefix(cid, "sha256:") {
		s.workEvent(w, r, u, body)
		return
	}

	switch body.eventType {
	case "work_like", "work_unlike", "work_repost", "work_unrepost",
		"work_bookmark", "work_unbookmark", "work_dislike", "work_undislike":
		s.workReactionEvent(w, r, u, body)
	case "work_delete":
		s.workDeleteEvent(w, r, u, body)
	case "follow", "unfollow":
		s.followEvent(w, r, u, body)
	case "block", "unblock":
		s.blockEvent(w, r, u, body)
	case "mute", "unmute":
		s.muteEvent(w, r, u, body)
	case "tip":
		s.tipEvent(w, r, u, body)
	case "set_reply_restriction":
		s.replyRestrictionEvent(w, r, u, body)
	case "pin_post", "pin_work", "unpin_post", "unpin_work":
		s.pinEvent(w, r, u, body)
	case "report":
		s.reportEvent(w, r, u, body)
	case "poll_vote":
		s.pollVoteEvent(w, r, u, body)
	case "notif_read":
		s.notifReadEvent(w, r, u, body)
	case "notif_read_all":
		if err := s.store.MarkAllRead(r.Context(), u.ID); err != nil {
			plainError(w, http.StatusInternalServerError, "server error")
			return
		}
		s.signalNotify(r, u.ID)
		w.WriteHeader(http.StatusNoContent)
	case "not_interested":
		// Recorded as a signal and nothing else: it never reaches the author.
		w.WriteHeader(http.StatusNoContent)
	case "vision":
		s.visionCreateEvent(w, r, u, body)
	case "vision_seen":
		s.visionSeenEvent(w, r, u, body)
	case "vision_delete":
		s.visionDeleteEvent(w, r, u, body)
	case "vision_reply":
		s.visionReplyEvent(w, r, u, body)
	case "vision_poll_vote":
		s.visionPollVoteEvent(w, r, u, body)
	case "vision_mute", "vision_unmute":
		s.visionMuteEvent(w, r, u, body)
	case "live_start":
		s.liveStartEvent(w, r, u, body)
	case "live_end":
		s.liveEndEvent(w, r, u, body)
	case "live_chat":
		s.liveChatEvent(w, r, u, body)
	case "live_pin":
		s.livePinEvent(w, r, u, body)
	case "live_heart":
		s.liveHeartEvent(w, r, u, body)
	case "profile_update":
		s.profileUpdateEvent(w, r, u, body)
	case "settings.privacy.show_sensitive", "settings.privacy.celebrations", "settings.privacy.account_private", "settings.privacy.content_setting":
		s.settingsEvent(w, r, u, body)
	case "password_change":
		s.passwordChangeEvent(w, r, u, body)
	case "session_revoke":
		s.sessionRevokeEvent(w, r, u, body)
	case "sessions_revoke_others":
		if err := s.store.RevokeOtherSessions(r.Context(), u.ID, bearerToken(r)); err != nil {
			plainError(w, http.StatusInternalServerError, "server error")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case "work_edit":
		s.workEditEvent(w, r, u, body)
	case "work_purchase":
		s.workPurchaseEvent(w, r, u, body)
	case "frequency_create", "frequency_update", "frequency_schedule", "frequency_start", "frequency_end",
		"frequency_cancel", "frequency_join", "frequency_leave", "frequency_request_mic",
		"frequency_request_withdraw", "frequency_request_upvote", "frequency_request_approve",
		"frequency_request_decline", "frequency_mute", "frequency_demote", "frequency_remove",
		"frequency_block", "frequency_unblock", "frequency_cohost", "frequency_lock", "frequency_requests_open":
		s.frequencyEvent(w, r, u, body)
	default:
		plainError(w, http.StatusNotFound, "unknown event_type: "+body.eventType)
	}
}

// workReactionEvent — like/repost/bookmark/dislike and their inverses. 204.
func (s *Server) workReactionEvent(w http.ResponseWriter, r *http.Request, u *store.User, body *eventBody) {
	workID := body.get("work_id")
	if workID == "" {
		plainError(w, http.StatusBadRequest, "work_id required")
		return
	}
	ops := map[string]struct {
		kind string
		add  bool
	}{
		"work_like": {"like", true}, "work_unlike": {"like", false},
		"work_repost": {"repost", true}, "work_unrepost": {"repost", false},
		"work_bookmark": {"bookmark", true}, "work_unbookmark": {"bookmark", false},
		"work_dislike": {"dislike", true}, "work_undislike": {"dislike", false},
	}
	op := ops[body.eventType]
	ctx := r.Context()
	authorID, authorPIAL, err := s.store.WorkOwner(ctx, workID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			plainError(w, http.StatusNotFound, "work not found")
			return
		}
		plainError(w, http.StatusInternalServerError, "server error")
		return
	}
	changed, err := s.store.React(ctx, workID, u.ID, op.kind, op.add)
	if err != nil {
		plainError(w, http.StatusInternalServerError, "server error")
		return
	}
	if changed && op.add && (op.kind == "like" || op.kind == "repost") {
		s.store.Notify(ctx, authorID, op.kind, u.ID, workID, "work", map[string]any{"preview": s.previewOf(r, workID)})
		if op.kind == "like" {
			s.store.AwardXP(ctx, authorPIAL, store.XPLike)
		}
		s.signalNotify(r, authorID)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) previewOf(r *http.Request, workID string) string {
	var body string
	s.store.DB().QueryRowContext(r.Context(), `SELECT body FROM works WHERE id = ?`, workID).Scan(&body)
	body = strings.TrimSpace(body)
	if len(body) > 120 {
		body = body[:120]
	}
	return body
}

// workDeleteEvent — soft-deletes the caller's own work. 200 with an empty
// body on success (the HTMX swap contract), 403 when not owned.
func (s *Server) workDeleteEvent(w http.ResponseWriter, r *http.Request, u *store.User, body *eventBody) {
	workID := body.get("work_id")
	if workID == "" {
		plainError(w, http.StatusBadRequest, "work_id required")
		return
	}
	if err := s.store.SoftDeleteWork(r.Context(), workID, u.ID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			plainError(w, http.StatusForbidden, "Post not found or you do not own it")
			return
		}
		plainError(w, http.StatusInternalServerError, "server error")
		return
	}
	// Everyone who follows the author learns the row is gone.
	if followers, err := s.store.FollowerIDs(r.Context(), u.ID); err == nil {
		for _, fid := range followers {
			s.hub.publish(userTopic(fid), "post_deleted", map[string]string{"work_id": workID})
		}
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) targetUser(r *http.Request, body *eventBody) (*store.User, error) {
	handle := body.get("target_handle")
	if handle != "" {
		return s.store.GetUserByHandle(r.Context(), handle)
	}
	if pial := body.get("target_pial"); pial != "" {
		return s.store.GetUserByPIAL(r.Context(), pial)
	}
	return nil, errors.New("target_pial or target_handle required")
}

// followEvent — follow/unfollow by handle (or PIAL). 204.
func (s *Server) followEvent(w http.ResponseWriter, r *http.Request, u *store.User, body *eventBody) {
	target, err := s.targetUser(r, body)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			plainError(w, http.StatusNotFound, "user not found")
			return
		}
		plainError(w, http.StatusBadRequest, err.Error())
		return
	}
	if target.ID == u.ID {
		plainError(w, http.StatusBadRequest, "cannot follow yourself")
		return
	}
	ctx := r.Context()
	var changed bool
	if body.eventType == "follow" {
		changed, err = s.store.Follow(ctx, u.ID, target.ID)
	} else {
		changed, err = s.store.Unfollow(ctx, u.ID, target.ID)
	}
	if err != nil {
		plainError(w, http.StatusInternalServerError, "could not save that — try again")
		return
	}
	if changed && body.eventType == "follow" {
		s.store.Notify(ctx, target.ID, "follow", u.ID, "", "profile", nil)
		s.store.AwardXP(ctx, target.PIALID, store.XPFollow)
		s.signalNotify(r, target.ID)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) blockEvent(w http.ResponseWriter, r *http.Request, u *store.User, body *eventBody) {
	target, err := s.targetUser(r, body)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			plainError(w, http.StatusNotFound, "user not found")
			return
		}
		plainError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.store.SetBlocked(r.Context(), u.ID, target.ID, body.eventType == "block"); err != nil {
		plainError(w, http.StatusInternalServerError, "server error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) muteEvent(w http.ResponseWriter, r *http.Request, u *store.User, body *eventBody) {
	target, err := s.targetUser(r, body)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			plainError(w, http.StatusNotFound, "user not found")
			return
		}
		plainError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.store.SetMuted(r.Context(), u.ID, target.ID, body.eventType == "mute"); err != nil {
		plainError(w, http.StatusInternalServerError, "server error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// tipEvent — `amount_aet` is in hundredths of an AET, as the web sends it.
// Optional `work_id` attributes the tip to a work so its card can show what it
// has earned.
func (s *Server) tipEvent(w http.ResponseWriter, r *http.Request, u *store.User, body *eventBody) {
	handle := body.get("target_handle")
	amountStr := body.get("amount_aet")
	if (handle == "" && body.get("stream_id") == "") || amountStr == "" {
		plainError(w, http.StatusBadRequest, "target_handle and amount_aet required")
		return
	}
	hundredths, err := strconv.Atoi(amountStr)
	if err != nil || hundredths <= 0 {
		plainError(w, http.StatusBadRequest, "invalid amount")
		return
	}
	ctx := r.Context()
	if streamID := body.get("stream_id"); streamID != "" {
		s.liveTip(w, r, u, streamID, int64(hundredths)*(store.UAETPerAET/100))
		return
	}
	target, err := s.store.GetUserByHandle(ctx, handle)
	if err != nil {
		plainError(w, http.StatusNotFound, "user not found")
		return
	}
	if target.ID == u.ID {
		plainError(w, http.StatusBadRequest, "cannot tip yourself")
		return
	}
	workID := body.get("work_id")
	if workID != "" {
		if owner, _, err := s.store.WorkOwner(ctx, workID); err != nil || owner != target.ID {
			plainError(w, http.StatusBadRequest, "work_id does not belong to target_handle")
			return
		}
	}
	amountUAET := int64(hundredths) * (store.UAETPerAET / 100)
	if err := s.store.Tip(ctx, u.PIALID, target.PIALID, workID, amountUAET); err != nil {
		if errors.Is(err, store.ErrInsufficientBalance) {
			plainError(w, http.StatusPaymentRequired, "insufficient balance")
			return
		}
		plainError(w, http.StatusInternalServerError, "server error")
		return
	}
	targetType := "profile"
	if workID != "" {
		targetType = "work"
	}
	s.store.Notify(ctx, target.ID, "tip", u.ID, workID, targetType, map[string]any{"amount_uaet": amountUAET})
	s.signalNotify(r, target.ID)
	s.signalBalance(r, target)
	s.signalBalance(r, u)
	w.WriteHeader(http.StatusNoContent)
}

// replyRestrictionEvent — owner sets who may reply. `everyone` is stored as
// `open`, matching feed-engine.
func (s *Server) replyRestrictionEvent(w http.ResponseWriter, r *http.Request, u *store.User, body *eventBody) {
	workID := body.get("post_id")
	if workID == "" {
		workID = body.get("work_id")
	}
	restriction := body.get("restriction")
	switch restriction {
	case "everyone", "followers", "circle", "none", "verified":
	default:
		plainError(w, http.StatusBadRequest, "invalid restriction")
		return
	}
	if !s.requireOwner(w, r, u, workID) {
		return
	}
	if err := s.store.SetCommentGating(r.Context(), workID, restriction); err != nil {
		plainError(w, http.StatusInternalServerError, "server error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// requireOwner is CLAUDE.md's verifyOwnership, answered on the wire.
func (s *Server) requireOwner(w http.ResponseWriter, r *http.Request, u *store.User, workID string) bool {
	if workID == "" {
		plainError(w, http.StatusBadRequest, "work_id required")
		return false
	}
	owner, _, err := s.store.WorkOwner(r.Context(), workID)
	if err != nil {
		plainError(w, http.StatusNotFound, "work not found")
		return false
	}
	if owner != u.ID {
		plainError(w, http.StatusForbidden, "forbidden")
		return false
	}
	return true
}

func (s *Server) pinEvent(w http.ResponseWriter, r *http.Request, u *store.User, body *eventBody) {
	workID := body.get("post_id")
	if workID == "" {
		workID = body.get("work_id")
	}
	if strings.HasPrefix(body.eventType, "unpin") {
		if err := s.store.SetPinnedWork(r.Context(), u.ID, ""); err != nil {
			plainError(w, http.StatusInternalServerError, "server error")
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !s.requireOwner(w, r, u, workID) {
		return
	}
	if err := s.store.SetPinnedWork(r.Context(), u.ID, workID); err != nil {
		plainError(w, http.StatusInternalServerError, "server error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) reportEvent(w http.ResponseWriter, r *http.Request, u *store.User, body *eventBody) {
	workID := body.get("post_id")
	if workID == "" {
		workID = body.get("work_id")
	}
	reason := body.get("reason")
	switch reason {
	case "spam", "hate", "nsfw", "other", "harassment", "violence", "illegal":
	default:
		plainError(w, http.StatusBadRequest, "reason required")
		return
	}
	targetHandle := body.get("target_handle")
	if workID == "" && targetHandle == "" {
		plainError(w, http.StatusBadRequest, "post_id or target_handle required")
		return
	}
	if err := s.store.InsertReport(r.Context(), u.ID, workID, targetHandle, reason); err != nil {
		plainError(w, http.StatusInternalServerError, "server error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// pollVoteEvent — {event_type:"poll_vote", work_id, option_idx}. A vote on a
// work that is not a poll, an option out of range, or a closed poll is 400.
func (s *Server) pollVoteEvent(w http.ResponseWriter, r *http.Request, u *store.User, body *eventBody) {
	workID := body.get("work_id")
	idx, err := strconv.Atoi(body.get("option_idx"))
	if workID == "" || err != nil {
		plainError(w, http.StatusBadRequest, "work_id and option_idx required")
		return
	}
	if err := s.store.CastPollVote(r.Context(), workID, u.ID, idx); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			plainError(w, http.StatusNotFound, "work not found")
			return
		}
		plainError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// notifReadEvent — marks one notification (and the group it heads) read.
func (s *Server) notifReadEvent(w http.ResponseWriter, r *http.Request, u *store.User, body *eventBody) {
	id := body.get("notif_id")
	if id == "" {
		plainError(w, http.StatusBadRequest, "notif_id required")
		return
	}
	ids, err := s.store.GroupMembers(r.Context(), u.ID, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		plainError(w, http.StatusInternalServerError, "server error")
		return
	}
	if err := s.store.MarkNotificationsRead(r.Context(), u.ID, ids); err != nil {
		plainError(w, http.StatusInternalServerError, "server error")
		return
	}
	// The badge on every one of this user's devices follows the read.
	s.signalNotify(r, u.ID)
	w.WriteHeader(http.StatusNoContent)
}
