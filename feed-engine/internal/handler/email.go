package handler

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/smtp"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
)

// errSMTPNotConfigured is sendEmail's answer when SMTP_HOST is unset: nothing
// was sent, and the caller says so in its own words.
var errSMTPNotConfigured = errors.New("email: SMTP_HOST is not configured")

// sendEmail sends one HTML email through the configured SMTP server. It returns
// why nothing was sent, so no caller can mistake an unsent email for a sent one.
func (h *Handler) sendEmail(to, subject, bodyHTML string) error {
	if h.cfg.SMTPHost == "" {
		return errSMTPNotConfigured
	}
	if to == "" {
		return errors.New("email: no recipient address")
	}
	auth := smtp.PlainAuth("", h.cfg.SMTPUser, h.cfg.SMTPPass, h.cfg.SMTPHost)
	msg := fmt.Sprintf(
		"From: F33D3R <%s>\r\nTo: %s\r\nSubject: %s\r\nContent-Type: text/html; charset=UTF-8\r\n\r\n%s",
		h.cfg.SMTPFrom, to, subject, bodyHTML,
	)
	addr := fmt.Sprintf("%s:%d", h.cfg.SMTPHost, h.cfg.SMTPPort)
	if err := smtp.SendMail(addr, auth, h.cfg.SMTPFrom, []string{to}, []byte(msg)); err != nil {
		return fmt.Errorf("email: sending %q: %w", subject, err)
	}
	return nil
}

// sendNewDeviceAlert emails the user when a login from an unrecognised device or IP is detected.
// It also appends an event to the PIAL audit trail. Called from a goroutine — never blocks the request.
func (h *Handler) sendNewDeviceAlert(user *model.User, r *http.Request) {
	ua := r.UserAgent()
	ip := r.RemoteAddr

	// PIAL audit trail — written regardless of whether email is configured.
	if h.db != nil && user.PIALID != "" {
		dbpkg.LogPIALEvent(h.db, user.PIALID, user.ID, "new_device_login", map[string]interface{}{
			"ip": ip, "user_agent": ua,
		}, "security")
	}

	// Resolve email — not stored on model.User; fetch from DB.
	email, err := dbpkg.GetUserEmail(h.db, user.ID)
	if err != nil {
		log.Printf("[security] new-device alert for user %s: %v", user.ID, err)
		return
	}
	if email == "" || h.cfg.SMTPHost == "" {
		return
	}

	subject := "New sign-in to your F33D3R account"
	body := fmt.Sprintf(
		`<p>We detected a new sign-in to <strong>@%s</strong>.</p>`+
			`<p><strong>IP address:</strong> %s<br>`+
			`<strong>Device / browser:</strong> %s</p>`+
			`<p>If this was you, no action is needed.</p>`+
			`<p>If this wasn't you, go to <strong>Settings &rarr; Security &rarr; Active Sessions</strong> and revoke it immediately.</p>`,
		user.Handle, ip, ua,
	)
	if err := h.sendEmail(email, subject, body); err != nil {
		log.Printf("[security] new-device alert for user %s from %s not emailed: %v", user.ID, ip, err)
		return
	}
	log.Printf("[security] new-device alert sent to user %s from %s", user.ID, ip)
}
