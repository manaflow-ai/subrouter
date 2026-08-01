use std::collections::BTreeMap;
use std::fs;
use std::io;
use std::path::{Path, PathBuf};
use std::process::Stdio;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use anyhow::{anyhow, bail};
use chrono::{DateTime, SecondsFormat, Utc};
use regex::Regex;
use reqwest::header::HeaderMap;
use reqwest::{Client, StatusCode};
use serde::{Deserialize, Serialize};
use serde_json::{Map, Value, json};
use sha2::{Digest, Sha256};
use tokio::process::Command;
use tracing::warn;

use crate::account::{Account, AuthMode, Provider};
use crate::accounts::UsageWindow;
use crate::storepath;

pub const FABLE_MODEL: &str = "claude-fable-5";
pub const FABLE_WINDOW_NAME: &str = "oauth-apps-weekly";
pub const FABLE_FEATURE: &str = "claude-fable";
pub const OPUS_FEATURE: &str = "claude-opus";
pub const SONNET_FEATURE: &str = "claude-sonnet";
const USAGE_URL: &str = "https://api.anthropic.com/api/oauth/usage";
const MESSAGES_URL: &str = "https://api.anthropic.com/v1/messages";
const OAUTH_TOKEN_URL: &str = "https://platform.claude.com/v1/oauth/token";
const OAUTH_CLIENT_ID: &str = "9d1c250a-e61b-44d9-88ed-5944d1962f5e";
const OAUTH_BETA_HEADER: &str = "oauth-2025-04-20";
const RESERVED_NAMES: &[&str] = &[
    "add", "login", "list", "ls", "switch", "use", "remove", "rm", "env", "status", "run", "help",
];

#[derive(Clone, Debug)]
pub struct Store {
    pub dir: PathBuf,
}

impl Default for Store {
    fn default() -> Self {
        Self {
            dir: storepath::codex_dir(),
        }
    }
}

#[derive(Clone, Debug, Default, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct Profile {
    pub name: String,
    pub created_at: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub last_used: String,
    pub dir: String,
}

#[derive(Default, Deserialize, Serialize)]
struct ProfilesFile {
    #[serde(default, skip_serializing_if = "String::is_empty")]
    active: String,
    #[serde(default)]
    profiles: BTreeMap<String, Profile>,
}

#[derive(Clone, Debug, Default, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct AuthStatus {
    pub logged_in: bool,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub auth_method: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub api_provider: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub email: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub org_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub org_name: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub subscription_type: String,
}

#[derive(Clone, Default, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct CredentialInfo {
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub access_token: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub refresh_token: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub subscription_type: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub rate_limit_tier: String,
    #[serde(default, skip_serializing_if = "is_zero_i64")]
    pub expires_at: i64,
}

const fn is_zero_i64(value: &i64) -> bool {
    *value == 0
}

#[derive(Clone, Debug, Default, Deserialize, PartialEq, Serialize)]
pub struct RateLimit {
    pub utilization: Option<f64>,
    #[serde(default)]
    pub resets_at: String,
}

#[derive(Clone, Debug, Default, Deserialize, PartialEq, Serialize)]
pub struct ExtraUsage {
    pub is_enabled: bool,
    pub monthly_limit: Option<f64>,
    pub used_credits: Option<f64>,
    pub utilization: Option<f64>,
}

#[derive(Clone, Debug, Default, Deserialize, PartialEq, Serialize)]
pub struct UsageResponse {
    pub five_hour: Option<RateLimit>,
    pub seven_day: Option<RateLimit>,
    pub seven_day_opus: Option<RateLimit>,
    pub seven_day_sonnet: Option<RateLimit>,
    pub seven_day_oauth_apps: Option<RateLimit>,
    pub extra_usage: Option<ExtraUsage>,
}

impl Store {
    #[must_use]
    pub fn new(dir: impl Into<PathBuf>) -> Self {
        Self { dir: dir.into() }
    }

    #[must_use]
    pub fn profiles_path(&self) -> PathBuf {
        self.dir.join("claude.json")
    }

    #[must_use]
    pub fn instances_dir(&self) -> PathBuf {
        self.dir.join("claude")
    }

    #[must_use]
    pub fn instance_path(&self, name: &str) -> PathBuf {
        let profiles = self.read_profiles();
        let dir = profiles
            .profiles
            .get(name)
            .map(|profile| profile.dir.as_str())
            .filter(|dir| !dir.is_empty())
            .map_or_else(|| sanitize_name(name), str::to_owned);
        self.instances_dir().join(dir)
    }

    #[must_use]
    pub fn claude_config_dir(&self, name: &str) -> PathBuf {
        self.preferred_instance_path(&self.instance_path(name))
    }

    #[must_use]
    pub fn preferred_instance_path(&self, instance_path: &Path) -> PathBuf {
        if self.dir.file_name().is_none_or(|name| name != "codex")
            || self
                .dir
                .parent()
                .and_then(Path::file_name)
                .is_none_or(|name| name != ".subrouter")
        {
            return instance_path.into();
        }
        let Ok(relative) = instance_path.strip_prefix(&self.dir) else {
            return instance_path.into();
        };
        if relative.as_os_str().is_empty() {
            return instance_path.into();
        }
        let Some(home) = self.dir.parent().and_then(Path::parent) else {
            return instance_path.into();
        };
        let candidate = home.join(".codex-accounts").join(relative);
        if candidate.is_dir() {
            candidate
        } else {
            instance_path.into()
        }
    }

    #[must_use]
    pub fn list_profiles(&self) -> Vec<Profile> {
        self.read_profiles().profiles.into_values().collect()
    }

    pub async fn list_accounts(&self) -> anyhow::Result<Vec<Account>> {
        let mut output = Vec::new();
        for profile in self.list_profiles() {
            let config_dir = self.claude_config_dir(&profile.name);
            match self.read_credential(&config_dir).await {
                Ok(Some(credential)) => {
                    if let Some(account) = profile_account(&profile, &config_dir, &credential) {
                        output.push(account);
                    }
                }
                Ok(None) => {}
                Err(error) => {
                    warn!(profile = %profile.name, %error, "Claude profile credential unreadable, skipping")
                }
            }
        }
        output.sort_by(|left, right| left.id.cmp(&right.id));
        Ok(output)
    }

    #[must_use]
    pub fn find_profile(&self, name: &str) -> Option<Profile> {
        self.read_profiles().profiles.get(name).cloned()
    }

    pub fn match_profile(&self, selector: &str) -> anyhow::Result<Option<Profile>> {
        if let Some(profile) = self.find_profile(selector) {
            return Ok(Some(profile));
        }
        let selector = selector.to_ascii_lowercase();
        let matches: Vec<_> = self
            .list_profiles()
            .into_iter()
            .filter(|profile| profile.name.to_ascii_lowercase().contains(&selector))
            .collect();
        match matches.as_slice() {
            [] => Ok(None),
            [profile] => Ok(Some(profile.clone())),
            _ => bail!(
                "multiple profiles match {:?}: {}",
                selector,
                matches
                    .iter()
                    .map(|profile| profile.name.as_str())
                    .collect::<Vec<_>>()
                    .join(", ")
            ),
        }
    }

    #[must_use]
    pub fn active_profile(&self) -> String {
        self.read_profiles().active
    }

    pub fn set_active_profile(&self, name: &str) -> anyhow::Result<()> {
        let mut data = self.read_profiles();
        let profile = data
            .profiles
            .get_mut(name)
            .ok_or_else(|| anyhow!("profile {name:?} not found"))?;
        profile.last_used = Utc::now().to_rfc3339_opts(SecondsFormat::Secs, true);
        data.active = name.into();
        self.write_profiles(&data)
    }

    pub fn create_profile(&self, name: &str) -> anyhow::Result<PathBuf> {
        validate_profile_name(name)?;
        if self.find_profile(name).is_some() {
            bail!("profile {name:?} already exists");
        }
        let dir = sanitize_name(name);
        let path = self.instances_dir().join(&dir);
        self.init_instance_dir(&path)?;
        let mut data = self.read_profiles();
        data.profiles.insert(
            name.into(),
            Profile {
                name: name.into(),
                created_at: Utc::now().to_rfc3339_opts(SecondsFormat::Secs, true),
                last_used: String::new(),
                dir,
            },
        );
        if data.active.is_empty() {
            data.active = name.into();
        }
        self.write_profiles(&data)?;
        Ok(path)
    }

    pub fn create_temp_instance(&self) -> anyhow::Result<(PathBuf, String)> {
        let millis = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap_or_default()
            .as_millis();
        let dir = format!("_p{millis}");
        let path = self.instances_dir().join(&dir);
        self.init_instance_dir(&path)?;
        Ok((path, dir))
    }

    pub fn register_profile(&self, name: &str, dir: &str) -> anyhow::Result<()> {
        validate_profile_name_allow_email(name)?;
        let mut data = self.read_profiles();
        let now = Utc::now().to_rfc3339_opts(SecondsFormat::Secs, true);
        data.profiles
            .entry(name.into())
            .and_modify(|profile| {
                profile.last_used.clone_from(&now);
                profile.dir = dir.into();
            })
            .or_insert_with(|| Profile {
                name: name.into(),
                created_at: now,
                last_used: String::new(),
                dir: dir.into(),
            });
        if data.active.is_empty() {
            data.active = name.into();
        }
        self.write_profiles(&data)
    }

    pub fn remove_profile(&self, name: &str) -> anyhow::Result<bool> {
        let mut data = self.read_profiles();
        let Some(profile) = data.profiles.remove(name) else {
            return Ok(false);
        };
        if data.active == name {
            data.active = data.profiles.keys().next().cloned().unwrap_or_default();
        }
        self.write_profiles(&data)?;
        match fs::remove_dir_all(self.instances_dir().join(if profile.dir.is_empty() {
            sanitize_name(name)
        } else {
            profile.dir
        })) {
            Ok(()) => {}
            Err(error) if error.kind() == io::ErrorKind::NotFound => {}
            Err(error) => return Err(error.into()),
        }
        Ok(true)
    }

    pub fn cleanup_instance(&self, dir: &str) -> io::Result<()> {
        if dir.is_empty() {
            Ok(())
        } else {
            fs::remove_dir_all(self.instances_dir().join(dir))
        }
    }

    pub async fn read_credential(
        &self,
        instance_path: &Path,
    ) -> anyhow::Result<Option<CredentialInfo>> {
        let file_path = instance_path.join(".credentials.json");
        if let Ok(body) = fs::read(&file_path) {
            return parse_credential_payload(&body).map(Some);
        }
        if !cfg!(target_os = "macos") {
            return Ok(None);
        }
        let username = std::env::var("USER").unwrap_or_default();
        let service = format!("Claude Code-credentials-{}", keychain_hash(instance_path));
        let output = tokio::time::timeout(
            Duration::from_secs(5),
            Command::new("security")
                .args([
                    "find-generic-password",
                    "-s",
                    &service,
                    "-a",
                    &username,
                    "-w",
                ])
                .stderr(Stdio::null())
                .output(),
        )
        .await;
        let Ok(Ok(output)) = output else {
            return Ok(None);
        };
        if !output.status.success() || output.stdout.iter().all(u8::is_ascii_whitespace) {
            return Ok(None);
        }
        parse_credential_payload(&output.stdout).map(Some)
    }

    pub async fn write_credential(
        &self,
        instance_path: &Path,
        credential: &CredentialInfo,
    ) -> anyhow::Result<()> {
        let body = credential_payload(credential)?;
        let file_path = instance_path.join(".credentials.json");
        if file_path.exists() {
            super::opencode::write_atomic_private(&file_path, &body)?;
            return Ok(());
        }
        if !cfg!(target_os = "macos") {
            bail!(
                "Claude credential persistence is only supported for .credentials.json files on this platform"
            );
        }
        let username = std::env::var("USER").unwrap_or_default();
        let service = format!("Claude Code-credentials-{}", keychain_hash(instance_path));
        let status = tokio::time::timeout(
            Duration::from_secs(5),
            Command::new("security")
                .args([
                    "add-generic-password",
                    "-U",
                    "-s",
                    &service,
                    "-a",
                    &username,
                    "-w",
                    &String::from_utf8_lossy(&body),
                ])
                .status(),
        )
        .await
        .map_err(|_| anyhow!("write Claude credential to keychain timed out"))??;
        if !status.success() {
            bail!("write Claude credential to keychain failed");
        }
        Ok(())
    }

    pub async fn refresh_credential_if_expired(
        &self,
        client: &Client,
        profile: &Profile,
    ) -> anyhow::Result<(Account, bool)> {
        let config_dir = self.claude_config_dir(&profile.name);
        let mut credential = self
            .read_credential(&config_dir)
            .await?
            .filter(|credential| !credential.access_token.is_empty())
            .ok_or_else(|| anyhow!("Claude profile {:?} has no access token", profile.name))?;
        let mut refreshed = false;
        if !credential.refresh_token.is_empty()
            && credential_expired(&credential, Duration::from_secs(60))
        {
            credential = refresh_credential(client, &credential).await?;
            self.write_credential(&config_dir, &credential).await?;
            refreshed = true;
        }
        let account = profile_account(profile, &config_dir, &credential)
            .ok_or_else(|| anyhow!("Claude profile {:?} has no usable credential", profile.name))?;
        Ok((account, refreshed))
    }

    pub async fn refresh_account_if_expired(
        &self,
        client: &Client,
        account: &Account,
    ) -> anyhow::Result<(Account, bool)> {
        let profile = self
            .find_profile(&account.id)
            .ok_or_else(|| anyhow!("Claude profile {:?} not found", account.id))?;
        self.refresh_credential_if_expired(client, &profile).await
    }

    pub fn migrate_session(
        &self,
        target_profile: &str,
        session_id: &str,
    ) -> anyhow::Result<Option<String>> {
        validate_session_id(session_id)?;
        let target = self.claude_config_dir(target_profile);
        if !session_transcripts(&target, session_id)?.is_empty() {
            return Ok(None);
        }
        for profile in self.list_profiles() {
            if profile.name == target_profile {
                continue;
            }
            let source = self.claude_config_dir(&profile.name);
            let transcripts = session_transcripts(&source, session_id)?;
            if transcripts.is_empty() {
                continue;
            }
            for transcript in transcripts {
                let project = transcript
                    .parent()
                    .and_then(Path::file_name)
                    .unwrap_or_default();
                let destination = target.join("projects").join(project);
                fs::create_dir_all(&destination)?;
                copy_path(
                    &transcript,
                    &destination.join(transcript.file_name().unwrap_or_default()),
                )?;
                let topic = transcript.with_extension("");
                if topic.is_dir() {
                    copy_path(&topic, &destination.join(session_id))?;
                }
            }
            for subdir in ["file-history", "session-env"] {
                let path = source.join(subdir).join(session_id);
                if path.exists() {
                    fs::create_dir_all(target.join(subdir))?;
                    copy_path(&path, &target.join(subdir).join(session_id))?;
                }
            }
            return Ok(Some(profile.name));
        }
        Ok(None)
    }

    fn read_profiles(&self) -> ProfilesFile {
        fs::read(self.profiles_path())
            .ok()
            .and_then(|body| serde_json::from_slice(&body).ok())
            .unwrap_or_default()
    }

    fn write_profiles(&self, data: &ProfilesFile) -> anyhow::Result<()> {
        let mut body = serde_json::to_vec_pretty(data)?;
        body.push(b'\n');
        super::opencode::write_atomic_private(&self.profiles_path(), &body)?;
        Ok(())
    }

    fn init_instance_dir(&self, instance_path: &Path) -> anyhow::Result<()> {
        fs::create_dir_all(instance_path)?;
        for name in [
            "session-env",
            "todos",
            "logs",
            "file-history",
            "shell-snapshots",
            "debug",
            ".anthropic",
        ] {
            fs::create_dir_all(instance_path.join(name))?;
        }
        self.sync_mcp_servers(instance_path)
    }

    fn sync_mcp_servers(&self, instance_path: &Path) -> anyhow::Result<()> {
        let Some(home) = super::opencode::home_dir() else {
            return Ok(());
        };
        let Ok(body) = fs::read(home.join(".claude.json")) else {
            return Ok(());
        };
        let Ok(global) = serde_json::from_slice::<Map<String, Value>>(&body) else {
            return Ok(());
        };
        let Some(global_mcp) = global
            .get("mcpServers")
            .and_then(Value::as_object)
            .filter(|servers| !servers.is_empty())
        else {
            return Ok(());
        };
        let path = instance_path.join(".claude.json");
        let mut content: Map<String, Value> = fs::read(&path)
            .ok()
            .and_then(|body| serde_json::from_slice(&body).ok())
            .unwrap_or_default();
        let mut merged = global_mcp.clone();
        if let Some(existing) = content.get("mcpServers").and_then(Value::as_object) {
            merged.extend(existing.clone());
        }
        content.insert("mcpServers".into(), Value::Object(merged));
        let mut body = serde_json::to_vec_pretty(&content)?;
        body.push(b'\n');
        super::opencode::write_atomic_private(&path, &body)?;
        Ok(())
    }
}

pub fn validate_profile_name(name: &str) -> anyhow::Result<()> {
    if name.is_empty() {
        bail!("profile name is required");
    }
    if RESERVED_NAMES.contains(&name.to_ascii_lowercase().as_str()) {
        bail!("{name:?} is a reserved command name");
    }
    if !name.as_bytes().first().is_some_and(u8::is_ascii_alphabetic)
        || !name
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_'))
    {
        bail!("invalid name. Use letters, numbers, dash, underscore. Must start with a letter");
    }
    Ok(())
}

pub fn validate_profile_name_allow_email(name: &str) -> anyhow::Result<()> {
    if name.contains('@') {
        if name.starts_with('@') || name.ends_with('@') {
            bail!("invalid email profile name");
        }
        Ok(())
    } else {
        validate_profile_name(name)
    }
}

fn sanitize_name(name: &str) -> String {
    name.chars()
        .map(|character| {
            if character.is_ascii_alphanumeric() || matches!(character, '_' | '-') {
                character.to_ascii_lowercase()
            } else {
                '-'
            }
        })
        .collect()
}

#[must_use]
pub fn detect_cli() -> Option<PathBuf> {
    let path = std::env::var_os("PATH")?;
    std::env::split_paths(&path)
        .map(|directory| {
            directory.join(if cfg!(windows) {
                "claude.exe"
            } else {
                "claude"
            })
        })
        .find(|candidate| candidate.is_file())
}

pub async fn auth_status_for_path(claude_path: &Path, instance_path: &Path) -> Option<AuthStatus> {
    if !claude_path.is_file() || !instance_path.exists() {
        return None;
    }
    let output = tokio::time::timeout(
        Duration::from_secs(10),
        Command::new(claude_path)
            .args(["auth", "status"])
            .env("CLAUDE_CONFIG_DIR", instance_path)
            .output(),
    )
    .await
    .ok()?
    .ok()?;
    if !output.status.success() {
        return None;
    }
    serde_json::from_slice(&output.stdout).ok()
}

fn parse_credential_payload(body: &[u8]) -> anyhow::Result<CredentialInfo> {
    #[derive(Deserialize)]
    struct Envelope {
        #[serde(rename = "claudeAiOauth")]
        oauth: Option<CredentialInfo>,
    }
    serde_json::from_slice::<Envelope>(body)?
        .oauth
        .ok_or_else(|| anyhow!("Claude credential payload is missing claudeAiOauth"))
}

fn credential_payload(credential: &CredentialInfo) -> anyhow::Result<Vec<u8>> {
    let mut body = serde_json::to_vec_pretty(&json!({"claudeAiOauth": credential}))?;
    body.push(b'\n');
    Ok(body)
}

fn credential_expired(credential: &CredentialInfo, skew: Duration) -> bool {
    if credential.expires_at <= 0 {
        return false;
    }
    let now = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_millis() as i64;
    credential.expires_at - now <= skew.as_millis() as i64
}

pub async fn refresh_credential(
    client: &Client,
    credential: &CredentialInfo,
) -> anyhow::Result<CredentialInfo> {
    refresh_credential_at(client, credential, OAUTH_TOKEN_URL).await
}

async fn refresh_credential_at(
    client: &Client,
    credential: &CredentialInfo,
    endpoint: &str,
) -> anyhow::Result<CredentialInfo> {
    if credential.refresh_token.is_empty() {
        return Ok(credential.clone());
    }
    let response = client
        .post(endpoint)
        .header(reqwest::header::CONTENT_TYPE, "application/json")
        .header("anthropic-beta", OAUTH_BETA_HEADER)
        .json(&json!({"client_id": OAUTH_CLIENT_ID, "grant_type": "refresh_token", "refresh_token": credential.refresh_token}))
        .send()
        .await?;
    if !response.status().is_success() {
        let status = response.status();
        let body = response.text().await.unwrap_or_default();
        bail!("Claude OAuth refresh failed: {status}: {}", body.trim());
    }
    #[derive(Deserialize)]
    struct RefreshResponse {
        access_token: String,
        #[serde(default)]
        refresh_token: String,
        #[serde(default)]
        expires_in: i64,
    }
    let response: RefreshResponse = response.json().await?;
    if response.access_token.is_empty() {
        bail!("Claude OAuth refresh response missing access_token");
    }
    let mut output = credential.clone();
    output.access_token = response.access_token;
    if !response.refresh_token.is_empty() {
        output.refresh_token = response.refresh_token;
    }
    if response.expires_in > 0 {
        output.expires_at = (SystemTime::now() + Duration::from_secs(response.expires_in as u64))
            .duration_since(UNIX_EPOCH)
            .unwrap_or_default()
            .as_millis() as i64;
    }
    Ok(output)
}

fn profile_account(
    profile: &Profile,
    config_dir: &Path,
    credential: &CredentialInfo,
) -> Option<Account> {
    if credential.access_token.is_empty() {
        return None;
    }
    Some(Account {
        id: profile.name.clone(),
        provider: Provider::Claude,
        auth_mode: AuthMode::Oauth,
        label: profile.name.clone(),
        email: if profile.name.contains('@') {
            profile.name.clone()
        } else {
            Default::default()
        },
        added_at: DateTime::parse_from_rfc3339(&profile.created_at)
            .ok()
            .map(|value| value.with_timezone(&Utc)),
        token: credential.access_token.clone(),
        source: config_dir.to_string_lossy().into(),
        ..Account::default()
    })
}

fn keychain_hash(instance_path: &Path) -> String {
    hex::encode(Sha256::digest(instance_path.to_string_lossy().as_bytes()))[..8].into()
}

pub async fn fetch_usage(
    client: &Client,
    access_token: &str,
) -> anyhow::Result<Option<UsageResponse>> {
    if access_token.is_empty() {
        return Ok(None);
    }
    let response = client
        .get(USAGE_URL)
        .bearer_auth(access_token)
        .header("anthropic-beta", OAUTH_BETA_HEADER)
        .header(reqwest::header::CONTENT_TYPE, "application/json")
        .send()
        .await?;
    if !response.status().is_success() {
        bail!("usage fetch failed: {}", response.status());
    }
    Ok(Some(response.json().await?))
}

pub async fn fetch_fable_usage_windows(
    client: &Client,
    access_token: &str,
) -> anyhow::Result<Vec<UsageWindow>> {
    fetch_fable_usage_windows_at(client, access_token, MESSAGES_URL).await
}

async fn fetch_fable_usage_windows_at(
    client: &Client,
    access_token: &str,
    endpoint: &str,
) -> anyhow::Result<Vec<UsageWindow>> {
    if access_token.is_empty() {
        return Ok(Vec::new());
    }
    let response = client
        .post(endpoint)
        .bearer_auth(access_token)
        .header("anthropic-beta", format!("claude-code-20250219,{OAUTH_BETA_HEADER}"))
        .header("anthropic-version", "2023-06-01")
        .header(reqwest::header::CONTENT_TYPE, "application/json")
        .header(reqwest::header::USER_AGENT, "claude-cli/2.1.199 (external, cli)")
        .header("x-app", "cli")
        .json(&json!({
            "model": FABLE_MODEL,
            "max_tokens": 1,
            "system": [{"type": "text", "text": "You are Claude Code, Anthropic's official CLI for Claude."}],
            "messages": [{"role": "user", "content": "."}],
        }))
        .send()
        .await?;
    let status = response.status();
    let windows = usage_windows_from_fable_headers(response.headers(), Utc::now());
    if !windows.is_empty() {
        return Ok(windows);
    }
    if status == StatusCode::UNAUTHORIZED {
        bail!("fable probe failed: {status}");
    }
    Ok(Vec::new())
}

#[must_use]
pub fn usage_windows_from_fable_headers(
    headers: &HeaderMap,
    now: DateTime<Utc>,
) -> Vec<UsageWindow> {
    let mut windows = Vec::new();
    for (prefix, name, seconds, feature) in [
        ("5h", "5h", 5 * 60 * 60, ""),
        ("7d", "7d", 7 * 24 * 60 * 60, ""),
        ("7d_oi", FABLE_WINDOW_NAME, 7 * 24 * 60 * 60, FABLE_FEATURE),
    ] {
        let get = |suffix: &str| {
            headers
                .get(format!("anthropic-ratelimit-unified-{prefix}-{suffix}"))
                .and_then(|value| value.to_str().ok())
                .map(str::trim)
                .unwrap_or_default()
        };
        let status = get("status").to_ascii_lowercase();
        let utilization = get("utilization");
        let reset = get("reset");
        if status.is_empty() && utilization.is_empty() && reset.is_empty() {
            continue;
        }
        let mut used = utilization.parse::<f64>().unwrap_or_default() * 100.0;
        if status == "rejected" && used < 100.0 {
            used = 100.0;
        }
        let reset_after_seconds = reset
            .parse::<i64>()
            .ok()
            .map_or(0, |epoch| (epoch - now.timestamp()).max(0));
        windows.push(UsageWindow {
            name: name.into(),
            used_percent: used,
            limit_window_seconds: seconds,
            reset_after_seconds,
            feature: feature.into(),
        });
    }
    windows
}

#[must_use]
pub fn resume_session_id(arguments: &[String]) -> Option<String> {
    let expression = Regex::new(
        r"^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$",
    )
    .unwrap();
    for (index, argument) in arguments.iter().enumerate() {
        if matches!(argument.as_str(), "--resume" | "-r")
            && let Some(candidate) = arguments
                .get(index + 1)
                .filter(|candidate| expression.is_match(candidate))
        {
            return Some(candidate.clone());
        }
        for prefix in ["--resume=", "-r="] {
            if let Some(candidate) = argument
                .strip_prefix(prefix)
                .filter(|candidate| expression.is_match(candidate))
            {
                return Some(candidate.into());
            }
        }
    }
    None
}

fn validate_session_id(session_id: &str) -> anyhow::Result<()> {
    let expression = Regex::new(
        r"^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$",
    )
    .unwrap();
    if expression.is_match(session_id) {
        Ok(())
    } else {
        bail!("invalid session id {session_id:?}")
    }
}

fn session_transcripts(config_dir: &Path, session_id: &str) -> io::Result<Vec<PathBuf>> {
    let projects = config_dir.join("projects");
    let entries = match fs::read_dir(projects) {
        Ok(entries) => entries,
        Err(error) if error.kind() == io::ErrorKind::NotFound => return Ok(Vec::new()),
        Err(error) => return Err(error),
    };
    let mut output = Vec::new();
    for entry in entries {
        let path = entry?.path().join(format!("{session_id}.jsonl"));
        if path.is_file() {
            output.push(path);
        }
    }
    Ok(output)
}

fn copy_path(source: &Path, destination: &Path) -> io::Result<()> {
    if source.is_dir() {
        fs::create_dir_all(destination)?;
        for entry in fs::read_dir(source)? {
            let entry = entry?;
            copy_path(&entry.path(), &destination.join(entry.file_name()))?;
        }
        return Ok(());
    }
    if let Some(parent) = destination.parent() {
        fs::create_dir_all(parent)?;
    }
    fs::copy(source, destination)?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        fs::set_permissions(destination, fs::Permissions::from_mode(0o600))?;
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn creates_switches_and_removes_profiles() {
        let temp = tempfile::tempdir().unwrap();
        let store = Store::new(temp.path());
        let path = store.create_profile("work").unwrap();
        assert!(path.join("session-env").is_dir());
        store.create_profile("personal").unwrap();
        store.set_active_profile("personal").unwrap();
        assert_eq!(store.active_profile(), "personal");
        assert!(store.remove_profile("work").unwrap());
        assert!(store.find_profile("work").is_none());
    }

    #[test]
    fn parses_resume_session_forms() {
        let id = "12345678-1234-1234-1234-123456789abc";
        assert_eq!(
            resume_session_id(&["--resume".into(), id.into()]).as_deref(),
            Some(id)
        );
        assert_eq!(
            resume_session_id(&[format!("--resume={id}")]).as_deref(),
            Some(id)
        );
        assert!(resume_session_id(&["--resume".into(), "latest".into()]).is_none());
    }
}
