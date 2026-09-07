//! Manhattan — the naming and graph plane.
//!
//! Across 88 tables and ~20 services the same thing answers to many names: works.id and
//! works.cid and the legacy posts.id; users.id and pial_id and handle. Because resolving a
//! name was expensive, `handle` got copied into dozens of columns and every brain grew its
//! own join into another brain's tables.
//!
//! Manhattan is the ONE resolver, and the rule it enforces is:
//!
//!     No brain may reference another brain's rows.
//!     It may only reference a NAME, and resolve that name here.
//!
//! Physical separation is the point. Manhattan is its own service with its own database,
//! so "just join, the table is right there" is not a temptation anyone can act on.
//!
//! Ownership is enforced at the wire, not by convention: every write carries the caller's
//! brain in X-F33D3R-Brain, and a write against a node owned by a different brain is 403.
//!
//! The brain header is a claim, and a claim is only worth what backs it. Every route except
//! /health and /metrics therefore also requires X-Internal-Key to equal INTERNAL_API_KEY —
//! the estate-wide service-to-service secret — and Manhattan refuses to boot without one.
//! Without that, any process that could reach this port could bind or retire names as any
//! brain simply by naming it.

mod addr;
mod cache;
mod db;
mod migrate;
mod models;
mod number;
mod observ;
#[cfg(test)]
mod db_tests;
#[cfg(test)]
mod testdb;

use std::collections::{BTreeMap, HashMap, HashSet};
use std::sync::Arc;
use std::time::{Duration, Instant};

use axum::{
    extract::{Path, Query, Request, State},
    http::{HeaderMap, StatusCode},
    middleware::Next,
    response::{IntoResponse, Json, Response},
    routing::{get, post},
    Router,
};
use chrono::{DateTime, SecondsFormat, Utc};
use serde::Deserialize;
use serde_json::{json, Value};
use sqlx::postgres::{PgConnectOptions, PgPoolOptions};
use sqlx::PgPool;
use std::str::FromStr;
use subtle::ConstantTimeEq;
use tracing::info;
use uuid::Uuid;

use models::*;

#[derive(Clone)]
struct AppState {
    pool: PgPool,
    /// The service-to-service secret every call must carry. Held in state so
    /// the guard never reads the environment per request.
    internal_api_key: String,
    /// Resolutions answered from memory, kept honest by Postgres announcing
    /// every change — see `cache.rs`.
    cache: Arc<cache::ResolveCache>,
}

type Res<T> = Result<Json<T>, (StatusCode, Json<Value>)>;

/// Every refusal this plane returns carries a stable machine-readable `code`
/// beside the human `error`. It is a contract, not a convenience: four different
/// conditions answer 409 — a name already bound, an edge already present, a name
/// whose namespace never reissues, and an edge past the depth ceiling — and a
/// caller that cannot tell them apart treats a write that never happened as
/// delivered. Codes are append-only; an existing one never changes meaning.
fn err(status: StatusCode, code: &str, msg: &str) -> (StatusCode, Json<Value>) {
    (status, Json(json!({ "error": msg, "code": code })))
}

fn internal<E: std::fmt::Display>(e: E) -> (StatusCode, Json<Value>) {
    err(
        StatusCode::INTERNAL_SERVER_ERROR,
        "internal",
        &e.to_string(),
    )
}

/// The four conditions that answer 409, each with its own code.
///
/// Every outbox drain in the fleet branches on these, so they are wire contract:
/// append a new one, never repurpose an existing one. They are named here rather
/// than spelled at each call site so a rename cannot reach one site and miss
/// another — the failure that would make a drain discard a real write.
mod conflict {
    /// The name is already actively bound. The body carries the current holder's
    /// node_id, kind, owner and status, so a caller can tell "already registered,
    /// carry on" from "bound to the wrong node".
    pub const NAME_EXISTS: &str = "name_exists";
    /// This exact (subject, predicate, object) triple is already recorded — the
    /// write the caller wanted is already true.
    pub const EDGE_EXISTS: &str = "edge_exists";
    /// The name was revoked and its namespace never reissues. The write did NOT
    /// happen, and retrying will never make it happen.
    pub const NAME_NEVER_REISSUED: &str = "name_never_reissued";
    /// The name was revoked and its namespace holds a revoked name for a while
    /// before it may be bound again — a Number sits out its printed life. The
    /// write did NOT happen. It is not a permanent refusal, but the hold is
    /// measured in months, so no retry schedule a queue keeps will outlast it;
    /// a queue treats it as it treats `name_never_reissued`, and an operator
    /// reads a different reason.
    pub const NAME_IN_QUARANTINE: &str = "name_in_quarantine";
    /// The edge would sit past the depth ceiling, or would push a tree already
    /// placed beneath its subject past it. The write did NOT happen.
    pub const TOO_DEEP: &str = "too_deep";
    /// The object already hangs from the subject, so the edge would close the
    /// chain into a loop and root and depth would mean nothing. The write did
    /// NOT happen and never will.
    pub const EDGE_CYCLE: &str = "edge_cycle";
    /// This identity already holds as many live Numbers as one identity may. The
    /// write did NOT happen, and it will succeed once a Number is retired — so
    /// unlike the two above, this refusal clears by an action the caller can
    /// take, and the message says which action.
    pub const NUMBER_CAP_REACHED: &str = "number_cap_reached";
}

// ── Wire-level authentication and ownership ───────────────────────────────────

/// The header every call must carry: the shared secret that makes the brain
/// header below a fact rather than a claim. Same header and variable as every
/// other guarded brain in the estate.
const INTERNAL_KEY_HEADER: &str = "x-internal-key";

/// The header every write must carry: the brain making the call.
const BRAIN_HEADER: &str = "x-f33d3r-brain";

/// Refuse any request that does not carry the configured internal key.
///
/// Applied to every route but /health and /metrics. An empty configured key
/// can never match — `main` refuses to start on one — so this guard has no
/// "unset means open" mode. Resolution is not exempted even though a name is
/// a public fact: address and Number resolution are enumeration primitives,
/// and the only clients of this plane are brains that hold the key.
async fn require_internal_key(
    State(state): State<Arc<AppState>>,
    req: Request,
    next: Next,
) -> Response {
    let presented = req
        .headers()
        .get(INTERNAL_KEY_HEADER)
        .and_then(|v| v.to_str().ok())
        .unwrap_or("");
    // Compared in constant time. A byte-by-byte `!=` returns at the first
    // mismatch, and how long it takes to refuse is then a function of how much
    // of the secret the caller has right — a shared secret must not grade its
    // own guesses. Length is not hidden; the length of a key is not the key.
    let matches: bool = presented
        .as_bytes()
        .ct_eq(state.internal_api_key.as_bytes())
        .into();
    if presented.is_empty() || !matches {
        return err(
            StatusCode::UNAUTHORIZED,
            "internal_key_required",
            "X-Internal-Key must equal this deployment's INTERNAL_API_KEY",
        )
        .into_response();
    }
    next.run(req).await
}

/// Largest batch a single resolve call will accept. A page's worth of names, not a crawl.
const MAX_BATCH: usize = 512;

/// Default and ceiling for edge listings, so no read is unbounded.
const DEFAULT_EDGE_LIMIT: i64 = 500;
const MAX_EDGE_LIMIT: i64 = 5000;

/// The calling brain, normalised. Absent or malformed is a malformed request (400);
/// present but not the owner is a refusal (403), decided per endpoint below.
fn caller_brain(headers: &HeaderMap) -> Result<String, (StatusCode, Json<Value>)> {
    let raw = headers
        .get(BRAIN_HEADER)
        .and_then(|v| v.to_str().ok())
        .unwrap_or("")
        .trim()
        .to_string();
    if raw.is_empty() {
        return Err(err(
            StatusCode::BAD_REQUEST,
            "brain_header_required",
            "X-F33D3R-Brain header is required on every write",
        ));
    }
    validate_owner(&raw)
}

fn forbid_namespace(namespace: &str, authority: &str) -> (StatusCode, Json<Value>) {
    err(
        StatusCode::FORBIDDEN,
        "namespace_forbidden",
        &format!(
            "the '{namespace}' namespace is governed by '{authority}'; only that brain may bind or retire names in it"
        ),
    )
}

fn forbid(owner: &str) -> (StatusCode, Json<Value>) {
    err(
        StatusCode::FORBIDDEN,
        "node_forbidden",
        &format!("node is owned by '{owner}'; only that brain may write facts about it"),
    )
}

// ── Validation ────────────────────────────────────────────────────────────────

fn validate_owner(owner: &str) -> Result<String, (StatusCode, Json<Value>)> {
    let o = owner.trim().to_lowercase();
    if o.is_empty() || o.len() > 64 {
        return Err(err(
            StatusCode::BAD_REQUEST,
            "invalid_owner",
            "owner must be 1–64 characters",
        ));
    }
    if !o
        .chars()
        .all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || c == '-' || c == '_')
    {
        return Err(err(
            StatusCode::BAD_REQUEST,
            "invalid_owner",
            "owner may only contain a-z 0-9 - _",
        ));
    }
    Ok(o)
}

fn validate_kind(kind: &str) -> Result<String, (StatusCode, Json<Value>)> {
    let k = kind.trim().to_lowercase();
    if KINDS.contains(&k.as_str()) {
        Ok(k)
    } else {
        Err(err(
            StatusCode::BAD_REQUEST,
            "invalid_kind",
            &format!("kind must be one of: {}", KINDS.join(", ")),
        ))
    }
}

fn validate_predicate(predicate: &str) -> Result<String, (StatusCode, Json<Value>)> {
    let p = predicate.trim().to_lowercase();
    if PREDICATES.contains(&p.as_str()) {
        Ok(p)
    } else {
        Err(err(
            StatusCode::BAD_REQUEST,
            "invalid_predicate",
            &format!("predicate must be one of: {}", PREDICATES.join(", ")),
        ))
    }
}

/// Shape only. Whether the association type EXISTS, and who may assert it, is a
/// lookup in `assoc_authorities` — there is deliberately no hardcoded list here.
/// A predicate is a constant of the lineage model; an association type is a
/// grant of authority to a brain, and a grant belongs in a migration where its
/// rationale is written down beside it.
fn validate_assoc_shape(assoc: &str) -> Result<String, (StatusCode, Json<Value>)> {
    let a = assoc.trim().to_lowercase();
    if a.is_empty() || a.len() > 64 {
        return Err(err(
            StatusCode::BAD_REQUEST,
            "invalid_assoc",
            "assoc must be 1\u{2013}64 characters",
        ));
    }
    if !a
        .chars()
        .all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || c == '_')
    {
        return Err(err(
            StatusCode::BAD_REQUEST,
            "invalid_assoc",
            "assoc may only contain a-z 0-9 _",
        ));
    }
    Ok(a)
}

fn validate_namespace(namespace: &str) -> Result<String, (StatusCode, Json<Value>)> {
    let ns = namespace.trim().to_lowercase();
    if ns.is_empty() || ns.len() > 64 {
        return Err(err(
            StatusCode::BAD_REQUEST,
            "invalid_namespace",
            "namespace must be 1–64 characters",
        ));
    }
    if !ns
        .chars()
        .all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || c == '-' || c == '_')
    {
        return Err(err(
            StatusCode::BAD_REQUEST,
            "invalid_namespace",
            "namespace may only contain a-z 0-9 - _",
        ));
    }
    Ok(ns)
}

/// A name is stored in its fully namespaced form, `<namespace>:<value>`. Composing it
/// here rather than accepting a bare value keeps one name one string everywhere: the
/// thing a caller writes down is the thing the primary key holds.
fn validate_name(name: &str, namespace: &str) -> Result<String, (StatusCode, Json<Value>)> {
    let n = name.trim();
    if n.is_empty() || n.len() > 512 {
        return Err(err(
            StatusCode::BAD_REQUEST,
            "invalid_name",
            "name must be 1–512 characters",
        ));
    }
    if n.chars().any(|c| c.is_whitespace() || c.is_control()) {
        return Err(err(
            StatusCode::BAD_REQUEST,
            "invalid_name",
            "name may not contain whitespace or control characters",
        ));
    }
    let prefix = format!("{namespace}:");
    let raw = n.as_bytes();
    let prefixed =
        raw.len() > prefix.len() && raw[..prefix.len()].eq_ignore_ascii_case(prefix.as_bytes());
    if !prefixed {
        return Err(err(
            StatusCode::BAD_REQUEST,
            "invalid_name",
            &format!("name must be stored namespaced as '{prefix}<value>'"),
        ));
    }
    Ok(n.to_string())
}

fn clamp_limit(limit: Option<i64>) -> i64 {
    limit.unwrap_or(DEFAULT_EDGE_LIMIT).clamp(1, MAX_EDGE_LIMIT)
}

/// The refusal for a name that may not be bound now. One mapping for every
/// path that binds a name, so a quarantined Number and a retired PIAL can
/// never be described with each other's code.
fn bind_refusal(verdict: db::BindVerdict) -> (StatusCode, Json<Value>) {
    match verdict {
        db::BindVerdict::Bindable => unreachable!("a bindable name is not refused"),
        db::BindVerdict::NeverReissued => err(
            StatusCode::CONFLICT,
            conflict::NAME_NEVER_REISSUED,
            "this name was revoked and names in this namespace are never reissued",
        ),
        db::BindVerdict::InQuarantine => err(
            StatusCode::CONFLICT,
            conflict::NAME_IN_QUARANTINE,
            "this name was revoked and is held in quarantine; it may be bound again once its namespace's hold has run out",
        ),
    }
}

/// Two placements waited on each other's chain, or a chain was re-rooted under
/// a write faster than it could be locked. Nothing was written. This is the
/// one refusal here that is neither a client error nor a fact about the graph,
/// and it answers 503 so that a queue retries it without counting it.
fn placement_contended() -> (StatusCode, Json<Value>) {
    err(
        StatusCode::SERVICE_UNAVAILABLE,
        "placement_contended",
        "the chain this edge joins was being re-rooted by another write; nothing was written, retry",
    )
}

/// How a refused edge insert is reported. One mapping for every edge this
/// plane writes — a relationship, an address's link, a Number's link — so no
/// two paths can describe the same refusal differently. `what` names the edge
/// in the caller's vocabulary.
fn edge_insert_refusal(what: &str, e: db::EdgeInsertError) -> (StatusCode, Json<Value>) {
    match e {
        db::EdgeInsertError::Duplicate => err(
            StatusCode::CONFLICT,
            conflict::EDGE_EXISTS,
            &format!("{what} already exists"),
        ),
        db::EdgeInsertError::TooDeep(depth) => err(
            StatusCode::CONFLICT,
            conflict::TOO_DEEP,
            &format!(
                "{what} would sit at depth {depth}, beyond the maximum of {}",
                db::MAX_DEPTH
            ),
        ),
        db::EdgeInsertError::Cycle => err(
            StatusCode::CONFLICT,
            conflict::EDGE_CYCLE,
            &format!(
                "{what} would close the chain into a loop: the object already hangs from the subject"
            ),
        ),
        db::EdgeInsertError::Contended => placement_contended(),
        db::EdgeInsertError::Db(e) if db::is_deadlock(&e) => placement_contended(),
        db::EdgeInsertError::Db(e) => internal(e),
    }
}

/// A keyset cursor over an edge listing: the last row of the previous page,
/// spelled as `depth~created_at~subject~object`. Every list read here orders
/// by exactly those four columns, so "everything after this row" is one row
/// comparison and page ten costs what page one costs. A caller builds the next
/// cursor from the last row it received; the thread read also hands it back.
#[derive(Debug, Clone, Copy)]
struct EdgeCursor {
    depth: i32,
    created_at: DateTime<Utc>,
    subject: Uuid,
    object: Uuid,
}

impl EdgeCursor {
    fn of(row: &EdgeRow) -> Self {
        EdgeCursor {
            depth: row.depth,
            created_at: row.created_at,
            subject: row.subject,
            object: row.object,
        }
    }

    fn encode(&self) -> String {
        format!(
            "{}~{}~{}~{}",
            self.depth,
            self.created_at.to_rfc3339_opts(SecondsFormat::Micros, true),
            self.subject,
            self.object
        )
    }

    fn parse(raw: Option<&str>) -> Result<Option<Self>, (StatusCode, Json<Value>)> {
        let Some(raw) = raw.map(str::trim).filter(|r| !r.is_empty()) else {
            return Ok(None);
        };
        let malformed = || {
            err(
                StatusCode::BAD_REQUEST,
                "malformed_cursor",
                "after must be a cursor of the form depth~created_at~subject~object, taken from a row of a previous page",
            )
        };
        let mut parts = raw.split('~');
        let depth = parts.next().and_then(|d| d.parse::<i32>().ok()).ok_or_else(malformed)?;
        let created_at = parts
            .next()
            .and_then(|t| DateTime::parse_from_rfc3339(t).ok())
            .map(|t| t.with_timezone(&Utc))
            .ok_or_else(malformed)?;
        let subject = parts.next().and_then(|u| Uuid::parse_str(u).ok()).ok_or_else(malformed)?;
        let object = parts.next().and_then(|u| Uuid::parse_str(u).ok()).ok_or_else(malformed)?;
        if parts.next().is_some() {
            return Err(malformed());
        }
        Ok(Some(EdgeCursor {
            depth,
            created_at,
            subject,
            object,
        }))
    }
}

/// A keyset cursor over an association listing: `created_at~node_id` of the
/// last row of the previous page.
fn parse_assoc_cursor(
    raw: Option<&str>,
) -> Result<Option<(DateTime<Utc>, Uuid)>, (StatusCode, Json<Value>)> {
    let Some(raw) = raw.map(str::trim).filter(|r| !r.is_empty()) else {
        return Ok(None);
    };
    let malformed = || {
        err(
            StatusCode::BAD_REQUEST,
            "malformed_cursor",
            "after must be a cursor of the form created_at~node_id, taken from a row of a previous page",
        )
    };
    let (t, id) = raw.split_once('~').ok_or_else(malformed)?;
    let created_at = DateTime::parse_from_rfc3339(t)
        .map(|t| t.with_timezone(&Utc))
        .map_err(|_| malformed())?;
    let node_id = Uuid::parse_str(id).map_err(|_| malformed())?;
    Ok(Some((created_at, node_id)))
}

fn assoc_cursor(row: &db::AssocListRow) -> String {
    format!(
        "{}~{}",
        row.created_at.to_rfc3339_opts(SecondsFormat::Micros, true),
        row.node_id
    )
}

/// A positive integer from the environment, or its default. A value that is
/// set but unparseable is a misconfiguration and refuses boot: silently
/// falling back would run a differently sized plane than the one deployed.
fn env_u64(name: &str, default: u64) -> anyhow::Result<u64> {
    let v = env_count(name, default)?;
    if v == 0 {
        anyhow::bail!("{name} must be a positive integer, got 0");
    }
    Ok(v)
}

/// A non-negative integer from the environment, or its default. Zero is a
/// meaningful value for a count — "none of these".
fn env_count(name: &str, default: u64) -> anyhow::Result<u64> {
    match std::env::var(name) {
        Ok(raw) if !raw.trim().is_empty() => raw
            .trim()
            .parse::<u64>()
            .map_err(|_| anyhow::anyhow!("{name} must be a non-negative integer, got {raw:?}")),
        _ => Ok(default),
    }
}

// ── Boot ──────────────────────────────────────────────────────────────────────

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    observ::init("manhattan")?;

    let database_url = std::env::var("DATABASE_URL").expect("DATABASE_URL must be set");
    let port = std::env::var("PORT").unwrap_or_else(|_| "8107".into());

    // The naming plane decides who owns every name on the platform. A plane
    // that cannot tell a brain from a stranger must not start and then accept
    // both — an empty key would let any process on the network bind a handle
    // to an identity it does not hold. Same posture Verity takes.
    let internal_api_key = std::env::var("INTERNAL_API_KEY").unwrap_or_default();
    if internal_api_key.trim().is_empty() {
        anyhow::bail!(
            "INTERNAL_API_KEY must be set for Manhattan: it authenticates every \
             call into the naming plane, and without it any caller could bind or \
             retire names as any brain"
        );
    }

    // Fourteen brains fan into this one resolver, so its pool is not left to
    // defaults. Size and acquire timeout bound how long a caller waits for a
    // connection; the statement timeout, applied at session start on every
    // connection, bounds how long one statement can hold one. A placement that
    // waits on a lock past it fails and is retried by the queue that sent it —
    // never allowed to sit on a connection the other thirteen brains need.
    let pool_max = env_u64("DB_POOL_MAX", 20)? as u32;
    let acquire_timeout = env_u64("DB_ACQUIRE_TIMEOUT_SECS", 5)?;
    let statement_timeout_ms = env_u64("DB_STATEMENT_TIMEOUT_MS", 5_000)?;
    let connect = PgConnectOptions::from_str(&database_url)?
        .options([("statement_timeout", statement_timeout_ms.to_string())]);
    let pool = PgPoolOptions::new()
        .max_connections(pool_max)
        .acquire_timeout(Duration::from_secs(acquire_timeout))
        .connect_with(connect)
        .await?;
    info!(
        pool_max,
        acquire_timeout_secs = acquire_timeout,
        statement_timeout_ms,
        "Manhattan database pool configured"
    );

    // Forward-only, checksum-frozen, one migration per transaction. A failure here is a
    // failure to boot: a half-migrated database is not one this process may serve from.
    migrate::run(&pool).await?;
    let version = migrate::schema_version(&pool).await?;
    info!(schema_version = version, "Manhattan schema up to date");

    // How many resolutions this replica may hold in memory. Zero disables the
    // cache; every resolve then reads the database, which is correct and slow.
    let cache_entries = env_count("RESOLVE_CACHE_ENTRIES", 500_000)? as usize;
    let cache = Arc::new(cache::ResolveCache::new(cache_entries));
    tokio::spawn(cache::listen(pool.clone(), cache.clone()));
    info!(cache_entries, "resolution cache configured");

    let state = Arc::new(AppState {
        pool,
        internal_api_key,
        cache,
    });

    let app = routes(state.clone())
        .with_state(state)
        .layer(axum::middleware::from_fn(observ::http_middleware));

    let addr = format!("0.0.0.0:{port}");
    let listener = tokio::net::TcpListener::bind(&addr).await?;
    info!("Manhattan listening on {addr}");
    axum::serve(listener, app).await?;
    Ok(())
}

/// The whole surface of the naming and graph plane, in one place.
///
/// Two routes are open — the container healthcheck and the Prometheus scrape,
/// both of which read nothing a name resolves to. Everything under /v1 sits
/// behind `require_internal_key`; a route added there is guarded by
/// construction rather than by remembering to guard it.
fn routes(state: Arc<AppState>) -> Router<Arc<AppState>> {
    Router::new()
        .route("/metrics", get(observ::metrics_handler))
        .route("/health", get(health))
        .merge(
            guarded_routes().route_layer(axum::middleware::from_fn_with_state(state, require_internal_key)),
        )
}

fn guarded_routes() -> Router<Arc<AppState>> {
    Router::new()
        // Nodes — identity and ownership.
        .route("/v1/nodes", post(create_node))
        .route("/v1/nodes/:node_id", get(get_node))
        // Names — everything a node answers to.
        .route("/v1/names", post(create_name))
        .route("/v1/names/:name/revoke", post(revoke_name))
        // Resolution — the hot path.
        .route("/v1/resolve/batch", post(resolve_batch))
        .route("/v1/resolve/:name", get(resolve_name))
        // Edges — every relationship, with root and depth materialised.
        .route("/v1/edges", post(create_edge).delete(delete_edge))
        .route("/v1/edges/one/:subject/:predicate/:object", get(edge_one))
        // Batch application — one request per outbox batch, one verdict per
        // row, idempotence decided here where the truth is.
        .route("/v1/apply", post(apply))
        .route("/v1/edges/thread/:root", get(edges_thread))
        .route("/v1/edges/out/:subject", get(edges_out))
        .route("/v1/edges/in/:object", get(edges_in))
        // Addresses — rotatable public contact credentials.
        .route("/v1/addresses", post(create_address))
        .route("/v1/addresses/:address", get(resolve_address))
        .route("/v1/addresses/:address/revoke", post(revoke_address))
        .route("/v1/identities/:node_id/addresses", get(identity_addresses))
        // Associations — every relationship that has no lineage. Name-addressed
        // end to end, because the brain that asserts one owns neither endpoint.
        .route("/v1/assocs", post(create_assoc).delete(retract_assoc))
        .route(
            "/v1/assocs/:assoc/between/:subject/:object",
            get(assoc_between),
        )
        .route("/v1/assocs/:assoc/out/:name", get(assoc_out))
        .route("/v1/assocs/:assoc/in/:name", get(assoc_in))
        .route("/v1/assocs/:assoc/count/:name", get(assoc_count))
        .route("/v1/numbers", post(create_number))
        .route("/v1/numbers/:number", get(resolve_number))
        .route("/v1/numbers/:number/revoke", post(revoke_number))
        .route("/v1/identities/:node_id/numbers", get(identity_numbers))
}

// ── Health ────────────────────────────────────────────────────────────────────

async fn health(State(state): State<Arc<AppState>>) -> Res<Value> {
    let version = migrate::schema_version(&state.pool).await.map_err(|e| {
        err(
            StatusCode::SERVICE_UNAVAILABLE,
            "schema_unavailable",
            &e.to_string(),
        )
    })?;
    Ok(Json(json!({
        "status":         "ok",
        "service":        "manhattan",
        "schema_version": version,
    })))
}

// ── POST /v1/nodes ────────────────────────────────────────────────────────────

async fn create_node(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Json(req): Json<CreateNodeReq>,
) -> Res<NodeResp> {
    let brain = caller_brain(&headers)?;
    op_create_node(&state, &brain, &req.kind, req.name.as_deref(), req.namespace.as_deref())
        .await
        .map(Json)
}

/// The write behind `POST /v1/nodes` and the `node` op of `/v1/apply`.
async fn op_create_node(
    state: &AppState,
    brain: &str,
    kind: &str,
    name: Option<&str>,
    namespace: Option<&str>,
) -> Result<NodeResp, (StatusCode, Json<Value>)> {
    let kind = validate_kind(kind)?;

    // Ownership is a lookup, not a claim. The caller says what the thing IS; the
    // authority map (migration 0002) already knows who owns that kind. A brain
    // asserting its own ownership is how two stores of one truth get created,
    // each convinced it is authoritative — so no caller is asked.
    //
    // Any brain may CREATE a node: registration has to work whoever encounters
    // the entity first, or a work cannot be registered until the identity brain
    // happens to have seen its author. Writing FACTS about it stays restricted
    // to the owner, which is where the boundary actually matters.
    let owner = db::owner_for_kind(&state.pool, &kind)
        .await
        .map_err(internal)?
        .unwrap_or_else(|| brain.to_string());

    let binding = match (name, namespace) {
        (Some(name), Some(namespace)) => {
            let ns = validate_namespace(namespace)?;
            let n = validate_name(name, &ns)?;
            // A derived name (pial:, uuid:, cid:) may be bound by whichever brain
            // meets the entity first — that is what lets a work be registered
            // before the identity brain has seen its author. An ALLOCATED name
            // (handle:, addr:) is a decision about who gets a scarce string, and
            // only the governing brain gets to make it. Without this, creating a
            // node was a way to mint a handle behind the registrar's back.
            if let Some(authority) = db::allocation_authority(&state.pool, &ns)
                .await
                .map_err(internal)?
            {
                if authority != brain {
                    return Err(forbid_namespace(&ns, &authority));
                }
            }
            Some((n, ns))
        }
        (None, None) => None,
        _ => {
            return Err(err(
                StatusCode::BAD_REQUEST,
                "name_namespace_pair_required",
                "name and namespace must be supplied together",
            ))
        }
    };

    let mut tx = state.pool.begin().await.map_err(internal)?;

    // The never-reissued rule belongs to the plane, not to one endpoint. This path
    // used to skip it entirely, and since 0005 made the uniqueness index partial on
    // active rows the insert would have succeeded — minting a second node under a
    // retired `pial:` name, which is two identities for one person.
    if let Some((name, namespace)) = binding.as_ref() {
        let verdict = db::may_bind_name(&mut tx, name, namespace, None)
            .await
            .map_err(internal)?;
        if !verdict.is_bindable() {
            return Err(bind_refusal(verdict));
        }
    }

    let node: NodeRow = sqlx::query_as(
        "INSERT INTO nodes (kind, owner) VALUES ($1, $2)
         RETURNING node_id, kind, owner, status, created_at, updated_at",
    )
    .bind(&kind)
    .bind(&owner)
    .fetch_one(&mut *tx)
    .await
    .map_err(internal)?;

    let mut names = Vec::new();
    if let Some((name, namespace)) = binding {
        let bound: Result<NameRow, sqlx::Error> = sqlx::query_as(
            "INSERT INTO names (name, node_id, namespace, is_primary)
             VALUES ($1::citext, $2, $3, TRUE)
             RETURNING name::text AS name, node_id, namespace, is_primary, status, created_at, revoked_at",
        )
        .bind(&name)
        .bind(node.node_id)
        .bind(&namespace)
        .fetch_one(&mut *tx)
        .await;

        let row = match bound {
            Ok(row) => row,
            Err(e) if db::is_unique_violation(&e) => {
                // The failed statement leaves the transaction unusable, so the
                // holder is read after the rollback.
                tx.rollback().await.map_err(internal)?;
                return Err(name_exists_conflict(&state.pool, &name).await);
            }
            Err(e) => return Err(internal(e)),
        };
        names.push(row);
    }

    tx.commit().await.map_err(internal)?;
    Ok(NodeResp { node, names })
}

// ── GET /v1/nodes/:node_id ────────────────────────────────────────────────────

async fn get_node(State(state): State<Arc<AppState>>, Path(node_id): Path<Uuid>) -> Res<NodeResp> {
    let node = db::fetch_node(&state.pool, node_id)
        .await
        .map_err(internal)?
        .ok_or_else(|| err(StatusCode::NOT_FOUND, "node_not_found", "node not found"))?;
    let names = db::fetch_active_names(&state.pool, node_id)
        .await
        .map_err(internal)?;
    Ok(Json(NodeResp { node, names }))
}

// ── POST /v1/names ────────────────────────────────────────────────────────────

async fn create_name(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Json(req): Json<CreateNameReq>,
) -> Res<NameRow> {
    let brain = caller_brain(&headers)?;
    op_create_name(&state, &brain, req.node_id, &req.name, &req.namespace, req.is_primary)
        .await
        .map(Json)
}

/// The write behind `POST /v1/names` and the `name` op of `/v1/apply`.
async fn op_create_name(
    state: &AppState,
    brain: &str,
    node_id: Uuid,
    name: &str,
    namespace: &str,
    is_primary: bool,
) -> Result<NameRow, (StatusCode, Json<Value>)> {
    let namespace = validate_namespace(namespace)?;
    let name = validate_name(name, &namespace)?;

    let node = db::fetch_node(&state.pool, node_id)
        .await
        .map_err(internal)?
        .ok_or_else(|| err(StatusCode::NOT_FOUND, "node_not_found", "node not found"))?;

    // Owning a node and being allowed to name it are different rights. The
    // registrar owns handle registration, transfer and reclaim, but a handle
    // points at an identity the registrar does not own — so naming is governed
    // by the namespace, and only falls back to node ownership for namespaces
    // nobody has claimed.
    match db::authority_for_namespace(&state.pool, &namespace)
        .await
        .map_err(internal)?
    {
        Some(authority) if authority != brain => {
            return Err(forbid_namespace(&namespace, &authority))
        }
        Some(_) => {}
        None if node.owner != brain => return Err(forbid(&node.owner)),
        None => {}
    }

    let mut tx = state.pool.begin().await.map_err(internal)?;

    // The shared gate: a revoked name comes back only if its namespace says it may.
    // Inside the transaction and behind the name lock, so the answer is still true
    // when the insert below runs.
    let verdict = db::may_bind_name(&mut tx, &name, &namespace, None)
        .await
        .map_err(internal)?;
    if !verdict.is_bindable() {
        return Err(bind_refusal(verdict));
    }

    // At most one primary name per node is a partial unique index. Demoting the incumbent
    // in the same transaction makes "make this the primary" mean what it says instead of
    // surfacing as an index collision.
    if is_primary {
        sqlx::query("UPDATE names SET is_primary = FALSE WHERE node_id = $1 AND is_primary")
            .bind(node_id)
            .execute(&mut *tx)
            .await
            .map_err(internal)?;
    }

    let bound: Result<NameRow, sqlx::Error> = sqlx::query_as(
        "INSERT INTO names (name, node_id, namespace, is_primary)
         VALUES ($1::citext, $2, $3, $4)
         RETURNING name::text AS name, node_id, namespace, is_primary, status, created_at, revoked_at",
    )
    .bind(&name)
    .bind(node_id)
    .bind(&namespace)
    .bind(is_primary)
    .fetch_one(&mut *tx)
    .await;

    let row = match bound {
        Ok(row) => row,
        Err(e) if db::is_unique_violation(&e) => {
            tx.rollback().await.map_err(internal)?;
            return Err(name_exists_conflict(&state.pool, &name).await);
        }
        Err(e) => return Err(internal(e)),
    };

    tx.commit().await.map_err(internal)?;
    Ok(row)
}

// ── POST /v1/names/:name/revoke ───────────────────────────────────────────────

async fn revoke_name(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Path(name): Path<String>,
) -> Res<NameRow> {
    let brain = caller_brain(&headers)?;
    op_revoke_name(&state, &brain, &name).await.map(Json)
}

/// The write behind `POST /v1/names/:name/revoke` and the `revoke_name` op of
/// `/v1/apply`.
async fn op_revoke_name(
    state: &AppState,
    brain: &str,
    name: &str,
) -> Result<NameRow, (StatusCode, Json<Value>)> {
    let existing = db::fetch_name(&state.pool, name.trim())
        .await
        .map_err(internal)?
        .ok_or_else(|| err(StatusCode::NOT_FOUND, "name_not_found", "name not found"))?;

    let node = db::fetch_node(&state.pool, existing.node_id)
        .await
        .map_err(internal)?
        .ok_or_else(|| err(StatusCode::NOT_FOUND, "node_not_found", "node not found"))?;

    // Retiring a name is the same right as binding one: whoever governs the
    // namespace. A handle is reclaimed by the registrar, not by the identity.
    match db::authority_for_namespace(&state.pool, &existing.namespace)
        .await
        .map_err(internal)?
    {
        Some(authority) if authority != brain => {
            return Err(forbid_namespace(&existing.namespace, &authority))
        }
        Some(_) => {}
        None if node.owner != brain => return Err(forbid(&node.owner)),
        None => {}
    }
    if existing.status == "revoked" {
        return Err(err(
            StatusCode::CONFLICT,
            "name_already_revoked",
            "name is already revoked",
        ));
    }

    let mut tx = state.pool.begin().await.map_err(internal)?;
    // Held for the rest of the transaction so a bind of this name cannot land
    // between the check above and the retirement below.
    db::lock_name(&mut tx, &existing.name)
        .await
        .map_err(internal)?;

    // The row stays forever, and only the LIVE row is retired. Since 0005 a name
    // carries history — many revoked rows and at most one active row — so an
    // unscoped UPDATE re-stamps every past holder's row with today's timestamp and
    // destroys the record of who held the name and when. Handles are the only
    // reusable namespace, so handles are exactly what accumulates that history.
    let row: Option<NameRow> = sqlx::query_as(
        "UPDATE names
            SET status = 'revoked', revoked_at = NOW(), is_primary = FALSE
          WHERE name = $1::citext AND status = 'active'
      RETURNING name::text AS name, node_id, namespace, is_primary, status, created_at, revoked_at",
    )
    .bind(&existing.name)
    .fetch_optional(&mut *tx)
    .await
    .map_err(internal)?;

    // No live row left: a concurrent revoke won the race. That is a refusal with a
    // reason, not a 500 from an empty result set.
    let row = row.ok_or_else(|| {
        err(
            StatusCode::CONFLICT,
            "name_already_revoked",
            "name is already revoked",
        )
    })?;

    tx.commit().await.map_err(internal)?;
    Ok(row)
}

// ── GET /v1/resolve/:name ─────────────────────────────────────────────────────

const RESOLVE_SELECT: &str = "SELECT nm.name::text AS name, n.node_id, n.kind, n.owner, n.status
       FROM names nm
       JOIN nodes n ON n.node_id = nm.node_id
      WHERE nm.status = 'active'";

/// The 409 for a name already bound, carrying the current holder.
///
/// A name already bound is only a problem when it is bound to something other than
/// what the caller meant. A bare conflict cannot tell "already registered, carry on"
/// from "this name points at a node of the wrong kind or the wrong owner, and every
/// edge you write behind it will be refused" — so the refusal carries the holder's
/// node_id, kind, owner and status. Same projection the resolve endpoint serves, so
/// nothing is exposed here that a caller could not already read.
async fn name_exists_conflict(pool: &PgPool, name: &str) -> (StatusCode, Json<Value>) {
    let sql = format!("{RESOLVE_SELECT} AND nm.name = $1::citext");
    let holder: Option<ResolveResp> =
        match sqlx::query_as(&sql).bind(name).fetch_optional(pool).await {
            Ok(h) => h,
            // The lookup that would have explained the conflict failed. Report that
            // failure — a conflict body assembled without it would be a guess.
            Err(e) => return internal(e),
        };

    let mut body = json!({
        "error": "name is already actively bound to a node",
        "code":  conflict::NAME_EXISTS,
    });
    if let Some(h) = holder {
        body["node_id"] = json!(h.node_id);
        body["kind"] = json!(h.kind);
        body["owner"] = json!(h.owner);
        body["status"] = json!(h.status);
    }
    (StatusCode::CONFLICT, Json(body))
}

/// One name, resolved: from the cache when it is hearing and holds the name,
/// from the database otherwise, and the answer — including "nothing" — is
/// then cached against the moment the read began (see `cache.rs` for why the
/// moment matters). This is the read behind `/v1/resolve`, the batch form,
/// and every name-addressed endpoint that resolves before it writes.
async fn resolve_one(
    state: &AppState,
    name: &str,
) -> Result<Option<ResolveResp>, (StatusCode, Json<Value>)> {
    let name = name.trim();
    if let cache::Lookup::Hit(hit) = state.cache.get(name) {
        return Ok(hit);
    }
    let read_started = Instant::now();
    let sql = format!("{RESOLVE_SELECT} AND nm.name = $1::citext");
    let row: Option<ResolveResp> = sqlx::query_as(&sql)
        .bind(name)
        .fetch_optional(&state.pool)
        .await
        .map_err(internal)?;
    state.cache.fill(name, read_started, row.clone());
    Ok(row)
}

async fn resolve_name(
    State(state): State<Arc<AppState>>,
    Path(name): Path<String>,
) -> Res<ResolveResp> {
    let row = resolve_one(&state, &name).await?;

    row.map(Json).ok_or_else(|| {
        err(
            StatusCode::NOT_FOUND,
            "name_unresolved",
            "name does not resolve",
        )
    })
}

// ── POST /v1/resolve/batch ────────────────────────────────────────────────────

async fn resolve_batch(
    State(state): State<Arc<AppState>>,
    Json(req): Json<ResolveBatchReq>,
) -> Res<ResolveBatchResp> {
    if req.names.len() > MAX_BATCH {
        return Err(err(
            StatusCode::BAD_REQUEST,
            "batch_too_large",
            &format!("at most {MAX_BATCH} names per batch"),
        ));
    }

    // Deduplicate case-insensitively: names are CITEXT, so two spellings are one name.
    let mut wanted: Vec<String> = Vec::with_capacity(req.names.len());
    let mut seen: HashSet<String> = HashSet::with_capacity(req.names.len());
    for raw in &req.names {
        let n = raw.trim();
        if n.is_empty() {
            continue;
        }
        if seen.insert(n.to_lowercase()) {
            wanted.push(n.to_string());
        }
    }

    // Whatever the cache holds is answered from it; only the rest goes to the
    // database, in one statement, and every answer it gives — hit or absence —
    // is cached in turn.
    let mut found: HashMap<String, ResolveResp> = HashMap::with_capacity(wanted.len());
    let mut unknown: Vec<String> = Vec::new();
    for name in &wanted {
        match state.cache.get(name) {
            cache::Lookup::Hit(Some(r)) => {
                found.insert(name.to_lowercase(), r);
            }
            cache::Lookup::Hit(None) => {}
            cache::Lookup::Miss => unknown.push(name.clone()),
        }
    }
    if !unknown.is_empty() {
        let read_started = Instant::now();
        let sql = format!("{RESOLVE_SELECT} AND nm.name = ANY($1::citext[])");
        let rows: Vec<ResolveResp> = sqlx::query_as(&sql)
            .bind(&unknown[..])
            .fetch_all(&state.pool)
            .await
            .map_err(internal)?;
        let mut fetched: HashMap<String, ResolveResp> =
            rows.into_iter().map(|r| (r.name.to_lowercase(), r)).collect();
        for name in unknown {
            let row = fetched.remove(&name.to_lowercase());
            state.cache.fill(&name, read_started, row.clone());
            if let Some(r) = row {
                found.insert(name.to_lowercase(), r);
            }
        }
    }

    let mut resolved = BTreeMap::new();
    let mut missing = Vec::new();
    for input in wanted {
        match found.remove(&input.to_lowercase()) {
            Some(r) => {
                resolved.insert(input, r);
            }
            None => missing.push(input),
        }
    }

    Ok(Json(ResolveBatchResp { resolved, missing }))
}

// ── POST /v1/edges ────────────────────────────────────────────────────────────

async fn create_edge(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Json(req): Json<EdgeReq>,
) -> Res<EdgeRow> {
    let brain = caller_brain(&headers)?;
    op_create_edge(&state, &brain, req.subject, &req.predicate, req.object)
        .await
        .map(Json)
}

/// The write behind `POST /v1/edges` and the `edge` op of `/v1/apply`.
async fn op_create_edge(
    state: &AppState,
    brain: &str,
    subject: Uuid,
    predicate: &str,
    object: Uuid,
) -> Result<EdgeRow, (StatusCode, Json<Value>)> {
    let predicate = validate_predicate(predicate)?;
    let req = EdgeReq {
        subject,
        predicate: predicate.clone(),
        object,
    };
    if req.subject == req.object {
        return Err(err(
            StatusCode::BAD_REQUEST,
            "edge_self_reference",
            "subject and object must differ — a node may not point at itself",
        ));
    }

    let subject = db::fetch_node(&state.pool, req.subject)
        .await
        .map_err(internal)?
        .ok_or_else(|| {
            err(
                StatusCode::NOT_FOUND,
                "subject_node_not_found",
                "subject node not found",
            )
        })?;
    if subject.owner != brain {
        return Err(forbid(&subject.owner));
    }
    if db::fetch_node(&state.pool, req.object)
        .await
        .map_err(internal)?
        .is_none()
    {
        return Err(err(
            StatusCode::NOT_FOUND,
            "object_node_not_found",
            "object node not found",
        ));
    }

    let mut tx = state.pool.begin().await.map_err(internal)?;
    let row = db::insert_edge(&mut tx, req.subject, &predicate, req.object)
        .await
        .map_err(|e| edge_insert_refusal("edge", e))?;
    tx.commit().await.map_err(internal)?;

    Ok(row)
}

// ── DELETE /v1/edges ──────────────────────────────────────────────────────────

async fn delete_edge(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Json(req): Json<EdgeReq>,
) -> Res<EdgeRow> {
    let brain = caller_brain(&headers)?;
    op_delete_edge(&state, &brain, req.subject, &req.predicate, req.object)
        .await
        .map(Json)
}

/// The write behind `DELETE /v1/edges` and the `delete_edge` op of `/v1/apply`.
async fn op_delete_edge(
    state: &AppState,
    brain: &str,
    subject: Uuid,
    predicate: &str,
    object: Uuid,
) -> Result<EdgeRow, (StatusCode, Json<Value>)> {
    let predicate = validate_predicate(predicate)?;
    let req = EdgeReq {
        subject,
        predicate: predicate.clone(),
        object,
    };

    let subject = db::fetch_node(&state.pool, req.subject)
        .await
        .map_err(internal)?
        .ok_or_else(|| {
            err(
                StatusCode::NOT_FOUND,
                "subject_node_not_found",
                "subject node not found",
            )
        })?;
    if subject.owner != brain {
        return Err(forbid(&subject.owner));
    }

    let mut tx = state.pool.begin().await.map_err(internal)?;
    let row = db::delete_edge(&mut tx, req.subject, &predicate, req.object)
        .await
        .map_err(|e| match e {
            db::EdgeDeleteError::HasDescendants(n) => err(
                StatusCode::CONFLICT,
                "edge_has_descendants",
                &format!(
                    "{n} edge(s) are placed beneath this one; remove them first — \
                     root and depth are materialised and this delete would leave them stale"
                ),
            ),
            db::EdgeDeleteError::NotFound => {
                err(StatusCode::NOT_FOUND, "edge_not_found", "edge not found")
            }
            db::EdgeDeleteError::Contended => placement_contended(),
            db::EdgeDeleteError::Db(e) if db::is_deadlock(&e) => placement_contended(),
            db::EdgeDeleteError::Db(e) => internal(e),
        })?;
    tx.commit().await.map_err(internal)?;

    Ok(row)
}

// ── GET /v1/edges/one/:subject/:predicate/:object ─────────────────────────────

/// One edge by its key. The read a writer uses to learn whether a refused
/// insert is nonetheless present: a primary-key probe, answered the same on a
/// subject with five edges and one with five million, so it can never be
/// truncated and never has to be paged.
async fn edge_one(
    State(state): State<Arc<AppState>>,
    Path((subject, predicate, object)): Path<(Uuid, String, Uuid)>,
) -> Res<EdgeRow> {
    let predicate = validate_predicate(&predicate)?;
    db::fetch_edge(&state.pool, subject, &predicate, object)
        .await
        .map_err(internal)?
        .map(Json)
        .ok_or_else(|| err(StatusCode::NOT_FOUND, "edge_not_found", "edge not found"))
}

// ── GET /v1/edges/thread/:root ────────────────────────────────────────────────
//
// Every listing below is keyset-paged on (depth, created_at, subject, object)
// — the order it is served in — through the `after` cursor. A page that comes
// back full is continued, never guessed at: a listing that stopped at its
// limit and said nothing was how a reader once mistook "the first 5000" for
// "all of them".

#[derive(Deserialize)]
struct ThreadQuery {
    max_depth: Option<i32>,
    limit: Option<i64>,
    after: Option<String>,
}

async fn edges_thread(
    State(state): State<Arc<AppState>>,
    Path(root): Path<Uuid>,
    Query(q): Query<ThreadQuery>,
) -> Res<ThreadResp> {
    // Bounded by construction: depth is materialised, so this is one indexed range scan.
    let max_depth = q.max_depth.unwrap_or(2).clamp(0, db::MAX_DEPTH);
    let limit = clamp_limit(q.limit);
    let after = EdgeCursor::parse(q.after.as_deref())?;

    let edges: Vec<EdgeRow> = sqlx::query_as(
        "SELECT subject, predicate, object, root, depth, created_at
           FROM edges
          WHERE root = $1 AND depth <= $2
            AND ($4::int IS NULL
                 OR (depth, created_at, subject, object)
                    > ($4::int, $5::timestamptz, $6::uuid, $7::uuid))
          ORDER BY depth ASC, created_at ASC, subject ASC, object ASC
          LIMIT $3",
    )
    .bind(root)
    .bind(max_depth)
    .bind(limit)
    .bind(after.map(|c| c.depth))
    .bind(after.map(|c| c.created_at))
    .bind(after.map(|c| c.subject))
    .bind(after.map(|c| c.object))
    .fetch_all(&state.pool)
    .await
    .map_err(internal)?;

    let next_cursor = if edges.len() as i64 >= limit {
        edges.last().map(|e| EdgeCursor::of(e).encode())
    } else {
        None
    };

    Ok(Json(ThreadResp {
        root,
        max_depth,
        count: edges.len(),
        next_cursor,
        edges,
    }))
}

// ── GET /v1/edges/out/:subject and /v1/edges/in/:object ───────────────────────
//
// Both answer with a bare array, as they always have. The next page's cursor is
// the last row received, spelled `depth~created_at~subject~object`; a caller
// that received a full page continues from it.

#[derive(Deserialize)]
struct EdgeListQuery {
    predicate: Option<String>,
    limit: Option<i64>,
    after: Option<String>,
}

async fn list_edges(
    pool: &PgPool,
    side_column: &'static str,
    node: Uuid,
    q: EdgeListQuery,
) -> Res<Vec<EdgeRow>> {
    let predicate = match q.predicate.as_deref() {
        Some(p) => Some(validate_predicate(p)?),
        None => None,
    };
    let after = EdgeCursor::parse(q.after.as_deref())?;

    // `side_column` is one of two literals chosen by the two handlers below,
    // never caller input.
    let sql = format!(
        "SELECT subject, predicate, object, root, depth, created_at
           FROM edges
          WHERE {side_column} = $1 AND ($2::text IS NULL OR predicate = $2)
            AND ($4::int IS NULL
                 OR (depth, created_at, subject, object)
                    > ($4::int, $5::timestamptz, $6::uuid, $7::uuid))
          ORDER BY depth ASC, created_at ASC, subject ASC, object ASC
          LIMIT $3"
    );
    let rows: Vec<EdgeRow> = sqlx::query_as(&sql)
        .bind(node)
        .bind(predicate)
        .bind(clamp_limit(q.limit))
        .bind(after.map(|c| c.depth))
        .bind(after.map(|c| c.created_at))
        .bind(after.map(|c| c.subject))
        .bind(after.map(|c| c.object))
        .fetch_all(pool)
        .await
        .map_err(internal)?;
    Ok(Json(rows))
}

async fn edges_out(
    State(state): State<Arc<AppState>>,
    Path(subject): Path<Uuid>,
    Query(q): Query<EdgeListQuery>,
) -> Res<Vec<EdgeRow>> {
    list_edges(&state.pool, "subject", subject, q).await
}

async fn edges_in(
    State(state): State<Arc<AppState>>,
    Path(object): Path<Uuid>,
    Query(q): Query<EdgeListQuery>,
) -> Res<Vec<EdgeRow>> {
    list_edges(&state.pool, "object", object, q).await
}

// ── The association plane ─────────────────────────────────────────────────────
//
// `edges` answers "what thread is this in". `assocs` answers "are these two
// related, and since when". The two never share a table because the first has a
// materialised root and depth and the second has neither — see migration 0007
// for why inventing them would be a lie rather than a convenience.
//
// Every entry point here is name-addressed. The brain asserting an association
// owns neither endpoint node (feed-engine says who follows whom; the identity
// nodes are elohim-veni's), so it must never need to hold another brain's node
// ids. It holds names, and Manhattan resolves them. That is the rule this whole
// plane exists to enforce, applied to itself.

/// Resolves an active name to an ACTIVE node, for the association endpoints.
///
/// A revoked name resolves to nothing, exactly as it does everywhere else: an
/// association asserted against a retired name must not silently attach to
/// whatever that name used to mean. And a name whose node has been revoked or
/// tombstoned resolves to nothing HERE, though `/v1/resolve` still reports it
/// with its status: an association is a live claim about a live pair, and a
/// follow of a tombstoned identity is a fact about nobody.
async fn resolve_active(
    state: &AppState,
    name: &str,
) -> Result<Option<ResolveResp>, (StatusCode, Json<Value>)> {
    Ok(resolve_one(state, name)
        .await?
        .filter(|r| r.status == "active"))
}

/// The caller's right to assert this association type, and the type's existence,
/// in one check. An association type with no authority row does not exist.
async fn assoc_authority_or_refuse(
    pool: &PgPool,
    assoc: &str,
    brain: &str,
) -> Result<(), (StatusCode, Json<Value>)> {
    let authority = db::assoc_authority(pool, assoc).await.map_err(internal)?;
    match authority {
        None => Err(err(
            StatusCode::BAD_REQUEST,
            "unknown_assoc",
            &format!(
                "'{assoc}' is not an association type; association types are granted to a brain in a migration, not claimed by a caller"
            ),
        )),
        Some(owner) if owner != brain => Err(err(
            StatusCode::FORBIDDEN,
            "assoc_forbidden",
            &format!("the '{assoc}' association is asserted by '{owner}'; only that brain may write or retract it"),
        )),
        Some(_) => Ok(()),
    }
}

/// Both endpoints of an association, resolved, with the refusal already shaped.
async fn assoc_endpoints(
    state: &AppState,
    req: &AssocReq,
) -> Result<(ResolveResp, ResolveResp), (StatusCode, Json<Value>)> {
    let subject = resolve_active(state, &req.subject_name)
        .await?
        .ok_or_else(|| {
            err(
                StatusCode::NOT_FOUND,
                "subject_name_unresolved",
                &format!("subject name '{}' does not resolve", req.subject_name),
            )
        })?;
    let object = resolve_active(state, &req.object_name)
        .await?
        .ok_or_else(|| {
            err(
                StatusCode::NOT_FOUND,
                "object_name_unresolved",
                &format!("object name '{}' does not resolve", req.object_name),
            )
        })?;
    if subject.node_id == object.node_id {
        return Err(err(
            StatusCode::BAD_REQUEST,
            "assoc_self_reference",
            "subject and object must differ \u{2014} a node may not associate with itself",
        ));
    }
    Ok((subject, object))
}

// ── POST /v1/assocs ───────────────────────────────────────────────────────────

/// Records an association. Idempotent: re-asserting one already recorded answers
/// 200 with `changed: false` rather than 409.
///
/// That is a deliberate difference from `POST /v1/edges`, which answers
/// `edge_exists` and makes every outbox drain in the fleet reason about what a
/// 409 meant. An edge insert has to distinguish them because placing an edge is
/// a decision about root and depth that must not be taken twice. An association
/// has no placement, so "already true" and "now true" are the same end state and
/// the wire says so plainly instead of making the caller confirm it.
async fn create_assoc(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Json(req): Json<AssocReq>,
) -> Res<AssocResp> {
    let brain = caller_brain(&headers)?;
    op_assoc(&state, &brain, &req, true).await.map(Json)
}

/// The write behind both association routes and the `assoc` / `unassoc` ops
/// of `/v1/apply`. `assert` records the association; otherwise it retracts.
async fn op_assoc(
    state: &AppState,
    brain: &str,
    req: &AssocReq,
    assert: bool,
) -> Result<AssocResp, (StatusCode, Json<Value>)> {
    let assoc = validate_assoc_shape(&req.assoc)?;
    assoc_authority_or_refuse(&state.pool, &assoc, brain).await?;
    let (subject, object) = assoc_endpoints(state, req).await?;

    let row = if assert {
        db::insert_assoc(&state.pool, subject.node_id, &assoc, object.node_id)
            .await
            .map_err(internal)?
    } else {
        db::delete_assoc(&state.pool, subject.node_id, &assoc, object.node_id)
            .await
            .map_err(internal)?
    };

    Ok(AssocResp {
        assoc,
        subject_name: subject.name,
        object_name: object.name,
        subject: subject.node_id,
        object: object.node_id,
        created_at: row.as_ref().map(|r| r.created_at),
        changed: row.is_some(),
    })
}

// ── DELETE /v1/assocs ─────────────────────────────────────────────────────────

/// Retracts an association. Idempotent for the same reason: retracting one that
/// is already absent is the end state the caller wanted.
///
/// Nothing is ever placed beneath an association, so unlike `DELETE /v1/edges`
/// this can never refuse. A retraction that can fail is a retraction that leaves
/// stale authorisation standing, and this table backs an authorisation decision.
async fn retract_assoc(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Json(req): Json<AssocReq>,
) -> Res<AssocResp> {
    let brain = caller_brain(&headers)?;
    op_assoc(&state, &brain, &req, false).await.map(Json)
}

// ── GET /v1/assocs/:assoc/between/:subject/:object ────────────────────────────

/// Both directions between two named nodes, in one round trip.
///
/// This is the read the contact decision makes. It is a read, so it carries no
/// brain header and no authority check: an association is a public fact about
/// two public names, exactly as an edge listing already is. What it deliberately
/// does NOT do is disclose anything else about either node — no other names, no
/// kind, no owner. A caller learns whether the pair it already named is related
/// and nothing more.
///
/// A name that does not resolve answers 200 with `subject_resolved: false`
/// rather than 404. The association cannot exist, so `false` is the truthful
/// answer; reporting the resolution separately is what lets the caller log "this
/// identity is not in the plane yet" as the bug it is, instead of recording it
/// as a relationship that does not exist.
async fn assoc_between(
    State(state): State<Arc<AppState>>,
    Path((assoc, subject_name, object_name)): Path<(String, String, String)>,
) -> Res<AssocBetweenResp> {
    let assoc = validate_assoc_shape(&assoc)?;
    if db::assoc_authority(&state.pool, &assoc)
        .await
        .map_err(internal)?
        .is_none()
    {
        return Err(err(
            StatusCode::BAD_REQUEST,
            "unknown_assoc",
            &format!("'{assoc}' is not an association type"),
        ));
    }

    let subject = resolve_active(&state, &subject_name).await?;
    let object = resolve_active(&state, &object_name).await?;

    let (forward_since, reverse_since) = match (&subject, &object) {
        (Some(s), Some(o)) if s.node_id != o.node_id => {
            db::assoc_between(&state.pool, &assoc, s.node_id, o.node_id)
                .await
                .map_err(internal)?
        }
        _ => (None, None),
    };

    Ok(Json(AssocBetweenResp {
        assoc,
        subject_name,
        object_name,
        subject_resolved: subject.is_some(),
        object_resolved: object.is_some(),
        forward: forward_since.is_some(),
        reverse: reverse_since.is_some(),
        mutual: forward_since.is_some() && reverse_since.is_some(),
        forward_since,
        reverse_since,
    }))
}

// ── GET /v1/assocs/:assoc/out/:name, /in/:name, /count/:name ─────────────────
//
// TAO's assoc_range and assoc_count, answered from the two indexes migration
// 0007 built for them. `out` is everything the named node associates with
// (who it follows); `in` is everything that associates with it (its
// followers). Both are newest first and keyset-paged on `created_at~node_id`.
//
// A listing is a harvest primitive in a way `between` is not: `between`
// answers about a pair the caller already named, a listing hands out names
// the caller did not have. So a listing is served only to the brain that
// asserts the association — the graph's own publisher reading its own
// projection back — while a count, which is a public number on every profile,
// is served to any brain holding the key.

#[derive(Deserialize)]
struct AssocListQuery {
    limit: Option<i64>,
    after: Option<String>,
}

async fn assoc_named_node(
    state: &AppState,
    assoc: &str,
    name: &str,
) -> Result<(String, ResolveResp), (StatusCode, Json<Value>)> {
    let assoc = validate_assoc_shape(assoc)?;
    if db::assoc_authority(&state.pool, &assoc)
        .await
        .map_err(internal)?
        .is_none()
    {
        return Err(err(
            StatusCode::BAD_REQUEST,
            "unknown_assoc",
            &format!("'{assoc}' is not an association type"),
        ));
    }
    let node = resolve_active(&state, name).await?.ok_or_else(|| {
        err(
            StatusCode::NOT_FOUND,
            "name_unresolved",
            &format!("name '{name}' does not resolve"),
        )
    })?;
    Ok((assoc, node))
}

async fn assoc_range(
    state: &AppState,
    headers: &HeaderMap,
    assoc: &str,
    name: &str,
    side: db::AssocSide,
    q: AssocListQuery,
) -> Res<AssocRangeResp> {
    let brain = caller_brain(headers)?;
    let (assoc, node) = assoc_named_node(state, assoc, name).await?;
    assoc_authority_or_refuse(&state.pool, &assoc, &brain).await?;
    let after = parse_assoc_cursor(q.after.as_deref())?;
    let limit = clamp_limit(q.limit);

    let entries = db::assoc_range(&state.pool, &assoc, node.node_id, side, after, limit)
        .await
        .map_err(internal)?;
    let next_cursor = if entries.len() as i64 >= limit {
        entries.last().map(assoc_cursor)
    } else {
        None
    };
    Ok(Json(AssocRangeResp {
        assoc,
        name: node.name,
        node_id: node.node_id,
        side: match side {
            db::AssocSide::Out => "out",
            db::AssocSide::In => "in",
        },
        count: entries.len(),
        next_cursor,
        entries,
    }))
}

async fn assoc_out(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Path((assoc, name)): Path<(String, String)>,
    Query(q): Query<AssocListQuery>,
) -> Res<AssocRangeResp> {
    assoc_range(&state, &headers, &assoc, &name, db::AssocSide::Out, q).await
}

async fn assoc_in(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Path((assoc, name)): Path<(String, String)>,
    Query(q): Query<AssocListQuery>,
) -> Res<AssocRangeResp> {
    assoc_range(&state, &headers, &assoc, &name, db::AssocSide::In, q).await
}

async fn assoc_count(
    State(state): State<Arc<AppState>>,
    Path((assoc, name)): Path<(String, String)>,
) -> Res<AssocCountResp> {
    let (assoc, node) = assoc_named_node(&state, &assoc, &name).await?;
    let (out, inbound) = db::assoc_count(&state.pool, &assoc, node.node_id)
        .await
        .map_err(internal)?;
    Ok(Json(AssocCountResp {
        assoc,
        name: node.name,
        node_id: node.node_id,
        out,
        r#in: inbound,
    }))
}

// ── POST /v1/apply ────────────────────────────────────────────────────────────
//
// A whole outbox batch in one request. Every brain that writes to this plane
// does so through a transactional outbox drained in id order, and the drain
// used to make two or three HTTP calls per row — resolve, write, confirm —
// which capped the whole estate's naming-plane throughput at a few hundred
// rows a second per brain. Here a drain sends up to MAX_APPLY_OPS rows as the
// trigger wrote them, and gets back a verdict for each.
//
// The verdicts move the idempotence question to the side that can answer it.
// "Is this bind already true?" used to be decided in eleven client copies by
// reading a 409 and resolving again; here it is decided next to the row, by
// the process that wrote it. A drain no longer interprets conflicts at all —
// it applies outcomes.
//
// Order holds. Ops run one at a time in the order given, each in its own
// transaction, and the first op that comes back `retry` or `error` stops the
// batch: everything after it is `skipped`, untouched, exactly as a row-by-row
// drain would have left it. `refused` and `already` do not stop the batch,
// because a row that will never land is quarantined by the drain and the row
// behind it does not depend on it landing.

/// Rows per apply. A drain's batch, not a crawl; large enough that one call
/// carries a whole tick's worth, small enough that one bad row costs at most
/// this many verdicts to find.
const MAX_APPLY_OPS: usize = 500;

async fn apply(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Json(req): Json<ApplyReq>,
) -> Res<ApplyResp> {
    let brain = caller_brain(&headers)?;
    if req.ops.len() > MAX_APPLY_OPS {
        return Err(err(
            StatusCode::BAD_REQUEST,
            "batch_too_large",
            &format!("at most {MAX_APPLY_OPS} ops per apply"),
        ));
    }
    Ok(Json(ApplyResp {
        results: apply_ops(&state, &brain, &req.ops).await,
    }))
}

/// Runs the ops in order and stops at the first that must hold its place.
async fn apply_ops(state: &AppState, brain: &str, ops: &[ApplyOp]) -> Vec<ApplyResult> {
    let mut results = Vec::with_capacity(ops.len());
    let mut halted = false;
    for op in ops {
        if halted {
            results.push(ApplyResult {
                outcome: "skipped",
                code: None,
                error: None,
                node_id: None,
            });
            continue;
        }
        let result = apply_one(state, brain, op).await;
        metrics::counter!(
            "manhattan_apply_ops_total",
            "op" => op.op.clone(),
            "outcome" => result.outcome,
        )
        .increment(1);
        if matches!(result.outcome, "retry" | "error") {
            halted = true;
        }
        results.push(result);
    }
    results
}

fn applied(node_id: Option<Uuid>) -> ApplyResult {
    ApplyResult {
        outcome: "applied",
        code: None,
        error: None,
        node_id,
    }
}

fn already(node_id: Option<Uuid>) -> ApplyResult {
    ApplyResult {
        outcome: "already",
        code: None,
        error: None,
        node_id,
    }
}

fn verdict(outcome: &'static str, code: &str, error: &str) -> ApplyResult {
    ApplyResult {
        outcome,
        code: Some(code.to_string()),
        error: Some(error.to_string()),
        node_id: None,
    }
}

/// The code and message of a refusal produced by one of the op functions.
fn refusal_parts(e: &(StatusCode, Json<Value>)) -> (String, String) {
    let code = e.1 .0["code"].as_str().unwrap_or("").to_string();
    let msg = e.1 .0["error"].as_str().unwrap_or("").to_string();
    (code, msg)
}

/// The default reading of a refusal the op-specific logic did not claim: by
/// status, which says whether the request or the plane is at fault.
fn classify(e: (StatusCode, Json<Value>)) -> ApplyResult {
    let (code, msg) = refusal_parts(&e);
    let outcome = match e.0 {
        // The op itself is wrong and will be wrong on every retry.
        StatusCode::BAD_REQUEST => "refused",
        // The plane could not answer: its fault, not the op's.
        StatusCode::SERVICE_UNAVAILABLE | StatusCode::INTERNAL_SERVER_ERROR => "error",
        // Forbidden, not found, a conflict this op did not name, a rate limit:
        // answered, and answerable differently once something else changes.
        _ => "retry",
    };
    verdict(outcome, &code, &msg)
}

fn is_permanent_bind_code(code: &str) -> bool {
    code == conflict::NAME_NEVER_REISSUED || code == conflict::NAME_IN_QUARANTINE
}

async fn apply_one(state: &AppState, brain: &str, op: &ApplyOp) -> ApplyResult {
    fn decode<T: serde::de::DeserializeOwned>(op: &ApplyOp) -> Result<T, ApplyResult> {
        serde_json::from_value(op.payload.clone()).map_err(|e| {
            verdict(
                "refused",
                "malformed_payload",
                &format!("the {} payload does not have the shape this plane expects: {e}", op.op),
            )
        })
    }

    match op.op.as_str() {
        "node" => {
            #[derive(Deserialize)]
            struct P {
                kind: String,
                name: String,
                namespace: String,
            }
            let p: P = match decode(op) {
                Ok(p) => p,
                Err(r) => return r,
            };
            match op_create_node(state, brain, &p.kind, Some(&p.name), Some(&p.namespace)).await {
                Ok(resp) => applied(Some(resp.node.node_id)),
                Err(e) => {
                    let (code, msg) = refusal_parts(&e);
                    if code == conflict::NAME_EXISTS {
                        let holder = e.1 .0["node_id"].as_str().and_then(|s| Uuid::parse_str(s).ok());
                        let held_kind = e.1 .0["kind"].as_str().unwrap_or("");
                        return match holder {
                            // The name is bound to a node of the kind this row
                            // wanted: registration is already true.
                            Some(id) if held_kind == p.kind.trim().to_lowercase() => already(Some(id)),
                            // Bound to something else. Nothing here can re-kind
                            // a node, and every edge behind this row would be
                            // refused for the wrong owner; hold and say so.
                            Some(id) => verdict(
                                "retry",
                                "kind_mismatch",
                                &format!(
                                    "{} is registered as kind {held_kind:?} on node {id} but must be {:?}",
                                    p.name, p.kind
                                ),
                            ),
                            None => verdict("retry", &code, &msg),
                        };
                    }
                    if is_permanent_bind_code(&code) {
                        return verdict("refused", &code, &msg);
                    }
                    classify(e)
                }
            }
        }

        "name" => {
            #[derive(Deserialize)]
            struct P {
                node_name: String,
                name: String,
                namespace: String,
            }
            let p: P = match decode(op) {
                Ok(p) => p,
                Err(r) => return r,
            };
            let node = match resolve_one(state, &p.node_name).await {
                Ok(Some(n)) => n,
                Ok(None) => {
                    return verdict(
                        "retry",
                        "node_unresolved",
                        &format!("{} does not resolve yet; its registration is an earlier row", p.node_name),
                    )
                }
                Err(e) => return classify(e),
            };
            match op_create_name(state, brain, node.node_id, &p.name, &p.namespace, false).await {
                Ok(_) => applied(Some(node.node_id)),
                Err(e) => {
                    let (code, msg) = refusal_parts(&e);
                    if code == conflict::NAME_EXISTS {
                        let holder = e.1 .0["node_id"].as_str().and_then(|s| Uuid::parse_str(s).ok());
                        return match holder {
                            Some(id) if id == node.node_id => already(Some(id)),
                            Some(id) => verdict(
                                "retry",
                                "name_held_elsewhere",
                                &format!(
                                    "{} is bound to node {id} but this row binds it to {}: not delivered while the name resolves to another node",
                                    p.name, node.node_id
                                ),
                            ),
                            None => verdict("retry", &code, &msg),
                        };
                    }
                    if is_permanent_bind_code(&code) {
                        return verdict("refused", &code, &msg);
                    }
                    classify(e)
                }
            }
        }

        "revoke_name" => {
            #[derive(Deserialize)]
            struct P {
                name: String,
            }
            let p: P = match decode(op) {
                Ok(p) => p,
                Err(r) => return r,
            };
            match op_revoke_name(state, brain, &p.name).await {
                Ok(row) => applied(Some(row.node_id)),
                Err(e) => {
                    let (code, _) = refusal_parts(&e);
                    // A name that no longer resolves, or is already retired, is
                    // in the state this row wants it in.
                    if e.0 == StatusCode::NOT_FOUND || code == "name_already_revoked" {
                        return already(None);
                    }
                    classify(e)
                }
            }
        }

        "edge" | "delete_edge" => {
            #[derive(Deserialize)]
            struct P {
                subject_name: String,
                predicate: String,
                object_name: String,
            }
            let p: P = match decode(op) {
                Ok(p) => p,
                Err(r) => return r,
            };
            let subject = match resolve_one(state, &p.subject_name).await {
                Ok(Some(n)) => n,
                Ok(None) => {
                    return verdict(
                        "retry",
                        "subject_unresolved",
                        &format!("subject {} does not resolve yet", p.subject_name),
                    )
                }
                Err(e) => return classify(e),
            };
            let object = match resolve_one(state, &p.object_name).await {
                Ok(Some(n)) => n,
                Ok(None) => {
                    return verdict(
                        "retry",
                        "object_unresolved",
                        &format!("object {} does not resolve yet", p.object_name),
                    )
                }
                Err(e) => return classify(e),
            };
            if op.op == "edge" {
                match op_create_edge(state, brain, subject.node_id, &p.predicate, object.node_id).await {
                    Ok(_) => applied(None),
                    Err(e) => {
                        let (code, msg) = refusal_parts(&e);
                        match code.as_str() {
                            c if c == conflict::EDGE_EXISTS => already(None),
                            c if c == conflict::TOO_DEEP || c == conflict::EDGE_CYCLE => {
                                verdict("refused", &code, &msg)
                            }
                            _ => classify(e),
                        }
                    }
                }
            } else {
                match op_delete_edge(state, brain, subject.node_id, &p.predicate, object.node_id).await {
                    Ok(_) => applied(None),
                    Err(e) => {
                        let (code, _) = refusal_parts(&e);
                        // An edge that is not there is already in the state
                        // this row wants; one with children beneath it is not,
                        // and clears once they are retracted.
                        if code == "edge_not_found" {
                            return already(None);
                        }
                        classify(e)
                    }
                }
            }
        }

        "assoc" | "unassoc" => {
            let req: AssocReq = match decode(op) {
                Ok(p) => p,
                Err(r) => return r,
            };
            let assert = op.op == "assoc";
            match op_assoc(state, brain, &req, assert).await {
                Ok(resp) if resp.changed => applied(None),
                Ok(_) => already(None),
                Err(e) => {
                    // A name that resolves to nothing can carry no association:
                    // for a retraction that is the end state, for an assertion
                    // it is an ordering dependency on an earlier row.
                    if e.0 == StatusCode::NOT_FOUND && !assert {
                        return already(None);
                    }
                    classify(e)
                }
            }
        }

        // An op this build does not know. A newer brain queued it during a
        // rolling deploy; the Manhattan that understands it is coming, so the
        // row holds its place rather than being quarantined.
        other => verdict(
            "retry",
            "unknown_op",
            &format!("this plane does not know the {other:?} op"),
        ),
    }
}

// ── POST /v1/addresses ────────────────────────────────────────────────────────

/// Attempts before giving up on finding a free address. With 2^40 addresses a collision is
/// already improbable; the retry exists so it is impossible rather than merely unlikely.
const ADDRESS_MINT_ATTEMPTS: u32 = 8;

async fn create_address(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Json(req): Json<CreateAddressReq>,
) -> Res<AddressResp> {
    let brain = caller_brain(&headers)?;

    let identity = db::fetch_node(&state.pool, req.identity_node_id)
        .await
        .map_err(internal)?
        .ok_or_else(|| {
            err(
                StatusCode::NOT_FOUND,
                "identity_node_not_found",
                "identity node not found",
            )
        })?;
    if identity.kind != "identity" {
        return Err(err(
            StatusCode::BAD_REQUEST,
            "node_not_identity",
            "node is not an identity — addresses resolve to identities only",
        ));
    }
    if identity.status != "active" {
        return Err(err(
            StatusCode::CONFLICT,
            "identity_not_active",
            "identity is not active",
        ));
    }
    if identity.owner != brain {
        return Err(forbid(&identity.owner));
    }

    for _ in 0..ADDRESS_MINT_ATTEMPTS {
        let address = addr::mint();
        let name = addr::to_name(&address);

        let mut tx = state.pool.begin().await.map_err(internal)?;

        // Minting inserts into `names` directly, so it passes the same gate every
        // other binding path does. Before 0005 the primary key refused a retired
        // address on its own; now the uniqueness index is partial on active rows and
        // this check is the only thing standing between a rotated address and the
        // stranger who would be handed it. A collision with history is not an error —
        // the draw was unlucky, so draw again.
        if !db::may_bind_name(&mut tx, &name, addr::NAMESPACE, None)
            .await
            .map_err(internal)?
            .is_bindable()
        {
            tx.rollback().await.map_err(internal)?;
            continue;
        }

        let node: NodeRow = sqlx::query_as(
            "INSERT INTO nodes (kind, owner) VALUES ('address', $1)
             RETURNING node_id, kind, owner, status, created_at, updated_at",
        )
        .bind(&identity.owner)
        .fetch_one(&mut *tx)
        .await
        .map_err(internal)?;

        let bound: Result<NameRow, sqlx::Error> = sqlx::query_as(
            "INSERT INTO names (name, node_id, namespace, is_primary)
             VALUES ($1::citext, $2, $3, TRUE)
             RETURNING name::text AS name, node_id, namespace, is_primary, status, created_at, revoked_at",
        )
        .bind(&name)
        .bind(node.node_id)
        .bind(addr::NAMESPACE)
        .fetch_one(&mut *tx)
        .await;

        let bound = match bound {
            Ok(row) => row,
            Err(e) if db::is_unique_violation(&e) => {
                // Address already minted. Discard this node and draw again.
                tx.rollback().await.map_err(internal)?;
                continue;
            }
            Err(e) => {
                // The insert's own error is the cause and the one reported; a
                // rollback failure behind it would only mask it.
                let _ = tx.rollback().await;
                return Err(internal(e));
            }
        };

        // The address node resolves_to the identity. Revoking the address later touches
        // this node and this name only — never the identity.
        db::insert_edge(&mut tx, node.node_id, "resolves_to", identity.node_id)
            .await
            .map_err(|e| edge_insert_refusal("address edge", e))?;

        tx.commit().await.map_err(internal)?;

        return Ok(Json(AddressResp {
            address,
            address_node_id: node.node_id,
            identity_node_id: identity.node_id,
            status: node.status,
            created_at: bound.created_at,
        }));
    }

    Err(err(
        StatusCode::INTERNAL_SERVER_ERROR,
        "address_mint_exhausted",
        "could not mint a free address",
    ))
}

// ── GET /v1/addresses/:address ────────────────────────────────────────────────

async fn resolve_address(
    State(state): State<Arc<AppState>>,
    Path(address): Path<String>,
) -> Res<AddressResolveResp> {
    let canonical = addr::normalise(&address).ok_or_else(|| {
        err(
            StatusCode::BAD_REQUEST,
            "malformed_address",
            "malformed address",
        )
    })?;

    // Deliberately narrow: possession of a live address proves the right to reach this
    // identity and nothing else, so the identity's other names never appear here.
    let row: Option<(Uuid, String)> = sqlx::query_as(
        "SELECT ident.node_id, an.status
           FROM names nm
           JOIN nodes an    ON an.node_id = nm.node_id AND an.kind = 'address'
           JOIN edges e     ON e.subject  = an.node_id AND e.predicate = 'resolves_to'
           JOIN nodes ident ON ident.node_id = e.object
          WHERE nm.name = $1::citext AND nm.status = 'active' AND an.status = 'active'
          LIMIT 1",
    )
    .bind(addr::to_name(&canonical))
    .fetch_optional(&state.pool)
    .await
    .map_err(internal)?;

    let (identity_node_id, status) = row.ok_or_else(|| {
        err(
            StatusCode::NOT_FOUND,
            "address_not_found",
            "address does not resolve — unknown or revoked",
        )
    })?;

    Ok(Json(AddressResolveResp {
        identity_node_id,
        status,
    }))
}

// ── POST /v1/addresses/:address/revoke ────────────────────────────────────────

async fn revoke_address(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Path(address): Path<String>,
) -> Res<AddressRevokeResp> {
    let brain = caller_brain(&headers)?;
    let canonical = addr::normalise(&address).ok_or_else(|| {
        err(
            StatusCode::BAD_REQUEST,
            "malformed_address",
            "malformed address",
        )
    })?;
    let name = addr::to_name(&canonical);

    let existing = db::fetch_name(&state.pool, &name)
        .await
        .map_err(internal)?
        .ok_or_else(|| {
            err(
                StatusCode::NOT_FOUND,
                "address_not_found",
                "address not found",
            )
        })?;

    let node = db::fetch_node(&state.pool, existing.node_id)
        .await
        .map_err(internal)?
        .ok_or_else(|| {
            err(
                StatusCode::NOT_FOUND,
                "address_node_not_found",
                "address node not found",
            )
        })?;
    if node.owner != brain {
        return Err(forbid(&node.owner));
    }
    if existing.status == "revoked" {
        return Err(err(
            StatusCode::CONFLICT,
            "address_already_revoked",
            "address is already revoked",
        ));
    }

    let identity_node_id: Option<Uuid> = sqlx::query_scalar(
        "SELECT object FROM edges WHERE subject = $1 AND predicate = 'resolves_to' LIMIT 1",
    )
    .bind(node.node_id)
    .fetch_optional(&state.pool)
    .await
    .map_err(internal)?;

    let mut tx = state.pool.begin().await.map_err(internal)?;
    db::lock_name(&mut tx, &name).await.map_err(internal)?;

    // Scoped to the active row: without it every historical revoked row for this
    // name is re-stamped and the provenance of who held it, and when, is lost.
    let revoked_at: Option<chrono::DateTime<chrono::Utc>> = sqlx::query_scalar(
        "UPDATE names
            SET status = 'revoked', revoked_at = NOW(), is_primary = FALSE
          WHERE name = $1::citext AND status = 'active'
      RETURNING revoked_at",
    )
    .bind(&name)
    .fetch_optional(&mut *tx)
    .await
    .map_err(internal)?
    // No live row left: a concurrent revoke won the race. A refusal with a reason,
    // not a 500 from an empty result set.
    .ok_or_else(|| {
        err(
            StatusCode::CONFLICT,
            "address_already_revoked",
            "address is already revoked",
        )
    })?;

    // The address node is retired. The identity node is untouched — that is the whole
    // feature: rotating a public contact address never moves the identity and never
    // touches an established conversation.
    sqlx::query("UPDATE nodes SET status = 'revoked', updated_at = NOW() WHERE node_id = $1")
        .bind(node.node_id)
        .execute(&mut *tx)
        .await
        .map_err(internal)?;

    tx.commit().await.map_err(internal)?;

    Ok(Json(AddressRevokeResp {
        address: canonical,
        address_node_id: node.node_id,
        identity_node_id,
        status: "revoked".into(),
        revoked_at,
    }))
}

// ── GET /v1/identities/:node_id/addresses ─────────────────────────────────────

async fn identity_addresses(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Path(node_id): Path<Uuid>,
) -> Res<Vec<AddressListItem>> {
    let brain = caller_brain(&headers)?;
    let identity = db::fetch_node(&state.pool, node_id)
        .await
        .map_err(internal)?
        .ok_or_else(|| {
            err(
                StatusCode::NOT_FOUND,
                "identity_node_not_found",
                "identity node not found",
            )
        })?;
    if identity.kind != "identity" {
        return Err(err(
            StatusCode::BAD_REQUEST,
            "node_not_identity",
            "node is not an identity",
        ));
    }
    // Enumerating an identity's addresses is a harvest primitive, so it is the
    // owning brain's read and no one else's.
    if identity.owner != brain {
        return Err(forbid(&identity.owner));
    }

    let rows: Vec<AddressListRow> = sqlx::query_as(
        "SELECT nm.name::text AS name, an.node_id AS address_node_id,
                nm.status AS status, nm.created_at, nm.revoked_at
           FROM edges e
           JOIN nodes an ON an.node_id = e.subject AND an.kind = 'address'
           JOIN names nm ON nm.node_id = an.node_id AND nm.namespace = $2
          WHERE e.object = $1 AND e.predicate = 'resolves_to'
          ORDER BY nm.created_at DESC",
    )
    .bind(node_id)
    .bind(addr::NAMESPACE)
    .fetch_all(&state.pool)
    .await
    .map_err(internal)?;

    Ok(Json(
        rows.into_iter()
            .map(|r| AddressListItem {
                address: addr::from_name(&r.name),
                address_node_id: r.address_node_id,
                status: r.status,
                created_at: r.created_at,
                revoked_at: r.revoked_at,
            })
            .collect(),
    ))
}

// ── Numbers ───────────────────────────────────────────────────────────────────

const NUMBER_MINT_ATTEMPTS: u32 = 8;

// ── What the address space can afford ─────────────────────────────────────────
//
// A Number is now eleven payload digits and a Luhn check: 10^11 addresses, down
// from the 2^60 the base32 shape carried. At that width the address space stops
// being free, and two limits below make it affordable. Both live HERE, at the
// endpoint that allocates, because a cap enforced by the brain that asks for
// Numbers is a cap enforced by nobody.
//
// THE ARITHMETIC, so the next person does not have to redo it. Utilisation is
// what matters, because a blind guess that passes the check digit hits an
// allocated Number with probability (allocated / 10^11):
//
//     10^6  Numbers  →  1 in 100,000 well-formed guesses hits
//     10^8  Numbers  →  1 in 1,000
//     10^10 Numbers  →  1 in 10
//
// Unlimited Numbers per identity puts the third line within reach of a large
// deployment, and a twelve-month quarantine holds retired Numbers out of the
// pool on top of whatever is live. A cap of five holds utilisation to 5% even at
// a billion identities all holding their maximum, and to a millionth of a
// percent at a realistic scale.

/// The most live Numbers one identity may hold at once.
///
/// FIVE, from the owner's own use cases rather than from a round number: a
/// conference, a class, a project group, a Number in a bio and one one-to-one
/// meet-up is five at the same time, which is already more than any of those
/// people run simultaneously. Somebody who needs a sixth has finished with one
/// of the first five, and retiring it is the action the refusal names.
///
/// Numbers in quarantine do NOT count against this. Their holder cannot use
/// them, so charging for them would punish rotation — the exact behaviour the
/// whole design is trying to encourage. They do still consume address space,
/// which is what the minting rate below bounds.
const NUMBER_CONCURRENT_CAP: i64 = 5;

/// The window every minting count is taken over: a rolling twelve months.
///
/// Rolling, never a lifetime total. A lifetime cap punishes somebody who
/// legitimately rotated a great deal one year for the rest of their life, and it
/// ends in an appeals path built to undo a rule this service wrote itself. A
/// rolling window bounds churn and then heals on its own.
///
/// It matches the quarantine deliberately: a Number minted and retired inside
/// this window is exactly a Number still sitting in quarantine, so what the
/// window counts is what the address space is currently carrying for this
/// identity.
const NUMBER_MINT_WINDOW_SECONDS: i64 = 365 * 24 * 60 * 60;

/// Mints inside the window that carry no friction at all.
///
/// TEN, because setting up a conference, a class, a project group and a couple
/// of meet-ups in one sitting is under ten, and nobody doing that should meet a
/// throttle. Past it, minting is not refused — it is slowed, by the ladder
/// below.
const NUMBER_MINT_FREE: i64 = 10;

/// Escalating friction, expressed as (mints already made in the window, the gap
/// required since the last mint). Read top down; the first row whose threshold
/// the count has reached is the one that applies.
///
/// A LADDER RATHER THAN A WALL, and the difference is the whole point. A hard
/// cap eventually tells a real person "no" and leaves them holding a Number they
/// have outgrown; a cooling-off period tells them "shortly" and never stops
/// being true. It is the same shape account-security systems use for repeated
/// credential changes, so it reads as a pace rather than as an accusation.
///
/// At the top rung an identity settles at about one mint a week — fifty-odd
/// names a year into a 10^11 space — which is churn the address space does not
/// notice and no honest use ever reaches.
const NUMBER_MINT_FRICTION: &[(i64, i64)] = &[
    (150, 7 * 24 * 60 * 60),
    (60, 24 * 60 * 60),
    (30, 6 * 60 * 60),
    (NUMBER_MINT_FREE, 60 * 60),
];

/// The gap required before the next mint, given how many were made in the
/// window. Zero means no friction.
fn number_mint_cooloff(minted_in_window: i64) -> i64 {
    for (threshold, gap) in NUMBER_MINT_FRICTION {
        if minted_in_window >= *threshold {
            return *gap;
        }
    }
    0
}

// RETIRING IS NEVER LIMITED, by any of this. One of the reasons a person rotates
// a Number is that somebody is using it to reach them and they want that to
// stop; a throttle on retiring would hold the door open for exactly as long as
// it lasted. So `revoke_number` below carries no cap, no window and no
// cooling-off, and the worst this ladder can do is leave somebody temporarily
// without a REPLACEMENT — unreachable by Number for a while, which is the safe
// direction for this to fail in.

/// Floor on every resolve, whatever the outcome. Live, revoked, unknown and
/// malformed must be indistinguishable by clock as well as by status code.
const NUMBER_RESOLVE_FLOOR: Duration = Duration::from_millis(30);

async fn hold_resolve_floor(started: Instant) {
    let elapsed = started.elapsed();
    if elapsed < NUMBER_RESOLVE_FLOOR {
        tokio::time::sleep(NUMBER_RESOLVE_FLOOR - elapsed).await;
    }
}

/// How a refused Number edge is reported, for both the draw and the reclaim.
fn edge_refusal(e: db::EdgeInsertError) -> (StatusCode, Json<Value>) {
    edge_insert_refusal("Number edge", e)
}

/// Creates the Number node and binds the name to it, inside the caller's
/// transaction.
///
/// `Ok((node, None))` means the name is already actively bound and the caller
/// should give up on this name — a fresh draw redraws, a reclaim refuses. The
/// node row is created either way and discarded with the rollback, which is what
/// keeps the two paths one piece of code instead of two that drift apart.
async fn bind_number(
    tx: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    identity: &NodeRow,
    name: &str,
) -> Result<(NodeRow, Option<NameRow>), (StatusCode, Json<Value>)> {
    let node: NodeRow = sqlx::query_as(
        "INSERT INTO nodes (kind, owner) VALUES ('number', $1)
         RETURNING node_id, kind, owner, status, created_at, updated_at",
    )
    .bind(&identity.owner)
    .fetch_one(&mut **tx)
    .await
    .map_err(internal)?;

    let bound: Result<NameRow, sqlx::Error> = sqlx::query_as(
        "INSERT INTO names (name, node_id, namespace, is_primary)
         VALUES ($1::citext, $2, $3, TRUE)
         RETURNING name::text AS name, node_id, namespace, is_primary, status, created_at, revoked_at",
    )
    .bind(name)
    .bind(node.node_id)
    .bind(number::NAMESPACE)
    .fetch_one(&mut **tx)
    .await;

    match bound {
        Ok(row) => Ok((node, Some(row))),
        Err(e) if db::is_unique_violation(&e) => Ok((node, None)),
        Err(e) => Err(internal(e)),
    }
}

// ── POST /v1/numbers ──────────────────────────────────────────────────────────

async fn create_number(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Json(req): Json<CreateNumberReq>,
) -> Res<NumberResp> {
    let brain = caller_brain(&headers)?;

    let identity = db::fetch_node(&state.pool, req.identity_node_id)
        .await
        .map_err(internal)?
        .ok_or_else(|| {
            err(
                StatusCode::NOT_FOUND,
                "identity_node_not_found",
                "identity node not found",
            )
        })?;
    if identity.kind != "identity" {
        return Err(err(
            StatusCode::BAD_REQUEST,
            "node_not_identity",
            "node is not an identity — Numbers resolve to identities only",
        ));
    }
    if identity.status != "active" {
        return Err(err(
            StatusCode::CONFLICT,
            "identity_not_active",
            "identity is not active",
        ));
    }
    if identity.owner != brain {
        return Err(forbid(&identity.owner));
    }

    // ── What this identity may still be allocated ───────────────────────────
    //
    // Both counts are taken against the database's own clock, before anything is
    // drawn, so a refusal costs no address space. They are deliberately NOT in
    // the retry loop below: that loop exists to step over a name collision, and
    // re-counting inside it would charge one mint several times.
    //
    // There is a benign race here — two simultaneous mints can both read four
    // and both commit, giving six — and it is left benign on purpose. Closing it
    // means serialising every mint for one identity behind a row lock on the
    // identity node, which is a contention cost paid on every mint to prevent an
    // over-allocation of one, self-correcting the moment anything is retired.
    // The address-space rule that actually matters is the minting rate, and that
    // one is not raced: it counts a window, not a balance.
    let live: (i64,) = sqlx::query_as(
        "SELECT COUNT(*)
           FROM edges e
           JOIN nodes nn ON nn.node_id = e.subject
                        AND nn.kind    = 'number'
                        AND nn.status  = 'active'
           JOIN names nm ON nm.node_id = nn.node_id
                        AND nm.namespace = $2
                        AND nm.status    = 'active'
          WHERE e.object = $1 AND e.predicate = 'resolves_to'",
    )
    .bind(identity.node_id)
    .bind(number::NAMESPACE)
    .fetch_one(&state.pool)
    .await
    .map_err(internal)?;
    if live.0 >= NUMBER_CONCURRENT_CAP {
        return Err(err(
            StatusCode::CONFLICT,
            conflict::NUMBER_CAP_REACHED,
            &format!(
                "this identity already holds {} live Numbers, which is the most one identity may hold at once. Retire one that is no longer in use and this will succeed. Retiring is never limited.",
                live.0
            ),
        ));
    }

    // Every Number ever minted for this identity inside the window, retired ones
    // included, and how long ago the most recent one was. A retired Number is
    // still address space held out of circulation — for exactly the length of
    // this window — so mint-and-retire costs precisely what mint-and-keep costs.
    // Counting only LIVE Numbers here would leave the churn attack wide open,
    // and the concurrent cap above does not touch it: five held, five retired,
    // repeat, is five live Numbers for ever and unbounded address space burned.
    let churn: (i64, Option<i64>) = sqlx::query_as(
        "SELECT COUNT(*),
                -- Cast, not inferred. EXTRACT returns NUMERIC, and decoding that
                -- as a float is a type mismatch that only shows up once the
                -- window has a row in it — so an uncast version passes the first
                -- mint and fails every one after it. Seconds as a whole number
                -- is what every other duration in this stack carries anyway.
                MIN(EXTRACT(EPOCH FROM (NOW() - nm.created_at)))::BIGINT
           FROM edges e
           JOIN nodes nn ON nn.node_id = e.subject AND nn.kind = 'number'
           JOIN names nm ON nm.node_id = nn.node_id AND nm.namespace = $2
          WHERE e.object = $1 AND e.predicate = 'resolves_to'
            AND nm.created_at > NOW() - ($3 * INTERVAL '1 second')",
    )
    .bind(identity.node_id)
    .bind(number::NAMESPACE)
    .bind(NUMBER_MINT_WINDOW_SECONDS)
    .fetch_one(&state.pool)
    .await
    .map_err(internal)?;
    let cooloff = number_mint_cooloff(churn.0);
    if cooloff > 0 {
        // MIN over the window is the MOST RECENT mint, because the value being
        // minimised is an age. A window with a count always has an age, but the
        // absent case is handled rather than unwrapped.
        let since = churn.1.unwrap_or(i64::MAX);
        if since < cooloff {
            return Err(err(
                StatusCode::TOO_MANY_REQUESTS,
                "number_mint_rate",
                &format!(
                    "this identity has been allocated {} Numbers in the last year, so new ones are paced. The next may be allocated in about {} minutes. Retiring a Number is never paced and works right now.",
                    churn.0,
                    ((cooloff - since) / 60).max(1)
                ),
            ));
        }
    }

    // ── A reclaim, when one was asked for ───────────────────────────────────
    //
    // Asking for a SPECIFIC Number is answered only for a Number this identity
    // really held. That restriction is not politeness: without it the endpoint
    // would answer "is this Number free?" about anybody's Number, one question
    // at a time, which is an enumeration oracle over a 10^11 space. With it, the
    // question is about the caller's own history and discloses nothing.
    //
    // Every failure below is ONE refusal with one code — never held, held by
    // somebody else, still live, still inside a stranger's quarantine — so the
    // refusal says nothing about the Number either.
    if let Some(requested) = req.reclaim.as_deref() {
        let canonical = number::normalise(requested).ok_or_else(|| {
            err(
                StatusCode::BAD_REQUEST,
                "malformed_number",
                "malformed Number",
            )
        })?;
        let name = number::to_name(&canonical);
        let refused = || {
            err(
                StatusCode::CONFLICT,
                "number_not_reclaimable",
                "that Number is not one this identity may reclaim",
            )
        };

        if !db::name_was_held_by(&state.pool, &name, identity.node_id)
            .await
            .map_err(internal)?
        {
            return Err(refused());
        }

        let mut tx = state.pool.begin().await.map_err(internal)?;
        // The same gate as every other binding path, with the reclaimer named so
        // the quarantine is waived for an identity asking for its OWN Number
        // back. It is not waived for anything else: a Number whose history holds
        // one other identity is still held for the full twelve months.
        if !db::may_bind_name(&mut tx, &name, number::NAMESPACE, Some(identity.node_id))
            .await
            .map_err(internal)?
            .is_bindable()
        {
            tx.rollback().await.map_err(internal)?;
            return Err(refused());
        }

        let (node, bound) = bind_number(&mut tx, &identity, &name).await?;
        let Some(bound) = bound else {
            // Still actively bound, so not reclaimable — the same refusal, so a
            // caller cannot tell "you already hold it" from "somebody else does"
            // from "you never held it".
            tx.rollback().await.map_err(internal)?;
            return Err(refused());
        };
        db::insert_edge(&mut tx, node.node_id, "resolves_to", identity.node_id)
            .await
            .map_err(edge_refusal)?;
        tx.commit().await.map_err(internal)?;

        // A NEW node id, so the policy row, label, lease and admission ledger the
        // previous binding carried are all keyed to a node nothing reaches any
        // more. A reclaimed Number comes back CLEAN: fresh budget, empty ledger,
        // no pending knocks, and no label — elohim-veni forgets a retired
        // Number's label on purpose, and reintroducing a store of owner-private
        // notes about Numbers that no longer exist would be a worse trade than
        // typing it again.
        return Ok(Json(NumberResp {
            number: canonical,
            number_node_id: node.node_id,
            identity_node_id: identity.node_id,
            status: node.status,
            created_at: bound.created_at,
        }));
    }

    for _ in 0..NUMBER_MINT_ATTEMPTS {
        let value = number::mint().ok_or_else(|| {
            err(
                StatusCode::INTERNAL_SERVER_ERROR,
                "number_draw_failed",
                "could not draw a Number from the system random source",
            )
        })?;
        let name = number::to_name(&value);

        let mut tx = state.pool.begin().await.map_err(internal)?;

        // Same gate as every other binding path, with NO reclaimer: a fresh draw
        // that lands on a Number in quarantine — anybody's, including this
        // identity's own — is discarded and redrawn rather than reissued. A
        // reclaim is something a person asks for by name, above; it is never
        // something a random draw quietly does to them.
        if !db::may_bind_name(&mut tx, &name, number::NAMESPACE, None)
            .await
            .map_err(internal)?
            .is_bindable()
        {
            tx.rollback().await.map_err(internal)?;
            continue;
        }

        let (node, bound) = bind_number(&mut tx, &identity, &name).await?;
        let Some(bound) = bound else {
            // Already actively bound. Discard this node and draw again.
            tx.rollback().await.map_err(internal)?;
            continue;
        };

        db::insert_edge(&mut tx, node.node_id, "resolves_to", identity.node_id)
            .await
            .map_err(edge_refusal)?;

        tx.commit().await.map_err(internal)?;

        return Ok(Json(NumberResp {
            number: value,
            number_node_id: node.node_id,
            identity_node_id: identity.node_id,
            status: node.status,
            created_at: bound.created_at,
        }));
    }

    Err(err(
        StatusCode::INTERNAL_SERVER_ERROR,
        "number_mint_exhausted",
        "could not allocate a free Number",
    ))
}

// ── GET /v1/numbers/:number ───────────────────────────────────────────────────

/// Resolves a Number to the identity behind it. The answer carries no name, no
/// handle and no profile — a Number buys a policy decision, which the caller
/// makes, and nothing else.
async fn resolve_number(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Path(value): Path<String>,
) -> Res<NumberResolveResp> {
    let started = Instant::now();
    caller_brain(&headers)?;

    let miss = NumberResolveResp {
        found: false,
        number_node_id: None,
        identity_node_id: None,
    };

    // Check symbol first: 36 of every 37 blind guesses die here, before any I/O.
    let canonical = match number::normalise(&value) {
        Some(c) => c,
        None => {
            hold_resolve_floor(started).await;
            return Ok(Json(miss));
        }
    };

    let row: Option<(Uuid, Uuid)> = sqlx::query_as(
        "SELECT nn.node_id, ident.node_id
           FROM names nm
           JOIN nodes nn    ON nn.node_id = nm.node_id AND nn.kind = 'number'
           JOIN edges e     ON e.subject  = nn.node_id AND e.predicate = 'resolves_to'
           JOIN nodes ident ON ident.node_id = e.object
          WHERE nm.name = $1::citext AND nm.status = 'active'
            AND nn.status = 'active' AND ident.status = 'active'
          LIMIT 1",
    )
    .bind(number::to_name(&canonical))
    .fetch_optional(&state.pool)
    .await
    .map_err(internal)?;

    hold_resolve_floor(started).await;

    Ok(Json(match row {
        Some((number_node_id, identity_node_id)) => NumberResolveResp {
            found: true,
            number_node_id: Some(number_node_id),
            identity_node_id: Some(identity_node_id),
        },
        None => miss,
    }))
}

// ── POST /v1/numbers/:number/revoke ───────────────────────────────────────────

/// Retires a Number. The identity node, its keys, its devices and every open
/// conversation are untouched: rotation mints a new node and revokes this name.
async fn revoke_number(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Path(value): Path<String>,
) -> Res<NumberRevokeResp> {
    let brain = caller_brain(&headers)?;
    let canonical = number::normalise(&value).ok_or_else(|| {
        err(
            StatusCode::BAD_REQUEST,
            "malformed_number",
            "malformed Number",
        )
    })?;
    let name = number::to_name(&canonical);

    let existing = db::fetch_name(&state.pool, &name)
        .await
        .map_err(internal)?
        .ok_or_else(|| {
            err(
                StatusCode::NOT_FOUND,
                "number_not_found",
                "Number not found",
            )
        })?;

    let node = db::fetch_node(&state.pool, existing.node_id)
        .await
        .map_err(internal)?
        .ok_or_else(|| {
            err(
                StatusCode::NOT_FOUND,
                "number_node_not_found",
                "Number node not found",
            )
        })?;
    if node.owner != brain {
        return Err(forbid(&node.owner));
    }
    if existing.status == "revoked" {
        return Err(err(
            StatusCode::CONFLICT,
            "number_already_revoked",
            "Number is already revoked",
        ));
    }

    let identity_node_id: Option<Uuid> = sqlx::query_scalar(
        "SELECT object FROM edges WHERE subject = $1 AND predicate = 'resolves_to' LIMIT 1",
    )
    .bind(node.node_id)
    .fetch_optional(&state.pool)
    .await
    .map_err(internal)?;

    let mut tx = state.pool.begin().await.map_err(internal)?;
    db::lock_name(&mut tx, &name).await.map_err(internal)?;

    // Scoped to the active row, so the history behind this Number keeps its own
    // timestamps.
    let revoked_at: Option<chrono::DateTime<chrono::Utc>> = sqlx::query_scalar(
        "UPDATE names
            SET status = 'revoked', revoked_at = NOW(), is_primary = FALSE
          WHERE name = $1::citext AND status = 'active'
      RETURNING revoked_at",
    )
    .bind(&name)
    .fetch_optional(&mut *tx)
    .await
    .map_err(internal)?
    // No live row left: a concurrent revoke won the race. A refusal with a reason,
    // not a 500 from an empty result set.
    .ok_or_else(|| {
        err(
            StatusCode::CONFLICT,
            "number_already_revoked",
            "Number is already revoked",
        )
    })?;

    sqlx::query("UPDATE nodes SET status = 'revoked', updated_at = NOW() WHERE node_id = $1")
        .bind(node.node_id)
        .execute(&mut *tx)
        .await
        .map_err(internal)?;

    tx.commit().await.map_err(internal)?;

    Ok(Json(NumberRevokeResp {
        number: canonical,
        number_node_id: node.node_id,
        identity_node_id,
        status: "revoked".into(),
        revoked_at,
    }))
}

// ── GET /v1/identities/:node_id/numbers ───────────────────────────────────────

async fn identity_numbers(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Path(node_id): Path<Uuid>,
) -> Res<Vec<NumberListItem>> {
    let brain = caller_brain(&headers)?;
    let identity = db::fetch_node(&state.pool, node_id)
        .await
        .map_err(internal)?
        .ok_or_else(|| {
            err(
                StatusCode::NOT_FOUND,
                "identity_node_not_found",
                "identity node not found",
            )
        })?;
    if identity.kind != "identity" {
        return Err(err(
            StatusCode::BAD_REQUEST,
            "node_not_identity",
            "node is not an identity",
        ));
    }
    if identity.owner != brain {
        return Err(forbid(&identity.owner));
    }

    let rows: Vec<NumberListRow> = sqlx::query_as(
        "SELECT nm.name::text AS name, nn.node_id AS number_node_id,
                nm.status AS status, nm.created_at, nm.revoked_at
           FROM edges e
           JOIN nodes nn ON nn.node_id = e.subject AND nn.kind = 'number'
           JOIN names nm ON nm.node_id = nn.node_id AND nm.namespace = $2
          WHERE e.object = $1 AND e.predicate = 'resolves_to'
          ORDER BY nm.created_at DESC",
    )
    .bind(node_id)
    .bind(number::NAMESPACE)
    .fetch_all(&state.pool)
    .await
    .map_err(internal)?;

    Ok(Json(
        rows.into_iter()
            .map(|r| {
                let value = number::from_name(&r.name);
                NumberListItem {
                    legacy: number::is_legacy(&value),
                    number: value,
                    number_node_id: r.number_node_id,
                    status: r.status,
                    created_at: r.created_at,
                    revoked_at: r.revoked_at,
                }
            })
            .collect(),
    ))
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Every path in the plane must insert into the router cleanly. This catches a route
    /// collision at test time instead of at boot.
    #[tokio::test]
    async fn routes_build() {
        // A lazy pool opens no connection (it only needs the runtime it will
        // later connect from); the router only needs a state value to bind the
        // guard, so no database is involved in this check.
        let pool = PgPool::connect_lazy("postgres://manhattan@localhost/manhattan_routes_test")
            .expect("lazy pool");
        let _ = routes(Arc::new(AppState {
            pool,
            internal_api_key: "routes-test-key".to_string(),
            cache: Arc::new(cache::ResolveCache::new(0)),
        }));
    }

    /// An association type is a grant recorded in a migration, not a string a
    /// caller invents. The shape check is all this function may decide; anything
    /// that looks like an assoc still has to exist in `assoc_authorities` before
    /// it can be written, and that is a database lookup by design.
    #[test]
    fn assoc_shape_is_validated_but_existence_is_not_hardcoded() {
        assert_eq!(validate_assoc_shape(" FOLLOWS ").unwrap(), "follows");
        assert_eq!(validate_assoc_shape("blocks_2").unwrap(), "blocks_2");
        // Shape-valid and entirely unknown: this function must still accept it,
        // because refusing here would move the authority map into the binary.
        assert_eq!(
            validate_assoc_shape("not_a_real_assoc").unwrap(),
            "not_a_real_assoc"
        );
        for bad in ["", "has space", "Colon:Name", "dash-ed", &"x".repeat(65)] {
            assert!(validate_assoc_shape(bad).is_err(), "accepted {bad:?}");
        }
    }

    /// The association plane refuses in machine-readable terms too, and its two
    /// refusals mean different things: `unknown_assoc` is "this relationship type
    /// does not exist", `assoc_forbidden` is "it exists and is not yours".
    /// A caller that cannot tell them apart cannot tell a typo from a privilege
    /// error.
    #[test]
    fn association_refusals_are_distinguishable() {
        let unknown = err(StatusCode::BAD_REQUEST, "unknown_assoc", "nope");
        let forbidden = err(StatusCode::FORBIDDEN, "assoc_forbidden", "nope");
        assert_eq!(unknown.0, StatusCode::BAD_REQUEST);
        assert_eq!(forbidden.0, StatusCode::FORBIDDEN);
        assert_ne!(
            unknown.1 .0["code"].as_str(),
            forbidden.1 .0["code"].as_str()
        );
    }

    #[test]
    fn addresses_round_trip_through_every_accepted_form() {
        let minted = addr::mint();
        assert_eq!(minted.len(), 9, "XXXX-XXXX");
        assert_eq!(addr::normalise(&minted).as_deref(), Some(minted.as_str()));
        assert_eq!(
            addr::normalise(&addr::to_name(&minted)).as_deref(),
            Some(minted.as_str())
        );
        assert_eq!(
            addr::normalise(&minted.replace('-', "").to_lowercase()).as_deref(),
            Some(minted.as_str())
        );
        assert_eq!(addr::from_name(&addr::to_name(&minted)), minted);
        // Ambiguous letters are not in the alphabet, so they are not addresses.
        assert_eq!(addr::normalise("IIII-LLLL"), None);
        assert_eq!(addr::normalise("too-short"), None);
    }

    /// The four 409 codes are a published contract: outbox drains across the fleet
    /// branch on these exact strings to tell a write that already landed from one
    /// that never happened. A rename here is a fleet-wide silent data loss, so the
    /// strings are pinned.
    #[test]
    fn conflict_codes_are_the_published_contract() {
        assert_eq!(conflict::NAME_EXISTS, "name_exists");
        assert_eq!(conflict::EDGE_EXISTS, "edge_exists");
        assert_eq!(conflict::NAME_NEVER_REISSUED, "name_never_reissued");
        assert_eq!(conflict::NAME_IN_QUARANTINE, "name_in_quarantine");
        assert_eq!(conflict::TOO_DEEP, "too_deep");
        assert_eq!(conflict::EDGE_CYCLE, "edge_cycle");
        assert_eq!(conflict::NUMBER_CAP_REACHED, "number_cap_reached");
    }

    /// The two limits that keep a 10^11 address space affordable. They are read
    /// here as a statement of intent: a cap in the tens would defeat the
    /// utilisation argument in the comment above `NUMBER_CONCURRENT_CAP`, and a
    /// minting rate at or above the concurrent cap would let mint-and-retire
    /// churn names into quarantine unboundedly.
    #[test]
    fn the_address_space_limits_are_the_ones_the_arithmetic_assumes() {
        assert!(
            (1..=8).contains(&NUMBER_CONCURRENT_CAP),
            "a concurrent cap outside single digits breaks the utilisation argument"
        );
        assert!(
            NUMBER_MINT_FREE > NUMBER_CONCURRENT_CAP,
            "an identity must be able to mint its full complement in one sitting, unthrottled"
        );
        assert!(
            NUMBER_MINT_WINDOW_SECONDS >= 60 * 60,
            "a window shorter than an hour bounds nothing over a year"
        );
    }

    /// The ladder must be a ladder: monotonically increasing friction, nothing
    /// before the free allowance, and never a refusal. A rung that went DOWN as
    /// churn went up would reward the churn it exists to slow, and a rung of
    /// zero past the allowance would silently remove the throttle.
    #[test]
    fn minting_friction_escalates_and_never_becomes_a_refusal() {
        for n in 0..NUMBER_MINT_FREE {
            assert_eq!(number_mint_cooloff(n), 0, "{n} mints must be free");
        }
        let mut previous = 0i64;
        for n in NUMBER_MINT_FREE..2_000 {
            let gap = number_mint_cooloff(n);
            assert!(gap > 0, "{n} mints must carry a cooling-off period");
            assert!(gap >= previous, "friction fell between {} and {n}", n - 1);
            previous = gap;
        }
        // Even at the top rung the answer is "shortly", never "no": a week's gap
        // still lets an identity mint about fifty times a year.
        assert!(
            number_mint_cooloff(10_000) <= 30 * 24 * 60 * 60,
            "a gap past a month is a refusal wearing a delay's clothes"
        );
    }

    /// Every refusal is machine-readable, not only the conflicts — a caller that has
    /// to string-match an English sentence is a caller that will get it wrong.
    #[test]
    fn every_error_shape_carries_a_code() {
        for (status, body) in [
            err(StatusCode::BAD_REQUEST, "invalid_kind", "nope"),
            forbid("elohim-veni"),
            forbid_namespace("handle", "registry-brain"),
            internal("boom"),
        ] {
            assert!(status.is_client_error() || status.is_server_error());
            assert!(
                body.0.get("code").and_then(Value::as_str).is_some(),
                "{body:?}"
            );
            assert!(
                body.0.get("error").and_then(Value::as_str).is_some(),
                "{body:?}"
            );
        }
        assert_eq!(
            forbid("elohim-veni").1 .0["code"].as_str(),
            Some("node_forbidden")
        );
        assert_eq!(
            forbid_namespace("handle", "registry-brain").1 .0["code"].as_str(),
            Some("namespace_forbidden")
        );
    }

    #[test]
    fn names_must_carry_their_namespace() {
        assert!(validate_name("handle:tehanibentley", "handle").is_ok());
        assert!(validate_name("cid:sha256:abc", "cid").is_ok());
        assert!(validate_name("tehanibentley", "handle").is_err());
        assert!(validate_name("handle:", "handle").is_err());
        assert!(validate_name("handle:with space", "handle").is_err());
    }
}
