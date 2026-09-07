package handler

import (
	"fmt"
	"log"
	"net/http"
	"net/smtp"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
)

// sendEmail sends a plain HTML email via configured SMTP.
// Logs and no-ops if SMTP is not configured (dev mode).
func (h *Handler) sendEmail(to, subject, bodyHTML string) {
	if h.cfg.SMTPHost == "" {
		log.Printf("[email] SMTP not configured — skipping send to %s: %s", to, subject)
		return
	}
	auth := smtp.PlainAuth("", h.cfg.SMTPUser, h.cfg.SMTPPass, h.cfg.SMTPHost)
	msg := fmt.Sprintf(
		"From: F33D3R <%s>\r\nTo: %s\r\nSubject: %s\r\nContent-Type: text/html; charset=UTF-8\r\n\r\n%s",
		h.cfg.SMTPFrom, to, subject, bodyHTML,
	)
	addr := fmt.Sprintf("%s:%d", h.cfg.SMTPHost, h.cfg.SMTPPort)
	if err := smtp.SendMail(addr, auth, h.cfg.SMTPFrom, []string{to}, []byte(msg)); err != nil {
		log.Printf("[email] send failed to %s: %v", to, err)
	}
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
	email := dbpkg.GetUserEmail(h.db, user.ID)
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
	h.sendEmail(email, subject, body)
	log.Printf("[security] new-device alert sent to user %s from %s", user.ID, ip)
}
