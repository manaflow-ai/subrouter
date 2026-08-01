use std::collections::BTreeMap;
use std::fs;
use std::io;
use std::path::{Path, PathBuf};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use anyhow::bail;
use serde::{Deserialize, Serialize};
use serde_json::Value;

use crate::accounts::{StoredCodexAccount, extract_chatgpt_account_id, jwt_expiry_millis};

#[derive(Clone, Default, Deserialize, Serialize)]
pub struct Entry {
    #[serde(rename = "type")]
    pub entry_type: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub refresh: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub access: String,
    #[serde(default, skip_serializing_if = "is_zero")]
    pub expires: i64,
    #[serde(
        rename = "accountId",
        default,
        skip_serializing_if = "String::is_empty"
    )]
    pub account_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub key: String,
    #[serde(default, skip_serializing_if = "BTreeMap::is_empty")]
    pub metadata: BTreeMap<String, String>,
}

const fn is_zero(value: &i64) -> bool {
    *value == 0
}

#[must_use]
pub fn default_auth_path() -> PathBuf {
    if let Some(data_home) =
        std::env::var_os("XDG_DATA_HOME").filter(|value| !value.to_string_lossy().is_empty())
    {
        return PathBuf::from(data_home).join("opencode/auth.json");
    }
    home_dir()
        .unwrap_or_else(|| PathBuf::from("."))
        .join(".local/share/opencode/auth.json")
}

pub fn entry_for_codex_account(
    account: &StoredCodexAccount,
) -> anyhow::Result<(&'static str, Entry)> {
    if account.is_api_key() {
        if account.auth.openai_api_key.is_empty() {
            bail!("OpenCode sync requires a non-empty OpenAI API key");
        }
        return Ok((
            "openai",
            Entry {
                entry_type: "api".into(),
                key: account.auth.openai_api_key.clone(),
                ..Entry::default()
            },
        ));
    }
    let Some(tokens) = account
        .auth
        .tokens
        .as_ref()
        .filter(|tokens| !tokens.access_token.is_empty() && !tokens.refresh_token.is_empty())
    else {
        bail!("OpenCode sync requires Codex OAuth tokens");
    };
    let expires = jwt_expiry_millis(&tokens.access_token).unwrap_or_else(one_hour_from_now_millis);
    Ok((
        "openai",
        Entry {
            entry_type: "oauth".into(),
            refresh: tokens.refresh_token.clone(),
            access: tokens.access_token.clone(),
            expires,
            account_id: extract_chatgpt_account_id(&account.auth),
            ..Entry::default()
        },
    ))
}

pub fn sync_codex_account(account: &StoredCodexAccount) -> anyhow::Result<PathBuf> {
    let (provider, entry) = entry_for_codex_account(account)?;
    let path = default_auth_path();
    write_auth_entry(&path, provider, &entry)?;
    Ok(path)
}

fn write_auth_entry(path: &Path, provider: &str, entry: &Entry) -> anyhow::Result<()> {
    let mut data: BTreeMap<String, Value> = match fs::read(path) {
        Ok(body) => serde_json::from_slice(&body)
            .map_err(|error| anyhow::anyhow!("parse {}: {error}", path.display()))?,
        Err(error) if error.kind() == io::ErrorKind::NotFound => BTreeMap::new(),
        Err(error) => return Err(error.into()),
    };
    data.insert(provider.into(), serde_json::to_value(entry)?);
    let mut body = serde_json::to_vec_pretty(&data)?;
    body.push(b'\n');
    write_atomic_private(path, &body)?;
    Ok(())
}

pub(crate) fn write_atomic_private(path: &Path, body: &[u8]) -> io::Result<()> {
    let parent = path.parent().unwrap_or_else(|| Path::new("."));
    fs::create_dir_all(parent)?;
    let mut temp = tempfile::NamedTempFile::new_in(parent)?;
    use std::io::Write as _;
    temp.write_all(body)?;
    temp.as_file().sync_all()?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        fs::set_permissions(temp.path(), fs::Permissions::from_mode(0o600))?;
    }
    temp.persist(path).map_err(|error| error.error)?;
    Ok(())
}

pub(super) fn home_dir() -> Option<PathBuf> {
    std::env::var_os(if cfg!(windows) { "USERPROFILE" } else { "HOME" }).map(PathBuf::from)
}

fn one_hour_from_now_millis() -> i64 {
    (SystemTime::now() + Duration::from_secs(60 * 60))
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_millis() as i64
}
