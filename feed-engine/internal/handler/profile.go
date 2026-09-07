package handler

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
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
	data := h.baseData(user)
	data["ProfileUser"] = user
	data["ProfileIsOnline"] = h.isHandleOnline(user.Handle)
	data["IsOwner"] = true
	data["Title"] = "@" + user.Handle
	data["Achievements"] = achievements
	data["SubscriberCount"] = subCount
	data["PinnedPostID"] = pinnedPostID
	// Own profile: nothing is walled from its owner, and a broadcaster is not a
	// viewer of their own broadcast, so the hero renders without a presence beat.
	data["LiveStage"] = h.profileLiveStage(r, user, user, true, false)
	h.render(w, r, "profile.html", h.withRail(data, user, "profile"))
}

func (h *Handler) userProfilePage(w http.ResponseWriter, r *http.Request) {
	viewerUser := h.userFromRequest(w, r)
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
	isFollowing := false
	isSubscribed := false
	if h.db != nil && !anon && !isOwner {
		isFollowing, _ = dbpkg.IsFollowing(h.db, viewerUser.ID, profileUser.ID)
		isSubscribed = dbpkg.IsSubscribed(h.db, viewerUser.ID, profileUser.ID)
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

	data := h.baseData(viewerUser)
	data["ProfileUser"] = profileUser
	data["ProfileIsOnline"] = h.isHandleOnline(profileUser.Handle)
	data["IsOwner"] = isOwner
	data["IsAnon"] = anon
	data["PrivateWall"] = privateWall
	data["AdultGate"] = adultGate
	data["IsFollowing"] = isFollowing
	data["IsSubscribed"] = isSubscribed
	data["Title"] = "@" + profileUser.Handle
	data["Achievements"] = achievements
	data["SubscriberCount"] = subCount
	data["PinnedPostID"] = pinnedPostID
	// The live hero carries the same author's content as the works below it, so
	// it is walled by the same two walls: a private account this viewer does not
	// follow, and an adult creator this viewer is not cleared for.
	data["LiveStage"] = h.profileLiveStage(r, profileUser, viewerUser, isOwner, privateWall || adultGate != "")
	h.render(w, r, "profile.html", h.withRail(data, viewerUser, "profile"))
}

func (h *Handler) followersPage(w http.ResponseWriter, r *http.Request) {
	viewer := h.userFromRequest(w, r)
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
	h.render(w, r, "followers.html", map[string]interface{}{
		"User":          viewer,
		"ProfileHandle": targetHandle,
		"Users":         users,
		"Title":         "Followers · @" + targetHandle,
		"Themes":        ThemesWithActive(viewer.ThemeID),
	})
}

func (h *Handler) followingPage(w http.ResponseWriter, r *http.Request) {
	viewer := h.userFromRequest(w, r)
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
	h.render(w, r, "following.html", map[string]interface{}{
		"User":          viewer,
		"ProfileHandle": targetHandle,
		"Users":         users,
		"Title":         "Following · @" + targetHandle,
		"Themes":        ThemesWithActive(viewer.ThemeID),
	})
}

func (h *Handler) followListPartial(w http.ResponseWriter, r *http.Request) {
	targetHandle := r.URL.Query().Get("handle")
	listType := r.URL.Query().Get("type")
	if targetHandle == "" || (listType != "followers" && listType != "following" && listType != "subscribers") {
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
	title := "Followers"
	if h.db != nil {
		switch listType {
		case "followers":
			users, _ = dbpkg.GetFollowers(h.db, targetUser.ID, viewer.ID, 200)
		case "following":
			users, _ = dbpkg.GetFollowing(h.db, targetUser.ID, viewer.ID, 200)
			title = "Following"
		case "subscribers":
			users, _ = dbpkg.GetSubscribers(h.db, targetUser.ID, viewer.ID, 200)
			title = "Subscribers"
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	h.renderPartial(w, "follow_list_modal", map[string]interface{}{
		"Users":        users,
		"Title":        title,
		"ListType":     listType,
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
	h.render(w, r, "settings.html", data)
}

// settingsSection serves GET /partials/settings/{section} for HTMX panel swaps.
func (h *Handler) settingsSection(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
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
	data["Themes"] = ThemesWithActive(func() string {
		if user != nil {
			return user.ThemeID
		}
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

	// Contact — Numbers, policy, links and requests, each fetched from the brain
	// that owns it. Built for that section only.
	if section == "contact" && user != nil && user.PIALID != "" {
		data["NumbersData"] = h.numbersData(r.Context(), user.PIALID)
		data["AddressesData"] = h.contactAddressesData(r, user.PIALID, "")
	}

	// Notification preferences — fetched from Herald for that section only.
	if section == "notifications" && h.cfg.HeraldURL != "" && user != nil && user.PIALID != "" {
		data["NotifPrefs"] = h.notificationPrefs(r.Context(), user.PIALID).flat()
	}
	// Org memberships — fetched for work/org settings section.
	if section == "org" && h.db != nil && user != nil {
		data["OrgMemberships"] = dbpkg.GetUserOrgMemberships(h.db, user.ID)
	}

	return data
}

// saveNotificationPrefs handles POST /api/settings/notifications — the web's
// notification form. The form and the native clients name the same preferences,
// so both are read by notificationPrefsPatch and written by the one Herald
// writer; the answer is the preferences as Herald then holds them.
//
// An HTML form is not a JSON PATCH body, and this route used to forward the
// raw request body to Herald as one. Herald could not read it, so nothing the
// web submitted here was ever saved.
func (h *Handler) saveNotificationPrefs(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil || user.PIALID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	rawBodyMap := jsonBodyMap(r)
	_ = r.ParseForm()
	patch := notificationPrefsPatch(r, rawBodyMap)
	if rawBodyMap == nil {
		// A checkbox that is off submits nothing at all, so every preference the
		// form declares is read as off unless the form named it. Only the ones
		// the form actually carries are touched.
		for key, field := range patch.boolFields() {
			if _, named := r.Form[key]; named || !webNotifFormFields[key] {
				continue
			}
			off := false
			*field = &off
		}
	}
	prefs, err := h.saveNotificationPrefsTo(r.Context(), user.PIALID, patch)
	if err != nil {
		log.Printf("[herald] save notif prefs: %v", err)
		http.Error(w, "herald unavailable", http.StatusServiceUnavailable)
		return
	}
	apiJSON(w, http.StatusOK, prefs)
}

// webNotifFormFields names the toggles the web's notification form draws. A
// preference the form has no control for is never turned off by submitting it.
var webNotifFormFields = map[string]bool{
	"push_enabled": true, "messages_enabled": true, "likes_enabled": true,
	"reposts_enabled": true, "replies_enabled": true, "follows_enabled": true,
	"achievements_enabled": true,
}

// jsonBodyMap reads a JSON request body into the raw field map the event lane
// uses, or nil when the body is not JSON. The body is restored either way.
func jsonBodyMap(r *http.Request) map[string]json.RawMessage {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return nil
	}
	body, err := peekBody(r, 1<<20)
	if err != nil || len(body) == 0 {
		return nil
	}
	var out map[string]json.RawMessage
	if json.Unmarshal(body, &out) != nil {
		return nil
	}
	return out
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
	if r.Method != http.MethodPost {
		http.Error(w, "405", 405)
		return
	}
	if err := r.ParseMultipartForm(15 << 20); err != nil {
		_ = r.ParseForm()
	}

	user := h.userFromRequest(w, r)
	themeID := r.FormValue("theme_id")
	if !isValidTheme(themeID) {
		themeID = user.ThemeID
	}
	if !isValidTheme(themeID) {
		themeID = "void"
	}

	accentHex := r.FormValue("accent_hex")
	if !accentHexRe.MatchString(accentHex) {
		accentHex = ""
	}

	socialJSON := linksJSON(normalizeSocialLinks(func(k string) string { return r.FormValue("social_" + k) }))
	tipLinksJSON := linksJSON(normalizeTipLinks(func(k string) string { return r.FormValue("tip_" + k) }))

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
		mdVis := r.FormValue("birthday_md_visibility")
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

	// One set of rules with the native clients' password_change event; see
	// changePassword. The form only differs in how it shows the answer.
	status, msg := h.changePassword(user, r.FormValue("current_password"), r.FormValue("new_password"), r.FormValue("confirm_password"))
	if status != http.StatusOK {
		fmt.Fprintf(w, `<span style="color:#ef4444;font-size:13px">%s</span>`, template.HTMLEscapeString(msg))
		return
	}
	fmt.Fprintf(w, `<span style="color:#22c55e;font-size:13px;font-weight:500">%s</span>`, template.HTMLEscapeString(msg))
}

func (h *Handler) bookmarksPage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	filter := r.URL.Query().Get("filter")

	works := h.fetchBookmarkWorks(user, filter)
	works = filterAdultForViewer(user, works)
	// The page template renders these works as cards itself; run the same
	// enrichment sequence renderWorkCardSet applies so a bookmarked work looks
	// identical on the page and on the /bookmarks/items facet fetch.
	h.enrichWorks(works, user)
	// The rail is one dataset with one owner. This surface used to assemble its
	// own two-key subset of it through sidebarData, which is a second place the
	// suggestion set could be fetched — and therefore a second place the quality
	// bar could be missed.
	h.render(w, r, "bookmarks.html", h.withRail(map[string]interface{}{
		"User":              user,
		"Title":             "Bookmarks",
		"SessionID":         uuid.New().String(),
		"Surface":           "bookmarks",
		"ShowScores":        false,
		"CurrentUserHandle": user.Handle,
		"Works":             works,
		"Filter":            filter,
		"Themes":            ThemesWithActive(user.ThemeID),
	}, user, "default"))
}

// bookmarksItemsPartial serves GET /bookmarks/items?filter= — HTMX-swappable work list.
func (h *Handler) bookmarksItemsPartial(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	filter := r.URL.Query().Get("filter")
	works := h.fetchBookmarkWorks(user, filter)
	cards := h.renderWorkCards(works, user, h.workCardCtx(user, "bookmarks"))

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if len(cards) == 0 {
		h.renderPartial(w, "stateEmpty", map[string]interface{}{
			"Title": "No bookmarks yet",
			"Sub":   "Works you save will appear here",
		})
		return
	}
	for _, frag := range cards {
		_, _ = w.Write([]byte(frag))
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
	// Fetch only — enrichment belongs to the render path (renderWorkCardSet) or,
	// for the full-page render, to the caller's enrichWorks call.
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

// socialLinkKeys and tipLinkKeys name the links a profile carries. Every
// surface that reads them — the web's edit form and the profile_update event —
// walks these lists, so a link that exists on one exists on the other.
var socialLinkKeys = []string{"soundcloud", "spotify", "kick", "twitch", "onlyfans", "youtube", "other"}

var tipLinkKeys = []string{"cashapp", "venmo", "paypal", "kofi", "buymeacoffee", "bitcoin", "xrp"}

// tipLinkPrefixes are the ways people paste each tip link. The stored value is
// always the bare handle, so the profile can render one canonical URL.
var tipLinkPrefixes = map[string][]string{
	"cashapp":      {"$", "cash.app/", "https://cash.app/"},
	"venmo":        {"@", "venmo.com/", "https://venmo.com/"},
	"paypal":       {"paypal.me/", "https://paypal.me/", "https://www.paypal.me/"},
	"kofi":         {"ko-fi.com/", "https://ko-fi.com/"},
	"buymeacoffee": {"buymeacoffee.com/", "https://www.buymeacoffee.com/", "https://buymeacoffee.com/"},
}

// normalizeSocialLinks reads the social links from get, whatever it reads them
// out of: a form, or the object a JSON event carries.
func normalizeSocialLinks(get func(string) string) map[string]string {
	out := make(map[string]string, len(socialLinkKeys))
	for _, k := range socialLinkKeys {
		out[k] = strings.TrimSpace(get(k))
	}
	return out
}

// normalizeTipLinks is the same reading for the payment links, with each value
// put in its stored form: a bare handle for the hosted services, a validated
// address for BTC and XRP. Anything that is not one of those is dropped rather
// than stored — a tip link that does not resolve sends money nowhere.
func normalizeTipLinks(get func(string) string) map[string]string {
	out := make(map[string]string, len(tipLinkKeys))
	for _, k := range tipLinkKeys {
		switch k {
		case "bitcoin":
			out[k] = normalizeCryptoAddress(get(k), btcAddressRe)
		case "xrp":
			out[k] = normalizeCryptoAddress(get(k), xrpAddressRe)
		default:
			out[k] = normalizeTipHandle(get(k), tipLinkPrefixes[k])
		}
	}
	return out
}

// linksJSON encodes a link map for the profile row.
func linksJSON(links map[string]string) string {
	b, err := json.Marshal(links)
	if err != nil {
		return "{}"
	}
	return string(b)
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
		{"cashapp", "Cash App", "$", "https://cash.app/$"},
		{"venmo", "Venmo", "@", "https://venmo.com/"},
		{"paypal", "PayPal", "paypal.me/", "https://paypal.me/"},
		{"kofi", "Ko-fi", "ko-fi.com/", "https://ko-fi.com/"},
		{"buymeacoffee", "Buy Me a Coffee", "buymeacoffee.com/", "https://buymeacoffee.com/"},
	}
	for _, p := range paymentProviders {
		h := strings.TrimSpace(m[p.key])
		if h == "" {
			continue
		}
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
		{"xrp", "XRP"},
	}
	for _, p := range cryptoProviders {
		h := strings.TrimSpace(m[p.key])
		if h == "" {
			continue
		}
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

// facetTipModal — GET /facets/tip_modal?handle=<target>
// The tip overlay for one creator: the tip form addressed to that handle and
// the creator's external payment links, rendered together. Opened into
// #overlay-slot by btn_tip / the action bar / the shop header.
func (h *Handler) facetTipModal(w http.ResponseWriter, r *http.Request) {
	viewer := h.userFromRequest(w, r)
	if viewer == nil || viewer.PIALID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	targetHandle := strings.TrimPrefix(strings.TrimSpace(r.URL.Query().Get("handle")), "@")
	if targetHandle == "" || strings.EqualFold(targetHandle, viewer.Handle) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var targetUser *model.User
	if h.db != nil {
		targetUser, _ = dbpkg.GetUserByHandle(h.db, targetHandle)
	}
	if targetUser == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	h.renderPartial(w, "tip_modal", map[string]interface{}{
		"Handle": targetUser.Handle,
		"Links":  buildTipExternalLinks(targetUser.ExternalTipLinksRaw),
	})
}

// ── live hero ────────────────────────────────────────────────────────────────
//
// A broadcast is the same author's content on the same page as their works, so
// it reaches the profile through the same gates the works do, and through the
// same rule the watch surface applies to the broadcast itself.

// liveSurfaceVisible decides whether a running broadcast may be rendered onto a
// surface that is not that stream's own page.
//
// It is the watch surface's rule, restated for a surface the viewer did not ask
// for by URL. liveWatchPage refuses a blocked stream and answers an 18+
// broadcast a viewer's account is not cleared for with renderLiveGate; the
// Playground's GetActiveStreams refuses a blocked or scan-blocked row before it
// is ever read. Nothing here is looser than either.
//
// The one thing a surface does differently is what it does with a refusal. On
// /live/{id} the gate may name the broadcaster, because the viewer typed that
// broadcaster's URL and already knows whose page they are on. A profile hero or
// a Playground card that rendered a gate would instead announce a broadcast the
// viewer is not cleared to know exists, and would leak its title and poster with
// it. So a refusal here is an omission: the surface renders as if the broadcast
// were not happening.
func liveSurfaceVisible(s *model.LiveStream, viewer *model.User) bool {
	if s == nil || s.Status != model.LiveStatusLive {
		return false
	}
	if s.IsBlocked || s.ScanState == "blocked" {
		return false
	}
	if s.IsNSFW && excludeNSFW(viewer) {
		return false
	}
	return true
}

// profileLiveStage resolves the profile_live_stage Facet's inputs for one
// profile. It always returns a complete map: V is nil when there is nothing to
// show, and the Facet then renders an empty, zero-height surface.
//
// walled is the profile's own works wall — a private account this viewer does
// not follow, or an adult creator this viewer is not cleared for. The broadcast
// stands or falls with the works: without this a private account's live camera
// would play to a stranger the works feed will not show a single post to.
//
// isOwner suppresses the presence beat. The hero plays the broadcast, so anyone
// looking at it is watching it and is counted and pushed to exactly like a
// watcher on /live/{id} — but a broadcaster is not a viewer of their own
// broadcast, which is the same rule livePresenceBeats encodes for the
// broadcaster's own stage.
func (h *Handler) profileLiveStage(r *http.Request, profileUser, viewer *model.User, isOwner, walled bool) map[string]interface{} {
	stage := map[string]interface{}{"AuthorID": "", "V": nil}
	if profileUser == nil {
		return stage
	}
	stage["AuthorID"] = profileUser.ID
	if h.db == nil || walled {
		return stage
	}

	s, err := dbpkg.GetStreamForAuthor(h.db, profileUser.ID)
	if err != nil {
		if !errors.Is(err, dbpkg.ErrLiveStreamNotFound) {
			log.Printf("[profile] live stream for %s: %v", profileUser.ID, err)
		}
		return stage
	}
	if !liveSurfaceVisible(s, viewer) {
		return stage
	}

	h.startLiveSweeper()
	v := h.buildLiveView(s, viewer, !isOwner)
	// A hero showing the idle wall is a surface waiting for the broadcast, and
	// it waits the same way the watch page does: present in the audience, with
	// a viewer token for its wait beat to carry. Nothing is tallied on a wall.
	waiting := !isOwner && s.Status == model.LiveStatusIdle
	if v.Heartbeat || waiting {
		// Count this watcher immediately, exactly as liveWatchPage does, so the
		// tally the hero renders already includes the person reading it and the
		// stream's Fragments start reaching this surface at once.
		token := uuid.New().String()
		key, pial := liveViewerKey(r, viewer)
		if key == "" {
			key = "anon:" + token
		}
		count := liveRuntime.touch(s.ID, key, pial)
		v.ViewerToken = token
		if v.Heartbeat {
			v.ViewerCount = count
			v.Viewers = fmtViewerCount(count)
			go h.publishViewerCount(s, count)
		}
	}
	stage["V"] = v
	return stage
}
