package manhattan

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

// ── A throwaway outbox ────────────────────────────────────────────────────────
//
// The drain's contract with the database is the manhattan_outbox table as
// migrations 0005 and 0012 leave it, so that is what these tests create — in a
// database of their own, named by OUTBOX_TEST_DATABASE_URL, which must point at
// a role allowed to CREATE DATABASE. Unset, the tests report themselves skipped
// and pass; they never fake a database. Same convention as PURGE_TEST_DATABASE_URL.
//
//	OUTBOX_TEST_DATABASE_URL=postgres://f33d3r:f33d3rdev@localhost:5432/postgres?sslmode=disable \
//	    go test ./internal/manhattan/

var outboxTestSeq atomic.Int64

func outboxTestDB(t *testing.T) *sql.DB {
	t.Helper()
	url := os.Getenv("OUTBOX_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("OUTBOX_TEST_DATABASE_URL not set — database-backed drain tests skipped")
	}
	admin, err := sql.Open("postgres", url)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if err := admin.Ping(); err != nil {
		admin.Close()
		t.Fatalf("ping: %v", err)
	}
	name := fmt.Sprintf("outbox_test_%d_%d", time.Now().UnixNano(), outboxTestSeq.Add(1))
	if _, err := admin.Exec("CREATE DATABASE " + name); err != nil {
		admin.Close()
		t.Fatalf("creating %s: %v", name, err)
	}

	// Same server, this database: swap the path of the URL.
	slash := strings.LastIndex(url, "/")
	q := strings.Index(url[slash:], "?")
	testURL := url[:slash+1] + name
	if q >= 0 {
		testURL += url[slash+q:]
	}
	database, err := sql.Open("postgres", testURL)
	if err != nil {
		t.Fatalf("sql.Open(%s): %v", name, err)
	}
	if _, err := database.Exec(`
		CREATE TABLE manhattan_outbox (
		    id              BIGSERIAL   PRIMARY KEY,
		    op              TEXT        NOT NULL,
		    payload         JSONB       NOT NULL,
		    dedup_key       TEXT        NOT NULL UNIQUE,
		    attempts        INT         NOT NULL DEFAULT 0,
		    last_error      TEXT,
		    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		    delivered_at    TIMESTAMPTZ,
		    blocked_at      TIMESTAMPTZ,
		    blocked_reason  TEXT
		)`); err != nil {
		t.Fatalf("creating manhattan_outbox in %s: %v", name, err)
	}
	t.Cleanup(func() {
		database.Close()
		if _, err := admin.Exec("DROP DATABASE " + name); err != nil {
			t.Errorf("dropping %s: %v", name, err)
		}
		admin.Close()
	})
	return database
}

func enqueueNode(t *testing.T, database *sql.DB, name string) int64 {
	t.Helper()
	var id int64
	if err := database.QueryRow(`
		INSERT INTO manhattan_outbox (op, dedup_key, payload)
		VALUES ('node', $1, $2) RETURNING id`,
		"node:"+name,
		fmt.Sprintf(`{"kind":"work","name":%q,"namespace":"uuid"}`, name),
	).Scan(&id); err != nil {
		t.Fatalf("enqueue %s: %v", name, err)
	}
	return id
}

type outboxState struct {
	attempts  int
	delivered bool
	blocked   bool
	lastError sql.NullString
	reason    sql.NullString
	dueIn     time.Duration
}

func readRow(t *testing.T, database *sql.DB, id int64) outboxState {
	t.Helper()
	var s outboxState
	var deliveredAt, blockedAt sql.NullTime
	var dueSecs float64
	if err := database.QueryRow(`
		SELECT attempts, delivered_at, blocked_at, last_error, blocked_reason,
		       EXTRACT(EPOCH FROM (next_attempt_at - NOW()))
		  FROM manhattan_outbox WHERE id = $1`, id).
		Scan(&s.attempts, &deliveredAt, &blockedAt, &s.lastError, &s.reason, &dueSecs); err != nil {
		t.Fatalf("reading row %d: %v", id, err)
	}
	s.delivered = deliveredAt.Valid
	s.blocked = blockedAt.Valid
	s.dueIn = time.Duration(dueSecs * float64(time.Second))
	return s
}

// A stand-in Manhattan that answers /v1/apply. verdict decides each op's
// outcome; every request it sees is recorded so a test can check what the
// drain sent.
type fakePlane struct {
	srv      *httptest.Server
	requests atomic.Int64
	brains   []string
	batches  [][]ApplyOp
	verdict  func(op ApplyOp) ApplyResult
}

func newFakePlane(t *testing.T) *fakePlane {
	t.Helper()
	f := &fakePlane{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/v1/apply" {
			http.Error(w, `{"error":"not found","code":"not_found"}`, http.StatusNotFound)
			return
		}
		var body struct {
			Ops []ApplyOp `json:"ops"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.brains = append(f.brains, r.Header.Get(headerBrain))
		f.batches = append(f.batches, body.Ops)
		results := make([]ApplyResult, 0, len(body.Ops))
		halted := false
		for _, op := range body.Ops {
			if halted {
				results = append(results, ApplyResult{Outcome: OutcomeSkipped})
				continue
			}
			res := f.verdict(op)
			if res.Outcome == OutcomeRetry || res.Outcome == OutcomeError {
				halted = true
			}
			results = append(results, res)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"results": results})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func opName(op ApplyOp) string {
	var p struct {
		Name string `json:"name"`
	}
	json.Unmarshal(op.Payload, &p)
	return p.Name
}

// writeNode answers POST /v1/nodes the way Manhattan does.
func writeNode(w http.ResponseWriter, name string) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"node":{"node_id":"11111111-1111-1111-1111-111111111111","kind":"work","owner":"feed-engine","status":"active"},"names":[{"name":%q,"namespace":"uuid","is_primary":true,"status":"active"}]}`, name)
}

func accept(ApplyOp) ApplyResult {
	return ApplyResult{Outcome: OutcomeApplied, NodeID: "11111111-1111-1111-1111-111111111111"}
}

// ── What counts ───────────────────────────────────────────────────────────────

// A plane that cannot be reached says nothing about the row. The row keeps the
// head, is due again next tick, and its attempt count does not move — however
// long the outage lasts. Counting outages was how a seventy-minute outage
// quarantined a write that was never refused.
func TestAnOutageHoldsTheHeadAndCountsNothing(t *testing.T) {
	database := outboxTestDB(t)
	id := enqueueNode(t, database, "uuid:outage-1")
	enqueueNode(t, database, "uuid:outage-2")

	// A server that is closed before the drain runs: connection refused.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := dead.URL
	dead.Close()
	client := New(url)

	for i := 0; i < 15; i++ {
		// Make the row due again, as the passage of a tick would.
		if _, err := database.Exec(`UPDATE manhattan_outbox SET next_attempt_at = NOW()`); err != nil {
			t.Fatal(err)
		}
		delivered, err := DrainOnce(context.Background(), database, client)
		if err != nil {
			t.Fatalf("DrainOnce: %v", err)
		}
		if delivered != 0 {
			t.Fatalf("delivered %d rows to a dead plane", delivered)
		}
	}

	s := readRow(t, database, id)
	if s.attempts != 0 {
		t.Errorf("attempts = %d after 15 outage ticks; an outage must not count", s.attempts)
	}
	if s.blocked {
		t.Errorf("row was quarantined by an outage: %s", s.reason.String)
	}
	if s.delivered {
		t.Error("row marked delivered to a dead plane")
	}
	if !s.lastError.Valid || !strings.Contains(s.lastError.String, "connection refused") {
		t.Errorf("last_error should say what happened, got %q", s.lastError.String)
	}
	if s.dueIn <= 0 || s.dueIn > drainInterval {
		t.Errorf("row due in %s, want within one drain interval", s.dueIn)
	}

	// A server fault on the whole apply is an outage too, and so is a
	// Manhattan that predates the endpoint, and so is a per-op error verdict.
	for name, srv := range map[string]*httptest.Server{
		"500 on apply": httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		})),
		"no /v1/apply": httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		})),
	} {
		defer srv.Close()
		if _, err := database.Exec(`UPDATE manhattan_outbox SET next_attempt_at = NOW()`); err != nil {
			t.Fatal(err)
		}
		if _, err := DrainOnce(context.Background(), database, New(srv.URL)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if s := readRow(t, database, id); s.attempts != 0 || s.blocked || s.delivered {
			t.Errorf("%s counted or settled: %+v", name, s)
		}
	}
	faulty := newFakePlane(t)
	faulty.verdict = func(ApplyOp) ApplyResult {
		return ApplyResult{Outcome: OutcomeError, Code: "placement_contended", Error: "retry"}
	}
	if _, err := database.Exec(`UPDATE manhattan_outbox SET next_attempt_at = NOW()`); err != nil {
		t.Fatal(err)
	}
	if _, err := DrainOnce(context.Background(), database, New(faulty.srv.URL)); err != nil {
		t.Fatal(err)
	}
	if s := readRow(t, database, id); s.attempts != 0 || s.blocked || !strings.Contains(s.lastError.String, "placement_contended") {
		t.Errorf("an error verdict counted: %+v", s)
	}
}

// An answer from Manhattan that is not the end state counts toward giving up,
// and an answer that says the write will never land quarantines at once — while
// the queue behind it keeps moving.
func TestARefusalCountsAndAPermanentRefusalQuarantinesAtOnce(t *testing.T) {
	database := outboxTestDB(t)
	refused := enqueueNode(t, database, "uuid:refused")
	behind := enqueueNode(t, database, "uuid:behind")

	plane := newFakePlane(t)
	plane.verdict = func(op ApplyOp) ApplyResult {
		if opName(op) == "uuid:refused" {
			return ApplyResult{Outcome: OutcomeRefused, Code: "name_never_reissued", Error: "never"}
		}
		return accept(op)
	}

	delivered, err := DrainOnce(context.Background(), database, New(plane.srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if delivered != 1 {
		t.Errorf("delivered %d, want the row behind the quarantined one", delivered)
	}
	if s := readRow(t, database, refused); !s.blocked || s.attempts != 1 || !strings.Contains(s.reason.String, "name_never_reissued") {
		t.Errorf("permanent refusal not quarantined at once: %+v", s)
	}
	if s := readRow(t, database, behind); !s.delivered {
		t.Error("the row behind a quarantined row did not move")
	}

	// A retry verdict counts one attempt per answer and holds; the row behind
	// it is skipped by the plane and untouched here.
	held := enqueueNode(t, database, "uuid:held")
	behindHeld := enqueueNode(t, database, "uuid:behind-held")
	plane.verdict = func(op ApplyOp) ApplyResult {
		return ApplyResult{Outcome: OutcomeRetry, Code: "node_unresolved", Error: "not yet"}
	}
	for i := 0; i < 3; i++ {
		if _, err := database.Exec(`UPDATE manhattan_outbox SET next_attempt_at = NOW() WHERE id = $1`, held); err != nil {
			t.Fatal(err)
		}
		if _, err := DrainOnce(context.Background(), database, New(plane.srv.URL)); err != nil {
			t.Fatal(err)
		}
	}
	if s := readRow(t, database, held); s.attempts != 3 || s.blocked || s.delivered {
		t.Errorf("a retry verdict must count once per answer and hold: %+v", s)
	}
	if s := readRow(t, database, behindHeld); s.attempts != 0 || s.delivered || s.lastError.Valid {
		t.Errorf("a skipped row must be left untouched: %+v", s)
	}
	// And after maxAttempts retry verdicts the row is quarantined and the one
	// behind it moves.
	if _, err := database.Exec(`UPDATE manhattan_outbox SET attempts = $2, next_attempt_at = NOW() WHERE id = $1`, held, maxAttempts-1); err != nil {
		t.Fatal(err)
	}
	plane.verdict = func(op ApplyOp) ApplyResult {
		if opName(op) == "uuid:held" {
			return ApplyResult{Outcome: OutcomeRetry, Code: "node_unresolved", Error: "still not"}
		}
		return accept(op)
	}
	if _, err := DrainOnce(context.Background(), database, New(plane.srv.URL)); err != nil {
		t.Fatal(err)
	}
	if s := readRow(t, database, held); !s.blocked || !strings.Contains(s.reason.String, "12 times") {
		t.Errorf("row was not quarantined after maxAttempts answers: %+v", s)
	}
	if s := readRow(t, database, behindHeld); !s.delivered {
		t.Error("the row behind a quarantined row did not move on the next call")
	}
}

// Two replicas, one outbox, one drain at a time. A replica that finds the lock
// held delivers nothing and touches nothing.
func TestOnlyOneReplicaDrainsAtATime(t *testing.T) {
	database := outboxTestDB(t)
	id := enqueueNode(t, database, "uuid:locked")
	plane := newFakePlane(t)
	plane.verdict = accept

	holder, err := database.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	var got bool
	if err := holder.QueryRowContext(context.Background(), `SELECT pg_try_advisory_lock($1)`, drainLockKey).Scan(&got); err != nil || !got {
		t.Fatalf("taking the drain lock from the other replica: %v (%v)", err, got)
	}

	delivered, err := DrainOnce(context.Background(), database, New(plane.srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if delivered != 0 || plane.requests.Load() != 0 {
		t.Errorf("a second replica drained while the first held the lock (delivered %d, %d requests)", delivered, plane.requests.Load())
	}
	if s := readRow(t, database, id); s.delivered || s.attempts != 0 {
		t.Errorf("the row was touched: %+v", s)
	}

	if _, err := holder.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, drainLockKey); err != nil {
		t.Fatal(err)
	}
	if delivered, err := DrainOnce(context.Background(), database, New(plane.srv.URL)); err != nil || delivered != 1 {
		t.Errorf("after the lock was released the drain delivered %d (%v), want 1", delivered, err)
	}
}

// A queue deeper than one batch is drained in one call, batch after batch,
// until a batch comes back short — not one batch per ten-second tick.
func TestADrainRunsUntilTheQueueIsShort(t *testing.T) {
	database := outboxTestDB(t)
	const rows = 2*drainBatch + 50
	for i := 0; i < rows; i++ {
		enqueueNode(t, database, fmt.Sprintf("uuid:bulk-%d", i))
	}
	plane := newFakePlane(t)
	plane.verdict = accept

	delivered, err := DrainOnce(context.Background(), database, New(plane.srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if delivered != rows {
		t.Errorf("one DrainOnce delivered %d of %d", delivered, rows)
	}
	if got := plane.requests.Load(); got != 3 {
		t.Errorf("%d rows took %d requests, want 3 — one per batch", rows, got)
	}
	var left int
	if err := database.QueryRow(`SELECT COUNT(*) FROM manhattan_outbox WHERE delivered_at IS NULL`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("%d rows left undelivered", left)
	}
}

// ── Round trips ───────────────────────────────────────────────────────────────

// Registering a name that is already bound costs one request, because the
// refusal carries the holder. Resolving first, as this once did, made every
// replayed registration two.
func TestEnsureNodeIsOneRoundTripWhetherFreshOrReplayed(t *testing.T) {
	var posts, gets atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/nodes":
			posts.Add(1)
			var body struct {
				Name string `json:"name"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if body.Name == "uuid:already" {
				http.Error(w, `{"error":"bound","code":"name_exists","node_id":"22222222-2222-2222-2222-222222222222","kind":"work","owner":"feed-engine","status":"active"}`, http.StatusConflict)
				return
			}
			writeNode(w, body.Name)
		case r.Method == http.MethodGet:
			gets.Add(1)
			http.Error(w, `{"error":"nope","code":"name_unresolved"}`, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	client := New(srv.URL)

	fresh, err := client.EnsureNode(context.Background(), KindWork, "uuid:fresh", NamespaceUUID)
	if err != nil {
		t.Fatalf("fresh: %v", err)
	}
	replayed, err := client.EnsureNode(context.Background(), KindWork, "uuid:already", NamespaceUUID)
	if err != nil {
		t.Fatalf("replayed: %v", err)
	}
	if fresh.NodeID != "11111111-1111-1111-1111-111111111111" || replayed.NodeID != "22222222-2222-2222-2222-222222222222" {
		t.Errorf("wrong nodes: fresh=%s replayed=%s", fresh.NodeID, replayed.NodeID)
	}
	if replayed.Kind != "work" || replayed.Owner != "feed-engine" || replayed.Name != "uuid:already" {
		t.Errorf("the holder carried by the refusal was not read: %+v", replayed)
	}
	if posts.Load() != 2 || gets.Load() != 0 {
		t.Errorf("EnsureNode made %d POSTs and %d GETs for two names, want 2 and 0", posts.Load(), gets.Load())
	}
}

// What the drain sends is the row as the trigger wrote it, under this brain's
// name. Manhattan enforces association authority and node ownership at the
// wire, and it knows every payload shape; a drain that rebuilt rows would be a
// second copy of that knowledge.
func TestTheDrainForwardsRowsVerbatimUnderThisBrainsName(t *testing.T) {
	database := outboxTestDB(t)
	payload := `{"assoc": "follows", "subject_name": "pial:aaa", "object_name": "pial:bbb"}`
	if _, err := database.Exec(`INSERT INTO manhattan_outbox (op, dedup_key, payload) VALUES ('assoc', 'a', $1)`, payload); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO manhattan_outbox (op, dedup_key, payload) VALUES ('unassoc', 'b', $1)`, payload); err != nil {
		t.Fatal(err)
	}
	plane := newFakePlane(t)
	plane.verdict = func(op ApplyOp) ApplyResult { return ApplyResult{Outcome: OutcomeAlready} }

	if delivered, err := DrainOnce(context.Background(), database, New(plane.srv.URL)); err != nil || delivered != 2 {
		t.Fatalf("delivered %d (%v), want 2", delivered, err)
	}
	if len(plane.batches) != 1 || len(plane.batches[0]) != 2 {
		t.Fatalf("sent %d batches: %+v", len(plane.batches), plane.batches)
	}
	if plane.brains[0] != brainName {
		t.Errorf("brain header was %q, want %q", plane.brains[0], brainName)
	}
	for i, want := range []string{"assoc", "unassoc"} {
		got := plane.batches[0][i]
		if got.Op != want {
			t.Errorf("op %d was %q, want %q", i, got.Op, want)
		}
		var sent, wrote map[string]interface{}
		json.Unmarshal(got.Payload, &sent)
		json.Unmarshal([]byte(payload), &wrote)
		if fmt.Sprint(sent) != fmt.Sprint(wrote) {
			t.Errorf("op %d payload was rebuilt: sent %v, trigger wrote %v", i, sent, wrote)
		}
	}
}

// The classification the whole outage rule rests on.
func TestIsOutageSeparatesNoAnswerFromAnAnswer(t *testing.T) {
	cases := []struct {
		err    error
		outage bool
	}{
		{&transportError{Method: "GET", Path: "/x", Err: fmt.Errorf("dial tcp: connection refused")}, true},
		{&StatusError{Status: 502}, true},
		{&StatusError{Status: 500}, true},
		{ErrUnauthorized, true},
		{&StatusError{Status: 403}, false},
		{&StatusError{Status: 400}, false},
		{&ConflictError{Code: ConflictEdgeExists}, false},
		{ErrNotFound, false},
		{ErrNotConfigured, false},
		{nil, false},
	}
	for _, c := range cases {
		if got := IsOutage(c.err); got != c.outage {
			t.Errorf("IsOutage(%v) = %v, want %v", c.err, got, c.outage)
		}
	}
	for _, code := range []ConflictCode{ConflictNameNeverReissued, ConflictNameInQuarantine, ConflictTooDeep, ConflictEdgeCycle} {
		if !(&ConflictError{Code: code}).Permanent() {
			t.Errorf("%s must quarantine at once", code)
		}
	}
}
