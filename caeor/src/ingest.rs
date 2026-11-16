//! Receiving upload bytes: streamed straight into the private store while
//! they are hashed, so a 10 GB video is never held in memory and its SHA-256
//! is known the moment the last byte lands.

use std::path::PathBuf;

use axum::extract::{FromRequest, Multipart, Request};
use axum::http::header::CONTENT_TYPE;
use futures_util::StreamExt;
use sha2::{Digest, Sha256};
use tokio::io::AsyncWriteExt;
use uuid::Uuid;

use crate::api::ApiError;
use crate::layout::{Layout, Location};

/// An upload sitting in the private store's incoming/ directory. Removed when
/// dropped unless it was moved into place with `persist`.
pub struct Incoming {
    pub abs: PathBuf,
    pub sha256: String,
    pub size: u64,
    /// The file name the caller gave, if any. Used only for its extension.
    pub filename: Option<String>,
    kept: bool,
}

impl Incoming {
    /// The extension the caller's file name carries, lowercased.
    pub fn extension(&self) -> Option<String> {
        self.filename
            .as_deref()
            .and_then(|n| n.rsplit_once('.').map(|(_, e)| e.to_ascii_lowercase()))
            .filter(|e| !e.is_empty() && e.len() <= 8 && e.chars().all(|c| c.is_ascii_alphanumeric()))
    }

    /// Moves the upload to its permanent private location.
    pub async fn persist(mut self, layout: &Layout, dest: &Location) -> anyhow::Result<PathBuf> {
        let abs = layout.abs(dest)?;
        if let Some(parent) = abs.parent() {
            tokio::fs::create_dir_all(parent).await?;
        }
        tokio::fs::rename(&self.abs, &abs).await?;
        self.kept = true;
        Ok(abs)
    }

    /// Reads the whole upload. Only for kinds with a small size limit.
    pub async fn read_all(&self) -> anyhow::Result<Vec<u8>> {
        Ok(tokio::fs::read(&self.abs).await?)
    }
}

impl Drop for Incoming {
    fn drop(&mut self) {
        if self.kept {
            return;
        }
        if let Err(e) = std::fs::remove_file(&self.abs) {
            if e.kind() != std::io::ErrorKind::NotFound {
                tracing::error!(path = %self.abs.display(), error = %e, "removing an abandoned upload");
            }
        }
    }
}

struct Sink {
    file: tokio::io::BufWriter<tokio::fs::File>,
    hasher: Sha256,
    size: u64,
    limit: u64,
}

impl Sink {
    async fn push(&mut self, chunk: &[u8]) -> Result<(), ApiError> {
        self.size += chunk.len() as u64;
        if self.size > self.limit {
            return Err(ApiError::new(
                axum::http::StatusCode::PAYLOAD_TOO_LARGE,
                format!("upload exceeds {} bytes", self.limit),
            ));
        }
        self.hasher.update(chunk);
        self.file
            .write_all(chunk)
            .await
            .map_err(|e| ApiError::internal(format!("writing upload: {e}")))
    }
}

/// Receives the request body into the private store.
///
/// Accepts either a multipart form (the first field that carries a file name,
/// or else the first field) or a raw body. A raw body's file name may be given
/// in `filename`.
pub async fn receive(
    layout: &Layout,
    req: Request,
    filename: Option<String>,
    limit: u64,
) -> Result<Incoming, ApiError> {
    let upload_id = Uuid::new_v4();
    let abs = layout
        .abs(&Layout::incoming(upload_id))
        .map_err(|e| ApiError::internal(e.to_string()))?;
    let file = tokio::fs::File::create(&abs)
        .await
        .map_err(|e| ApiError::internal(format!("creating {}: {e}", abs.display())))?;
    // From here on the file is owned by `incoming`, which removes it on any
    // early return.
    let mut incoming = Incoming {
        abs,
        sha256: String::new(),
        size: 0,
        filename,
        kept: false,
    };
    let mut sink = Sink {
        file: tokio::io::BufWriter::new(file),
        hasher: Sha256::new(),
        size: 0,
        limit,
    };

    let is_multipart = req
        .headers()
        .get(CONTENT_TYPE)
        .and_then(|v| v.to_str().ok())
        .map(|ct| ct.to_ascii_lowercase().starts_with("multipart/form-data"))
        .unwrap_or(false);

    if is_multipart {
        let mut multipart = Multipart::from_request(req, &())
            .await
            .map_err(|e| ApiError::bad_request(format!("multipart: {e}")))?;
        let mut got = false;
        while let Some(mut field) = multipart
            .next_field()
            .await
            .map_err(|e| ApiError::bad_request(format!("multipart: {e}")))?
        {
            if got {
                // One file per upload; later fields are not part of it.
                continue;
            }
            if let Some(name) = field.file_name() {
                incoming.filename = Some(name.to_string());
            } else if field.name() != Some("file") {
                continue;
            }
            while let Some(chunk) = field
                .chunk()
                .await
                .map_err(|e| ApiError::bad_request(format!("multipart read: {e}")))?
            {
                sink.push(&chunk).await?;
            }
            got = true;
        }
        if !got {
            return Err(ApiError::bad_request("no file in the multipart body"));
        }
    } else {
        let mut stream = req.into_body().into_data_stream();
        while let Some(chunk) = stream.next().await {
            let chunk = chunk.map_err(|e| ApiError::bad_request(format!("reading body: {e}")))?;
            sink.push(&chunk).await?;
        }
    }

    if sink.size == 0 {
        return Err(ApiError::bad_request("empty upload"));
    }
    sink.file
        .flush()
        .await
        .map_err(|e| ApiError::internal(format!("flushing upload: {e}")))?;
    sink.file
        .get_mut()
        .sync_all()
        .await
        .map_err(|e| ApiError::internal(format!("syncing upload: {e}")))?;
    incoming.size = sink.size;
    incoming.sha256 = hex::encode(sink.hasher.finalize());
    Ok(incoming)
}
