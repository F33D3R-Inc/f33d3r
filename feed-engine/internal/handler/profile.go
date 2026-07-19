package handler

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"

	"github.com/google/uuid"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
)

func (h *Handler) profilePage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	var achievements []model.Achievement
	var subCount int
	var pinnedPostID string
	if h.db != nil {
		achievements, _ = dbpkg.GetEarnedAchievements(h.db, user.ID)
		if user.IsCreator {
			subCount = dbpkg.GetSubscriberCount(h.db, user.ID)
		}
		user.OrgMemberships = dbpkg.GetUserOrgMemberships(h.db, user.ID)
		pinnedPostID = dbpkg.GetPinnedPostID(h.db, user.ID)
	}
	rail := h.railData(user, "profile")
	data := h.baseData(user)
	data["ProfileUser"]      = user
	data["ProfileIsOnline"]  = h.isHandleOnline(user.Handle)
	data["IsOwner"]          = true
	data["Title"]           = "@" + user.Handle
	data["Achievements"]    = achievements
	data["SubscriberCount"] = subCount
	data["PinnedPostID"]    = pinnedPostID
	data["TrendingTags"]    = rail["TrendingTags"]
	data["SuggestedUsers"]  = rail["SuggestedUsers"]
	data["RailContext"]     = rail["RailContext"]
	data["RailCreators"]    = rail["RailCreators"]
	data["RailNewsItems"]   = rail["RailNewsItems"]
	data["RailNewsLabel"]   = rail["RailNewsLabel"]
	h.render(w, "profile.html", data)
}

func (h *Handler) userProfilePage(w http.ResponseWriter, r *http.Request) {
	viewerUser   := h.userFromRequest(w, r)
	targetHandle := r.PathValue("handle")

	var profileUser *model.User
	if h.db != nil {
		profileUser, _ = dbpkg.GetUserByHandle(h.db, targetHandle)
	}
	if profileUser == nil {
		http.NotFound(w, r)
		return
	}

	// Anonymous (logged-out) visitor — userFromRequest returns the safe_mode DemoUser
	// sentinel. The profile page ALWAYS renders (avatar, banner, bio, counts) just like
	// X.com / OnlyFans; only the works feed is walled behind sign-up/sign-in below.
	anon := viewerUser == nil || viewerUser.ID == "demo_user"

	isOwner := !anon && profileUser.ID == viewerUser.ID

	// Follower relationship — only meaningful for an authenticated, non-owner viewer.
	isFollowing  := false
	isSubscribed := false
	if h.db != nil && !anon && !isOwner {
		isFollowing, _ = dbpkg.IsFollowing(h.db, viewerUser.ID, profileUser.ID)
		isSubscribed   = dbpkg.IsSubscribed(h.db, viewerUser.ID, profileUser.ID)
	}

	// ── Feed-area wall ──────────────────────────────────────────────────────────
	// The profile header always renders. Only the works feed swaps to a wall:
	//   privateWall   → account is private and viewer isn't owner/approved follower.
	//   adultGate      → adult-creator works gated for this viewer:
	//       "auth"   → anonymous visitor: sign up / log in to view (no NSFW to public).
	//       "verify" → 18+ but unverified (must KYC, US law).
	//       "enable" → 18+ verified but not adult_enabled.
	privateWall := profileUser.IsPrivate && !isOwner && !isFollowing

	adultGate := ""
	if !privateWall && profileUser.IsAdultCreator && !isOwner {
		switch {
		case anon:
			adultGate = "auth"
		case excludeNSFW(viewerUser):
			// Authenticated minor / safe_mode / gov / business — adult works are
			// never shown. Header still renders; feed area is walled (no 403/404).
			adultGate = "restricted"
		case !(viewerUser.IsAgeVerified || viewerUser.IsVerified):
			adultGate = "verify"
		case viewerUser.ContentSetting != "adult_enabled":
			adultGate = "enable"
		}
	}

	var achievements []model.Achievement
	var subCount int
	var pinnedPostID string
	if h.db != nil {
		if !isOwner {
			// Count profile views for Known Terrorist achievement (fire-and-forget, don't block render)
			go dbpkg.TryAwardTrollOnProfileView(h.db, profileUser.ID)
		}
		achievements, _ = dbpkg.GetEarnedAchievements(h.db, profileUser.ID)
		if profileUser.IsCreator {
			subCount = dbpkg.GetSubscriberCount(h.db, profileUser.ID)
		}
		profileUser.OrgMemberships = dbpkg.GetUserOrgMemberships(h.db, profileUser.ID)
		pinnedPostID = dbpkg.GetPinnedPostID(h.db, profileUser.ID)
	}

	rail := h.railData(viewerUser, "profile")
	data := h.baseData(viewerUser)
	data["ProfileUser"]      = profileUser
	data["ProfileIsOnline"]  = h.isHandleOnline(profileUser.Handle)
	data["IsOwner"]          = isOwner
	data["IsAnon"]          = anon
	data["PrivateWall"]     = privateWall
	data["AdultGate"]       = adultGate
	data["IsFollowing"]     = isFollowing
	data["IsSubscribed"]    = isSubscribed
	data["Title"]           = "@" + profileUser.Handle
	data["Achievements"]    = achievements
	data["SubscriberCount"] = subCount
	data["PinnedPostID"]    = pinnedPostID
	data["TrendingTags"]    = rail["TrendingTags"]
	data["SuggestedUsers"]  = rail["SuggestedUsers"]
	data["RailContext"]     = rail["RailContext"]
	data["RailCreators"]    = rail["RailCreators"]
	data["RailNewsItems"]   = rail["RailNewsItems"]
	data["RailNewsLabel"]   = rail["RailNewsLabel"]
	h.render(w, "profile.html", data)
}

func (h *Handler) followersPage(w http.ResponseWriter, r *http.Request) {
	viewer       := h.userFromRequest(w, r)
	targetHandle := r.PathValue("handle")
	var targetUser *model.User
	if h.db != nil {
		targetUser, _ = dbpkg.GetUserByHandle(h.db, targetHandle)
	}
	if targetUser == nil {
		http.NotFound(w, r)
		return
	}
	var users []dbpkg.FollowListEntry
	if h.db != nil {
		users, _ = dbpkg.GetFollowers(h.db, targetUser.ID, viewer.ID, 200)
	}
	h.render(w, "followers.html", map[string]interface{}{
		"User":          viewer,
		"ProfileHandle": targetHandle,
		"Users":         users,
		"Title":         "Followers · @" + targetHandle,
		"Themes":        ThemesWithActive(viewer.ThemeID),
	})
}

func (h *Handler) followingPage(w http.ResponseWriter, r *http.Request) {
	viewer       := h.userFromRequest(w, r)
	targetHandle := r.PathValue("handle")
	var targetUser *model.User
	if h.db != nil {
		targetUser, _ = dbpkg.GetUserByHandle(h.db, targetHandle)
	}
	if targetUser == nil {
		http.NotFound(w, r)
		return
	}
	var users []dbpkg.FollowListEntry
	if h.db != nil {
		users, _ = dbpkg.GetFollowing(h.db, targetUser.ID, viewer.ID, 200)
	}
	h.render(w, "following.html", map[string]interface{}{
		"User":          viewer,
		"ProfileHandle": targetHandle,
		"Users":         users,
		"Title":         "Following · @" + targetHandle,
		"Themes":        ThemesWithActive(viewer.ThemeID),
	})
}

func (h *Handler) followListPartial(w http.ResponseWriter, r *http.Request) {
	targetHandle := r.URL.Query().Get("handle")
	listType     := r.URL.Query().Get("type")
	if targetHandle == "" || (listType != "followers" && listType != "following") {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var targetUser *model.User
	if h.db != nil {
		targetUser, _ = dbpkg.GetUserByHandle(h.db, targetHandle)
	}
	if targetUser == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	viewer := h.userFromRequest(w, r)
	var users []dbpkg.FollowListEntry
	if h.db != nil {
		if listType == "followers" {
			users, _ = dbpkg.GetFollowers(h.db, targetUser.ID, viewer.ID, 200)
		} else {
			users, _ = dbpkg.GetFollowing(h.db, targetUser.ID, viewer.ID, 200)
		}
	}
	title := "Followers"
	if listType == "following" {
		title = "Following"
	}
	h.renderPartial(w, "follow_list_modal", map[string]interface{}{
		"Users":       users,
		"Title":       title,
		"ListType":    listType,
		"ViewerHandle": viewer.Handle,
	})
}

func (h *Handler) settingsPage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)

	// Section from URL: /settings → empty panel, /settings/security → "security"
	section := r.PathValue("section")

	data := h.settingsSectionData(user, section, r)
	data["ActiveSection"] = section
	data["Title"] = "Settings"
	h.render(w, "settings.html", data)
}

// settingsSection serves GET /partials/settings/{section} for HTMX panel swaps.
func (h *Handler) settingsSection(w http.ResponseWriter, r *http.Request) {
	user    := h.userFromRequest(w, r)
	section := r.PathValue("section")
	// danger_confirm is a sub-panel of danger — use the same base data but a different template.
	templateName := "settings_section_" + section
	if section == "danger_confirm" {
		section = "danger"
		templateName = "settings_section_danger_confirm"
	}
	data := h.settingsSectionData(user, section, r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderPartial(w, templateName, data)
}

// settingsSectionData builds the data map for any settings section.
func (h *Handler) settingsSectionData(user *model.User, section string, r *http.Request) map[string]interface{} {
	data := h.baseData(user)
	data["SaveSuccess"] = r.URL.Query().Get("saved") == "1"

	socialLinks := map[string]string{}
	if user != nil && user.SocialLinksRaw != "" {
		_ = json.Unmarshal([]byte(user.SocialLinksRaw), &socialLinks)
	}
	data["SocialLinks"] = socialLinks
	data["Themes"]      = ThemesWithActive(func() string {
		if user != nil { return user.ThemeID }
		return ""
	}())

	birthdayStr := ""
	if user != nil && user.Birthday != nil {
		birthdayStr = user.Birthday.Format("2006-01-02")
	}
	data["BirthdayStr"] = birthdayStr

	if h.db != nil && user != nil {
		data["HasPassword"] = dbpkg.HasPassword(h.db, user.ID)
		_, totpEnabled := dbpkg.GetTOTPSecret(h.db, user.ID)
		user.TwoFAEnabled = totpEnabled
	}

	// Notification preferences — fetched from Herald for that section only.
	if section == "notifications" && h.cfg.HeraldURL != "" && user != nil && user.PIALID != "" {
		data["NotifPrefs"] = h.fetchNotifPrefs(user.PIALID)
	}
	// Org memberships — fetched for work/org settings section.
	if section == "org" && h.db != nil && user != nil {
		data["OrgMemberships"] = dbpkg.GetUserOrgMemberships(h.db, user.ID)
	}

	return data
}

// fetchNotifPrefs calls Herald GET /v1/preferences/{pial} and returns a flat map
// so templates don't need to know the Herald struct layout.
func (h *Handler) fetchNotifPrefs(pialID string) map[string]interface{} {
	defaults := map[string]interface{}{
		"push_enabled": true, "messages_enabled": true, "likes_enabled": true,
		"reposts_enabled": true, "replies_enabled": true, "follows_enabled": true,
		"achievements_enabled": true, "mentions_enabled": true,
		"quiet_hours_enabled": false, "quiet_hours_start": 22, "quiet_hours_end": 8,
	}
	req, err := http.NewRequest(http.MethodGet, h.cfg.HeraldURL+"/v1/preferences/"+pialID, nil)
	if err != nil { return defaults }
	resp, err := h.httpClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK { return defaults }
	defer resp.Body.Close()
	var out map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil { return defaults }
	return out
}

// saveNotificationPrefs handles POST /api/settings/notifications.
// Accepts JSON body matching Herald UpdatePreferencesRequest, proxies to Herald PATCH.
func (h *Handler) saveNotificationPrefs(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil || user.PIALID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if h.cfg.HeraldURL == "" {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPatch,
		h.cfg.HeraldURL+"/v1/preferences/"+user.PIALID,
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.httpClient.Do(req)
	if err != nil {
		log.Printf("[herald] save notif prefs: %v", err)
		http.Error(w, "herald unavailable", http.StatusServiceUnavailable)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// facetShareSheet renders the ShareSheet overlay facet for a work — a copyable
// canonical link plus the native Web Share trigger. Pushed into #overlay-slot by the
// work_action_bar share button (hx-get).
func (h *Handler) facetShareSheet(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("work_id"))
	if id == "" || h.db == nil {
		http.NotFound(w, r)
		return
	}
	viewer := h.userFromRequest(w, r)
	viewerID := ""
	if viewer != nil && viewer.ID != "demo_user" {
		viewerID = viewer.ID
	}
	work, err := dbpkg.GetWorkByID(h.db, id, viewerID)
	if err != nil || work == nil {
		http.NotFound(w, r)
		return
	}
	// Mirror the work-page wall: never leak a private/adult work's body into the
	// native-share text for a viewer who isn't allowed to see it.
	author, err := dbpkg.GetUserByHandle(h.db, work.AuthorHandle)
	if err != nil {
		log.Printf("[profile] author lookup %s: %v", work.AuthorHandle, err)
	}
	privateWall, workGate := h.workWall(work, author, viewer)
	shareText := truncate(work.Body, 100)
	if privateWall || workGate != "" {
		shareText = "@" + work.AuthorHandle + " on f33d3r"
	}
	data := map[string]interface{}{
		"ShareURL":  "https://f33d3r.com/" + work.AuthorHandle + "/work/" + id,
		"ShareText": shareText,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderPartial(w, "share_sheet", data)
}

func (h *Handler) facetEditProfileModal(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	birthdayStr := ""
	if user.Birthday != nil {
		birthdayStr = user.Birthday.Format("2006-01-02")
	}
	socialLinks := map[string]string{}
	if user.SocialLinksRaw != "" {
		_ = json.Unmarshal([]byte(user.SocialLinksRaw), &socialLinks)
	}
	extTipLinks := map[string]string{}
	if user.ExternalTipLinksRaw != "" {
		_ = json.Unmarshal([]byte(user.ExternalTipLinksRaw), &extTipLinks)
	}
	data := map[string]interface{}{
		"User":             user,
		"BirthdayStr":      birthdayStr,
		"SocialLinks":      socialLinks,
		"Themes":           ThemesWithActive(user.ThemeID),
		"ExternalTipLinks": extTipLinks,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderPartial(w, "edit_profile_modal", data)
}

func (h *Handler) saveProfile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { http.Error(w, "405", 405); return }
	if err := r.ParseMultipartForm(15 << 20); err != nil {
		_ = r.ParseForm()
	}

	user := h.userFromRequest(w, r)
	themeID := r.FormValue("theme_id")
	if !isValidTheme(themeID) { themeID = user.ThemeID }
	if !isValidTheme(themeID) { themeID = "void" }

	accentHex := r.FormValue("accent_hex")
	if !accentHexRe.MatchString(accentHex) { accentHex = "" }

	socialLinks := map[string]string{
		"soundcloud": r.FormValue("social_soundcloud"),
		"spotify":    r.FormValue("social_spotify"),
		"kick":       r.FormValue("social_kick"),
		"twitch":     r.FormValue("social_twitch"),
		"onlyfans":   r.FormValue("social_onlyfans"),
		"youtube":    r.FormValue("social_youtube"),
		"other":      r.FormValue("social_other"),
	}
	socialJSON := "{}"
	if b, err := json.Marshal(socialLinks); err == nil {
		socialJSON = string(b)
	}

	tipLinks := map[string]string{
		"cashapp":      normalizeTipHandle(r.FormValue("tip_cashapp"),      []string{"$", "cash.app/", "https://cash.app/"}),
		"venmo":        normalizeTipHandle(r.FormValue("tip_venmo"),        []string{"@", "venmo.com/", "https://venmo.com/"}),
		"paypal":       normalizeTipHandle(r.FormValue("tip_paypal"),       []string{"paypal.me/", "https://paypal.me/", "https://www.paypal.me/"}),
		"kofi":         normalizeTipHandle(r.FormValue("tip_kofi"),         []string{"ko-fi.com/", "https://ko-fi.com/"}),
		"buymeacoffee": normalizeTipHandle(r.FormValue("tip_buymeacoffee"), []string{"buymeacoffee.com/", "https://www.buymeacoffee.com/", "https://buymeacoffee.com/"}),
		"bitcoin":      normalizeCryptoAddress(r.FormValue("tip_bitcoin"), btcAddressRe),
		"xrp":          normalizeCryptoAddress(r.FormValue("tip_xrp"), xrpAddressRe),
	}
	tipLinksJSON := "{}"
	if b, err := json.Marshal(tipLinks); err == nil {
		tipLinksJSON = string(b)
	}

	// For text fields, fall back to the existing value when the field is absent
	// from the submission (e.g. partial form POSTs that only carry theme_id).
	formStr := func(key, fallback string, max int) string {
		if _, ok := r.Form[key]; ok {
			return truncate(r.FormValue(key), max)
		}
		return fallback
	}

	// is_adult_creator: full-form POST always includes the sentinel hidden input ("0"),
	// plus the checkbox value ("1") if checked. Check if "1" appears in the submitted values.
	// If the key is absent entirely (partial POST), preserve the existing value.
	isAdultCreator := user.IsAdultCreator
	if vals, ok := r.Form["is_adult_creator"]; ok {
		isAdultCreator = false
		for _, v := range vals {
			if v == "1" {
				isAdultCreator = true
				break
			}
		}
	}

	save := &model.ProfileSave{
		UserID:               user.ID,
		DisplayName:          formStr("display_name", user.DisplayName, 100),
		Bio:                  formStr("bio", user.Bio, 160),
		Pronouns:             formStr("pronouns", user.Pronouns, 50),
		Location:             formStr("location", user.Location, 100),
		CountryCode:          strings.ToUpper(truncate(r.FormValue("country_code"), 2)),
		Website:              formStr("website", user.Website, 200),
		ThemeID:              themeID,
		AccentHex:            accentHex,
		JungArchetype:        user.JungArchetype,
		PinnedTrackID:        truncate(r.FormValue("pinned_track_id"), 100),
		SocialLinksJSON:      socialJSON,
		ExternalTipLinksJSON: tipLinksJSON,
		IsAdultCreator:       isAdultCreator,
	}

	// Handle avatar upload — store via Caeor so it lands in the named volume (survives rebuilds).
	// Falls back to local disk if Caeor is unavailable.
	if avatarFile, avatarHeader, err := r.FormFile("avatar"); err == nil {
		defer avatarFile.Close()
		fileBytes, rerr := io.ReadAll(io.LimitReader(avatarFile, 10<<20))
		if rerr == nil {
			avatarURL, uerr := h.caeorUpload(fileBytes, avatarHeader.Filename, "avatar")
			if uerr != nil {
				log.Printf("[upload] avatar caeor failed, falling back: %v", uerr)
				avatarURL, uerr = h.saveUpload(bytes.NewReader(fileBytes), avatarHeader.Filename, "avatars")
			}
			if uerr == nil {
				save.AvatarURL = avatarURL
			} else {
				log.Printf("[upload] avatar error: %v", uerr)
			}
		}
	}

	// Handle header upload — same caeor-first strategy.
	if headerFile, headerHeader, err := r.FormFile("header"); err == nil {
		defer headerFile.Close()
		fileBytes, rerr := io.ReadAll(io.LimitReader(headerFile, 10<<20))
		if rerr == nil {
			headerURL, uerr := h.caeorUpload(fileBytes, headerHeader.Filename, "header")
			if uerr != nil {
				log.Printf("[upload] header caeor failed, falling back: %v", uerr)
				headerURL, uerr = h.saveUpload(bytes.NewReader(fileBytes), headerHeader.Filename, "headers")
			}
			if uerr == nil {
				save.HeaderURL = headerURL
			} else {
				log.Printf("[upload] header error: %v", uerr)
			}
		}
	}

	// Invalidate session cache so the redirect immediately shows fresh data.
	if tok := GetSessionToken(r); tok != "" {
		h.sessionCache.Delete(tok)
	}
	if handle := HandleFromCookie(r); handle != "" {
		h.sessionCache.Delete("__handle__" + handle)
	}

	if h.db != nil {
		// Award XP for profile fields being filled for the first time.
		xpGain := 0
		if user.Bio == "" && save.Bio != "" {
			xpGain += 50
		}
		if user.AvatarURL == "" && save.AvatarURL != "" {
			xpGain += 75
		}
		if user.Website == "" && save.Website != "" {
			xpGain += 25
		}
		// Social links: award if at least one link was newly added.
		if user.SocialLinksRaw == "{}" || user.SocialLinksRaw == "" {
			var newLinks map[string]string
			if json.Unmarshal([]byte(save.SocialLinksJSON), &newLinks) == nil {
				for _, v := range newLinks {
					if v != "" {
						xpGain += 30
						break
					}
				}
			}
		}
		if err := dbpkg.SaveProfile(h.db, save); err != nil {
			log.Printf("[profile] save error: %v", err)
			http.Error(w, "could not save profile", http.StatusInternalServerError)
			return
		}
		if xpGain > 0 {
			dbpkg.AwardXP(h.db, user.ID, "profile_completion", xpGain)
		}

		// Birthday date is owned by PIAL — not writable from profile edit.
		// Only visibility preferences are user-editable.
		showBirthday := r.FormValue("show_birthday") == "1"
		mdVis   := r.FormValue("birthday_md_visibility")
		yearVis := r.FormValue("birthday_year_visibility")
		if err := dbpkg.SetBirthdayVisibility(h.db, user.ID, showBirthday, mdVis, yearVis); err != nil {
			log.Printf("[profile] birthday visibility save error: %v", err)
		}
	}
	returnTo := r.FormValue("return_to")
	if returnTo == "" || !strings.HasPrefix(returnTo, "/") {
		returnTo = "/" + user.Handle
	}
	http.Redirect(w, r, returnTo, http.StatusSeeOther)
}

// changePasswordAPI handles POST /api/settings/password.
// Accepts current_password (if one is set), new_password, confirm_password.
// Returns a small HTML fragment for HTMX swap.
func (h *Handler) changePasswordAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	_ = r.ParseForm()
	user := h.userFromRequest(w, r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	if h.db == nil {
		w.Write([]byte(`<span style="color:#ef4444;font-size:13px">Database unavailable.</span>`))
		return
	}

	currentPw  := r.FormValue("current_password")
	newPw      := r.FormValue("new_password")
	confirmPw  := r.FormValue("confirm_password")

	if newPw == "" || len(newPw) < 8 {
		w.Write([]byte(`<span style="color:#ef4444;font-size:13px">New password must be at least 8 characters.</span>`))
		return
	}
	if newPw != confirmPw {
		w.Write([]byte(`<span style="color:#ef4444;font-size:13px">Passwords do not match.</span>`))
		return
	}

	// If the user already has a password, verify the current one first.
	if dbpkg.HasPassword(h.db, user.ID) {
		if currentPw == "" {
			w.Write([]byte(`<span style="color:#ef4444;font-size:13px">Please enter your current password.</span>`))
			return
		}
		confirmedUID, err := dbpkg.CheckPassword(h.db, user.Handle, currentPw)
		if err != nil || confirmedUID == "" {
			w.Write([]byte(`<span style="color:#ef4444;font-size:13px">Current password is incorrect.</span>`))
			return
		}
	}

	if err := dbpkg.SetPassword(h.db, user.ID, newPw); err != nil {
		log.Printf("[settings] SetPassword error: %v", err)
		w.Write([]byte(`<span style="color:#ef4444;font-size:13px">Could not save password — try again.</span>`))
		return
	}

	w.Write([]byte(`<span style="color:#22c55e;font-size:13px;font-weight:500">Password updated successfully.</span>`))
}

func (h *Handler) bookmarksPage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	filter := r.URL.Query().Get("filter")

	works := h.fetchBookmarkWorks(user, filter)
	works = filterAdultForViewer(user, works)
	trending, suggested := h.sidebarData(user)
	h.render(w, "bookmarks.html", map[string]interface{}{
		"User":              user,
		"Title":             "Bookmarks",
		"SessionID":         uuid.New().String(),
		"Surface":           "bookmarks",
		"ShowScores":        false,
		"CurrentUserHandle": user.Handle,
		"Works":             works,
		"Filter":            filter,
		"Themes":            ThemesWithActive(user.ThemeID),
		"TrendingTags":      trending,
		"SuggestedUsers":    suggested,
	})
}

// bookmarksItemsPartial serves GET /bookmarks/items?filter= — HTMX-swappable work list.
func (h *Handler) bookmarksItemsPartial(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	filter := r.URL.Query().Get("filter")
	works := h.fetchBookmarkWorks(user, filter)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if len(works) == 0 {
		h.renderPartial(w, "stateEmpty", map[string]interface{}{
			"Title": "No bookmarks yet",
			"Sub":   "Works you save will appear here",
		})
		return
	}
	ctx := map[string]interface{}{"CurrentUserHandle": user.Handle}
	for _, wk := range works {
		h.renderPartial(w, "work_card", map[string]interface{}{"W": wk, "Ctx": ctx})
	}
}

func (h *Handler) fetchBookmarkWorks(user *model.User, filter string) []*model.Work {
	if h.db == nil || user == nil {
		return nil
	}
	works, err := dbpkg.GetWorksSavesFiltered(h.db, user.ID, h.cfg.FeedPageSize, "", filter)
	if err != nil {
		return nil
	}
	dbpkg.EnrichWorksWithReactions(h.db, works, user.ID)
	dbpkg.EnrichWorksWithQuotes(h.db, works)
	dbpkg.EnrichWorksWithLinkPreviews(h.db, works)
	return works
}

// ── Vault recovery ────────────────────────────────────────────────────────────
// Stores and retrieves a server-side encrypted private key blob.
// The server never holds the plaintext — blob is PBKDF2(recovery_pin) wrapped.

func (h *Handler) vaultRecoveryGet(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil || user.ID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var blobB64, saltB64 string
	err := h.db.QueryRow(
		`SELECT blob_b64, salt_b64 FROM vault_recovery WHERE user_id = $1`,
		user.ID,
	).Scan(&blobB64, &saltB64)
	if err == sql.ErrNoRows {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"not_found"}`))
		return
	}
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"blob_b64": blobB64,
		"salt_b64": saltB64,
	})
}

func (h *Handler) vaultRecoverySet(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil || user.ID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req struct {
		BlobB64 string `json:"blob_b64"`
		SaltB64 string `json:"salt_b64"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 64*1024))
	if err != nil || len(body) == 0 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := json.Unmarshal(body, &req); err != nil || req.BlobB64 == "" || req.SaltB64 == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	_, err = h.db.Exec(
		`INSERT INTO vault_recovery (user_id, blob_b64, salt_b64, updated_at)
		 VALUES ($1, $2, $3, NOW())
		 ON CONFLICT (user_id) DO UPDATE SET blob_b64=$2, salt_b64=$3, updated_at=NOW()`,
		user.ID, req.BlobB64, req.SaltB64,
	)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}

// tipHandleRe validates a normalized tip username: alphanumeric, dash, underscore, dot.
var tipHandleRe = regexp.MustCompile(`^[a-zA-Z0-9\-_.]{1,50}$`)

// Bitcoin: loose check for legacy (1/3) and bech32 (bc1) addresses.
var btcAddressRe = regexp.MustCompile(`^(1|3)[a-zA-Z0-9]{25,33}$|^bc1[a-zA-Z0-9]{6,90}$`)

// XRP: r-addresses are 25–34 base58 characters.
var xrpAddressRe = regexp.MustCompile(`^r[1-9A-HJ-NP-Za-km-z]{24,33}$`)

// normalizeTipHandle strips known URL and sigil prefixes, then validates.
func normalizeTipHandle(raw string, prefixes []string) string {
	h := strings.TrimSpace(raw)
	if h == "" {
		return ""
	}
	for _, p := range prefixes {
		if strings.HasPrefix(strings.ToLower(h), strings.ToLower(p)) {
			h = h[len(p):]
			break
		}
	}
	h = strings.TrimSpace(h)
	if !tipHandleRe.MatchString(h) {
		return ""
	}
	return h
}

// normalizeCryptoAddress trims whitespace and validates against the given regex.
func normalizeCryptoAddress(raw string, re *regexp.Regexp) string {
	h := strings.TrimSpace(raw)
	if h == "" || !re.MatchString(h) {
		return ""
	}
	return h
}

// tipExternalLink is one external payment option shown in the tip modal.
type tipExternalLink struct {
	Provider      string
	Label         string
	DisplayHandle string // shown in UI (may be truncated for crypto)
	FullHandle    string // full value, used by copy-to-clipboard for crypto
	URL           string // empty for crypto addresses (copy only)
	IsCrypto      bool   // true → show copy button instead of link
}

// buildTipExternalLinks converts raw JSON to ordered link list, skipping empty entries.
func buildTipExternalLinks(rawJSON string) []tipExternalLink {
	m := map[string]string{}
	if rawJSON != "" {
		_ = json.Unmarshal([]byte(rawJSON), &m)
	}

	var links []tipExternalLink

	// Standard payment providers — open external URL
	paymentProviders := []struct{ key, label, prefix, base string }{
		{"cashapp",      "Cash App",       "$",                  "https://cash.app/$"},
		{"venmo",        "Venmo",          "@",                  "https://venmo.com/"},
		{"paypal",       "PayPal",         "paypal.me/",         "https://paypal.me/"},
		{"kofi",         "Ko-fi",          "ko-fi.com/",         "https://ko-fi.com/"},
		{"buymeacoffee", "Buy Me a Coffee", "buymeacoffee.com/",  "https://buymeacoffee.com/"},
	}
	for _, p := range paymentProviders {
		h := strings.TrimSpace(m[p.key])
		if h == "" { continue }
		links = append(links, tipExternalLink{
			Provider:      p.key,
			Label:         p.label,
			DisplayHandle: p.prefix + h,
			FullHandle:    h,
			URL:           p.base + h,
		})
	}

	// Crypto addresses — copy to clipboard
	cryptoProviders := []struct{ key, label string }{
		{"bitcoin", "Bitcoin (BTC)"},
		{"xrp",     "XRP"},
	}
	for _, p := range cryptoProviders {
		h := strings.TrimSpace(m[p.key])
		if h == "" { continue }
		display := h
		if len(h) > 16 {
			display = h[:8] + "…" + h[len(h)-6:]
		}
		links = append(links, tipExternalLink{
			Provider:      p.key,
			Label:         p.label,
			DisplayHandle: display,
			FullHandle:    h,
			IsCrypto:      true,
		})
	}

	return links
}

// facetTipExternalLinks — GET /facets/tip_external_links?handle=xxx
// Returns the external payment links fragment for the target user's tip modal.
func (h *Handler) facetTipExternalLinks(w http.ResponseWriter, r *http.Request) {
	targetHandle := strings.TrimPrefix(r.URL.Query().Get("handle"), "@")
	if targetHandle == "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		return
	}
	var rawJSON string
	if h.db != nil {
		var targetUser *model.User
		targetUser, _ = dbpkg.GetUserByHandle(h.db, targetHandle)
		if targetUser != nil {
			rawJSON = targetUser.ExternalTipLinksRaw
		}
	}
	links := buildTipExternalLinks(rawJSON)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderPartial(w, "tip_external_links", map[string]interface{}{
		"Links": links,
	})
}
