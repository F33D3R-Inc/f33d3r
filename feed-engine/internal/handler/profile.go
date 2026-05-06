package handler

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"

	"github.com/google/uuid"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
)

func (h *Handler) profilePage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	h.render(w, "profile.html", map[string]interface{}{
		"User":       user,
		"ProfileUser": user,
		"IsOwner":    true,
		"Title":      "@" + user.Handle,
		"SessionID":  uuid.New().String(),
		"ShowScores": h.cfg.ShowScores,
		"Themes":     ThemesWithActive(user.ThemeID),
	})
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

	isOwner      := profileUser.ID == viewerUser.ID
	isFollowing   := false
	isSubscribed  := false
	if h.db != nil && !isOwner {
		isFollowing, _ = dbpkg.IsFollowing(h.db, viewerUser.ID, profileUser.ID)
		isSubscribed    = dbpkg.IsSubscribed(h.db, viewerUser.ID, profileUser.ID)
	}

	h.render(w, "profile.html", map[string]interface{}{
		"User":         viewerUser,
		"ProfileUser":  profileUser,
		"IsOwner":      isOwner,
		"IsFollowing":  isFollowing,
		"IsSubscribed": isSubscribed,
		"Title":        "@" + profileUser.Handle,
		"SessionID":    uuid.New().String(),
		"ShowScores":   h.cfg.ShowScores,
		"Themes":       ThemesWithActive(viewerUser.ThemeID),
	})
}

func (h *Handler) settingsPage(w http.ResponseWriter, r *http.Request) {
	user        := h.userFromRequest(w, r)
	saveSuccess := r.URL.Query().Get("saved") == "1"

	// Parse stored social links JSON into a flat map for template pre-fill
	socialLinks := map[string]string{}
	if user.SocialLinksRaw != "" {
		_ = json.Unmarshal([]byte(user.SocialLinksRaw), &socialLinks)
	}

	h.render(w, "settings.html", map[string]interface{}{
		"User":        user,
		"Themes":      ThemesWithActive(user.ThemeID),
		"ShowScores":  h.cfg.ShowScores,
		"SaveSuccess": saveSuccess,
		"Title":       "Settings",
		"SocialLinks": socialLinks,
	})
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

	save := &model.ProfileSave{
		UserID:          user.ID,
		DisplayName:     truncate(r.FormValue("display_name"), 100),
		Bio:             truncate(r.FormValue("bio"), 500),
		Pronouns:        truncate(r.FormValue("pronouns"), 50),
		Location:        truncate(r.FormValue("location"), 100),
		Website:         truncate(r.FormValue("website"), 200),
		ThemeID:         themeID,
		AccentHex:       accentHex,
		JungArchetype:   user.JungArchetype,
		PinnedTrackID:   truncate(r.FormValue("pinned_track_id"), 100),
		SocialLinksJSON: socialJSON,
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
	}
	http.Redirect(w, r, "/settings?saved=1", http.StatusSeeOther)
}

func (h *Handler) bookmarksPage(w http.ResponseWriter, r *http.Request) {
	user      := h.userFromRequest(w, r)
	sessionID := uuid.New().String()

	var posts []*model.Post
	if h.db != nil {
		if bm, err := dbpkg.GetBookmarks(h.db, user.ID, h.cfg.FeedPageSize, ""); err == nil {
			posts = bm
		}
	}
	for _, p := range posts { p.TimeAgo = TimeAgo(p.CreatedAt) }

	h.render(w, "bookmarks.html", map[string]interface{}{
		"User":      user,
		"Title":     "Bookmarks",
		"SessionID": sessionID,
		"Posts":     posts,
		"Themes":    ThemesWithActive(user.ThemeID),
	})
}
