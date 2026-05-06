package handler

import (
	"fmt"
	"log"
	"net/http"
	"net/smtp"
	"strings"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
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

// ── Forgot password ───────────────────────────────────────────────────────────

func (h *Handler) forgotPasswordPage(w http.ResponseWriter, r *http.Request) {
	h.render(w, "forgot_password.html", map[string]interface{}{
		"Title": "Reset password · F33D3R",
	})
}

func (h *Handler) forgotPasswordSubmit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/forgot-password", http.StatusSeeOther)
		return
	}
	_ = r.ParseForm()
	email := strings.TrimSpace(strings.ToLower(r.FormValue("email")))

	// Always show the same message — don't leak whether email exists
	done := map[string]interface{}{
		"Title": "Reset password · F33D3R",
		"Sent":  true,
	}

	if h.db == nil || email == "" {
		h.render(w, "forgot_password.html", done)
		return
	}

	user, _ := dbpkg.GetUserByEmail(h.db, email)
	if user == nil {
		h.render(w, "forgot_password.html", done)
		return
	}

	token, err := dbpkg.CreatePasswordResetToken(h.db, user.ID)
	if err != nil {
		log.Printf("[reset] create token: %v", err)
		h.render(w, "forgot_password.html", done)
		return
	}

	link := h.cfg.BaseURL + "/reset-password?token=" + token
	body := fmt.Sprintf(`
<div style="font-family:sans-serif;max-width:480px;margin:0 auto;padding:32px 24px">
  <h2 style="font-size:22px;margin-bottom:8px">Reset your F33D3R password</h2>
  <p style="color:#666;margin-bottom:24px">Someone requested a password reset for <b>@%s</b>. If this was you, click the button below. This link expires in 1 hour.</p>
  <a href="%s" style="display:inline-block;padding:12px 24px;background:#7c3aed;color:#fff;border-radius:8px;text-decoration:none;font-weight:600">Reset password</a>
  <p style="color:#999;font-size:12px;margin-top:24px">If you didn't request this, ignore this email. Your password won't change.</p>
</div>`, user.Handle, link)

	go h.sendEmail(email, "Reset your F33D3R password", body)
	h.render(w, "forgot_password.html", done)
}

// ── Reset password ────────────────────────────────────────────────────────────

func (h *Handler) resetPasswordPage(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		http.Redirect(w, r, "/forgot-password", http.StatusSeeOther)
		return
	}
	if h.db != nil {
		if user, _ := dbpkg.GetUserByResetToken(h.db, token); user == nil {
			h.render(w, "reset_password.html", map[string]interface{}{
				"Title":   "Reset password · F33D3R",
				"Invalid": true,
			})
			return
		}
	}
	h.render(w, "reset_password.html", map[string]interface{}{
		"Title": "Reset password · F33D3R",
		"Token": token,
	})
}

func (h *Handler) resetPasswordSubmit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	_ = r.ParseForm()
	token    := strings.TrimSpace(r.FormValue("token"))
	password := r.FormValue("password")
	confirm  := r.FormValue("confirm")

	fail := func(msg string) {
		h.render(w, "reset_password.html", map[string]interface{}{
			"Title": "Reset password · F33D3R",
			"Token": token,
			"Error": msg,
		})
	}

	if len(password) < 8 {
		fail("Password must be at least 8 characters.")
		return
	}
	if password != confirm {
		fail("Passwords don't match.")
		return
	}
	if h.db == nil {
		fail("Service unavailable — try again.")
		return
	}

	user, _ := dbpkg.GetUserByResetToken(h.db, token)
	if user == nil {
		h.render(w, "reset_password.html", map[string]interface{}{
			"Title":   "Reset password · F33D3R",
			"Invalid": true,
		})
		return
	}

	if err := dbpkg.SetPassword(h.db, user.ID, password); err != nil {
		log.Printf("[reset] set password: %v", err)
		fail("Failed to set password — try again.")
		return
	}
	_ = dbpkg.MarkResetTokenUsed(h.db, token)

	h.render(w, "reset_password.html", map[string]interface{}{
		"Title": "Reset password · F33D3R",
		"Done":  true,
	})
}
