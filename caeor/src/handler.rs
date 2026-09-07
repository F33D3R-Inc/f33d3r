use axum::{
    extract::{Multipart, Query, State},
    http::StatusCode,
    response::Json,
};
use image::{imageops::FilterType, DynamicImage, ImageFormat};
use serde::{Deserialize, Serialize};
use std::path::PathBuf;
use uuid::Uuid;

#[derive(Deserialize)]
pub struct DeleteRequest {
    pub url: String,
}

#[derive(Clone)]
pub struct AppState {
    pub media_dir: String,
}

#[derive(Deserialize)]
pub struct UploadQuery {
    #[serde(rename = "type")]
    pub media_type: Option<String>,
}

#[derive(Serialize)]
pub struct UploadResponse {
    pub primary: String,
    pub urls: Vec<Derivative>,
}

#[derive(Serialize)]
pub struct Derivative {
    pub variant: String,
    pub url: String,
    pub width: u32,
    pub height: u32,
}

pub async fn health() -> Json<serde_json::Value> {
    Json(serde_json::json!({
        "status": "ok",
        "brain": "caeor",
        "version": "1.0"
    }))
}

pub async fn upload(
    State(state): State<AppState>,
    Query(q): Query<UploadQuery>,
    mut multipart: Multipart,
) -> Result<Json<UploadResponse>, (StatusCode, String)> {
    let mut file_data: Option<Vec<u8>> = None;
    let mut original_name = String::from("upload");

    while let Some(field) = multipart
        .next_field()
        .await
        .map_err(|e| (StatusCode::BAD_REQUEST, format!("multipart error: {e}")))?
    {
        // Accept any field name — callers use file, media, avatar, header
        if file_data.is_none() {
            if let Some(fname) = field.file_name() {
                original_name = fname.to_string();
            }
            let bytes = field
                .bytes()
                .await
                .map_err(|e| (StatusCode::BAD_REQUEST, format!("read error: {e}")))?;
            if bytes.len() > 10 * 1024 * 1024 {
                return Err((StatusCode::BAD_REQUEST, "file too large (10 MB max)".into()));
            }
            file_data = Some(bytes.to_vec());
        }
    }

    let data = file_data.ok_or((StatusCode::BAD_REQUEST, "no file in request".into()))?;

    let ext = std::path::Path::new(&original_name)
        .extension()
        .and_then(|e| e.to_str())
        .unwrap_or("bin")
        .to_lowercase();

    // Video: store as-is, no processing
    if matches!(ext.as_str(), "mp4" | "mov" | "webm") {
        return store_video(&state, &data, &ext);
    }

    let img = image::load_from_memory(&data)
        .map_err(|e| (StatusCode::BAD_REQUEST, format!("invalid image: {e}")))?;

    match q.media_type.as_deref().unwrap_or("post") {
        "avatar" => process_avatar(&state, img),
        "header" => process_header(&state, img),
        _ => process_post(&state, img),
    }
}

// ── Avatar: square crop → 4 sizes ────────────────────────────────────────────

fn process_avatar(
    state: &AppState,
    img: DynamicImage,
) -> Result<Json<UploadResponse>, (StatusCode, String)> {
    let id = Uuid::new_v4().to_string();
    let dir = PathBuf::from(&state.media_dir).join("avatars");

    let sq = crop_square(&img);
    let sizes: &[(u32, &str)] = &[(64, "thumb"), (128, "sm"), (256, "feed"), (512, "hd")];
    let mut urls = Vec::new();

    for (px, variant) in sizes {
        let resized = sq.resize_exact(*px, *px, FilterType::Lanczos3);
        let fname = format!("{id}-{variant}.webp");
        webp_save(&resized, &dir.join(&fname))?;
        urls.push(Derivative {
            variant: variant.to_string(),
            url: format!("/static/media/avatars/{fname}"),
            width: *px,
            height: *px,
        });
    }

    let primary = urls
        .iter()
        .find(|u| u.variant == "feed")
        .map(|u| u.url.clone())
        .unwrap_or_default();

    Ok(Json(UploadResponse { primary, urls }))
}

// ── Header/banner: 3:1 center crop → 3 widths ────────────────────────────────

fn process_header(
    state: &AppState,
    img: DynamicImage,
) -> Result<Json<UploadResponse>, (StatusCode, String)> {
    let id = Uuid::new_v4().to_string();
    let dir = PathBuf::from(&state.media_dir).join("headers");

    let banner = crop_3x1(&img);
    let sizes: &[(u32, u32, &str)] = &[(600, 200, "sm"), (1200, 400, "md"), (2400, 800, "lg")];
    let mut urls = Vec::new();

    for (w, h, variant) in sizes {
        let resized = banner.resize_exact(*w, *h, FilterType::Lanczos3);
        let fname = format!("{id}-{variant}.webp");
        webp_save(&resized, &dir.join(&fname))?;
        urls.push(Derivative {
            variant: variant.to_string(),
            url: format!("/static/media/headers/{fname}"),
            width: *w,
            height: *h,
        });
    }

    let primary = urls
        .iter()
        .find(|u| u.variant == "md")
        .map(|u| u.url.clone())
        .unwrap_or_default();

    Ok(Json(UploadResponse { primary, urls }))
}

// ── Post image: thumb (300px sq) + feed (800w) + full (1200w) ─────────────────

fn process_post(
    state: &AppState,
    img: DynamicImage,
) -> Result<Json<UploadResponse>, (StatusCode, String)> {
    let id = Uuid::new_v4().to_string();
    let dir = PathBuf::from(&state.media_dir).join("posts");
    let mut urls = Vec::new();

    // Square thumbnail for grid layouts
    let thumb = crop_square(&img).resize_exact(300, 300, FilterType::Lanczos3);
    let thumb_fname = format!("{id}-thumb.webp");
    webp_save(&thumb, &dir.join(&thumb_fname))?;
    urls.push(Derivative {
        variant: "thumb".into(),
        url: format!("/static/media/posts/{thumb_fname}"),
        width: 300,
        height: 300,
    });

    // Feed size: max 800px wide, preserve aspect
    let feed = img.resize(800, 2400, FilterType::Lanczos3);
    let fw = feed.width();
    let fh = feed.height();
    let feed_fname = format!("{id}-feed.webp");
    webp_save(&feed, &dir.join(&feed_fname))?;
    urls.push(Derivative {
        variant: "feed".into(),
        url: format!("/static/media/posts/{feed_fname}"),
        width: fw,
        height: fh,
    });

    // Full: max 1200px wide
    let full = img.resize(1200, 3600, FilterType::Lanczos3);
    let fw2 = full.width();
    let fh2 = full.height();
    let full_fname = format!("{id}-full.webp");
    webp_save(&full, &dir.join(&full_fname))?;
    urls.push(Derivative {
        variant: "full".into(),
        url: format!("/static/media/posts/{full_fname}"),
        width: fw2,
        height: fh2,
    });

    let primary = format!("/static/media/posts/{feed_fname}");
    Ok(Json(UploadResponse { primary, urls }))
}

// ── Video: store as-is ────────────────────────────────────────────────────────

fn store_video(
    state: &AppState,
    data: &[u8],
    ext: &str,
) -> Result<Json<UploadResponse>, (StatusCode, String)> {
    let dir = PathBuf::from(&state.media_dir).join("posts");
    let fname = format!("{}.{ext}", Uuid::new_v4());
    std::fs::write(dir.join(&fname), data)
        .map_err(|e| (StatusCode::INTERNAL_SERVER_ERROR, e.to_string()))?;
    let url = format!("/static/media/posts/{fname}");
    Ok(Json(UploadResponse {
        primary: url.clone(),
        urls: vec![Derivative {
            variant: "original".into(),
            url,
            width: 0,
            height: 0,
        }],
    }))
}

// ── DELETE /v1/media — remove duplicate/orphaned media files ─────────────────
// Body: {"url": "/static/media/posts/{uuid}-feed.webp"}
// For post images: deletes all 3 variants (thumb, feed, full) by UUID prefix.
// For HLS video dirs: deletes the entire job directory under media-derived/posts/.

pub async fn delete_media(
    State(state): State<AppState>,
    Json(req): Json<DeleteRequest>,
) -> Json<serde_json::Value> {
    let url = req.url.trim();

    if url.starts_with("/static/media/posts/") {
        let fname = url.strip_prefix("/static/media/posts/").unwrap_or("");
        // UUID is everything before the first "-thumb", "-feed", or "-full" suffix.
        let id = fname
            .strip_suffix(".webp")
            .unwrap_or(fname)
            .rsplitn(2, '-')
            .last()
            .unwrap_or("");
        if !id.is_empty() {
            let dir = PathBuf::from(&state.media_dir).join("posts");
            for variant in &["thumb", "feed", "full"] {
                let _ = std::fs::remove_file(dir.join(format!("{id}-{variant}.webp")));
            }
        }
    } else if url.starts_with("/static/media/media-derived/posts/") {
        let rel = url
            .strip_prefix("/static/media/media-derived/posts/")
            .unwrap_or("");
        let job_dir = rel.split('/').next().unwrap_or("");
        if !job_dir.is_empty() {
            let dir = PathBuf::from(&state.media_dir)
                .join("media-derived/posts")
                .join(job_dir);
            let _ = std::fs::remove_dir_all(dir);
        }
    }

    Json(serde_json::json!({"ok": true}))
}

// ── Image helpers ─────────────────────────────────────────────────────────────

fn webp_save(img: &DynamicImage, path: &PathBuf) -> Result<(), (StatusCode, String)> {
    img.save_with_format(path, ImageFormat::WebP).map_err(|e| {
        (
            StatusCode::INTERNAL_SERVER_ERROR,
            format!("save failed: {e}"),
        )
    })
}

fn crop_square(img: &DynamicImage) -> DynamicImage {
    let (w, h) = (img.width(), img.height());
    let side = w.min(h);
    img.crop_imm((w - side) / 2, (h - side) / 2, side, side)
}

fn crop_3x1(img: &DynamicImage) -> DynamicImage {
    let (w, h) = (img.width(), img.height());
    let target_h = w / 3;
    if target_h <= h {
        img.crop_imm(0, (h - target_h) / 2, w, target_h)
    } else {
        let target_w = h * 3;
        img.crop_imm((w - target_w) / 2, 0, target_w, h)
    }
}
