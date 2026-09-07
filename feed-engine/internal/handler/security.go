package handler

// security.go — cybersecurity threat detection for F33D3R.
// Provides functions for logging security events, checking blocked IPs,
// recording admin audit actions, and running threat detection rules.
// Threat detection is designed to be called non-blocking (goroutine) from middleware.

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/f33d3r/feed-engine/internal/middleware"
	"github.com/f33d3r/feed-engine/internal/model"
)

// alerter is set once at startup. Called whenever a high/critical security event fires.
// No-op until InitSecurityAlerter is called.
var alerter func(eventType, severity, ip, details string)

// InitSecurityAlerter wires in the alert sink (email/webhook). Safe to call before any events fire.
func InitSecurityAlerter(fn func(eventType, severity, ip, details string)) {
	alerter = fn
}

// requestIP is the client address every security decision in this package is
// keyed on. It is middleware.ClientIP: the edge's forwarding headers are
// honoured only when the TCP peer is on the container network, so a caller
// that reaches this process directly cannot name its own address and walk
// past an IP block or a rate limit.
func requestIP(r *http.Request) string {
	return middleware.ClientIP(r)
}

// loginAccountKey names the account a login-shaped POST is acting on, for the
// per-account limiter: the handle in the form, normalised the way handleLogin
// normalises it, so the limiter and the handler agree on which account this is.
func loginAccountKey(r *http.Request) string {
	_ = r.ParseForm()
	return strings.TrimSpace(strings.ToLower(r.FormValue("handle")))
}

// sessionAccountKey names the account behind an authenticated request, for the
// per-account limiter on the 2FA routes: a TOTP guess is charged to the session
// making it, not to whichever address it came from.
func sessionAccountKey(r *http.Request) string {
	return HandleFromCookie(r)
}

// ── In-memory IP block cache ─────────────────────────────────────────────────
// Refreshed every 30 seconds so hot-path requests avoid a DB round-trip.

type ipBlockCache struct {
	mu        sync.RWMutex
	blocked   map[string]time.Time // ip → expires_at (zero = permanent)
	lastFetch time.Time
}

var globalIPBlockCache = &ipBlockCache{blocked: make(map[string]time.Time)}

func (c *ipBlockCache) refresh(db *sql.DB) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if db == nil {
		return
	}
	rows, err := db.Query(`
		SELECT ip_address, expires_at
		FROM blocked_ips
		WHERE expires_at IS NULL OR expires_at > NOW()`)
	if err != nil {
		log.Printf("[security] failed to refresh IP block cache: %v", err)
		return
	}
	defer rows.Close()
	fresh := make(map[string]time.Time)
	for rows.Next() {
		var ip string
		var exp *time.Time
		rows.Scan(&ip, &exp)
		if exp == nil {
			fresh[ip] = time.Time{} // zero = permanent
		} else {
			fresh[ip] = *exp
		}
	}
	c.blocked = fresh
	c.lastFetch = time.Now()
}

func (c *ipBlockCache) isBlocked(ip string) bool {
	c.mu.RLock()
	exp, ok := c.blocked[ip]
	c.mu.RUnlock()
	if !ok {
		return false
	}
	if exp.IsZero() {
		return true // permanent
	}
	return time.Now().Before(exp)
}

func (c *ipBlockCache) add(ip string, exp *time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if exp == nil {
		c.blocked[ip] = time.Time{}
	} else {
		c.blocked[ip] = *exp
	}
}

func (c *ipBlockCache) remove(ip string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.blocked, ip)
}

// maybeRefreshCache triggers a cache refresh if it is stale (> 30 s).
// Called on every IsIPBlocked call — lightweight because it takes an RLock first.
func maybeRefreshCache(db *sql.DB) {
	globalIPBlockCache.mu.RLock()
	stale := time.Since(globalIPBlockCache.lastFetch) > 30*time.Second
	globalIPBlockCache.mu.RUnlock()
	if stale {
		go globalIPBlockCache.refresh(db)
	}
}

// ── Public API ────────────────────────────────────────────────────────────────

// IsIPBlocked returns true if the given IP is in the blocked_ips table and
// either has no expiry or has not yet expired. Uses an in-memory cache refreshed
// every 30 s — safe to call on every non-static request.
func IsIPBlocked(db *sql.DB, ip string) bool {
	maybeRefreshCache(db)
	return globalIPBlockCache.isBlocked(ip)
}

// LogSecurityEvent inserts a row into security_events and fires the alerter for
// critical/high events. Non-critical path; errors logged but not propagated. Safe to call in a goroutine.
func LogSecurityEvent(db *sql.DB, eventType, severity, userID, ip, ua, path string, details map[string]interface{}) {
	if db == nil {
		return
	}
	raw, _ := json.Marshal(details)
	var uid *string
	if userID != "" {
		uid = &userID
	}
	_, err := db.Exec(`
		INSERT INTO security_events (event_type, severity, user_id, ip_address, user_agent, path, details)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		eventType, severity, uid, ip, ua, path, raw)
	if err != nil {
		log.Printf("[security] LogSecurityEvent error: %v", err)
	}
	if (severity == "critical" || severity == "high") && alerter != nil {
		detailStr := string(raw)
		go alerter(eventType, severity, ip, detailStr)
	}
}

// LogAdminAction inserts a row into admin_audit_log. Safe to call in a goroutine.
func LogAdminAction(db *sql.DB, adminID, action, targetType, targetID, ip string, details map[string]interface{}) {
	if db == nil {
		return
	}
	raw, _ := json.Marshal(details)
	_, err := db.Exec(`
		INSERT INTO admin_audit_log (admin_id, action, target_type, target_id, details, ip_address)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		adminID, action, targetType, targetID, raw, ip)
	if err != nil {
		log.Printf("[security] LogAdminAction error: %v", err)
	}
}

// BlockIP inserts or replaces a row in blocked_ips and updates the in-memory cache.
func BlockIP(db *sql.DB, ip, reason, adminID string, autoBlocked bool) error {
	if db == nil {
		return nil
	}
	var aid *string
	if adminID != "" {
		aid = &adminID
	}
	_, err := db.Exec(`
		INSERT INTO blocked_ips (ip_address, reason, blocked_by, auto_blocked)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (ip_address) DO UPDATE
		  SET reason = EXCLUDED.reason,
		      blocked_by = EXCLUDED.blocked_by,
		      auto_blocked = EXCLUDED.auto_blocked,
		      expires_at = NULL,
		      created_at = NOW()`,
		ip, reason, aid, autoBlocked)
	if err != nil {
		return err
	}
	globalIPBlockCache.add(ip, nil)
	return nil
}

// UnblockIP removes a row from blocked_ips and from the in-memory cache.
func UnblockIP(db *sql.DB, ip string) error {
	if db == nil {
		return nil
	}
	_, err := db.Exec(`DELETE FROM blocked_ips WHERE ip_address = $1`, ip)
	if err != nil {
		return err
	}
	globalIPBlockCache.remove(ip)
	return nil
}

// GetSecurityEvents returns the most recent security events ordered by created_at DESC.
func GetSecurityEvents(db *sql.DB, limit int) []model.SecurityEvent {
	if db == nil {
		return nil
	}
	rows, err := db.Query(`
		SELECT id, event_type, severity, COALESCE(user_id::text,''), ip_address, user_agent, path, details, created_at
		FROM security_events
		ORDER BY created_at DESC LIMIT $1`, limit)
	if err != nil {
		log.Printf("[security] GetSecurityEvents: %v", err)
		return nil
	}
	defer rows.Close()
	var out []model.SecurityEvent
	for rows.Next() {
		var e model.SecurityEvent
		var detailsRaw []byte
		rows.Scan(&e.ID, &e.EventType, &e.Severity, &e.UserID, &e.IPAddress, &e.UserAgent, &e.Path, &detailsRaw, &e.CreatedAt)
		json.Unmarshal(detailsRaw, &e.Details)
		out = append(out, e)
	}
	return out
}

// GetBlockedIPs returns all currently active blocked IPs.
func GetBlockedIPs(db *sql.DB) []model.BlockedIP {
	if db == nil {
		return nil
	}
	rows, err := db.Query(`
		SELECT ip_address, reason, auto_blocked, created_at, expires_at
		FROM blocked_ips
		WHERE expires_at IS NULL OR expires_at > NOW()
		ORDER BY created_at DESC`)
	if err != nil {
		log.Printf("[security] GetBlockedIPs: %v", err)
		return nil
	}
	defer rows.Close()
	var out []model.BlockedIP
	for rows.Next() {
		var b model.BlockedIP
		rows.Scan(&b.IPAddress, &b.Reason, &b.AutoBlocked, &b.CreatedAt, &b.ExpiresAt)
		out = append(out, b)
	}
	return out
}

// GetAdminAuditLog returns the most recent admin audit entries with handle joined.
func GetAdminAuditLog(db *sql.DB, limit int) []model.AdminAuditEntry {
	if db == nil {
		return nil
	}
	// admin_id is released to NULL when the admin's own account is purged
	// (migration 0018: the audit trail outlives the auditor), so both the id and
	// the joined handle are COALESCEd. The entry then reads as an action taken by
	// an account that no longer exists, which is exactly what happened.
	rows, err := db.Query(`
		SELECT a.id, COALESCE(a.admin_id::text,''), COALESCE(u.handle,''), a.action, a.target_type, a.target_id, a.ip_address, a.created_at
		FROM admin_audit_log a
		LEFT JOIN users u ON u.id = a.admin_id
		ORDER BY a.created_at DESC LIMIT $1`, limit)
	if err != nil {
		log.Printf("[security] GetAdminAuditLog: %v", err)
		return nil
	}
	defer rows.Close()
	var out []model.AdminAuditEntry
	for rows.Next() {
		var e model.AdminAuditEntry
		rows.Scan(&e.ID, &e.AdminID, &e.AdminHandle, &e.Action, &e.TargetType, &e.TargetID, &e.IPAddress, &e.CreatedAt)
		out = append(out, e)
	}
	return out
}

// PendingSecurityCount returns the number of critical or high events in the last hour.
func PendingSecurityCount(db *sql.DB) int {
	if db == nil {
		return 0
	}
	var n int
	db.QueryRow(`
		SELECT COUNT(*) FROM security_events
		WHERE severity IN ('critical','high') AND created_at > NOW() - INTERVAL '1 hour'`).Scan(&n)
	return n
}

// ── Threat detection helpers ──────────────────────────────────────────────────
// These are called from a goroutine in middleware so they never block the response.

// sqlInjectionSignatures are simple patterns that match obvious SQL injection attempts.
// Not exhaustive — sophisticated WAF belongs in infrastructure, not app layer.
var sqlInjectionSignatures = []string{
	"UNION SELECT", "union select",
	"DROP TABLE", "drop table",
	"1=1", "' OR '", "\" OR \"",
	"--", "xp_cmdshell", "EXEC(", "exec(",
}

// CheckSQLInjection logs a medium severity event if the request path or body
// contains known SQL injection signatures.
func CheckSQLInjection(db *sql.DB, r *http.Request, userID, ip string) {
	target := r.URL.RawPath + r.URL.RawQuery
	if r.Body != nil {
		// Already read by middleware — use form values which have been parsed
		for _, v := range r.Form {
			target += " " + strings.Join(v, " ")
		}
	}
	for _, sig := range sqlInjectionSignatures {
		if strings.Contains(target, sig) {
			go LogSecurityEvent(db, "suspicious_request", "medium", userID, ip,
				r.UserAgent(), r.URL.Path,
				map[string]interface{}{"signature": sig, "path": r.URL.Path, "query": r.URL.RawQuery})
			return
		}
	}
}

// CheckBruteForce counts recent login failures from an IP and auto-blocks if threshold exceeded.
// Threshold: >5 login failures from same IP in 5 minutes → block + critical event.
func CheckBruteForce(db *sql.DB, ip string) {
	if db == nil {
		return
	}
	var count int
	db.QueryRow(`
		SELECT COUNT(*) FROM security_events
		WHERE event_type = 'login_failed'
		  AND ip_address = $1
		  AND created_at > NOW() - INTERVAL '5 minutes'`, ip).Scan(&count)
	if count > 5 {
		if !globalIPBlockCache.isBlocked(ip) {
			// A block that failed to persist is no block at all, and the log line
			// below would otherwise claim one was applied.
			if err := BlockIP(db, ip, "brute force: >5 login failures in 5 minutes", "", true); err != nil {
				log.Printf("[security] brute force detected from %s (%d failures in 5m) but the IP block FAILED to apply: %v",
					ip, count, err)
				return
			}
			go LogSecurityEvent(db, "brute_force", "critical", "", ip, "", "",
				map[string]interface{}{"failed_attempts": count, "window": "5m"})
			log.Printf("[security] brute force detected — auto-blocked %s (%d failures in 5m)", ip, count)
		}
	}
}

// CheckBruteForceHandle counts recent login failures against a specific handle and
// temporarily locks it after 10 failures in 15 minutes. Uses security_events so the
// window survives restarts.
func CheckBruteForceHandle(db *sql.DB, handle, ip string) {
	if db == nil {
		return
	}
	var count int
	db.QueryRow(`
		SELECT COUNT(*) FROM security_events
		WHERE event_type = 'login_failed'
		  AND details->>'handle' = $1
		  AND created_at > NOW() - INTERVAL '15 minutes'`, handle).Scan(&count)
	if count >= 10 {
		go LogSecurityEvent(db, "account_lockout", "high", "", ip, "", "",
			map[string]interface{}{"handle": handle, "failed_attempts": count, "window": "15m"})
		log.Printf("[security] account lockout triggered — handle @%s (%d failures in 15m from %s)", handle, count, ip)
	}
}

// IsAccountLocked returns true if a handle has been locked out within the last 30 minutes.
// The lockout is recorded as an 'account_lockout' event by CheckBruteForceHandle.
func IsAccountLocked(db *sql.DB, handle string) bool {
	if db == nil {
		return false
	}
	var count int
	db.QueryRow(`
		SELECT COUNT(*) FROM security_events
		WHERE event_type = 'account_lockout'
		  AND details->>'handle' = $1
		  AND created_at > NOW() - INTERVAL '30 minutes'`, handle).Scan(&count)
	return count > 0
}

// RegisterDevice records a device fingerprint for the user on successful login.
// If the device is new, a new_device_login security event is fired.
// device_hash is sha256(user-agent) — simple but catches bots and browser switches.
func RegisterDevice(db *sql.DB, userID, ua, ip string) {
	if db == nil || userID == "" {
		return
	}
	h := sha256.Sum256([]byte(ua))
	hash := fmt.Sprintf("%x", h)

	var existing int
	db.QueryRow(`SELECT COUNT(*) FROM user_devices WHERE user_id = $1 AND device_hash = $2`,
		userID, hash).Scan(&existing)

	if existing == 0 {
		db.Exec(`
			INSERT INTO user_devices (user_id, device_hash, ip_address, user_agent)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (user_id, device_hash) DO NOTHING`,
			userID, hash, ip, ua)
		go LogSecurityEvent(db, "new_device_login", "medium", userID, ip, ua, "/login",
			map[string]interface{}{"device_hash": hash})
		log.Printf("[security] new device login — user %s from %s", userID, ip)
	} else {
		db.Exec(`UPDATE user_devices SET last_seen = NOW(), ip_address = $3
			WHERE user_id = $1 AND device_hash = $2`, userID, hash, ip)
	}
}

// CheckBulkAction counts recent action events for a user and logs a high event if threshold exceeded.
// Threshold: >50 like/follow/post events from same user in 60 seconds.
func CheckBulkAction(db *sql.DB, userID, ip string) {
	if db == nil || userID == "" {
		return
	}
	var count int
	db.QueryRow(`
		SELECT COUNT(*) FROM security_events
		WHERE event_type IN ('like','follow','post')
		  AND user_id = $1::uuid
		  AND created_at > NOW() - INTERVAL '60 seconds'`, userID).Scan(&count)
	if count > 50 {
		go LogSecurityEvent(db, "bulk_action", "high", userID, ip, "", "",
			map[string]interface{}{"action_count": count, "window": "60s"})
		log.Printf("[security] bulk action detected — user %s performed %d actions in 60s from %s", userID, count, ip)
	}
}

// ── Admin panel handlers ──────────────────────────────────────────────────────

// adminPanelSecurity renders the Security tab panel for HTMX swap.
func (h *Handler) adminPanelSecurity(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if !user.IsAdmin() {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return
	}

	var todayCount, criticalCount, blockedCount, auditCount int
	if h.db != nil {
		h.db.QueryRow(`SELECT COUNT(*) FROM security_events WHERE created_at > NOW() - INTERVAL '24 hours'`).Scan(&todayCount)
		h.db.QueryRow(`SELECT COUNT(*) FROM security_events WHERE severity = 'critical' AND created_at > NOW() - INTERVAL '24 hours'`).Scan(&criticalCount)
		h.db.QueryRow(`SELECT COUNT(*) FROM blocked_ips WHERE expires_at IS NULL OR expires_at > NOW()`).Scan(&blockedCount)
		h.db.QueryRow(`SELECT COUNT(*) FROM admin_audit_log WHERE created_at > NOW() - INTERVAL '24 hours'`).Scan(&auditCount)
	}

	recentEvents := GetSecurityEvents(h.db, 100)
	blockedIPs := GetBlockedIPs(h.db)
	auditLog := GetAdminAuditLog(h.db, 50)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderPartial(w, "admin_panel_security", map[string]interface{}{
		"TotalEventsToday": todayCount,
		"CriticalEvents":   criticalCount,
		"BlockedIPCount":   blockedCount,
		"AdminActionCount": auditCount,
		"RecentEvents":     recentEvents,
		"BlockedIPs":       blockedIPs,
		"AuditLog":         auditLog,
	})
}

// adminBlockIP handles POST /api/admin/security/block-ip
func (h *Handler) adminBlockIP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "405", http.StatusMethodNotAllowed)
		return
	}
	caller := h.userFromRequest(w, r)
	if !caller.IsAdmin() {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return
	}
	_ = r.ParseForm()
	ip := strings.TrimSpace(r.FormValue("ip_address"))
	reason := strings.TrimSpace(r.FormValue("reason"))
	if ip == "" {
		http.Error(w, "ip_address required", http.StatusBadRequest)
		return
	}
	if reason == "" {
		reason = "manual block by admin"
	}

	callerID := ""
	if caller != nil {
		callerID = caller.ID
	}

	if err := BlockIP(h.db, ip, reason, callerID, false); err != nil {
		log.Printf("[security] adminBlockIP error: %v", err)
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}

	go LogAdminAction(h.db, callerID, "block_ip", "ip", ip, requestIP(r),
		map[string]interface{}{"reason": reason})
	go LogSecurityEvent(h.db, "ip_blocked", "medium", callerID, ip, r.UserAgent(), r.URL.Path,
		map[string]interface{}{"reason": reason, "by": caller.Handle})

	// Re-render the panel so the admin sees the updated blocked IPs list
	h.adminPanelSecurity(w, r)
}

// adminUnblockIP handles POST /api/admin/security/unblock-ip
func (h *Handler) adminUnblockIP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "405", http.StatusMethodNotAllowed)
		return
	}
	caller := h.userFromRequest(w, r)
	if !caller.IsAdmin() {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return
	}
	_ = r.ParseForm()
	ip := strings.TrimSpace(r.FormValue("ip_address"))
	if ip == "" {
		http.Error(w, "ip_address required", http.StatusBadRequest)
		return
	}

	callerID := ""
	if caller != nil {
		callerID = caller.ID
	}

	if err := UnblockIP(h.db, ip); err != nil {
		log.Printf("[security] adminUnblockIP error: %v", err)
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}

	go LogAdminAction(h.db, callerID, "unblock_ip", "ip", ip, requestIP(r),
		map[string]interface{}{"by": caller.Handle})

	h.adminPanelSecurity(w, r)
}
