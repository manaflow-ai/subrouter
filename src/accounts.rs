use std::fs::{self, File, OpenOptions};
use std::io::{self, Write};
use std::path::{Path, PathBuf};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use anyhow::{Context, anyhow, bail};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use chrono::{DateTime, SecondsFormat, Utc};
use fs2::FileExt;
use reqwest::{Client, StatusCode};
use serde::{Deserialize, Serialize};
use serde_json::{Map, Value, json};
use sha2::{Digest, Sha256};
use thiserror::Error;
use tracing::{debug, info, warn};
use uuid::Uuid;

use crate::account::{Account, AuthMode, Provider};
use crate::storepath;

mod openai_admin;

pub use openai_admin::{
    UsageSummary, estimate_cost_usd, fetch_api_key_usage_snapshot, fetch_openai_usage_rollup,
    list_openai_projects, summarize_daily_usage, validate_admin_key,
};

const CODEX_OAUTH_TOKEN_URL: &str = "https://auth.openai.com/oauth/token";
const CODEX_OAUTH_CLIENT_ID: &str = "app_EMoamEEZ73f0CkXaXp7hrann";
const CODEX_USAGE_URL: &str = "https://chatgpt.com/backend-api/wham/usage";
const CODEX_RESET_CREDITS_URL: &str =
    "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits";
const CODEX_CLI_USER_AGENT: &str = "codex_cli_rs/0.0.0";
const CODEX_AUTH_BREADCRUMB_LIMIT: usize = 50;

#[derive(Clone, Default, Deserialize, PartialEq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct StoredCodexAccount {
    pub email: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub provider: Option<Provider>,
    #[serde(default)]
    pub added_at: String,
    pub auth: CodexAuthFile,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub project_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub project_name: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub admin_key_label: String,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub breadcrumbs: Vec<CodexAuthBreadcrumb>,
}

#[derive(Clone, Default, Deserialize, PartialEq, Serialize)]
pub struct CodexAuthFile {
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub tokens: Option<CodexTokens>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub last_refresh: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub auth_mode: String,
    #[serde(
        rename = "OPENAI_API_KEY",
        default,
        skip_serializing_if = "String::is_empty"
    )]
    pub openai_api_key: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub refresh_failure: Option<CodexRefreshFailure>,
}

#[derive(Clone, Default, Deserialize, PartialEq, Serialize)]
pub struct CodexTokens {
    pub access_token: String,
    pub refresh_token: String,
    pub id_token: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub account_id: String,
}

#[derive(Clone, Default, Deserialize, Eq, PartialEq, Serialize)]
pub struct CodexRefreshFailure {
    pub at: String,
    #[serde(default, skip_serializing_if = "is_zero_i32")]
    pub status_code: i32,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub provider_type: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub provider_code: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub provider_message: String,
}

#[derive(Clone, Default, Deserialize, PartialEq, Serialize)]
pub struct CodexAuthBreadcrumb {
    pub at: String,
    pub event: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub source: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub reason: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub host: String,
    #[serde(default, skip_serializing_if = "is_zero_u32")]
    pub pid: u32,
    #[serde(default, skip_serializing_if = "is_zero_u32")]
    pub ppid: u32,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub executable: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub working_dir: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub store_dir: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub source_path: String,
    pub force: bool,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub last_refresh: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub access_exp: String,
    pub access_expired: bool,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub access_fp: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub refresh_fp: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub account_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub old_access_exp: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub old_access_fp: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub old_refresh_fp: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub old_account_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub new_access_exp: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub new_access_fp: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub new_refresh_fp: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub new_account_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub recovered_access_exp: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub recovered_access_fp: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub recovered_refresh_fp: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub recovered_account_id: String,
    #[serde(default, skip_serializing_if = "is_zero_i32")]
    pub status_code: i32,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub provider_type: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub provider_code: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub provider_message: String,
}

const fn is_zero_i32(value: &i32) -> bool {
    *value == 0
}

const fn is_zero_u32(value: &u32) -> bool {
    *value == 0
}

#[derive(Clone, Debug)]
pub struct CodexStore {
    pub dir: PathBuf,
}

impl Default for CodexStore {
    fn default() -> Self {
        Self {
            dir: storepath::codex_dir().join("accounts"),
        }
    }
}

impl CodexStore {
    #[must_use]
    pub fn new(dir: impl Into<PathBuf>) -> Self {
        Self { dir: dir.into() }
    }

    #[must_use]
    pub fn store_dir(&self) -> PathBuf {
        self.dir
            .parent()
            .unwrap_or_else(|| Path::new("."))
            .to_path_buf()
    }

    pub fn list(&self) -> anyhow::Result<Vec<Account>> {
        let mut accounts: Vec<_> = self
            .list_stored()?
            .into_iter()
            .filter_map(|stored| stored.to_account(stored.source_path(self)))
            .collect();
        accounts.sort_by(|left, right| left.id.cmp(&right.id));
        Ok(accounts)
    }

    pub fn list_stored(&self) -> anyhow::Result<Vec<StoredCodexAccount>> {
        let entries = match fs::read_dir(&self.dir) {
            Ok(entries) => entries,
            Err(error) if error.kind() == io::ErrorKind::NotFound => return Ok(Vec::new()),
            Err(error) => return Err(error.into()),
        };
        let mut accounts = Vec::new();
        for entry in entries {
            let entry = entry?;
            let name = entry.file_name();
            let name = name.to_string_lossy();
            if entry.file_type()?.is_dir() || name.starts_with('.') || !name.ends_with(".json") {
                continue;
            }
            let path = entry.path();
            let account: StoredCodexAccount = serde_json::from_slice(&fs::read(&path)?)
                .with_context(|| format!("parse {}", path.display()))?;
            if !account.email.trim().is_empty() {
                accounts.push(account);
            }
        }
        accounts.sort_by(|left, right| left.email.cmp(&right.email));
        Ok(accounts)
    }

    pub fn save_stored(&self, account: &mut StoredCodexAccount) -> anyhow::Result<()> {
        if account.email.trim().is_empty() {
            bail!("account email is required");
        }
        let _lock = self.lock_stored_account(&account.email)?;
        self.save_stored_unlocked(account)
    }

    fn save_stored_unlocked(&self, account: &mut StoredCodexAccount) -> anyhow::Result<()> {
        if account.email.trim().is_empty() {
            bail!("account email is required");
        }
        if account.added_at.is_empty() {
            account.added_at = Utc::now().to_rfc3339_opts(SecondsFormat::Secs, true);
        }
        let mut body = serde_json::to_vec_pretty(account)?;
        body.push(b'\n');
        write_file_atomic(&self.dir.join(email_to_filename(&account.email)), &body)
    }

    pub fn find_stored(&self, identifier: &str) -> anyhow::Result<Option<StoredCodexAccount>> {
        let needle = identifier.trim();
        if needle.is_empty() {
            return Ok(None);
        }
        let path = self.dir.join(email_to_filename(needle));
        match fs::read(&path) {
            Ok(body) => return Ok(Some(serde_json::from_slice(&body)?)),
            Err(error) if error.kind() == io::ErrorKind::NotFound => {}
            Err(error) => return Err(error.into()),
        }
        if !needle.starts_with("apikey:")
            && !needle.contains('@')
            && let Some(account) = self.find_stored(&format!("apikey:{needle}"))?
        {
            return Ok(Some(account));
        }
        let needle_lower = needle.to_ascii_lowercase();
        let matches: Vec<_> = self
            .list_stored()?
            .into_iter()
            .filter(|account| account.email.to_ascii_lowercase().contains(&needle_lower))
            .collect();
        match matches.as_slice() {
            [] => Ok(None),
            [account] => Ok(Some(account.clone())),
            _ => bail!(
                "multiple accounts match {:?}: {}",
                identifier,
                matches
                    .iter()
                    .map(|account| account.email.as_str())
                    .collect::<Vec<_>>()
                    .join(", ")
            ),
        }
    }

    pub fn remove_stored(&self, identifier: &str) -> anyhow::Result<Option<StoredCodexAccount>> {
        let Some(account) = self.find_stored(identifier)? else {
            return Ok(None);
        };
        fs::remove_file(self.dir.join(email_to_filename(&account.email)))?;
        Ok(Some(account))
    }

    pub fn migrate_stored_away(&self, identifier: &str) -> anyhow::Result<Option<PathBuf>> {
        let Some(account) = self.find_stored(identifier)? else {
            return Ok(None);
        };
        let name = email_to_filename(&account.email);
        let destination = self.dir.join("migrated");
        fs::create_dir_all(&destination)?;
        let target = destination.join(&name);
        fs::rename(self.dir.join(name), &target)?;
        Ok(Some(target))
    }

    pub fn add_api_key(
        &self,
        label: &str,
        key: &str,
    ) -> anyhow::Result<(StoredCodexAccount, bool)> {
        self.add_provider_api_key(Provider::Codex, label, key)
    }

    pub fn add_provider_api_key(
        &self,
        provider: Provider,
        label: &str,
        key: &str,
    ) -> anyhow::Result<(StoredCodexAccount, bool)> {
        let label = label.trim();
        let key = key.trim();
        if label.is_empty() {
            bail!("label is required");
        }
        if provider == Provider::Codex && !key.starts_with("sk-") {
            bail!("invalid API key format, expected sk-...");
        }
        if key.is_empty() {
            bail!("API key is required");
        }
        let email = if provider == Provider::Codex {
            format!("apikey:{label}")
        } else {
            format!("{}:{label}", provider.as_str())
        };
        let existing = self.find_stored(&email)?;
        let existed = existing.is_some();
        let mut account = existing.unwrap_or_else(|| StoredCodexAccount {
            email,
            ..StoredCodexAccount::default()
        });
        account.provider = Some(provider);
        account.auth = CodexAuthFile {
            auth_mode: "apikey".into(),
            openai_api_key: key.into(),
            ..CodexAuthFile::default()
        };
        self.save_stored(&mut account)?;
        Ok((account, existed))
    }

    pub fn import_active(&self) -> anyhow::Result<(StoredCodexAccount, bool)> {
        let Some(auth) = read_codex_auth_file(&default_codex_auth_path())? else {
            bail!(
                "no active Codex OAuth auth found in {}",
                default_codex_auth_path().display()
            );
        };
        let Some(tokens) = auth
            .tokens
            .as_ref()
            .filter(|tokens| !tokens.id_token.is_empty())
        else {
            bail!(
                "no active Codex OAuth auth found in {}",
                default_codex_auth_path().display()
            );
        };
        let email = extract_email_from_jwt(&tokens.id_token)
            .context("could not extract email from current auth token")?;
        let existing = self.find_stored(&email)?;
        let existed = existing.is_some();
        let mut account = existing.unwrap_or_else(|| StoredCodexAccount {
            email,
            ..StoredCodexAccount::default()
        });
        let previous = account.clone();
        account.auth = auth;
        let next = account.clone();
        append_breadcrumb(
            self,
            &mut account,
            "active_auth_imported",
            "active_auth",
            "unspecified",
            false,
            Some(&previous),
            Some(&next),
            None,
            None,
        );
        self.save_stored(&mut account)?;
        Ok((account, existed))
    }

    pub fn detect_active_account(&self) -> anyhow::Result<Option<String>> {
        let Some(auth) = read_codex_auth_file(&default_codex_auth_path())? else {
            return Ok(None);
        };
        if let Some(tokens) = auth
            .tokens
            .as_ref()
            .filter(|tokens| !tokens.id_token.is_empty())
            && let Ok(email) = extract_email_from_jwt(&tokens.id_token)
            && self.find_stored(&email)?.is_some()
        {
            return Ok(Some(email));
        }
        if !auth.openai_api_key.is_empty() {
            return Ok(self
                .list_stored()?
                .into_iter()
                .find(|account| account.auth.openai_api_key == auth.openai_api_key)
                .map(|account| account.email));
        }
        Ok(None)
    }

    pub fn sync_active_to_store(&self) -> anyhow::Result<()> {
        let Some(auth) = read_codex_auth_file(&default_codex_auth_path())? else {
            return Ok(());
        };
        let Some(tokens) = auth
            .tokens
            .as_ref()
            .filter(|tokens| !tokens.id_token.is_empty())
        else {
            return Ok(());
        };
        let Ok(email) = extract_email_from_jwt(&tokens.id_token) else {
            return Ok(());
        };
        let _lock = self.lock_stored_account(&email)?;
        let Some(mut account) = self.find_stored(&email)? else {
            return Ok(());
        };
        if account_auth_newer_than_incoming(&account.auth, &auth) {
            debug!(account = %email, "codex oauth active auth sync skipped because stored auth is newer");
            return Ok(());
        }
        let previous = account.clone();
        account.auth = auth;
        let next = account.clone();
        append_breadcrumb(
            self,
            &mut account,
            "active_auth_synced",
            "active_auth",
            "unspecified",
            false,
            Some(&previous),
            Some(&next),
            None,
            None,
        );
        self.save_stored_unlocked(&mut account)?;
        Ok(())
    }

    pub fn switch_active(&self, account_id: &str) -> anyhow::Result<Account> {
        let needle = account_id.trim();
        if needle.is_empty() {
            bail!("account id is required");
        }
        let Some(stored) = self.find_stored(needle)? else {
            bail!("account {:?} not found", account_id)
        };
        let source = stored.source_path(self);
        let account = stored
            .to_account(source.clone())
            .ok_or_else(|| anyhow!("account {:?} is not usable", account_id))?;
        let body: Value = serde_json::from_slice(&fs::read(source)?)?;
        let auth: CodexAuthFile = serde_json::from_value(
            body.get("auth")
                .cloned()
                .ok_or_else(|| anyhow!("stored account has no auth"))?,
        )?;
        write_active_codex_auth(&auth)?;
        Ok(account)
    }

    fn lock_stored_account(&self, email: &str) -> anyhow::Result<FileLock> {
        fs::create_dir_all(&self.dir)?;
        FileLock::exclusive(self.dir.join(format!(".{}.lock", email_to_filename(email))))
            .map_err(Into::into)
    }

    pub async fn refresh_stored_if_expired(
        &self,
        client: &Client,
        account: StoredCodexAccount,
        reason: &str,
    ) -> anyhow::Result<(StoredCodexAccount, bool)> {
        self.refresh_stored(client, account, reason, false).await
    }

    pub async fn refresh_stored_force(
        &self,
        client: &Client,
        account: StoredCodexAccount,
        reason: &str,
    ) -> anyhow::Result<(StoredCodexAccount, bool)> {
        self.refresh_stored(client, account, reason, true).await
    }

    async fn refresh_stored(
        &self,
        client: &Client,
        mut account: StoredCodexAccount,
        reason: &str,
        force: bool,
    ) -> anyhow::Result<(StoredCodexAccount, bool)> {
        let Some(tokens) = account.auth.tokens.as_ref() else {
            return Ok((account, false));
        };
        if !force && !is_jwt_expired(&tokens.access_token, Duration::from_secs(60)) {
            return Ok((account, false));
        }
        terminal_refresh_failure(&account)?;
        let _lock = self.lock_stored_account(&account.email)?;
        if let Some(latest) = self.find_stored(&account.email)? {
            account = latest;
        }
        let Some(tokens) = account.auth.tokens.as_ref() else {
            return Ok((account, false));
        };
        if !force && !is_jwt_expired(&tokens.access_token, Duration::from_secs(60)) {
            return Ok((account, false));
        }
        terminal_refresh_failure(&account)?;
        let previous = account.clone();
        info!(account = %account.email, reason, force, access_fp = %access_fingerprint(&account), "codex oauth refresh start");
        match refresh_codex_auth_at(client, &account.auth, CODEX_OAUTH_TOKEN_URL).await {
            Ok(auth) => {
                account.auth = auth;
                account.auth.refresh_failure = None;
                let next = account.clone();
                append_breadcrumb(
                    self,
                    &mut account,
                    "refresh_succeeded",
                    "oauth_refresh",
                    reason,
                    force,
                    Some(&previous),
                    Some(&next),
                    None,
                    None,
                );
                self.save_stored_unlocked(&mut account)?;
                if active_codex_auth_email()?.as_deref() == Some(&account.email) {
                    write_active_codex_auth(&account.auth)?;
                }
                info!(account = %account.email, reason, access_fp = %access_fingerprint(&account), "codex oauth refresh succeeded");
                Ok((account, true))
            }
            Err(error) => {
                if let Some(mut recovered) = self.recover_refreshed_account(&account)? {
                    let recovered_snapshot = recovered.clone();
                    append_breadcrumb(
                        self,
                        &mut recovered,
                        "refresh_recovered_concurrent_update",
                        "oauth_refresh",
                        reason,
                        force,
                        Some(&previous),
                        None,
                        Some(&recovered_snapshot),
                        Some(&error),
                    );
                    self.save_stored_unlocked(&mut recovered)?;
                    warn!(account = %account.email, reason, "codex oauth refresh recovered from concurrent update");
                    return Ok((recovered, false));
                }
                if let Some(refresh_error) = error
                    .downcast_ref::<CodexAuthRefreshError>()
                    .filter(|error| error.terminal())
                {
                    account.auth.refresh_failure = Some(CodexRefreshFailure {
                        at: Utc::now().to_rfc3339_opts(SecondsFormat::Secs, true),
                        status_code: i32::from(refresh_error.status.as_u16()),
                        provider_type: refresh_error.provider_type.clone(),
                        provider_code: refresh_error.provider_code.clone(),
                        provider_message: refresh_error.provider_message.clone(),
                    });
                    append_breadcrumb(
                        self,
                        &mut account,
                        "refresh_terminal_failure",
                        "oauth_refresh",
                        reason,
                        force,
                        Some(&previous),
                        None,
                        None,
                        Some(&error),
                    );
                    self.save_stored_unlocked(&mut account)?;
                }
                Err(error)
            }
        }
    }

    fn recover_refreshed_account(
        &self,
        previous: &StoredCodexAccount,
    ) -> anyhow::Result<Option<StoredCodexAccount>> {
        let Some(latest) = self.find_stored(&previous.email)? else {
            return Ok(None);
        };
        let (Some(previous_tokens), Some(latest_tokens)) =
            (previous.auth.tokens.as_ref(), latest.auth.tokens.as_ref())
        else {
            return Ok(None);
        };
        if latest_tokens.access_token == previous_tokens.access_token
            && latest_tokens.refresh_token == previous_tokens.refresh_token
        {
            return Ok(None);
        }
        Ok(
            (!is_jwt_expired(&latest_tokens.access_token, Duration::from_secs(60)))
                .then_some(latest),
        )
    }
}

impl StoredCodexAccount {
    #[must_use]
    pub fn source_path(&self, store: &CodexStore) -> PathBuf {
        store.dir.join(email_to_filename(&self.email))
    }

    #[must_use]
    pub fn is_api_key(&self) -> bool {
        self.auth.auth_mode == "apikey" || !self.auth.openai_api_key.is_empty()
    }

    #[must_use]
    pub fn api_key_label(&self) -> &str {
        self.email
            .strip_prefix("apikey:")
            .or_else(|| self.email.strip_prefix("claude:"))
            .or_else(|| self.email.strip_prefix("kimi:"))
            .or_else(|| self.email.strip_prefix("zai:"))
            .unwrap_or(&self.email)
    }

    #[must_use]
    pub fn provider_or_default(&self) -> Provider {
        self.provider.unwrap_or_default()
    }

    #[must_use]
    pub fn to_account(&self, source: PathBuf) -> Option<Account> {
        let id = self.email.trim();
        if id.is_empty() {
            return None;
        }
        let mut output = Account {
            id: id.into(),
            provider: self.provider_or_default(),
            label: id.into(),
            email: id.into(),
            added_at: DateTime::parse_from_rfc3339(&self.added_at)
                .ok()
                .map(|value| value.with_timezone(&Utc)),
            source: source.to_string_lossy().into(),
            ..Account::default()
        };
        if self.is_api_key() {
            output.auth_mode = AuthMode::ApiKey;
            output.token.clone_from(&self.auth.openai_api_key);
            return (!output.token.is_empty()).then_some(output);
        }
        let tokens = self.auth.tokens.as_ref()?;
        if tokens.access_token.is_empty() {
            return None;
        }
        output.auth_mode = AuthMode::Oauth;
        output.token.clone_from(&tokens.access_token);
        output.account_id.clone_from(&tokens.account_id);
        Some(output)
    }
}

struct FileLock(File);

impl FileLock {
    fn exclusive(path: PathBuf) -> io::Result<Self> {
        if let Some(parent) = path.parent() {
            fs::create_dir_all(parent)?;
        }
        let file = OpenOptions::new()
            .create(true)
            .truncate(false)
            .read(true)
            .write(true)
            .open(path)?;
        file.lock_exclusive()?;
        Ok(Self(file))
    }

    fn try_exclusive(path: PathBuf) -> io::Result<Option<Self>> {
        if let Some(parent) = path.parent() {
            fs::create_dir_all(parent)?;
        }
        let file = OpenOptions::new()
            .create(true)
            .truncate(false)
            .read(true)
            .write(true)
            .open(path)?;
        match file.try_lock_exclusive() {
            Ok(()) => Ok(Some(Self(file))),
            Err(error) if error.kind() == io::ErrorKind::WouldBlock => Ok(None),
            Err(error) => Err(error),
        }
    }
}

impl Drop for FileLock {
    fn drop(&mut self) {
        let _ = self.0.unlock();
    }
}

pub struct ActiveCodexAuthLock {
    _lock: FileLock,
}

pub fn try_lock_active_codex_auth() -> io::Result<Option<ActiveCodexAuthLock>> {
    Ok(
        FileLock::try_exclusive(default_codex_auth_path().with_extension("json.lock"))?
            .map(|lock| ActiveCodexAuthLock { _lock: lock }),
    )
}

pub fn lock_active_codex_auth() -> io::Result<ActiveCodexAuthLock> {
    FileLock::exclusive(default_codex_auth_path().with_extension("json.lock"))
        .map(|lock| ActiveCodexAuthLock { _lock: lock })
}

#[must_use]
pub fn default_codex_auth_path() -> PathBuf {
    std::env::var_os("CODEX_HOME")
        .filter(|value| !value.to_string_lossy().trim().is_empty())
        .map(PathBuf::from)
        .unwrap_or_else(|| {
            std::env::var_os(if cfg!(windows) { "USERPROFILE" } else { "HOME" }).map_or_else(
                || PathBuf::from(".codex"),
                |home| PathBuf::from(home).join(".codex"),
            )
        })
        .join("auth.json")
}

pub fn read_codex_auth_file(path: &Path) -> anyhow::Result<Option<CodexAuthFile>> {
    match fs::read(path) {
        Ok(body) => Ok(Some(serde_json::from_slice(&body)?)),
        Err(error) if error.kind() == io::ErrorKind::NotFound => Ok(None),
        Err(error) => Err(error.into()),
    }
}

pub fn write_active_codex_auth(auth: &CodexAuthFile) -> anyhow::Result<()> {
    let _lock = lock_active_codex_auth()?;
    write_codex_active_auth(&default_codex_auth_path(), &serde_json::to_value(auth)?)
}

fn write_codex_active_auth(path: &Path, raw_auth: &Value) -> anyhow::Result<()> {
    let auth: CodexAuthFile = serde_json::from_value(raw_auth.clone())?;
    let payload = if auth.auth_mode == "apikey" || !auth.openai_api_key.is_empty() {
        json!({"auth_mode": "apikey", "OPENAI_API_KEY": auth.openai_api_key})
    } else {
        raw_auth.clone()
    };
    let mut body = serde_json::to_vec_pretty(&payload)?;
    body.push(b'\n');
    if let Some(parent) = path.parent() {
        fs::create_dir_all(parent)?;
    }
    if path.exists() {
        let _ = fs::rename(path, path.with_extension("json.bak"));
    }
    write_file_atomic(path, &body)
}

fn active_codex_auth_email() -> anyhow::Result<Option<String>> {
    let Some(auth) = read_codex_auth_file(&default_codex_auth_path())? else {
        return Ok(None);
    };
    let Some(tokens) = auth
        .tokens
        .as_ref()
        .filter(|tokens| !tokens.id_token.is_empty())
    else {
        return Ok(None);
    };
    Ok(extract_email_from_jwt(&tokens.id_token).ok())
}

pub async fn refresh_codex_auth(
    client: &Client,
    auth: &CodexAuthFile,
) -> anyhow::Result<CodexAuthFile> {
    refresh_codex_auth_at(client, auth, CODEX_OAUTH_TOKEN_URL).await
}

async fn refresh_codex_auth_at(
    client: &Client,
    auth: &CodexAuthFile,
    endpoint: &str,
) -> anyhow::Result<CodexAuthFile> {
    let Some(tokens) = auth.tokens.as_ref() else {
        return Ok(auth.clone());
    };
    let response = client
        .post(endpoint)
        .header(reqwest::header::CONTENT_TYPE, "application/json")
        .json(&json!({
            "client_id": CODEX_OAUTH_CLIENT_ID,
            "grant_type": "refresh_token",
            "refresh_token": tokens.refresh_token,
        }))
        .send()
        .await?;
    if !response.status().is_success() {
        let status = response.status();
        let body = response.text().await.unwrap_or_default();
        return Err(CodexAuthRefreshError::new(status, body.trim()).into());
    }
    #[derive(Deserialize)]
    struct RefreshResponse {
        access_token: String,
        refresh_token: String,
        id_token: String,
    }
    let refreshed: RefreshResponse = response.json().await?;
    if refreshed.access_token.is_empty()
        || refreshed.refresh_token.is_empty()
        || refreshed.id_token.is_empty()
    {
        bail!("token refresh response missing required fields");
    }
    let mut auth = auth.clone();
    let tokens = auth.tokens.as_mut().expect("tokens checked above");
    tokens.access_token = refreshed.access_token;
    tokens.refresh_token = refreshed.refresh_token;
    tokens.id_token = refreshed.id_token;
    auth.last_refresh = Utc::now().to_rfc3339_opts(SecondsFormat::Secs, true);
    Ok(auth)
}

#[derive(Debug, Error)]
#[error("token refresh failed ({status}): {body}")]
pub struct CodexAuthRefreshError {
    pub status: StatusCode,
    pub body: String,
    pub provider_type: String,
    pub provider_code: String,
    pub provider_message: String,
}

impl CodexAuthRefreshError {
    fn new(status: StatusCode, body: &str) -> Self {
        let provider = serde_json::from_str::<Value>(body)
            .ok()
            .and_then(|value| value.get("error").cloned())
            .unwrap_or_default();
        Self {
            status,
            body: body.into(),
            provider_type: provider
                .get("type")
                .and_then(Value::as_str)
                .unwrap_or_default()
                .into(),
            provider_code: provider
                .get("code")
                .and_then(Value::as_str)
                .unwrap_or_default()
                .into(),
            provider_message: provider
                .get("message")
                .and_then(Value::as_str)
                .unwrap_or_default()
                .into(),
        }
    }

    fn terminal(&self) -> bool {
        is_terminal_refresh_failure(self.status, &self.provider_code, &self.provider_message)
    }
}

#[derive(Debug, Error)]
#[error("token refresh previously failed{status}: {code}{message}: reauth required{recorded}")]
struct StoredRefreshFailureError {
    status: String,
    code: String,
    message: String,
    recorded: String,
}

fn terminal_refresh_failure(account: &StoredCodexAccount) -> anyhow::Result<()> {
    let Some(failure) = account.auth.refresh_failure.as_ref() else {
        return Ok(());
    };
    let status = StatusCode::from_u16(u16::try_from(failure.status_code).unwrap_or_default())
        .unwrap_or(StatusCode::INTERNAL_SERVER_ERROR);
    if !is_terminal_refresh_failure(status, &failure.provider_code, &failure.provider_message) {
        return Ok(());
    }
    Err(StoredRefreshFailureError {
        status: if failure.status_code != 0 {
            format!(" ({})", failure.status_code)
        } else {
            Default::default()
        },
        code: if !failure.provider_code.is_empty() {
            format!(": {}", failure.provider_code)
        } else {
            Default::default()
        },
        message: if !failure.provider_message.is_empty() {
            format!(": {}", failure.provider_message)
        } else {
            Default::default()
        },
        recorded: if !failure.at.is_empty() {
            format!("; recorded at {}", failure.at)
        } else {
            Default::default()
        },
    }
    .into())
}

fn is_terminal_refresh_failure(
    status: StatusCode,
    provider_code: &str,
    provider_message: &str,
) -> bool {
    let code = provider_code.trim().to_ascii_lowercase();
    let message = provider_message.to_ascii_lowercase();
    message.contains("logged out or signed in to another account")
        || message.contains("access token could not be refreshed because you have since")
        || (matches!(status, StatusCode::BAD_REQUEST | StatusCode::UNAUTHORIZED)
            && (code.contains("refresh_token") || message.contains("refresh token")))
}

fn account_auth_newer_than_incoming(stored: &CodexAuthFile, incoming: &CodexAuthFile) -> bool {
    let (Some(stored), Some(incoming)) = (stored.tokens.as_ref(), incoming.tokens.as_ref()) else {
        return false;
    };
    if stored.access_token == incoming.access_token
        && stored.refresh_token == incoming.refresh_token
    {
        return false;
    }
    match (
        jwt_expiry_millis(&stored.access_token),
        jwt_expiry_millis(&incoming.access_token),
    ) {
        (Some(stored), Some(incoming)) => stored > incoming,
        _ => {
            !is_jwt_expired(&stored.access_token, Duration::from_secs(60))
                && is_jwt_expired(&incoming.access_token, Duration::from_secs(60))
        }
    }
}

pub fn decode_jwt_claims(token: &str) -> anyhow::Result<Map<String, Value>> {
    let mut parts = token.split('.');
    let (_header, Some(payload), Some(_signature), None) =
        (parts.next(), parts.next(), parts.next(), parts.next())
    else {
        bail!("invalid JWT format");
    };
    Ok(serde_json::from_slice(&URL_SAFE_NO_PAD.decode(payload)?)?)
}

pub fn extract_email_from_jwt(token: &str) -> anyhow::Result<String> {
    let claims = decode_jwt_claims(token)?;
    claims
        .get("https://api.openai.com/profile")
        .and_then(Value::as_object)
        .and_then(|profile| profile.get("email"))
        .and_then(Value::as_str)
        .or_else(|| claims.get("email").and_then(Value::as_str))
        .filter(|email| !email.is_empty())
        .map(str::to_owned)
        .ok_or_else(|| anyhow!("JWT has no email claim"))
}

#[must_use]
pub fn extract_chatgpt_account_id(auth: &CodexAuthFile) -> String {
    let Some(tokens) = auth.tokens.as_ref() else {
        return String::new();
    };
    if !tokens.account_id.is_empty() {
        return tokens.account_id.clone();
    }
    [&tokens.id_token, &tokens.access_token]
        .into_iter()
        .find_map(|token| extract_chatgpt_account_id_from_jwt(token))
        .unwrap_or_default()
}

fn extract_chatgpt_account_id_from_jwt(token: &str) -> Option<String> {
    let claims = decode_jwt_claims(token).ok()?;
    claims
        .get("chatgpt_account_id")
        .and_then(Value::as_str)
        .or_else(|| {
            claims
                .get("https://api.openai.com/auth")
                .and_then(Value::as_object)
                .and_then(|auth| auth.get("chatgpt_account_id"))
                .and_then(Value::as_str)
        })
        .or_else(|| {
            claims
                .get("organizations")
                .and_then(Value::as_array)
                .and_then(|organizations| organizations.first())
                .and_then(Value::as_object)
                .and_then(|organization| organization.get("id"))
                .and_then(Value::as_str)
        })
        .filter(|value| !value.is_empty())
        .map(str::to_owned)
}

#[must_use]
pub fn jwt_expiry_millis(token: &str) -> Option<i64> {
    let claims = decode_jwt_claims(token).ok()?;
    let expiry = claims.get("exp")?.as_f64()?;
    Some((expiry * 1_000.0) as i64)
}

#[must_use]
pub fn is_jwt_expired(token: &str, grace: Duration) -> bool {
    let Some(expiry) = jwt_expiry_millis(token) else {
        return false;
    };
    let now = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_millis() as i64;
    expiry - now < grace.as_millis() as i64
}

#[allow(clippy::too_many_arguments)]
fn append_breadcrumb(
    store: &CodexStore,
    account: &mut StoredCodexAccount,
    event: &str,
    source: &str,
    reason: &str,
    force: bool,
    old: Option<&StoredCodexAccount>,
    next: Option<&StoredCodexAccount>,
    recovered: Option<&StoredCodexAccount>,
    error: Option<&anyhow::Error>,
) {
    let mut crumb =
        CodexAuthBreadcrumb {
            at: Utc::now().to_rfc3339_opts(SecondsFormat::Nanos, true),
            event: if event.trim().is_empty() {
                "unknown".into()
            } else {
                event.trim().into()
            },
            source: source.trim().into(),
            reason: if reason.trim().is_empty() {
                "unspecified".into()
            } else {
                reason.trim().into()
            },
            host: hostname::get().unwrap_or_default().to_string_lossy().into(),
            pid: std::process::id(),
            ppid: parent_pid(),
            executable: std::env::current_exe()
                .unwrap_or_default()
                .to_string_lossy()
                .into(),
            working_dir: std::env::current_dir()
                .unwrap_or_default()
                .to_string_lossy()
                .into(),
            store_dir: store.store_dir().to_string_lossy().into(),
            source_path: account.source_path(store).to_string_lossy().into(),
            force,
            last_refresh: account.auth.last_refresh.clone(),
            access_exp: access_expiry(account),
            access_expired: account.auth.tokens.as_ref().is_some_and(|tokens| {
                is_jwt_expired(&tokens.access_token, Duration::from_secs(60))
            }),
            access_fp: access_fingerprint(account),
            refresh_fp: refresh_fingerprint(account),
            account_id: account
                .auth
                .tokens
                .as_ref()
                .map_or_else(String::new, |tokens| tokens.account_id.clone()),
            ..CodexAuthBreadcrumb::default()
        };
    if let Some(old) = old {
        crumb.old_access_exp = access_expiry(old);
        crumb.old_access_fp = access_fingerprint(old);
        crumb.old_refresh_fp = refresh_fingerprint(old);
        crumb.old_account_id = old
            .auth
            .tokens
            .as_ref()
            .map_or_else(String::new, |tokens| tokens.account_id.clone());
    }
    if let Some(next) = next {
        crumb.new_access_exp = access_expiry(next);
        crumb.new_access_fp = access_fingerprint(next);
        crumb.new_refresh_fp = refresh_fingerprint(next);
        crumb.new_account_id = next
            .auth
            .tokens
            .as_ref()
            .map_or_else(String::new, |tokens| tokens.account_id.clone());
    }
    if let Some(recovered) = recovered {
        crumb.recovered_access_exp = access_expiry(recovered);
        crumb.recovered_access_fp = access_fingerprint(recovered);
        crumb.recovered_refresh_fp = refresh_fingerprint(recovered);
        crumb.recovered_account_id = recovered
            .auth
            .tokens
            .as_ref()
            .map_or_else(String::new, |tokens| tokens.account_id.clone());
    }
    if let Some(error) = error.and_then(|error| error.downcast_ref::<CodexAuthRefreshError>()) {
        crumb.status_code = i32::from(error.status.as_u16());
        crumb.provider_type.clone_from(&error.provider_type);
        crumb.provider_code.clone_from(&error.provider_code);
        crumb.provider_message.clone_from(&error.provider_message);
    }
    account.breadcrumbs.push(crumb);
    let extra = account
        .breadcrumbs
        .len()
        .saturating_sub(CODEX_AUTH_BREADCRUMB_LIMIT);
    if extra > 0 {
        account.breadcrumbs.drain(..extra);
    }
}

#[cfg(unix)]
fn parent_pid() -> u32 {
    nix::unistd::getppid().as_raw() as u32
}

#[cfg(not(unix))]
const fn parent_pid() -> u32 {
    0
}

fn access_expiry(account: &StoredCodexAccount) -> String {
    account
        .auth
        .tokens
        .as_ref()
        .and_then(|tokens| jwt_expiry_millis(&tokens.access_token))
        .and_then(DateTime::from_timestamp_millis)
        .map(|value| value.to_rfc3339_opts(SecondsFormat::Secs, true))
        .unwrap_or_default()
}

fn token_fingerprint(token: &str) -> String {
    if token.is_empty() {
        String::new()
    } else {
        hex::encode(Sha256::digest(token.as_bytes()))[..16].into()
    }
}

fn access_fingerprint(account: &StoredCodexAccount) -> String {
    account
        .auth
        .tokens
        .as_ref()
        .map_or_else(String::new, |tokens| {
            token_fingerprint(&tokens.access_token)
        })
}

fn refresh_fingerprint(account: &StoredCodexAccount) -> String {
    account
        .auth
        .tokens
        .as_ref()
        .map_or_else(String::new, |tokens| {
            token_fingerprint(&tokens.refresh_token)
        })
}

fn email_to_filename(email: &str) -> String {
    let name: String = email
        .chars()
        .map(|character| {
            if character.is_ascii_alphanumeric() || matches!(character, '.' | '_' | '@' | '-') {
                character
            } else {
                '_'
            }
        })
        .collect();
    format!("{name}.json")
}

fn safe_filename(value: &str) -> String {
    value
        .chars()
        .map(|character| {
            if character.is_ascii_alphanumeric() || matches!(character, '.' | '_' | '-') {
                character
            } else {
                '_'
            }
        })
        .collect()
}

fn write_file_atomic(path: &Path, body: &[u8]) -> anyhow::Result<()> {
    let parent = path.parent().unwrap_or_else(|| Path::new("."));
    fs::create_dir_all(parent)?;
    let mut temp = tempfile::Builder::new()
        .prefix(&format!(
            ".{}.tmp-",
            path.file_name().unwrap_or_default().to_string_lossy()
        ))
        .tempfile_in(parent)?;
    temp.write_all(body)?;
    temp.as_file().sync_all()?;
    set_private_permissions(temp.path())?;
    temp.persist(path).map_err(|error| error.error)?;
    if let Ok(directory) = File::open(parent) {
        let _ = directory.sync_all();
    }
    Ok(())
}

#[cfg(unix)]
fn set_private_permissions(path: &Path) -> io::Result<()> {
    use std::os::unix::fs::PermissionsExt;
    fs::set_permissions(path, fs::Permissions::from_mode(0o600))
}

#[cfg(not(unix))]
fn set_private_permissions(_path: &Path) -> io::Result<()> {
    Ok(())
}

#[derive(Clone, Debug, Default, Deserialize, PartialEq, Serialize)]
pub struct UsageWindow {
    pub name: String,
    pub used_percent: f64,
    pub limit_window_seconds: i64,
    pub reset_after_seconds: i64,
    pub feature: String,
}

#[derive(Clone, Debug, Default, Deserialize, PartialEq, Serialize)]
pub struct CreditsInfo {
    pub has_credits: bool,
    pub unlimited: bool,
    pub balance: String,
}

#[derive(Clone, Debug, Default, Deserialize, Eq, PartialEq, Serialize)]
pub struct ComplimentaryResetInfo {
    pub known: bool,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub available: bool,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub consumed: bool,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub eligible: Option<bool>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub remaining: Option<i64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub used: Option<i64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub total: Option<i64>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub status: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub resets_at: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub source: String,
}

#[derive(Clone, Debug, Default)]
pub struct CodexUsageDetails {
    pub plan_type: String,
    pub windows: Vec<UsageWindow>,
    pub base_windows: Vec<UsageWindow>,
    pub credits: Option<CreditsInfo>,
    pub complimentary_reset: Option<ComplimentaryResetInfo>,
}

#[derive(Clone, Debug, Default, Deserialize)]
struct LimitWindowWire {
    #[serde(default)]
    used_percent: f64,
    #[serde(default)]
    limit_window_seconds: i64,
    #[serde(default)]
    reset_after_seconds: i64,
    #[serde(default)]
    reset_at: i64,
}

impl LimitWindowWire {
    fn remaining_seconds(&self) -> i64 {
        if self.reset_after_seconds > 0 {
            return self.reset_after_seconds;
        }
        if self.reset_at <= 0 {
            return 0;
        }
        let now = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap_or_default()
            .as_secs() as i64;
        (self.reset_at - now).max(0)
    }
}

#[derive(Clone, Debug, Default, Deserialize)]
struct RateLimitWire {
    #[serde(default, rename = "allowed")]
    _allowed: bool,
    #[serde(default)]
    limit_reached: bool,
    primary_window: Option<LimitWindowWire>,
    secondary_window: Option<LimitWindowWire>,
}

#[derive(Clone, Debug, Default, Deserialize)]
struct CreditsWire {
    #[serde(default)]
    has_credits: bool,
    #[serde(default)]
    unlimited: bool,
    #[serde(default)]
    balance: String,
}

#[derive(Clone, Debug, Default, Deserialize)]
struct AdditionalRateLimitWire {
    #[serde(default)]
    metered_feature: String,
    #[serde(default)]
    limit_name: String,
    #[serde(default)]
    rate_limit: RateLimitWire,
}

#[derive(Clone, Debug, Default, Deserialize)]
struct UsageResponseWire {
    #[serde(default)]
    plan_type: String,
    #[serde(default)]
    rate_limit: RateLimitWire,
    credits: Option<CreditsWire>,
    #[serde(default)]
    additional_rate_limits: Vec<AdditionalRateLimitWire>,
}

pub async fn fetch_codex_usage(
    client: &Client,
    account: &Account,
) -> anyhow::Result<Vec<UsageWindow>> {
    Ok(fetch_codex_usage_details(client, account).await?.windows)
}

pub async fn fetch_codex_usage_details(
    client: &Client,
    account: &Account,
) -> anyhow::Result<CodexUsageDetails> {
    fetch_codex_usage_details_at(client, account, CODEX_USAGE_URL).await
}

async fn fetch_codex_usage_details_at(
    client: &Client,
    account: &Account,
    endpoint: &str,
) -> anyhow::Result<CodexUsageDetails> {
    if account.auth_mode != AuthMode::Oauth {
        bail!("usage is only available for OAuth accounts");
    }
    if account.token.is_empty() {
        bail!("account has no access token");
    }
    let mut request = client.get(endpoint).bearer_auth(&account.token);
    if !account.account_id.is_empty() {
        request = request.header("ChatGPT-Account-ID", &account.account_id);
    }
    let response = request.send().await?;
    if !response.status().is_success() {
        bail!("usage fetch failed: {}", response.status());
    }
    let body = response.bytes().await?;
    let usage: UsageResponseWire = serde_json::from_slice(&body)?;
    let base_windows = rate_limit_windows("", "", &usage.rate_limit);
    let mut windows = base_windows.clone();
    for additional in &usage.additional_rate_limits {
        let feature = if additional.limit_name.is_empty() {
            if additional.metered_feature.is_empty() {
                "unknown"
            } else {
                &additional.metered_feature
            }
        } else {
            &additional.limit_name
        };
        windows.extend(rate_limit_windows(
            &format!("{feature}/"),
            feature,
            &additional.rate_limit,
        ));
    }
    Ok(CodexUsageDetails {
        plan_type: usage.plan_type,
        windows,
        base_windows,
        credits: usage.credits.map(|credits| CreditsInfo {
            has_credits: credits.has_credits,
            unlimited: credits.unlimited,
            balance: credits.balance,
        }),
        complimentary_reset: parse_complimentary_reset(&serde_json::from_slice(&body)?),
    })
}

fn rate_limit_windows(prefix: &str, feature: &str, details: &RateLimitWire) -> Vec<UsageWindow> {
    let mut windows = Vec::new();
    for (name, window) in [
        ("primary", details.primary_window.as_ref()),
        ("secondary", details.secondary_window.as_ref()),
    ] {
        if let Some(window) = window {
            windows.push(UsageWindow {
                name: format!("{prefix}{name}"),
                used_percent: window.used_percent,
                limit_window_seconds: window.limit_window_seconds,
                reset_after_seconds: window.remaining_seconds(),
                feature: feature.into(),
            });
        }
    }
    if details.limit_reached {
        windows.push(UsageWindow {
            name: format!("{prefix}reached"),
            used_percent: 100.0,
            feature: feature.into(),
            ..UsageWindow::default()
        });
    }
    windows
}

fn parse_complimentary_reset(root: &Value) -> Option<ComplimentaryResetInfo> {
    fn walk(value: &Value, path: &str) -> Option<ComplimentaryResetInfo> {
        let object = value.as_object()?;
        for (key, child) in object {
            let child_path = if path.is_empty() {
                key.clone()
            } else {
                format!("{path}.{key}")
            };
            if complimentary_key(key)
                && let Some(info) = parse_reset_candidate(child, &child_path)
            {
                return Some(info);
            }
        }
        for (key, child) in object {
            if rate_limit_reset_key(key) {
                continue;
            }
            let child_path = if path.is_empty() {
                key.clone()
            } else {
                format!("{path}.{key}")
            };
            if let Some(info) = walk(child, &child_path) {
                return Some(info);
            }
        }
        None
    }
    walk(root, "")
}

fn normalized_usage_key(value: &str) -> String {
    value
        .chars()
        .filter(char::is_ascii_alphanumeric)
        .map(|character| character.to_ascii_lowercase())
        .collect()
}

fn complimentary_key(key: &str) -> bool {
    let key = normalized_usage_key(key);
    (key.contains("complimentary") && key.contains("reset"))
        || (key.contains("onetime") && key.contains("reset"))
        || key.contains("resetcredit")
}

fn rate_limit_reset_key(key: &str) -> bool {
    matches!(
        normalized_usage_key(key).as_str(),
        "resetafterseconds" | "resetat" | "resetsat"
    )
}

fn parse_reset_candidate(value: &Value, source: &str) -> Option<ComplimentaryResetInfo> {
    let mut info = ComplimentaryResetInfo {
        known: true,
        source: source.into(),
        ..ComplimentaryResetInfo::default()
    };
    match value {
        Value::Bool(value) => {
            let source = normalized_usage_key(source);
            if source.contains("used") || source.contains("consumed") || source.contains("redeemed")
            {
                info.consumed = *value;
                info.available = !value;
            } else {
                info.available = *value;
                info.consumed = !value;
            }
        }
        Value::String(status) => apply_reset_status(&mut info, status),
        Value::Object(object) => {
            if let Some(status) = string_from(object, &["status", "state"]) {
                info.status = status.into();
                apply_reset_status(&mut info, status);
            }
            if let Some(value) = bool_from(
                object,
                &[
                    "available",
                    "is_available",
                    "can_reset",
                    "can_use",
                    "claimable",
                ],
            ) {
                info.available = value;
            }
            if let Some(value) = bool_from(
                object,
                &[
                    "consumed",
                    "is_consumed",
                    "used",
                    "is_used",
                    "redeemed",
                    "is_redeemed",
                ],
            ) {
                info.consumed = value;
            }
            if let Some(value) = bool_from(object, &["eligible", "is_eligible"]) {
                info.eligible = Some(value);
                if !value {
                    info.available = false;
                }
            }
            info.remaining = int_from(object, &["remaining", "remaining_count", "available_count"]);
            info.used = int_from(object, &["used_count", "uses", "redeemed_count"]);
            info.total = int_from(object, &["total", "total_count", "limit"]);
            info.resets_at = string_from(object, &["resets_at", "reset_at", "expires_at"])
                .unwrap_or_default()
                .into();
        }
        _ => return None,
    }
    if let Some(remaining) = info.remaining {
        info.available = remaining > 0;
        info.consumed |= remaining == 0;
    }
    if let (Some(total), Some(used)) = (info.total, info.used)
        && total > 0
    {
        info.consumed = used >= total;
        info.available = used < total;
    }
    Some(info)
}

fn apply_reset_status(info: &mut ComplimentaryResetInfo, status: &str) {
    match normalized_usage_key(status).as_str() {
        "available" | "eligible" | "unused" | "unclaimed" | "claimable" => {
            info.available = true;
            info.consumed = false;
        }
        "consumed" | "used" | "redeemed" | "claimed" => {
            info.consumed = true;
            info.available = false;
        }
        "ineligible" | "unavailable" | "disabled" => {
            info.eligible = Some(false);
            info.available = false;
        }
        _ => {}
    }
}

fn bool_from(object: &Map<String, Value>, names: &[&str]) -> Option<bool> {
    names
        .iter()
        .find_map(|name| object.get(*name).and_then(Value::as_bool))
}

fn int_from(object: &Map<String, Value>, names: &[&str]) -> Option<i64> {
    names.iter().find_map(|name| {
        object.get(*name).and_then(|value| {
            value
                .as_i64()
                .or_else(|| value.as_f64().map(|value| value as i64))
        })
    })
}

fn string_from<'a>(object: &'a Map<String, Value>, names: &[&str]) -> Option<&'a str> {
    names
        .iter()
        .find_map(|name| object.get(*name).and_then(Value::as_str))
}

#[derive(Clone, Debug, Default, Deserialize, Eq, PartialEq, Serialize)]
pub struct RateLimitResetCredit {
    pub id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub reset_type: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub status: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub granted_at: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub expires_at: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub redeem_started_at: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub redeemed_at: String,
}

pub async fn list_rate_limit_reset_credits(
    client: &Client,
    account: &Account,
) -> anyhow::Result<Vec<RateLimitResetCredit>> {
    list_rate_limit_reset_credits_at(client, account, CODEX_RESET_CREDITS_URL).await
}

async fn list_rate_limit_reset_credits_at(
    client: &Client,
    account: &Account,
    endpoint: &str,
) -> anyhow::Result<Vec<RateLimitResetCredit>> {
    validate_reset_account(account)?;
    let response = apply_wham_headers(client.get(endpoint), account)
        .send()
        .await?;
    if !response.status().is_success() {
        bail!(
            "list rate-limit reset credits failed: {}",
            response.status()
        );
    }
    #[derive(Deserialize)]
    struct Envelope {
        #[serde(default)]
        credits: Vec<RateLimitResetCredit>,
    }
    Ok(response.json::<Envelope>().await?.credits)
}

pub async fn consume_rate_limit_reset_credit(
    client: &Client,
    account: &Account,
    credit_id: &str,
    redeem_request_id: Option<Uuid>,
) -> anyhow::Result<RateLimitResetCredit> {
    consume_rate_limit_reset_credit_at(
        client,
        account,
        credit_id,
        redeem_request_id,
        &format!("{CODEX_RESET_CREDITS_URL}/consume"),
    )
    .await
}

async fn consume_rate_limit_reset_credit_at(
    client: &Client,
    account: &Account,
    credit_id: &str,
    redeem_request_id: Option<Uuid>,
    endpoint: &str,
) -> anyhow::Result<RateLimitResetCredit> {
    if credit_id.is_empty() {
        bail!("credit_id is required");
    }
    validate_reset_account(account)?;
    let response = apply_wham_headers(
        client.post(endpoint).json(&json!({
            "credit_id": credit_id,
            "redeem_request_id": redeem_request_id.unwrap_or_else(Uuid::new_v4),
        })),
        account,
    )
    .send()
    .await?;
    if !response.status().is_success() {
        bail!(
            "consume rate-limit reset credit failed: {}",
            response.status()
        );
    }
    #[derive(Deserialize)]
    struct Envelope {
        credit: RateLimitResetCredit,
    }
    Ok(response.json::<Envelope>().await?.credit)
}

pub async fn redeem_rate_limit_reset(
    client: &Client,
    account: &Account,
) -> anyhow::Result<RateLimitResetCredit> {
    let credits = list_rate_limit_reset_credits(client, account).await?;
    let credit = credits
        .into_iter()
        .find(|credit| credit.status.is_empty() || credit.status == "available")
        .ok_or_else(|| {
            anyhow!(
                "no available rate-limit reset credits for {}",
                account.email
            )
        })?;
    consume_rate_limit_reset_credit(client, account, &credit.id, None).await
}

fn validate_reset_account(account: &Account) -> anyhow::Result<()> {
    if account.auth_mode != AuthMode::Oauth {
        bail!("rate-limit reset is only available for OAuth accounts");
    }
    if account.token.is_empty() {
        bail!("account has no access token");
    }
    Ok(())
}

fn apply_wham_headers(
    request: reqwest::RequestBuilder,
    account: &Account,
) -> reqwest::RequestBuilder {
    let mut request = request
        .bearer_auth(&account.token)
        .header(reqwest::header::ACCEPT, "application/json")
        .header("OAI-Client-Id", "codex")
        .header("OAI-Language", "en-US")
        .header(reqwest::header::USER_AGENT, CODEX_CLI_USER_AGENT);
    if !account.account_id.is_empty() {
        request = request.header("ChatGPT-Account-ID", &account.account_id);
    }
    request
}

#[derive(Clone, Default, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct AdminKeyEntry {
    pub label: String,
    pub key: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub org_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub org_name: String,
    #[serde(default)]
    pub added_at: String,
}

#[derive(Clone, Debug, Default, Deserialize, Eq, PartialEq, Serialize)]
pub struct OpenAiProject {
    pub id: String,
    pub name: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub status: String,
}

#[derive(Clone, Debug, Default, Deserialize, PartialEq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct DailyUsage {
    pub date: String,
    pub cost_usd: f64,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub cost_estimated: bool,
    pub input_tokens: i64,
    pub cached_input_tokens: i64,
    pub output_tokens: i64,
    pub requests: i64,
}

#[derive(Clone, Debug, Default, Deserialize, Eq, PartialEq, Serialize)]
pub struct TopModel {
    pub model: String,
    pub tokens: i64,
}

#[derive(Clone, Debug, Default, Deserialize, PartialEq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct ApiKeyUsageSnapshot {
    pub admin_key_label: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub org_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub project_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub project_name: String,
    pub fetched_at: String,
    pub today_usd: f64,
    pub today_cost_estimated: bool,
    pub week_usd: f64,
    pub month_usd: f64,
    pub today_tokens: i64,
    pub week_tokens: i64,
    pub month_tokens: i64,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub top_model: Option<TopModel>,
    #[serde(default)]
    pub daily: Vec<DailyUsage>,
}

#[derive(Deserialize, Serialize)]
#[serde(rename_all = "camelCase")]
struct CachedUsageSnapshot {
    fetched_at: String,
    snapshot: ApiKeyUsageSnapshot,
}

impl CodexStore {
    fn admin_keys_dir(&self) -> PathBuf {
        self.store_dir().join("admin-keys")
    }

    fn usage_cache_dir(&self) -> PathBuf {
        self.store_dir().join("usage-cache")
    }

    pub fn list_admin_keys(&self) -> anyhow::Result<Vec<AdminKeyEntry>> {
        let entries = match fs::read_dir(self.admin_keys_dir()) {
            Ok(entries) => entries,
            Err(error) if error.kind() == io::ErrorKind::NotFound => return Ok(Vec::new()),
            Err(error) => return Err(error.into()),
        };
        let mut output = Vec::new();
        for entry in entries {
            let entry = entry?;
            if entry.file_type()?.is_dir()
                || entry
                    .path()
                    .extension()
                    .is_none_or(|extension| extension != "json")
            {
                continue;
            }
            let value: AdminKeyEntry = serde_json::from_slice(&fs::read(entry.path())?)?;
            if !value.label.is_empty() {
                output.push(value);
            }
        }
        output.sort_by(|left, right| left.label.cmp(&right.label));
        Ok(output)
    }

    pub fn find_admin_key(&self, label: &str) -> anyhow::Result<Option<AdminKeyEntry>> {
        match fs::read(
            self.admin_keys_dir()
                .join(format!("{}.json", safe_filename(label))),
        ) {
            Ok(body) => Ok(Some(serde_json::from_slice(&body)?)),
            Err(error) if error.kind() == io::ErrorKind::NotFound => Ok(None),
            Err(error) => Err(error.into()),
        }
    }

    pub fn save_admin_key(&self, entry: &AdminKeyEntry) -> anyhow::Result<()> {
        if entry.label.trim().is_empty() {
            bail!("admin key label is required");
        }
        let mut body = serde_json::to_vec_pretty(entry)?;
        body.push(b'\n');
        write_file_atomic(
            &self
                .admin_keys_dir()
                .join(format!("{}.json", safe_filename(&entry.label))),
            &body,
        )
    }

    pub fn remove_admin_key(&self, label: &str) -> anyhow::Result<bool> {
        match fs::remove_file(
            self.admin_keys_dir()
                .join(format!("{}.json", safe_filename(label))),
        ) {
            Ok(()) => Ok(true),
            Err(error) if error.kind() == io::ErrorKind::NotFound => Ok(false),
            Err(error) => Err(error.into()),
        }
    }

    pub fn pick_admin_key_for(
        &self,
        account: &StoredCodexAccount,
    ) -> anyhow::Result<Option<AdminKeyEntry>> {
        if !account.admin_key_label.is_empty()
            && let Some(entry) = self.find_admin_key(&account.admin_key_label)?
        {
            return Ok(Some(entry));
        }
        Ok(self.list_admin_keys()?.into_iter().next())
    }

    pub fn read_usage_cache(
        &self,
        admin_label: &str,
        project_id: &str,
        max_age: Duration,
    ) -> anyhow::Result<Option<ApiKeyUsageSnapshot>> {
        let Some(snapshot) = self.read_usage_cache_stale(admin_label, project_id)? else {
            return Ok(None);
        };
        if max_age > Duration::ZERO {
            let fresh = DateTime::parse_from_rfc3339(&snapshot.fetched_at)
                .ok()
                .and_then(|time| (Utc::now() - time.with_timezone(&Utc)).to_std().ok())
                .is_some_and(|age| age <= max_age);
            if !fresh {
                return Ok(None);
            }
        }
        Ok(Some(snapshot))
    }

    pub fn read_usage_cache_stale(
        &self,
        admin_label: &str,
        project_id: &str,
    ) -> anyhow::Result<Option<ApiKeyUsageSnapshot>> {
        match fs::read(self.usage_cache_path(admin_label, project_id)) {
            Ok(body) => Ok(Some(
                serde_json::from_slice::<CachedUsageSnapshot>(&body)?.snapshot,
            )),
            Err(error) if error.kind() == io::ErrorKind::NotFound => Ok(None),
            Err(error) => Err(error.into()),
        }
    }

    pub fn write_usage_cache(&self, snapshot: &ApiKeyUsageSnapshot) -> anyhow::Result<()> {
        let cached = CachedUsageSnapshot {
            fetched_at: snapshot.fetched_at.clone(),
            snapshot: snapshot.clone(),
        };
        let mut body = serde_json::to_vec_pretty(&cached)?;
        body.push(b'\n');
        write_file_atomic(
            &self.usage_cache_path(&snapshot.admin_key_label, &snapshot.project_id),
            &body,
        )
    }

    fn usage_cache_path(&self, admin_label: &str, project_id: &str) -> PathBuf {
        let project_id = if project_id.is_empty() {
            "all"
        } else {
            project_id
        };
        self.usage_cache_dir().join(format!(
            "{}__{}.json",
            safe_filename(admin_label),
            safe_filename(project_id)
        ))
    }
}
