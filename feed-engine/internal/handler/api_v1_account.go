package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
)

// ── /api/v1 account & settings ───────────────────────────────────────────────
//
// The settings screens the native clients draw read from here and write through
// the one mutation lane, POST /events. Nothing on this surface is a second
// implementation of a rule the web already has: the blocked and muted lists,
// the data export, the TOTP secret and the notification preferences each have
// one owner elsewhere in this package or in internal/db, and the handlers below
// are the JSON transport over it.

// ── DTOs ─────────────────────────────────────────────────────────────────────

// BlockListDTO is GET /api/v1/blocks — everyone this account has silenced, in
// the two ways it can be done.
type BlockListDTO struct {
	Blocked []UserDTO `json:"blocked"`
	Muted   []UserDTO `json:"muted"`
}

// NotificationPrefsDTO is the account's notification preferences as Herald
// holds them. The quiet-hours bounds are wall-clock "HH:MM" in the account's
// own timezone, or null when Herald has no bound recorded.
type NotificationPrefsDTO struct {
	PushEnabled         bool    `json:"push_enabled"`
	MessagesEnabled     bool    `json:"messages_enabled"`
	LikesEnabled        bool    `json:"likes_enabled"`
	RepostsEnabled      bool    `json:"reposts_enabled"`
	RepliesEnabled      bool    `json:"replies_enabled"`
	FollowsEnabled      bool    `json:"follows_enabled"`
	AchievementsEnabled bool    `json:"achievements_enabled"`
	MentionsEnabled     bool    `json:"mentions_enabled"`
	FrequenciesEnabled  bool    `json:"frequencies_enabled"`
	QuietHoursEnabled   bool    `json:"quiet_hours_enabled"`
	QuietHoursStart     *string `json:"quiet_hours_start"`
	QuietHoursEnd       *string `json:"quiet_hours_end"`
}

// TwoFactorSetupDTO is the enrolment step: the otpauth:// URI a QR is drawn
// from, and the same secret in the form a person can type by hand.
type TwoFactorSetupDTO struct {
	URI    string `json:"uri"`
	Secret string `json:"secret"`
}

// BackupCodesDTO is a freshly issued set of single-use recovery codes. They are
// shown once — only their hashes are kept.
type BackupCodesDTO struct {
	Codes []string `json:"codes"`
}

// ── GET /api/v1/blocks ───────────────────────────────────────────────────────

func (h *Handler) apiV1Blocks(w http.ResponseWriter, r *http.Request, u *model.User) {
	blocked, err := dbpkg.ListBlockedUsers(h.db, u.ID)
	if err != nil {
		apiServerError(w, err)
		return
	}
	muted, err := dbpkg.ListMutedUsers(h.db, u.ID)
	if err != nil {
		apiServerError(w, err)
		return
	}
	apiJSON(w, http.StatusOK, BlockListDTO{
		Blocked: userDTOs(blocked),
		Muted:   userDTOs(muted),
	})
}

// userDTOs maps a resolved account list to the public DTO, never nil so the
// client decodes an empty list rather than a missing one.
func userDTOs(users []*model.User) []UserDTO {
	out := make([]UserDTO, 0, len(users))
	for _, u := range users {
		if u == nil {
			continue
		}
		out = append(out, userDTO(u))
	}
	return out
}

// ── GET /api/v1/notifications/preferences ────────────────────────────────────

func (h *Handler) apiV1NotificationPrefs(w http.ResponseWriter, r *http.Request, u *model.User) {
	apiJSON(w, http.StatusOK, h.notificationPrefs(r.Context(), u.PIALID).dto())
}

// heraldPrefs is Herald's NotificationPreferences as this brain reads it. Every
// field is a pointer so "absent" and "false" stay different things: a PATCH
// sends only what changed, and a GET that omits a field falls back to the
// platform default rather than silently turning a notification off.
type heraldPrefs struct {
	PushEnabled         *bool   `json:"push_enabled,omitempty"`
	MessagesEnabled     *bool   `json:"messages_enabled,omitempty"`
	LikesEnabled        *bool   `json:"likes_enabled,omitempty"`
	RepostsEnabled      *bool   `json:"reposts_enabled,omitempty"`
	RepliesEnabled      *bool   `json:"replies_enabled,omitempty"`
	FollowsEnabled      *bool   `json:"follows_enabled,omitempty"`
	AchievementsEnabled *bool   `json:"achievements_enabled,omitempty"`
	MentionsEnabled     *bool   `json:"mentions_enabled,omitempty"`
	FrequenciesEnabled  *bool   `json:"frequencies_enabled,omitempty"`
	QuietHoursEnabled   *bool   `json:"quiet_hours_enabled,omitempty"`
	QuietHoursStart     *int    `json:"quiet_hours_start,omitempty"`
	QuietHoursEnd       *int    `json:"quiet_hours_end,omitempty"`
	Timezone            *string `json:"timezone,omitempty"`
}

// notifPrefFields names every boolean preference once, paired with the field it
// reads on the struct. Every surface that iterates the preferences — the GET,
// the event, the web form — walks this table instead of repeating the list.
func (p *heraldPrefs) boolFields() map[string]**bool {
	return map[string]**bool{
		"push_enabled":         &p.PushEnabled,
		"messages_enabled":     &p.MessagesEnabled,
		"likes_enabled":        &p.LikesEnabled,
		"reposts_enabled":      &p.RepostsEnabled,
		"replies_enabled":      &p.RepliesEnabled,
		"follows_enabled":      &p.FollowsEnabled,
		"achievements_enabled": &p.AchievementsEnabled,
		"mentions_enabled":     &p.MentionsEnabled,
		"frequencies_enabled":  &p.FrequenciesEnabled,
		"quiet_hours_enabled":  &p.QuietHoursEnabled,
	}
}

func boolOr(v *bool, def bool) bool {
	if v == nil {
		return def
	}
	return *v
}

// hourLabel renders an hour-of-day bound as the "HH:MM" the clients show, or
// nil when Herald holds no usable bound.
func hourLabel(v *int) *string {
	if v == nil || *v < 0 || *v > 23 {
		return nil
	}
	s := fmt.Sprintf("%02d:00", *v)
	return &s
}

// parseHour reads a quiet-hours bound sent as "HH:MM", "HH" or a bare hour.
func parseHour(v string) (int, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if i := strings.IndexByte(v, ':'); i >= 0 {
		v = v[:i]
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 || n > 23 {
		return 0, false
	}
	return n, true
}

// dto is the preference set as the clients read it. Every notification is on by
// default: an account that has never opened this screen has no Herald row, and
// the honest reading of that is "nothing has been turned off".
func (p *heraldPrefs) dto() NotificationPrefsDTO {
	return NotificationPrefsDTO{
		PushEnabled:         boolOr(p.PushEnabled, true),
		MessagesEnabled:     boolOr(p.MessagesEnabled, true),
		LikesEnabled:        boolOr(p.LikesEnabled, true),
		RepostsEnabled:      boolOr(p.RepostsEnabled, true),
		RepliesEnabled:      boolOr(p.RepliesEnabled, true),
		FollowsEnabled:      boolOr(p.FollowsEnabled, true),
		AchievementsEnabled: boolOr(p.AchievementsEnabled, true),
		MentionsEnabled:     boolOr(p.MentionsEnabled, true),
		FrequenciesEnabled:  boolOr(p.FrequenciesEnabled, true),
		QuietHoursEnabled:   boolOr(p.QuietHoursEnabled, false),
		QuietHoursStart:     hourLabel(p.QuietHoursStart),
		QuietHoursEnd:       hourLabel(p.QuietHoursEnd),
	}
}

// flat is the same preferences as the settings templates read them: a map of
// the contract's keys, so a template does not have to know Herald's struct.
func (p *heraldPrefs) flat() map[string]interface{} {
	d := p.dto()
	raw, err := json.Marshal(d)
	if err != nil {
		return nil
	}
	out := map[string]interface{}{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	// The web section shows the bounds as hours, which is what it always did.
	out["quiet_hours_start"] = intOr(p.QuietHoursStart, 22)
	out["quiet_hours_end"] = intOr(p.QuietHoursEnd, 8)
	return out
}

func intOr(v *int, def int) int {
	if v == nil {
		return def
	}
	return *v
}

// notificationPrefs reads the account's preferences from Herald. An unreachable
// or unconfigured Herald yields the defaults, never an error screen: this is a
// preference set, and the platform default is a truthful answer for it.
func (h *Handler) notificationPrefs(ctx context.Context, pialID string) *heraldPrefs {
	p := &heraldPrefs{}
	if h.cfg == nil || h.cfg.HeraldURL == "" || pialID == "" {
		return p
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.cfg.HeraldURL+"/v1/preferences/"+pialID, nil)
	if err != nil {
		return p
	}
	resp, err := h.httpClient.Do(req)
	if err != nil {
		return p
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return p
	}
	if err := json.NewDecoder(resp.Body).Decode(p); err != nil {
		return &heraldPrefs{}
	}
	return p
}

// saveNotificationPrefsTo sends the named changes to Herald and answers with
// the preferences as Herald then holds them. Only the fields set on patch are
// sent, so a screen that shows one toggle can change one toggle.
func (h *Handler) saveNotificationPrefsTo(ctx context.Context, pialID string, patch *heraldPrefs) (NotificationPrefsDTO, error) {
	if h.cfg == nil || h.cfg.HeraldURL == "" || pialID == "" {
		return patch.dto(), nil
	}
	body, err := json.Marshal(patch)
	if err != nil {
		return NotificationPrefsDTO{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch,
		h.cfg.HeraldURL+"/v1/preferences/"+pialID, bytes.NewReader(body))
	if err != nil {
		return NotificationPrefsDTO{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.httpClient.Do(req)
	if err != nil {
		return NotificationPrefsDTO{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return NotificationPrefsDTO{}, fmt.Errorf("herald preferences: status %d", resp.StatusCode)
	}
	return h.notificationPrefs(ctx, pialID).dto(), nil
}

// notificationPrefsPatch reads the changes an event body names. A key that is
// absent is not changed; a key that is present is, whichever way the body came.
func notificationPrefsPatch(r *http.Request, rawBodyMap map[string]json.RawMessage) *heraldPrefs {
	patch := &heraldPrefs{}
	for key, field := range patch.boolFields() {
		if v, present := eventBool(r, rawBodyMap, key); present {
			b := v
			*field = &b
		}
	}
	if n, ok := parseHour(eventField(r, rawBodyMap, "quiet_hours_start")); ok {
		patch.QuietHoursStart = &n
	}
	if n, ok := parseHour(eventField(r, rawBodyMap, "quiet_hours_end")); ok {
		patch.QuietHoursEnd = &n
	}
	if tz := eventField(r, rawBodyMap, "timezone"); tz != "" {
		patch.Timezone = &tz
	}
	return patch
}

// apiNotificationPrefsEvent — POST /events {"event_type":"notification_prefs", …}
func (h *Handler) apiNotificationPrefsEvent(w http.ResponseWriter, r *http.Request, u *model.User, rawBodyMap map[string]json.RawMessage) {
	if u.PIALID == "" {
		apiError(w, http.StatusForbidden, "no_identity", "This account has no identity yet.")
		return
	}
	prefs, err := h.saveNotificationPrefsTo(r.Context(), u.PIALID, notificationPrefsPatch(r, rawBodyMap))
	if err != nil {
		log.Printf("[herald] save notif prefs for %s: %v", u.Handle, err)
		apiError(w, http.StatusServiceUnavailable, "herald_unavailable", "Could not save your notification preferences — try again.")
		return
	}
	apiJSON(w, http.StatusOK, prefs)
}

// ── GET /api/v1/2fa/setup ────────────────────────────────────────────────────

func (h *Handler) apiV12FASetup(w http.ResponseWriter, r *http.Request, u *model.User) {
	uri, secret, status, msg := h.totpBeginSetup(u)
	if status != http.StatusOK {
		code := "server_error"
		if status == http.StatusConflict {
			code = "two_fa_already_enabled"
		}
		apiError(w, status, code, msg)
		return
	}
	apiJSON(w, http.StatusOK, TwoFactorSetupDTO{URI: uri, Secret: secret})
}

// api2FAEvent applies two_fa_enable / two_fa_disable and answers with the
// account, so the client's own view of two_fa_enabled comes from the server.
func (h *Handler) api2FAEvent(w http.ResponseWriter, r *http.Request, u *model.User, rawBodyMap map[string]json.RawMessage, enable bool) {
	code := eventField(r, rawBodyMap, "code")
	var status int
	var msg string
	if enable {
		status, msg = h.totpEnableCore(u, code)
	} else {
		status, msg = h.totpDisableCore(u, code)
	}
	if status != http.StatusOK {
		apiError(w, status, twoFAErrorCode(status), msg)
		return
	}
	if tok := GetSessionToken(r); tok != "" {
		h.sessionCache.Delete(tok)
	}
	apiJSON(w, http.StatusOK, h.meDTOFor(u))
}

func twoFAErrorCode(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "two_fa_invalid"
	case http.StatusConflict:
		return "two_fa_already_enabled"
	case http.StatusBadRequest:
		return "two_fa_bad_request"
	default:
		return "server_error"
	}
}

// ── Backup codes ─────────────────────────────────────────────────────────────

// backupCodeCount is how many single-use recovery codes an account holds.
const backupCodeCount = 8

// issueBackupCodes replaces the account's recovery codes with a fresh set and
// returns them in the clear — the only moment they exist in the clear, since
// only their hashes are stored. One issuer for the web's reveal page and for
// the two_fa_backup_codes event.
func (h *Handler) issueBackupCodes(userID string) ([]string, error) {
	codes := make([]string, backupCodeCount)
	for i := range codes {
		c, err := dbpkg.GenerateBackupCode()
		if err != nil {
			return nil, fmt.Errorf("generate: %w", err)
		}
		codes[i] = c
	}
	if err := dbpkg.StoreBackupCodes(h.db, userID, codes); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	return codes, nil
}

func (h *Handler) apiBackupCodesEvent(w http.ResponseWriter, r *http.Request, u *model.User) {
	codes, err := h.issueBackupCodes(u.ID)
	if err != nil {
		log.Printf("[backup-codes] issue for %s: %v", u.Handle, err)
		apiServerError(w, err)
		return
	}
	apiJSON(w, http.StatusOK, BackupCodesDTO{Codes: codes})
}

// ── GET /api/v1/account/export ───────────────────────────────────────────────

// apiV1AccountExport hands the account its own subject-access export — the same
// document /api/data/export writes, built by the same function.
func (h *Handler) apiV1AccountExport(w http.ResponseWriter, r *http.Request, u *model.User) {
	export, err := h.dataExportBody(u)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "export_unavailable",
			"We could not assemble your export right now. Try again in a moment.")
		return
	}
	apiJSON(w, http.StatusOK, export)
}

// ── account_delete ───────────────────────────────────────────────────────────

// apiAccountDeleteEvent queues the account's deletion and signs every device
// out. The deletion itself is deferred — 2257 records outlive the account — so
// what this writes is the pending instruction, and what it ends is the access.
func (h *Handler) apiAccountDeleteEvent(w http.ResponseWriter, r *http.Request, u *model.User, rawBodyMap map[string]json.RawMessage) {
	if err := h.queueDataDeletion(u); err != nil {
		log.Printf("[gdpr] deletion request for %s: %v", u.Handle, err)
		apiServerError(w, err)
		return
	}
	dbpkg.LogPIALEvent(h.db, u.PIALID, u.ID, "account_delete_requested", map[string]interface{}{
		"reason": truncate(eventField(r, rawBodyMap, "reason"), 500),
	}, "gdpr")
	h.endEverySession(r, u)
	w.WriteHeader(http.StatusNoContent)
}

// endEverySession signs the account out everywhere and drops the cached
// resolutions of the token this request arrived on, so the very next request
// resolves to nobody rather than to a cached account.
func (h *Handler) endEverySession(r *http.Request, u *model.User) {
	if err := dbpkg.RevokeAllSessions(h.db, u.ID); err != nil {
		log.Printf("[sessions] revoke all for %s: %v", u.Handle, err)
	}
	if tok := GetSessionToken(r); tok != "" {
		h.sessionCache.Delete(tok)
	}
	h.sessionCache.Delete("__handle__" + u.Handle)
}
