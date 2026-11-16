package db

// ranking_outbox.go — ranking signals, written with the action they describe.
//
// AethyrRank learns from Sitra Achra ranking.events and from nothing else. A
// signal is queued here inside the transaction of its action (SetWorkReaction,
// FollowUserNotifying, UnfollowUser, SoftDeleteWork) or with its own write (a
// view, "not interested", a citation), and aethyr.StartSignalRelay publishes it.
// The contract is aethyrrank-engine/src/learning/signal.rs.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
)

// RankingAction is the closed vocabulary of ranking.events actions. Mirrors
// Action in aethyrrank-engine/src/learning/signal.rs; change one and the other
// must change with it.
type RankingAction string

const (
	RankLike          RankingAction = "like"
	RankUnlike        RankingAction = "unlike"
	RankRepost        RankingAction = "repost"
	RankUnrepost      RankingAction = "unrepost"
	RankBookmark      RankingAction = "bookmark"
	RankUnbookmark    RankingAction = "unbookmark"
	RankDislike       RankingAction = "dislike"
	RankUndislike     RankingAction = "undislike"
	RankReply         RankingAction = "reply"
	RankQuote         RankingAction = "quote"
	RankView          RankingAction = "view"
	RankNotInterested RankingAction = "not_interested"
	RankFollow        RankingAction = "follow"
	RankUnfollow      RankingAction = "unfollow"
	RankDelete        RankingAction = "delete"
	// Listening to an audio work: started, played to its end, left early.
	RankPlay     RankingAction = "play"
	RankComplete RankingAction = "complete"
	RankSkip     RankingAction = "skip"
)

// reactionActions maps a work_reactions reaction type to the signal of adding
// and of taking it back.
var reactionActions = map[string][2]RankingAction{
	"like":     {RankLike, RankUnlike},
	"repost":   {RankRepost, RankUnrepost},
	"bookmark": {RankBookmark, RankUnbookmark},
	"dislike":  {RankDislike, RankUndislike},
}

// ReactionRankingAction is the signal for adding (or, add=false, taking back)
// a reaction of this type.
func ReactionRankingAction(reactionType string, add bool) (RankingAction, bool) {
	pair, ok := reactionActions[reactionType]
	if !ok {
		return "", false
	}
	if add {
		return pair[0], true
	}
	return pair[1], true
}

// RankingSignal is one thing a person did that says what they want to see.
type RankingSignal struct {
	// EventID is optional. Set it when the same action may be queued more than
	// once (a retried obligation), so it is queued once.
	EventID string
	Action  RankingAction
	// Viewer is the account that acted.
	Viewer string
	// WorkID is the work acted on, for every action but follow and unfollow.
	WorkID string
	// TargetAccount is the account followed or unfollowed.
	TargetAccount string
	// OccurredAt is when the person acted; zero means now.
	OccurredAt time.Time
}

// rankingNamespace derives stable event ids for signals that may be queued twice.
var rankingNamespace = uuid.MustParse("6b0c2f8e-91a4-4d37-b5e2-0f7d3c9a8e14")

// RankingEventID is the stable event id of one action on one subject.
func RankingEventID(parts ...string) string {
	key := ""
	for _, p := range parts {
		key += p + "\x00"
	}
	return uuid.NewSHA1(rankingNamespace, []byte(key)).String()
}

// EnqueueRankingSignal queues a signal on a pool or inside the caller's
// transaction. An account with no identity has no PIAL to key its signals by,
// and that is not a reason to undo the action: it is said and skipped.
func EnqueueRankingSignal(q DBTX, s RankingSignal) error {
	var pial string
	err := q.QueryRow(`SELECT COALESCE(pial_id::text, '') FROM users WHERE id = $1::uuid`, s.Viewer).Scan(&pial)
	if errors.Is(err, sql.ErrNoRows) {
		log.Printf("[ranking] %s by %s: no such account; not queued", s.Action, s.Viewer)
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading the identity of %s for a ranking signal: %w", s.Viewer, err)
	}
	if pial == "" {
		log.Printf("[ranking] %s by %s: account has no identity; not queued", s.Action, s.Viewer)
		return nil
	}
	if s.EventID == "" {
		s.EventID = uuid.NewString()
	}
	now := time.Now().UTC()
	occurred := s.OccurredAt.UTC()
	if s.OccurredAt.IsZero() {
		occurred = now
	}
	body := map[string]interface{}{
		"event_id":       s.EventID,
		"source":         "feed-engine",
		"viewer_account": s.Viewer,
		"viewer_pial":    pial,
		"action":         string(s.Action),
		"occurred_at":    occurred.Format(time.RFC3339Nano),
		"received_at":    now.Format(time.RFC3339Nano),
	}
	if s.WorkID != "" {
		body["work_id"] = s.WorkID
	}
	if s.TargetAccount != "" {
		body["target_account"] = s.TargetAccount
	}
	event, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encoding the %s signal of %s: %w", s.Action, s.Viewer, err)
	}
	if _, err := q.Exec(`
		INSERT INTO ranking_outbox (event_id, viewer_pial, event)
		VALUES ($1::uuid, $2, $3::jsonb)
		ON CONFLICT (event_id) DO NOTHING`, s.EventID, pial, string(event)); err != nil {
		return fmt.Errorf("queueing the %s signal of %s: %w", s.Action, s.Viewer, err)
	}
	return nil
}
