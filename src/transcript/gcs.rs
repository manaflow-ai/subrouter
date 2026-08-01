use std::collections::HashMap;
use std::fs;
use std::path::{Path, PathBuf};
use std::process::Stdio;
use std::sync::Arc;
use std::time::{Duration, SystemTime};

use anyhow::{Context, anyhow, bail};
use chrono::{DateTime, SecondsFormat, Utc};
use percent_encoding::{NON_ALPHANUMERIC, utf8_percent_encode};
use reqwest::{Client, StatusCode, Url};
use serde::Deserialize;
use sha2::{Digest, Sha256};
use tokio::process::Command;
use tokio::sync::Mutex;
use tracing::warn;

const DEFAULT_TIMEOUT: Duration = Duration::from_secs(30 * 60);
const QUIET_PERIOD: Duration = Duration::from_secs(2 * 60);
const TOKEN_REFRESH_PADDING: Duration = Duration::from_secs(60);
const METADATA_TOKEN_URL: &str =
    "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token";
const UPLOAD_BASE_URL: &str = "https://storage.googleapis.com/upload/storage/v1";
const STORAGE_BASE_URL: &str = "https://storage.googleapis.com/storage/v1";

#[derive(Clone, Debug)]
pub struct Config {
    pub source_dir: PathBuf,
    pub destination: String,
    pub interval: Duration,
    pub timeout: Duration,
    pub local_retention: Duration,
    pub max_local_bytes: u64,
    /// Optional gsutil-compatible command. Production defaults to the native
    /// GCS JSON API; this hook keeps controlled deployments and tests usable.
    pub command: Option<PathBuf>,
}

impl Default for Config {
    fn default() -> Self {
        Self {
            source_dir: PathBuf::new(),
            destination: String::new(),
            interval: Duration::ZERO,
            timeout: DEFAULT_TIMEOUT,
            local_retention: Duration::ZERO,
            max_local_bytes: 0,
            command: None,
        }
    }
}

struct Token {
    value: String,
    expires_at: SystemTime,
}

pub struct Syncer {
    source_dir: PathBuf,
    destination: String,
    bucket: String,
    prefix: String,
    interval: Duration,
    timeout: Duration,
    retention: Duration,
    max_bytes: u64,
    command: Option<PathBuf>,
    client: Client,
    token: Mutex<Option<Token>>,
    metadata_url: Url,
    upload_base: Url,
    storage_base: Url,
}

impl std::fmt::Debug for Syncer {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter
            .debug_struct("Syncer")
            .field("source_dir", &self.source_dir)
            .field("destination", &self.destination)
            .field("interval", &self.interval)
            .field("timeout", &self.timeout)
            .field("retention", &self.retention)
            .field("max_bytes", &self.max_bytes)
            .field("command", &self.command)
            .finish_non_exhaustive()
    }
}

impl Syncer {
    pub fn new(config: Config) -> anyhow::Result<Option<Arc<Self>>> {
        let source_dir = config.source_dir;
        let Some((destination, bucket, prefix)) = normalize_destination(&config.destination) else {
            if config.destination.trim().is_empty() || source_dir.as_os_str().is_empty() {
                return Ok(None);
            }
            bail!("transcript GCS destination must be a gs:// bucket URI");
        };
        if source_dir.as_os_str().is_empty() {
            return Ok(None);
        }
        let client = Client::builder()
            .connect_timeout(Duration::from_secs(10))
            .redirect(reqwest::redirect::Policy::none())
            .build()?;
        Ok(Some(Arc::new(Self {
            source_dir,
            destination,
            bucket,
            prefix,
            interval: config.interval,
            timeout: if config.timeout.is_zero() {
                DEFAULT_TIMEOUT
            } else {
                config.timeout
            },
            retention: config.local_retention,
            max_bytes: config.max_local_bytes,
            command: config.command,
            client,
            token: Mutex::new(None),
            metadata_url: Url::parse(METADATA_TOKEN_URL).expect("static metadata URL"),
            upload_base: Url::parse(UPLOAD_BASE_URL).expect("static GCS upload URL"),
            storage_base: Url::parse(STORAGE_BASE_URL).expect("static GCS storage URL"),
        })))
    }

    #[must_use]
    pub fn enabled(&self) -> bool {
        !self.source_dir.as_os_str().is_empty() && !self.destination.is_empty()
    }

    pub async fn run(self: Arc<Self>) {
        if !self.enabled() || self.interval.is_zero() {
            return;
        }
        loop {
            if let Err(error) = self.sync_once().await {
                warn!(destination = %self.destination, %error, "transcript GCS sync failed");
            }
            tokio::time::sleep(self.interval).await;
        }
    }

    pub async fn sync_once(&self) -> anyhow::Result<()> {
        if !self.enabled() || !self.source_dir.exists() {
            return Ok(());
        }
        tokio::time::timeout(self.timeout, self.sync_once_inner())
            .await
            .map_err(|_| {
                anyhow!(
                    "transcript GCS sync timed out after {}",
                    humantime::format_duration(self.timeout)
                )
            })?
    }

    async fn sync_once_inner(&self) -> anyhow::Result<()> {
        if let Err(error) = self.prune_local(SystemTime::now()).await {
            warn!(destination = %self.destination, %error, "transcript local prune skipped before sync");
        }
        if let Some(command) = &self.command {
            self.run_command(
                command,
                &["-m", "rsync", "-r"],
                &[self.source_dir.as_os_str(), self.destination.as_ref()],
            )
            .await?;
        } else {
            self.sync_native().await?;
        }
        self.prune_local(SystemTime::now()).await
    }

    async fn run_command(
        &self,
        command: &Path,
        fixed: &[&str],
        trailing: &[&std::ffi::OsStr],
    ) -> anyhow::Result<()> {
        let output = Command::new(command)
            .args(fixed)
            .args(trailing)
            .stdin(Stdio::null())
            .output()
            .await
            .with_context(|| format!("run {}", command.display()))?;
        if !output.status.success() {
            let detail = String::from_utf8_lossy(&output.stderr);
            bail!(
                "{} exited with {}: {}",
                command.display(),
                output.status,
                detail.trim()
            );
        }
        Ok(())
    }

    async fn sync_native(&self) -> anyhow::Result<()> {
        let (mut files, _) = local_files(&self.source_dir)?;
        files.sort_by_key(|file| (file.modified, file.size, file.path.clone()));
        let now = SystemTime::now();
        for file in files {
            if now.duration_since(file.modified).unwrap_or_default() < QUIET_PERIOD {
                continue;
            }
            self.upload_if_needed(&file).await?;
        }
        Ok(())
    }

    async fn upload_if_needed(&self, file: &LocalFile) -> anyhow::Result<()> {
        let object = self.object_name(&file.relative);
        if self.object_size(&object).await? == Some(file.size) {
            return Ok(());
        }
        let mut init = self.upload_base.clone();
        init.set_path(&format!("/upload/storage/v1/b/{}/o", encode(&self.bucket)));
        init.query_pairs_mut()
            .append_pair("uploadType", "resumable")
            .append_pair("name", &object);
        let response = self
            .authorized(self.client.post(init))
            .await?
            .header("x-upload-content-type", "application/octet-stream")
            .header("x-upload-content-length", file.size)
            .send()
            .await?;
        if !response.status().is_success() {
            return Err(response_error(response).await);
        }
        let location = response
            .headers()
            .get(http::header::LOCATION)
            .and_then(|value| value.to_str().ok())
            .map(str::to_owned)
            .ok_or_else(|| anyhow!("GCS resumable upload did not return a Location header"))?;
        let body = tokio::fs::read(&file.path).await?;
        let response = self
            .authorized(self.client.put(location))
            .await?
            .header(http::header::CONTENT_TYPE, "application/octet-stream")
            .header(http::header::CONTENT_LENGTH, body.len())
            .body(body)
            .send()
            .await?;
        if !response.status().is_success() {
            return Err(response_error(response).await);
        }
        Ok(())
    }

    async fn object_size(&self, object: &str) -> anyhow::Result<Option<u64>> {
        let mut url = self.storage_base.clone();
        url.set_path(&format!(
            "/storage/v1/b/{}/o/{}",
            encode(&self.bucket),
            encode(object)
        ));
        url.query_pairs_mut().append_pair("fields", "size");
        let response = self.authorized(self.client.get(url)).await?.send().await?;
        if response.status() == StatusCode::NOT_FOUND {
            return Ok(None);
        }
        if !response.status().is_success() {
            return Err(response_error(response).await);
        }
        #[derive(Deserialize)]
        struct ObjectSize {
            size: String,
        }
        let size = response.json::<ObjectSize>().await?.size.parse()?;
        Ok(Some(size))
    }

    async fn copy_object(&self, source: &str, destination: &str) -> anyhow::Result<bool> {
        if self.object_size(destination).await?.is_some() {
            return Ok(true);
        }
        let mut url = self.storage_base.clone();
        url.set_path(&format!(
            "/storage/v1/b/{}/o/{}/copyTo/b/{}/o/{}",
            encode(&self.bucket),
            encode(source),
            encode(&self.bucket),
            encode(destination)
        ));
        let response = self.authorized(self.client.post(url)).await?.send().await?;
        if response.status() == StatusCode::NOT_FOUND {
            return Ok(false);
        }
        if !response.status().is_success() {
            return Err(response_error(response).await);
        }
        Ok(true)
    }

    async fn authorized(
        &self,
        request: reqwest::RequestBuilder,
    ) -> anyhow::Result<reqwest::RequestBuilder> {
        let token = self.bearer_token().await?;
        Ok(request.bearer_auth(token))
    }

    async fn bearer_token(&self) -> anyhow::Result<String> {
        {
            let token = self.token.lock().await;
            if let Some(token) = token.as_ref()
                && token
                    .expires_at
                    .duration_since(SystemTime::now())
                    .unwrap_or_default()
                    > TOKEN_REFRESH_PADDING
            {
                return Ok(token.value.clone());
            }
        }
        let response = self
            .client
            .get(self.metadata_url.clone())
            .header("metadata-flavor", "Google")
            .send()
            .await?;
        if !response.status().is_success() {
            return Err(response_error(response).await);
        }
        #[derive(Deserialize)]
        struct MetadataToken {
            access_token: String,
            expires_in: u64,
        }
        let value = response.json::<MetadataToken>().await?;
        if value.access_token.is_empty() {
            bail!("metadata token response did not include an access token");
        }
        let token = Token {
            value: value.access_token.clone(),
            expires_at: SystemTime::now() + Duration::from_secs(value.expires_in),
        };
        *self.token.lock().await = Some(token);
        Ok(value.access_token)
    }

    async fn prune_local(&self, now: SystemTime) -> anyhow::Result<()> {
        if self.retention.is_zero() && self.max_bytes == 0 {
            return Ok(());
        }
        let (mut files, total) = local_files(&self.source_dir)?;
        let mut selected = HashMap::<PathBuf, LocalFile>::new();
        for file in &files {
            if !self.retention.is_zero()
                && now.duration_since(file.modified).unwrap_or_default() >= self.retention
            {
                selected.insert(file.path.clone(), file.clone());
            }
        }
        if self.max_bytes > 0 && total > self.max_bytes {
            files.sort_by_key(|file| (file.modified, file.path.clone()));
            let mut remaining = total;
            for file in files {
                if remaining <= self.max_bytes {
                    break;
                }
                remaining = remaining.saturating_sub(file.size);
                selected.insert(file.path.clone(), file);
            }
        }
        let mut selected: Vec<_> = selected.into_values().collect();
        selected.sort_by_key(|file| (file.modified, file.path.clone()));
        for file in selected {
            self.archive_and_remove(&file).await?;
        }
        prune_empty_dirs(&self.source_dir)?;
        Ok(())
    }

    async fn archive_and_remove(&self, file: &LocalFile) -> anyhow::Result<()> {
        if !unchanged(file)? {
            return Ok(());
        }
        let source_object = self.object_name(&file.relative);
        let archive_object = self.object_name(&format!(
            "_archive/{}/{}",
            file.relative,
            archive_filename(file)?
        ));
        if let Some(command) = &self.command {
            let source_uri = format!("{}{}", self.destination, file.relative);
            let archive_uri = format!("gs://{}/{}", self.bucket, archive_object);
            self.run_command(
                command,
                &["cp", "-n"],
                &[source_uri.as_ref(), archive_uri.as_ref()],
            )
            .await?;
        } else if !self.copy_object(&source_object, &archive_object).await? {
            return Ok(());
        }
        if unchanged(file)? {
            match tokio::fs::remove_file(&file.path).await {
                Ok(()) => {}
                Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
                Err(error) => return Err(error.into()),
            }
        }
        Ok(())
    }

    fn object_name(&self, relative: &str) -> String {
        let relative = relative.trim_start_matches('/');
        if self.prefix.is_empty() {
            relative.into()
        } else {
            format!("{}/{relative}", self.prefix)
        }
    }
}

#[derive(Clone, Debug)]
struct LocalFile {
    path: PathBuf,
    relative: String,
    size: u64,
    modified: SystemTime,
}

fn local_files(root: &Path) -> anyhow::Result<(Vec<LocalFile>, u64)> {
    let mut output = Vec::new();
    collect_files(root, root, &mut output)?;
    let total = output.iter().map(|file| file.size).sum();
    Ok((output, total))
}

fn collect_files(root: &Path, directory: &Path, output: &mut Vec<LocalFile>) -> anyhow::Result<()> {
    let entries = match fs::read_dir(directory) {
        Ok(value) => value,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(()),
        Err(error) => return Err(error.into()),
    };
    for entry in entries {
        let entry = entry?;
        let kind = entry.file_type()?;
        if kind.is_symlink() {
            continue;
        }
        if kind.is_dir() {
            collect_files(root, &entry.path(), output)?;
        } else if kind.is_file()
            && entry
                .path()
                .extension()
                .is_some_and(|value| value == "jsonl")
        {
            let metadata = entry.metadata()?;
            let relative = entry
                .path()
                .strip_prefix(root)?
                .to_string_lossy()
                .replace('\\', "/");
            output.push(LocalFile {
                path: entry.path(),
                relative,
                size: metadata.len(),
                modified: metadata.modified()?,
            });
        }
    }
    Ok(())
}

fn unchanged(file: &LocalFile) -> anyhow::Result<bool> {
    match fs::metadata(&file.path) {
        Ok(metadata) => Ok(metadata.len() == file.size && metadata.modified()? == file.modified),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(false),
        Err(error) => Err(error.into()),
    }
}

fn archive_filename(file: &LocalFile) -> anyhow::Result<String> {
    let body = fs::read(&file.path)?;
    let hash = hex::encode(Sha256::digest(body));
    let modified: DateTime<Utc> = file.modified.into();
    Ok(format!(
        "{}-{}-{}.jsonl",
        modified
            .to_rfc3339_opts(SecondsFormat::Nanos, true)
            .replace([':', '-'], ""),
        file.size,
        &hash[..16]
    ))
}

fn prune_empty_dirs(root: &Path) -> anyhow::Result<()> {
    let mut directories = Vec::new();
    collect_directories(root, root, &mut directories)?;
    directories.sort_by_key(|path| std::cmp::Reverse(path.components().count()));
    for directory in directories {
        let _ = fs::remove_dir(directory);
    }
    Ok(())
}

fn collect_directories(
    root: &Path,
    directory: &Path,
    output: &mut Vec<PathBuf>,
) -> anyhow::Result<()> {
    let entries = match fs::read_dir(directory) {
        Ok(value) => value,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(()),
        Err(error) => return Err(error.into()),
    };
    for entry in entries {
        let entry = entry?;
        if entry.file_type()?.is_dir() {
            collect_directories(root, &entry.path(), output)?;
            if entry.path() != root {
                output.push(entry.path());
            }
        }
    }
    Ok(())
}

fn normalize_destination(value: &str) -> Option<(String, String, String)> {
    let rest = value.trim().strip_prefix("gs://")?;
    let (bucket, prefix) = rest.split_once('/').unwrap_or((rest, ""));
    let bucket = bucket.trim();
    if bucket.is_empty() {
        return None;
    }
    let prefix = prefix.trim().trim_matches('/');
    let destination = if prefix.is_empty() {
        format!("gs://{bucket}/")
    } else {
        format!("gs://{bucket}/{prefix}/")
    };
    Some((destination, bucket.into(), prefix.into()))
}

fn encode(value: &str) -> String {
    utf8_percent_encode(value, NON_ALPHANUMERIC).to_string()
}

async fn response_error(response: reqwest::Response) -> anyhow::Error {
    let status = response.status();
    let body = response.bytes().await.unwrap_or_default();
    let body = &body[..body.len().min(4096)];
    anyhow!("GCS {status}: {}", String::from_utf8_lossy(body).trim())
}

pub fn parse_byte_size(value: &str) -> Result<u64, String> {
    let value = value.trim();
    if value.is_empty() || value == "0" {
        return Ok(0);
    }
    let split = value
        .find(|character: char| !character.is_ascii_digit() && character != '.')
        .unwrap_or(value.len());
    let number: f64 = value[..split]
        .parse()
        .map_err(|_| "invalid byte size".to_owned())?;
    let suffix = value[split..].trim().to_ascii_lowercase();
    let factor = match suffix.as_str() {
        "" | "b" => 1.0,
        "kib" => 1024.0,
        "mib" => 1024.0 * 1024.0,
        "gib" => 1024.0 * 1024.0 * 1024.0,
        "tib" => 1024.0 * 1024.0 * 1024.0 * 1024.0,
        "kb" => 1000.0,
        "mb" => 1_000_000.0,
        "gb" => 1_000_000_000.0,
        "tb" => 1_000_000_000_000.0,
        _ => return Err(format!("unknown byte-size suffix {suffix:?}")),
    };
    let bytes = number * factor;
    if !bytes.is_finite() || bytes < 0.0 || bytes > u64::MAX as f64 {
        return Err("byte size is out of range".into());
    }
    Ok(bytes.round() as u64)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn destination_and_sizes_are_strict() {
        assert_eq!(
            normalize_destination(" gs://bucket/prefix/ "),
            Some((
                "gs://bucket/prefix/".into(),
                "bucket".into(),
                "prefix".into()
            ))
        );
        assert_eq!(normalize_destination("https://example.com"), None);
        assert_eq!(parse_byte_size("2GiB").unwrap(), 2 * 1024 * 1024 * 1024);
        assert_eq!(parse_byte_size("1.5MiB").unwrap(), 1_572_864);
    }

    #[tokio::test]
    async fn command_mode_uses_rsync_shape() {
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt as _;
            let root = tempfile::tempdir().unwrap();
            let source = root.path().join("transcripts");
            fs::create_dir_all(&source).unwrap();
            let log = root.path().join("args");
            let command = root.path().join("gsutil");
            fs::write(
                &command,
                format!("#!/bin/sh\nprintf '%s\\n' \"$*\" > '{}'\n", log.display()),
            )
            .unwrap();
            fs::set_permissions(&command, fs::Permissions::from_mode(0o700)).unwrap();
            let syncer = Syncer::new(Config {
                source_dir: source.clone(),
                destination: "gs://bucket/prefix".into(),
                timeout: Duration::from_secs(2),
                command: Some(command),
                ..Config::default()
            })
            .unwrap()
            .unwrap();
            syncer.sync_once().await.unwrap();
            assert_eq!(
                fs::read_to_string(log).unwrap(),
                format!("-m rsync -r {} gs://bucket/prefix/\n", source.display())
            );
        }
    }
}
