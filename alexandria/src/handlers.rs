use axum::{
    extract::{Path, Query, State},
    http::{HeaderMap, StatusCode},
    response::{IntoResponse, Response},
    Json,
};
use std::collections::HashMap;
use std::sync::Arc;
use uuid::Uuid;

use crate::directory::{self, AuthorIdentity};
use crate::{db, models::*, AppState};
use manhattan_client::{pial_name, ManhattanError};

pub async fn health(State(s): State<Arc<AppState>>) -> impl IntoResponse {
    // A health check that cannot read its own outbox is not healthy, and saying
    // "ok" anyway is how a queue rots unobserved.
    let pending = match db::outbox_pending(&s.pool).await {
        Ok(n) => n,
        Err(e) => {
            tracing::error!("health: reading manhattan outbox depth: {e}");
            return (
                StatusCode::SERVICE_UNAVAILABLE,
                Json(serde_json::json!({
                    "status": "degraded",
                    "brain":  "alexandria",
                    "error":  "cannot read manhattan outbox",
                })),
            )
                .into_response();
        }
    };

    Json(serde_json::json!({
        "status":                   "ok",
        "brain":                    "alexandria",
        "manhattan":                s.manhattan.configured(),
        "manhattan_outbox_pending": pending,
    }))
    .into_response()
}

/// Turn a resolver outcome into either the identities a read needs or the
/// response it must return instead.
///
/// The two failure modes are not the same thing and must not be treated alike:
///
///   `NotConfigured` is a fact about the deployment — the naming plane is not
///   wired up here yet. There is no local handle column to fall back to (that
///   column was the bug), so the honest answer is a catalog with attribution
///   marked unknown, said loudly in the log and on /health.
///
///   Anything else is a dependency that is supposed to be there and is not.
///   Serving a page whose attribution is silently absent would look identical
///   to a page whose authors genuinely hold no handles, so the read refuses.
fn identities(
    ctx: &str,
    result: Result<HashMap<Uuid, AuthorIdentity>, ManhattanError>,
) -> Result<HashMap<Uuid, AuthorIdentity>, Response> {
    match result {
        Ok(m) => Ok(m),
        Err(ManhattanError::NotConfigured) => {
            tracing::error!(
                "{ctx}: MANHATTAN_URL is not set — serving the catalog without resolved handles"
            );
            Ok(HashMap::new())
        }
        Err(e) => {
            tracing::error!("{ctx}: resolving authors: {e}");
            Err((
                StatusCode::SERVICE_UNAVAILABLE,
                Json(ApiError::new("identity resolver unavailable")),
            )
                .into_response())
        }
    }
}

/// Assemble what a reader sees for one author: alexandria's catalog columns,
/// plus attribution resolved from the naming plane a moment ago.
fn author_view(author: Author, identity: Option<&AuthorIdentity>) -> AuthorView {
    let author_name = pial_name(&author.pial_id.to_string());
    AuthorView {
        author,
        author_name,
        handle: identity.and_then(|i| i.handle.clone()),
        identity_node_id: identity.map(|i| i.node_id.clone()),
    }
}

// ── Authors ────────────────────────────────────────────────────────────────────

/// Create or refresh an author's catalog row.
///
/// `req.handle` is ignored on purpose — see `BootstrapAuthorReq`. The insert
/// also enqueues the identity's registration on the naming plane, in the same
/// transaction, through the trigger on this table.
pub async fn bootstrap_author(
    State(s): State<Arc<AppState>>,
    Json(req): Json<BootstrapAuthorReq>,
) -> impl IntoResponse {
    if let Some(h) = &req.handle {
        if !h.is_empty() {
            tracing::debug!(
                "bootstrap_author: discarding supplied handle {h:?} — registry-brain owns handles"
            );
        }
    }

    let result = sqlx::query_as::<_, Author>(
        r#"
        INSERT INTO authors (pial_id, display_name, avatar_url)
        VALUES ($1, $2, $3)
        ON CONFLICT (pial_id) DO UPDATE SET
            display_name = CASE WHEN EXCLUDED.display_name != '' THEN EXCLUDED.display_name ELSE authors.display_name END,
            avatar_url   = CASE WHEN EXCLUDED.avatar_url   != '' THEN EXCLUDED.avatar_url   ELSE authors.avatar_url   END,
            updated_at   = NOW()
        RETURNING *
        "#,
    )
    .bind(req.pial_id)
    .bind(req.display_name.unwrap_or_default())
    .bind(req.avatar_url.unwrap_or_default())
    .fetch_one(&s.pool)
    .await;

    let author = match result {
        Ok(a) => a,
        Err(e) => {
            tracing::error!("bootstrap_author: {e}");
            return (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(ApiError::new("failed to bootstrap author")),
            )
                .into_response();
        }
    };

    // Attribution for the response, best effort — and deliberately not through
    // `identities`. The row is already written and its registration is already
    // queued; turning a landed write into a 503 because a DISPLAY lookup failed
    // would tell the caller to retry something that succeeded. The failure is
    // reported in the log and on /health, where an operator acts on it, rather
    // than in a status code that misdescribes what happened.
    //
    // A null handle here is also the normal case immediately after a first
    // insert: the identity's registration is queued and the drain has not run.
    let pial_id = author.pial_id;
    let resolved = match directory::resolve_authors(&s.manhattan, [pial_id]).await {
        Ok(m) => m,
        Err(e) => {
            tracing::error!("bootstrap_author: resolving {pial_id} for the response: {e}");
            HashMap::new()
        }
    };

    let view = author_view(author, resolved.get(&pial_id));
    (StatusCode::CREATED, Json(view)).into_response()
}

/// An author is addressed by PIAL, because that is the identity. A handle would
/// address whoever holds it today, which is a different question.
pub async fn get_author(
    State(s): State<Arc<AppState>>,
    Path(pial_id): Path<Uuid>,
) -> impl IntoResponse {
    let author = match sqlx::query_as::<_, Author>(
        "SELECT * FROM authors WHERE pial_id = $1 AND status != 'tombstoned'",
    )
    .bind(pial_id)
    .fetch_optional(&s.pool)
    .await
    {
        Ok(Some(a)) => a,
        Ok(None) => {
            return (
                StatusCode::NOT_FOUND,
                Json(ApiError::new("author not found")),
            )
                .into_response()
        }
        Err(e) => {
            tracing::error!("get_author: {e}");
            return (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(ApiError::new("server error")),
            )
                .into_response();
        }
    };

    let resolved = match identities(
        "get_author",
        directory::resolve_authors(&s.manhattan, [pial_id]).await,
    ) {
        Ok(m) => m,
        Err(resp) => return resp,
    };

    Json(author_view(author, resolved.get(&pial_id))).into_response()
}

pub async fn update_author(
    State(s): State<Arc<AppState>>,
    Path(pial_id): Path<Uuid>,
    Json(req): Json<UpdateAuthorReq>,
) -> impl IntoResponse {
    match sqlx::query_as::<_, Author>(
        r#"
        UPDATE authors SET
            display_name       = COALESCE($2, display_name),
            avatar_url         = COALESCE($3, avatar_url),
            header_url         = COALESCE($4, header_url),
            bio                = COALESCE($5, bio),
            primary_section_id = COALESCE($6, primary_section_id),
            creator_tier       = COALESCE($7, creator_tier),
            is_verified        = COALESCE($8, is_verified),
            updated_at         = NOW()
        WHERE pial_id = $1 AND status != 'tombstoned'
        RETURNING *
        "#,
    )
    .bind(pial_id)
    .bind(req.display_name)
    .bind(req.avatar_url)
    .bind(req.header_url)
    .bind(req.bio)
    .bind(req.primary_section_id)
    .bind(req.creator_tier)
    .bind(req.is_verified)
    .fetch_optional(&s.pool)
    .await
    {
        Ok(Some(a)) => Json(a).into_response(),
        Ok(None) => (
            StatusCode::NOT_FOUND,
            Json(ApiError::new("author not found")),
        )
            .into_response(),
        Err(e) => {
            tracing::error!("update_author: {e}");
            (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(ApiError::new("server error")),
            )
                .into_response()
        }
    }
}

// ── Titles ─────────────────────────────────────────────────────────────────────

pub async fn create_title(
    State(s): State<Arc<AppState>>,
    Json(req): Json<CreateTitleReq>,
) -> impl IntoResponse {
    // Resolve section_id from slug
    let section_id: Option<Uuid> = if let Some(slug) = &req.section_slug {
        sqlx::query_scalar::<_, Uuid>("SELECT id FROM sections WHERE slug = $1")
            .bind(slug)
            .fetch_optional(&s.pool)
            .await
            .unwrap_or(None)
    } else {
        None
    };

    // Resolve genre_id from slug
    let genre_id: Option<Uuid> = if let Some(slug) = &req.genre_slug {
        sqlx::query_scalar::<_, Uuid>("SELECT id FROM genres WHERE slug = $1")
            .bind(slug)
            .fetch_optional(&s.pool)
            .await
            .unwrap_or(None)
    } else {
        None
    };

    // Compute lineage from parent
    let lineage = if let Some(pid) = req.parent_id {
        let parent_lineage: Option<String> =
            sqlx::query_scalar::<_, String>("SELECT lineage FROM titles WHERE id = $1")
                .bind(pid)
                .fetch_optional(&s.pool)
                .await
                .unwrap_or(None);
        match parent_lineage {
            Some(l) if !l.is_empty() => format!("{}/{}", l, pid),
            _ => pid.to_string(),
        }
    } else {
        String::new()
    };

    let depth: i16 = if lineage.is_empty() {
        0
    } else {
        lineage.split('/').count() as i16
    };

    let media_area = req.media_area.unwrap_or_else(|| "text".to_string());
    let visibility = req.visibility.unwrap_or_else(|| "public".to_string());

    let title = match sqlx::query_as::<_, Title>(
        r#"
        INSERT INTO titles (
            author_pial_id, section_id, genre_id, headline, body,
            media_area, status, visibility,
            parent_id, root_id, thread_id, depth, lineage,
            is_nsfw, is_sensitive, published_at,
            legacy_work_id, legacy_post_id
        ) VALUES (
            $1, $2, $3, $4, $5,
            $6, 'published', $7,
            $8, $9, $10, $11, $12,
            $13, $14, NOW(),
            $15, $16
        )
        RETURNING *
        "#,
    )
    .bind(req.author_pial_id)
    .bind(section_id)
    .bind(genre_id)
    .bind(&req.headline)
    .bind(&req.body)
    .bind(&media_area)
    .bind(&visibility)
    .bind(req.parent_id)
    .bind(req.root_id)
    .bind(req.thread_id)
    .bind(depth)
    .bind(&lineage)
    .bind(req.is_nsfw.unwrap_or(false))
    .bind(req.is_sensitive.unwrap_or(false))
    .bind(req.legacy_work_id)
    .bind(req.legacy_post_id)
    .fetch_one(&s.pool)
    .await
    {
        Ok(t) => t,
        Err(e) => {
            tracing::error!("create_title insert: {e}");
            return (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(ApiError::new("failed to create title")),
            )
                .into_response();
        }
    };

    // Signals row
    let _ = sqlx::query("INSERT INTO title_signals (title_id) VALUES ($1) ON CONFLICT DO NOTHING")
        .bind(title.id)
        .execute(&s.pool)
        .await;

    // Tags
    if let Some(tags) = &req.tags {
        for tag in tags {
            let _ = sqlx::query(
                "INSERT INTO title_tags (title_id, tag) VALUES ($1, $2) ON CONFLICT DO NOTHING",
            )
            .bind(title.id)
            .bind(tag)
            .execute(&s.pool)
            .await;
        }
    }

    // Holdings
    if let Some(hs) = &req.holdings {
        for h in hs {
            let _ = sqlx::query(
                r#"INSERT INTO holdings (title_id, asset_type, caeor_path, mime_type, width, height, duration_secs, is_primary)
                   VALUES ($1, $2, $3, $4, $5, $6, $7, $8)"#,
            )
            .bind(title.id)
            .bind(&h.asset_type)
            .bind(&h.caeor_path)
            .bind(h.mime_type.clone().unwrap_or_default())
            .bind(h.width)
            .bind(h.height)
            .bind(h.duration_secs)
            .bind(h.is_primary.unwrap_or(false))
            .execute(&s.pool)
            .await;
        }
    }

    // Establish the author row and count the title in one statement.
    //
    // This was an UPDATE, which matched nothing whenever the author had never
    // been bootstrapped — every title by an unseen author silently failed to
    // count, and the identity was never registered on the naming plane because
    // the trigger that registers it fires on this table. Upserting makes the
    // counter true and the registration happen for the same reason: the first
    // title by a person is the moment alexandria learns that person exists.
    if let Err(e) = sqlx::query(
        r#"
        INSERT INTO authors (pial_id, title_count)
        VALUES ($1, 1)
        ON CONFLICT (pial_id) DO UPDATE SET
            title_count = authors.title_count + 1,
            updated_at  = NOW()
        "#,
    )
    .bind(req.author_pial_id)
    .execute(&s.pool)
    .await
    {
        tracing::error!(
            "create_title: counting title for author {}: {e}",
            req.author_pial_id
        );
    }

    (
        StatusCode::CREATED,
        Json(serde_json::json!({
            "id":        title.id,
            "work_name": title.work_name,
            "status":    "published",
        })),
    )
        .into_response()
}

pub async fn get_title(State(s): State<Arc<AppState>>, Path(id): Path<Uuid>) -> impl IntoResponse {
    let title = match sqlx::query_as::<_, Title>(
        "SELECT * FROM titles WHERE id = $1 AND tombstoned_at IS NULL",
    )
    .bind(id)
    .fetch_optional(&s.pool)
    .await
    {
        Ok(Some(t)) => t,
        Ok(None) => {
            return (
                StatusCode::NOT_FOUND,
                Json(ApiError::new("title not found")),
            )
                .into_response()
        }
        Err(e) => {
            tracing::error!("get_title: {e}");
            return (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(ApiError::new("server error")),
            )
                .into_response();
        }
    };

    let author_pial_id = title.author_pial_id;

    let author = match sqlx::query_as::<_, Author>("SELECT * FROM authors WHERE pial_id = $1")
        .bind(author_pial_id)
        .fetch_optional(&s.pool)
        .await
    {
        Ok(a) => a,
        Err(e) => {
            tracing::error!("get_title: loading author {author_pial_id}: {e}");
            return (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(ApiError::new("server error")),
            )
                .into_response();
        }
    };

    let signals =
        match sqlx::query_as::<_, TitleSignals>("SELECT * FROM title_signals WHERE title_id = $1")
            .bind(id)
            .fetch_optional(&s.pool)
            .await
        {
            Ok(v) => v,
            Err(e) => {
                tracing::error!("get_title: loading signals: {e}");
                return (
                    StatusCode::INTERNAL_SERVER_ERROR,
                    Json(ApiError::new("server error")),
                )
                    .into_response();
            }
        };

    let holdings = match sqlx::query_as::<_, Holding>(
        "SELECT * FROM holdings WHERE title_id = $1 ORDER BY is_primary DESC",
    )
    .bind(id)
    .fetch_all(&s.pool)
    .await
    {
        Ok(v) => v,
        Err(e) => {
            tracing::error!("get_title: loading holdings: {e}");
            return (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(ApiError::new("server error")),
            )
                .into_response();
        }
    };

    let tags =
        match sqlx::query_scalar::<_, String>("SELECT tag FROM title_tags WHERE title_id = $1")
            .bind(id)
            .fetch_all(&s.pool)
            .await
        {
            Ok(v) => v,
            Err(e) => {
                tracing::error!("get_title: loading tags: {e}");
                return (
                    StatusCode::INTERNAL_SERVER_ERROR,
                    Json(ApiError::new("server error")),
                )
                    .into_response();
            }
        };

    // Attribution is resolved, never read from a column here.
    let resolved = match identities(
        "get_title",
        directory::resolve_authors(&s.manhattan, [author_pial_id]).await,
    ) {
        Ok(m) => m,
        Err(resp) => return resp,
    };

    let author = author.map(|a| author_view(a, resolved.get(&author_pial_id)));

    Json(TitleFull {
        title,
        author,
        signals,
        holdings,
        tags,
    })
    .into_response()
}

/// A page of the catalog.
///
/// Attribution comes back as an `authors` map keyed by identity name rather than
/// a handle stamped onto every row. One batch call answers the whole page, and
/// nothing in the response repeats a pointer that registry-brain may move
/// tomorrow — which is precisely the shape that made a copied handle look
/// affordable before `resolve_batch` existed.
pub async fn list_titles(
    State(s): State<Arc<AppState>>,
    Query(q): Query<ListTitlesQuery>,
) -> impl IntoResponse {
    let limit = q.limit.unwrap_or(20).clamp(1, 100);

    // `genre` was accepted and then never applied: ?genre=jazz returned the
    // whole catalog and said nothing about it. A filter that silently does
    // nothing is worse than one that errors, so it filters.
    let titles = match sqlx::query_as::<_, Title>(
        r#"
        SELECT t.* FROM titles t
        LEFT JOIN sections sec ON t.section_id = sec.id
        LEFT JOIN genres   gen ON t.genre_id   = gen.id
        WHERE t.tombstoned_at IS NULL
          AND t.status = 'published'
          AND ($1::text        IS NULL OR sec.slug         = $1)
          AND ($2::text        IS NULL OR gen.slug         = $2)
          AND ($3::uuid        IS NULL OR t.author_pial_id = $3)
          AND ($4::timestamptz IS NULL OR t.published_at   < $4)
        ORDER BY t.published_at DESC
        LIMIT $5
        "#,
    )
    .bind(q.section)
    .bind(q.genre)
    .bind(q.author_pial)
    .bind(q.after)
    .bind(limit)
    .fetch_all(&s.pool)
    .await
    {
        Ok(v) => v,
        Err(e) => {
            tracing::error!("list_titles: {e}");
            return (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(ApiError::new("server error")),
            )
                .into_response();
        }
    };

    let resolved = match identities(
        "list_titles",
        directory::resolve_authors(&s.manhattan, titles.iter().map(|t| t.author_pial_id)).await,
    ) {
        Ok(m) => m,
        Err(resp) => return resp,
    };

    let authors: HashMap<String, &AuthorIdentity> = resolved
        .iter()
        .map(|(pial_id, ident)| (pial_name(&pial_id.to_string()), ident))
        .collect();

    Json(serde_json::json!({"titles": titles, "authors": authors})).into_response()
}

pub async fn update_title(
    State(s): State<Arc<AppState>>,
    Path(id): Path<Uuid>,
    Json(req): Json<UpdateTitleReq>,
) -> impl IntoResponse {
    let genre_id: Option<Uuid> = if let Some(slug) = &req.genre_slug {
        sqlx::query_scalar::<_, Uuid>("SELECT id FROM genres WHERE slug = $1")
            .bind(slug)
            .fetch_optional(&s.pool)
            .await
            .unwrap_or(None)
    } else {
        None
    };

    match sqlx::query(
        r#"
        UPDATE titles SET
            headline     = COALESCE($2, headline),
            body         = COALESCE($3, body),
            status       = COALESCE($4, status),
            visibility   = COALESCE($5, visibility),
            genre_id     = COALESCE($6, genre_id),
            is_nsfw      = COALESCE($7, is_nsfw),
            is_sensitive = COALESCE($8, is_sensitive),
            updated_at   = NOW()
        WHERE id = $1 AND tombstoned_at IS NULL
        "#,
    )
    .bind(id)
    .bind(&req.headline)
    .bind(&req.body)
    .bind(&req.status)
    .bind(&req.visibility)
    .bind(genre_id)
    .bind(req.is_nsfw)
    .bind(req.is_sensitive)
    .execute(&s.pool)
    .await
    {
        Ok(r) if r.rows_affected() > 0 => StatusCode::NO_CONTENT.into_response(),
        Ok(_) => (
            StatusCode::NOT_FOUND,
            Json(ApiError::new("title not found")),
        )
            .into_response(),
        Err(e) => {
            tracing::error!("update_title: {e}");
            (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(ApiError::new("server error")),
            )
                .into_response()
        }
    }
}

pub async fn tombstone_title(
    State(s): State<Arc<AppState>>,
    Path(id): Path<Uuid>,
    headers: HeaderMap,
) -> impl IntoResponse {
    let caller = match headers
        .get("x-pial-identity")
        .and_then(|v| v.to_str().ok())
        .and_then(|v| Uuid::parse_str(v).ok())
    {
        Some(u) => u,
        None => {
            return (
                StatusCode::UNAUTHORIZED,
                Json(ApiError::new("missing identity")),
            )
                .into_response()
        }
    };

    match sqlx::query(
        r#"
        UPDATE titles SET
            tombstoned_at = NOW(),
            status        = 'tombstoned',
            updated_at    = NOW()
        WHERE id = $1 AND author_pial_id = $2 AND tombstoned_at IS NULL
        "#,
    )
    .bind(id)
    .bind(caller)
    .execute(&s.pool)
    .await
    {
        Ok(r) if r.rows_affected() > 0 => {
            let _ = sqlx::query(
                "UPDATE authors SET title_count = GREATEST(0, title_count - 1), updated_at = NOW() WHERE pial_id = $1",
            )
            .bind(caller)
            .execute(&s.pool)
            .await;
            StatusCode::NO_CONTENT.into_response()
        }
        Ok(_) => (
            StatusCode::NOT_FOUND,
            Json(ApiError::new("title not found or not owner")),
        )
            .into_response(),
        Err(e) => {
            tracing::error!("tombstone_title: {e}");
            (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(ApiError::new("server error")),
            )
                .into_response()
        }
    }
}

// ── Holdings ───────────────────────────────────────────────────────────────────

pub async fn add_holding(
    State(s): State<Arc<AppState>>,
    Path(title_id): Path<Uuid>,
    Json(h): Json<HoldingInput>,
) -> impl IntoResponse {
    match sqlx::query_as::<_, Holding>(
        r#"
        INSERT INTO holdings (title_id, asset_type, caeor_path, mime_type, width, height, duration_secs, is_primary)
        VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
        RETURNING *
        "#,
    )
    .bind(title_id)
    .bind(&h.asset_type)
    .bind(&h.caeor_path)
    .bind(h.mime_type.unwrap_or_default())
    .bind(h.width)
    .bind(h.height)
    .bind(h.duration_secs)
    .bind(h.is_primary.unwrap_or(false))
    .fetch_one(&s.pool)
    .await
    {
        Ok(holding) => (StatusCode::CREATED, Json(holding)).into_response(),
        Err(e) => {
            tracing::error!("add_holding: {e}");
            (StatusCode::INTERNAL_SERVER_ERROR, Json(ApiError::new("failed to add holding"))).into_response()
        }
    }
}

// ── Signals ────────────────────────────────────────────────────────────────────

pub async fn increment_signals(
    State(s): State<Arc<AppState>>,
    Path(title_id): Path<Uuid>,
    Json(req): Json<IncrementSignalsReq>,
) -> impl IntoResponse {
    match sqlx::query(
        r#"
        INSERT INTO title_signals (title_id) VALUES ($1)
        ON CONFLICT (title_id) DO UPDATE SET
            like_count     = title_signals.like_count     + COALESCE($2, 0),
            repost_count   = title_signals.repost_count   + COALESCE($3, 0),
            reply_count    = title_signals.reply_count    + COALESCE($4, 0),
            quote_count    = title_signals.quote_count    + COALESCE($5, 0),
            view_count     = title_signals.view_count     + COALESCE($6, 0),
            bookmark_count = title_signals.bookmark_count + COALESCE($7, 0),
            updated_at     = NOW()
        "#,
    )
    .bind(title_id)
    .bind(req.like_delta)
    .bind(req.repost_delta)
    .bind(req.reply_delta)
    .bind(req.quote_delta)
    .bind(req.view_delta)
    .bind(req.bookmark_delta)
    .execute(&s.pool)
    .await
    {
        Ok(_) => StatusCode::NO_CONTENT.into_response(),
        Err(e) => {
            tracing::error!("increment_signals: {e}");
            (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(ApiError::new("server error")),
            )
                .into_response()
        }
    }
}

// ── Catalog ────────────────────────────────────────────────────────────────────

pub async fn list_sections(State(s): State<Arc<AppState>>) -> impl IntoResponse {
    let sections = sqlx::query_as::<_, Section>("SELECT * FROM sections ORDER BY display_order")
        .fetch_all(&s.pool)
        .await
        .unwrap_or_default();
    Json(serde_json::json!({"sections": sections})).into_response()
}

pub async fn list_genres(
    State(s): State<Arc<AppState>>,
    Query(q): Query<ListGenresQuery>,
) -> impl IntoResponse {
    let genres = sqlx::query_as::<_, Genre>(
        r#"
        SELECT g.* FROM genres g
        LEFT JOIN sections s ON g.section_id = s.id
        WHERE ($1::text IS NULL OR s.slug = $1)
        ORDER BY g.display_order
        "#,
    )
    .bind(q.section)
    .fetch_all(&s.pool)
    .await
    .unwrap_or_default();
    Json(serde_json::json!({"genres": genres})).into_response()
}
