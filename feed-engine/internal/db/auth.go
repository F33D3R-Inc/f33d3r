package db

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"

	"github.com/f33d3r/feed-engine/internal/herald"
)

// ── User roles ────────────────────────────────────────────────────────────────

type UserRole struct {
	UserID        string
	RoleType      string // user | creator | official | minor_user | minor_creator
	CreatorType   string // musician | streamer | artist | podcaster | athlete | journalist | educator | adult_content_creator | other
	IsAdult       bool
	IsMinor       bool
	IsIDVerified  bool
	IsAgeVerified bool
	ContentPrefs  map[string]bool
	OnboardStep   int
	OnboardDone   bool
}

func GetUserRole(db *sql.DB, userID string) (*UserRole, error) {
	r := &UserRole{UserID: userID, RoleType: "user", ContentPrefs: map[string]bool{}}
	var prefsJSON []byte
	// role / is_adult / is_minor / is_age_verified are read from the identity
	// projection (elohim-veni is the record);
	// creator_type / is_id_verified / content_prefs / onboard_* remain on user_roles.
	err := db.QueryRow(`
		SELECT COALESCE(pr.role,'user'), ur.creator_type,
		       COALESCE(pr.is_adult,FALSE), COALESCE(pr.is_minor,FALSE),
		       ur.is_id_verified, COALESCE(pr.age_verified,FALSE), ur.content_prefs,
		       ur.onboard_step, ur.onboard_done
		FROM user_roles ur
		JOIN users u ON u.id = ur.user_id
		LEFT JOIN identity_facts pr ON pr.pial_id = u.pial_id
		WHERE ur.user_id = $1
	`, userID).Scan(
		&r.RoleType, &r.CreatorType, &r.IsAdult, &r.IsMinor,
		&r.IsIDVerified, &r.IsAgeVerified, &prefsJSON,
		&r.OnboardStep, &r.OnboardDone,
	)
	if err == sql.ErrNoRows {
		return r, nil // return default
	}
	return r, err
}

// UpsertUserRole writes the user_roles columns that remain local to feed-engine.
// role / is_adult / is_minor / is_age_verified are identity facts owned by
// elohim-veni and recorded there at signup — never here.
func UpsertUserRole(db *sql.DB, r *UserRole) error {
	_, err := db.Exec(`
		INSERT INTO user_roles
		  (user_id, creator_type, is_id_verified, onboard_step, onboard_done)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (user_id) DO UPDATE SET
		  creator_type   = EXCLUDED.creator_type,
		  is_id_verified = EXCLUDED.is_id_verified,
		  onboard_step   = EXCLUDED.onboard_step,
		  onboard_done   = EXCLUDED.onboard_done,
		  updated_at     = NOW()
	`,
		r.UserID, r.CreatorType, r.IsIDVerified, r.OnboardStep, r.OnboardDone,
	)
	return err
}

// ── Notifications ─────────────────────────────────────────────────────────────

type Notification struct {
	ID          string
	Type        string // like | follow | reply | mention | message | system | tip | milestone
	ActorID     string
	ActorName   string
	ActorHandle string
	ActorAvatar string
	TargetID    string
	TargetType  string
	Payload     map[string]interface{}
	IsRead      bool
	TimeAgo     string
	Others      int    // other people in this grouped entry
	Preview     string // the words the notification is about
	AmountAET   string // formatted tip amount, "" when there is none
	Href        string // where the entry leads, "" when nowhere
	CreatedAt   time.Time
}

func MarkAllNotificationsRead(db *sql.DB, userID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), heraldTimeout)
	defer cancel()
	return herald.MarkRead(ctx, userID, nil, true)
}

// CountUnreadNotifications asks Herald, which owns the rows. While Herald cannot
// answer, the last count it announced stands in.
func CountUnreadNotifications(db *sql.DB, userID string) int {
	ctx, cancel := context.WithTimeout(context.Background(), heraldTimeout)
	defer cancel()
	count, err := herald.Unread(ctx, userID)
	if err != nil {
		log.Printf("[notifications] unread for %s: %v", userID, err)
		_ = db.QueryRow(`SELECT COALESCE(unread_count, 0) FROM users WHERE id = $1`, userID).Scan(&count)
	}
	return count
}

// GetUserEmail returns the email for a user, or "" if unset.
func GetUserEmail(db *sql.DB, userID string) (string, error) {
	var email string
	if err := db.QueryRow(`SELECT COALESCE(email, '') FROM users WHERE id = $1`, userID).Scan(&email); err != nil {
		return "", fmt.Errorf("reading the email address of account %s: %w", userID, err)
	}
	return email, nil
}
