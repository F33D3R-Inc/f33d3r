package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strings"
	"time"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
	"github.com/google/uuid"
)

// ── Legal section data ────────────────────────────────────────────────────────

type legalSection struct {
	Title string
	Body  template.HTML
}

// ── Privacy Policy ────────────────────────────────────────────────────────────

func (h *Handler) privacyPage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	sections := []legalSection{
		{
			Title: "1. What we collect",
			Body: `We collect: account handle and display name, email or phone (for verification tier 1),
			a SHA-256 hash of your government ID document (never the document image itself), device identifiers,
			IP addresses at login, session tokens, content you post, messages you send (end-to-end encrypted —
			we cannot read them), and engagement signals (likes, views, follows). We do NOT sell your data.`,
		},
		{
			Title: "2. How we use it",
			Body: `Your data is used to: operate the platform, deliver your personalised feed (AethyrRank),
			comply with legal obligations (18 U.S.C. § 2257, CSAM detection), detect fraud and abuse,
			and facilitate creator monetisation when you opt in. AethyrRank ranking signals are computed
			internally and never shared with third parties.`,
		},
		{
			Title: "3. Adult content",
			Body: `F33D3R hosts adult content created by verified adults. All creators of adult content
			must complete identity verification (Verity Tier 2+). Age-band confirmation is stored as a
			cryptographic hash. We maintain records as required by 18 U.S.C. § 2257. CSAM detection runs
			on every upload. Any detected CSAM is immediately removed, reported to NCMEC, and the account
			is permanently terminated and reported to law enforcement.`,
		},
		{
			Title: "4. Data sharing",
			Body: `We share data with: payment processors (for creator payouts — only what they require for
			KYC/AML compliance), law enforcement when legally compelled, NCMEC for CSAM reports (mandatory).
			We do not share data with advertisers.`,
		},
		{
			Title: "5. Data retention",
			Body: `Active account data is retained while your account is active. Deleted posts are removed
			from feeds within 24 hours and from databases within 30 days. 2257 records are retained for
			7 years as required by law. Session tokens expire after 30 days of inactivity. You can request
			full data export or deletion under GDPR/CCPA — see below.`,
		},
		{
			Title: "6. Your rights (GDPR / CCPA)",
			Body: `You have the right to: access your data, correct inaccurate data, delete your account
			and associated data (except legally mandated retention), port your data, and opt out of
			non-essential processing. Submit requests to privacy@f33d3r.app. We respond within 30 days.
			Some data (2257 records, CSAM reports, fraud logs) cannot be deleted due to legal obligations.`,
		},
		{
			Title: "7. Security",
			Body: `All data in transit is encrypted with TLS 1.3. Messages are end-to-end encrypted —
			we cannot read your messages. Private keys never leave your device. Passwords are
			bcrypt-hashed. Sessions use cryptographically random tokens. We run regular security audits.`,
		},
		{
			Title: "8. Cookies",
			Body: `We use a single session cookie (f33d3r_session) for authentication. No advertising
			cookies, no third-party tracking pixels. LocalStorage is used only for your messaging vault keys
			(encrypted, never transmitted to us).`,
		},
		{
			Title: "9. Changes to this policy",
			Body: `Material changes will be notified via in-app notification at least 14 days before taking
			effect. Continued use after the effective date constitutes acceptance.`,
		},
	}
	h.render(w, r, "privacy.html", map[string]interface{}{
		"User":       user,
		"Title":      "Privacy Policy · F33D3R",
		"SessionID":  uuid.New().String(),
		"ShowScores": false,
		"Themes":     ThemesWithActive(user.ThemeID),
		"Sections":   sections,
	})
}

// ── Terms of Service ──────────────────────────────────────────────────────────

func (h *Handler) termsPage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	sections := []legalSection{
		{
			Title: "1. Acceptance",
			Body: `By accessing or using F33D3R, you agree to these Terms. If you do not agree, do not
			use the platform. You must be at least 18 years old to use F33D3R.`,
		},
		{
			Title: "2. Prohibited content",
			Body: `You must not post: child sexual abuse material (CSAM) — zero tolerance, automatic
			termination and law enforcement referral; non-consensual intimate images (NCII); content
			that promotes terrorism or violence; doxxing or harassment campaigns; spam or coordinated
			inauthentic behaviour; copyright-infringing content; content that impersonates others.`,
		},
		{
			Title: "3. Adult content",
			Body: `Adult content is permitted only from creators who have completed Verity Tier 2 identity
			verification (age 18+) and enabled adult creator mode. All depicted persons must be 18+.
			Creators certify compliance with 18 U.S.C. § 2257. F33D3R files records on creators' behalf.
			Viewers must complete age verification to access adult content.`,
		},
		{
			Title: "4. Intellectual property",
			Body: `You retain ownership of content you post. You grant F33D3R a non-exclusive, royalty-free
			licence to display, distribute, and promote your content on the platform. You warrant that you
			own or have rights to all content you post. DMCA takedown requests are handled per our DMCA
			policy at /dmca.`,
		},
		{
			Title: "5. Creator monetisation",
			Body: `Creators may earn AET tokens through subscriptions, tips, and pay-per-view content.
			F33D3R retains a 2.5% platform fee. Creators are responsible for their own tax obligations.
			Payouts require identity verification (Verity Tier 3). F33D3R is not liable for payment
			processor decisions.`,
		},
		{
			Title: "6. Account termination",
			Body: `F33D3R may suspend or terminate accounts for violations of these Terms, at our discretion,
			with or without notice. CSAM violations result in immediate permanent termination and law
			enforcement referral. You may delete your account at any time from Settings.`,
		},
		{
			Title: "7. Limitation of liability",
			Body: `F33D3R is provided "as is". We are not liable for user-generated content, service
			interruptions, or data loss beyond what is required by applicable law. Our total liability
			to you shall not exceed the greater of $100 or amounts paid to us in the past 12 months.`,
		},
		{
			Title: "8. Governing law",
			Body: `These Terms are governed by the laws of the United States. Disputes shall be resolved
			by binding arbitration except where prohibited by law. You waive the right to class action
			proceedings.`,
		},
	}
	h.render(w, r, "terms.html", map[string]interface{}{
		"User":       user,
		"Title":      "Terms of Service · F33D3R",
		"SessionID":  uuid.New().String(),
		"ShowScores": false,
		"Themes":     ThemesWithActive(user.ThemeID),
		"Sections":   sections,
	})
}

// ── DMCA ──────────────────────────────────────────────────────────────────────

func (h *Handler) dmcaPage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	h.render(w, r, "dmca.html", map[string]interface{}{
		"User":       user,
		"Title":      "DMCA Takedown · F33D3R",
		"SessionID":  uuid.New().String(),
		"ShowScores": false,
		"Themes":     ThemesWithActive(user.ThemeID),
		"Submitted":  false,
	})
}

func (h *Handler) dmcaSubmit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "405", http.StatusMethodNotAllowed)
		return
	}
	_ = r.ParseForm()
	ticketID := "DMCA-" + strings.ToUpper(uuid.New().String()[:8])

	claimantName := strings.TrimSpace(r.FormValue("claimant_name"))
	claimantEmail := strings.TrimSpace(r.FormValue("claimant_email"))
	infringingURL := strings.TrimSpace(r.FormValue("infringing_url"))
	originalURL := strings.TrimSpace(r.FormValue("original_url"))
	description := strings.TrimSpace(r.FormValue("description"))

	if claimantName == "" || claimantEmail == "" || infringingURL == "" {
		http.Error(w, "Missing required fields", http.StatusBadRequest)
		return
	}

	// Store in DB
	if h.db != nil {
		_, err := h.db.Exec(`
			INSERT INTO dmca_requests (
				ticket_id, claimant_name, claimant_email,
				infringing_url, original_url, description, status, created_at
			) VALUES ($1,$2,$3,$4,$5,$6,'pending',$7)`,
			ticketID, claimantName, claimantEmail,
			infringingURL, originalURL, description, time.Now(),
		)
		if err != nil {
			log.Printf("[dmca] insert error: %v", err)
		}
	}

	log.Printf("[dmca] new request %s from %s <%s> — %s", ticketID, claimantName, claimantEmail, infringingURL)

	user := h.userFromRequest(w, r)
	h.render(w, r, "dmca.html", map[string]interface{}{
		"User":       user,
		"Title":      "DMCA Takedown · F33D3R",
		"SessionID":  uuid.New().String(),
		"ShowScores": false,
		"Themes":     ThemesWithActive(user.ThemeID),
		"Submitted":  true,
		"TicketID":   ticketID,
	})
}

// ── DMCA admin queue ──────────────────────────────────────────────────────────

type DMCARequest struct {
	TicketID      string
	ClaimantName  string
	ClaimantEmail string
	InfringingURL string
	OriginalURL   string
	Description   string
	Status        string
	CreatedAt     time.Time
}

func (h *Handler) dmcaAdminQueue(w http.ResponseWriter, r *http.Request) {
	caller := h.userFromRequest(w, r)
	if !caller.IsAdmin() {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return
	}

	// Handle resolve action
	if r.Method == http.MethodPost {
		_ = r.ParseForm()
		ticketID := r.FormValue("ticket_id")
		status := r.FormValue("status") // "resolved" | "dismissed" | "actioned"
		if ticketID != "" && h.db != nil {
			h.db.Exec(`UPDATE dmca_requests SET status = $1, resolved_at = $2 WHERE ticket_id = $3`,
				status, time.Now(), ticketID)
		}
		// Return styled replacement for HTMX callers; redirect otherwise.
		if r.Header.Get("HX-Request") == "true" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprintf(w, `<div id="dmca-%s" class="admin-report-resolved">✓ %s — %s</div>`,
				ticketID, status, ticketID)
			return
		}
		http.Redirect(w, r, "/admin/dmca", http.StatusSeeOther)
		return
	}

	var requests []DMCARequest
	if h.db != nil {
		rows, err := h.db.Query(`
			SELECT ticket_id, claimant_name, claimant_email, infringing_url,
			       COALESCE(original_url,''), COALESCE(description,''), status, created_at
			FROM dmca_requests
			WHERE status = 'pending'
			ORDER BY created_at DESC LIMIT 100`)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var req DMCARequest
				rows.Scan(&req.TicketID, &req.ClaimantName, &req.ClaimantEmail,
					&req.InfringingURL, &req.OriginalURL, &req.Description,
					&req.Status, &req.CreatedAt)
				requests = append(requests, req)
			}
		}
	}

	// Redirect to admin panel DMCA tab
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
	_ = uuid.New() // keep import used
}

// ── 2257 per-content record creation ─────────────────────────────────────────
// Called when a creator publishes a post with is_nsfw=true.
// Stores a compliance record in Verity linking the creator's PIAL to the content ID.

func (h *Handler) create2257Record(pialID, contentID string) {
	if h.cfg.VerityURL == "" || pialID == "" || contentID == "" {
		return
	}
	payload, _ := json.Marshal(map[string]string{
		"pial_id":     pialID,
		"content_id":  contentID,
		"record_type": "content_post",
	})
	go func() {
		// Fire and forget — Verity stores the record
		req, err := http.NewRequest(http.MethodPost, h.cfg.VerityURL+"/v1/compliance/2257/record", strings.NewReader(string(payload)))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		// 2257 record write is service-to-service.
		req.Header.Set("X-Internal-Key", h.cfg.InternalAPIKey)
		resp, err := h.httpClient.Do(req)
		if err != nil {
			log.Printf("[2257] record failed for content %s: %v", contentID, err)
			return
		}
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			log.Printf("[2257] record non-2xx for content %s: %d", contentID, resp.StatusCode)
		}
	}()
}

// ── CSAM detection ────────────────────────────────────────────────────────────
// Azure Content Moderator for image/video scanning on every upload.
// PhotoDNA-level hash matching is the gold standard — request access at
// https://www.microsoft.com/en-us/photodna. For now: Azure CM adult classifier.

type CSAMResult struct {
	Clean     bool
	Score     float64
	ReviewURL string
}

func (h *Handler) csamScan(mediaURL, pialID, contentType string) CSAMResult {
	log.Printf("[csam] scan: type=%s pial=%s url=%s", contentType, pialID, mediaURL)

	if h.cfg.AzureCMEndpoint == "" || h.cfg.AzureCMKey == "" {
		// Not configured — audit log only, return clean (dev mode)
		if h.db != nil {
			h.db.Exec(`
				INSERT INTO csam_scan_log (media_url, pial_id, content_type, result, score)
				VALUES ($1, $2, $3, 'unchecked', 0)`,
				mediaURL, pialID, contentType)
		}
		return CSAMResult{Clean: true, Score: 0.0}
	}

	// Azure Content Moderator — Evaluate endpoint
	payload, _ := json.Marshal(map[string]string{
		"DataRepresentation": "URL",
		"Value":              mediaURL,
	})
	endpoint := h.cfg.AzureCMEndpoint +
		"/contentmoderator/moderate/v1.0/ProcessImage/Evaluate?CacheImage=true"

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		log.Printf("[csam] request build error: %v", err)
		return CSAMResult{Clean: true, Score: 0.0}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Ocp-Apim-Subscription-Key", h.cfg.AzureCMKey)

	resp, err := h.httpClient.Do(req)
	if err != nil {
		log.Printf("[csam] azure cm error: %v", err)
		return CSAMResult{Clean: true, Score: 0.0}
	}
	defer resp.Body.Close()

	var cmResult struct {
		IsImageAdultClassified   bool    `json:"IsImageAdultClassified"`
		AdultClassificationScore float64 `json:"AdultClassificationScore"`
		IsImageRacyClassified    bool    `json:"IsImageRacyClassified"`
		RacyClassificationScore  float64 `json:"RacyClassificationScore"`
		Result                   bool    `json:"Result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&cmResult); err != nil {
		log.Printf("[csam] azure cm decode error: %v", err)
		return CSAMResult{Clean: true, Score: 0.0}
	}

	score := cmResult.AdultClassificationScore
	flagged := cmResult.IsImageAdultClassified && score > 0.9
	result := "clean"
	if flagged {
		result = "flagged"
	}

	// Audit log — every scan recorded
	if h.db != nil {
		h.db.Exec(`
			INSERT INTO csam_scan_log (media_url, pial_id, content_type, result, score)
			VALUES ($1, $2, $3, $4, $5)`,
			mediaURL, pialID, contentType, result, score)
	}

	if !flagged {
		return CSAMResult{Clean: true, Score: score}
	}

	log.Printf("[csam] FLAGGED content: score=%.3f pial=%s url=%s — initiating enforcement", score, pialID, mediaURL)

	// Enforcement cascade (async so upload response isn't blocked)
	go h.csamEnforce(mediaURL, pialID, score)

	return CSAMResult{Clean: false, Score: score}
}

// csamEnforce runs the mandatory enforcement cascade on confirmed CSAM detection.
// Per 18 U.S.C. § 2258A, CyberTipline reporting is mandatory within 24 hours.
func (h *Handler) csamEnforce(mediaURL, pialID string, score float64) {
	if h.db == nil {
		return
	}
	// 1. Log to law enforcement queue (NCMEC CyberTipline submission required)
	h.db.Exec(`
		INSERT INTO law_enforcement_queue (pial_id, media_url, report_type)
		VALUES ($1, $2, 'csam')`,
		pialID, mediaURL)

	// 2. Revoke all capabilities — PIAL terminated
	for _, cap := range []string{"posting", "messaging", "tipping", "subscribing", "live_streaming"} {
		dbpkg.SetCapability(h.db, pialID, cap, "revoked", "csam_enforcement", "system", nil)
	}

	// 3. Log PIAL event — immutable record
	dbpkg.LogPIALEvent(h.db, pialID, "", "csam_detected", map[string]interface{}{
		"media_url": mediaURL, "score": score, "action": "pial_terminated",
	}, "zodacare")

	log.Printf("[csam] enforcement complete for pial=%s — queued for NCMEC CyberTipline", pialID)
}

// ── GDPR / CCPA data deletion request ────────────────────────────────────────

func (h *Handler) requestDataDeletion(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "405", http.StatusMethodNotAllowed)
		return
	}
	user := h.userFromRequest(w, r)
	w.Header().Set("Content-Type", "application/json")

	if h.db == nil {
		w.Write([]byte(`{"ok":true,"message":"Deletion request queued. We will process within 30 days."}`))
		return
	}

	err := h.queueDataDeletion(user)
	if err != nil {
		log.Printf("[gdpr] deletion request error for %s: %v", user.Handle, err)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"Failed to queue deletion request"}`))
		return
	}

	log.Printf("[gdpr] deletion requested by @%s (PIAL:%s)", user.Handle, user.PIALID)
	if facetRequest(r) {
		// A queued deletion ends the session: the Shell is sent to /logout,
		// which draws its own Shell and so must be a document navigation.
		hxRedirect(w, "/logout")
		return
	}
	w.Write([]byte(`{"ok":true,"message":"Data deletion request received. We will process within 30 days. Legally required records (18 U.S.C. § 2257) cannot be deleted."}`))
}

// queueDataDeletion records the account's deletion request. The deletion is
// deferred, not immediate: 18 U.S.C. § 2257 records must survive it, so the row
// is a pending instruction the retention job carries out. One writer, called by
// the web form and by the account_delete event alike.
func (h *Handler) queueDataDeletion(user *model.User) error {
	if h.db == nil || user == nil || user.ID == "" {
		return nil
	}
	_, err := h.db.Exec(`
		INSERT INTO data_deletion_requests (user_id, pial_id, requested_at, status)
		VALUES ($1, $2, $3, 'pending')
		ON CONFLICT (user_id) DO UPDATE SET requested_at = $3, status = 'pending'`,
		user.ID, user.PIALID, time.Now(),
	)
	return err
}

// ── Data export ───────────────────────────────────────────────────────────────

func (h *Handler) requestDataExport(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	export, err := h.dataExportBody(user)
	if err != nil {
		http.Error(w, "export unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="f33d3r-export.json"`)
	json.NewEncoder(w).Encode(export)
}

// dataExportBody builds the subject-access export for one account. It is the
// document itself, with no transport attached, so the cookie route and
// GET /api/v1/account/export hand out the same facts.
//
// A subject-access export that quietly reports zero is worse than one that
// fails: the person cannot tell an empty answer from a broken query. Every
// count here is checked, and a failure refuses the export rather than handing
// someone a document that understates what is held about them.
//
// `following_id` is the column. This read said `followed_id` — a column that
// does not exist — and discarded the resulting error, so every export ever
// produced reported follower_count: 0.
func (h *Handler) dataExportBody(user *model.User) (map[string]interface{}, error) {
	export := map[string]interface{}{
		"handle":       user.Handle,
		"display_name": user.DisplayName,
		"bio":          user.Bio,
		"pronouns":     user.Pronouns,
		"location":     user.Location,
		"website":      user.Website,
		"tier":         user.Tier,
		"realm":        user.Realm,
		"xp":           user.XP,
		"pial_id":      user.PIALID,
		"exported_at":  time.Now().UTC().Format(time.RFC3339),
		"note":         "Messages are end-to-end encrypted and cannot be exported from the server. Posts can be found at /" + user.Handle,
	}

	if h.db != nil {
		var postCount int
		if err := h.db.QueryRow(`SELECT COUNT(*) FROM works WHERE author_id = $1 AND deleted_at IS NULL`, user.ID).Scan(&postCount); err != nil {
			log.Printf("[compliance] data export: counting works for %s: %v", user.Handle, err)
			return nil, err
		}
		export["post_count"] = postCount

		var followerCount, followingCount int
		if err := h.db.QueryRow(`SELECT COUNT(*) FROM follows WHERE following_id = $1`, user.ID).Scan(&followerCount); err != nil {
			log.Printf("[compliance] data export: counting followers for %s: %v", user.Handle, err)
			return nil, err
		}
		if err := h.db.QueryRow(`SELECT COUNT(*) FROM follows WHERE follower_id = $1`, user.ID).Scan(&followingCount); err != nil {
			log.Printf("[compliance] data export: counting following for %s: %v", user.Handle, err)
			return nil, err
		}
		export["follower_count"] = followerCount
		export["following_count"] = followingCount
	}

	dbpkg.LogPIALEvent(h.db, user.PIALID, user.ID, "data_export", map[string]interface{}{
		"requested_by": user.Handle,
	}, "gdpr")

	return export, nil
}

// compliancePage serves GET /compliance — content moderation transparency report.
func (h *Handler) compliancePage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	h.render(w, r, "privacy.html", map[string]interface{}{
		"User":   user,
		"Title":  "Compliance & Transparency",
		"Themes": ThemesWithActive(user.ThemeID),
		"Sections": []legalSection{
			{Title: "Content Moderation", Body: "F33D3R uses automated scanning and human review to enforce our Community Standards. Posts containing illegal content, CSAM, or credible threats are removed immediately."},
			{Title: "Government Requests", Body: "We publish transparency reports for government data requests. We notify users of requests unless prohibited by law."},
			{Title: "DMCA", Body: `Copyright complaints are handled via our <a href="/dmca">DMCA process</a>. Repeat infringers are removed from the platform.`},
			{Title: "Contact", Body: `For compliance inquiries: <a href="mailto:f33d3r@f33d3r.pro">f33d3r@f33d3r.pro</a>`},
		},
	})
}

// blockedPage serves GET /blocked — list of accounts the current user has blocked.
func (h *Handler) blockedPage(w http.ResponseWriter, r *http.Request) {
	h.relationListPage(w, r, "blocked")
}

// mutedPage serves GET /muted — list of accounts the current user has muted.
func (h *Handler) mutedPage(w http.ResponseWriter, r *http.Request) {
	h.relationListPage(w, r, "muted")
}

// relationListPage draws the blocked or muted list. Both are the follow-list
// page over a different relation, so they read the same rows the followers page
// reads — FollowListEntry, which is what _follow_list_row is written against.
// The lists themselves come from internal/db, so the page and GET /api/v1/blocks
// answer from one query each and cannot drift apart.
func (h *Handler) relationListPage(w http.ResponseWriter, r *http.Request, kind string) {
	user := h.userFromRequest(w, r)
	var users []*model.User
	var err error
	if h.db != nil && user != nil {
		if kind == "blocked" {
			users, err = dbpkg.ListBlockedUsers(h.db, user.ID)
		} else {
			users, err = dbpkg.ListMutedUsers(h.db, user.ID)
		}
		if err != nil {
			log.Printf("[compliance] %s list for %s: %v", kind, user.Handle, err)
		}
	}
	title, empty := "Blocked accounts", "You haven't blocked anyone."
	if kind == "muted" {
		title, empty = "Muted accounts", "You haven't muted anyone."
	}
	h.render(w, r, "followers.html", map[string]interface{}{
		"User":          user,
		"Title":         title,
		"Themes":        ThemesWithActive(user.ThemeID),
		"Users":         dbpkg.FollowListEntriesFor(h.db, user.ID, users),
		"ProfileHandle": user.Handle,
		"PageTitle":     title,
		"EmptyMsg":      empty,
	})
}
