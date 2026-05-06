//! S3-compatible object storage wrapper. Targets MinIO at T0–T2,
//! migrates to Cloudflare R2 at T3 by changing only the endpoint URL.

use std::time::Duration;

use anyhow::{Context, Result};
use aws_config::{BehaviorVersion, Region};
use aws_credential_types::Credentials;
use aws_sdk_s3::{
    config::{Builder as S3ConfigBuilder, Region as S3Region},
    presigning::PresigningConfig,
    primitives::ByteStream,
    Client,
};
use bytes::Bytes;

#[derive(Clone)]
pub struct Storage {
    client: Client,
    pub bucket_raw: String,
    pub bucket_derived: String,
}

#[derive(Debug, Clone)]
pub struct StorageConfig {
    pub endpoint: String,
    pub access_key: String,
    pub secret_key: String,
    pub region: String,
    pub bucket_raw: String,
    pub bucket_derived: String,
}

impl Storage {
    pub async fn from_env() -> Result<Self> {
        let cfg = StorageConfig {
            endpoint: std::env::var("MINIO_ENDPOINT").unwrap_or_else(|_| "http://minio:9000".into()),
            access_key: std::env::var("MINIO_ACCESS_KEY")
                .unwrap_or_else(|_| "f33d3r_minio".into()),
            secret_key: std::env::var("MINIO_SECRET_KEY")
                .unwrap_or_else(|_| "f33d3rdev_change_in_prod_32c".into()),
            region: std::env::var("MINIO_REGION").unwrap_or_else(|_| "us-east-1".into()),
            bucket_raw: std::env::var("MINIO_BUCKET_RAW").unwrap_or_else(|_| "media-raw".into()),
            bucket_derived: std::env::var("MINIO_BUCKET_DERIVED")
                .unwrap_or_else(|_| "media-derived".into()),
        };
        Self::new(cfg).await
    }

    pub async fn new(cfg: StorageConfig) -> Result<Self> {
        let creds = Credentials::new(&cfg.access_key, &cfg.secret_key, None, None, "static");

        let shared = aws_config::defaults(BehaviorVersion::latest())
            .region(Region::new(cfg.region.clone()))
            .credentials_provider(creds)
            .load()
            .await;

        let s3cfg = S3ConfigBuilder::from(&shared)
            .endpoint_url(&cfg.endpoint)
            .force_path_style(true)
            .region(S3Region::new(cfg.region.clone()))
            .build();

        Ok(Self {
            client: Client::from_conf(s3cfg),
            bucket_raw: cfg.bucket_raw,
            bucket_derived: cfg.bucket_derived,
        })
    }

    /// Upload bytes to a bucket+key.
    pub async fn put(
        &self,
        bucket: &str,
        key: &str,
        body: Bytes,
        content_type: &str,
    ) -> Result<()> {
        self.client
            .put_object()
            .bucket(bucket)
            .key(key)
            .body(ByteStream::from(body))
            .content_type(content_type)
            .send()
            .await
            .with_context(|| format!("put_object {bucket}/{key}"))?;
        Ok(())
    }

    /// Upload a local file to bucket+key. Useful for transcoder output.
    pub async fn put_file(
        &self,
        bucket: &str,
        key: &str,
        path: &std::path::Path,
        content_type: &str,
    ) -> Result<()> {
        let body = ByteStream::from_path(path)
            .await
            .with_context(|| format!("read {}", path.display()))?;
        self.client
            .put_object()
            .bucket(bucket)
            .key(key)
            .body(body)
            .content_type(content_type)
            .send()
            .await
            .with_context(|| format!("put_object {bucket}/{key}"))?;
        Ok(())
    }

    /// Download an object to local bytes.
    pub async fn get(&self, bucket: &str, key: &str) -> Result<Bytes> {
        let r = self
            .client
            .get_object()
            .bucket(bucket)
            .key(key)
            .send()
            .await
            .with_context(|| format!("get_object {bucket}/{key}"))?;
        let body = r.body.collect().await?.into_bytes();
        Ok(body)
    }

    /// Pre-signed GET URL for time-bounded access (used for NSFW gating).
    pub async fn presign_get(&self, bucket: &str, key: &str, ttl: Duration) -> Result<String> {
        let cfg = PresigningConfig::expires_in(ttl)?;
        let url = self
            .client
            .get_object()
            .bucket(bucket)
            .key(key)
            .presigned(cfg)
            .await?;
        Ok(url.uri().to_string())
    }

    /// Pre-signed PUT URL for resumable upload from the browser.
    pub async fn presign_put(
        &self,
        bucket: &str,
        key: &str,
        ttl: Duration,
        content_type: &str,
    ) -> Result<String> {
        let cfg = PresigningConfig::expires_in(ttl)?;
        let url = self
            .client
            .put_object()
            .bucket(bucket)
            .key(key)
            .content_type(content_type)
            .presigned(cfg)
            .await?;
        Ok(url.uri().to_string())
    }

    /// Public URL (when bucket allows anonymous read, e.g. media-derived).
    /// At T2+ this becomes the CDN URL instead of MinIO directly.
    pub fn public_url(&self, bucket: &str, key: &str) -> String {
        let base = std::env::var("MINIO_PUBLIC_BASE")
            .unwrap_or_else(|_| "http://localhost:9000".into());
        format!("{base}/{bucket}/{key}")
    }
}
