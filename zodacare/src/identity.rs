//! Who a moderation decision is about — resolved, never assumed.
//!
//! Zodacare does not own identity. `elohim-veni` does (manhattan/migrations/
//! 0002_authority_map.sql). Everything zodacare stores about a person — a
//! report, a risk profile, a ban — is therefore a REFERENCE, and a reference is
//! a name resolved through Manhattan rather than a row copied out of another
//! brain's database.
//!
//! # Why a safety brain in particular
//!
//! A ban applied to the wrong identity is not a rendering glitch, it is a
//! person silenced who was never accused. Two ways that happens today:
//!
//!   * Three column spellings for one concept — `reported_pial_id`,
//!     `reporter_pial_id`, `pial_id` — and nothing that says they mean the same
//!     thing, so nothing catches it when they stop meaning it.
//!
//!   * Handle reuse. A handle is a POINTER: user A releases it, user B takes
//!     it, and every denormalised copy of that handle still aims at A. A ban
//!     keyed on a stored handle copy lands on the wrong person. Resolving
//!     through Manhattan makes that impossible, because a revoked name never
//!     resolves and is never re-minted silently — `RESOLVE_SELECT` reads only
//!     `names.status = 'active'`.
//!
//! So this module reduces every accepted spelling of a person to exactly one
//! thing: the PIAL, the identity root that never moves. That single UUID is
//! what reaches the database, and what reaches Elohim Veni when enforcement is
//! handed over.
//!
//! # What it will not do
//!
//! It will not guess. If a pointer cannot be followed — the naming plane is
//! unreachable, or the name does not resolve — the caller gets an error and no
//! moderation action is applied. Acting on an unverified identity is the exact
//! failure this exists to prevent, so there is no fallback path.

use uuid::Uuid;

use manhattan_client::{handle_name, pial_name, Manhattan, ManhattanError, KIND_IDENTITY, NS_PIAL};

// ── Errors ────────────────────────────────────────────────────────────────────

#[derive(Debug)]
pub enum ResolveError {
    /// The caller sent something that is not an identity reference at all.
    Malformed(String),
    /// The name does not resolve, or resolves to something revoked. Manhattan
    /// deliberately does not distinguish the two.
    Unresolvable(String),
    /// The name resolves, but to a node that is not a person.
    NotAnIdentity { name: String, kind: String },
    /// The name resolves to an identity that Manhattan holds under no PIAL.
    /// A broken node, not a broken request.
    NoPial(String),
    /// The naming plane could not be reached, so a pointer cannot be followed.
    /// Never downgraded into a guess.
    PlaneUnavailable(String),
}

impl std::fmt::Display for ResolveError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            ResolveError::Malformed(s) => write!(f, "not an identity reference: {s}"),
            ResolveError::Unresolvable(n) => write!(f, "{n} does not resolve"),
            ResolveError::NotAnIdentity { name, kind } => {
                write!(f, "{name} resolves to a '{kind}', not a person")
            }
            ResolveError::NoPial(n) => write!(f, "{n} resolves to an identity with no PIAL"),
            ResolveError::PlaneUnavailable(n) => {
                write!(
                    f,
                    "the naming plane is unreachable, so {n} cannot be resolved"
                )
            }
        }
    }
}

impl std::error::Error for ResolveError {}

// ── Shapes ────────────────────────────────────────────────────────────────────

/// What a caller's spelling of a person actually is.
///
/// The distinction is the whole point. A PIAL is the root: it is already the
/// answer, and Manhattan is asked only to confirm it. A pointer is a name that
/// aims at a root and may aim somewhere else tomorrow; only Manhattan can say
/// where it aims today.
enum Shape {
    Pial(Uuid),
    Pointer(String),
}

/// Classify one accepted spelling. Shape only — no network.
///
/// Accepted:
///   `<uuid>`            a bare PIAL, the historic wire form
///   `pial:<uuid>`       the same, named
///   `@handle` / `handle:<h>`  a pointer
///   `<other-ns>:<v>`    any other Manhattan name, followed as a pointer
///   `<bare word>`       a handle written without its namespace
fn shape(input: &str) -> Result<Shape, ResolveError> {
    let raw = input.trim();
    if raw.is_empty() {
        return Err(ResolveError::Malformed("empty identity reference".into()));
    }

    if let Some(h) = raw.strip_prefix('@') {
        if h.is_empty() {
            return Err(ResolveError::Malformed(raw.into()));
        }
        return Ok(Shape::Pointer(handle_name(h)));
    }

    if let Some(rest) = raw.strip_prefix("pial:") {
        return match Uuid::parse_str(rest) {
            Ok(u) => Ok(Shape::Pial(u)),
            // `pial:` promises a UUID. Anything else is a malformed name, not a
            // pointer to go hunting for.
            Err(_) => Err(ResolveError::Malformed(raw.into())),
        };
    }

    if raw.contains(':') {
        return Ok(Shape::Pointer(raw.to_string()));
    }

    match Uuid::parse_str(raw) {
        Ok(u) => Ok(Shape::Pial(u)),
        Err(_) => Ok(Shape::Pointer(handle_name(raw))),
    }
}

fn manhattan_failure(name: &str, e: ManhattanError) -> ResolveError {
    match e {
        ManhattanError::NotFound => ResolveError::Unresolvable(name.to_string()),
        ManhattanError::NotConfigured => ResolveError::PlaneUnavailable(name.to_string()),
        other => ResolveError::PlaneUnavailable(format!("{name}: {other}")),
    }
}

// ── Resolution ────────────────────────────────────────────────────────────────

/// Resolve one reference to the PIAL zodacare will store and act on.
pub async fn resolve_one(manhattan: &Manhattan, input: &str) -> Result<Uuid, ResolveError> {
    let mut out = resolve_all(manhattan, &[input]).await?;
    out.pop()
        .ok_or_else(|| ResolveError::Malformed(input.trim().to_string()))
}

/// Resolve several references in one round trip, preserving order.
///
/// `resolve_batch` is the reason a handle never needed copying into a row in
/// the first place: a whole request's worth of names costs one indexed lookup,
/// so there is nothing left to optimise by denormalising.
pub async fn resolve_all(
    manhattan: &Manhattan,
    inputs: &[&str],
) -> Result<Vec<Uuid>, ResolveError> {
    if inputs.is_empty() {
        return Ok(Vec::new());
    }

    let shapes: Vec<Shape> = inputs
        .iter()
        .map(|i| shape(i))
        .collect::<Result<Vec<_>, _>>()?;

    let names: Vec<String> = shapes
        .iter()
        .map(|s| match s {
            Shape::Pial(u) => pial_name(&u.to_string()),
            Shape::Pointer(n) => n.clone(),
        })
        .collect();

    // No naming plane configured. A PIAL is still the identity root and carries
    // its own answer — the outbox has already queued its registration, so it
    // becomes resolvable the moment the plane is reachable. A pointer has no
    // such answer, and inventing one is precisely the drift this client's own
    // header forbids.
    if !manhattan.configured() {
        return shapes
            .iter()
            .zip(names.iter())
            .map(|(s, n)| match s {
                Shape::Pial(u) => Ok(*u),
                Shape::Pointer(_) => Err(ResolveError::PlaneUnavailable(n.clone())),
            })
            .collect();
    }

    let resolved = manhattan
        .resolve_batch(&names)
        .await
        .map_err(|e| manhattan_failure("identity batch", e))?;

    let mut out = Vec::with_capacity(shapes.len());
    for (s, name) in shapes.iter().zip(names.iter()) {
        let found = resolved.get(name);
        match (s, found) {
            // A PIAL Manhattan has not seen yet. Registration works whoever
            // encounters the entity first — that is Manhattan's own rule for
            // node creation — and the outbox trigger on the row being written
            // has already queued it. The root is not in doubt, so proceed.
            (Shape::Pial(u), None) => out.push(*u),

            (Shape::Pial(u), Some(node)) => {
                if node.kind != KIND_IDENTITY {
                    return Err(ResolveError::NotAnIdentity {
                        name: name.clone(),
                        kind: node.kind.clone(),
                    });
                }
                out.push(*u);
            }

            // Revoked, transferred away, or never real. Manhattan does not say
            // which, and a safety brain must not act on the difference.
            (Shape::Pointer(_), None) => return Err(ResolveError::Unresolvable(name.clone())),

            (Shape::Pointer(_), Some(node)) => {
                if node.kind != KIND_IDENTITY {
                    return Err(ResolveError::NotAnIdentity {
                        name: name.clone(),
                        kind: node.kind.clone(),
                    });
                }
                // The pointer named the node; the node names the root. Ask for
                // the PIAL rather than assuming the pointer's spelling is one.
                let full = manhattan
                    .get_node(&node.node_id)
                    .await
                    .map_err(|e| manhattan_failure(name, e))?;
                let pial = full
                    .name_in(NS_PIAL)
                    .ok_or_else(|| ResolveError::NoPial(name.clone()))?;
                let uuid =
                    Uuid::parse_str(&pial).map_err(|_| ResolveError::NoPial(name.clone()))?;
                out.push(uuid);
            }
        }
    }

    Ok(out)
}
