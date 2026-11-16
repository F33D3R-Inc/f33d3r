// Package manhattan is feed-engine's client for the Manhattan naming plane.
//
// Manhattan is the one resolver. The rule it exists to enforce is that no brain
// may reference another brain's rows — it may only reference a name, and it
// resolves that name through Manhattan.
//
// That rule is why this package holds no queries. There is deliberately no
// database handle here and no way to add one: the moment a resolver can reach
// the tables it resolves, the temptation to "just join, it's right there" wins
// and the naming plane becomes decoration. Resolution crosses a process boundary
// because the boundary is the feature.
//
// What a name looks like: <namespace>:<value>. A work is uuid:<uuid> and
// cid:sha256:<hex>. A person is handle:<handle> and addr:<XXXX-XXXX>. The same
// node answers to all of its names, which is what makes copying a handle into
// every row that renders unnecessary — the copy existed because there was no
// cheap way to resolve a name, and ResolveBatch is that cheap way.
package manhattan

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// brainName is sent on every write so Manhattan can enforce node ownership at
// the wire. A write to a node this brain does not own is refused there, not
// here — an ownership rule a caller can decline to check is not a rule.
const brainName = "feed-engine"

const (
	headerBrain     = "X-F33D3R-Brain"
	headerRequestID = "X-Request-Id"
	// headerInternalKey carries INTERNAL_API_KEY, the estate-wide
	// service-to-service secret. Manhattan authenticates every call with it:
	// the brain name above is a claim, and without the key anyone on the
	// network could make it and bind or retire names as any brain.
	headerInternalKey = "X-Internal-Key"
)

// Node kinds. These mirror the kind CHECK constraint in Manhattan's schema.
const (
	KindIdentity = "identity"
	KindWork     = "work"
	KindMedia    = "media"
	KindKey      = "key"
	KindAddress  = "address"
	KindFacet    = "facet"
)

// Edge predicates. These mirror the predicate CHECK constraint in Manhattan's schema.
const (
	PredicateQuotes      = "quotes"
	PredicateRepliesTo   = "replies_to"
	PredicateAuthoredBy  = "authored_by"
	PredicateDerivedFrom = "derived_from"
	PredicateResolvesTo  = "resolves_to"
	PredicateSignsFor    = "signs_for"
	PredicateContains    = "contains"
)

// Namespaces. A name is always stored and resolved in its namespaced form, so
// two namespaces can never collide on the same literal string.
const (
	NamespaceHandle  = "handle"
	NamespaceUUID    = "uuid"
	NamespaceCID     = "cid"
	NamespaceAddress = "addr"
	// A person's identity is named by PIAL, never by handle. A handle is a
	// pointer that can be transferred, sold or reclaimed; PIAL is the root that
	// does not move. Binding the handle as a secondary name is what makes a
	// handle change a rename rather than a migration.
	NamespacePIAL = "pial"
)

// ErrNotConfigured is returned when no Manhattan URL is set. Callers must treat
// this as "the naming plane is not reachable from this deployment yet" and log
// it — never as licence to fall back to a local join, which is the exact drift
// this package exists to end.
var ErrNotConfigured = errors.New("manhattan: no MANHATTAN_URL configured")

// ErrNotFound is returned when a name resolves to nothing, or resolves to
// something that has been revoked. A revoked name is indistinguishable from an
// unknown one by design: a rotated contact address must not confirm that it was
// ever real.
var ErrNotFound = errors.New("manhattan: name does not resolve")

// ErrConflict is what every 409 unwraps to. It is never returned bare: a 409
// always arrives as *ConflictError, whose Code says which of the several
// conditions behind that status actually occurred.
var ErrConflict = errors.New("manhattan: conflict")

// ConflictCode is Manhattan's machine-readable reason for a 409.
//
// Several different conditions answer 409, and only some of them mean the write
// this client asked for is already done. The others mean it did not happen — and
// two of them mean it never will. A caller that cannot tell them apart marks
// naming-plane writes delivered that never landed, which is why Manhattan sends
// a stable code beside the human message and why this type exists. Codes are
// append-only; an existing one never changes meaning.
type ConflictCode string

const (
	// The name is already actively bound. Manhattan sends the holder's node_id,
	// kind, owner and status alongside, so a caller can tell "already bound to
	// the node I meant, carry on" from "this name points at another identity"
	// without a second round trip.
	ConflictNameExists ConflictCode = "name_exists"
	// The edge is already present — an at-least-once writer's replay, and the
	// end state it wanted.
	ConflictEdgeExists ConflictCode = "edge_exists"
	// A revoke of a name already retired. Also the end state the caller wanted.
	ConflictNameAlreadyRevoked ConflictCode = "name_already_revoked"
	// The name was revoked and its namespace never reissues one. The write did
	// not happen and no retry will change that.
	ConflictNameNeverReissued ConflictCode = "name_never_reissued"
	// The name was revoked and its namespace holds a revoked name before
	// reissuing it — a Number sits out twelve months. The write did not
	// happen. Not permanent, but the hold outlasts any retry schedule this
	// queue keeps, so the queue treats it as it treats name_never_reissued and
	// an operator reads a different reason.
	ConflictNameInQuarantine ConflictCode = "name_in_quarantine"
	// The edge would sit past Manhattan's depth ceiling, or would push a tree
	// already placed beneath its subject past it. The write did not happen and
	// no retry will change that.
	ConflictTooDeep ConflictCode = "too_deep"
	// The object already hangs from the subject, so the edge would close the
	// chain into a loop. The write did not happen and no retry will change that.
	ConflictEdgeCycle ConflictCode = "edge_cycle"
	// A delete refused because edges sit beneath this one. Removing those first
	// makes it succeed, so unlike the two above this refusal can still clear.
	ConflictEdgeHasDescendants ConflictCode = "edge_has_descendants"
	// A mint against a suspended or retired identity.
	ConflictIdentityNotActive ConflictCode = "identity_not_active"
	// A revoke of an address already retired.
	ConflictAddressAlreadyRevoked ConflictCode = "address_already_revoked"
	// A revoke of a Number already retired.
	ConflictNumberAlreadyRevoked ConflictCode = "number_already_revoked"
)

// ConflictError is a 409 together with the reason Manhattan gave for it.
//
// A code this build does not recognise — an older Manhattan that sends none, or
// a condition added since — is carried through verbatim and is NEVER success.
// Treating an unrecognised refusal as delivered is the exact failure the codes
// exist to end.
type ConflictError struct {
	Code   ConflictCode
	Detail string

	// The node the name is already bound to, when Manhattan could still resolve
	// it. Sent only with name_exists, and the same projection /v1/resolve/:name
	// serves publicly, so nothing here could not already be read.
	Holder       string
	HolderKind   string
	HolderOwner  string
	HolderStatus string
}

func (e *ConflictError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("manhattan: unrecognised conflict: %s", e.Detail)
	}
	if e.Holder != "" {
		return fmt.Sprintf("manhattan: %s (held by node %s, kind %s, owned by %s, status %s)",
			e.Code, e.Holder, e.HolderKind, e.HolderOwner, e.HolderStatus)
	}
	return fmt.Sprintf("manhattan: %s: %s", e.Code, e.Detail)
}

// Unwrap makes errors.Is(err, ErrConflict) answer for every 409, whatever its
// code — including one this build has never heard of.
func (e *ConflictError) Unwrap() error { return ErrConflict }

// Permanent reports whether Manhattan has stated the write will not be
// accepted by any retry a queue could schedule: never (name_never_reissued,
// too_deep, edge_cycle), or not for months (name_in_quarantine). A queue
// holding such a row must quarantine it rather than block behind it, since no
// number of retries can change the answer within the queue's horizon.
func (e *ConflictError) Permanent() bool {
	switch e.Code {
	case ConflictNameNeverReissued, ConflictNameInQuarantine, ConflictTooDeep, ConflictEdgeCycle:
		return true
	}
	return false
}

// Recognised reports whether this build knows what the code means. An
// unrecognised conflict says nothing about whether the write landed, so a caller
// must verify or retry — never assume.
func (e *ConflictError) Recognised() bool {
	switch e.Code {
	case ConflictNameExists, ConflictEdgeExists, ConflictNameAlreadyRevoked,
		ConflictNameNeverReissued, ConflictNameInQuarantine, ConflictTooDeep,
		ConflictEdgeCycle, ConflictEdgeHasDescendants, ConflictIdentityNotActive,
		ConflictAddressAlreadyRevoked, ConflictNumberAlreadyRevoked:
		return true
	}
	return false
}

// parseConflict reads Manhattan's refusal body. A body that will not parse, or
// carries no code, yields a ConflictError with an empty Code — which Recognised
// reports as unknown and no caller may treat as success.
func parseConflict(body []byte) *ConflictError {
	var wire struct {
		Error  string `json:"error"`
		Code   string `json:"code"`
		NodeID string `json:"node_id"`
		Kind   string `json:"kind"`
		Owner  string `json:"owner"`
		Status string `json:"status"`
	}
	detail := strings.TrimSpace(string(body))
	if detail == "" {
		detail = "409 with no body"
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return &ConflictError{Detail: detail}
	}
	if wire.Error != "" {
		detail = wire.Error
	}
	return &ConflictError{
		Code:         ConflictCode(wire.Code),
		Detail:       detail,
		Holder:       wire.NodeID,
		HolderKind:   wire.Kind,
		HolderOwner:  wire.Owner,
		HolderStatus: wire.Status,
	}
}

// StatusError is a response from Manhattan with a status this client has no
// specific meaning for: neither success, nor 404, nor 409. It keeps the status
// so a caller can tell a server fault from a refusal of the request.
type StatusError struct {
	Method string
	Path   string
	Status int
	Detail string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("manhattan: %s %s: http %d: %s", e.Method, e.Path, e.Status, e.Detail)
}

// ErrUnauthorized is what a 401 unwraps to: Manhattan rejected the internal key
// this client sent. The key in this process's environment does not match
// Manhattan's, and no retry will change that until one of them is rotated.
var ErrUnauthorized = errors.New("manhattan: internal key rejected — INTERNAL_API_KEY differs from Manhattan's")

// IsOutage reports whether err says nothing about the write that was attempted:
// the plane could not be reached, it answered with a server fault, or it
// refused this brain's key. Every one of those is a fact about the deployment,
// not about the row, and a queue must not count it toward giving up on the row —
// an outage of an hour would otherwise quarantine whatever write happened to be
// at the head of the queue when it began, and an operator would then have to
// un-quarantine writes that were never refused. Only an answer FROM Manhattan
// about THIS write counts, and a 409, a 404, a 403 and a 4xx are answers.
func IsOutage(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrUnauthorized) {
		return true
	}
	var status *StatusError
	if errors.As(err, &status) {
		return status.Status >= 500
	}
	var conflict *ConflictError
	if errors.As(err, &conflict) || errors.Is(err, ErrNotFound) || errors.Is(err, ErrNotConfigured) {
		return false
	}
	var transport *transportError
	return errors.As(err, &transport)
}

// transportError wraps a failure to get any response at all — a refused
// connection, a timeout, a body that would not decode.
type transportError struct {
	Method string
	Path   string
	Err    error
}

func (e *transportError) Error() string {
	return fmt.Sprintf("manhattan: %s %s: %v", e.Method, e.Path, e.Err)
}

func (e *transportError) Unwrap() error { return e.Err }

// Client talks to the Manhattan brain over HTTP.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// New returns a client for the Manhattan brain at baseURL. An empty baseURL
// yields a client whose every call returns ErrNotConfigured, so a deployment
// without Manhattan fails loudly at the call site instead of silently skipping
// graph writes.
//
// The internal key is read from INTERNAL_API_KEY — the same variable
// config.Load reads into Config.InternalAPIKey — so the client and the rest of
// the process can never disagree about which secret is in force.
func New(baseURL string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  os.Getenv("INTERNAL_API_KEY"),
		http: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
}

// Configured reports whether this client can reach a naming plane at all.
func (c *Client) Configured() bool { return c != nil && c.baseURL != "" }

// ── Wire types ───────────────────────────────────────────────────────────────

// Node is a thing with an identity, and an owner that is the only brain allowed
// to write facts about it.
//
// A resolution returns the node flat, with the single name that was resolved. A
// node fetch returns it wrapped alongside every name bound to it — see nodeEnvelope.
type Node struct {
	NodeID    string `json:"node_id"`
	Kind      string `json:"kind"`
	Owner     string `json:"owner"`
	Status    string `json:"status"`
	Name      string `json:"name,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`

	// Names is every active name bound to this node, populated only by the
	// calls that return the full node. It is not part of the wire shape;
	// nodeEnvelope flattens the server's name rows into it.
	Names []string `json:"-"`
}

// nodeEnvelope is the wire shape of the calls that return a node together with
// its names: {"node": {...}, "names": [{"name": "...", ...}, ...]}.
type nodeEnvelope struct {
	Node  Node `json:"node"`
	Names []struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
		IsPrimary bool   `json:"is_primary"`
		Status    string `json:"status"`
	} `json:"names"`
}

// node flattens the envelope into a Node carrying only its live names. A
// revoked name is dropped here rather than at every call site, so a rotated
// handle can never be read back as current.
func (e nodeEnvelope) node() *Node {
	n := e.Node
	n.Names = make([]string, 0, len(e.Names))
	for _, row := range e.Names {
		if row.Status == "active" {
			n.Names = append(n.Names, row.Name)
		}
	}
	return &n
}

// Edge is a typed relationship carrying the chain it belongs to. root and depth
// are assigned by Manhattan, never supplied — which is what makes a bounded
// thread read a predicate rather than a walk.
type Edge struct {
	Subject   string `json:"subject"`
	Predicate string `json:"predicate"`
	Object    string `json:"object"`
	Root      string `json:"root"`
	Depth     int    `json:"depth"`
	// When the edge was recorded, as Manhattan serialises it. Only needed to
	// build the cursor for the next page of a listing.
	CreatedAt string `json:"created_at,omitempty"`
}

// Cursor is the keyset cursor that continues a listing after this edge —
// depth~created_at~subject~object, the four columns every edge listing is
// ordered by. Empty when the plane that served it sent no timestamp, in which
// case the listing cannot be continued.
func (e Edge) Cursor() string {
	if e.CreatedAt == "" {
		return ""
	}
	return fmt.Sprintf("%d~%s~%s~%s", e.Depth, e.CreatedAt, e.Subject, e.Object)
}

// Address is a public, cheap, revocable contact credential pointing at an
// identity. Rotating one mints a new address and revokes the old; the identity
// behind it never moves, so no established conversation is touched.
type Address struct {
	Address        string `json:"address"`
	AddressNodeID  string `json:"address_node_id,omitempty"`
	IdentityNodeID string `json:"identity_node_id,omitempty"`
	Status         string `json:"status"`
	CreatedAt      string `json:"created_at,omitempty"`
}

// ── Name construction ────────────────────────────────────────────────────────

// Name builds the namespaced form of a name. Always route a name through this
// rather than concatenating, so every caller produces the same string for the
// same thing.
func Name(namespace, value string) string {
	return namespace + ":" + value
}

// WorkName is the canonical name for a work: its UUID.
func WorkName(workID string) string { return Name(NamespaceUUID, workID) }

// WorkCIDName is a work's content-addressed name. A work answers to both; this
// is what lets a caller holding only a CID resolve without knowing the UUID.
func WorkCIDName(cid string) string { return Name(NamespaceCID, cid) }

// HandleName is an identity's public handle name — a pointer to the identity,
// not the identity itself.
func HandleName(handle string) string { return Name(NamespaceHandle, handle) }

// PIALName is the canonical name for an identity.
func PIALName(pialID string) string { return Name(NamespacePIAL, pialID) }

// AddressName is the name of a contact address.
func AddressName(addr string) string { return Name(NamespaceAddress, addr) }

// ── Resolution ───────────────────────────────────────────────────────────────

// Resolve returns the node a name points at. This is the hot path — one
// indexed lookup on Manhattan's side.
func (c *Client) Resolve(ctx context.Context, name string) (*Node, error) {
	var out Node
	err := c.do(ctx, http.MethodGet, "/v1/resolve/"+urlSegment(name), nil, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ResolveBatch resolves many names in one round trip and returns only the ones
// that resolved. This is the call that removes the reason handles were copied
// into every row that renders: a whole page of names costs one request.
func (c *Client) ResolveBatch(ctx context.Context, names []string) (map[string]Node, error) {
	if len(names) == 0 {
		return map[string]Node{}, nil
	}
	var out struct {
		Resolved map[string]Node `json:"resolved"`
	}
	body := map[string]interface{}{"names": names}
	if err := c.do(ctx, http.MethodPost, "/v1/resolve/batch", body, &out); err != nil {
		return nil, err
	}
	if out.Resolved == nil {
		out.Resolved = map[string]Node{}
	}
	return out.Resolved, nil
}

// ── Nodes and names ──────────────────────────────────────────────────────────

// EnsureNode registers a node and binds name as its primary name. It is
// idempotent on the name: registering a name that already resolves returns the
// node it already resolves to rather than minting a second identity for the
// same thing. Two nodes for one thing is the failure this whole plane exists to
// prevent, so the client must never be the thing that creates one.
//
// One round trip in both the fresh and the replayed case. The create is
// attempted directly; a name already bound answers name_exists, and that
// refusal carries the holder — node id, kind, owner, status — so the replay
// costs nothing more than the create did. Resolving first, as this once did,
// doubled the cost of every registration the drain delivered.
func (c *Client) EnsureNode(ctx context.Context, kind, name, namespace string) (*Node, error) {
	if !c.Configured() {
		return nil, ErrNotConfigured
	}
	var out nodeEnvelope
	body := map[string]interface{}{
		"kind":      kind,
		"owner":     brainName,
		"name":      name,
		"namespace": namespace,
	}
	err := c.do(ctx, http.MethodPost, "/v1/nodes", body, &out)
	if err == nil {
		return out.node(), nil
	}
	var conflict *ConflictError
	if !errors.As(err, &conflict) {
		return nil, err
	}
	// A permanent refusal is not a lost race and must not be reported as one —
	// Manhattan refuses reissue on node creation too, so a name whose namespace
	// never reissues answers name_never_reissued here, and resolving it would
	// then return ErrNotFound, turning a plain statement that the write will
	// never happen into a puzzle for whoever reads the queue.
	if conflict.Permanent() {
		return nil, err
	}
	// Already bound, and Manhattan said to what.
	if conflict.Code == ConflictNameExists && conflict.Holder != "" {
		return &Node{
			NodeID: conflict.Holder,
			Kind:   conflict.HolderKind,
			Owner:  conflict.HolderOwner,
			Status: conflict.HolderStatus,
			Name:   name,
		}, nil
	}
	// Bound, but by a Manhattan too old to say to what: resolve it.
	return c.Resolve(ctx, name)
}

// GetNode returns a node and every active name bound to it. Used when a caller
// holds a node id — from an address resolution, say — and needs the identity's
// canonical PIAL name to go on with.
func (c *Client) GetNode(ctx context.Context, nodeID string) (*Node, error) {
	var out nodeEnvelope
	if err := c.do(ctx, http.MethodGet, "/v1/nodes/"+urlSegment(nodeID), nil, &out); err != nil {
		return nil, err
	}
	return out.node(), nil
}

// PrimaryNameIn returns this node's name in the given namespace, or "" if it has
// none. A node answers to many names; this is how a caller asks for the one it
// can actually use.
func (n *Node) PrimaryNameIn(namespace string) string {
	prefix := namespace + ":"
	for _, name := range n.Names {
		if strings.HasPrefix(name, prefix) {
			return strings.TrimPrefix(name, prefix)
		}
	}
	return ""
}

// BindName attaches an additional name to an existing node — a work's CID
// alongside its UUID, a new handle alongside an identity.
func (c *Client) BindName(ctx context.Context, nodeID, name, namespace string) error {
	body := map[string]interface{}{
		"node_id":   nodeID,
		"name":      name,
		"namespace": namespace,
	}
	return c.do(ctx, http.MethodPost, "/v1/names", body, nil)
}

// RevokeName retires a name. The row survives revocation so the name can never
// be re-minted to someone else.
func (c *Client) RevokeName(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodPost, "/v1/names/"+urlSegment(name)+"/revoke", nil, nil)
}

// ── Edges ────────────────────────────────────────────────────────────────────

// AddEdge records a typed relationship. Manhattan assigns root and depth and
// refuses an edge that would close a cycle, so the returned Edge is the
// authoritative account of where this relationship sits in its chain.
func (c *Client) AddEdge(ctx context.Context, subject, predicate, object string) (*Edge, error) {
	var out Edge
	body := map[string]interface{}{
		"subject":   subject,
		"predicate": predicate,
		"object":    object,
	}
	if err := c.do(ctx, http.MethodPost, "/v1/edges", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ── Associations ─────────────────────────────────────────────────────────────
//
// An association is a relationship with no lineage — no root, no depth, nothing
// placed beneath it. Manhattan keeps them in their own table for exactly that
// reason (its migration 0007); forcing a follow into `edges` would mean writing
// an invented root and depth into two NOT NULL columns.
//
// These two calls are name-addressed, unlike AddEdge. This brain asserts who
// follows whom, but the identity nodes on both ends belong to elohim-veni, so it
// must never need to hold that brain's node ids. It holds names; Manhattan
// resolves them. That is the whole rule, applied to the one place it would have
// been easiest to break.

// AssocFollows is the association this brain publishes: the follow graph, whose
// system of record stays in f33d3r_feed.
const AssocFollows = "follows"

// Assoc is what a write or retraction did. Changed is false when the write was a
// replay of one already recorded, or a retraction of one already absent — both
// of which are the end state the caller asked for, which is why neither is a
// conflict.
type Assoc struct {
	Assoc       string `json:"assoc"`
	SubjectName string `json:"subject_name"`
	ObjectName  string `json:"object_name"`
	Subject     string `json:"subject"`
	Object      string `json:"object"`
	Changed     bool   `json:"changed"`
}

// AddAssoc records an association between two named nodes. Idempotent: asserting
// one that already exists succeeds with Changed false rather than answering 409,
// so unlike an edge write there is no conflict for a drain to interpret.
func (c *Client) AddAssoc(ctx context.Context, subjectName, assoc, objectName string) (*Assoc, error) {
	var out Assoc
	body := map[string]interface{}{
		"assoc":        assoc,
		"subject_name": subjectName,
		"object_name":  objectName,
	}
	if err := c.do(ctx, http.MethodPost, "/v1/assocs", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RemoveAssoc retracts an association. Idempotent for the same reason, and it
// can never be refused: nothing is ever placed beneath an association, so a
// retraction has no descendants to orphan. That matters here because this
// retraction is what stops a contact policy honouring a follow that no longer
// exists.
func (c *Client) RemoveAssoc(ctx context.Context, subjectName, assoc, objectName string) (*Assoc, error) {
	var out Assoc
	body := map[string]interface{}{
		"assoc":        assoc,
		"subject_name": subjectName,
		"object_name":  objectName,
	}
	if err := c.do(ctx, http.MethodDelete, "/v1/assocs", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Edge returns one edge by its key, or ErrNotFound. This is how a caller learns
// whether an edge is present: a primary-key probe on Manhattan's side, so the
// answer is the same on a subject with five edges and one with five million. A
// listing can be truncated; this cannot.
func (c *Client) Edge(ctx context.Context, subject, predicate, object string) (*Edge, error) {
	var out Edge
	path := fmt.Sprintf("/v1/edges/one/%s/%s/%s", urlSegment(subject), urlSegment(predicate), urlSegment(object))
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// MaxEdgeList mirrors Manhattan's ceiling on one page of an edge listing.
const MaxEdgeList = 5000

// EdgesOut returns one page of the edges leaving a subject node, shallowest and
// oldest first, optionally filtered by predicate. An empty predicate means all
// of them. A page holds at most MaxEdgeList edges; a full page is continued by
// passing the last edge's Cursor() as after. To ask whether ONE edge is present,
// use Edge instead — it cannot be truncated.
//
// /v1/edges/out answers with a bare JSON array, not the {"edges": [...]} envelope
// /v1/edges/thread uses.
func (c *Client) EdgesOut(ctx context.Context, subjectNodeID, predicate, after string) ([]Edge, error) {
	path := fmt.Sprintf("/v1/edges/out/%s?limit=%d", urlSegment(subjectNodeID), MaxEdgeList)
	if predicate != "" {
		path += "&predicate=" + urlSegment(predicate)
	}
	if after != "" {
		path += "&after=" + urlSegment(after)
	}
	var out []Edge
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Thread returns every edge under a root, bounded by depth. The bound is the
// query, not a counter in the caller.
func (c *Client) Thread(ctx context.Context, root string, maxDepth int) ([]Edge, error) {
	var out struct {
		Edges []Edge `json:"edges"`
	}
	path := fmt.Sprintf("/v1/edges/thread/%s?max_depth=%d", urlSegment(root), maxDepth)
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out.Edges, nil
}

// ── Addresses ────────────────────────────────────────────────────────────────

// No MintAddress/RevokeAddress here: Manhattan owns `address`/`addr` to
// elohim-veni and rejects this brain's writes. Mutations go via elohim-veni
// (POST /v1/addresses/mint, /revoke). Reads below need no ownership.

// ResolveAddress turns a live contact address into the identity it grants a
// channel to. Returns ErrNotFound for an unknown or revoked address.
func (c *Client) ResolveAddress(ctx context.Context, addr string) (*Address, error) {
	var out Address
	if err := c.do(ctx, http.MethodGet, "/v1/addresses/"+urlSegment(addr), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// No ListAddresses: Manhattan gates that read to the identity's owning brain.
// Listing goes through elohim-veni (POST /v1/addresses/list).

// ── Batch application ────────────────────────────────────────────────────────

// ApplyOp is one outbox row, forwarded exactly as the trigger wrote it: the op
// name and the payload. Manhattan knows every payload shape the triggers
// produce, so nothing here rebuilds a row.
type ApplyOp struct {
	Op      string          `json:"op"`
	Payload json.RawMessage `json:"payload"`
}

// The six verdicts an apply can give one op. They are the whole of what a
// drain acts on; see ApplyResult.
const (
	OutcomeApplied = "applied"
	OutcomeAlready = "already"
	OutcomeRetry   = "retry"
	OutcomeRefused = "refused"
	OutcomeError   = "error"
	OutcomeSkipped = "skipped"
)

// ApplyResult is Manhattan's verdict on one op.
//
// applied and already are delivered. refused is quarantined and the batch
// carries on past it. retry holds its place and counts the attempt. error holds
// its place and does not count — the plane, not the row, is at fault. skipped
// was never attempted because an earlier op held, and is left as it was.
// Code is the same machine-readable code the single-op route would have sent.
type ApplyResult struct {
	Outcome string `json:"outcome"`
	Code    string `json:"code,omitempty"`
	Error   string `json:"error,omitempty"`
	NodeID  string `json:"node_id,omitempty"`
}

// MaxApplyOps mirrors Manhattan's ceiling on one apply.
const MaxApplyOps = 500

// Apply sends up to MaxApplyOps outbox rows in order and returns one verdict
// per row, in the same order. One request per batch is what makes an outbox
// drain fast; a verdict per row decided by the plane is what makes it right
// without a copy of the plane's rules in every brain.
//
// ErrNotFound from this call means the Manhattan reached predates the
// endpoint. Nothing was applied.
func (c *Client) Apply(ctx context.Context, ops []ApplyOp) ([]ApplyResult, error) {
	if len(ops) == 0 {
		return nil, nil
	}
	var out struct {
		Results []ApplyResult `json:"results"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/apply", map[string]interface{}{"ops": ops}, &out); err != nil {
		return nil, err
	}
	if len(out.Results) != len(ops) {
		return nil, &transportError{Method: http.MethodPost, Path: "/v1/apply",
			Err: fmt.Errorf("apply answered %d verdicts for %d ops", len(out.Results), len(ops))}
	}
	return out.Results, nil
}

// ── Transport ────────────────────────────────────────────────────────────────

func (c *Client) do(ctx context.Context, method, path string, body interface{}, out interface{}) error {
	if !c.Configured() {
		return ErrNotConfigured
	}

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("manhattan: encoding %s %s: %w", method, path, err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("manhattan: building %s %s: %w", method, path, err)
	}
	req.Header.Set(headerBrain, brainName)
	req.Header.Set(headerInternalKey, c.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if rid, ok := ctx.Value(requestIDKey).(string); ok && rid != "" {
		req.Header.Set(headerRequestID, rid)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return &transportError{Method: method, Path: path, Err: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return ErrUnauthorized
	}
	// A conflict's body is read where a 404's is not: 409 covers several
	// different conditions and the code naming which one is in there. Discarding
	// it is how a refusal became a success.
	if resp.StatusCode == http.StatusConflict {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return parseConflict(detail)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return &StatusError{Method: method, Path: path, Status: resp.StatusCode,
			Detail: strings.TrimSpace(string(detail))}
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return &transportError{Method: method, Path: path, Err: fmt.Errorf("decoding response: %w", err)}
	}
	return nil
}

type ctxKey string

const requestIDKey ctxKey = "request_id"

// WithRequestID propagates a request id across the brain boundary so one user
// action stays one traceable line through every service it touches.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// urlSegment escapes a value for use as a single path segment. Names contain a
// colon by construction, and an address contains a dash; neither may be allowed
// to change the shape of the path it is placed in.
func urlSegment(s string) string {
	return strings.NewReplacer("/", "%2F", "?", "%3F", "#", "%23", " ", "%20").Replace(s)
}
