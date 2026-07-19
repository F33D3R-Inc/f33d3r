package gnosis

import (
	"database/sql"
	"strings"
	"time"

	"github.com/lib/pq"
)

// Member is a participant identity: the account is the messaging anchor, the PIAL
// is recorded for the 18+ gate.
type Member struct {
	AccountID string
	PIALID    string
}

// Conversation mirrors a gnosis_conversations row.
type Conversation struct {
	ID        string
	Mode      string
	CreatedBy string
	IsGroup   bool
	Title     string
	CreatedAt time.Time
}

// Message mirrors a gnosis_messages row. In plain mode Body holds server-readable
// text; in sealed mode Body is empty and BodyCtB64/BodyNonceB64 carry the envelope
// body, with the viewer's per-recipient wrapped key in Eph/Sealed/SealedNonce.
type Message struct {
	ID             string
	ConversationID string
	SenderAccount  string
	Mode           string
	Body           string
	BodyCtB64      string
	BodyNonceB64   string
	// sealed mode, viewer-specific (filled by ListMessages from gnosis_sealed_keys)
	EphPubB64      string
	SealedB64      string
	SealedNonceB64 string
	CreatedAt      time.Time
}

// SealedKey is one recipient's wrapped content key for a sealed message. Keyed by
// the recipient's ACCOUNT (each persona is its own sealed island).
type SealedKey struct {
	RecipientAccount string
	EphPubB64        string
	SealedB64        string
	SealedNonceB64   string
}

// ConvoListRow is a sidebar conversation row for one viewer.
type ConvoListRow struct {
	ConversationID string
	Mode           string
	IsGroup        bool
	Title          string
	OtherHandle    string
	OtherDisplay   string
	OtherAvatar    string
	Preview        string // server-side preview: plaintext (plain) or lock placeholder (sealed)
	LastAt         time.Time
	Unread         int
}

// CreateConversation inserts a conversation with mode (already decided by the
// caller via ModeForParticipants) and its members, in one transaction. mode is
// written exactly once here; nothing ever UPDATEs it.
func CreateConversation(database *sql.DB, createdBy string, members []Member, mode string) (string, error) {
	tx, err := database.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	var convoID string
	isGroup := len(members) > 2
	if err = tx.QueryRow(`
		INSERT INTO gnosis_conversations (mode, created_by, is_group)
		VALUES ($1, $2, $3) RETURNING id`,
		mode, createdBy, isGroup).Scan(&convoID); err != nil {
		return "", err
	}
	for _, m := range members {
		if _, err = tx.Exec(`
			INSERT INTO gnosis_members (conversation_id, account_id, pial_id)
			VALUES ($1, $2, $3) ON CONFLICT (conversation_id, account_id) DO NOTHING`,
			convoID, m.AccountID, m.PIALID); err != nil {
			return "", err
		}
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	return convoID, nil
}

// GetConversation returns a conversation by id, or sql.ErrNoRows if absent.
func GetConversation(database *sql.DB, id string) (*Conversation, error) {
	c := &Conversation{}
	err := database.QueryRow(`
		SELECT id, mode, created_by, is_group, title, created_at
		FROM gnosis_conversations WHERE id = $1`, id,
	).Scan(&c.ID, &c.Mode, &c.CreatedBy, &c.IsGroup, &c.Title, &c.CreatedAt)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// IsMember reports whether an account participates in a conversation.
func IsMember(database *sql.DB, convoID, accountID string) (bool, error) {
	var x int
	err := database.QueryRow(`
		SELECT 1 FROM gnosis_members WHERE conversation_id = $1 AND account_id = $2`,
		convoID, accountID).Scan(&x)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

// MemberAccounts returns the account IDs of every participant (used for SSE fanout).
func MemberAccounts(database *sql.DB, convoID string) ([]string, error) {
	return scanStrings(database, `SELECT account_id FROM gnosis_members WHERE conversation_id = $1`, convoID)
}

// AddMember adds a participant, enforcing no-silent-downgrade: an unverified PIAL
// cannot join a sealed conversation.
func AddMember(database *sql.DB, convoID, accountID, pialID string) error {
	c, err := GetConversation(database, convoID)
	if err != nil {
		return err
	}
	if err := guardAddMember(c.Mode, CanAddToSealed(database, pialID)); err != nil {
		return err
	}
	_, err = database.Exec(`
		INSERT INTO gnosis_members (conversation_id, account_id, pial_id)
		VALUES ($1, $2, $3) ON CONFLICT (conversation_id, account_id) DO NOTHING`,
		convoID, accountID, pialID)
	return err
}

// ListMessages returns a conversation's messages oldest-first, attaching the
// viewer's own sealed key (for sealed messages) so the client can decrypt. The
// key is matched on the viewer's ACCOUNT id.
func ListMessages(database *sql.DB, convoID, viewerAccount string, limit int) ([]Message, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := database.Query(`
		SELECT m.id, m.sender_account, m.mode, m.body, m.body_ct_b64, m.body_nonce_b64, m.created_at,
		       COALESCE(sk.eph_pub_b64,''), COALESCE(sk.sealed_b64,''), COALESCE(sk.sealed_nonce_b64,'')
		FROM gnosis_messages m
		LEFT JOIN gnosis_sealed_keys sk
		       ON sk.message_id = m.id AND sk.recipient_account = $2
		WHERE m.conversation_id = $1
		ORDER BY m.created_at ASC
		LIMIT $3`, convoID, viewerAccount, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Message
	for rows.Next() {
		m := Message{ConversationID: convoID}
		if err := rows.Scan(&m.ID, &m.SenderAccount, &m.Mode, &m.Body, &m.BodyCtB64, &m.BodyNonceB64,
			&m.CreatedAt, &m.EphPubB64, &m.SealedB64, &m.SealedNonceB64); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// InsertPlainMessage stores a server-readable plaintext message.
func InsertPlainMessage(database *sql.DB, convoID, sender, body string) (Message, error) {
	m := Message{ConversationID: convoID, SenderAccount: sender, Mode: ModePlain, Body: body}
	if err := database.QueryRow(`
		INSERT INTO gnosis_messages (conversation_id, sender_account, mode, body)
		VALUES ($1, $2, 'plain', $3) RETURNING id, created_at`,
		convoID, sender, body).Scan(&m.ID, &m.CreatedAt); err != nil {
		return Message{}, err
	}
	touchConversation(database, convoID)
	return m, nil
}

// InsertSealedMessage stores an opaque ciphertext envelope plus its per-recipient
// wrapped keys in one transaction. The server never sees plaintext.
func InsertSealedMessage(database *sql.DB, convoID, sender, bodyCtB64, bodyNonceB64 string, keys []SealedKey) (Message, error) {
	tx, err := database.Begin()
	if err != nil {
		return Message{}, err
	}
	defer tx.Rollback()

	m := Message{ConversationID: convoID, SenderAccount: sender, Mode: ModeSealed,
		BodyCtB64: bodyCtB64, BodyNonceB64: bodyNonceB64}
	if err = tx.QueryRow(`
		INSERT INTO gnosis_messages (conversation_id, sender_account, mode, body_ct_b64, body_nonce_b64)
		VALUES ($1, $2, 'sealed', $3, $4) RETURNING id, created_at`,
		convoID, sender, bodyCtB64, bodyNonceB64).Scan(&m.ID, &m.CreatedAt); err != nil {
		return Message{}, err
	}
	for _, k := range keys {
		if _, err = tx.Exec(`
			INSERT INTO gnosis_sealed_keys (message_id, recipient_account, eph_pub_b64, sealed_b64, sealed_nonce_b64)
			VALUES ($1, $2, $3, $4, $5)`,
			m.ID, k.RecipientAccount, k.EphPubB64, k.SealedB64, k.SealedNonceB64); err != nil {
			return Message{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return Message{}, err
	}
	touchConversation(database, convoID)
	return m, nil
}

// ListConversationsForAccount returns the viewer's conversation rows newest-first,
// with the other participant's display fields, an unread count, and a preview that
// is a lock placeholder for sealed conversations (the server cannot read them).
func ListConversationsForAccount(database *sql.DB, accountID string) ([]ConvoListRow, error) {
	rows, err := database.Query(`
		SELECT c.id, c.mode, c.is_group, c.title,
		       COALESCE(ou.handle,''),
		       COALESCE(NULLIF(TRIM(op.display_name),''), ou.handle, ''),
		       COALESCE(op.avatar_url,''),
		       COALESCE(lm.mode,''), COALESCE(lm.body,''), COALESCE(lm.created_at, c.created_at),
		       (SELECT COUNT(*) FROM gnosis_messages um
		         WHERE um.conversation_id = c.id
		           AND um.sender_account <> $1
		           AND um.created_at > COALESCE(me.last_read_at, TIMESTAMPTZ 'epoch')) AS unread
		FROM gnosis_members me
		JOIN gnosis_conversations c ON c.id = me.conversation_id
		LEFT JOIN LATERAL (
		    SELECT om.account_id FROM gnosis_members om
		    WHERE om.conversation_id = c.id AND om.account_id <> $1
		    ORDER BY om.joined_at ASC LIMIT 1
		) other ON TRUE
		LEFT JOIN users ou ON ou.id = other.account_id
		LEFT JOIN user_profiles op ON op.user_id = other.account_id
		LEFT JOIN LATERAL (
		    SELECT gm.mode, gm.body, gm.created_at FROM gnosis_messages gm
		    WHERE gm.conversation_id = c.id ORDER BY gm.created_at DESC LIMIT 1
		) lm ON TRUE
		WHERE me.account_id = $1
		ORDER BY COALESCE(lm.created_at, c.created_at) DESC`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ConvoListRow
	for rows.Next() {
		var r ConvoListRow
		var lastMode, lastBody string
		if err := rows.Scan(&r.ConversationID, &r.Mode, &r.IsGroup, &r.Title,
			&r.OtherHandle, &r.OtherDisplay, &r.OtherAvatar,
			&lastMode, &lastBody, &r.LastAt, &r.Unread); err != nil {
			return nil, err
		}
		r.Preview = previewFor(lastMode, lastBody)
		out = append(out, r)
	}
	return out, rows.Err()
}

// FindDirectConversation returns the id of the existing 1:1 conversation between
// two accounts, or "" if none exists.
func FindDirectConversation(database *sql.DB, a, b string) (string, error) {
	var id string
	err := database.QueryRow(`
		SELECT m1.conversation_id
		FROM gnosis_members m1
		JOIN gnosis_members m2 ON m2.conversation_id = m1.conversation_id
		JOIN gnosis_conversations c ON c.id = m1.conversation_id
		WHERE m1.account_id = $1 AND m2.account_id = $2 AND c.is_group = FALSE
		  AND (SELECT COUNT(*) FROM gnosis_members mc WHERE mc.conversation_id = m1.conversation_id) = 2
		LIMIT 1`, a, b).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return id, err
}

// OtherParticipant returns the display fields of the first other member of a
// conversation (used for the 1:1 thread header). Empty strings if none.
func OtherParticipant(database *sql.DB, convoID, viewerID string) (handle, display, avatar string) {
	_ = database.QueryRow(`
		SELECT COALESCE(u.handle,''),
		       COALESCE(NULLIF(TRIM(p.display_name),''), u.handle, ''),
		       COALESCE(p.avatar_url,'')
		FROM gnosis_members m
		JOIN users u ON u.id = m.account_id
		LEFT JOIN user_profiles p ON p.user_id = m.account_id
		WHERE m.conversation_id = $1 AND m.account_id <> $2
		ORDER BY m.joined_at ASC LIMIT 1`, convoID, viewerID).Scan(&handle, &display, &avatar)
	return
}

// MarkRead advances a member's last_read_at to now (clears their unread count).
func MarkRead(database *sql.DB, convoID, accountID string) error {
	_, err := database.Exec(`
		UPDATE gnosis_members SET last_read_at = NOW()
		WHERE conversation_id = $1 AND account_id = $2`, convoID, accountID)
	return err
}

// Identity is an account's messaging keypair custody: the public directory key and
// the private key wrapped under the account's login-derived key. There is no
// separate recovery — the account's backup codes are the sole recovery.
type Identity struct {
	PubB64         string
	WrappedPrivB64 string
	WrapNonceB64   string
}

// GetIdentityPub returns an account's X25519 messaging public key, or "" if none.
func GetIdentityPub(database *sql.DB, account string) string {
	var pub string
	_ = database.QueryRow(`SELECT pub_b64 FROM gnosis_identity WHERE account_id = $1`, account).Scan(&pub)
	return pub
}

// GetIdentity returns an account's full identity custody (ok=false if none).
func GetIdentity(database *sql.DB, account string) (Identity, bool, error) {
	var id Identity
	err := database.QueryRow(`
		SELECT pub_b64, wrapped_priv_b64, wrap_nonce_b64
		FROM gnosis_identity WHERE account_id = $1`, account,
	).Scan(&id.PubB64, &id.WrappedPrivB64, &id.WrapNonceB64)
	if err == sql.ErrNoRows {
		return Identity{}, false, nil
	}
	if err != nil {
		return Identity{}, false, err
	}
	return id, true, nil
}

// GetIdentityPubs returns account→pub for the requested accounts that have an identity.
func GetIdentityPubs(database *sql.DB, accounts []string) (map[string]string, error) {
	out := map[string]string{}
	if len(accounts) == 0 {
		return out, nil
	}
	rows, err := database.Query(`SELECT account_id, pub_b64 FROM gnosis_identity WHERE account_id = ANY($1)`, pq.Array(accounts))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var a, k string
		if err := rows.Scan(&a, &k); err != nil {
			return nil, err
		}
		out[a] = k
	}
	return out, rows.Err()
}

// UpsertIdentity registers (or rotates) an account's messaging keypair custody. It
// returns changed=true when it overwrote a DIFFERENT existing public key — a key
// rotation the caller logs (anti-silent-MITM continuity).
func UpsertIdentity(database *sql.DB, account, pub, wrappedPriv, wrapNonce string) (changed bool, err error) {
	var prev string
	_ = database.QueryRow(`SELECT pub_b64 FROM gnosis_identity WHERE account_id = $1`, account).Scan(&prev)
	changed = prev != "" && prev != pub
	_, err = database.Exec(`
		INSERT INTO gnosis_identity (account_id, pub_b64, wrapped_priv_b64, wrap_nonce_b64) VALUES ($1, $2, $3, $4)
		ON CONFLICT (account_id) DO UPDATE SET
		  pub_b64 = EXCLUDED.pub_b64, wrapped_priv_b64 = EXCLUDED.wrapped_priv_b64,
		  wrap_nonce_b64 = EXCLUDED.wrap_nonce_b64, updated_at = NOW()`,
		account, pub, wrappedPriv, wrapNonce)
	return changed, err
}

// MemberAccountList returns the account IDs of every participant (used to seal to
// each recipient's per-account key).
func MemberAccountList(database *sql.DB, convoID string) ([]string, error) {
	return scanStrings(database, `SELECT account_id FROM gnosis_members WHERE conversation_id = $1`, convoID)
}

// MembersWithPIAL returns (account_id, pial_id) for every participant — used to
// route per-recipient sealed keys to the right accounts on fan-out.
func MembersWithPIAL(database *sql.DB, convoID string) ([]Member, error) {
	rows, err := database.Query(`SELECT account_id, pial_id FROM gnosis_members WHERE conversation_id = $1`, convoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Member
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.AccountID, &m.PIALID); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// touchConversation bumps updated_at on new activity. It NEVER touches mode.
func touchConversation(database *sql.DB, convoID string) {
	_, _ = database.Exec(`UPDATE gnosis_conversations SET updated_at = NOW() WHERE id = $1`, convoID)
}

// previewFor builds a sidebar preview string. Sealed conversations get a lock
// placeholder because the server holds only ciphertext.
func previewFor(mode, body string) string {
	if mode == ModeSealed {
		return "🔒 Encrypted message"
	}
	body = strings.TrimSpace(body)
	const max = 80
	if len([]rune(body)) > max {
		return string([]rune(body)[:max]) + "…"
	}
	return body
}

// scanStrings runs a single-column query and collects the results.
func scanStrings(database *sql.DB, query string, args ...interface{}) ([]string, error) {
	rows, err := database.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
