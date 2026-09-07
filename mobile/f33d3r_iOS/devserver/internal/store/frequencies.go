package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Frequencies — F33D3R's live audio rooms, as the Auralis brain models them.
//
// This file is the brain's `src/domain` and `src/repository/postgres` in one
// package: the state machine from `domain/lifecycle.rs`, the authorization
// matrix from `domain/role.rs`, the admission rules from
// `domain/permissions.rs`, and the queries that back them, over the tables
// schema.sql transcribes from `frequencies/migrations/0001,0002`.
//
// The rules are copied, not approximated. A UI that hides a button is not
// security: every mutation asks the matrix again here, and the `can_*`
// booleans the client renders from are computed from the same function that
// refuses the write.
//
// What is missing is the audio and only the audio. There is no media node, no
// WebRTC session, no Redis presence: presence is a participant row in state
// `joined`, and every count comes from rows.

// ── Errors ───────────────────────────────────────────────────────────────────

// FreqError is one of the brain's stable machine codes with the status and
// the sentence Nantar answers with. The client branches on Code.
type FreqError struct {
	Status  int
	Code    string
	Message string
}

func (e *FreqError) Error() string { return e.Code + ": " + e.Message }

func freqErr(status int, code, message string) *FreqError {
	return &FreqError{Status: status, Code: code, Message: message}
}

// The codes the contract names. Anything not in this list is a bug, not a
// branch the client has to handle.
var (
	ErrFreqNotFound      = freqErr(http.StatusNotFound, "not_found", "That Frequency doesn't exist.")
	ErrFreqNotLive       = freqErr(http.StatusConflict, "not_live", "That Frequency isn't live.")
	ErrFreqLocked        = freqErr(http.StatusForbidden, "locked", "The host has locked this Frequency.")
	ErrFreqBlocked       = freqErr(http.StatusForbidden, "blocked", "You've been blocked from this Frequency.")
	ErrFreqFull          = freqErr(http.StatusConflict, "full", "This Frequency is full.")
	ErrFreqSpeakersFull  = freqErr(http.StatusConflict, "speakers_full", "Every speaker slot is taken.")
	ErrFreqNotJoined     = freqErr(http.StatusConflict, "not_joined", "Tune in first.")
	ErrFreqRequestsShut  = freqErr(http.StatusConflict, "requests_closed", "The host has closed requests.")
	ErrFreqOwnRequest    = freqErr(http.StatusConflict, "own_request", "You can't upvote your own request.")
	ErrFreqNotSpeaker    = freqErr(http.StatusConflict, "not_a_speaker", "That person isn't a speaker.")
	ErrFreqNotCohost     = freqErr(http.StatusConflict, "not_a_cohost", "That person isn't a co-host.")
	ErrFreqIsHost        = freqErr(http.StatusConflict, "is_host", "That person is the host.")
	ErrFreqHostLive      = freqErr(http.StatusConflict, "host_already_live", "You already have an open Frequency.")
	ErrFreqAlreadyOver   = freqErr(http.StatusConflict, "over", "This Frequency is over.")
	ErrFreqAlreadyEnding = freqErr(http.StatusConflict, "already_ending", "This Frequency is already ending.")
	ErrFreqCancelled     = freqErr(http.StatusConflict, "already_cancelled", "This Frequency is already cancelled.")
	ErrFreqBadTransition = freqErr(http.StatusConflict, "invalid_transition", "That isn't a move this Frequency can make.")
)

func freqBadRequest(msg string) *FreqError {
	return freqErr(http.StatusBadRequest, "bad_request", msg)
}

// ── Domain: roles and the authorization matrix (domain/role.rs) ──────────────

// The four roles, in the vocabulary the wire uses.
const (
	FreqRoleHost     = "host"
	FreqRoleCohost   = "cohost"
	FreqRoleSpeaker  = "speaker"
	FreqRoleListener = "listener"
)

// The actions the matrix answers for.
const (
	FreqActEnd            = "end"
	FreqActCohost         = "cohost"
	FreqActLock           = "lock"
	FreqActToggleRequests = "toggle_requests"
	FreqActRemove         = "remove"
	FreqActBlock          = "block"
	FreqActApprove        = "approve"
	FreqActDecline        = "decline"
	FreqActDemote         = "demote"
	FreqActMuteOther      = "mute_other"
	FreqActMuteSelf       = "mute_self"
	FreqActRequestMic     = "request_mic"
	FreqActSpeak          = "speak"
	FreqActListen         = "listen"
)

func freqRank(role string) int {
	switch role {
	case FreqRoleHost:
		return 3
	case FreqRoleCohost:
		return 2
	case FreqRoleSpeaker:
		return 1
	default:
		return 0
	}
}

// FreqOutranks reports whether a may act on b. A moderator acts only on
// people strictly below them: a co-host cannot remove the host or a peer
// co-host, and the host cannot be removed by anyone.
func FreqOutranks(a, b string) bool { return freqRank(a) > freqRank(b) }

// FreqSpeaks reports whether the role publishes audio.
func FreqSpeaks(role string) bool {
	return role == FreqRoleHost || role == FreqRoleCohost || role == FreqRoleSpeaker
}

// FreqModerates reports whether the role is host or co-host.
func FreqModerates(role string) bool {
	return role == FreqRoleHost || role == FreqRoleCohost
}

// FreqMay is the matrix. True means the role may attempt the action; whether
// the specific target is permitted is FreqOutranks's question.
//
//	                       Host   CoHost   Speaker   Listener
//	End Frequency           YES     NO        NO        NO
//	Add / remove co-host    YES     NO        NO        NO
//	Lock / requests toggle  YES     NO        NO        NO
//	Remove / block          YES     YES       NO        NO
//	Approve / decline       YES     YES       NO        NO
//	Demote speaker          YES     YES       NO        NO
//	Mute other              YES     YES       NO        NO
//	Mute self               YES     YES       YES       NO
//	Request microphone      N/A     N/A       N/A       YES
//	Speak                   YES     YES       YES       NO
//	Listen                  YES     YES       YES       YES
func FreqMay(role, action string) bool {
	switch action {
	case FreqActEnd, FreqActCohost, FreqActLock, FreqActToggleRequests:
		return role == FreqRoleHost
	case FreqActRemove, FreqActBlock, FreqActApprove, FreqActDecline, FreqActDemote, FreqActMuteOther:
		return FreqModerates(role)
	case FreqActMuteSelf, FreqActSpeak:
		return FreqSpeaks(role)
	case FreqActRequestMic:
		return role == FreqRoleListener
	case FreqActListen:
		return role == FreqRoleHost || role == FreqRoleCohost || role == FreqRoleSpeaker || role == FreqRoleListener
	}
	return false
}

// ── Domain: the state machine (domain/lifecycle.rs) ──────────────────────────

// The states, in the vocabulary the wire uses.
const (
	FreqStateDraft      = "draft"
	FreqStateScheduled  = "scheduled"
	FreqStateStarting   = "starting"
	FreqStateLive       = "live"
	FreqStateEnding     = "ending"
	FreqStateEnded      = "ended"
	FreqStateProcessing = "processing_replay"
	FreqStateArchived   = "archived"
	FreqStateCancelled  = "cancelled"
	FreqStateFailed     = "failed"
	FreqStateTerminated = "moderation_terminated"
)

// freqTransitions is the permitted-moves table, exhaustive on purpose: a
// state without a row here is unreachable, which is the safe default.
var freqTransitions = map[[2]string]bool{
	{FreqStateDraft, FreqStateScheduled}:     true,
	{FreqStateDraft, FreqStateStarting}:      true,
	{FreqStateDraft, FreqStateCancelled}:     true,
	{FreqStateScheduled, FreqStateDraft}:     true,
	{FreqStateScheduled, FreqStateStarting}:  true,
	{FreqStateScheduled, FreqStateCancelled}: true,
	{FreqStateStarting, FreqStateLive}:       true,
	{FreqStateStarting, FreqStateFailed}:     true,
	{FreqStateStarting, FreqStateTerminated}: true,
	{FreqStateLive, FreqStateEnding}:         true,
	{FreqStateLive, FreqStateFailed}:         true,
	{FreqStateLive, FreqStateTerminated}:     true,
	{FreqStateEnding, FreqStateEnded}:        true,
	{FreqStateEnding, FreqStateFailed}:       true,
	{FreqStateEnded, FreqStateProcessing}:    true,
	{FreqStateEnded, FreqStateArchived}:      true,
	{FreqStateProcessing, FreqStateArchived}: true,
}

// FreqTransitionAllowed answers the state machine. Never: ended -> live,
// cancelled -> live, archived -> starting. A new session is a new Frequency.
func FreqTransitionAllowed(from, to string) bool {
	return freqTransitions[[2]string{from, to}]
}

// FreqStateOpen reports whether a session is running: starting, live, ending.
func FreqStateOpen(state string) bool {
	return state == FreqStateStarting || state == FreqStateLive || state == FreqStateEnding
}

// FreqStateAcceptsJoins: only a live Frequency takes people in.
func FreqStateAcceptsJoins(state string) bool { return state == FreqStateLive }

// FreqStateOver reports whether the Frequency will never carry audio again.
func FreqStateOver(state string) bool {
	switch state {
	case FreqStateEnded, FreqStateProcessing, FreqStateArchived,
		FreqStateCancelled, FreqStateFailed, FreqStateTerminated:
		return true
	}
	return false
}

// ── Types ────────────────────────────────────────────────────────────────────

// Frequency is one row of the frequencies table.
type Frequency struct {
	ID                   string
	Version              int64
	HostPIAL             string
	Host                 *User
	Title                string
	Description          string
	State                string
	Visibility           string
	Language             string
	AdultContent         bool
	SpeakerVerityMinTier int
	ScheduledAt          *time.Time
	StartedAt            *time.Time
	EndedAt              *time.Time
	EndReason            string
	RecordingEnabled     bool
	ReplayStatus         string
	MaxSpeakers          int
	MaxListeners         int
	RequestsOpen         bool
	Locked               bool
	MediaNode            string
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// FrequencyParticipant is one row of frequency_participants, hydrated with the
// account behind the PIAL.
type FrequencyParticipant struct {
	ID       int64
	PIAL     string
	User     *User
	Role     string
	State    string
	Muted    bool
	JoinedAt time.Time
}

// Present is this server's presence: a participant row still in state joined.
// The brain reads a Redis TTL key here; there is no Redis, and a room's counts
// must come from rows rather than from a number somebody typed.
func (p *FrequencyParticipant) Present() bool { return p.State == "joined" }

// FrequencySpeakerRequest is one row of frequency_speaker_requests.
type FrequencySpeakerRequest struct {
	ID          string
	FrequencyID string
	PIAL        string
	User        *User
	Reason      string
	Status      string
	Upvotes     int
	CreatedAt   time.Time
}

// FrequencyInput is what a create takes. Zero values take the schema default.
type FrequencyInput struct {
	HostPIAL         string
	Title            string
	Description      string
	Visibility       string
	Language         string
	AdultContent     bool
	RecordingEnabled bool
	ScheduledAt      *time.Time
	MaxSpeakers      int
	MaxListeners     int
}

// FrequencyPatch is an update. A nil field is left alone; this is how
// `frequency_update` changes one thing without clearing the rest.
type FrequencyPatch struct {
	Title            *string
	Description      *string
	Visibility       *string
	Language         *string
	AdultContent     *bool
	RecordingEnabled *bool
	MaxSpeakers      *int
	MaxListeners     *int
}

// ── Reads ────────────────────────────────────────────────────────────────────

const frequencySelectSQL = `
SELECT f.id, f.version, f.host_pial, f.title, f.description, f.state, f.visibility, f.language,
       f.adult_content, f.speaker_verity_min_tier, f.scheduled_at, f.started_at, f.ended_at, f.end_reason,
       f.recording_enabled, f.replay_status, f.max_speakers, f.max_listeners, f.requests_open, f.locked,
       f.media_node, f.created_at, f.updated_at
FROM frequencies f
`

func scanFrequency(row interface{ Scan(...any) error }) (*Frequency, error) {
	var f Frequency
	var scheduled, started, ended, endReason, mediaNode sql.NullString
	var created, updated string
	var adult, recording, requestsOpen, locked int
	if err := row.Scan(&f.ID, &f.Version, &f.HostPIAL, &f.Title, &f.Description, &f.State, &f.Visibility, &f.Language,
		&adult, &f.SpeakerVerityMinTier, &scheduled, &started, &ended, &endReason,
		&recording, &f.ReplayStatus, &f.MaxSpeakers, &f.MaxListeners, &requestsOpen, &locked,
		&mediaNode, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrFreqNotFound
		}
		return nil, err
	}
	f.AdultContent = adult == 1
	f.RecordingEnabled = recording == 1
	f.RequestsOpen = requestsOpen == 1
	f.Locked = locked == 1
	f.ScheduledAt = timePtr(scheduled)
	f.StartedAt = timePtr(started)
	f.EndedAt = timePtr(ended)
	f.EndReason = endReason.String
	f.MediaNode = mediaNode.String
	f.CreatedAt = ParseTime(created)
	f.UpdatedAt = ParseTime(updated)
	return &f, nil
}

// GetFrequency loads one Frequency with its host resolved.
func (s *Store) GetFrequency(ctx context.Context, id string) (*Frequency, error) {
	f, err := scanFrequency(s.db.QueryRowContext(ctx, frequencySelectSQL+`WHERE f.id = ?`, id))
	if err != nil {
		return nil, err
	}
	return f, s.hydrateFrequencies(ctx, []*Frequency{f})
}

// FrequencyLane is a discovery lane: what is live, what is coming, what is
// over, and what is the caller's own.
type FrequencyLane string

const (
	FrequencyLaneLive      FrequencyLane = "live"
	FrequencyLaneScheduled FrequencyLane = "scheduled"
	FrequencyLaneEnded     FrequencyLane = "ended"
	FrequencyLaneMine      FrequencyLane = "mine"
)

// ListFrequencies answers one lane. `mine` is everything the caller hosts, in
// any state; the other three are the states named, ordered the way the brain's
// partial indexes are.
func (s *Store) ListFrequencies(ctx context.Context, lane FrequencyLane, viewerPIAL string, limit int) ([]*Frequency, error) {
	var where, order string
	var args []any
	switch lane {
	case FrequencyLaneLive:
		where, order = `f.state = 'live'`, `f.started_at DESC`
	case FrequencyLaneScheduled:
		where, order = `f.state = 'scheduled'`, `f.scheduled_at ASC`
	case FrequencyLaneEnded:
		where, order = `f.state IN ('ended','processing_replay','archived')`, `f.ended_at DESC`
	case FrequencyLaneMine:
		where, order = `f.host_pial = ?`, `f.created_at DESC`
		args = append(args, viewerPIAL)
	default:
		return nil, freqBadRequest("unknown lane")
	}
	rows, err := s.db.QueryContext(ctx, frequencySelectSQL+`WHERE `+where+` ORDER BY `+order+` LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	out := []*Frequency{}
	for rows.Next() {
		f, err := scanFrequency(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, f)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, s.hydrateFrequencies(ctx, out)
}

// OpenFrequencyFor is `GET /v1/hosts/{pial}/open`: the caller's currently open
// Frequency, so the UI can offer "Return to your Frequency". ErrFreqNotFound
// when there is none.
func (s *Store) OpenFrequencyFor(ctx context.Context, hostPIAL string) (*Frequency, error) {
	f, err := scanFrequency(s.db.QueryRowContext(ctx,
		frequencySelectSQL+`WHERE f.host_pial = ? AND f.state IN ('starting','live','ending')`, hostPIAL))
	if err != nil {
		return nil, err
	}
	return f, s.hydrateFrequencies(ctx, []*Frequency{f})
}

func (s *Store) hydrateFrequencies(ctx context.Context, fs []*Frequency) error {
	if len(fs) == 0 {
		return nil
	}
	pials := make([]string, 0, len(fs))
	for _, f := range fs {
		pials = append(pials, f.HostPIAL)
	}
	users, err := s.GetUsersByPIAL(ctx, pials)
	if err != nil {
		return err
	}
	for _, f := range fs {
		f.Host = users[f.HostPIAL]
	}
	return nil
}

// GetUsersByPIAL loads many accounts at once, keyed by PIAL.
func (s *Store) GetUsersByPIAL(ctx context.Context, pials []string) (map[string]*User, error) {
	out := make(map[string]*User, len(pials))
	if len(pials) == 0 {
		return out, nil
	}
	seen := map[string]bool{}
	uniq := make([]string, 0, len(pials))
	for _, p := range pials {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		uniq = append(uniq, p)
	}
	q, args := inClause(uniq)
	rows, err := s.db.QueryContext(ctx, userSelectSQL+`WHERE u.pial_id IN (`+q+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out[u.PIALID] = u
	}
	return out, rows.Err()
}

// FrequencyParticipants lists everyone currently joined, hydrated, host first
// then co-hosts then speakers then listeners, each group oldest first.
func (s *Store) FrequencyParticipants(ctx context.Context, id string) ([]*FrequencyParticipant, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, pial, role, state, muted, joined_at FROM frequency_participants
		WHERE frequency_id = ? AND state = 'joined' ORDER BY joined_at ASC`, id)
	if err != nil {
		return nil, err
	}
	out := []*FrequencyParticipant{}
	for rows.Next() {
		var p FrequencyParticipant
		var muted int
		var joined string
		if err := rows.Scan(&p.ID, &p.PIAL, &p.Role, &p.State, &muted, &joined); err != nil {
			rows.Close()
			return nil, err
		}
		p.Muted = muted == 1
		p.JoinedAt = ParseTime(joined)
		out = append(out, &p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	pials := make([]string, 0, len(out))
	for _, p := range out {
		pials = append(pials, p.PIAL)
	}
	users, err := s.GetUsersByPIAL(ctx, pials)
	if err != nil {
		return nil, err
	}
	for _, p := range out {
		p.User = users[p.PIAL]
	}
	// Host first, then co-hosts, then speakers, then listeners — the order the
	// brain's read model hands to the speaker grid.
	stable := make([]*FrequencyParticipant, 0, len(out))
	for _, role := range []string{FreqRoleHost, FreqRoleCohost, FreqRoleSpeaker, FreqRoleListener} {
		for _, p := range out {
			if p.Role == role {
				stable = append(stable, p)
			}
		}
	}
	return stable, nil
}

// FrequencyParticipantFor loads one person's joined row, or nil.
func (s *Store) FrequencyParticipantFor(ctx context.Context, id, pial string) (*FrequencyParticipant, error) {
	var p FrequencyParticipant
	var muted int
	var joined string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, pial, role, state, muted, joined_at FROM frequency_participants
		WHERE frequency_id = ? AND pial = ? AND state = 'joined'`, id, pial).
		Scan(&p.ID, &p.PIAL, &p.Role, &p.State, &muted, &joined)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p.Muted = muted == 1
	p.JoinedAt = ParseTime(joined)
	p.User, err = s.GetUserByPIAL(ctx, pial)
	if errors.Is(err, ErrNotFound) {
		return &p, nil
	}
	return &p, err
}

// FrequencyGrant is the role the roles table grants this person, or "". A
// co-host grant outlives a connection: a co-host who drops and rejoins is
// still a co-host.
func (s *Store) FrequencyGrant(ctx context.Context, id, pial string) (string, error) {
	var role string
	err := s.db.QueryRowContext(ctx,
		`SELECT role FROM frequency_roles WHERE frequency_id = ? AND pial = ? AND revoked_at IS NULL`, id, pial).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return role, err
}

// FrequencyCohosts lists the PIALs holding an active co-host grant.
func (s *Store) FrequencyCohosts(ctx context.Context, id string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT pial FROM frequency_roles WHERE frequency_id = ? AND role = 'cohost' AND revoked_at IS NULL ORDER BY granted_at ASC`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// FrequencyBlocked reports whether this person is blocked from the room.
func (s *Store) FrequencyBlocked(ctx context.Context, id, pial string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM frequency_blocks WHERE frequency_id = ? AND pial = ?`, id, pial).Scan(&n)
	return n > 0, err
}

// PendingFrequencyRequests is the queue, best first: most upvoted, then oldest.
func (s *Store) PendingFrequencyRequests(ctx context.Context, id string) ([]*FrequencySpeakerRequest, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, frequency_id, pial, reason, status, upvotes, created_at
		FROM frequency_speaker_requests WHERE frequency_id = ? AND status = 'pending'
		ORDER BY upvotes DESC, created_at ASC`, id)
	if err != nil {
		return nil, err
	}
	out, err := scanFrequencyRequests(rows)
	if err != nil {
		return nil, err
	}
	return out, s.hydrateFrequencyRequests(ctx, out)
}

func scanFrequencyRequests(rows *sql.Rows) ([]*FrequencySpeakerRequest, error) {
	defer rows.Close()
	out := []*FrequencySpeakerRequest{}
	for rows.Next() {
		var r FrequencySpeakerRequest
		var created string
		if err := rows.Scan(&r.ID, &r.FrequencyID, &r.PIAL, &r.Reason, &r.Status, &r.Upvotes, &created); err != nil {
			return nil, err
		}
		r.CreatedAt = ParseTime(created)
		out = append(out, &r)
	}
	return out, rows.Err()
}

func (s *Store) hydrateFrequencyRequests(ctx context.Context, rs []*FrequencySpeakerRequest) error {
	if len(rs) == 0 {
		return nil
	}
	pials := make([]string, 0, len(rs))
	for _, r := range rs {
		pials = append(pials, r.PIAL)
	}
	users, err := s.GetUsersByPIAL(ctx, pials)
	if err != nil {
		return err
	}
	for _, r := range rs {
		r.User = users[r.PIAL]
	}
	return nil
}

// PendingFrequencyRequestFor is this person's own open request, or nil.
func (s *Store) PendingFrequencyRequestFor(ctx context.Context, id, pial string) (*FrequencySpeakerRequest, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, frequency_id, pial, reason, status, upvotes, created_at
		FROM frequency_speaker_requests WHERE frequency_id = ? AND pial = ? AND status = 'pending'`, id, pial)
	if err != nil {
		return nil, err
	}
	out, err := scanFrequencyRequests(rows)
	if err != nil || len(out) == 0 {
		return nil, err
	}
	return out[0], s.hydrateFrequencyRequests(ctx, out)
}

// GetFrequencyRequest loads one request by id, whatever its status.
func (s *Store) GetFrequencyRequest(ctx context.Context, id, requestID string) (*FrequencySpeakerRequest, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, frequency_id, pial, reason, status, upvotes, created_at
		FROM frequency_speaker_requests WHERE id = ? AND frequency_id = ?`, requestID, id)
	if err != nil {
		return nil, err
	}
	out, err := scanFrequencyRequests(rows)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, ErrFreqNotFound
	}
	return out[0], s.hydrateFrequencyRequests(ctx, out)
}

// ── Admission (domain/permissions.rs) ────────────────────────────────────────

// FrequencyRoleOnEntry is what the person would be on entry: the host is
// always the host, a granted co-host or speaker keeps that role across a
// rejoin, everyone else is a listener.
func FrequencyRoleOnEntry(f *Frequency, pial, granted string) string {
	if f.HostPIAL == pial {
		return FreqRoleHost
	}
	switch granted {
	case FreqRoleCohost:
		return FreqRoleCohost
	case FreqRoleSpeaker:
		return FreqRoleSpeaker
	}
	return FreqRoleListener
}

// FrequencyAdmit is the admission decision. The host is never refused by lock
// or capacity — the host coming back is what unlocks a stuck session — but is
// refused by state like anyone else.
func FrequencyAdmit(f *Frequency, role string, blocked bool, listenersNow, speakersNow int) *FreqError {
	if !FreqStateAcceptsJoins(f.State) {
		return ErrFreqNotLive
	}
	if blocked {
		return ErrFreqBlocked
	}
	if role == FreqRoleHost {
		return nil
	}
	if f.Locked && !FreqModerates(role) {
		return ErrFreqLocked
	}
	if FreqSpeaks(role) {
		if speakersNow >= f.MaxSpeakers {
			return ErrFreqSpeakersFull
		}
	} else if listenersNow >= f.MaxListeners {
		return ErrFreqFull
	}
	return nil
}

// FrequencyCounts is the room's presence, from rows: everyone joined, split by
// whether their role publishes audio.
func FrequencyCounts(ps []*FrequencyParticipant) (listeners, speakers, participants int) {
	for _, p := range ps {
		if !p.Present() {
			continue
		}
		if FreqSpeaks(p.Role) {
			speakers++
		} else {
			listeners++
		}
	}
	return listeners, speakers, listeners + speakers
}

// ── Writes ───────────────────────────────────────────────────────────────────

// CreateFrequency writes a new Frequency in draft, or scheduled when the input
// names a time. One open Frequency per host is enforced at start, not here:
// scheduled ones may stack up.
func (s *Store) CreateFrequency(ctx context.Context, in FrequencyInput) (*Frequency, error) {
	title := strings.TrimSpace(in.Title)
	if title == "" {
		return nil, freqBadRequest("Give the Frequency a title so people know what they are tuning into.")
	}
	if len([]rune(title)) > 120 {
		title = string([]rune(title)[:120])
	}
	desc := strings.TrimSpace(in.Description)
	if len([]rune(desc)) > 600 {
		desc = string([]rune(desc)[:600])
	}
	visibility := in.Visibility
	if visibility == "" {
		visibility = "public"
	}
	switch visibility {
	case "public", "followers", "subscribers", "private":
	default:
		return nil, freqBadRequest("unknown visibility")
	}
	language := in.Language
	if language == "" {
		language = "en"
	}
	maxSpeakers := in.MaxSpeakers
	if maxSpeakers == 0 {
		maxSpeakers = 10
	}
	if maxSpeakers < 1 || maxSpeakers > 50 {
		return nil, freqBadRequest("max_speakers must be between 1 and 50")
	}
	maxListeners := in.MaxListeners
	if maxListeners == 0 {
		maxListeners = 1000
	}
	if maxListeners < 1 || maxListeners > 100000 {
		return nil, freqBadRequest("max_listeners must be between 1 and 100000")
	}
	state := FreqStateDraft
	if in.ScheduledAt != nil {
		state = FreqStateScheduled
	}
	now := Now()
	id := NewID()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO frequencies (id, version, host_pial, title, description, state, visibility, language,
		                         adult_content, speaker_verity_min_tier, scheduled_at, recording_enabled,
		                         replay_status, max_speakers, max_listeners, requests_open, locked, created_at, updated_at)
		VALUES (?, 1, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, 'none', ?, ?, 1, 0, ?, ?)`,
		id, in.HostPIAL, title, desc, state, visibility, language,
		boolInt(in.AdultContent), nullTime(in.ScheduledAt), boolInt(in.RecordingEnabled),
		maxSpeakers, maxListeners, now, now)
	if err != nil {
		return nil, err
	}
	return s.GetFrequency(ctx, id)
}

// UpdateFrequency applies a patch and bumps the version. A Frequency that is
// over takes no edits.
func (s *Store) UpdateFrequency(ctx context.Context, f *Frequency, p FrequencyPatch) error {
	if FreqStateOver(f.State) {
		return ErrFreqAlreadyOver
	}
	sets := []string{}
	args := []any{}
	if p.Title != nil {
		title := strings.TrimSpace(*p.Title)
		if title == "" {
			return freqBadRequest("A Frequency needs a title.")
		}
		if len([]rune(title)) > 120 {
			title = string([]rune(title)[:120])
		}
		sets, args = append(sets, "title = ?"), append(args, title)
	}
	if p.Description != nil {
		desc := strings.TrimSpace(*p.Description)
		if len([]rune(desc)) > 600 {
			desc = string([]rune(desc)[:600])
		}
		sets, args = append(sets, "description = ?"), append(args, desc)
	}
	if p.Visibility != nil {
		switch *p.Visibility {
		case "public", "followers", "subscribers", "private":
		default:
			return freqBadRequest("unknown visibility")
		}
		sets, args = append(sets, "visibility = ?"), append(args, *p.Visibility)
	}
	if p.Language != nil && *p.Language != "" {
		sets, args = append(sets, "language = ?"), append(args, *p.Language)
	}
	if p.AdultContent != nil {
		sets, args = append(sets, "adult_content = ?"), append(args, boolInt(*p.AdultContent))
	}
	if p.RecordingEnabled != nil {
		// Recording cannot be switched while the session is running: the
		// consent people gave on the way in has to stay true.
		if FreqStateOpen(f.State) && *p.RecordingEnabled != f.RecordingEnabled {
			return freqErr(http.StatusConflict, "recording_locked_while_live", "Recording can't be changed while the Frequency is running.")
		}
		sets, args = append(sets, "recording_enabled = ?"), append(args, boolInt(*p.RecordingEnabled))
	}
	if p.MaxSpeakers != nil {
		if *p.MaxSpeakers < 1 || *p.MaxSpeakers > 50 {
			return freqBadRequest("max_speakers must be between 1 and 50")
		}
		sets, args = append(sets, "max_speakers = ?"), append(args, *p.MaxSpeakers)
	}
	if p.MaxListeners != nil {
		if *p.MaxListeners < 1 || *p.MaxListeners > 100000 {
			return freqBadRequest("max_listeners must be between 1 and 100000")
		}
		sets, args = append(sets, "max_listeners = ?"), append(args, *p.MaxListeners)
	}
	if len(sets) == 0 {
		return nil
	}
	sets = append(sets, "version = version + 1", "updated_at = ?")
	args = append(args, Now(), f.ID)
	_, err := s.db.ExecContext(ctx, `UPDATE frequencies SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
	return err
}

// ScheduleFrequency sets or clears the scheduled time. Clearing it puts a
// scheduled Frequency back in draft; setting it moves a draft to scheduled.
func (s *Store) ScheduleFrequency(ctx context.Context, f *Frequency, at *time.Time) error {
	to := FreqStateDraft
	if at != nil {
		to = FreqStateScheduled
	}
	if f.State != to && !FreqTransitionAllowed(f.State, to) {
		return ErrFreqBadTransition
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE frequencies SET scheduled_at = ?, state = ?, version = version + 1, updated_at = ? WHERE id = ?`,
		nullTime(at), to, Now(), f.ID)
	return err
}

// StartFrequency takes a draft or scheduled Frequency through starting to
// live, joins the host, and refuses a second open Frequency for the same host.
func (s *Store) StartFrequency(ctx context.Context, f *Frequency) error {
	if FreqStateOpen(f.State) {
		return ErrFreqHostLive
	}
	if !FreqTransitionAllowed(f.State, FreqStateStarting) {
		if FreqStateOver(f.State) {
			return ErrFreqAlreadyOver
		}
		return ErrFreqBadTransition
	}
	if open, err := s.OpenFrequencyFor(ctx, f.HostPIAL); err == nil && open.ID != f.ID {
		return ErrFreqHostLive
	} else if err != nil && !errors.Is(err, ErrFreqNotFound) {
		return err
	}
	now := Now()
	// starting → live is one step here: there is no media node to wait for,
	// and a state the client can never observe is a state that does not exist.
	res, err := s.db.ExecContext(ctx, `
		UPDATE frequencies SET state = 'live', started_at = ?, version = version + 2, updated_at = ?
		WHERE id = ? AND version = ? AND state = ?`, now, now, f.ID, f.Version, f.State)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return freqErr(http.StatusConflict, "version_conflict", "Someone else moved this Frequency first. Try again.")
	}
	return s.joinRow(ctx, f.ID, f.HostPIAL, FreqRoleHost)
}

// EndFrequency runs live → ending → ended, stamps the reason, and marks
// everyone still joined as left. Ending is not a state a client can catch
// here; it is written so the transition table is honoured rather than skipped.
func (s *Store) EndFrequency(ctx context.Context, f *Frequency, reason string) error {
	switch {
	case f.State == FreqStateEnding:
		return ErrFreqAlreadyEnding
	case FreqStateOver(f.State):
		return ErrFreqAlreadyOver
	case !FreqTransitionAllowed(f.State, FreqStateEnding):
		return ErrFreqBadTransition
	}
	if reason == "" {
		reason = "host_ended"
	}
	now := Now()
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`UPDATE frequencies SET state = 'ending', version = version + 1, updated_at = ? WHERE id = ?`, now, f.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE frequencies SET state = 'ended', ended_at = ?, end_reason = ?, version = version + 1, updated_at = ? WHERE id = ?`,
			now, reason, now, f.ID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`UPDATE frequency_participants SET state = 'left', left_at = ? WHERE frequency_id = ? AND state = 'joined'`, now, f.ID)
		return err
	})
}

// CancelFrequency calls off a Frequency that never started.
func (s *Store) CancelFrequency(ctx context.Context, f *Frequency) error {
	if f.State == FreqStateCancelled {
		return ErrFreqCancelled
	}
	if !FreqTransitionAllowed(f.State, FreqStateCancelled) {
		if FreqStateOpen(f.State) {
			return freqErr(http.StatusConflict, "invalid_transition", "A running Frequency is ended, not cancelled.")
		}
		return ErrFreqAlreadyOver
	}
	now := Now()
	_, err := s.db.ExecContext(ctx,
		`UPDATE frequencies SET state = 'cancelled', ended_at = ?, end_reason = 'cancelled', version = version + 1, updated_at = ? WHERE id = ?`,
		now, now, f.ID)
	return err
}

// SetFrequencyLock opens or closes the door.
func (s *Store) SetFrequencyLock(ctx context.Context, f *Frequency, locked bool) error {
	if FreqStateOver(f.State) {
		return ErrFreqAlreadyOver
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE frequencies SET locked = ?, version = version + 1, updated_at = ? WHERE id = ?`, boolInt(locked), Now(), f.ID); err != nil {
		return err
	}
	action := "unlock"
	if locked {
		action = "lock"
	}
	return s.logFrequencyAction(ctx, f.ID, f.HostPIAL, "", action, "")
}

// SetFrequencyRequestsOpen opens or closes the microphone queue.
func (s *Store) SetFrequencyRequestsOpen(ctx context.Context, f *Frequency, open bool) error {
	if FreqStateOver(f.State) {
		return ErrFreqAlreadyOver
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE frequencies SET requests_open = ?, version = version + 1, updated_at = ? WHERE id = ?`, boolInt(open), Now(), f.ID); err != nil {
		return err
	}
	action := "requests_closed"
	if open {
		action = "requests_opened"
	}
	return s.logFrequencyAction(ctx, f.ID, f.HostPIAL, "", action, "")
}

// ── Participation ────────────────────────────────────────────────────────────

func (s *Store) joinRow(ctx context.Context, id, pial, role string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO frequency_participants (frequency_id, pial, role, state, muted, joined_at)
		VALUES (?, ?, ?, 'joined', 0, ?)
		ON CONFLICT (frequency_id, pial) WHERE state = 'joined'
		DO UPDATE SET role = excluded.role`, id, pial, role, Now())
	return err
}

// JoinFrequency is Tune In: the admission decision, then the row. It answers
// the role the person entered as.
func (s *Store) JoinFrequency(ctx context.Context, f *Frequency, pial string) (string, error) {
	granted, err := s.FrequencyGrant(ctx, f.ID, pial)
	if err != nil {
		return "", err
	}
	role := FrequencyRoleOnEntry(f, pial, granted)
	blocked, err := s.FrequencyBlocked(ctx, f.ID, pial)
	if err != nil {
		return "", err
	}
	present, err := s.FrequencyParticipants(ctx, f.ID)
	if err != nil {
		return "", err
	}
	listeners, speakers, _ := FrequencyCounts(present)
	// Someone already in the room is not admitted twice, and is never refused
	// for capacity they are already part of.
	already := false
	for _, p := range present {
		if p.PIAL == pial {
			already = true
			break
		}
	}
	if !already {
		if refusal := FrequencyAdmit(f, role, blocked, listeners, speakers); refusal != nil {
			return "", refusal
		}
	} else if blocked {
		return "", ErrFreqBlocked
	}
	return role, s.joinRow(ctx, f.ID, pial, role)
}

// LeaveFrequency closes the person's joined row.
func (s *Store) LeaveFrequency(ctx context.Context, id, pial string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE frequency_participants SET state = 'left', left_at = ? WHERE frequency_id = ? AND pial = ? AND state = 'joined'`,
		Now(), id, pial)
	return err
}

// RemoveFromFrequency turns a joined row into a removed one and records who
// did it.
func (s *Store) RemoveFromFrequency(ctx context.Context, id, pial, by, reason string) error {
	now := Now()
	res, err := s.db.ExecContext(ctx, `
		UPDATE frequency_participants SET state = 'removed', removed_at = ?, remove_reason = ?, removed_by = ?
		WHERE frequency_id = ? AND pial = ? AND state = 'joined'`, now, reason, by, id, pial)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrFreqNotJoined
	}
	return s.logFrequencyAction(ctx, id, by, pial, "remove", reason)
}

// SetFrequencyMuted mutes or unmutes one joined participant.
func (s *Store) SetFrequencyMuted(ctx context.Context, id, pial, by string, muted bool) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE frequency_participants SET muted = ? WHERE frequency_id = ? AND pial = ? AND state = 'joined'`,
		boolInt(muted), id, pial)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrFreqNotJoined
	}
	action := "unmute"
	if muted {
		action = "mute"
	}
	return s.logFrequencyAction(ctx, id, by, pial, action, "")
}

// SetFrequencyParticipantRole changes a joined person's effective role.
func (s *Store) SetFrequencyParticipantRole(ctx context.Context, id, pial, role string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE frequency_participants SET role = ? WHERE frequency_id = ? AND pial = ? AND state = 'joined'`, role, id, pial)
	return err
}

// GrantFrequencyRole records a co-host or speaker grant, replacing any grant
// the person already holds.
func (s *Store) GrantFrequencyRole(ctx context.Context, id, pial, role, by string) error {
	now := Now()
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`UPDATE frequency_roles SET revoked_at = ?, revoked_by = ? WHERE frequency_id = ? AND pial = ? AND revoked_at IS NULL`,
			now, by, id, pial); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO frequency_roles (frequency_id, pial, role, granted_by, granted_at) VALUES (?, ?, ?, ?, ?)`,
			id, pial, role, by, now)
		return err
	})
}

// RevokeFrequencyRole ends the person's active grant, if any.
func (s *Store) RevokeFrequencyRole(ctx context.Context, id, pial, by string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE frequency_roles SET revoked_at = ?, revoked_by = ? WHERE frequency_id = ? AND pial = ? AND revoked_at IS NULL`,
		Now(), by, id, pial)
	return err
}

// BlockFromFrequency blocks for the life of the object and removes the person
// if they are in the room.
func (s *Store) BlockFromFrequency(ctx context.Context, id, pial, by, reason string) error {
	now := Now()
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO frequency_blocks (frequency_id, pial, blocked_by, reason, created_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (frequency_id, pial) DO UPDATE SET reason = excluded.reason`, id, pial, by, reason, now); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE frequency_participants SET state = 'removed', removed_at = ?, remove_reason = ?, removed_by = ?
		WHERE frequency_id = ? AND pial = ? AND state = 'joined'`, now, reason, by, id, pial); err != nil {
		return err
	}
	if err := s.RevokeFrequencyRole(ctx, id, pial, by); err != nil {
		return err
	}
	return s.logFrequencyAction(ctx, id, by, pial, "block", reason)
}

// UnblockFromFrequency lifts a block. It does not put anyone back in the room.
func (s *Store) UnblockFromFrequency(ctx context.Context, id, pial, by string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM frequency_blocks WHERE frequency_id = ? AND pial = ?`, id, pial); err != nil {
		return err
	}
	return s.logFrequencyAction(ctx, id, by, pial, "unblock", "")
}

// LogFrequencyModeration records one moderation action in the room's history.
func (s *Store) LogFrequencyModeration(ctx context.Context, id, actor, target, action, reason string) error {
	return s.logFrequencyAction(ctx, id, actor, target, action, reason)
}

func (s *Store) logFrequencyAction(ctx context.Context, id, actor, target, action, reason string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO frequency_moderation_actions (frequency_id, actor, target, action, reason, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`, id, actor, nullable(target), action, reason, Now())
	return err
}

// ── Request Mic ──────────────────────────────────────────────────────────────

// CreateSpeakerRequest opens one request. One pending request per person: a
// second call answers the one already standing rather than queueing twice.
func (s *Store) CreateSpeakerRequest(ctx context.Context, f *Frequency, pial, reason string) (*FrequencySpeakerRequest, error) {
	if f.State != FreqStateLive {
		return nil, ErrFreqNotLive
	}
	if !f.RequestsOpen {
		return nil, ErrFreqRequestsShut
	}
	reason = strings.TrimSpace(reason)
	if len([]rune(reason)) > 140 {
		reason = string([]rune(reason)[:140])
	}
	if existing, err := s.PendingFrequencyRequestFor(ctx, f.ID, pial); err != nil {
		return nil, err
	} else if existing != nil {
		return existing, nil
	}
	id := NewID()
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO frequency_speaker_requests (id, frequency_id, pial, reason, status, upvotes, created_at)
		VALUES (?, ?, ?, ?, 'pending', 0, ?)`, id, f.ID, pial, reason, Now()); err != nil {
		return nil, err
	}
	return s.GetFrequencyRequest(ctx, f.ID, id)
}

// ResolveSpeakerRequest closes a request as approved, declined or withdrawn.
func (s *Store) ResolveSpeakerRequest(ctx context.Context, id, requestID, status, by string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE frequency_speaker_requests SET status = ?, resolved_at = ?, resolved_by = ?
		WHERE id = ? AND frequency_id = ? AND status = 'pending'`, status, Now(), nullable(by), requestID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrFreqNotFound
	}
	return nil
}

// UpvoteSpeakerRequest counts one person's upvote, once. It answers whether
// this call was the one that counted and the running total.
func (s *Store) UpvoteSpeakerRequest(ctx context.Context, requestID, pial string) (bool, int, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO frequency_request_upvotes (request_id, pial, created_at) VALUES (?, ?, ?)`,
		requestID, pial, Now())
	if err != nil {
		return false, 0, err
	}
	counted := false
	if n, _ := res.RowsAffected(); n > 0 {
		counted = true
		if _, err := s.db.ExecContext(ctx,
			`UPDATE frequency_speaker_requests SET upvotes = upvotes + 1 WHERE id = ?`, requestID); err != nil {
			return false, 0, err
		}
	}
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT upvotes FROM frequency_speaker_requests WHERE id = ?`, requestID).Scan(&total); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, 0, ErrFreqNotFound
		}
		return false, 0, err
	}
	return counted, total, nil
}

// ── Seed ─────────────────────────────────────────────────────────────────────

// SeedFrequencies gives a dev database two Frequencies so the app's lanes have
// content on first launch: one live room hosted by @miiyazuko with a co-host,
// a muted speaker, two listeners and a question in the queue, and one
// scheduled for tomorrow hosted by @dev.
//
// Idempotent, and separately so from SeedIfEmpty: a database seeded before
// Frequencies existed still gets them on the next start. Every count the app
// draws comes from these rows — there is no synthetic listener number here,
// because a number that no row backs is a lie the UI would repeat.
func (s *Store) SeedFrequencies(ctx context.Context) (bool, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM frequencies`).Scan(&n); err != nil {
		return false, err
	}
	if n > 0 {
		return false, nil
	}
	users := map[string]*User{}
	for _, handle := range []string{"miiyazuko", "tehanibentley", "admin", "guest", "dev"} {
		u, err := s.GetUserByHandle(ctx, handle)
		if err != nil {
			// A database without the seeded accounts has nothing to host a
			// Frequency; that is not an error, there is just nothing to do.
			if errors.Is(err, ErrNotFound) {
				return false, nil
			}
			return false, fmt.Errorf("seed frequencies: %s: %w", handle, err)
		}
		users[handle] = u
	}

	live, err := s.CreateFrequency(ctx, FrequencyInput{
		HostPIAL:    users["miiyazuko"].PIALID,
		Title:       "Darkroom hours: the rooftop roll",
		Description: "Talking through the whole roll frame by frame. Hands up if you want in.",
		Visibility:  "public", Language: "en", MaxSpeakers: 4, MaxListeners: 1000,
	})
	if err != nil {
		return false, fmt.Errorf("seed live frequency: %w", err)
	}
	if err := s.StartFrequency(ctx, live); err != nil {
		return false, fmt.Errorf("seed start frequency: %w", err)
	}

	// @tehanibentley co-hosts: the grant first, then the room, so she enters
	// in the role the grant gives her exactly as a real rejoin would.
	if err := s.GrantFrequencyRole(ctx, live.ID, users["tehanibentley"].PIALID, FreqRoleCohost, users["miiyazuko"].PIALID); err != nil {
		return false, err
	}
	// @admin was approved for the microphone earlier in the session.
	if err := s.GrantFrequencyRole(ctx, live.ID, users["admin"].PIALID, FreqRoleSpeaker, users["miiyazuko"].PIALID); err != nil {
		return false, err
	}
	fresh, err := s.GetFrequency(ctx, live.ID)
	if err != nil {
		return false, err
	}
	for _, handle := range []string{"tehanibentley", "admin", "guest", "dev"} {
		if _, err := s.JoinFrequency(ctx, fresh, users[handle].PIALID); err != nil {
			return false, fmt.Errorf("seed join %s: %w", handle, err)
		}
	}
	if err := s.SetFrequencyMuted(ctx, live.ID, users["admin"].PIALID, users["miiyazuko"].PIALID, true); err != nil {
		return false, err
	}
	if _, err := s.CreateSpeakerRequest(ctx, fresh, users["guest"].PIALID, "got a question about the drop"); err != nil {
		return false, fmt.Errorf("seed request: %w", err)
	}

	tomorrow := time.Now().Add(24 * time.Hour).Truncate(time.Minute)
	if _, err := s.CreateFrequency(ctx, FrequencyInput{
		HostPIAL:    users["dev"].PIALID,
		Title:       "Build log: the native client, out loud",
		Description: "What shipped this week and what broke. Bring questions.",
		Visibility:  "public", Language: "en", ScheduledAt: &tomorrow,
		MaxSpeakers: 6, MaxListeners: 1000,
	}); err != nil {
		return false, fmt.Errorf("seed scheduled frequency: %w", err)
	}
	return true, nil
}
