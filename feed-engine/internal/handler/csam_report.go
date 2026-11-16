package handler

import (
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
)

// CSAM handling after a match. Detection is Abraxas's, the integrity
// authority: it judges every upload (asked by Caeor) and every live frame
// (asked by the live lane), and publishes each banned match on safety.events.
// The safety consumer (safety_events.go) brings a csam match here: the account
// action, and the report a person files with NCMEC. The account action is
// queued for elohim-veni in identity_commands on the report's own commit and
// delivered by the identity relay. The content is refused and not served, and
// nothing here shows it to anyone.

var (
	csamMatches = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "csam_matches_total",
		Help: "Content matching a csam registry entry, by surface and account action.",
	}, []string{"surface", "action"})
	csamReportFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "csam_report_failures_total",
		Help: "Attempts to record a csam match's report and account action that failed (each is retried).",
	})
)

func init() {
	prometheus.MustRegister(csamMatches, csamReportFailures)
}

// recordCSAMMatch opens the report and applies the account action in one
// transaction. An error is returned, never swallowed: the safety consumer
// retries the event until it is recorded. Recording is idempotent while the
// report is open, so a redelivered match opens nothing new.
func (h *Handler) recordCSAMMatch(m dbpkg.CSAMMatch) error {
	if h.db == nil {
		csamReportFailures.Inc()
		return fmt.Errorf("csam match surface=%s sha256=%s: no database", m.Surface, hashPrefix(m.SHA256))
	}
	result, err := dbpkg.RecordCSAMMatch(h.db, m)
	if err != nil {
		csamReportFailures.Inc()
		log.Printf("[csam-REPORT-FAILED] surface=%s pial=%s sha256=%s: %v — will retry", m.Surface, m.PIALID, hashPrefix(m.SHA256), err)
		return err
	}
	if result.ReportID == "" {
		return nil
	}
	csamMatches.WithLabelValues(m.Surface, result.Action).Inc()
	log.Printf("[csam-REPORT] %s opened: surface=%s action=%s — an admin must file it", result.ReportID, m.Surface, result.Action)
	if m.PIALID != "" {
		// A ban or a hold was queued for elohim-veni with the report; deliver
		// it now and drop this replica's cached sessions of the person.
		h.applyQueuedIdentityAction(result.Accounts)
	}
	return nil
}

// applyQueuedIdentityAction follows a transaction that queued a ban, a hold or
// its release for elohim-veni: the identity command relay is kicked so the
// authority applies it now, not at its next tick, and every cached session of
// the person's accounts is dropped from this replica so none keeps resolving
// with the old capabilities. The authority itself ends the sessions of a ban.
func (h *Handler) applyQueuedIdentityAction(accounts []dbpkg.AccountRef) {
	h.identityRelay.Kick()
	for _, a := range accounts {
		h.evictCachedAccount(a.ID, "")
	}
}

// ── Admin queue ──────────────────────────────────────────────────────────────

func (h *Handler) openCSAMReportCount() int {
	if h.db == nil {
		return 0
	}
	n, err := dbpkg.CountOpenCSAMReports(h.db)
	if err != nil {
		log.Printf("[csam-REPORT] counting queue: %v", err)
	}
	return n
}

func (h *Handler) adminPanelCSAMQueue(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if !user.IsAdmin() {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return
	}
	reports, err := dbpkg.ListCSAMReports(h.db)
	if err != nil {
		log.Printf("[csam-REPORT] listing queue: %v", err)
		http.Error(w, "could not load the report queue", http.StatusInternalServerError)
		return
	}
	open := 0
	for _, report := range reports {
		if report.Status == "pending" {
			open++
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderPartial(w, "admin_panel_csam_queue", map[string]interface{}{"Reports": reports, "Open": open})
}

// adminCSAMReportAction — POST /admin/csam-queue/{id}/{action}
// submit records the NCMEC reference. ban confirms a hold. dismiss closes a hold as a
// false match. Each answers with the report's new row.
func (h *Handler) adminCSAMReportAction(w http.ResponseWriter, r *http.Request) {
	admin := h.userFromRequest(w, r)
	if !admin.IsAdmin() {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return
	}
	parsed, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "report not found", http.StatusNotFound)
		return
	}
	id := parsed.String()
	report, err := dbpkg.GetCSAMReport(h.db, id)
	if err != nil {
		log.Printf("[csam-REPORT] %s: %v", id, err)
		http.Error(w, "could not load the report", http.StatusInternalServerError)
		return
	}
	if report == nil {
		http.Error(w, "report not found", http.StatusNotFound)
		return
	}
	if report.Status != "pending" {
		http.Error(w, "this report is already closed", http.StatusConflict)
		return
	}
	action := r.PathValue("action")
	note := strings.TrimSpace(r.FormValue("note"))
	switch action {
	case "submit":
		ref := strings.TrimSpace(r.FormValue("ncmec_ref"))
		if ref == "" || len(ref) > 100 {
			http.Error(w, "enter the reference NCMEC gave you", http.StatusBadRequest)
			return
		}
		err = dbpkg.MarkCSAMReportSubmitted(h.db, id, ref, admin.Handle)
	case "ban":
		if report.Action != dbpkg.CSAMActionHeld || report.PIALID == "" {
			http.Error(w, "this report has no held account", http.StatusConflict)
			return
		}
		err = dbpkg.BanHeldCSAMAccount(h.db, report, admin.Handle)
	case "dismiss":
		if report.Action != dbpkg.CSAMActionHeld {
			http.Error(w, "only a held account can be released here", http.StatusConflict)
			return
		}
		if note == "" {
			http.Error(w, "say why this is a false match", http.StatusBadRequest)
			return
		}
		err = dbpkg.DismissHeldCSAMReport(h.db, report, note, admin.Handle)
	default:
		http.Error(w, "unknown action", http.StatusNotFound)
		return
	}
	if err != nil {
		log.Printf("[csam-REPORT] %s %s by %s: %v", action, id, admin.Handle, err)
		http.Error(w, "that could not be saved", http.StatusInternalServerError)
		return
	}
	go LogAdminAction(h.db, admin.ID, "csam_report_"+action, "law_enforcement_report", id, requestIP(r), map[string]interface{}{"note": note})

	if report.PIALID != "" {
		accounts, aerr := dbpkg.AccountsOfPIAL(h.db, report.PIALID)
		if aerr != nil {
			log.Printf("[csam-REPORT] %s: accounts of the person: %v", id, aerr)
		}
		h.applyQueuedIdentityAction(accounts)
	}

	updated, err := dbpkg.GetCSAMReport(h.db, id)
	if err != nil || updated == nil {
		log.Printf("[csam-REPORT] %s reload: %v", id, err)
		http.Error(w, fmt.Sprintf("saved, but reload the page to see report %s", id), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderPartial(w, "admin_csam_report", updated)
}
