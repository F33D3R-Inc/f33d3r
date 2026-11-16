package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/f33d3r/feed-engine/internal/identity"
	"github.com/f33d3r/feed-engine/internal/model"
)

// Account actions a CSAM match leads to. An exact match on bytes the server received
// ends the account. Anything a person has to judge holds it until they do: perceptual
// hashes can collide on low-detail media, and a digest alone was only a claim.
const (
	CSAMActionBanned = "banned"
	CSAMActionHeld   = "held"
)

// csamHoldCapabilities are what a hold stops: putting anything new in front of people.
var csamHoldCapabilities = []string{model.CapPosting, model.CapMusicUpload}

// AccountRef is one account under a PIAL.
type AccountRef struct {
	ID     string
	Handle string
}

// AccountsOfPIAL lists every account belonging to the person. Capabilities live on the
// PIAL, so an action on one account is an action on all of them.
func AccountsOfPIAL(database DBTX, pialID string) ([]AccountRef, error) {
	rows, err := database.Query(`SELECT id::text, handle FROM users WHERE pial_id::text = $1 ORDER BY created_at`, pialID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AccountRef
	for rows.Next() {
		var a AccountRef
		if err := rows.Scan(&a.ID, &a.Handle); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// BanAccount is the one way a person is banned. It runs inside the caller's
// transaction, because a ban that half-applied is the worst of both: this
// brain's enforcement record and trust state are written, and the ban itself —
// every account, every capability, every session — is queued for elohim-veni,
// which owns it, on the same commit. The identity command relay delivers it;
// the caller kicks the relay after commit.
func BanAccount(tx *sql.Tx, pialID, reason, actor string, evidence map[string]interface{}) error {
	if err := SetEnforcementState(tx, pialID, 0, "terminated"); err != nil {
		return err
	}
	if err := EnqueueIdentityCommand(tx, identity.CommandBan, pialID, identity.StandingCommand{Reason: reason, By: actor}); err != nil {
		return err
	}
	LogEnforcementAction(tx, pialID, "ban", reason, actor, evidence)
	LogPIALEvent(tx, pialID, "", "account_banned", map[string]interface{}{"reason": reason, "by": actor}, "moderation")
	return nil
}

// queueCapabilities queues one capability state for every named capability.
func queueCapabilities(tx *sql.Tx, pialID string, capabilities []string, state, reason, by string) error {
	for _, capability := range capabilities {
		if err := EnqueueIdentityCommand(tx, identity.CommandCapability, pialID, identity.CapabilityCommand{
			Capability: capability,
			Write:      identity.CapabilityWrite{State: state, Reason: reason, By: by},
		}); err != nil {
			return err
		}
	}
	return nil
}

// SetEnforcementState moves a person's trust level and state, keeping their NSFW tier.
func SetEnforcementState(tx *sql.Tx, pialID string, trustLevel int, state string) error {
	var nsfwTier int
	err := tx.QueryRow(`SELECT nsfw_tier FROM trust_scores WHERE pial_id::text = $1`, pialID).Scan(&nsfwTier)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return SetTrustScore(tx, pialID, trustLevel, nsfwTier, state)
}

// CSAMMatch is one piece of content that matched a csam entry in the banned registry.
type CSAMMatch struct {
	PIALID   string
	Surface  string // upload | live
	MediaRef string // the name it arrived under; the content itself is never stored
	HashType string
	SHA256   string
	Exact    bool // exact bytes the server itself received
}

// SessionEvidence is what is known about where an account was signed in from: the
// devices this brain saw it sign in on. A ban ends the sessions at elohim-veni, so
// what is known here is copied to the report first.
type SessionEvidence struct {
	Handle     string    `json:"handle"`
	IPAddress  string    `json:"ip_address"`
	DeviceName string    `json:"device_name"`
	LastSeenAt time.Time `json:"last_seen_at"`
}

// CSAMQueueResult says what recording a match did.
type CSAMQueueResult struct {
	ReportID string // empty when this person's report for this content was already open
	Action   string
	Accounts []AccountRef
}

// RecordCSAMMatch applies the account action and opens the report in one transaction,
// and tells the admins there is a report waiting. Neither half stands without the other.
func RecordCSAMMatch(database *sql.DB, m CSAMMatch) (CSAMQueueResult, error) {
	result := CSAMQueueResult{Action: CSAMActionHeld}
	if m.Exact {
		result.Action = CSAMActionBanned
	}
	err := WithTx(database, func(tx *sql.Tx) error {
		if m.PIALID != "" {
			accounts, err := AccountsOfPIAL(tx, m.PIALID)
			if err != nil {
				return err
			}
			result.Accounts = accounts
		}
		handles := make([]string, 0, len(result.Accounts))
		for _, a := range result.Accounts {
			handles = append(handles, a.Handle)
		}
		evidence, err := sessionEvidence(tx, m.PIALID)
		if err != nil {
			return err
		}
		evidenceJSON, err := json.Marshal(evidence)
		if err != nil {
			return err
		}
		err = tx.QueryRow(`
			INSERT INTO law_enforcement_queue
			  (pial_id, media_url, report_type, account_handles, surface, hash_type, content_sha256, account_action, session_evidence)
			VALUES ($1, $2, 'csam', $3, $4, $5, $6, $7, $8)
			ON CONFLICT (pial_id, content_sha256, surface) WHERE status = 'pending' DO NOTHING
			RETURNING id::text`,
			m.PIALID, m.MediaRef, strings.Join(handles, ", "), m.Surface, m.HashType, m.SHA256, result.Action, string(evidenceJSON),
		).Scan(&result.ReportID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil // Already open: its account action already happened.
		}
		if err != nil {
			return err
		}
		if m.PIALID != "" {
			detail := map[string]interface{}{"report_id": result.ReportID, "hash_type": m.HashType, "surface": m.Surface}
			if m.Exact {
				if err := BanAccount(tx, m.PIALID, "csam_enforcement", "system", detail); err != nil {
					return err
				}
			} else if err := HoldAccountForCSAMReview(tx, m.PIALID, detail); err != nil {
				return err
			}
		}
		for _, admin := range GetAdminUserIDs(tx) {
			if _, err := WriteNotice(tx, Notice{
				Recipient: admin, Kind: "moderation", TargetID: result.ReportID, TargetType: "law_enforcement_report",
			}); err != nil {
				return err
			}
		}
		return nil
	})
	return result, err
}

// HoldAccountForCSAMReview stops a person posting or going live until an admin decides.
func HoldAccountForCSAMReview(tx *sql.Tx, pialID string, evidence map[string]interface{}) error {
	if err := SetEnforcementState(tx, pialID, 20, "suspended"); err != nil {
		return err
	}
	if err := queueCapabilities(tx, pialID, csamHoldCapabilities, model.CapStateRevoked, "csam_review_hold", "system"); err != nil {
		return err
	}
	LogEnforcementAction(tx, pialID, "csam_review_hold", "csam_review_hold", "system", evidence)
	return nil
}

func sessionEvidence(tx *sql.Tx, pialID string) ([]SessionEvidence, error) {
	out := []SessionEvidence{}
	if pialID == "" {
		return out, nil
	}
	rows, err := tx.Query(`
		SELECT u.handle, COALESCE(d.ip_address::text, ''), COALESCE(d.user_agent, ''), d.last_seen
		FROM user_devices d JOIN users u ON u.id = d.user_id
		WHERE u.pial_id::text = $1
		ORDER BY d.last_seen DESC LIMIT 50`, pialID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var e SessionEvidence
		if err := rows.Scan(&e.Handle, &e.IPAddress, &e.DeviceName, &e.LastSeenAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// CSAMReport is one row of the queue, as an admin reads it.
type CSAMReport struct {
	ID         string
	PIALID     string
	Handles    string
	Surface    string
	MediaRef   string
	HashType   string
	SHA256     string
	Action     string
	Status     string
	ReportedAt time.Time
	NCMECRef   string
	ResolvedBy string
	ResolvedAt *time.Time
	Note       string
	Sessions   []SessionEvidence
}

const csamReportColumns = `id::text, pial_id, account_handles, surface, media_url, hash_type, content_sha256,
	account_action, status, reported_at, COALESCE(ncmec_ref, ''), resolved_by, resolved_at,
	resolution_note, session_evidence`

func scanCSAMReport(scan func(...interface{}) error) (CSAMReport, error) {
	var r CSAMReport
	var sessions []byte
	err := scan(&r.ID, &r.PIALID, &r.Handles, &r.Surface, &r.MediaRef, &r.HashType, &r.SHA256, &r.Action, &r.Status,
		&r.ReportedAt, &r.NCMECRef, &r.ResolvedBy, &r.ResolvedAt, &r.Note, &sessions)
	if err != nil {
		return r, err
	}
	err = json.Unmarshal(sessions, &r.Sessions)
	return r, err
}

// ListCSAMReports returns open reports oldest first, then the most recently closed.
func ListCSAMReports(database *sql.DB) ([]CSAMReport, error) {
	rows, err := database.Query(`
		SELECT ` + csamReportColumns + ` FROM (
		  (SELECT * FROM law_enforcement_queue WHERE status = 'pending' ORDER BY reported_at ASC)
		  UNION ALL
		  (SELECT * FROM law_enforcement_queue WHERE status <> 'pending' ORDER BY resolved_at DESC NULLS LAST LIMIT 25)
		) q`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CSAMReport
	for rows.Next() {
		r, err := scanCSAMReport(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetCSAMReport returns one report, or nil when there is no such report.
func GetCSAMReport(database *sql.DB, id string) (*CSAMReport, error) {
	r, err := scanCSAMReport(database.QueryRow(`SELECT `+csamReportColumns+` FROM law_enforcement_queue WHERE id::text = $1`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// CountOpenCSAMReports is how many reports still need a person.
func CountOpenCSAMReports(database *sql.DB) (int, error) {
	var n int
	err := database.QueryRow(`SELECT COUNT(*) FROM law_enforcement_queue WHERE status = 'pending'`).Scan(&n)
	return n, err
}

// MarkCSAMReportSubmitted records the reference NCMEC gave for a filed report.
func MarkCSAMReportSubmitted(database *sql.DB, id, ncmecRef, admin string) error {
	return closeCSAMReport(database, id, "submitted", ncmecRef, "", admin)
}

// DismissHeldCSAMReport closes a report an admin judged to be a false match, and lifts
// the hold. Only a hold can be lifted this way: a ban came from exact bytes.
func DismissHeldCSAMReport(database *sql.DB, r *CSAMReport, note, admin string) error {
	return WithTx(database, func(tx *sql.Tx) error {
		if err := closeCSAMReportTx(tx, r.ID, "dismissed", "", note, admin); err != nil {
			return err
		}
		if r.PIALID == "" {
			return nil
		}
		if err := SetEnforcementState(tx, r.PIALID, 50, "none"); err != nil {
			return err
		}
		if err := queueCapabilities(tx, r.PIALID, csamHoldCapabilities, model.CapStateGranted, "csam_review_dismissed", admin); err != nil {
			return err
		}
		LogEnforcementAction(tx, r.PIALID, "csam_review_dismissed", note, admin, map[string]interface{}{"report_id": r.ID})
		return nil
	})
}

// BanHeldCSAMAccount is an admin confirming a held match. The report stays open until
// it has been filed.
func BanHeldCSAMAccount(database *sql.DB, r *CSAMReport, admin string) error {
	return WithTx(database, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`UPDATE law_enforcement_queue SET account_action = $2 WHERE id::text = $1`, r.ID, CSAMActionBanned); err != nil {
			return err
		}
		return BanAccount(tx, r.PIALID, "csam_enforcement", admin, map[string]interface{}{"report_id": r.ID})
	})
}

func closeCSAMReport(database *sql.DB, id, status, ncmecRef, note, admin string) error {
	return WithTx(database, func(tx *sql.Tx) error { return closeCSAMReportTx(tx, id, status, ncmecRef, note, admin) })
}

func closeCSAMReportTx(tx *sql.Tx, id, status, ncmecRef, note, admin string) error {
	res, err := tx.Exec(`
		UPDATE law_enforcement_queue
		SET status = $2, ncmec_ref = $3, resolution_note = $4, resolved_by = $5, resolved_at = NOW()
		WHERE id::text = $1 AND status = 'pending'`, id, status, ncmecRef, note, admin)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return errors.New("report is not open")
	}
	return nil
}
