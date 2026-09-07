package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"f33d3r.com/ios/devserver/internal/store"
)

// ── Follow lists ──────────────────────────────────────────────────────────────

func (s *Server) followers(w http.ResponseWriter, r *http.Request, viewer *store.User) {
	s.userList(w, r, viewer, s.store.Followers)
}

func (s *Server) following(w http.ResponseWriter, r *http.Request, viewer *store.User) {
	s.userList(w, r, viewer, s.store.Following)
}

func (s *Server) userList(w http.ResponseWriter, r *http.Request, viewer *store.User,
	load func(context.Context, string, int, string) ([]*store.User, string, error)) {
	target, err := s.store.GetUserByHandle(r.Context(), r.PathValue("handle"))
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No one by that handle.")
		return
	}
	users, next, err := load(r.Context(), target.ID, limitParam(r, defaultPageSize, maxPageSize), r.URL.Query().Get("cursor"))
	if err != nil {
		serverError(w, err)
		return
	}
	ids := make([]string, 0, len(users))
	out := UserPageDTO{Users: make([]UserDTO, 0, len(users)), NextCursor: next, ViewerFollows: []string{}}
	for _, u := range users {
		ids = append(ids, u.ID)
		out.Users = append(out.Users, userDTO(u))
	}
	followed, _ := s.store.FollowStateFor(r.Context(), viewer.ID, ids)
	for _, u := range users {
		if followed[u.ID] {
			out.ViewerFollows = append(out.ViewerFollows, u.Handle)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// trendingTags — GET /api/v1/tags/trending
func (s *Server) trendingTags(w http.ResponseWriter, r *http.Request, _ *store.User) {
	tags, err := s.store.TrendingTags(r.Context(), 12)
	if err != nil {
		serverError(w, err)
		return
	}
	out := make([]TagDTO, 0, len(tags))
	for _, t := range tags {
		out = append(out, TagDTO{Tag: t.Tag, Count: t.Count})
	}
	writeJSON(w, http.StatusOK, map[string]any{"tags": out})
}

// ── Profile and settings ──────────────────────────────────────────────────────

// profileUpdateEvent — {event_type:"profile_update", display_name, bio, …}.
// Only the fields present change. Answers the fresh MeDTO.
func (s *Server) profileUpdateEvent(w http.ResponseWriter, r *http.Request, u *store.User, body *eventBody) {
	var upd store.ProfileUpdate
	pick := func(key string) *string {
		if body.raw != nil {
			if _, ok := body.raw[key]; !ok {
				return nil
			}
		} else if _, ok := body.form[key]; !ok {
			return nil
		}
		v := body.get(key)
		return &v
	}
	upd.DisplayName = pick("display_name")
	upd.Bio = pick("bio")
	upd.Pronouns = pick("pronouns")
	upd.Location = pick("location")
	upd.Website = pick("website")
	upd.AccentHex = pick("accent_hex")
	upd.ThemeID = pick("theme_id")
	upd.AvatarURL = pick("avatar_url")
	upd.HeaderURL = pick("header_url")
	upd.BirthdayMdVisibility = pick("birthday_md_visibility")
	upd.BirthdayYearVisibility = pick("birthday_year_visibility")
	upd.CountryCode = pick("country_code")
	if v, ok := body.raw["is_adult_creator"]; ok {
		var b bool
		if json.Unmarshal(v, &b) != nil {
			plainError(w, http.StatusBadRequest, "is_adult_creator must be true or false")
			return
		}
		upd.IsAdultCreator = &b
	} else if v, ok := body.form["is_adult_creator"]; ok {
		b := v == "true" || v == "1" || v == "on"
		upd.IsAdultCreator = &b
	}
	for _, f := range []struct {
		key  string
		into **map[string]string
	}{{"social_links", &upd.SocialLinks}, {"external_tip_links", &upd.ExternalTipLinks}} {
		v, ok := body.raw[f.key]
		if !ok {
			continue
		}
		var m map[string]string
		if json.Unmarshal(v, &m) != nil {
			plainError(w, http.StatusBadRequest, f.key+" must be an object of strings")
			return
		}
		if m == nil {
			m = map[string]string{}
		}
		*f.into = &m
	}
	for _, p := range []*string{upd.AvatarURL, upd.HeaderURL} {
		if p != nil && *p != "" && !strings.HasPrefix(*p, "/media/") {
			plainError(w, http.StatusBadRequest, "media paths must be uploads (/media/…)")
			return
		}
	}
	if err := s.store.UpdateProfile(r.Context(), u.ID, upd); err != nil {
		plainError(w, http.StatusBadRequest, err.Error())
		return
	}
	fresh, err := s.store.GetUserByID(r.Context(), u.ID)
	if err != nil {
		serverError(w, err)
		return
	}
	unread, _ := s.store.UnreadCount(r.Context(), u.ID)
	writeJSON(w, http.StatusOK, meDTO(fresh, unread))
}

// settingsEvent — the web's settings.privacy.* events, `value` carried.
func (s *Server) settingsEvent(w http.ResponseWriter, r *http.Request, u *store.User, body *eventBody) {
	value := body.get("value")
	on := value != "" && value != "0" && value != "false" && value != "off"
	var err error
	switch body.eventType {
	case "settings.privacy.show_sensitive":
		err = s.store.SetShowSensitive(r.Context(), u.ID, on)
	case "settings.privacy.celebrations":
		err = s.store.SetCelebrations(r.Context(), u.ID, on)
	case "settings.privacy.account_private":
		err = s.store.SetPrivate(r.Context(), u.ID, on)
	case "settings.privacy.content_setting":
		if value == "adult_enabled" && !u.IsAdult {
			plainError(w, http.StatusForbidden, "Your account is not cleared for adult content.")
			return
		}
		err = s.store.SetContentSetting(r.Context(), u.ID, value)
	}
	if err != nil {
		plainError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) passwordChangeEvent(w http.ResponseWriter, r *http.Request, u *store.User, body *eventBody) {
	if err := s.store.ChangePassword(r.Context(), u.ID, body.get("current_password"), body.get("new_password")); err != nil {
		plainError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── Sessions ──────────────────────────────────────────────────────────────────

func (s *Server) sessions(w http.ResponseWriter, r *http.Request, u *store.User) {
	list, err := s.store.Sessions(r.Context(), u.ID, bearerToken(r))
	if err != nil {
		serverError(w, err)
		return
	}
	out := make([]SessionInfoDTO, 0, len(list))
	for _, si := range list {
		out = append(out, SessionInfoDTO{ID: si.ID, DeviceName: si.DeviceName, IPAddress: si.IPAddress, CreatedAt: si.CreatedAt, LastSeenAt: si.LastSeenAt, IsCurrent: si.IsCurrent})
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": out})
}

func (s *Server) sessionRevokeEvent(w http.ResponseWriter, r *http.Request, u *store.User, body *eventBody) {
	if err := s.store.RevokeSession(r.Context(), u.ID, body.get("session_id")); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			plainError(w, http.StatusNotFound, "session not found")
			return
		}
		plainError(w, http.StatusInternalServerError, "server error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── Editing ───────────────────────────────────────────────────────────────────

// workEditEvent — {event_type:"work_edit", work_id, body}. Owner, 60 minutes.
func (s *Server) workEditEvent(w http.ResponseWriter, r *http.Request, u *store.User, body *eventBody) {
	if err := s.store.EditWork(r.Context(), body.get("work_id"), u.ID, body.get("body")); err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			plainError(w, http.StatusNotFound, "work not found")
		case errors.Is(err, store.ErrEditWindowClosed):
			plainError(w, http.StatusConflict, err.Error())
		case err.Error() == "forbidden":
			plainError(w, http.StatusForbidden, "forbidden")
		default:
			plainError(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── The user's own event stream ───────────────────────────────────────────────

func userTopic(userID string) string { return "user:" + userID }

// signalNotify pushes the recipient's fresh unread count.
func (s *Server) signalNotify(r *http.Request, userID string) {
	n, err := s.store.UnreadCount(r.Context(), userID)
	if err != nil {
		return
	}
	s.hub.publish(userTopic(userID), "notify", map[string]int{"unread": n})
}

// signalBalance pushes a wallet balance to its owner.
func (s *Server) signalBalance(r *http.Request, u *store.User) {
	bal, err := s.store.Balance(r.Context(), u.PIALID)
	if err != nil {
		return
	}
	s.hub.publish(userTopic(u.ID), "balance", map[string]int64{"balance_uaet": bal.SettledUAET, "pending_uaet": bal.PendingUAET})
}

// userEvents — GET /api/v1/events: the signed-in user's stream. Signal frames
// only — `notify {unread}`, `new_post {work_id, author_handle}`,
// `post_deleted {work_id}`, `balance {balance_uaet, pending_uaet}` — the app
// refetches what it needs. This is the plan's A5, without HTML.
func (s *Server) userEvents(w http.ResponseWriter, r *http.Request, u *store.User) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "no_stream", "SSE not supported.")
		return
	}
	sub, cancel := s.hub.subscribe(userTopic(u.ID), u.ID)
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, ": connected\n\n")
	flusher.Flush()

	// Open with the current truth so a reconnect never shows a stale badge.
	s.signalNotify(r, u.ID)
	s.signalBalance(r, u)

	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			fmt.Fprintf(w, ": ping\n\n")
			flusher.Flush()
		case f := <-sub.ch:
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", f.Event, f.Data)
			flusher.Flush()
		}
	}
}
