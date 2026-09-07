package gnosis

import (
	"database/sql"
	"time"
)

// ── The message that did not get through ─────────────────────────────────────
//
// A conversation used to be the only place a message could live, and a
// conversation only existed once the contact policy had already said yes. That
// ordering made the honest case impossible: somebody writing to a person they
// are not yet permitted to reach.
//
// A Pending is that message, held by the server between the moment it was
// written and the moment contact is allowed. There is at most one per (sender,
// target) pair — a retry amends what was said rather than queueing a second
// attempt — and it carries what the contact authority last answered, so
// re-opening the thread can render the same gate without asking the authority
// again and therefore without knocking on the recipient twice.
//
// It is deliberately NOT a gnosis_messages row. A message belongs to a
// conversation; this exists precisely because no conversation does.

// Pending decisions. These are the contact authority's own words for everything
// that is not an allow; an allowed message is delivered and its row is gone, so
// there is no "allow" here to store.
const (
	PendingUnasked     = ""
	PendingRequest     = "request"
	PendingDeny        = "deny"
	PendingUnavailable = "unavailable"
)

// Pending mirrors a gnosis_pending_messages row.
type Pending struct {
	ID            string
	SenderAccount string
	TargetAccount string
	Body          string
	LastDecision  string
	CreatedAt     time.Time
}

const pendingSelect = `
	SELECT id, sender_account, target_account, body, last_decision, created_at
	FROM gnosis_pending_messages `

// UpsertPending records what the sender wrote for a target they cannot yet
// reach, replacing anything they had written before. The decision is reset to
// unasked because a new sentence has not been judged yet.
func UpsertPending(database *sql.DB, sender, target, body string) (Pending, error) {
	p := Pending{SenderAccount: sender, TargetAccount: target, Body: body}
	err := database.QueryRow(`
		INSERT INTO gnosis_pending_messages (sender_account, target_account, body)
		VALUES ($1, $2, $3)
		ON CONFLICT (sender_account, target_account) DO UPDATE
		  SET body = EXCLUDED.body, last_decision = '', updated_at = NOW()
		RETURNING id, last_decision, created_at`,
		sender, target, body).Scan(&p.ID, &p.LastDecision, &p.CreatedAt)
	if err != nil {
		return Pending{}, err
	}
	return p, nil
}

// SetPendingDecision records what the contact authority answered, so the gate
// can be redrawn later from the server's own account of the attempt instead of
// asking again.
func SetPendingDecision(database *sql.DB, id, decision string) error {
	_, err := database.Exec(`
		UPDATE gnosis_pending_messages SET last_decision = $2, updated_at = NOW()
		WHERE id = $1`, id, decision)
	return err
}

// FindPending returns the undelivered message from one account to another, if
// there is one.
func FindPending(database *sql.DB, sender, target string) (Pending, bool, error) {
	return scanPending(database.QueryRow(
		pendingSelect+`WHERE sender_account = $1 AND target_account = $2`, sender, target))
}

// GetPendingForSender returns a pending message by id, and only to the account
// that wrote it. The sender is part of the query rather than checked after it,
// so an id belonging to somebody else reads as absent.
func GetPendingForSender(database *sql.DB, id, sender string) (Pending, bool, error) {
	return scanPending(database.QueryRow(
		pendingSelect+`WHERE id = $1 AND sender_account = $2`, id, sender))
}

// DeletePending removes an undelivered message — because it was delivered, or
// because the conversation it was waiting on has opened and been used.
func DeletePending(database *sql.DB, id string) error {
	_, err := database.Exec(`DELETE FROM gnosis_pending_messages WHERE id = $1`, id)
	return err
}

// DeletePendingBetween clears any undelivered message between two accounts. Used
// once a conversation between them is genuinely in use, so a draft written
// before contact existed cannot resurface inside a conversation that has moved
// on without it.
func DeletePendingBetween(database *sql.DB, sender, target string) error {
	_, err := database.Exec(`
		DELETE FROM gnosis_pending_messages
		WHERE sender_account = $1 AND target_account = $2`, sender, target)
	return err
}

func scanPending(row *sql.Row) (Pending, bool, error) {
	var p Pending
	err := row.Scan(&p.ID, &p.SenderAccount, &p.TargetAccount, &p.Body, &p.LastDecision, &p.CreatedAt)
	if err == sql.ErrNoRows {
		return Pending{}, false, nil
	}
	if err != nil {
		return Pending{}, false, err
	}
	return p, true, nil
}

// conversationPeerSQL matches a pending message to the OTHER member of a
// one-to-one conversation, and to nothing else.
//
// Every clause here is load-bearing:
//
//   - NOT c.is_group. A held message is addressed to one person. A group has
//     many members, so an unrestricted member join matches a private message to
//     anyone who happens to also be in the group — which is how a sentence
//     written to one person ends up in a composer aimed at everyone.
//   - m.account_id = p.target_account. The row must be addressed to the peer.
//   - s.account_id = p.sender_account. The sender must be in this conversation
//     too, so a conversation id belonging to somebody else cannot be used to
//     read or destroy a held message.
//   - p.target_account <> p.sender_account. A conversation with yourself has one
//     member who is both, and it must not satisfy "the other member".
//
// The two callers below share this text so the read and the delete can never
// disagree about which row is the peer's — a delete that matched more than the
// read would destroy a message the sender was still being shown.
const conversationPeerSQL = `
	FROM gnosis_pending_messages p
	JOIN gnosis_conversations c ON c.id = $1 AND NOT c.is_group
	JOIN gnosis_members m ON m.conversation_id = c.id AND m.account_id = p.target_account
	JOIN gnosis_members s ON s.conversation_id = c.id AND s.account_id = p.sender_account
	WHERE p.sender_account = $2 AND p.target_account <> p.sender_account `

// FindPendingForConversationPeer returns the sender's undelivered message to the
// other member of a one-to-one conversation.
//
// A pending message can outlive the opening of a conversation in exactly one
// case: the conversation opened SEALED, and plaintext must never be written into
// a sealed thread. The message is therefore still the server's, and the thread
// renders it back into the composer, where the client seals it and sends it.
//
// A group conversation never has such a message, and asking for one there must
// answer "none" rather than the sender's private message to some member of it.
func FindPendingForConversationPeer(database *sql.DB, convoID, sender string) (Pending, bool, error) {
	return scanPending(database.QueryRow(`
		SELECT p.id, p.sender_account, p.target_account, p.body, p.last_decision, p.created_at`+
		conversationPeerSQL+`LIMIT 1`, convoID, sender))
}

// DeletePendingForConversationPeer clears the sender's undelivered message to the
// other member of a one-to-one conversation. Called once they have actually sent
// something in it, so a sentence written before the conversation existed cannot
// resurface inside one that has moved on without it.
//
// It matches exactly what FindPendingForConversationPeer reads. Posting into a
// group must not silently destroy a held message addressed to one of its
// members, which was never delivered and which the sender was last told was
// still waiting.
func DeletePendingForConversationPeer(database *sql.DB, convoID, sender string) error {
	_, err := database.Exec(`
		DELETE FROM gnosis_pending_messages
		WHERE id IN (SELECT p.id`+conversationPeerSQL+`)`, convoID, sender)
	return err
}
