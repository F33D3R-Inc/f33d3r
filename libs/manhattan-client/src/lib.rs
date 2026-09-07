//! F33D3R Manhattan client — the naming plane, reached by name.
//!
//! One crate, depended on by every F33D3R Rust brain as
//! `manhattan-client = { path = "../libs/manhattan-client" }`. The brain name
//! is a runtime parameter to `Manhattan::from_env()`, not a const. There is no
//! per-brain copy of this file: the copies drifted into three variants, and a
//! wire-contract change that reaches some brains and misses others is exactly
//! the failure a single crate makes impossible.
//!
//! # The rule this enforces
//!
//! No brain may reference another brain's rows. It may only reference a NAME,
//! and it resolves that name through Manhattan.
//!
//! That is why this file holds no `sqlx` import and no pool. The moment a
//! resolver can reach the tables it resolves, "just join, it's right there"
//! wins and the naming plane becomes decoration. Resolution crosses a process
//! boundary because the boundary is the feature.
//!
//! # Names
//!
//! A name is always `<namespace>:<value>`, so two namespaces can never collide
//! on the same literal string:
//!
//!   pial:<uuid>          an identity — the root that never moves
//!   handle:<handle>      a pointer to an identity; transferable, revocable
//!   uuid:<uuid>          a work, media item, or other entity by id
//!   cid:sha256:<hex>     the same work by content address
//!   addr:XXXX-XXXX       a public, revocable contact address
//!
//! A node answers to all of its names. That is what makes copying a handle into
//! every row that renders unnecessary: `resolve_batch` answers a whole page of
//! names in one round trip, which is the cheap lookup whose absence caused the
//! copying in the first place.
//!
//! # Ownership and authentication
//!
//! Every call carries two headers. `X-Internal-Key` is the estate-wide
//! service-to-service secret (`INTERNAL_API_KEY`), and Manhattan refuses every
//! request that does not carry it — a brain name alone is a claim anyone on the
//! network could make. `X-F33D3R-Brain` is that name; Manhattan decides a
//! node's owner from its own kind→owner map and refuses writes from any other
//! brain, so ownership is enforced at the wire rather than by convention in
//! twelve codebases.
//!
//! # Usage
//!
//! ```no_run
//! use manhattan_client::{handle_name, pial_name, Manhattan, KIND_IDENTITY};
//!
//! # async fn demo(pial_id: &str) -> manhattan_client::Result<()> {
//! let manhattan = Manhattan::from_env("elohim-veni")?;
//! let node = manhattan.ensure_node(KIND_IDENTITY, &pial_name(pial_id)).await?;
//! let who = manhattan.resolve(&handle_name("tehanibentley")).await?;
//! # let _ = (node, who);
//! # Ok(())
//! # }
//! ```

use std::collections::HashMap;
use std::time::Duration;

use serde::{Deserialize, Serialize};

// ── Node kinds ────────────────────────────────────────────────────────────────
pub const KIND_IDENTITY: &str = "identity";
pub const KIND_WORK: &str = "work";
pub const KIND_MEDIA: &str = "media";
pub const KIND_KEY: &str = "key";
pub const KIND_ADDRESS: &str = "address";
pub const KIND_FACET: &str = "facet";
/// A verified natural person, who may hold several identities. Not an identity:
/// conflating the person with the account defeats separable personas.
pub const KIND_NEXUS: &str = "nexus";
/// A speakable, rotatable contact name resolving to an identity. Carries a
/// contact policy; never a bearer secret.
pub const KIND_NUMBER: &str = "number";

// ── Edge predicates ───────────────────────────────────────────────────────────
pub const PRED_QUOTES: &str = "quotes";
pub const PRED_REPLIES_TO: &str = "replies_to";
pub const PRED_AUTHORED_BY: &str = "authored_by";
pub const PRED_DERIVED_FROM: &str = "derived_from";
pub const PRED_RESOLVES_TO: &str = "resolves_to";
pub const PRED_SIGNS_FOR: &str = "signs_for";
pub const PRED_CONTAINS: &str = "contains";

// ── Associations ──────────────────────────────────────────────────────────────
/// The follow graph, published to Manhattan's association plane by feed-engine
/// and read here. It is deliberately not an edge predicate: an edge carries a
/// materialised root and depth because it belongs to a lineage, and a follow
/// belongs to none.
pub const ASSOC_FOLLOWS: &str = "follows";

// ── Namespaces ────────────────────────────────────────────────────────────────
pub const NS_PIAL: &str = "pial";
pub const NS_HANDLE: &str = "handle";
pub const NS_UUID: &str = "uuid";
pub const NS_CID: &str = "cid";
pub const NS_ADDR: &str = "addr";

/// Manhattan's ceiling on one edge listing, mirrored here so a caller reading a
/// full page knows the answer was truncated rather than complete.
pub const MAX_EDGE_LIST: i64 = 5000;
pub const NS_NUMBER: &str = "number";

/// Build a namespaced name. Always route a name through one of these helpers
/// rather than concatenating, so every brain produces the same string for the
/// same thing.
pub fn name(namespace: &str, value: &str) -> String {
    format!("{namespace}:{value}")
}

/// An identity's canonical name. A person is a PIAL, never a handle.
pub fn pial_name(pial_id: &str) -> String {
    name(NS_PIAL, pial_id)
}

/// A pointer to an identity. Transferable and revocable, which is precisely why
/// it is not the identity.
pub fn handle_name(handle: &str) -> String {
    name(NS_HANDLE, handle)
}

pub fn uuid_name(id: &str) -> String {
    name(NS_UUID, id)
}

pub fn cid_name(cid: &str) -> String {
    name(NS_CID, cid)
}

pub fn addr_name(addr: &str) -> String {
    name(NS_ADDR, addr)
}

pub fn number_name(number: &str) -> String {
    name(NS_NUMBER, number)
}

// ── Errors ────────────────────────────────────────────────────────────────────

/// Manhattan's machine-readable reason for a 409.
///
/// Several different conditions answer `409 Conflict`, and only some of them
/// mean the write this client asked for is already done. The others mean it did
/// not happen — and two of them mean it never will. A caller that cannot tell
/// them apart marks naming-plane writes delivered that never landed, which is
/// why Manhattan sends a stable `code` beside the human message and why this
/// type exists.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum ConflictCode {
    /// `name_exists` — the name is already actively bound. Manhattan sends the
    /// holder's node_id, kind, owner and status with the refusal whenever it is
    /// still resolvable, so a caller can tell "already bound to the node I
    /// meant, carry on" from "this name points at another identity" without a
    /// second round trip. The projection is the one `/v1/resolve/:name` already
    /// serves, so nothing is exposed here that a caller could not read anyway.
    NameExists {
        holder: Option<String>,
        kind: Option<String>,
        owner: Option<String>,
        status: Option<String>,
    },
    /// `edge_exists` — the edge is already present. An at-least-once writer's
    /// replay, and the end state it wanted.
    EdgeExists,
    /// `name_already_revoked` — a revoke of a name already retired. Also the end
    /// state the caller wanted.
    NameAlreadyRevoked,
    /// `name_never_reissued` — the name was revoked and its namespace never
    /// reissues one. The write did not happen and no retry will change that.
    NameNeverReissued,
    /// `name_in_quarantine` — the name was revoked and its namespace holds a
    /// revoked name before reissuing it (a Number sits out twelve months). The
    /// write did not happen. It is not permanent, but the hold outlasts any
    /// retry schedule a queue keeps, so a queue treats it exactly as it treats
    /// `name_never_reissued` and an operator reads a different reason.
    NameInQuarantine,
    /// `too_deep` — the edge would sit past Manhattan's depth ceiling, or would
    /// push a tree already placed beneath its subject past it. The write did
    /// not happen and no retry will change that.
    TooDeep,
    /// `edge_cycle` — the object already hangs from the subject, so the edge
    /// would close the chain into a loop. The write did not happen and no
    /// retry will change that.
    EdgeCycle,
    /// `edge_has_descendants` — a delete refused because edges sit beneath this
    /// one. Removing those first makes the delete succeed, so unlike the two
    /// above this refusal can still clear.
    EdgeHasDescendants,
    /// `identity_not_active` — a mint against a suspended or retired identity.
    IdentityNotActive,
    /// `address_already_revoked` — a revoke of an address already retired.
    AddressAlreadyRevoked,
    /// `number_already_revoked` — a revoke of a Number already retired.
    NumberAlreadyRevoked,
    /// `number_cap_reached` — this identity already holds as many live Numbers
    /// as one identity may. The mint did not happen and will succeed once a
    /// Number is retired, so the person is told which action clears it.
    NumberCapReached,
    /// `number_not_reclaimable` — a request for a specific Number back that this
    /// identity may not have. Deliberately one code for every cause: never held,
    /// held by somebody else, still live, still inside a stranger's quarantine.
    /// Telling them apart would answer "is this Number free?" for any Number.
    NumberNotReclaimable,
    /// A 409 this build does not recognise: a Manhattan that predates the codes
    /// and sends none, or a condition added since. Never success. Treating an
    /// unrecognised refusal as delivered is the exact failure the codes exist to
    /// end, so a caller must retry or escalate on this and never mark it done.
    Unrecognised(String),
}

impl ConflictCode {
    /// Read the code out of a refusal body. An absent or unknown code becomes
    /// `Unrecognised`, which no caller may treat as success — that is what makes
    /// this client correct against a Manhattan deployed before the codes as well
    /// as after one deployed with them.
    pub fn from_body(body: &str) -> Self {
        let parsed: serde_json::Value =
            serde_json::from_str(body).unwrap_or(serde_json::Value::Null);
        match parsed.get("code").and_then(|c| c.as_str()) {
            Some("name_exists") => {
                let field = |k: &str| {
                    parsed
                        .get(k)
                        .and_then(|v| v.as_str())
                        .map(|s| s.to_string())
                };
                ConflictCode::NameExists {
                    holder: field("node_id"),
                    kind: field("kind"),
                    owner: field("owner"),
                    status: field("status"),
                }
            }
            Some("edge_exists") => ConflictCode::EdgeExists,
            Some("name_already_revoked") => ConflictCode::NameAlreadyRevoked,
            Some("name_never_reissued") => ConflictCode::NameNeverReissued,
            Some("name_in_quarantine") => ConflictCode::NameInQuarantine,
            Some("too_deep") => ConflictCode::TooDeep,
            Some("edge_cycle") => ConflictCode::EdgeCycle,
            Some("edge_has_descendants") => ConflictCode::EdgeHasDescendants,
            Some("identity_not_active") => ConflictCode::IdentityNotActive,
            Some("address_already_revoked") => ConflictCode::AddressAlreadyRevoked,
            Some("number_already_revoked") => ConflictCode::NumberAlreadyRevoked,
            Some("number_cap_reached") => ConflictCode::NumberCapReached,
            Some("number_not_reclaimable") => ConflictCode::NumberNotReclaimable,
            _ => {
                let trimmed = body.trim();
                ConflictCode::Unrecognised(if trimmed.is_empty() {
                    "409 with no body".to_string()
                } else {
                    trimmed.chars().take(400).collect()
                })
            }
        }
    }

    /// True when Manhattan has said the write will not be accepted by any
    /// retry a queue could schedule: never (`name_never_reissued`, `too_deep`,
    /// `edge_cycle`), or not for months (`name_in_quarantine`). A queue holding
    /// such a row must quarantine it rather than block behind it, since no
    /// number of retries can change the answer within the queue's horizon.
    pub fn is_permanent(&self) -> bool {
        matches!(
            self,
            ConflictCode::NameNeverReissued
                | ConflictCode::NameInQuarantine
                | ConflictCode::TooDeep
                | ConflictCode::EdgeCycle
        )
    }
}

impl std::fmt::Display for ConflictCode {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            ConflictCode::NameExists {
                holder,
                kind,
                owner,
                status,
            } => {
                write!(f, "name_exists")?;
                if let Some(h) = holder {
                    write!(f, " (held by node {h}")?;
                    if let Some(k) = kind {
                        write!(f, ", kind {k}")?;
                    }
                    if let Some(o) = owner {
                        write!(f, ", owned by {o}")?;
                    }
                    if let Some(s) = status {
                        write!(f, ", status {s}")?;
                    }
                    write!(f, ")")?;
                }
                Ok(())
            }
            ConflictCode::EdgeExists => write!(f, "edge_exists"),
            ConflictCode::NameAlreadyRevoked => write!(f, "name_already_revoked"),
            ConflictCode::NameNeverReissued => write!(f, "name_never_reissued"),
            ConflictCode::NameInQuarantine => write!(f, "name_in_quarantine"),
            ConflictCode::TooDeep => write!(f, "too_deep"),
            ConflictCode::EdgeCycle => write!(f, "edge_cycle"),
            ConflictCode::EdgeHasDescendants => write!(f, "edge_has_descendants"),
            ConflictCode::IdentityNotActive => write!(f, "identity_not_active"),
            ConflictCode::AddressAlreadyRevoked => write!(f, "address_already_revoked"),
            ConflictCode::NumberAlreadyRevoked => write!(f, "number_already_revoked"),
            ConflictCode::NumberCapReached => write!(f, "number_cap_reached"),
            ConflictCode::NumberNotReclaimable => write!(f, "number_not_reclaimable"),
            ConflictCode::Unrecognised(detail) => write!(f, "unrecognised conflict: {detail}"),
        }
    }
}

#[derive(Debug)]
pub enum ManhattanError {
    /// No MANHATTAN_URL configured. Treat this as "the naming plane is not
    /// reachable from this deployment yet" and log it — never as licence to
    /// fall back to a local join, which is the drift this client exists to end.
    NotConfigured,
    /// `INTERNAL_API_KEY` is unset or empty. Unlike `NotConfigured` this is
    /// not a degraded mode: Manhattan refuses every unauthenticated call, so a
    /// brain without the key cannot reach the naming plane at all and must not
    /// start as though it could. `from_env` returns this so `main` fails to
    /// boot with the reason in the log.
    MissingInternalKey,
    /// The name does not resolve, or resolves to something revoked. The two are
    /// deliberately indistinguishable: a rotated contact address must not
    /// confirm that it was ever real.
    NotFound,
    /// Manhattan refused with 409. That status covers several conditions and
    /// they are not interchangeable: some mean the write is already done, two
    /// mean it did not happen and never will. The code says which, so a caller
    /// decides from what Manhattan stated rather than from a status number.
    Conflict(ConflictCode),
    /// This brain is not permitted to write here.
    Forbidden,
    /// Manhattan rejected the internal key this client sent (401). The key in
    /// this brain's environment does not match Manhattan's; no retry will
    /// change that until one of them is rotated.
    Unauthorized,
    Transport(String),
    Status(u16, String),
}

impl std::fmt::Display for ManhattanError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            ManhattanError::NotConfigured => write!(f, "manhattan: MANHATTAN_URL is not set"),
            ManhattanError::MissingInternalKey => write!(
                f,
                "manhattan: INTERNAL_API_KEY must be set — every call to the naming plane \
                 is authenticated with it and Manhattan refuses calls without it"
            ),
            ManhattanError::NotFound => write!(f, "manhattan: name does not resolve"),
            ManhattanError::Conflict(code) => write!(f, "manhattan: conflict: {code}"),
            ManhattanError::Forbidden => write!(f, "manhattan: this brain may not write here"),
            ManhattanError::Unauthorized => write!(
                f,
                "manhattan: internal key rejected — INTERNAL_API_KEY differs from Manhattan's"
            ),
            ManhattanError::Transport(e) => write!(f, "manhattan: transport: {e}"),
            ManhattanError::Status(c, b) => write!(f, "manhattan: http {c}: {b}"),
        }
    }
}

impl ManhattanError {
    /// True when the error says nothing about the write that was attempted:
    /// the plane could not be reached, it answered with a server fault, or it
    /// refused this brain's key. Every one of those is a fact about the
    /// deployment, not about the row, and a queue must not count it toward
    /// giving up on the row — an outage of an hour would otherwise quarantine
    /// whatever write happened to be at the head of the queue when it began,
    /// and an operator would then have to un-quarantine writes that were never
    /// refused. Only an answer FROM Manhattan about THIS write counts.
    pub fn is_outage(&self) -> bool {
        matches!(
            self,
            ManhattanError::Transport(_)
                | ManhattanError::Unauthorized
                | ManhattanError::Status(500..=599, _)
        )
    }
}

impl std::error::Error for ManhattanError {}

pub type Result<T> = std::result::Result<T, ManhattanError>;

// ── Wire types ────────────────────────────────────────────────────────────────

/// A thing with an identity and exactly one owning brain.
#[derive(Debug, Clone, Deserialize, Serialize)]
pub struct Node {
    pub node_id: String,
    pub kind: String,
    pub owner: String,
    pub status: String,
    #[serde(default)]
    pub name: Option<String>,
}

#[derive(Debug, Clone, Deserialize)]
pub struct NameRow {
    pub name: String,
    pub namespace: String,
    pub is_primary: bool,
    pub status: String,
}

#[derive(Debug, Clone, Deserialize)]
pub struct NodeWithNames {
    pub node: Node,
    #[serde(default)]
    pub names: Vec<NameRow>,
}

impl NodeWithNames {
    /// This node's live name in a namespace, or None. A node answers to many
    /// names; this is how a caller asks for the one it can actually use.
    pub fn name_in(&self, namespace: &str) -> Option<String> {
        let prefix = format!("{namespace}:");
        self.names
            .iter()
            .find(|n| n.status == "active" && n.name.starts_with(&prefix))
            .map(|n| n.name[prefix.len()..].to_string())
    }
}

/// A typed relationship carrying the chain it belongs to. `root` and `depth` are
/// assigned by Manhattan, never supplied, which is what makes a bounded thread
/// read a predicate instead of a walk.
#[derive(Debug, Clone, Deserialize, Serialize)]
pub struct Edge {
    pub subject: String,
    pub predicate: String,
    pub object: String,
    pub root: String,
    pub depth: i32,
    /// When the edge was recorded, as Manhattan serialises it. Defaulted so a
    /// Manhattan that predates the field still decodes; it is only needed to
    /// build the cursor for the next page of a listing.
    #[serde(default)]
    pub created_at: Option<String>,
}

impl Edge {
    /// The keyset cursor that continues a listing after this edge —
    /// `depth~created_at~subject~object`, the four columns every edge listing
    /// is ordered by. `None` when the plane that served it sent no timestamp,
    /// in which case the listing cannot be continued and a caller must treat
    /// a full page as an unanswered question.
    pub fn cursor(&self) -> Option<String> {
        self.created_at.as_ref().map(|t| {
            format!("{}~{t}~{}~{}", self.depth, self.subject, self.object)
        })
    }
}

/// A public, cheap, revocable contact credential pointing at an identity.
/// Rotating one never moves the identity behind it and never touches a
/// conversation already open.
#[derive(Debug, Clone, Deserialize, Serialize)]
pub struct Address {
    pub address: String,
    #[serde(default)]
    pub address_node_id: Option<String>,
    #[serde(default)]
    pub identity_node_id: Option<String>,
    pub status: String,
}

/// A F33D3R Number. Rotating one mints a new node and revokes this name; the
/// identity, its keys and its open conversations are never touched.
#[derive(Debug, Clone, Deserialize, Serialize)]
pub struct NumberRecord {
    pub number: String,
    #[serde(default)]
    pub number_node_id: Option<String>,
    #[serde(default)]
    pub identity_node_id: Option<String>,
    pub status: String,
}

/// The uniform answer to a resolve. Live, revoked, unknown and malformed all
/// return the same shape and the same status code; only `found` differs.
#[derive(Debug, Clone, Deserialize)]
pub struct NumberResolution {
    pub found: bool,
    #[serde(default)]
    pub number_node_id: Option<String>,
    #[serde(default)]
    pub identity_node_id: Option<String>,
}

/// Both directions of an association between two named nodes, answered in one
/// round trip.
///
/// `subject_resolved` and `object_resolved` are carried separately from the
/// booleans on purpose. "This identity is not in the plane yet" and "these two
/// are not associated" both refuse, but only the first is a bug — an outbox that
/// has not drained, or an account that never got an identity node — and a
/// security decision that cannot tell them apart cannot report the one that
/// needs fixing.
#[derive(Debug, Clone, Deserialize)]
pub struct AssocBetween {
    #[serde(default)]
    pub subject_resolved: bool,
    #[serde(default)]
    pub object_resolved: bool,
    #[serde(default)]
    pub forward: bool,
    #[serde(default)]
    pub reverse: bool,
    #[serde(default)]
    pub mutual: bool,
}

#[derive(Debug, Clone, Deserialize)]
pub struct NumberListEntry {
    pub number: String,
    pub number_node_id: String,
    pub status: String,
    pub created_at: String,
    #[serde(default)]
    pub revoked_at: Option<String>,
    /// Whether this is one of the Crockford base32 Numbers minted before the
    /// digit format. Owner-facing only, and only ever on the owner's own
    /// listing. Defaulted, so a Manhattan that predates the field is read as
    /// "not legacy" rather than failing to decode.
    #[serde(default)]
    pub legacy: bool,
}

// ── Batch application ─────────────────────────────────────────────────────────

/// One outbox row, forwarded as the trigger wrote it.
#[derive(Debug, Clone, Serialize)]
pub struct ApplyOp {
    pub op: String,
    pub payload: serde_json::Value,
}

/// What became of one op in an `apply`. The six outcomes are the whole of what
/// a drain acts on; see `Outcome`.
#[derive(Debug, Clone, Deserialize)]
pub struct ApplyResult {
    pub outcome: Outcome,
    #[serde(default)]
    pub code: Option<String>,
    #[serde(default)]
    pub error: Option<String>,
    #[serde(default)]
    pub node_id: Option<String>,
}

/// Manhattan's verdict on one op.
///
/// `Applied` and `Already` are delivered. `Refused` is quarantined and the
/// batch carries on past it. `Retry` holds its place and counts the attempt.
/// `Error` holds its place and does not count — the plane, not the row, is at
/// fault. `Skipped` was never attempted because an earlier op held, and is
/// left exactly as it was.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Outcome {
    Applied,
    Already,
    Retry,
    Refused,
    Error,
    Skipped,
}

impl std::fmt::Display for Outcome {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(match self {
            Outcome::Applied => "applied",
            Outcome::Already => "already",
            Outcome::Retry => "retry",
            Outcome::Refused => "refused",
            Outcome::Error => "error",
            Outcome::Skipped => "skipped",
        })
    }
}

/// Manhattan's ceiling on one apply.
pub const MAX_APPLY_OPS: usize = 500;

#[derive(Debug, Deserialize)]
struct ApplyResp {
    results: Vec<ApplyResult>,
}

#[derive(Debug, Deserialize)]
struct BatchResolved {
    // Manhattan also lists the names it could not resolve; resolve_batch is
    // documented to return only those that resolved, so that list is not read.
    resolved: HashMap<String, Node>,
}

#[derive(Debug, Deserialize)]
struct ThreadResp {
    edges: Vec<Edge>,
}

// ── Client ────────────────────────────────────────────────────────────────────

/// The header Manhattan authenticates every call with. Same header and same
/// environment variable as every other guarded brain in the estate.
pub const HEADER_INTERNAL_KEY: &str = "X-Internal-Key";

/// The header naming the calling brain, from which Manhattan decides ownership.
pub const HEADER_BRAIN: &str = "X-F33D3R-Brain";

#[derive(Clone)]
pub struct Manhattan {
    base: String,
    brain: String,
    internal_key: String,
    http: reqwest::Client,
}

impl Manhattan {
    /// Build a client from `MANHATTAN_URL` and `INTERNAL_API_KEY`.
    ///
    /// An unset URL yields a client whose every call returns `NotConfigured`,
    /// so a deployment without the naming plane fails loudly at the call site
    /// instead of silently skipping writes. An unset or empty key is an error
    /// here, at boot: Manhattan refuses unauthenticated calls, so a brain
    /// without the key has no naming plane and must not start as if it did.
    pub fn from_env(brain: &str) -> Result<Self> {
        let key = std::env::var("INTERNAL_API_KEY").unwrap_or_default();
        if key.trim().is_empty() {
            return Err(ManhattanError::MissingInternalKey);
        }
        Ok(Self::new(
            std::env::var("MANHATTAN_URL").unwrap_or_default(),
            brain,
            key,
        ))
    }

    pub fn new(base_url: impl Into<String>, brain: &str, internal_key: impl Into<String>) -> Self {
        let base = base_url.into().trim_end_matches('/').to_string();
        Manhattan {
            base,
            brain: brain.to_string(),
            internal_key: internal_key.into(),
            http: reqwest::Client::builder()
                .timeout(Duration::from_secs(5))
                .build()
                .unwrap_or_default(),
        }
    }

    /// Every request carries the internal key and the brain name; one place
    /// sets both so no call can be sent without them.
    fn authed(&self, req: reqwest::RequestBuilder) -> reqwest::RequestBuilder {
        req.header(HEADER_INTERNAL_KEY, &self.internal_key)
            .header(HEADER_BRAIN, &self.brain)
    }

    pub fn configured(&self) -> bool {
        !self.base.is_empty()
    }

    // ── Resolution ────────────────────────────────────────────────────────────

    /// Resolve one name to the node behind it. The hot path: one indexed lookup.
    pub async fn resolve(&self, name: &str) -> Result<Node> {
        self.get(&format!("/v1/resolve/{}", seg(name))).await
    }

    /// Resolve many names in one round trip, returning only those that
    /// resolved. This is the call that removes the reason handles were copied
    /// into every row that renders.
    pub async fn resolve_batch(&self, names: &[String]) -> Result<HashMap<String, Node>> {
        if names.is_empty() {
            return Ok(HashMap::new());
        }
        let body = serde_json::json!({ "names": names });
        let out: BatchResolved = self.post("/v1/resolve/batch", &body).await?;
        Ok(out.resolved)
    }

    /// Resolve a name to its PIAL, when the caller wants the identity rather
    /// than the node. Convenience over `resolve` + `get_node`.
    pub async fn resolve_pial(&self, name: &str) -> Result<String> {
        let node = self.resolve(name).await?;
        let full = self.get_node(&node.node_id).await?;
        full.name_in(NS_PIAL).ok_or(ManhattanError::NotFound)
    }

    // ── Nodes and names ───────────────────────────────────────────────────────

    /// Register a node under `name`, or return the node that name already
    /// resolves to. Idempotent on the name: two nodes for one thing is the
    /// failure this whole plane exists to prevent, so this client must never be
    /// the thing that creates one.
    ///
    /// One round trip in both the fresh and the replayed case. The create is
    /// attempted directly; a name already bound answers `name_exists` and that
    /// refusal carries the holder — node id, kind, owner, status — so the
    /// replay costs nothing more than the create did. Resolving first, as this
    /// once did, doubled the cost of every registration a drain delivered.
    ///
    /// The node's owner is decided by Manhattan from its kind, not by the
    /// caller — the ownership map lives in one place rather than in twelve.
    pub async fn ensure_node(&self, kind: &str, name: &str) -> Result<Node> {
        let namespace = name.split(':').next().unwrap_or("").to_string();
        let body = serde_json::json!({
            "kind": kind,
            "name": name,
            "namespace": namespace,
        });
        match self.post::<NodeWithNames>("/v1/nodes", &body).await {
            Ok(env) => Ok(env.node),
            // Already bound, and Manhattan said to what.
            Err(ManhattanError::Conflict(ConflictCode::NameExists {
                holder: Some(holder),
                kind: Some(kind),
                owner: Some(owner),
                status: Some(status),
            })) => Ok(Node {
                node_id: holder,
                kind,
                owner,
                status,
                name: Some(name.to_string()),
            }),
            // A permanent refusal is not a lost race and must not be reported as
            // one. Manhattan refuses reissue on node creation too, so a name its
            // namespace never reissues answers `name_never_reissued` here — and
            // resolving it would then return NotFound, turning a plain statement
            // that the write will never happen into a puzzle for whoever reads
            // the queue.
            Err(ManhattanError::Conflict(code)) if code.is_permanent() => {
                Err(ManhattanError::Conflict(code))
            }
            // Bound, but by a Manhattan too old to say to what: resolve it.
            Err(ManhattanError::Conflict(_)) => self.resolve(name).await,
            Err(e) => Err(e),
        }
    }

    /// Attach an additional name to a node — a work's CID alongside its UUID, a
    /// handle alongside an identity.
    pub async fn bind_name(&self, node_id: &str, name: &str) -> Result<()> {
        let namespace = name.split(':').next().unwrap_or("").to_string();
        let body = serde_json::json!({
            "node_id": node_id,
            "name": name,
            "namespace": namespace,
        });
        self.post_unit("/v1/names", &body).await
    }

    /// Retire a name. The row survives revocation so the name can never be
    /// re-minted to someone else — which is what stops a released handle from
    /// carrying its previous owner's identity to whoever takes it next.
    pub async fn revoke_name(&self, name: &str) -> Result<()> {
        self.post_unit(
            &format!("/v1/names/{}/revoke", seg(name)),
            &serde_json::json!({}),
        )
        .await
    }

    pub async fn get_node(&self, node_id: &str) -> Result<NodeWithNames> {
        self.get(&format!("/v1/nodes/{}", seg(node_id))).await
    }

    // ── Edges ─────────────────────────────────────────────────────────────────

    /// Record a typed relationship. Manhattan assigns root and depth and refuses
    /// an edge that would close a cycle.
    pub async fn add_edge(&self, subject: &str, predicate: &str, object: &str) -> Result<Edge> {
        let body = serde_json::json!({
            "subject": subject,
            "predicate": predicate,
            "object": object,
        });
        self.post("/v1/edges", &body).await
    }

    /// Remove an edge. Manhattan refuses to delete a mid-chain edge while
    /// descendants exist, because the children's materialized root and depth
    /// would silently become wrong — and a bounded thread read that returns
    /// stale depths is worse than one that fails. Delete leaves first.
    pub async fn delete_edge(&self, subject: &str, predicate: &str, object: &str) -> Result<()> {
        if !self.configured() {
            return Err(ManhattanError::NotConfigured);
        }
        let resp = self
            .authed(self.http.delete(format!("{}/v1/edges", self.base)))
            .json(&serde_json::json!({
                "subject": subject,
                "predicate": predicate,
                "object": object,
            }))
            .send()
            .await
            .map_err(|e| ManhattanError::Transport(e.to_string()))?;
        let status = resp.status().as_u16();
        match status {
            200..=299 => Ok(()),
            404 => Err(ManhattanError::NotFound),
            401 => Err(ManhattanError::Unauthorized),
            403 => Err(ManhattanError::Forbidden),
            // A conflict's body is read where a 404's is not: 409 covers
            // several different conditions and the code naming which one is
            // in there. Discarding it is how a refusal became a success.
            409 => Err(ManhattanError::Conflict(ConflictCode::from_body(
                &resp.text().await.unwrap_or_default(),
            ))),
            _ => {
                let body = resp.text().await.unwrap_or_default();
                Err(ManhattanError::Status(status, body))
            }
        }
    }

    /// One edge by its key, or `NotFound`. The way to learn whether an edge is
    /// present: a primary-key probe on Manhattan's side, so the answer is the
    /// same on a subject with five edges and one with five million. A listing
    /// can be truncated; this cannot.
    pub async fn edge(&self, subject: &str, predicate: &str, object: &str) -> Result<Edge> {
        self.get(&format!(
            "/v1/edges/one/{}/{}/{}",
            seg(subject),
            seg(predicate),
            seg(object)
        ))
        .await
    }

    /// Every edge under a root, bounded by depth. The bound is the query, not a
    /// counter in the caller.
    pub async fn thread(&self, root: &str, max_depth: u32) -> Result<Vec<Edge>> {
        let out: ThreadResp = self
            .get(&format!(
                "/v1/edges/thread/{}?max_depth={max_depth}",
                seg(root)
            ))
            .await?;
        Ok(out.edges)
    }

    /// One page of the edges out of a subject, shallowest and oldest first,
    /// optionally filtered by predicate.
    ///
    /// `/v1/edges/out` and `/v1/edges/in` answer with a bare JSON array, not the
    /// `{"edges": [...]}` envelope `/v1/edges/thread` uses — decoding them as an
    /// envelope failed every call. A page holds at most `MAX_EDGE_LIST` edges;
    /// a full page is continued by passing the last edge's `cursor()` as
    /// `after`. To ask whether ONE edge is present, use `edge` instead — it
    /// cannot be truncated.
    pub async fn edges_out(
        &self,
        subject: &str,
        predicate: Option<&str>,
        after: Option<&str>,
    ) -> Result<Vec<Edge>> {
        self.get(&format!(
            "/v1/edges/out/{}{}",
            seg(subject),
            edge_list_query(predicate, after)
        ))
        .await
    }

    pub async fn edges_in(
        &self,
        object: &str,
        predicate: Option<&str>,
        after: Option<&str>,
    ) -> Result<Vec<Edge>> {
        self.get(&format!(
            "/v1/edges/in/{}{}",
            seg(object),
            edge_list_query(predicate, after)
        ))
        .await
    }

    // ── Addresses ─────────────────────────────────────────────────────────────

    pub async fn mint_address(&self, identity_node_id: &str) -> Result<Address> {
        let body = serde_json::json!({ "identity_node_id": identity_node_id });
        self.post("/v1/addresses", &body).await
    }

    pub async fn revoke_address(&self, addr: &str) -> Result<()> {
        self.post_unit(
            &format!("/v1/addresses/{}/revoke", seg(addr)),
            &serde_json::json!({}),
        )
        .await
    }

    pub async fn resolve_address(&self, addr: &str) -> Result<Address> {
        self.get(&format!("/v1/addresses/{}", seg(addr))).await
    }

    pub async fn list_addresses(&self, identity_node_id: &str) -> Result<Vec<Address>> {
        self.get(&format!(
            "/v1/identities/{}/addresses",
            seg(identity_node_id)
        ))
        .await
    }

    // ── Numbers ───────────────────────────────────────────────────────────────

    /// Allocate a Number for an identity.
    ///
    /// `reclaim` asks for a SPECIFIC Number back. Manhattan honours it only for a
    /// Number this identity really held, and refuses everything else with one
    /// indistinguishable code — so the call cannot be used to ask whether
    /// somebody else's Number is free. A reclaim is a mint in every other
    /// respect: it counts against the concurrent cap, it counts against the
    /// minting rate, and the Number comes back on a new node carrying none of
    /// its previous policy, label, lease or admissions.
    pub async fn mint_number(
        &self,
        identity_node_id: &str,
        reclaim: Option<&str>,
    ) -> Result<NumberRecord> {
        let mut body = serde_json::json!({ "identity_node_id": identity_node_id });
        if let Some(value) = reclaim {
            body["reclaim"] = serde_json::json!(value);
        }
        self.post("/v1/numbers", &body).await
    }

    /// Resolve a Number to the identity behind it. Never returns NotFound: an
    /// unknown Number is a `found: false` answer, so the call cannot be used to
    /// tell an unallocated Number from a retired one.
    pub async fn resolve_number(&self, number: &str) -> Result<NumberResolution> {
        self.get(&format!("/v1/numbers/{}", seg(number))).await
    }

    pub async fn revoke_number(&self, number: &str) -> Result<()> {
        self.post_unit(
            &format!("/v1/numbers/{}/revoke", seg(number)),
            &serde_json::json!({}),
        )
        .await
    }

    pub async fn list_numbers(&self, identity_node_id: &str) -> Result<Vec<NumberListEntry>> {
        self.get(&format!("/v1/identities/{}/numbers", seg(identity_node_id)))
            .await
    }

    // ── Associations ──────────────────────────────────────────────────────────

    /// Both directions of an association between two names, in one round trip.
    ///
    /// This is the call that ended a security decision taken on hearsay. The
    /// contact policies `followers` and `mutuals` are decisions about the follow
    /// graph; the follow graph is feed-engine's; and until this existed
    /// feed-engine simply asserted `follows` and `mutual` as booleans on the
    /// evaluate call, which this brain had no way to check and which on the
    /// Number path it never received at all.
    ///
    /// Name-addressed, so this brain never holds feed-engine's row ids and
    /// feed-engine never holds this brain's. Both sides name a PIAL; Manhattan
    /// resolves it. That is the entire rule the naming plane exists to enforce.
    pub async fn assoc_between(
        &self,
        assoc: &str,
        subject_name: &str,
        object_name: &str,
    ) -> Result<AssocBetween> {
        self.get(&format!(
            "/v1/assocs/{}/between/{}/{}",
            seg(assoc),
            seg(subject_name),
            seg(object_name)
        ))
        .await
    }

    // ── Batch application ─────────────────────────────────────────────────────

    /// Applies up to `MAX_APPLY_OPS` outbox rows in order and returns one
    /// verdict per row, in the same order. One request per batch is what
    /// makes an outbox drain fast; a verdict per row decided by the plane is
    /// what makes it right without a copy of the plane's rules in every
    /// brain.
    ///
    /// `NotFound` from this call means the Manhattan reached is older than the
    /// endpoint. Nothing was applied; a caller treats it as it treats an
    /// outage.
    pub async fn apply(&self, ops: &[ApplyOp]) -> Result<Vec<ApplyResult>> {
        if ops.is_empty() {
            return Ok(Vec::new());
        }
        let body = serde_json::json!({ "ops": ops });
        let resp: ApplyResp = self.post("/v1/apply", &body).await?;
        if resp.results.len() != ops.len() {
            return Err(ManhattanError::Transport(format!(
                "apply answered {} verdicts for {} ops",
                resp.results.len(),
                ops.len()
            )));
        }
        Ok(resp.results)
    }

    // ── Transport ─────────────────────────────────────────────────────────────

    async fn get<T: for<'de> Deserialize<'de>>(&self, path: &str) -> Result<T> {
        if !self.configured() {
            return Err(ManhattanError::NotConfigured);
        }
        let resp = self
            .authed(self.http.get(format!("{}{}", self.base, path)))
            .send()
            .await
            .map_err(|e| ManhattanError::Transport(e.to_string()))?;
        Self::decode(resp).await
    }

    async fn post<T: for<'de> Deserialize<'de>>(
        &self,
        path: &str,
        body: &serde_json::Value,
    ) -> Result<T> {
        if !self.configured() {
            return Err(ManhattanError::NotConfigured);
        }
        let resp = self
            .authed(self.http.post(format!("{}{}", self.base, path)))
            .json(body)
            .send()
            .await
            .map_err(|e| ManhattanError::Transport(e.to_string()))?;
        Self::decode(resp).await
    }

    async fn post_unit(&self, path: &str, body: &serde_json::Value) -> Result<()> {
        if !self.configured() {
            return Err(ManhattanError::NotConfigured);
        }
        let resp = self
            .authed(self.http.post(format!("{}{}", self.base, path)))
            .json(body)
            .send()
            .await
            .map_err(|e| ManhattanError::Transport(e.to_string()))?;
        let status = resp.status().as_u16();
        match status {
            200..=299 => Ok(()),
            404 => Err(ManhattanError::NotFound),
            401 => Err(ManhattanError::Unauthorized),
            403 => Err(ManhattanError::Forbidden),
            // A conflict's body is read where a 404's is not: 409 covers
            // several different conditions and the code naming which one is
            // in there. Discarding it is how a refusal became a success.
            409 => Err(ManhattanError::Conflict(ConflictCode::from_body(
                &resp.text().await.unwrap_or_default(),
            ))),
            _ => {
                let body = resp.text().await.unwrap_or_default();
                Err(ManhattanError::Status(status, body))
            }
        }
    }

    async fn decode<T: for<'de> Deserialize<'de>>(resp: reqwest::Response) -> Result<T> {
        let status = resp.status().as_u16();
        match status {
            200..=299 => resp
                .json::<T>()
                .await
                .map_err(|e| ManhattanError::Transport(e.to_string())),
            404 => Err(ManhattanError::NotFound),
            401 => Err(ManhattanError::Unauthorized),
            403 => Err(ManhattanError::Forbidden),
            // A conflict's body is read where a 404's is not: 409 covers
            // several different conditions and the code naming which one is
            // in there. Discarding it is how a refusal became a success.
            409 => Err(ManhattanError::Conflict(ConflictCode::from_body(
                &resp.text().await.unwrap_or_default(),
            ))),
            _ => {
                let body = resp.text().await.unwrap_or_default();
                Err(ManhattanError::Status(status, body))
            }
        }
    }
}

/// Escape a value used as a single path segment. Names carry a colon by
/// construction and addresses carry a dash; neither may change the shape of the
/// path it is placed in.
fn edge_list_query(predicate: Option<&str>, after: Option<&str>) -> String {
    let mut q = format!("?limit={MAX_EDGE_LIST}");
    if let Some(p) = predicate {
        q.push_str("&predicate=");
        q.push_str(&seg(p));
    }
    if let Some(a) = after {
        q.push_str("&after=");
        q.push_str(&seg(a));
    }
    q
}

fn seg(s: &str) -> String {
    s.replace('/', "%2F")
        .replace('?', "%3F")
        .replace('#', "%23")
        .replace(' ', "%20")
}
